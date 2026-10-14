//go:build darwin || linux

package ax

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

const networkProtocol = "ax-network/1"
const networkConfigLimit = 256 << 10
const networkAgentsPath = "/ax/network/v1/agents"

type networkCard struct {
	Protocol    string `json:"protocol"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	Certificate []byte `json:"certificate"`
}
type networkIdentity struct {
	Card networkCard `json:"card"`
	Key  []byte      `json:"private_key"`
}
type networkPeer struct {
	Card      networkCard `json:"card"`
	Exports   []string    `json:"exports"`
	Messaging bool        `json:"messaging,omitempty"`
}
type networkAgent struct {
	ID    string `json:"agent_id"`
	Name  string `json:"name"`
	Host  string `json:"host"`
	State string `json:"state"`
}
type networkDiscovery struct {
	Protocol string         `json:"protocol"`
	Machine  string         `json:"machine"`
	Agents   []networkAgent `json:"agents"`
}

func tailnetIP(ip netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(ip)
}

func tailscaleAddresses(ctx context.Context) ([]netip.Addr, error) {
	data, err := doctorProbe(ctx, "tailscale", "status", "--json", "--peers=false")
	if err != nil {
		return nil, fmt.Errorf("read local Tailscale status: %w", err)
	}
	var status struct {
		BackendState string
		Self         struct{ TailscaleIPs []netip.Addr }
	}
	if json.Unmarshal(data, &status) != nil || status.BackendState != "Running" {
		return nil, errors.New("Tailscale must be running and connected")
	}
	var ips []netip.Addr
	for _, ip := range status.Self.TailscaleIPs {
		if tailnetIP(ip) {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("Tailscale reported no local tailnet address")
	}
	return ips, nil
}

func validateNetworkCard(card networkCard) (*x509.Certificate, error) {
	u, err := url.Parse(card.Address)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("peer address must be an HTTPS tailnet IP and port")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, portErr := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || !tailnetIP(ip) || portErr != nil || port == 0 || card.Protocol != networkProtocol || !validOrigin.MatchString(card.Name) {
		return nil, errors.New("invalid AX network pairing card")
	}
	cert, err := x509.ParseCertificate(card.Certificate)
	if err != nil || cert.VerifyHostname(ip.String()) != nil {
		return nil, errors.New("pairing certificate does not match its tailnet address")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}})
	return cert, err
}

func networkFingerprint(card networkCard) string {
	sum := sha256.Sum256(card.Certificate)
	return hex.EncodeToString(sum[:])
}

func readNetworkJSON(path string, out any) error {
	f, err := privateFile(path, syscall.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, networkConfigLimit+1))
	if err != nil || len(data) > networkConfigLimit {
		return errors.New("AX network configuration exceeds its size limit")
	}
	return json.Unmarshal(data, out)
}

func loadNetworkIdentity(dir string) (networkIdentity, tls.Certificate, error) {
	var identity networkIdentity
	if err := readNetworkJSON(filepath.Join(dir, "network", "identity.json"), &identity); err != nil {
		return identity, tls.Certificate{}, err
	}
	if _, err := validateNetworkCard(identity.Card); err != nil {
		return identity, tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: identity.Card.Certificate}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: identity.Key}))
	return identity, cert, err
}

func initNetwork(ctx context.Context, dir, name string, port uint16) (networkCard, error) {
	if !validOrigin.MatchString(name) || port == 0 {
		return networkCard{}, errors.New("choose a lowercase machine name and a nonzero port")
	}
	path := filepath.Join(dir, "network")
	if err := privateDir(path); err != nil {
		return networkCard{}, err
	}
	lock, err := lockFile(filepath.Join(path, "config.lock"))
	if err != nil {
		return networkCard{}, err
	}
	defer lock.Close()
	identity, _, err := loadNetworkIdentity(dir)
	if err == nil {
		u, _ := url.Parse(identity.Card.Address)
		if identity.Card.Name != name || u.Port() != strconv.Itoa(int(port)) {
			return networkCard{}, errors.New("network identity already exists with a different name or port")
		}
		return identity.Card, nil
	}
	if !os.IsNotExist(err) {
		return networkCard{}, err
	}
	ips, err := tailscaleAddresses(ctx)
	if err != nil {
		return networkCard{}, err
	}
	identity, err = newNetworkIdentity(name, ips[0], port)
	if err == nil {
		err = savePrivateJSON(filepath.Join(path, "identity.json"), identity)
	}
	return identity.Card, err
}

func newNetworkIdentity(name string, ip netip.Addr, port uint16) (networkIdentity, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return networkIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return networkIdentity{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.IP(ip.AsSlice())}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		return networkIdentity{}, err
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	return networkIdentity{Card: networkCard{Protocol: networkProtocol, Name: name, Address: "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), Certificate: der}, Key: privateKey}, err
}

func networkPeers(dir string) (map[string]networkPeer, error) {
	peers := map[string]networkPeer{}
	err := readNetworkJSON(filepath.Join(dir, "network", "peers.json"), &peers)
	if os.IsNotExist(err) {
		err = nil
	}
	if err == nil && (peers == nil || len(peers) > 32) {
		err = errors.New("invalid AX network peer configuration")
	}
	return peers, err
}

func trustNetworkPeer(dir, alias string, card networkCard, exports []string) error {
	if !validOrigin.MatchString(alias) || len(exports) > 64 {
		return errors.New("invalid peer alias or too many exported names")
	}
	for _, name := range exports {
		if name != "*" && !validName.MatchString(name) {
			return errors.New("invalid exported agent name")
		}
	}
	if _, err := validateNetworkCard(card); err != nil {
		return err
	}
	lock, err := lockFile(filepath.Join(dir, "network", "config.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	peers, err := networkPeers(dir)
	if err != nil {
		return err
	}
	for name, peer := range peers {
		same := bytes.Equal(peer.Card.Certificate, card.Certificate)
		if name == alias && !same || name != alias && same {
			return errors.New("peer alias or certificate is already paired; revoke it explicitly before replacing it")
		}
	}
	if _, exists := peers[alias]; !exists && len(peers) >= 32 {
		return errors.New("AX network peer limit reached")
	}
	peers[alias] = networkPeer{Card: card, Exports: exports, Messaging: peers[alias].Messaging}
	if len(raw(peers)) > networkConfigLimit {
		return errors.New("AX network configuration exceeds its size limit")
	}
	return savePrivateJSON(filepath.Join(dir, "network", "peers.json"), peers)
}

func revokeNetworkPeer(dir, alias string) error {
	lock, err := lockFile(filepath.Join(dir, "network", "config.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	peers, err := networkPeers(dir)
	if err != nil {
		return err
	}
	if _, exists := peers[alias]; !exists {
		return errors.New("AX network peer is not paired")
	}
	delete(peers, alias)
	return savePrivateJSON(filepath.Join(dir, "network", "peers.json"), peers)
}

func setNetworkMessaging(dir, alias string, enabled bool) error {
	lock, err := lockFile(filepath.Join(dir, "network", "config.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	peers, err := networkPeers(dir)
	if err != nil {
		return err
	}
	peer, exists := peers[alias]
	if !exists {
		return errors.New("AX network peer is not paired")
	}
	peer.Messaging = enabled
	peers[alias] = peer
	return savePrivateJSON(filepath.Join(dir, "network", "peers.json"), peers)
}

func authenticatedNetworkPeer(dir string, state tls.ConnectionState) (networkPeer, error) {
	if len(state.PeerCertificates) != 1 {
		return networkPeer{}, errors.New("AX network requires a paired client certificate")
	}
	peers, err := networkPeers(dir)
	if err != nil {
		return networkPeer{}, err
	}
	for _, peer := range peers {
		if bytes.Equal(peer.Card.Certificate, state.PeerCertificates[0].Raw) {
			_, err := validateNetworkCard(peer.Card)
			return peer, err
		}
	}
	return networkPeer{}, errors.New("AX network peer is not paired")
}

func networkHandler(dir string, identity networkCard) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.TLS == nil {
			http.Error(w, "mutual TLS required", http.StatusForbidden)
			return
		}
		// Recheck on every request: TLS keepalive cannot outlive revocation.
		peer, err := authenticatedNetworkPeer(dir, *r.TLS)
		if err != nil {
			http.Error(w, "peer is not authorized", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != networkAgentsPath || r.URL.RawQuery != "" || r.ContentLength != 0 {
			http.Error(w, "unsupported network operation", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		c, err := dial(socketPath(dir))
		if err != nil {
			http.Error(w, "local broker unavailable", http.StatusServiceUnavailable)
			return
		}
		defer c.close()
		var agents []Agent
		if err = c.callContext(ctx, "ax.inspect", object{}, &agents); err != nil {
			http.Error(w, "local broker unavailable", http.StatusServiceUnavailable)
			return
		}
		out := networkDiscovery{Protocol: networkProtocol, Machine: identity.Name, Agents: []networkAgent{}}
		for _, agent := range agents {
			if agent.Remote == nil && (slices.Contains(peer.Exports, "*") || slices.Contains(peer.Exports, agent.Name)) {
				out.Agents = append(out.Agents, networkAgent{ID: agent.ID, Name: agent.Name, Host: agent.Host, State: agent.State})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

func serveNetwork(ctx context.Context, dir string, listener net.Listener, identity networkIdentity, cert tls.Certificate) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, VerifyConnection: func(state tls.ConnectionState) error {
		_, err := authenticatedNetworkPeer(dir, state)
		return err
	}}
	server := &http.Server{Handler: networkHandler(dir, identity.Card), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	defer close(done)
	err := server.Serve(tls.NewListener(limitListener(listener, 16), tlsConfig))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func networkHTTPClient(cert tls.Certificate, card networkCard) (*http.Client, error) {
	remoteCert, err := validateNetworkCard(card)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(remoteCert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 || !bytes.Equal(state.PeerCertificates[0].Raw, card.Certificate) {
			return errors.New("peer certificate does not match the pairing card")
		}
		return nil
	}}, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 8 << 10}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("AX network redirects are not permitted")
	}}, nil
}

func discoverNetwork(ctx context.Context, dir, alias string) (networkDiscovery, error) {
	_, cert, err := loadNetworkIdentity(dir)
	if err != nil {
		return networkDiscovery{}, err
	}
	peers, err := networkPeers(dir)
	if err != nil {
		return networkDiscovery{}, err
	}
	peer, exists := peers[alias]
	if !exists {
		return networkDiscovery{}, errors.New("AX network peer is not paired")
	}
	client, err := networkHTTPClient(cert, peer.Card)
	if err != nil {
		return networkDiscovery{}, err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.Card.Address+networkAgentsPath, nil)
	if err != nil {
		return networkDiscovery{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return networkDiscovery{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return networkDiscovery{}, fmt.Errorf("peer discovery returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxFrame+1))
	var out networkDiscovery
	if err != nil || len(data) > maxFrame || json.Unmarshal(data, &out) != nil || out.Protocol != networkProtocol || out.Machine != peer.Card.Name || len(out.Agents) > 256 {
		return out, errors.New("invalid AX network discovery response")
	}
	return out, nil
}

//go:build darwin || linux

package ax

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const networkJoinPath = "/ax/network/v1/join"
const networkInvitePrefix = "ax1."
const networkInviteLifetime = 10 * time.Minute
const networkInviteLimit = 4096

type networkInvite struct {
	Host   networkCard `json:"host"`
	Secret string      `json:"secret"`
}

type networkInviteState struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
	Exports []string  `json:"exports"`
	Claimed string    `json:"claimed,omitempty"`
}

type networkJoinRequest struct {
	Card   networkCard `json:"card"`
	Secret string      `json:"secret"`
}

func networkInvitePath(dir string) string {
	return filepath.Join(dir, "network", "invite.json")
}

func createNetworkInvite(dir string, host networkCard, exports []string, now time.Time) (string, error) {
	lock, err := lockFile(filepath.Join(dir, "network", "config.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	secret := randomID("")
	state := networkInviteState{Hash: digest(secret), Expires: now.Add(networkInviteLifetime), Exports: exports}
	if err := savePrivateJSON(networkInvitePath(dir), state); err != nil {
		return "", err
	}
	return networkInvitePrefix + base64.RawURLEncoding.EncodeToString(raw(networkInvite{Host: host, Secret: secret})), nil
}

func readNetworkInvite(in io.Reader) (networkInvite, error) {
	line, err := bufio.NewReader(io.LimitReader(in, networkInviteLimit+2)).ReadString('\n')
	if err != nil && err != io.EOF {
		return networkInvite{}, err
	}
	encoded, ok := strings.CutPrefix(strings.TrimSpace(line), networkInvitePrefix)
	if !ok || len(line) > networkInviteLimit {
		return networkInvite{}, errors.New("paste a complete AX invitation")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var invite networkInvite
	if err != nil || json.Unmarshal(data, &invite) != nil || !validInviteSecret(invite.Secret) {
		return networkInvite{}, errors.New("invalid AX invitation")
	}
	_, err = validateNetworkCard(invite.Host)
	return invite, err
}

func awaitNetworkInvite(ctx context.Context, in io.Reader) (networkInvite, error) {
	type result struct {
		invite networkInvite
		err    error
	}
	// Reading a paste or pipe cannot block cancellation of the CLI process.
	read := make(chan result, 1)
	go func() {
		invite, err := readNetworkInvite(in)
		read <- result{invite, err}
	}()
	select {
	case <-ctx.Done():
		return networkInvite{}, ctx.Err()
	case got := <-read:
		if err := ctx.Err(); err != nil {
			return networkInvite{}, err
		}
		return got.invite, got.err
	}
}

func validInviteSecret(secret string) bool {
	if len(secret) != 32 {
		return false
	}
	_, err := hex.DecodeString(secret)
	return err == nil
}

func networkInviteAcceptsCertificate(dir string, tlsState tls.ConnectionState) bool {
	if len(tlsState.PeerCertificates) != 1 {
		return false
	}
	var invite networkInviteState
	if err := readNetworkJSON(networkInvitePath(dir), &invite); err != nil || !time.Now().Before(invite.Expires) {
		return false
	}
	if invite.Claimed == "" {
		return true
	}
	return invite.Claimed == networkFingerprint(networkCard{Certificate: tlsState.PeerCertificates[0].Raw})
}

// Call with config.lock held. Invalidate a claimed invite before revoking its
// pairing so a retry cannot recreate a grant that the operator revoked.
func invalidateNetworkInvite(dir, fingerprint string) error {
	var invite networkInviteState
	path := networkInvitePath(dir)
	err := readNetworkJSON(path, &invite)
	if os.IsNotExist(err) || err == nil && invite.Claimed != fingerprint {
		return nil
	}
	return os.Remove(path)
}

func handleNetworkJoin(dir string, host networkCard, w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Error(w, "unsupported network operation", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST to redeem an AX invitation", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	var request networkJoinRequest
	if err != nil || json.Unmarshal(data, &request) != nil || !validInviteSecret(request.Secret) {
		http.Error(w, "invalid AX join request", http.StatusBadRequest)
		return
	}
	card := request.Card
	if _, err := validateNetworkCard(card); err != nil || card.Name == host.Name ||
		!networkCertificateMatches(*r.TLS, card.Certificate) {
		http.Error(w, "joining certificate or machine name is invalid", http.StatusForbidden)
		return
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	u, _ := url.Parse(card.Address)
	advertised, _ := netip.ParseAddr(u.Hostname())
	// Authenticate before running Tailscale or competing for the config lock.
	// Redemption checks again under the lock to catch rotation and expiry.
	if _, err := authenticateNetworkInvite(dir, request.Secret, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err != nil || !sameTailscaleDevice(r.Context(), remote.Addr().Unmap(), advertised.Unmap()) {
		http.Error(w, "joining address must belong to the originating Tailscale device", http.StatusForbidden)
		return
	}
	if err := redeemNetworkInvite(dir, request, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func sameTailscaleDevice(ctx context.Context, source, advertised netip.Addr) bool {
	if !tailnetIP(source) || !tailnetIP(advertised) {
		return false
	}
	if source == advertised {
		return true
	}
	if source.BitLen() == advertised.BitLen() {
		return false
	}
	// A device can connect over IPv4 while advertising its IPv6 listener, or
	// vice versa. Node.Addresses contains the assigned addresses of this device.
	data, err := doctorProbe(ctx, "tailscale", "whois", "--json", source.String())
	var peer struct {
		Node struct{ Addresses []netip.Prefix }
	}
	return err == nil && json.Unmarshal(data, &peer) == nil &&
		slices.Contains(peer.Node.Addresses, netip.PrefixFrom(advertised, advertised.BitLen()))
}

func redeemNetworkInvite(dir string, request networkJoinRequest, now time.Time) error {
	lock, err := lockFile(filepath.Join(dir, "network", "config.lock"))
	if err != nil {
		return errors.New("AX pairing is busy; retry the invite")
	}
	defer lock.Close()
	invite, err := authenticateNetworkInvite(dir, request.Secret, now)
	if err != nil {
		return err
	}
	fingerprint := networkFingerprint(request.Card)
	if invite.Claimed != "" && invite.Claimed != fingerprint {
		return errors.New("AX invitation was used by another installation")
	}
	peers, err := networkPeers(dir)
	if err != nil {
		return errors.New("read local AX pairing configuration")
	}
	existing, exists := peers[request.Card.Name]
	if exists && !sameNetworkGrant(existing, request.Card, invite.Exports) {
		return errors.New("peer already paired; revoke it explicitly before joining again")
	}
	if err := validateNetworkExports(request.Card.Name, invite.Exports); err != nil {
		return err
	}
	paired := networkPeer{Card: request.Card, Exports: invite.Exports, Messaging: true}
	if err := prepareNetworkPeer(peers, request.Card.Name, paired); err != nil {
		return err
	}
	// Reserve the invitation for this exact certificate before writing the
	// grant. A lost response or interrupted write can safely retry the claim.
	if invite.Claimed == "" {
		invite.Claimed = fingerprint
		if err := savePrivateJSON(networkInvitePath(dir), invite); err != nil {
			return errors.New("save local AX invitation claim")
		}
	}
	if exists {
		return nil
	}
	if err := savePrivateJSON(filepath.Join(dir, "network", "peers.json"), peers); err != nil {
		return errors.New("could not save the invited AX peer; check the existing pairings and peer limit")
	}
	return nil
}

func authenticateNetworkInvite(dir, secret string, now time.Time) (networkInviteState, error) {
	var invite networkInviteState
	if err := readNetworkJSON(networkInvitePath(dir), &invite); err != nil ||
		!now.Before(invite.Expires) || subtle.ConstantTimeCompare([]byte(invite.Hash), []byte(digest(secret))) != 1 {
		return invite, errors.New("AX invitation expired or is invalid")
	}
	return invite, nil
}

func sameNetworkGrant(peer networkPeer, card networkCard, exports []string) bool {
	return peer.Messaging && peer.Card.Name == card.Name && peer.Card.Address == card.Address &&
		bytes.Equal(peer.Card.Certificate, card.Certificate) && slices.Equal(peer.Exports, exports)
}

func requestNetworkJoin(ctx context.Context, client *http.Client, invite networkInvite, card networkCard) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, invite.Host.Address+networkJoinPath, bytes.NewReader(raw(networkJoinRequest{Card: card, Secret: invite.Secret})))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	res, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("reach %s over Tailscale for invite redemption: %w", invite.Host.Name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return fmt.Errorf("AX invite rejected by %s (HTTP %d): %q", invite.Host.Name, res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func joinNetwork(ctx context.Context, dir string, invite networkInvite, card networkCard, exports []string, client *http.Client) error {
	lock, err := lockFile(filepath.Join(dir, "network", "config.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	peers, err := networkPeers(dir)
	if err != nil {
		return err
	}
	current, exists := peers[invite.Host.Name]
	if exists && !sameNetworkGrant(current, invite.Host, exports) {
		return errors.New("inviting machine already paired with different settings")
	}
	peer := networkPeer{Card: invite.Host, Exports: exports, Messaging: true}
	if err := prepareNetworkPeer(peers, invite.Host.Name, peer); err != nil {
		return err
	}
	if err := requestNetworkJoin(ctx, client, invite, card); err != nil {
		return err
	}
	if exists {
		return nil
	}
	return savePrivateJSON(filepath.Join(dir, "network", "peers.json"), peers)
}

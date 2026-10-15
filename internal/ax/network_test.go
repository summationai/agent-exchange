//go:build darwin || linux

package ax

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNetworkIdentity(t *testing.T, dir, name, ip string) (networkIdentity, tls.Certificate) {
	t.Helper()
	identity, err := newNetworkIdentity(name, netip.MustParseAddr(ip), 48889)
	if err != nil {
		t.Fatal(err)
	}
	if err = privateDir(filepath.Join(dir, "network")); err != nil {
		t.Fatal(err)
	}
	if err = savePrivateJSON(filepath.Join(dir, "network", "identity.json"), identity); err != nil {
		t.Fatal(err)
	}
	saved, cert, err := loadNetworkIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	return saved, cert
}

func testNetworkServer(t *testing.T, dir string, identity networkIdentity, cert tls.Certificate) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveNetwork(ctx, dir, listener, identity, cert) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("network adapter did not stop")
		}
	}
	t.Cleanup(cancel)
	return listener.Addr().String(), stop
}

func testNetworkClient(t *testing.T, cert tls.Certificate, remote networkCard, address string) *http.Client {
	t.Helper()
	client, err := networkHTTPClient(cert, remote)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func TestNetworkScopedDiscoveryAndRevocation(t *testing.T) {
	a, b := startTestServer(t), startTestServer(t)
	ia, ca := testNetworkIdentity(t, a, "laptop", "100.64.0.1")
	ib, cb := testNetworkIdentity(t, b, "buildbox", "100.64.0.2")
	if err := trustNetworkPeer(a, "buildbox", ib.Card, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := trustNetworkPeer(b, "laptop", ia.Card, []string{"api"}); err != nil {
		t.Fatal(err)
	}
	add := func(dir, name string) *client {
		return connectDiscoveryPeer(t, dir, Session{ID: randomID("agt_"), Secret: randomID(""), Name: name, Host: "claude", Mesh: testMesh, Native: uuid(), Workspace: "/private/project"})
	}
	web, hidden := add(a, "web"), add(a, "hidden")
	add(b, "api")
	add(b, "private")
	aa, stopA := testNetworkServer(t, a, ia, ca)
	ab, stopB := testNetworkServer(t, b, ib, cb)
	defer stopB()
	clientA, clientB := testNetworkClient(t, ca, ib.Card, ab), testNetworkClient(t, cb, ia.Card, aa)
	fetch := func(client *http.Client, card networkCard, expected string) {
		t.Helper()
		res, err := client.Get(card.Address + networkAgentsPath)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		var got networkDiscovery
		if err != nil || res.StatusCode != 200 || json.Unmarshal(data, &got) != nil || len(got.Agents) != 1 || got.Agents[0].Name != expected || got.Machine != card.Name {
			t.Fatalf("unexpected discovery: %s, %v", data, err)
		}
		for _, secret := range []string{"private", "hidden", "native_session", "permission", "workspace", "secret"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("discovery leaked %q", secret)
			}
		}
	}
	fetch(clientA, ib.Card, "api")
	fetch(clientB, ia.Card, "web")
	// A different OS user's AX identity is unpaired even on the same device IP.
	_, outsider := testNetworkIdentity(t, testDir(t), "outsider", "100.64.0.1")
	bad := testNetworkClient(t, outsider, ib.Card, ab)
	if res, err := bad.Get(ib.Card.Address + networkAgentsPath); err == nil {
		res.Body.Close()
		t.Fatal("unpaired installation authenticated")
	}
	for _, path := range []string{"/ax.inspect", "/ax.enroll", "/ax/network/v1/agents?scope=*"} {
		res, err := clientA.Get(ib.Card.Address + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("exported unsupported operation %s", path)
		}
	}
	if err := trustNetworkPeer(b, "laptop", ia.Card, nil); err != nil {
		t.Fatal(err)
	}
	res, err := clientA.Get(ib.Card.Address + networkAgentsPath)
	if err != nil {
		t.Fatal(err)
	}
	var empty networkDiscovery
	json.NewDecoder(res.Body).Decode(&empty)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if len(empty.Agents) != 0 {
		t.Fatal("empty export list exposed agents")
	}
	if err := revokeNetworkPeer(b, "laptop"); err != nil {
		t.Fatal(err)
	}
	reused := false
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
	req, _ := http.NewRequestWithContext(ctx, "GET", ib.Card.Address+networkAgentsPath, nil)
	res, err = clientA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 || !reused {
		t.Fatalf("revocation failed on keepalive: status=%d reused=%v", res.StatusCode, reused)
	}
	stopA()
	var sent Message
	if err := web.call("ax.send", object{"target": "hidden", "text": "local still works", "client_message_id": "after-adapter-exit"}, &sent); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-hidden.offers:
		if got.ID != sent.ID {
			t.Fatal("wrong local message")
		}
	case <-time.After(time.Second):
		t.Fatal("stopping network adapter disrupted local delivery")
	}
}

func TestNetworkInitializationAndPairingGuards(t *testing.T) {
	dir, bin := testDir(t), testDir(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	status := `{"BackendState":"Running","Self":{"TailscaleIPs":["100.100.1.2"]}}`
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\nprintf '%s' '"+status+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	if err := networkCLI(context.Background(), dir, []string{"init", "laptop"}, nil, &public); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(public.String(), "private_key") {
		t.Fatal("public card contains private key")
	}
	first, _, err := loadNetworkIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := initNetwork(context.Background(), dir, "laptop", 48889)
	if err != nil || !bytes.Equal(first.Card.Certificate, second.Certificate) {
		t.Fatal("initialization replaced identity")
	}
	if _, err := initNetwork(context.Background(), dir, "renamed", 48889); err == nil {
		t.Fatal("silently renamed identity")
	}
	other, _ := testNetworkIdentity(t, testDir(t), "buildbox", "100.100.1.3")
	if err := trustNetworkPeer(dir, "buildbox", other.Card, []string{"api"}); err != nil {
		t.Fatal(err)
	}
	replacement, _ := testNetworkIdentity(t, testDir(t), "buildbox", "100.100.1.3")
	if err := trustNetworkPeer(dir, "buildbox", replacement.Card, nil); err == nil {
		t.Fatal("silently replaced peer identity")
	}
	if err := trustNetworkPeer(dir, "other-alias", other.Card, nil); err == nil {
		t.Fatal("paired one certificate under ambiguous aliases")
	}
	for _, address := range []string{"https://0.0.0.0:48889", "https://127.0.0.1:48889", "https://192.168.1.1:48889", "http://100.100.1.3:48889", "https://100.100.1.3:48889/admin", "https://100.100.1.4:48889"} {
		card := other.Card
		card.Address = address
		if err := trustNetworkPeer(dir, "invalid", card, nil); err == nil {
			t.Fatalf("accepted invalid pairing address %s", address)
		}
	}
	// Corrupt state is an error, never an invitation to silently generate keys.
	if err := savePrivateJSON(filepath.Join(dir, "network", "identity.json"), object{"corrupt": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := initNetwork(context.Background(), dir, "laptop", 48889); err == nil {
		t.Fatal("silently replaced corrupt identity")
	}
}

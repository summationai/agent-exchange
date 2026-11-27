//go:build darwin || linux

package ax

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNetworkInvitePairingAndLostResponse(t *testing.T) {
	a, b := startTestServer(t), startTestServer(t)
	ia, ca := testNetworkIdentity(t, a, "laptop", "100.64.0.1")
	ib, cb := testNetworkIdentity(t, b, "buildbox", "fd7a:115c:a1e0::2")
	ctx := context.Background()
	commands := t.TempDir()
	whois := "#!/bin/sh\ntest \"$1 $2 $3\" = 'whois --json 100.64.0.2' || exit 1\n" +
		"printf '%s\\n' '{\"Node\":{\"Addresses\":[\"100.64.0.2/32\",\"fd7a:115c:a1e0::2/128\"]}}'\n"
	if err := os.WriteFile(filepath.Join(commands, "tailscale"), []byte(whois), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))

	token, err := createNetworkInvite(a, ia.Card, []string{"web"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	invite, err := readNetworkInvite(strings.NewReader(token + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	// The TLS connection runs over loopback. The handler sees the client's
	// fixture Tailscale address, just as it would on the two actual devices.
	handler := networkHandler(a, ia.Card)
	var tailnetSource atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tailnetSource.Load() {
			r.RemoteAddr = "100.64.0.2:48123"
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = networkServerTLS(a, ca)
	server.StartTLS()
	t.Cleanup(server.Close)
	client := testNetworkClient(t, cb, ia.Card, server.Listener.Addr().String())

	res, err := client.Get(ia.Card.Address + networkAgentsPath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unpaired client accessed discovery: %d", res.StatusCode)
	}
	if err := requestNetworkJoin(ctx, client, invite, ib.Card); err == nil {
		t.Fatal("joining request from a different source IP was accepted")
	}
	tailnetSource.Store(true)
	otherInvite := invite
	otherInvite.Secret = randomID("")
	if err := requestNetworkJoin(ctx, client, otherInvite, ib.Card); err == nil {
		t.Fatal("wrong secret redeemed the invitation")
	}
	different, _ := testNetworkIdentity(t, testDir(t), "buildbox", "fd7a:115c:a1e0::2")
	if err := requestNetworkJoin(ctx, client, invite, different.Card); err == nil {
		t.Fatal("request's certificate did not match the TLS client")
	}
	// The inviter wrote its grant, but the joining side lost the response.
	if err := requestNetworkJoin(ctx, client, invite, ib.Card); err != nil {
		t.Fatal(err)
	}
	if err := joinNetwork(ctx, b, invite, ib.Card, []string{"api"}, client); err != nil {
		t.Fatal(err)
	}
	add := func(dir, name string) Session {
		s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: name, Host: "codex", Mesh: testMesh, Native: uuid()}
		connectDiscoveryPeer(t, dir, s)
		return s
	}
	web, api := add(a, "web"), add(b, "api")
	add(a, "hidden")
	add(b, "private")
	baddr, stop := testNetworkServer(t, b, ib, cb)
	defer stop()
	for _, c := range []struct {
		client *http.Client
		remote networkCard
		from   Session
		to     Session
	}{
		{client, ia.Card, api, web},
		{testNetworkClient(t, ca, ib.Card, baddr), ib.Card, web, api},
	} {
		d, err := discoverNetworkClient(ctx, c.client, c.remote)
		if err != nil || len(d.Agents) != 1 || d.Agents[0].Name != c.to.Name {
			t.Fatalf("wrong discovery scope: %+v, %v", d, err)
		}
		m := networkEnvelope{ID: randomID("msg_"), SenderID: c.from.ID, SenderName: c.from.Name, SenderHost: c.from.Host,
			Recipient: c.to.ID, Text: "fixture", Expires: time.Now().Add(time.Minute).UnixMilli()}
		if id, err := networkSend(ctx, c.client, c.remote, &m); err != nil || id == "" {
			t.Fatalf("paired message failed: %q, %v", id, err)
		}
	}

	if err := setNetworkMessaging(a, "buildbox", false); err != nil {
		t.Fatal(err)
	}
	if err := joinNetwork(ctx, b, invite, ib.Card, []string{"api"}, client); err == nil {
		t.Fatal("retry silently restored a disabled grant")
	}
	if err := revokeNetworkPeer(a, "buildbox"); err != nil {
		t.Fatal(err)
	}
	if err := requestNetworkJoin(ctx, client, invite, ib.Card); err == nil {
		t.Fatal("retry resurrected a revoked grant")
	}
}

func TestNetworkInvitationClaimsOneInstallation(t *testing.T) {
	dir := testDir(t)
	host, _ := testNetworkIdentity(t, dir, "laptop", "100.64.0.1")
	peer, _ := testNetworkIdentity(t, testDir(t), "buildbox", "100.64.0.2")
	other, _ := testNetworkIdentity(t, testDir(t), "desktop", "100.64.0.3")
	now := time.Now()
	token, err := createNetworkInvite(dir, host.Card, []string{"web"}, now)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := readNetworkInvite(strings.NewReader(token))
	if err != nil {
		t.Fatal(err)
	}
	req := networkJoinRequest{Card: peer.Card, Secret: invite.Secret}
	if err := redeemNetworkInvite(dir, req, now.Add(networkInviteLifetime)); err == nil {
		t.Fatal("expired invitation was accepted")
	}
	if err := redeemNetworkInvite(dir, req, now); err != nil {
		t.Fatal(err)
	}
	if err := redeemNetworkInvite(dir, networkJoinRequest{Card: other.Card, Secret: invite.Secret}, now); err == nil {
		t.Fatal("second installation used the same invitation")
	}
	pairs, err := networkPeers(dir)
	if err != nil || len(pairs) != 1 || !pairs["buildbox"].Messaging || !slices.Equal(pairs["buildbox"].Exports, []string{"web"}) {
		t.Fatalf("unexpected pairing configuration: %+v, %v", pairs, err)
	}
}

func TestNetworkInviteChecksPeerCapacityBeforeRemoteGrant(t *testing.T) {
	dir := testDir(t)
	self, _ := testNetworkIdentity(t, dir, "buildbox", "100.64.0.2")
	host, _ := testNetworkIdentity(t, testDir(t), "laptop", "100.64.0.1")
	peers := map[string]networkPeer{
		"old": {Exports: []string{strings.Repeat("x", networkConfigLimit-300)}},
	}
	if err := savePrivateJSON(filepath.Join(dir, "network", "peers.json"), peers); err != nil {
		t.Fatal(err)
	}
	transport := &offlineNetworkTransport{}
	client := &http.Client{Transport: transport}
	err := joinNetwork(context.Background(), dir, networkInvite{Host: host.Card}, self.Card, []string{"api"}, client)
	if err == nil || !strings.Contains(err.Error(), "configuration exceeds its size limit") || transport.calls.Load() != 0 {
		t.Fatalf("invalid local config reached the inviter: %v", err)
	}
}

func TestNetworkJoinCancelsWhileWaitingForInvite(t *testing.T) {
	dir := testDir(t)
	in, writer := io.Pipe()
	defer in.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- networkPairCLI(ctx, dir, "join", "buildbox", nil, in, io.Discard)
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted prompt returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin blocked cancellation")
	}
	if _, err := os.Stat(filepath.Join(dir, "network")); !os.IsNotExist(err) {
		t.Fatalf("canceled prompt initialized network state: %v", err)
	}
}

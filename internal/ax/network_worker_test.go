//go:build darwin || linux

package ax

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type offlineNetworkTransport struct{ calls atomic.Int32 }

func (t *offlineNetworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("peer offline")
}
func TestNetworkWorkerBackoffSurvivesHintFlood(t *testing.T) {
	a, _, _, _, _, _, _, _ := networkWireFixture(t)
	peers, err := networkPeers(a)
	if err != nil {
		t.Fatal(err)
	}
	transport := &offlineNetworkTransport{}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	go func() { defer close(done); networkPeerWorker(ctx, a, "buildbox", peers["buildbox"], client, wake) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(1600 * time.Millisecond)
	defer deadline.Stop()
	running := true
	for running {
		select {
		case <-ticker.C:
			select {
			case wake <- struct{}{}:
			default:
			}
		case <-deadline.C:
			running = false
		}
	}
	if got := transport.calls.Load(); got < 1 || got > 2 {
		t.Fatalf("outage hints defeated backoff: %d attempts", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt backoff")
	}
}

func TestNetworkRequestRejectsReplacedPairing(t *testing.T) {
	a, b := pairedNetworkBrokers(t)
	web, _, _ := endpoint(t, a, "web", testMesh)
	api, _, _ := endpoint(t, b, "api", testMesh)
	envelope := networkEnvelope{ID: "replaced", SenderID: web.ID, SenderName: web.Name, SenderHost: web.Host, Recipient: api.ID, Text: "task", Expires: time.Now().Add(time.Minute).UnixMilli()}
	for _, method := range []string{"ax.network.receive", "ax.network.outbox", "ax.network.sync", "ax.network.tasks", "ax.network.task", "ax.network.cancel"} {
		if _, err := b.request(&serverConn{}, method, raw(object{"alias": "laptop", "fingerprint": "old-certificate", "envelope": envelope, "recipient_id": api.ID})); err == nil {
			t.Fatalf("stale authenticated operation reached replacement identity: %s", method)
		}
	}
}

func TestNetworkDiscoveryOverflowStillDrainsKnownRecipient(t *testing.T) {
	a, b, _, ib, _, _, client, _ := networkWireFixture(t)
	web := startAttachedMCP(t, a, "-n", "web", "-s", "overflow_web", "-p", "default")
	api := startAttachedMCP(t, b, "-n", "api", "-s", "overflow_api", "-p", "default")
	var listed struct{ Agents []Agent }
	if err := api.call(t, "list_agents", object{}, &listed); err != "" {
		t.Fatal(err)
	}
	recipient := listed.Agents[0]
	admin, err := dial(socketPath(a))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.close()
	if err = admin.call("ax.network.sync", object{"alias": "buildbox", "agents": []networkAgent{{ID: recipient.ID, Name: recipient.Name, Host: recipient.Host, State: recipient.State}}}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		connectDiscoveryPeer(t, b, Session{ID: randomID("agt_"), Secret: randomID(""), Name: fmt.Sprintf("a%02d", i), Host: "claude", Mesh: testMesh, Native: uuid(), Workspace: "/test"})
	}
	paired, err := networkPeers(b)
	if err != nil {
		t.Fatal(err)
	}
	if err = trustNetworkPeer(b, "laptop", paired["laptop"].Card, []string{"*"}); err != nil {
		t.Fatal(err)
	}
	discovery, err := discoverNetworkClient(context.Background(), client, ib.Card)
	if err != nil || len(discovery.Agents) != networkDiscoveryLimit || !discovery.Truncated {
		t.Fatalf("overflow not explicit and bounded: %+v %v", discovery, err)
	}
	for _, agent := range discovery.Agents {
		if agent.ID == recipient.ID {
			t.Fatal("fixture recipient should be outside capped advertisement")
		}
	}
	if err := web.call(t, "send_message", object{"target": "api@buildbox", "text": "still deliver", "client_message_id": "overflow_send"}, nil); err != "" {
		t.Fatal(err)
	}
	peers, err := networkPeers(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	go func() { defer close(done); networkPeerWorker(ctx, a, "buildbox", peers["buildbox"], client, wake) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("overflow worker did not stop")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var incoming struct{ Message *Message }
		if err := api.call(t, "check_inbox", object{}, &incoming); err != "" {
			t.Fatal(err)
		}
		if incoming.Message != nil {
			if incoming.Message.Text != "still deliver" {
				t.Fatal("changed queued request")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("discovery/cache overflow blocked queued delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var snapshot []Agent
	if err = admin.call("ax.inspect", object{}, &snapshot); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, agent := range snapshot {
		if agent.Remote != nil {
			count++
		}
	}
	if count != networkDiscoveryLimit {
		t.Fatalf("cache bound changed: %d", count)
	}
}

type oversizedDiscoveryTransport struct{ card networkCard }

func (t oversizedDiscoveryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	out := networkDiscovery{Protocol: networkProtocol, Machine: t.card.Name, Agents: make([]networkAgent, 65)}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw(out))), Header: make(http.Header)}, nil
}
func TestNetworkLegacyOversizedDiscoveryIsPartialNotOutage(t *testing.T) {
	card := networkCard{Name: "buildbox", Address: "https://100.64.0.2:48889"}
	discovery, err := discoverNetworkClient(context.Background(), &http.Client{Transport: oversizedDiscoveryTransport{card}}, card)
	if err != nil || len(discovery.Agents) != networkDiscoveryLimit || !discovery.Truncated {
		t.Fatalf("legacy advertisement blocked routing: %+v %v", discovery, err)
	}
}

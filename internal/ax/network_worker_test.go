//go:build darwin || linux

package ax

import (
	"context"
	"errors"
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

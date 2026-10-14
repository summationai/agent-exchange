//go:build darwin || linux

package ax

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// One local subscriber receives coalesced hints, not one poller per session.
func (b *broker) watchNetwork(c *serverConn) (any, error) {
	if b.networkWatcher != nil && b.networkWatcher != c {
		return nil, errors.New("network worker is already connected")
	}
	if c.networkWake == nil {
		c.networkWake = make(chan struct{}, 1)
		c.networkDone = make(chan struct{})
		b.networkWatcher = c
		go func() {
			for {
				select {
				case <-c.networkDone:
					return
				case <-c.networkWake:
					if c.send(packet{Method: "ax.peers.changed"}) != nil {
						c.Close()
						return
					}
				}
			}
		}()
	}
	return object{"ok": true}, nil
}
func (b *broker) networkChanged() {
	if c := b.networkWatcher; c != nil {
		select {
		case c.networkWake <- struct{}{}:
		default:
		}
	}
}

func networkSend(ctx context.Context, client *http.Client, card networkCard, envelope *networkEnvelope) (string, error) {
	bounded := *client
	bounded.Transport = networkBoundedTransport{client.Transport}
	transport := a2aclient.NewJSONRPCTransport(card.Address+networkA2APath+envelope.Recipient, &bounded)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(envelope.Text))
	msg.ID = envelope.ID
	msg.Extensions = []string{networkAXExtension}
	wire := *envelope
	wire.Text = "" // The A2A text part is the single wire copy.
	msg.Metadata = map[string]any{networkAXExtension: wire}
	history := 0
	result, err := transport.SendMessage(ctx, a2aclient.ServiceParams{a2a.SvcParamVersion: []string{string(a2a.Version)}, a2a.SvcParamExtensions: []string{networkAXExtension}}, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true, HistoryLength: &history}})
	if err != nil {
		return "", err
	}
	task, ok := result.(*a2a.Task)
	if !ok || !validID.MatchString(string(task.ID)) {
		return "", errors.New("peer returned an invalid durable task receipt")
	}
	return string(task.ID), nil
}

func discoverNetworkClient(ctx context.Context, client *http.Client, card networkCard) (networkDiscovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, card.Address+networkAgentsPath, nil)
	if err != nil {
		return networkDiscovery{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return networkDiscovery{}, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxFrame+1))
	var out networkDiscovery
	if err != nil || res.StatusCode != http.StatusOK || len(data) > maxFrame || json.Unmarshal(data, &out) != nil || out.Protocol != networkProtocol || out.Machine != card.Name || len(out.Agents) > 64 {
		return out, errors.New("invalid network discovery response")
	}
	return out, nil
}

// Timers are only for discovery/health and bounded outage recovery. New local
// messages wake these workers immediately through the private broker socket.
func networkPeerWorker(ctx context.Context, dir, alias string, paired networkPeer, client *http.Client, wake <-chan struct{}) {
	defer client.CloseIdleConnections()
	delay := time.Duration(0)
	failures := 0
	var discoveryAt time.Time
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
			if failures > 0 { // Hints cannot defeat outage backoff.
				timer = time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		case <-timer.C:
		}
		batchFull := false
		err := func() error {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			ctx := callCtx
			current, err := networkPeers(dir)
			if err != nil {
				return err
			}
			p, ok := current[alias]
			if !ok || !p.Messaging || networkFingerprint(p.Card) != networkFingerprint(paired.Card) {
				return errors.New("messaging grant changed")
			}
			c, err := dial(socketPath(dir))
			if err != nil {
				return err
			}
			defer c.close()
			if time.Since(discoveryAt) >= 30*time.Second {
				discovery, err := discoverNetworkClient(ctx, client, paired.Card)
				if err != nil {
					return err
				}
				if err = c.callContext(ctx, "ax.network.sync", object{"alias": alias, "fingerprint": networkFingerprint(paired.Card), "agents": discovery.Agents}, nil); err != nil {
					return err
				}
				discoveryAt = time.Now()
			}
			var firstFailure error
			var skipped []string
			for range 16 {
				var envelope *networkEnvelope
				if err = c.callContext(ctx, "ax.network.outbox", object{"alias": alias, "fingerprint": networkFingerprint(paired.Card), "skip_recipients": skipped}, &envelope); err != nil {
					return err
				}
				if envelope == nil {
					return firstFailure
				}
				remoteID, err := networkSend(ctx, client, paired.Card, envelope)
				if err != nil {
					if firstFailure == nil {
						firstFailure = err
					}
					skipped = append(skipped, envelope.Recipient)
					continue
				}
				if err = c.callContext(ctx, "ax.network.accepted", object{"alias": alias, "fingerprint": networkFingerprint(paired.Card), "message_id": envelope.ID, "remote_message_id": remoteID}, nil); err != nil {
					return err
				}
			}
			batchFull = true
			return firstFailure
		}()
		if err != nil {
			failures = min(failures+1, 7)
			delay = min(time.Second*time.Duration(1<<uint(failures-1)), time.Minute)
			delay += time.Duration(rand.Int64N(int64(delay/4) + 1))
		} else {
			failures = 0
			delay = 30 * time.Second
			if batchFull {
				delay = 0
			}
		}
	}
}

func runNetworkWorkers(ctx context.Context, dir string, cert tls.Certificate) {
	runNetworkWorkersWithClients(ctx, dir, cert, networkHTTPClient)
}

func runNetworkWorkersWithClients(ctx context.Context, dir string, cert tls.Certificate, makeClient func(tls.Certificate, networkCard) (*http.Client, error)) {
	type worker struct {
		paired networkPeer
		wake   chan struct{}
		cancel context.CancelFunc
	}
	workers := map[string]worker{}
	var wg sync.WaitGroup
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		wg.Wait()
	}()
	var watcher *client
	defer func() {
		if watcher != nil {
			watcher.close()
		}
	}()
	refresh := func() {
		peers, err := networkPeers(dir)
		if err != nil {
			peers = map[string]networkPeer{}
		}
		for alias, w := range workers {
			p, exists := peers[alias]
			if !exists || !p.Messaging || string(raw(w.paired)) != string(raw(p)) {
				w.cancel()
				delete(workers, alias)
			}
		}
		for alias, p := range peers {
			if !p.Messaging {
				continue
			}
			if _, exists := workers[alias]; exists {
				continue
			}
			client, err := makeClient(cert, p.Card)
			if err != nil {
				continue
			}
			child, cancel := context.WithCancel(ctx)
			w := worker{paired: p, wake: make(chan struct{}, 1), cancel: cancel}
			workers[alias] = w
			wg.Add(1)
			go func() { defer wg.Done(); networkPeerWorker(child, dir, alias, p, client, w.wake) }()
		}
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if watcher == nil {
			c, err := dial(socketPath(dir))
			if err == nil {
				var features object
				callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				err = c.callContext(callCtx, "ax.ping", object{}, &features)
				if err == nil && features["network_mailbox"] == true {
					err = c.callContext(callCtx, "ax.network.watch", object{}, nil)
				} else {
					err = errors.New("broker update required")
				}
				cancel()
				if err == nil {
					watcher = c
				} else {
					c.close()
				}
			}
		}
		refresh()
		var changed <-chan struct{}
		var disconnected <-chan struct{}
		if watcher != nil {
			changed = watcher.peers
			disconnected = watcher.done
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
			if watcher != nil {
				pingCtx, cancel := context.WithTimeout(ctx, time.Second)
				err := watcher.callContext(pingCtx, "ax.ping", object{}, nil)
				cancel()
				if err != nil {
					watcher.close()
				}
			}
		case <-disconnected:
			watcher.close()
			watcher = nil
			// A dead socket cannot turn into a reconnect spin.
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case <-changed:
			for _, w := range workers {
				select {
				case w.wake <- struct{}{}:
				default:
				}
			}
		}
	}
}

func networkRunLock(dir string) (io.Closer, error) {
	return lockFile(filepath.Join(dir, "network", "run.lock"))
}

// The SDK decodes JSON responses; cap its input before decoding a paired peer's
// response, including chunked bodies whose size was not announced in headers.
type networkBoundedTransport struct{ base http.RoundTripper }

func (t networkBoundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if res.ContentLength > maxFrame {
		res.Body.Close()
		return nil, errors.New("network response exceeds frame limit")
	}
	res.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(res.Body, maxFrame+1), res.Body}
	return res, nil
}

//go:build darwin || linux

package ax

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func pairedNetworkBrokers(t *testing.T) (*broker, *broker) {
	t.Helper()
	a, da := localBroker(t)
	b, db := localBroker(t)
	ia, _ := testNetworkIdentity(t, da, "laptop", "100.64.0.1")
	ib, _ := testNetworkIdentity(t, db, "buildbox", "100.64.0.2")
	for _, pair := range []struct {
		dir, alias string
		card       networkCard
		exports    []string
	}{{da, "buildbox", ib.Card, []string{"web"}}, {db, "laptop", ia.Card, []string{"api"}}} {
		if err := trustNetworkPeer(pair.dir, pair.alias, pair.card, pair.exports); err != nil {
			t.Fatal(err)
		}
		if err := setNetworkMessaging(pair.dir, pair.alias, true); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

func TestNetworkMailboxRoundTripAndLostAcceptance(t *testing.T) {
	a, b := pairedNetworkBrokers(t)
	web, wa, _ := endpoint(t, a, "web", testMesh)
	api, ab, _ := endpoint(t, b, "api", testMesh)
	request(t, a, &serverConn{}, "ax.network.sync", object{"alias": "buildbox", "agents": []networkAgent{{ID: api.ID, Name: "api", Host: "codex", State: "ready"}}})
	outgoing := request(t, a, wa, "ax.send", object{"target": "api@buildbox", "text": "review this change", "client_message_id": "review"}).(Message)
	envelope := request(t, a, &serverConn{}, "ax.network.outbox", object{"alias": "buildbox"}).(*networkEnvelope)
	if envelope == nil || envelope.ID != outgoing.ID || envelope.SenderID != web.ID {
		t.Fatal("missing durable outgoing work")
	}
	first := request(t, b, &serverConn{}, "ax.network.receive", object{"alias": "laptop", "envelope": envelope}).(Message)
	if first.Expires != outgoing.Expires || !strings.Contains(guidanceFor(first), "paired machine") {
		t.Fatal("lost expiry or delegation boundary")
	}
	// Destination crashes after committing, before source learns the outcome.
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := openBroker(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.db.Close() })
	again := request(t, recovered, &serverConn{}, "ax.network.receive", object{"alias": "laptop", "envelope": envelope}).(Message)
	if again.ID != first.ID || again.Seq != first.Seq {
		t.Fatal("retry created duplicate native work")
	}
	changed := *envelope
	changed.Text = "different work"
	if _, err := recovered.networkReceive("laptop", changed); err == nil {
		t.Fatal("accepted conflicting retry")
	}
	if err := recovered.event(first.ID, "channel_written", 0); err != nil {
		t.Fatal(err)
	}
	// Restore the existing native connection's identity after broker restart.
	apiPeer := recovered.peers[api.ID]
	ab.epoch = apiPeer.epoch
	apiPeer.conn = ab
	apiPeer.seen = time.Now()
	reply := request(t, recovered, ab, "ax.reply", object{"message_id": first.ID, "text": "review complete", "client_message_id": "verdict"}).(Message)
	back, err := recovered.networkOutbox("laptop")
	if err != nil || back == nil || back.Parent != outgoing.ID || back.Recipient != web.ID {
		t.Fatalf("reply did not map to original request: %+v, %v", back, err)
	}
	returned := request(t, a, &serverConn{}, "ax.network.receive", object{"alias": "buildbox", "envelope": back}).(Message)
	if returned.Parent != outgoing.ID || returned.Recipient != web.ID || returned.Sender.Name != "api@buildbox" {
		t.Fatal("reply lost its session or thread")
	}
	original, _ := a.message(outgoing.ID)
	if original.RemoteMessageID != first.ID {
		t.Fatal("early reply lost the destination receipt")
	}
	request(t, a, &serverConn{}, "ax.network.accepted", object{"alias": "buildbox", "message_id": outgoing.ID, "remote_message_id": first.ID})
	if next, err := a.networkOutbox("buildbox"); err != nil || next != nil {
		t.Fatalf("reply-acknowledged work was replayed: %v", err)
	}
	if original.State != "acknowledged" || reply.ID != back.ID {
		t.Fatal("reply did not acknowledge the original request")
	}
	// Follow-up mapping needs the destination's durable local message ID.
	request(t, a, wa, "ax.follow_up", object{"message_id": outgoing.ID, "text": "also check this constraint", "client_message_id": "followup"})
	followup, err := a.networkOutbox("buildbox")
	if err != nil || followup == nil || followup.Parent != first.ID {
		t.Fatalf("follow-up lost destination parent: %+v, %v", followup, err)
	}
}

func TestNetworkMailboxScopesAndRevocation(t *testing.T) {
	a, b := pairedNetworkBrokers(t)
	web, wa, _ := endpoint(t, a, "web", testMesh)
	api, _, _ := endpoint(t, b, "api", testMesh)
	private, _, _ := endpoint(t, b, "private", testMesh)
	envelope := networkEnvelope{ID: "remote_1", SenderID: web.ID, SenderName: web.Name, SenderHost: web.Host, Recipient: api.ID, Text: "task", Expires: time.Now().Add(time.Minute).UnixMilli()}
	for _, method := range []string{"ax.network.sync", "ax.network.receive", "ax.network.outbox", "ax.network.accepted"} {
		if _, err := a.request(wa, method, raw(object{"alias": "buildbox", "envelope": envelope})); err == nil {
			t.Fatalf("model obtained network administration: %s", method)
		}
	}
	if err := setNetworkMessaging(b.dir, "laptop", false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.networkReceive("laptop", envelope); err == nil {
		t.Fatal("discovery-only pairing granted messaging")
	}
	if err := setNetworkMessaging(b.dir, "laptop", true); err != nil {
		t.Fatal(err)
	}
	outside := envelope
	outside.Recipient = private.ID
	if _, err := b.networkReceive("laptop", outside); err == nil {
		t.Fatal("message reached an unexported recipient")
	}
	expired := envelope
	expired.Expires = time.Now().Add(-time.Second).UnixMilli()
	if _, err := b.networkReceive("laptop", expired); err == nil {
		t.Fatal("accepted expired work")
	}
	m, err := b.networkReceive("laptop", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !b.networkHandoffAllowed(m, b.peers[api.ID]) {
		t.Fatal("authorized incoming task was blocked")
	}
	if err := revokeNetworkPeer(b.dir, "laptop"); err != nil {
		t.Fatal(err)
	}
	if b.networkHandoffAllowed(m, b.peers[api.ID]) {
		t.Fatal("revocation allowed a later native handoff")
	}
	if _, err := b.networkReceive("laptop", envelope); err == nil {
		t.Fatal("revoked peer could retrieve an idempotent result")
	}
}

func TestNetworkMailboxPreservesExternalProvenance(t *testing.T) {
	a, b := pairedNetworkBrokers(t)
	_, relay := relayEndpoint(t, a, "bridge")
	api, _, _ := endpoint(t, b, "api", testMesh)
	request(t, a, &serverConn{}, "ax.network.sync", object{"alias": "buildbox", "agents": []networkAgent{{ID: api.ID, Name: "api", Host: "codex", State: "ready"}}})
	provenance := json.RawMessage(`{"origin":"external","trust":"unverified","tainted":true}`)
	sent := request(t, a, relay, "ax.send", object{"target": "api@buildbox", "text": "quoted external content", "client_message_id": "external", "provenance": provenance}).(Message)
	envelope, err := a.networkOutbox("buildbox")
	if err != nil || envelope == nil || envelope.ID != sent.ID || string(envelope.Provenance) != string(provenance) {
		t.Fatalf("outbound relay lost provenance: %+v %v", envelope, err)
	}
	if _, err := b.networkReceive("laptop", *envelope); err == nil {
		t.Fatal("network grant bypassed recipient external policy")
	}
	b.peers[api.ID].External = "all"
	m, err := b.networkReceive("laptop", *envelope)
	if err != nil || string(m.Provenance) != string(provenance) || !strings.Contains(guidanceFor(m), "no delegation") {
		t.Fatalf("external content became a peer delegation: %+v %v", m, err)
	}
}

func TestNetworkRevocationBlocksNativeHandoffs(t *testing.T) {
	for _, delivery := range []string{"automatic", "manual"} {
		for _, change := range []string{"disable", "revoke", "scope"} {
			t.Run(delivery+"/"+change, func(t *testing.T) {
				a, b := pairedNetworkBrokers(t)
				web, _, _ := endpoint(t, a, "web", testMesh)
				api, _, _ := endpoint(t, b, "api", testMesh)
				envelope := networkEnvelope{ID: "held", SenderID: web.ID, SenderName: web.Name, SenderHost: web.Host, Recipient: api.ID, Text: "held task", Expires: time.Now().Add(time.Minute).UnixMilli()}
				m, err := b.networkReceive("laptop", envelope)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "disable":
					err = setNetworkMessaging(b.dir, "laptop", false)
				case "revoke":
					err = revokeNetworkPeer(b.dir, "laptop")
				case "scope":
					peers, e := networkPeers(b.dir)
					if e != nil {
						t.Fatal(e)
					}
					err = trustNetworkPeer(b.dir, "laptop", peers["laptop"].Card, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if delivery == "manual" {
					b.peers[api.ID].DeliveryMode = "manual"
					result, err := b.checkInbox(b.peers[api.ID])
					if err != nil {
						t.Fatal(err)
					}
					if result.(object)["message"] != nil {
						t.Fatal("revocation allowed manual content delivery")
					}
				} else {
					b.dispatch()
				}
				saved, err := b.message(m.ID)
				if err != nil || saved.State != "queued" {
					t.Fatalf("revocation allowed native handoff: %+v %v", saved, err)
				}
			})
		}
	}
}

func TestNetworkCacheReservesLocalSlotsAndRetainsHistory(t *testing.T) {
	_, b := pairedNetworkBrokers(t)
	paired, err := b.networkPeer("laptop")
	if err != nil {
		t.Fatal(err)
	}
	local, _, _ := endpoint(t, b, "api", testMesh)
	var first *peer
	for i := 0; i < 64; i++ {
		p, err := b.networkProxy("laptop", paired, fmt.Sprintf("remote_%d", i), fmt.Sprintf("agent%d", i), "codex")
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = p
		}
	}
	if _, err := b.networkProxy("laptop", paired, "overflow", "overflow", "codex"); err == nil {
		t.Fatal("remote cache ignored its limit")
	}
	// Remote saturation still permits local identities.
	endpoint(t, b, "another-local", testMesh)
	b.reapNetworkPeers(time.Now().Add(2 * time.Minute))
	if b.peers[local.ID] == nil || b.peers[first.ID] != nil {
		t.Fatal("remote eviction changed local state or retained expired cache")
	}
	saved, err := storedPeer(b.db, first.ID)
	if err != nil || saved.Remote == nil {
		t.Fatal("eviction destroyed durable identity")
	}
	p, err := b.networkProxy("laptop", paired, "remote_0", "agent0", "codex")
	if err != nil || p.ID != first.ID {
		t.Fatal("rehydration replaced durable identity")
	}
}

func TestNetworkRevocationWithholdsOfferedHistory(t *testing.T) {
	for _, change := range []string{"disable", "revoke", "scope"} {
		t.Run(change, func(t *testing.T) {
			a, b := pairedNetworkBrokers(t)
			web, _, _ := endpoint(t, a, "web", testMesh)
			api, conn, _ := endpoint(t, b, "api", testMesh)
			m, err := b.networkReceive("laptop", networkEnvelope{ID: "offered", SenderID: web.ID, SenderName: web.Name, SenderHost: web.Host, Recipient: api.ID, Text: "private task", Data: json.RawMessage(`{"private":true}`), Expires: time.Now().Add(time.Minute).UnixMilli()})
			if err != nil {
				t.Fatal(err)
			}
			if err = b.event(m.ID, "channel_written", 0); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "disable":
				err = setNetworkMessaging(b.dir, "laptop", false)
			case "revoke":
				err = revokeNetworkPeer(b.dir, "laptop")
			case "scope":
				peers, e := networkPeers(b.dir)
				if e != nil {
					t.Fatal(e)
				}
				err = trustNetworkPeer(b.dir, "laptop", peers["laptop"].Card, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			thread, err := b.thread(b.peers[api.ID], m.ID, "")
			if err != nil || len(thread.Messages) != 1 || !thread.Messages[0].Withheld || thread.Messages[0].Text != "" || len(thread.Messages[0].Data) != 0 || len(thread.Messages[0].Provenance) != 0 {
				t.Fatalf("revoked thread exposed content: %+v %v", thread, err)
			}
			pending, err := b.pending(b.peers[api.ID], 0)
			if err != nil || len(pending.Messages) != 1 || pending.Messages[0].Preview != "" {
				t.Fatalf("revoked preview exposed content: %+v %v", pending, err)
			}
			for _, method := range []string{"ax.get_message", "ax.status"} {
				if _, err = b.request(conn, method, raw(object{"message_id": m.ID})); err == nil {
					t.Fatalf("revoked content accessible through %s", method)
				}
			}
		})
	}
}

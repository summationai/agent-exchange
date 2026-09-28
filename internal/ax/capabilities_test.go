package ax

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDeliveryCapabilitiesConformance(t *testing.T) {
	for _, host := range []string{"claude", "codex", "grok", "opencode", "pi"} {
		t.Run(host, func(t *testing.T) {
			b, _ := localBroker(t)
			s, to, wire := endpoint(t, b, "api", testMesh)
			_, from, _ := endpoint(t, b, "web", testMesh)
			p := b.peers[to.agent]
			p.Host = host
			caps, err := sessionCapabilities(Session{Host: host})
			if err != nil {
				t.Fatal(err)
			}
			request(t, b, to, "ax.capabilities", object{"delivery_capabilities": caps})
			if agentSnapshot(p).Capabilities.Content != caps.Content {
				t.Fatal("contract not visible")
			}
			// Readiness never overrides permission checks or an uncertain handoff.
			p.Permission = "unknown"
			m := request(t, b, from, "ax.send", object{"target": "api", "text": "do work", "client_message_id": "caps"}).(Message)
			b.dispatch()
			assertMessageState(t, b, m.ID, "queued")
			p.Permission, p.State = "default", "busy"
			if caps.Boundary == "idle" {
				b.dispatch()
				assertMessageState(t, b, m.ID, "queued")
				if !strings.Contains(queueReason(p, b.peers[from.agent]), "idle boundary") {
					t.Fatal("missing boundary reason")
				}
				request(t, b, &serverConn{}, "ax.lifecycle", object{"agent_id": s.ID, "secret": s.Secret, "native_session_id": s.Native, "state": "ready"})
			}
			offer := make(chan packet, 1)
			go func() { packet, _ := readFrame(wire); offer <- packet }()
			b.dispatch()
			if event := <-offer; event.Method != "ax.delivery.offer" {
				t.Fatal(event)
			}
			assertMessageState(t, b, m.ID, "handoff_started")
			request(t, b, to, "ax.receipt", object{"message_id": m.ID, "receipt": "delivery_uncertain"})
			for range 3 {
				request(t, b, to, "ax.capabilities", object{"delivery_capabilities": caps})
				b.dispatch()
			}
			assertMessageState(t, b, m.ID, "delivery_uncertain")
			// A second conversation retains its independent binding and legacy mode.
			if b.peers[from.agent].capabilities != nil || b.peers[from.agent].Native == p.Native {
				t.Fatal("contract leaked across conversations")
			}
			b.disconnect(to)
			x, y := net.Pipe()
			defer x.Close()
			defer y.Close()
			reconnected := &serverConn{Conn: x}
			request(t, b, reconnected, "ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret})
			if p.capabilities != nil || p.ready {
				t.Fatal("old bridge inherited stale capabilities or readiness")
			}
		})
	}
}

func assertMessageState(t *testing.T, b *broker, id, want string) {
	t.Helper()
	m, err := b.message(id)
	if err != nil || m.State != want {
		t.Fatalf("want %s, got %s: %v", want, m.State, err)
	}
}

func TestCapabilityValidationAndIdleFIFO(t *testing.T) {
	b, _ := localBroker(t)
	_, to, wire := endpoint(t, b, "api", testMesh)
	_, from, _ := endpoint(t, b, "web", testMesh)
	p := b.peers[to.agent]
	p.Host = "claude"
	caps, _ := sessionCapabilities(Session{Host: "claude", DeliveryBoundary: "idle"})
	for _, mutate := range []func(*deliveryCapabilities){
		func(c *deliveryCapabilities) { c.Version = 2 },
		func(c *deliveryCapabilities) { c.Boundary = "interrupt" },
		func(c *deliveryCapabilities) { c.Content = "user_prompt" },
		func(c *deliveryCapabilities) { c.Lifecycle = append(c.Lifecycle, "task_completed") },
	} {
		var bad deliveryCapabilities
		json.Unmarshal(raw(caps), &bad)
		mutate(&bad)
		if _, err := b.request(to, "ax.capabilities", raw(object{"delivery_capabilities": bad})); err == nil {
			t.Fatal("accepted unsupported capability", bad)
		}
	}
	if _, err := sessionCapabilities(Session{Host: "codex", DeliveryBoundary: "idle"}); err == nil {
		t.Fatal("claimed unobserved Codex idle boundary")
	}
	request(t, b, to, "ax.capabilities", object{"delivery_capabilities": caps})
	p.State = "busy"
	first := request(t, b, from, "ax.send", object{"target": "api", "text": "expired", "client_message_id": "first"}).(Message)
	second := request(t, b, from, "ax.send", object{"target": "api", "text": "second", "client_message_id": "second"}).(Message)
	if _, err := b.db.Exec("UPDATE messages SET expires=? WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), first.ID); err != nil {
		t.Fatal(err)
	}
	b.dispatch()
	assertMessageState(t, b, first.ID, "expired")
	assertMessageState(t, b, second.ID, "queued")
	p.State = "ready"
	go readFrame(wire)
	b.dispatch()
	assertMessageState(t, b, second.ID, "handoff_started")
}

func TestIdleDeferralIsAttemptScopedAndNeverReplaysAmbiguity(t *testing.T) {
	b, _ := localBroker(t)
	_, to, wire := endpoint(t, b, "api", testMesh)
	_, from, _ := endpoint(t, b, "web", testMesh)
	p := b.peers[to.agent]
	p.Host = "opencode"
	caps, _ := sessionCapabilities(Session{Host: "opencode"})
	request(t, b, to, "ax.capabilities", object{"delivery_capabilities": caps})
	m := request(t, b, from, "ax.send", object{"target": "api", "text": "work", "client_message_id": "deferral"}).(Message)
	// Consume failure notices too, so an uncertain delivery cannot block the fixture.
	offers := make(chan Message, 8)
	go func() {
		for {
			packet, err := readFrame(wire)
			if err != nil {
				return
			}
			if packet.Method == "ax.delivery.offer" {
				var offer Message
				json.Unmarshal(packet.Params, &offer)
				offers <- offer
			}
		}
	}()
	b.dispatch()
	first := <-offers
	p.State = "busy"
	args := object{"message_id": m.ID, "receipt": "deferred_idle", "handoff_id": first.HandoffID}
	// Idle can arrive before the HTTP receipt. A deferral must not overwrite it.
	p.State = "ready"
	for range 2 {
		request(t, b, to, "ax.receipt", args)
	}
	if p.State != "ready" {
		t.Fatal("late deferral overwrote native idle")
	}
	p.State = "busy"
	b.dispatch()
	assertMessageState(t, b, m.ID, "queued")
	var deferrals int
	b.db.QueryRow("SELECT count(*) FROM events WHERE message=? AND state='deferred_idle'", m.ID).Scan(&deferrals)
	if deferrals != 1 {
		t.Fatal("duplicate deferral wrote another event", deferrals)
	}
	p.State = "ready"
	b.dispatch()
	second := <-offers
	if first.HandoffID == second.HandoffID || second.ID != first.ID {
		t.Fatal("attempt identity not renewed")
	}
	p.State = "busy"
	if _, err := b.request(to, "ax.receipt", raw(args)); err == nil {
		t.Fatal("stale deferral rolled back another attempt")
	}
	assertMessageState(t, b, m.ID, "handoff_started")
	request(t, b, to, "ax.receipt", object{"message_id": m.ID, "receipt": "delivery_uncertain"})
	args["handoff_id"] = second.HandoffID
	if _, err := b.request(to, "ax.receipt", raw(args)); err == nil {
		t.Fatal("uncertain delivery replayed")
	}
	b.dispatch()
	assertMessageState(t, b, m.ID, "delivery_uncertain")
}

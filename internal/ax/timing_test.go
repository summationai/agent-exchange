package ax

import (
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestTimingPreservesHistoricalContext(t *testing.T) {
	b, dir := localBroker(t)
	_, from, _ := endpoint(t, b, "web", testMesh)
	_, to, _ := endpoint(t, b, "api", testMesh)
	for _, state := range []string{"ready", "busy", "starting", "offline", "permission-blocked"} {
		p := b.peers[to.agent]
		p.State, p.Permission, p.conn = state, "default", to
		if state == "offline" {
			p.conn = nil
		}
		if state == "permission-blocked" {
			p.State, p.Permission = "ready", "unknown"
		}
		args := object{"target": "api", "text": state, "client_message_id": state}
		m := request(t, b, from, "ax.send", args).(Message)
		if m.QueuedContext.RecipientState != state {
			t.Fatalf("wrong snapshot: %+v", m.QueuedContext)
		}
		p.State, p.Permission, p.conn = "ready", "default", to
		again := request(t, b, from, "ax.send", args).(Message)
		if again.ID != m.ID || *again.QueuedContext != *m.QueuedContext || again.Receipt.Recipient.State != "ready" {
			t.Fatal("retry changed historical context")
		}
	}
	// Context survives a broker restart and old rows remain explicitly unknown.
	b.db.Close()
	reopened, err := openBroker(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var data string
	if err := reopened.db.QueryRow("SELECT data FROM messages WHERE client_id='busy'").Scan(&data); err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := json.Unmarshal([]byte(data), &m); err != nil || m.QueuedContext.RecipientState != "busy" {
		t.Fatal("lost stored context", err)
	}
	legacy := messageTiming(Message{}, nil)
	if rawContext := string(raw(legacy["queued_context"])); rawContext != "null" {
		t.Fatal("invented legacy context", rawContext)
	}
}

func TestTimingStagesAndMissingEvidence(t *testing.T) {
	events := []object{}
	for i, name := range []string{"queued", "handoff_started", "wake_accepted", "content_served", "replied"} {
		events = append(events, object{"event": name, "at_ms": int64(1000 + i*10)})
	}
	timing := messageTiming(Message{}, events)
	stages := timing["stages_at_ms"].(map[string]*int64)
	elapsed := timing["elapsed_ms"].(object)
	if stages["recipient_turn_started"] != nil || stages["channel_written"] != nil || *stages["acknowledged"] != 1040 {
		t.Fatal("invented evidence", timing)
	}
	if *elapsed["queue_to_handoff"].(*int64) != 10 || *elapsed["queue_to_acknowledgment"].(*int64) != 40 {
		t.Fatal("wrong durations", elapsed)
	}
	// Receipt RPCs can arrive late, and wall clocks can move backwards.
	events[2]["at_ms"] = int64(1100)
	timing = messageTiming(Message{}, events)
	if timing["elapsed_ms"].(object)["native_receipt_to_content_fetch"].(*int64) != nil {
		t.Fatal("negative duration reported")
	}
}

func TestLateNativeReceiptsRetainTimingWithoutRegressingState(t *testing.T) {
	for _, terminal := range []string{"content_served", "acknowledged"} {
		for _, receipt := range []string{"wake_accepted", "channel_written"} {
			t.Run(terminal+"/"+receipt, func(t *testing.T) {
				b, _ := localBroker(t)
				_, from, _ := endpoint(t, b, "web", testMesh)
				_, to, _ := endpoint(t, b, "api", testMesh)
				m := request(t, b, from, "ax.send", object{"target": "api", "text": "request", "client_message_id": "late"}).(Message)
				if err := b.event(m.ID, "handoff_started", to.epoch); err != nil {
					t.Fatal(err)
				}
				request(t, b, to, "ax.get_message", object{"message_id": m.ID})
				if terminal == "acknowledged" {
					request(t, b, to, "ax.ack", object{"message_id": m.ID})
				}
				for range 3 {
					request(t, b, to, "ax.receipt", object{"message_id": m.ID, "receipt": receipt})
				}
				stored, err := b.message(m.ID)
				if err != nil || stored.State != terminal {
					t.Fatal("late receipt regressed state", stored, err)
				}
				var count int
				if err := b.db.QueryRow("SELECT count(*) FROM events WHERE message=? AND state=?", m.ID, receipt).Scan(&count); err != nil || count != 1 {
					t.Fatal("unbounded receipt history", count, err)
				}
				status := request(t, b, from, "ax.status", object{"message_id": m.ID}).(object)
				if status["timing"].(object)["stages_at_ms"].(map[string]*int64)[receipt] == nil || status["task_completion"] != "unknown" {
					t.Fatal("missing timing or false completion", status)
				}
			})
		}
	}
}

// Unix-socket transport and durable broker storage; the recipient is a fixture,
// not a model. Cleanup and helper CPU measurements include fixture work.
func BenchmarkDeliveryTiming(b *testing.B) {
	dir := startTestServer(b)
	fixture, err := openBroker(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer fixture.db.Close()
	peers := make([]*client, 2)
	for i, name := range []string{"web", "api"} {
		peers[i] = connectDiscoveryPeer(b, dir, Session{ID: randomID("agt_"), Secret: randomID(""), Host: "codex", Name: name, Mesh: testMesh, Native: uuid()})
	}
	samples := make([]int64, 0, min(b.N, 1000))
	cpuStart, err := processCPU()
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("AX %s; %s; %s/%s; fake recipient, no inference", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		var m Message
		if err := peers[0].call("ax.send", object{"target": "api", "text": fmt.Sprint(i), "client_message_id": fmt.Sprint(i)}, &m); err != nil {
			b.Fatal(err)
		}
		select {
		case offer := <-peers[1].offers:
			if offer.ID != m.ID {
				b.Fatal("wrong message")
			}
		case <-time.After(time.Second):
			b.Fatal("offer timeout")
		}
		if len(samples) < cap(samples) {
			samples = append(samples, time.Since(start).Nanoseconds())
		}
		for _, step := range []struct {
			method string
			args   object
		}{
			{"ax.receipt", object{"message_id": m.ID, "receipt": "wake_accepted"}},
			{"ax.get_message", object{"message_id": m.ID}}, {"ax.ack", object{"message_id": m.ID}},
		} {
			if err := peers[1].call(step.method, step.args, nil); err != nil {
				b.Fatal(err)
			}
		}
		// Limit retained fixture mail, including rate accounting, in this isolated DB.
		if _, err := fixture.db.Exec("DELETE FROM messages WHERE id=?", m.ID); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	cpuEnd, err := processCPU()
	if err != nil {
		b.Fatal(err)
	}
	slices.Sort(samples)
	b.ReportMetric(float64(len(samples)), "samples")
	b.ReportMetric(float64(samples[(len(samples)-1)/2]), "offer-p50-ns")
	b.ReportMetric(float64(samples[(95*len(samples)+99)/100-1]), "offer-p95-ns")
	b.ReportMetric(float64(cpuEnd-cpuStart)/float64(b.N), "fixture-cpu-ns/op")
}

package ax

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestDepthRecoveryKeepsHandoffAndAcknowledgmentSeparate(t *testing.T) {
	for _, method := range []string{"ax.reply", "ax.follow_up"} {
		t.Run(method, func(t *testing.T) {
			b, _ := localBroker(t)
			_, from, _ := endpoint(t, b, "web", testMesh)
			_, to, _ := endpoint(t, b, "api", testMesh)
			first := request(t, b, from, "ax.send", object{"target": "api", "text": "review", "client_message_id": "first"}).(Message)
			parent := first
			for depth := 1; depth <= 8; depth++ {
				parent = followup(t, b, from, parent.ID, fmt.Sprint(depth), fmt.Sprint(depth))
			}
			if err := b.event(parent.ID, "content_served", to.epoch); err != nil {
				t.Fatal(err)
			}
			caller, recipient := from, to
			if method == "ax.reply" {
				caller, recipient = to, from
			}
			var before, after int
			if err := b.db.QueryRow("SELECT count(*) FROM events").Scan(&before); err != nil {
				t.Fatal(err)
			}
			_, err := b.request(caller, method, raw(object{"message_id": parent.ID, "text": "review ready", "client_message_id": "blocked"}))
			if err == nil {
				t.Fatal("depth bound bypassed")
			}
			failure := toolFailure(&requestFailure{cause: err, submitted: true}, object{"client_message_id": "blocked"})
			if failure["code"] != "reply_depth_limit" || failure["submission"] != "not_submitted" || failure["retryable"] != false || failure["target"] != recipient.agent || failure["previous_message_id"] != parent.ID || failure["thread_id"] != first.ID {
				t.Fatalf("wrong continuation: %v", failure)
			}
			if err := b.db.QueryRow("SELECT count(*) FROM events").Scan(&after); err != nil || after != before {
				t.Fatalf("rejected handoff changed events: %d -> %d, %v", before, after, err)
			}
			// Follow the returned recovery using the known handoff context. A new
			// thread still honors recipient policy and keeps retries idempotent.
			args := object{"target": failure["target"], "text": "Review ready. Continuing " + parent.ID, "client_message_id": "continuation"}
			b.peers[recipient.agent].Policy = "refuse"
			if _, err := b.request(caller, "ax.send", raw(args)); err == nil {
				t.Fatal("continuation bypassed recipient policy")
			}
			b.peers[recipient.agent].Policy = "accept"
			next := request(t, b, caller, "ax.send", args).(Message)
			retry := request(t, b, caller, "ax.send", args).(Message)
			if next.ID != retry.ID || next.Recipient != recipient.agent || next.Depth != 0 || next.Thread != next.ID || next.Parent != "" {
				t.Fatalf("wrong new thread or duplicate: %+v %+v", next, retry)
			}
			saved, err := b.message(parent.ID)
			if err != nil || saved.State != "content_served" {
				t.Fatalf("send acknowledged parent: %+v %v", saved, err)
			}
			if method == "ax.reply" {
				request(t, b, caller, "ax.ack", object{"message_id": parent.ID})
				saved, err = b.message(parent.ID)
				if err != nil || saved.State != "acknowledged" {
					t.Fatalf("receipt acknowledgment failed: %+v %v", saved, err)
				}
			}
		})
	}
}

func TestDepthRecoverySurvivesBrokerWireAndBridge(t *testing.T) {
	dir := startTestServer(t)
	c, err := dial(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	call := func(method string, args, result any) {
		t.Helper()
		if err := c.call(method, args, result); err != nil {
			t.Fatal(err)
		}
	}
	s := Session{ID: randomID("agt_"), Secret: randomID("") + randomID(""), Name: "web", Host: "claude", Mesh: testMesh, Native: uuid(), Started: true}
	call("ax.enroll", s, nil)
	target := s
	target.ID, target.Name, target.Native = randomID("agt_"), "api", uuid()
	call("ax.enroll", target, nil)
	call("ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret}, nil)
	file := filepath.Join(dir, "session.json")
	if err := saveSession(file, s); err != nil {
		t.Fatal(err)
	}
	var m Message
	call("ax.send", object{"target": "api", "text": "review", "client_message_id": "first"}, &m)
	for depth := 1; depth <= 8; depth++ {
		call("ax.follow_up", object{"message_id": m.ID, "text": fmt.Sprint(depth), "client_message_id": fmt.Sprint(depth)}, &m)
	}
	bridge := &bridge{ctx: context.Background(), dir: dir, file: file, session: s, c: c}
	args := object{"message_id": m.ID, "text": "handoff", "client_message_id": "blocked"}
	err = bridge.toolCall(context.Background(), nil, "ax.follow_up", args, nil)
	if err == nil {
		t.Fatal("depth bound bypassed over wire")
	}
	failure := toolFailure(err, args)
	if failure["code"] != "reply_depth_limit" || failure["submission"] != "not_submitted" || failure["target"] != target.ID || failure["previous_message_id"] != m.ID || failure["client_message_id"] != "blocked" {
		t.Fatalf("lost recovery over wire: %v", failure)
	}
	if !strings.Contains(failure["recovery"].(string), "standing instruction") {
		t.Fatalf("lost recovery guidance: %v", failure)
	}
	// Other broker failures remain conservative about commit uncertainty.
	generic := toolFailure(&requestFailure{cause: &rpcError{Code: -32000, Message: "commit failed"}, submitted: true}, args)
	if generic["submission"] != "outcome_unknown" {
		t.Fatalf("unrelated failure was treated as safely unsent: %v", generic)
	}
}

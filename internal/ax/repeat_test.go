package ax

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestStructuredRepeatSuppression(t *testing.T) {
	for _, method := range []string{"ax.send", "ax.reply", "ax.follow_up"} {
		for _, field := range []string{"data", "provenance"} {
			t.Run(method+"/"+field, func(t *testing.T) {
				b, _ := localBroker(t)
				relay, from := relayEndpoint(t, b, "bridge")
				target, to, _ := endpoint(t, b, "target", testMesh)
				openExternal(t, b, target, "all")
				args := object{
					"target": target.ID, "text": "Review requested", "client_message_id": "first",
					"data":       json.RawMessage(`{ "pr": 100, "label": "<review>" }`),
					"provenance": object{"origin": "amx", "trust": "verified", "tainted": false, "event_id": "one"},
				}
				if method == "ax.reply" {
					parent := request(t, b, to, "ax.send", object{"target": relay.ID, "text": "parent", "client_message_id": "parent"}).(Message)
					if err := b.event(parent.ID, "content_served", from.epoch); err != nil {
						t.Fatal(err)
					}
					args["message_id"] = parent.ID
				} else if method == "ax.follow_up" {
					parentArgs := object{"target": target.ID, "text": "parent", "client_message_id": "parent", "provenance": args["provenance"]}
					args["message_id"] = request(t, b, from, "ax.send", parentArgs).(Message).ID
				}
				first := request(t, b, from, method, args).(Message)
				if retry := request(t, b, from, method, args).(Message); retry.ID != first.ID {
					t.Fatal("identical retry stored another message")
				}
				if field == "data" {
					args[field] = object{"pr": 101, "label": "<review>"}
				} else {
					args[field] = object{"origin": "amx", "trust": "verified", "tainted": false, "event_id": "two"}
				}
				if _, err := b.request(from, method, raw(args)); err == nil || !strings.Contains(err.Error(), "different content") {
					t.Fatalf("changed content reused an idempotency key: %v", err)
				}
				args["client_message_id"] = "second"
				second := request(t, b, from, method, args).(Message)
				if second.ID == first.ID || second.State != "queued" {
					t.Fatalf("distinct structured event not queued: %+v", second)
				}
				stored, err := b.message(second.ID)
				if err != nil || string(stored.Data) != string(raw(args["data"])) || string(stored.Provenance) != string(raw(args["provenance"])) {
					t.Fatalf("stored structured content differs: %+v %v", stored, err)
				}
				if retry := request(t, b, from, method, args).(Message); retry.ID != second.ID {
					t.Fatal("second event retry stored another message")
				}
				args["client_message_id"] = "third"
				if _, err := b.request(from, method, raw(args)); err == nil || !strings.Contains(err.Error(), "repeated message suppressed") {
					t.Fatalf("identical structured content bypassed repeat guard: %v", err)
				}
				var count int
				if err := b.db.QueryRow("SELECT count(*) FROM messages WHERE sender=? AND json_extract(data,'$.text')=?", relay.ID, args["text"]).Scan(&count); err != nil || count != 2 {
					t.Fatalf("expected exactly two events: count=%d err=%v", count, err)
				}
			})
		}
	}
}

func TestPeerRepeatSuppressionAndStructuredRateLimit(t *testing.T) {
	b, _ := localBroker(t)
	_, from, _ := endpoint(t, b, "sender", testMesh)
	target, _, _ := endpoint(t, b, "target", testMesh)
	args := object{"target": target.ID, "text": "Review requested", "client_message_id": "first"}
	first := request(t, b, from, "ax.send", args).(Message)
	if retry := request(t, b, from, "ax.send", args).(Message); retry.ID != first.ID {
		t.Fatal("plain retry stored another message")
	}
	args["client_message_id"] = "repeat"
	if _, err := b.request(from, "ax.send", raw(args)); err == nil || !strings.Contains(err.Error(), "repeated message suppressed") {
		t.Fatalf("plain repeat guard changed: %v", err)
	}
	// An explicitly present empty object is distinct from omitted data.
	args["data"] = object{}
	request(t, b, from, "ax.send", args)
	args["client_message_id"] = "empty-repeat"
	if _, err := b.request(from, "ax.send", raw(args)); err == nil || !strings.Contains(err.Error(), "repeated message suppressed") {
		t.Fatalf("empty structured repeat bypassed guard: %v", err)
	}
	for i := 2; i < 30; i++ {
		args["client_message_id"], args["data"] = fmt.Sprint(i), object{"pr": i}
		request(t, b, from, "ax.send", args)
	}
	args["client_message_id"], args["data"] = "over-limit", object{"pr": 30}
	if _, err := b.request(from, "ax.send", raw(args)); err == nil || !strings.Contains(err.Error(), "send rate limit reached") {
		t.Fatalf("distinct data bypassed rate limit: %v", err)
	}
	var count int
	if err := b.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 30 {
		t.Fatalf("rate limit stored extra mail: count=%d err=%v", count, err)
	}
}

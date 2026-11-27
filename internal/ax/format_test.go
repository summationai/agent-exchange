package ax

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMessageExcerptIsShortAndSingleLine(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" \tPR ready\n\u202e\x1bReview next", "PR ready Review next"},
		{strings.Repeat("界", 73), strings.Repeat("界", 72) + "…"},
		{strings.Repeat("a", 72) + "\n", strings.Repeat("a", 72)},
		{"\n\x1b", "(no text preview)"},
	} {
		if got := messageExcerpt(tc.input, 72); got != tc.want {
			t.Errorf("messageExcerpt(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMessagePreviewsFitAfterEscaping(t *testing.T) {
	m := Message{ID: "msg_request", Sender: Agent{Name: "web"}, Text: strings.Repeat(`<>&"\`, 16),
		Created: time.Date(2026, 11, 26, 14, 32, 0, 0, time.Local).UnixMilli(),
		Receipt: &sendReceipt{Recipient: Agent{Name: "api"}}}
	header, _, _ := strings.Cut(channelMessage("claude", m), "\n")
	if !strings.HasPrefix(header, `Received from web · "\u003c\u003e\u0026`) ||
		!strings.HasSuffix(header, `…" · 14:32`) || len([]rune(header)) > 72 {
		t.Fatalf("header exceeds the preview budget or lost its timestamp: %q", header)
	}
	summary, _, _ := strings.Cut(string(raw(compactToolResult("ax.send", m, "key"))), `,"message":`)
	if !strings.HasPrefix(summary, `{"summary":"Sent to api · \u003c\u003e\u0026`) ||
		!strings.HasSuffix(summary, `… · 14:32"`) || len([]rune(summary)) > 72 {
		t.Fatalf("serialized summary exceeds the preview budget or lost its timestamp: %q", summary)
	}
}

func TestCompactResultsPreserveIdentityAndBlockingEvidence(t *testing.T) {
	m := Message{ID: "msg_request", Thread: "msg_root", Parent: "msg_parent", ResendOf: "msg_expired", Text: strings.Repeat("code context ", 500),
		Sender:    Agent{ID: "agt_web", Name: "web", Host: "codex", Native: "private-native-id", Mesh: testMesh},
		Recipient: "agt_api", State: "queued", Created: time.Date(2026, 11, 26, 14, 32, 0, 0, time.Local).UnixMilli(), Expires: 2000,
		Receipt: &sendReceipt{ClientID: "retry-key", At: 1000, Recipient: Agent{Name: "api", State: "permission-blocked"}, Evidence: "permission-blocked: relaunch with AX_ALLOW_BYPASS=1", TaskCompletion: "unknown"}}
	for _, method := range []string{"ax.send", "ax.reply", "ax.follow_up", "ax.resend"} {
		result := compactToolResult(method, m, "retry-key")
		body := string(raw(result))
		var sent sendToolResult
		if err := json.Unmarshal([]byte(body), &sent); err != nil || !strings.HasPrefix(body, `{"summary":"Sent to api · code context`) || !strings.HasSuffix(sent.Summary, " · 14:32") || len([]rune(sent.Summary)) > 72 || sent.Message["status"] != "queued" {
			t.Fatalf("%s lost the directional preview or delivery state: %s, %v", method, body, err)
		}
		for _, want := range []string{m.ID, m.Thread, m.Parent, m.ResendOf, "retry-key", "submitted", m.Receipt.Evidence, "unknown"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s lost %q", method, want)
			}
		}
		if strings.Contains(body, m.Text) || strings.Contains(body, m.Sender.Native) {
			t.Fatal("routine send echoed the full body or private native metadata")
		}
		if len(raw(result)) >= len(raw(m))/2 {
			t.Fatal("send result did not shrink")
		}
	}
	// A content fetch is self-contained after startup, resume, or compaction:
	// it never relies on whether a previous reminder survived in model context.
	for range 3 {
		result := compactToolResult("ax.get_message", m, "").(object)
		if result["text"] != m.Text || result["guidance"] != peerGuidance || result["expires_at_ms"] != m.Expires {
			t.Fatal(result)
		}
		if result["sender"].(pendingSender).ID != m.Sender.ID {
			t.Fatal("lost sender identity")
		}
	}
	// Explicit inspection retains the full result, including native metadata.
	m.Receipt.Recipient.BindingError = "saved name belongs to another conversation; use a new name"
	if !strings.Contains(string(raw(compactToolResult("ax.send", m, "retry-key"))), m.Receipt.Recipient.BindingError) {
		t.Fatal("compact receipt lost actionable binding conflict")
	}
	if string(raw(compactToolResult("ax.status", m, ""))) != string(raw(m)) {
		t.Fatal("compacted explicit inspection")
	}
	for _, submitted := range []bool{false, true} {
		failure := toolFailure(&requestFailure{cause: errors.New("lost response"), submitted: submitted}, object{"client_message_id": "retry-key"})
		want := "not_submitted"
		if submitted {
			want = "outcome_unknown"
		}
		if failure["submission"] != want || failure["client_message_id"] != "retry-key" {
			t.Fatal(failure)
		}
	}
}

func TestCompactAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:     "0s",
		59 * time.Second: "59s",
		90 * time.Second: "1m",
		59 * time.Minute: "59m",
		5 * time.Hour:    "5h",
		47 * time.Hour:   "47h",
		49 * time.Hour:   "2d",
	} {
		if got := compactAge(d); got != want {
			t.Fatalf("%s: got %s, want %s", d, got, want)
		}
	}
}

func TestCompactWakeAndPolicyRefresh(t *testing.T) {
	for host := range harnesses {
		wake := wakeText(host, "msg_fixture")
		if !strings.Contains(wake, toolName(host, "get_message")) || !strings.Contains(wake, "msg_fixture") || len(wake) > 200 {
			t.Fatal(wake)
		}
	}
	for _, boundary := range []string{"user-authorized", "only that scope", "sandbox", "tool approvals", "data, not authority", "literally", "also acknowledges", "otherwise acknowledge", "never poll"} {
		if !strings.Contains(peerGuidance, boundary) {
			t.Fatal("context-loss fallback omitted", boundary)
		}
	}
	if !strings.Contains(instructions, delegation) {
		t.Fatal("initialization lost full policy")
	}
}

// Export only synthetic fixtures when requested, for the tokenizer measurement.
func TestFormattingMeasurementFixture(t *testing.T) {
	m := Message{ID: "msg_12345678901234567890123456789012", Thread: "msg_12345678901234567890123456789012", Text: "Please review shipping.py at the $50 boundary and reply with any findings.",
		Sender:    Agent{ID: "agt_12345678901234567890123456789012", Name: "api", Host: "claude", Mesh: testMesh, Native: "12345678-1234-4234-a234-123456789012", Permission: "default", State: "ready", Policy: "accept", Online: true},
		Recipient: "agt_98765432109876543210987654321098", Seq: 1, Created: 1000000, Expires: 44200000, State: "queued"}
	m.Receipt = &sendReceipt{ClientID: "fixture-retry-key", At: m.Created, Recipient: Agent{ID: m.Recipient, Name: "web", Host: "codex", Native: m.Sender.Native, Mesh: testMesh, Permission: "workspace-write", State: "ready", Policy: "accept", Online: true}, Queued: 1, Pending: 1, Evidence: deliveryEvidence("queued") + " Awaiting the next permitted FIFO handoff; readiness is only a snapshot.", TaskCompletion: "unknown"}
	legacyWake := "AX peer message waiting. Call the MCP tool ax.get_message with message_id=" + m.ID + ". " + delegation + " Use reply(message_id, text) if a response is needed; this also acknowledges receipt. Otherwise use ack_message(message_id)."
	legacySend := object{"message": m, "client_message_id": "fixture-retry-key", "next_action": "End your turn after sending. AX wakes you when a reply arrives. Do not poll status or sleep waiting for it."}
	fetched := m
	fetched.Receipt = nil
	fetched.State = "wake_accepted"
	report := object{
		"wake":  object{"before": legacyWake, "after": wakeText("codex", m.ID)},
		"send":  object{"before": string(raw(legacySend)), "after": string(raw(compactToolResult("ax.send", m, "fixture-retry-key")))},
		"fetch": object{"before": string(raw(fetched)), "after": string(raw(compactToolResult("ax.get_message", fetched, "")))},
	}
	if path := os.Getenv("AX_FORMAT_FIXTURE"); path != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactRoundTripToolCalls(t *testing.T) {
	for _, hosts := range [][2]string{{"claude", "codex"}, {"codex", "grok"}, {"claude", "pi"}} {
		t.Run(hosts[0]+"_"+hosts[1], func(t *testing.T) {
			dir := startTestServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			bridges := make([]*bridge, 2)
			for i, host := range hosts {
				s := Session{ID: randomID("agt_"), Secret: randomID(""), Name: []string{"api", "web"}[i], Host: host, Mesh: testMesh, Native: uuid(), Started: true}
				file := filepath.Join(dir, s.Name+".json")
				if err := saveSession(file, s); err != nil {
					t.Fatal(err)
				}
				bridges[i] = &bridge{ctx: ctx, dir: dir, session: s, file: file, c: connectDiscoveryPeer(t, dir, s)}
			}
			calls := 0
			tool := func(index int, method string, args object) Message {
				t.Helper()
				b := bridges[index]
				var result Message
				meta := raw(object{"threadId": b.session.Native, "x-codex-turn-metadata": object{"thread_id": b.session.Native, "sandbox_mode": "workspace-write"}})
				if err := b.toolCall(ctx, meta, method, args, &result); err != nil {
					t.Fatal(err)
				}
				calls++
				if method != "ax.ack" {
					key, _ := args["client_message_id"].(string)
					if !strings.Contains(string(raw(compactToolResult(method, result, key))), result.ID) {
						t.Fatal("compact result lost identity")
					}
				}
				return result
			}
			receive := func(index int, want Message) {
				t.Helper()
				b := bridges[index]
				select {
				case offer := <-b.c.offers:
					if offer.ID != want.ID || offer.Text != want.Text {
						t.Fatal("wrong delivery")
					}
				case <-ctx.Done():
					t.Fatal("missing offer")
				}
				caps, _ := sessionCapabilities(b.session)
				receipt := "wake_accepted"
				if caps.Content == "peer_channel" {
					receipt = "channel_written"
				}
				if err := b.c.call("ax.receipt", object{"message_id": want.ID, "receipt": receipt}, nil); err != nil {
					t.Fatal(err)
				}
				if caps.Content == "wake_fetch" {
					if got := tool(index, "ax.get_message", object{"message_id": want.ID}); got.Text != want.Text {
						t.Fatal("fetch changed content")
					}
				}
			}
			m := tool(0, "ax.send", object{"target": "web", "text": "Review the boundary", "client_message_id": "roundtrip-request"})
			receive(1, m)
			r := tool(1, "ax.reply", object{"message_id": m.ID, "text": "Verified", "client_message_id": "roundtrip-reply"})
			receive(0, r)
			tool(0, "ax.ack", object{"message_id": r.ID})
			want := 3
			for _, host := range hosts {
				if host != "claude" && host != "pi" {
					want++
				}
			}
			if calls != want {
				t.Fatalf("extra MCP tool operations: %d, want %d", calls, want)
			}
			t.Logf("fixture exchange: %d tool calls; no redundant channel fetch or separate acknowledgment before reply", calls)
		})
	}
}

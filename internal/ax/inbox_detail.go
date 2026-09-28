package ax

import (
	"fmt"
	"strings"
	"time"
)

type inboxDetail struct {
	Message  Message           `json:"message"`
	Snapshot *deliverySnapshot `json:"current_snapshot"`
	Timing   struct {
		Stages  map[string]*int64 `json:"stages_at_ms"`
		Elapsed map[string]*int64 `json:"elapsed_ms"`
		Queued  *deliveryContext  `json:"queued_context"`
	} `json:"timing"`
}

func (d inboxDetail) text(now time.Time) string {
	m := d.Message
	thread := m.Thread
	if thread == "" {
		thread = "unavailable (legacy history)"
	}
	lines := []string{
		fmt.Sprintf("Message %s · age %s", m.ID, max(time.Duration(0), now.Sub(time.UnixMilli(m.Created))).Round(time.Second)),
		"Thread " + thread,
		"Expires " + time.UnixMilli(m.Expires).Local().Format(time.RFC3339),
	}
	if s := d.Snapshot; s != nil {
		lines = append(lines, "Current snapshot ("+time.UnixMilli(s.At).Local().Format("15:04:05")+"): "+s.Reason)
		if c := s.Recipient.Capabilities; c != nil {
			lines = append(lines, "Delivery: "+c.Content+" · boundary: "+c.Boundary)
		} else {
			lines = append(lines, "Delivery capabilities unavailable (legacy adapter).")
		}
	} else {
		lines = append(lines, "Current waiting reason unavailable (older broker).")
	}
	if q := d.Timing.Queued; q != nil {
		lines = append(lines, "At queue time: recipient="+q.RecipientState+", sender="+q.SenderState)
	} else {
		lines = append(lines, "Queue-time context unavailable (legacy history).")
	}
	lines = append(lines, "", "Observed timeline (broker clock):")
	for _, stage := range []struct{ key, label string }{
		{"queued", "Durable queue"}, {"handoff_started", "Handoff started"},
		{"deferred_idle", "Deferred before submit"},
		{"wake_accepted", "Native wake accepted"}, {"channel_written", "Channel written"},
		{"content_served", "Content fetched"}, {"acknowledged", "Acknowledged / replied"},
		{"recipient_turn_started", "Correlated model turn"},
	} {
		at := d.Timing.Stages[stage.key]
		value := "unavailable"
		if at != nil {
			value = time.UnixMilli(*at).Local().Format("15:04:05.000")
		}
		lines = append(lines, fmt.Sprintf("  %-23s %s", stage.label, value))
	}
	for _, interval := range []struct{ key, label string }{
		{"queue_to_handoff", "Queue → handoff"}, {"handoff_to_native_receipt", "Handoff → native receipt"},
		{"native_receipt_to_content_fetch", "Receipt → content fetch"}, {"queue_to_acknowledgment", "Queue → acknowledgment"},
	} {
		value := "unavailable / out of order"
		if ms := d.Timing.Elapsed[interval.key]; ms != nil {
			value = fmt.Sprintf("%d ms", *ms)
		}
		lines = append(lines, "  "+interval.label+": "+value)
	}
	lines = append(lines, "Task completion: unknown. A native receipt is not proof the model read it.", "", "Message:", m.Text)
	return strings.Join(lines, "\n")
}

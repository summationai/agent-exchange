package ax

// This snapshot is immutable, unlike a send retry's current readiness receipt.
// "ready" is the adapter's reported state, not proof that a model is idle.
type deliveryContext struct {
	BrokerVersion  string `json:"broker_version"`
	RecipientHost  string `json:"recipient_host"`
	RecipientState string `json:"recipient_state"`
	SenderState    string `json:"sender_state"`
}

func messageTiming(m Message, events []object) object {
	stages := map[string]*int64{}
	for _, name := range []string{"queued", "handoff_started", "deferred_idle", "wake_accepted", "channel_written", "content_served", "acknowledged", "recipient_turn_started"} {
		stages[name] = nil
	}
	for _, event := range events {
		name := event["event"].(string)
		if name == "replied" {
			name = "acknowledged"
		}
		if at, known := stages[name]; known && at == nil {
			value := event["at_ms"].(int64)
			stages[name] = &value
		}
	}
	receipt := stages["wake_accepted"]
	if channel := stages["channel_written"]; channel != nil && (receipt == nil || *channel < *receipt) {
		receipt = channel
	}
	delta := func(start, end *int64) *int64 {
		if start == nil || end == nil || *end < *start {
			return nil
		}
		d := *end - *start
		return &d
	}
	return object{
		"clock": "broker_wall_time", "queued_context": m.QueuedContext, "stages_at_ms": stages,
		"elapsed_ms": object{
			"queue_to_handoff":                delta(stages["queued"], stages["handoff_started"]),
			"handoff_to_native_receipt":       delta(stages["handoff_started"], receipt),
			"native_receipt_to_content_fetch": delta(receipt, stages["content_served"]),
			"queue_to_acknowledgment":         delta(stages["queued"], stages["acknowledged"]),
		},
		"guidance": "Broker observations, not model processing time. Queue time is recorded inside the durable send transaction. Null means unobserved or out-of-order timing. Native receipts may arrive after content fetch. Turn start is unavailable. Queue context is historical; ready does not prove idle. Acknowledgment does not prove task completion.",
	}
}

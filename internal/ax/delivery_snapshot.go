package ax

import (
	"database/sql"
	"errors"
	"time"
)

type deliverySnapshot struct {
	At        int64  `json:"at_ms"`
	Recipient Agent  `json:"recipient"`
	Reason    string `json:"waiting_reason"`
	BlockedBy string `json:"blocked_by_message_id,omitempty"`
}

func (b *broker) deliverySnapshot(m Message) (deliverySnapshot, error) {
	s := deliverySnapshot{At: time.Now().UnixMilli()}
	to, err := b.messagePeer(b.db, m.Recipient)
	if err != nil {
		return s, err
	}
	s.Recipient = agentSnapshot(to)
	s.Reason = deliveryEvidence(m.State)
	if m.State != "queued" {
		return s, nil
	}
	from, err := b.messagePeer(b.db, m.Sender.ID)
	if err != nil {
		return s, err
	}
	s.Reason = queueReason(to, from)
	if pause := resourcePause(b.dir, time.Now()); pause != nil {
		s.Reason = pause.Error()
	}
	var earlierState string
	err = b.db.QueryRow(`SELECT id,state FROM messages WHERE recipient=? AND seq<?
 AND state IN ('queued','handoff_started','delivery_uncertain') ORDER BY seq LIMIT 1`, m.Recipient, m.Seq).Scan(&s.BlockedBy, &earlierState)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if s.BlockedBy != "" {
		s.Reason += " Earlier message " + s.BlockedBy + " is " + earlierState + "."
	}
	return s, err
}

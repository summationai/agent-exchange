//go:build darwin || linux

package ax

import (
	"errors"
	"slices"
	"time"
)

// A task outcome is explicit, separate from delivery receipts and ordinary replies.
func (b *broker) networkTask(alias, recipient, id string) (Message, error) {
	paired, err := b.networkPeer(alias)
	if err != nil {
		return Message{}, err
	}
	m, err := b.message(id)
	if err != nil || m.Recipient != recipient || m.NetworkOrigin == nil || m.NetworkOrigin.Fingerprint != networkFingerprint(paired.Card) {
		return Message{}, errors.New("task not accessible")
	}
	to, err := b.messagePeer(b.db, recipient)
	if err != nil || !b.networkHandoffAllowed(m, to) {
		return Message{}, errors.New("task not accessible")
	}
	return m, nil
}

type networkTaskPage struct {
	Messages []Message `json:"messages"`
	Total    int       `json:"total"`
	Next     string    `json:"next,omitempty"`
}

func (b *broker) networkTaskList(alias, recipient, after string) (networkTaskPage, error) {
	out := networkTaskPage{Messages: []Message{}}
	paired, err := b.networkPeer(alias)
	if err != nil {
		return out, err
	}
	to, err := b.messagePeer(b.db, recipient)
	if err != nil || to.Remote != nil || !(slices.Contains(paired.Exports, "*") || slices.Contains(paired.Exports, to.Name)) {
		return out, errors.New("task recipient not accessible")
	}
	var cursor int64
	if after != "" {
		if _, err = b.networkTask(alias, recipient, after); err != nil {
			return out, err
		}
		if err = b.db.QueryRow("SELECT rowid FROM messages WHERE id=?", after).Scan(&cursor); err != nil {
			return out, err
		}
	}
	fingerprint := networkFingerprint(paired.Card)
	if err = b.db.QueryRow(`SELECT count(*) FROM messages WHERE recipient=? AND json_extract(data,'$.network_origin.fingerprint')=?`, recipient, fingerprint).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := b.db.Query(`SELECT id FROM messages WHERE recipient=? AND json_extract(data,'$.network_origin.fingerprint')=? AND rowid>? ORDER BY rowid LIMIT 51`, recipient, fingerprint, cursor)
	if err != nil {
		return out, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	used := 0
	for _, id := range ids {
		m, err := b.message(id)
		if err != nil {
			return out, err
		}
		// ListTasks history contains only the original text, not AX's full envelope.
		m.Data, m.Provenance = nil, nil
		if len(out.Messages) > 0 && (len(out.Messages) == 50 || used+len(m.Text) > maxText) {
			out.Next = out.Messages[len(out.Messages)-1].ID
			break
		}
		used += len(m.Text)
		out.Messages = append(out.Messages, m)
	}
	return out, nil
}

func (b *broker) saveNetworkTask(m Message, outcome string, cancel bool) (Message, error) {
	m.TaskOutcome = outcome
	tx, err := b.db.Begin()
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if cancel {
		m.State = "abandoned"
	}
	if _, err = tx.Exec("UPDATE messages SET data=?,state=? WHERE id=?", string(raw(m)), m.State, m.ID); err != nil {
		return Message{}, err
	}
	if _, err = tx.Exec("INSERT INTO events(message,state,at,epoch) VALUES(?,?,?,0)", m.ID, "task_"+outcome, time.Now().UnixMilli()); err != nil {
		return Message{}, err
	}
	return m, tx.Commit()
}

func (b *broker) reportNetworkTask(p *peer, id, outcome string) (Message, error) {
	m, err := b.message(id)
	if err != nil {
		return m, err
	}
	if m.Recipient != p.ID || m.NetworkOrigin == nil || !b.networkHandoffAllowed(m, p) || p.Policy != "accept" || !safe(p) || !externalHandoff(p, m) {
		return m, errors.New("network task not accessible")
	}
	if m.State == "queued" || m.State == "expired" || m.State == "refused" || m.State == "abandoned" {
		return m, errors.New("task has not been offered or is terminal")
	}
	if m.TaskOutcome == "completed" || m.TaskOutcome == "failed" || m.TaskOutcome == "canceled" {
		if m.TaskOutcome == outcome {
			return m, nil
		}
		return m, errors.New("task outcome is terminal")
	}
	if !slices.Contains([]string{"working", "input_required", "completed", "failed"}, outcome) {
		return m, errors.New("invalid task outcome")
	}
	return b.saveNetworkTask(m, outcome, false)
}

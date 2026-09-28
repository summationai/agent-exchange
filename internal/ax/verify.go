package ax

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Verification is explicitly requested by a human. A successful socket write,
// native wake receipt, or acknowledgment alone cannot pass this check.
func verifyAgent(ctx context.Context, dir, target string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, _, release, err := attachedSession(dir, "ax-verifier", "verification", "adapter", "")
	if err != nil {
		return err
	}
	defer release()
	c, err := dial(socketPath(dir))
	if err != nil {
		return err
	}
	defer c.close()
	for _, step := range []struct {
		method string
		args   any
	}{
		{"ax.connect", object{"version": "1", "agent_id": s.ID, "secret": s.Secret}},
		{"ax.presence", object{"native_session_id": s.Native, "permission_mode": "default", "state": "ready"}},
		{"ax.ready", object{}},
	} {
		if err = c.callContext(ctx, step.method, step.args, nil); err != nil {
			return err
		}
	}
	// This reserved endpoint only receives verification traffic. A prior command
	// may have exited after handoff, leaving an uncertain FIFO barrier behind.
	var pending pendingPage
	if err = c.callContext(ctx, "ax.pending", object{}, &pending); err != nil {
		return err
	}
	for _, m := range pending.Messages {
		if m.State != "queued" {
			if err = c.callContext(ctx, "ax.ack", object{"message_id": m.ID}, nil); err != nil {
				return err
			}
		}
	}
	var agents []Agent
	if err = c.callContext(ctx, "ax.list", object{}, &agents); err != nil {
		return err
	}
	var recipient Agent
	for _, a := range agents {
		if a.Name == target {
			recipient = a
		}
	}
	if recipient.ID == "" || recipient.ID == s.ID {
		return errors.New("choose another connected agent from ax agents")
	}
	if recipient.Connectivity.Wake == "user_turn" {
		fmt.Fprintln(out, "This endpoint needs a user turn. Ask it to call check_inbox once while verification is running.")
	}
	token := randomID("AX_VERIFY_")
	var sent Message
	if err = c.callContext(ctx, "ax.send", object{"target": recipient.ID, "text": "AX adapter verification requested by the user. Reply to this message through AX with exactly: " + token + ". Do not perform any other task.", "client_message_id": token, "ttl_seconds": 60}, &sent); err != nil {
		return err
	}
	fmt.Fprintf(out, "Verification queued for %s: %s. Waiting up to 60 seconds for a matching reply.\n", target, sent.ID)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	started := time.Now()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("round trip not verified for %s; inspect ax status %s: %w", target, sent.ID, ctx.Err())
		case <-c.done:
			return fmt.Errorf("broker disconnected; verification is inconclusive, inspect ax status %s", sent.ID)
		case <-ticker.C:
			if err = c.callContext(ctx, "ax.heartbeat", object{}, nil); err != nil {
				return err
			}
		case reply := <-c.offers:
			if err = c.callContext(ctx, "ax.ack", object{"message_id": reply.ID}, nil); err != nil {
				return err
			}
			// Old verification replies and unrelated messages cannot pass a new
			// challenge. Acknowledging them releases the ordinary FIFO barrier.
			if reply.Sender.ID != recipient.ID || reply.Parent != sent.ID || reply.Text != token {
				continue
			}
			fmt.Fprintf(out, "Verified %s: request delivered, matching reply received and acknowledged in %s. Wake mode: %s.\n", target, time.Since(started).Round(time.Millisecond), recipient.Connectivity.Wake)
			return nil
		}
	}
}

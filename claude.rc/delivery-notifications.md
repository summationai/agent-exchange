# Delivery failure notifications

Tracks [issue #3](https://github.com/summationai/agent-exchange/issues/3).

When a queued request expires or is refused, AX stores a notification for its
sender in the same transaction as the terminal delivery state. Each notification
contains the original message ID, recipient name, failure time and a preview of
the first 100 Unicode characters. Existing failures from before this schema
upgrade are not backfilled.

The sender receives a status wake when connected and ready. Claude receives the
notification as channel data. Other harnesses receive an instruction to call
`list_notifications`, keeping the quoted preview out of their user prompt queue.
The agent reports the failure and calls `ack_notification`. Neither tool executes,
resends or acknowledges the original task.

Notification delivery is at least once across reconnects. AX sends one status
wake per connection until acknowledgment; if a wake fails, retries on that
connection wait 30 seconds. An unacknowledged notification may appear again after
reconnection. Its stable ID lets the recipient recognize it. Notifications never
request replies, consume message reply depth or trigger an automatic resend.

`list_notifications` returns the oldest 100 unacknowledged notifications for the
authenticated agent. Acknowledging them exposes the next batch. The sender can
also invoke it once when asked to recover missed status reports. It is not a
polling mechanism. Notification rows share the original message's seven-day
retention from creation, and cleanup removes them with that message.

Native wake handling runs separately from heartbeats so a slow harness does not
expire its own broker lease. Mail and status wakes remain serialized through the
bridge; there is one heartbeat worker per connection and no worker per message.

This introduces mailbox schema version 3. Older AX binaries reject that version
rather than silently ignoring notification state. Do not install an older broker
against an upgraded mailbox. The schema is upgraded when the new broker opens
the mailbox; this source change does not replace any running process.

## Relay sender notices

Relay senders also receive `acknowledged` after an explicit acknowledgment without
a reply, and `abandoned` after owner recovery. These are stored atomically with
the terminal state. A reply itself reaches the relay, so it does not produce an
extra acknowledgment notice. Ordinary session senders keep the existing expiry
and refusal notifications. MCP delivery carries the complete notice JSON with
`meta.kind: ax_delivery_status`; `ack_notification` and restart semantics are
unchanged. Relay support moves the mailbox to schema 4; older brokers refuse it.

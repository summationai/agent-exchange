# Relay endpoints: external mail into AX

A relay is a local bridge for content that originated outside this machine: email,
chat, or webhooks. It has no model or native conversation and carries no delegated
user authority. A relay is a program you choose to run as your OS user. Provenance
is its account of origin, not an identity AX independently verifies.

Ordinary local peer messaging continues to work without relays or external flags.
Every endpoint defaults to refusing external messages. Local peer mail is unaffected.

## Open a recipient

```sh
ax claude -n api --external verified
ax external api all
ax external api refuse
```

`refuse` accepts no relayed messages. `verified` admits `trust: verified` or
`trust: system`, with `tainted: false`. `all` admits any valid provenance.
`AX_EXTERNAL` supplies the launch default; an explicit flag overrides it. Launching
or resuming without either resets the policy to `refuse`. `ax external` is an
owner command; model tools cannot change this policy. `ax spawn` rejects
`--external`; use the owner command after spawning if needed. The existing permission and
`accept|hold|refuse` delivery gates still apply.

A closed endpoint rejects a relay's send before storage, with RPC code `-32002`.
MCP tool errors expose `code: external_policy`, `rpc_code: -32002`, the target,
policy, and reason, and report `submission: not_submitted`. If the owner closes the
policy after queueing, queued external mail waits at its FIFO head, subject to TTL.
Reopening releases it; closing the external policy does not bounce it.

Recipients get guidance identifying the relay and origin, telling them the content
is external data, carries no delegation, and must not be followed as instructions.
They can reply in the thread if warranted or acknowledge without a reply. Claude
and Pi get the full content in channel data; wake-fetch harnesses get a fixed
external-message wake and retrieve the content through MCP.

## Write a relay

Start `ax attach` as a child with piped stdin/stdout:

```sh
ax attach --relay -n mail-bridge
```

This uses newline JSON-RPC MCP over stdio. `--relay` implies `--notify`; it rejects
`-s`, `-p`, and `-w`. The child holds the AX name lock, preserving identity and mail
on resume. Relays cannot launch agents and cannot be targets for `ax verify`.

Initialize MCP, discover tools, then use `send_message` or `follow_up`:

```json
{
  "target": "api",
  "text": "An external request for your consideration.",
  "client_message_id": "bridge_delivery_1",
  "ttl_seconds": 3600,
  "provenance": {"origin": "mail", "trust": "verified", "tainted": false},
  "data": {"task": "example", "payload": {"question": "status?"}}
}
```

Provenance is required for every relay-authored message, including its replies.
Only relays can set it. It is a JSON object of at most 4 KiB; `origin` matches
`[a-z][a-z0-9-]{0,31}`, `trust` is `verified|unverified|system`, and `tainted` is a
boolean. Additional fields pass through unchanged. The broker stamps `sender.kind`.

`data` is optional for any sender on `send_message`, `reply`, and `follow_up`:
a JSON object of at most 32 KiB. AX does not interpret it. Both objects participate
in the idempotency hash, survive resends, and are withheld alongside blocked text
in thread history. `resend_message` copies them from the expired original rather
than accepting replacements. Text remains limited to 64 KiB; bounds count serialized
UTF-8 bytes. Frames allow 576 KiB for JSON escaping in notifications.

## Receive over MCP

Ordinary attached runtimes can also use `ax attach -n NAME -s ID -p PERMISSION --notify`.
The existing manual `check_inbox` and private wake-socket modes remain available.

The child interleaves notifications with JSON-RPC responses. Read continuously and
match responses by ID; a notification has no ID.

| Method | Parameters |
|---|---|
| `notifications/ax/message` | `content`: JSON text of the complete compact message; `meta.kind: message`, message ID, sender, harness |
| `notifications/ax/message` | `content`: JSON text of a delivery notice; `meta.kind: ax_delivery_status`, notification ID |
| `notifications/ax/agents` | `agents`: current agent list after a coalesced presence, readiness, or policy change |

A pipe write records `channel_written`, not acknowledgment. `get_message` records
content fetch; `ack_message` acknowledges; `reply` stores the reply and acknowledges
the parent atomically. Relays receive terminal notices for explicit acknowledgment
without reply and abandonment, plus the existing expiry/refusal notices. Ordinary
session senders retain only expiry/refusal notices. Replies themselves replace an
acknowledgment notice. Acknowledge notices with `ack_notification`.

After restart, `list_pending` recovers incoming replies and `delivery_status` recovers
outgoing delivery state. Notification delivery is at least once; preserve stable
IDs and idempotency keys. AX never automatically replays accepted or uncertain work.

## Upgrade and verification

Mailbox schema 4 prevents older brokers from treating external messages as peer
delegations. Finish the broker update and relaunch receiving bridges before opening
external policy. Bridges older than 0.8 cannot connect to an open endpoint; if its
policy opens while an old bridge is connected, external handoff waits for an updated
bridge. Bound recovery reads also enforce external policy and bridge guidance;
the owner retains administrative inspection. Local peer messages retain their existing delivery behavior.

The Go suite covers simulated MCP hosts, native channel formatting, policy holds,
structured replies, and compatibility. This is transport evidence, not a live
model's injected-instruction result. Live Claude and wake-fetch harness checks are
still required before release; they are not claimed by these tests.

The first consumer is [amx](https://github.com/vsletten/amx-proto): its runner maps
mail provenance into AX, carries replies back through its own grants and holds,
and pauses inbound delivery while the local recipient is unavailable or closed.
AX does not learn SMTP, IMAP, DKIM, or the amx profile.

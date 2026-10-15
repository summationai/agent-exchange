# AX across a Tailscale network

Status: implementation started, October 14, 2026. Tracks [issue 27](https://github.com/summationai/agent-exchange/issues/27).

## Experience

Keep the coding sessions and commands people already use. A session on a laptop can discover `reviewer@buildbox`, send a request, and receive its reply in the original conversation. Unqualified names continue to mean local agents. AX never starts a coding agent unless the user explicitly asks.

Each OS user keeps one broker, a private Unix socket, and a local SQLite mailbox. An optional, separately stoppable network adapter connects those brokers. Pairing identifies an AX installation belonging to an OS user, not every user of a machine. Tailnet membership alone grants no AX access.

```text
Claude ─ MCP ─ local broker ─ network adapter
                SQLite             │
                           TLS over Tailscale
                                   │
Codex  ─ MCP ─ local broker ─ network adapter
                SQLite
```

## Decisions

Use the installed Tailscale service, rather than embedding a second Tailscale node in AX. The adapter uses the installed CLI once at initialization and once at listener startup to obtain the local addresses. It neither logs in nor modifies the tailnet, ACLs, Serve, or Funnel. There is no Tailscale dependency in local-only operation and no new Go dependency for the foundation.

Bind only to an address reported by the running local Tailscale service. Never bind a wildcard, LAN, or public address. Tailscale controls connectivity; mutually authenticated TLS and explicit AX pairing control application access. Per-installation certificates distinguish OS users even when they share a Tailscale device. Public pairing cards contain no session credentials or private keys. Exchange cards through an independently trusted channel and verify fingerprints before trusting them.

Each peer has a local alias and an explicit allowlist of agent names it can discover. An empty allowlist grants no discovery. A literal `*` explicitly exports all agents. The first milestone grants discovery only; messaging authorization will be a separate capability, not an implicit consequence of upgrading a discovery pairing. A certificate cannot silently replace another certificate at an existing alias. Revocation takes effect on the next request, including an existing TLS connection. A request authorized before revocation may finish its bounded response.

## First milestone: implemented foundation

These development commands establish identity, explicit trust, scoped discovery, and revocation. Pairing starts with discovery only. Enable messaging separately on both machines to use the existing MCP tools across the connection.

```sh
# Laptop, choose the installation's name and save its public card.
ax network init laptop > laptop.json

# Build machine.
ax network init buildbox > buildbox.json

# Exchange the public files through a trusted channel, then on the laptop:
ax network trust buildbox buildbox.json api

# On the build machine, permit the laptop to discover reviewer:
ax network trust laptop laptop.json reviewer

# Enable messaging on the laptop, and separately on the build machine:
ax network messaging buildbox on
ax network messaging laptop on

# Keep the network adapter running on each machine.
ax network serve

# From a second terminal on the laptop:
ax network agents buildbox

# Remove access without restarting coding agents:
ax network revoke buildbox
```

The names after a trust command are local agents that the named remote installation may discover. They are not remote targets. `ax network identity` prints the existing public card; `ax network trust ALIAS - AGENT` reads a card from stdin. Initialization defaults to port 48889; an optional final port supports multiple OS users on one machine. No production listener is enabled by installing or updating AX.

The private network state lives under `AX_HOME/network`. Identity keys and pairing policy use the existing private file, ownership, locking, and atomic-write helpers. Losing this directory requires pairing again. Initialization is idempotent and never silently regenerates a key. Certificates expire after one year; the foundation reports an expired identity rather than rotating it behind a peer's back. Certificate renewal with the same installation identity is follow-up work before general release.

The authenticated `GET /ax/network/v1/agents` discovery endpoint returns a protocol version, machine alias, and a bounded list of exported AX agent IDs, names, harness types, and current state. It excludes native session IDs, workspaces, permission settings, credentials, messages, and transcripts. There is no socket proxy, remote broker administration, shell, spawn, kill, or recovery API. Unsupported methods and routes fail closed.

## Message routing milestone

The broker checkpoint implements private adapter operations for remote discovery snapshots, a durable outbox, destination import, and destination acceptance. The optional network adapter now connects these operations through the official A2A Go SDK, pinned to version 2.0.0, and one worker per paired machine. It does not expose the local socket as a TCP API.

`ax network messaging ALIAS on` explicitly grants messaging to a paired installation; `off` holds later outbound transfers and incoming native handoffs. Both sides must opt in. Incoming scope uses the same explicit local-agent export list as discovery. Pairing and AX updates alone never grant messaging. Remote identities are qualified by the local peer alias. External provenance remains intact, and the receiving agent's external policy still applies.

Destination idempotency and absolute expiration are stored in the existing SQLite message transaction. Lost network acceptance can be retried with the same source message ID, returning the same destination message ID and recipient sequence. Acceptance records and cross-machine parent mappings survive restart. A reply acknowledges the original request; network acceptance alone does not claim native delivery or completed work.

Keep the existing MCP tool names. Discovery merges explicitly permitted remote agents, qualified by the paired local machine alias. Cache entries carry the installation fingerprint and remote agent ID; a name or IP alone is not an identity. A duplicate remote name never shadows a local name. Pairing is direct, with no transitive discovery or forwarding through other machines.

The source broker commits outgoing work before attempting transmission. The destination commits the incoming message and deduplication record together before acknowledging network acceptance. The stable key includes authenticated source installation, source agent ID, and client message ID. A retry with different content is rejected. Local and remote message IDs are mapped durably, so replies and follow-up threads survive restart. Remote claims cannot impersonate a local agent or modify permission and provenance fields.

Preserve native sandbox and approval controls. An explicit messaging grant authorizes delegation to specific recipients; discovery grants do not. Unpaired, revoked, out-of-scope, and ambiguous requests are rejected before content enters a native session. Existing external-relay provenance remains tainted when crossing machines. A trusted machine must not launder an external message into an authorized peer task.

Source outbox work stays `queued` until the destination commits it, then becomes `remote_accepted`. Destination harness receipt remains separate from network acceptance, model acknowledgment, and task completion. If network acceptance is lost, retry the same idempotency key. If a native handoff is uncertain, preserve the uncertainty and never automatically execute the task again. A disconnected peer remains explicitly offline; cached discovery is not evidence of reachability.

## A2A boundary

The development binding uses A2A 1.0 JSON-RPC and declares `protocolVersion: "1.0"`. Scoped Agent Cards are available at `/ax/network/v1/a2a/AGENT_ID/agent-card.json`, with their JSON-RPC endpoint at the same path without the card suffix. Cards declare pinned mutual TLS and a required `https://useax.dev/extensions/named-mailbox/v1` extension. The AX discovery route remains a small AX protocol.

The binding implements authenticated Agent Cards, SendMessage, GetTask, ListTasks, and CancelTask. Initial cards declare streaming and push notifications unsupported; unsupported optional operations return their standard errors. Standard task state is distinct from AX delivery evidence. A durable queued request maps to submitted; actual execution can map to working. Receipt, channel write, and model acknowledgment must never map to completed. The recipient reports an explicit outcome through the MCP tool `report_task`, using working, input_required, completed, or failed. Completion requires this explicit report, and cancellation must not claim to terminate native work that the harness cannot cancel. AX receipt metadata belongs in a declared extension with stable installation and message IDs. The required named-mailbox extension carries the source AX agent identity, source message ID, recipient, absolute expiration, optional parent mapping, provenance, and structured AX data in message metadata keyed by its URI. The A2A text part is the only wire copy of the text; the metadata envelope uses an empty text field. Clients activate the extension in both the A2A-Extensions header and the message extensions list. The mutual TLS certificate, never metadata, establishes the installation identity.

Validate normative fields, task ownership, message roles and parts, version headers, idempotency, errors, and authorization with an independent A2A client before claiming interoperability. An AX-to-AX test alone is insufficient. An independently constructed official SDK client exercises the advertised Agent Card and task methods in tests. This is an experimental A2A binding, not a full conformance claim. Task/context continuation fields and ListTasks filters are explicitly rejected in this checkpoint; AX thread continuation uses the declared mailbox extension. Streaming, push, file content, and artifacts are unsupported.

## Resource and failure budgets

Foundation limits: 32 trusted peers, 64 exported names per peer, 16 accepted connections, 8 KiB HTTP headers, 256 KiB configuration, 5-second read/write deadlines, and 10-second idle keepalive. The existing one-core Go runtime and shared AX CPU guard also apply to the network adapter. Discovery-only pairing has no outbound worker. Messaging workers refresh discovery every 30 seconds, read pairing changes every 5 seconds, and use a coalesced private broker event to wake immediately for new outgoing work. No model polling is involved. Closing the adapter never closes a harness or the local broker.

Discovery advertises at most 64 exported agents and reports `truncated: true` when more exist. The CLI and workers share this limit; older development advertisements of up to 256 agents are read as partial discovery. A full remote cache skips new identities and reports the skipped count without preventing delivery to known recipients.

The broker checkpoint caches at most 64 remote identities and reserves at least 64 of the existing endpoint slots for local agents. It archives revoked or idle remote entries after one minute without deleting their durable identities, mail, or retry keys. Agent discovery reads and verifies each paired installation once per list rather than once per remote agent.

Each permitted peer has one worker and one active HTTP request. Outages start at 1 second of backoff, double to 60 seconds, and add up to 25 percent jitter. New-message hints cannot defeat that backoff. HTTP calls have a 5-second deadline; a drain cycle has a 15-second deadline and a batch of at most 16 messages. Requests and SDK response bodies are bounded by the existing frame limit. A rejected recipient is excluded from the remainder of its bounded drain batch, so it cannot block unrelated recipients; its original request remains queued for later retry or expiry. The existing per-recipient and global mailbox quotas apply. A private process lock and single broker event subscriber prevent duplicate adapter workers. Measure idle CPU, reconnect CPU, RSS, and file descriptors under peer outage and suspend/resume before release.

## Verification and rollout

1. Foundation: real TLS connections between isolated AX homes, scoped broker discovery, denial of unpaired and mismatched identities, duplicate-alias rejection, request bounds, revocation on keepalive, and local operation after adapter shutdown.
2. Live network: run isolated development adapters on two real tailnet machines; test both directions, ACL denial, address changes, offline peers, and macOS/Linux/WSL behavior. Do not replace the user's active broker to perform this test.
3. Routing: Claude-to-Codex request and reply on two machines; same-name agents, restart, suspend, lost acceptance, duplicate payloads, expiry, in-flight revocation, and tainted provenance. Fault injection must prove bounded resource use and no duplicated native execution.
4. A2A: independent client and conformance evidence at the exact release head. No compliance claim from a draft or an AX-only test.
5. Public release: update installation, website privacy statements, doctor diagnostics, and upgrade instructions. Local-only remains the default. Say that optional remote messaging leaves the machine, uses encrypted Tailscale paths that may traverse relays, and remains subject to each harness's model-provider processing.

## Sources

- [Issue 27 findings and acceptance criteria](https://github.com/summationai/agent-exchange/issues/27).
- [Tailscale identity](https://tailscale.com/docs/concepts/tailscale-identity), [grants](https://tailscale.com/docs/features/access-control/grants), and [connection types](https://tailscale.com/docs/reference/connection-types).
- [A2A specification](https://a2a-protocol.org/latest/specification/), checked October 14, 2026. Pin the reviewed protocol schema when implementing the routing milestone.

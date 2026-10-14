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

Use the installed Tailscale service, rather than embedding a second Tailscale node in AX. The initial adapter uses the installed CLI once at initialization and once at listener startup to obtain the local addresses. It neither logs in nor modifies the tailnet, ACLs, Serve, or Funnel. There is no Tailscale dependency in local-only operation and no new Go dependency for the foundation.

Bind only to an address reported by the running local Tailscale service. Never bind a wildcard, LAN, or public address. Tailscale controls connectivity; mutually authenticated TLS and explicit AX pairing control application access. Per-installation certificates distinguish OS users even when they share a Tailscale device. Public pairing cards contain no session credentials or private keys. Exchange cards through an independently trusted channel and verify fingerprints before trusting them.

Each peer has a local alias and an explicit allowlist of agent names it can discover. An empty allowlist grants no discovery. A literal `*` explicitly exports all agents. The first milestone grants discovery only; messaging authorization will be a separate capability, not an implicit consequence of upgrading a discovery pairing. A certificate cannot silently replace another certificate at an existing alias. Revocation takes effect on the next request, including an existing TLS connection. A request authorized before revocation may finish its bounded response.

## First milestone: implemented foundation

These development commands establish identity, explicit trust, scoped discovery, and revocation. They do not yet route messages or change the harness-facing MCP tools.

```sh
# Laptop, choose the installation's name and save its public card.
ax network init laptop > laptop.json

# Build machine.
ax network init buildbox > buildbox.json

# Exchange the public files through a trusted channel, then on the laptop:
ax network trust buildbox buildbox.json api

# On the build machine, permit the laptop to discover reviewer:
ax network trust laptop laptop.json reviewer

# Keep the network adapter running on each machine.
ax network serve

# From a second terminal on the laptop:
ax network agents buildbox

# Remove access without restarting coding agents:
ax network revoke buildbox
```

The names after a trust command are local agents that the named remote installation may discover. They are not remote targets. `ax network identity` prints the existing public card; `ax network trust ALIAS - AGENT` reads a card from stdin. Initialization defaults to port 48889; an optional final port supports multiple OS users on one machine. No production listener is enabled by installing or updating AX.

The private network state lives under `AX_HOME/network`. Identity keys and pairing policy use the existing private file, ownership, locking, and atomic-write helpers. Losing this directory requires pairing again. Initialization is idempotent and never silently regenerates a key. Certificates expire after one year; the foundation reports an expired identity rather than rotating it behind a peer's back. Certificate renewal with the same installation identity is follow-up work before general release.

Only authenticated `GET /ax/network/v1/agents` is exposed. It returns a protocol version, machine alias, and a bounded list of exported agent names, harness types, and current state. It excludes native session IDs, workspaces, permission settings, credentials, messages, and transcripts. There is no socket proxy, remote broker administration, shell, spawn, kill, or recovery API. Unsupported methods and routes fail closed.

## Message routing milestone

Keep the existing MCP tool names. Discovery merges explicitly permitted remote agents, qualified by the paired local machine alias. Cache entries carry the installation fingerprint and remote agent ID; a name or IP alone is not an identity. A duplicate remote name never shadows a local name. Pairing is direct, with no transitive discovery or forwarding through other machines.

The source broker commits outgoing work before attempting transmission. The destination commits the incoming message and deduplication record together before acknowledging network acceptance. The stable key includes authenticated source installation, source agent ID, and client message ID. A retry with different content is rejected. Local and remote message IDs are mapped durably, so replies and follow-up threads survive restart. Remote claims cannot impersonate a local agent or modify permission and provenance fields.

Preserve native sandbox and approval controls. An explicit future messaging grant authorizes delegation to specific recipients; discovery grants do not. Unpaired, revoked, out-of-scope, and ambiguous requests are rejected before content enters a native session. Existing external-relay provenance remains tainted when crossing machines. A trusted machine must not launder an external message into an authorized peer task.

Source outbox states add `remote_queued` and `remote_accepted`. Destination harness receipt remains separate from network acceptance, model acknowledgment, and task completion. If network acceptance is lost, retry the same idempotency key. If a native handoff is uncertain, preserve the uncertainty and never automatically execute the task again. A disconnected peer remains explicitly offline; cached discovery is not evidence of reachability.

## A2A boundary

Target A2A 1.0, JSON-RPC binding, declared `protocolVersion: "1.0"`. Keep the local AX socket protocol private. The discovery-only foundation is a small AX protocol and advertises no A2A endpoint or compliance. Do not expose an Agent Card promising methods before those methods work.

The routing milestone must implement authenticated Agent Cards, SendMessage, GetTask, ListTasks, and CancelTask against the named version. Initial cards declare streaming and push notifications unsupported; unsupported optional operations return their standard errors. Standard task state is distinct from AX delivery evidence. A durable queued request maps to submitted; actual execution can map to working. Receipt, channel write, and model acknowledgment must never map to completed. Completion requires an explicit task outcome, and cancellation must not claim to terminate native work that the harness cannot cancel. AX receipt metadata belongs in a declared extension with stable installation and message IDs.

Validate normative fields, task ownership, message roles and parts, version headers, idempotency, errors, and authorization with an independent A2A client before claiming interoperability. An AX-to-AX test alone is insufficient. The protocol implementation and its task-outcome contract will receive their own review before message delivery is enabled.

## Resource and failure budgets

Foundation limits: 32 trusted peers, 64 exported names per peer, 16 accepted connections, 8 KiB HTTP headers, 256 KiB configuration, 5-second read/write deadlines, and 10-second idle keepalive. The existing one-core Go runtime and shared AX CPU guard also apply to the network adapter. Discovery has no background refresh loop, model calls, reconnect loop, or persistent outbound connection. Closing the adapter never closes a harness or the local broker.

Before routing ships: one worker per paired peer, one active outbound request per peer, 1-second initial backoff growing to 60 seconds with jitter, deadlines and TTL checked before every retry, and a bounded durable outbox. Wake workers on new messages and connectivity hints, not tight polling. Resolve and bound the retry, queue, and storage budgets in the routing PR. Measure idle CPU, reconnect CPU, RSS, and file descriptors under peer outage and suspend/resume.

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

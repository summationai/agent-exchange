# Agent Exchange 0.8.0

AX can now carry messages between agents on explicitly paired Tailscale machines. Your laptop can ask `reviewer@buildbox` for a review and receive the reply in the same conversation. Local-only messaging remains the default.

## Across machines

Each OS user keeps a local broker and SQLite mailbox. An optional network adapter connects paired installations through pinned mutual TLS. Pairing grants discovery first; both sides explicitly enable messaging and choose which local agents the peer can reach. Tailnet membership alone grants no AX access.

Messages stay queued on the sender while a destination is unavailable, subject to expiry. Retries use stable message identities and bounded backoff. Network acceptance, native handoff, acknowledgment, and task completion remain separate. Remote completion reports are not yet synchronized into the sender's receipt.

Follow [Tailscale setup](https://useax.dev/docs/tailscale). Installing AX does not configure Tailscale or enable a listener. AX has no hosted relay or telemetry; optional remote traffic leaves the machine over encrypted Tailscale paths, which may traverse encrypted relays.

## Other changes since 0.7.2

- Attach an existing runtime through MCP with `ax attach`; capabilities distinguish automatic wake from inbox delivery on a user turn.
- Add follow-ups to a request and read retained thread history. Reply-depth errors explain how to continue in a fresh thread without replaying completed work.
- Read clearer delivery timelines and native-boundary information. Opt-in idle delivery is limited to harnesses that expose that boundary.
- Connect external relay endpoints with provenance and an owner-controlled recipient policy. External content remains data rather than a local delegation.
- Keep exited sessions out of active discovery while preserving their names, conversations, and mail for resume.

The saved bypass opt-in, explicit Claude bypass handling, and Claude channel lifetime fixes from 0.7.2 are retained.

## Install and upgrade

```sh
curl -fsSL https://useax.dev/install.sh | sh
```

Or ask your agent to follow [AGENTS.md](https://useax.dev/agents.md). macOS and Linux have prebuilt binaries for ARM64 and x86_64. Windows uses WSL 2.

**Mailbox upgrade:** the 0.8.0 broker uses schema 4. Before starting it, close active AX sessions normally, stop only the verified old broker, and privately back up its stopped mailbox files and saved sessions. Follow the [running-session update guide](https://useax.dev/docs/installation#updating-running-sessions). A 0.7.x broker cannot open the upgraded mailbox; reinstalling an old executable is not a rollback. Preserve messages written after the upgrade if recovery becomes necessary.

Existing bridges can reconnect for local mail, but relaunch saved names after active work finishes to load new tools and cross-machine guidance. Native conversations and AX identities are retained.

## Verification and limits

Two physical Apple Silicon Macs exchanged native Grok-to-Codex requests and replies, in both directions across the network. A queued request also recovered after restarting only the destination network adapter, without restarting Codex. A schema-3 production-mailbox copy upgraded without changing its message payloads or credential hashes; running 0.7.2 bridge fixtures reconnected and exchanged local mail after the broker upgrade.

The release workflow checks four native binary builds, race tests, vet, installer behavior, and installation inside WSL 2. These checks do not prove live harness interoperability on every platform. Live two-machine Linux/WSL, suspend/resume, automatic certificate renewal, and remote task-outcome synchronization remain follow-up work. The network binding uses an AX-specific extension and does not claim general A2A conformance. Codex and Grok still use AX-owned native backends; sampled resource guards are not hard quotas.

See the [verification record](https://useax.dev/docs/verification) for evidence and its boundaries.

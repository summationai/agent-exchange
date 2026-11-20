# Agent Exchange 0.8.3

Codex sessions can recover AX tools after an MCP client reconnects. Agent discovery now shows mailbox backlog and why delivery is waiting, and Claude keeps its readiness during compaction.

## Messaging recovery

Codex can retain multiple MCP clients for the same conversation. Previously one could hold the broker lease while the model called another, causing message retrieval to time out even though incoming wakes reached the native queue. A fresh tool call can now reclaim the lease only when its native conversation, current launch and credentials match. Background clients and automatic retries cannot take it back. Accepted messages remain in the mailbox; recovery does not resend tasks.

Canceled bridge helpers can now exit while their MCP input pipe is idle. This closes the cleanup failure that left duplicate helpers behind after cancellation.

## Delivery diagnostics

Local agent discovery reports queued and unacknowledged counts, the oldest unresolved message, and the reason mail is waiting. Known permission prompts and user questions are identified without exposing message contents. Status remains separate from proof of receipt or task completion.

A send that the broker definitely refuses before storing anything reports that nothing was sent, with a recovery step. Rate-limit refusals include a retry delay. An earlier attempt with a lost response remains uncertain. Database lookup failures retain their original error rather than being mistaken for an invalid message ID.

Claude compaction keeps its current readiness and effective native permissions. Test fixtures also ignore AX identity and policy variables inherited from a developer's active session.

## Install and update

```sh
curl -fsSL https://useax.dev/install.sh | sh
```

Or ask your agent to follow [AGENTS.md](https://useax.dev/agents.md). Prebuilt binaries support macOS and Linux on ARM64 and x86_64. Windows uses WSL 2.

The Codex ownership fix needs both the updated broker and bridge. Replacing the installed binary does not update already running helpers. Let active work finish and follow the [running-session update guide](https://useax.dev/docs/installation#updating-running-sessions). Preserve saved AX names when relaunching.

There is no mailbox schema change from 0.8.x. Upgrading from 0.7.x still requires the schema-4 backup precautions in that guide; a 0.7.x broker cannot open the upgraded mailbox.

## Verification

Focused regressions cover accepted-message retrieval after lease replacement, displaced-client retries, launch and conversation fencing, canceled input readers, compaction state, definite-refusal recovery, database failures, and discovery diagnostics. The release workflow gates publication on race tests, vet, four native builds, installer verification, and WSL installation.

Native messaging recovery on this Mac is tracked separately from automated transport tests. These checks do not establish every harness lifecycle on every platform; [issue 82](https://github.com/summationai/agent-exchange/issues/82) retains the separate Claude cold-start and idle-wake validation work.

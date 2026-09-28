# Testing the coordination changes

This development build combines the readiness and timing work in #69 / PR #73 with the compact message formats in #70, session delivery contracts in #71, and local inbox timelines in #72. It is not a release. Native session testing is still required before calling these changes verified in the installed harnesses.

## What to try

Use the development binary in a separate AX home in each terminal. Keep the same AX home across the test sessions. Existing installations, conversations, and the normal broker can keep running.

```sh
export AX_HOME="$HOME/.ax-coordination-test"
./bin/ax claude -n test-api
```

In a second terminal:

```sh
export AX_HOME="$HOME/.ax-coordination-test"
./bin/ax codex -n test-web
```

Ask one agent to send the other a question, then reply. Look for automatic readiness, short wake messages, a concise send receipt, and complete incoming content. A reply acknowledges the original message; the agent should not send a separate acknowledgment first. Claude and Pi receive full peer content and should not fetch it again.

Open the observer in another terminal:

```sh
export AX_HOME="$HOME/.ax-coordination-test"
./bin/ax inbox -a test-api
```

Select a message with the arrow keys or j/k. Scroll its details with u/d. The detail includes thread identity, age, expiry, the observed timeline, stage durations, the historical queue context, and a separate current waiting reason. Filter by thread with `-t THREAD_ID` or delivery state with `-s queued`. Filters combine, and the legacy `ax inbox NAME` syntax still works. Use `ax status MESSAGE_ID` for the full JSON record.

Opening or selecting a message never acknowledges it, wakes an agent, or replays work. The view requests at most 100 matching rows, bounds the list query to 250 ms, fetches one selected detail, refreshes every two seconds, and retains one update and one selection. It never starts a broker. Quit remains responsive when the broker is absent. Empty results do not establish that no messages ever existed: terminal mail is retained for up to seven days.

To try idle delivery, launch Claude or Pi with `AX_DELIVERY_BOUNDARY=idle`. While the agent is working, send it a message and inspect the queued wait in another terminal. The native idle event releases it. OpenCode uses this boundary by default. Close and resume the same test names, and repeat while the other agent is offline or waiting for permission. An unknown handoff outcome must stay uncertain until explicitly recovered.

## Per-session delivery contracts

The credentialed bridge sends `delivery_capabilities` in `ax.connect`. The broker validates version 1 and echoes the accepted contract. Capabilities are negotiated again on each reconnect and can be updated through the adapter-only `ax.capabilities` RPC. They are not model tool arguments, permissions, or proof of model receipt. `ax agents`, `ax doctor`, and explicit delivery status expose the accepted contract; routine discovery gives its content mode and selected boundary.

| Adapter | Content | Default boundary | Other supported boundary |
| :--- | :--- | :--- | :--- |
| Claude | Native peer channel | Native channel scheduling | Idle |
| Codex | ID wake, then MCP fetch | Native queue | None |
| Grok | ID wake, then MCP fetch | Native queue | None |
| OpenCode | ID wake, then MCP fetch | Observed idle | None |
| Pi | Native custom peer message | Native follow-up after the turn | Idle |

These describe the existing adapter APIs. They do not imply that all installed native versions have passed a live exchange with this build. Codex and Grok adapters do not currently prove busy/idle transitions, so asking them for `idle` fails explicitly. Next-tool, next-message, interrupt, and manual modes are not advertised by these built-in adapters. The separate attachment work in PR #65 is not included in this batch.

An OpenCode wake rechecks native status before calling the prompt API. If the session became busy or blocked, the adapter returns a definite non-submission response. AX records `deferred_idle` and keeps the message queued. A unique handoff ID ties that response to one attempt, so a delayed duplicate cannot undo a newer handoff. A timeout, failed native call, or unknown outcome remains uncertain and is never automatically replayed.

Older bridges negotiate no contract and retain legacy behavior. A new bridge with an older broker can retain native queue/channel behavior, but refuses an idle boundary that the broker cannot enforce. Upgrade the broker and relaunch the test session in that case. Capability changes never override identity, permission, FIFO, expiry, or resource limits.

### Adapter example

An adapter that has verified Claude's channel and lifecycle hooks negotiates:

```json
{
  "version": 1,
  "adapter_version": "0.7.1-dev",
  "content": "peer_channel",
  "boundary": "idle",
  "supported_boundaries": ["native_queue", "idle"],
  "observable_lifecycle": ["session_bound", "busy", "idle", "blocked"]
}
```

Register the adapter's supported contract in `sessionCapabilities`, then pass this object as `delivery_capabilities` with its authenticated connection. Only native lifecycle events may update presence. Peer bodies belong in a native peer-content event; a user-prompt API receives only fixed AX guidance and generated message IDs. An adapter without a safe peer-content API must keep the fetch fallback. See [adding an adapter](adapters.md).

The shared conformance tests cover permissions, deferral, expiry, uncertain delivery, duplicate events, reconnect negotiation, legacy clients, and independent conversations. The OpenCode plugin fixture executes the actual JavaScript adapter against a fake native API and proves a busy wake makes zero prompt submissions.

## Message overhead and context refresh

Full guidance remains in MCP initialization and the native instruction surfaces already supported by each adapter. Initialization, resume that creates a new bridge, and MCP reconnection resend those instructions. AX does not guess when the model compacted its history. Instead, every fetched or native-channel message carries a self-contained scope, permission, literal-content, reply/acknowledgment, and no-polling reminder. This conservative fallback applies even when the host has no context-loss event.

Routine send/reply results omit the echoed body and redundant native session metadata. They retain message and thread IDs, parent/resend links, expiry, the retry key, submission certainty, and actionable delivery evidence. Explicit status retains the rich record. Error responses still distinguish not-submitted from outcome-unknown and retain the original idempotency key.

Synthetic review request, serialized UTF-8 and tiktoken 0.12.0 `o200k_base` estimates:

| Payload | Before bytes | After bytes | Before estimated tokens | After estimated tokens |
| :--- | ---: | ---: | ---: | ---: |
| ID wake | 676 | 160 | 134 | 46 |
| Send result | 1,398 | 640 | 379 | 161 |
| Content fetch | 630 | 808 | 184 | 198 |
| Combined | 2,704 | 1,608 | 697 | 405 |

The combined estimate drops about 42%. Fetches alone grow slightly because they now restore guidance themselves. These are fixture payload estimates, not provider billing, native context-token counts, or measured model latency. The body, including its quoting, is unchanged.

A fixture round trip exercised the real broker and bridge tool-call path: Claude/Codex used four tool calls, Codex/Grok five, and Claude/Pi three. These counts include request, reply, final acknowledgment, and fetches only where required. Formatting does not itself remove a necessary fetch or claim a tool-count reduction from the previous implementation. Native channels already avoided that fetch.

The measurement fixture can be exported with `AX_FORMAT_FIXTURE=/tmp/ax-format.json go test ./internal/ax -run TestFormattingMeasurementFixture`. Tokenize each before/after string with the named encoding to reproduce the table. [Delivery timing](latency.md) describes the separate local latency sampler and its evidence limits.

## Scope and verification

The scope is #70–#72 on top of the reviewed #69 implementation. Owner boundaries are the MCP formatter, authenticated bridge/broker negotiation, native OpenCode preflight, and read-only observer. There is one broker, no new database schema, no terminal ownership, and no new harness integration. Generic attachment, new Codex conversation rebinding, Tailscale, release tagging, and installation into the normal runtime are outside this build.

Version probes on the development machine: Claude Code 2.1.283, Codex CLI 0.157.1, Grok 1.0.41, OpenCode 1.18.32. Pi is unavailable on PATH. Version probes are not native messaging verification. Existing coding sessions were not restarted.

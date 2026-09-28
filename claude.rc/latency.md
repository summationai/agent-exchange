# AX readiness and delivery timing

AX activates messaging when native conversation binding and MCP tool discovery are both confirmed. Either event can arrive first. New brokers send an optional lifecycle hint to the connected bridge, and discovery also triggers a readiness check. Hints coalesce rather than starting more workers. The five-second heartbeat remains for leases, bounded recovery, and compatibility with older brokers; startup no longer waits for that timer when both components support readiness events.

Existing coding sessions keep their installed code until relaunched. This change does not install a binary, restart a broker, or rebind a conversation. See [updating running sessions](../AGENTS.md#updating-running-sessions).

## Inspect one delivery

Run `ax status MESSAGE_ID`, or use the existing MCP `delivery_status` tool when investigating a particular request. The `timing` field reports the first broker observation of each stage:

| Stage | Evidence |
| --- | --- |
| `queued` | Timestamp recorded inside the durable send transaction, before its commit completes. |
| `handoff_started` | Broker recorded its intent to offer the message to the bridge. |
| `wake_accepted` | Bridge reported native acceptance. |
| `channel_written` | Bridge reported writing the native channel event. |
| `content_served` | Broker processed the recipient's content-fetch request. |
| `acknowledged` | Recipient acknowledged or replied through AX. |
| `recipient_turn_started` | Unavailable: these adapters do not supply a correlated native event. |

These are wall-clock observations at the broker, not model processing durations. The queue interval includes durable-write work. A delayed receipt RPC can arrive after a fetch or acknowledgment; the receipt remains in history without moving message state backward. Missing observations, or intervals whose endpoints arrive out of order or move backward with the wall clock, are `null`.

The immutable `queued_context` records the broker version and sender/recipient state at initial queueing. It is distinct from the current readiness snapshot returned on a send retry. Older messages have no historical context and remain unavailable rather than borrowing today's state. `ready` is an adapter report, not proof of an idle model; `offline` does not establish whether reconnection is underway. A receipt or acknowledgment does not prove task completion.

## Measure a bounded workload sample

From a source checkout with Python 3:

```sh
python3 scripts/measure_latency.py -n 1000 -w 24
```

The script reads the mailbox selected by `AX_HOME`, or the default user mailbox. Use `-d /path/to/mailbox.db` for another database. It opens SQLite read-only, caps the sample at 1,000 recent messages and the window at seven days, and stops SQL work after a two-second deadline. It exports no message text, IDs, session names, or credentials and starts no broker or harness.

The JSON report groups intervals by the states recorded at queue time, recipient harness, and broker version. It reports sample counts, nearest-rank p50/p95, missing observations, and out-of-order observations. Supply `-v 'codex=VERSION, claude=VERSION'` to record independently checked native versions. Those versions describe the operator's measurement environment, not each historical message. Results reflect actual workload, including busy and offline sessions; they are not an idle-agent benchmark. There is no external telemetry.

## Transport benchmark and native verification

```sh
go test ./internal/ax -run '^$' -bench '^BenchmarkDeliveryTiming$' -benchtime=100x -count=1
python3 scripts/test_measure_latency.py
```

The benchmark uses an isolated Unix-socket broker, durable SQLite writes, and a fixture recipient. It reports send-to-offer p50/p95 for up to 1,000 samples, allocation counts, and process CPU per operation. CPU includes the fixture and its bounded cleanup; it is not a standalone broker CPU measurement. The standard `ns/op` includes simulated receipt/fetch/ack calls and cleanup. No model or native coding agent runs.

For native response measurements, record the exact AX and harness versions, OS/CPU, and whether the session is fresh, resumed, idle, busy, permission-blocked, or reconnecting. Test those cases separately using explicitly authorized sessions. Record first correlated native turn activity only when the harness provides it. Measure helper CPU and RSS separately with the OS's process tools; never infer those from model response time or the Python sampler's own usage. Keep missing native evidence explicit.

Related: [delivery evidence](README.md), [resource protection](resource-safety.md), and [adapter verification](adapters.md).

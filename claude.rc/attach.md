# Connect an existing local runtime

This feature is unreleased. Build the current source to try it; the published 0.7.1 binary does not include these commands.

`ax attach` runs AX's MCP server as a child of an existing runtime. It does not launch, wrap, restart, or signal the agent. The native runtime owns its conversation and approval controls. Its MCP configuration must start a separate AX child for each conversation.

This integration uses the same local Unix-socket broker, mailbox, credentials, receipts, and resource limits as the built-in harness adapters. It adds no network listener or remote connectivity. Muse, ChatGPT, and Grok Bot cloud integrations are not verified or enabled by this change.

## Configure the MCP child

For a runtime with standard MCP tools but no native wake integration:

```json
{
  "mcpServers": {
    "ax": {
      "command": "/absolute/path/to/ax",
      "args": ["attach", "-n", "planner", "-s", "conversation_123", "-p", "default"]
    }
  }
}
```

Replace the path and conversation ID with values from the native runtime. IDs accept letters, numbers, underscores, and hyphens, up to 80 characters. The AX name stays bound to that ID on reconnect; selecting a different conversation requires a different AX name. A second active child cannot take over the same name.

The native integration supplies `-p` from its effective permission state. Omitting it leaves permissions unknown and blocks delivery. Do not select `default` just to clear an error. Supported modes follow the existing AX permission policy; intentional native bypass still needs the existing `AX_ALLOW_BYPASS=1` opt-in. No setting changes the native sandbox or tool approvals.

Use the same `AX_HOME` as the local agents you want to reach. An older running broker will reject attachment with an update instruction; installing a binary alone does not replace that broker.

An attached runtime has messaging tools but cannot launch peers through AX. Attachment itself is not authorization to start another agent.

## Receive on a user turn

Without a wake adapter, discovery reports `wake=user_turn` and state `user-turn` after tool discovery. Ask the agent to check AX mail. It calls `check_inbox` once, receives the next permitted queued message, and replies or acknowledges normally. It can also call `list_notifications` once for delivery failures.

The agent must not hold a turn open or repeatedly check for replies. AX's tool instructions make this limitation explicit. `list_pending` remains a read-only recovery tool; it neither delivers queued content nor acknowledges anything. If a receive response is lost, use it to locate already-fetched messages before requesting another item. Uncertain delivery blocks later FIFO handoffs until explicitly recovered.

## Add automatic wake

A native plugin can supply a private Unix socket with `-w /absolute/path/to/wake.sock`. The socket must be owned by the current OS user with mode `0600`. AX sends:

```http
POST /wake
Content-Type: application/json
```

```json
{
  "native": "conversation_123",
  "text": "AX peer message waiting. Call the MCP tool ax.get_message ..."
}
```

The text contains fixed AX instructions and generated IDs. Peer message bodies remain in the shared messaging tools. Native tool prefixes vary, so the plugin should translate the `ax.` prefix if its runtime names MCP tools differently.

Before accepting a wake, the plugin must verify the conversation ID and submit the notification through its native API. Return HTTP `204` only after the runtime confirms acceptance. Other statuses, disconnects, or the 15-second deadline produce uncertain delivery. AX does not automatically repeat an uncertain wake. A failing wake adapter leaves the native host running and AX tools available.

This is the same callback contract used by built-in adapters. An integration compiled into AX can instead use `startAdapterHost` with a native callback, or its existing `/next` and `/receipt` interface for a listener that waits for wakes. Neither approach needs a separate broker or mailbox.

## Report lifecycle changes

The native MCP client can update its permission and activity state through a notification on the child's stdin:

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/ax/presence",
  "params": {
    "native_session_id": "conversation_123",
    "permission_mode": "read-only",
    "state": "busy"
  }
}
```

Valid states are `ready`, `busy`, and `blocked`. Send updates when native permissions change. The conversation ID is immutable. These fields belong to the host protocol; model-supplied tool arguments cannot change them. Closing the MCP child releases the AX name, while the existing broker cleanup archives the endpoint and preserves its mailbox for resume.

## Verify the integration

```sh
ax agents
ax doctor
ax verify planner
```

Discovery separates tool readiness from wake mode and confirmation semantics. A channel write proves less than native host acceptance; neither proves that the model received the request or replied.

`ax verify` sends one harmless challenge, then waits up to 60 seconds for a reply from that exact agent containing the expected token. An acknowledgment, wake receipt, wrong token, or old reply cannot pass. For a manual endpoint, give the agent a user turn to check its inbox while verification runs. The command uses the reserved local name `ax-verifier` and does not launch a coding agent. It reports its result for this exchange, not a permanent health guarantee.

Adapter authors should also verify fresh attachment, resume, concurrent-name rejection, host permission changes, busy delivery, disconnect recovery, and uncertain wake handling. Kill only the adapter child during a failure test and confirm the native conversation remains usable. Tests in `internal/ax/attach_test.go` exercise the shared contract with a simulated MCP host and native wake callback; they are not live Muse, Grok Bot, or ChatGPT certification.

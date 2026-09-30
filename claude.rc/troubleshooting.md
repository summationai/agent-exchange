# Delivery and recovery

Start by checking the agents and installed harnesses from a terminal:

```sh
ax agents
ax doctor
```

## An agent is missing

Confirm that the session was launched through AX with the name you expect. Names work across repositories, but the sessions must share the same OS user and AX state directory. `AX_HOME` can select a separate directory for testing; agents using different state directories do not share a broker. The `list_agents` result names the runtime it read, so an agent can compare `ax_home` without leaving its session.

On Windows, AX and both harnesses must run in the same WSL 2 distribution. If the harness is missing from doctor, install and sign in to that harness separately.

`ax agents` names the condition. `unstarted` means the session enrolled but its harness never reported it running; `inactive` means the harness is running but AX has not completed MCP tool discovery and native session confirmation.

Claude and Codex become reachable after their native startup confirmation and MCP tool discovery, including resumed conversations. No model setup turn is needed. Older AX releases may need a single `list_agents` call to finish connecting. Do not leave the agent polling for another agent to join.

## A resumed conversation has a binding conflict

An AX name belongs to one saved conversation. If the native resume picker selects a different conversation, AX holds incoming mail and reports `binding-conflict`. The diagnostic includes the saved and selected conversation IDs and appears in `ax agents`, `ax doctor`, and AX tool errors. A late lifecycle hook cannot silently clear it.

Exit the affected session normally. To return to its saved conversation, launch the same name without a resume argument:

```sh
ax claude -n api
```

To adopt the other conversation, use an unused AX name with the harness's native resume command:

```sh
ax claude -n api-resumed -r CONVERSATION_ID
```

Codex follows the same rule with `ax codex -n web-resumed resume CONVERSATION_ID`. AX rejects an unambiguous leading resume UUID that conflicts with a saved name before enrolling or starting the harness. Names and picker selections are checked when the harness confirms its selection.

Saved mail stays with the original AX identity. AX does not delete a name, rebind it, or replay its mail into another conversation. Even a startup hook for a conversation whose transcript has not yet been created does not authorize rebinding; its conflict is reported so the user can choose the intended conversation explicitly.

## A message is queued

Queued means the broker stored the message. It does not mean the recipient has read it or completed the task. An offline recipient can receive stored mail when its named session returns, subject to expiration.

Inspect a specific message using the ID returned by AX:

```sh
ax status MESSAGE_ID
```

Delivery evidence progresses from queued to native handoff, content fetch, and acknowledgment. An acknowledgment records receipt. Ask for an explicit result when you need proof that a review, test, or other task finished.

AX 0.6.1 notifies the sender when queued mail expires or is refused. These notifications report delivery failure; they do not resend the task.

## The message is held by policy

An unknown permission mode or an unapproved bypass mode can hold mail. The receiving harness’s permissions still apply to delegated work. Inspect the launch settings before changing policy.

The terminal owner can deliberately hold or accept incoming mail:

```sh
ax policy api hold
ax policy api accept
```

These commands change AX’s incoming-mail policy; they do not grant native tool permissions or disable a sandbox. See [permissions and delegation](README.md#permissions-and-delegation).

## A handoff is uncertain

AX does not automatically repeat a handoff whose native acceptance is uncertain. Repeating it could execute the task twice. Later mail can wait behind that handoff.

Inspect its status and the recipient’s conversation first. If you deliberately choose to abandon that handoff:

```sh
ax resolve MESSAGE_ID abandon
```

Abandonment releases the queue. It does not cancel work the harness already accepted and does not claim the message was delivered.

## After an update

Replacing the binary does not replace already-running processes. The broker and relaunched sessions must both use the new binary. Follow the [running-session update instructions](../AGENTS.md#updating-running-sessions), preserving saved AX names and native conversations.

The [resource-protection guide](resource-safety.md) describes the helper cooldown and CPU accounting included in AX 0.6.1. Run `ax doctor` to see whether messaging is paused.

## Report a problem

Include `ax version`, your OS, the harness and its version, and the relevant agent names and message IDs. Share the observed delivery state and what you expected. Redact message bodies or logs that contain private code, credentials, or customer data.

[Open an issue on GitHub](https://github.com/summationai/agent-exchange/issues).

## External mail waits

An `external policy` error rejects relayed mail before storage. The owner can
open the recipient with `ax external NAME verified|all`. A queued relay message
held by a subsequent policy change waits at its FIFO head until reopening or TTL.
A bridge lacking external guidance must be updated and relaunched before external
handoff; `ax status` and inbox details report the waiting reason. Ordinary peer
mail does not pass through the external-policy gate.

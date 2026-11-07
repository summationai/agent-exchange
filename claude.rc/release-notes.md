# Agent Exchange 0.8.2

New Claude panes now receive one fixed AX setup prompt before queued tasks are handed to the channel. This addresses the startup gap where MCP connected but the first task could wait until someone typed into the child pane.

## Claude pane startup

For recognized option-only Claude pane launches, AX waits until the model calls an AX tool before activating queued delivery. That call clears the startup hold durably, even if the request then fails during a resource cooldown or broker outage. Normal native binding, current launch ownership, permissions, and cooldown checks still apply.

Reconnecting the bridge does not repeat the setup turn or replay a task. Explicit native prompts, resume commands, and unfamiliar syntax pass through without an added prompt. Debug and strict MCP configuration options are covered. Business task text remains in AX's mailbox.

Covered launches add one model setup turn. Native login, workspace trust, tool approvals, and Channel confirmation remain under the harness's controls.

## Install and update

```sh
curl -fsSL https://useax.dev/install.sh | sh
```

Or ask your agent to follow [AGENTS.md](https://useax.dev/agents.md). Prebuilt binaries support macOS and Linux on ARM64 and x86_64. Windows uses WSL 2.

The startup fix needs the updated launcher and bridge. Installing the binary does not replace a running bridge. Let existing work finish, close the AX-launched session normally, and relaunch its saved name before testing a new pane. See the [running-session update guide](https://useax.dev/docs/installation#updating-running-sessions) for a complete runtime update.

There is no mailbox schema change from 0.8.0 or 0.8.1. Upgrading from 0.7.x still requires the schema-4 backup precautions in that guide; a 0.7.x broker cannot open the upgraded mailbox.

## Verification

Regression tests cover queued first-task release, a failed setup call during cooldown, stale-launch protection, native argument preservation, and no duplicate offer after bridge replacement. The release workflow checks race tests, vet, four native builds, installer behavior, and WSL installation.

These checks do not establish native cold-start receipt on every machine. [Issue 82](https://github.com/summationai/agent-exchange/issues/82) stays open for live verification and the separate idle-wake cases. The missing-server banner alone does not identify the cause of a stalled session.

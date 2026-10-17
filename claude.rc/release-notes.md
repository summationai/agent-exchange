# Agent Exchange 0.8.1

Reuse an AX name when starting or selecting a different native conversation. AX keeps the same agent identity and mailbox, and launching the name without extra arguments resumes its latest selected conversation. A previous launch cannot overwrite the new selection.

## Fixes

- Reusing a name no longer blocks Claude startup when the harness selects a new conversation. Launch ownership also protects Codex, Grok, OpenCode, and Pi from stale session updates.
- Child launches receive their own launch identity. Attaching an independent runtime from inside another AX session preserves the attachment's separate identity.
- Owners can abandon obsolete queued mail with `ax resolve MESSAGE_ID abandon`. AX retains the history and stops future automatic delivery attempts. This does not cancel an in-flight transfer or recall a message already accepted by another machine or harness. Agent tools cannot abandon messages.
- Attached runtimes without delivery configuration remain blocked from sending, including when bypass delivery is enabled.

## Install and upgrade

```sh
curl -fsSL https://useax.dev/install.sh | sh
```

Or ask your agent to follow [AGENTS.md](https://useax.dev/agents.md). Prebuilt binaries support macOS and Linux on ARM64 and x86_64. Windows uses WSL 2.

The conversation-selection fix needs the updated broker as well as the binary. Follow the [running-session update guide](https://useax.dev/docs/installation#updating-running-sessions): finish active work, close AX sessions normally, stop only the verified broker, back up its private state, and relaunch the saved names. Installing the binary alone does not replace a running broker or bridge.

There is no mailbox schema change from 0.8.0. Upgrading from 0.7.x still requires the schema-4 precautions in that guide; a 0.7.x broker cannot open the upgraded mailbox.

Optional Tailscale messaging and its existing limits are unchanged. See [Tailscale setup](https://useax.dev/docs/tailscale) and the [verification record](https://useax.dev/docs/verification). The release workflow checks four native builds, race tests, vet, installer behavior, and installation inside WSL 2. These checks do not establish live harness interoperability on every platform.

# Agent Exchange 0.9.0

## Antigravity CLI

Run `AX_ALLOW_BYPASS=1 ax agy -n helper` to connect a new Antigravity session. AX saves delivery consent for that name and preserves Antigravity's native permission settings. Run `ax agy -n helper` again to resume the same conversation.

Antigravity checks queued messages on user turns. A message that arrives while idle waits for the next turn. Request and reply were verified with Antigravity CLI 1.5.17 on macOS, including after resume. See [Antigravity setup and delivery limits](https://useax.dev/docs/harnesses#antigravity-cli).

## Message previews

Claude Code and Pi channel messages start with the sender, a short excerpt, and the local send time.

```text
Received from web · "PR is ready" · 14:32
```

Send, reply, follow-up, and resend receipts show the recipient in the same format. `Sent to api` confirms submission to AX. The receipt reports the observed delivery stage.

## Install and update

```sh
curl -fsSL https://useax.dev/install.sh | sh
```

Restart the AX broker and relaunch named sessions to load the new adapter and previews. This release keeps the same mailbox schema. Follow the [running-session update guide](https://useax.dev/docs/installation#updating-running-sessions).

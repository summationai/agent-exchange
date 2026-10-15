# Agent Exchange 0.7.2

AX 0.7.2 fixes permission opt-in on resumed and spawned sessions and prevents a Claude transition that silently loses incoming messages.

- A named session keeps its saved AX bypass opt-in when the environment has no override. An explicit `AX_ALLOW_BYPASS=0` still disables it.
- Explicit native Claude bypass arguments opt a spawned peer into AX messaging. Ordinary permission modes and the option that merely makes bypass available do not opt in. Spawned peers still do not inherit a parent's bypass opt-in implicitly.
- AX disables Claude's agent view for sessions it launches. Claude can otherwise re-host the conversation without its channel flag, leaving AX tools connected while incoming messages fail to wake the model. Background tool tasks retain their native behavior. See [the upstream report](https://github.com/anthropics/claude-code/issues/90160).

## Update

```sh
curl -fsSL https://useax.dev/install.sh | sh
ax version
```

Existing processes keep running with their previous code. Relaunch saved AX names after active work finishes to apply these launcher fixes. A Claude session already re-hosted without its channel needs to be closed, including its background copy, before resuming through AX. Installation does not repair a changed native conversation binding or replay its messages.

This patch retains mailbox schema 3 and the existing broker protocol. Native harness permissions remain in force. Claude Code, Codex CLI, Grok Build, OpenCode, and Pi remain supported on macOS and Linux; Windows uses WSL 2. No Tailscale or 0.8 features are included.

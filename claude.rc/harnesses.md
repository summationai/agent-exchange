# Supported harnesses

AX connects Claude Code, Codex CLI, Grok Build, OpenCode, Pi, and Antigravity CLI. Each integration loads shared messaging tools and uses native APIs to identify the selected conversation. Its delivery boundary depends on the harness.

## Launch commands

| Harness | Start a named session | Native integration |
| --- | --- | --- |
| Claude Code | `ax claude --name api` | MCP tools and a development Channel |
| Codex CLI | `ax codex --name web` | MCP tools and a private native app server |
| Grok Build | `ax grok --name reviewer` | MCP tools and a private native leader |
| OpenCode | `ax opencode --name editor` | MCP tools and a per-launch TUI plugin |
| Antigravity CLI | `AX_ALLOW_BYPASS=1 ax agy -n helper` | Native MCP plugin and lifecycle hooks; next-turn delivery while idle |
| Pi | `ax pi --name worker` | Native extension and an optional messaging child |

Names work across repositories for the same OS user. AX consumes the name option and passes other arguments to the native harness. [Resume commands](README.md#launch-and-resume) work with existing conversations.

A harness that names its own sessions also receives the AX name, so the agent is identifiable in its native interface. Claude Code is the only one today: the name appears in its prompt box, `/resume` picker, and terminal title. A Claude release without that option still launches without a native name.

## Integration limits

Claude uses an experimental Channel and requires development Channel confirmation at launch. Organization policy can disable Channels.

Codex requires native queue support. AX owns the private app-server connection used by this integration; complete crash isolation for that backend remains pending.

Grok runs through its native leader. Leader-mode restrictions apply, and standalone-only flags and sandbox modes need further adapter work. Complete backend crash isolation is also pending for Grok.

OpenCode uses the full TUI plugin API. Its pure and mini modes do not load AX’s adapter. An existing `OPENCODE_TUI_CONFIG` override must currently contain JSON.

Pi uses its native extension API. Messaging failures leave Pi running. Starting a new conversation or fork disables messaging for the original AX name; use a new name for the new conversation. Pi 0.86.1 is tested. See [Pi setup and limits](pi.md).

Forwarding an argument does not guarantee that every native mode supports messaging.

Antigravity uses a global AX plugin that exposes no tools outside a matching AX launch. The agent checks queued mail through the inbox tool on user turns. Remote-agent mode and pane spawning are unsupported. See [Antigravity setup and verification](#antigravity-cli).

## Platforms and tested versions

Prebuilt AX binaries support macOS 13 or later and Linux, on ARM64 and x86_64. Windows uses WSL 2; AX and the harnesses must run in the same distribution.

Claude, Codex, Grok, OpenCode, and Pi have completed live exchanges on macOS. Pi’s live checks used its RPC mode; interactive TUI rendering remains unverified. Binary and broker checks cover macOS, Linux, and installation inside WSL 2. Live harness exchanges on Linux and WSL remain unverified. The [verification record](verification.md) lists exact versions and evidence.

## Another harness?

Adapters currently require a source contribution and rebuild. AX does not yet offer installable adapter packages or a scaffolding command.

The [adapter guide](adapters.md) explains the native capabilities, lifecycle contract, and checks needed to add one. A new adapter reuses the broker, mailbox, and messaging tools.

## Antigravity CLI

Start with `AX_ALLOW_BYPASS=1 ax agy -n helper` to authorize AX delivery once for this name. Later launches use `ax agy -n helper`. Antigravity does not attest its live approval mode, so AX records it as unverified and requires saved consent. Native flags control Antigravity permissions independently of this opt-in. Resume a native conversation with `ax agy -n helper -r CONVERSATION_ID`, or repeat the AX name without extra arguments to resume its saved conversation. Other arguments, including model and execution mode, pass through to the native CLI.

On first launch AX writes its managed plugin under `~/.gemini/config/plugins/ax`. It preserves other plugins and refuses to replace an unrelated plugin at that path. Ordinary Antigravity sessions get an inactive MCP server with no AX tools. AX identities and credentials remain in AX_HOME rather than global plugin configuration.

The native SessionStart hook supplies the selected conversation ID. Native hooks bind the selected conversation and report busy or ready state. AX asks the agent to call its shared MCP check_inbox tool once on each new user turn. That tool applies AX delivery policy and returns the original message and guidance. Hooks do not fetch or acknowledge peer content and never force the agent to continue working.

There is no supported idle wake integration in this adapter: mail arriving after a turn ends waits for another user turn. Ask the agent to check AX mail if needed. Do not poll. Antigravity retains its native tool permissions and sandbox. AX replaces its launcher with the native CLI, so a messaging helper failure leaves the terminal session running.

Remote-agent mode, the multi-session ACP server, automatic pane spawning, and custom Antigravity data roots are unsupported. If the plugin was disabled, enable AX in Antigravity's plugin manager and reload its MCP servers. A different conversation needs a different AX name.

This integration requires a broker built with Antigravity support. Installing a new CLI alone does not update an older running broker. See the [update procedure](../AGENTS.md#updating-running-sessions).

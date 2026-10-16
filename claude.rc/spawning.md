# Launch a peer in another pane

Pane launching is included in AX 0.6.1 for Claude Code, Codex CLI, Grok Build, and OpenCode. Start Pi directly with `ax pi --name NAME`; Pi pane spawning is not supported yet.

Ask an AX agent: **“Launch a Codex agent named reviewer in a new pane, and ask it to review this change.”** The agent uses `spawn_agent`, then sends the task through the ordinary AX mailbox. The new conversation appears beside yours and keeps the name `reviewer` for replies.

Agents may launch another agent only when you explicitly ask them to. A task assignment, a peer asking for help, or an opportunity to parallelize does not authorize a launch. The same rule applies to further launches requested of the new agent. AX includes this policy in its server and tool instructions. Native tool approval settings still apply; AX cannot independently infer your intent from a model's tool call.

## Terminal selection

AX detects the terminal when your named session starts. Inside tmux, it splits that exact pane on the same tmux server. Otherwise, on macOS, it splits that exact iTerm2 session. Moving focus to another window does not change the target. Missing or stale targeting information produces an error rather than opening an unrelated window.

iTerm2 uses its built-in AppleScript API. macOS may ask you to allow automation access. tmux uses `split-window` and works on macOS, Linux, and WSL. Both create a side-by-side pane. No terminal keystrokes or transcript scraping are used.

The terminal starts the new AX session independently of the requesting bridge. Losing that bridge or the broker does not close the new pane. Each harness still has its existing integration limits: Codex and Grok use AX-owned native backends, so this feature does not provide complete crash isolation for those adapters.

## CLI and agent tool

From an interactive terminal:

```sh
ax spawn codex --name reviewer
ax spawn claude --name api --cwd /absolute/path/to/project
ax spawn-status reviewer
```

AX consumes `name` and `cwd` for this command and forwards the other arguments to the native harness. From an AX agent, prefer the structured tool:

```json
{
  "harness": "codex",
  "name": "reviewer",
  "cwd": "/absolute/path/to/project",
  "args": []
}
```

The working directory defaults to the calling agent's workspace. Native arguments are a string array; send the delegated task separately with `send_message`. Task text stays in AX's durable mailbox. The launcher does not copy the caller's AX identity, implicit permission bypass, or credential environment into the new session. Native harness sign-in and configuration remain in effect. Explicit native arguments can select permissions, just as with an ordinary AX launch.

## Readiness and retries

Creating a pane does not mean its agent is ready. The result includes a launch phase and a separate `ready` value. Readiness requires a selected native conversation and an active AX messaging bridge. For option-only Claude pane launches using recognized native options, AX supplies one fixed startup prompt that calls its discovery tool. The bridge holds queued tasks until that first model tool call, rather than treating MCP discovery alone as proof that Claude can consume its channel. Reconnecting that bridge does not repeat the startup turn. Native prompts, resume commands, and unfamiliar argument syntax pass through unchanged without an added prompt. A new session may still need login, trust, or Claude's development Channel confirmation. Mail can queue while those steps are pending; a reply proves the peer handled the task.

The name is the retry key. Repeating the same launch from the same root session with the same arguments inspects its existing reservation. It does not create another pane. Changing the arguments under that name is an error. Use `ax spawn-status NAME` to inspect a reservation from an earlier root session. Names with existing saved conversations must use the normal resume command, not spawn.

An unconfirmed terminal response is reported as uncertain. Inspect the terminal and `ax spawn-status NAME`; do not retry under a different name. If the session exits, its reservation remains for inspection, and normal AX resume syntax can reopen the saved conversation.

Each root AX session can request at most eight pane launches across its whole tree, including failed or uncertain attempts. Nesting is limited to three levels. Repeating the same reservation consumes no additional slot. Relaunching a root session starts a new budget. Manual CLI launches outside an AX session share a budget for the originating terminal pane. These bounds prevent accidental launch cascades; they are not a security boundary against a process that can run arbitrary shell commands.

Claude may show `server:ax · no MCP server configured with that name` even when the inline MCP server is connected. This matches [an upstream banner-validation report](https://github.com/anthropics/claude-code/issues/100865). Check MCP connectivity and message receipts before diagnosing missing configuration. A channel write alone does not prove model receipt; an acknowledgment or reply does. If an older session misses a wake, ask it to call `list_pending` once and handle the existing message, rather than resending the task.

## Verification

Automated tests use stub harnesses and terminal responses. They cover duplicate and concurrent requests, uncertain outcomes, tree limits, native readiness, argument preservation, permission handling, and session identity. Real iTerm2/tmux pane launches and a Claude-to-Codex round trip still require a user-requested live test. No coding sessions are launched automatically for that verification.

Terminal contracts: [iTerm2 scripting](https://iterm2.com/documentation-scripting.html) and [tmux manual](https://man.openbsd.org/tmux).

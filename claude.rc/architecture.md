# How Agent Exchange works

```text
Claude Code     Codex CLI     Grok Build     OpenCode
     ↕              ↕             ↕             ↕
native Channel  app server   native leader   TUI plugin
     ↕              ↕             ↕             ↕
External bridges ↔ AX relay MCP children
              AX MCP bridges
                    ↕
         one private local Go broker
                    ↕
               SQLite mailbox
```

`ax HARNESS --name NAME` enrolls a named endpoint and launches the ordinary harness. The native harness selects or resumes its conversation. AX binds that native ID to the name, loads six messaging tools, and establishes readiness. Names are local to the OS user and work across repositories.

The broker validates the sender and target, stores the message transactionally, and offers it to the recipient's connected bridge. Claude receives an authenticated Channel event containing the message. Codex, Grok, and OpenCode receive a fixed wake instruction with a generated message ID, then fetch the body through MCP. Arbitrary peer text does not enter their user-prompt APIs.

The recipient replies by message ID. The broker resolves the original sender and atomically stores the reply while acknowledging the request. Agents finish their turn after sending; the broker wakes them for replies. The model never polls the mailbox.

The broker distinguishes storage, native acceptance, content fetch, and model acknowledgment. A native receipt releases subsequent handoffs, independently of task completion. If native acceptance is uncertain, AX keeps a barrier and avoids automatic reinjection. Recorded receipts also preserve confirmed handoffs across upgrades from older brokers.

Each connection is fenced by a lease epoch. A stale bridge cannot acknowledge a new connection's offers. The broker uses a private Unix socket and SQLite WAL; session files contain per-endpoint credentials with owner-only permissions. MCP exposes messaging operations, never enrollment, lifecycle, policy changes, or recovery controls.

Harness adapters share the broker, message schema, MCP tools, and delegation policy. They own native configuration, immutable conversation binding, readiness, wake acceptance, and cleanup. Claude and Codex retain their existing paths; the new adapter registry allows other harnesses to use the same messaging core.

Relay endpoints add an external-data path alongside local peer delegation. Only
a relay can assert provenance; the recipient owner's external policy defaults to
refuse, and guidance follows provenance. Opaque structured data travels with the
text. Attached hosts may receive messages, sender notices, and coalesced agent
changes over their MCP pipe. See [the relay contract](relay.md).

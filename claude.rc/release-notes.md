# Agent Exchange 0.8.4

Agent discovery separates active mail from overdue records. A message handed to the harness can stay unacknowledged after its delivery deadline. Those older receipts now appear in the overdue count and stay available in mailbox history. An in-progress or uncertain handoff remains visible because it can still block later mail.

The messaging tools report the latest observed delivery stage. A send receipt shows what the broker knew when it returned. Use `ax status MESSAGE_ID` or the `delivery_status` tool for a fresh view. A channel write, a message fetch, and an acknowledgment each provide different evidence of receipt. Task completion is tracked separately.

## Install and update

```sh
curl -fsSL https://useax.dev/install.sh | sh
```

The broker needs a restart to use the corrected counts. Relaunch your saved AX sessions to load the updated messaging tools and instructions. Let active work finish first and follow the [running-session update guide](https://useax.dev/docs/installation#updating-running-sessions). AX keeps saved conversations and mailbox history. This release adds no mailbox schema version change.

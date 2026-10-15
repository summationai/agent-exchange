# Across machines with Tailscale

AX 0.8.0 can connect named coding agents on explicitly paired Tailscale machines. Ask `reviewer@buildbox` for a review and receive the reply in your current conversation. Each machine keeps its own local broker and mailbox.

Local messaging remains the default. Installing or updating AX does not pair machines or start a network listener.

## Before pairing

Install AX 0.8.0 or later and sign in to your coding harness on each machine. Both machines need the installed Tailscale service connected to your tailnet, with its CLI on PATH. Check `tailscale status` on each machine. AX does not configure Tailscale accounts, grants, firewalls, Serve, or Funnel.

The network adapter binds only to a Tailscale address. Allow the paired machines to reach the selected TCP port through your tailnet's access policy. The default port is 48889. Tailnet membership alone does not grant AX access: pairing pins a certificate for one AX installation belonging to one OS user.

## 1. Create public pairing cards

On the laptop:

```sh
ax network init laptop > laptop.json
```

On the build machine:

```sh
ax network init buildbox > buildbox.json
```

These files are public cards, not private keys. Exchange them through a channel you already trust, such as authenticated SSH. Each machine should now have both cards. Keep the private `network/identity.json` file in the AX state directory on its original machine.

Initialization is idempotent. `ax network identity` prints an existing public card. For another port, pass it after the machine name, for example `ax network init buildbox 48890`.

## 2. Choose which local agents the peer can reach

This example uses a local agent named `api` on the laptop and `reviewer` on the build machine.

On the laptop, export the laptop's `api` to the build machine:

```sh
ax network trust buildbox buildbox.json api
```

On the build machine, export its `reviewer` to the laptop:

```sh
ax network trust laptop laptop.json reviewer
```

The names after the card filename are **local agents permitted to that peer**, not remote targets. Each trust command prints the certificate fingerprint; verify it with the other machine before granting messaging access. To explicitly export every local agent, use a quoted `'*'` instead of a list of names.

Pairing initially grants discovery only. Enable messaging separately on both sides:

```sh
# Laptop
ax network messaging buildbox on

# Build machine
ax network messaging laptop on
```

A messaging grant permits scoped delegation to those exported agents. It preserves each receiving harness's sandbox and approval controls. Relayed external content retains its provenance and the recipient's external-content policy.

## 3. Run the adapters and agents

Keep this command running on each machine, in a separate terminal or under your process supervisor:

```sh
ax network serve
```

There is no automatic network-service installation. Start the adapter again after reboot or arrange that through your operating system's process supervisor. Stopping it leaves native coding sessions and local AX messaging running.

Launch your agents in their own terminals:

```sh
# Laptop
ax claude -n api

# Build machine
ax codex -n reviewer
```

On the laptop, run `ax network agents buildbox` to check the peer's exported agents. `ax agents` and the MCP `list_agents` tool also include permitted remote agents after discovery, which refreshes every 30 seconds.

Ask the laptop agent: **“Ask reviewer@buildbox to review my changes.”** On the build machine, the laptop agent is `api@laptop`. Unqualified names continue to address local agents. Any supported harness can use these names.

## When a machine is unavailable

Each installation stores messages in its own `AX_HOME/mailbox.db`, normally `~/.ax/mailbox.db`. There is no shared inbox or AX-hosted mailbox.

- Before transfer, the sender keeps the outgoing message durably queued. A previously discovered remote identity can still be addressed; a new remote agent must be discovered before AX can resolve its name.
- After the destination commits a message, that machine owns the receiving inbox. If it goes offline, its stored message remains there. A lost network receipt is retried with the same identity and deduplicated at the destination.
- Retries use exponential backoff with jitter, up to about 75 seconds between attempts. New messages cannot bypass outage backoff. Expiry still applies: the default lifetime is 12 hours.
- A confirmed native handoff and an uncertain handoff remain different. AX does not automatically rerun work that might already have executed. Delivery acknowledgment is not task completion. Remote task outcomes are not yet synchronized back into the sender's receipt.

Use `ax status MESSAGE_ID` on the sender and `ax inbox` on either machine to inspect delivery. Remote discovery is cached, so a listed name is not proof that its machine is currently reachable. Power loss can also leave an executing task incomplete; AX does not resume the coding harness itself.

## Pause or revoke access

Hold later transfers and incoming native handoffs without removing the pairing:

```sh
ax network messaging buildbox off
```

Remove the laptop's trust in the build machine:

```sh
ax network revoke buildbox
```

Run the corresponding command on the other machine to remove its grant too. Revocation is checked on subsequent requests, including an existing TLS connection. A request already authorized may finish its bounded response. Revocation does not erase mail or undo completed work.

## Privacy, credentials, and rotation

AX has no hosted relay or telemetry. Optional remote messaging leaves the originating machine over encrypted Tailscale paths, which may use Tailscale's encrypted relay transport when a direct path is unavailable. Your coding harnesses still communicate with their model providers as usual.

The certificate and private key live in `AX_HOME/network`, alongside pairing policy. Keep this directory owner-only and back it up privately. Certificates last one year. Automatic renewal is not included in 0.8.0.

To rotate an installation's certificate, stop its network adapter and preserve its existing `network` directory as a private backup outside that path. Run `ax network init` again with the same machine name and port to create a new identity. Exchange fresh public cards. Each peer must explicitly revoke the old pairing, trust the new card, and grant messaging again. Re-create the rotating installation's outgoing pairings too, then restart its adapter. Keep the existing mailbox and saved sessions. Messages bound to the old certificate remain held; do not replay them automatically or assume the replacement certificate inherits their identity.

## Current boundaries

Remote mode carries messages between paired brokers. It does not expose remote shell, spawning, process termination, configuration, or recovery APIs. Launch a remote coding session yourself, or explicitly ask an authorized agent to launch it through SSH; AX can coordinate that session afterward.

The cross-machine live record covers two Apple Silicon Macs, native Codex and Grok, and network-adapter restart recovery. Linux transport tests run in CI; live two-machine Linux/WSL exchanges and suspend/resume remain unverified. See [the verification record](verification.md). The internal A2A binding is experimental; this release does not claim general A2A conformance.

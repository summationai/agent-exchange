export const pages = [
  { slug: 'relays', source: 'claude.rc/relay.md', title: 'Relay endpoints', group: 'Reference', description: 'Deliver external data with provenance, an owner policy, and MCP notifications.' },
  { slug: '', source: 'claude.rc/getting-started.md', title: 'Your first exchange', group: 'Get started', description: 'Give two agents names. Send your first message between them.' },
  { slug: 'installation', source: 'AGENTS.md', title: 'Install Agent Exchange', group: 'Get started', description: 'Prebuilt binaries for macOS and Linux. Windows users can join through WSL 2.' },
  { slug: 'sessions', source: 'claude.rc/README.md', title: 'Sessions and permissions', group: 'Get started', description: 'Launch, resume, and delegate work while keeping your harness’s native controls.' },
  { slug: 'harnesses', source: 'claude.rc/harnesses.md', title: 'Supported harnesses', group: 'Get started', description: 'Choose an integration and understand its current limits.' },
  { slug: 'troubleshooting', source: 'claude.rc/troubleshooting.md', title: 'Delivery and recovery', group: 'Reference', description: 'Find your agents, understand delivery status, and investigate a stuck handoff.' },
  { slug: 'architecture', source: 'claude.rc/architecture.md', title: 'How AX works', group: 'Reference', description: 'One local broker, a durable mailbox, and native integrations for each harness.' },
  { slug: 'adapters', source: 'claude.rc/adapters.md', title: 'Add a harness', group: 'Reference', description: 'Connect a harness’s native lifecycle to the existing messaging core.' },
  { slug: 'verification', source: 'claude.rc/verification.md', title: 'Tested versions', group: 'Reference', description: 'Recorded live exchanges, platform checks, and the boundaries of that evidence.' },
  { slug: 'pi', source: 'claude.rc/pi.md', title: 'Pi', group: 'Get started', description: 'Connect Pi through its native extension API, with an independent messaging helper.' },
  { slug: 'inbox', source: 'claude.rc/inbox.md', title: 'Separate inbox', group: 'Coordination', description: 'Watch your agents’ messages in a separate terminal.' },
  { slug: 'spawning', source: 'claude.rc/spawning.md', title: 'Launch a peer', group: 'Coordination', description: 'Open a named peer in tmux or iTerm2 when you request a new agent.' },
  { slug: 'resource-safety', source: 'claude.rc/resource-safety.md', title: 'Resource protection', group: 'Reference', description: 'CPU accounting, bounded retries, and the limits of AX’s resource controls.' },
];
export const docPath = page => '/docs' + (page.slug ? '/' + page.slug : '');

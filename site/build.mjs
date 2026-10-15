import { mkdir, copyFile, readFile, writeFile, rm } from 'node:fs/promises';
import { posix } from 'node:path';
import MarkdownIt from 'markdown-it';
import { pages, docPath } from './pages.mjs';

const root = new URL('../', import.meta.url);
const output = new URL('./dist/', import.meta.url);
const github = 'https://github.com/summationai/agent-exchange';
const md = new MarkdownIt({ html: false });
const escape = md.utils.escapeHtml;
const header = `<a class="skip-link" href="#main">Skip to content</a>
<header class="site-header"><div class="header-inner wrap">
<a class="brand" href="/" aria-label="Agent Exchange home"><span class="brand-symbol">ax<span aria-hidden="true">↗</span></span><span class="brand-name">Agent<br>Exchange</span></a>
<nav class="main-nav" aria-label="Main"><a href="/docs">Docs</a><a href="${github}">GitHub <span aria-hidden="true">↗</span></a><a class="nav-install" href="/docs/installation">Get started</a></nav>
</div></header>`;
const footer = `<footer class="site-footer"><div class="footer-inner wrap"><p>Agent Exchange · Built by the <a href="https://summation.com">summation.com</a> team.</p><nav aria-label="Footer"><a href="/docs">Docs</a><a href="/agents.md">agents.md</a><a href="${github}">GitHub ↗</a><a href="${github}/blob/main/LICENSE">MIT license</a></nav></div></footer>`;

function shell(title, description, path, body) {
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>${escape(title)} — AX Docs</title><meta name="description" content="${escape(description)}"><meta name="theme-color" content="#f7f6f0">
<link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="stylesheet" href="/style.css"><link rel="canonical" href="https://useax.dev${path}"><script src="/site.js" defer></script>
</head><body>${header}${body}${footer}</body></html>`;
}

function navigation(current) {
  return `<aside class="docs-sidebar"><details class="docs-menu" open><summary>Browse the docs</summary><div class="docs-menu-content">
<label class="page-finder" hidden><span>Find a page</span><input type="search" data-page-finder placeholder="Sessions, adapters…" autocomplete="off"></label>
<nav aria-label="Documentation">${[...new Set(pages.map(page => page.group))].map(group => `<div class="docs-nav-group"><h2>${group}</h2>${pages.filter(page => page.group === group).map(page => `<a href="${docPath(page)}" data-doc-link${page === current ? ' aria-current="page"' : ''}>${escape(page.title)}${page.unreleased ? '<small>UNRELEASED</small>' : ''}</a>`).join('')}</div>`).join('')}</nav>
<p class="empty-search" role="status" hidden>No matching pages. Try “sessions” or “install”.</p></div></details></aside>`;
}

// Repository links become local docs links where a rendered page exists.
const renderLink = md.renderer.rules.link_open || ((tokens, index, options, env, self) => self.renderToken(tokens, index, options));
md.renderer.rules.link_open = (tokens, index, options, env, self) => {
  const token = tokens[index];
  let href = token.attrGet('href');
  const repoPrefix = github + '/blob/main/';
  if (href.startsWith(repoPrefix)) href = href.slice(repoPrefix.length);
  else if (!/^(?:[a-z]+:|\/|#)/i.test(href)) href = posix.join(posix.dirname(env.source), href);
  else return renderLink(tokens, index, options, env, self);
  const [path, fragment] = href.split('#');
  const target = pages.find(page => page.source === path);
  token.attrSet('href', (target ? docPath(target) : repoPrefix + path) + (fragment ? '#' + fragment : ''));
  return renderLink(tokens, index, options, env, self);
};
md.renderer.rules.table_open = () => '<div class="table-scroll"><table>\n';
md.renderer.rules.table_close = () => '</table></div>\n';

await rm(output, { recursive: true, force: true });
await mkdir(new URL('docs/', output), { recursive: true });
await mkdir(new URL('fonts/', output));
await mkdir(new URL('media/', output));
for (const file of ['ax-demo.mp4', 'ax-demo-poster.jpg']) {
  await copyFile(new URL('media/' + file, import.meta.url), new URL('media/' + file, output));
}
for (const file of ['JetBrainsMono-Regular.woff2', 'JetBrainsMono-Bold.woff2', 'OFL.txt']) {
  await copyFile(new URL('fonts/' + file, import.meta.url), new URL('fonts/' + file, output));
}
await copyFile(new URL('AGENTS.md', root), new URL('agents.md', output));
await copyFile(new URL('install.sh', root), new URL('install.sh', output));
for (const file of ['_headers', '_redirects', 'style.css', 'site.js', 'favicon.svg', 'tailscale.svg']) {
  await copyFile(new URL(file, import.meta.url), new URL(file, output));
}
const landing = await readFile(new URL('index.html', import.meta.url), 'utf8');
const installGuide = await readFile(new URL('AGENTS.md', root), 'utf8');
await writeFile(new URL('index.html', output), landing.replace('{{HEADER}}', header).replace('{{FOOTER}}', footer).replace('{{AGENTS_MD}}', () => escape(installGuide)));

for (const page of pages) {
  const env = { source: page.source };
  const tokens = md.parse(await readFile(new URL(page.source, root), 'utf8'), env);
  if (tokens[0]?.tag === 'h1') tokens.splice(0, 3);
  const headings = [], ids = new Map();
  tokens.forEach((token, index) => {
    if (token.type !== 'heading_open') return;
    const title = tokens[index + 1].content;
    const base = title.toLowerCase().replace(/[^a-z0-9\s-]/g, '').trim().replace(/\s+/g, '-');
    const count = ids.get(base) || 0;
    ids.set(base, count + 1);
    const id = base + (count ? '-' + count : '');
    token.attrSet('id', id);
    if (token.tag === 'h2') headings.push({ id, title });
  });
  const notice = page.unreleased ? '<aside class="doc-notice"><p><strong>Unreleased.</strong> This feature is on main but is not included in the latest published binary. The installer downloads the latest published release.</p></aside>' : '';
  const body = `<div class="docs-layout wrap">${navigation(page)}<main id="main" class="doc-article"><header><p class="eyebrow">Documentation / ${page.group}</p><h1>${escape(page.title)}</h1><p class="doc-description">${escape(page.description)}</p></header>${notice}<div class="doc-content">${md.renderer.render(tokens, md.options, env)}</div><footer class="doc-bottom"><a href="${github}/blob/main/${page.source}">View source on GitHub ↗</a><a href="${github}/issues">Something unclear? Open an issue ↗</a></footer></main><nav class="doc-toc" aria-label="On this page"><strong>On this page</strong>${headings.map(h => `<a href="#${h.id}">${escape(h.title)}</a>`).join('')}</nav></div>`;
  await writeFile(new URL(page.slug ? `docs/${page.slug}.html` : 'docs/index.html', output), shell(page.title, page.description, docPath(page), body));
}
await writeFile(new URL('404.html', output), shell('Page not found', 'Find your way back to Agent Exchange.', '/404', '<main id="main" class="not-found wrap"><p class="eyebrow">404 / No connection here</p><h1>This page wandered off.</h1><p>The docs are a good place to pick up the thread.</p><a class="button primary" href="/docs">Open the docs <span aria-hidden="true">↗</span></a></main>'));
console.log(`Built landing page, ${pages.length} docs pages, canonical agents.md, and install.sh.`);

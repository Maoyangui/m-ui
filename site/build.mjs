// 文档 / 介绍站生成器:零依赖的 Node 脚本,把 site/content/<lang>/*.html 套进 site/layout.html,
// 输出到 site/dist(GitHub Pages 用 Actions 部署)。同时生成 sitemap.xml、robots.txt、
// feed.xml(取 GitHub Releases)、404 和 IndexNow 密钥文件。
//
//   node site/build.mjs            → site/dist/
//
// 内容文件第一行是一段 JSON 注释,例如:
//   <!--meta {"title":"…","description":"…","nav":"Getting started","order":2} -->
// 正文里的 {{base}} 会替换成站点根路径(GitHub Pages 的 /m-ui/),所有内部链接都用它。
import { readFileSync, writeFileSync, mkdirSync, readdirSync, existsSync, copyFileSync, statSync } from 'node:fs';
import { join, dirname, basename } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = dirname(fileURLToPath(import.meta.url));
const REPO = join(ROOT, '..');
const DIST = join(ROOT, 'dist');
const SITE = 'https://maoyangui.github.io/m-ui/';
const BASE = '/m-ui/';
const GITHUB = 'https://github.com/Maoyangui/m-ui';
const LANGS = { en: { prefix: '', hreflang: 'en', html: 'en' }, zh: { prefix: 'zh/', hreflang: 'zh-Hans', html: 'zh-CN' } };
const NOW = new Date().toISOString().slice(0, 10);

const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const out = (rel, data) => { const p = join(DIST, rel); mkdirSync(dirname(p), { recursive: true }); writeFileSync(p, data); };
const walk = (dir, list = []) => { for (const f of readdirSync(dir)) { const p = join(dir, f); statSync(p).isDirectory() ? walk(p, list) : list.push(p); } return list; };

const layout = readFileSync(join(ROOT, 'layout.html'), 'utf8');
const pages = []; // {lang, slug, meta, body}
for (const lang of Object.keys(LANGS)) {
  const dir = join(ROOT, 'content', lang);
  if (!existsSync(dir)) continue;
  for (const file of walk(dir).filter(f => f.endsWith('.html'))) {
    const raw = readFileSync(file, 'utf8');
    const m = raw.match(/^<!--meta\s*([\s\S]*?)-->/);
    if (!m) throw new Error('缺 meta 注释: ' + file);
    const meta = JSON.parse(m[1]);
    const slug = file.slice(dir.length + 1).replace(/\\/g, '/').replace(/\.html$/, '');
    pages.push({ lang, slug, meta, body: raw.slice(m[0].length).trim() });
  }
}

// 页面路径:index → 根;其余 → <slug>/;中文加 zh/ 前缀
const pathOf = (lang, slug) => LANGS[lang].prefix + (slug === 'index' ? '' : slug + '/');
const urlOf = (lang, slug) => SITE + pathOf(lang, slug);
const has = (lang, slug) => pages.some(p => p.lang === lang && p.slug === slug);

const navFor = (lang, current) => pages
  .filter(p => p.lang === lang && p.meta.nav)
  .sort((a, b) => (a.meta.order || 99) - (b.meta.order || 99))
  .map(p => `<a href="${BASE}${pathOf(lang, p.slug)}"${p.slug === current ? ' aria-current="page"' : ''}>${esc(p.meta.nav)}</a>`)
  .join('');

const t = {
  en: { github: 'GitHub', install: 'Install', docs: 'Docs', switch: '中文', switchLang: 'zh', releases: 'Releases', discussions: 'Discussions', feed: 'Release feed', license: 'GPL-3.0 · built on sing-box', updated: 'Updated' },
  zh: { github: 'GitHub', install: '安装', docs: '文档', switch: 'English', switchLang: 'en', releases: '发布', discussions: '讨论区', feed: '版本订阅', license: 'GPL-3.0 · 基于 sing-box', updated: '更新于' },
};

const ogImage = existsSync(join(ROOT, 'assets', 'og.png')) ? SITE + 'assets/og.png' : SITE + 'logo.svg';

for (const p of pages) {
  const L = LANGS[p.lang], tr = t[p.lang];
  const other = tr.switchLang;
  const switchHref = BASE + pathOf(other, has(other, p.slug) ? p.slug : 'index');
  const alternates = Object.keys(LANGS).filter(l => has(l, p.slug))
    .map(l => `<link rel="alternate" hreflang="${LANGS[l].hreflang}" href="${urlOf(l, p.slug)}">`).join('\n  ')
    + `\n  <link rel="alternate" hreflang="x-default" href="${urlOf('en', has('en', p.slug) ? p.slug : 'index')}">`;
  const jsonld = p.slug === 'index' ? JSON.stringify({
    '@context': 'https://schema.org', '@type': 'SoftwareApplication', name: 'm-ui',
    applicationCategory: 'NetworkingApplication', operatingSystem: 'Linux', url: urlOf(p.lang, 'index'),
    downloadUrl: GITHUB + '/releases/latest', softwareHelp: SITE + pathOf(p.lang, 'getting-started'),
    codeRepository: GITHUB, license: 'https://www.gnu.org/licenses/gpl-3.0.html', image: ogImage,
    description: p.meta.description, offers: { '@type': 'Offer', price: '0', priceCurrency: 'USD' },
  }) : (p.meta.type === 'article' ? JSON.stringify({
    '@context': 'https://schema.org', '@type': 'TechArticle', headline: p.meta.title, description: p.meta.description,
    datePublished: p.meta.date || NOW, dateModified: p.meta.updated || p.meta.date || NOW, inLanguage: L.html,
    author: { '@type': 'Person', name: 'Maoyangui', url: 'https://github.com/Maoyangui' }, url: urlOf(p.lang, p.slug), image: ogImage,
  }) : '');
  const html = layout
    .replaceAll('{{lang}}', L.html)
    .replaceAll('{{title}}', esc(p.meta.title))
    .replaceAll('{{description}}', esc(p.meta.description || ''))
    .replaceAll('{{canonical}}', urlOf(p.lang, p.slug))
    .replaceAll('{{alternates}}', alternates)
    .replaceAll('{{og_image}}', ogImage)
    .replaceAll('{{og_type}}', p.meta.type === 'article' ? 'article' : 'website')
    .replaceAll('{{jsonld}}', jsonld ? `<script type="application/ld+json">${jsonld}</script>` : '')
    .replaceAll('{{nav}}', navFor(p.lang, p.slug))
    .replaceAll('{{switch_href}}', switchHref)
    .replaceAll('{{switch_label}}', tr.switch)
    .replaceAll('{{t_github}}', tr.github).replaceAll('{{t_install}}', tr.install).replaceAll('{{t_docs}}', tr.docs)
    .replaceAll('{{t_releases}}', tr.releases).replaceAll('{{t_discussions}}', tr.discussions).replaceAll('{{t_feed}}', tr.feed).replaceAll('{{t_license}}', tr.license)
    .replaceAll('{{home}}', BASE + pathOf(p.lang, 'index'))
    .replaceAll('{{getting_started}}', BASE + pathOf(p.lang, 'getting-started'))
    .replaceAll('{{content}}', p.body)
    .replaceAll('{{github}}', GITHUB)
    .replaceAll('{{year}}', String(new Date().getFullYear()))
    .replaceAll('{{base}}', BASE);
  out(pathOf(p.lang, p.slug) + 'index.html', html);
}

// 静态资源:样式、品牌 logo、截图、OG 图
mkdirSync(join(DIST, 'assets'), { recursive: true });
for (const f of readdirSync(join(ROOT, 'assets'))) copyFileSync(join(ROOT, 'assets', f), join(DIST, 'assets', f));
copyFileSync(join(REPO, 'brand', 'logo.svg'), join(DIST, 'logo.svg'));
const shots = join(REPO, 'docs', 'screenshots');
if (existsSync(shots)) {
  mkdirSync(join(DIST, 'img'), { recursive: true });
  for (const f of readdirSync(shots).filter(f => f.endsWith('.png'))) copyFileSync(join(shots, f), join(DIST, 'img', f));
}

// sitemap:每个 URL 带上语言替代
const urls = pages.map(p => {
  const alts = Object.keys(LANGS).filter(l => has(l, p.slug)).map(l => `<xhtml:link rel="alternate" hreflang="${LANGS[l].hreflang}" href="${urlOf(l, p.slug)}"/>`).join('');
  return `<url><loc>${urlOf(p.lang, p.slug)}</loc><lastmod>${p.meta.updated || p.meta.date || NOW}</lastmod>${alts}</url>`;
});
out('sitemap.xml', `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">\n${urls.join('\n')}\n</urlset>\n`);
out('robots.txt', `User-agent: *\nAllow: /\nSitemap: ${SITE}sitemap.xml\n`);
out('404.html', layout
  .replaceAll('{{lang}}', 'en').replaceAll('{{title}}', 'Not found · m-ui').replaceAll('{{description}}', '')
  .replaceAll('{{canonical}}', SITE).replaceAll('{{alternates}}', '').replaceAll('{{og_image}}', ogImage).replaceAll('{{og_type}}', 'website').replaceAll('{{jsonld}}', '')
  .replaceAll('{{nav}}', navFor('en', '')).replaceAll('{{switch_href}}', BASE + 'zh/').replaceAll('{{switch_label}}', '中文')
  .replaceAll('{{t_github}}', 'GitHub').replaceAll('{{t_install}}', 'Install').replaceAll('{{t_docs}}', 'Docs').replaceAll('{{t_releases}}', 'Releases').replaceAll('{{t_discussions}}', 'Discussions').replaceAll('{{t_feed}}', 'Release feed').replaceAll('{{t_license}}', 'GPL-3.0 · built on sing-box')
  .replaceAll('{{home}}', BASE).replaceAll('{{getting_started}}', BASE + 'getting-started/')
  .replaceAll('{{content}}', `<section class="hero"><h1>404</h1><p class="lead">That page does not exist. <a href="${BASE}">Back to the start</a>.</p></section>`)
  .replaceAll('{{github}}', GITHUB).replaceAll('{{year}}', String(new Date().getFullYear())).replaceAll('{{base}}', BASE));

// IndexNow 密钥文件:内容就是密钥本身,放在站点根目录
const keyFile = join(ROOT, 'indexnow.key');
if (existsSync(keyFile)) { const key = readFileSync(keyFile, 'utf8').trim(); if (key) out(key + '.txt', key); }

// feed.xml:Atom,来自 GitHub Releases;取不到(离线 / 限流)就给一个空但合法的 feed,构建不失败
async function feed() {
  let releases = [];
  try {
    const headers = { 'User-Agent': 'm-ui-site', Accept: 'application/vnd.github+json' };
    if (process.env.GITHUB_TOKEN) headers.Authorization = 'Bearer ' + process.env.GITHUB_TOKEN;
    const res = await fetch('https://api.github.com/repos/Maoyangui/m-ui/releases?per_page=20', { headers, signal: AbortSignal.timeout(15000) });
    if (res.ok) releases = (await res.json()).filter(r => !r.draft);
  } catch (e) { console.warn('feed: 取不到 Releases,生成空 feed:', e.message); }
  const entries = releases.map(r => `  <entry>
    <title>${esc(r.name || r.tag_name)}</title>
    <link href="${esc(r.html_url)}"/>
    <id>${esc(r.html_url)}</id>
    <updated>${esc(r.published_at || r.created_at)}</updated>
    <content type="text">${esc((r.body || '').slice(0, 4000))}</content>
  </entry>`).join('\n');
  const updated = releases[0] ? (releases[0].published_at || releases[0].created_at) : new Date().toISOString();
  out('feed.xml', `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>m-ui releases</title>
  <subtitle>New versions of m-ui, the embedded sing-box multi-server panel</subtitle>
  <link href="${SITE}feed.xml" rel="self"/>
  <link href="${SITE}"/>
  <id>${SITE}</id>
  <updated>${esc(updated)}</updated>
${entries}
</feed>
`);
  console.log(`站点已生成:${pages.length} 个页面,${releases.length} 条 release → ${DIST}`);
}
await feed();

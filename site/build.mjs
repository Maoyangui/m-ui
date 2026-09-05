// 文档 / 介绍站生成器:零依赖的 Node 脚本,把 site/content/<lang>/*.html 套进 site/layout.html,
// 输出到 site/dist(GitHub Pages 用 Actions 部署)。同时生成 sitemap.xml、robots.txt、
// feed.xml(Releases + 文章)、404、IndexNow 密钥文件、文章索引页,以及给发布脚本用的 content.json。
//
//   node site/build.mjs            → site/dist/
//   INCLUDE_FUTURE=1 node …        → 连未到发布日期的文章一起生成(本地预览用)
//
// 内容文件第一行是一段 JSON 注释,例如:
//   <!--meta {"title":"…","description":"…","nav":"Getting started","order":2} -->
// 文章额外字段:type "article"、date(站点发布日,未到不生成)、discussionDate(发到 Discussions 的日期)、
// discussion(true = 到日期后由 content 工作流发一份到 Discussions)、summary。
// 正文里的 {{base}} 会替换成站点根路径(GitHub Pages 的 /m-ui/),所有内部链接都用它。
import { readFileSync, writeFileSync, mkdirSync, readdirSync, existsSync, copyFileSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = dirname(fileURLToPath(import.meta.url));
const REPO = join(ROOT, '..');
const DIST = join(ROOT, 'dist');
const SITE = 'https://maoyangui.github.io/m-ui/';
const BASE = '/m-ui/';
const GITHUB = 'https://github.com/Maoyangui/m-ui';
const AUTHOR = { name: 'Maoyangui', url: 'https://github.com/Maoyangui' };
// 搜索身份:仓库只叫 m-ui,但 "m-ui" 这个词被别的项目占满了,所以对外一律带上限定语
const BRAND = { en: 'm-ui · sing-box panel by Maoyangui', zh: 'm-ui · Maoyangui 的 sing-box 面板' };
const LANGS = { en: { prefix: '', hreflang: 'en', html: 'en' }, zh: { prefix: 'zh/', hreflang: 'zh-Hans', html: 'zh-CN' } };
const TODAY = new Date().toISOString().slice(0, 10);
const INCLUDE_FUTURE = process.env.INCLUDE_FUTURE === '1';

const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const out = (rel, data) => { const p = join(DIST, rel); mkdirSync(dirname(p), { recursive: true }); writeFileSync(p, data); };
const walk = (dir, list = []) => { for (const f of readdirSync(dir)) { const p = join(dir, f); statSync(p).isDirectory() ? walk(p, list) : list.push(p); } return list; };

const layout = readFileSync(join(ROOT, 'layout.html'), 'utf8');
const pages = []; // {lang, slug, meta, body}
const scheduled = []; // 未到日期的文章(不生成,但记进 content.json 让发布脚本知道)
for (const lang of Object.keys(LANGS)) {
  const dir = join(ROOT, 'content', lang);
  if (!existsSync(dir)) continue;
  for (const file of walk(dir).filter(f => f.endsWith('.html'))) {
    const raw = readFileSync(file, 'utf8');
    const m = raw.match(/^<!--meta\s*([\s\S]*?)-->/);
    if (!m) throw new Error('缺 meta 注释: ' + file);
    const meta = JSON.parse(m[1]);
    const slug = file.slice(dir.length + 1).replace(/\\/g, '/').replace(/\.html$/, '');
    const page = { lang, slug, meta, body: raw.slice(m[0].length).trim() };
    if (meta.date && meta.date > TODAY && !INCLUDE_FUTURE) { scheduled.push(page); continue; }
    pages.push(page);
  }
}

const t = {
  en: { github: 'GitHub', install: 'Install', switch: '中文', switchLang: 'zh', releases: 'Releases', discussions: 'Discussions', feed: 'Feed', about: 'About', articles: 'Articles', articlesNav: 'Articles', articlesDesc: 'Technical write-ups on how m-ui is built: embedded sing-box, hot reload, multi-server sync, lines, quotas, backups.', license: 'GPL-3.0 · built on sing-box · by Maoyangui', readMore: 'Read' },
  zh: { github: 'GitHub', install: '安装', switch: 'English', switchLang: 'en', releases: '发布', discussions: '讨论区', feed: '订阅', about: '关于', articles: '文章', articlesNav: '文章', articlesDesc: '关于 m-ui 怎么做的技术文章:内嵌 sing-box、热更新、多服务器同步、线路、配额、备份。', license: 'GPL-3.0 · 基于 sing-box · Maoyangui 出品', readMore: '阅读' },
};

// 文章索引页由这里生成(不用手写内容文件):/articles/ 与 /zh/articles/
for (const lang of Object.keys(LANGS)) {
  const arts = pages.filter(p => p.lang === lang && p.meta.type === 'article').sort((a, b) => (b.meta.date || '').localeCompare(a.meta.date || ''));
  if (!arts.length) continue;
  const tr = t[lang];
  const list = arts.map(a => `<li><a href="${BASE}${LANGS[lang].prefix}${a.slug}/">${esc(a.meta.title)}</a><span class="muted small"> · ${esc(a.meta.date || '')}</span><p class="muted">${esc(a.meta.summary || a.meta.description || '')}</p></li>`).join('\n');
  pages.push({ lang, slug: 'articles', meta: { title: `${tr.articles} · m-ui`, description: tr.articlesDesc, nav: tr.articlesNav, order: 8 }, body: `<article class="doc"><h1>${tr.articles}</h1><p class="lead">${tr.articlesDesc}</p><ul class="article-list">${list}</ul></article>` });
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

const ogImage = existsSync(join(ROOT, 'assets', 'og.png')) ? SITE + 'assets/og.png' : SITE + 'logo.svg';

// 实体图谱:每页都带同一组 @graph,让搜索引擎把 m-ui、Maoyangui/m-ui、sing-box panel 和站点串起来。
// name 是仓库名 m-ui,alternateName 才是带限定语的搜索身份;sameAs 指向仓库与 Releases。
let latestVersion = '';
function graph(p) {
  const L = LANGS[p.lang];
  const nodes = [
    { '@type': 'Person', '@id': AUTHOR.url + '#person', name: AUTHOR.name, url: AUTHOR.url, sameAs: [AUTHOR.url] },
    { '@type': 'WebSite', '@id': SITE + '#website', name: 'm-ui', alternateName: [BRAND.en, 'm-ui sing-box panel'], url: SITE, inLanguage: ['en', 'zh-CN'], publisher: { '@id': AUTHOR.url + '#person' } },
    { '@type': 'SoftwareApplication', '@id': SITE + '#software', name: 'm-ui', alternateName: [BRAND.en, 'm-ui sing-box panel', 'Maoyangui/m-ui'],
      applicationCategory: 'NetworkingApplication', applicationSubCategory: 'sing-box panel', operatingSystem: 'Linux',
      url: SITE, downloadUrl: GITHUB + '/releases/latest', installUrl: SITE + 'getting-started/', softwareHelp: { '@type': 'CreativeWork', url: SITE + 'getting-started/' },
      ...(latestVersion ? { softwareVersion: latestVersion } : {}),
      license: 'https://www.gnu.org/licenses/gpl-3.0.html', isAccessibleForFree: true, offers: { '@type': 'Offer', price: '0', priceCurrency: 'USD' },
      author: { '@id': AUTHOR.url + '#person' }, sameAs: [GITHUB, GITHUB + '/releases', GITHUB + '/discussions'], image: ogImage,
      description: 'Self-hosted multi-server sing-box panel with an embedded core, one Go binary, hot user/upstream reload with validation and rollback, subscriptions, resellers, WARP and certificates.' },
    { '@type': 'SoftwareSourceCode', '@id': GITHUB + '#source', name: 'Maoyangui/m-ui', alternateName: 'm-ui', codeRepository: GITHUB, programmingLanguage: 'Go', runtimePlatform: 'Linux',
      license: 'https://www.gnu.org/licenses/gpl-3.0.html', author: { '@id': AUTHOR.url + '#person' }, targetProduct: { '@id': SITE + '#software' }, url: GITHUB },
  ];
  if (p.meta.type === 'article') {
    nodes.push({ '@type': 'TechArticle', '@id': urlOf(p.lang, p.slug) + '#article', headline: p.meta.title, description: p.meta.description, url: urlOf(p.lang, p.slug),
      datePublished: p.meta.date || TODAY, dateModified: p.meta.updated || p.meta.date || TODAY, inLanguage: L.html, image: ogImage,
      author: { '@id': AUTHOR.url + '#person' }, publisher: { '@id': AUTHOR.url + '#person' }, about: { '@id': SITE + '#software' }, isPartOf: { '@id': SITE + '#website' } });
  } else {
    nodes.push({ '@type': 'WebPage', '@id': urlOf(p.lang, p.slug), url: urlOf(p.lang, p.slug), name: p.meta.title, description: p.meta.description, inLanguage: L.html,
      isPartOf: { '@id': SITE + '#website' }, about: { '@id': SITE + '#software' }, ...(p.meta.date ? { datePublished: p.meta.date } : {}), ...(p.meta.updated ? { dateModified: p.meta.updated } : {}) });
  }
  return JSON.stringify({ '@context': 'https://schema.org', '@graph': nodes });
}

function render(p) {
  const L = LANGS[p.lang], tr = t[p.lang];
  const other = tr.switchLang;
  const switchHref = BASE + pathOf(other, has(other, p.slug) ? p.slug : 'index');
  const alternates = Object.keys(LANGS).filter(l => has(l, p.slug))
    .map(l => `<link rel="alternate" hreflang="${LANGS[l].hreflang}" href="${urlOf(l, p.slug)}">`).join('\n  ')
    + `\n  <link rel="alternate" hreflang="x-default" href="${urlOf('en', has('en', p.slug) ? p.slug : 'index')}">`;
  return layout
    .replaceAll('{{lang}}', L.html)
    .replaceAll('{{title}}', esc(p.meta.title))
    .replaceAll('{{description}}', esc(p.meta.description || ''))
    .replaceAll('{{canonical}}', urlOf(p.lang, p.slug))
    .replaceAll('{{alternates}}', alternates)
    .replaceAll('{{og_image}}', ogImage)
    .replaceAll('{{og_type}}', p.meta.type === 'article' ? 'article' : 'website')
    .replaceAll('{{og_locale}}', p.lang === 'zh' ? 'zh_CN' : 'en_US')
    .replaceAll('{{jsonld}}', `<script type="application/ld+json">${graph(p)}</script>`)
    .replaceAll('{{nav}}', navFor(p.lang, p.slug))
    .replaceAll('{{switch_href}}', switchHref)
    .replaceAll('{{switch_label}}', tr.switch)
    .replaceAll('{{brand_line}}', esc(BRAND[p.lang]))
    .replaceAll('{{t_github}}', tr.github).replaceAll('{{t_install}}', tr.install).replaceAll('{{t_about}}', tr.about).replaceAll('{{t_articles}}', tr.articles)
    .replaceAll('{{t_releases}}', tr.releases).replaceAll('{{t_discussions}}', tr.discussions).replaceAll('{{t_feed}}', tr.feed).replaceAll('{{t_license}}', tr.license)
    .replaceAll('{{home}}', BASE + pathOf(p.lang, 'index'))
    .replaceAll('{{getting_started}}', BASE + pathOf(p.lang, 'getting-started'))
    .replaceAll('{{about}}', BASE + pathOf(p.lang, 'about'))
    .replaceAll('{{articles}}', BASE + pathOf(p.lang, 'articles'))
    .replaceAll('{{content}}', p.body)
    .replaceAll('{{github}}', GITHUB)
    .replaceAll('{{author_url}}', AUTHOR.url)
    .replaceAll('{{year}}', String(new Date().getFullYear()))
    .replaceAll('{{base}}', BASE);
}

// feed.xml 需要 Releases,先取(取不到就空),softwareVersion 也从这里来
async function fetchReleases() {
  try {
    const headers = { 'User-Agent': 'm-ui-site', Accept: 'application/vnd.github+json' };
    if (process.env.GITHUB_TOKEN) headers.Authorization = 'Bearer ' + process.env.GITHUB_TOKEN;
    const res = await fetch('https://api.github.com/repos/Maoyangui/m-ui/releases?per_page=20', { headers, signal: AbortSignal.timeout(15000) });
    if (res.ok) return (await res.json()).filter(r => !r.draft);
  } catch (e) { console.warn('取不到 Releases,feed 只含文章:', e.message); }
  return [];
}
const releases = await fetchReleases();
latestVersion = releases[0] ? String(releases[0].tag_name || '').replace(/^v/, '') : '';

for (const p of pages) out(pathOf(p.lang, p.slug) + 'index.html', render(p));

// 静态资源:样式、品牌 logo、截图、OG 图
mkdirSync(join(DIST, 'assets'), { recursive: true });
for (const f of readdirSync(join(ROOT, 'assets'))) copyFileSync(join(ROOT, 'assets', f), join(DIST, 'assets', f));
copyFileSync(join(REPO, 'brand', 'logo.svg'), join(DIST, 'logo.svg'));
const shots = join(REPO, 'docs', 'screenshots');
if (existsSync(shots)) {
  mkdirSync(join(DIST, 'img'), { recursive: true });
  for (const f of readdirSync(shots).filter(f => f.endsWith('.png'))) copyFileSync(join(shots, f), join(DIST, 'img', f));
}

// sitemap:每个 URL 带上语言替代;没有日期的页面用站点构建日
const urls = pages.map(p => {
  const alts = Object.keys(LANGS).filter(l => has(l, p.slug)).map(l => `<xhtml:link rel="alternate" hreflang="${LANGS[l].hreflang}" href="${urlOf(l, p.slug)}"/>`).join('');
  return `<url><loc>${urlOf(p.lang, p.slug)}</loc><lastmod>${p.meta.updated || p.meta.date || TODAY}</lastmod>${alts}</url>`;
});
out('sitemap.xml', `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">\n${urls.join('\n')}\n</urlset>\n`);
out('robots.txt', `User-agent: *\nAllow: /\nSitemap: ${SITE}sitemap.xml\n`);
out('404.html', render({ lang: 'en', slug: '404', meta: { title: 'Not found · m-ui', description: '' }, body: `<section class="hero"><h1>404</h1><p class="lead">That page does not exist. <a href="${BASE}">Back to the start</a>.</p></section>` }));

// IndexNow 密钥文件:内容就是密钥本身,放在站点根目录
const keyFile = join(ROOT, 'indexnow.key');
if (existsSync(keyFile)) { const key = readFileSync(keyFile, 'utf8').trim(); if (key) out(key + '.txt', key); }

// content.json:发布脚本(site/publish.mjs)据此决定哪篇文章该发到 Discussions
const manifest = [...pages, ...scheduled].filter(p => p.meta.type === 'article').map(p => ({
  lang: p.lang, slug: p.slug, title: p.meta.title, summary: p.meta.summary || p.meta.description || '', url: urlOf(p.lang, p.slug),
  date: p.meta.date || '', discussionDate: p.meta.discussionDate || p.meta.date || '', discussion: !!p.meta.discussion, published: pages.includes(p),
  source: `content/${p.lang}/${p.slug}.html`,
}));
out('content.json', JSON.stringify({ site: SITE, github: GITHUB, generated: TODAY, articles: manifest }, null, 2));

// feed.xml:Atom,Releases 与已发布文章按时间混排
const entries = [
  ...releases.map(r => ({ title: `m-ui ${r.tag_name} · sing-box panel release`, url: r.html_url, date: r.published_at || r.created_at, text: (r.body || '').slice(0, 4000) })),
  ...pages.filter(p => p.meta.type === 'article').map(p => ({ title: p.meta.title, url: urlOf(p.lang, p.slug), date: (p.meta.date || TODAY) + 'T00:00:00Z', text: p.meta.summary || p.meta.description || '' })),
].sort((a, b) => b.date.localeCompare(a.date));
out('feed.xml', `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>m-ui · sing-box panel by Maoyangui — releases and articles</title>
  <subtitle>New versions and technical write-ups for m-ui, the embedded sing-box multi-server panel</subtitle>
  <link href="${SITE}feed.xml" rel="self"/>
  <link href="${SITE}"/>
  <id>${SITE}</id>
  <author><name>${AUTHOR.name}</name><uri>${AUTHOR.url}</uri></author>
  <updated>${esc(entries[0] ? entries[0].date : new Date().toISOString())}</updated>
${entries.map(e => `  <entry>
    <title>${esc(e.title)}</title>
    <link href="${esc(e.url)}"/>
    <id>${esc(e.url)}</id>
    <updated>${esc(e.date)}</updated>
    <content type="text">${esc(e.text)}</content>
  </entry>`).join('\n')}
</feed>
`);
console.log(`站点已生成:${pages.length} 个页面(${scheduled.length} 篇待发布),${releases.length} 条 release → ${DIST}`);

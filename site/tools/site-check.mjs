// 文档站构建产物检查:站内链接与锚点、sitemap、hreflang / canonical 没有坏的;首页与功能页在手机(390)和桌面(1280)宽度下
// 不出现横向滚动,并各截一张图(放在系统临时目录的 m-ui-site-shots/ 下,不进仓库、不进站点)。
//
//   node site/build.mjs && npm i --no-save playwright-core && node site/tools/site-check.mjs
//   node site/tools/site-check.mjs --no-browser   只做链接检查
//
// 浏览器部分和 demo-smoke 一样:本机 Chrome 经管道控制,page.route 从 site/dist 回填,不在本机起监听端口。
import { readFileSync, existsSync, statSync, readdirSync, mkdirSync } from 'node:fs';
import { join, dirname, extname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const DIST = join(ROOT, 'dist');
const SITE = 'https://maoyangui.github.io/m-ui/';
const BASE = '/m-ui/';
const failures = [];
const fail = m => { failures.push(m); console.log('✗', m); };
const pass = m => console.log('✓', m);
if (!existsSync(join(DIST, 'index.html'))) { console.log('site/dist 不存在,先 node site/build.mjs'); process.exit(1); }

const walk = (dir, list = []) => { for (const f of readdirSync(dir)) { const p = join(dir, f); statSync(p).isDirectory() ? walk(p, list) : list.push(p); } return list; };
// 站点路径(/m-ui/xxx)→ dist 里的文件
const fileOf = path => {
  let f = join(DIST, decodeURIComponent(path.slice(BASE.length)));
  if (existsSync(f) && statSync(f).isDirectory()) f = join(f, 'index.html');
  return existsSync(f) && statSync(f).isFile() ? f : null;
};
const idsCache = new Map();
const idsOf = file => {
  if (!idsCache.has(file)) idsCache.set(file, new Set([...readFileSync(file, 'utf8').matchAll(/\sid="([^"]+)"/g)].map(m => m[1])));
  return idsCache.get(file);
};

// ---- 1. 站内链接与锚点(演示目录是面板前端原样复制,只查文档页)----
const pages = walk(DIST).filter(f => f.endsWith('.html') && !f.startsWith(join(DIST, 'demo')));
let links = 0;
const bad = [];
for (const file of pages) {
  const html = readFileSync(file, 'utf8');
  const rel = '/' + file.slice(DIST.length + 1).replace(/\\/g, '/');
  for (const m of html.matchAll(/\s(?:href|src)="([^"]+)"/g)) {
    let u = m[1].replace(/&amp;/g, '&');
    if (/^(mailto:|tel:|javascript:|data:)/.test(u)) continue;
    if (u.startsWith(SITE)) u = BASE + u.slice(SITE.length);
    if (/^https?:\/\//.test(u)) continue; // 外部链接不在这里查
    let path, hash;
    if (u.startsWith('#')) { path = null; hash = u.slice(1); } else {
      if (!u.startsWith('/')) { bad.push(`${rel}: 相对链接 ${u}`); continue; }
      [path, hash] = u.split('#');
      path = path.split('?')[0];
    }
    links++;
    const target = path ? fileOf(path) : file;
    if (!target) { bad.push(`${rel}: ${u} 不存在`); continue; }
    if (hash && target.endsWith('.html') && !idsOf(target).has(decodeURIComponent(hash))) bad.push(`${rel}: ${u} 锚点不存在`);
  }
}
if (bad.length) bad.slice(0, 30).forEach(fail); else pass(`站内链接与锚点都能到(${pages.length} 个页面,${links} 个链接)`);

// ---- 2. sitemap:每个 loc 与替代链接都在 dist 里;每个文档页都在 sitemap 里 ----
const sm = readFileSync(join(DIST, 'sitemap.xml'), 'utf8');
const locs = [...sm.matchAll(/<loc>([^<]+)<\/loc>/g)].map(m => m[1]);
const smBad = [];
for (const u of [...locs, ...[...sm.matchAll(/href="([^"]+)"/g)].map(m => m[1])]) {
  if (!u.startsWith(SITE)) { smBad.push(`sitemap 里有站外地址 ${u}`); continue; }
  if (!fileOf(BASE + u.slice(SITE.length))) smBad.push(`sitemap: ${u} 不存在`);
}
const inSitemap = new Set(locs);
for (const file of pages) {
  const rel = file.slice(DIST.length + 1).replace(/\\/g, '/');
  if (rel === '404.html' || !rel.endsWith('index.html')) continue;
  const url = SITE + rel.replace(/index\.html$/, '');
  if (!inSitemap.has(url)) smBad.push(`页面不在 sitemap 里:${url}`);
}
if (new Set(locs).size !== locs.length) smBad.push('sitemap 里有重复的 loc');
if (smBad.length) smBad.slice(0, 20).forEach(fail); else pass(`sitemap 的 ${locs.length} 个地址都在,每个页面都收录了`);

// ---- 3. hreflang 与 canonical:指向的页存在、互相指回、canonical 是自己 ----
const hlBad = [];
const alt = new Map(); // url → {hreflang: href}
for (const file of pages) {
  const rel = file.slice(DIST.length + 1).replace(/\\/g, '/');
  if (rel === '404.html' || !rel.endsWith('index.html')) continue;
  const html = readFileSync(file, 'utf8');
  const self = SITE + rel.replace(/index\.html$/, '');
  const canon = (html.match(/<link rel="canonical" href="([^"]+)"/) || [])[1];
  if (canon !== self) hlBad.push(`${self} 的 canonical 是 ${canon}`);
  const map = {};
  for (const m of html.matchAll(/<link rel="alternate" hreflang="([^"]+)" href="([^"]+)">/g)) {
    map[m[1]] = m[2];
    if (!m[2].startsWith(SITE) || !fileOf(BASE + m[2].slice(SITE.length))) hlBad.push(`${self}: hreflang ${m[1]} → ${m[2]} 不存在`);
  }
  if (!map['x-default']) hlBad.push(`${self} 没有 x-default`);
  if (!Object.values(map).includes(self)) hlBad.push(`${self} 的 hreflang 不含自己`);
  alt.set(self, map);
}
for (const [self, map] of alt) for (const [lang, href] of Object.entries(map)) {
  if (lang === 'x-default' || href === self) continue;
  const back = alt.get(href);
  if (!back || !Object.values(back).includes(self)) hlBad.push(`${self} → ${href}(${lang})没有指回来`);
}
if (hlBad.length) hlBad.slice(0, 20).forEach(fail); else pass(`hreflang / canonical 正确(${alt.size} 个页面,双向互指)`);

// ---- 4. 不该出现在页面里的:给未合并功能留位置的注释、未替换的占位符 ----
const leftBad = [];
for (const file of pages) {
  const html = readFileSync(file, 'utf8');
  const rel = file.slice(DIST.length + 1).replace(/\\/g, '/');
  if (/待补|To fill in once/.test(html)) leftBad.push(`${rel}: 留位置的注释漏进了页面`);
  const ph = html.match(/\{\{[a-z_]+\}\}/);
  if (ph) leftBad.push(`${rel}: 没替换的占位符 ${ph[0]}`);
}
if (leftBad.length) leftBad.slice(0, 20).forEach(fail); else pass('页面里没有留位置的注释和未替换的占位符');

// ---- 5. 浏览器:首页、功能页在 390 / 1280 宽度下不横向溢出,并截图 ----
if (!process.argv.includes('--no-browser')) {
  const { chromium } = await import('playwright-core');
  const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.png': 'image/png', '.svg': 'image/svg+xml', '.xml': 'application/xml', '.webp': 'image/webp', '.txt': 'text/plain' };
  const origin = 'https://site-check.invalid';
  const shots = join(tmpdir(), 'm-ui-site-shots');
  mkdirSync(shots, { recursive: true });
  const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });
  const targets = [['', 'en-home'], ['features/', 'en-features'], ['zh/', 'zh-home'], ['zh/features/', 'zh-features'], ['faq/', 'en-faq'], ['zh/faq/', 'zh-faq']];
  for (const width of [390, 1280]) {
    const ctx = await browser.newContext({ viewport: { width, height: 900 } });
    const errors = [];
    await ctx.route('**/*', route => {
      const url = new URL(route.request().url());
      if (url.origin !== origin) return route.fulfill({ status: 204, body: '' }); // 站外资源(GitHub 头像之类)不取
      const f = fileOf(url.pathname);
      if (!f) return route.fulfill({ status: 404, body: 'not found' });
      return route.fulfill({ status: 200, contentType: TYPES[extname(f)] || 'application/octet-stream', body: readFileSync(f) });
    });
    const page = await ctx.newPage();
    page.on('pageerror', e => errors.push(String(e)));
    page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
    for (const [path, name] of targets) {
      const res = await page.goto(origin + BASE + path, { waitUntil: 'load' });
      await page.waitForTimeout(300);
      const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      await page.screenshot({ path: join(shots, `${name}-${width}.png`), fullPage: true });
      if (!res || res.status() !== 200) fail(`${path || '/'} 返回 ${res && res.status()}`);
      else if (over > 1) fail(`${name} 在 ${width} 宽度横向溢出 ${over}px`);
      else pass(`${name} 在 ${width} 宽度不溢出`);
    }
    if (errors.length) fail(`${width} 宽度下浏览器报错:${errors.slice(0, 3).join(' | ')}`); else pass(`${width} 宽度下没有浏览器报错`);
    await ctx.close();
  }
  await browser.close();
  console.log(`截图在 ${shots}`);
}

console.log(failures.length ? `\n${failures.length} 项失败` : '\n文档站检查全部通过');
process.exit(failures.length ? 1 : 0);

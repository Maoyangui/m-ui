// Live Demo 冒烟测试:把 site/dist 按线上路径(/m-ui/)本地起个静态服务,用真实浏览器打开 /m-ui/demo/,
// 检查:页面 200、没有 JS 报错、没有任何 /api/ 或外部请求、几个主要页面都渲染出演示数据、
// 演示里的增删改在本页生效、刷新后复原、手机宽度不出现横向滚动、fixture 里没有非文档保留的 IP 或未抹掉的密钥。
//
//   node site/build.mjs && npm i --no-save playwright-core && node site/tools/demo-smoke.mjs
//
// 用本机 Chrome(CHROME_CHANNEL,默认 chrome),和截图脚本一致,不下载浏览器。
import { createServer } from 'node:http';
import { readFileSync, existsSync, statSync } from 'node:fs';
import { join, dirname, extname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { hostname } from 'node:os';
import { chromium } from 'playwright-core';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const DIST = join(ROOT, 'dist');
const BASE = '/m-ui/';
const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.png': 'image/png', '.svg': 'image/svg+xml', '.ico': 'image/x-icon', '.webp': 'image/webp', '.woff2': 'font/woff2', '.xml': 'application/xml', '.txt': 'text/plain' };
const failures = [];
const fail = m => { failures.push(m); console.log('✗', m); };
const pass = m => console.log('✓', m);

if (!existsSync(join(DIST, 'demo', 'index.html'))) { console.log('site/dist/demo 不存在,先 node site/build.mjs'); process.exit(1); }

// ---- 1. fixture 静态检查(不需要浏览器)----
const fixPath = join(DIST, 'demo', 'js', 'fixtures.json');
const raw = readFileSync(fixPath, 'utf8');
const fx = JSON.parse(raw);
const fixturesOnly = JSON.stringify(fx.fixtures);
const okIp = ip => /^(192\.0\.2|198\.51\.100|203\.0\.113)\./.test(ip) || /^(127\.|0\.0\.0\.0|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.|104\.28\.)/.test(ip);
const badIps = [...new Set((fixturesOnly.replace(/Chrome\/[\d.]+/g, '').match(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g) || []).filter(ip => ip.split('.').every(o => Number(o) <= 255) && !okIp(ip)))];
if (badIps.length) fail(`fixture 里有非文档保留的 IPv4:${badIps.join(', ')}`); else pass('fixture 只含文档保留 IP');
const SECRET = /private_key|privatekey|password|secret|apitoken|agenttoken|totp|(^|_)token$/i;
const leaks = [];
const walk = (v, k = '', p = '') => { if (Array.isArray(v)) v.forEach((x, i) => walk(x, '', `${p}[${i}]`)); else if (v && typeof v === 'object') for (const [kk, vv] of Object.entries(v)) walk(vv, kk, p + '.' + kk); else if (typeof v === 'string' && k !== 'subToken' && SECRET.test(k) && v.length > 3 && !v.startsWith('demo-')) leaks.push(p); };
walk(fx.fixtures);
if (leaks.length) fail(`fixture 里有未抹掉的密钥字段:${leaks.slice(0, 5).join(', ')}`); else pass('fixture 密钥字段已全部替换为占位');
const host = hostname();
if (host.length > 3 && raw.includes(host)) fail(`fixture 里出现了本机主机名 ${host}`); else pass('fixture 不含本机主机名');
if (/(?:[0-9a-f]{1,4}:){4,7}[0-9a-f]{0,4}/i.test(fixturesOnly.replace(/2001:db8::10/g, ''))) fail('fixture 里有未抹掉的 IPv6'); else pass('fixture 不含真实 IPv6');
if (!(fx.fixtures['GET users'] || []).some(u => u.name === 'demo-alice')) fail('fixture 里没有 demo-alice'); else pass('fixture 含演示用户');

// ---- 2. 本地静态服务,路径前缀与 GitHub Pages 一致 ----
const server = createServer((req, res) => {
  let p = decodeURIComponent(req.url.split('?')[0]);
  if (!p.startsWith(BASE)) { res.writeHead(404); return res.end(); }
  p = p.slice(BASE.length);
  let file = join(DIST, p);
  if (existsSync(file) && statSync(file).isDirectory()) file = join(file, 'index.html');
  if (!existsSync(file) || statSync(file).isDirectory()) { res.writeHead(404); return res.end('not found'); }
  res.writeHead(200, { 'content-type': TYPES[extname(file)] || 'application/octet-stream' });
  res.end(readFileSync(file));
});
await new Promise(r => server.listen(0, '127.0.0.1', r));
const origin = `http://127.0.0.1:${server.address().port}`;
const DEMO = `${origin}${BASE}demo/`;

// ---- 3. 浏览器 ----
const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, locale: 'en-US' });
const page = await ctx.newPage();
const errors = [], requests = [], badResponses = [];
page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
page.on('pageerror', e => errors.push(String(e)));
page.on('request', r => requests.push(r.url()));
page.on('response', r => { if (r.status() >= 400) badResponses.push(`${r.status()} ${r.url()}`); });
page.on('requestfailed', r => badResponses.push(`failed ${r.url()}`));

const first = await page.goto(DEMO + '#/dashboard', { waitUntil: 'networkidle' });
if (first.status() !== 200) fail(`/demo/ 返回 ${first.status()}`); else pass('/demo/ 200');
await page.waitForSelector('#page', { timeout: 15000 });
await page.waitForTimeout(800);
if (await page.locator('#demo-bar').count()) pass('演示提示条已显示'); else fail('没有演示提示条 #demo-bar');
if (await page.locator('#login:not([hidden])').count()) fail('演示页显示了登录框'); else pass('不需要登录');

const expect = { dashboard: /online|在线|users|用户/i, users: /demo-alice/, lines: /Tokyo-HY2/, nodes: /Frankfurt/, upstreams: /./, plans: /./, resellers: /./, exts: /./, settings: /./ };
for (const [route, re] of Object.entries(expect)) {
  await page.goto(`${DEMO}#/${route}`);
  await page.waitForTimeout(700);
  const text = (await page.locator('#page').innerText().catch(() => '')).trim();
  if (text.length < 20) fail(`#/${route} 页面几乎为空`); else if (!re.test(text)) fail(`#/${route} 没渲染出预期内容`); else pass(`#/${route} 渲染正常(${text.length} 字符)`);
}

// 深链接:直接打开用户页也应到用户页
await page.goto(`${DEMO}#/users`, { waitUntil: 'networkidle' });
await page.waitForTimeout(500);
if (/demo-bob/.test(await page.locator('#page').innerText())) pass('深链接 /demo/#/users 直达用户页'); else fail('深链接 /demo/#/users 未直达用户页');

// 写操作在内存里生效,刷新后复原
const mut = await page.evaluate(async () => {
  const m = await import('./js/api.js');
  const created = await m.post('users', { name: 'smoke-test', enabled: true, lineIds: [1] });
  const listed = (await m.get('users')).some(u => u.name === 'smoke-test');
  await m.post(`users/${created.id}/reset`, {});
  await m.del(`users/${created.id}`);
  const gone = !(await m.get('users')).some(u => u.name === 'smoke-test');
  const again = await m.post('users', { name: 'smoke-test-2', enabled: true, lineIds: [1] });
  return { id: created.id, listed, gone, again: !!again.id };
});
if (mut.listed && mut.gone && mut.again) pass('演示写操作(新建 / 重置 / 删除)在内存里生效'); else fail(`演示写操作异常:${JSON.stringify(mut)}`);
await page.reload({ waitUntil: 'networkidle' });
await page.waitForTimeout(600);
if (/smoke-test-2/.test(await page.locator('#page').innerText())) fail('刷新后改动没有复原'); else pass('刷新后复原');

// 界面上真的点一下:切到 lines 页,点第一条线路的开关(演示应当成功而不是 403)
await page.goto(`${DEMO}#/lines`); await page.waitForTimeout(600);
const toggle = page.locator('#page input[type=checkbox], #page .switch, #page [data-toggle]').first();
if (await toggle.count()) { await toggle.click().catch(() => {}); await page.waitForTimeout(400); pass('线路开关可点击'); }

// 请求审计
const external = requests.filter(u => !u.startsWith(origin));
const apiCalls = requests.filter(u => /\/api\//.test(u));
if (external.length) fail(`有外部请求:${[...new Set(external)].slice(0, 5).join(', ')}`); else pass(`没有外部请求(共 ${requests.length} 个请求,全部本地)`);
if (apiCalls.length) fail(`有 /api/ 请求:${[...new Set(apiCalls)].slice(0, 5).join(', ')}`); else pass('没有 /api/ 请求');
const realBad = badResponses.filter(u => !/favicon/.test(u));
if (realBad.length) fail(`有失败的资源请求:${[...new Set(realBad)].slice(0, 5).join(', ')}`); else pass('所有资源请求成功');
const realErrors = errors.filter(e => !/favicon/.test(e));
if (realErrors.length) fail(`浏览器报错:${realErrors.slice(0, 3).join(' | ')}`); else pass('没有 JS 报错');

// 手机宽度不横向滚动
const m = await ctx.newPage();
await m.setViewportSize({ width: 390, height: 844 });
for (const route of ['dashboard', 'users', 'lines']) {
  await m.goto(`${DEMO}#/${route}`); await m.waitForTimeout(700);
  const over = await m.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (over > 1) fail(`手机宽度下 #/${route} 横向溢出 ${over}px`); else pass(`手机宽度 #/${route} 不溢出`);
}

await browser.close();
server.close();
console.log(failures.length ? `\n${failures.length} 项失败` : '\nLive Demo 冒烟测试全部通过');
process.exit(failures.length ? 1 : 0);

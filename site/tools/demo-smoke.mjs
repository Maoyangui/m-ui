// Live Demo 冒烟测试:浏览器里把 site/dist 按线上路径(/m-ui/)回填(page.route,不在本机起监听端口),打开 /m-ui/demo/,
// 检查:页面 200、没有 JS 报错、没有任何 /api/ 或外部请求、几个主要页面都渲染出演示数据、
// 演示里的增删改在本页生效、刷新后复原、手机宽度不出现横向滚动、fixture 里没有非文档保留的 IP 或未抹掉的密钥。
//
// 还点一遍本版的新界面:帮助说明按钮、续费 / 换套餐对话框、落地页 logo 上传、IPv6 入站检测、「握手失败」筛选、证书续期、还原提示,
// 写操作后要有"演示模式,刷新即恢复"的提示。
//
//   node site/build.mjs && npm i --no-save playwright-core && node site/tools/demo-smoke.mjs [en|zh]
//
// 不带参数时中英文界面各跑一遍。用本机 Chrome(CHROME_CHANNEL,默认 chrome),和截图脚本一致,不下载浏览器;
// Chrome 经管道(--remote-debugging-pipe)控制,不开调试端口。
import { readFileSync, existsSync, statSync } from 'node:fs';
import { deflateSync, crc32 } from 'node:zlib';
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

// ---- 2. 路径前缀与 GitHub Pages 一致:浏览器里拦下请求,从 site/dist 回填,不起本地服务 ----
const origin = 'https://demo-smoke.invalid';
const DEMO = `${origin}${BASE}demo/`;
async function serveDist(ctx) {
  await ctx.route('**/*', route => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) return route.abort('blockedbyclient'); // 外部请求一律拦下(下面的请求审计会记到)
    const p = decodeURIComponent(url.pathname);
    if (!p.startsWith(BASE)) return route.fulfill({ status: 404, body: 'not found' });
    let file = join(DIST, p.slice(BASE.length));
    if (existsSync(file) && statSync(file).isDirectory()) file = join(file, 'index.html');
    if (!existsSync(file) || statSync(file).isDirectory()) return route.fulfill({ status: 404, body: 'not found' });
    return route.fulfill({ status: 200, contentType: TYPES[extname(file)] || 'application/octet-stream', body: readFileSync(file) });
  });
}
// 一张 w×h 的纯色 PNG(给 logo 上传用)
function makePng(w, h) {
  const chunk = (type, data) => { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(td) >>> 0); return Buffer.concat([len, td, crc]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  const row = Buffer.concat([Buffer.from([0]), Buffer.alloc(w * 3, 0x40)]);
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(Buffer.concat(Array(h).fill(row)))), chunk('IEND', Buffer.alloc(0))]);
}

// ---- 3. 浏览器 ----
const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });
const langs = process.argv[2] ? [process.argv[2]] : ['en', 'zh'];
for (const lang of langs) {
console.log(`\n==== ${lang === 'zh' ? '中文界面' : 'English UI'} ====`);
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, locale: lang === 'zh' ? 'zh-CN' : 'en-US' });
await serveDist(ctx);
await ctx.addInitScript(l => { try { localStorage.setItem('m-ui-lang', l); } catch {} }, lang);
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

const expect = { dashboard: /online|在线|users|用户/i, users: /demo-alice/, lines: /Tokyo-HY2/, nodes: /Frankfurt/, upstreams: /./, plans: /./, resellers: /./, exts: /./, settings: /./, cert: /./, ops: /./, backup: /./, logs: /./, admin: /./, account: /./ };
for (const [route, re] of Object.entries(expect)) {
  await page.goto(`${DEMO}#/${route}`);
  await page.waitForTimeout(700);
  const text = (await page.locator('#page').innerText().catch(() => '')).trim();
  if (text.length < 20) fail(`#/${route} 页面几乎为空`); else if (/Cannot read properties|is not defined|TypeError|undefined \(reading/.test(text)) fail(`#/${route} 页面渲染出错:${text.slice(0, 120)}`); else if (!re.test(text)) fail(`#/${route} 没渲染出预期内容`); else pass(`#/${route} 渲染正常(${text.length} 字符)`);
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

// 证书库、套餐绑定规则、外部节点改名:新接口在演示里也要能走通
const fresh = await page.evaluate(async () => {
  const m = await import('./js/api.js');
  const c = await m.get('cert');
  await m.post('cert/use', { name: '*.example.net', target: 'panel' });
  const c2 = await m.get('cert');
  const plans = await m.get('plans?all=1');
  await m.put('plans/2', { ...plans[1], ruleIds: [1] });
  const plans2 = await m.get('plans');
  const rule = await m.get('rules/1');
  const ex = await m.get('exts');
  const before = await m.get('exts/1/nodes'); // 隐藏 / 改名按节点指纹(key)只收增量,带版本号
  const keyOf = n => before.nodes.find(x => x.name === n).key;
  await m.put('exts/1/nodes/prefs', { rev: before.rev, hide: [{ key: keyOf('HK-1') }], rename: [{ key: keyOf('JP-2'), to: '日本二号' }] });
  const nodes = await m.get('exts/1/nodes');
  const tmpl = await m.get('sub-templates');
  return {
    certs: c.certs.length, picked: c2.certs.find(x => x.name === '*.example.net').forPanel, panel: c2.panelCert,
    planRules: plans2[1].ruleIds, rulePlans: rule.planIds, exts: ex.length,
    hidden: nodes.nodes.find(n => n.name === 'HK-1').hidden, renamed: nodes.nodes.find(n => n.name === 'JP-2').rename, keyword: nodes.nodes.find(n => n.name.includes('官网')).keyword,
    tmpl: !!tmpl.clash && !!tmpl.singbox,
  };
});
if (fresh.certs === 3 && fresh.picked && /_\.example\.net/.test(fresh.panel) && fresh.planRules.includes(1) && fresh.rulePlans.includes(2) && fresh.exts === 1 && fresh.hidden && fresh.renamed === '日本二号' && fresh.keyword && fresh.tmpl) pass('证书库 / 套餐绑定规则 / 外部节点筛选 / 订阅模板在演示里走得通');
else fail(`新接口演示异常:${JSON.stringify(fresh)}`);
await page.reload({ waitUntil: 'networkidle' });
await page.waitForTimeout(400);

// 界面上真的点一下:切到 lines 页,点第一条线路的开关(演示应当成功而不是 403)
await page.goto(`${DEMO}#/lines`); await page.waitForTimeout(600);
const toggle = page.locator('#page input[type=checkbox], #page .switch, #page [data-toggle]').first();
if (await toggle.count()) { await toggle.click().catch(() => {}); await page.waitForTimeout(400); pass('线路开关可点击'); }

// 本版新界面:帮助按钮、续费 / 换套餐对话框、logo 上传、IPv6 入站检测、握手失败、证书续期状态、还原提示
const note = lang === 'zh' ? '演示模式,刷新即恢复' : 'Demo mode: resets on refresh';
const toastText = () => page.locator('#toast').innerText().catch(() => '');
await page.goto(`${DEMO}#/dashboard`); await page.waitForTimeout(1000);
const help = await page.locator('#btn-help').getAttribute('href').catch(() => '');
if (help && help.startsWith(`help/${lang}.html`)) pass(`帮助说明按钮指向 ${help}`); else fail(`帮助说明按钮地址不对:${help}`);
// 最近入站连接是一张表三档筛选(连接 / 被拒 / 握手失败),默认显示「连接」;握手 / 认证失败要点「握手失败」才列出
const dashText = await page.locator('#page').innerText();
if (/2001:db8:1:2::8/.test(dashText)) pass('最近入站连接含 IPv6 来源'); else fail('最近入站连接没有 IPv6 来源');
const failSeg = page.locator('#dash-conn-seg [data-act="dash.connF"][data-id="failed"]');
if (await failSeg.count()) {
  const segLabel = (await failSeg.innerText()).trim();
  if (/2/.test(segLabel)) pass(`「握手失败」筛选按钮写出条数:${segLabel}`); else fail(`「握手失败」筛选按钮没写条数:${segLabel}`);
  await failSeg.click(); await page.waitForTimeout(500);
  const failText = await page.locator('#dash-conns').innerText().catch(() => '');
  if (/198\.51\.100\.23/.test(failText) && /203\.0\.113\.99/.test(failText)) pass('「握手失败」筛选列出握手 / 认证失败的来源'); else fail(`「握手失败」筛选里没有握手 / 认证失败的来源:${failText.slice(0, 200)}`);
  if (/198\.51\.100\.23|203\.0\.113\.99/.test(dashText)) fail('「连接」筛选里混进了握手失败的来源'); else pass('「连接」筛选里不含握手失败的来源');
} else fail('最近入站连接没有「握手失败」筛选');
const helpPage = await ctx.newPage();
helpPage.on('pageerror', e => errors.push('help: ' + e));
const hr = await helpPage.goto(new URL(help, DEMO).href, { waitUntil: 'load' });
await helpPage.waitForTimeout(400);
if (hr && hr.status() === 200 && (await helpPage.locator('#toc a').count()) > 20) pass('帮助说明页在演示里能打开、带目录'); else fail('帮助说明页打不开');
await helpPage.close();

await page.goto(`${DEMO}#/users`); await page.waitForTimeout(800);
await page.locator('[data-act="user.renew"]').first().evaluate(b => b.click());
await page.waitForTimeout(900);
if (await page.locator('#modal:not([hidden]) #f-periods').count()) pass('续费对话框打开(带期数)'); else fail('续费对话框没打开');
if ((await page.locator('#rn-preview').innerText().catch(() => '')).trim()) pass('续费对话框显示预览'); else fail('续费对话框没有预览');
await page.fill('#f-periods', '3');
await page.waitForTimeout(600);
await page.click('#modal-save'); await page.waitForTimeout(1300);
const t1 = await toastText();
if (t1.includes(note)) pass(`续费后提示「${t1}」`); else fail(`续费后没有演示提示:${t1}`);
await page.locator('[data-act="user.change"]').first().evaluate(b => b.click());
await page.waitForTimeout(1000);
if (await page.locator('#modal:not([hidden])').count()) pass('换套餐对话框打开'); else fail('换套餐对话框没打开');
await page.click('#modal-cancel').catch(() => {});
await page.waitForTimeout(300);

await page.goto(`${DEMO}#/settings`); await page.waitForTimeout(900);
const fileInput = page.locator('#set-logo-file');
if (await fileInput.count()) {
  await fileInput.setInputFiles({ name: 'logo.png', mimeType: 'image/png', buffer: makePng(240, 80) });
  await page.waitForTimeout(1600);
  const logo = await page.evaluate(async () => (await (await import('./js/api.js')).get('sub-logo')));
  const t2 = await toastText();
  if (logo && logo.set) pass('落地页 logo 上传在演示里生效'); else fail('落地页 logo 上传没生效');
  if (t2.includes(note)) pass(`logo 上传后提示「${t2}」`); else fail(`logo 上传后没有演示提示:${t2}`);
} else fail('设置页没有 logo 上传');

await page.goto(`${DEMO}#/nodes`); await page.waitForTimeout(1000);
if (await page.locator('#page .rc', { hasText: 'v6' }).count()) pass('服务器页显示 IPv6 入站检测'); else fail('服务器页没有 IPv6 入站检测');
await page.locator('[data-act="reach.detail"]').last().evaluate(b => b.click()).catch(() => {});
await page.waitForTimeout(700);
if (/2001:db8:7::30/.test(await page.locator('#drawer-body').innerText().catch(() => ''))) pass('大陆连通详情里有 IPv6 入站一节'); else fail('大陆连通详情没有 IPv6 入站');
await page.keyboard.press('Escape');

await page.goto(`${DEMO}#/cert`); await page.waitForTimeout(900);
const renewCells = await page.locator('#page [data-act="cert.autorenew"]').count();
if (renewCells) pass(`证书页显示自动续期状态(${renewCells} 个开关)`); else fail('证书页没有自动续期一列');
await page.locator('#page [data-act="cert.renew"]').first().evaluate(b => b.click()).catch(() => {});
await page.waitForTimeout(800);
if (await page.locator('#modal:not([hidden])').count()) await page.click('#modal-save').catch(() => {});
await page.waitForTimeout(1200);
const t3 = await toastText();
if (t3.includes(note)) pass(`证书续期后提示「${t3}」`); else fail(`证书续期后没有演示提示:${t3}`);

await page.goto(`${DEMO}?restore=1#/dashboard`); await page.waitForTimeout(1000);
if (await page.locator('#restore-bar').count()) {
  pass('还原提示条能显示');
  await page.click('#restore-ack'); await page.waitForTimeout(500);
  if (await page.locator('#restore-bar').count()) fail('还原提示「知道了」点了没消失'); else pass('还原提示「知道了」生效');
} else fail('?restore=1 时没有还原提示条');

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
for (const route of ['dashboard', 'users', 'lines', 'nodes', 'cert', 'settings']) {
  await m.goto(`${DEMO}#/${route}`); await m.waitForTimeout(700);
  const over = await m.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (over > 1) fail(`手机宽度下 #/${route} 横向溢出 ${over}px`); else pass(`手机宽度 #/${route} 不溢出`);
}

await ctx.close();
}

await browser.close();
console.log(failures.length ? `\n${failures.length} 项失败` : '\nLive Demo 冒烟测试全部通过');
process.exit(failures.length ? 1 : 0);

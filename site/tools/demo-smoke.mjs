// Live Demo 冒烟测试:浏览器里把 site/dist 按线上路径(/m-ui/)回填(page.route,不在本机起监听端口),打开 /m-ui/demo/,
// 检查:页面 200、没有 JS 报错、没有任何 /api/ 或外部请求、几个主要页面都渲染出演示数据、
// 演示里的增删改在本页生效、刷新后复原、手机宽度不出现横向滚动、fixture 里没有非文档保留的 IP 或未抹掉的密钥。
//
// 还点一遍本版的新界面:帮助说明按钮、续费 / 换套餐对话框、落地页 logo 上传、IPv6 入站检测、「握手失败」筛选、主机容灾(角色标签、备用卡片、设置页就绪检查)、证书续期、还原提示,
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
// 「建议 / 支持」页在面板里是 Go 模板,演示站要预先按中英渲染(build.mjs 的 buildDemoSupport);原样拷过去会把 {{…}} 露给访客
for (const [lang, title] of [['zh', '建议与支持'], ['en', 'Feedback &amp; Support']]) {
  const f = join(DIST, 'demo', `support.${lang}.html`);
  const s = existsSync(f) ? readFileSync(f, 'utf8') : '';
  if (!s) fail(`演示站没有 support.${lang}.html`);
  else if (s.includes('{{')) fail(`support.${lang}.html 里还有模板语法`);
  else if (!s.includes(`<title>${title}`)) fail(`support.${lang}.html 的标题不对`);
  else pass(`支持页 ${lang} 已渲染(无模板语法)`);
}
if (!existsSync(join(DIST, 'demo', 'support-qr.png'))) fail('演示站没有打赏二维码 support-qr.png'); else pass('支持页二维码在');

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

const expect = { dashboard: /online|在线|users|用户/i, users: /demo-alice/, lines: /Tokyo-HY2/, nodes: /Frankfurt/, upstreams: /./, plans: /./, resellers: /./, exts: /./, settings: /./, cert: /./, ops: /./, fw: /ufw/, backup: /./, logs: /./, admin: /./, account: /./ };
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
// 按服务器名找那一行(服务器按分组分段后,行的先后不固定)
await page.locator('#nodes-body tr', { hasText: 'Frankfurt' }).locator('[data-act="reach.detail"]').first().evaluate(b => b.click()).catch(() => {});
await page.waitForTimeout(700);
if (/2001:db8:7::30/.test(await page.locator('#drawer-body').innerText().catch(() => ''))) pass('大陆连通详情里有 IPv6 入站一节'); else fail('大陆连通详情没有 IPv6 入站');
await page.keyboard.press('Escape');

// 主机容灾:服务器页的角色标签(主机 · 本机、备用 1、备用 2)、点开的卡片、设置页的容灾卡片与就绪检查
await page.goto(`${DEMO}#/nodes`); await page.waitForTimeout(1000);
const chips = await page.locator('#page .role-chip').allInnerTexts();
const want = lang === 'zh' ? ['主机', '备用 1', '备用 2'] : ['Master', 'Standby 1', 'Standby 2'];
if (want.every(w => chips.some(c => c.includes(w)))) pass(`服务器页显示容灾角色:${chips.map(c => c.replace(/\s*▾/, '')).join(' / ')}`); else fail(`服务器页容灾角色不对:${JSON.stringify(chips)}`);
await page.locator('#page .role-chip.k-standby').first().evaluate(b => b.click()).catch(() => {});
await page.waitForTimeout(900);
const card = await page.locator('.ha-pop').innerText().catch(() => '');
if (card && /[0-9a-f]{16}/.test(card) && !/ha\.|\{[a-z]+\}|undefined|NaN/.test(card)) pass('备用卡片显示副本时间与公钥指纹'); else fail(`备用卡片内容不对:${card.slice(0, 160)}`);
if (/(演示模式|Demo mode)/.test(card) && await page.locator('.ha-pop [data-act="ha.setMaster"][disabled]').count()) pass('卡片里「设为主机」写明演示不真做'); else fail('卡片里的「设为主机」没写明演示模式');
await page.keyboard.press('Escape');
await page.goto(`${DEMO}#/settings`); await page.waitForTimeout(1200);
const haCard = await page.locator('#ha-card').innerText().catch(() => '');
const ready = await page.locator('#ha-card .ha-check li').count();
const confirmSel = await page.locator('#ha-card input[name="ha-mode"][value="confirm"]').isChecked().catch(() => false);
if (ready >= 5 && confirmSel && !/ha\.ready|\{[a-z]+\}|undefined/.test(haCard)) pass(`设置页主机容灾:默认「先确认再接任」,就绪检查 ${ready} 项`); else fail(`设置页主机容灾卡片不对(选中确认模式:${confirmSel},检查 ${ready} 项):${haCard.slice(0, 160)}`);

// 设置页的保存条:改一项就在底部浮出来;清单里逐项列出(容灾卡片的改动也算),能单独撤销;保存后收起
await page.locator('#set-subUpdates').fill('8');
await page.waitForTimeout(300);
const barTxt = await page.locator('#set-bar:not([hidden])').innerText().catch(() => '');
if (/1/.test(barTxt)) pass('设置页改一项后底部浮出保存条'); else fail(`设置页改动后没有保存条:${barTxt.slice(0, 80)}`);
await page.locator('#ha-card input[name="ha-mode"][value="auto"]').check().catch(() => {});
await page.waitForTimeout(300);
await page.locator('[data-act="set.barToggle"]').click().catch(() => {});
await page.waitForTimeout(300);
const barRows = await page.locator('#set-bar .sb-row').count();
if (barRows === 2) pass('保存条清单列出两处改动(含主机容灾)'); else fail(`保存条清单行数不对:${barRows}`);
await page.locator('#set-bar .sb-row [data-act="set.undo"]').last().click().catch(() => {}); // 撤销容灾那一项
await page.waitForTimeout(300);
if (await page.locator('#ha-card input[name="ha-mode"][value="confirm"]').isChecked().catch(() => false)) pass('保存条里单独撤销一项,控件跟着还原'); else fail('撤销后容灾模式没还原');
await page.locator('[data-act="set.saveAll"]').click().catch(() => {});
await page.waitForTimeout(1500);
if (await page.locator('#set-bar[hidden]').count()) pass('保存后保存条收起'); else fail('保存后保存条还在');

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

// 故障处理掉以后,提醒要跟着消失(演示里的故障状态跟着操作走;以前巡检结果是一份死数据,修好了、删掉了还在报)
await page.goto(`${DEMO}#/dashboard`); await page.waitForTimeout(1000);
const attnOf = () => page.locator('#dash-attn-card').innerText().catch(() => '');
const attn0 = await attnOf();
if (/Frankfurt-exit/.test(attn0)) pass('概况「需要处理」列出故障的上游'); else fail(`概况没有列出故障的上游:${attn0.slice(0, 160)}`);
const stillBad = await page.evaluate(async () => {
  const api = await import('./js/api.js');
  await api.post('upstreams/test');
  const h = await api.get('upstreams/health');
  return ((h.results || []).find(r => r.name === 'Frankfurt-exit') || { servers: [{}] }).servers.every(s => s.state === 'fail');
});
if (stillBad) pass('没修的上游再测一遍还是不通(不会一测就自己变好)'); else fail('没修的上游一测就变通了');
// 没测通的原因不列在页面上:在悬停提示里(结论和那一行各有),点一下那一行才在下面露出来
await page.goto(`${DEMO}#/upstreams`); await page.waitForTimeout(900);
await page.locator('#up-res-1 .hbtn').click(); await page.waitForTimeout(300);
const why = await page.evaluate(() => {
  const sub = document.getElementById('up-sub-1'), sum = document.querySelector('#up-res-1 .hbtn'), item = sub && sub.querySelector('.hitem.why'), hw = item && item.querySelector('.hwhy');
  const shown = el => !!el && getComputedStyle(el).display !== 'none';
  const before = shown(hw), listed = /generate_204/.test(document.getElementById('up-body').innerText);
  if (item) item.focus();
  return { rows: sub ? sub.querySelectorAll('.hrow').length : 0, tipSum: !!sum && /generate_204/.test(sum.title), tipRow: !!item && /generate_204/.test(item.title), before, listed, after: shown(hw) };
});
if (why.rows >= 1 && why.tipSum && why.tipRow && !why.before && !why.listed && why.after) pass('上游页:故障原因在悬停提示里,平时不列出来,点那一行才露出来');
else fail(`上游页的故障原因不对:${JSON.stringify(why)}`);
await page.evaluate(async () => {
  const api = await import('./js/api.js');
  const u = (await api.get('upstreams')).find(x => x.name === 'Frankfurt-exit');
  await api.put('upstreams/' + u.id, u); // 编辑保存 = 修好了
});
await page.goto(`${DEMO}#/upstreams`); await page.waitForTimeout(500);
await page.goto(`${DEMO}#/dashboard`); await page.waitForTimeout(1200);
const attn1 = await attnOf();
if (!/Frankfurt-exit/.test(attn1)) pass('上游修好(编辑保存)后,概况不再报它故障'); else fail(`上游修好后概况还在报:${attn1.slice(0, 160)}`);
// 还有线路在用的上游删不掉(演示站和后端同一口径),报错里点名是哪些线路;「跟随」是个开关:停下不删来源、上游也还在,能再打开
const guard = await page.evaluate(async () => {
  const api = await import('./js/api.js');
  const ups = await api.get('upstreams');
  const fra = ups.find(x => x.name === 'Frankfurt-exit'), la = ups.find(x => x.name === 'LA-streaming');
  let delErr = '';
  try { await api.del('upstreams/' + fra.id); } catch (e) { delErr = e.message; }
  const still = (await api.get('upstreams')).some(x => x.id === fra.id);
  await api.put(`upstreams/${la.id}/source`, { follow: false });
  const off = (await api.get('upstreams')).find(x => x.id === la.id);
  await api.put(`upstreams/${la.id}/source`, { follow: true });
  const on = (await api.get('upstreams')).find(x => x.id === la.id);
  return { delErr, still, offStop: off && off.source ? off.source.stop : 'gone', onStop: on && on.source ? on.source.stop || '' : 'gone' };
});
if (guard.still && /Tokyo-HY2/.test(guard.delErr) && /Global-SS2022/.test(guard.delErr) && guard.offStop === 'user' && guard.onStop === '') pass('在用的上游删不掉并点名线路;停止跟随后上游和来源都还在,能再打开');
else fail(`上游的删除拦截 / 跟随开关不对:${JSON.stringify(guard)}`);
await page.goto(`${DEMO}#/upstreams`); await page.waitForTimeout(700);
await page.locator('#up-body [data-act="up.del"]').first().click(); await page.waitForTimeout(300);
const inUse = await page.evaluate(() => ({ title: document.getElementById('modal-title').textContent, rows: document.querySelectorAll('#modal-body .up-inuse li').length, danger: document.getElementById('modal-save').classList.contains('danger') }));
if (/Frankfurt-exit/.test(inUse.title) && inUse.rows === 2 && !inUse.danger) pass('上游页:点删除先列出在用它的线路,不给删除按钮');
else fail(`点删除在用的上游,弹出来的不对:${JSON.stringify(inUse)}`);
await page.keyboard.press('Escape'); await page.waitForTimeout(200);
// 中转入口换个端口:那台服务器上的「端口被占」跟着消失
const blockedOf = () => page.evaluate(async () => ((await (await import('./js/api.js')).get('nodes')).nodes.find(n => n.id === 2).status.blocked || []).length);
const b0 = await blockedOf();
await page.evaluate(async () => {
  const api = await import('./js/api.js');
  const l = (await api.get('lines')).find(x => x.id === 1);
  await api.put('lines/1', { ...l, forwards: (l.forwards || []).map(f => (f.via === 2 ? { ...f, port: 30011 } : f)) });
});
const b1 = await blockedOf();
if (b0 === 1 && b1 === 0) pass('中转入口换了端口,「端口被占」跟着消失'); else fail(`「端口被占」没有跟着线路变:改前 ${b0} 条、改后 ${b1} 条`);
// 上游的结果按服务器记:又有两台服务器上的线路用了同一条上游,延迟那一格就多出两个点(以前不管线路怎么改都只有 Tokyo 一个点);点开逐台看
const upServers = () => page.evaluate(async () => (((await (await import('./js/api.js')).get('upstreams/health')).results || []).find(r => r.name === 'LA-streaming') || { servers: [] }).servers.map(s => s.name));
const us0 = await upServers();
await page.evaluate(async () => {
  const api = await import('./js/api.js');
  const ls = await api.get('lines');
  for (const id of [2, 3]) await api.put('lines/' + id, { ...ls.find(x => x.id === id), upstreamId: 2 });
});
const us1 = await upServers();
if (us0.length === 1 && us1.length === 3) pass('上游的结果跟着线路部署到的服务器走(1 台 → 3 台)'); else fail(`上游的结果没有跟着线路变:改前 ${us0.join('/')},改后 ${us1.join('/')}`);
await page.goto(`${DEMO}#/dashboard`); await page.waitForTimeout(400);
await page.goto(`${DEMO}#/upstreams`); await page.waitForTimeout(900);
const dots = await page.locator('#up-res-2 .hdots .hd').count();
await page.locator('#up-res-2 .hbtn').click(); await page.waitForTimeout(300);
// 展开的明细是该行下面的一条通栏(表格排法),每台服务器正好一行
const rowsOpen = await page.locator('#up-sub-2 .hrow').count();
const rowH = await page.locator('#up-sub-2 .hrow').evaluateAll(els => Math.max(0, ...els.map(e => e.getBoundingClientRect().height)));
if (dots === 3 && rowsOpen === 3 && rowH > 0 && rowH < 34) pass('上游页:三台在用就是三个点,点这一行在下面展开,每台一行');
else fail(`上游页的点 / 展开行数不对:${dots} 个点、${rowsOpen} 行、最高的一行 ${Math.round(rowH)}px`);
// 节点页:每条线路的直连和中转入口各一行;倍率一格分得出跟随服务器和单独设的;行里的开关点了就存;设置弹框改倍率
await page.goto(`${DEMO}#/subnodes`); await page.waitForTimeout(900);
const sn0 = await page.evaluate(() => ({
  rows: document.querySelectorAll('#sn-body tr:not(.grp-row)').length, groups: document.querySelectorAll('#sn-body tr.grp-row').length,
  own: document.querySelectorAll('#sn-body .sn-own').length, follow: document.querySelectorAll('#sn-body .sn-follow').length,
  relay: document.querySelectorAll('#sn-body .sn-node .badge').length, off: document.querySelectorAll('#sn-body .switch input:not(:checked)').length,
}));
if (sn0.rows >= 5 && sn0.groups >= 3 && sn0.own >= 1 && sn0.follow >= 1 && sn0.rows === sn0.own + sn0.follow) pass(`节点页:${sn0.rows} 个节点按 ${sn0.groups} 条线路分组,单独设倍率的 ${sn0.own} 个、跟随服务器的 ${sn0.follow} 个`);
else fail(`节点页的行不对:${JSON.stringify(sn0)}`);
await page.locator('#sn-body .switch input:checked').first().evaluate(el => el.click()); await page.waitForTimeout(500);
const snOff = await page.locator('#sn-body .switch input:not(:checked)').count();
if (snOff === sn0.off + 1) pass('节点页:行里的「输出到订阅」开关点了就存'); else fail(`节点页的开关没存上:关着的 ${sn0.off} → ${snOff}`);
await page.locator('#sn-body [data-act="sn.edit"]').first().click(); await page.waitForTimeout(500);
await page.locator('#sn-ratio').fill('2.5'); await page.locator('#sn-cap').fill('40');
await page.locator('#modal-save').click(); await page.waitForTimeout(700);
const sn1 = await page.evaluate(() => { const tr = document.querySelector('#sn-body tr:not(.grp-row)'); return { open: !document.getElementById('modal').hidden, text: tr ? tr.innerText.replace(/\s+/g, ' ') : '' }; });
if (!sn1.open && /×2\.5/.test(sn1.text) && /40/.test(sn1.text)) pass('节点页:设置弹框里改倍率和容量,保存后行里跟着变'); else fail(`节点页保存后不对:${JSON.stringify(sn1)}`);
await page.locator('#sn-only [data-id="own"]').click(); await page.waitForTimeout(300);
const snOwn = await page.evaluate(() => ({ rows: document.querySelectorAll('#sn-body tr:not(.grp-row)').length, follow: document.querySelectorAll('#sn-body .sn-follow').length }));
if (snOwn.rows >= 2 && snOwn.rows < sn0.rows) pass(`节点页:「单独设置的」只留下设过的 ${snOwn.rows} 个`); else fail(`节点页筛选不对:${JSON.stringify(snOwn)}`);
// 节点页的筛选(线路 / 服务器 / 类型)和勾选批量:筛出一台服务器上的节点 → 全选 → 批量设倍率 → 这些行都变
await page.locator('#sn-only [data-id="all"]').click(); await page.waitForTimeout(200);
await page.locator('#sn-f-server').selectOption({ index: 2 }); await page.waitForTimeout(300);
const snSrv = await page.locator('#sn-body tr:not(.grp-row)').count();
await page.locator('#sn-f-kind').selectOption('relay'); await page.waitForTimeout(300);
const snRelay = await page.evaluate(() => ({ rows: document.querySelectorAll('#sn-body tr:not(.grp-row)').length, badges: document.querySelectorAll('#sn-body .sn-node .badge').length }));
if (snSrv >= 1 && snSrv < sn0.rows && snRelay.rows === snRelay.badges) pass(`节点页:按服务器筛出 ${snSrv} 个,再按类型只留中转的 ${snRelay.rows} 个`);
else fail(`节点页按服务器 / 类型筛选不对:服务器 ${snSrv} 个,中转 ${JSON.stringify(snRelay)}`);
await page.locator('#sn-f-kind').selectOption(''); await page.waitForTimeout(300);
await page.locator('#sn-sel-all').click(); await page.waitForTimeout(300);
const snBar = await page.evaluate(() => ({ shown: !document.getElementById('sn-batch-bar').hidden, text: document.getElementById('sn-batch-count').textContent, checked: document.querySelectorAll('#sn-body tr:not(.grp-row) input.sel:checked').length }));
await page.locator('[data-act="sn.batch"][data-id="ratio"]').click(); await page.waitForTimeout(400);
await page.locator('#sn-b-ratio').fill('4'); await page.locator('#modal-save').click(); await page.waitForTimeout(700);
const snAfter = await page.evaluate(() => ({ rows: document.querySelectorAll('#sn-body tr:not(.grp-row)').length, x4: [...document.querySelectorAll('#sn-body .sn-own')].filter(b => b.textContent.trim() === '×4').length }));
if (snBar.shown && snBar.checked === snSrv && snAfter.x4 === snSrv) pass(`节点页:筛选后全选 ${snSrv} 个,批量设倍率,这些行都变成 ×4`);
else fail(`节点页批量设倍率不对:${JSON.stringify({ snBar, snAfter, snSrv })}`);
await page.locator('[data-act="sn.batch"][data-id="off"]').click(); await page.waitForTimeout(600);
const snOffAll = await page.locator('#sn-body .switch input:checked').count();
await page.locator('[data-act="sn.clearSel"]').click(); await page.waitForTimeout(300);
const snCleared = await page.evaluate(() => document.getElementById('sn-batch-bar').hidden);
await page.locator('[data-act="sn.clearFilter"]').click(); await page.waitForTimeout(400);
const snAll = await page.locator('#sn-body tr:not(.grp-row)').count();
if (snOffAll === 0 && snCleared && snAll === sn0.rows) pass('节点页:批量「不输出」、取消选择、清除筛选都生效'); else fail(`节点页批量 / 清除不对:${JSON.stringify({ snOffAll, snCleared, snAll })}`);
// 概况「24 小时流量」那一格点开是流量明细:三栏排行,换范围后还在
await page.goto(`${DEMO}#/dashboard`); await page.waitForTimeout(900);
await page.locator('[data-act="dash.traffic"]').first().click(); await page.waitForTimeout(700);
const tbCols = await page.locator('.tb-cols .tb-col').count(), tbRows = await page.locator('.tb-cols .tb-row').count();
await page.locator('#tb-range [data-id="720"]').click(); await page.waitForTimeout(700);
const tbCols30 = await page.locator('.tb-cols .tb-col').count();
if (tbCols === 3 && tbRows >= 6 && tbCols30 === 3) pass('概况的流量一格点开是流量明细(用户 / 线路 / 服务器三栏,可换范围)'); else fail(`流量明细不对:${tbCols} 栏 ${tbRows} 行,30 天 ${tbCols30} 栏`);
await page.keyboard.press('Escape'); await page.waitForTimeout(200);
// 用户行「更多」里的停用 / 启用:启用着的显示停用,点了状态变成停用、这一项变成启用
await page.goto(`${DEMO}#/users`); await page.waitForTimeout(900);
const row2 = '#users-table tbody tr:nth-child(2)';
const menuLabel = async () => { await page.locator(`${row2} details.menu > summary`).click(); await page.waitForTimeout(200); return (await page.locator(`${row2} [data-act="user.toggle"]`).innerText()).trim(); };
const lb0 = await menuLabel();
await page.locator(`${row2} [data-act="user.toggle"]`).click(); await page.waitForTimeout(900);
const st1 = (await page.locator(`${row2} td:nth-child(3)`).innerText()).trim();
const lb1 = await menuLabel();
await page.keyboard.press('Escape'); await page.waitForTimeout(150);
if (/停用|Disable/.test(lb0) && /停用|Disabled/.test(st1) && /启用|Enable/.test(lb1) && lb0 !== lb1) pass('用户「更多」里的停用 / 启用跟着状态换'); else fail(`用户的停用 / 启用不对:菜单 ${lb0} → 状态 ${st1} → 菜单 ${lb1}`);
// 备份页:存一份到服务器,列表多一行;定时备份的四项都在页面上(开关、钟点、份数、推送到 Telegram)
await page.goto(`${DEMO}#/backup`); await page.waitForTimeout(900);
const bk0 = await page.locator('.bk-list tbody tr').count();
await page.locator('[data-act="bk.run"]').click(); await page.waitForTimeout(900);
const bk1 = await page.locator('.bk-list tbody tr').count();
const schedCtl = await page.evaluate(() => ['bk-on', 'bk-hour', 'bk-keep', 'bk-tg'].filter(id => { const el = document.getElementById(id); return el && el.getClientRects().length > 0 || (el && el.closest('.switch') && el.closest('.switch').getClientRects().length > 0); }).length);
if (bk0 >= 1 && bk1 === bk0 + 1 && schedCtl === 4) pass('备份页:存一份列表多一行,定时备份的四项都在页面上'); else fail(`备份页不对:${bk0} → ${bk1} 行,定时备份控件 ${schedCtl}/4`);

// 防火墙页:三台服务器各一行,点开看端口;规则的防呆(拉黑自己先拦下、确认了才存)、集群服务器不让拉黑;
// 放开 / 撤销;黑名单一条可以移出、可以升成规则。演示站的校验照着后端做,报的是同一句话
await page.goto(`${DEMO}#/fw`); await page.waitForTimeout(800);
const fwRows = await page.locator('#wall-sys tbody tr:not(.wall-sub)').count();
await page.locator('[data-act="fw.expand"][data-id="2"]').click(); await page.waitForTimeout(500);
const fwChips = await page.locator('#wall-sys tr.wall-sub .chip').count();
if (fwRows === 3 && fwChips >= 3) pass(`防火墙页:三台服务器,展开看到这台用到的 ${fwChips} 个端口`); else fail(`防火墙页的服务器表不对:${fwRows} 行,展开后 ${fwChips} 个端口`);
const fwApi = await page.evaluate(async () => {
  const api = await import('./js/api.js');
  const out = {};
  const tryIt = async fn => { try { await fn(); return 'ok'; } catch (e) { return (e.data && e.data.code) || String(e.status); } };
  const me = (await api.get('fw')).myIp;
  out.lock = await tryIt(() => api.post('fw/rules', { kind: 'deny', sources: me }));
  out.forced = await tryIt(() => api.post('fw/rules', { kind: 'deny', sources: me, force: true }));
  out.cluster = await tryIt(() => api.post('fw/rules', { kind: 'deny', sources: '198.51.100.10' }));
  out.sshNoMe = await tryIt(() => api.post('fw/rules', { kind: 'only', proto: 'tcp', ports: '22', sources: '203.0.113.200' }));
  out.allowSrc = await tryIt(() => api.post('fw/rules', { kind: 'allow', ports: '8080', sources: '203.0.113.200' }));
  let v = await api.get('fw');
  out.rules = v.rules.length;
  const mine = v.rules.find(r => r.sources === me + '/32');
  v = await api.del('fw/rules/' + mine.id);
  out.afterDel = v.rules.length;
  const sg = await api.post('fw/servers/2/open');
  out.opened = sg.state.opened === true && sg.state.blocking === false;
  const back = await api.post('fw/servers/2/undo');
  out.undone = !back.state.opened && back.state.blocking === true && back.state.allow === 'ok';
  v = await api.put('fw/settings', { autoAllow: false });
  out.allowOff = v.servers.find(s => s.id === 2).state.allow;
  await api.put('fw/settings', { autoAllow: true });
  v = await api.post('fw/bans', { ip: '203.0.113.99', note: 'smoke', seconds: 3600 });
  const b = v.bans.find(x => x.ip === '203.0.113.99');
  v = await api.post(`fw/bans/${b.id}/promote`, { nodeIds: [1] });
  out.promoted = v.rules.some(r => r.origin === 'ban' && r.sources === '203.0.113.99/32' && r.expiry > 0);
  v = await api.del('fw/bans/' + b.id);
  out.banGone = !v.bans.some(x => x.ip === '203.0.113.99');
  out.delLocal = await tryIt(() => api.del('nodes/1'));
  out.delCert = await tryIt(() => api.post('cert/delete', { name: 'panel.example.com' }));
  return out;
});
if (fwApi.lock === 'self_lockout' && fwApi.forced === 'ok' && fwApi.cluster === '400' && fwApi.sshNoMe === 'self_lockout' && fwApi.allowSrc === '400'
  && fwApi.afterDel === fwApi.rules - 1 && fwApi.opened && fwApi.undone && fwApi.allowOff === 'off' && fwApi.promoted && fwApi.banGone) pass('防火墙:拉黑自己先拦下、集群服务器不让拉黑、放开 / 撤销、关掉自动放行、黑名单升成规则');
else fail(`防火墙的演示逻辑不对:${JSON.stringify(fwApi)}`);
if (fwApi.delLocal === '400' && fwApi.delCert === '400') pass('演示站和后端一样:本机删不掉、正在用的证书删不掉'); else fail(`演示站的删除保护不对:${JSON.stringify({ delLocal: fwApi.delLocal, delCert: fwApi.delCert })}`);
await page.goto(`${DEMO}#/fw`); await page.waitForTimeout(600);
await page.locator('#wall-rules .card-head [data-act="fw.rule.add"]').click(); await page.waitForTimeout(300);
await page.locator('.wall-kind input[value="only"]').check({ force: true }); await page.waitForTimeout(150);
await page.fill('#wall-ports', '22'); await page.fill('#wall-src', '203.0.113.200');
await page.locator('#modal-save').click(); await page.waitForTimeout(500);
const lockUi = await page.evaluate(() => ({ title: document.getElementById('modal-title').textContent, danger: document.getElementById('modal-save').classList.contains('danger'), text: document.getElementById('modal-body').textContent }));
if (lockUi.danger && /22/.test(lockUi.text) && /198\.51\.100\.77/.test(lockUi.text)) pass('防火墙:会把自己挡在外面的规则,保存时先说明后果、再确认一次');
else fail(`防呆的确认框不对:${JSON.stringify(lockUi).slice(0, 200)}`);
await page.keyboard.press('Escape'); await page.waitForTimeout(300);
await page.keyboard.press('Escape'); await page.waitForTimeout(200);

// 流量奖励:顶栏的金币、活动页四页、入账筛选、积分榜查地址、登记地址、留名捐赠(提交 → 等到账 → 到账上榜)、
// 「godusevpn 节点订阅」的链接、去设置里关闭(精确到那一行)、设置里关掉后金币消失
await page.goto(`${DEMO}#/dashboard`); await page.reload(); await page.waitForSelector('.rw-coin', { timeout: 15000 }); await page.waitForTimeout(600);
const rwCoin = await page.evaluate(() => { const b = document.querySelector('.rw-coin'); return b ? { svg: !!b.querySelector('svg.rw-coin-svg'), ring: !!b.querySelector('.rw-ring'), n: (b.querySelector('.rw-n') || {}).textContent || '', tip: !!document.querySelector('.rw-tip') } : null; });
if (rwCoin && rwCoin.svg && !rwCoin.ring && rwCoin.n === '3' && rwCoin.tip) pass('流量奖励:顶栏有金币(没有外圈)、角标 3 积分、没登记时在概览页冒提醒气泡'); else fail(`顶栏的金币不对:${JSON.stringify(rwCoin)}`);
// 气泡不挡操作:点一下别处就收起,金币右下角留一个小点;换到别的页面也不再冒
await page.mouse.click(400, 500); await page.waitForTimeout(300);
const rwTip2 = await page.evaluate(() => ({ tip: !!document.querySelector('.rw-tip'), dot: !!document.querySelector('.rw-coin .rw-dot') }));
if (!rwTip2.tip && rwTip2.dot) pass('提醒气泡点别处就收起,金币上留一个小点'); else fail(`提醒气泡收起后不对:${JSON.stringify(rwTip2)}`);
await page.locator('.rw-coin').click(); await page.waitForTimeout(1300);
const rwOpen = await page.evaluate(() => ({ open: !!document.querySelector('#rw-ov:not([hidden]) .rw-sheet'), tabs: document.querySelectorAll('#rw-tabs button').length, pool: Number((document.getElementById('rw-pool-n') || { dataset: {} }).dataset.v) || 0,
  rows: document.querySelectorAll('#rw-led .rw-row:not(.head)').length, eq: document.querySelectorAll('.rw-eq-c').length, kept: !!document.querySelector('.rw-note.ok'), tipGone: !document.querySelector('.rw-tip'),
  sub: (document.querySelector('.rw-eq a.rw-link') || {}).href || '' }));
if (rwOpen.open && rwOpen.tabs === 4 && rwOpen.pool > 0 && rwOpen.rows === 10 && rwOpen.eq === 4 && !rwOpen.kept && rwOpen.tipGone && rwOpen.sub === 'https://godusevpn.org/subscription/') pass('活动页:四页、奖池数字、算式四格、逐笔入账十条、节点订阅链接可点');
else fail(`活动页打开后不对:${JSON.stringify(rwOpen)}`);
await page.locator('[data-act="rw.ledKind"][data-v="donation"]').click(); await page.waitForTimeout(500);
const rwDon = await page.evaluate(() => { const r = [...document.querySelectorAll('#rw-led .rw-row:not(.head)')]; return { n: r.length, all: r.every(x => x.querySelector('.rw-tag.don')), named: r.filter(x => x.querySelector('.c-name:not(.anon)')).length }; });
await page.locator('[data-act="rw.ledChain"][data-v="tron"]').click(); await page.waitForTimeout(500);
const rwTron = await page.locator('#rw-led .rw-row:not(.head)').count();
if (rwDon.n > 0 && rwDon.all && rwDon.named > 0 && rwTron === 1) pass(`入账明细:按「捐赠」筛出 ${rwDon.n} 条(${rwDon.named} 条带留名),再按 TRON 筛剩 1 条`); else fail(`入账明细的筛选不对:${JSON.stringify(rwDon)} tron=${rwTron}`);
await page.locator('#rw-tabs button[data-v="board"]').click(); await page.waitForTimeout(700);
const rwBrd0 = await page.evaluate(() => ({ rows: document.querySelectorAll('#rw-brd .rw-row:not(.head)').length, av: document.querySelectorAll('.rw-av').length, first: (document.querySelector('#rw-brd .rw-a') || { dataset: {} }).dataset.v || '' }));
await page.fill('#rw-q', rwBrd0.first.slice(0, 6)); await page.waitForTimeout(800);
const rwFound = await page.locator('#rw-brd .rw-row:not(.head)').count();
if (rwBrd0.rows === 10 && rwBrd0.av === 0 && rwFound >= 1 && rwFound < 10) pass('积分榜:每页十条、地址前没有彩色圆点、按地址能查到'); else fail(`积分榜不对:${JSON.stringify(rwBrd0)} 查到 ${rwFound}`);
// 登记地址:填错有提示,填对之后「我的」那条显示地址、积分榜里有「我」
await page.locator('.rw-me [data-act="rw.bindNow"]').click(); await page.waitForTimeout(300);
await page.fill('#rw-addr-in', 'not-an-address'); await page.locator('[data-act="rw.bindSave"]').click(); await page.waitForTimeout(300);
const rwBindErr = (await page.locator('#rw-bind-err').textContent() || '').trim();
await page.fill('#rw-addr-in', '7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU'); await page.locator('[data-act="rw.bindSave"]').click(); await page.waitForTimeout(900);
await page.fill('#rw-q', ''); await page.waitForTimeout(800);
const rwBound = await page.evaluate(() => ({ addr: (document.querySelector('.rw-addr-v code') || {}).textContent || '', me: document.querySelectorAll('#rw-brd .rw-row.me').length, cta: !!document.querySelector('.rw-me-c.cta') }));
if (rwBindErr && /7xKXt/.test(rwBound.addr) && rwBound.me === 1 && !rwBound.cta) pass('登记地址:填错有提示;登记后显示地址,积分榜里「我」那一行钉在最上面'); else fail(`登记地址不对:err=${rwBindErr} ${JSON.stringify(rwBound)}`);
// 留名捐赠
await page.locator('[data-act="rw.toDonate"]').click(); await page.waitForTimeout(700);
await page.locator('[data-act="rw.dnSubmit"]').click(); await page.waitForTimeout(200);
const rwDnErr = (await page.locator('#rw-dn-err').textContent() || '').trim();
await page.fill('#rw-dn-name', 'smoke 测试'); await page.fill('#rw-dn-amt', '12.34'); await page.locator('[data-act="rw.dnSubmit"]').click(); await page.waitForTimeout(800);
const rwWait = await page.evaluate(() => ({ timer: (document.getElementById('rw-dn-t') || {}).textContent || '', amt: (document.querySelector('.rw-dn-amt b') || {}).textContent || '', qr: ((document.querySelector('.rw-dn-qr img') || {}).src || '').slice(0, 14), board: document.querySelectorAll('#rw-dnr .rw-row:not(.head)').length }));
if (rwDnErr && /^5\d:\d\d$/.test(rwWait.timer) && rwWait.amt === '12.34' && rwWait.qr === 'data:image/svg' && rwWait.board >= 5) pass(`留名捐赠:空着提交有提示;提交后给出金额 12.34、倒计时 ${rwWait.timer}、收款二维码;下面是捐赠榜`);
else fail(`留名捐赠的等待状态不对:err=${rwDnErr} ${JSON.stringify(rwWait)}`);
await page.waitForTimeout(31500); // 演示站 20 秒后模拟到账,活动页每 10 秒查一次
const rwPaid = await page.evaluate(() => ({ done: !!document.querySelector('.rw-dn.done'), me: [...document.querySelectorAll('#rw-dnr .rw-row.me .c-dname b')].map(b => b.textContent).join() }));
if (rwPaid.done && rwPaid.me === 'smoke 测试') pass('留名捐赠:到账后显示「收到了」,捐赠榜里出现这个名字'); else fail(`留名捐赠到账后不对:${JSON.stringify(rwPaid)}`);
await page.locator('#rw-tabs button[data-v="pool"]').click(); await page.waitForTimeout(500);
await page.locator('[data-act="rw.ledKind"][data-v=""]').click(); await page.waitForTimeout(300);
await page.locator('[data-act="rw.ledChain"][data-v=""]').click(); await page.waitForTimeout(700);
const rwLedName = await page.evaluate(() => (document.querySelector('#rw-led .rw-row:not(.head) .c-name') || {}).textContent || '');
if (rwLedName === 'smoke 测试') pass('逐笔入账:最新一条是刚才那笔捐赠,带着留名'); else fail(`逐笔入账里没看到留名:${rwLedName}`);
// 规则页:去设置里关闭 → 设置页的那一行
await page.locator('#rw-tabs button[data-v="rules"]').click(); await page.waitForTimeout(500);
const rwRules = await page.locator('.rw-rules li').count();
await page.locator('[data-act="rw.toSettings"]').click(); await page.waitForTimeout(1800);
const rwSet = await page.evaluate(() => ({ hash: location.hash, focus: (document.activeElement || {}).id || '', inView: (() => { const el = document.getElementById('set-rewardEnabled'); if (!el) return false; const r = el.getBoundingClientRect(); return r.top > 0 && r.bottom < innerHeight; })(), ov: !!document.querySelector('#rw-ov:not([hidden])') }));
if (rwRules === 5 && rwSet.hash === '#/settings' && rwSet.focus === 'set-rewardEnabled' && rwSet.inView && !rwSet.ov) pass('规则五条;「去设置里关闭」跳到设置页、正好停在「流量奖励」那个开关上'); else fail(`去设置里关闭不对:规则 ${rwRules} 条 ${JSON.stringify(rwSet)}`);
await page.locator('label.switch:has(#set-rewardEnabled)').click(); await page.waitForTimeout(300);
await page.locator('[data-act="set.saveAll"]').click(); await page.waitForTimeout(1200);
const rwOff = await page.locator('.rw-coin').count();
await page.locator('label.switch:has(#set-rewardEnabled)').click(); await page.waitForTimeout(300);
await page.locator('[data-act="set.saveAll"]').click(); await page.waitForTimeout(1200);
const rwOn = await page.locator('.rw-coin').count();
if (rwOff === 0 && rwOn === 1) pass('设置里关掉流量奖励:金币消失;再打开:金币回来'); else fail(`设置里的开关不对:关 ${rwOff} 开 ${rwOn}`);

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
for (const route of ['dashboard', 'users', 'lines', 'nodes', 'cert', 'settings', 'fw']) {
  await m.goto(`${DEMO}#/${route}`); await m.waitForTimeout(700);
  const over = await m.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  if (over > 1) fail(`手机宽度下 #/${route} 横向溢出 ${over}px`); else pass(`手机宽度 #/${route} 不溢出`);
}
// 活动页在手机宽度下:四页都不能横向溢出,关闭按钮不压着金币
await m.goto(`${DEMO}#/dashboard`); await m.waitForTimeout(800);
await m.locator('.rw-coin').click(); await m.waitForTimeout(1200);
for (const tab of ['pool', 'board', 'donate', 'rules']) {
  await m.locator(`#rw-tabs button[data-v="${tab}"]`).click(); await m.waitForTimeout(700);
  const o = await m.evaluate(() => { const sc = document.getElementById('rw-scroll'); const bad = [...sc.querySelectorAll('*')].filter(el => { const r = el.getBoundingClientRect(); return r.width > 0 && (r.right > innerWidth + 1 || r.left < -1); }).length; return { over: sc.scrollWidth - sc.clientWidth, bad }; });
  if (o.over > 1 || o.bad) fail(`手机宽度下活动页「${tab}」横向溢出:${JSON.stringify(o)}`); else pass(`手机宽度 活动页「${tab}」不溢出`);
}
const mx = await m.evaluate(() => { const a = document.querySelector('.rw-x').getBoundingClientRect(), b = document.querySelector('.rw-art .rw-coin-svg').getBoundingClientRect(); return !(a.right < b.left || a.left > b.right || a.bottom < b.top || a.top > b.bottom); });
if (mx) fail('手机宽度下活动页的关闭按钮压在金币上'); else pass('手机宽度 活动页的关闭按钮不压金币');

await ctx.close();
}

await browser.close();
console.log(failures.length ? `\n${failures.length} 项失败` : '\nLive Demo 冒烟测试全部通过');
process.exit(failures.length ? 1 : 0);

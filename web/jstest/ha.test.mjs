// 主机容灾界面(web/assets/js/ha.js)与共用对话框(web/assets/js/ui.js 的 openModal)的前端测试:
// 对话框恢复、设为主机进度框的轮询不串到别的对话框、名单转义、DNS 测试的错误、顶部提示合并、角色卡片跟着服务器 id 走。
// 桩模块(api / i18n / errmsg)+ 小号桩 DOM 加载真实的 ha.js 与 ui.js。
// 运行:node --test web/jstest/ha.test.mjs
import test, { after, mock } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const jsDir = process.env.JS_DIR || path.join(here, '..', 'assets', 'js');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ha-test-'));
after(() => fs.rmSync(dir, { recursive: true, force: true }));

// ---- 桩模块 ----
fs.copyFileSync(path.join(jsDir, 'ha.js'), path.join(dir, 'ha.js'));
fs.copyFileSync(path.join(jsDir, 'ui.js'), path.join(dir, 'ui.js'));
fs.writeFileSync(path.join(dir, 'api.js'), `
export const get = (p, o) => globalThis.__api('GET', p, undefined, o);
export const post = (p, b, o) => globalThis.__api('POST', p, b, o);
export const SLOW = { timeout: 90000 };
export const LONG = { timeout: 600000 };
`);
// 文案:带参数的拼成 键|名=值|…,用例能看出用的是哪条、传了什么;字典里「有」的键用 __has 控制
fs.writeFileSync(path.join(dir, 'i18n.js'), `
export const t = (k, p) => (globalThis.__has && !globalThis.__has(k) ? k : (p && Object.keys(p).length ? k + '|' + Object.entries(p).map(([a, b]) => a + '=' + b).join('|') : k + '·'));
export const getLang = () => 'zh';
`);
fs.writeFileSync(path.join(dir, 'errmsg.js'), `export const trErr = s => 'tr(' + s + ')';`);

// ---- 桩 DOM ----
const attrName = k => 'data-' + k.replace(/[A-Z]/g, c => '-' + c.toLowerCase());
class El {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase(); this.id = ''; this.className = ''; this.dataset = {}; this.attrs = {};
    this.style = {}; this.kids = []; this.parent = null; this.html = ''; this.hidden = false; this.disabled = false;
    this.textContent = ''; this.value = ''; this.checked = false; this.on = {}; this.focused = false; this.rect = { left: 0, top: 0, bottom: 0, right: 0 };
    this.offsetWidth = 200; this.offsetHeight = 100;
    this.classList = { toggle() {}, add() {}, remove() {}, contains: c => this.className.split(/\s+/).includes(c) };
  }
  get innerHTML() { return this.html; }
  set innerHTML(v) { this.html = String(v); for (const k of this.kids) k.parent = null; this.kids = []; }
  appendChild(k) { if (k.parent) k.remove(); k.parent = this; this.kids.push(k); return k; }
  insertBefore(k, ref) { if (k.parent) k.remove(); k.parent = this; const i = this.kids.indexOf(ref); this.kids.splice(i < 0 ? this.kids.length : i, 0, k); return k; }
  remove() { if (this.parent) { this.parent.kids = this.parent.kids.filter(x => x !== this); this.parent = null; } }
  contains(x) { for (let e = x; e; e = e.parent) if (e === this) return true; return false; }
  get isConnected() { return doc.body.contains(this); }
  setAttribute(k, v) { if (k.startsWith('data-')) this.dataset[k.slice(5).replace(/-(.)/g, (_, c) => c.toUpperCase())] = String(v); else this.attrs[k] = String(v); }
  getAttribute(k) {
    if (k === 'class') return this.className;
    if (k === 'id') return this.id || null;
    if (k.startsWith('data-')) { const d = Object.keys(this.dataset).find(x => attrName(x) === k); return d === undefined ? null : this.dataset[d]; }
    return k in this.attrs ? this.attrs[k] : (k === 'disabled' && this.disabled ? '' : null);
  }
  hasAttribute(k) { return this.getAttribute(k) !== null; }
  addEventListener(type, fn) { (this.on[type] ||= []).push(fn); }
  removeEventListener(type, fn) { this.on[type] = (this.on[type] || []).filter(f => f !== fn); }
  dispatchEvent(ev) { for (const f of this.on[ev.type] || []) f(ev); return true; }
  focus() { this.focused = true; doc.activeElement = this; }
  getBoundingClientRect() { return this.isConnected ? this.rect : { left: 0, top: 0, bottom: 0, right: 0 }; }
  *walk() { for (const k of this.kids) { yield k; yield* k.walk(); } }
  querySelectorAll(sel) { return [...this.walk()].filter(e => matches(e, sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  closest(sel) { for (let e = this; e; e = e.parent) if (e instanceof El && matches(e, sel)) return e; return null; }
}
// 只认这些写法:标签、.类、#id、[属性]、[属性="值"]、:not([属性])、:checked,逗号分隔
function matches(el, sel) {
  return sel.split(',').some(one => {
    one = one.trim();
    const m = one.match(/^([a-z]+)?(.*)$/i);
    if (m[1] && el.tagName !== m[1].toUpperCase()) return false;
    for (const [, part] of m[2].matchAll(/(\.[\w-]+|#[\w-]+|\[[^\]]+\]|:not\(\[[^\]]+\]\)|:checked)/g)) {
      if (part[0] === '.') { if (!el.classList.contains(part.slice(1))) return false; }
      else if (part[0] === '#') { if (el.id !== part.slice(1)) return false; }
      else if (part === ':checked') { if (!el.checked) return false; }
      else if (part.startsWith(':not')) { if (el.hasAttribute(part.slice(6, -2))) return false; }
      else {
        const [, k, v] = part.match(/^\[([\w-]+)(?:="([^"]*)")?\]$/);
        const got = el.getAttribute(k);
        if (got === null || (v !== undefined && got !== v)) return false;
      }
    }
    return true;
  });
}
const doc = new El('#document');
doc.body = new El('body');
doc.body.parent = null;
doc.activeElement = null;
doc.createElement = tag => new El(tag);
doc.getElementById = id => [...doc.body.walk()].find(e => e.id === id) || null;
doc.querySelector = sel => doc.body.querySelector(sel);
doc.querySelectorAll = sel => doc.body.querySelectorAll(sel);
const mk = (tag, props = {}, parent = doc.body) => Object.assign(parent.appendChild(new El(tag)), props);
globalThis.document = doc;
globalThis.window = { innerWidth: 1200, innerHeight: 800, on: {}, addEventListener(type, fn) { (this.on[type] ||= []).push(fn); } };
globalThis.CustomEvent = class { constructor(type) { this.type = type; } };
globalThis.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.location = { hash: '' };

// index.html 里固定有的元素
const main = mk('div', { className: 'main' });
const page = mk('div', { id: 'page' }, main);
const modal = mk('div', { id: 'modal', hidden: true });
mk('div', { className: 'modal-box' }, modal);
mk('h3', { id: 'modal-title' }, modal);
const mbody = mk('div', { id: 'modal-body' }, modal);
mk('p', { id: 'modal-err' }, modal);
const save = mk('button', { id: 'modal-save' }, modal);
const cancel = mk('button', { id: 'modal-cancel' }, modal);
mk('div', { id: 'toast' });
const tbody = mk('tbody', { id: 'nodes-body' });

// 接口桩:默认按表回;用例可以换成手动放行的
let api = () => Promise.resolve({});
globalThis.__api = (...a) => api(...a);

const ui = await import(pathToFileURL(path.join(dir, 'ui.js')).href);
const ha = await import(pathToFileURL(path.join(dir, 'ha.js')).href);

const click = target => { for (const f of doc.on.click || []) f({ target, preventDefault() {}, stopPropagation() {} }); };
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };

test('打开新对话框时恢复主按钮和取消键(设为主机的确认框、进度框改过它们)', () => {
  ui.openModal('A', 'a', async () => {});
  save.disabled = true; // 对不合格的服务器打开设为主机:按钮禁用着关掉
  cancel.textContent = 'common.close·'; // 进度框把取消键改成「关闭」
  ui.openModal('B', 'b', async () => {});
  assert.equal(save.disabled, false);
  assert.equal(cancel.textContent, 'common.cancel·');
  ui.closeModal();
});

test('进度框关掉、或换成别的对话框后,轮询回来不再改写对话框', async () => {
  mock.timers.enable({ apis: ['setTimeout'] });
  try {
    const plan = { targetName: 'TW', selfName: 'HK' };
    const st = { id: 's1', stage: 'send', startedAt: 1000, freezeUntil: 1000 + 15 * 60 };
    // ① 关掉进度框,再打开别的对话框:进行中的请求回来后不能写进新对话框
    let release;
    api = (m, p) => (p === 'ha' ? new Promise(r => { release = () => r({ switch: { ...st, stage: 'done', newPanel: 'https://x' } }); }) : Promise.resolve({}));
    ui.openModal('设为主机', '', async () => {});
    ha.showProgress(st, plan);
    mock.timers.tick(1000); // 第一轮轮询发出
    await flush();
    ui.closeModal();
    ui.openModal('别的对话框', 'NEW', async () => {});
    release();
    await flush();
    assert.equal(mbody.innerHTML, 'NEW', '别的对话框的正文被进度覆盖了');
    assert.equal(mbody.kids.length, 0);
    ui.closeModal();

    // ② 还开着的进度框照常更新到完成
    api = (m, p) => Promise.resolve(p === 'ha' ? { switch: { ...st, stage: 'done', newPanel: 'https://new.example' } } : {});
    ui.openModal('设为主机', '', async () => {});
    ha.showProgress(st, plan);
    const prog = mbody.kids[0];
    assert.match(prog.innerHTML, /ha\.stage\.send/);
    mock.timers.tick(1000);
    await flush();
    assert.match(prog.innerHTML, /ha\.sm\.done/);
    assert.match(prog.innerHTML, /https:\/\/new\.example/);
    ui.closeModal();
  } finally {
    mock.timers.reset();
    api = () => Promise.resolve({});
  }
});

test('结果不明:显示原因、最晚截止和「放弃」;非终态出错也显示原因', () => {
  const plan = { targetName: 'TW', selfName: 'HK' };
  const unk = ha.progressHTML({ id: 's', stage: 'unknown', err: '提交请求没有回话', startedAt: 100, freezeUntil: 1900 }, plan);
  assert.match(unk, /ha\.sm\.unknown/);
  assert.match(unk, /tr\(提交请求没有回话\)/);
  assert.match(unk, /ha\.sm\.unknownHow/);
  assert.match(unk, /ha\.sm\.running\|m=30/, '最长暂停按冻结截止算');
  const retry = ha.progressHTML({ id: 's', stage: 'commit', err: '目标没回话', freezeUntil: 1800 }, plan);
  assert.match(retry, /tr\(目标没回话\)/);
  assert.match(ha.progressHTML({ id: 's', stage: 'fenced' }, plan), /ha\.sm\.fenced/);
  const done = ha.progressHTML({ id: 's', stage: 'done', newPanel: 'https://p', newReseller: 'https://r' }, plan);
  assert.match(done, /ha\.sm\.newReseller/);
  assert.match(done, /https:\/\/r/);
});

test('主机卡片里的备用名单要转义', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  api = (m, p) => Promise.resolve(p === 'ha' ? { role: 'master', standbyIds: [2], config: { mode: 'confirm' }, standbys: [] } : {});
  await ha.loadHA();
  const nodes = [{ id: 1, name: 'HK', isLocal: true }, { id: 2, name: evil }];
  ha.roleChip(nodes[0], { nodes });
  const html = ha.cardHTML(nodes[0], { kind: 'master', local: true });
  assert.ok(!html.includes('<img'), html);
  assert.ok(html.includes('&lt;img'));
  api = () => Promise.resolve({});
});

test('DNS 测试:令牌无效时说出错误,不说成「没找到记录」', () => {
  const out = ha.dnsTestHTML({ records: [], error: 'Cloudflare 令牌无效' });
  assert.match(out, /tr\(Cloudflare 令牌无效\)/);
  assert.ok(!out.includes('ha.dns.none'));
  assert.match(ha.dnsTestHTML({ records: [] }), /ha\.dns\.none/);
  const both = ha.dnsTestHTML({ records: [{ name: 'a.example', type: 'A', content: '1.1.1.1', match: true }], error: '区域 b.example 没权限' });
  assert.match(both, /区域 b\.example 没权限/);
  assert.match(both, /a\.example/);
});

test('DNS 提示按三态写、列原值 → 新值;订阅地址保不保持用后端的结论', () => {
  const changes = [{ name: 'sub.example.com', type: 'A', from: '1.1.1.1', to: '2.2.2.2' }];
  assert.match(ha.dnsHTML({ subKept: true, host: 'sub.example.com', ip: '2.2.2.2', dnsState: 'done', changes }), /ha\.n\.dnsDone.*1\.1\.1\.1 → 2\.2\.2\.2/);
  assert.match(ha.dnsHTML({ subKept: true, host: 'sub.example.com', dnsState: 'partial', changes }), /ha\.n\.dnsPartial/);
  assert.match(ha.dnsHTML({ subKept: true, host: 'sub.example.com', dnsState: 'failed' }), /ha\.n\.dnsFailed/);
  // 订阅主机名是 IPv6(含字母)、或端口冲突:后端给 subKept=false,不提示改 DNS 后订阅地址不变
  assert.equal(ha.dnsHTML({ subKept: false, host: '2001:db8::1' }), '');
  assert.match(ha.dnsHTML({ subKept: true, host: 'sub.example.com', ip: '2.2.2.2' }), /ha\.n\.dnsTodo/);
});

test('顶部提示:同类合并、带次数,最多 3 条其余折叠;有未读的「变为副机」时「已接入」并进去', () => {
  const ns = [
    { id: 'a1', kind: 'switchFailed', name: 'TW', at: 1 }, { id: 'a2', kind: 'switchFailed', name: 'TW', at: 2 },
    { id: 'a3', kind: 'switchFailed', name: 'TW', at: 3 }, { id: 'a4', kind: 'switchFailed', name: 'JP', at: 4 },
    { id: 'd1', kind: 'demoted', name: 'TW', at: 5, reason: 'yielded' }, { id: 'j1', kind: 'joined', at: 6 },
    { id: 'b1', kind: 'became', name: 'HK', at: 7 }, { id: 'f1', kind: 'formerBack', name: 'HK', at: 8 },
  ];
  const groups = ha.groupNotices(ns);
  const sf = groups.find(g => g.kind === 'switchFailed');
  assert.equal(sf.count, 4);
  assert.equal(sf.latest.id, 'a4');
  assert.ok(!groups.some(g => g.kind === 'joined'), '「已接入」应并进「变为副机」');
  assert.deepEqual(groups.find(g => g.kind === 'demoted').ids.sort(), ['d1', 'j1']);
  ha.haNotices({ ha: { notices: ns } });
  const bars = doc.querySelectorAll('.alert-bar[data-ha]');
  assert.equal(bars.filter(b => b.dataset.ha.startsWith('ha-n-')).length, 3);
  assert.ok(bars.some(b => b.dataset.ha === 'ha-more'));
  const sfBar = bars.find(b => b.dataset.ha === 'ha-n-switchFailed');
  assert.match(sfBar.innerHTML, /ha\.n\.times\|n=4/);
  assert.match(sfBar.innerHTML, /data-id="a4,a3,a2,a1"/);
  assert.match(bars.find(b => b.dataset.ha === 'ha-n-demoted').innerHTML, /ha\.n\.demoted\.yielded/);
  ha.haNotices({ ha: { notices: [] } });
  assert.equal(doc.querySelectorAll('.alert-bar[data-ha]').length, 0);
});

test('顶部条:不能接任时不给按钮,写原因', () => {
  ha.haNotices({ ha: { mode: 'confirm', lost: 600, masterName: 'HK', rank: 1, canTakeover: false, why: 'peerSeesMaster' } });
  const bar = doc.querySelector('.alert-bar[data-ha="ha-lost"]');
  assert.ok(!bar.innerHTML.includes('data-act="ha.takeover"'));
  assert.match(bar.innerHTML, /ha\.ev\.peerSeesMaster/);
  ha.haNotices({ ha: { mode: 'confirm', lost: 600, masterName: 'HK', rank: 1, canTakeover: true } });
  assert.match(doc.querySelector('.alert-bar[data-ha="ha-lost"]').innerHTML, /data-act="ha\.takeover"/);
  ha.haNotices({ ha: null });
});

test('角色卡片跟着服务器 id 走:表格重画后照样定位、回焦、再点收起', async () => {
  api = (m, p) => Promise.resolve(p === 'ha' ? { role: 'master', standbyIds: [2], config: { mode: 'confirm' }, standbys: [{ id: 2, eligible: true }] } : { ok: true, blockers: [] });
  await ha.loadHA();
  const nodes = [{ id: 1, name: 'HK', isLocal: true }, { id: 2, name: 'TW' }];
  const chip = id => { const b = new El('button'); b.className = 'role-chip'; b.dataset.act = 'ha.card'; b.dataset.id = String(id); b.rect = { left: 300, top: 100, bottom: 120, right: 360 }; return b; };
  const draw = () => { tbody.innerHTML = ''; nodes.forEach(n => tbody.appendChild(chip(n.id))); };
  draw();
  click(tbody.querySelector('.role-chip[data-id="2"]'));
  await flush();
  const pop = doc.querySelector('.ha-pop');
  assert.ok(pop, '卡片没打开');
  assert.equal(pop.style.left, '300px');
  // 重画(每几秒一次):标签元素全换了。roleChip 照样给打开着的那台 aria-expanded="true"
  assert.match(ha.roleChip(nodes[1], { nodes }), /aria-expanded="true"/);
  assert.match(ha.roleChip(nodes[0], { nodes }), /aria-expanded="false"/);
  draw();
  const fresh = tbody.querySelector('.role-chip[data-id="2"]');
  fresh.rect = { left: 320, top: 200, bottom: 220, right: 380 };
  for (const f of window.on.scroll || []) f({});
  assert.equal(pop.style.left, '320px', '滚动后卡片应贴着新的标签,而不是跑到左上角');
  assert.equal(pop.style.top, '226px');
  // 再点同一台的(新)标签:收起,并把焦点还给它
  click(fresh);
  await flush();
  assert.equal(doc.querySelector('.ha-pop'), null);
  // 打开后那一行被删了:滚动时收起
  click(tbody.querySelector('.role-chip[data-id="2"]'));
  await flush();
  assert.ok(doc.querySelector('.ha-pop'));
  tbody.innerHTML = '';
  for (const f of window.on.scroll || []) f({});
  assert.equal(doc.querySelector('.ha-pop'), null);
  // Esc 收起时回焦到当前的标签
  draw();
  click(tbody.querySelector('.role-chip[data-id="2"]'));
  await flush();
  draw();
  const now = tbody.querySelector('.role-chip[data-id="2"]');
  for (const f of doc.on.keydown || []) f({ key: 'Escape', stopPropagation() {} });
  assert.equal(doc.activeElement, now);
  assert.equal(now.getAttribute('aria-expanded'), 'false');
  api = () => Promise.resolve({});
});

test('设为主机计划:名单数组按界面分隔符拼、服务名翻译、旧写法 text 原文翻译', () => {
  assert.equal(ha.planText({ code: 'oldNodes', names: ['A', 'B'] }), 'ha.plan.oldNodes|names=Areach.sep·B');
  assert.match(ha.planText({ code: 'subPortSvc', args: { port: '2096', svc: 'panel' } }), /svc=ha\.svc\.panel/);
  assert.equal(ha.planText({ code: 'text', args: { text: '某句中文' } }), 'tr(某句中文)');
  assert.match(ha.readyText({ code: 'replicaFail', level: 'warn', names: ['TW'], args: { err: '连接被拒绝' } }), /^ha\.ready\.replicaFail\.warn\|err=tr\(连接被拒绝\)\|names=TW$/);
});

test('Cloudflare 令牌:证书页和容灾共用一个「去 Cloudflare 创建 ↗」,新标签页打开、不带来源', () => {
  const h = ui.cfTokenLink();
  assert.match(h, /href="https:\/\/dash\.cloudflare\.com\/profile\/api-tokens\?permissionGroupKeys=/);
  assert.match(h, /target="_blank"/);
  assert.match(h, /rel="noopener noreferrer"/);
  assert.match(h, /cf\.create· ↗/);
  assert.match(h, /cf\.steps/);
  assert.match(fs.readFileSync(path.join(jsDir, 'pages', 'cert.js'), 'utf8'), /id="c-cftoken"[^`]*\$\{cfTokenLink\(\)\}/);
  assert.match(fs.readFileSync(path.join(jsDir, 'ha.js'), 'utf8'), /id="ha-dnsToken"[^`]*\$\{cfTokenLink\(\)\}/);
});

test('请求超时档:设为主机、保存、重新信任、放弃、DNS 测试用 SLOW,接任用 LONG', () => {
  const src = fs.readFileSync(path.join(jsDir, 'ha.js'), 'utf8');
  const call = p => (src.match(new RegExp(`post\\('${p.replace(/\//g, '\\/')}',[\\s\\S]*?\\}(?:, (SLOW|LONG))?\\);`)) || [])[1];
  assert.equal(call('ha/set-master'), 'SLOW');
  assert.equal(call('ha/repin'), 'SLOW');
  assert.equal(call('ha/switch/abandon'), 'SLOW');
  assert.equal(call('ha/dns-test'), 'SLOW');
  assert.equal(call('ha/takeover'), 'LONG');
  assert.match(src, /post\('ha', \{ \.\.\.body, password, code \}, SLOW\)/);
});

// 用户页(web/assets/js/pages/users.js)换套餐 / 续费对话框的前端测试:桩模块 + 桩 DOM 加载真实的 users.js,后端按面板接口仿真。
// 运行:node --test web/jstest/users.test.mjs
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = process.env.USERS_JS || path.join(here, '..', 'assets', 'js', 'pages', 'users.js');

// ---- 桩模块 ----
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'users-test-'));
fs.mkdirSync(path.join(dir, 'pages'));
fs.copyFileSync(src, path.join(dir, 'pages', 'users.js'));
const stub = (name, body) => fs.writeFileSync(path.join(dir, name), body);
stub('app.js', `export const state = globalThis.__state;
export async function load(...what) { globalThis.__loads.push(what.join(',')); }
export const isReseller = () => false;
// 列表页从地址栏带进来的筛选(#/users?f=…):用例不带,给空的
export const takeHashParams = () => new URLSearchParams(globalThis.__hash || '');`);
stub('api.js', `export const get = (p, o) => globalThis.__api('GET', p);
export const post = (p, b, o) => globalThis.__api('POST', p, b);
export const put = (p, b, o) => globalThis.__api('PUT', p, b);
export const del = (p, o) => globalThis.__api('DELETE', p);
export const qrUrl = () => '';
export const upload = async () => ({});`);
// 带参数的文案把参数拼在后面(k|名=值|…),用例能看出传了什么
stub('i18n.js', `export const t = (k, p) => p && Object.keys(p).length ? k + '|' + Object.entries(p).map(([a, b]) => a + '=' + b).join('|') : k;`);
stub('chart.js', `export const barChart = () => ''; export const bucketFor = () => 3600;`);
stub('linepicker.js', `export const lineItems = () => []; export const keysFromRefs = () => []; export const refLabels = () => [];
export const linePicker = () => ({ read: () => [] }); export const refChips = () => '';`);
stub('ui.js', `const el = (id) => document.getElementById(id);
export const esc = (s) => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
export const deviceCount = (a) => (a || []).length;
export const gbValue = (b) => b / 1073741824; export const gbBytes = (g) => g * 1073741824;
export const fmtBytes = (n) => String(n) + 'B'; export const fmtDay = (n) => 'D' + n; export const fmtDate = (n) => 'T' + n;
export const fmtDuration = (n) => n + 's'; export const fmtRelative = () => 'x'; export const daysLeft = () => 10;
export const toast = (m, k = '') => { globalThis.__toasts.push([k, m]); };
export const confirm = async () => true;
export const openModal = (title, html, onSave, opts) => { globalThis.__modal = { title, html, onSave, opts }; };
export const closeModal = () => {}; export const openDrawer = () => {}; export const closeDrawer = () => {};
export const registerActions = (a) => { globalThis.__actions = Object.assign(globalThis.__actions || {}, a); };
export const badge = (s) => String(s); export const dot = () => ''; export const progress = () => '';
export const field = () => ''; export const check = () => ''; export const empty = () => '';
export const fv = (id) => el(id).value; export const fchk = (id) => !!el(id).checked;
export const matches = () => true; export const debounce = (fn) => (...a) => fn(...a);
export const copy = async (s) => { (globalThis.__copied ||= []).push(s); };
export const setHTML = (t, h) => { const e = typeof t === 'string' ? el(t) : t; if (e) e.innerHTML = h; };
export const setText = (t, s) => { const e = typeof t === 'string' ? el(t) : t; if (e) e.textContent = s; };
export const tzOffsetMinutes = () => 0;
// 长名单折叠成 +N:用例只看内容,原样拼起来
export const chipList = (items) => '<span class="chips">' + (items || []).join('') + '</span>';`);
after(() => fs.rmSync(dir, { recursive: true, force: true }));

// ---- 桩 DOM:按 id 取元素(没有就建),能挂事件、触发事件 ----
let els;
const mkEl = (id) => ({
  id, value: '', checked: false, disabled: false, hidden: false, innerHTML: '', textContent: '', min: '', title: '', dataset: {},
  classList: { toggle() {}, add() {}, remove() {} },
  on: {}, addEventListener(type, fn) { (this.on[type] ||= []).push(fn); },
  fire(type, ev = {}) { for (const fn of this.on[type] || []) fn({ target: this, ...ev }); },
  querySelector: () => null,
});
globalThis.document = {
  getElementById: (id) => (els[id] ||= mkEl(id)),
  querySelectorAll: () => [],
  querySelector: () => null,
};
const flush = async (n = 6) => { for (let i = 0; i < n; i++) await new Promise((r) => setTimeout(r, 0)); };

// ---- 仿真后端:预览挂起,由用例决定什么时候回来、回什么;正式提交按用例给的结果 ----
const plan = (id, name) => ({ id, name, volumeGb: 10, days: 30, price: 0 });
const pstate = (o = {}) => ({ planId: 1, plan: '月付', volume: 10, used: 5, expiry: 100, activateDays: 0, autoReset: false, resetDays: 0, nextReset: 0,
  deviceLimit: 0, speedUp: 0, speedDown: 0, enabled: true, disabledReason: '', lineRefs: [], ...o });
const result = (needsForce, o = {}) => ({ op: 'replace', direction: 'upgrade', periods: 1, usage: 'reset', dryRun: true, from: pstate(), to: pstate({ planId: 2, plan: '季付' }),
  remaining: {}, credit: null, warnings: needsForce ? [{ code: 'expiry_earlier', text: 'x', args: { from: 1, to: 2 }, block: true }] : [], needsForce, ...o });

function backend() {
  const be = { previews: [], calls: [], submit: null };
  globalThis.__api = (method, p, body) => {
    be.calls.push({ method, p, body: body && JSON.parse(JSON.stringify(body)) });
    if (method === 'POST' && body && body.dryRun) {
      let resolve, reject;
      const pr = new Promise((res, rej) => { resolve = res; reject = rej; });
      be.previews.push({ body, resolve, reject });
      return pr;
    }
    if (method === 'POST' && (/\/change$/.test(p) || /\/plan$/.test(p))) return be.submit(body);
    if (method === 'GET') return Promise.resolve(be.onGet ? be.onGet(p) : []);
    return Promise.reject(new Error('404 ' + method + ' ' + p));
  };
  be.submits = () => be.calls.filter((c) => c.method === 'POST' && c.body && !c.body.dryRun);
  return be;
}

// app.js 的桩在第一次加载时就把 state 绑到这个对象上:每个用例只能改它的内容,不能换对象
globalThis.__state = {};
let seq = 0;
async function setup() {
  els = {};
  globalThis.__toasts = [];
  globalThis.__loads = [];
  for (const k of Object.keys(globalThis.__state)) delete globalThis.__state[k];
  Object.assign(globalThis.__state, {
    users: [{ id: 1, name: 'alice', planId: 1, enabled: true, up: 0, down: 0, volume: 10, expiry: 100 }],
    plans: [plan(1, '月付'), plan(2, '季付')], lines: [], nodes: [], onlines: { users: [] }, limitStates: [], status: {},
  });
  const be = backend();
  const mod = await import(pathToFileURL(path.join(dir, 'pages', 'users.js')).href + '?case=' + (++seq));
  // 表单初值(真实页面里由 openModal 渲染出来)
  Object.assign(document.getElementById('pc-plan'), { value: '2' });
  Object.assign(document.getElementById('pc-periods'), { value: '1' });
  Object.assign(document.getElementById('pc-usage'), { value: 'auto' });
  Object.assign(document.getElementById('pc-remark'), { value: '' });
  return { be, A: globalThis.__actions, mod };
}
const save = () => globalThis.__modal.onSave();
const $ = (id) => document.getElementById(id);
const sent = (b) => ({ planId: b.planId, mode: b.mode, periods: b.periods, usage: b.usage, force: b.force });

test('换套餐:参数一变就取消「仍然执行」并禁用提交,直到新预览回来;提交的是预览过的参数', async () => {
  const { be, A } = await setup();
  A['user.change']('1');
  await flush();
  assert.equal(be.previews.length, 1, '打开就预览一次');
  assert.equal($('modal-save').disabled, true, '第一次预览回来之前不能提交');
  be.previews[0].resolve(result(true));
  await flush();
  assert.equal($('modal-save').disabled, false);
  assert.equal($('pc-force-wrap').hidden, false, '有要确认的影响:显示「仍然执行」');

  $('pc-force').checked = true; // 用户确认了这份预览……
  $('pc-periods').value = '3'; // ……然后又改了期数
  $('pc-periods').fire('input');
  await flush();
  assert.equal($('pc-force').checked, false, '参数变了,勾选要取消');
  assert.equal($('modal-save').disabled, true, '新预览回来之前禁用提交');
  await assert.rejects(save(), /pc\.previewPending/);
  assert.equal(be.submits().length, 0, '没有预览过的参数不能提交');

  be.previews[1].resolve(result(true, { periods: 3 }));
  await flush();
  assert.equal($('modal-save').disabled, false);
  $('pc-usage').value = 'keep'; // 预览回来以后改了用量处理,但新的预览出错了
  $('pc-usage').fire('change');
  await flush();
  be.previews[2].reject(Object.assign(new Error('boom'), { status: 400, data: { error: 'boom' } }));
  await flush();
  assert.equal($('modal-save').disabled, true, '预览出错:没有可确认的预览,不能提交');
  assert.equal($('pc-force-wrap').hidden, true);
  await assert.rejects(save(), /pc\.previewPending/);

  $('pc-usage').value = 'auto';
  $('pc-usage').fire('change');
  await flush();
  be.previews[3].resolve(result(true, { periods: 3 }));
  await flush();
  $('pc-force').checked = true;
  be.submit = async () => ({ ...result(true), dryRun: false, opId: 7 });
  await save();
  const subs = be.submits();
  assert.equal(subs.length, 1);
  assert.deepEqual(sent(subs[0].body), { planId: 2, mode: 'replace', periods: 3, usage: 'auto', force: true });
  assert.match(subs[0].body.requestId, /^ui-[0-9a-f]{24}$/);
});

test('换套餐:预览之后(防抖期间)回来的旧预览不会顶掉新参数', async () => {
  const { be, A } = await setup();
  A['user.change']('1');
  await flush();
  $('pc-periods').value = '2';
  $('pc-periods').fire('input');
  await flush();
  be.previews[0].resolve(result(false)); // 旧参数的预览这才回来
  await flush();
  assert.equal($('modal-save').disabled, true, '旧预览不能解锁提交');
  await assert.rejects(save(), /pc\.previewPending/);
  be.previews[1].resolve(result(false, { periods: 2 }));
  await flush();
  be.submit = async () => ({ ...result(false), dryRun: false });
  await save();
  assert.deepEqual(sent(be.submits()[0].body), { planId: 2, mode: 'replace', periods: 2, usage: 'auto', force: false });
});

test('换套餐:提交时 409 needs_force 给面板用户看得懂的提示,按最新预览重新确认', async () => {
  const { be, A } = await setup();
  A['user.change']('1');
  await flush();
  be.previews[0].resolve(result(false));
  await flush();
  assert.equal($('pc-force-wrap').hidden, true);
  const preview = result(true, { to: pstate({ planId: 2, plan: '季付', expiry: 50 }) });
  be.submit = async () => { throw Object.assign(new Error('这次变更有需要确认的影响(见 warnings),确认后带 force: true 重试'), { status: 409, data: { code: 'needs_force', preview, warnings: preview.warnings } }); };
  await assert.rejects(save(), (e) => e.message === 'pc.err.needsForce');
  assert.equal($('pc-force-wrap').hidden, false, '要显示「仍然执行」');
  assert.equal($('pc-force').checked, false);
  assert.match($('pc-preview').innerHTML, /D50/, '预览换成了服务端给的最新预览');
  assert.equal($('modal-save').disabled, false);
  $('pc-force').checked = true;
  be.submit = async () => ({ ...preview, dryRun: false });
  await save();
  assert.deepEqual(sent(be.submits()[1].body), { planId: 2, mode: 'replace', periods: 1, usage: 'auto', force: true });
});

test('换套餐:409 idempotency_conflict 说明上一次提交其实已经生效,并刷新列表', async () => {
  const { be, A } = await setup();
  A['user.change']('1');
  await flush();
  be.previews[0].resolve(result(false));
  await flush();
  be.submit = async () => { throw Object.assign(new Error('requestId 已用于另一笔不同的请求'), { status: 409, data: { code: 'idempotency_conflict' } }); };
  await assert.rejects(save(), (e) => e.message === 'pc.err.idemConflict');
  assert.ok(globalThis.__loads.includes('users'), '要刷新用户列表');
});

test('续费:409 idempotency_conflict 同样给面板用户看得懂的提示', async () => {
  const { be, A } = await setup();
  Object.assign($('f-plan'), { value: '1' });
  Object.assign($('f-mode'), { value: 'renew' });
  Object.assign($('f-periods'), { value: '2' });
  A['user.renew']('1');
  await flush();
  be.submit = async () => { throw Object.assign(new Error('requestId 已用于另一笔不同的请求'), { status: 409, data: { code: 'idempotency_conflict' } }); };
  await assert.rejects(save(), (e) => e.message === 'pc.err.idemConflict');
  assert.ok(globalThis.__loads.includes('users'));
});

// 套用套餐(续费 / 延期 / 换套餐)一律会启用手动停用的用户(外部 API 也一样,计费系统依赖这个行为),
// 而「延长 30 天」、批量延期、重置流量都不会:执行前要在对话框里说清楚,免得月底批量续费把封掉的人放回来。
const manualOff = (o = {}) => ({ planId: 1, enabled: false, disabledReason: 'manual', up: 0, down: 0, volume: 10, expiry: 100, ...o });

test('续费:手动停用的用户,对话框里明说续费后会被启用;只归入套餐不改启停,不提示', async () => {
  const { A } = await setup();
  Object.assign(globalThis.__state.users[0], manualOff());
  Object.assign($('f-plan'), { value: '1' });
  Object.assign($('f-mode'), { value: 'renew' });
  Object.assign($('f-periods'), { value: '1' });
  A['user.renew']('1');
  await flush();
  assert.match(globalThis.__modal.html, /user\.manualRenew/, '要有手动停用的提示');
  assert.equal($('rn-manual').hidden, false);
  $('f-mode').value = 'bind';
  $('f-mode').fire('change');
  assert.equal($('rn-manual').hidden, true, '只归入套餐不改启停,不该提示');
  $('f-mode').value = 'extend';
  $('f-mode').fire('change');
  assert.equal($('rn-manual').hidden, false, '延期(保留已用流量)同样会启用');
});

test('续费:超量、到期停用的和启用中的用户不提示', async () => {
  for (const u of [{ enabled: true }, manualOff({ disabledReason: 'quota' }), manualOff({ disabledReason: 'expired' })]) {
    const { A } = await setup();
    Object.assign(globalThis.__state.users[0], u);
    Object.assign($('f-plan'), { value: '1' });
    Object.assign($('f-mode'), { value: 'renew' });
    A['user.renew']('1');
    await flush();
    assert.doesNotMatch(globalThis.__modal.html, /user\.manual/, JSON.stringify(u));
  }
});

test('批量套用套餐:选中的人里有手动停用的,确认框里列出人数和名字', async () => {
  const { A } = await setup();
  globalThis.__state.users = [
    { id: 1, name: 'alice', planId: 1, enabled: true, up: 0, down: 0, volume: 10, expiry: 100 },
    manualOff({ id: 2, name: 'bob' }),
    manualOff({ id: 3, name: 'carol', disabledReason: 'quota' }),
    manualOff({ id: 4, name: 'dave' }),
  ];
  for (const id of ['1', '2', '3', '4']) A['user.sel'](id, { checked: true });
  Object.assign($('f-plan'), { value: '1' });
  Object.assign($('f-mode'), { value: 'renew' });
  await A['user.batch']('plan');
  await flush();
  const html = globalThis.__modal.html;
  assert.match(html, /user\.manualBatch\|n=2\|/, '要列出人数');
  assert.match(html, /bob/);
  assert.match(html, /dave/);
  assert.doesNotMatch(html, /carol/, '超量停用的不算');
  $('f-mode').value = 'bind';
  $('f-mode').fire('change');
  assert.equal($('rn-manual').hidden, true);
});

test('批量套用套餐:选中的人里没有手动停用的,不提示', async () => {
  const { A } = await setup();
  globalThis.__state.users = [
    { id: 1, name: 'alice', planId: 1, enabled: true, up: 0, down: 0, volume: 10, expiry: 100 },
    manualOff({ id: 3, name: 'carol', disabledReason: 'expired' }),
  ];
  for (const id of ['1', '3']) A['user.sel'](id, { checked: true });
  await A['user.batch']('plan');
  await flush();
  assert.doesNotMatch(globalThis.__modal.html, /user\.manual/);
});

test('换套餐:手动停用的用户,对话框里明说执行后会被启用,预览里不再重复一遍', async () => {
  const { be, A } = await setup();
  Object.assign(globalThis.__state.users[0], manualOff());
  A['user.change']('1');
  await flush();
  assert.match(globalThis.__modal.html, /user\.manualChange/, '要有手动停用的提示');
  be.previews[0].resolve(result(false, { warnings: [{ code: 'enables_manual', text: '这个用户是手动停用的,执行后会被启用', args: null, block: false }, { code: 'volume_lower', text: '配额变少', args: { from: 2, to: 1 }, block: false }] }));
  await flush();
  const pv = $('pc-preview').innerHTML;
  assert.doesNotMatch(pv, /enables_manual|执行后会被启用/, '上面已经说了,预览的影响列表里不再重复');
  assert.match(pv, /pc\.w\.volume_lower/, '其它影响照常列出');
});

// 真实的 ui.js:换套餐对话框在等预览时禁用了提交按钮,用户这时关掉对话框,下一个对话框的提交按钮不能还禁用着。
test('openModal 打开时把提交按钮恢复成可用', async () => {
  const uiDir = fs.mkdtempSync(path.join(os.tmpdir(), 'ui-test-'));
  try {
    fs.copyFileSync(path.join(here, '..', 'assets', 'js', 'ui.js'), path.join(uiDir, 'ui.js'));
    fs.writeFileSync(path.join(uiDir, 'i18n.js'), `export const t = (k) => k; export const getLang = () => 'zh';`);
    els = {};
    const doc = globalThis.document;
    globalThis.document = { ...doc, addEventListener() {}, querySelector: () => null };
    globalThis.window = { addEventListener() {}, innerWidth: 1000, innerHeight: 800 };
    const box = { classList: { toggle() {} } };
    Object.assign(document.getElementById('modal'), { hidden: true, querySelector: (sel) => (sel === '.modal-box' ? box : null) });
    const ui = await import(pathToFileURL(path.join(uiDir, 'ui.js')).href);
    $('modal-save').disabled = true; // 上一个对话框留下的
    ui.openModal('x', '', async () => {});
    assert.equal($('modal-save').disabled, false);
    globalThis.document = doc;
    delete globalThis.window;
  } finally {
    fs.rmSync(uiDir, { recursive: true, force: true });
  }
});

// 列表「更多」菜单里的三种订阅各复制各的地址:sing-box 那项以前复制成了 Clash 地址(代理面板用的是同一份代码)。
test('「更多」菜单:Clash / 链接 / sing-box 订阅各复制对应格式的地址', async () => {
  const { be, A, mod } = await setup();
  const links = { link: 'https://h:2056/sub/alice', clash: 'https://h:2056/sub/alice?format=clash', json: 'https://h:2056/sub/alice?format=json' };
  be.onGet = (p) => (p === 'users/1/sub' ? links : []);
  // 菜单里每个按钮的 data-fmt 和它的文字对得上
  await mod.render(document.getElementById('page'));
  const menu = $('users-body').innerHTML;
  for (const [fmt, label] of [['clash', 'user.subClash'], ['link', 'user.subLink'], ['json', 'user.subJson']]) {
    assert.ok(menu.includes(`data-act="user.copy" data-id="1" data-fmt="${fmt}">${label}<`), `${label} 的按钮要带 data-fmt="${fmt}"`);
  }
  globalThis.__copied = [];
  for (const fmt of ['clash', 'link', 'json']) await A['user.copy']('1', { dataset: { fmt } });
  assert.deepEqual(globalThis.__copied, [links.clash, links.link, links.json]);
});

// 外部节点页(web/assets/js/pages/exts.js)的前端测试:用桩模块和桩 DOM 加载真实的 exts.js,后端按面板接口仿真。
// 运行:node --test web/jstest/exts.test.mjs(CI 里和 deploy/donors 的测试一样跑)。
// 文件放在 web/assets 之外:那个目录整个嵌进二进制、对外提供。
import test, { after, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = process.env.EXTS_JS || path.join(here, '..', 'assets', 'js', 'pages', 'exts.js');

// ---- 桩模块:exts.js 只从这几个模块取东西,都转到 globalThis 上由测试控制 ----
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'exts-test-'));
fs.mkdirSync(path.join(dir, 'pages'));
fs.copyFileSync(src, path.join(dir, 'pages', 'exts.js'));
const stub = (name, body) => fs.writeFileSync(path.join(dir, name), body);
stub('app.js', `export const state = globalThis.__state;
export async function load(...what) { if (globalThis.__onLoad) await globalThis.__onLoad(...what); }`);
stub('api.js', `export const SLOW = {};
export const get = (...a) => globalThis.__api('GET', ...a);
export const post = (...a) => globalThis.__api('POST', ...a);
export const put = (...a) => globalThis.__api('PUT', ...a);
export const del = (...a) => globalThis.__api('DELETE', ...a);`);
stub('i18n.js', `export const t = (k) => k;`);
stub('errmsg.js', `export const trErr = (m) => m;`);
stub('ui.js', `export const esc = (s) => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
export const fmtRelative = () => 'x';
export const toast = (m, k = '') => { globalThis.__toasts.push([k, m]); };
export const confirm = async () => true;
export const openModal = (title, html, onSave) => { globalThis.__modal = onSave; };
export const openDrawer = () => {};
export const registerActions = (a) => { globalThis.__actions = a; };
export const badge = (s) => String(s);
export const field = () => '';
export const check = () => '';
export const empty = (s) => String(s);
export const fv = (id) => (globalThis.__fv || {})[id] ?? '';
export const fchk = () => true;
export const copy = async () => {};
export const matches = () => true;
export const hostPort = (h, p) => h + ':' + p;`);

// ---- 桩 DOM:按 id 取元素(没有就建),记下 innerHTML ----
globalThis.__state = {};
let els;
globalThis.document = {
  getElementById: (id) => (els[id] ||= { id, innerHTML: '', textContent: '', hidden: false, dataset: {}, addEventListener() {}, querySelector: () => null }),
  querySelectorAll: () => [],
  querySelector: () => null,
};

// ---- 仿真后端:隐藏按指纹记,合并保存带版本号(和 web/ext_nodes.go 一样的语义)----
const realSetTimeout = setTimeout;
const realClearTimeout = clearTimeout;
const flush = async (n = 6) => { for (let i = 0; i < n; i++) await new Promise((r) => realSetTimeout(r, 0)); };

function backend(names) {
  const be = {
    names, exclude: '', hidden: new Set(), renames: {}, rev: 0, lastFetch: 1000, sig: 's1',
    calls: [], puts: [], conflictOnce: false, reachPosts: 0, testPolls: 0, jobDone: false,
  };
  be.key = (name) => 'k' + Buffer.from(name).toString('hex').slice(0, 14);
  be.nodes = () => be.names.map((name, index) => {
    const kw = be.exclude && name.includes(be.exclude);
    const key = be.key(name);
    return { index, key, name, type: 'ss', server: '1.2.3.' + index, port: 443, upstream: true, hidden: kw || be.hidden.has(key), keyword: !!kw, rename: be.renames[key] };
  });
  be.payload = () => ({ nodes: be.nodes(), servers: [{ id: 1, name: '主机', isLocal: true }], fetchedAt: be.lastFetch, sig: be.sig, rev: be.rev, stale: 0 });
  be.row = () => ({ id: 1, name: '机场A', type: 'sub', value: 'https://x', prefix: '', remark: '', exclude: be.exclude, sort: 0, lastFetch: be.lastFetch, lastTry: be.lastFetch,
    contentSig: be.sig, nodeCount: be.nodes().filter((n) => !n.hidden).length, nodeTotal: be.names.length, enabled: true, userCount: 1 });
  globalThis.__api = async (method, p, body) => {
    be.calls.push(method + ' ' + p);
    if (method === 'GET' && p === 'exts/1/nodes') return be.payload();
    if (method === 'PUT' && p === 'exts/1/nodes/prefs') {
      be.puts.push(JSON.parse(JSON.stringify(body)));
      if (body.rev === undefined || body.rev !== be.rev || be.conflictOnce) {
        be.conflictOnce = false;
        be.rev++;
        const e = new Error('conflict'); e.status = 409; throw e;
      }
      for (const x of body.hide || []) be.hidden.add(x.key);
      for (const x of body.show || []) be.hidden.delete(x.key);
      for (const x of body.rename || []) { if (x.to) be.renames[x.key] = x.to; else delete be.renames[x.key]; }
      be.rev++;
      return be.payload();
    }
    if (method === 'PUT' && p === 'exts/1') { be.exclude = body.exclude; return { ok: '1' }; }
    if (method === 'POST' && p === 'exts/1/nodes/test') return { job: 'j1', keys: be.nodes().filter((n) => !n.hidden).map((n) => n.key) };
    if (method === 'GET' && p === 'exts/1/nodes/test/j1') {
      be.testPolls++;
      const k = be.key(be.names[1]);
      return { done: be.jobDone, results: { [k]: { 1: be.jobDone ? { state: 'ok', delayMs: 88 } : { state: 'pending' } } } };
    }
    if (method === 'POST' && p === 'exts/1/nodes/reach') { be.reachPosts++; await new Promise((r) => realSetTimeout(r, 30)); return { job: 'r1', keys: body.keys }; }
    if (method === 'GET' && p === 'exts/1/nodes/reach/r1') return { done: true, results: {} };
    throw new Error('404 ' + method + ' ' + p);
  };
  globalThis.__onLoad = async () => { globalThis.__state.exts = [be.row()]; };
  globalThis.__state.exts = [be.row()];
  return be;
}

// 每个用例一份新的模块实例(展开状态、缓存都是模块级的),计时器换成手动推进的
let seq = 0;
async function setup(names) {
  els = {};
  globalThis.__toasts = [];
  globalThis.__fv = {};
  const be = backend(names);
  const timers = [];
  globalThis.setTimeout = (fn, ms) => { timers.push({ fn, ms }); return timers.length; };
  globalThis.clearTimeout = (id) => { if (timers[id - 1]) timers[id - 1].fn = null; };
  const mod = await import(pathToFileURL(path.join(dir, 'pages', 'exts.js')).href + '?case=' + (++seq));
  const A = globalThis.__actions;
  const runTimers = async () => {
    const due = timers.splice(0);
    for (const t of due) if (t.fn) await t.fn();
    await flush();
  };
  A['ext.toggle']('1'); // 展开
  await flush();
  return { be, mod, A, runTimers };
}
afterEach(() => { globalThis.setTimeout = realSetTimeout; globalThis.clearTimeout = realClearTimeout; });
after(() => fs.rmSync(dir, { recursive: true, force: true }));

const toggle = (be, name, checked) => ({ dataset: { key: be.key(name), idx: String(be.names.indexOf(name)) }, checked });
const saved = () => globalThis.__toasts.some(([k, m]) => k === 'ok' && String(m).startsWith('ext.prefsSaved'));

test('展开后编辑只改排除关键词,再点开关:照样发出保存并提示已保存(以前静默不保存)', async () => {
  const { be, A } = await setup(['剩余流量:120.53 GB', '香港 01', '日本 01']);
  globalThis.__fv = { 'f-name': '机场A', 'f-type': 'sub', 'f-value': 'https://x', 'f-prefix': '', 'f-exclude': '到期', 'f-remark': '', 'f-sort': '0' };
  A['ext.edit']('1');
  await globalThis.__modal();
  await flush();
  be.puts.length = 0;
  globalThis.__toasts = [];
  A['ext.toggleNode']('1', toggle(be, '剩余流量:120.53 GB', false));
  await flush();
  assert.equal(be.puts.length, 1, '应发出一次保存');
  assert.deepEqual(be.puts[0].hide.map((x) => x.key), [be.key('剩余流量:120.53 GB')]);
  assert.ok(be.hidden.has(be.key('剩余流量:120.53 GB')), '后端应记下');
  assert.ok(saved(), '应提示已保存: ' + JSON.stringify(globalThis.__toasts));
});

test('保存只发这次的增量:列表里暂时没有的隐藏项不会被抹掉', async () => {
  const { be, A, mod } = await setup(['剩余流量:120.53 GB', '香港 01', '日本 01']);
  A['ext.toggleNode']('1', toggle(be, '日本 01', false));
  await flush();
  // 自动刷新:日本 01 这次没有,信息节点改了名(指纹不变由后端负责,这里只看前端发什么)
  be.names = ['剩余流量:120.53 GB', '香港 01'];
  be.lastFetch = 2800; be.sig = 's2';
  await mod.tick(); await flush();
  be.puts.length = 0;
  A['ext.toggleNode']('1', toggle(be, '香港 01', false));
  await flush();
  assert.equal(be.puts.length, 1);
  const body = be.puts[0];
  assert.equal(body.hidden, undefined, '不能再整份提交隐藏名单');
  assert.deepEqual(body.hide, [{ name: '香港 01', key: be.key('香港 01') }]);
  assert.ok(be.hidden.has(be.key('日本 01')), '列表外的隐藏项应还在');
});

test('版本号冲突(别的标签页刚改过):重新拉取后再试一次', async () => {
  const { be, A } = await setup(['a', 'b']);
  be.conflictOnce = true;
  A['ext.toggleNode']('1', toggle(be, 'b', false));
  await flush();
  assert.equal(be.puts.length, 2, '409 后应重试一次');
  assert.ok(be.hidden.has(be.key('b')));
  assert.ok(saved());
});

test('节点已经不在列表里:明说列表已更新,不发保存', async () => {
  const { be, A } = await setup(['a', 'b']);
  const stale = toggle(be, 'b', false);
  be.names = ['a'];
  be.lastFetch = 2000; be.sig = 's2';
  A['ext.edit']('1'); // 编辑后缓存作废
  globalThis.__fv = { 'f-name': '机场A', 'f-type': 'sub', 'f-value': 'https://x', 'f-exclude': '', 'f-sort': '0' };
  await globalThis.__modal();
  await flush();
  be.puts.length = 0; globalThis.__toasts = [];
  A['ext.toggleNode']('1', stale);
  await flush();
  assert.equal(be.puts.length, 0);
  assert.ok(globalThis.__toasts.some(([k, m]) => k === 'err' && m === 'ext.listChanged'), JSON.stringify(globalThis.__toasts));
});

test('刷新了但内容没变(指纹相同):测速轮询不停,结果不丢', async () => {
  const { be, A, mod, runTimers } = await setup(['a', 'b']);
  A['ext.testAll']('1');
  await flush();
  await runTimers();
  const polls = be.testPolls;
  assert.ok(polls >= 1, '应开始轮询');
  be.lastFetch = 5000; // 定时刷新:抓取时间变了,内容指纹没变
  await mod.tick(); await flush();
  await runTimers();
  assert.ok(be.testPolls > polls, '轮询不该因为刷新而停');
  be.jobDone = true;
  await runTimers();
  assert.ok(globalThis.__toasts.some(([, m]) => String(m).startsWith('ext.testDone')), JSON.stringify(globalThis.__toasts));
  assert.match(document.getElementById('ext-nodes-1').innerHTML, /88 ms/, '结果应按指纹贴到节点上');
});

test('大陆连通在途时重复点击不再发请求(每次都扣测点额度)', async () => {
  const { be, A } = await setup(['a', 'b']);
  A['ext.pick']('1', { dataset: { key: be.key('a') }, checked: true });
  A['ext.reachSel']('1');
  A['ext.reachSel']('1');
  await flush(10);
  assert.equal(be.reachPosts, 1);
  assert.ok(globalThis.__toasts.some(([k, m]) => k === 'err' && m === 'ext.reachRunning'), JSON.stringify(globalThis.__toasts));
});

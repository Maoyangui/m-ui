// 面板点了没反应的两个根因,各钉一组测试:
// 1) 换页时上一页还在路上的读取要能放弃(api.js 的 abortPageLoads):页面渲染是排队的,不放弃的话新页面要等上一页的慢接口回来;
// 2) setHTML 的「内容没变就不重画」不能只比字符串:别处直接换过 innerHTML(路由换成「加载中」)之后必须重画,
//    否则回到内容没变的页面(备份页)就一直停在「加载中」。
// 运行:node --test web/jstest/api.test.mjs
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const assets = path.join(here, '..', 'assets', 'js');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-test-'));
for (const f of ['api.js', 'ui.js']) fs.copyFileSync(path.join(assets, f), path.join(dir, f));
fs.writeFileSync(path.join(dir, 'i18n.js'), 'export const t = (k, v) => (v ? k + JSON.stringify(v) : k);');
fs.writeFileSync(path.join(dir, 'errmsg.js'), 'export const trErr = s => s;');
globalThis.document = { addEventListener() {}, dispatchEvent() {}, getElementById: () => null };
globalThis.window = { addEventListener() {} };
after(() => fs.rmSync(dir, { recursive: true, force: true }));

// 假的 fetch:每个请求挂着,等测试让它回来;认 signal(被放弃时按浏览器的规矩抛 AbortError)
const calls = [];
globalThis.fetch = (url, init = {}) => new Promise((resolve, reject) => {
  const call = { url, init, done: false };
  const abort = () => { call.done = true; const e = new Error('aborted'); e.name = 'AbortError'; reject(e); };
  if (init.signal) { if (init.signal.aborted) return abort(); init.signal.addEventListener('abort', abort, { once: true }); }
  // body:响应体怎么读(默认立刻给);slowBody:读响应体时也挂着,被放弃就抛
  call.reply = (status, data, { slowBody = false } = {}) => {
    call.done = true;
    resolve({ status, ok: status >= 200 && status < 300, json: () => (slowBody
      ? new Promise((res, rej) => { call.body = () => res(data); if (init.signal) init.signal.addEventListener('abort', () => { const e = new Error('aborted'); e.name = 'AbortError'; rej(e); }, { once: true }); })
      : Promise.resolve(data)) });
  };
  calls.push(call);
});
const api = await import(pathToFileURL(path.join(dir, 'api.js')).href);
const ui = await import(pathToFileURL(path.join(dir, 'ui.js')).href);
const tick = () => new Promise(r => setTimeout(r, 0));
const settle = p => p.then(v => ({ ok: v }), e => ({ err: e }));

test('换页:还在路上的读取被放弃(aborted、message 为空);改东西的请求、keep 的读取照常', async () => {
  calls.length = 0;
  const read = settle(api.get('ops/status'));
  const write = settle(api.post('users', { name: 'a' }));
  const kept = settle(api.get('update', { keep: true }));
  const del = settle(api.del('users/1'));
  await tick();
  assert.equal(calls.length, 4);
  api.abortPageLoads();
  const r = await read;
  assert.ok(r.err instanceof api.ApiError, '读取应当被放弃');
  assert.equal(r.err.aborted, true);
  assert.equal(r.err.message, '', '消息是空的:拿去 toast 也不显示');
  assert.equal(calls[0].done, true);
  assert.equal(calls[1].done, false, 'POST 不放弃');
  assert.equal(calls[2].done, false, 'keep 的读取不放弃');
  assert.equal(calls[3].done, false, 'DELETE 不放弃');
  calls[1].reply(200, { id: 7 }); calls[2].reply(200, { v: 1 }); calls[3].reply(200, { ok: true });
  assert.deepEqual((await write).ok, { id: 7 });
  assert.deepEqual((await kept).ok, { v: 1 });
  assert.deepEqual((await del).ok, { ok: true });
});

test('换页之后新发的读取不受影响;再换一次页只放弃那之后的', async () => {
  calls.length = 0;
  api.abortPageLoads();
  const a = settle(api.get('lines'));
  await tick();
  calls[0].reply(200, [{ id: 1 }]);
  assert.deepEqual((await a).ok, [{ id: 1 }]);
  const b = settle(api.get('users'));
  await tick();
  api.abortPageLoads();
  assert.equal((await b).err.aborted, true);
  const c = settle(api.get('users'));
  await tick();
  calls[2].reply(200, []);
  assert.deepEqual((await c).ok, []);
});

test('读响应体读到一半换页:也算放弃,不把读了一半的当成空对象交出去', async () => {
  calls.length = 0;
  const p = settle(api.get('nodes'));
  await tick();
  calls[0].reply(200, { nodes: [1, 2] }, { slowBody: true });
  await tick();
  api.abortPageLoads();
  const r = await p;
  assert.ok(r.err && r.err.aborted === true, '应当抛被放弃的错,实际 ' + JSON.stringify(r));
});

test('超时、报错、401 的行为不变', async () => {
  calls.length = 0;
  const slow = settle(api.get('x', { timeout: 20 }));
  const r = await slow;
  assert.ok(r.err instanceof api.ApiError);
  assert.equal(r.err.message, 'api.timeout');
  assert.notEqual(r.err.aborted, true, '超时不是换页放弃');
  let unauth = 0;
  api.setUnauthorizedHandler(() => { unauth++; });
  const e1 = settle(api.get('status')), e2 = settle(api.get('lines'));
  await tick();
  calls[1].reply(401, { error: '未登录' }); calls[2].reply(500, { error: '坏了' });
  assert.equal((await e1).err.status, 401);
  assert.equal(unauth, 1);
  const bad = (await e2).err;
  assert.equal(bad.status, 500);
  assert.equal(bad.message, '坏了');
});

// ---- setHTML ----
// 假元素:赋 innerHTML 就换一个新的 firstChild(和浏览器一样:旧节点整批换掉)
const fakeEl = () => {
  let html = '', first = null;
  return { get innerHTML() { return html; }, set innerHTML(v) { html = v; first = v ? { of: v } : null; }, get firstChild() { return first; }, querySelectorAll: () => [] };
};

test('setHTML:内容没变不重画;别处直接换过 innerHTML 之后,同样的内容也要重画', () => {
  const el = fakeEl();
  assert.equal(ui.setHTML(el, '<p>备份</p>'), true);
  const node = el.firstChild;
  assert.equal(ui.setHTML(el, '<p>备份</p>'), false, '内容没变:不重画');
  assert.equal(el.firstChild, node, '节点没被换掉(开着的菜单、输入框里的字都还在)');
  // 换页:路由把页面区直接换成「加载中」,没经过 setHTML
  el.innerHTML = '<div class="empty">加载中…</div>';
  assert.equal(ui.setHTML(el, '<p>备份</p>'), true, '回到同一页、内容没变:必须重画,不能停在「加载中」');
  assert.equal(el.innerHTML, '<p>备份</p>');
  assert.equal(ui.setHTML(el, '<p>备份</p>'), false);
  assert.equal(ui.setHTML(el, '<p>改了</p>'), true);
  // 被别处清空的也认得出来
  el.innerHTML = '';
  assert.equal(ui.setHTML(el, '<p>改了</p>'), true);
  // 空内容:画过一次空的,再画空的不重画
  const e2 = fakeEl();
  assert.equal(ui.setHTML(e2, ''), true);
  assert.equal(ui.setHTML(e2, ''), false);
  assert.equal(ui.setHTML(null, 'x'), false);
});

test('toast:空话不弹(换页时被放弃的读取抛的错消息是空的)', () => {
  let touched = 0;
  globalThis.document.getElementById = () => { touched++; return { className: '', textContent: '', hidden: true }; };
  ui.toast('', 'err');
  ui.toast(undefined);
  assert.equal(touched, 0, '空消息不该去碰提示条');
  ui.toast('存好了', 'ok');
  assert.equal(touched, 1);
  globalThis.document.getElementById = () => null;
});

// 页面顶部证书提示条(web/assets/js/app.js 的 certNotice)「知道了」的前端测试:桩模块 + 桩 DOM 加载真实的 app.js。
// 运行:node --test web/jstest/certbar.test.mjs
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const jsDir = path.join(here, '..', 'assets', 'js');
const src = fs.readFileSync(process.env.APP_JS || path.join(jsDir, 'app.js'), 'utf8');

// ---- 桩模块:按 app.js 的 import 逐个生成。用得上的几个名字给实现,其余给一个什么都接得住的桩;
// 纯函数模块(不 import 别的、不碰 DOM / 存储 / 网络)直接用真的:提示条的签名要是抽到了这种模块里,测的仍是真逻辑 ----
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'certbar-test-'));
after(() => fs.rmSync(dir, { recursive: true, force: true }));
fs.writeFileSync(path.join(dir, 'app.js'), src);
const IMPL = {
  get: `(p) => globalThis.__api('GET', p)`,
  post: `(p, b) => globalThis.__api('POST', p, b)`,
  // 带参数的文案把参数拼在后面(k|名=值|…),用例能看出用的是哪条、传了什么
  t: `(k, p) => (p && Object.keys(p).length ? k + '|' + Object.entries(p).map(([a, b]) => a + '=' + b).join('|') : k)`,
  esc: `(s) => String(s ?? '')`,
  trErr: `(s) => s`,
  getLang: `() => 'zh'`,
  langs: `({ zh: '中文' })`,
  ICONS: `({})`,
  LONG: `1`,
  fmtDay: `(n) => 'D' + n`,
};
const ANY = `const __any = new Proxy(function () {}, { get: (_, k) => (k === 'then' ? undefined : k === Symbol.toPrimitive ? () => '' : __any), apply: () => __any });`;
const PAGE = `export const title = () => '', subtitle = () => '', render = async () => {}, forceSetPassword = () => {};`;
const pure = (code) => !/^\s*import\s/m.test(code) && !/\b(document|window|localStorage|fetch|location|navigator)\b/.test(code);
for (const [, what, spec] of src.matchAll(/^import\s+(\{[^}]*\}|\*\s+as\s+\w+)\s+from\s+'([^']+)';/gm)) {
  const file = path.join(dir, spec);
  if (fs.existsSync(file)) continue;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const real = path.join(jsDir, spec);
  if (fs.existsSync(real) && pure(fs.readFileSync(real, 'utf8'))) {
    fs.copyFileSync(real, file);
  } else if (what.startsWith('*')) {
    fs.writeFileSync(file, PAGE);
  } else {
    const names = what.slice(1, -1).split(',').map((s) => s.trim().split(/\s+as\s+/)[0]).filter(Boolean);
    fs.writeFileSync(file, ANY + '\n' + names.map((n) => `export const ${n} = ${IMPL[n] || '__any'};`).join('\n') + '\n');
  }
}

// ---- 桩 DOM:index.html 里的元素按 id 先建好;运行中建的元素插进页面(insertBefore)后才按 id 找得到,remove() 后找不到 ----
const byId = new Map();
function mkEl(tag = 'div') {
  return {
    tagName: tag.toUpperCase(), id: '', hidden: false, disabled: false, value: '', textContent: '', title: '', className: '', href: '',
    dataset: {}, style: {}, on: {}, kids: new Map(), html: '',
    classList: { toggle() {}, add() {}, remove() {}, contains: () => false },
    get innerHTML() { return this.html; },
    set innerHTML(v) { this.html = String(v); this.kids.clear(); },
    addEventListener(type, fn) { (this.on[type] ||= []).push(fn); },
    removeEventListener() {},
    dispatchEvent() { return true; },
    fire(type) { for (const fn of this.on[type] || []) fn({ target: this, preventDefault() {} }); },
    insertBefore(child) { if (child.id) byId.set(child.id, child); return child; },
    appendChild(child) { return this.insertBefore(child); },
    insertAdjacentHTML() {},
    remove() { if (byId.get(this.id) === this) byId.delete(this.id); },
    // bar.querySelector('#cert-dismiss'):innerHTML 里真有这个 id 才给,同一次渲染给同一个元素
    querySelector(sel) {
      const m = /^#([\w-]+)$/.exec(sel);
      if (!m || !this.html.includes(`id="${m[1]}"`)) return null;
      if (!this.kids.has(sel)) this.kids.set(sel, mkEl());
      return this.kids.get(sel);
    },
    querySelectorAll: () => [],
    contains: () => false,
    closest: () => null,
    focus() {}, select() {}, click() {},
  };
}
for (const [, id] of fs.readFileSync(path.join(here, '..', 'assets', 'index.html'), 'utf8').matchAll(/\bid="([^"]+)"/g)) {
  const e = mkEl();
  e.id = id;
  byId.set(id, e);
}
const mainEl = mkEl(); // <div class="main">
globalThis.document = {
  getElementById: (id) => byId.get(id) || null,
  querySelector: (sel) => (sel === '.main' ? mainEl : null),
  querySelectorAll: () => [],
  createElement: (tag) => mkEl(tag),
  addEventListener() {},
  documentElement: { dataset: {}, lang: '' },
  body: mkEl('body'),
  hidden: false,
  activeElement: null,
};
const storage = new Map();
const ls = { getItem: (k) => (storage.has(k) ? storage.get(k) : null), setItem: (k, v) => storage.set(k, String(v)), removeItem: (k) => storage.delete(k) };
try { Object.defineProperty(globalThis, 'localStorage', { value: ls, configurable: true, writable: true }); } catch { globalThis.localStorage = ls; }
globalThis.window = { addEventListener() {} };
globalThis.location = { hash: '#/cert', reload() {} }; // 停在证书页:概况页可能有自己的「需要处理」卡,不叠提示条
globalThis.history = { replaceState() {} };
globalThis.MutationObserver = class { observe() {} disconnect() {} };
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);
// 定时刷新的循环不能把测试进程一直拖着:页面里的定时器一律 unref
const realSetTimeout = globalThis.setTimeout;
globalThis.setTimeout = (fn, ms, ...a) => { const h = realSetTimeout(fn, ms, ...a); h.unref?.(); return h; };
const flush = async (n = 8) => { for (let i = 0; i < n; i++) await new Promise((r) => realSetTimeout(r, 0)); };

// ---- 仿真后端:status 里的 certAlerts 由用例决定 ----
let status = { version: 't', role: 'master', certAlerts: [] };
globalThis.__api = async (method, p) => {
  if (method === 'GET' && p === 'status') return JSON.parse(JSON.stringify(status));
  if (method === 'GET' && p === 'settings') return { timezone: 'Asia/Shanghai' };
  return {};
};

const app = await import(pathToFileURL(path.join(dir, 'app.js')).href);
await flush();

const bar = () => document.getElementById('cert-bar');
const dismiss = () => bar().querySelector('#cert-dismiss').fire('click');
const show = async (certAlerts) => { status = { version: 't', role: 'master', certAlerts }; await app.load('status'); await flush(2); };

test('关掉「文件读不出来」之后,面板 / 订阅改用临时自签证书(比如重启之后)要重新提示', async () => {
  storage.clear();
  await show([{ name: 'a.example.com', trouble: 'missing' }]); // 运行中文件被删:提示读不出来
  assert.ok(bar(), '应有提示条');
  dismiss();
  assert.equal(bar(), null, '点了「知道了」就收起');
  await show([{ name: 'a.example.com', trouble: 'missing' }]);
  assert.equal(bar(), null, '同样的情况关掉了就不再出');

  await show([{ name: 'a.example.com', trouble: 'missing', fallback: 'panel,sub', error: 'open: no such file or directory' }]);
  assert.ok(bar(), '面板、订阅改用临时证书了,是新情况,要再提示');
  assert.match(bar().dataset.text, /alert\.certFallback\|svc=alert\.svc\.panel/);
  dismiss();
  await show([{ name: 'a.example.com', trouble: 'missing', fallback: 'panel,sub' }]);
  assert.equal(bar(), null, '同一组服务顶着,关掉了就不再出');
  await show([{ name: 'a.example.com', trouble: 'missing', fallback: 'panel,sub,reseller' }]);
  assert.ok(bar(), '代理面板也顶上了临时证书,要再提示');
});

test('没有 fallback 的提示签名和以前一样:升级前已经关掉的不会又冒出来', async () => {
  storage.clear();
  storage.set('m-ui.certDismissed', '0:b.example.com:expiring,2:c.example.com:failing');
  const left5 = Math.floor(Date.now() / 1000) + 5 * 86400;
  await show([{ name: 'b.example.com', trouble: 'expiring', notAfter: left5 }, { nodeId: 2, node: 'sg', name: 'c.example.com', trouble: 'failing' }]);
  assert.equal(bar(), null);
  await show([{ name: 'b.example.com', trouble: 'expiring', notAfter: left5 }, { nodeId: 2, node: 'sg', name: 'c.example.com', trouble: 'missing' }]);
  assert.ok(bar(), '情况变了照样再提示');
});

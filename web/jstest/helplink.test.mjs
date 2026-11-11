// 面板右上角与登录页的「帮助说明」链接(web/assets/js/app.js helpLink)的前端测试:加载真实的 app.js 与 api.js,
// app.js 引用的其它模块按它的 import 语句自动生成同名导出的桩;桩 DOM、假的 fetch,不联网、不监听端口。
// 重点是代理面板的登录页:那时还没有 status,只能靠代理面板接口在未登录时回的 401 { scope: 'reseller' } 认出来,
// 帮助页要打开代理视图(?view=reseller),而不是管理员视图里的「用 admin / admin 登录」。
// 运行:node --test web/jstest/helplink.test.mjs
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const js = process.env.PANEL_JS || path.join(here, '..', 'assets', 'js');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'helplink-test-'));
after(() => fs.rmSync(dir, { recursive: true, force: true }));

// ---- 真实的 app.js、api.js;其它模块是桩 ----
const appSrc = fs.readFileSync(path.join(js, 'app.js'), 'utf8');
fs.writeFileSync(path.join(dir, 'app.js'), appSrc);
fs.copyFileSync(path.join(js, 'api.js'), path.join(dir, 'api.js'));
const write = (rel, body) => { fs.mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true }); fs.writeFileSync(path.join(dir, rel), body); };
// 有固定行为的桩:语言可切换,t 原样返回键
const fixed = {
  './i18n.js': `let lang = 'zh';
export const t = (k) => k; export const getLang = () => lang; export const setLang = (l) => { lang = l; };
export const langs = { zh: '中文', en: 'English' };`,
  './errmsg.js': 'export const trErr = (m) => m;',
};
for (const [rel, body] of Object.entries(fixed)) write(rel, body);
// 其余的按 import 语句生成:import * as x → 页面模块的常用导出;import { a, b } → 同名的空函数(原样返回第一个参数)
const pageStub = `export const title = () => ''; export const subtitle = () => ''; export async function render() {} export async function tick() {} export function forceSetPassword() {}`;
for (const m of appSrc.matchAll(/^import\s+(?:\*\s+as\s+\w+|\{([^}]*)\})\s+from\s+'(\.[^']+)';/gm)) {
  const [, names, rel] = m;
  if (rel === './api.js' || fixed[rel]) continue;
  write(rel, names === undefined ? pageStub
    : names.split(',').map((s) => s.trim().split(/\s+as\s+/)[0]).filter(Boolean).map((n) => `export const ${n} = (...a) => a[0];`).join('\n'));
}

// ---- 桩 DOM:按 id 取元素(没有就建) ----
let els = {};
const mkEl = (id) => ({
  id, hidden: false, disabled: false, value: '', textContent: '', innerHTML: '', href: '', title: '', className: '', dataset: {}, style: {},
  classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
  addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, focus() {}, blur() {}, select() {},
  setAttribute() {}, getAttribute: () => null, removeAttribute() {}, appendChild() {}, remove() {},
  querySelector: () => null, querySelectorAll: () => [], closest: () => null, contains: () => false,
});
const docEl = mkEl('html');
globalThis.document = {
  getElementById: (id) => (els[id] ||= mkEl(id)), querySelector: () => null, querySelectorAll: () => [],
  createElement: (tag) => mkEl(tag), addEventListener() {}, removeEventListener() {}, documentElement: docEl, body: mkEl('body'), hidden: false,
};
const win = { addEventListener() {}, removeEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }), innerWidth: 1280, innerHeight: 800 };
const define = (k, v) => Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true });
define('window', win);
define('matchMedia', win.matchMedia);
define('localStorage', { getItem: () => null, setItem() {}, removeItem() {} });
define('location', { hash: '', pathname: '/dl/', search: '', origin: 'http://x', reload() {} });
define('history', { replaceState() {}, pushState() {} });
define('requestAnimationFrame', (fn) => setTimeout(fn, 0));
define('Event', class { constructor(type) { this.type = type; } });
define('MutationObserver', class { observe() {} disconnect() {} });
// 定时刷新(几秒一轮)和请求超时都不跑,不然测试进程退不出去;界面上几十毫秒的小延时照常
const realSetTimeout = globalThis.setTimeout;
globalThis.setTimeout = (fn, ms = 0, ...a) => (ms >= 1000 ? 0 : realSetTimeout(fn, ms, ...a));
after(() => { globalThis.setTimeout = realSetTimeout; });

// ---- 假后端:未登录时 status 回 401(代理面板带 scope) ----
let unauthorized = {};
globalThis.fetch = async (url) => {
  const status = /api\/status$/.test(url) ? 401 : 404;
  const body = status === 401 ? unauthorized : { error: 'not found' };
  return { status, ok: false, json: async () => body };
};
const flush = async (n = 10) => { for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r)); };

let seq = 0;
async function openPanel(lang, body) {
  els = {};
  unauthorized = body;
  // 先定语言(i18n 的桩全程只有一份),再加载一份新的 app.js:模块顶层就会去取 status,401 后停在登录页
  (await import(pathToFileURL(path.join(dir, 'i18n.js')).href)).setLang(lang);
  await import(pathToFileURL(path.join(dir, 'app.js')).href + '?case=' + (++seq));
  await flush();
  return els['login-help'];
}

test('代理面板的登录页(还没登录):帮助说明打开代理视图,定位到「第一次登录与设密码」', async () => {
  const a = await openPanel('zh', { error: '未登录', scope: 'reseller' });
  assert.equal(els.login.hidden, false, '应停在登录页');
  assert.equal(a.href, 'help/zh.html?view=reseller#rs-login');
});

test('主面板的登录页:管理员视图,定位到「第一次登录」', async () => {
  const a = await openPanel('zh', { error: '未登录' });
  assert.equal(a.href, 'help/zh.html#first-login');
});

test('英文界面的代理面板登录页打开英文帮助的代理视图', async () => {
  const a = await openPanel('en', { error: '未登录', scope: 'reseller' });
  assert.equal(a.href, 'help/en.html?view=reseller#rs-login');
});

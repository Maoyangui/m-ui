// 下拉菜单(<details class="menu">)与事件委托、弹窗、抽屉的配合:桩 DOM 加载真实的 ui.js。
// setHTML 会把重画前开着的菜单重新打开,所以点过菜单项、或者打开了弹窗 / 抽屉之后,菜单必须已经收起,
// 否则它(z-index 高于弹窗)会一直挂着、压在弹窗上。
// 运行:node --test web/jstest/menu.test.mjs
import test, { after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'menu-test-'));
fs.copyFileSync(path.join(here, '..', 'assets', 'js', 'ui.js'), path.join(dir, 'ui.js'));
fs.writeFileSync(path.join(dir, 'i18n.js'), 'export const t = k => k;');
after(() => fs.rmSync(dir, { recursive: true, force: true }));

// ---- 桩 DOM ----
const listeners = {};
let menus = [], els = {};
const classes = () => { const s = new Set(); return { add: c => s.add(c), remove: c => s.delete(c), contains: c => s.has(c), toggle: (c, on) => (on ? s.add(c) : s.delete(c)) }; };
const mkEl = id => ({ id, hidden: true, innerHTML: '', textContent: '', className: '', scrollTop: 0, disabled: false, offsetWidth: 0, dataset: {}, classList: classes(),
  querySelector: sel => (sel === '.modal-box' ? (els['modal-box'] ||= mkEl('modal-box')) : null), querySelectorAll: () => [], addEventListener() {}, removeEventListener() {} });
function mkMenu(keep = false) {
  const attrs = new Set(['open']);
  if (keep) attrs.add('data-keep-open');
  const m = { hasAttribute: a => attrs.has(a), removeAttribute: a => attrs.delete(a), get open() { return attrs.has('open'); } };
  menus.push(m);
  return m;
}
// 菜单里的一项(或菜单外的按钮,menu 为 null)
const mkItem = (act, menu = null, id = '') => ({ dataset: { act, id }, closest: sel => (sel === 'details.menu' ? menu : null) });
const click = item => {
  const e = { target: { closest: sel => (sel === '[data-act]' ? item : null) }, preventDefault() {} };
  for (const f of listeners.click || []) f(e);
};
globalThis.document = {
  addEventListener: (type, f) => (listeners[type] ||= []).push(f),
  querySelectorAll: sel => (sel === 'details.menu[open]' ? menus.filter(m => m.open) : []),
  getElementById: id => (els[id] ||= mkEl(id)),
  dispatchEvent() {},
};
globalThis.window = { addEventListener() {}, innerWidth: 1280, innerHeight: 800 };

const ui = await import(pathToFileURL(path.join(dir, 'ui.js')).href);
const calls = [];
ui.registerActions({ 'x.go': id => calls.push(['go', id]), 'x.modal': () => ui.openModal('t', '<p>x</p>', null), 'x.colsAll': id => calls.push(['all', id]) });
beforeEach(() => { menus = []; els = {}; calls.length = 0; });

test('点菜单里的项:先收起所在的菜单再执行(之后重画不会把它重新打开)', () => {
  const m = mkMenu(), other = mkMenu();
  click(mkItem('x.go', m, '7'));
  assert.deepEqual(calls, [['go', '7']]);
  assert.equal(m.open, false);
  assert.equal(other.open, true, '别的菜单由「点菜单外关闭」处理,这里不动');
});

test('标了 data-keep-open 的菜单(外部节点「服务器列」的全选 / 清空)点了不收起', () => {
  const m = mkMenu(true);
  click(mkItem('x.colsAll', m, '1'));
  assert.deepEqual(calls, [['all', '1']]);
  assert.equal(m.open, true);
});

test('菜单外的按钮照常执行,不报错', () => {
  click(mkItem('x.go', null, '2'));
  assert.deepEqual(calls, [['go', '2']]);
});

test('菜单项打开弹窗:菜单收起、弹窗打开', () => {
  const m = mkMenu();
  click(mkItem('x.modal', m));
  assert.equal(m.open, false);
  assert.equal(els.modal.hidden, false);
});

test('openModal / openDrawer 收起所有开着的菜单(包括 keep-open 的):菜单层级比弹窗高,开着就压在上面', () => {
  const a = mkMenu(), b = mkMenu(true);
  ui.openModal('t', '<p>x</p>', null);
  assert.equal(a.open, false);
  assert.equal(b.open, false);
  const c = mkMenu();
  ui.openDrawer('t', '<p>x</p>');
  assert.equal(c.open, false);
  assert.equal(els.drawer.hidden, false);
});

test('closeMenus 收起全部', () => {
  const a = mkMenu(), b = mkMenu();
  ui.closeMenus();
  assert.deepEqual([a.open, b.open], [false, false]);
});

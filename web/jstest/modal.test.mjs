// 弹窗(web/assets/js/ui.js 的 openModal / confirm)的保存流程:桩 DOM 加载真实的 ui.js。
// 页面上只有一个弹窗。保存回调里换成了别的弹窗(批量生成完弹出「生成结果」框、确认框之后接着开对话框),
// 回调结束后不能再按旧弹窗收尾:不关掉新弹窗、不把错误写进新弹窗、不动新弹窗的按钮。
// 运行:node --test web/jstest/modal.test.mjs
import test, { after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'modal-test-'));
fs.copyFileSync(path.join(here, '..', 'assets', 'js', 'ui.js'), path.join(dir, 'ui.js'));
fs.writeFileSync(path.join(dir, 'i18n.js'), 'export const t = k => k;');
after(() => fs.rmSync(dir, { recursive: true, force: true }));

// ---- 桩 DOM ----
let els = {};
const mkEl = id => {
  const on = {};
  return {
    id, hidden: true, innerHTML: '', textContent: '', className: '', scrollTop: 0, disabled: false, onclick: null, dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    querySelector: sel => (sel === '.modal-box' ? (els['modal-box'] ||= mkEl('modal-box')) : null), querySelectorAll: () => [],
    addEventListener: (type, f) => (on[type] ||= []).push(f),
    removeEventListener: (type, f) => { on[type] = (on[type] || []).filter(x => x !== f); },
    dispatchEvent: e => { const fs = on[e.type] || []; on[e.type] = []; fs.forEach(f => f(e)); },
  };
};
globalThis.document = {
  addEventListener() {},
  querySelectorAll: () => [],
  getElementById: id => (els[id] ||= mkEl(id)),
  dispatchEvent() {},
};
globalThis.window = { addEventListener() {}, innerWidth: 1280, innerHeight: 800 };
globalThis.Element = class {};
globalThis.Event = class { constructor(type) { this.type = type; } };

const ui = await import(pathToFileURL(path.join(dir, 'ui.js')).href);
beforeEach(() => { els = {}; });
const save = () => els['modal-save'].onclick();
// 和 app.js 一样:点「取消」= 关掉弹窗 + 发 modal-cancel
const cancel = () => { ui.closeModal(); els.modal.dispatchEvent(new Event('modal-cancel')); };

test('普通保存:成功就关;出错就留着并显示错误、按钮重新可点', async () => {
  ui.openModal('编辑', '<input>', async () => {});
  await save();
  assert.equal(els.modal.hidden, true);

  ui.openModal('编辑', '<input>', async () => { throw new Error('名字重复'); });
  await save();
  assert.equal(els.modal.hidden, false);
  assert.equal(els['modal-err'].textContent, '名字重复');
  assert.equal(els['modal-save'].disabled, false);
});

test('保存回调里换成结果框(批量生成):回调结束后结果框还开着,要用户自己关', async () => {
  ui.openModal('user.bulk', '<input id="f-count">', async () => {
    await Promise.resolve(); // 等接口
    ui.openModal('user.bulkResult', '<textarea readonly>u1\tvless://…</textarea>', null, { cancelText: 'common.close' });
  }, { size: 'xl' });
  await save();
  await new Promise(r => setTimeout(r, 20));
  assert.equal(els.modal.hidden, false, '结果框不能被前一个弹窗的保存流程关掉');
  assert.equal(els['modal-title'].textContent, 'user.bulkResult');
  assert.match(els['modal-body'].innerHTML, /vless:/);
  assert.equal(els['modal-save'].hidden, true, '结果框没有保存按钮');
  assert.equal(els['modal-cancel'].textContent, 'common.close');
  cancel();
  assert.equal(els.modal.hidden, true);
  // 下一个弹窗的取消按钮回到默认文字
  ui.openModal('x', '', async () => {});
  assert.equal(els['modal-cancel'].textContent, 'common.cancel');
});

test('换了弹窗之后旧回调才出错:错误不写进新弹窗,改用 toast', async () => {
  ui.openModal('a', '', async () => {
    ui.openModal('b', '', async () => {});
    els['modal-save'].disabled = true; // 新弹窗自己把按钮禁用着(比如在等预览)
    throw new Error('旧弹窗的错误');
  });
  await save();
  assert.equal(els.modal.hidden, false);
  assert.equal(els['modal-title'].textContent, 'b');
  assert.equal(els['modal-err'].textContent, '');
  assert.equal(els['modal-save'].disabled, true, '新弹窗的按钮状态不动');
  assert.equal(els.toast.textContent, '旧弹窗的错误');
});

test('确认框点确认后,调用方接着开的对话框不会被确认框的收尾关掉', async () => {
  const p = ui.confirm('确定?').then(ok => { if (ok) ui.openModal('下一步', '<input>', async () => {}); });
  await save();
  await p;
  await new Promise(r => setTimeout(r, 0));
  assert.equal(els.modal.hidden, false);
  assert.equal(els['modal-title'].textContent, '下一步');
});

test('确认框点取消:返回 false,弹窗关掉', async () => {
  const p = ui.confirm('确定?');
  cancel();
  assert.equal(await p, false);
  assert.equal(els.modal.hidden, true);
});

// 保存慢了:1.5 秒后按钮写「保存中…」,4 秒后错误栏写「还在保存」—— 只说还在保存,不猜在等什么
// (慢的原因没法在前端分辨:服务端排队校验、保存后刷新列表的请求、浏览器连接排队都可能),
// 调用方也不能塞进一句断言原因的话。保存完成后两处都复原;出错时显示错误本身。
test('保存慢了只说「还在保存」,完成后复原', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let finish;
  ui.openModal('编辑线路', '', () => new Promise(r => { finish = r; }), { slowHint: '在排队等 sing-box 校验' });
  const p = save();
  t.mock.timers.tick(1500);
  assert.equal(els['modal-save'].textContent, 'common.saving');
  assert.equal(els['modal-err'].textContent, '');
  t.mock.timers.tick(2500);
  assert.equal(els['modal-err'].textContent, 'common.savingSlow');
  finish();
  await p;
  assert.equal(els.modal.hidden, true);
  assert.equal(els['modal-save'].textContent, 'common.save');
  assert.equal(els['modal-err'].textContent, '');

  let fail;
  ui.openModal('编辑上游', '', () => new Promise((_, j) => { fail = j; }));
  const q = save();
  t.mock.timers.tick(4000);
  assert.equal(els['modal-err'].textContent, 'common.savingSlow');
  fail(new Error('名字重复'));
  await q;
  assert.equal(els.modal.hidden, false);
  assert.equal(els['modal-err'].textContent, '名字重复');
  assert.equal(els['modal-save'].textContent, 'common.save');
  assert.equal(els['modal-save'].disabled, false);
});

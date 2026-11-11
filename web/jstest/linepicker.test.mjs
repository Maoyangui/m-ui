// 线路 × 服务器选择器(web/assets/js/linepicker.js)的写回语义:界面重做后,同样的勾选写回的 lineRefs 必须和以前一字不差。
// 整条线路的入口全勾 = {lineId}(以后新加的服务器自动包含);部分勾 = {lineId, nodeIds};代理授权收窄过的线路勾满也写 nodeIds。
// 运行:node --test web/jstest/linepicker.test.mjs(CI 里和 exts.test.mjs 一起跑)。
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'linepicker-test-'));
fs.copyFileSync(path.join(here, '..', 'assets', 'js', 'linepicker.js'), path.join(dir, 'linepicker.js'));
fs.writeFileSync(path.join(dir, 'i18n.js'), 'export const t = (k, v) => (v ? k + JSON.stringify(v) : k);');
fs.writeFileSync(path.join(dir, 'ui.js'), `export const esc = (s) => String(s ?? '');
export const chipList = (items, o = {}) => (items.length > (o.max ?? 12) ? 'FOLD:' : '') + items.join('');`);
const lp = await import(pathToFileURL(path.join(dir, 'linepicker.js')).href);
after(() => fs.rmSync(dir, { recursive: true, force: true }));

const nodes = [{ id: 1, name: 'A', isLocal: true }, { id: 2, name: 'B' }, { id: 3, name: 'C' }];
const lines = [
  { id: 10, name: 'all-servers', nodeIds: [] },        // 部署到全部服务器
  { id: 11, name: 'two', nodeIds: [2, 3] },            // 两台
  { id: 12, name: 'one', nodeIds: '[1]' },             // 老数据:字符串形式
];

test('lineItems:按部署的服务器展开成入口;授权收窄的标 narrowed', () => {
  const items = lp.lineItems(lines, nodes);
  assert.deepEqual(items.map(i => i.key), ['10:1', '10:2', '10:3', '11:2', '11:3', '12:1']);
  const granted = lp.lineItems(lines, nodes, [{ lineId: 10, nodeIds: [1, 2] }, { lineId: 11 }]);
  assert.deepEqual(granted.map(i => i.key), ['10:1', '10:2', '11:2', '11:3']);
  assert.deepEqual(granted.map(i => i.narrowed), [true, true, false, false]);
  assert.deepEqual(lp.lineItems(lines, []).map(i => i.key), ['10:0', '11:0', '12:0'], '没有服务器列表(单机)时每条线路一个入口');
});

test('写回:整条全勾 = {lineId};部分勾 = nodeIds;来回换算不变', () => {
  const items = lp.lineItems(lines, nodes);
  const keys = new Set(['10:1', '10:2', '10:3', '11:3', '12:1']);
  const refs = lp.refsFromKeys(keys, items);
  assert.deepEqual(refs, [{ lineId: 10 }, { lineId: 11, nodeIds: [3] }, { lineId: 12 }]);
  assert.deepEqual([...lp.keysFromRefs(refs, items)].sort(), [...keys].sort());
  assert.deepEqual(lp.refsFromKeys(new Set(), items), []);
});

test('写回:代理授权收窄过的线路勾满也只写具体服务器', () => {
  const items = lp.lineItems(lines, nodes, [{ lineId: 10, nodeIds: [1, 2] }, { lineId: 11 }]);
  const refs = lp.refsFromKeys(new Set(['10:1', '10:2', '11:2', '11:3']), items);
  assert.deepEqual(refs, [{ lineId: 10, nodeIds: [1, 2] }, { lineId: 11 }]);
});

test('refLabels:按线路聚合,整条写线路名,部分写「线路名 · n/m 台」', () => {
  const labels = lp.refLabels([{ lineId: 10 }, { lineId: 11, nodeIds: [3] }, { lineId: 12, nodeIds: [1] }, { lineId: 99 }], lines, nodes);
  assert.deepEqual(labels, ['all-servers', 'two · lp.nOf{"n":1,"m":2}', 'one']);
  assert.deepEqual(lp.refLabels([{ lineId: 11, nodeIds: [2] }], lines, [nodes[0]]), ['two'], '只有一台服务器的面板不写台数');
  const chips = lp.refChips([{ lineId: 10 }, { lineId: 11, nodeIds: [2, 3] }], lines, nodes, { key: 'k', max: 1 });
  assert.match(chips, /^FOLD:/, '多了折叠');
  assert.match(chips, /title="Breach\.sepC"/, '服务器名放进 title(桩里的 t 原样返回键名,分隔符就是 reach.sep)');
  assert.equal(lp.refChips([], lines, nodes), '');
});

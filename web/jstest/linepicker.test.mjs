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

test('refLabels:只算线路现在还部署着的服务器;换套餐预览可以写出服务器名', () => {
  // 线路 11 现在部署在 2、3;用户分配里还留着已撤下的 1 和 2:是 1/2 台,不是整条
  assert.deepEqual(lp.refLabels([{ lineId: 11, nodeIds: [1, 2] }], lines, nodes), ['two · lp.nOf{"n":1,"m":2}']);
  assert.deepEqual(lp.refLabels([{ lineId: 11, nodeIds: [1, 2, 3] }], lines, nodes), ['two'], '现部署的都在 = 整条');
  // 同一线路换了同样台数的服务器:默认写法一样,servers 写法能看出差别(按名字排序,和顺序无关)
  const a = [{ lineId: 10, nodeIds: [1, 2] }], b = [{ lineId: 10, nodeIds: [3, 1] }];
  assert.deepEqual(lp.refLabels(a, lines, nodes), lp.refLabels(b, lines, nodes));
  assert.deepEqual(lp.refLabels(a, lines, nodes, { servers: true }), ['all-servers · Areach.sepB']);
  assert.deepEqual(lp.refLabels(b, lines, nodes, { servers: true }), ['all-servers · Areach.sepC']);
});

// ---- 中转入口也是一个个节点(relay: true,用户 / 套餐的选择器):逐个勾,范围里没勾的进 hide ----
const rlines = [
  { id: 20, name: 'hk', nodeIds: [], nodeOpts: '{"f4":{"hidden":true}}', forwards: [
    { id: 1, name: '', via: 2, port: 40001, to: 1 },                         // 集群中转,没起名字:经 B 转到 A
    { id: 2, name: '广州入口', host: 'gz.example.com', port: 40002, to: 3 },   // 别处的中转,转到 C
    { id: 3, host: 'old.example.com', port: 40003, to: 3, off: true },        // 暂停着
    { id: 4, name: '关了输出', via: 2, port: 40004, to: 3 },                   // 线路上关了输出
  ] },
  { id: 21, name: 'plain', nodeIds: [2, 3] },
];

test('lineItems(relay):线路后面列出会进订阅的中转入口;暂停的、关了输出的不列', () => {
  const items = lp.lineItems(rlines, nodes, null, { relay: true });
  assert.deepEqual(items.map(i => i.key), ['20:1', '20:2', '20:3', '20:f1', '20:f2', '21:2', '21:3']);
  const f1 = items.find(i => i.key === '20:f1'), f2 = items.find(i => i.key === '20:f2');
  assert.equal(f1.nodeName, 'B', '没起名字的集群中转用那台服务器的名字');
  assert.equal(f1.nodeId, 1, '归在出口那台名下(按服务器筛的时候跟着出口走)');
  assert.equal(f1.exitName, 'A');
  assert.equal(f2.nodeName, '广州入口');
  assert.equal(f2.nodeId, 3);
  assert.deepEqual(lp.lineItems(rlines, nodes).map(i => i.key), ['20:1', '20:2', '20:3', '21:2', '21:3'], '不带 relay(代理授权那个选择器)和以前一样');
  assert.deepEqual(lp.lineItems(rlines, nodes, [{ lineId: 20, nodeIds: [1, 2] }], { relay: true }).map(i => i.key), ['20:1', '20:2', '20:f1'], '授权只给了 A、B:出口在 C 的入口不列');
  const slim = [{ id: 20, name: 'hk', nodeIds: [], relays: [{ id: 2, name: '广州入口', to: 3 }] }];
  assert.deepEqual(lp.lineItems(slim, nodes, [{ lineId: 20 }], { relay: true }).map(i => i.key), ['20:1', '20:2', '20:3', '20:f2'], '代理面板:用后端算好的 relays');
});

test('写回(relay):全勾 = {lineId};不勾的节点进 hide;只勾中转 = 范围里有它的出口、不要那台的直连', () => {
  const items = lp.lineItems(rlines, nodes, null, { relay: true });
  const all = new Set(items.filter(i => i.lineId === 20).map(i => i.key));
  assert.deepEqual(lp.refsFromKeys(all, items), [{ lineId: 20 }], '全勾:以后新加的服务器、入口都算');
  let keys = new Set([...all].filter(k => k !== '20:f2'));
  let refs = lp.refsFromKeys(keys, items);
  assert.deepEqual(refs, [{ lineId: 20, hide: ['f2'] }], '不要广州入口');
  assert.deepEqual([...lp.keysFromRefs(refs, items)].sort(), [...keys].sort(), '来回换算不变');
  keys = new Set(['20:1', '20:f1']);
  refs = lp.refsFromKeys(keys, items);
  assert.deepEqual(refs, [{ lineId: 20, nodeIds: [1] }], '只要 A 上的直连 + 转到 A 的入口');
  assert.deepEqual([...lp.keysFromRefs(refs, items)].sort(), [...keys].sort());
  keys = new Set(['20:f2']);
  refs = lp.refsFromKeys(keys, items);
  assert.deepEqual(refs, [{ lineId: 20, nodeIds: [3], hide: ['s3'] }], '只勾广州入口(出口 C):范围里要有 C,但不要 C 的直连');
  assert.deepEqual([...lp.keysFromRefs(refs, items)], ['20:f2']);
  refs = lp.refsFromKeys(new Set(['20:1', '20:2', '20:3']), items);
  assert.deepEqual(refs, [{ lineId: 20, hide: ['f1', 'f2'] }], '直连全要、入口都不要:服务器范围仍是全部');
  assert.deepEqual(lp.refsFromKeys(new Set(['21:2']), items), [{ lineId: 21, nodeIds: [2] }], '没有中转的线路和以前一样');
});

test('写回(keep):这里没列出来的节点,原先不要的照样留着', () => {
  const items = lp.lineItems([{ id: 20, name: 'hk', nodeIds: [] }], nodes, [{ lineId: 20 }], { relay: true }); // 拿不到中转信息:只有服务器
  const keep = [{ lineId: 20, hide: ['f2', 's2'] }];
  const keys = lp.keysFromRefs(keep, items);
  assert.deepEqual([...keys].sort(), ['20:1', '20:3']);
  // B 的直连没勾、也没有勾着的入口转到 B:B 这台就不在范围里了(和以前「不勾这台服务器」一样,连凭据都不给),不是只在订阅里藏起来
  assert.deepEqual(lp.refsFromKeys(keys, items, keep), [{ lineId: 20, nodeIds: [1, 3], hide: ['f2'] }], 'f2 这里看不见,留着');
  keys.add('20:2');
  assert.deepEqual(lp.refsFromKeys(keys, items, keep), [{ lineId: 20, hide: ['f2'] }]);
  assert.deepEqual(lp.refsFromKeys(keys, items), [{ lineId: 20 }], '不给 keep(新建)就没有可留的');
});

test('refLabels(relay):线路有中转入口、或者有不要的节点时按节点数写', () => {
  assert.deepEqual(lp.refLabels([{ lineId: 20 }], rlines, nodes), ['hk'], '都要 = 只写线路名');
  assert.deepEqual(lp.refLabels([{ lineId: 20, hide: ['f2'] }], rlines, nodes), ['hk · lp.nOfNodes{"n":4,"m":5}']);
  assert.deepEqual(lp.refLabels([{ lineId: 21, hide: ['s2'] }], rlines, nodes), ['plain · lp.nOfNodes{"n":1,"m":2}']);
  assert.deepEqual(lp.refLabels([{ lineId: 21, nodeIds: [2] }], rlines, nodes), ['plain · lp.nOf{"n":1,"m":2}'], '没有中转、没有不要的:照旧按台数');
  assert.match(lp.refChips([{ lineId: 20, nodeIds: [3], hide: ['s3'] }], rlines, nodes), /title="⇄ 广州入口"/);
});

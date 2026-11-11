// 「哪里有问题」的共用判定(web/assets/js/issues.js)与 ui.js 里概况用的两个小工具(chipList、auditText)的测试。
// 概况的「需要处理」、服务器页「只看异常」、上游页「只看故障」、顶部提示条都靠 issues.js,口径必须一致。
// 运行:node --test web/jstest/issues.test.mjs(CI 里和 exts.test.mjs 一起跑)。
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const assets = path.join(here, '..', 'assets', 'js');
const I = await import(pathToFileURL(path.join(assets, 'issues.js')).href);

// ui.js 依赖 i18n.js,还在模块顶层挂了 document / window 的事件:换成桩
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'issues-test-'));
fs.copyFileSync(path.join(assets, 'ui.js'), path.join(dir, 'ui.js'));
fs.writeFileSync(path.join(dir, 'i18n.js'), 'export const t = (k, v) => (v ? k + JSON.stringify(v) : k);');
const handlers = {}; // ui.js 的事件委托(click / change / toggle)挂在 document 上:记下来,测试里模拟点击
globalThis.document = { addEventListener: (type, fn) => { handlers[type] = fn; }, dispatchEvent() {} };
globalThis.window = { addEventListener() {} };
const ui = await import(pathToFileURL(path.join(dir, 'ui.js')).href);
after(() => fs.rmSync(dir, { recursive: true, force: true }));

const on = (name, st = {}) => ({ name, enabled: true, status: { ok: true, coreRunning: true, synced: true, ...st } });

test('nodeIssues:离线时只报离线;在线时按严重程度列出各项;本机不算', () => {
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: { ok: false, error: 'timeout' } }), [{ code: 'offline', sev: 'bad' }]);
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: { ok: false, error: 'x', coreRunning: false, synced: false } }).map(x => x.code), ['offline']);
  assert.deepEqual(I.nodeIssues({ name: 'l', isLocal: true, enabled: true, status: null }), []);
  const many = on('b', { coreRunning: false, reloadError: 'bind', ledgerFails: 3, synced: false, versionMismatch: true });
  assert.deepEqual(I.nodeIssues(many).map(x => x.code), ['coreStopped', 'reloadErr', 'ledger', 'unsynced', 'version']);
  assert.deepEqual(I.nodeIssues(many).map(x => x.sev), ['bad', 'bad', 'bad', 'warn', 'warn']);
  assert.deepEqual(I.nodeIssues(on('c')), []);
});

test('nodeIssues:离线也防抖:一轮同步失败、上次联系上不到 30 秒的先不算;从没联系上的直接算', () => {
  const now = 10_000;
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: { ok: false, lastSeen: now - 10 } }, { now }), []);
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: { ok: false, lastSeen: now - 30 } }, { now }).map(x => x.code), ['offline']);
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: { ok: false, error: 'connection refused' } }, { now }).map(x => x.code), ['offline']);
  const sum = I.summarizeNodes({ nodes: [{ name: 'l', isLocal: true, enabled: true }, { name: 'blip', enabled: true, status: { ok: false, lastSeen: now - 5 } }] }, { now });
  assert.deepEqual([sum.total, sum.online, sum.red], [2, 2, 0]);
});

test('nodeIssues:主机还没轮到的(刚启动、刚添加)算待定不算离线;离线防抖按服务器时间(GET nodes 的 now)', () => {
  const now = 10_000;
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: null }, { now }), [], '主机刚启动,状态还没建');
  assert.deepEqual(I.nodeIssues({ name: 'a', enabled: true, status: { ok: false } }, { now }), [], '首轮同步还在途');
  // 浏览器时钟快了 10 分钟:按服务器给的 now 算,刚失联 5 秒的不报
  const resp = { now, nodes: [{ name: 'blip', enabled: true, status: { ok: false, lastSeen: now - 5, error: 'x' } }] };
  assert.equal(I.summarizeNodes(resp).offline, 0);
  assert.equal(I.summarizeNodes({ ...resp, now: now + 60 }).offline, 1);
});

test('nodeIssues:未同步要连续 30 秒才算(一次保存后会有几秒未同步)', () => {
  const n = on('a', { synced: false });
  assert.deepEqual(I.nodeIssues(n, { unsyncedFor: 29 }), []);
  assert.deepEqual(I.nodeIssues(n, { unsyncedFor: 30 }).map(x => x.code), ['unsynced']);
  assert.deepEqual(I.nodeIssues(n).map(x => x.code), ['unsynced'], '不给持续时间时按已超过门槛算');
});

test('nodeIssues:停用的服务器只看下线通知;启用的有下线通知也报', () => {
  assert.deepEqual(I.nodeIssues({ name: 'x', enabled: false, decommission: { attempts: 3 }, status: null }).map(x => x.code), ['decom']);
  assert.deepEqual(I.nodeIssues({ name: 'x', enabled: false, status: null }), []);
});

test('trackUnsynced:记第一次看到的时间,同步了就清掉', () => {
  const since = new Map();
  const nodes = [on('a', { synced: false }), on('b'), { name: 'c', enabled: true, status: { ok: false } }];
  let f = I.trackUnsynced(since, nodes, 1000);
  assert.equal(f('a'), 0);
  f = I.trackUnsynced(since, nodes, 41000);
  assert.equal(f('a'), 40);
  assert.equal(f('b'), 0);
  assert.equal(since.has('c'), false, '离线的不算未同步');
  nodes[0].status.synced = true;
  I.trackUnsynced(since, nodes, 42000);
  assert.equal(since.has('a'), false);
});

test('summarizeNodes:总数 = 启用的远端 + 本机;已删除但通知没送到的也算下线通知;名单按严重程度', () => {
  const resp = {
    nodes: [
      { name: 'local', isLocal: true, enabled: true },
      on('ok'),
      { name: 'down', enabled: true, status: { ok: false, error: 'timeout' } },
      on('stopped', { coreRunning: false }),
      on('old', { versionMismatch: true }),
      { name: 'off', enabled: false, decommission: {} },
    ],
    decommissions: [{ name: 'gone' }],
  };
  const sum = I.summarizeNodes(resp);
  assert.equal(sum.total, 5); // local + ok + down + stopped + old(停用的不算)
  assert.equal(sum.online, 4);
  assert.deepEqual(sum.byCode.decom.sort(), ['gone', 'off']);
  assert.equal(sum.offline, 1);
  assert.equal(sum.fault, 3); // coreStopped 1 + decom 2
  assert.equal(sum.warn, 1);
  assert.equal(sum.red, 4);
  assert.equal(sum.yellow, 1);
  assert.equal(sum.names[0], 'down');
  assert.equal(sum.names[sum.names.length - 1], 'old');
  assert.equal(I.worstCode(sum), 'offline');
  const single = I.summarizeNodes({ nodes: [{ name: 'l', isLocal: true, enabled: true }], decommissions: [{ name: 'gone' }] });
  assert.deepEqual([single.total, single.online, single.red], [1, 1, 1], '单服务器时只可能出现下线通知');
});

test('healthSummary:按上游分组;没在用的不算;坏的台数多的在前;待测 / 过期单独计', () => {
  assert.equal(I.healthSummary({}), null);
  const h = { lastRun: 100, intervalMinutes: 10, results: [
    { id: 1, name: 'b-up', servers: [{ name: 's1', state: 'fail', error: 'timeout' }, { name: 's2', state: 'ok' }] },
    { id: 2, name: 'a-up', servers: [{ name: 's1', state: 'fail' }, { name: 's2', state: 'fail', error: 'eof' }] },
    { id: 3, name: 'c-up', servers: [{ name: 's1', state: 'fail' }] },
    { id: 4, name: 'good', servers: [{ name: 's1', state: 'ok' }, { name: 's2', state: 'stale' }] },
    { id: 5, name: 'wait', servers: [{ name: 's1', state: 'pending' }] },
    { id: 6, name: 'unused', unused: true, servers: [{ name: 's1', state: 'fail' }] },
  ] };
  const s = I.healthSummary(h);
  assert.equal(s.rows.length, 5);
  assert.equal(s.ok, 1);
  assert.equal(s.pending, 1);
  assert.deepEqual(s.bad.map(b => b.name), ['a-up', 'b-up', 'c-up']);
  assert.deepEqual(s.bad[0].fails, ['s1', 's2']);
  assert.equal(s.bad[0].err, 'eof');
  assert.equal(s.bad[1].total, 2);
  assert.equal(s.servers.size, 2);
  assert.equal(s.single, false);
  assert.equal(I.healthSummary({ lastRun: 1, results: [{ id: 1, name: 'x', servers: [{ name: 'only', state: 'fail' }] }] }).single, true);
  assert.equal(I.upstreamBad(h.results[0]), true);
  assert.equal(I.upstreamBad(h.results[3]), false);
  assert.equal(I.upstreamBad(h.results[5]), false, '没在用的不算故障');
});

test('证书:续期失败 → 文件读不出 → 已过期 → 快到期;签名格式和以前一样(已忽略的不会因为升级又冒出来)', () => {
  const now = 1_000_000;
  const list = [
    { nodeId: 2, name: 'b', trouble: 'expiring', notAfter: now + 5 * 86400 },
    { nodeId: 0, name: 'a', trouble: 'expiring', notAfter: now - 86400 },
    { nodeId: 3, name: 'c', trouble: 'missing' },
    { nodeId: 0, name: 'd', trouble: 'failing', notAfter: now + 86400 },
    { nodeId: 4, name: 'e', trouble: 'expiring', notAfter: now + 2 * 86400 },
  ];
  assert.deepEqual(I.sortCerts(list, now).map(x => x.name), ['d', 'c', 'a', 'e', 'b']);
  assert.deepEqual(I.certCounts(list, now), { failing: 1, missing: 1, expired: 1, expiring: 2 });
  assert.equal(I.certSig(list, now), '2:b:expiring,0:a:expiring:x,3:c:missing,0:d:failing,4:e:expiring');
  // 关掉「快到期」后证书过期了:签名变了,提示回来
  const soon = [{ nodeId: 0, name: 'a', trouble: 'expiring', notAfter: now + 3600 }];
  assert.notEqual(I.certSig(soon, now), I.certSig(soon, now + 7200));
  assert.equal(I.certSig(soon, now), '0:a:expiring', '没过期的签名格式和以前一样');
  assert.equal(I.certLeftDays(list[0], now), 5);
  assert.equal(I.certLeftDays(list[1], now), -1);
});

test('大陆连通:不通的在前、部分不通的在后;签名按原顺序', () => {
  const bad = [{ nodeId: 1, verdict: 'partial' }, { nodeId: 2, verdict: 'blocked' }, { nodeId: 3, verdict: 'partial' }, { nodeId: 4, verdict: 'portBlocked' }];
  assert.deepEqual(I.sortReach(bad).map(r => r.nodeId), [2, 4, 1, 3]);
  assert.equal(I.reachSig(bad), '1:partial,2:blocked,3:partial,4:portBlocked');
  assert.deepEqual(I.reachHard(bad).map(r => r.nodeId), [2, 4]);
});

test('auditText:字符串原样、对象写成 JSON、null 为空、数组超过 3 项写成「前 3 项 等 N 个」', () => {
  assert.equal(ui.auditText('alice'), 'alice');
  assert.equal(ui.auditText('"alice"'), 'alice');
  assert.equal(ui.auditText('null'), '');
  assert.equal(ui.auditText(null), '');
  assert.equal(ui.auditText({ name: 'x', target: 'panel' }), '{"name":"x","target":"panel"}');
  assert.equal(ui.auditText([1, 2]), '1, 2');
  assert.equal(ui.auditText([100, 101, 102, 103, 104]), 'common.andMore{"list":"100, 101, 102","n":5,"more":2}');
  assert.equal(ui.auditText([100, 101, 102, 103, 104], { max: 0 }), '100, 101, 102, 103, 104');
  assert.equal(ui.auditText([{ a: 1 }, null]), '{"a":1}, ');
  assert.equal(ui.auditText('not json {'), 'not json {');
});

test('chipList:不多时原样;多了露前 max 个 + 「+N」;lazy 折起时不渲染藏起来的,展开后最多渲染 cap 个', () => {
  const items = Array.from({ length: 10 }, (_, i) => `<i>${i}</i>`);
  assert.equal(ui.chipList(items.slice(0, 3), { max: 3 }), '<span class="chips"><i>0</i><i>1</i><i>2</i></span>');
  const folded = ui.chipList(items, { key: 'k1', max: 4 });
  assert.match(folded, /class="chips fold"/);
  assert.match(folded, /data-n="6"/);
  assert.match(folded, />\+6<\/button>/);
  assert.equal((folded.match(/fold-x/g) || []).length, 6, '不 lazy 时藏起来的也在 DOM 里(CSS 藏)');
  const lazy = ui.chipList(items, { key: 'k2', max: 4, lazy: true, cap: 7 });
  assert.equal((lazy.match(/<i>/g) || []).length, 4, 'lazy 折起时只渲染露出来的');
  assert.equal(ui.chipOpen('k2'), false);
});

test('chipList:展开状态按 key 记住(5 秒一轮重画后不收起),lazy 时点「+N」会调登记的重画函数', () => {
  const items = Array.from({ length: 9 }, (_, i) => `<i>${i}</i>`);
  let redraws = 0;
  ui.onChipToggle('k3', () => { redraws++; });
  const html = ui.chipList(items, { key: 'k3', max: 3, lazy: true, cap: 5 });
  assert.match(html, /data-act="ui.chips" data-id="k3" data-n="6"/);
  // 模拟点「+6」:走 ui.js 的事件委托
  const btn = { dataset: { act: 'ui.chips', id: 'k3', n: '6' }, closest: sel => (sel === '[data-act]' ? btn : null), textContent: '', setAttribute() {} };
  handlers.click({ target: btn, preventDefault() {} });
  assert.equal(ui.chipOpen('k3'), true);
  assert.equal(redraws, 1, 'lazy 时展开要让页面重画(藏起来的本来就不在 DOM 里)');
  const open = ui.chipList(items, { key: 'k3', max: 3, lazy: true, cap: 5 });
  assert.match(open, /class="chips fold open"/);
  assert.equal((open.match(/<i>/g) || []).length, 5, '展开后最多渲染 cap 个');
  assert.match(open, /common.less/);
  handlers.click({ target: btn, preventDefault() {} });
  assert.equal(ui.chipOpen('k3'), false);
  assert.equal(redraws, 2);
  // 不 lazy、没登记重画:原地切换 class
  const box = { classList: { on: false, contains() { return this.on; }, toggle(c, v) { this.on = v; } } };
  const b2 = { dataset: { act: 'ui.chips', id: 'k4', n: '2' }, closest: sel => (sel === '[data-act]' ? b2 : sel === '.fold' ? box : null), textContent: '', setAttribute() {} };
  handlers.click({ target: b2, preventDefault() {} });
  assert.equal(box.classList.on, true);
  assert.equal(b2.textContent, 'common.less');
  assert.equal(ui.chipOpen('k4'), true);
});

test('大陆连通检测停摆(reachStall):lastRun 每轮都往前走,额度用尽、一台都没测成也要报出来', () => {
  const now = 1_800_000_000, m = 60;
  const base = { auto: true, minutes: m, lastRun: now - 60, lastOk: now - 60, tried: 3, failed: 0 };
  assert.equal(I.reachStall(base, now), '');
  assert.equal(I.reachStall({ ...base, auto: false, lastRun: 1 }, now), '', '没开自动检测不算');
  assert.equal(I.reachStall({ ...base, lastRun: now - 3 * m * 60 - 1 }, now), 'idle');
  assert.equal(I.reachStall({ ...base, error: 'x' }, now), 'error', '取测点出错:以前要求 lastRun 为 0,第一轮之后永远不报');
  assert.equal(I.reachStall({ ...base, failed: 3 }, now), 'failing', '最近一轮全没测成(额度用尽)');
  assert.equal(I.reachStall({ ...base, failed: 2 }, now), '', '部分没测成不报');
  assert.equal(I.reachStall({ ...base, lastOk: now - 3 * m * 60 - 1, failed: 1 }, now), 'failing', '在跑但很久没测成');
  assert.equal(I.reachStall({ ...base, lastOk: 0 }, now), 'never');
  assert.equal(I.reachStall({ auto: true, minutes: m, lastRun: now - 60 }, now), '', '老后端没有 lastOk / tried:只按 idle、error');
});

test('复合控件(线路 / 用户选择器、勾选列表、一排勾选框)不包在 <label> 里:用 fieldBox,不用 field', () => {
  assert.match(ui.fieldBox('L', '<div id="x"></div>', 'h'), /^<div class="field">/);
  assert.match(ui.field('L', '<input>'), /^<label class="field">/);
  const pages = path.join(assets, 'pages');
  const bad = [];
  for (const f of fs.readdirSync(pages).filter(n => n.endsWith('.js'))) {
    const src = fs.readFileSync(path.join(pages, f), 'utf8');
    for (const m of src.matchAll(/\bfield\(([^`]{0,80})`(<div (?:id="f-(?:lines|targets)"|class="check-list)|<div class="row"[^>]*>\$\{\[1, 2)/g)) bad.push(`${f}: field(${m[1]}\`${m[2]}`);
  }
  assert.deepEqual(bad, [], '点标签字或空白会被浏览器转给里面第一个控件');
});

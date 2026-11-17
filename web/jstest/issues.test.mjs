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
  // 防火墙:规则没落成 / 端口没放行成算一项(提醒级);这台没有 nft / iptables、又有规则管到它也算;命令行特意暂停的、老版本没报的不算
  assert.deepEqual(I.nodeIssues(on('d', { fw: { engine: 'nft', rules: 2, error: 'nft: syntax error' } })), [{ code: 'fw', sev: 'warn' }]);
  assert.deepEqual(I.nodeIssues(on('d', { fw: { engine: '', rules: 1 } })).map(x => x.code), ['fw']);
  assert.deepEqual(I.nodeIssues(on('d', { fw: { engine: '', rules: 0 } })), []);
  assert.deepEqual(I.nodeIssues(on('d', { fw: { engine: 'nft', rules: 2, error: 'x', paused: true } })), []);
  assert.deepEqual(I.nodeIssues(on('d', { fw: { engine: 'nft', rules: 3 } })), []);
  assert.equal(I.fwBad(undefined), false);
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

test('磁盘:快满 / 满了算服务器的问题(和后端 model.DiskLevel 同一口径,按 df 的 Use%);读不到、老副机不报不算', () => {
  const G = 2 ** 30, M = 2 ** 20;
  const on = (disk, name = 'd') => ({ name, enabled: true, status: { ok: true, coreRunning: true, synced: true, disk } });
  assert.deepEqual(I.nodeIssues(on(undefined)), []);
  assert.deepEqual(I.nodeIssues(on({ used: 0, free: 0 })), []);
  assert.deepEqual(I.nodeIssues(on({ used: 89 * G, free: 11 * G })), []);
  assert.deepEqual(I.nodeIssues(on({ used: 90 * G, free: 10 * G })), [{ code: 'disk', sev: 'warn' }]);
  assert.deepEqual(I.nodeIssues(on({ used: 5 * G, free: 900 * M })), [{ code: 'disk', sev: 'warn' }]);
  assert.deepEqual(I.nodeIssues(on({ used: 970 * G, free: 30 * G })), [{ code: 'diskFull', sev: 'bad' }]);
  assert.deepEqual(I.nodeIssues(on({ used: 2 * G, free: 250 * M })), [{ code: 'diskFull', sev: 'bad' }]);
  assert.equal(I.diskPct({ used: 1, free: 199 }), 1);
  assert.equal(I.diskPct({ used: 91, free: 9 }), 91);
  assert.equal(I.diskPct(null), 0);
  const sum = I.summarizeNodes({ nodes: [on({ used: 98 * G, free: 2 * G }), on({ used: 91 * G, free: 9 * G }, 'e')] });
  assert.equal(sum.red, 1);
  assert.equal(sum.yellow, 1);
  assert.deepEqual(sum.byCode.diskFull, ['d']);
  assert.deepEqual(sum.byCode.disk, ['e']);
});

// ---- 服务器的流量额度 / 到期(web/node_quota.go 给的 n.quota)----
// 用完 / 到期被停不是故障:不进 nodeIssues,概况里单独一条提醒,点「知道了」收起
const GB = 1073741824;
const quota = (over = {}) => ({ volume: 500 * GB, used: 100 * GB, expiry: 0, state: '', stateAt: 0, autoOff: false, ...over });

test('quotaBlocked:被额度拦下的才列(整台自动停用 / 只停了代理服务),手动停用的、没设的、快用完的都不列', () => {
  const list = I.quotaBlocked([
    { id: 1, name: 'Tokyo', isLocal: true, enabled: true, suspended: true, quota: quota({ used: 500 * GB, state: 'traffic', stateAt: 111 }) }, // 主机自己:只能停服务
    { id: 2, name: 'Frankfurt', enabled: false, quota: quota({ used: 503 * GB, state: 'traffic', stateAt: 222, autoOff: true }) },            // 整台自动停用
    { id: 3, name: 'Singapore', enabled: true, suspended: true, quota: quota({ expiry: 5, state: 'expired', stateAt: 333 }) },                // 关了自动停用:只停服务
    { id: 4, name: 'Manual', enabled: false, quota: quota({ used: 600 * GB, state: 'traffic', stateAt: 444 }) },                              // 管理员自己停用的
    { id: 5, name: 'Soon', enabled: true, quota: quota({ used: 499 * GB }) },                                                                 // 快用完:不提前提醒
    { id: 6, name: 'Plain', enabled: true },
    null,
  ]);
  assert.deepEqual(list, [
    { id: 1, name: 'Tokyo', why: 'traffic', mode: 'hold', at: 111 },
    { id: 2, name: 'Frankfurt', why: 'traffic', mode: 'off', at: 222 },
    { id: 3, name: 'Singapore', why: 'expired', mode: 'hold', at: 333 },
  ]);
  // 副机自己的面板:没有额度数据,只知道被主机停了代理服务
  assert.deepEqual(I.quotaBlocked([{ id: 9, name: 'self', isLocal: true, enabled: true, suspended: true }]), [{ id: 9, name: 'self', why: '', mode: 'hold', at: 0 }]);
  assert.deepEqual(I.quotaBlocked(null), []);
});

test('被额度拦下的不算服务器的问题:nodeIssues / summarizeNodes 不变,「只看异常」「故障」里都没有它', () => {
  const off = { id: 2, name: 'Frankfurt', enabled: false, quota: quota({ used: 503 * GB, state: 'traffic', autoOff: true }) };
  const held = { ...on('Singapore'), id: 3, suspended: true, quota: quota({ used: 500 * GB, state: 'traffic' }) };
  const local = { id: 1, name: 'Tokyo', isLocal: true, enabled: true, suspended: true, quota: quota({ used: 500 * GB, state: 'traffic' }) };
  assert.deepEqual(I.nodeIssues(off), []);
  assert.deepEqual(I.nodeIssues(held), []);
  assert.deepEqual(I.nodeIssues(local), []);
  const sum = I.summarizeNodes({ nodes: [local, off, held, on('Fine')] });
  assert.equal(sum.red, 0);
  assert.equal(sum.yellow, 0);
  assert.equal(sum.total, 3, '启用的远端 2 台 + 本机;被自动停用的那台不在总数里');
  assert.equal(sum.online, 3);
  assert.ok(!I.NODE_CODES.includes('quota'));
});

test('「知道了」:只收起已经看过的;新被拦下、恢复后再次被拦下、原因变了才重新提醒;恢复的从记录里划掉', () => {
  const a = { id: 2, name: 'Frankfurt', why: 'traffic', mode: 'off', at: 100 };
  const b = { id: 3, name: 'Singapore', why: 'traffic', mode: 'hold', at: 200 };
  assert.deepEqual(I.quotaFresh([a, b], null), [a, b], '没点过:都是新的');
  const ack = I.quotaAckKeys([a, b]);
  assert.equal(ack, '2:traffic:100,3:traffic:200');
  assert.deepEqual(I.quotaFresh([a, b], ack), []);
  assert.deepEqual(I.quotaFresh([b], ack), [], '有一台恢复了、名单变短:不重新提醒');
  const c = { id: 4, name: 'Osaka', why: 'expired', mode: 'off', at: 300 };
  assert.deepEqual(I.quotaFresh([a, b, c], ack), [c], '又多了一台');
  assert.deepEqual(I.quotaFresh([{ ...a, at: 500 }, b], ack), [{ ...a, at: 500 }], '同一台恢复后再次被拦下');
  assert.deepEqual(I.quotaFresh([{ ...a, why: 'expired' }, b], ack), [{ ...a, why: 'expired' }], '原因变了');
  // 划掉已经恢复的:只留现在还被拦着的
  assert.equal(I.quotaAckPrune([b], ack), '3:traffic:200');
  assert.equal(I.quotaAckPrune([], ack), '');
  assert.equal(I.quotaAckPrune([a, b, c], ack), ack);
  assert.equal(I.quotaAckPrune([a], ''), '');
  // 副机自己的面板(没有时间):恢复时记录被划掉,第二次被停照样提醒
  const self = { id: 9, name: 'self', why: '', mode: 'hold', at: 0 };
  const ack2 = I.quotaAckKeys([self]);
  assert.deepEqual(I.quotaFresh([self], ack2), []);
  assert.deepEqual(I.quotaFresh([self], I.quotaAckPrune([], ack2)), [self]);
});

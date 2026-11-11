import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { settle, parsePledge, validName, toMicros, deviceCount, publicBoard, checkDonors, run, check, trackSources, staleSources, ADDRESS, USDT, TOPIC, MAX_MICROS, PENDING_KEEP } from './settle.mjs';

const SETTLE = fileURLToPath(new URL('./settle.mjs', import.meta.url));

// 测试一律不联网:stats 的下载计数是公开的(打一次就多算一台),ntfy、TronGrid 也是别人的服务。
// run() 都传假的 fetch;万一哪里漏了走到全局 fetch,直接失败。
globalThis.fetch = async url => { throw new Error('测试里不该联网: ' + url); };

const H = 3600, DAY = 24 * H;
const tx = (id, usdt, at, extra = {}) => ({ transaction_id: id, value: String(Math.round(usdt * 1e6)), block_timestamp: at * 1000, to: ADDRESS, type: 'Transfer', token_info: { address: USDT }, ...extra });
const pledge = (id, name, usdt, at) => ({ id, name, micros: Math.round(usdt * 1e6), at });
const ntfyMsg = (id, name, amount, time) => ({ event: 'message', id, time, message: JSON.stringify({ n: name, a: amount }) });

// 假的三个外部接口。ntfy 只给 12 小时内的留名(和 ntfy.sh 一样);TronGrid 按 min_timestamp 过滤、新的在前、
// 按 fingerprint 翻页;GitHub 给 stats 的下载计数。xxStatus 不是 200 就回那个状态码。
function fakeNet({ now, ntfy = [], ntfyStatus = 200, tron = [], tronStatus = 200, pageSize = 200, count = 100, ghStatus = 200, asset = true } = {}) {
  const calls = [];
  const reply = (status, body) => new Response(typeof body === 'string' ? body : JSON.stringify(body), { status });
  const fetch = async url => {
    const u = new URL(String(url));
    calls.push(u);
    if (u.host === 'ntfy.sh') {
      assert.equal(u.pathname, `/${TOPIC}/json`);
      if (ntfyStatus !== 200) return reply(ntfyStatus, 'bad gateway');
      return reply(200, ntfy.filter(m => m.time >= now - 12 * H).map(m => JSON.stringify(m) + '\n').join(''));
    }
    if (u.host === 'api.trongrid.io') {
      if (tronStatus !== 200) return reply(tronStatus, 'bad gateway');
      const min = Number(u.searchParams.get('min_timestamp'));
      const all = tron.filter(t => t.block_timestamp >= min).sort((a, b) => b.block_timestamp - a.block_timestamp);
      const from = Number(u.searchParams.get('fingerprint') || 0);
      const meta = from + pageSize < all.length ? { page_size: pageSize, fingerprint: String(from + pageSize) } : { page_size: pageSize };
      return reply(200, { data: all.slice(from, from + pageSize), success: true, meta });
    }
    if (u.host === 'api.github.com') {
      if (ghStatus !== 200) return reply(ghStatus, { message: 'Not Found' });
      return reply(200, { tag_name: 'stats', assets: asset ? [{ name: 'ping', download_count: count }] : [] });
    }
    throw new Error('意外的请求 ' + url);
  };
  return { fetch, calls };
}

const dirs = [];
test.after(() => { for (const d of dirs) fs.rmSync(d, { recursive: true, force: true }); });
function workdir(state, board) {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), 'donors-test-'));
  dirs.push(d);
  if (state) fs.writeFileSync(path.join(d, 'state.json'), JSON.stringify(state, null, 1) + '\n');
  if (board) fs.writeFileSync(path.join(d, 'donors.json'), JSON.stringify(board, null, 1) + '\n');
  return d;
}
const read = (d, f) => JSON.parse(fs.readFileSync(path.join(d, f), 'utf8'));
const quiet = { log() {}, warn() {} };

test('名字规则和面板一致', () => {
  assert.equal(validName(' Larry  Wang '), 'Larry Wang');
  assert.equal(validName('张三'), '张三');
  for (const bad of ['', 'http://a.com', '加我 t.me', '<b>x</b>', 'a'.repeat(25), 12]) assert.equal(validName(bad), '', String(bad));
});

test('留名消息只信 ntfy 的时间,乱写的不要', () => {
  const ok = parsePledge({ event: 'message', id: 'x', time: 100, message: '{"n":"Larry","a":"10.037"}' });
  assert.deepEqual(ok, { id: 'x', name: 'Larry', micros: 10037000, at: 100 });
  for (const m of [{ event: 'open', id: 'x', time: 1 }, { event: 'message', id: 'x', time: 1, message: 'not json' }, { event: 'message', id: 'x', time: 1, message: '{"n":"http://a.com","a":"1"}' }, { event: 'message', id: 'x', time: 1, message: '{"n":"a","a":"0.5"}' }]) assert.equal(parsePledge(m), null);
  assert.equal(toMicros('32.83885'), 32838850);
});

test('面板允许的最大金额(100000 加零头)也能对账', () => {
  // 面板 pledgeMaxUSDT = 100000,payableAmount 再加 0.001–0.099
  assert.equal(parsePledge(ntfyMsg('x', 'Big', '100000.099', 1)).micros, 100000099000);
  assert.equal(parsePledge(ntfyMsg('x', 'Big', '100000.1', 1)).micros, MAX_MICROS);
  assert.equal(parsePledge(ntfyMsg('x', 'Big', '100000.101', 1)), null);
  const r = settle({}, [parsePledge(ntfyMsg('p', 'Big', '100000.099', 1000))], [tx('t', 100000.099, 1100)], 1200);
  assert.equal(r.state.donors.Big, 100000099000);
});

test('60 分钟内金额相同才算,累计到名字上', () => {
  const r = settle({}, [pledge('p1', 'Larry', 10.037, 1000)], [tx('t1', 10.037, 1500)], 2000);
  assert.equal(r.changed, true);
  assert.equal(r.state.donors.Larry, 10037000);
  assert.equal(r.state.pending.length, 0);
  const r2 = settle(r.state, [pledge('p2', 'Larry', 5.001, 3000)], [tx('t1', 10.037, 1500), tx('t2', 5.001, 3100)], 3200);
  assert.equal(r2.state.donors.Larry, 15038000, '同一笔不能算两次,同名累加');
  assert.deepEqual(publicBoard(r2.state, { updatedAt: 9, devices: 7, devicesAt: 8 }), { updatedAt: 9, devices: 7, devicesAt: 8, donors: [{ name: 'Larry', total: 15.038 }] });
});

test('对不上的都不算', () => {
  for (const [name, p, t] of [
    ['金额差一点', pledge('a', 'X', 10.037, 1000), tx('t', 10.038, 1100)],
    ['转账在留名之前', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 900)],
    ['超过 60 分钟', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1000 + 3601)],
    ['转给别的地址', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1100, { to: 'TOther' })],
    ['别的币', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1100, { token_info: { address: 'TFake' } })],
    ['授权不是转账', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1100, { type: 'Approval' })],
  ]) {
    const r = settle({}, [p], [t], 1200);
    assert.equal(r.changed, false, name);
    assert.deepEqual(Object.keys(r.state.donors), [], name);
  }
});

// 名字恰好是 Object.prototype 上的成员名:以前 donors 是普通对象,donors['constructor'] 读到的是继承来的函数,
// 加上金额拼成一串字符,整轮在发榜单时失败(待对账的那条留着,之后每轮都失败,最长约 49 小时);
// donors['__proto__'] = … 被原型的 setter 吞掉,这笔捐赠静默丢失。名字规则允许这些名字,面板也照常显示。
const PROTO_NAMES = ['constructor', 'toString', 'valueOf', 'hasOwnProperty', 'isPrototypeOf', 'propertyIsEnumerable', 'toLocaleString', '__proto__', '__defineGetter__', '__defineSetter__', '__lookupGetter__', '__lookupSetter__'];

test('名字是 constructor、toString、__proto__ 这类原型上的名字也照常记账、累加、上榜', () => {
  for (const name of PROTO_NAMES) {
    assert.equal(validName(name), name, name);
    const r = settle({}, [pledge('p', name, 10.037, 1000)], [tx('t', 10.037, 1100)], 1200);
    assert.equal(r.changed, true, name);
    assert.ok(Object.hasOwn(r.state.donors, name), name);
    assert.equal(r.state.donors[name], 10037000, name);
    const r2 = settle(r.state, [pledge('p2', name, 1.5, 2000)], [tx('t2', 1.5, 2100)], 2200);
    assert.equal(r2.state.donors[name], 11537000, name);
    assert.deepEqual(Object.keys(r2.state.donors), [name], name);
    assert.deepEqual(publicBoard(r2.state, { updatedAt: 1, devices: 0, devicesAt: 0 }).donors, [{ name, total: 11.537 }], name);
  }
});

test('run:这类名字写进 state.json 再读回来也照常累加,和别的名字互不影响', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: { 'Larry Wang': 10000000 } });
  const usdt = (round, i) => round + i / 1000; // 每个名字一个金额,互相对不错
  const micros = (round, i) => Math.round(usdt(round, i) * 1e6);
  const msgs1 = PROTO_NAMES.map((n, i) => ntfyMsg('a' + i, n, usdt(2, i).toFixed(3), now - 1200 + i));
  const txs1 = PROTO_NAMES.map((n, i) => tx('ta' + i, usdt(2, i), now - 600 + i));
  const r1 = await run({ dir: d, fetch: fakeNet({ now, ntfy: msgs1, tron: txs1 }).fetch, now, ...quiet });
  assert.equal(r1.changed, true);
  const t = now + 1800;
  const msgs2 = PROTO_NAMES.map((n, i) => ntfyMsg('b' + i, n, usdt(3, i).toFixed(3), t - 900 + i));
  const txs2 = PROTO_NAMES.map((n, i) => tx('tb' + i, usdt(3, i), t - 300 + i));
  await run({ dir: d, fetch: fakeNet({ now: t, ntfy: [...msgs1, ...msgs2], tron: [...txs1, ...txs2] }).fetch, now: t, ...quiet });
  const s = read(d, 'state.json');
  assert.deepEqual(Object.keys(s.donors).sort(), ['Larry Wang', ...PROTO_NAMES].sort());
  for (const [i, name] of PROTO_NAMES.entries()) assert.equal(s.donors[name], micros(2, i) + micros(3, i), name);
  assert.equal(s.donors['Larry Wang'], 10000000);
  assert.equal(s.pending.length, 0);
  const b = read(d, 'donors.json');
  assert.deepEqual(b.donors.map(x => x.name).sort(), ['Larry Wang', ...PROTO_NAMES].sort());
  assert.equal(b.donors.find(x => x.name === '__proto__').total, (micros(2, 7) + micros(3, 7)) / 1e6);
});

test('同样的金额两个人留名:先到先得,第二笔给第二个人', () => {
  const r = settle({}, [pledge('b', 'B', 10.037, 1100), pledge('a', 'A', 10.037, 1000)], [tx('t1', 10.037, 1200), tx('t2', 10.037, 1300)], 1400);
  assert.equal(r.state.donors.A, 10037000);
  assert.equal(r.state.donors.B, 10037000);
});

test('留名消息反复读到也只算一次,对过账的不会再进待对账', () => {
  const r = settle({}, [pledge('a', 'A', 1.5, 1000)], [tx('t1', 1.5, 1100)], 1200);
  const r2 = settle(r.state, [pledge('a', 'A', 1.5, 1000)], [tx('t1', 1.5, 1100), tx('t2', 1.5, 1150)], 1300);
  assert.equal(r2.state.pending.length, 0);
  assert.equal(r2.state.donors.A, 1500000);
});

test('待对账的留名留 48 小时,屏蔽名单生效', () => {
  let r = settle({}, [pledge('a', 'A', 1.5, 1000)], [], 1000 + 15 * H);
  assert.equal(r.state.pending.length, 1, '隔十几个小时才跑一次,留名还在');
  r = settle(r.state, [], [], 1000 + PENDING_KEEP + 1);
  assert.equal(r.state.pending.length, 0, '48 小时后清掉');
  const r2 = settle({ blocked: ['bad guy'] }, [pledge('b', 'Bad Guy', 1.5, 1000)], [tx('t', 1.5, 1100)], 1200);
  assert.deepEqual(Object.keys(r2.state.donors), []);
});

test('读不到到账(transfers 为 null):新留名照样收下,不对账;没核对过的不按 48 小时清,只清以后也对不上的', () => {
  const now = 1_800_000_000;
  const st = { pending: [pledge('hopeless', 'A', 1.5, now - 51 * H), pledge('old', 'A', 1.5, now - 49 * H)] };
  const r = settle(st, [pledge('new', 'B', 2.5, now - 60)], null, now);
  assert.deepEqual(r.state.pending.map(p => p.id), ['old', 'new'], '到账最多回看 49 小时:51 小时前的留名再也等不到它的到账');
  assert.equal(r.changed, false);
});

test('待对账最多留 2000 条(有人刷留名时 state.json 不会无限变大),留最新的', () => {
  const now = 1_800_000_000;
  const flood = Array.from({ length: 2100 }, (_, i) => pledge('f' + i, 'Spam', 1, now - 2100 + i));
  const r = settle({}, flood, [], now);
  assert.equal(r.state.pending.length, 2000);
  assert.equal(r.state.pending[0].id, 'f100');
});

test('运行中的面板数:下载计数 24 小时涨了多少', () => {
  let r = deviceCount([], 100, 1000);
  assert.equal(r.devices, null, '不到 24 小时先不给(null 是"不知道",不是 0)');
  r = deviceCount(r.points, 150, 1000 + DAY + 10);
  assert.equal(r.devices, 50);
  r = deviceCount(r.points, 160, 1000 + DAY + 20);
  assert.equal(r.points.length, 2, '50 分钟内不重复记点');
  assert.equal(r.devices, 60);
  r = deviceCount(r.points, 160, 1000 + 2 * DAY);
  r = deviceCount(r.points, 160, 1000 + 3 * DAY);
  assert.equal(r.devices, 0, '一天没涨就是真的 0');
  r = deviceCount({ t: 1, n: 2 }, 5, 1000);
  assert.deepEqual([r.points, r.devices], [[{ t: 1000, n: 5 }], null], 'pings 被手改坏了就从头记');
});

// 一群面板各在一天里的随机时刻 ping 一次;运行间隔 6 小时、10 小时(各带 ±30 分钟抖动)。
function mulberry32(a) {
  return () => { a |= 0; a = a + 0x6D2B79F5 | 0; let t = Math.imul(a ^ a >>> 15, 1 | a); t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t; return ((t ^ t >>> 14) >>> 0) / 4294967296; };
}
for (const gap of [6, 10]) {
  test(`运行间隔 ${gap} 小时:满 24 小时后每次都能算出数,和实际台数相差不到 5%`, () => {
    const rnd = mulberry32(gap), N = 1000, t0 = 1_800_000_000;
    const phases = Array.from({ length: N }, () => rnd() * DAY);
    const count = t => phases.reduce((c, p) => c + (t - t0 >= p ? Math.floor((t - t0 - p) / DAY) + 1 : 0), 0);
    let pts = [], checked = 0;
    const first = t0 + DAY; // 这时所有面板都已经 ping 过一次
    for (let t = first; t < first + 6 * DAY; t += gap * H + Math.round((rnd() - 0.5) * H)) {
      const r = deviceCount(pts, count(t), t);
      pts = r.points;
      if (t - first < DAY) continue;
      assert.notEqual(r.devices, null, `第 ${((t - first) / H).toFixed(1)} 小时算不出数`);
      assert.ok(Math.abs(r.devices - N) <= N * 0.05, `第 ${((t - first) / H).toFixed(1)} 小时: ${r.devices},实际 ${N}`);
      checked++;
    }
    assert.ok(checked >= 10);
  });
}

// donors 分支 10-31 到 11-10 的真实下载计数(每次成功运行记的那个点,共 41 次)。
// 旧算法在这上面冻结 11 次(一直显示 22,实际约 29),上线 33.6 小时才出第一个数。
const HISTORY = [[1793441709, 0], [1793461696, 12], [1793475387, 19], [1793486975, 28], [1793497251, 28], [1793520809, 28], [1793546304, 34], [1793562537, 42], [1793574790, 44], [1793587014, 45], [1793613474, 45], [1793650523, 64], [1793672892, 66], [1793699867, 67], [1793727777, 80], [1793745316, 94], [1793760096, 95], [1793814979, 109], [1793835179, 123], [1793850204, 124], [1793878696, 126], [1793905188, 145], [1793923647, 156], [1793947380, 160], [1793975978, 172], [1793997440, 194], [1794010886, 203], [1794032894, 210], [1794058108, 221], [1794075317, 235], [1794088827, 252], [1794101105, 254], [1794124246, 262], [1794149728, 276], [1794166072, 292], [1794179077, 305], [1794191915, 306], [1794219784, 319], [1794257958, 347], [1794281218, 356], [1794310772, 374]];

test('真实历史回放:不再冻结,满 24 小时后的第一次运行就出数', () => {
  let pts = [], shown = null, frozen = 0, firstAt = null;
  const t0 = HISTORY[0][0];
  for (const [t, n] of HISTORY) {
    const r = deviceCount(pts, n, t);
    pts = r.points;
    if (r.devices === null) { if (shown !== null) frozen++; continue; }
    if (firstAt === null) firstAt = t;
    shown = r.devices;
    assert.ok(shown >= 0 && shown <= 80, `${t}: ${shown}`);
  }
  assert.equal(frozen, 0);
  assert.equal(firstAt, HISTORY.find(([t]) => t - t0 >= DAY)[0]);
  assert.ok(firstAt - t0 < 30 * H, '运行间隔 6 小时左右时,满 24 小时后很快出数');
});

test('计数变小(ping 文件被重传):旧点作废、先不出数,24 小时后恢复正确值', () => {
  const t = 1_800_000_000;
  let r = deviceCount([{ t: t - 30 * H, n: 300 }, { t: t - 6 * H, n: 380 }], 391, t);
  assert.ok(r.devices > 0);
  r = deviceCount(r.points, 2, t + 6 * H);
  assert.equal(r.reset, true);
  assert.equal(r.devices, null);
  assert.deepEqual(r.points, [{ t: t + 6 * H, n: 2 }]);
  // 重传后每小时涨 2(一天 48 台),每 6 小时跑一次
  for (let k = 1; k <= 3; k++) {
    r = deviceCount(r.points, 2 + 12 * k, t + 6 * H + 6 * k * H);
    assert.equal(r.devices, null, '还不满 24 小时');
  }
  r = deviceCount(r.points, 2 + 48, t + 30 * H);
  assert.equal(r.devices, 48);
});

test('点留 8 天:停跑 30 小时后恢复,第一次运行就能算', () => {
  const t = 1_800_000_000;
  const r = deviceCount([{ t: t - 40 * H, n: 100 }, { t: t - 30 * H, n: 120 }], 180, t);
  assert.equal(r.devices, 48, '按停跑前后两点的平均速度估');
  assert.equal(deviceCount([{ t: t - 9 * DAY, n: 1 }], 50, t).points.length, 1, '8 天前的点丢掉');
});

test('state.donors 写错单位或类型就报错,不悄悄显示错的数', () => {
  for (const bad of ['50000000', 50.5, 666, -1, null, 1e6 + 0.5]) {
    assert.throws(() => publicBoard({ donors: { maomaocloud: bad } }, {}), /微 USDT/, JSON.stringify(bad));
    assert.throws(() => checkDonors({ maomaocloud: bad }), /微 USDT/);
  }
  assert.throws(() => checkDonors([]), /donors/);
  checkDonors(undefined);
  checkDonors({ '赛博云': 666000000, maomaocloud: 50000000 });
});

test('名字不合规的不上榜(和面板一致),写日志说明', () => {
  const logs = [];
  const b = publicBoard({ donors: { 'Larry Wang': 10e6, 'a.com': 5e6, '  Bob  ': 2e6, '<b>x</b>': 3e6 } }, { updatedAt: 1, devices: 0, devicesAt: 0 }, m => logs.push(m));
  assert.deepEqual(b.donors, [{ name: 'Larry Wang', total: 10 }, { name: 'Bob', total: 2 }]);
  assert.equal(logs.length, 2);
  assert.match(logs.join('\n'), /a\.com/);
});

test('run:三样都读得到时和原来一样,对上账、写榜单、算面板数', async () => {
  const now = 1_800_000_000;
  const d = workdir({ pending: [], seenTx: [], donors: { 'Larry Wang': 10000000 }, donePledges: [], blocked: [], pings: [{ t: now - 25 * H, n: 100 }, { t: now - 20 * H, n: 110 }], devices: 9 });
  const net = fakeNet({ now, ntfy: [ntfyMsg('p1', 'maomaocloud', '5.037', now - 1200)], tron: [tx('t1', 5.037, now - 600)], count: 150 });
  const r = await run({ dir: d, fetch: net.fetch, now, ...quiet });
  assert.deepEqual(r.ok, { ntfy: true, tron: true, stats: true });
  assert.equal(r.changed, true);
  const s = read(d, 'state.json'), b = read(d, 'donors.json');
  assert.equal(s.donors.maomaocloud, 5037000);
  assert.deepEqual(s.seenTx, ['t1']);
  assert.equal(s.devices, 48, '基准 = 24 小时前插值的 102');
  assert.equal(s.devicesAt, now);
  assert.deepEqual(b, { updatedAt: now, devices: 48, devicesAt: now, donors: [{ name: 'Larry Wang', total: 10 }, { name: 'maomaocloud', total: 5.037 }] });
});

test('run:ntfy 读不到,已收进来的留名照常对账,下载计数照常更新并写盘', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: {}, pending: [pledge('p1', 'A', 5.037, now - 600)], pings: [{ t: now - 25 * H, n: 100 }, { t: now - 20 * H, n: 110 }], devices: 7, devicesAt: now - 30 * H });
  const net = fakeNet({ now, ntfyStatus: 502, tron: [tx('t1', 5.037, now - 300)], count: 150 });
  const warns = [];
  const r = await run({ dir: d, fetch: net.fetch, now, log() {}, warn: m => warns.push(m) });
  assert.deepEqual(r.ok, { ntfy: false, tron: true, stats: true });
  assert.match(warns.join('\n'), /ntfy/);
  const s = read(d, 'state.json');
  assert.equal(s.donors.A, 5037000);
  assert.deepEqual(s.pings.at(-1), { t: now, n: 150 });
  assert.equal(s.devices, 48);
  assert.equal(s.devicesAt, now);
  assert.equal(read(d, 'donors.json').devices, 48);
});

test('run:TronGrid 读不到,待对账不清理、新留名照收,面板数照常更新', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: {}, pending: [pledge('old', 'A', 5.037, now - 49 * H)], pings: [{ t: now - 25 * H, n: 100 }], devices: 0 });
  const net = fakeNet({ now, ntfy: [ntfyMsg('new', 'B', '2.011', now - 100)], tronStatus: 502, count: 130 });
  const r = await run({ dir: d, fetch: net.fetch, now, ...quiet });
  assert.deepEqual(r.ok, { ntfy: true, tron: false, stats: true });
  const s = read(d, 'state.json');
  assert.deepEqual(s.pending.map(p => p.id), ['old', 'new'], '超过 48 小时的那条也先留着:它还没核对过');
  assert.equal(s.devicesAt, now);
  // TronGrid 恢复后才按 48 小时清理
  await run({ dir: d, fetch: fakeNet({ now: now + 600, count: 131 }).fetch, now: now + 600, ...quiet });
  assert.deepEqual(read(d, 'state.json').pending.map(p => p.id), ['new']);
});

test('run:两次运行隔 15 小时,空档前收下的留名、空档里到的账,下一次仍能对上', async () => {
  const t0 = 1_800_000_000;
  const msgs = [ntfyMsg('p1', '赛博云', '12.034', t0 - 600)];
  const d = workdir({ donors: {} });
  await run({ dir: d, fetch: fakeNet({ now: t0, ntfy: msgs }).fetch, now: t0, ...quiet });
  assert.equal(read(d, 'state.json').pending.length, 1);
  const transfers = [tx('t1', 12.034, t0 + 1200)]; // 到账在空档里
  const now = t0 + 15 * H; // 这时 ntfy 已经没有那条留名了,到账也早于"12 小时前"
  const net = fakeNet({ now, ntfy: msgs, tron: transfers });
  const r = await run({ dir: d, fetch: net.fetch, now, ...quiet });
  assert.equal(r.changed, true);
  assert.equal(read(d, 'state.json').donors['赛博云'], 12034000);
  const q = net.calls.find(u => u.host === 'api.trongrid.io');
  assert.equal(Number(q.searchParams.get('min_timestamp')), (t0 - 600) * 1000, '回看到最早那条待对账的留名');
});

test('run:到账回看最多 48 小时,翻页取全(被刷零额转账也不会挤掉真到账)', async () => {
  const now = 1_800_000_000;
  const spam = Array.from({ length: 450 }, (_, i) => tx('spam' + i, 0, now - 3 * H + i, { value: '0' }));
  const d = workdir({ donors: {}, pending: [pledge('p1', 'Z', 3.021, now - 5 * H), pledge('junk', 'J', 1, 5)] });
  const net = fakeNet({ now, tron: [...spam, tx('real', 3.021, now - 5 * H + 60)], pageSize: 200 });
  const r = await run({ dir: d, fetch: net.fetch, now, ...quiet });
  assert.equal(r.state.donors.Z, 3021000);
  const qs = net.calls.filter(u => u.host === 'api.trongrid.io');
  assert.equal(qs.length, 3, '451 条要翻 3 页');
  assert.equal(Number(qs[0].searchParams.get('min_timestamp')), (now - PENDING_KEEP - 3600) * 1000, '留名时间再早也只回看 48 小时 + 窗口');
});

test('run:stats 读不到(404)或没有 ping 文件时,面板数和统计时间都不动', async () => {
  const now = 1_800_000_000;
  for (const opts of [{ ghStatus: 404 }, { asset: false }]) {
    const st = { donors: {}, pings: [{ t: now - 30 * H, n: 300 }], devices: 55, devicesAt: now - 6 * H };
    const d = workdir(st);
    const r = await run({ dir: d, fetch: fakeNet({ now, ...opts }).fetch, now, ...quiet });
    assert.equal(r.ok.stats, false);
    const s = read(d, 'state.json');
    assert.equal(s.devices, 55);
    assert.equal(s.devicesAt, now - 6 * H);
    assert.deepEqual(s.pings, st.pings);
    assert.equal(read(d, 'donors.json').devices, 55);
  }
});

test('run:面板数超过 48 小时没算出来就不公开(老版本面板也就不显示了)', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: {}, devices: 55, devicesAt: now - 49 * H });
  await run({ dir: d, fetch: fakeNet({ now, ghStatus: 500 }).fetch, now, ...quiet });
  const b = read(d, 'donors.json');
  assert.equal(b.devices, 0);
  assert.equal(b.devicesAt, now - 49 * H);
  assert.equal(read(d, 'state.json').devices, 55, 'state 里留着上次的值');
});

test('run:计数回退后面板数不出数也不更新统计时间,满 24 小时恢复', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: {}, pings: [{ t: now - 30 * H, n: 300 }, { t: now - 6 * H, n: 380 }], devices: 50, devicesAt: now - 6 * H });
  const warns = [];
  await run({ dir: d, fetch: fakeNet({ now, count: 2 }).fetch, now, log() {}, warn: m => warns.push(m) });
  let s = read(d, 'state.json');
  assert.deepEqual(s.pings, [{ t: now, n: 2 }]);
  assert.equal(s.devicesAt, now - 6 * H, '算不出数时统计时间不动');
  assert.match(warns.join('\n'), /380 → 2/);
  await run({ dir: d, fetch: fakeNet({ now: now + DAY, count: 52 }).fetch, now: now + DAY, ...quiet });
  s = read(d, 'state.json');
  assert.equal(s.devices, 50);
  assert.equal(s.devicesAt, now + DAY);
});

test('run:donors 写错时整次失败,文件不动', async () => {
  const now = 1_800_000_000;
  for (const bad of ['50000000', 666, 50.5]) {
    const st = { donors: { maomaocloud: bad } };
    const d = workdir(st);
    await assert.rejects(run({ dir: d, fetch: fakeNet({ now }).fetch, now, ...quiet }), /微 USDT/);
    assert.deepEqual(read(d, 'state.json'), st);
    assert.equal(fs.existsSync(path.join(d, 'donors.json')), false);
  }
});

test('run:手动改了 donors 也刷新 updatedAt;state 里手动加的其它字段留着', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: { 'Larry Wang': 10000000 }, notes: { manual: '场外到账' } });
  const offline = t => ({ dir: d, fetch: fakeNet({ now: t, ghStatus: 503 }).fetch, now: t, ...quiet }); // 面板数不变,只看名单
  await run(offline(now));
  assert.equal(read(d, 'donors.json').updatedAt, now);
  await run(offline(now + H));
  assert.equal(read(d, 'donors.json').updatedAt, now, '什么都没变就不动');
  const s = read(d, 'state.json');
  s.donors['赛博云'] = 666000000;
  s.donors.maomaocloud = 50000000;
  fs.writeFileSync(path.join(d, 'state.json'), JSON.stringify(s));
  await run(offline(now + 2 * H));
  const b = read(d, 'donors.json');
  assert.equal(b.updatedAt, now + 2 * H);
  assert.deepEqual(b.donors.map(x => [x.name, x.total]), [['赛博云', 666], ['maomaocloud', 50], ['Larry Wang', 10]]);
  assert.deepEqual(read(d, 'state.json').notes, { manual: '场外到账' });
});

test('run:手动记账的名字不合规时不上榜(面板也不会显示),并报警;state 里原样留着,改好名字就上榜', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: { 'Larry Wang': 10000000, 'a.com': 5000000 } });
  const warns = [];
  await run({ dir: d, fetch: fakeNet({ now, ghStatus: 503 }).fetch, now, log() {}, warn: m => warns.push(m) });
  assert.deepEqual(read(d, 'donors.json').donors, [{ name: 'Larry Wang', total: 10 }]);
  assert.match(warns.join('\n'), /a\.com/);
  assert.equal(read(d, 'state.json').donors['a.com'], 5000000);
});

// ---- 数据源多久没读成功(state.sources):一直读不到时运行不能还算成功 ----

test('trackSources:读成功记 okAt(50 分钟内不重复挪,免得每次运行都多一个提交),失败记错误和从什么时候开始失败', () => {
  const t = 1_800_000_000;
  let s = trackSources(undefined, {}, t);
  assert.deepEqual(s, { ntfy: { okAt: t }, tron: { okAt: t }, stats: { okAt: t } });
  s = trackSources(s, {}, t + 600);
  assert.equal(s.ntfy.okAt, t, '50 分钟内不挪');
  s = trackSources(s, {}, t + 3000);
  assert.equal(s.ntfy.okAt, t + 3000);
  s = trackSources(s, { tron: 'https://api.trongrid.io/v1/accounts/X/transactions/trc20?only_to=true&min_timestamp=1 → 429' }, t + 3600);
  assert.deepEqual(s.tron, { okAt: t + 3000, err: 'https://api.trongrid.io/v1/accounts/X/transactions/trc20 → 429', errAt: t + 3600 }, '错误里去掉查询参数(每次都变的话,失败期间每次运行都多一个提交)');
  assert.deepEqual(s.ntfy, { okAt: t + 3000 }, '别的数据源不受影响');
  s = trackSources(s, { tron: 'fetch failed' }, t + 7200);
  assert.deepEqual(s.tron, { okAt: t + 3000, err: 'fetch failed', errAt: t + 3600 }, '一直失败:errAt 是开始失败的时间');
  s = trackSources(s, {}, t + 7300);
  assert.deepEqual(s.tron, { okAt: t + 7300 }, '恢复了马上记');
  assert.deepEqual(trackSources({ stats: { okAt: 'x' } }, { stats: 'boom' }, t).stats, { err: 'boom', errAt: t }, '没有成功过的记录');
  assert.deepEqual(trackSources({ ntfy: { okAt: t + DAY } }, {}, t).ntfy, { okAt: t }, '记录的时间比现在还晚(手改坏了)就按现在记');
});

test('staleSources:还在失败、距上次成功超过 6 小时的数据源', () => {
  const t = 1_800_000_000;
  const src = {
    ntfy: { okAt: t - 7 * H, err: 'x', errAt: t - 6 * H },
    tron: { okAt: t - 5 * H, err: 'y', errAt: t - 4 * H },
    stats: { okAt: t - 9 * H }, // 最近一次是读成功的:不算(多久没运行由 donors.yml 的第一步管)
  };
  assert.deepEqual(staleSources(src, t).map(x => [x.source, x.since]), [['ntfy', t - 7 * H]]);
  assert.deepEqual(staleSources({ stats: { err: 'z', errAt: t - 7 * H } }, t).map(x => [x.source, x.since]), [['stats', t - 7 * H]], '没成功过的,从开始失败算');
  assert.deepEqual(staleSources(undefined, t), []);
  assert.deepEqual(staleSources({ ntfy: 'x', tron: null }, t), []);
});

test('run:每个数据源最近一次读成功的时间和错误记进 state.sources;榜单 donors.json 不变', async () => {
  const now = 1_800_000_000;
  const d = workdir({ donors: {} });
  const r = await run({ dir: d, fetch: fakeNet({ now, tronStatus: 502 }).fetch, now, ...quiet });
  const s = read(d, 'state.json');
  assert.deepEqual(s.sources.ntfy, { okAt: now });
  assert.deepEqual(s.sources.stats, { okAt: now });
  assert.equal(s.sources.tron.errAt, now);
  assert.match(s.sources.tron.err, /502/);
  assert.doesNotMatch(s.sources.tron.err, /min_timestamp/);
  assert.equal(s.sources.tron.okAt, undefined);
  assert.deepEqual(Object.keys(read(d, 'donors.json')).sort(), ['devices', 'devicesAt', 'donors', 'updatedAt']);
  assert.deepEqual(r.problems, [], '才失败一次,不报');
  // ping 文件里的计数不是整数:也算读不到
  const d2 = workdir({ donors: {} });
  const r2 = await run({ dir: d2, fetch: fakeNet({ now, count: '12' }).fetch, now, ...quiet });
  assert.equal(r2.ok.stats, false);
  assert.match(read(d2, 'state.json').sources.stats.err, /计数/);
});

test('run + check:TronGrid 超过 6 小时没读成功,照常保存算出来的状态;check 写进摘要、报错', async () => {
  const now = 1_800_000_000;
  const sources = { ntfy: { okAt: now - 600 }, tron: { okAt: now - 7 * H, err: 'old', errAt: now - 6 * H }, stats: { okAt: now - 600 } };
  const d = workdir({ donors: {}, pending: [pledge('p1', 'A', 5.037, now - 600)], sources });
  const logs = [];
  const r = await run({ dir: d, fetch: fakeNet({ now, ntfy: [ntfyMsg('p2', 'B', '2.011', now - 100)], tronStatus: 503 }).fetch, now, log: m => logs.push(m), warn() {} });
  assert.equal(r.problems.length, 1);
  assert.match(r.problems[0], /TronGrid/);
  assert.match(logs.join('\n'), /TronGrid/);
  const s = read(d, 'state.json');
  assert.deepEqual(s.pending.map(p => p.id), ['p1', 'p2'], '照常收留名、保存');
  assert.deepEqual(s.sources.tron, { okAt: now - 7 * H, err: s.sources.tron.err, errAt: now - 6 * H });
  assert.match(s.sources.tron.err, /503/);
  // donors.yml 最后一步:写进这次运行的摘要,返回问题(命令行据此非零退出)
  const summary = path.join(d, 'summary.md');
  const problems = check({ dir: d, now: now + 60, summary, log() {} });
  assert.equal(problems.length, 1);
  const md = fs.readFileSync(summary, 'utf8');
  assert.match(md, /TronGrid/);
  assert.match(md, /7 小时/);
  assert.match(md, /503/);
  // 恢复之后不再报
  await run({ dir: d, fetch: fakeNet({ now: now + 600 }).fetch, now: now + 600, ...quiet });
  assert.deepEqual(check({ dir: d, now: now + 660, summary: path.join(d, 'ok.md'), log() {} }), []);
  assert.equal(fs.existsSync(path.join(d, 'ok.md')), false);
});

test('命令行 --check:有问题时写摘要、打 ::error:: 并以 1 退出;没问题退出 0(不联网)', () => {
  const now = Math.floor(Date.now() / 1000); // 命令行用真实的时间
  const bad = workdir({ donors: {}, sources: { ntfy: { okAt: now - 13 * H, err: 'fetch failed', errAt: now - 12 * H }, tron: { okAt: now - 60 }, stats: { okAt: now - 60 } } });
  const md = path.join(bad, 'summary.md');
  let cli = spawnSync(process.execPath, [SETTLE, '--check', bad], { env: { ...process.env, GITHUB_ACTIONS: 'true', GITHUB_STEP_SUMMARY: md }, encoding: 'utf8' });
  assert.equal(cli.status, 1, cli.stderr + cli.stdout);
  assert.match(cli.stdout, /::error::.*ntfy/);
  assert.match(fs.readFileSync(md, 'utf8'), /ntfy/);
  const good = workdir({ donors: {}, sources: { ntfy: { okAt: now - 5 * H, err: 'fetch failed', errAt: now - 4 * H }, tron: { okAt: now - 60 }, stats: { okAt: now - 60 } } });
  cli = spawnSync(process.execPath, [SETTLE, '--check', good], { env: { ...process.env, GITHUB_STEP_SUMMARY: path.join(good, 'summary.md') }, encoding: 'utf8' });
  assert.equal(cli.status, 0, cli.stderr + cli.stdout);
  assert.equal(fs.existsSync(path.join(good, 'summary.md')), false);
});

test('donors.yml:先提交算出来的状态,再跑 --check 让运行失败;多久没运行只看有没有跑,不看成没成功', () => {
  const yml = fs.readFileSync(new URL('../../.github/workflows/donors.yml', import.meta.url), 'utf8');
  const push = yml.indexOf('push -q origin HEAD:donors'), chk = yml.indexOf('node deploy/donors/settle.mjs --check donors-data');
  assert.ok(push > 0 && chk > push, '--check 要在推送之后');
  assert.ok(yml.indexOf('--check') > push, '对账那一步里不能带 --check(非零退出会跳过提交)');
  assert.doesNotMatch(yml, /runs\?status=success/);
  assert.match(yml, /runs\?status=completed/);
});

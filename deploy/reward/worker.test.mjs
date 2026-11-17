// 登记服务的测试:node --test deploy/reward/
// D1 用 Node 自带的 sqlite 充当;订单库以只读方式打开(登记服务要是往里写一个字,整套测试就挂);链上节点是模拟的。
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import worker, * as W from './worker.mjs';
import { d1, openDB, COMMERCE_SCHEMA, newChain, b58, USDT_TRC20 } from './devkit.mjs';
import { checkLedger as ledgerProblems, checkGrowth } from './check-ledger.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const SCHEMA = fs.readFileSync(path.join(here, 'schema.sql'), 'utf8');
const { CFG } = W;
const DAY = 86400, TB = 1024;
const T0 = Date.UTC(2026, 11, 1, 4) / 1000; // 2026-12-01 12:00(UTC+8),计划开始后两周
const U = n => Math.round(n * 1e6);

// ---- 地址、密钥 ----
let seq = 0;
function bytesOf(n, first) { // 每次都不一样、但每次运行都一样的字节
  const out = new Uint8Array(n); let x = (++seq) * 2654435761 >>> 0;
  for (let i = 0; i < n; i++) { x = (x * 1664525 + 1013904223) >>> 0; out[i] = x >>> 24; }
  if (first !== undefined) out[0] = first;
  return out;
}
const sol = () => b58(bytesOf(32, 0x8a));
const tron = () => b58(bytesOf(25, 0x41));
const sig = () => b58(bytesOf(64, 0x5a));
async function newPanel() {
  const kp = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']);
  return { pub: Buffer.from(await crypto.subtle.exportKey('raw', kp.publicKey)).toString('base64url'), key: kp.privateKey };
}
async function signed(panel, address, gb, ts) {
  const r = { v: 1, pub: panel.pub, address, gb, ts };
  r.sig = Buffer.from(await crypto.subtle.sign({ name: 'Ed25519' }, panel.key, new TextEncoder().encode(W.canonical(r)))).toString('base64url');
  return r;
}

// ---- 一套环境 ----
function setup(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rw-test-'));
  const file = path.join(dir, 'commerce.db');
  const commerce = openDB(file); commerce.exec(COMMERCE_SCHEMA);
  const readonly = openDB(file, { readOnly: true });
  const db = openDB(); db.exec(SCHEMA);
  const chain = newChain({ sol, sig });
  const calls = { n: 0, max: 0 };
  const env = { DB: d1(db, () => calls.n++), COMMERCE: d1(readonly, () => calls.n++), SALT: 'test-salt', SOLANA_RPC: 'http://rpc.test', TRON_API: 'http://tron.test', FETCH: chain.fetch };
  t.after(() => { db.close(); readonly.close(); commerce.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  const x = {
    env, db, commerce, chain, calls,
    // 跑一轮定时任务,顺便独立数一遍它用了多少个子请求:免费套餐一次调用最多 50 个
    async tick(now) {
      const before = calls.n + chain.fetches;
      const l = await W.tick(env, now);
      const used = calls.n + chain.fetches - before;
      calls.max = Math.max(calls.max, used);
      assert.ok(used <= 50, `这一轮用了 ${used} 个子请求`);
      // 哪一步出了错都要当场看见(除非这条测试就是在测出错:x.allowErrs)
      const errs = JSON.parse(db.prepare("SELECT v FROM kv WHERE k = 'state'").get().v).tick.errs;
      if (!x.allowErrs) assert.deepEqual(errs, {}, '定时任务有一步出错了');
      // 每一轮出的账本都要过提交前的那道核对:写法、自洽、比上一轮只增不减
      if (l) { assert.deepEqual(ledgerProblems(l, now), []); if (x.last) assert.deepEqual(checkGrowth(x.last, l), []); x.last = l; }
      return l;
    },
    solAddr(position = 0) { const a = sol(); commerce.prepare('INSERT INTO solana_payment_addresses VALUES(?,1,?,0,0)').run(a, position); chain.open(a); return a; },
    tronAddr(position = 0) { const a = tron(); commerce.prepare('INSERT INTO payment_addresses VALUES(?,1,?,0,0)').run(a, position); return a; },
    payment({ tx, address, micros, at, from, status = 'credited', reason = null }) {
      commerce.prepare('INSERT INTO payments(tx_id,address,amount_micros,block_timestamp,event_json,status,reason,checked_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)')
        .run(tx, address, String(micros), at * 1000, JSON.stringify(from ? { from, to: address, value: String(micros) } : { network: 'SOLANA' }), status, reason, at * 1000 + 5000, at * 1000);
    },
    reserve(address, micros) { commerce.prepare('INSERT INTO solana_amount_reservations VALUES(?,?,0)').run(address, micros); },
    async report(panel, address, gb, now, ip = '203.0.113.7') {
      const res = await W.handleReport(env, await signed(panel, address, gb, now), ip, now);
      return { status: res.status, body: await res.json() };
    },
    async donate(name, amount, now, ip = '198.51.100.9') { const res = await W.handleDonate(env, { name, amount }, ip, now); return { status: res.status, body: await res.json() }; },
    addr: a => ({ ...db.prepare('SELECT * FROM addresses WHERE address = ?').get(a) }),
    panel: p => ({ ...db.prepare('SELECT * FROM panels WHERE pub = ?').get(p.pub) }),
    deposits: () => db.prepare('SELECT * FROM deposits ORDER BY ts, tx').all().map(r => ({ ...r })),
  };
  return x;
}
// 面板账本校验的移植(reward.Ledger.check):登记服务出的账本,面板必须认
function checkLedger(l, now) {
  assert.equal(l.v, 1);
  assert.ok(l.updatedAt > 0 && l.updatedAt <= now + 600);
  assert.ok(l.orderRate > 0 && l.orderRate <= 1);
  assert.ok(l.pool.total >= 0 && l.pool.points >= 0);
  for (const c of l.chains) { assert.ok(['solana', 'tron'].includes(c.chain)); assert.equal(W.chainOf(c.address), c.chain); }
  if (l.donate.address) assert.equal(W.chainOf(l.donate.address), l.donate.chain);
  for (const d of l.deposits) { assert.ok(['order', 'donation'].includes(d.kind)); assert.ok(d.amount >= 0 && d.from.length <= 64 && d.tx.length <= 128 && [...(d.name || '')].length <= 24); }
  for (const b of l.board) { assert.ok(W.validSolana(b.address) && b.points >= 0 && [...(b.reason || '')].length <= 200); }
}

// ================= 基础 =================
test('地址、来源、金额这些小工具', () => {
  assert.ok(W.validSolana('Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB'));
  assert.ok(W.validSolana('11111111111111111111111111111111'));
  assert.ok(!W.validSolana('Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNY0'), '0 不是 base58');
  assert.ok(!W.validSolana('TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t'), 'TRON 地址解出来是 25 字节');
  assert.ok(W.validTron('TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t'));
  assert.equal(W.chainOf('TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm'), 'tron');
  assert.equal(W.chainOf('hello'), '');
  for (let i = 0; i < 20; i++) { assert.ok(W.validSolana(sol())); assert.ok(W.validTron(tron())); }

  assert.equal(W.srcKey('203.0.113.7'), '203.0.113.7');
  assert.equal(W.srcKey('::ffff:203.0.113.7'), '203.0.113.7', 'IPv4 映射地址按 IPv4 算');
  assert.equal(W.srcKey('2001:db8:12:3400:abcd::1'), '2001:db8:12::/48');
  assert.equal(W.srcKey('2001:0db8:0012:ffff:1:2:3:4'), '2001:db8:12::/48', '同一个 /48 是同一处来源');
  assert.equal(W.srcKey('2001:db8::1'), '2001:db8:0::/48');
  assert.equal(W.srcKey('::1'), '0:0:0::/48');
  assert.equal(W.srcKey('fe80::1%eth0'), 'fe80:0:0::/48');
  for (const bad of ['', 'abc', '1.2.3', '1.2.3.999', '1:2:3', '2001:db8:::1', 'g::1', '1:2:3:4:5:6:7:8:9']) assert.equal(W.srcKey(bad), '', bad);

  assert.equal(W.micros(10), 10_000_000);
  assert.equal(W.micros('10.03'), 10_030_000);
  assert.equal(W.micros(0.07), 70_000);
  assert.ok(Number.isNaN(W.micros('abc')) && Number.isNaN(W.micros(null)) && Number.isNaN(W.micros('1e3')));
  assert.equal(W.usd(11_992_500), 11.99);
  assert.equal(W.dayOf(Date.UTC(2026, 11, 1, 15, 59, 59) / 1000) + 1, W.dayOf(Date.UTC(2026, 11, 1, 16) / 1000), '按 UTC+8 的零点换日');

  assert.deepEqual(W.cleanName('  老 王\t的  猫 '), { name: '老 王 的 猫' });
  assert.ok(W.cleanName('').err && W.cleanName('   ').err);
  assert.ok(W.cleanName('a'.repeat(21)).err);
  assert.ok(W.cleanName('看 https://x.y').err && W.cleanName('WWW.x.y').err);
  assert.ok(W.cleanName('a‮b').err && W.cleanName('a\u0007b').err);
  assert.deepEqual(W.cleanName('🐱'.repeat(20)), { name: '🐱'.repeat(20) }, '按字数算,不按字节');
});

test('上报的签名:改一个字都不认', async () => {
  const p = await newPanel(), a = sol();
  const r = await signed(p, a, 1234, T0);
  assert.ok(await W.verifyReport(r));
  assert.ok(!await W.verifyReport({ ...r, gb: 1235 }));
  assert.ok(!await W.verifyReport({ ...r, ts: T0 + 1 }));
  assert.ok(!await W.verifyReport({ ...r, address: sol() }));
  assert.ok(!await W.verifyReport({ ...r, pub: (await newPanel()).pub }));
  assert.ok(!await W.verifyReport({ ...r, sig: 'AAAA' }));
  assert.ok(!await W.verifyReport({ ...r, pub: '***' }));
  assert.equal(W.canonical(r), `m-ui-reward-v1\n${p.pub}\n${a}\n1234\n${T0}`);
});

// ================= 上报 =================
test('上报:登记、只增不减、报得太勤只回现状、格式和时间不对的不收', async t => {
  const x = setup(t), p = await newPanel(), a = sol();
  let r = await x.report(p, a, 300, T0);
  assert.deepEqual(r, { status: 200, body: { gb: 300, credited: 0 } });
  assert.equal(x.panel(p).counted_gb, 300);
  assert.equal(x.panel(p).ip_hash.length, 24);
  assert.ok(!JSON.stringify(x.db.prepare('SELECT * FROM panels').all()).includes('203.0.113'), '来源只存哈希');

  r = await x.report(p, a, 900, T0 + 60);
  assert.equal(r.body.gb, 300, '4 分钟内再报:不处理');
  r = await x.report(p, a, 900, T0 + 1800);
  assert.equal(r.body.gb, 900);
  assert.equal(x.panel(p).counted_gb, 834, '半小时最多算 25 TB/天 × 1/48 = 534 GB');

  r = await x.report(p, a, 500, T0 + 3600);
  assert.equal(r.body.gb, 900, '报上来的比记着的小(接任 / 还原):把记着的回给它');
  assert.equal(x.panel(p).gb, 900);

  assert.equal((await x.report(p, a, 901, T0 + 99999)).status, 200);
  const stale = await W.handleReport(x.env, await signed(p, a, 950, T0), '203.0.113.7', T0 + 5000);
  assert.equal(stale.status, 400, '上报里的时间和现在差太多');
  const bad = await signed(p, a, 950, T0 + 9000);
  assert.equal((await W.handleReport(x.env, { ...bad, gb: 99999 }, '203.0.113.7', T0 + 9000)).status, 400, '签名不对');
  assert.equal((await W.handleReport(x.env, { ...bad, v: 2 }, '203.0.113.7', T0 + 9000)).status, 400);
  assert.equal((await W.handleReport(x.env, { ...bad, gb: -1 }, '203.0.113.7', T0 + 9000)).status, 400);
  assert.equal((await W.handleReport(x.env, { ...bad, gb: 1.5 }, '203.0.113.7', T0 + 9000)).status, 400);
  assert.equal((await W.handleReport(x.env, { ...bad, address: 'nope' }, '203.0.113.7', T0 + 9000)).status, 400);
  assert.equal((await W.handleReport(x.env, null, '203.0.113.7', T0 + 9000)).status, 400);
});

test('上报:头一次能带进来的历史累计有上限', async t => {
  const x = setup(t), p = await newPanel(), a = sol();
  const days = Math.ceil((T0 - CFG.programStart) / DAY);
  await x.report(p, a, 5_000_000, T0);
  assert.equal(x.panel(p).gb, 5_000_000);
  assert.equal(x.panel(p).counted_gb, days * CFG.capGbPerDay, '计划开始以来的天数 × 每天 25 TB');
});

test('上报:没有新流量的不白写库,但隔一小时、换了来源、过了一天要记一笔', async t => {
  const x = setup(t), p = await newPanel(), a = sol();
  await x.report(p, a, 100, T0);
  await x.report(p, a, 100, T0 + 1800);
  assert.equal(x.panel(p).last_seen, T0, '半小时后没有新流量:不写');
  await x.report(p, a, 100, T0 + 3700);
  assert.equal(x.panel(p).last_seen, T0 + 3700);
  assert.equal(x.panel(p).src_since, T0);
  const before = x.panel(p).ip_hash;
  await x.report(p, a, 100, T0 + 3700 + 600, '203.0.113.99');
  assert.notEqual(x.panel(p).ip_hash, before, '换了来源:马上记');
  assert.equal(x.panel(p).src_since, T0 + 3700 + 600, '到新来源的时间从这一刻算');
  await x.report(p, a, 100, T0 + DAY, '203.0.113.99');
  assert.equal(x.db.prepare('SELECT COUNT(*) AS n FROM samples WHERE pub = ?').get(p.pub).n, 2, '每天一条');
});

test('上报:一处来源一天新登记的面板有上限', async t => {
  const x = setup(t), a = sol();
  for (let i = 0; i < CFG.newPanelsPerSrcDay; i++) assert.equal((await x.report(await newPanel(), a, 1, T0 + i)).status, 200);
  const r = await x.report(await newPanel(), a, 1, T0 + 10);
  assert.equal(r.status, 429);
  assert.match(r.body.error, /太多/);
  assert.equal(r.body.code, 'busy_src');
  assert.equal((await x.report(await newPanel(), a, 1, T0 + DAY + 20)).status, 200, '过了一天又可以');
  assert.equal((await x.report(await newPanel(), a, 1, T0 + 30, '203.0.113.200')).status, 200, '别的来源不受影响');
});

// ================= 入账 =================
test('入账:每个地址每天最多 1 积分,多出来的顺延;几台面板合在一个地址下也是每天 1 分', async t => {
  const x = setup(t), p1 = await newPanel(), p2 = await newPanel(), a = sol();
  await x.report(p1, a, 2000, T0);
  await x.report(p2, a, 1200, T0 + 5); // 合计 3200 GB = 3 个积分
  assert.equal(await W.creditPoints(x.env.DB, T0 + 10), 1);
  assert.equal(await W.creditPoints(x.env.DB, T0 + 3600), 0, '同一天不再入');
  assert.equal(x.addr(a).credited, 1);
  for (let d = 1; d <= 4; d++) { await x.report(p1, a, 2000, T0 + d * DAY); await W.creditPoints(x.env.DB, T0 + d * DAY + 60); }
  assert.equal(x.addr(a).credited, 3, '挣到 3 个就入 3 个,不多入');
  assert.equal((await x.report(p1, a, 2000, T0 + 4 * DAY + 600)).body.credited, 3, '面板问得到自己地址已入账多少');
});

test('入账:不到 1 TB 不入;刚好 1 TB 入一个', async t => {
  const x = setup(t), p = await newPanel(), a = sol();
  await x.report(p, a, 1023, T0);
  await W.creditPoints(x.env.DB, T0 + 10);
  assert.equal(x.addr(a).credited, 0);
  await x.report(p, a, 1024, T0 + 3600);
  await W.creditPoints(x.env.DB, T0 + 3700);
  assert.equal(x.addr(a).credited, 1);
});

test('入账:当天要有面板在上报。面板不出现的日子不入,回来之后接着入(攒着的不作废)', async t => {
  const x = setup(t), p = await newPanel(), a = sol();
  await x.report(p, a, 5000, T0); // 4 个积分
  await W.creditPoints(x.env.DB, T0 + 60);
  for (let d = 1; d <= 5; d++) await W.creditPoints(x.env.DB, T0 + d * DAY + 3600); // 面板停了 5 天
  assert.equal(x.addr(a).credited, 1, '光登记一大笔历史累计、然后不再出现,拿不到后面的积分');
  await x.report(p, a, 5000, T0 + 6 * DAY);
  await W.creditPoints(x.env.DB, T0 + 6 * DAY + 60);
  assert.equal(x.addr(a).credited, 2);
});

test('换地址:冷却期内不让换;换了之后只有新增的流量算到新地址,原地址的积分不动', async t => {
  const x = setup(t), p = await newPanel(), a = sol(), b = sol();
  await x.report(p, a, 2100, T0);
  await W.creditPoints(x.env.DB, T0 + 10);
  let r = await x.report(p, b, 2100, T0 + DAY);
  assert.equal(r.status, 400);
  assert.match(r.body.error, /冷却期,6 天后/);
  assert.deepEqual([r.body.code, r.body.days], ['cooldown', 6]);
  assert.equal(x.panel(p).address, a);

  const t1 = T0 + 7 * DAY + 60;
  r = await x.report(p, b, 2100 + 500, t1);
  assert.equal(r.status, 200);
  assert.equal(x.panel(p).address, b);
  assert.equal(x.panel(p).base_gb, 2100, '换绑那一刻之前的不算到新地址');
  assert.equal(x.addr(a).settled_gb, 2100, '留给原地址');
  await W.creditPoints(x.env.DB, t1 + 10);
  assert.equal(x.addr(a).credited, 1, '原地址已入账的留着;名下没有面板了,没入账的那 1 分不再入');
  assert.equal(x.addr(b).credited, 0, '新地址只有 500 GB');
  await x.report(p, b, 2100 + 1100, t1 + DAY);
  await W.creditPoints(x.env.DB, t1 + DAY + 10);
  assert.equal(x.addr(b).credited, 1);
  assert.equal((await x.report(p, a, 3300, t1 + 2 * DAY)).status, 400, '换回去同样有冷却期');
  const l = await W.buildExport(x.env, t1 + 2 * DAY);
  assert.deepEqual(l.board.map(r2 => [r2.address, r2.points, r2.panels]).sort(), [[a, 1, 0], [b, 1, 1]].sort());
  assert.equal(l.pool.points, 2);
});

// ================= 自动识别作弊 =================
test('一处来源只认一个地址:后到的那台面板当场不计入、带进来的历史作废,答复里写明原因;先到的照常', async t => {
  const x = setup(t), p1 = await newPanel(), p2 = await newPanel(), a = sol(), b = sol();
  await x.report(p1, a, 1500, T0);
  const r = await x.report(p2, b, 3000, T0 + 600);
  assert.deepEqual(r.body, { gb: 3000, credited: 0, excluded: { rule: 'same_src', reason: W.RULES.same_src, at: T0 + 600 } }, '登记时就知道');
  assert.equal(x.panel(p1).excluded, 0, '先到的不受影响');
  assert.deepEqual([x.panel(p2).excluded, x.panel(p2).rule, x.panel(p2).base_gb], [1, 'same_src', 3000]);
  await x.tick(T0 + 700);
  assert.equal(x.addr(a).credited, 1);
  assert.equal(x.addr(b).credited, 0);

  const l = await W.buildExport(x.env, T0 + 4000);
  checkLedger(l, T0 + 4000);
  assert.deepEqual(l.board, [{ address: a, points: 1, panels: 1 }, { address: b, points: 0, panels: 1, excluded: true, rule: 'same_src', reason: W.RULES.same_src }]);
  assert.deepEqual([l.pool.points, l.pool.addresses], [1, 1]);
  assert.deepEqual(l.events.map(e => ({ ...e })), [{ ts: T0 + 600, address: b, panel: p2.pub.slice(0, 8), kind: 'excluded', rule: 'same_src', reason: W.RULES.same_src }]);

  // 不计入期间:累计值跟着记,但不算数
  await x.report(p2, b, 9000, T0 + DAY);
  assert.deepEqual([x.panel(p2).gb, x.panel(p2).counted_gb - x.panel(p2).base_gb], [9000, 0]);

  // 先到的那台不再出现,后到的接着用:3 天后不再命中,再过 14 天自动恢复;恢复之后新跑的才算
  let gb = 9000;
  for (let d = 2; d <= 20; d++) {
    const now = T0 + d * DAY;
    gb += 200 + (d * d * 37) % 90; // 每天涨的量不一样(真流量的样子),免得撞上「每天一模一样」那条
    await x.report(p2, b, gb, now);
    await x.tick(now + 60);
    if (d === 15) assert.equal(x.panel(p2).excluded, 1, '最后一次命中是第 2 天,到第 15 天还不满 14 天:不恢复');
  }
  assert.equal(x.panel(p2).excluded, 0, '自动恢复');
  assert.deepEqual(x.db.prepare('SELECT kind, rule FROM events ORDER BY id').all().map(e => ({ ...e })), [{ kind: 'excluded', rule: 'same_src' }, { kind: 'restored', rule: 'same_src' }]);
  assert.equal(x.addr(b).credited, 0, '不计入期间报的流量不补记');
  const counted = x.panel(p2).counted_gb - x.panel(p2).base_gb;
  assert.ok(counted > 0 && counted < 1024, `恢复后几天的新流量开始算了(${counted} GB)`);
  await x.report(p2, b, gb + 1100, T0 + 21 * DAY);
  await x.tick(T0 + 21 * DAY + 60);
  assert.equal(x.addr(b).credited, 1);
});

test('一处来源:先后用(不同时)的两个地址不算;早就登记、后来搬到别人来源上的面板,以前在别处跑的照旧算', async t => {
  const x = setup(t), p1 = await newPanel(), p2 = await newPanel(), a = sol(), b = sol();
  await x.report(p1, a, 100, T0);
  const r = await x.report(p2, b, 100, T0 + 4 * DAY); // 重装了面板、换了地址:前一台已经 4 天没出现
  assert.equal(r.body.excluded, undefined);
  assert.equal((await W.findCheats(x.env.DB, T0 + 4 * DAY + 60)).size, 0);

  // p3 在自己的机器上跑了 5 天,挣了 2 分;然后搬到 p2 那处来源
  const p3 = await newPanel(), c = sol();
  await x.report(p3, c, 2500, T0, '203.0.114.1');
  for (let d = 0; d <= 4; d++) { await x.report(p3, c, 2500, T0 + d * DAY + 300, '203.0.114.1'); await x.report(p2, b, 100, T0 + 4 * DAY + d * DAY + 30); await x.tick(T0 + d * DAY + 600); }
  assert.equal(x.addr(c).credited, 2);
  const moved = await x.report(p3, c, 2600, T0 + 5 * DAY, '203.0.113.7');
  assert.equal(moved.body.excluded.reason, W.RULES.same_src, '后到的不计入');
  assert.equal(moved.body.credited, 2, '以前挣的留着');
  assert.equal(x.panel(p3).base_gb, 0, '早就登记的面板:以前的贡献不作废');
  assert.equal(x.db.prepare("SELECT COUNT(*) AS n FROM events WHERE kind = 'voided'").get().n, 0);
  // 名下的面板全被剔除着的地址:积分留着,但剔除期间不参加分配(不算进合计);恢复之后照旧
  const l = await x.tick(T0 + 5 * DAY + 60);
  assert.deepEqual(l.board.find(r2 => r2.address === c), { address: c, points: 2, panels: 1, excluded: true, rule: 'same_src', reason: W.RULES.same_src });
  assert.deepEqual([l.pool.points, l.pool.addresses], [0, 0], '这里只有它有积分,它被剔除着:合计是 0');
  // 它搬回自己的机器:不再命中,14 天后自动恢复,那 2 分又算进合计
  let back = null;
  for (let d = 6; d <= 21; d++) {
    await x.report(p3, c, 2600 + (d - 5) * 40 + (d * 29) % 30, T0 + d * DAY, '203.0.114.1');
    await x.report(p2, b, 100, T0 + d * DAY + 30);
    back = await x.tick(T0 + d * DAY + 60);
  }
  assert.equal(x.panel(p3).excluded, 0);
  assert.deepEqual(back.board.find(r2 => r2.address === c), { address: c, points: 2, panels: 1 });
  assert.deepEqual([back.pool.points, back.pool.addresses], [2, 1]);
});

test('只有 IPv6 的机器:同一个 /48 算同一处来源(一条隧道就有 65536 个 /64,不能各算各的)', async t => {
  const x = setup(t), p1 = await newPanel(), p2 = await newPanel(), p3 = await newPanel(), a = sol(), b = sol(), c = sol();
  await x.report(p1, a, 100, T0, '2001:db8:1:2:aaaa::1');
  const same = await x.report(p2, b, 100, T0 + 60, '2001:db8:1:ffff::2');
  assert.equal(same.body.excluded.reason, W.RULES.same_src);
  const other = await x.report(p3, c, 100, T0 + 120, '2001:db8:2:1::1');
  assert.equal(other.body.excluded, undefined, '另一个 /48 是另一处来源');
});

test('冒用别人的地址害不到人:把受害者的地址绑到作弊的面板上,被判的只是那台面板', async t => {
  const x = setup(t), victim = await newPanel(), V = sol();
  // 受害者:自己的机器,正常挣了 2 分
  for (let d = 0; d < 2; d++) { await x.report(victim, V, 1100 * (d + 1), T0 + d * DAY, '203.0.114.1'); await x.tick(T0 + d * DAY + 60); }
  assert.equal(x.addr(V).credited, 2);

  // 攻击者在自己的来源上先挂了自己的地址 E,再登记一台绑着 V 的面板
  const e1 = await newPanel(), fake = await newPanel(), E = sol();
  await x.report(e1, E, 10, T0 + 2 * DAY, '198.51.100.66');
  const r = await x.report(fake, V, 50_000, T0 + 2 * DAY + 60, '198.51.100.66');
  assert.equal(r.body.excluded.reason, W.RULES.same_src, '被判的是攻击者那台');
  await x.report(victim, V, 3400, T0 + 2 * DAY + 120, '203.0.114.1');
  await x.tick(T0 + 2 * DAY + 180);
  assert.equal(x.panel(victim).excluded, 0);
  assert.equal(x.addr(V).credited, 3, '受害者照常入账');
  let l = await W.buildExport(x.env, T0 + 2 * DAY + 200);
  assert.deepEqual(l.board.find(b => b.address === V), { address: V, points: 3, panels: 2 }, '榜上也不会把这个地址标成不计入');

  // 攻击者换个办法:让那台绑着 V 的面板「每天涨得一模一样」。它带进来的都作废,受害者自己挣的一分不少
  const robot = await newPanel();
  let gb = 0;
  for (let d = 3; d <= 19; d++) {
    gb += 2048;
    await x.report(robot, V, gb, T0 + d * DAY, '198.51.100.77');
    await x.report(victim, V, 3400 + (d - 2) * 300 + (d * 53) % 70, T0 + d * DAY + 30, '203.0.114.1');
    await x.tick(T0 + d * DAY + 60);
  }
  assert.deepEqual([x.panel(robot).excluded, x.panel(robot).rule], [1, 'robot']);
  assert.equal(x.panel(robot).counted_gb - x.panel(robot).base_gb, 0, '造假那台报过的全部作废');
  const own = Math.floor((x.panel(victim).counted_gb - x.panel(victim).base_gb) / 1024);
  assert.equal(x.addr(V).credited, own, '作废掉的只是攻击者送的那部分,受害者留下的正好是自己流量挣到的');
  assert.ok(own >= 8);
  assert.equal(x.panel(victim).excluded, 0);
  l = await W.buildExport(x.env, T0 + 20 * DAY);
  assert.equal(l.board.find(b => b.address === V).excluded, undefined);
  assert.match(l.events.find(e => e.kind === 'voided').reason, /^作废 \d+ 积分$/);
});

test('拿一个老地址来「抢位置」没有用:一处来源认的是先在这里上报的,不是地址登记得早的', async t => {
  const x = setup(t), old = await newPanel(), O = sol(), honest = await newPanel(), H = sol(), attacker = await newPanel();
  await x.report(old, O, 100, T0 - 30 * DAY, '203.0.114.1');   // O 是个很早就登记的地址(榜上谁都看得到)
  await x.report(honest, H, 100, T0, '198.51.100.20');          // H 在共用出口的机器上,先到
  const r = await x.report(attacker, O, 100, T0 + 3600, '198.51.100.20'); // 攻击者在同一个出口上绑 O
  assert.equal(r.body.excluded.reason, W.RULES.same_src);
  assert.equal(x.panel(honest).excluded, 0);
  assert.equal((await W.findCheats(x.env.DB, T0 + 7200)).get(honest.pub), undefined);
});

test('不变量:一处来源每天最多带来 1 个积分,不管挂多少个地址、多少台面板、怎么轮换', async t => {
  const x = setup(t), list = [];
  for (let i = 0; i < 5; i++) list.push({ p: await newPanel(), a: sol(), gb: 0 });
  const total = () => x.db.prepare('SELECT COALESCE(SUM(credited), 0) AS n FROM addresses').get().n;
  for (let d = 0; d < 12; d++) {
    const now = T0 + d * DAY, before = total();
    for (const [i, it] of list.entries()) {
      if (d % 5 === i) continue;                                // 每天歇一台,轮着来
      it.gb += 3000 + i * 111 + (d * 97) % 500;
      await x.report(it.p, it.a, it.gb, now + i * 400);
    }
    await x.tick(now + 3600);
    await x.tick(now + 7200);
    assert.ok(total() - before <= 1, `第 ${d} 天入账了 ${total() - before} 个`);
  }
  assert.ok(total() >= 6 && total() <= 12);
});

// 连着 n 天每天报一次,每天涨 steps[i](循环用)
async function daily(x, panel, address, ip, start, days, steps, from = 0) {
  let gb = from;
  for (let d = 0; d < days; d++) { gb += steps[d % steps.length]; const r = await x.report(panel, address, gb, start + d * DAY, ip); assert.equal(r.status, 200); }
  return gb;
}
test('每天涨得一模一样:14 天后自动判为不计入,已经入账的作废;真流量(每天不一样)和小流量不算', async t => {
  const x = setup(t), fake = await newPanel(), real = await newPanel(), small = await newPanel(), fa = sol(), ra = sol(), sa = sol();
  const real14 = [900, 1310, 1100, 760, 1500, 980, 1210, 870, 1400, 1020, 940, 1330, 800, 1150, 1260, 990];
  for (let d = 0; d < 16; d++) {
    await x.report(fake, fa, 1024 * (d + 1), T0 + d * DAY, '203.0.113.1');
    await x.report(real, ra, real14.slice(0, d + 1).reduce((s, v) => s + v, 0), T0 + d * DAY + 10, '203.0.114.1');
    await x.report(small, sa, 3 * (d + 1), T0 + d * DAY + 20, '203.0.115.1');
    if (d === 13) assert.equal((await W.findCheats(x.env.DB, T0 + d * DAY + 60)).size, 0, '不满 14 天不判');
    if (d < 15) await W.creditPoints(x.env.DB, T0 + d * DAY + 90);
  }
  assert.equal(x.addr(fa).credited, 15);
  assert.deepEqual([...await W.findCheats(x.env.DB, T0 + 15 * DAY + 60)], [[fake.pub, 'robot']]);
  assert.deepEqual(await W.judge(x.env.DB, T0 + 15 * DAY + 60), { hit: 1, excluded: 1, restored: 0 });
  assert.equal(x.addr(fa).credited, 0, '靠造假入账的积分全部作废');
  assert.ok(x.addr(ra).credited >= 14, '真流量的不受影响');
  const l = await W.buildExport(x.env, T0 + 15 * DAY + 100);
  assert.deepEqual(l.board.find(b => b.address === fa), { address: fa, points: 0, panels: 1, excluded: true, rule: 'robot', reason: W.RULES.robot });
  assert.deepEqual(l.events.map(e => [e.kind, e.address, e.reason]), [['voided', fa, '作废 15 积分'], ['excluded', fa, W.RULES.robot]]);

  assert.ok(W.tooRegular(Array(14).fill(1024)));
  assert.ok(W.tooRegular([1024, 1025, 1024, 1024, 1025, 1024, 1024, 1024, 1025, 1024, 1024, 1024, 1024, 1025]), '差 1 GB 以内也算一模一样');
  assert.ok(!W.tooRegular([1080, 1075, 1083, 1079, 1071, 1085, 1080, 1077, 1082, 1079, 1074, 1081, 1078, 1086]), '跑满带宽的线路每天也会差出几个 GB:不算');
  assert.ok(!W.tooRegular(Array(14).fill(10)), '很小的面板不判');
});

test('两台面板的流量曲线一模一样(同一份流量登记了两次):后登记的那台不计入、全部作废;各跑各的不算', async t => {
  const x = setup(t), p1 = await newPanel(), p2 = await newPanel(), p3 = await newPanel(), a = sol(), b = sol(), c = sol();
  const curve = [1420, 1610, 1380, 1905, 1540, 1770, 1660, 1495, 1830];
  await x.report(p1, a, 0, T0 - 3600, '203.0.113.1');            // p1 先登记
  for (let d = 0; d < 9; d++) {
    const upto = curve.slice(0, d + 1).reduce((s, v) => s + v, 0);
    await x.report(p1, a, upto, T0 + d * DAY, '203.0.113.1');
    await x.report(p2, b, 7000 + upto, T0 + d * DAY + 300, '203.0.114.1'); // 复制过去的:累计值不同,但每天涨的一样
    await x.report(p3, c, Math.round(upto * 1.03) + d * 17, T0 + d * DAY + 600, '203.0.115.1'); // 另一台真面板:量级相近但不一样
    if (d < 8) await W.creditPoints(x.env.DB, T0 + d * DAY + 900);
  }
  assert.equal(x.addr(b).credited, 8);
  assert.deepEqual([...await W.findCheats(x.env.DB, T0 + 8 * DAY + 3600)], [[p2.pub, 'twins']]);
  await W.judge(x.env.DB, T0 + 8 * DAY + 3600);
  assert.deepEqual([x.panel(p1).excluded, x.panel(p2).excluded, x.panel(p3).excluded], [0, 1, 0]);
  assert.equal(x.addr(b).credited, 0, '重复登记那台挣的全部作废');
  assert.equal(x.addr(a).credited, 8);
  assert.ok(W.sameCurve([420, 610, 380], [421, 609, 380]));
  assert.ok(!W.sameCurve([420, 610, 380], [420, 640, 380]));
});

// ================= 奖池:订单 =================
test('订单:只认订单库里「监听成功」的付款(两条链),按 40% 计入;只加不删;订单库一个字都不写', async t => {
  const x = setup(t), solA = x.solAddr(), tronA = x.tronAddr();
  const buyer = tron(), paid = x.chain.pay(solA, 19_990_013, T0 - 5000);
  x.payment({ tx: 'a'.repeat(64), address: tronA, micros: 9_991_237, at: T0 - 9000, from: buyer });
  x.payment({ tx: 'solana:' + paid.signature, address: solA, micros: 19_990_013, at: T0 - 5000 });
  x.payment({ tx: 'b'.repeat(64), address: tronA, micros: 5_000_000, at: T0 - 4000, from: buyer, status: 'review', reason: 'UNMATCHED_AMOUNT' });
  x.payment({ tx: 'c'.repeat(64), address: tronA, micros: 9_991_555, at: T0 - 3000, from: buyer, status: 'observed' });
  x.payment({ tx: 'solana:' + sig(), address: solA, micros: 9_990_021, at: T0 - 2000, status: 'review', reason: 'LATE_PAYMENT' });

  const l = await x.tick(T0);
  checkLedger(l, T0);
  assert.deepEqual(l.pool, { total: 11.99, orderCount: 2, orderAmount: 29.98, orderShare: 11.99, donateCount: 0, donateAmount: 0, points: 0, addresses: 0 });
  assert.equal(l.orderRate, 0.4);
  assert.equal(l.payoutAt, Date.UTC(2027, 8, 2, 16) / 1000);
  assert.deepEqual(l.deposits.map(d => [d.kind, d.chain, d.amount, d.tx, d.from]), [
    ['order', 'solana', 19.990013, paid.signature, paid.payer],          // 交易号不带 solana: 前缀;付款方是去链上补的
    ['order', 'tron', 9.991237, 'a'.repeat(64), buyer],
  ]);
  assert.deepEqual(l.chains, [
    { chain: 'solana', address: solA, received: 19.99 },
    { chain: 'tron', address: tronA, received: 9.99 },
  ]);
  assert.deepEqual(l.donate, { chain: 'solana', address: solA, min: 1, max: 100000 });

  // 再跑不重复;订单库里那条记录没了(订单被永久删除),账本上照样留着:钱确实到过
  await x.tick(T0 + 120);
  x.commerce.prepare('DELETE FROM payments WHERE tx_id = ?').run('a'.repeat(64));
  x.payment({ tx: 'd'.repeat(64), address: tronA, micros: 30_000_007, at: T0 + 200, from: buyer });
  const l2 = await x.tick(T0 + 2 * DAY);
  assert.equal(l2.pool.orderCount, 3);
  assert.equal(l2.pool.orderAmount, 59.98);
  assert.equal(l2.pool.total, 23.99);
  assert.equal(x.commerce.prepare('SELECT COUNT(*) AS n FROM payments').get().n, 5);
});

test('登记服务对订单库只有 SELECT(读源码逐处核对)', () => {
  const src = fs.readFileSync(path.join(here, 'worker.mjs'), 'utf8');
  const uses = [...src.matchAll(/env\.COMMERCE\s*,\s*[`'"]\s*(\w+)/g)].map(m => m[1]);
  assert.ok(uses.length >= 5, '找到的调用太少,这条测试的写法该更新了');
  assert.deepEqual([...new Set(uses)], ['SELECT']);
  // 除了带 SQL 的查询,只有一处提到订单库:给它套上计数的那一行(metered)
  const others = [...src.matchAll(/^.*env\.COMMERCE(?!\s*,\s*[`'"]).*$/gm)].map(m => m[0].trim());
  assert.equal(others.length, 1, others.join('\n'));
  assert.match(others[0], /COMMERCE: wrap\(env\.COMMERCE\)/);
  // 用到的表(子查询里的也算)
  const sqls = [...src.matchAll(/env\.COMMERCE\s*,\s*([`'"])([\s\S]*?)\1/g)].map(m => m[2]);
  const tables = sqls.flatMap(s => [...s.matchAll(/\bFROM\s+(\w+)/g)].map(m => m[1]));
  assert.deepEqual([...new Set(tables)].sort(), ['payment_addresses', 'payments', 'solana_amount_reservations', 'solana_payment_addresses']);
  for (const s of sqls) assert.ok(!/\b(INSERT|UPDATE|DELETE|REPLACE|CREATE|DROP|ALTER|PRAGMA|ATTACH)\b/i.test(s), s);
  // 订单号是订阅的使用凭证:登记服务不读订单表,不读付款记录里订单的那一列,也不整行整行地取
  for (const s of sqls) assert.ok(!/\*/.test(s) && !/order/i.test(s.replace(/\bORDER\s+BY\b/gi, '')), '查询里不该出现订单的列或 SELECT *: ' + s);
  assert.ok(!/order_hash|order_id|order_no|orderId|FROM\s+orders\b/i.test(src), '源码里不该提到订单号');
});

test('订单号不出库:订单库里每笔付款都带着订单的标识,登记服务的库和公开账本里一个字都没有它', async t => {
  const x = setup(t), solA = x.solAddr(), tronA = x.tronAddr();
  const marks = [], buyer = tron();
  const pay = (tx, address, micros, from, mark) => {
    x.payment({ tx, address, micros, at: T0 - 3600, from });
    // 给这笔付款挂上订单的标识(订单库里真实的样子:order_hash 一列;链上事件里就算带着别的字段也一样不该被带出来)
    x.commerce.prepare('UPDATE payments SET order_hash = ?, event_json = json_set(event_json, ?, ?) WHERE tx_id = ?').run(mark, '$.orderRef', mark, tx);
    marks.push(mark);
  };
  pay('solana:' + sig(), solA, 9_900_001, '', 'ORDERHASH0A1B2C3D4E5F60718293A4B5C6D7E8F9');
  pay('d'.repeat(64), tronA, 29_900_002, buyer, 'ORDERHASH9F8E7D6C5B4A39281706F5E4D3C2B1A0');
  const l = await x.tick(T0);
  assert.equal(l.pool.orderCount, 2);
  assert.deepEqual(l.deposits.map(d => d.from).sort(), ['', buyer].sort(), '付款方地址照常取到(只从链上事件里单拿这一项)');
  const dump = JSON.stringify(l) + JSON.stringify(x.db.prepare('SELECT * FROM deposits').all()) + JSON.stringify(x.db.prepare('SELECT * FROM kv').all());
  for (const m of marks) assert.ok(!dump.includes(m), '订单的标识出现在了登记服务这边');
  assert.ok(!/order_?hash|orderRef|order_?id/i.test(dump));
});

// ================= 奖池:捐赠(只认留名的) =================
const ZERO = { total: 0, orderCount: 0, orderAmount: 0, orderShare: 0, donateCount: 0, donateAmount: 0, points: 0, addresses: 0 };
test('只有留名捐赠计入奖池:没登记就转进来的(不管多少)、转出的、失败的,一律不计、不列', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  x.chain.pay(solA, U(25), T0 + 60);                  // 没登记,直接转了 25
  x.chain.pay(solA, 999_999, T0 + 61);
  x.chain.pay(solA, -U(10), T0 + 62);                 // 转出
  x.chain.pay(solA, U(8), T0 + 63, { failed: true });
  x.chain.pay(solA, 3_141_592, T0 + 64);
  let l = await x.tick(T0 + 700);
  checkLedger(l, T0 + 700);
  assert.deepEqual(l.pool, ZERO, '地址上别的资金流动不进奖池');
  assert.deepEqual(l.deposits, []);
  assert.deepEqual(x.chain.asked, [], '没有留名在等到账:一次链都不查');
  assert.equal(x.db.prepare('SELECT COUNT(*) AS n FROM sigs').get().n, 0);

  // 有人留名捐 10:只有金额正好是给他的那个数的那一笔算
  const it = (await x.donate('老王', 10, T0 + 800)).body;
  x.chain.pay(solA, U(10), T0 + 850);                 // 同一时间别人转了个整 10:不是留名的那个数,不算
  const pay = x.chain.pay(solA, U(it.exact), T0 + 900);
  x.chain.pay(solA, U(it.exact) + 1, T0 + 910);       // 差一点点也不算
  l = await x.tick(T0 + 960);
  checkLedger(l, T0 + 960);
  assert.deepEqual(l.deposits.map(d => [d.kind, d.chain, d.amount, d.name, d.from, d.tx]), [['donation', 'solana', it.exact, '老王', pay.payer, pay.signature]]);
  assert.deepEqual(l.pool, { ...ZERO, total: W.usd(U(it.exact)), donateCount: 1, donateAmount: W.usd(U(it.exact)) });
  assert.deepEqual(l.chains, [{ chain: 'solana', address: solA, received: W.usd(U(it.exact)) }]);
  assert.equal(x.db.prepare("SELECT COUNT(*) AS n FROM sigs WHERE state = 'new'").get().n, 0);
  await x.tick(T0 + 1080);
  assert.equal(x.deposits().length, 1, '再跑不重复');
});

test('订单的付款不会被当成捐赠:订阅站认了才按订单算(40%),只记一笔', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  // 一张订单的金额是 9.990123:客人付了,订阅站还没来得及认
  x.reserve(solA, 9_990_123);
  const pay = x.chain.pay(solA, 9_990_123, T0 + 60);
  let l = await x.tick(T0 + 700);
  assert.deepEqual(l.pool, ZERO, '没人认的转入不进奖池');
  // 订阅站认了
  x.payment({ tx: 'solana:' + pay.signature, address: solA, micros: 9_990_123, at: T0 + 60 });
  l = await x.tick(T0 + 820);
  assert.deepEqual(l.pool, { ...ZERO, total: 4, orderCount: 1, orderAmount: 9.99, orderShare: 4 });
  // 万一有一笔留名的金额和订阅站认的订单付款是同一笔交易:按订单算,不重复记成捐赠
  const it = (await x.donate('碰巧', 12, T0 + 900)).body;
  const same = x.chain.pay(solA, U(it.exact), T0 + 950);
  x.payment({ tx: 'solana:' + same.signature, address: solA, micros: U(it.exact), at: T0 + 950 });
  l = await x.tick(T0 + 1000);
  assert.equal(l.pool.orderCount, 2);
  assert.equal(l.pool.donateCount, 0);
  assert.equal(x.deposits().length, 2);
});

test('链上节点不通:这一轮看不了转入,积分照样入账、账本照样出;通了之后补上', async t => {
  const x = setup(t), solA = x.solAddr(), p = await newPanel(), a = sol();
  await x.tick(T0);
  await x.report(p, a, 1100, T0 + 30);
  const it = (await x.donate('断网时捐的', 5, T0 + 40)).body;
  x.chain.pay(solA, U(it.exact), T0 + 60);
  x.chain.down = true; x.allowErrs = true;
  const l = await x.tick(T0 + 120);
  x.allowErrs = false;
  assert.equal(l.pool.donateCount, 0);
  assert.equal(l.pool.points, 1);
  const health = await (await worker.fetch(new Request('https://x/v1/health'), x.env)).json();
  assert.equal(health.at, T0 + 120);
  assert.match(health.errs.chain, /503/);
  x.chain.down = false;
  const l2 = await x.tick(T0 + 240);
  assert.deepEqual([l2.pool.donateCount, l2.deposits[0].name], [1, '断网时捐的']);
  const ok = await (await worker.fetch(new Request('https://x/v1/health'), x.env)).json();
  assert.deepEqual(ok.errs, {});
  assert.ok(ok.used > 0 && ok.used <= 50);
});

test('有人拿零碎转账刷屏:混在几百笔里的那笔留名捐赠几轮之内就认出来,每一轮都不超预算', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  const it = (await x.donate('被淹没的好人', 20, T0 + 30)).body;
  for (let i = 0; i < 160; i++) x.chain.pay(solA, 1000 + i, T0 + 60 + i);
  const pay = x.chain.pay(solA, U(it.exact), T0 + 230);
  for (let i = 0; i < 170; i++) x.chain.pay(solA, 5000 + i, T0 + 240 + i);
  let now = T0 + 480, seenAt = 0;
  for (let i = 1; i <= 14; i++) {
    const l = await x.tick(now);
    if (!seenAt && l.pool.donateCount === 1) seenAt = i;
    now += 120;
  }
  assert.ok(seenAt > 0 && seenAt <= 8, `第 ${seenAt} 轮认出来(一轮 2 分钟)`);
  assert.ok(x.db.prepare('SELECT COUNT(*) AS n FROM sigs').get().n <= 331);
  assert.equal(x.db.prepare("SELECT COUNT(*) AS n FROM sigs WHERE state = 'new'").get().n, 0);
  assert.deepEqual(x.deposits().map(d => [d.name, d.micros, d.tx]), [['被淹没的好人', U(it.exact), pay.signature]]);
  const state = JSON.parse(x.db.prepare("SELECT v FROM kv WHERE k = 'state'").get().v);
  assert.deepEqual(Object.values(state.cur).map(c => c.gaps), [[]], '留名兑现之后没拉完的那些不用再拉');
  const asked = x.chain.asked.length;
  await x.tick(now); await x.tick(now + 120);
  assert.equal(x.chain.asked.length, asked, '认出来之后就不再查链');
  assert.ok(x.calls.max > 20 && x.calls.max <= 50, `最忙的一轮用了 ${x.calls.max} 个子请求`);
});

test('Solana 节点不认批量请求:退回一笔一笔问,照样认得出留名捐赠', async t => {
  const x = setup(t), solA = x.solAddr();
  x.chain.noBatch = true;
  await x.tick(T0);
  const it = (await x.donate('慢节点上的好人', 8, T0 + 30)).body;
  for (let i = 0; i < 12; i++) x.chain.pay(solA, 2000 + i, T0 + 60 + i);
  x.chain.pay(solA, U(it.exact), T0 + 100);
  let l = null, now = T0 + 240;
  for (let i = 0; i < 6 && !(l && l.pool.donateCount); i++) { l = await x.tick(now); now += 120; }
  assert.deepEqual([l.pool.donateCount, l.deposits[0].name], [1, '慢节点上的好人']);
});

test('收款账户的地址是算出来的,不用问链:和链上实际的账户对得上', async () => {
  // 三个公开的钱包地址和它们在链上的 USDT 收款账户(2026-11 从官方节点查到的)
  assert.equal(await W.ataOf('FWznbcNXWQuHTawe9RxvQ2LdCENssh12dsznf4RiouN5'), 'VQkRW7jv4NVe5TJeSMTYMvWUzZUoHYeHZKa39MbYnMN');
  assert.equal(await W.ataOf('9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM'), 'TB5FCqbNsnuLQgEjUuPaT9qtVPTT4U1A8rvi7qzEj2M');
  assert.equal(await W.ataOf('5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9'), 'CyBjGpte4Npi5zNkdtWumPxVW4kpMR8BuFSbA587xZES');
  const base = new Uint8Array(32); base[0] = 0x58; for (let i = 1; i < 31; i++) base[i] = 0x66; base[31] = 0x66; // ed25519 的基点(y = 4/5)
  assert.ok(W.onCurve(base));
  const two = new Uint8Array(32); two[0] = 2; // y = 2 解不出 x
  assert.ok(!W.onCurve(two));
});

test('查账的节点:自己配的排最前;被限速的歇一会儿、换下一个;不给这种查询的一天内不拿它问这个', async t => {
  const x = setup(t), solA = x.solAddr();
  const st = () => JSON.parse(x.db.prepare("SELECT v FROM kv WHERE k = 'state'").get().v);
  assert.deepEqual(W.rpcList(x.env), ['http://rpc.test', 'https://api.mainnet-beta.solana.com', 'https://public.rpc.solanavibestation.com', 'https://solana-rpc.publicnode.com']);
  assert.equal(W.rpcList({}).length, 3, '不配自己的节点也有内置的几个公共节点');
  await x.tick(T0);
  const a = (await x.donate('甲', 5, T0 + 10)).body;
  x.chain.pay(solA, U(a.exact), T0 + 60);
  // 自己的节点被限速;第一个公共节点不给查交易号(要令牌);再下一个正常
  x.chain.rule = (host, method) => host === 'rpc.test' ? new Response('slow down', { status: 429 })
    : host === 'api.mainnet-beta.solana.com' && method === 'getSignaturesForAddress' ? new Response('token required', { status: 403 }) : null;
  let l = await x.tick(T0 + 120);
  assert.equal(l.pool.donateCount, 1, '换到能用的节点,照样查到');
  assert.deepEqual(x.chain.asked, [
    'rpc.test getSignaturesForAddress', 'api.mainnet-beta.solana.com getSignaturesForAddress', 'public.rpc.solanavibestation.com getSignaturesForAddress',
    'api.mainnet-beta.solana.com getTransaction', // 自己的节点在歇;官方节点只是不给查交易号,查单笔交易照样用它
  ]);
  let s = st().rpc;
  assert.deepEqual([s['rpc.test'].fails, s['rpc.test'].until], [1, T0 + 120 + 60], '被限速:歇 1 分钟');
  assert.ok(s['api.mainnet-beta.solana.com'].no.getSignaturesForAddress === T0 + 120 + 86400 && s['api.mainnet-beta.solana.com'].until === 0, '不给这种查询:记一天,不算它出错');

  // 还在限速:再试一次又不行,歇的时间翻倍;已经知道不给查交易号的那个不再问
  const b = (await x.donate('乙', 6, T0 + 200, '198.51.100.21')).body;
  x.chain.pay(solA, U(b.exact), T0 + 230);
  x.chain.asked.length = 0;
  l = await x.tick(T0 + 240);
  assert.equal(l.pool.donateCount, 2);
  assert.deepEqual(x.chain.asked.slice(0, 2), ['rpc.test getSignaturesForAddress', 'public.rpc.solanavibestation.com getSignaturesForAddress']);
  s = st().rpc;
  assert.deepEqual([s['rpc.test'].fails, s['rpc.test'].until], [2, T0 + 240 + 120], '第二次:歇 2 分钟');

  // 全都不行:这一轮查不了,错记在健康检查里;留名和钱都不丢,节点好了之后补上
  const c = (await x.donate('丙', 7, T0 + 300, '198.51.100.22')).body;
  x.chain.pay(solA, U(c.exact), T0 + 330);
  x.chain.rule = () => new Response('busy', { status: 429 });
  x.allowErrs = true;
  l = await x.tick(T0 + 480);
  assert.equal(l.pool.donateCount, 2);
  assert.match(st().tick.errs.chain, /答复 429/);
  l = await x.tick(T0 + 600);                 // 都在歇:只挑最快该醒的那个试一次,不是把每个都再敲一遍
  x.allowErrs = false;
  x.chain.rule = null;
  for (let i = 0, now = T0 + 720; i < 20 && l.pool.donateCount < 3; i++, now += 120) l = await x.tick(now);
  assert.deepEqual([l.pool.donateCount, l.deposits[0].name], [3, '丙'], '节点恢复后补上');
  assert.equal(st().rpc['rpc.test'].fails, 0, '好了就清零');
});

test('查账很省:没人捐的时候一次不查;有人在等到账时一轮也就两三次', async t => {
  const x = setup(t), solA = x.solAddr();
  for (let i = 0; i < 30; i++) await x.tick(T0 + i * 120);
  assert.deepEqual(x.chain.asked, [], '一小时没人捐:零次');
  const it = (await x.donate('省着查', 9, T0 + 3700)).body;
  for (let i = 0; i < 5; i++) await x.tick(T0 + 3720 + i * 120);
  assert.ok(x.chain.asked.length === 5 && x.chain.asked.every(a => a.endsWith('getSignaturesForAddress')), '等到账的 10 分钟:每轮拉一次交易号,共 ' + x.chain.asked.length + ' 次');
  x.chain.pay(solA, U(it.exact), T0 + 4400);
  const l = await x.tick(T0 + 4440);
  assert.equal(l.pool.donateCount, 1);
  assert.equal(x.chain.asked.length, 7, '到账那一轮:拉一次交易号 + 看一笔交易');
  for (let i = 0; i < 10; i++) await x.tick(T0 + 4560 + i * 120);
  assert.equal(x.chain.asked.length, 7, '兑现之后又回到零');
  // 没到账就过期的:期限(连余量)过后拉全一次,之后不再查
  const it2 = (await x.donate('没转', 9, T0 + 6000, '198.51.100.30')).body;
  x.chain.asked.length = 0;
  let last = 0;
  for (let now = T0 + 6060; now < T0 + 6000 + 3600 + 1800; now += 120) { await x.tick(now); if (x.chain.asked.length > last) last = x.chain.asked.length; }
  const during = x.chain.asked.length;
  for (let i = 0; i < 10; i++) await x.tick(T0 + 12000 + i * 120);
  assert.equal(x.chain.asked.length, during, '过期并且查完之后不再查');
  assert.ok(during <= 35, '一笔没到账的留名,从登记到过期一共查了 ' + during + ' 次');
  assert.equal((await (await worker.fetch(new Request('https://x/v1/donate/' + it2.id), x.env)).json()).status, 'expired');
});

// ================= 留名捐赠 =================
test('留名捐赠:登记后给一个带零头的确切金额,期限内这个数到账就计入奖池、记上名字', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  const r = await x.donate('  老王的 猫 ', 10, T0 + 10);
  assert.equal(r.status, 200);
  const it = r.body;
  assert.equal(it.name, '老王的 猫');
  assert.equal(it.amount, 10);
  assert.ok(it.exact > 10 && it.exact < 10.1);
  assert.equal(U(it.exact) % 1000, 0, '最多 3 位小数');
  assert.notEqual(U(it.exact) % 10000, 0, '带零头');
  assert.deepEqual([it.chain, it.address, it.status, it.expiresAt - it.createdAt], ['solana', solA, 'pending', 3600]);
  assert.match(it.id, /^[0-9a-f]{24}$/);

  const get = async () => (await (await worker.fetch(new Request('https://x/v1/donate/' + it.id), x.env)).json());
  assert.equal((await get()).status, 'pending');
  const pay = x.chain.pay(solA, U(it.exact), T0 + 900);
  const l = await x.tick(T0 + 960);
  assert.deepEqual(l.deposits.map(d => [d.kind, d.amount, d.name, d.from]), [['donation', it.exact, '老王的 猫', pay.payer]]);
  assert.deepEqual({ ...x.db.prepare('SELECT status, tx FROM intents WHERE id = ?').get(it.id) }, { status: 'paid', tx: pay.signature });
  // 同一个数再转一次:那笔留名已经兑现过了,这一笔对不上任何登记,不计入
  x.chain.pay(solA, U(it.exact), T0 + 1000);
  const l2 = await x.tick(T0 + 1100);
  assert.equal(l2.pool.donateCount, 1);
  assert.equal(x.deposits().length, 1);
});

test('留名捐赠:过了期限才到账的不计入奖池;期限内转的,登记服务晚看到也照样算;点了取消却还是在期限内转了的,钱不能白转,照样算', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  const late = (await x.donate('迟到的', 5, T0)).body, gone = (await x.donate('反悔的', 6, T0)).body, slow = (await x.donate('慢半拍', 7, T0)).body;
  const cancel = await worker.fetch(new Request(`https://x/v1/donate/${gone.id}/cancel`, { method: 'POST' }), x.env);
  assert.deepEqual(await cancel.json(), { ok: true });
  x.chain.pay(solA, U(late.exact), T0 + 3600 + 301);   // 期限 + 余量之外
  const back = x.chain.pay(solA, U(gone.exact), T0 + 600); // 点了取消,却还是在期限内转了(或者是转完才点的取消)
  const pay = x.chain.pay(solA, U(slow.exact), T0 + 3500); // 期限内转的
  const st = async id => (await (await worker.fetch(new Request('https://x/v1/donate/' + id), x.env)).json()).status;
  assert.equal(await st(gone.id), 'expired', '取消了的:面板上看是「没成」');
  const l = await x.tick(T0 + 3 * 3600);                // 登记服务两小时后才看到(那时这几笔留名都已经标成过期)
  assert.deepEqual(l.deposits.map(d => [d.amount, d.name, d.tx]), [[slow.exact, '慢半拍', pay.signature], [gone.exact, '反悔的', back.signature]]);
  assert.equal(l.pool.donateCount, 2);
  assert.deepEqual([await st(late.id), await st(gone.id), await st(slow.id)], ['expired', 'paid', 'paid']);
  // 取消了的那个金额,期限过完之前不会再发给别人(不然同一个数对着两笔登记)
  const y = setup(t), solB = y.solAddr();
  await y.tick(T0);
  const first = (await y.donate('先来的', 20, T0)).body;
  await worker.fetch(new Request(`https://x/v1/donate/${first.id}/cancel`, { method: 'POST' }), y.env);
  for (let k = 1; k <= 99; k++) { const m = U(20) + k * 1000; if (m !== U(first.exact)) y.reserve(solB, m); }
  assert.equal((await y.donate('后到的', 20, T0 + 60, '198.51.100.77')).body.code, 'no_slot', '期限内:取消的那个数还占着');
  assert.equal((await y.donate('后到的', 20, T0 + 3600 + 601, '198.51.100.77')).body.exact, first.exact, '期限(连两倍余量)过了才放出来');
});

test('留名捐赠:同一个数两个人同时填,拿到的确切金额不一样;避开订单占着的金额;排不开会说', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  const seen = new Set();
  for (let i = 0; i < 40; i++) { const r = await x.donate('n' + i, 20, T0 + i, `198.51.100.${i}`); assert.equal(r.status, 200); assert.ok(!seen.has(r.body.exact)); seen.add(r.body.exact); }
  // 把剩下的零头都让订单占掉(实际上订单金额不可能是这种整的数,这里只为走到那条分支)
  for (let k = 1; k <= 99; k++) { const m = U(20) + k * 1000; if (!seen.has(m / 1e6)) x.reserve(solA, m); }
  const full = await x.donate('没位置', 20, T0 + 100, '198.51.100.200');
  assert.equal(full.status, 503);
  assert.match(full.body.error, /排不开/);
  // 一个小时后那 40 个都过期了,再过一会儿它们占的数就让出来
  const again = await x.donate('后来的', 20, T0 + 3600 + 700, '198.51.100.201');
  assert.equal(again.status, 200);
  assert.ok(seen.has(again.body.exact));
});

test('留名捐赠:填得不对的不收;提交太勤的拦一下;不让上榜的字眼照样计入、名字不显示', async t => {
  const x = setup(t), solA = x.solAddr();
  await x.tick(T0);
  for (const [name, amount, why] of [['', 10, /留名/], ['好人', 0.5, /金额/], ['好人', 100001, /金额/], ['好人', 10.123, /两位小数/], ['好人', 'abc', /金额/], ['好人', -5, /金额/], ['http://a.b', 10, /网址/], ['x'.repeat(21), 10, /20 个字/]]) {
    const r = await x.donate(name, amount, T0 + 5);
    assert.equal(r.status, 400, `${name} ${amount}`);
    assert.match(r.body.error, why);
  }
  for (let i = 0; i < CFG.intentsPerIpHour; i++) assert.equal((await x.donate('手快', 3, T0 + 10 + i)).status, 200);
  assert.equal((await x.donate('手快', 3, T0 + 30)).status, 429);
  assert.equal((await x.donate('手快', 3, T0 + 3700)).status, 200, '一小时后又可以');

  x.env.NAME_BLOCK = '广告位, Spam';
  const it = (await x.donate('来看spam吧', 4, T0 + 40, '198.51.100.77')).body;
  x.chain.pay(solA, U(it.exact), T0 + 60);
  const l = await x.tick(T0 + 120);
  assert.deepEqual(l.deposits.map(d => [d.amount, d.name]), [[it.exact, W.HIDDEN_NAME]], '钱照样进奖池,名字不上榜');
  assert.equal(l.pool.donateCount, 1);
});

test('没有可用的奖池地址时:留名捐赠说明白,账本照样出', async t => {
  const x = setup(t);
  const l = await x.tick(T0);
  checkLedger(l, T0);
  assert.deepEqual(l.donate, { chain: 'solana', address: '', min: 1, max: 100000 });
  assert.deepEqual(l.chains, []);
  const r = await x.donate('好人', 10, T0 + 5);
  assert.equal(r.status, 503);
});

// ================= 接口 =================
test('接口:路由、请求格式、公开账本可跨域读', async t => {
  const x = setup(t), solA = x.solAddr(), p = await newPanel(), a = sol();
  const now = Math.floor(Date.now() / 1000);
  const call = (method, p2, body, headers = {}) => worker.fetch(new Request('https://reward.test' + p2, { method, body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body), headers: { 'CF-Connecting-IP': '203.0.113.50', ...headers } }), x.env);

  let res = await call('POST', '/v1/report', await signed(p, a, 2048, now));
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { gb: 2048, credited: 0 });
  assert.equal((await call('POST', '/v1/report', '{oops')).status, 400);
  assert.equal((await call('POST', '/v1/report', '[1,2]')).status, 400);
  assert.equal((await call('POST', '/v1/report', 'x'.repeat(5000))).status, 400);
  assert.equal((await call('GET', '/v1/report')).status, 404);
  assert.equal((await call('GET', '/v1/nothing')).status, 404);
  assert.equal((await call('GET', '/v1/donate/' + '0'.repeat(24))).status, 404);
  assert.equal((await call('GET', '/v1/donate/short')).status, 404);

  res = await call('GET', '/v1/export');
  assert.equal(res.status, 200);
  assert.equal(res.headers.get('Access-Control-Allow-Origin'), '*');
  const l = await res.json();
  checkLedger(l, now);
  assert.equal(l.donate.address, solA);

  await x.tick(now);
  res = await call('GET', '/v1/export/');
  assert.equal((await res.json()).pool.points, 1, '定时任务入了账,账本跟着更新');

  res = await call('POST', '/v1/donate', { name: '好人', amount: 12.5 });
  assert.equal(res.status, 200);
  const it = await res.json();
  assert.equal((await (await call('GET', '/v1/donate/' + it.id)).json()).status, 'pending');
  assert.equal((await call('DELETE', '/v1/donate/' + it.id)).status, 405);
  assert.equal((await call('GET', `/v1/donate/${it.id}/cancel`)).status, 405);
  assert.equal((await call('POST', `/v1/donate/${it.id}/cancel`)).status, 200);
  assert.equal((await (await call('GET', '/v1/donate/' + it.id)).json()).status, 'expired', '取消了的在面板那边显示为已结束');
  assert.equal((await call('POST', '/v1/donate', { name: '好人' })).status, 400);
});

test('日常清理:旧的每日记录、没到账的留名、看过的交易号,过期就删;积分和入账永远留着', async t => {
  const x = setup(t), solA = x.solAddr(), p = await newPanel(), a = sol();
  await x.tick(T0);
  await x.report(p, a, 1100, T0 + 10);
  await x.donate('没付', 5, T0 + 20);
  const it = (await x.donate('付了', 6, T0 + 30, '198.51.100.3')).body;
  x.chain.pay(solA, U(it.exact), T0 + 100);
  await x.tick(T0 + 200);
  const later = T0 + 70 * DAY;
  await x.report(p, a, 1200, later);
  await x.tick(later + 60);
  assert.equal(x.db.prepare('SELECT COUNT(*) AS n FROM samples').get().n, 1, '70 天前那条每日记录删了');
  assert.deepEqual(x.db.prepare('SELECT name FROM intents').all().map(r => r.name), ['付了']);
  assert.equal(x.db.prepare('SELECT COUNT(*) AS n FROM sigs').get().n, 0);
  assert.equal(x.addr(a).credited, 1);
  assert.equal(x.deposits().length, 1);
});

// 账本核对脚本的测试:node --test deploy/reward/check-ledger.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checkLedger, checkGrowth, verdict } from './check-ledger.mjs';

const NOW = 1_800_000_000;
const SOL = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB', TRON = 'TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t';
const A1 = '7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU', A2 = '11111111111111111111111111111112';
const good = () => ({
  v: 1, updatedAt: NOW - 60, payoutAt: NOW + 86400 * 200, orderRate: 0.4,
  pool: { total: 22.99, orderCount: 2, orderAmount: 29.98, orderShare: 11.99, donateCount: 2, donateAmount: 11, points: 5, addresses: 2 },
  chains: [{ chain: 'solana', address: SOL, received: 30.99, balance: 30.99 }, { chain: 'tron', address: TRON, received: 9.99, balance: 9.99 }],
  donate: { chain: 'solana', address: SOL, min: 1, max: 100000 },
  deposits: [
    { ts: NOW - 100, kind: 'donation', chain: 'solana', address: SOL, amount: 6.037, from: A1, tx: 'sig3', name: '老王' },
    { ts: NOW - 200, kind: 'donation', chain: 'solana', address: SOL, amount: 4.963, from: '', tx: 'sig2' },
    { ts: NOW - 300, kind: 'order', chain: 'solana', address: SOL, amount: 19.990013, from: A1, tx: 'sig1' },
    { ts: NOW - 400, kind: 'order', chain: 'tron', address: TRON, amount: 9.991237, from: TRON, tx: 'a'.repeat(64) },
  ],
  board: [{ address: A1, points: 4, panels: 2 }, { address: A2, points: 1, panels: 1 }, { address: SOL, points: 3, panels: 1, excluded: true, rule: 'same_src', reason: '这处来源上已经有别的地址在登记' }], // 被剔除的那 3 分不算进合计
  events: [],
});
const edit = fn => { const l = good(); fn(l); return l; };

test('没问题的账本:通过', () => {
  assert.deepEqual(checkLedger(good(), NOW), []);
  assert.deepEqual(verdict(good(), null, NOW), { code: 0, problems: [] });
});

test('写坏的、自相矛盾的:说出哪里不对', () => {
  const cases = [
    [l => { l.v = 2; }, /版本/],
    [l => { l.updatedAt = NOW + 3600; }, /更新时间/],
    [l => { l.orderRate = 1.5; }, /比例/],
    [l => { delete l.board; }, /缺/],
    [l => { l.chains[0].address = 'nope'; }, /收款地址不对/],
    [l => { l.chains[1].chain = 'solana'; }, /收款地址不对/],
    [l => { l.donate.address = A1; }, /捐赠地址不在收款地址里/],
    [l => { l.deposits[0].kind = 'gift'; }, /类型不对/],
    [l => { l.deposits[1].tx = 'sig3'; }, /交易号重复/],
    [l => { l.deposits[2].name = '不该有'; }, /订单,不该有留名/],
    [l => { l.deposits[0].name = 'x'.repeat(30); }, /写得不对/],
    [l => { l.pool.total = 99; }, /奖池 99 ≠/],
    [l => { l.pool.orderShare = 20; l.pool.total = 31; }, /≠ 订单合计/],
    [l => { l.pool.orderCount = 3; }, /笔数对不上/],
    [l => { l.deposits[1].amount = 40; }, /金额对不上/],
    [l => { l.board[0].points = 9; }, /积分对不上/],
    [l => { l.board[1].address = A1; }, /地址重复/],
    [l => { l.board[0].address = TRON; }, /积分榜第 1 行/],
    [l => { l.board[0].points = -1; }, /积分榜第 1 行/],
  ];
  for (const [fn, why] of cases) {
    const problems = checkLedger(edit(fn), NOW);
    assert.ok(problems.some(p => why.test(p)), `${fn} → ${JSON.stringify(problems)}`);
    assert.equal(verdict(edit(fn), null, NOW).code, 1);
  }
  assert.deepEqual(checkLedger(null, NOW), ['不是一个 JSON 对象']);
});

test('只增不减:比分支上的少了、丢了已有的入账,不让提交', () => {
  const prev = good();
  const more = edit(l => { l.deposits.unshift({ ts: NOW - 10, kind: 'donation', chain: 'solana', address: SOL, amount: 2, from: A2, tx: 'sig4' }); l.pool.donateCount = 3; l.pool.donateAmount = 13; l.pool.total = 24.99; });
  assert.deepEqual(checkGrowth(prev, more), []);
  assert.equal(verdict(more, prev, NOW).code, 0);

  const wiped = edit(l => { l.deposits = []; l.board = []; l.pool = { total: 0, orderCount: 0, orderAmount: 0, orderShare: 0, donateCount: 0, donateAmount: 0, points: 0, addresses: 0 }; });
  assert.deepEqual(checkLedger(wiped, NOW), [], '一份空账本本身写法没错');
  const v = verdict(wiped, prev, NOW);
  assert.equal(v.code, 1, '但它比分支上的少:登记服务的库被清空了,拦住');
  assert.ok(v.problems.some(p => /total 比分支上的少了/.test(p)) && v.problems.some(p => /丢了 4 笔/.test(p)));

  const swapped = edit(l => { l.deposits[1].tx = 'other'; });
  assert.ok(checkGrowth(prev, swapped).some(p => /丢了 1 笔,比如 sig2/.test(p)));
  // 积分可以少(作弊的被作废),不算问题
  const voided = edit(l => { l.board[0].points = 2; l.pool.points = 3; });
  assert.equal(verdict(voided, prev, NOW).code, 0);
});

test('和分支上的一样:不用提交;分支上那份放了 6 小时以上:提交一次,把更新时间带上去', () => {
  const prev = edit(l => { l.updatedAt = NOW - 1800; });
  assert.equal(verdict(good(), prev, NOW).code, 3);
  const old = edit(l => { l.updatedAt = NOW - 7 * 3600; });
  assert.equal(verdict(good(), old, NOW).code, 0);
  const changed = edit(l => { l.board[1].points = 2; l.pool.points = 6; });
  assert.equal(verdict(changed, prev, NOW).code, 0);
});

// ---- 发放金额(payout.mjs)----
import { payout } from './payout.mjs';
test('发放:按积分比例,向下取到分;被剔除的不参加;发出去的不超过奖池', () => {
  const r = payout(good());
  // 奖池 22.99,A1 4 分、A2 1 分;SOL 那个地址被剔除着,它的 3 分不参加
  assert.deepEqual(r.rows, [{ address: A1, points: 4, share: 0.8, amount: 18.39 }, { address: A2, points: 1, share: 0.2, amount: 4.59 }]);
  assert.deepEqual([r.pool, r.points, r.paid, r.left], [22.99, 5, 22.98, 0.01]);
  // 三个人平分 100:每人 33.33,剩 0.01 留在奖池地址上
  const three = edit(l => { l.pool.total = 100; l.board = [A1, A2, SOL].map(address => ({ address, points: 7, panels: 1 })); });
  const t3 = payout(three);
  assert.deepEqual(t3.rows.map(x => x.amount), [33.33, 33.33, 33.33]);
  assert.deepEqual([t3.paid, t3.left], [99.99, 0.01]);
  // 没有人有积分:谁也不发,钱都留着
  const none = payout(edit(l => { l.board = []; }));
  assert.deepEqual([none.rows.length, none.paid, none.left], [0, 0, 22.99]);
  // 很大的数也不丢精度
  const big = payout(edit(l => { l.pool.total = 98765432.1; l.board = [{ address: A1, points: 999999, panels: 1 }, { address: A2, points: 1, panels: 1 }]; }));
  assert.deepEqual(big.rows.map(x => x.amount), [98765333.33, 98.76]);
  assert.ok(big.paid <= big.pool);
});

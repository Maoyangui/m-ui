// 提交到 rewards 分支之前,把登记服务出的账本核对一遍。给 ledger-workflow.yml 用;任何人也可以拿它核对分支上的那份。
//
//   node check-ledger.mjs <新取到的 ledger.json> [<分支上现在的 ledger.json>]
//
// 退出码:0 = 账本没问题,该提交;3 = 和分支上的一样(只有更新时间不同),分支上那份也还不旧,这次不用提交;
//        1 = 账本不对(写坏了、自相矛盾、比分支上的还少),不能提交,原因打在标准错误里。
//
// 核对三件事:
//   1. 写法:和面板收账本时查的一样(reward.Ledger.check)—— 面板不认的东西不往分支上放。
//   2. 自洽:合计对得上逐笔(订单 × 比例 + 捐赠 = 奖池;积分合计 = 榜上各行之和;作废的不进合计),没有重复的交易号和地址。
//   3. 只增不减:分支上已有的入账一笔都不能丢;奖池、笔数不能比分支上的少 —— 唯一的例外是订单取消(退款):
//      那一笔留在账本上标成作废,合计少掉的正好是它。登记服务的库要是被清空或者写乱了,在这里拦住。
import fs from 'node:fs';
import { chainOf, validSolana } from './worker.mjs';

const near = (a, b, tol = 0.011) => Math.abs(a - b) <= tol;
const len = s => [...String(s ?? '')].length;
export const MAX_ROWS = 5000; // 账本里最多带这么多笔入账(更早的不带,合计里照样算)

// 写法 + 自洽。返回问题的清单(空 = 没问题)。
export function checkLedger(l, now = Math.floor(Date.now() / 1000)) {
  const bad = [];
  const need = (ok, what) => { if (!ok) bad.push(what); };
  if (!l || typeof l !== 'object') return ['不是一个 JSON 对象'];
  need(l.v === 1, `不认识的账本版本 ${l.v}`);
  need(Number.isInteger(l.updatedAt) && l.updatedAt > 0 && l.updatedAt <= now + 600, '更新时间不对');
  need(typeof l.orderRate === 'number' && l.orderRate > 0 && l.orderRate <= 1, '订单计入的比例不对');
  const p = l.pool || {};
  for (const k of ['total', 'orderCount', 'orderAmount', 'orderShare', 'donateCount', 'donateAmount', 'points', 'addresses']) need(typeof p[k] === 'number' && p[k] >= 0, `合计里的 ${k} 不对`);
  need(Array.isArray(l.chains) && Array.isArray(l.deposits) && Array.isArray(l.board), '缺 chains / deposits / board');
  if (bad.length) return bad;

  for (const c of l.chains) need(['solana', 'tron'].includes(c.chain) && chainOf(c.address) === c.chain, `收款地址不对: ${c.chain} ${c.address}`);
  need(new Set(l.chains.map(c => c.address)).size === l.chains.length, '收款地址有重复');
  const d = l.donate || {};
  need(d.chain === 'solana' && (d.address === '' || validSolana(d.address)), '捐赠地址不对');
  need(!d.address || l.chains.some(c => c.address === d.address), '捐赠地址不在收款地址里');

  const seen = new Set();
  const sums = { order: { n: 0, v: 0 }, donation: { n: 0, v: 0 } };
  l.deposits.forEach((x, i) => {
    const at = `第 ${i + 1} 笔入账`;
    if (!sums[x.kind]) return bad.push(`${at}的类型不对`);
    need(typeof x.amount === 'number' && x.amount > 0 && typeof x.tx === 'string' && x.tx.length <= 128 && len(x.from) <= 64 && len(x.name) <= 24 && Number.isInteger(x.ts) && x.ts > 0, `${at}写得不对`);
    need(['solana', 'tron'].includes(x.chain), `${at}的链不对`);
    need(x.kind === 'donation' || !x.name, `${at}是订单,不该有留名`);
    need(x.kind !== 'donation' || !!x.name, `${at}是捐赠却没有留名(只有留名捐赠计入奖池)`);
    // note = 'manual':人工确认发放的订单;只有它可以没有交易号(没对上链上那一笔的)。void = true:作废(订单取消了),不进合计
    need(x.note === undefined || (x.note === 'manual' && x.kind === 'order'), `${at}的标记不对`);
    need(x.void === undefined || (x.void === true && x.kind === 'order'), `${at}的作废标记不对`);
    need(!!x.tx || x.note === 'manual', `${at}没有交易号(只有人工确认发放的订单可以没有)`);
    if (x.tx) { need(!seen.has(x.tx), `${at}的交易号重复了`); seen.add(x.tx); }
    if (!x.void) { sums[x.kind].n++; sums[x.kind].v += x.amount; }
  });
  need(near(p.orderShare, p.orderAmount * l.orderRate), `订单计入奖池的部分 ${p.orderShare} ≠ 订单合计 ${p.orderAmount} × ${l.orderRate}`);
  need(near(p.total, p.orderShare + p.donateAmount), `奖池 ${p.total} ≠ 订单计入的 ${p.orderShare} + 捐赠 ${p.donateAmount}`);
  if (l.deposits.length < MAX_ROWS) { // 逐笔都带着的时候,合计要和逐笔对得上
    need(sums.order.n === p.orderCount && sums.donation.n === p.donateCount, `笔数对不上:逐笔是 订单 ${sums.order.n} / 捐赠 ${sums.donation.n},合计写的是 ${p.orderCount} / ${p.donateCount}`);
    need(near(sums.order.v, p.orderAmount, 0.011 + sums.order.n * 0.000001) && near(sums.donation.v, p.donateAmount, 0.011 + sums.donation.n * 0.000001), '金额对不上:逐笔加起来不等于合计');
  }

  const addrs = new Set();
  let points = 0, holders = 0;
  l.board.forEach((b, i) => {
    need(validSolana(b.address) && Number.isInteger(b.points) && b.points >= 0 && len(b.reason) <= 200 && len(b.rule) <= 32, `积分榜第 ${i + 1} 行写得不对`);
    need(!addrs.has(b.address), `积分榜第 ${i + 1} 行的地址重复了`);
    addrs.add(b.address);
    if (!b.excluded) { points += b.points || 0; if (b.points > 0) holders++; } // 被剔除的地址不算进合计
  });
  need(points === p.points && holders === p.addresses, `积分对不上:榜上没被剔除的加起来 ${points} 分 / ${holders} 个地址,合计写的是 ${p.points} / ${p.addresses}`);
  return bad;
}

// 和分支上现在的那份比:只增不减。返回问题的清单。
// 例外只有一种:上一份里计入着的订单,这一份里标成了作废(订单取消 / 退款)—— 合计可以少,但少掉的不能超过这些作废的。
export function checkGrowth(prev, next) {
  const bad = [];
  if (!prev || !prev.pool) return bad;
  const a = prev.pool, b = next.pool;
  const key = x => x.tx || `manual:${x.ts}:${x.amount}`; // 没有交易号的(人工确认的订单)拿时间和金额认
  const counted = new Map((prev.deposits || []).filter(x => !x.void).map(x => [key(x), x]));
  const gone = (next.deposits || []).filter(x => x.void && x.kind === 'order' && counted.has(key(x)));
  const goneAmt = gone.reduce((s, x) => s + x.amount, 0);
  const allow = { total: goneAmt * next.orderRate, orderCount: gone.length, orderAmount: goneAmt, donateCount: 0, donateAmount: 0 };
  for (const k of ['total', 'orderCount', 'orderAmount', 'donateCount', 'donateAmount']) {
    if (b[k] < a[k] - allow[k] - 0.011 - gone.length * 0.006) bad.push(`${k} 比分支上的少了:${a[k]} → ${b[k]}${gone.length ? `(这一份里新作废了 ${gone.length} 笔订单,解释不了这么多)` : ''}`);
  }
  if (next.deposits.length < MAX_ROWS) {
    const have = new Set(next.deposits.map(key));
    const lost = prev.deposits.filter(x => !have.has(key(x)));
    if (lost.length) bad.push(`分支上已有的入账丢了 ${lost.length} 笔,比如 ${key(lost[0])}`);
  }
  return bad;
}
const same = (a, b) => JSON.stringify({ ...a, updatedAt: 0 }) === JSON.stringify({ ...b, updatedAt: 0 });

// 判定:{ code, problems }。code 的意思见文件头。
export function verdict(next, prev, now = Math.floor(Date.now() / 1000), refreshAfter = 6 * 3600) {
  const problems = [...checkLedger(next, now), ...(prev ? checkGrowth(prev, next) : [])];
  if (problems.length) return { code: 1, problems };
  if (prev && same(prev, next) && now - prev.updatedAt < refreshAfter) return { code: 3, problems: [] };
  return { code: 0, problems: [] };
}

if (process.argv[1] && import.meta.url.endsWith(process.argv[1].replace(/\\/g, '/').split('/').pop())) {
  const [nextFile, prevFile] = process.argv.slice(2);
  if (!nextFile) { console.error('用法: node check-ledger.mjs <新的 ledger.json> [<分支上现在的 ledger.json>]'); process.exit(1); }
  let next, prev = null;
  try { next = JSON.parse(fs.readFileSync(nextFile, 'utf8')); } catch (e) { console.error('新取到的账本读不出来: ' + e.message); process.exit(1); }
  if (prevFile && fs.existsSync(prevFile)) { try { prev = JSON.parse(fs.readFileSync(prevFile, 'utf8')); } catch { console.error('分支上现在的账本读不出来,当它没有'); } }
  const v = verdict(next, prev);
  for (const p of v.problems) console.error('✗ ' + p);
  console.log(v.code === 0 ? `账本没问题:奖池 ${next.pool.total} USDT,${next.pool.orderCount} 笔订单 + ${next.pool.donateCount} 笔捐赠,${next.pool.points} 积分 / ${next.pool.addresses} 个地址` : v.code === 3 ? '和分支上的一样,不用提交' : '账本不对,不提交');
  process.exit(v.code);
}

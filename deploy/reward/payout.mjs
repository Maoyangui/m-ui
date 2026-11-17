// 发放那天每个地址该发多少:拿公开账本自己算一遍,谁都可以算,结果应当和实际发出去的逐笔对得上。
//
//   node payout.mjs <ledger.json>            打印一张表(地址、积分、占比、金额)
//   node payout.mjs <ledger.json> --csv      输出 CSV(address,points,amount),方便核对或批量转账
//
// 算法只有一条:金额 = 奖池 × 这个地址的积分 ÷ 全部积分,向下取到 0.01 USDT。
// 名下面板全被剔除着的地址不参加(它们的积分也不在「全部积分」里);向下取整剩下的零头留在奖池地址上,不归任何人。
// 这个脚本只算数,不碰链、不转账。
import fs from 'node:fs';

// 返回 { rows: [{ address, points, share, amount }], pool, points, paid, left }。金额的单位是 USDT,内部按分算,不用浮点累加。
export function payout(ledger) {
  const pool = Math.round(Number(ledger.pool.total) * 100); // 分
  const live = (ledger.board || []).filter(b => !b.excluded && b.points > 0);
  const points = live.reduce((s, b) => s + b.points, 0);
  const rows = live.map(b => {
    const cents = points > 0 ? Number(BigInt(pool) * BigInt(b.points) / BigInt(points)) : 0;
    return { address: b.address, points: b.points, share: points > 0 ? b.points / points : 0, amount: cents / 100 };
  }).sort((a, b) => b.points - a.points || (a.address < b.address ? -1 : 1));
  const paid = rows.reduce((s, r) => s + Math.round(r.amount * 100), 0);
  return { rows, pool: pool / 100, points, paid: paid / 100, left: (pool - paid) / 100 };
}

if (process.argv[1] && import.meta.url.endsWith(process.argv[1].replace(/\\/g, '/').split('/').pop())) {
  const [file, flag] = process.argv.slice(2);
  if (!file) { console.error('用法: node payout.mjs <ledger.json> [--csv]'); process.exit(1); }
  const r = payout(JSON.parse(fs.readFileSync(file, 'utf8')));
  if (flag === '--csv') {
    console.log('address,points,amount');
    for (const x of r.rows) console.log(`${x.address},${x.points},${x.amount.toFixed(2)}`);
  } else {
    for (const x of r.rows) console.log(`${x.address}  ${String(x.points).padStart(6)} 分  ${(x.share * 100).toFixed(2).padStart(6)}%  ${x.amount.toFixed(2).padStart(12)} USDT`);
    console.log(`\n奖池 ${r.pool.toFixed(2)} USDT · ${r.points} 积分 · ${r.rows.length} 个地址 · 发出 ${r.paid.toFixed(2)} · 取整剩下 ${r.left.toFixed(2)}`);
  }
}

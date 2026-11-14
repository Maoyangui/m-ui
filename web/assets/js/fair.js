// 按服务器公平取前 n 条(和后端 web.fairMerge 同一个办法):先给每台服务器留它最近的一条(最近有记录的服务器先留,
// 服务器比 n 还多时只留前 n 台的),剩下的行数按时间从新到旧补满;结果按时间从新到旧排。只有一台服务器(或都没标服务器)、
// 或总数不超过 n 时就是最近的 n 条。
// 概览"最近入站连接"每档筛选都只显示前几条:混在一起按时间截断,一台正被扫描、记录特别多的服务器能把别的服务器
// (比如用户刚在上面试过 IPv6 线路的那台)的记录整条挤到看不见的地方;反过来也不能严格按服务器轮流取 —— 跟踪器只按条数淘汰、
// 不按时间过期,安静的服务器上留着几天前的旧记录,轮流取会让它们挤掉忙的那台几分钟前的连接。
// 后端只在总数超过接口上限时才这样筛,没超过时原样按时间给全部,所以界面截断时这里还要再筛一次。
export function fairPick(list, n) {
  const byTs = (a, b) => (b.ts || 0) - (a.ts || 0);
  const all = [...(list || [])].sort(byTs);
  if (all.length <= n) return all;
  const seen = new Set(), keep = new Set();
  for (const c of all) { // 按时间从新到旧:每台第一次出现的那条就是它最近的
    const k = c.nodeId ?? c.server ?? '';
    if (!seen.has(k) && keep.size < n) { seen.add(k); keep.add(c); }
  }
  for (const c of all) if (keep.size < n) keep.add(c);
  return all.filter(c => keep.has(c));
}

// shownConns 不传行数时的默认值:通过认证的(含被设备数拒的)、"到了但没过握手 / 认证"的。概览页按筛选和屏宽自己传行数。
export const CONN_ROWS = 30, FAIL_ROWS = 15;

// shownConns 概览"最近入站连接"各筛选显示哪几行:接口给的一份分成通过认证的(rows,「连接」/「被拒」筛选传进来的已是对应子集)
// 与带 failed 的"到了但没过"(fails,「握手失败」筛选),各自按 fairPick 取。页面只管渲染,截断都在这里(行数由页面传入,别在页面里按时间 slice)。
export function shownConns(all, rows = CONN_ROWS, fails = FAIL_ROWS) {
  const list = Array.isArray(all) ? all : []; // 官网演示的假接口没有这项,回的是 {}
  return { rows: fairPick(list.filter(c => !c.failed), rows), fails: fairPick(list.filter(c => c.failed), fails) };
}

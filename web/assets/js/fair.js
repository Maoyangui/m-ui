// 按服务器公平取前 n 条(和后端 web.fairMerge 同一个办法):每台服务器轮流取自己最近的一条(最近有记录的服务器先取),
// 取够 n 条为止,结果按时间从新到旧排;只有一台服务器(或都没标服务器)时就是最近的 n 条。
// 概览"最近入站连接"的两张表都只显示前几条:混在一起按时间截断,一台正被扫描、记录特别多的服务器
// 能把别的服务器(比如用户刚在上面试过 IPv6 线路的那台)的记录整条挤到看不见的地方。
// 后端只在总数超过接口上限时才轮流取,没超过时原样按时间给全部,所以界面截断时这里还要再按服务器轮流取一次。
export function fairPick(list, n) {
  const groups = new Map();
  for (const c of list || []) {
    const k = c.nodeId ?? c.server ?? '';
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(c);
  }
  const byTs = (a, b) => (b.ts || 0) - (a.ts || 0);
  const gs = [...groups.values()].map(g => g.sort(byTs)).sort((a, b) => byTs(a[0], b[0]));
  const out = [];
  for (let i = 0; out.length < n && gs.some(g => i < g.length); i++) {
    for (const g of gs) if (i < g.length && out.length < n) out.push(g[i]);
  }
  return out.sort(byTs);
}

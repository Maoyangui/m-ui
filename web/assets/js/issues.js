// 全站同一口径的"哪里有问题":概况的「需要处理」、服务器页「只看异常」、上游页「只看故障」、顶部提示条都从这里取。
// 纯函数,不碰 DOM、不依赖 i18n(返回代码,由调用方翻译),web/jstest/issues.test.mjs 用 node --test 直接测。

// ---- 服务器 ----

// 严重程度从高到低的顺序:「需要处理」的分项、名单都按它排
export const NODE_CODES = ['offline', 'coreStopped', 'reloadErr', 'decom', 'ledger', 'unsynced', 'version'];
const RED = new Set(['offline', 'coreStopped', 'reloadErr', 'decom', 'ledger']);
export const nodeSev = code => (RED.has(code) ? 'bad' : 'warn');

// 一次保存后副机会有几秒"未同步":连续看到这么久才算
export const UNSYNCED_GRACE = 30;
// 主机一轮同步失败就把那台标成离线(偶发超时):上次联系上的时间离现在不到这么久时先不算离线
export const OFFLINE_GRACE = 30;

// nodeIssues 一台服务器的问题:[{code, sev}]。只看远端服务器(本机的数据面与重载另有来源)。
// 停用的服务器只看"下线通知未送达";离线时不再看其它项(状态是上一次的,不可信)。
// unsyncedFor:已经连续看到"未同步"多少秒(调用方记,见 trackUnsynced);不给时按已超过门槛算。
// now:当前时间(秒,测试用);离线要上次联系上(lastSeen)已过 OFFLINE_GRACE 秒、或者从没联系上过才算。
export function nodeIssues(n, { unsyncedFor = Infinity, now = Date.now() / 1000 } = {}) {
  const out = [];
  if (!n || n.isLocal) return out;
  const add = code => out.push({ code, sev: nodeSev(code) });
  if (n.decommission) add('decom');
  if (!n.enabled) return out;
  const s = n.status;
  if (!s || !s.ok) {
    if (s && s.lastSeen && now - s.lastSeen < OFFLINE_GRACE) return sortIssues(out); // 刚失联一轮:先不报
    add('offline');
    return sortIssues(out);
  }
  if (!s.coreRunning) add('coreStopped');
  if (s.reloadError) add('reloadErr');
  if (s.ledgerFails > 0) add('ledger');
  if (!s.synced && unsyncedFor >= UNSYNCED_GRACE) add('unsynced');
  if (s.versionMismatch) add('version');
  return sortIssues(out);
}
const sortIssues = list => list.sort((a, b) => NODE_CODES.indexOf(a.code) - NODE_CODES.indexOf(b.code));

// trackUnsynced 记下每台服务器第一次看到"未同步"的时间(毫秒),同步了就删掉;返回 名字 → 已持续秒数 的函数。
// since 是调用方自己的 Map(各页面各记各的),nowMs 给测试用。
export function trackUnsynced(since, nodes, nowMs = Date.now()) {
  const seen = new Set();
  for (const n of nodes || []) {
    if (!n || n.isLocal || !n.enabled) continue;
    const s = n.status;
    if (s && s.ok && !s.synced) {
      seen.add(n.name);
      if (!since.has(n.name)) since.set(n.name, nowMs);
    }
  }
  for (const k of [...since.keys()]) if (!seen.has(k)) since.delete(k);
  return name => (since.has(name) ? (nowMs - since.get(name)) / 1000 : 0);
}

// summarizeNodes GET nodes 的返回汇总成概况要的数:
// total = 启用的远端台数 + 本机;online = total - 离线;byCode = {代码: [服务器名]};
// 已删除但下线通知还没送到的(decommissions[])也计入 decom。
// offline / fault(数据面停止 + 重载失败 + 下线通知未送达 + 记账失败)/ warn(未同步 + 版本不一致)是台次,
// names(sev) 按严重程度列出有问题的服务器(去重)。
export function summarizeNodes(resp, { unsyncedFor, now = Date.now() / 1000 } = {}) {
  const nodes = ((resp && resp.nodes) || []).filter(n => !n.isLocal);
  const byCode = {};
  for (const c of NODE_CODES) byCode[c] = [];
  const order = [];
  for (const n of nodes) {
    const list = nodeIssues(n, { unsyncedFor: unsyncedFor ? unsyncedFor(n.name) : Infinity, now });
    for (const x of list) byCode[x.code].push(n.name);
    if (list.length) order.push({ name: n.name, rank: NODE_CODES.indexOf(list[0].code) });
  }
  for (const d of (resp && resp.decommissions) || []) {
    byCode.decom.push(d.name);
    order.push({ name: d.name, rank: NODE_CODES.indexOf('decom') });
  }
  const total = nodes.filter(n => n.enabled).length + 1;
  const offline = byCode.offline.length;
  const fault = byCode.coreStopped.length + byCode.reloadErr.length + byCode.decom.length + byCode.ledger.length;
  const warn = byCode.unsynced.length + byCode.version.length;
  const names = [...new Map(order.sort((a, b) => a.rank - b.rank).map(x => [x.name, x])).keys()];
  return { total, online: total - offline, byCode, offline, fault, warn, red: offline + fault, yellow: warn, names };
}

// worstCode summarizeNodes 结果里最严重、且有服务器的那一类
export function worstCode(sum) {
  return NODE_CODES.find(c => sum.byCode[c] && sum.byCode[c].length) || '';
}

// ---- 上游巡检 ----

// upstreamBad 一条上游在真正用它的服务器上有失败的(待测、数据过期不算故障)
export const upstreamBad = r => !!r && !r.unused && (r.servers || []).some(x => x.state === 'fail');

// healthSummary 按上游分组(不是"上游 × 服务器"):
// rows = 在用的上游;ok = 全部正常的条数;bad = [{name, id, fails:[服务器名], total, err}](坏的台数多的在前,同数按名字);
// pending = 既没失败也没有正常结果的(待测 / 过期);servers = 出过故障的服务器;single = 整个巡检只涉及一台服务器。
// 从没巡检过(没有 lastRun)返回 null。
export function healthSummary(h) {
  if (!h || !h.lastRun) return null;
  const rows = (h.results || []).filter(x => !x.unused);
  const bad = [], servers = new Set(), all = new Set();
  let ok = 0, pending = 0;
  for (const r of rows) {
    const list = r.servers || [];
    list.forEach(x => all.add(x.name));
    const fails = list.filter(x => x.state === 'fail');
    if (fails.length) {
      bad.push({ id: r.id, name: r.name, fails: fails.map(x => x.name), total: list.length, err: (fails.find(x => x.error) || {}).error || '' });
      fails.forEach(x => servers.add(x.name));
    } else if (list.some(x => x.state === 'ok')) ok++;
    else pending++;
  }
  bad.sort((a, b) => b.fails.length - a.fails.length || String(a.name).localeCompare(String(b.name)));
  return { rows, ok, bad, pending, servers, single: all.size <= 1, lastRun: h.lastRun, interval: h.intervalMinutes };
}

// ---- 证书 ----

// certRank 续期失败 0 < 文件读不出 1 < 已过期 2 < 快到期 3
export function certRank(a, now = Date.now() / 1000) {
  if (a.trouble === 'failing') return 0;
  if (a.trouble === 'missing') return 1;
  return a.notAfter && a.notAfter < now ? 2 : 3;
}
export const sortCerts = (list, now = Date.now() / 1000) =>
  (list || []).slice().sort((a, b) => certRank(a, now) - certRank(b, now) || (a.notAfter || 0) - (b.notAfter || 0));
// certSig 提示条"知道了"记的签名:按后端给的顺序拼,和以前的格式一致(已经关掉的不会因为升级又冒出来)。
// 签名带上顶着临时证书的服务:关掉过「文件读不出来」后面板 / 订阅改用临时自签证书(重启之后),那是新情况,要再提示;
// 已过期的多一个 :x —— 后端对快到期和已过期都给 expiring(续期失败的过期了也还是 failing),不加的话关掉「快到期」后过期了也不再出来。
// 没有 fallback、没过期的条目签名和以前一样
export const certSig = (list, now = Date.now() / 1000) => (list || []).map(a =>
  `${a.nodeId || 0}:${a.name}:${a.trouble}${a.fallback ? '+' + a.fallback : ''}${a.notAfter && a.notAfter < now ? ':x' : ''}`).join(',');
export function certCounts(list, now = Date.now() / 1000) {
  const c = { failing: 0, missing: 0, expired: 0, expiring: 0 };
  for (const a of list || []) c[['failing', 'missing', 'expired', 'expiring'][certRank(a, now)]]++;
  return c;
}
// certLeftDays 剩余整天数(过期为负)
export const certLeftDays = (a, now = Date.now() / 1000) => Math.floor(((a.notAfter || 0) - now) / 86400);

// ---- 大陆连通 ----

// sortReach 不通的在前、部分运营商不通的在后(同类保持原顺序)
export const sortReach = list => (list || []).slice().sort((a, b) => (a.verdict === 'partial') - (b.verdict === 'partial'));
export const reachSig = list => (list || []).map(r => r.nodeId + ':' + r.verdict).join(',');
export const reachHard = list => (list || []).filter(r => r.verdict !== 'partial');

// reachStall 大陆连通检测自己是不是停摆了(GET status 的 reachState),返回原因代码,没停摆给 '':
// idle 巡检很久没跑完(超过 3 个间隔);error 取测点列表出错;failing 最近一轮全量巡检一台都没测成(额度用尽、令牌无效、接口出错),
// 或者有测成过、但已经超过 3 个间隔没测成;never 跑过却从没测成过。
// lastRun 每轮都往前走,不管测没测成,所以不能只看它。老后端没有 lastOk / tried 时只按 idle、error 判断
export function reachStall(rs, now = Date.now() / 1000) {
  if (!rs || !rs.auto || !(rs.minutes > 0)) return '';
  const lim = 3 * rs.minutes * 60;
  if (rs.lastRun && now - rs.lastRun > lim) return 'idle';
  if (rs.error) return 'error';
  const newer = 'lastOk' in rs;
  if (newer && rs.lastRun && !rs.lastOk) return 'never';
  if (rs.tried > 0 && rs.failed >= rs.tried) return 'failing';
  if (newer && rs.lastOk && now - rs.lastOk > lim) return 'failing';
  return '';
}

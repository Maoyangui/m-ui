// 官网的静态 Live Demo:同一套面板前端(web/assets 原样复制),只把 api.js 的传输层换成
// 内存里的演示数据。不发任何网络请求,没有后端,改动只在本页内存里,刷新即恢复。
//
// 演示数据来自 docs/screenshots/make.sh 跑起来的真实实例(fixtures.json),所以接口形状和生产一致;
// 这里只做两件事:按 "METHOD path" 找 fixture,以及把增删改落到内存里的几张表上。
import { setTransport, setQrUrl, ApiError } from './api.js';
import { toast } from './ui.js';
import { trErr } from './errmsg.js';

const SITE = 'https://maoyangui.github.io/m-ui/';
const GITHUB = 'https://github.com/Maoyangui/m-ui';
const data = await (await fetch('js/fixtures.json')).json();
const F = data.fixtures || {};
// 把导出那一刻的时间戳整体平移到现在:流量图窗口、"几分钟前同步"、到期剩余天数都保持导出当天的样子,演示不会随时间变旧
const GEN = data.generatedAt || Math.floor(Date.parse((data.generated || '2026-01-01') + 'T12:00:00Z') / 1000);
const DELTA = Math.floor(Date.now() / 1000) - GEN;
const TS_KEYS = new Set(['bootTime', 'expiry', 'createdAt', 'onlineAt', 'lastSeen', 'lastPush', 'claimBefore', 'checkedAt', 'dateTime', 'lastRun', 'ts', 'end', 't', 'start', 'shareAt', 'nextReset', 'updatedAt']);
const shift = (v, k = '') => Array.isArray(v) ? v.map(x => shift(x)) : (v && typeof v === 'object') ? Object.fromEntries(Object.entries(v).map(([kk, vv]) => [kk, shift(vv, kk)])) : (typeof v === 'number' && TS_KEYS.has(k) && v > 1e9 && v < 2e9) ? v + DELTA : v;
for (const k of Object.keys(F)) F[k] = shift(F[k]);
const clone = v => JSON.parse(JSON.stringify(v));
const fx = k => clone(F[k] ?? null);
// 演示里的故障要跟着操作走:处理掉了,提醒就得消失(以前巡检结果是导出时的一份死数据 ——
// 在上游页测出来是通的、甚至把那条上游删了,概况的「需要处理」还报它故障)。
// 上游的结果是按服务器记的:哪台服务器上有启用的线路用着这条上游,就有哪台的一份(和真面板同一口径 ——
// 线路自己选的上游、分流规则指向的、所在上游组的成员都算)。以前不管线路怎么改都只回「Tokyo」一台:
// 把两台服务器上的线路都指到同一条上游,延迟那一格还是一个点,看着像「只测了一台」。
function demoUpUsers(u) {
  const ids = new Set([u.id]);
  for (const g of S.upstreams) if (g.type === 'urltest' && ((g.options && g.options.outbounds) || []).includes(u.name)) ids.add(g.id);
  const rules = l => { let v = l.routeRules; if (typeof v === 'string') { try { v = JSON.parse(v); } catch { v = []; } } return Array.isArray(v) ? v : []; };
  const lines = S.lines.filter(l => l.enabled !== false && (ids.has(l.upstreamId) || rules(l).some(r => ids.has(Number(r.to)))));
  return (S.nodes.nodes || []).filter(n => n.enabled !== false && lines.some(l => { const on = lineIdsOf(l); return !on.length || on.includes(n.id); }));
}
// 一台服务器上的一次结果:一开始就不通的(S.upBroken)一直不通,直到在演示里编辑保存过(算修好了)或删掉;
// 通的话每台有自己稳定的延迟(各台离上游远近不同),每次再带一点抖动
function demoUpOne(u, n, prev) {
  const base = { nodeId: n.id, name: n.name, isLocal: !!n.isLocal, checkedAt: now() };
  if (S.upBroken.has(u.id)) return { ...base, state: 'fail', delayMs: 0, method: 'urltest', error: (prev && prev.error) || 'Head "http://www.gstatic.com/generate_204": EOF' };
  const via = u.type === 'urltest' ? 'via:' + ((u.options && u.options.outbounds) || [''])[0] : '';
  return { ...base, state: 'ok', delayMs: 46 + ((u.id * 37 + n.id * 53) % 9) * 13 + Math.floor(Math.random() * 12), method: via || 'urltest', error: '' };
}
// demoUpTest 测一条上游(面板的「测试」):在每台用它的服务器上各测一次并记进巡检结果;没有服务器用它时在本机测一次给个参考值,不记
function demoUpTest(u) {
  const users = demoUpUsers(u);
  const h = (S.health.results || []).find(r => r.id === u.id);
  const prev = new Map(((h && h.servers) || []).map(s => [s.nodeId, s]));
  const local = (S.nodes.nodes || []).find(n => n.isLocal) || { id: 1, name: 'Tokyo', isLocal: true };
  const out = (users.length ? users : [local]).map(n => demoUpOne(u, n, prev.get(n.id)));
  const row = { id: u.id, name: u.name, unused: !users.length, servers: users.length ? out : [] };
  if (h) Object.assign(h, row); else (S.health.results = S.health.results || []).push(row);
  return clone(out);
}
// ---- 流量:概况「流量走势」、用户详情的流量图、流量排行、流量明细都从下面这一份现算,彼此对得上 ----
// 以前用导出的时序:那是当时前端要的桶宽(1 小时按分钟、7 天 / 30 天按小时),而且只有导出前一天有量。前端后来把桶宽改成
// 5 分钟 / 半小时 / 1 小时 / 6 小时 / 1 天,键对不上就退回「随便哪一份」—— 选了 7 天、30 天,画出来的还是最近一小时的分钟柱,
// 横轴也跟着是分钟;排行的 7 天、30 天和 24 小时是同一组数。现在每个用户每小时的量现算(同一个小时每次算出来都一样):
//   日均(导出的 24 小时排行里各人的量)× 一天里的起伏(晚上高、凌晨低)× 这一天的起伏(周末多一点)× 一点噪声,
// 再按请求的范围和桶宽归到柱子里,对齐方式和后端 stats.Series 一样:日桶对齐到查看者时区的零点,小时级的桶对齐到当天内的
// 整数倍(6 小时桶 → 0 / 6 / 12 / 18 点),更细的按 UTC 取整;最后一根是正在走的这一格。演示里新建的用户没有历史,图是空的。
const demoNoise = (a, b) => { const x = Math.sin(a * 12.9898 + b * 78.233) * 43758.5453; return x - Math.floor(x); };
const DEMO_DIURNAL = (raw => { const s = raw.reduce((a, x) => a + x, 0); return raw.map(x => x * 24 / s); })(
  [0.55, 0.38, 0.27, 0.2, 0.17, 0.18, 0.26, 0.42, 0.62, 0.78, 0.88, 0.95, 1.05, 1, 0.95, 0.98, 1.05, 1.15, 1.3, 1.5, 1.75, 1.85, 1.6, 1]); // 当地 0 点到 23 点,平均为 1
const DEMO_DAILY = Object.fromEntries((F['GET stats/top?hours=24&limit=10'] || []).map((u, i) => {
  const bytes = (u.up || 0) + (u.down || 0);
  return [u.name, { bytes, upShare: (u.up || 0) / Math.max(1, bytes), seed: i + 1 }];
}));
// 排行、明细这些不带时区的请求按面板时区(设置里那个,图也是按它对齐的)算:和图上是同一段时间、同一个合计
const demoTzMin = () => { const at = now(), c = demoClock(at); return Math.round((Date.UTC(+c.year, c.month - 1, +c.day, +c.hour, +c.minute, +c.second) / 1000 - at) / 60); };
const demoBucket = hours => (hours <= 1 ? 300 : hours <= 6 ? 1800 : hours <= 24 ? 3600 : hours <= 168 ? 21600 : 86400); // 和前端 chart.js 的 bucketFor 一样
const demoHours = v => { const h = Number(v) || 0; return h > 0 && h <= 24 * 90 ? h : 24; };
// 一个用户在 [a, b) 这段时间里的 [上行, 下行] 字节。a、b 是秒;off 是时区的秒偏移(一天里的起伏按当地钟点);
// fine:桶比一小时细(5 分钟、半小时)时每格各有一点起伏,不是把这一小时平摊
function demoBytes(name, a, b, off, fine) {
  const p = DEMO_DAILY[name];
  if (!p || b <= a) return [0, 0];
  let up = 0, down = 0;
  for (let h = Math.floor(a / 3600); h * 3600 < b; h++) {
    const from = Math.max(a, h * 3600), to = Math.min(b, (h + 1) * 3600);
    const loc = h * 3600 + off, day = Math.floor(loc / 86400), wd = new Date(day * 86400000).getUTCDay();
    const dayK = (wd === 0 || wd === 6 ? 1.2 : 0.94) * (0.7 + 0.6 * demoNoise(p.seed * 31, day));
    const jitter = fine ? 0.55 + 0.9 * demoNoise(p.seed * 17, Math.floor(from / 300)) : 1;
    const bytes = p.bytes / 24 * DEMO_DIURNAL[((Math.floor(loc / 3600) % 24) + 24) % 24] * dayK * (0.72 + 0.56 * demoNoise(p.seed * 7.3, h)) * jitter * (to - from) / 3600;
    const share = Math.min(0.4, Math.max(0.04, p.upShare * (0.8 + 0.4 * demoNoise(p.seed * 3.1, h))));
    up += bytes * share; down += bytes * (1 - share);
  }
  return [Math.round(up), Math.round(down)];
}
// 这些用户合起来的时序(和后端 stats.Result 同一个形状):起点按桶对齐,n = 起点到现在跨了几个桶
function demoSeries(names, hours, span, tzMin) {
  const off = tzMin * 60, end = now();
  let start = end - hours * 3600;
  if (span >= 86400) start = Math.floor((start + off) / 86400) * 86400 - off;
  else if (span >= 3600) start -= (((start + off) % 86400) + 86400) % 86400 % span;
  else start -= start % span;
  const n = Math.floor((end - start) / span) + 1;
  const points = [];
  let totalUp = 0, totalDown = 0;
  for (let i = 0; i < n; i++) {
    const a = start + i * span, b = Math.min(a + span, end);
    let up = 0, down = 0;
    for (const name of names) { const [u, d] = demoBytes(name, a, b, off, span < 3600); up += u; down += d; }
    points.push({ t: a, up, down });
    totalUp += up; totalDown += down;
  }
  return { points, span, start, end, totalUp, totalDown };
}
// 流量排行:这段时间里各个用户的量(和图上同一段时间、同一份数),多的在前
function demoTop(hours) {
  const tz = demoTzMin();
  return S.users.map(u => { const r = demoSeries([u.name], hours, demoBucket(hours), tz); return { name: u.name, up: r.totalUp, down: r.totalDown }; })
    .filter(u => u.up + u.down > 0).sort((x, y) => (y.up + y.down) - (x.up + x.down));
}
// 流量明细(概况点「流量」那一格):用户排行就是流量排行那份;线路、服务器按固定的比例分同一个合计。
// 30 天那一档假装有一台副机没问到,演示「没归到线路」那一行和下面的说明
function demoBreakdown(hours) {
  const users = demoTop(hours).map(u => ({ ...u, owner: '' }));
  const total = users.reduce((a, u) => ({ up: a.up + u.up, down: a.down + u.down }), { up: 0, down: 0 });
  const split = (names, weights) => names.map((name, i) => ({ name, up: Math.round(total.up * weights[i]), down: Math.round(total.down * weights[i]) }));
  const nodes = (S.nodes.nodes || []).slice(0, 3);
  const nodeW = [0.46, 0.33, 0.21];
  const nodeRows = split(nodes.map(n => n.name), nodeW).map((x, i) => ({ ...x, local: !!nodes[i].isLocal }));
  const lineNames = S.lines.map(l => l.name).slice(0, 5);
  const lineW = [0.34, 0.27, 0.19, 0.12, 0.08].slice(0, lineNames.length);
  const short = hours >= 720 && nodes[2] ? nodeW[2] : 0; // 没问到的那台占的份额,从各线路里按比例扣掉
  const lines = split(lineNames, lineW.map(w => w * (1 - short)));
  return { hours, total, users, userCount: users.length, nodes: nodeRows, lines, linesMissing: short ? [nodes[2].name] : [] };
}
// 备份:演示里备份目录从三份每天 04:00 的定时备份和一份升级前备份起步;存一份多一行、删一份少一行,定时备份的设置存在 S.settings 里
// 文件名里的时间按面板时区写(和真面板一样),列表里的时间才对得上名字
const demoClock = at => { const o = {}; for (const p of new Intl.DateTimeFormat('en-GB', { timeZone: S.settings.timezone || 'Asia/Shanghai', hourCycle: 'h23', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }).formatToParts(new Date(at * 1000))) o[p.type] = p.value; return o; };
const demoBkName = at => { const c = demoClock(at); return `m-ui-${c.year}${c.month}${c.day}-${c.hour}${c.minute}${c.second}.zip`; };
function demoBackupList() {
  if (!S.backups) {
    const c = demoClock(now());
    const four = now() - ((Number(c.hour) - 4 + 24) % 24) * 3600 - Number(c.minute) * 60 - Number(c.second); // 面板时区里最近的一个 04:00
    S.backups = [0, 1, 2].map(i => ({ name: demoBkName(four - i * 86400), size: 4318000 - i * 21500, time: four - i * 86400 }));
    S.backups.push({ name: 'pre-upgrade-0.7.17-' + demoBkName(four - 6 * 86400 - 40000).slice(5), size: 4102000, time: four - 6 * 86400 - 40000 });
    if (S.settings.backupHour === undefined || S.settings.backupHour === '') Object.assign(S.settings, { backupHour: '4', backupKeep: S.settings.backupKeep || '7' });
  }
  const tf = v => v === true || v === 'true';
  return { files: S.backups, dir: '/etc/m-ui/backups', backupHour: S.settings.backupHour, backupKeep: S.settings.backupKeep, backupTelegram: tf(S.settings.backupTelegram), restorePending: false, lastErr: '', lastErrAt: 0, tgReady: tf(S.settings.tgEnabled) && !!S.settings.tgChatId };
}
// demoHealth 巡检结果:每条上游一行,行里是此刻用它的各台服务器(跟着线路的上游、分流规则、部署到的服务器变;名字跟着改名走)。
// 新出现的「上游 × 服务器」当场补一个结果 —— 真面板是保存线路十秒后自动补测一次
function demoHealth() {
  const old = new Map((S.health.results || []).map(r => [r.id, r]));
  S.health.results = S.upstreams.map(u => {
    const users = demoUpUsers(u);
    const prev = new Map(((old.get(u.id) || {}).servers || []).map(s => [s.nodeId, s]));
    return { id: u.id, name: u.name, unused: !users.length, servers: users.map(n => (prev.has(n.id) ? { ...prev.get(n.id), name: n.name, isLocal: !!n.isLocal } : demoUpOne(u, n))) };
  });
  return clone(S.health);
}
// 「端口被占」跟着线路走:那个中转入口改了端口、暂停或删了,线路停了或删了,服务器上这一条就没有了
function demoSyncBlocked() {
  for (const n of S.nodes.nodes || []) {
    const st = n.status;
    if (!st || !Array.isArray(st.blocked)) continue;
    st.blocked = st.blocked.filter(b => S.lines.some(l => l.enabled !== false && (l.forwards || []).some(f => f.via === n.id && Number(f.port) === Number(b.port) && !f.off)));
  }
}

// 内存状态:从 fixture 复制一份,增删改都落在这里
const S = {
  settings: fx('GET settings') || {},
  lines: fx('GET lines') || [],
  upstreams: fx('GET upstreams') || [],
  users: fx('GET users') || [],
  plans: fx('GET plans') || [],
  nodes: (fx('GET nodes') || { nodes: [] }),
  exts: fx('GET exts') || [],
  resellers: fx('GET resellers') || [],
  onlines: fx('GET onlines') || { users: [], lines: [], upstreams: [], connCounts: {} },
  // 上游巡检结果:从导出的那一份起步,之后跟着演示里的操作变(见 demoUpTest / demoHealth)
  health: fx('GET upstreams/health') || { intervalMinutes: 10, lastRun: 0, results: [] },
};
S.logsCleared = {}; // 日志页点过「清空」的那几类
// 导出时就不通的上游:在演示里编辑保存过(算修好了)或删掉之前,测试和巡检一直报不通
S.upBroken = new Set((S.health.results || []).filter(r => (r.servers || []).length && r.servers.every(s => s.state === 'fail')).map(r => r.id));
// 让演示像在跑:两个用户在线,各带文档保留段里的 IP
const online = { 'demo-alice': ['203.0.113.5', '198.51.100.7'], 'demo-bob': ['192.0.2.44'] };
for (const u of S.users) if (online[u.name]) { u.onlineIps = online[u.name]; u.onlineAt = Math.floor(Date.now() / 1000) - 60; }
S.onlines.users = Object.keys(online).filter(n => S.users.some(u => u.name === n));
S.onlines.connCounts = Object.fromEntries(S.onlines.users.map((n, i) => [n, 3 + i * 2]));
S.onlines.lines = S.lines.slice(0, 2).map(l => l.name);
for (const n of S.nodes.nodes || []) if (n.status) n.status.uptime = (n.status.uptime || 0) + DELTA; // 副机运行时长随导出日期累加
// 磁盘占用:第二台副机快满了(服务器页的 💾 标记、概况「需要处理」、清理磁盘都看得到),其余宽裕
{ const G = 1073741824, at = Math.floor(Date.now() / 1000); let k = 0;
  for (const n of S.nodes.nodes || []) { if (n.isLocal || !n.status) continue; k++;
    n.status.disk = k === 2 ? { path: '/etc/m-ui', total: 9.6 * G, used: 8.8 * G, free: 0.8 * G, logs: 4.1 * G, cache: 0.3 * G, swap: 4 * G, opsAgent: true, at }
      : { path: '/etc/m-ui', total: 40 * G, used: (9 + 4 * k) * G, free: (31 - 4 * k) * G, logs: 0.4 * G, cache: 0.2 * G, swap: 2 * G, at }; } }

// 服务器的流量额度与到期(和后端 web/node_quota.go 同一口径,只在本页内存里):法兰克福那台是按量计费的,
// 每月 1 日重置、本期用到八成多(用量条发黄);新加坡那台设了到期时间。改了额度当场判:用完 / 到期的按「自动停用」开关
// 停用整台或只停代理服务,重置、加额度、续期后自己恢复。
const demoQuotaNext = (mode, days, day, at = now()) => {
  if (mode === 'days') return days > 0 ? at + days * 86400 : 0;
  if (mode !== 'month' || day < 1 || day > 31) return 0;
  const d = new Date(at * 1000);
  const pick = (y, m) => { const last = new Date(Date.UTC(y, m + 1, 0)).getUTCDate(); return Date.UTC(y, m, Math.min(day, last)) / 1000 - 8 * 3600; }; // 面板时区(演示里是 UTC+8)的 0 点
  let next = pick(d.getUTCFullYear(), d.getUTCMonth());
  if (next <= at) next = pick(d.getUTCFullYear(), d.getUTCMonth() + 1);
  return next;
};
const demoQuotaState = q => (q.expiry > 0 && now() >= q.expiry ? 'expired' : q.volume > 0 && q.used >= q.volume ? 'traffic' : '');
// 判一次:状态变了记下时间;该停用的停用(记 autoOff)、该恢复的恢复;只停服务的打 suspended
function demoQuotaJudge(n) {
  const q = n.quota;
  if (!q) return;
  if (q.resetMode && q.nextReset > 0 && now() >= q.nextReset) { q.total += 0; q.used = q.up = q.down = 0; q.periodStart = now(); q.nextReset = demoQuotaNext(q.resetMode, q.resetDays, q.resetDay); }
  const state = demoQuotaState(q);
  if (state !== q.state) { q.state = state; q.stateAt = now(); }
  const off = !!state && q.autoDisable && !n.isLocal;
  if (off && n.enabled !== false) { n.enabled = false; q.autoOff = true; }
  else if (!off && q.autoOff) { n.enabled = true; q.autoOff = false; }
  n.suspended = !!state && !off;
}
function demoQuotaApply(n, p) {
  const was = n.quota || {};
  const q = { volume: 0, used: 0, up: 0, down: 0, total: 0, periodStart: now(), expiry: 0, resetMode: '', resetDays: 0, resetDay: 0, nextReset: 0, autoDisable: !n.isLocal, state: '', stateAt: 0, autoOff: false, tracked: true, ...was };
  const pick = k => (p && p[k] !== undefined && p[k] !== null);
  if (pick('volume')) q.volume = Number(p.volume);
  if (pick('expiry')) q.expiry = Number(p.expiry);
  if (pick('resetMode')) q.resetMode = String(p.resetMode || '').trim();
  if (pick('resetDays')) q.resetDays = Number(p.resetDays);
  if (pick('resetDay')) q.resetDay = Number(p.resetDay);
  if (pick('autoDisable')) q.autoDisable = !!p.autoDisable && !n.isLocal;
  if (!(q.volume >= 0) || q.volume > 2 ** 60) throw demoErr(400, '流量额度填 0(不限)或一个正数');
  if (pick('expiry') && q.expiry > 0 && q.expiry <= now() && q.expiry !== (was.expiry || 0)) throw demoErr(400, '到期时间已经过了:填一个将来的时间,或者留空不设');
  if (q.resetMode === '') { q.resetDays = 0; q.resetDay = 0; }
  else if (q.resetMode === 'days') { if (!(q.resetDays >= 1 && q.resetDays <= 3650)) throw demoErr(400, '重置周期填 1–3650 天'); q.resetDay = 0; }
  else if (q.resetMode === 'month') { if (!(q.resetDay >= 1 && q.resetDay <= 31)) throw demoErr(400, '每月重置的日子填 1–31'); q.resetDays = 0; }
  else throw demoErr(400, '重置周期只能是不重置、按天数或每月');
  const cycleChanged = q.resetMode !== (was.resetMode || '') || q.resetDays !== (was.resetDays || 0) || q.resetDay !== (was.resetDay || 0);
  if (!q.resetMode) q.nextReset = 0;
  else if (cycleChanged || !(q.nextReset > 0)) q.nextReset = demoQuotaNext(q.resetMode, q.resetDays, q.resetDay);
  return q;
}
{ const G = 1073741824, at = Math.floor(Date.now() / 1000), fra = (S.nodes.nodes || []).find(n => n.name === 'Frankfurt'), sgp = (S.nodes.nodes || []).find(n => n.name === 'Singapore'); // now() 在后面才定义
  const base = { used: 0, up: 0, down: 0, total: 0, periodStart: at - 17 * 86400, expiry: 0, resetMode: '', resetDays: 0, resetDay: 0, nextReset: 0, autoDisable: true, state: '', stateAt: 0, autoOff: false, tracked: true };
  if (fra) fra.quota = { ...base, volume: 500 * G, used: 412.6 * G, up: 38.4 * G, down: 374.2 * G, total: 2318.9 * G, resetMode: 'month', resetDay: 1, nextReset: demoQuotaNext('month', 0, 1, at) };
  if (sgp) sgp.quota = { ...base, volume: 0, used: 96.3 * G, up: 9.1 * G, down: 87.2 * G, total: 96.3 * G, periodStart: at - 40 * 86400, expiry: at + 23 * 86400 + 3600 };
}

// 外部 API 的密钥(主面板 admin、代理面板 self 各一套;只在本页内存里)。第一把是默认密钥,和别的在同一张表里
const API_PERMS = ['users.read', 'users.create', 'users.update', 'users.delete', 'billing', 'plans.read', 'panel.read'];
{ const at = Math.floor(Date.now() / 1000);
  S.apiKeys = {
    admin: [
      { id: 1, resellerId: 0, name: '', isDefault: true, token: 'demo-defaulttoken0for0the0demo0only', perms: API_PERMS.join(','), enabled: true, ipAllow: false, ipList: '', createdAt: at - 86400 * 40, lastUsedAt: at - 95, lastUsedIp: '203.0.113.80' },
      { id: 2, resellerId: 0, name: 'telegram-bot', isDefault: false, token: 'demo-telegrambot0key0for0the0demo0only', perms: 'users.read,panel.read', enabled: true, ipAllow: true, ipList: '198.51.100.23, 198.51.100.24', createdAt: at - 86400 * 9, lastUsedAt: at - 420, lastUsedIp: '198.51.100.23' },
    ],
    self: [],
  }; }
function demoApiKeys(scope, method, id, sub, body) {
  const list = S.apiKeys[scope];
  const view = k => ({ ...k, permList: API_PERMS.filter(p => k.perms.split(',').includes(p)) });
  const fill = (k, b) => {
    if (!b) return k;
    if (b.name != null) k.name = String(b.name).trim() || (k.isDefault ? '' : k.name);
    if (Array.isArray(b.perms)) k.perms = API_PERMS.filter(p => b.perms.includes(p)).join(',');
    if (b.enabled != null) k.enabled = !!b.enabled;
    if (b.ipAllow != null) k.ipAllow = !!b.ipAllow;
    if (b.ipList != null) k.ipList = String(b.ipList).split(/[\s,;，]+/).filter(Boolean).join(', ');
    return k;
  };
  if (!id && method === 'POST') { const k = fill({ id: nextId(list), resellerId: 0, name: 'key', isDefault: false, token: 'demo-' + rnd(32), perms: 'users.read', enabled: true, ipAllow: false, ipList: '', createdAt: Math.floor(Date.now() / 1000), lastUsedAt: 0, lastUsedIp: '' }, body); list.push(k); return view(k); }
  const k = list.find(x => String(x.id) === String(id));
  if (!k) return { error: 'not found' };
  if (sub === 'logs') { // 调用日志的设置(记不记、留几天)与一键清空
    if (method === 'DELETE') { const n = demoLogsOf(k).length; k.logs = []; return { ok: true, deleted: n }; }
    if (body && body.days != null) { if (![1, 7, 30, 90].includes(Number(body.days))) throw demoErr(400, '调用日志只能留 1、7、30 或 90 天'); k.logDays = Number(body.days); }
    if (body && body.off != null) k.logOff = !!body.off;
    return ok();
  }
  if (method === 'DELETE') { list.splice(list.indexOf(k), 1); return ok(); }
  if (sub === 'rotate') { k.token = 'demo-' + rnd(32); return view(k); }
  return view(fill(k, body));
}

const nextId = list => list.reduce((m, x) => Math.max(m, x.id || 0), 0) + 1;
// 调用日志(演示):默认密钥是商城在用(续费、建号、换套餐失败一次……),telegram-bot 是机器人(按订阅地址找人、查用户,没权限的被拒)
function demoLogsOf(k) {
  if (k.logs) return k.logs;
  const at = Math.floor(Date.now() / 1000), ip = k.lastUsedIp || '203.0.113.80';
  const names = (S.users || []).map(u => u.name).concat(['demo-alice', 'demo-bob', 'demo-charlie']);
  const mk = (i, method, path, action, target, o = {}) => ({ id: 100 * k.id + 50 - i, keyId: k.id, at: at - 95 - i * 2400 - (i * 397) % 900, ip, method, path, query: '', body: '', status: 200, ms: 24 + (i * 37) % 160, action, target, err: '', ...o });
  k.logs = k.isDefault ? [
    mk(0, 'POST', '/api/v1/users/' + names[0] + '/renew', 'user.renew', names[0], { body: '{"periods":1}' }),
    mk(1, 'POST', '/api/v1/users', 'user.create', 'shop-1024', { body: '{"name":"shop-1024","plan":"Pro","days":30}' }),
    mk(2, 'GET', '/api/v1/users/' + names[1] + '/traffic', 'user.traffic', names[1], { query: 'hours=168' }),
    mk(3, 'POST', '/api/v1/users/' + names[2] + '/change', 'user.change', names[2], { body: '{"plan":"Team","mode":"replace"}', status: 400, err: '套餐不存在' }),
    mk(4, 'GET', '/api/v1/users', 'users.list', '', { query: 'q=shop&limit=50' }),
    mk(5, 'POST', '/api/v1/users/batch', 'users.batch', '5', { body: '{"prefix":"trial","count":5,"plan":"Trial"}' }),
    mk(6, 'POST', '/api/v1/users/' + names[1] + '/rotate', 'user.rotate', names[1]),
    mk(7, 'GET', '/api/v1/ping', 'ping', ''),
  ] : [
    mk(0, 'POST', '/api/v1/users/lookup', 'user.lookup', '', { body: '{"sub":"***"}' }),
    mk(1, 'GET', '/api/v1/users/' + names[0], 'user.get', names[0]),
    mk(2, 'GET', '/api/v1/overview', 'overview', ''),
    mk(3, 'POST', '/api/v1/users/' + names[1] + '/kick', 'user.kick', names[1], { status: 403, err: '这把密钥没有「修改用户」权限' }),
    mk(4, 'GET', '/api/v1/servers', 'servers', '', { ip: '203.0.113.99', status: 403, err: '来源 IP 不在这把密钥允许的范围里' }),
  ];
  return k.logs;
}
function demoKeyLogs(scope, id, query) {
  const k = (S.apiKeys[scope] || []).find(x => String(x.id) === String(id));
  if (!k) throw demoErr(404, '密钥不存在');
  const q = new URLSearchParams(query || ''), limit = Number(q.get('limit')) || 50, before = Number(q.get('before')) || 0;
  const all = demoLogsOf(k).slice().sort((a, b) => b.id - a.id), rest = before ? all.filter(x => x.id < before) : all;
  return { items: rest.slice(0, limit), more: rest.length > limit, total: all.length, logOff: !!k.logOff, logDays: k.logDays || 7, dayChoices: [1, 7, 30, 90] };
}
// 外部订阅展开:节点明细 / 逐台测速 / 大陆连通 / 隐藏改名 / 添加为上游(GET 与 POST 都走这里)。
// 和真面板一样按节点指纹(key)认节点;隐藏 / 改名只收增量,带版本号。
function extNodes(id, seg, method, body) {
  let servers = ((S.nodes && S.nodes.nodes) || (Array.isArray(S.nodes) ? S.nodes : [])).filter(n => n.enabled !== false).map(n => ({ id: n.id, name: n.name, isLocal: !!n.isLocal }));
  if (!servers.length) servers = [{ id: 1, name: '主机', isLocal: true }, { id: 2, name: '副机', isLocal: false }];
  const nodes = demoExtNodes(id);
  const ext = S.exts.find(e => e.id === id) || {};
  ext.hiddenSet = ext.hiddenSet || {};
  ext.renameSet = ext.renameSet || {};
  ext.prefsRev = ext.prefsRev || 0;
  const annotate = () => {
    const words = String(ext.exclude || '').split(',').map(s => s.trim()).filter(Boolean);
    for (const n of nodes) {
      n.hidden = n.keyword = false;
      if (words.some(w => n.name.includes(w))) { n.hidden = true; n.keyword = true; }
      else if (ext.hiddenSet[n.key]) n.hidden = true;
      n.rename = ext.renameSet[n.key] || undefined;
    }
    ext.nodeCount = nodes.filter(n => !n.hidden).length;
    ext.nodeTotal = nodes.length;
  };
  annotate();
  const payload = () => ({ nodes, servers, fetchedAt: now() - 600, sig: 'demo', rev: ext.prefsRev, stale: 0 });
  if (!seg[3]) return payload();
  if (seg[3] === 'prefs') {
    if (!body || body.rev !== ext.prefsRev) throw new ApiError(409, '节点的隐藏 / 改名设置已在别处修改,请刷新后重试');
    for (const x of body.hide || []) ext.hiddenSet[x.key] = true;
    for (const x of body.show || []) delete ext.hiddenSet[x.key];
    for (const x of body.rename || []) { if (x.to) ext.renameSet[x.key] = x.to; else delete ext.renameSet[x.key]; }
    ext.prefsRev++;
    annotate();
    return payload();
  }
  const keysOf = b => (b && b.keys && b.keys.length) ? b.keys : nodes.filter(n => !n.hidden || (b && b.includeHidden)).map(n => n.key);
  if (seg[3] === 'reach' && !seg[4]) { S.reachKeys = keysOf(body); return { job: 'demo-reach-' + id, keys: S.reachKeys }; }
  if (seg[3] === 'reach') {
    // 演示用:第一个被墙,其余通;UDP 协议只 ping
    const results = {};
    (S.reachKeys || []).forEach((k, i) => {
      const n = nodes.find(x => x.key === k) || {};
      const udp = ['hysteria2', 'tuic'].includes(n.type);
      results[k] = { state: 'done', verdict: i === 0 ? 'blocked' : 'ok', note: udp ? 'udpPing' : '', groups: [{ key: 'ct', ok: i === 0 ? 0 : 3, total: 3 }, { key: 'cu', ok: i === 0 ? 0 : 3, total: 3 }, { key: 'cm', ok: i === 0 ? 0 : 3, total: 3 }] };
    });
    return { done: true, results };
  }
  if (seg[3] === 'test' && !seg[4]) { S.testKeys = keysOf(body); return { job: 'demo-' + id, keys: S.testKeys }; }
  if (seg[3] === 'test') {
    const results = {};
    for (const k of S.testKeys || []) {
      results[k] = {};
      for (const s of servers) results[k][s.id] = Math.random() < 0.8 ? { state: 'ok', delayMs: 60 + Math.floor(Math.random() * 300), method: 'urltest', at: now() } : { state: 'fail', error: 'timeout', at: now() };
    }
    const total = (S.testKeys || []).length * servers.length;
    return { done: true, total, finished: total, results };
  }
  if (seg[3] === 'add-upstream') return { results: ((body && body.items) || []).map((x, i) => ({ key: x.key, name: x.name, ok: true, id: 900 + i })) };
  return ok();
}
// 演示用的外部订阅节点:三种协议各两个,带完整参数
const demoExtNodes = id => [
  ['trojan', '官网 www.example.net', 'www.example.net', 443, [['password', 'x' + id]]],
  ['hysteria2', 'HK-1', 'hk1.example.net', 443, [['password', 'p' + id + 'a'], ['sni', 'hk1.example.net'], ['skip-cert-verify', 'false'], ['alpn', '["h3"]']]],
  ['hysteria2', 'HK-2', 'hk2.example.net', 443, [['password', 'p' + id + 'b'], ['sni', 'hk2.example.net'], ['up', '100'], ['down', '500']]],
  ['tuic', 'JP-1', 'jp1.example.net', 8443, [['uuid', '3f2a1c9e-0000-4000-8000-00000000' + String(id).padStart(4, '0')], ['password', 'tp' + id], ['sni', 'jp1.example.net'], ['congestion-controller', 'bbr'], ['udp-relay-mode', 'native']]],
  ['tuic', 'JP-2', 'jp2.example.net', 8443, [['uuid', '3f2a1c9e-0000-4000-8000-00000000' + String(id + 1).padStart(4, '0')], ['password', 'tq' + id], ['sni', 'jp2.example.net']]],
  ['anytls', 'SG-1', 'sg1.example.net', 8444, [['password', 'ap' + id], ['sni', 'sg1.example.net'], ['client-fingerprint', 'chrome']]],
  ['anytls', 'US-1', 'us1.example.net', 8444, [['password', 'aq' + id], ['sni', 'us1.example.net'], ['skip-cert-verify', 'true']]],
].map(([type, name, server, port, extra], i) => ({
  index: i, key: 'd' + id + 'k' + i, name, type, server, port, upstream: true,
  fields: [['name', name], ['type', type], ['server', server], ['port', String(port)], ...extra].map(([k, v]) => ({ k, v })),
  link: type + '://' + encodeURIComponent(extra[0][1]) + '@' + server + ':' + port + '?sni=' + server + '#' + encodeURIComponent(name),
}));
const now = () => Math.floor(Date.now() / 1000);
const subBase = (S.settings.webDomain || 'panel.example.com');
const subUrl = u => `https://${subBase}:2056/sub/${u.subToken || u.name}`;
const notFound = () => { throw new ApiError(404, 'not found'); };

// 线路分流规则的"查域名":真面板由服务器去 v2fly 域名库查,演示只认几个常见应用(各取几个代表域名)
const DEMO_APPS = {
  douyin: ['douyin.com', 'douyinvod.com', 'douyincdn.com', 'douyinpic.com', 'amemv.com', 'snssdk.com', 'zijieapi.com'],
  netflix: ['netflix.com', 'netflix.net', 'nflxvideo.net', 'nflximg.net', 'nflxext.com', 'nflxso.net', 'fast.com'],
  openai: ['openai.com', 'chatgpt.com', 'oaistatic.com', 'oaiusercontent.com', 'sora.com'],
  tiktok: ['tiktok.com', 'tiktokv.com', 'tiktokcdn.com', 'byteoversea.com'],
  wechat: ['wechat.com', 'servicewechat.com', 'weixin.com', 'qpic.cn', 'qlogo.cn', 'tenpay.com', 'weixin.qq.com', 'wx.qq.com'],
  gmail: ['gmail.com', 'googlemail.com', 'mail.google.com'],
};
const DEMO_PART = { wechat: 'tencent', gmail: 'google' }; // 没有单独列表、从大厂列表里挑的
const DEMO_ALIAS = { '抖音': 'douyin', '奈飞': 'netflix', '网飞': 'netflix', chatgpt: 'openai', gpt: 'openai', '微信': 'wechat', weixin: 'wechat', '谷歌邮箱': 'gmail' };
const demoApps = names => (names || []).map(q => {
  const k = String(q).trim().toLowerCase(), list = DEMO_ALIAS[k] || k;
  if (DEMO_APPS[list]) return DEMO_PART[list] ? { query: q, list: DEMO_PART[list], part: true, suffix: DEMO_APPS[list] } : { query: q, list, suffix: DEMO_APPS[list] };
  return { query: q, error: '没找到这个应用', suggest: Object.keys(DEMO_APPS).filter(n => n.includes(k.slice(-4)) || k.includes(n)) };
});
const ok = (extra = {}) => ({ ok: '1', ...extra });
const rnd = n => Array.from(crypto.getRandomValues(new Uint8Array(n)), b => 'abcdefghijklmnopqrstuvwxyz0123456789'[b % 36]).join('');
// 规则页的演示数据(导出实例还没有这一页):一条晚高峰时段规则、一条突发规则,alice 正被突发规则限着
S.rules = [
  { id: 1, name: '晚高峰 Evening peak', enabled: true, kind: 'schedule', allUsers: true, userIds: [], resellerIds: [], days: '1,2,3,4,5', start: '19:00', end: '23:00', windowMin: 0, thresholdGb: 0, penaltyMin: 0, upMbps: 0, downMbps: 30, tightenOnly: true, remark: '', sort: 0, createdAt: now() - 86400 * 20 },
  { id: 2, name: '突发惩罚 Burst penalty', enabled: true, kind: 'burst', allUsers: false, userIds: [1, 2], resellerIds: [], days: '', start: '', end: '', windowMin: 10, thresholdGb: 1, penaltyMin: 20, upMbps: 0, downMbps: 20, tightenOnly: true, remark: '', sort: 0, createdAt: now() - 86400 * 3 },
];
S.limitStates = [{ id: 1, ruleId: 2, ruleName: '突发惩罚 Burst penalty', userId: 1, userName: 'demo-alice', upMbps: 0, downMbps: 20, tightenOnly: true, since: now() - 300, until: now() + 900, reason: '10 分钟内 1.2 GB' }];
// 选了套餐的规则:Basic 套餐的用户(现有和以后的)晚上限速
S.rules.push({ ...S.rules[0], id: 3, name: 'Basic 套餐晚间 Basic plan nights', allUsers: false, planIds: [1], downMbps: 50 });
for (const u of S.users) if (u.name === 'demo-alice' || u.name === 'demo-charlie') u.planId = 1; else if (u.name === 'demo-bob') u.planId = 2;
const planView = p => ({ ...p, userCount: S.users.filter(u => u.planId === p.id).length, ruleIds: S.rules.filter(r => (r.planIds || []).includes(p.id)).map(r => r.id), resellerName: '' });
const setPlanRules = (planId, ruleIds) => { if (!ruleIds) return; for (const r of S.rules) { const on = ruleIds.includes(r.id); r.planIds = (r.planIds || []).filter(x => x !== planId); if (on) r.planIds.push(planId); } };
// 证书库(导出实例只有一张自签):一张域名证书给线路默认、面板、订阅用,一张泛域名给单独选了它的线路用,一张自签闲着
const DAY = 86400;
// 续期状态和后端 CertView 一样:能不能自动续、为什么、从哪天起续
const renewOf = (source, method, days, self) => {
  if (self || source === 'selfsign') return { renewable: false, reason: 'selfsign', renewBefore: 30 * DAY };
  if (source !== 'acme') return { renewable: false, reason: 'external', renewBefore: 30 * DAY };
  if (method === 'manual') return { renewable: false, reason: 'manual', renewBefore: 30 * DAY };
  const at = now() + (days - 30) * DAY;
  return { renewable: true, renewBefore: 30 * DAY, renewAt: at, nextCheckAt: at, overdue: days <= 30, lastRenewOk: now() - 30 * DAY };
};
const certOf = (id, name, source, method, names, days, self, lineNames) => ({
  id, name, source, method, autoRenew: source === 'acme' && method !== 'manual', staging: false, createdAt: now() - 30 * DAY, ...renewOf(source, method, days, self),
  certFile: '/etc/m-ui/certs/' + name.replace('*', '_') + '.crt', keyFile: '/etc/m-ui/certs/' + name.replace('*', '_') + '.key',
  info: { exists: true, path: '/etc/m-ui/certs/' + name.replace('*', '_') + '.crt', subject: names[0], dnsNames: names, ips: null, issuer: self ? names[0] : "Let's Encrypt", notBefore: new Date((now() - 30 * DAY) * 1000).toISOString(), notAfter: new Date((now() + days * DAY) * 1000).toISOString(), daysLeft: days, selfSigned: !!self },
  forLines: false, forPanel: false, forSub: false, lineNames,
});
S.certs = [
  certOf(1, 'panel.example.com', 'acme', 'http', ['panel.example.com'], 61, false, []),
  certOf(2, '*.example.net', 'acme', 'manual', ['*.example.net', 'example.net'], 74, false, ['Tokyo-HY2']),
  certOf(3, 'self-signed', 'selfsign', '', ['panel.example.com'], 3640, true, []),
];
S.certUse = { lines: S.certs[0].certFile, panel: S.certs[0].certFile, sub: S.certs[0].certFile };
const syncCertUse = () => { for (const c of S.certs) { c.forLines = c.certFile === S.certUse.lines; c.forPanel = c.certFile === S.certUse.panel; c.forSub = c.certFile === S.certUse.sub; } };
syncCertUse();
// 线路选证书的抽屉按服务器列:本机三张都有,副机没有泛域名那张
const briefs = list => JSON.stringify(list.map(c => ({ name: c.name, domains: c.info.dnsNames, selfSigned: c.info.selfSigned, notAfter: now() + c.info.daysLeft * DAY, default: c.forLines })));
for (const n of S.nodes.nodes || []) n.certs = briefs(n.isLocal ? S.certs : S.certs.filter(c => c.id !== 2));
const tlsOf = l => { try { return typeof l.tls === 'string' ? JSON.parse(l.tls || '{}') : (l.tls || {}); } catch { return {}; } };
for (const l of S.lines) if (l.name === 'Tokyo-HY2' && tlsOf(l).mode === 'cert') l.tls = JSON.stringify({ ...tlsOf(l), cert: '*.example.net' });
// 外部订阅(导出实例没有):关键词已排除带"官网"的,展开能隐藏、改名、测大陆连通
if (!S.exts.length) S.exts.push({ id: 1, name: 'Demo Sub', type: 'sub', value: 'https://sub.example.net/demo', prefix: '', enabled: true, sort: 1, remark: '', lastFetch: now() - 600, lastError: '', nodeCount: 6, nodeTotal: 7, exclude: '官网' });
// 第一个用户分到这个外部订阅:用户表单「外部节点」那一行右边是「单独成组」开关,详情里列着它
{ const u0 = S.users[0]; if (u0 && !(u0.extIds || []).length) { u0.extIds = [S.exts[0].id]; u0.extNoGroupIds = []; } }
// 演示「上游跟随外部节点」:LA-streaming 当作是从外部订阅里加来的,两小时前全部测不通、已经跟着订阅换过一次端口
{
  const la = S.upstreams.find(u => u.name === 'LA-streaming'), e0 = (S.exts || [])[0];
  if (la && e0) la.source = { extId: e0.id, extName: e0.name, node: 'US-1', note: 'followed', detail: '203.0.113.40:8000 → 203.0.113.40:8388', triedAt: Math.floor(Date.now() / 1000) - 7200, followedAt: Math.floor(Date.now() / 1000) - 7200 };
}
const ruleView = r => ({ ...r, targetCount: r.allUsers ? S.users.filter(u => u.enabled).length : new Set([...(r.userIds || []), ...S.users.filter(u => (r.resellerIds || []).includes(u.resellerId) || (r.planIds || []).includes(u.planId)).map(u => u.id)]).size, activeCount: S.limitStates.filter(s => s.ruleId === r.id).length });

// 换套餐 / 续 N 期的演示版:和面板同一套规则的简化(不处理首次连接计时),只改内存;套餐记录也记在内存里
const GB = 1073741824;
for (const p of S.plans) if (p.price === undefined) p.price = ({ Basic: 5, Pro: 20 })[p.name] || 0; // 演示用的参考价
S.planOps = [];
const planStateOf = (u, plan) => ({ planId: u.planId || 0, plan: plan ? plan.name : '', volume: u.volume || 0, used: (u.up || 0) + (u.down || 0), expiry: u.expiry || 0,
  activateDays: u.activateDays || 0, autoReset: !!u.autoReset, resetDays: u.resetDays || 0, nextReset: u.nextReset || 0, deviceLimit: u.deviceLimit || 0,
  speedUp: u.speedUp || 0, speedDown: u.speedDown || 0, enabled: u.enabled !== false, disabledReason: u.disabledReason || '', lineRefs: (u.lineIds || []).map(id => ({ lineId: id })) });
const demoErr = (status, zh) => new ApiError(status, trErr(zh)); // 英文界面也看得懂
const demoWarn = (code, args, block) => ({ code, text: code, args, block: !!block });
function demoPlanOp(u, p, op, n, usage, dry, remark) {
  const t = now(), v = Math.round((p.volumeGb || 0) * GB), used = (u.up || 0) + (u.down || 0);
  const old = S.plans.find(x => x.id === u.planId);
  if (!usage || usage === 'auto') usage = op === 'stack' && u.volume ? 'keep' : 'reset';
  if (op === 'renew') usage = 'reset'; else if (op === 'extend') usage = 'keep';
  let credit = null, creditSecs = 0;
  if (op === 'prorate') {
    if (!old) throw demoErr(400, '用户没有所属套餐,不能按价值折算');
    if (old.id === p.id) throw demoErr(400, '目标套餐就是当前套餐,不能折算:要从现在重新开始用 replace,要带结转续费用 stack');
    if (!(old.price > 0) || !(p.price > 0)) throw demoErr(400, `套餐「${!(old.price > 0) ? old.name : p.name}」没填参考价,不能按价值折算`);
    const ratio = old.days && u.expiry ? Math.max(0, u.expiry - t) / (old.days * DAY) : 0;
    const periods = ratio * old.price / p.price;
    creditSecs = Math.round(periods * (p.days || 0) * DAY);
    credit = { fromPrice: old.price, toPrice: p.price, ratio, value: ratio * old.price, periods, seconds: creditSecs, bytes: 0 };
  }
  const nx = { ...u, planId: p.id, deviceLimit: p.deviceLimit || 0, speedUp: p.speedUp || 0, speedDown: p.speedDown || 0, autoReset: !!p.autoReset, resetDays: p.resetDays || 0, enabled: true, disabledReason: '' };
  const fromNow = op === 'replace' || op === 'prorate';
  // 叠加:已过期的旧订阅不结转;目标不限期 / 不限量时,原来有限的到期 / 配额照旧保留(和面板一样)
  const expired = !u.activateDays && u.expiry > 0 && u.expiry <= t, vleft = u.volume ? Math.max(0, u.volume - used) : 0;
  const keepEnd = op === 'stack' && !p.days && !expired && (u.expiry || u.activateDays), keepVol = op === 'stack' && !v && u.volume && !expired;
  nx.expiry = keepEnd ? u.expiry : !p.days ? 0 : (fromNow ? t : Math.max(t, u.expiry || 0)) + n * p.days * DAY + creditSecs;
  nx.volume = keepVol ? (usage === 'reset' ? Math.max(1, vleft) : u.volume) : !v ? 0 : p.autoReset || op === 'extend' ? v
    : op === 'stack' && u.volume ? (expired ? (usage === 'reset' ? 0 : used) : (usage === 'reset' ? vleft : u.volume)) + n * v : n * v;
  if (usage === 'reset') { nx.up = 0; nx.down = 0; }
  nx.nextReset = p.autoReset ? (usage === 'keep' && u.nextReset > t ? Math.min(u.nextReset, t + p.resetDays * DAY) : t + p.resetDays * DAY) : 0;
  if (p.lineIds && p.lineIds.length) nx.lineIds = [...p.lineIds];
  const warnings = [];
  const nUsed = (nx.up || 0) + (nx.down || 0);
  if (nx.volume && usage === 'keep' && nUsed >= nx.volume) warnings.push(demoWarn('over_quota', { used: nUsed, volume: nx.volume, nextReset: nx.nextReset || 0 }, true));
  if (nx.expiry && (!u.expiry || nx.expiry < u.expiry - 60)) warnings.push(demoWarn('expiry_earlier', { from: u.expiry || 0, to: nx.expiry }, true));
  if (nx.volume && (!u.volume || nx.volume < u.volume)) warnings.push(demoWarn('volume_lower', { from: u.volume || 0, to: nx.volume }));
  if (op === 'stack' && v && expired && vleft) warnings.push(demoWarn('carry_expired', { lost: vleft }));
  if (op === 'stack' && p.autoReset && v) warnings.push(demoWarn('per_period_not_stacked', { lost: expired ? 0 : vleft }));
  if (keepEnd) warnings.push(demoWarn('expiry_kept', { expiry: u.expiry || 0, activateDays: u.activateDays || 0 }));
  if (keepVol) warnings.push(demoWarn('volume_kept', { left: vleft }));
  const left = u.expiry ? Math.max(0, u.expiry - t) : null;
  const res = { op, direction: (p.volumeGb || 0) > ((old && old.volumeGb) || 0) ? 'upgrade' : old && old.id === p.id ? 'same' : 'downgrade', periods: n, usage, dryRun: !!dry, replayed: false,
    from: planStateOf(u, old), to: planStateOf(nx, p), remaining: { seconds: left, bytes: u.volume ? (expired ? 0 : vleft) : null, timeRatio: null, trafficRatio: null, ratio: null },
    credit, warnings, needsForce: warnings.some(w => w.block) };
  if (dry) return { res };
  S.planOps.unshift({ id: S.planOps.length + 1, createdAt: t, actor: 'admin', userId: u.id, op, fromPlanId: u.planId || 0, fromPlan: old ? old.name : '', toPlanId: p.id, toPlan: p.name, periods: n, usage, remark: remark || '', before: res.from, after: res.to, warnings });
  Object.assign(u, nx);
  res.opId = S.planOps.length;
  return { res };
}

// 大陆连通 + IPv6 入站检测(导出实例没有这项):三台三网都通;法兰克福默认走 IPv6,境外 IPv6 测点连得进来
const probe = (city, network, asn, ms, country) => ({ ...(country ? { country } : {}), city, asn, network, ok: true, avgMs: ms, loss: 0 });
const cnGroups = base => [
  { key: 'cm', state: 'ok', ok: 3, total: 3, avgMs: base + 18, probes: [probe('Guangzhou', 'China Mobile', 9808, base + 15), probe('Shanghai', 'China Mobile', 56040, base + 20), probe('Beijing', 'China Mobile', 56048, base + 19)] },
  { key: 'ct', state: 'ok', ok: 3, total: 3, avgMs: base + 9, probes: [probe('Shanghai', 'China Telecom', 4812, base + 7), probe('Guangzhou', 'China Telecom', 4134, base + 10), probe('Chengdu', 'China Telecom', 4134, base + 11)] },
  { key: 'cu', state: 'ok', ok: 3, total: 3, avgMs: base + 14, probes: [probe('Beijing', 'China Unicom', 4808, base + 12), probe('Tianjin', 'China Unicom', 4837, base + 15), probe('Shenyang', 'China Unicom', 4837, base + 16)] },
  { key: 'hk', state: 'ok', ok: 2, total: 2, avgMs: 8, probes: [probe('Hong Kong', 'Example HK', 64510, 8), probe('Hong Kong', 'Example HK', 64511, 9)] },
];
S.reach = { auto: true, minutes: 90, nextRun: now() + 50 * 60, running: false, runningIds: [], perCheck: 15, quota: { remaining: 210, limit: 250, resetAt: now() + 40 * 60 }, history: {}, results: [
  { nodeId: 1, name: 'Tokyo', target: '192.0.2.10', port: 24443, portFrom: 'sub', at: now() - 40 * 60, auto: true, verdict: 'ok', groups: cnGroups(52) },
  { nodeId: 2, name: 'Singapore', target: '198.51.100.10', port: 28443, portFrom: 'line', portLine: 'Singapore-AnyTLS', at: now() - 40 * 60, auto: true, verdict: 'ok', groups: cnGroups(68) },
  { nodeId: 3, name: 'Frankfurt', target: '203.0.113.30', port: 24433, portFrom: 'line', portLine: 'Frankfurt-Reality', at: now() - 40 * 60, auto: true, verdict: 'ok', groups: cnGroups(160),
    // IPv6 入站:先用大陆有 IPv6 的家宽测点(这里连上了,所以没再问境外测点)
    v6: { state: 'ok', scope: 'cn', target: '2001:db8:7::30', port: 24433, portLine: 'Frankfurt-Reality', ok: 4, total: 4, cnOk: 4, cnTotal: 4, at: now() - 40 * 60,
      probes: [{ ...probe('Wuhan', 'CHINA UNICOM China169 Backbone', 4837, 182, 'CN'), group: 'cu' }, { ...probe('Guilin', 'China Telecom', 4134, 171, 'CN'), group: 'ct' },
        { ...probe('Ningbo', 'China Mobile Communications', 56041, 165, 'CN'), group: 'cm' }, { ...probe('Guangzhou', 'China Mobile Communications Group', 9808, 176, 'CN'), group: 'cm' }] } },
] };
for (const r of S.reach.results) S.reach.history[r.nodeId] = Array.from({ length: 8 }, (_, i) => ({ at: r.at - (8 - i) * 90 * 60, verdict: 'ok', auto: true }));
// 概览「最近入站连接」:通过握手与认证的、被设备数上限拒的,以及到了但没过握手 / 认证的(都用文档保留地址)
const recentConns = () => [
  { ip: '203.0.113.5', user: 'demo-alice', line: 'Tokyo-HY2', protocol: 'hysteria2', count: 14, ts: now() - 60, server: 'Tokyo', nodeId: 1, family: 'v4' },
  { ip: '2001:db8:1:2::8', user: 'demo-alice', line: 'Frankfurt-Reality', protocol: 'vless', count: 6, ts: now() - 180, server: 'Frankfurt', nodeId: 3, family: 'v6' },
  { ip: '198.51.100.7', user: 'demo-alice', line: 'Singapore-AnyTLS', protocol: 'anytls', count: 3, ts: now() - 420, server: 'Singapore', nodeId: 2, family: 'v4' },
  { ip: '192.0.2.44', user: 'demo-bob', line: 'Global-SS2022', protocol: 'shadowsocks', count: 9, ts: now() - 240, server: 'Tokyo', nodeId: 1, family: 'v4' },
  { ip: '192.0.2.77', user: 'demo-bob', line: 'Global-SS2022', protocol: 'shadowsocks', count: 2, ts: now() - 900, server: 'Singapore', nodeId: 2, family: 'v4', rejected: 'device' },
  { ip: '198.51.100.23', line: 'Frankfurt-Reality', protocol: 'vless', count: 4, ts: now() - 600, server: 'Frankfurt', nodeId: 3, family: 'v4', failed: 'tls', detail: 'REALITY: processed invalid connection' },
  { ip: '203.0.113.99', line: 'Global-TUIC', protocol: 'tuic', count: 2, ts: now() - 1500, server: 'Tokyo', nodeId: 1, family: 'v4', failed: 'auth', detail: 'authentication failed' },
];
// 「换机还原」的顶部提示只在地址带 ?restore 时演示(正常演示不挂一条警告)
const showRestore = /[?&]restore\b/.test(location.search);

// 线路详情:各服务器的流量按全站流量图的形状摊开(每条线路、每台服务器占比不同),在线的是上面那两位
const lineIdsOf = l => { try { const v = typeof l.nodeIds === 'string' ? JSON.parse(l.nodeIds || '[]') : (l.nodeIds || []); return Array.isArray(v) ? v : []; } catch { return []; } };
function lineDetail(l, q) {
  const all = (S.nodes && S.nodes.nodes) || [];
  const ids = lineIdsOf(l);
  const nodes = all.filter(n => !ids.length || ids.includes(n.id));
  const onl = S.onlines.lines.includes(l.name) ? Object.entries(online).filter(([u]) => S.onlines.users.includes(u)) : [];
  const onlines = onl.map(([user, ips], i) => { const n = nodes[i % Math.max(1, nodes.length)] || { id: 1, name: '主机' }; return { user, nodeId: n.id, server: n.name, ips: [...ips].sort() }; });
  onlines.sort((x, y) => (x.user < y.user ? -1 : x.user > y.user ? 1 : x.server < y.server ? -1 : x.server > y.server ? 1 : 0));
  const perNode = {}; onlines.forEach(o => { perNode[o.nodeId] = (perNode[o.nodeId] || 0) + 1; });
  const head = { onlines, onlineUsers: new Set(onlines.map(o => o.user)).size, onlineIPs: onlines.reduce((a, o) => a + o.ips.length, 0) };
  if (q.get('part') === 'online') return { ...head, perNode };
  // 流量序列按请求的时间范围与分桶现算(和真面板一样:分桶起点按浏览器时区对齐整点 / 整天,最后一格是正在走的这一格),
  // 不再借用录制的全站数据 —— 那份只有 24 小时与 7 天(逐小时),30 天会退回 24 小时,时间也没对齐整点。
  const hours = Number(q.get('hours')) || 24;
  const span = Number(q.get('bucket')) || (hours <= 24 ? 3600 : hours <= 168 ? 21600 : 86400);
  const off = (Number(q.get('tz')) || 0) * 60;
  const now = Math.floor(Date.now() / 1000);
  const end = Math.floor((now + off) / span) * span - off;
  const n = Math.max(1, Math.round(hours * 3600 / span));
  const lf = 0.25 + ((l.id * 37) % 60) / 100; // 每条线路的量级不同
  const weights = [0.5, 0.3, 0.12, 0.08, 0.05];
  const noise = (a, b) => { const x = Math.sin(a * 12.9898 + b * 78.233) * 43758.5453; return x - Math.floor(x); };
  // 一小时的流量(字节):晚上高、凌晨低,按线路与小时有起伏;start 起的 [start, end) 按小时累加,当前这一格只算已经过去的部分
  const hourBytes = (h, w) => { const loc = new Date((h * 3600 + off) * 1000).getUTCHours(); const day = 0.35 + 0.65 * Math.max(0, Math.sin((loc - 6) / 24 * 2 * Math.PI)) ** 1.4; return 160e6 * lf * w * day * (0.7 + 0.6 * noise(l.id * 7 + w * 1000, h)); };
  const series = (w) => {
    const pts = [];
    for (let i = 0; i <= n; i++) {
      const t = end - (n - i) * span, stop = Math.min(t + span, now);
      let bytes = 0;
      for (let s = t; s < stop; s += 3600) bytes += hourBytes(Math.floor(s / 3600), w) * Math.min(1, (stop - s) / 3600);
      const upShare = 0.14 + 0.06 * noise(l.id, Math.floor(t / span));
      pts.push({ t, up: Math.round(bytes * upShare), down: Math.round(bytes * (1 - upShare)) });
    }
    return { points: pts, span, start: end - n * span, end: now, totalUp: pts.reduce((a, p) => a + p.up, 0), totalDown: pts.reduce((a, p) => a + p.down, 0) };
  };
  const servers = nodes.map((n, i) => n.enabled === false || (n.status && n.status.ok === false)
    ? { nodeId: n.id, name: n.name, isLocal: !!n.isLocal, ok: false, disabled: n.enabled === false, error: n.enabled === false ? '服务器已停用' : '联系不上这台服务器', up: 0, down: 0, series: { points: [] }, online: perNode[n.id] || 0 }
    : (() => { const sr = series(weights[i] || 0.04); return { nodeId: n.id, name: n.name, isLocal: !!n.isLocal, ok: true, up: sr.totalUp, down: sr.totalDown, series: sr, online: perNode[n.id] || 0 }; })())
    .sort((a, b) => (b.up + b.down) - (a.up + a.down));
  const okOnes = servers.filter(x => x.ok);
  const total = okOnes.length ? { ...clone(okOnes[0].series), points: okOnes[0].series.points.map((p, i) => ({ t: p.t, up: okOnes.reduce((a, x) => a + ((x.series.points[i] || {}).up || 0), 0), down: okOnes.reduce((a, x) => a + ((x.series.points[i] || {}).down || 0), 0) })) } : { points: [], span: 3600 };
  total.totalUp = (total.points || []).reduce((a, p) => a + p.up, 0); total.totalDown = (total.points || []).reduce((a, p) => a + p.down, 0);
  return { line: { id: l.id, name: l.name, protocol: l.protocol, port: l.port, enabled: l.enabled }, total, servers, ...head };
}

// 日志页的演示数据:导出的那份只有两条订阅访问和一串「新建用户」,看不出这一页能干什么。
// 这里按现在的用户现编一份:各种客户端、共享地址、没找到、出错、还有一行「地址不对 ×N」(同一个 IP 的 404 合成一行);IP 都用文档保留段。
function demoSubLogs(query) {
  if (S.logsCleared.sub) return [];
  const users = S.users.map(u => u.name), u = i => users[i % Math.max(1, users.length)] || 'demo';
  const rows = [
    [40, u(0), '203.0.113.5', 'clash-verge/v2.4.2', 'clash', 200], [95, u(1), '192.0.2.44', 'Shadowrocket/2.2.60 (iPhone; iOS 18.5)', 'link', 200],
    [180, u(0), '198.51.100.7', 'godusevpn/0.8.1 (Windows)', 'json', 200], [320, u(2), '203.0.113.61', 'SFA/1.12.4 (Build 512; sing-box 1.12.4; language zh_CN)', 'json', 200],
    [610, u(3), '198.51.100.23', 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36', 'page', 200],
    [1250, '-', '203.0.113.199', '404 ×37(4 分钟内,最近 /sub/3f9a…c21)', 'miss', 404],
    [1900, u(1), '192.0.2.44', 'Stash/3.1.0 Clash/1.19.0', 'clash', 200], [2600, u(4), '198.51.100.88', 'ClashMetaForAndroid/2.11.12.Meta', 'clash-share', 200],
    [3900, u(2), '203.0.113.61', 'v2rayNG/1.10.2', 'link', 404], [5200, u(3), '198.51.100.23', 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1', 'page', 200],
    [7400, u(0), '203.0.113.5', 'clash-verge/v2.4.2', 'clash', 200], [9100, u(1), '192.0.2.44', 'Loon/3.2.9 CFNetwork/3826.500.131 Darwin/24.5.0', 'link', 200],
    [12800, u(4), '198.51.100.88', 'mihomo/1.19.34', 'clash', 500], [16500, u(2), '203.0.113.61', 'curl/8.9.1', 'link', 200],
  ].map(([ago, user, ip, ua, format, status], i, all) => ({ id: all.length - i, ts: now() - ago, user, ip, ua, format, status }));
  const want = new URLSearchParams(query).get('user');
  return want ? rows.filter(r => r.user === want) : rows;
}
function demoAudit(query) {
  if (S.logsCleared.audit) return [];
  const q = new URLSearchParams(query), names = S.users.map(x => x.name), line = (S.lines[0] || {}).name || 'Tokyo-HY2';
  const rows = [
    [120, 'admin', 'user', 'plan:renew:Pro - monthly', names[0]], [900, 'admin', 'user', 'batch:disable', names.slice(2, 4)],
    [2400, 'RuleJob', 'rule', 'apply', `${names[0]} ← Burst penalty(30 GB / 1h,5 / 5 Mbps)`], [4100, 'admin', 'line', 'update', line],
    [7300, 'demo-reseller', 'user', 'create', 'rs-user-01'], [9800, 'admin', 'upstream', 'health-check', 0],
    [15200, 'admin', 'cert', 'renew', 'panel.example.com'], [18800, 'DepleteJob', 'user', 'disable:quota', names[3]], [22000, 'ResetJob', 'user', 'reset', names[1]],
    [30500, 'admin', 'backup', 'create', 'm-ui-demo.zip'], [41000, 'admin', 'settings', 'update', ['backupHour', 'backupKeep']],
    [52000, 'admin', 'ops', 'run:tune-all', ''], [66000, 'admin', 'node', 'group-rename', { from: 'Asia', to: 'APAC' }],
    [80000, 'api:shop', 'user', 'plan:extend:Basic', names[3]], [93000, 'admin', 'admin', 'totp:enable', ''],
  ].map(([ago, actor, key, action, obj], i, all) => ({ id: all.length - i + 100, dateTime: now() - ago, actor, key, action, obj }));
  const jobs = ['ResetJob', 'DepleteJob', 'RuleJob'];
  return rows.filter(r => (!q.get('key') || r.key === q.get('key')) && (q.get('actor') !== 'human' || !jobs.includes(r.actor))).slice(0, Number(q.get('limit')) || 300);
}

function status() {
  const s = fx('GET status') || {};
  s.users = S.users.length; s.enabledUsers = S.users.filter(u => u.enabled).length; s.lines = S.lines.length; s.linesEnabled = S.lines.filter(l => l.enabled).length;
  s.upstreams = S.upstreams.length; s.onlineUsers = S.onlines.users.length; s.version = 'demo'; s.repo = GITHUB; s.defaultPassword = false;
  { const self = (S.nodes.nodes || []).find(n => n.isLocal), ids = l => (Array.isArray(l.nodeIds) ? l.nodeIds : []); // 「本机 · 数据面」卡的本机线路数
    const mine = S.lines.filter(l => !ids(l).length || (self && ids(l).includes(self.id))); s.coreLines = mine.length; s.coreLinesEnabled = mine.filter(l => l.enabled !== false).length; }
  s.ha = { role: 'master', epoch: HA.epoch, mode: HA.config.mode, notices: [] };
  if (showRestore && !S.restoreAcked) s.restore = { at: now() - 300, copies: [{ from: '/etc/letsencrypt/live/panel.example.com/fullchain.pem', to: '/etc/m-ui/cert/restored/panel.example.com.crt', names: ['panel.example.com'], notAfter: now() + 61 * DAY }] };
  return s;
}

// 落地页 logo 只存在内存里:上传前浏览器已经转成 PNG,宽高从 PNG 头里读
function logoOf(data) {
  const b64 = String(data || '').split(',')[1] || '';
  const head = atob(b64.slice(0, 44));
  const u32 = i => ((head.charCodeAt(i) << 24) | (head.charCodeAt(i + 1) << 16) | (head.charCodeAt(i + 2) << 8) | head.charCodeAt(i + 3)) >>> 0;
  return { set: true, sum: rnd(16), mime: 'image/png', width: u32(16), height: u32(20), size: Math.floor(b64.length * 3 / 4), data };
}

// 主机容灾:演示站是一台开着「先确认再接任」的主机,服务器列表里前两台副机当备用,手里各有一份刚自检过的副本。
// 设置页的模式、台数、高级里的数字和暂停在本页内存里生效;设为主机、接任、重新信任这类不真做(卡片里写明演示模式)。
const haFp = seed => Array.from({ length: 16 }, (_, i) => '0123456789abcdef'[(seed * 7 + i * 5) % 16]).join('');
const HA = {
  epoch: 3,
  config: { mode: 'confirm', standbys: 2, replicaMinutes: 5, confirmMinutes: 3, autoMinutes: 10, rankStepMinutes: 3, witness: true, pauseUntil: 0, dnsNames: 'sub.example.com', dnsTokenSet: false, allowPlain: false, takeSub: false, dnsProvider: 'cloudflare' },
};
const haNodes = () => ((S.nodes && S.nodes.nodes) || []).filter(n => n.enabled !== false);
function haStandbys() {
  const c = HA.config, t0 = now();
  return haNodes().filter(n => !n.isLocal).map((n, i) => {
    const rank = c.mode !== 'off' && i < c.standbys ? i + 1 : 0;
    const fp = haFp(n.id);
    const replica = rank ? { epoch: HA.epoch, seq: 412 + n.id, at: t0 - 75, size: 18 * 1048576 + n.id * 4096, asOf: t0 - 20, verifiedAt: t0 - 60 } : undefined;
    return { id: n.id, name: n.name, rank, eligible: true, version: 'demo', pubFp: fp, pinnedFp: fp, replica,
      ack: rank ? { seq: 412 + n.id, sentAt: t0 - 75, asOf: t0 - 20, size: replica.size } : undefined,
      history: rank ? { epoch: HA.epoch, seq: 398 + n.id, at: t0 - 1260, size: 41 * 1048576 } : undefined,
      histAck: rank ? { asOf: t0 - 1260, size: 41 * 1048576 } : undefined,
      leaseAgo: 3, diskFree: 24 * 1073741824, peers: { ok: 2, total: 2, at: t0 - 40 }, epoch: HA.epoch, webPort: 2053, resellerPort: 2054 };
  });
}
function haView(full) {
  const me = haNodes().find(n => n.isLocal) || {};
  const c = HA.config, on = c.mode !== 'off';
  const sb = haStandbys(), picked = sb.filter(x => x.rank);
  const v = { role: 'master', epoch: HA.epoch, selfId: me.id || 1, selfName: me.name || '', masterId: me.id || 1, masterName: me.name || '', version: 'demo', mode: c.mode,
    notices: [], config: clone(c), standbys: on || full ? sb : [], standbyIds: on ? picked.map(x => x.id) : [], signFp: haFp(1) };
  if (full) {
    v.ready = on ? [
      { code: 'standbys', level: picked.length >= c.standbys ? 'ok' : 'warn', args: { n: String(picked.length), want: String(c.standbys) } },
      { code: 'versions', level: 'ok' }, { code: 'replica', level: 'ok' }, { code: 'https', level: 'ok' },
      { code: 'selfLink', level: 'ok', args: { url: `https://${subBase}:2053/app/` } },
      ...(c.takeSub ? [{ code: 'subDomain', level: 'ok', args: { host: 'sub.example.com' } },
        c.dnsTokenSet ? { code: 'takeSubDns', level: 'ok', names: c.dnsNames.split(/[\s,]+/).filter(Boolean), args: { provider: c.dnsProvider || 'cloudflare' } } : { code: 'takeSubDns', level: 'bad' },
        { code: 'takeSubCert', level: 'ok', args: { days: '71' } }] : [{ code: 'takeSub', level: 'info', args: { host: 'sub.example.com' } }]),
      { code: 'telegram', level: 'ok' }, { code: 'peers', level: 'ok' },
      c.witness ? { code: 'witness', level: 'ok' } : { code: 'witness', level: c.mode === 'auto' && haNodes().length <= 2 ? 'bad' : 'info' },
    ] : [{ code: 'standbys', level: 'info' }, { code: 'replica', level: 'info' }];
    if (!on) v.standbys = sb.map(x => ({ ...x, replica: undefined, ack: undefined, history: undefined, histAck: undefined }));
  }
  return v;
}
// 设为主机的检查:演示站不真的换主机,卡片里写明原因(code 为 text 的原文照样显示)
const haDemoBlock = () => ({ ok: false, blockers: [{ code: 'text', args: { text: zh ? '演示模式:不会真的换主机。装好以后在这里一键把主机换到这台。' : 'Demo mode: the master is not actually moved. On a real install this hands the master over to this server in one step.' } }] });

// 清理磁盘(演示):开始后两秒多显示「正在清理」,之后给出完整输出,那台的磁盘占用降下来(输出照真机的样子)
const dcLines = ['[12:00:00] 开始 disk-clean', '== 清理磁盘 ==', '清理前:已用 9011 MB / 共 9830 MB,可用 819 MB(92%)', '已清空 apt 软件包缓存',
  'journald 回收到 200MB 以内,现在:198.0M', '截断 /var/log/syslog:原 3812MB,只留最后 5MB', '删掉轮转下来的旧日志 14 个,共 286MB',
  '已装 m-ui-syslog-cap.timer:每小时看一次,syslog 超过 200MB 就立刻轮转', '清理后:已用 4720 MB / 共 9830 MB,可用 5110 MB(49%)', 'm-ui 的数据目录和备份没有动', '[12:00:04] 完成'];
function demoDiskClean(id, start) {
  const n = (S.nodes.nodes || []).find(x => x.id === id) || {}; // 不能用 find:顶层的 find 是浏览器的 window.find
  if (start) S.dc = { id, at: Date.now() };
  const st = S.dc && S.dc.id === id ? S.dc : null;
  const d = n.isLocal ? null : (n.status || {}).disk;
  if (!st) return { running: false, log: [], disk: d };
  if (Date.now() - st.at < 2500) return { running: true, current: 'disk-clean', log: dcLines.slice(0, 4), disk: d };
  if (d && !st.applied) { st.applied = true; const used = d.used * 0.5; Object.assign(d, { used, free: d.total - used, logs: 0.2 * 1073741824, cache: 0 }); }
  return { running: false, log: dcLines, last: { name: 'disk-clean', ok: true, time: now() }, disk: d };
}

// 一条路由:method + 路径(去掉 ./api/ 与查询串),query 单独给
// ---- 防火墙(演示):三台服务器三种状况 —— 本机没有系统防火墙、新加坡 ufw 开着(m-ui 的端口已自动放行)、法兰克福已放开(原来是镜像自带的 iptables 规则);
// 三种规则各一条;黑名单里两条自动拉黑的、一条手动加的;白名单一条。校验和拦截照着后端做(同一句报错),改动只在内存里。
S.fw = {
  autoBan: true, autoAllow: true, myIp: '198.51.100.77',
  sys: { 1: { frontend: 'none' }, 2: { frontend: 'ufw', blocking: true, detail: 'ufw active' }, 3: { frontend: 'none', opened: true, openedAt: now() - 3 * DAY, openedFrom: 'iptables' } },
  rules: [
    { id: 1, kind: 'only', proto: 'tcp', ports: '22', sources: '198.51.100.77/32,203.0.113.50/32', nodeIds: null, expiry: 0, note: 'SSH', origin: 'manual', enabled: true, createdAt: now() - 20 * DAY },
    { id: 2, kind: 'allow', proto: 'tcp', ports: '80,443', sources: '', nodeIds: [2], expiry: 0, note: 'Web', origin: 'manual', enabled: true, createdAt: now() - 9 * DAY },
    { id: 3, kind: 'deny', proto: '', ports: '', sources: '192.0.2.66/32', nodeIds: null, expiry: now() + 5 * 3600 + 900, note: '', origin: 'ban', enabled: true, createdAt: now() - 3600 },
  ],
  bans: [
    { id: 1, ip: '192.0.2.66', reason: 'login', evidence: '10 分钟内登录失败 12 次', count: 12, hits: 1, firstAt: now() - 1100, lastAt: now() - 1080, expiry: now() + 2520, auto: true },
    { id: 2, ip: '2001:db8:bad::/64', reason: 'sub', evidence: '10 分钟内试了 8 个不存在的订阅地址', count: 8, hits: 3, firstAt: now() - 20 * 3600, lastAt: now() - 2 * 3600, expiry: now() + 22 * 3600, auto: true },
    { id: 3, ip: '203.0.113.0/28', reason: 'manual', evidence: 'competitor probes', count: 0, hits: 0, firstAt: now() - 3 * DAY, lastAt: now() - 3 * DAY, expiry: 0, auto: false },
  ],
  allow: [{ id: 1, ip: '198.51.100.0/24', note: 'office', createdAt: now() - 30 * DAY }],
};
// ---- 线路详情「测大陆连通」(js/pages/lines.js)的演示 ----
// 和后端一样按订阅里的地址来:线路部署到的每台服务器一个(线路单独选了 IPv6 的用那台的 IPv6 地址),接着是中转入口。
// 结果约 1.2 秒出一个:三台服务器都通(延迟和服务器页那份一致);UDP 的线路注明只 ping 了地址;别处的中转入口演示「大陆通、香港不通」。
S.lineReach = {}; // 任务号 → { lineId, t0, items }
function demoLineReach(l, method, job) {
  if (job) {
    const j = S.lineReach[job];
    if (!j || j.lineId !== l.id) throw demoErr(404, '检测任务不存在或已过期');
    const n = Math.min(j.items.length, Math.floor((Date.now() - j.t0) / 1200));
    const items = j.items.map((it, i) => (it.state === 'skipped' || i < n ? it : { label: it.label, target: it.target, port: it.port, relay: it.relay, udp: it.udp, v6: it.v6, state: 'pending' }));
    return { done: n >= j.items.length, total: items.length, finished: items.filter(x => x.state !== 'pending').length, items: clone(items) };
  }
  if (method !== 'POST') throw demoErr(405, '方法不允许');
  if (l.enabled === false) throw demoErr(400, '这条线路停用着,没有在监听,启用后再测');
  const parse = v => { try { return typeof v === 'string' ? JSON.parse(v) : (v || null); } catch { return null; } };
  const nodes = S.nodes.nodes.filter(n => n.enabled !== false);
  const ids = parse(l.nodeIds), fam = parse(l.nodeFamily) || {};
  const on = nodes.filter(n => !Array.isArray(ids) || !ids.length || ids.includes(n.id));
  const hostOf = n => (fam[n.id] === 'v6' && n.publicIp6 ? n.publicIp6 : (n.addr || n.publicIp || n.domain || ''));
  const eps = on.map(n => ({ label: n.name, target: hostOf(n), port: l.port, nodeId: n.id }));
  for (const fw of (parse(l.forwards) || [])) {
    const exit = on.find(n => n.id === fw.to), via = fw.via ? nodes.find(n => n.id === fw.via) : null;
    if (fw.off || !exit || (fw.via && !via)) continue;
    eps.push({ label: exit.name + '-' + (fw.name || (via ? via.name : fw.host)), target: via ? hostOf(via) : fw.host, port: fw.port, relay: true, nodeId: via ? via.id : 0, ext: !via });
  }
  if (!eps.length) throw demoErr(400, '这条线路在订阅里没有可连的地址(没有部署到启用的服务器,或节点都关了输出)');
  const udp = l.protocol === 'hysteria2' || l.protocol === 'tuic';
  const base = { 1: 52, 2: 68, 3: 160 };
  const light = gs => gs.map(g => ({ key: g.key, state: g.state, ok: g.ok, total: g.total, avgMs: g.avgMs }));
  const items = eps.map(e => {
    const it = { label: e.label, target: e.target, port: e.port, ...(e.relay ? { relay: true } : {}), ...(udp ? { udp: true } : {}), ...(e.target.includes(':') ? { v6: true } : {}), state: 'done' };
    if (it.v6 && udp) return { ...it, state: 'skipped', skip: 'udp6' };
    if (it.v6) return { ...it, v6r: { state: 'ok', scope: 'cn', target: e.target, port: e.port, ok: 4, total: 4, cnOk: 4, cnTotal: 4, at: now() } };
    const gs = light(cnGroups(base[e.nodeId] || 34));
    if (e.ext) { const hk = gs.find(g => g.key === 'hk'); hk.state = 'down'; hk.ok = 0; hk.avgMs = 0; } // 国内的中转入口常只放行大陆 IP
    return { ...it, verdict: 'ok', note: [udp ? 'udpPing' : '', e.ext ? 'hkDown' : ''].filter(Boolean).join(','), groups: gs };
  });
  const id = 'demo-' + rnd(8);
  S.lineReach[id] = { lineId: l.id, t0: Date.now(), items };
  return { job: id, items: items.map(it => (it.state === 'skipped' ? it : { label: it.label, target: it.target, port: it.port, relay: it.relay, udp: it.udp, v6: it.v6, state: 'pending' })) };
}
// ---- 线路详情「测大陆连通」的演示:完 ----

// ---- 流量奖励(js/reward.js)的演示数据 ----
// 地址全是演示用的:中间故意带着 base58 里没有的「0」,钱包一粘贴就会拒绝 —— 不会有人照着演示站把钱打到一个不存在的地址上。
const RW_TB = 1099511627776;
const RW_QR = 'data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAzMyAzMyIgc2hhcGUtcmVuZGVyaW5nPSJjcmlzcEVkZ2VzIj48cGF0aCBmaWxsPSIjZmZmZmZmIiBkPSJNMCAwaDMzdjMzSDB6Ii8+PHBhdGggc3Ryb2tlPSIjMDAwMDAwIiBkPSJNMCAwLjVoN20zIDBoMW0yIDBoMm0xIDBoMm0xIDBoMm0yIDBoMW0yIDBoN00wIDEuNWgxbTUgMGgxbTMgMGgxbTIgMGgzbTEgMGgxbTIgMGgxbTEgMGgxbTEgMGgxbTEgMGgxbTUgMGgxTTAgMi41aDFtMSAwaDNtMSAwaDFtMSAwaDRtMSAwaDFtMSAwaDFtMSAwaDRtMiAwaDFtMiAwaDFtMSAwaDNtMSAwaDFNMCAzLjVoMW0xIDBoM20xIDBoMW0xIDBoMW01IDBoMW0xIDBoMW0yIDBoNm0xIDBoMW0xIDBoM20xIDBoMU0wIDQuNWgxbTEgMGgzbTEgMGgxbTEgMGgxbTIgMGgzbTEgMGgxbTIgMGgxbTIgMGgxbTIgMGgxbTEgMGgxbTEgMGgzbTEgMGgxTTAgNS41aDFtNSAwaDFtMSAwaDVtMiAwaDJtMyAwaDJtMiAwaDFtMSAwaDFtNSAwaDFNMCA2LjVoN20xIDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0xIDBoN004IDcuNWgzbTEgMGgxbTIgMGgxbTIgMGgxbTIgMGgybTEgMGgxTTAgOC41aDFtMSAwaDVtMiAwaDFtMSAwaDZtNCAwaDRtMSAwaDVNMCA5LjVoMW0xIDBoMW0xIDBoMm0yIDBoMm0yIDBoMW0xIDBoMW0xIDBoMW0yIDBoM20xIDBoMW0xIDBoM20xIDBoNE0wIDEwLjVoMW0xIDBoMm0yIDBoNm0zIDBoMW0yIDBoMW0yIDBoMW00IDBoMW0xIDBoNE0wIDExLjVoMW0yIDBoM20xIDBoMW0zIDBoMm0xIDBoMm0zIDBoNG0xIDBoMW0zIDBoM00zIDEyLjVoNG0xIDBoMW0xIDBoMm02IDBoMm0xIDBoMW0zIDBoMW0yIDBoMW0xIDBoMW0xIDBoMU0wIDEzLjVoNG0xIDBoMW00IDBoMW0zIDBoM20zIDBoNG0xIDBoMk0wIDE0LjVoNW0xIDBoMW03IDBoMW0yIDBoM20xIDBoMW0yIDBoMW0yIDBoMW0yIDBoMk0wIDE1LjVoMm0xIDBoMm0yIDBoMW0xIDBoNG0zIDBoMW0xIDBoNW0xIDBoMm0xIDBoNE0wIDE2LjVoMW0xIDBoM20xIDBoM20xIDBoMm0xIDBoMm0xIDBoMm0xIDBoMW0yIDBoNG0xIDBoM20xIDBoMU0yIDE3LjVoNG00IDBoM20zIDBoM20xIDBoMW0yIDBoMW0xIDBoMW0zIDBoMW0xIDBoMk0wIDE4LjVoM20zIDBoMm0zIDBoMW0xIDBoM20xIDBoMW00IDBoMW0zIDBoMW0xIDBoNE0wIDE5LjVoMm0xIDBoMm0zIDBoMW0xIDBoMm0xIDBoM20xIDBoMm01IDBoMW0zIDBoM00wIDIwLjVoMm0xIDBoMm0xIDBoM20yIDBoM20yIDBoMW0zIDBoM20yIDBoMW0yIDBoMW0zIDBoMU0wIDIxLjVoMW0yIDBoM20zIDBoMm00IDBoMW0yIDBoMW0yIDBoMW0xIDBoMW0xIDBoMk0wIDIyLjVoMW0xIDBoMm0xIDBoM20yIDBoMW00IDBoMm0zIDBoM20xIDBoMW0yIDBoMW0yIDBoMk0wIDIzLjVoMW0xIDBoMW0xIDBoMW01IDBoM20xIDBoMm01IDBoMW0yIDBoN00wIDI0LjVoMW0zIDBoNG0xIDBoMm0yIDBoMW0xIDBoM20yIDBoMW0xIDBoOG0xIDBoMk04IDI1LjVoMW0yIDBoMW0xIDBoMW0yIDBoMW0xIDBoMm0xIDBoMW0xIDBoMm0zIDBoMm0xIDBoMk0wIDI2LjVoN20yIDBoMm0yIDBoMW0xIDBoMm0xIDBoMW0yIDBoMW0xIDBoMm0xIDBoMW0xIDBoMm0xIDBoMU0wIDI3LjVoMW01IDBoMW0xIDBoMW0xIDBoMW0xIDBoMW0yIDBoMW0zIDBoMW0xIDBoMm0xIDBoMW0zIDBoM00wIDI4LjVoMW0xIDBoM20xIDBoMW0xIDBoMW0xIDBoM20xIDBoMm0yIDBoMW0yIDBoMW0yIDBoNm0yIDBoMU0wIDI5LjVoMW0xIDBoM20xIDBoMW0xIDBoNG0zIDBoMm0zIDBoNG0xIDBoMW0xIDBoMm0zIDBoMU0wIDMwLjVoMW0xIDBoM20xIDBoMW0xIDBoNG0xIDBoMm0zIDBoMm0xIDBoM20yIDBoMW0zIDBoMU0wIDMxLjVoMW01IDBoMW00IDBoMW0yIDBoMW0xIDBoMW0yIDBoM20xIDBoMW0yIDBoMm0xIDBoMk0wIDMyLjVoN20xIDBoNW0zIDBoNW0xIDBoM20yIDBoMW0xIDBoMW0xIDBoMSIvPjwvc3ZnPgo=';
const rwRand = (() => { let s = 20270903; return () => { s = (s * 1664525 + 1013904223) >>> 0; return s / 4294967296; }; })();
const RW_B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
const rwAddr = (n, head = '') => { let a = head; while (a.length < n) a += RW_B58[Math.floor(rwRand() * 58)]; const m = Math.floor(n / 2); return a.slice(0, m - 2) + '0DEM' + a.slice(m + 2); };
S.reward = (() => {
  const day = 86400, t0 = Math.floor(Date.now() / 1000);
  const mk = (ts, kind, chain, amount, name) => ({ ts, kind, chain, amount, name: name || '', from: chain === 'tron' ? rwAddr(34, 'T') : rwAddr(44), tx: rwAddr(chain === 'tron' ? 64 : 88) });
  const amounts = [9.9, 29.9, 9.9, 59, 9.9, 29.9, 99, 9.9, 29.9, 9.9, 199, 29.9, 9.9, 59, 9.9, 29.9, 9.9, 99, 29.9, 9.9, 59, 9.9, 29.9];
  const ledger = amounts.map((a, i) => mk(t0 - Math.floor((i * 2.6 + rwRand() * 2) * day) - 3600, 'order', rwRand() < 0.62 ? 'solana' : 'tron', a));
  // 两种少见的:一笔人工确认发放的订单(付款金额和订单对不上,核对到账之后确认的;按实收的算),一笔到账后又退款的(作废,不计入)
  ledger[3].note = 'manual'; ledger[3].amount = 58.3;
  ledger[6].void = true;
  // 捐赠:只有留名捐赠计入奖池(没登记就转进来的不算),所以每一笔都有名字,都在 Solana 上
  const gifts = [[2, 50, '老王'], [5, 20, 'night_owl'], [9, 100, '路过的面板党'], [12, 10, 'Tokyo 小鸡'], [15, 30, '老王'], [19, 66.66, 'k8s 练习生'], [23, 8.88, '半夜修机器的人'], [27, 20, 'night_owl'],
    [31, 200, 'M 记铁粉'], [36, 15, 'hy2 一把梭'], [41, 5, '隔壁机房'], [47, 88, '路过的面板党'], [52, 12.5, '夜班运维'], [29, 25, 'Tokyo 小鸡'], [8, 6, '一个路人'], [33, 40, '隔壁机房']];
  for (const [d, a, name] of gifts) ledger.push(mk(t0 - d * day - 7200 - Math.floor(rwRand() * 30000), 'donation', 'solana', a, name));
  ledger.sort((a, b) => b.ts - a.ts);
  const board = [];
  for (let i = 0; i < 63; i++) board.push({ address: rwAddr(44), points: Math.max(1, Math.round(46 * Math.pow(0.9, i) + rwRand() * 2)), panels: rwRand() < 0.16 ? 2 + Math.floor(rwRand() * 3) : 1 });
  board[7].excluded = true; board[7].rule = 'robot'; board[7].reason = '每天的流量增长一模一样,不像真实流量';
  board[26].excluded = true; board[26].rule = 'same_src'; board[26].reason = '这处来源(公网 IP)上已经有别的地址在登记:一处来源只认先到的那个地址';
  return {
    tb: RW_TB, bytes: Math.round(3.618 * RW_TB), address: '', boundAt: 0, credited: 0, remind: 'tb', intent: null,
    payoutAt: Math.floor(new Date(2027, 8, 3, 0, 0, 0).getTime() / 1000), rate: 0.4,
    poolAddr: 'DEMopoo1So1anaAddr0NotRea1xxxxxxxxxxxxxxxxxx', // 留名捐赠要转到的地址(演示用的假地址)
    ledger, board,
  };
})();
// 留名捐赠的零头:0.001–0.099 里第三位小数不为 0 的(整分的数太像普通转账),和登记服务同一条规则
const rwTail = () => { for (;;) { const k = 1 + Math.floor(Math.random() * 99); if (k % 10) return k; } };
function demoReward(method, seg, query, body) {
  const R = S.reward, q = new URLSearchParams(query || '');
  const now = () => Math.floor(Date.now() / 1000);
  const r2 = n => Math.round(n * 100) / 100;
  const share = x => x.void ? 0 : x.kind === 'donation' ? x.amount : r2(x.amount * R.rate);
  const points = Math.floor(R.bytes / R.tb);
  const rows = () => { // 榜单:没被剔除的按积分排名;绑定了的把「我」放进去
    const list = R.board.map(x => ({ ...x }));
    if (R.address) list.push({ address: R.address, points: R.credited, panels: 1, me: true });
    const ok = list.filter(x => !x.excluded && x.points > 0).sort((a, b) => b.points - a.points);
    ok.forEach((x, i) => { x.rank = i + 1; });
    return { ok, out: list.filter(x => x.excluded) };
  };
  const settle = () => { // 留名的那一笔:20 秒后模拟到账;超过 60 分钟算过期
    const it = R.intent;
    if (!it || it.status !== 'pending') return;
    if (now() - it.createdAt >= 20) {
      it.status = 'paid'; it.paidAt = now();
      R.ledger.unshift({ ts: now(), kind: 'donation', chain: 'solana', amount: it.exact, name: it.name, from: rwAddr(44), tx: rwAddr(88) });
    } else if (now() > it.expiresAt) it.status = 'expired';
  };
  const summary = () => {
    if (S.settings.rewardEnabled === 'false') return { enabled: false };
    settle();
    R.bytes += Math.round(2.4e9 + Math.random() * 1.6e9); // 演示:看着它一点点涨
    const orders = R.ledger.filter(x => x.kind === 'order' && !x.void), dons = R.ledger.filter(x => x.kind === 'donation'); // 作废的订单不进合计
    const sum = a => r2(a.reduce((s, x) => s + x.amount, 0));
    const { ok } = rows();
    const total = r2(sum(orders) * R.rate + sum(dons));
    return {
      enabled: true, tb: R.tb, bytes: R.bytes, points: Math.floor(R.bytes / R.tb), address: R.address, boundAt: R.boundAt, credited: R.credited,
      remind: R.address ? '' : R.remind, excluded: null, donation: R.intent ? { ...R.intent } : null,
      pool: {
        total, orderRate: R.rate, orderCount: orders.length, orderAmount: sum(orders), orderShare: r2(sum(orders) * R.rate), donateCount: dons.length, donateAmount: sum(dons),
        donate: true, donateMin: 1, donateMax: 100000,
        points: ok.reduce((s, x) => s + x.points, 0), addresses: ok.length, payoutAt: R.payoutAt, updatedAt: now() - 240, stale: false,
        ledgerUrl: 'https://github.com/Maoyangui/m-ui/tree/rewards', demo: true,
      },
    };
  };
  const page = list => { const size = Math.min(50, Math.max(1, Number(q.get('size')) || 10)), p = Math.max(1, Number(q.get('page')) || 1); return { items: list.slice((p - 1) * size, p * size), total: list.length }; };
  if (method === 'GET') {
    if (!seg[1]) return summary();
    settle();
    if (seg[1] === 'ledger') {
      const kind = q.get('kind') || '', chain = q.get('chain') || '';
      return page(R.ledger.filter(x => (!kind || x.kind === kind) && (!chain || x.chain === chain)).map(x => ({ ...x, share: share(x) })));
    }
    if (seg[1] === 'board') {
      const { ok, out } = rows(), k = (q.get('q') || '').trim().toLowerCase();
      let list = [...ok, ...out];
      if (k) list = list.filter(x => x.address.toLowerCase().includes(k));
      else if (R.address) { const me = list.find(x => x.me); if (me && me.rank > 10) list = [me, ...list.filter(x => !x.me)]; } // 我不在第一页时,钉在最上面
      return { ...page(list), top: ok.length ? ok[0].points : 1 };
    }
    if (seg[1] === 'donors') {
      const dons = R.ledger.filter(x => x.kind === 'donation'), by = new Map();
      for (const x of dons) { const o = by.get(x.name) || { name: x.name, amount: 0, count: 0, lastAt: 0 }; o.amount = r2(o.amount + x.amount); o.count++; o.lastAt = Math.max(o.lastAt, x.ts); by.set(x.name, o); }
      const list = [...by.values()].sort((a, b) => b.amount - a.amount || b.lastAt - a.lastAt);
      const mine = R.intent && R.intent.status === 'paid' ? R.intent.name : '';
      list.forEach((x, i) => { x.rank = i + 1; if (x.name === mine) x.me = true; });
      return { ...page(list), top: list.length ? list[0].amount : 1, named: { donors: list.length, count: dons.length, amount: r2(list.reduce((s, x) => s + x.amount, 0)) } };
    }
    throw demoErr(404, '路径无效');
  }
  if (seg[1] === 'bind') {
    const a = String((body && body.address) || '').trim();
    if (!/^[1-9A-HJ-NP-Za-km-z]{32,44}$/.test(a)) throw demoErr(400, '这不是 Solana 地址');
    R.address = a; R.boundAt = now(); R.remind = '';
    R.credited = Math.max(0, points - 1); // 演示:留 1 分「待入账」(每个地址每天最多入账 1 分)
    return summary();
  }
  if (seg[1] === 'unbind') { R.address = ''; R.boundAt = 0; R.credited = 0; return summary(); }
  if (seg[1] === 'remind') { R.remind = ''; return { ok: '1' }; }
  if (seg[1] === 'donate') {
    if (seg[2] === 'cancel' || seg[2] === 'clear') { R.intent = null; return summary(); }
    const name = String((body && body.name) || '').trim().replace(/\s+/g, ' '), amount = Number(body && body.amount);
    if (!name) throw demoErr(400, '请填写留名');
    if ([...name].length > 20) throw demoErr(400, '留名最多 20 个字');
    if (!(amount >= 1) || amount > 100000 || Math.round(amount * 100) !== amount * 100) throw demoErr(400, '金额要在 1 到 100000 之间,最多两位小数');
    R.intent = { id: 'demo-' + now(), name, amount, exact: Math.round(amount * 1000 + rwTail()) / 1000, chain: 'solana', address: R.poolAddr, createdAt: now(), expiresAt: now() + 3600, status: 'pending' };
    return summary();
  }
  throw demoErr(404, '路径无效');
}
// ---- 流量奖励的演示数据:完 ----

function demoFw(method, seg, body) {
  const W = S.fw;
  const arr = v => { try { const x = typeof v === 'string' ? JSON.parse(v || '[]') : (v || []); return Array.isArray(x) ? x : []; } catch { return []; } };
  const nodesNow = () => (S.nodes.nodes || []);
  // 一台服务器上 m-ui 用到的端口:面板(本机还有订阅)、部署在它上面的线路、由它来转的中转入口
  const portsOf = n => {
    const out = [];
    const add = (p, proto) => { const k = `${p}/${proto}`; if (!out.includes(k)) out.push(k); };
    add(Number(S.settings.webPort) || 2053, 'tcp');
    if (n.isLocal) add(Number(S.settings.subPort) || 2056, 'tcp');
    for (const l of S.lines) {
      if (l.enabled === false) continue;
      const ids = arr(l.nodeIds);
      if (!ids.length || ids.includes(n.id)) {
        const udp = ['hysteria2', 'tuic'].includes(l.protocol), both = l.protocol === 'shadowsocks';
        if (!udp) add(l.port, 'tcp');
        if (udp || both) add(l.port, 'udp');
      }
      for (const f of arr(l.forwards)) if (!f.off && f.via === n.id && f.port) add(f.port, ['hysteria2', 'tuic'].includes(l.protocol) ? 'udp' : 'tcp');
    }
    return out.sort((a, b) => parseInt(a) - parseInt(b) || a.localeCompare(b));
  };
  const liveRules = n => W.rules.filter(r => r.enabled && !(r.expiry > 0 && r.expiry <= now()) && (!Array.isArray(r.nodeIds) || !r.nodeIds.length || r.nodeIds.includes(n.id)));
  const stateOf = n => {
    const sy = W.sys[n.id] || (W.sys[n.id] = { frontend: 'none' });
    const ports = portsOf(n), rules = liveRules(n);
    const st = { frontend: sy.frontend, blocking: !!sy.blocking, detail: sy.detail || '', engine: 'nft', rules: rules.length, ports: ports.length, at: now() - 40 };
    if (sy.opened) Object.assign(st, { opened: true, openedAt: sy.openedAt, openedFrom: sy.openedFrom });
    if (st.blocking) {
      st.allow = W.autoAllow ? 'ok' : 'off';
      st.allowed = (W.autoAllow ? ports.length : 0) + rules.filter(r => r.kind === 'allow').reduce((a, r) => a + r.ports.split(',').filter(Boolean).length * (r.proto ? 1 : 2), 0);
    }
    return { st, ports };
  };
  const view = () => {
    W.rules = W.rules.filter(r => !(r.expiry > 0 && r.expiry <= now()));
    W.bans = W.bans.filter(b => !(b.expiry > 0 && b.expiry <= now()));
    const online = id => { const n = nodesNow().find(x => x.id === id); return !!n && (n.isLocal || !n.status || n.status.ok !== false); };
    return clone({ autoBan: W.autoBan, autoAllow: W.autoAllow, myIp: W.myIp, banTotal: W.bans.length,
      servers: nodesNow().map(n => { const { st, ports } = stateOf(n); return { id: n.id, name: n.name, isLocal: !!n.isLocal, enabled: n.enabled !== false, online: online(n.id), state: st, ports: n.isLocal ? ports : undefined }; }),
      rules: [...W.rules].sort((a, b) => b.id - a.id), bans: [...W.bans].sort((a, b) => b.lastAt - a.lastAt), allow: [...W.allow].sort((a, b) => b.id - a.id) });
  };
  // 地址和网段:够演示用的解析(IPv4 精确算网段,IPv6 只比前缀的写法)
  const v4 = s => { const m = /^(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(s); return m && m.slice(1).every(x => Number(x) <= 255) ? m.slice(1).reduce((a, x) => a * 256 + Number(x), 0) : null; };
  const parseSrc = text => String(text || '').split(/[\s,，、;]+/).filter(Boolean).map(part => {
    const [a, bits] = part.split('/');
    const n4 = v4(a);
    if (n4 !== null) { const b = bits === undefined ? 32 : Number(bits); if (!(b >= 0 && b <= 32)) throw demoErr(400, `「${part}」不是 IP 或网段(例:1.2.3.4、203.0.113.0/24、2001:db8::/32)`); if (b === 0) throw demoErr(400, `「${part}」是全部地址,不能当来源写进规则`); const net = b === 32 ? n4 : Math.floor(n4 / 2 ** (32 - b)) * 2 ** (32 - b); return { v: 4, n: net, bits: b, text: [net >>> 24, (net >>> 16) & 255, (net >>> 8) & 255, net & 255].join('.') + '/' + b }; }
    if (/^[0-9a-f:]+$/i.test(a) && a.includes(':')) { const b = bits === undefined ? 128 : Number(bits); if (!(b >= 0 && b <= 128)) throw demoErr(400, `「${part}」不是 IP 或网段(例:1.2.3.4、203.0.113.0/24、2001:db8::/32)`); if (b === 0) throw demoErr(400, `「${part}」是全部地址,不能当来源写进规则`); return { v: 6, a: a.toLowerCase(), bits: b, text: a.toLowerCase() + '/' + b }; }
    throw demoErr(400, `「${part}」不是 IP 或网段(例:1.2.3.4、203.0.113.0/24、2001:db8::/32)`);
  });
  const covers = (srcs, ip) => { const n = v4(ip); return srcs.some(s => (s.v === 4 && n !== null ? Math.floor(n / 2 ** (32 - s.bits)) === Math.floor(s.n / 2 ** (32 - s.bits)) : s.v === 6 && n === null && String(ip).toLowerCase().startsWith(s.a.replace(/::$/, ':').replace(/:0*$/, ':')))); };
  const parsePorts = text => String(text || '').split(/[\s,，、;]+/).filter(Boolean).map(part => {
    const [a, b] = part.replace(':', '-').split('-'), lo = Number(a), hi = b === undefined ? lo : Number(b);
    if (!Number.isInteger(lo) || !Number.isInteger(hi) || lo < 1 || hi > 65535 || lo > hi) throw demoErr(400, `「${part}」不是端口或端口区间(例:443、8000-9000)`);
    return [lo, hi];
  }).sort((x, y) => x[0] - y[0] || x[1] - y[1]);
  const lock = msg => { const e = demoErr(409, msg); e.data = { code: 'self_lockout' }; return e; };
  const cluster = () => nodesNow().flatMap(n => [n.publicIp, n.publicIp6].filter(Boolean).map(ip => [ip, n.name]));
  // 校验并规整一条规则(和后端 fwToRule 同一套顺序和报错)
  const toRule = (p, cur) => {
    const kind = String(p.kind || '').trim(), proto = String(p.proto || '').trim().toLowerCase();
    const ports = parsePorts(p.ports), srcs = parseSrc(p.sources);
    if (kind === 'allow') { if (!ports.length) throw demoErr(400, '「放行端口」的规则要填端口'); if (srcs.length) throw demoErr(400, '「放行端口」是对所有人放行,不填来源;要限制来源,另加一条「端口只给指定来源」'); }
    else if (kind === 'deny') { if (!srcs.length) throw demoErr(400, '「拉黑来源」的规则要填来源 IP 或网段'); }
    else if (kind === 'only') { if (!ports.length) throw demoErr(400, '「端口只给指定来源」的规则要填端口'); if (!srcs.length) throw demoErr(400, '「端口只给指定来源」的规则要填允许的来源'); }
    else throw demoErr(400, `不认识的规则种类 "${kind}"`);
    if (proto && proto !== 'tcp' && proto !== 'udp') throw demoErr(400, '协议只能是 tcp、udp 或留空(两种都算)');
    const ids = (p.nodeIds || []).map(Number);
    if (ids.some(i => !nodesNow().some(n => n.id === i))) throw demoErr(400, '选的服务器里有已经不在的,刷新后重选');
    const note = String(p.note || '').trim();
    if ([...note].length > 200) throw demoErr(400, '备注太长(最多 200 个字)');
    const out = { ...(cur || { id: nextId(W.rules), origin: 'manual', enabled: true, createdAt: now(), expiry: 0 }), kind, proto,
      ports: ports.map(([a, b]) => (a === b ? String(a) : a + '-' + b)).join(','), sources: srcs.map(s => s.text).join(','), nodeIds: ids.length ? [...ids].sort((a, b) => a - b) : null, note };
    if (p.seconds !== undefined && p.seconds !== null) { if (p.seconds < 0 || p.seconds > 10 * 365 * 86400) throw demoErr(400, '有效期要在 0 到 10 年之间(0 = 一直有效)'); out.expiry = p.seconds ? now() + Number(p.seconds) : 0; }
    if (p.enabled !== undefined && p.enabled !== null) out.enabled = !!p.enabled;
    if (kind === 'deny') for (const [ip, name] of cluster()) if (covers(srcs, ip)) throw demoErr(400, `来源里有集群服务器「${name}」的地址 ${ip}:拉黑它会断开主机和这台之间的同步`);
    if (!p.force && out.enabled) {
      if (kind === 'deny' && covers(srcs, W.myIp)) throw lock(`来源里包含你现在用的 IP ${W.myIp}:保存后你自己也连不上这些服务器了`);
      if (kind === 'only' && proto !== 'udp' && !covers(srcs, W.myIp)) {
        const web = Number(S.settings.webPort) || 2053, hit = [22, web].filter((x, i, a) => a.indexOf(x) === i && ports.some(([lo, hi]) => x >= lo && x <= hi));
        if (hit.length) throw lock(`这条规则管到了 SSH 或面板的端口(${hit.join(', ')}),允许的来源里却没有你现在用的 IP ${W.myIp}:保存后你自己会被挡在外面`);
      }
    }
    return out;
  };
  const one = text => { const s = parseSrc(text); if (s.length !== 1) return null; return s[0]; };
  const shown = s => (s.v === 4 && s.bits === 32 ? s.text.replace('/32', '') : s.v === 6 && s.bits === 128 ? s.text.replace('/128', '') : s.text);
  const idx = Number(seg[2]);
  if (!seg[1]) return view();
  if (seg[1] === 'settings') {
    if (method !== 'PUT') throw demoErr(405, '方法不允许');
    if (body && typeof body.autoBan === 'boolean') W.autoBan = body.autoBan;
    if (body && typeof body.autoAllow === 'boolean') W.autoAllow = body.autoAllow;
    return view();
  }
  if (seg[1] === 'rules') {
    if (!seg[2]) { W.rules.push(toRule(body || {}, null)); return view(); }
    const cur = W.rules.find(r => r.id === idx);
    if (!cur) throw demoErr(404, '这条规则已经不在了(可能刚到期)');
    if (seg[3] === 'toggle') {
      if (!cur.enabled) toRule({ ...cur, nodeIds: cur.nodeIds || [], enabled: true, seconds: undefined, force: !!(body && body.force) }, cur);
      cur.enabled = !cur.enabled; return view();
    }
    if (method === 'DELETE') { W.rules = W.rules.filter(r => r.id !== idx); return view(); }
    Object.assign(cur, toRule(body || {}, cur)); return view();
  }
  if (seg[1] === 'bans') {
    if (!seg[2]) {
      const s = one(body && body.ip);
      if (!s) throw demoErr(400, '黑名单一次填一个 IP 或网段(例:1.2.3.4、203.0.113.0/24)');
      if (s.v === 4 && (/^(10|127)\./.test(s.text) || /^192\.168\./.test(s.text) || /^172\.(1[6-9]|2\d|3[01])\./.test(s.text))) throw demoErr(400, `${s.text.split('/')[0]} 是回环或内网地址,不能加进黑名单`);
      for (const [ip, name] of cluster()) if (covers([s], ip)) throw demoErr(400, `来源里有集群服务器「${name}」的地址 ${ip}:拉黑它会断开主机和这台之间的同步`);
      if (!(body && body.force) && covers([s], W.myIp)) throw lock(`这里面包含你现在用的 IP ${W.myIp}:保存后你自己就打不开面板了`);
      const ip = shown(s), old = W.bans.find(b => b.ip === ip);
      const row = { id: old ? old.id : nextId(W.bans), ip, reason: 'manual', evidence: String((body && body.note) || '').trim(), count: 0, hits: old ? old.hits : 0, firstAt: old ? old.firstAt : now(), lastAt: now(), expiry: body && body.seconds > 0 ? now() + Number(body.seconds) : 0, auto: false };
      W.bans = W.bans.filter(b => b.ip !== ip).concat(row); return view();
    }
    if (seg[2] === 'clear') { W.bans = body && body.onlyAuto ? W.bans.filter(b => !b.auto) : []; return view(); }
    const b = W.bans.find(x => x.id === idx);
    if (!b) throw demoErr(404, '这条已经不在黑名单里了(可能刚到期)');
    if (seg[3] === 'promote') {
      const r = toRule({ kind: 'deny', sources: b.ip, nodeIds: (body && body.nodeIds) || [], seconds: b.expiry > now() ? b.expiry - now() : 0, note: '', force: !!(body && body.force) }, null);
      r.origin = 'ban'; W.rules.push(r); return view();
    }
    W.bans = W.bans.filter(x => x.id !== idx); return view();
  }
  if (seg[1] === 'allow') {
    if (!seg[2]) {
      const s = one(body && body.ip);
      if (!s) throw demoErr(400, '白名单一次填一个 IP 或网段(例:1.2.3.4、203.0.113.0/24)');
      const ip = shown(s), old = W.allow.find(a => a.ip === ip);
      if (old) old.note = String((body && body.note) || '').trim(); else W.allow.push({ id: nextId(W.allow), ip, note: String((body && body.note) || '').trim(), createdAt: now() });
      return view();
    }
    if (!W.allow.some(a => a.id === idx)) throw demoErr(404, '这条白名单已经不在了');
    W.allow = W.allow.filter(a => a.id !== idx); return view();
  }
  if (seg[1] === 'servers' && seg[3]) {
    const n = nodesNow().find(x => x.id === idx);
    if (!n) throw demoErr(404, '服务器不存在');
    const sy = W.sys[n.id] || (W.sys[n.id] = { frontend: 'none' });
    if (seg[3] === 'open' && sy.blocking) Object.assign(sy, { opened: true, openedAt: now(), openedFrom: sy.frontend, blocking: false, frontend: 'none' });
    else if (seg[3] === 'undo' && sy.opened) Object.assign(sy, { opened: false, blocking: true, frontend: sy.openedFrom || 'ufw', detail: (sy.openedFrom || 'ufw') + ' active' });
    else if (!['state', 'open', 'undo'].includes(seg[3])) throw demoErr(404, '路径无效');
    const { st, ports } = stateOf(n);
    st.at = now();
    return { id: n.id, state: st, ports, error: '' };
  }
  throw demoErr(404, '路径无效');
}

function route(method, path, query, body) {
  const seg = path.split('/');
  const id = Number(seg[1]);
  const find = (list, i) => list.find(x => x.id === i) || notFound();
  const listByPrefix = k => { const exact = F[`GET ${path}${query ? '?' + query : ''}`]; if (exact !== undefined) return clone(exact); const key = Object.keys(F).find(x => x.startsWith(`GET ${path}?`) || x === `GET ${path}`); return key ? clone(F[key]) : null; };

  if (method === 'GET') {
    switch (seg[0]) {
      case 'status': return status();
      case 'settings': return clone(S.settings);
      case 'lines':
        if (seg[1] === 'nodes' && seg[2] === 'online') return { online: S.nodeOnline || { '1:s1': 1, '1:f2': 2, '2:s2': 1, '4:s1': 1, '4:s2': 3 } }; // 节点设置页「在线 / 容量」(截图用的场景可以换成自己的)
        if (seg[2] === 'detail') return lineDetail(find(S.lines, id), new URLSearchParams(query));
        if (seg[2] === 'reach') return demoLineReach(find(S.lines, id), 'GET', seg[3]); // 线路详情「测大陆连通」的进度
        return clone(S.lines);
      case 'upstreams': if (seg[1] === 'health') return demoHealth(); return clone(S.upstreams);
      case 'users': if (seg[2] === 'plan-ops') return clone(S.planOps.filter(o => o.userId === id).slice(0, Number(new URLSearchParams(query).get('limit')) || 20));
        if (seg[2] === 'sub') { const u = find(S.users, id); return { link: subUrl(u), clash: subUrl(u) + '?format=clash', json: subUrl(u) + '?format=json' }; } return clone(S.users);
      case 'plans': return S.plans.map(planView);
      case 'nodes':
        if (seg[2] === 'disk-clean') return demoDiskClean(id, false);
        if (seg[2] === 'quota') { const n = (S.nodes.nodes || []).find(x => x.id === id); if (!n) throw demoErr(404, '服务器不存在'); return { quota: n.quota ? clone(n.quota) : null, enabled: n.enabled !== false, suspended: !!n.suspended }; }
        return clone(S.nodes);
      // 主机容灾(见 haView;没有这一条时界面拿到 {},会把每台都标成「副机」)
      case 'ha': if (seg[1] === 'plan') return haDemoBlock(); if (seg[1]) return notFound();
        return haView(/(^|&)full=1(&|$)/.test(query));
      case 'exts': if (seg[2] === 'nodes') return extNodes(id, seg, 'GET', null); return clone(S.exts);
      case 'onlines': return clone(S.onlines);
      case 'rules': if (seg[2] === 'states') return clone(S.limitStates.filter(s => s.ruleId === id)); if (seg[1]) return ruleView(find(S.rules, id)); return S.rules.map(ruleView);
      case 'limitstates': return clone(S.limitStates);
      case 'fw': return demoFw('GET', seg, null);
      case 'reward': return demoReward('GET', seg, query, null);
      case 'resellers': if (seg[2] === 'users') return S.users.filter(u => u.resellerId === id); return clone(S.resellers);
      case 'update': return { current: 'demo', latest: '', hasUpdate: false, canUpdate: false };
      case 'ops': { if (seg[1] === 'status') return fx('GET ops/status') || { running: false, log: [] }; const o = fx('GET ops') || { info: {}, status: {}, params: {} };
        // 导出实例跑在 Windows 上,系统信息是空的;演示里给一台像样的 Linux 主机
        o.info = { ...(o.info || {}), linux: true, root: true, os: 'linux', kernel: '6.8.0-45-generic', arch: 'amd64', hostname: 'tokyo', uptime: 37 * 86400 + 4 * 3600, load: '0.12 0.09 0.05',
          memTotal: 2 * 1073741824, memAvail: 1.3 * 1073741824, swapTotal: 2 * 1073741824, swapFree: 2 * 1073741824, diskTotal: 40 * 1073741824, diskFree: 29 * 1073741824,
          cc: 'bbr', qdisc: 'fq', nofile: 1048576, ntp: 'yes', tuned: true, limits: true,
          warp: { ...((o.info || {}).warp || {}), installed: true, service: 'active', socks: 'active', port: 40000, listening: true, status: 'Connected', exit: 'on', exitIp: '104.28.0.1', exitLoc: 'JP', exitColo: 'NRT' } };
        o.status = o.status || { running: false, log: [] };
        if (!(o.tasks || []).some(x => x.name === 'disk-clean')) o.tasks = [...(o.tasks || []), { name: 'disk-clean', title: '清理磁盘', desc: '清掉软件包缓存、把 journald 回收到上限、超过 100MB 的文本日志只留最后 5MB、删掉一天前轮转下来的旧日志,并装一个每小时检查的定时器(syslog 超过 200MB 就立刻轮转)。不碰 m-ui 的数据和备份' }];
        o.disk = { path: '/etc/m-ui', total: 40 * 1073741824, used: 11 * 1073741824, free: 29 * 1073741824, logs: 1.2 * 1073741824, cache: 0.3 * 1073741824, swap: 2 * 1073741824, at: now() };
        return o; }
      case 'cert': if (seg[1] === 'status') return fx('GET cert/status') || { running: false, log: [] }; { const c = fx('GET cert') || { settings: S.settings, status: {} }; const use = S.certs.find(x => x.certFile === S.certUse.lines); return { ...c, notifyConfigured: true, certs: clone(S.certs), certPath: S.certUse.lines, panelCert: S.certUse.panel, subCert: S.certUse.sub, panelTLS: !!S.certUse.panel, subTLS: !!S.certUse.sub, source: use ? use.source : '', info: use ? clone(use.info) : { exists: false } }; }
      case 'sub-logo': return clone(S.logo || { set: false });
      case 'reach': return clone(S.reach);
      case 'conns': return S.connsOverride || recentConns(); // S.connsOverride:截图场景按生产量级给
      case 'sub-templates': return { clash: '# 只写想加的部分,节点和分组由面板填\nrules:\n  - DOMAIN-SUFFIX,example.org,DIRECT\n  - MATCH,Proxy\n', singbox: '{\n  "route": {\n    "rules": [{ "domain_suffix": ["example.org"], "outbound": "direct" }],\n    "final": "proxy"\n  }\n}\n' };
      case 'stats': { // 流量图、排行、明细:按请求的范围现算(见 demoSeries)
        const q = new URLSearchParams(query), hours = demoHours(q.get('hours'));
        if (seg[1] === 'top') { const lim = Number(q.get('limit')) || 0; return (S.topOverride || demoTop(hours)).slice(0, lim > 0 && lim <= 100 ? lim : 10); } // S.topOverride:截图场景按生产量级给
        if (seg[1] === 'breakdown') return demoBreakdown(hours);
        const tag = q.get('tag'), b = Number(q.get('bucket')) || 0, tz = q.get('tz');
        const tzMin = tz !== null && tz !== '' && Math.abs(Number(tz)) <= 14 * 60 ? Number(tz) : demoTzMin();
        // 桶宽不合适(没给、太细、超过 7 天)时真面板自己取 ≤240 个点;演示里退回这段范围该用的桶宽
        return demoSeries(tag ? [tag] : S.users.map(u => u.name), hours, b >= 60 && b <= 7 * 86400 ? b : demoBucket(hours), tzMin); }
      case 'backup': if (seg[1] === 'list') return clone(demoBackupList()); return listByPrefix(path) ?? {};
      case 'sublogs': return demoSubLogs(query);
      case 'audit': { const lim = Number(new URLSearchParams(query).get('limit')) || 0; return lim && lim <= 40 ? (S.logsCleared.audit ? [] : S.auditOverride || (listByPrefix(path) ?? [])) : demoAudit(query); } // S.auditOverride:截图场景 // 概况「最近操作」沿用导出的那份,日志页用现编的
      case 'logs': return S.logsCleared.core ? [] : listByPrefix(path) ?? [];
      case 'keygen': { const t = new URLSearchParams(query).get('type');
        if (t === 'uuid') return { uuid: crypto.randomUUID() };
        if (t === 'port') return { port: 10000 + Math.floor(Math.random() * 55000) };
        if (t === 'shortid') return { shortId: rnd(8) };
        if (t === 'password') return { password: rnd(Number(new URLSearchParams(query).get('len')) || 16) };
        return { privateKey: 'demo-private-key-' + rnd(24), publicKey: 'demo-public-key-' + rnd(24) }; }
      case 'admin': case 'self':
        if (seg[1] === 'api' && seg[2] === 'keys' && seg[4] === 'logs') return demoKeyLogs(seg[0], seg[3], query);
        if (seg[1] === 'api' && seg[2] === 'keys') return { keys: S.apiKeys[seg[0]].map(k => demoApiKeys(seg[0], 'GET', k.id)), perms: API_PERMS, you: '203.0.113.5' };
        if (seg[0] === 'admin' && seg[1] === 'info') { // 演示里外部 API 开着:看得到密钥列表(默认密钥和一把只读、限了来源的);两步验证可以走一遍开启流程
          const i = listByPrefix(path) ?? {};
          return { ...i, apiEnabled: S.apiOn !== false, totpEnabled: !!S.totpOn, totpPending: S.totpPending ? { secret: S.totpPending } : undefined };
        }
        return listByPrefix(path) ?? {};
      default: return listByPrefix(path) ?? {};
    }
  }
  // 写操作:只改内存
  switch (seg[0]) {
    case 'login': return { username: 'admin' };
    case 'logout': return ok();
    case 'settings': Object.assign(S.settings, body || {}); return ok({ note: 'Demo mode' });
    case 'logs': // 清空某一类日志;改「记录日志」开关和记录级别
      if (method === 'DELETE') { S.logsCleared[new URLSearchParams(query).get('kind') || 'core'] = true; return ok(); }
      if (body && body.enabled !== undefined) S.settings.logEnabled = String(body.enabled);
      if (body && body.level) S.settings.coreLogLevel = body.level;
      return ok();
    case 'sub-logo': if (method === 'DELETE') { S.logo = null; return { set: false }; } S.logo = logoOf(body && body.data); return clone(S.logo);
    case 'users': {
      if (seg[1] === 'batch') { const { ids = [], action, days = 0 } = body || {}; let n = 0;
        for (const uid of ids) { const u = S.users.find(x => x.id === uid); if (!u) continue; n++;
          if (action === 'enable') u.enabled = true; else if (action === 'disable') u.enabled = false; else if (action === 'reset') { u.up = 0; u.down = 0; }
          else if (action === 'extend') u.expiry = Math.max(u.expiry || 0, now()) + days * 86400; else if (action === 'delete') S.users = S.users.filter(x => x.id !== uid);
          else if (action === 'plan' && body.mode !== 'bind') { const p = S.plans.find(x => x.id === body.planId); if (p) demoPlanOp(u, p, body.mode === 'extend' ? 'extend' : 'renew', Math.max(1, body.periods || 1)); } }
        return { affected: n }; }
      if (seg[1] === 'bulk') { const { prefix = 'user', count = 1 } = body || {}; const made = [];
        for (let i = 0; i < Math.min(count, 50); i++) { const u = { id: nextId(S.users), name: `${prefix}-${rnd(5)}`, enabled: true, volume: body.volume || 0, expiry: 0, up: 0, down: 0, deviceLimit: 0, lineIds: body.lineIds || S.lines.map(l => l.id), onlineIps: [], createdAt: now() }; u.subUrl = subUrl(u); S.users.push(u); made.push({ name: u.name, link: u.subUrl + '?format=clash' }); }
        return made; }
      if (method === 'POST' && !seg[1]) { const u = { id: nextId(S.users), enabled: true, up: 0, down: 0, onlineIps: [], createdAt: now(), ...body }; u.lineIds = body.lineIds || []; u.subUrl = subUrl(u); S.users.push(u); return clone(u); }
      const u = find(S.users, id);
      if (method === 'DELETE' && !seg[2]) { S.users = S.users.filter(x => x.id !== id); return ok(); }
      if (method === 'PUT') { Object.assign(u, body, { id }); u.subUrl = subUrl(u); return clone(u); }
      if (seg[2] === 'reset') { u.up = 0; u.down = 0; return ok(); }
      if (seg[2] === 'kick') { const n = (u.onlineIps || []).length; u.onlineIps = []; S.onlines.users = S.onlines.users.filter(n2 => n2 !== u.name); return { closed: n }; }
      if (seg[2] === 'plan') { const b = body || {}; if (b.mode === 'bind') { u.planId = b.planId || 0; return ok(); }
        const p = S.plans.find(x => x.id === (b.planId || u.planId)); if (!p) throw demoErr(400, '用户没有所属套餐,请给 planId 或 plan');
        const { res } = demoPlanOp(u, p, b.mode === 'extend' ? 'extend' : 'renew', Math.max(1, b.periods || 1), '', b.dryRun, b.remark); return b.dryRun ? res : ok({ expiry: u.expiry }); }
      if (seg[2] === 'change') { const b = body || {}; const p = S.plans.find(x => x.id === b.planId); if (!p) throw demoErr(400, '缺少 planId 或 plan');
        const pre = demoPlanOp(u, p, b.mode, Math.max(b.mode === 'prorate' ? 0 : 1, b.periods ?? 1), b.usage, true).res;
        if (b.dryRun) return pre;
        if (pre.needsForce && !b.force) { const e = demoErr(409, '这次变更有需要确认的影响(见 warnings),确认后带 force: true 重试'); e.data = { code: 'needs_force', warnings: pre.warnings, preview: pre }; throw e; }
        return demoPlanOp(u, p, b.mode, Math.max(b.mode === 'prorate' ? 0 : 1, b.periods ?? 1), b.usage, false, b.remark).res; }
      if (seg[2] === 'share') { if (method === 'DELETE') { u.shareUrl = ''; return ok(); } u.shareUrl = subUrl(u).replace(/\/sub\/.*$/, '/sub/share-' + rnd(20)); return { url: u.shareUrl }; }
      return ok();
    }
    case 'route-apps': return { results: demoApps(body && body.names) };
    case 'reality-check': {
      const sites = ['www.microsoft.com', 'www.apple.com', 'www.cloudflare.com', 'www.amazon.com', 'www.nvidia.com', 'www.samsung.com'];
      if (body && body.server) return { server: body.server, port: body.port || 443, ok: true, ms: 12 + Math.floor(Math.random() * 40) };
      return { results: sites.map((server, i) => ({ server, port: 443, ok: true, ms: 8 + i * 7 })) };
    }
    case 'lines': {
      if (seg[1] === 'sort') { const ids = body || []; ids.forEach((lid, i) => { const l = S.lines.find(x => x.id === lid); if (l) l.sort = i + 1; }); S.lines.sort((a, b) => (a.sort || 0) - (b.sort || 0)); return ok(); }
      if (method === 'POST' && !seg[1]) { const l = { id: nextId(S.lines), enabled: true, sort: S.lines.length + 1, userCount: 0, ...body }; l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; if (body.assignAll) { l.userCount = S.users.length; S.users.forEach(u => { u.lineIds = [...(u.lineIds || []), l.id]; }); } S.lines.push(l); return clone(l); }
      if (seg[1] === 'nodes' && method === 'PUT') { // 节点页的批量操作:勾选的节点改同样的几项(set 里没有的项不动)
        const parse = v => { try { return typeof v === 'string' ? JSON.parse(v) : (v || null); } catch { return null; } };
        const touched = new Map();
        for (const it of (body.items || [])) {
          const ln = S.lines.find(x => x.id === it.line);
          if (!ln) continue;
          const opts = parse(ln.nodeOpts) || {}, o = { ...(opts[it.key] || {}), ...(body.set || {}) };
          const kept = {};
          if (o.hidden) kept.hidden = true;
          for (const k of ['ratio', 'speed', 'userSpeed', 'cap']) if (o[k] > 0) kept[k] = o[k];
          if (Object.keys(kept).length) opts[it.key] = kept; else delete opts[it.key];
          ln.nodeOpts = Object.keys(opts).length ? opts : null;
          touched.set(ln.id, ln);
        }
        return { lines: clone([...touched.values()]), changed: (body.items || []).length };
      }
      const l = find(S.lines, id);
      if (seg[2] === 'reach') return demoLineReach(l, method, seg[3]);
      if (seg[2] === 'toggle') { l.enabled = !l.enabled; demoSyncBlocked(); return ok(); }
      if (seg[2] === 'node' && method === 'PUT') { // 节点页:整份替换一个节点的设置;带 src 的顺手改那个入口的来源 IP
        const parse = v => { try { return typeof v === 'string' ? JSON.parse(v) : (v || null); } catch { return null; } };
        const opts = parse(l.nodeOpts) || {}, o = body.opt || {};
        const kept = {};
        if (o.hidden) kept.hidden = true;
        for (const k of ['ratio', 'speed', 'userSpeed', 'cap']) if (o[k] > 0) kept[k] = o[k];
        if (Object.keys(kept).length) opts[body.key] = kept; else delete opts[body.key];
        l.nodeOpts = Object.keys(opts).length ? opts : null;
        if (body.src !== undefined && String(body.key).startsWith('f')) {
          const fs = parse(l.forwards) || [];
          fs.forEach(f => { if ('f' + f.id === body.key) { if (body.src) f.src = body.src; else delete f.src; } });
          l.forwards = fs;
        }
        return clone(l);
      }
      if (method === 'DELETE') {
        // 和后端同一口径:被「线路上游」用着的线路不让删(那条上游会悬空)
        const relay = S.upstreams.find(u => Number(u.fromLine) === id);
        if (relay) throw demoErr(400, `上游「${relay.name}」用着这条线路,先删掉或改掉它`);
        S.lines = S.lines.filter(x => x.id !== id); S.users.forEach(u => { u.lineIds = (u.lineIds || []).filter(x => x !== id); }); demoSyncBlocked(); return ok();
      }
      if (method === 'PUT') { Object.assign(l, body, { id }); l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; demoSyncBlocked(); return clone(l); }
      return ok();
    }
    case 'upstreams': {
      if (seg[1] === 'test') return S.upstreams.map(u => ({ id: u.id, servers: demoUpTest(u) }));
      if (seg[1] === 'health') { S.upstreams.forEach(demoUpTest); S.health.lastRun = now(); return ok(); } // 立即巡检
      if (seg[1] === 'parse') return { name: 'Parsed upstream', type: 'shadowsocks', options: { server: '203.0.113.20', server_port: 8388, method: 'aes-256-gcm', password: 'demo' } };
      if (seg[1] === 'batch') { // 勾选几条一起删、一起停止 / 恢复跟随(和后端同一口径,见 web/upstream_batch.go)
        const ids = [...new Set((body && body.ids) || [])];
        if (!ids.length) throw demoErr(400, '没有选中上游');
        const ups = ids.map(i => S.upstreams.find(x => x.id === i));
        if (ups.some(x => !x)) throw demoErr(400, '有上游不存在,请刷新后重试');
        if (body.action === 'follow' || body.action === 'unfollow') {
          const on = body.action === 'follow';
          let n = 0;
          for (const u of ups) {
            if (!u.source || !u.source.stop === on) continue;
            if (on) { delete u.source.stop; u.source.note = ''; u.source.detail = ''; u.source.triedAt = 0; } else u.source.stop = 'user';
            n++;
          }
          return { changed: n };
        }
        if (body.action !== 'delete') throw demoErr(400, '不认识的批量操作');
        const rulesOf = l => { let v = l.routeRules; if (typeof v === 'string') { try { v = JSON.parse(v); } catch { v = []; } } return Array.isArray(v) ? v : []; };
        const optsOf = g => (typeof g.options === 'string' ? JSON.parse(g.options || '{}') : g.options || {});
        const skipped = [], gone = new Set();
        for (const u of ups) {
          const lines = S.lines.filter(l => l.upstreamId === u.id || rulesOf(l).some(r => Number(r.to) === u.id)).map(l => l.name);
          if (lines.length) skipped.push({ name: u.name, lines }); else gone.add(u.id);
        }
        const groups = new Set(ups.filter(u => gone.has(u.id) && u.type === 'urltest').map(u => u.name));
        for (const u of ups) {
          if (!gone.has(u.id) || u.type === 'urltest') continue;
          const g = S.upstreams.find(x => x.type === 'urltest' && x.id !== u.id && (optsOf(x).outbounds || []).includes(u.name) && !groups.has(x.name));
          if (g) { gone.delete(u.id); skipped.push({ name: u.name, group: g.name }); }
        }
        S.upstreams = S.upstreams.filter(x => !gone.has(x.id)); gone.forEach(i => S.upBroken.delete(i));
        return { deleted: gone.size, skipped };
      }
      if (method === 'POST' && !seg[1]) { const u = { id: nextId(S.upstreams), sort: S.upstreams.length + 1, ...body }; S.upstreams.push(u); return clone(u); }
      const u = find(S.upstreams, id);
      if (seg[2] === 'test') return { id, servers: demoUpTest(u) };
      if (seg[2] === 'source') { // 跟随是个开关:停下只是不再自动换,来源留着,能再打开(DELETE 是老写法,等于停下)
        if (!u.source) throw demoErr(400, '这条上游不是从外部节点加来的,没有可以跟随的订阅');
        if (method === 'PUT' && body && body.follow === true) { delete u.source.stop; u.source.note = ''; u.source.detail = ''; u.source.triedAt = 0; }
        else u.source.stop = 'user';
        return ok();
      }
      if (method === 'DELETE') {
        // 和后端同一口径:还有线路在用(当出站的、分流规则指过来的)不让删,并点名是哪些;是某个上游组的成员也不让删
        const rulesOf = l => { let v = l.routeRules; if (typeof v === 'string') { try { v = JSON.parse(v); } catch { v = []; } } return Array.isArray(v) ? v : []; };
        const using = S.lines.filter(l => l.upstreamId === id || rulesOf(l).some(r => Number(r.to) === id)).map(l => l.name);
        if (using.length) throw demoErr(400, `这条上游还有 ${using.length} 条线路在用,不能删除:${using.slice(0, 8).join(', ')}${using.length > 8 ? ' …' : ''}。先把这些线路的出站(或指向它的分流规则)换成别的`);
        const grp = S.upstreams.find(g => g.type === 'urltest' && g.id !== id && ((typeof g.options === 'string' ? JSON.parse(g.options || '{}') : g.options || {}).outbounds || []).includes(u.name));
        if (grp) throw demoErr(400, `这条上游在上游组「${grp.name}」里,先从组里去掉`);
        S.upstreams = S.upstreams.filter(x => x.id !== id); S.upBroken.delete(id); return ok();
      }
      if (method === 'PUT' && u.source && !u.source.stop && JSON.stringify(body.options) !== JSON.stringify(u.options)) u.source.stop = 'edited'; // 手动改过参数就停止跟随(来源留着)
      if (method === 'PUT') { Object.assign(u, body, { id }); S.upBroken.delete(id); demoUpTest(u); return clone(u); } // 改过就算修好了,顺手复查一次
      return ok();
    }
    case 'nodes': {
      const list = S.nodes.nodes;
      if (method === 'POST' && !seg[1] && body && body.quota) { const probe = demoQuotaApply({ isLocal: false }, body.quota); body = { ...body, quota: probe }; } // 一起给了流量额度:先校验,不合适的整份不建
      if (method === 'POST' && !seg[1]) { const n = { id: nextId(list), enabled: true, isLocal: false, status: { ok: true, version: 'demo', synced: true, coreRunning: true, uptime: 60, onlineUsers: 0, lastPush: now(), lastSeen: now() }, ...body }; delete n.token; n.hasToken = true; list.push(n); return clone(n); }
      if (method === 'POST' && seg[1] === 'group') { // 改组名:整组一起改
        const from = String((body || {}).from || '').trim(), to = String((body || {}).to || '').trim();
        let changed = 0;
        for (const x of list) if ((x.group || '').trim() === from) { x.group = to; changed++; }
        return ok({ changed });
      }
      const n = find(list, id);
      if (seg[2] === 'test') return { ok: true, version: 'demo', hostname: 'demo', coreRunning: true };
      // 编辑中转入口时现问端口:演示里新加坡那台的 30001 被 realm 占着(和它上报的「端口被占」是同一件事)
      if (seg[2] === 'port-check') return id === 2 && Number((body || {}).port) === 30001 ? { asked: true, free: false, self: false, by: 'realm' } : { asked: true, free: true, self: false, by: '' };
      if (seg[2] === 'push') return ok();
      if (seg[2] === 'disk-clean') return demoDiskClean(id, true);
      if (seg[2] === 'quota') { // 流量额度:单独改 / 重置本期用量
        if (seg[3] === 'reset') {
          if (n.quota) { const q = n.quota; q.used = q.up = q.down = 0; q.periodStart = now(); if (q.resetMode === 'days') q.nextReset = demoQuotaNext(q.resetMode, q.resetDays, q.resetDay); }
        } else if (method === 'PUT') n.quota = demoQuotaApply(n, body || {});
        demoQuotaJudge(n);
        return { quota: n.quota ? clone(n.quota) : null, enabled: n.enabled !== false, suspended: !!n.suspended };
      }
      if (method === 'DELETE') {
        // 和后端同一口径:本机删不得;被「线路上游」用着的删不得;删掉后引用它的东西一起收拾 ——
        // 只部署在这台上的线路停用、经它中转或转给它的入口去掉、防火墙规则里只管它一台的整条删掉(不留成「全部服务器」)
        if (n.isLocal) throw demoErr(400, '不能删除本机');
        const relay = S.upstreams.find(u => Number(u.fromNode) === id);
        if (relay) throw demoErr(400, `上游「${relay.name}」用着这台服务器上的线路,先删掉或改掉它`);
        const arr = v => { try { const x = typeof v === 'string' ? JSON.parse(v || '[]') : (v || []); return Array.isArray(x) ? x : []; } catch { return []; } };
        const disabledLines = [], prunedForwards = [];
        for (const l of S.lines) {
          const ids = arr(l.nodeIds);
          if (ids.includes(id)) {
            const kept = ids.filter(x => x !== id);
            if (!kept.length) { l.enabled = false; disabledLines.push(l.name); }
            l.nodeIds = typeof l.nodeIds === 'string' ? JSON.stringify(kept) : kept;
          }
          const fs = arr(l.forwards), left = fs.filter(f => f.via !== id && f.to !== id);
          if (left.length !== fs.length) { prunedForwards.push(l.name); l.forwards = typeof l.forwards === 'string' ? JSON.stringify(left) : left; }
        }
        let removedFwRules = 0;
        S.fw.rules = S.fw.rules.filter(r => {
          if (!Array.isArray(r.nodeIds) || !r.nodeIds.includes(id)) return true;
          r.nodeIds = r.nodeIds.filter(x => x !== id);
          if (r.nodeIds.length) return true;
          removedFwRules++; return false;
        });
        S.fw.servers = S.fw.servers.filter(x => x.id !== id);
        S.nodes.nodes = list.filter(x => x.id !== id);
        demoSyncBlocked();
        return ok({ disabledLines, revokedUsers: [], revokedResellers: [], prunedForwards, removedFwRules });
      }
      if (method === 'PUT') {
        const { token, quota, suspended, ...rest } = body || {};
        const next = quota ? demoQuotaApply(n, quota) : null; // 额度和服务器一起存:不合适的整份不存
        // 被额度自动停用着的:手动启用了下一轮又会被停,直接说清楚该改什么(同一次保存里调高了额度、延了期的放行)
        if (!n.isLocal && n.enabled === false && rest.enabled) {
          const q = next || n.quota;
          const st = q && q.autoDisable ? demoQuotaState(q) : '';
          if (st === 'traffic') throw demoErr(400, '这台服务器本期流量已用完,按设置自动停用着:调高额度或重置用量后它会自动启用(关掉「用完或到期后自动停用」只是不停用整台,代理服务仍然停着)');
          if (st === 'expired') throw demoErr(400, '这台服务器已到期,按设置自动停用着:延长到期时间后它会自动启用(关掉「用完或到期后自动停用」只是不停用整台,代理服务仍然停着)');
        }
        if (n.quota && rest.enabled !== undefined && (rest.enabled !== false) !== (n.enabled !== false)) n.quota.autoOff = false; // 手动启停:以后归管理员
        Object.assign(n, rest, { id });
        if (next) { next.autoOff = n.quota ? n.quota.autoOff : false; n.quota = next; }
        demoQuotaJudge(n);
        return { ...clone(n), ok: '1', enabled: n.enabled !== false, suspended: !!n.suspended };
      }
      return ok();
    }
    case 'plans': {
      if (method === 'POST' && !seg[1]) { const { ruleIds, ...rest } = body || {}; const p = { id: nextId(S.plans), ...rest }; S.plans.push(p); setPlanRules(p.id, ruleIds); return planView(p); }
      const p = find(S.plans, id);
      if (method === 'DELETE') { S.plans = S.plans.filter(x => x.id !== id); for (const u of S.users) if (u.planId === id) u.planId = 0; setPlanRules(id, []); return ok(); }
      if (method === 'PUT') { const { ruleIds, ...rest } = body || {}; Object.assign(p, rest, { id }); setPlanRules(id, ruleIds); return planView(p); }
      return ok();
    }
    case 'resellers': {
      if (method === 'POST' && !seg[1]) { const r = { id: nextId(S.resellers), enabled: true, used: 0, ...body }; S.resellers.push(r); return clone(r); }
      const r = find(S.resellers, id);
      if (seg[2] === 'passwd') return { password: rnd(12) };
      if (seg[2] === 'reset') return ok();
      if (method === 'DELETE') { // 和后端一样:代理名下的用户一起删
        const mine = S.users.filter(u => Number(u.resellerId) === id).length;
        S.users = S.users.filter(u => Number(u.resellerId) !== id);
        S.resellers = S.resellers.filter(x => x.id !== id);
        return { users: mine };
      }
      if (method === 'PUT') { Object.assign(r, body, { id }); return clone(r); }
      return ok();
    }
    case 'exts': {
      if (method === 'POST' && !seg[1]) { const e = { id: nextId(S.exts), enabled: true, nodeCount: 3, ...body }; S.exts.push(e); return clone(e); }
      const e = find(S.exts, id);
      if (seg[2] === 'refresh') return { clash: 7, links: 7, total: 7, out: e.nodeCount || 7, unchanged: true };
      if (seg[2] === 'preview') return { nodes: [] };
      if (seg[2] === 'nodes') return extNodes(id, seg, method, body);
      if (method === 'DELETE') { S.exts = S.exts.filter(x => x.id !== id); for (const u of S.upstreams) if (u.source && u.source.extId === id) delete u.source; return ok(); } // 从它加来的上游还在,只是没有订阅可跟了
      if (method === 'PUT') { Object.assign(e, body, { id }); return clone(e); }
      return ok();
    }
    case 'rules': {
      if (method === 'POST' && !seg[1]) { const r = { id: nextId(S.rules), enabled: true, tightenOnly: true, userIds: [], resellerIds: [], createdAt: now(), ...body }; S.rules.push(r); return ruleView(r); }
      const r = find(S.rules, id);
      if (method === 'DELETE') { S.rules = S.rules.filter(x => x.id !== id); S.limitStates = S.limitStates.filter(s => s.ruleId !== id); return ok(); }
      if (method === 'PUT') { Object.assign(r, body, { id }); if (!r.enabled) S.limitStates = S.limitStates.filter(s => s.ruleId !== id); return ruleView(r); }
      return ok();
    }
    case 'fw': return demoFw(method, seg, body);
    case 'reward': return demoReward(method, seg, query, body);
    case 'update': throw new ApiError(400, 'Demo mode: nothing to update here');
    case 'ha':
      if (!seg[1]) { // 保存设置
        const b = body || {}, c = HA.config;
        if (b.takeSub && !b.dnsToken && !c.dnsTokenSet) throw new ApiError(400, zh ? '接管订阅要先填 DNS 接口(Cloudflare 令牌,或阿里云、腾讯云 DNSPod 的密钥):接任后靠它把订阅域名改指到新主机' : 'Taking over subscriptions needs a DNS interface (a Cloudflare token, or Alibaba Cloud / Tencent Cloud DNSPod keys): after a takeover it points the subscription domain to the new master');
        for (const k of ['mode', 'standbys', 'replicaMinutes', 'confirmMinutes', 'autoMinutes', 'rankStepMinutes', 'witness', 'allowPlain', 'dnsNames', 'takeSub', 'dnsProvider']) if (b[k] !== undefined) c[k] = b[k];
        if (c.takeSub && !/(^|[\s,])sub\.example\.com([\s,]|$)/.test(c.dnsNames || '')) c.dnsNames = ['sub.example.com', c.dnsNames].filter(Boolean).join(',');
        if (b.dnsToken) c.dnsTokenSet = true;
        if (b.clearDnsToken) c.dnsTokenSet = false;
        return ok();
      }
      if (seg[1] === 'pause') { HA.config.pauseUntil = body && body.minutes > 0 ? now() + body.minutes * 60 : 0; return ok(); }
      if (seg[1] === 'notice') return ok();
      if (seg[1] === 'dns-test') return { records: [], error: zh ? '演示模式:不连 DNS 服务商' : 'Demo mode: the DNS provider is not contacted' };
      throw new ApiError(400, zh ? '演示模式:不会真的换主机或接任' : 'Demo mode: the master is not actually moved or taken over');
    case 'backup': {
      if (seg[1] === 'restore-ack') { S.restoreAcked = true; return ok(); }
      const list = demoBackupList().files;
      if (seg[1] === 'run') { // 存一份:列表多一行,超出保留份数的旧备份跟着删(升级前那份不算)
        const f = { name: demoBkName(now()), size: 4300000 + Math.floor(Math.random() * 200000), time: now() };
        list.unshift(f);
        const keep = Number(S.settings.backupKeep) > 0 ? Number(S.settings.backupKeep) : 7;
        const own = list.filter(x => x.name.startsWith('m-ui-'));
        S.backups = list.filter(x => !x.name.startsWith('m-ui-') || own.indexOf(x) < keep);
        return clone(f);
      }
      if (seg[1] === 'file' && method === 'DELETE') { const name = new URLSearchParams(query).get('name'); S.backups = list.filter(x => x.name !== name); return ok(); }
      if (seg[1] === 'inspect') return { users: S.users.length, lines: S.lines.length, upstreams: S.upstreams.length, meta: { host: 'tokyo', version: 'demo', time: ((body && list.find(x => x.name === body.name)) || { time: now() }).time, certs: ['panel.example.com'] }, note: 'Demo mode' };
      return ok();
    }
    case 'cert':
      if (seg[1] === 'use') { const c = S.certs.find(x => x.name === body.name); S.certUse[body.target] = c ? c.certFile : ''; syncCertUse(); return ok(); }
      if (seg[1] === 'delete') {
        // 和后端同一口径:还在用的证书不让删,报的是同一句话
        const c = S.certs.find(x => x.name === body.name);
        if (!c) throw demoErr(400, '证书库里没有这张证书');
        if (c.certFile === S.certUse.lines) throw demoErr(400, '这张证书是线路默认证书,先换一张再删');
        if (c.certFile === S.certUse.panel) throw demoErr(400, '面板 HTTPS 正在用这张证书,先换一张再删');
        if (c.certFile === S.certUse.sub) throw demoErr(400, '订阅 HTTPS 正在用这张证书,先换一张再删');
        const using = S.lines.filter(l => { const tl = tlsOf(l); return tl.mode === 'cert' && tl.cert === c.name; }).map(l => l.name);
        if (using.length) throw demoErr(400, `线路「${using.join('、')}」单独选了这张证书,先改掉再删`);
        S.certs = S.certs.filter(x => x.name !== body.name); syncCertUse(); return ok();
      }
      if (seg[1] === 'renew') { const c = S.certs.find(x => x.name === body.name); if (c && c.renewable) { c.lastRenewOk = now(); c.renewFails = 0; c.lastRenewErr = ''; } return ok({ note: 'Demo mode' }); }
      if (seg[1] === 'autorenew') { const c = S.certs.find(x => x.name === body.name); if (c && c.source === 'acme' && c.method !== 'manual') { c.autoRenew = !!body.on; Object.assign(c, body.on ? renewOf(c.source, c.method, c.info.daysLeft, c.info.selfSigned) : { renewable: false, reason: 'disabled' }); } return ok(); }
      return ok({ note: 'Demo mode' });
    case 'reach': { const ids = body && body.nodeId ? [body.nodeId] : S.reach.results.map(r => r.nodeId);
      for (const r of S.reach.results) if (ids.includes(r.nodeId)) { r.at = now(); r.auto = false; if (r.v6) r.v6.at = now(); (S.reach.history[r.nodeId] = S.reach.history[r.nodeId] || []).push({ at: now(), verdict: r.verdict, auto: false }); }
      return ok(); }
    case 'ops': if (seg[1] === 'warp-check') return { exit: 'on', ip: '104.28.0.1', loc: 'US', colo: 'LAX' }; return ok();
    case 'agent': if (seg[1] === 'rotate') return { token: 'demo-' + rnd(32) }; return ok();
    case 'admin': case 'self':
      if (seg[1] === 'api' && seg[2] === 'keys') return demoApiKeys(seg[0], method, seg[3], seg[4], body);
      if (seg[0] === 'admin' && seg[1] === 'api' && !seg[2]) { S.apiOn = !!(body && body.enabled); return { enabled: S.apiOn }; }
      if (seg[0] === 'admin' && seg[1] === 'totp') {
        if (seg[2] === 'setup') { S.totpPending = 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP'; return { secret: S.totpPending }; }
        if (seg[2] === 'cancel') { S.totpPending = ''; return ok(); }
        if (seg[2] === 'enable') { if (!/^\d{6}$/.test(String((body && body.code) || ''))) throw demoErr(400, '验证码不正确,请确认认证器已添加此密钥且手机时间准确'); S.totpOn = true; S.totpPending = ''; return ok(); }
        if (seg[2] === 'disable') { S.totpOn = false; return ok(); }
      }
      return ok();
    case 'reload': case 'password': case 'notify': return ok();
    default: return ok();
  }
}

// 页面伪装(用户 N2):设置页的预览。样页是真面板按两个种子生成的首页(site/demo/camo.json,带图包照片的小图,链接、脚本已拿掉;按需读,只有设置页用到),
// 「换一个样子」在两个种子之间切换;入口按演示里的线路数(截图场景可以用 S.camoEntries 换成生产的量),
// 80 / 443 开着时给一个已放上、一个被线路占着(S.camoPorts 可换)
let CAMO = null, camoSeedIx = 0;
async function demoCamo(method, query) {
  if (method === 'POST') { camoSeedIx = 1 - camoSeedIx; return ok(); }
  if (!CAMO) CAMO = await (await fetch('js/camo.json')).json();
  const q = new URLSearchParams(query);
  const want = q.has('template') ? q.get('template') : (S.settings.camoTemplate || '');
  const on = k => String(S.settings[k] || '').toLowerCase() === 'true';
  const parse = v => { if (typeof v !== 'string') return v; try { return JSON.parse(v); } catch { return null; } };
  const ownMasq = l => { const o = parse(l.options) || {}; const m = o.masquerade; return !!(m && (typeof m === 'string' ? m.trim() : m.type)); };
  const hasTransport = l => { const v = parse(l.transport); return !!(v && v.type && v.type !== 'tcp'); };
  const lines = S.lines.filter(l => l.enabled !== false);
  const count = f => lines.filter(f).length;
  const entries = clone(S.camoEntries || {
    panel: Number(S.settings.webPort) || 2053, sub: Number(S.settings.subPort) || 2056,
    reseller: String(S.settings.resellerEnabled || '').toLowerCase() === 'false' ? 0 : Number(S.settings.resellerPort) || 2054,
    anytls: count(l => l.protocol === 'anytls'), trojan: count(l => l.protocol === 'trojan' && !hasTransport(l)),
    hy2: count(l => l.protocol === 'hysteria2' && !ownMasq(l)), hy2Own: count(l => l.protocol === 'hysteria2' && ownMasq(l)),
  });
  if (want === 'redirect') { // 跳转到管理员自己的网站:和后端一样只认 http(s) 网址
    const raw = (q.has('redirect') ? q.get('redirect') : S.settings.camoRedirect || '').trim();
    let target = '', error = '';
    try { const u = new URL(raw.includes('://') ? raw : 'https://' + raw); if (!/^https?:$/.test(u.protocol) || !u.hostname) throw 0; u.hash = ''; target = u.href; }
    catch { if (raw) error = '跳转的网址要以 http:// 或 https:// 开头,比如 https://www.example.com'; }
    return { mode: 'redirect', target, error, host: S.settings.subDomain || S.settings.webDomain || 'tokyo.example.com',
      ports: on('camoEnabled') && on('camoPorts') ? clone(S.camoPorts || [{ port: 80, on: true }, { port: 443, on: false, why: 'line' }]) : [],
      entries };
  }
  const tmpl = want || (camoSeedIx ? 'video' : 'mirror'); // 自动:每个种子各挑一种
  const page = (CAMO[tmpl] || CAMO.mirror)[camoSeedIx];
  return {
    mode: 'site', template: page.template, auto: !want, name: page.name, html: page.html,
    photos: page.photos, // 封面照片:样页是带着照片生成的(真面板上是这台服务器取下图包、裁切调色后的)
    host: S.settings.subDomain || S.settings.webDomain || 'tokyo.example.com',
    ports: on('camoEnabled') && on('camoPorts') ? clone(S.camoPorts || [{ port: 80, on: true }, { port: 443, on: false, why: 'line' }]) : [],
    entries,
  };
}

setTransport(async (rawPath, opts = {}) => {
  const method = (opts.method || 'GET').toUpperCase();
  const [path, query = ''] = rawPath.split('?');
  let body = null; if (opts.body) { try { body = JSON.parse(opts.body); } catch { body = null; } }
  await new Promise(r => setTimeout(r, 40)); // 像一次真实请求
  const res = path === 'camo' ? await demoCamo(method, query) : route(method, path, query, body);
  if (method !== 'GET' && !(body && body.dryRun) && !READONLY_POST.test(path)) demoNote();
  return res;
});
// 两步验证的二维码(id 为 'totp')演示里没有真的:拿一张现成的订阅二维码顶上,样子对就行
setQrUrl((id, fmt) => (id === 'reward' && RW_QR) || (data.qr || {})[`${id}:${fmt}`] || (id === 'totp' && Object.values(data.qr || {})[0]) || 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7');

// 写操作在本页内存里"成功"后提示一句:面板自己的提示(已保存、已续费……)后面接上"演示模式,刷新即恢复";
// 面板这次没弹提示就单独弹一条。只算真正改东西的请求:测速、预览、解析、生成密钥这类不算。
const READONLY_POST = /^(login|logout|upstreams\/(parse|test|\d+\/test)|exts\/\d+\/(preview|nodes\/(test|reach))|nodes\/\d+\/(test|port-check)|reality-check|route-apps|cert\/precheck|backup\/inspect|ops\/warp-check|keygen)/;
let noteWanted = 0, noteTimer = 0;
const noteText = () => ((localStorage.getItem('m-ui-lang') || navigator.language || 'en').startsWith('zh') ? '演示模式,刷新即恢复' : 'Demo mode: resets on refresh');
function demoNote() {
  noteWanted = Date.now();
  clearTimeout(noteTimer);
  noteTimer = setTimeout(() => { if (noteWanted) { noteWanted = 0; toast(noteText(), 'ok'); } }, 700);
}
const toastEl = document.getElementById('toast');
if (toastEl) new MutationObserver(() => {
  if (!noteWanted || toastEl.hidden || Date.now() - noteWanted > 3000) return;
  const note = noteText();
  noteWanted = 0; clearTimeout(noteTimer);
  if (!toastEl.textContent.includes(note)) toastEl.textContent += ' · ' + note;
}).observe(toastEl, { childList: true, characterData: true, subtree: true, attributes: true, attributeFilter: ['hidden', 'class'] });

// 演示提示条 + 把"支持"链接指到官网;登录框不会出现(status 直接返回),退出登录后随便填也能进
const zh = (localStorage.getItem('m-ui-lang') || navigator.language || 'en').startsWith('zh');
const bar = document.createElement('div');
bar.className = 'alert-bar info';
bar.id = 'demo-bar';
bar.innerHTML = zh
  ? `<span><b>演示模式</b> —— 改动只在本页内存里,刷新即恢复;没有真实服务器,订阅地址与二维码不可用。</span><a class="btn sm" href="${SITE}zh/getting-started/">五分钟装一个</a><a class="btn sm ghost" href="${GITHUB}" rel="noopener">GitHub</a>`
  : `<span><b>Demo mode</b> — changes are local and reset on refresh. No real server behind this: subscription links and QR codes are placeholders.</span><a class="btn sm" href="${SITE}getting-started/">Install in 5 minutes</a><a class="btn sm ghost" href="${GITHUB}" rel="noopener">GitHub</a>`;
const mount = () => { const main = document.querySelector('.main'); const page = document.getElementById('page'); if (main && page && !document.getElementById('demo-bar')) main.insertBefore(bar, page); };
for (const id of ['btn-support', 'login-support']) { const a = document.getElementById(id); if (a) a.href = GITHUB + '/discussions'; }
document.title = zh ? 'm-ui 在线演示 · sing-box 面板' : 'm-ui live demo · sing-box panel';

await import('./app.js');
mount();
new MutationObserver(mount).observe(document.getElementById('app'), { childList: true });
setTimeout(() => toast(zh ? '演示模式:可以随便点,刷新即恢复' : 'Demo mode: click anything, refresh to reset', 'ok'), 800);

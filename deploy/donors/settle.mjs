// 功德箱的"后台":GitHub Actions 定时跑(.github/workflows/donors.yml),结果写进本仓库 donors 分支,不需要任何服务器。
//   node deploy/donors/settle.mjs <donors 分支的工作目录>
//   node deploy/donors/settle.mjs --check <donors 分支的工作目录>   提交之后查有没有要人处理的(有就以 1 退出)
// 读三样公开数据:ntfy 上的留名、TronGrid 上的 USDT 到账、stats 这个 release 的下载计数。
// 留名后 60 分钟内有一笔金额完全相同的到账,就把金额累加到这个名字上。
//
// 不假设多久跑一次:GitHub 的定时任务只是尽力而为,写的是每 10 分钟,实际常常几个小时才跑一次(见过 15 小时)。
// 所以面板数按"恰好 24 小时前"两侧的点插值;待对账的留名留 48 小时,到账回看到最早那条待对账的留名;
// 三个数据源谁读不到都只影响自己那一项,不连累别的;哪一样超过 6 小时没读成功,提交之后的 --check 让这次运行失败。
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

export const ADDRESS = 'TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm';
export const USDT = 'TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t';
export const TOPIC = 'm-ui-gongde-c7e2d14f';
export const REPO = 'Maoyangui/m-ui';
const WINDOW = 3600; // 留名后多久内到账才算(面板 web/donate.go 的 pledgeWindow)
export const MAX_MICROS = 100_000_100_000; // 面板最多让人填 100000 USDT,再加 0.001–0.099 的零头(web/donate.go 的 pledgeMaxMicros)
export const PENDING_KEEP = 48 * 3600; // 待对账的留名留多久:两次运行隔十几个小时,中间到的账也能补上
const LOOKBACK = 12 * 3600; // 到账至少回看这么久(ntfy 公共服务也只缓存 12 小时的留名)
const DAY = 24 * 3600;
const POINTS_KEEP = 8 * DAY; // 下载计数的点留 8 天:停跑一两天,恢复后马上就能算
export const DEVICES_FRESH = 48 * 3600; // 面板数这么久没算出来就不公开(面板侧同一规则)
const TRON_PAGES = 25; // TronGrid 一页 200 条,最多翻这么多页
export const SOURCES = ['ntfy', 'tron', 'stats'];
export const SOURCE_STALE = 6 * 3600; // 数据源这么久没读成功就报警、让这次运行失败(ntfy 只缓存 12 小时的留名)
const SOURCE_BEAT = 3000; // 一直读得到时 okAt 隔这么久才往前挪一次(和下载计数的点一样),不让每次运行都多一个提交

// 和面板 web/donate.go 的 cleanDonorName 同一规则:名字显示在所有面板上,不能带链接。
export function validName(s) {
  if (typeof s !== 'string') return '';
  const name = s.trim().split(/\s+/).join(' ');
  if (!name || [...name].length > 24) return '';
  if (!/^[\p{L}\p{N}\p{M} ._\-·']+$/u.test(name)) return '';
  if (/[a-z0-9-]+\.[a-z]{2,}/i.test(name)) return '';
  return name;
}

// 以名字为键的表(donors)一律不带原型:名字可以是 constructor、toString、__proto__,在普通对象上
// donors['constructor'] 读到的是继承来的函数,写 donors['__proto__'] 会被原型的 setter 吞掉。读写都用 hasOwn;
// JSON 读回来的是普通对象,先用 dict 转一遍。交易号、留名 id 都放在 Set 里,按名字合计用 Map。
const hasOwn = Object.hasOwn;
const isTable = o => !!o && typeof o === 'object' && !Array.isArray(o);
export function dict(o) {
  const d = Object.create(null);
  if (isTable(o)) for (const [k, v] of Object.entries(o)) d[k] = v;
  return d;
}

export const toMicros = s => {
  const m = /^(\d{1,9})(?:\.(\d{1,6}))?$/.exec(String(s));
  return m ? Number(m[1]) * 1e6 + Number((m[2] || '').padEnd(6, '0')) : 0;
};

// 留名消息 → 待对账的一条。消息的时间用 ntfy 服务器记的,不信消息体。
export function parsePledge(msg) {
  if (!msg || msg.event !== 'message' || !msg.id || !msg.time) return null;
  let body;
  try { body = JSON.parse(msg.message); } catch { return null; }
  const name = validName(body && body.n), micros = toMicros(body && body.a);
  if (!name || micros < 1e6 || micros > MAX_MICROS) return null;
  return { id: msg.id, name, micros, at: msg.time };
}

// 对账:纯函数,不碰网络。state = { pending, seenTx, donors:{名字:微 USDT}, blocked:[] }。
// transfers 为 null 表示这次读不到到账:新留名照样收进待对账,但不对账,待对账也先不按 48 小时清(它们还没核对过)。
export function settle(state, pledges, transfers, now) {
  const s = { pending: [...(state.pending || [])], seenTx: [...(state.seenTx || [])], donors: dict(state.donors) };
  const blocked = new Set((state.blocked || []).map(x => x.toLowerCase()));
  const known = new Set(s.pending.map(p => p.id).concat(state.donePledges || []));
  for (const p of pledges) {
    if (p && !known.has(p.id) && !blocked.has(p.name.toLowerCase())) { s.pending.push(p); known.add(p.id); }
  }
  s.pending.sort((a, b) => a.at - b.at);
  const done = [...(state.donePledges || [])];
  let changed = false;
  if (transfers) {
    const seen = new Set(s.seenTx);
    for (const t of [...transfers].sort((a, b) => a.block_timestamp - b.block_timestamp)) {
      if (seen.has(t.transaction_id)) continue;
      if (t.to !== ADDRESS || t.type !== 'Transfer' || (t.token_info && t.token_info.address !== USDT)) continue;
      const at = Math.floor(t.block_timestamp / 1000), value = Number(t.value);
      const i = s.pending.findIndex(p => p.micros === value && p.at <= at && at <= p.at + WINDOW);
      if (i < 0) continue; // 没留名的转账不上榜;也不记下来,说不定留名消息还在路上
      const [p] = s.pending.splice(i, 1);
      s.donors[p.name] = (hasOwn(s.donors, p.name) ? s.donors[p.name] : 0) + value;
      s.seenTx.push(t.transaction_id);
      seen.add(t.transaction_id);
      done.push(p.id);
      changed = true;
    }
    s.pending = s.pending.filter(p => p.at + PENDING_KEEP > now);
  } else {
    // 没核对过的先留着,只清掉以后也对不上的:到账最多回看 PENDING_KEEP + WINDOW,再早的留名等不到它的到账了
    s.pending = s.pending.filter(p => p.at + PENDING_KEEP + 2 * WINDOW > now);
  }
  s.pending = s.pending.slice(-2000); // ntfy 的主题是公开的,有人刷留名时 state.json 也不会无限变大
  s.seenTx = s.seenTx.slice(-2000);
  s.donePledges = done.slice(-2000);
  s.blocked = state.blocked || [];
  return { state: s, changed };
}

// 运行中的面板数:下载计数在过去 24 小时涨了多少。"恰好 24 小时前"的计数用它两侧的点线性插值:
// 两次运行隔几个小时也能算,也不会把 25 小时的增量当成一天。算不出来(还没有 24 小时前的点)返回 null,
// 0 就是真的 0。计数变小说明 ping 文件被删了重传:旧点作废(reset),满 24 小时后重新出数。
export function deviceCount(points, count, now) {
  let pts = (Array.isArray(points) ? points : []).filter(p => p && Number.isFinite(p.t) && Number.isFinite(p.n)).sort((a, b) => a.t - b.t);
  if (!Number.isSafeInteger(count) || count < 0) return { points: pts, devices: null, reset: false };
  const reset = pts.length > 0 && count < pts[pts.length - 1].n;
  if (reset) pts = [];
  if (!pts.length || now - pts[pts.length - 1].t >= 3000) pts.push({ t: now, n: count }); // 50 分钟内不重复记点
  pts = pts.filter(p => p.t >= now - POINTS_KEEP);
  const target = now - DAY;
  let before = null, after = null;
  for (const p of pts) {
    if (p.t <= target) before = p;
    else if (!after) after = p;
  }
  if (!before) return { points: pts, devices: null, reset };
  const base = after ? before.n + (after.n - before.n) * (target - before.t) / (after.t - before.t) : before.n;
  return { points: pts, devices: Math.max(0, Math.round(count - base)), reset };
}

// state.donors 的值必须是微 USDT 整数(USDT × 1000000)。手动记账写成 USDT、小数或字符串时直接报错让这次运行失败,
// 不悄悄显示成 0.001 或拼接出来的怪数。
export function checkDonors(donors) {
  if (donors === undefined) return;
  if (!donors || typeof donors !== 'object' || Array.isArray(donors)) throw new Error('state.json 的 donors 要写成 {"名字": 微 USDT 整数}');
  for (const [name, m] of Object.entries(donors)) {
    if (!Number.isSafeInteger(m) || m < 1e6) throw new Error(`state.json 的 donors[${JSON.stringify(name)}] = ${JSON.stringify(m)}:要写微 USDT 整数(USDT × 1000000,至少 1000000)`);
  }
}

// 公开的榜单。名字和面板用同一规则:面板不显示的名字这里也不放,文档站和面板看到的一样。
export function publicBoard(state, { updatedAt, devices, devicesAt }, log = () => {}) {
  checkDonors(state.donors);
  const totals = new Map();
  for (const [raw, m] of Object.entries(state.donors || {})) {
    const name = validName(raw);
    if (!name) { log(`名字不合规,不上榜: ${JSON.stringify(raw)}`); continue; }
    totals.set(name, (totals.get(name) || 0) + m);
  }
  const donors = [...totals].map(([name, m]) => ({ name, total: Math.round(m / 1000) / 1000 })).sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
  return { updatedAt, devices, devicesAt, donors };
}

// 收款地址 since(unix 秒)以来收到的 USDT-TRC20。TronGrid 一页最多 200 条、新的在前,按 meta.fingerprint 往下翻:
// 被人刷大量零额转账时也不会把真正的到账挤出去。任何一页失败都算这次读不到。
export async function tronTransfers(getJSON, since, warn = () => {}) {
  const base = `https://api.trongrid.io/v1/accounts/${ADDRESS}/transactions/trc20?only_to=true&limit=200&contract_address=${USDT}&min_timestamp=${since * 1000}`;
  const all = [];
  for (let page = 1, url = base; ; page++) {
    const r = await getJSON(url);
    if (!r || r.success === false || !Array.isArray(r.data)) throw new Error('TronGrid 返回的不是到账列表');
    all.push(...r.data);
    const fp = r.meta && r.meta.fingerprint;
    if (!fp || !r.data.length || r.data.some(t => t.block_timestamp < since * 1000)) return all;
    if (page >= TRON_PAGES) { warn(`TronGrid 翻了 ${TRON_PAGES} 页还没到头,更早的到账这次没看`); return all; }
    url = base + '&fingerprint=' + encodeURIComponent(fp);
  }
}

// 各数据源最近一次读成功的时间 okAt,和读不到时的错误 err(从 errAt 起一直失败),记进 state.sources。
// 提交之后的检查(--check)看它:哪一样超过 6 小时没读成功,这次运行就标成失败。一直读得到时 okAt 50 分钟才往前挪
// 一次,报警因此最多早 50 分钟;错误去掉网址里的查询参数(TronGrid 的每次都不一样),失败期间也不是每次运行都多一个提交。
export function trackSources(prev, errs, now) {
  const out = {};
  for (const k of SOURCES) {
    const p = isTable(prev) && isTable(prev[k]) ? prev[k] : {};
    const okAt = Number.isSafeInteger(p.okAt) && p.okAt > 0 && p.okAt <= now ? p.okAt : 0; // 比现在还晚的是手改坏了,不认
    if (!errs[k]) {
      out[k] = { okAt: !okAt || p.err || now - okAt >= SOURCE_BEAT ? now : okAt };
      continue;
    }
    const err = String(errs[k]).replace(/\?\S*/g, '').replace(/\s+/g, ' ').trim().slice(0, 300) || '读不到';
    const errAt = p.err && Number.isSafeInteger(p.errAt) && p.errAt <= now ? p.errAt : now;
    out[k] = okAt ? { okAt, err, errAt } : { err, errAt };
  }
  return out;
}

// 还在失败、距上次读成功(没成功过就从开始失败算)超过 SOURCE_STALE 的数据源。
// 最近一次读成功了的不算:多久没运行由 donors.yml 的第一步管。
export function staleSources(sources, now) {
  const out = [];
  for (const k of SOURCES) {
    const x = isTable(sources) && isTable(sources[k]) ? sources[k] : null;
    if (!x || !x.err) continue;
    const okAt = Number.isSafeInteger(x.okAt) && x.okAt > 0 ? x.okAt : 0, since = okAt || x.errAt;
    if (Number.isSafeInteger(since) && now - since > SOURCE_STALE) out.push({ source: k, since, okAt, err: String(x.err) });
  }
  return out;
}

const SOURCE_INFO = {
  ntfy: ['ntfy(留名)', 'ntfy 只缓存 12 小时的留名,读不到超过 12 小时就开始丢留名'],
  tron: ['TronGrid(到账)', '这期间的到账对不上;待对账的留名最多留 48 小时,过了就再也对不上'],
  stats: ['stats 的下载计数(在线设备数)', '面板数不再更新,超过 48 小时就不再显示'],
};
const utc = t => new Date(t * 1000).toISOString().slice(0, 16).replace('T', ' ') + ' UTC';

// 要人处理的问题。run 写进日志;提交之后的检查(--check,donors.yml 最后一步)写进这次运行的摘要并让运行失败。
export function problems(state, now) {
  return staleSources(state.sources, now).map(x => {
    const [what, risk] = SOURCE_INFO[x.source];
    const last = x.okAt ? `上次成功是 ${utc(x.okAt)}` : `从 ${utc(x.since)} 起一直没成功过`;
    return `${what}已经 ${Math.floor((now - x.since) / 3600)} 小时没读成功(${last};最近的错误:${x.err}):${risk}`;
  });
}

// GitHub Actions 的注解(::warning:: / ::error::),本地跑时原样输出。
const annotate = (level, msg) => process.env.GITHUB_ACTIONS === 'true' ? `::${level}::` + String(msg).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A') : String(msg);
const defaultWarn = msg => console.log(annotate('warning', msg));
const why = e => (e && e.message) || String(e) || '读不到';

// 读 state.json。以名字为键的 donors 转成不带原型的表:读回来的是普通对象,名字是 __proto__ 之类的也要当普通的键。
function readState(file) {
  const state = fs.existsSync(file) ? JSON.parse(fs.readFileSync(file, 'utf8')) : {};
  if (isTable(state.donors)) state.donors = dict(state.donors);
  return state;
}

// 跑一轮:读 dir 下的 state.json(全部状态)和 donors.json(公开榜单),算完写回去。fetch 可以换成假的(测试里不联网)。
export async function run({ dir, fetch = globalThis.fetch, now = Math.floor(Date.now() / 1000), token = process.env.GITHUB_TOKEN, log = console.log, warn = defaultWarn } = {}) {
  if (!dir) throw new Error('用法: node deploy/donors/settle.mjs <donors 分支的工作目录>');
  const statePath = path.join(dir, 'state.json'), boardPath = path.join(dir, 'donors.json');
  const state = readState(statePath);
  checkDonors(state.donors); // 手动记账写错了先报出来,不带着错数据往下算
  const get = async (url, headers = {}) => {
    const r = await fetch(url, { headers, signal: AbortSignal.timeout(30000) });
    if (!r.ok) throw new Error(url + ' → ' + r.status);
    return r;
  };
  const getJSON = async (url, headers) => (await get(url, headers)).json();
  const errs = {}; // 这次读不到的数据源 → 错误

  // 留名:ntfy 公共服务只缓存 12 小时。读不到就当这次没有新留名,已经收进来的照常对账
  let pledges = [];
  try {
    const text = await (await get(`https://ntfy.sh/${TOPIC}/json?poll=1&since=12h`)).text();
    pledges = text.split('\n').filter(Boolean).map(l => { try { return parsePledge(JSON.parse(l)); } catch { return null; } }).filter(Boolean);
  } catch (e) { errs.ntfy = why(e); warn('ntfy 读不到,这次不收新留名: ' + why(e)); }

  // 到账:至少回看 12 小时,有更早的待对账留名就回看到它(最多 48 小时)。读不到就不对账,待对账的留名先留着
  const ats = [...(state.pending || []), ...pledges].map(p => p && p.at).filter(Number.isFinite);
  const since = Math.max(now - PENDING_KEEP - WINDOW, Math.min(now - LOOKBACK, ...ats));
  let transfers = null;
  try { transfers = await tronTransfers(getJSON, since, warn); } catch (e) { errs.tron = why(e); warn('TronGrid 读不到,这次不对账,待对账的留名先留着: ' + why(e)); }
  const { state: settled, changed } = settle(state, pledges, transfers, now);

  // 运行中的面板数:读不到就沿用上次的,统计时间 devicesAt 不变,面板按它判断新旧
  let pings = state.pings || [];
  let devices = Number.isSafeInteger(state.devices) ? state.devices : 0, devicesAt = Number.isSafeInteger(state.devicesAt) ? state.devicesAt : 0;
  try {
    const rel = await getJSON(`https://api.github.com/repos/${REPO}/releases/tags/stats`, token ? { Authorization: 'Bearer ' + token } : {});
    const asset = (rel.assets || []).find(a => a.name === 'ping');
    if (!asset) throw new Error('stats 里没有 ping 文件');
    if (!Number.isSafeInteger(asset.download_count) || asset.download_count < 0) throw new Error('ping 文件的下载计数不对: ' + JSON.stringify(asset.download_count));
    const prev = pings.length ? pings[pings.length - 1].n : null;
    const d = deviceCount(pings, asset.download_count, now);
    pings = d.points;
    if (d.reset) warn(`下载计数变小了(${prev} → ${asset.download_count}),ping 文件可能被重传过:旧点作废,满 24 小时后重新出数`);
    if (d.devices !== null) { devices = d.devices; devicesAt = now; }
  } catch (e) { errs.stats = why(e); warn('下载计数读不到,面板数沿用上次的: ' + why(e)); }

  const sources = trackSources(state.sources, errs, now);
  const next = { ...state, ...settled, pings, devices, devicesAt, sources }; // 手动加的其它顶层字段(比如记账来源)原样留着
  let old = {};
  try { old = JSON.parse(fs.readFileSync(boardPath, 'utf8')) || {}; } catch { old = {}; }
  const shown = devicesAt > 0 && now - devicesAt <= DEVICES_FRESH ? devices : 0; // 太久没算出来就不公开,老版本面板也就不显示了
  const board = publicBoard(next, { updatedAt: old.updatedAt || now, devices: shown, devicesAt }, warn); // 不合规的名字只可能是手动记账写的,报警让人看见
  if (changed || shown !== old.devices || JSON.stringify(board.donors) !== JSON.stringify(old.donors)) board.updatedAt = now;
  fs.writeFileSync(statePath, JSON.stringify(next, null, 1) + '\n');
  fs.writeFileSync(boardPath, JSON.stringify(board, null, 1) + '\n');
  log(`留名 ${pledges.length} 条,待对账 ${next.pending.length} 条,上榜 ${board.donors.length} 人,运行中 ${shown} 台${changed ? ',有新到账' : ''}`);
  const todo = problems(next, now);
  for (const m of todo) log('要处理: ' + m); // 这里只记日志,照常以 0 退出、提交;让运行失败的是提交之后的 --check
  return { state: next, board, changed, ok: Object.fromEntries(SOURCES.map(k => [k, !errs[k]])), problems: todo };
}

// 提交之后的检查(donors.yml 最后一步,不联网):把要人处理的问题写进这次运行的摘要、打成错误注解,返回问题列表;
// 命令行有问题就以 1 退出,运行标成失败,GitHub 会发通知。只写警告的话没人会去翻运行记录,一直读不到的数据源
// (接口改了、被限流、ping 文件没了)会让留名和到账无声地丢。算出来的状态在这之前已经提交了。
export function check({ dir, now = Math.floor(Date.now() / 1000), summary = process.env.GITHUB_STEP_SUMMARY, log = console.log } = {}) {
  if (!dir) throw new Error('用法: node deploy/donors/settle.mjs --check <donors 分支的工作目录>');
  const todo = problems(readState(path.join(dir, 'state.json')), now);
  for (const m of todo) log(annotate('error', m));
  if (summary && todo.length) fs.appendFileSync(summary, '### 功德箱要处理\n\n' + todo.map(m => '- ' + m).join('\n') + '\n');
  return todo;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [a, b] = process.argv.slice(2);
  const main = a === '--check' ? async () => { if (check({ dir: b }).length) process.exitCode = 1; } : () => run({ dir: a });
  main().catch(e => { console.error(e); process.exit(1); });
}

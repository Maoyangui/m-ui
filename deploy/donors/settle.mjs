// 功德箱的"后台":GitHub Actions 定时跑(.github/workflows/donors.yml),结果写进本仓库 donors 分支,不需要任何服务器。
//   node deploy/donors/settle.mjs <donors 分支的工作目录>
// 读三样公开数据:ntfy 上的留名、TronGrid 上的 USDT 到账、stats 这个 release 的下载计数。
// 留名后 60 分钟内有一笔金额完全相同的到账,就把金额累加到这个名字上。
//
// 不假设多久跑一次:GitHub 的定时任务只是尽力而为,写的是每 10 分钟,实际常常几个小时才跑一次(见过 15 小时)。
// 所以面板数按"恰好 24 小时前"两侧的点插值;待对账的留名留 48 小时,到账回看到最早那条待对账的留名;
// 三个数据源谁读不到都只影响自己那一项,不连累别的。
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

// 和面板 web/donate.go 的 cleanDonorName 同一规则:名字显示在所有面板上,不能带链接。
export function validName(s) {
  if (typeof s !== 'string') return '';
  const name = s.trim().split(/\s+/).join(' ');
  if (!name || [...name].length > 24) return '';
  if (!/^[\p{L}\p{N}\p{M} ._\-·']+$/u.test(name)) return '';
  if (/[a-z0-9-]+\.[a-z]{2,}/i.test(name)) return '';
  return name;
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
  const s = { pending: [...(state.pending || [])], seenTx: [...(state.seenTx || [])], donors: { ...(state.donors || {}) } };
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
      s.donors[p.name] = (s.donors[p.name] || 0) + value;
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

const defaultWarn = msg => console.log((process.env.GITHUB_ACTIONS === 'true' ? '::warning::' : '') + msg);

// 跑一轮:读 dir 下的 state.json(全部状态)和 donors.json(公开榜单),算完写回去。fetch 可以换成假的(测试里不联网)。
export async function run({ dir, fetch = globalThis.fetch, now = Math.floor(Date.now() / 1000), token = process.env.GITHUB_TOKEN, log = console.log, warn = defaultWarn } = {}) {
  if (!dir) throw new Error('用法: node deploy/donors/settle.mjs <donors 分支的工作目录>');
  const statePath = path.join(dir, 'state.json'), boardPath = path.join(dir, 'donors.json');
  const state = fs.existsSync(statePath) ? JSON.parse(fs.readFileSync(statePath, 'utf8')) : {};
  checkDonors(state.donors); // 手动记账写错了先报出来,不带着错数据往下算
  const get = async (url, headers = {}) => {
    const r = await fetch(url, { headers, signal: AbortSignal.timeout(30000) });
    if (!r.ok) throw new Error(url + ' → ' + r.status);
    return r;
  };
  const getJSON = async (url, headers) => (await get(url, headers)).json();
  const ok = { ntfy: true, tron: true, stats: true };

  // 留名:ntfy 公共服务只缓存 12 小时。读不到就当这次没有新留名,已经收进来的照常对账
  let pledges = [];
  try {
    const text = await (await get(`https://ntfy.sh/${TOPIC}/json?poll=1&since=12h`)).text();
    pledges = text.split('\n').filter(Boolean).map(l => { try { return parsePledge(JSON.parse(l)); } catch { return null; } }).filter(Boolean);
  } catch (e) { ok.ntfy = false; warn('ntfy 读不到,这次不收新留名: ' + e.message); }

  // 到账:至少回看 12 小时,有更早的待对账留名就回看到它(最多 48 小时)。读不到就不对账,待对账的留名先留着
  const ats = [...(state.pending || []), ...pledges].map(p => p && p.at).filter(Number.isFinite);
  const since = Math.max(now - PENDING_KEEP - WINDOW, Math.min(now - LOOKBACK, ...ats));
  let transfers = null;
  try { transfers = await tronTransfers(getJSON, since, warn); } catch (e) { ok.tron = false; warn('TronGrid 读不到,这次不对账,待对账的留名先留着: ' + e.message); }
  const { state: settled, changed } = settle(state, pledges, transfers, now);

  // 运行中的面板数:读不到就沿用上次的,统计时间 devicesAt 不变,面板按它判断新旧
  let pings = state.pings || [];
  let devices = Number.isSafeInteger(state.devices) ? state.devices : 0, devicesAt = Number.isSafeInteger(state.devicesAt) ? state.devicesAt : 0;
  try {
    const rel = await getJSON(`https://api.github.com/repos/${REPO}/releases/tags/stats`, token ? { Authorization: 'Bearer ' + token } : {});
    const asset = (rel.assets || []).find(a => a.name === 'ping');
    if (!asset) throw new Error('stats 里没有 ping 文件');
    const prev = pings.length ? pings[pings.length - 1].n : null;
    const d = deviceCount(pings, asset.download_count, now);
    pings = d.points;
    if (d.reset) warn(`下载计数变小了(${prev} → ${asset.download_count}),ping 文件可能被重传过:旧点作废,满 24 小时后重新出数`);
    if (d.devices !== null) { devices = d.devices; devicesAt = now; }
  } catch (e) { ok.stats = false; warn('下载计数读不到,面板数沿用上次的: ' + e.message); }

  const next = { ...state, ...settled, pings, devices, devicesAt }; // 手动加的其它顶层字段(比如记账来源)原样留着
  let old = {};
  try { old = JSON.parse(fs.readFileSync(boardPath, 'utf8')) || {}; } catch { old = {}; }
  const shown = devicesAt > 0 && now - devicesAt <= DEVICES_FRESH ? devices : 0; // 太久没算出来就不公开,老版本面板也就不显示了
  const board = publicBoard(next, { updatedAt: old.updatedAt || now, devices: shown, devicesAt }, warn); // 不合规的名字只可能是手动记账写的,报警让人看见
  if (changed || shown !== old.devices || JSON.stringify(board.donors) !== JSON.stringify(old.donors)) board.updatedAt = now;
  fs.writeFileSync(statePath, JSON.stringify(next, null, 1) + '\n');
  fs.writeFileSync(boardPath, JSON.stringify(board, null, 1) + '\n');
  log(`留名 ${pledges.length} 条,待对账 ${next.pending.length} 条,上榜 ${board.donors.length} 人,运行中 ${shown} 台${changed ? ',有新到账' : ''}`);
  return { state: next, board, changed, ok };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) run({ dir: process.argv[2] }).catch(e => { console.error(e); process.exit(1); });

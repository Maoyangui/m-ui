// 功德箱的"后台":GitHub Actions 定时跑(.github/workflows/donors.yml),结果写进本仓库 donors 分支,不需要任何服务器。
//   node deploy/donors/settle.mjs <donors 分支的工作目录>
// 读三样公开数据:ntfy 上的留名、TronGrid 上的 USDT 到账、stats 这个 release 的下载计数。
// 留名后 60 分钟内有一笔金额完全相同的到账,就把金额累加到这个名字上。
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

export const ADDRESS = 'TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm';
export const USDT = 'TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t';
export const TOPIC = 'm-ui-gongde-c7e2d14f';
export const REPO = 'Maoyangui/m-ui';
const WINDOW = 3600;

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
  if (!name || micros < 1e6 || micros > 1e11) return null;
  return { id: msg.id, name, micros, at: msg.time };
}

// 对账:纯函数,不碰网络。state = { pending, seenTx, donors:{名字:微 USDT}, blocked:[] }。
export function settle(state, pledges, transfers, now) {
  const s = { pending: [...(state.pending || [])], seenTx: [...(state.seenTx || [])], donors: { ...(state.donors || {}) } };
  const blocked = new Set((state.blocked || []).map(x => x.toLowerCase()));
  const known = new Set(s.pending.map(p => p.id).concat(state.donePledges || []));
  for (const p of pledges) {
    if (p && !known.has(p.id) && !blocked.has(p.name.toLowerCase())) { s.pending.push(p); known.add(p.id); }
  }
  s.pending.sort((a, b) => a.at - b.at);
  const seen = new Set(s.seenTx);
  const done = [...(state.donePledges || [])];
  let changed = false;
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
  s.pending = s.pending.filter(p => p.at + WINDOW + 3600 > now); // 窗口过了再留一小时(转账可能晚些才查到)
  s.seenTx = s.seenTx.slice(-2000);
  s.donePledges = done.slice(-2000);
  s.blocked = state.blocked || [];
  return { state: s, changed };
}

// 运行中的面板数:下载计数 24 小时涨了多少。不到 24 小时的数据先不给。
export function deviceCount(points, count, now) {
  const pts = [...(points || [])];
  const last = pts[pts.length - 1];
  if (!last || now - last.t >= 3000) pts.push({ t: now, n: count });
  const kept = pts.filter(p => p.t >= now - 26 * 3600);
  const base = kept.filter(p => p.t <= now - 24 * 3600).pop();
  return { points: kept, devices: base ? Math.max(0, count - base.n) : 0 };
}

export function publicBoard(state, devices, updatedAt) {
  const donors = Object.entries(state.donors).map(([name, m]) => ({ name, total: Math.round(m / 1000) / 1000 })).sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
  return { updatedAt, devices, donors };
}

async function getJSON(url, headers = {}) {
  const r = await fetch(url, { headers, signal: AbortSignal.timeout(30000) });
  if (!r.ok) throw new Error(url + ' → ' + r.status);
  return r.json();
}

async function main() {
  const dir = process.argv[2];
  const statePath = path.join(dir, 'state.json'), boardPath = path.join(dir, 'donors.json');
  const state = fs.existsSync(statePath) ? JSON.parse(fs.readFileSync(statePath, 'utf8')) : {};
  const now = Math.floor(Date.now() / 1000);

  const res = await fetch(`https://ntfy.sh/${TOPIC}/json?poll=1&since=12h`, { signal: AbortSignal.timeout(30000) });
  if (!res.ok) throw new Error('ntfy → ' + res.status);
  const pledges = (await res.text()).split('\n').filter(Boolean).map(l => { try { return parsePledge(JSON.parse(l)); } catch { return null; } }).filter(Boolean);

  const since = (now - 12 * 3600) * 1000;
  const tron = await getJSON(`https://api.trongrid.io/v1/accounts/${ADDRESS}/transactions/trc20?only_to=true&limit=200&contract_address=${USDT}&min_timestamp=${since}`);
  const { state: next, changed } = settle(state, pledges, tron.data || [], now);

  // 下载计数读不到不影响对账
  let pings = state.pings || [], devices = state.devices || 0;
  try {
    const headers = process.env.GITHUB_TOKEN ? { Authorization: 'Bearer ' + process.env.GITHUB_TOKEN } : {};
    const rel = await getJSON(`https://api.github.com/repos/${REPO}/releases/tags/stats`, headers);
    const asset = (rel.assets || []).find(a => a.name === 'ping');
    if (asset) {
      const d = deviceCount(pings, asset.download_count, now);
      pings = d.points;
      if (d.devices > 0 || !devices) devices = d.devices;
    }
  } catch (e) { console.log('下载计数读不到:', e.message); }

  next.pings = pings;
  next.devices = devices;
  const old = fs.existsSync(boardPath) ? JSON.parse(fs.readFileSync(boardPath, 'utf8')) : {};
  const board = publicBoard(next, devices, old.updatedAt || now);
  if (changed || devices !== old.devices || !old.donors) board.updatedAt = now;
  fs.writeFileSync(statePath, JSON.stringify(next, null, 1) + '\n');
  fs.writeFileSync(boardPath, JSON.stringify(board, null, 1) + '\n');
  console.log(`留名 ${pledges.length} 条,待对账 ${next.pending.length} 条,上榜 ${board.donors.length} 人,运行中 ${devices} 台${changed ? ',有新到账' : ''}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main().catch(e => { console.error(e); process.exit(1); });

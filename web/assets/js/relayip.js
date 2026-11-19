// 在线 IP 是集群里某台服务器自己的 IP:用户是经它的中转入口连进来的(出口服务器只看得到中转的 IP)。
// 用户详情的在线设备、线路详情的在线 IP 据此标「⇄ 服务器名」。
import { state } from './app.js';

// 中转入口的来源地址(和后端 runner.relaySources 同一套,改那边要一起改):每条启用线路上没暂停的中转入口 ——
// 手填的来源 IP、别处的中转填的地址、集群里当中转的那台服务器的公网 IPv4 / IPv6 和连接地址。
// 这些是中转机,不是用户的设备:不占设备名额(用户 11-20 定),数「在线设备」时不算。
export function relayIPs() {
  const set = new Set(), nodes = state.nodes || [];
  const add = s => { const a = String(s || '').trim().replace(/^\[|\]$/g, ''); if (a && /^[0-9a-f:.]+$/i.test(a)) set.add(a.toLowerCase()); };
  for (const l of state.lines || []) {
    if (l.enabled === false) continue;
    let fs = l.forwards;
    if (typeof fs === 'string') { try { fs = JSON.parse(fs); } catch { fs = null; } }
    for (const f of Array.isArray(fs) ? fs : []) {
      if (f.off) continue;
      String(f.src || '').split(',').forEach(add);
      if (!f.via) { add(f.host); continue; }
      const n = nodes.find(x => x.id === f.via);
      if (n) { add(n.publicIp); add(n.publicIp6); add(n.addr); }
    }
  }
  return set;
}
// 在线 IP 里去掉中转机的地址(数设备用);relay 不传就现算
export const deviceIPs = (ips, relay = relayIPs()) => (ips || []).filter(ip => !relay.has(String(ip).toLowerCase()));

export function serverOfIP(ip) {
  if (!ip) return '';
  for (const n of state.nodes || []) {
    if (n.publicIp === ip || n.publicIp6 === ip || n.addr === ip) return n.name || '';
  }
  return '';
}

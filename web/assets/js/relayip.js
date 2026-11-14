// 在线 IP 是集群里某台服务器自己的 IP:用户是经它的中转入口连进来的(出口服务器只看得到中转的 IP)。
// 用户详情的在线设备、线路详情的在线 IP 据此标「⇄ 服务器名」。
import { state } from './app.js';

export function serverOfIP(ip) {
  if (!ip) return '';
  for (const n of state.nodes || []) {
    if (n.publicIp === ip || n.publicIp6 === ip || n.addr === ip) return n.name || '';
  }
  return '';
}

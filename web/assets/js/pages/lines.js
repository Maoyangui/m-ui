import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, toast, confirm, openModal, openDrawer, drawerOpen, registerActions, badge, dot, field, check, empty, fv, fchk, matches, debounce, setHTML, setText, chipList, fieldBox, fmtBytes, tzOffsetMinutes } from '../ui.js';
import { barChart, bucketFor } from '../chart.js';
import { serverOfIP } from '../relayip.js';
import { openCertDrawer, certLabel } from '../certpicker.js';
import { editRelayUpstream } from './upstreams.js';
import { loadReach, reachLoaded, v6Of, v6LineBadge, v6ForLine } from '../reach.js';

const selected = new Set(); // 批量设置勾选的线路 id
export const title = () => t('line.title');
export const subtitle = () => t('line.subtitle');
let query = '';
let nodeFilter = 0, protoFilter = ''; // 快捷筛选:部署在某台服务器上(部署到全部服务器的也算)、某种协议

// 与后端 render.Protocols 对应:TLS 是否必需、默认模式、是否支持传输层
const PROTOCOLS = {
  vless: { tlsRequired: false, tlsDefault: 'reality', transport: true },
  vmess: { tlsRequired: false, tlsDefault: 'none', transport: true },
  trojan: { tlsRequired: false, tlsDefault: 'cert', transport: true },
  hysteria2: { tlsRequired: true, tlsDefault: 'cert', transport: false },
  tuic: { tlsRequired: true, tlsDefault: 'cert', transport: false },
  anytls: { tlsRequired: true, tlsDefault: 'cert', transport: false },
  shadowsocks: { tlsRequired: false, tlsDefault: 'none', transport: false, noTls: true },
  socks: { tlsRequired: false, tlsDefault: 'none', transport: false, noTls: true },
  http: { tlsRequired: false, tlsDefault: 'cert', transport: false },
  mixed: { tlsRequired: false, tlsDefault: 'none', transport: false, noTls: true },
};
const SS_METHODS = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305', '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305', 'none'];
const FPS = ['chrome', 'firefox', 'safari', 'ios', 'android', 'edge', 'random'];
// 表单接管的 Options 键,其余进"高级参数"
const OPT_KEYS = {
  hysteria2: ['up_mbps', 'down_mbps', 'obfs', 'port_hopping', 'masquerade'], tuic: ['congestion_control'], shadowsocks: ['method', 'password'],
  anytls: ['padding_scheme'], vless: ['vision'],
};
const parseJ = o => { try { return typeof o === 'string' ? JSON.parse(o) : (o || {}); } catch { return {}; } };

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="line-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      ${(state.nodes || []).length > 1 ? `<select class="sm" id="line-node" style="width:auto;max-width:14rem" aria-label="${esc(t('nav.nodes'))}"><option value="0">${esc(t('line.allNodes'))}</option>${state.nodes.map(n => `<option value="${n.id}" ${n.id === nodeFilter ? 'selected' : ''}>${esc(n.name)}</option>`).join('')}</select>` : ''}
      <span id="line-protos"></span>
      <span class="muted small" id="line-count"></span>
      <span class="grow"></span>
      <details class="menu"><summary class="btn">${t('line.quick')} ▾</summary><div class="menu-list">
        ${PRESETS.map(p => `<button data-act="line.preset" data-id="${p.id}"><b>${esc(p.label)}</b><span class="muted small" style="display:block">${esc(p.hint)}</span></button>`).join('')}
      </div></details>
      <button class="btn primary" data-act="line.add">${t('line.add')}</button>
    </div>
    <div class="toolbar batch-bar" id="line-batch-bar" hidden>
      <span class="badge primary" id="line-batch-count"></span>
      <button class="btn sm" data-act="line.batch" data-id="enable">${t('line.batch.enable')}</button>
      <button class="btn sm" data-act="line.batch" data-id="disable">${t('line.batch.disable')}</button>
      <button class="btn sm" data-act="line.batch" data-id="upstream">${t('line.batch.upstream')}</button>
      ${(state.nodes || []).length > 1 ? `<button class="btn sm" data-act="line.batch" data-id="nodes">${t('line.batch.nodes')}</button>` : ''}
      <button class="btn sm ghost" data-act="line.clearSel">${t('line.batch.clear')}</button>
    </div>
    <div class="table-wrap fill"><table class="grid">
      <thead><tr><th></th><th style="width:1.5rem"><input type="checkbox" id="line-sel-all"></th><th>${t('common.name')}</th><th>${t('line.protocol')}</th><th>${t('common.port')}</th><th>${t('line.upstream')}</th><th>${t('nav.nodes')}</th><th class="c-p3">${t('line.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="lines-body"></tbody>
    </table></div>`;
  document.getElementById('line-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  const ns = document.getElementById('line-node');
  if (ns) ns.addEventListener('change', e => { nodeFilter = Number(e.target.value) || 0; renderRows(); });
  document.getElementById('line-sel-all').addEventListener('change', e => {
    visibleRows().forEach(l => e.target.checked ? selected.add(l.id) : selected.delete(l.id));
    renderRows();
  });
  renderRows();
  loadReach().then(renderRows); // 走 IPv6 的线路标上服务器页那份 IPv6 入站检测结果;拿不到不影响列表
}
const lineNodeIds = l => { const v = l.nodeIds; if (!v) return []; try { return Array.isArray(v) ? v : JSON.parse(v); } catch { return []; } };
function visibleRows() {
  return state.lines.filter(l => matches(query, l.name, l.protocol, l.port, l.upstreamName)
    && (!nodeFilter || !lineNodeIds(l).length || lineNodeIds(l).includes(nodeFilter))
    && (!protoFilter || l.protocol === protoFilter));
}
// 协议筛选只列实际用到的协议(两种以上才出现)
function renderProtoSeg() {
  const used = [...new Set(state.lines.map(l => l.protocol))];
  if (protoFilter && !used.includes(protoFilter)) protoFilter = '';
  setHTML('line-protos', used.length > 1 ? `<div class="seg sm">${['', ...used].map(pr => `<button data-act="line.proto" data-id="${esc(pr)}" class="${pr === protoFilter ? 'active' : ''}">${esc(pr || t('line.allProtocols'))}</button>`).join('')}</div>` : '');
}
export function tick() { renderRows(); refreshLineDetail(); }
const filtering = () => !!(query || nodeFilter || protoFilter);

function tlsModeOf(l) {
  const tls = parseJ(l.tls);
  return tls.mode || (PROTOCOLS[l.protocol] || {}).tlsDefault || 'none';
}
function protoBadges(l) {
  const mode = tlsModeOf(l);
  const tr = parseJ(l.transport);
  let h = badge(l.protocol, 'primary');
  if (!(PROTOCOLS[l.protocol] || {}).noTls) h += ' ' + badge(mode === 'reality' ? 'Reality' : mode === 'cert' ? 'TLS' : 'plain', mode === 'reality' ? 'ok' : mode === 'cert' ? '' : 'warn');
  if (mode === 'cert' && parseJ(l.tls).cert) h += ' ' + badge(certLabel(parseJ(l.tls).cert));
  if (tr.type && tr.type !== 'tcp') h += ' ' + badge(tr.type);
  return h;
}

function renderRows() {
  const body = document.getElementById('lines-body');
  if (!body) return;
  const online = new Set(state.onlines.lines || []);
  renderProtoSeg();
  const rows = visibleRows();
  document.getElementById('line-count').textContent = `${rows.length} / ${state.lines.length}`;
  for (const id of [...selected]) if (!state.lines.some(l => l.id === id)) selected.delete(id); // 被删掉的线路不再算选中
  const bar = document.getElementById('line-batch-bar');
  if (bar) { bar.hidden = selected.size === 0; document.getElementById('line-batch-count').textContent = t('line.selected', { n: selected.size }); }
  const selAll = document.getElementById('line-sel-all');
  if (selAll) selAll.checked = rows.length > 0 && rows.every(l => selected.has(l.id));
  // 库里一条线路都没有(不是搜索没结果):说清线路是什么,并给出第一步
  if (!rows.length) {
    setHTML(body, `<tr><td colspan="10">${state.lines.length ? empty() : `<div class="empty-guide"><p>${t('line.emptyFirst')}</p><button class="btn primary" data-act="line.add">${t('line.emptyFirstBtn')}</button></div>`}</td></tr>`);
    return;
  }
  const nodeIdsOf = l => { const v = l.nodeIds; if (!v) return []; try { return Array.isArray(v) ? v : JSON.parse(v); } catch { return []; } };
  const serversCell = l => {
    const ids = nodeIdsOf(l), fam = familyOf(l);
    // 走 IPv6 的服务器后面跟这条线路自己的 IPv6 入站结论(按它的端口和传输层,见 reach.js v6ForLine)
    const tag = (id, inAll) => {
      const n = (state.nodes || []).find(x => x.id === id);
      const v6 = !!n && lineUsesV6(l, n);
      // "全部服务器"里因为默认走 IPv6 才列出来的那台,名字后面注明 IPv6(不然还没测过时看不出为什么列它)
      const fl = fam[id] ? ' · ' + famName(fam[id]) : inAll && v6 ? ' · IPv6' : '';
      return badge((n ? n.name : '#' + id) + fl, 'primary') + (v6 ? ' ' + v6LineBadge(v6Of(id), l) : '');
    };
    if (!ids.length) { // 全部服务器:把单独选了 IP 版本的、以及这条线路在上面走 IPv6 的(默认给 IPv6 的服务器)列出来
      const shown = allServersShown(l);
      return `<span class="muted">${t('line.allServers')}</span>${shown.length ? ' ' + shown.map(id => tag(id, true)).join(' ') : ''}`;
    }
    return chipList(ids.map(id => tag(id, false)), { key: 'ln-' + l.id, max: 3 }); // 服务器多时只露前 3 个,其余折成 +N(展开状态 5 秒刷新不丢)
  };
  setHTML(body, rows.map(l => `
    <tr draggable="${filtering() ? 'false' : 'true'}" data-id="${l.id}" class="${selected.has(l.id) ? 'selected' : ''}">
      <td class="handle" title="拖动排序">⠿</td>
      <td><input type="checkbox" class="sel" data-change="line.sel" data-id="${l.id}" ${selected.has(l.id) ? 'checked' : ''}></td>
      <td class="primary-cell">${dot(online.has(l.name))}<a href="#" data-act="line.detail" data-id="${l.id}">${esc(l.name)}</a>${fwBadge(l)}</td>
      <td>${protoBadges(l)}</td>
      <td class="num">${l.port}</td>
      <td><span class="cell-clip" title="${esc(l.upstreamName)}">${esc(l.upstreamName)}</span>${rrOf(l).length ? ' ' + badge(t('line.rr.badge', { n: rrOf(l).length }), 'primary') : ''}</td>
      <td>${serversCell(l)}</td>
      <td class="num c-p3">${l.userCount}</td>
      <td><label class="switch" title="${l.enabled ? t('common.enabled') : t('common.disabled')}"><input type="checkbox" data-change="line.toggle" data-id="${l.id}" ${l.enabled ? 'checked' : ''}><span></span></label></td>
      <td class="actions">
        <button class="btn sm" data-act="line.detail" data-id="${l.id}">${t('common.details')}</button>
        <button class="btn sm" data-act="line.edit" data-id="${l.id}">${t('common.edit')}</button>
        <details class="menu"><summary class="btn sm">${t('common.more')} ▾</summary><div class="menu-list">
          <button data-act="line.clone" data-id="${l.id}">⧉ ${t('line.clone')}</button>
          <button data-act="line.asUpstream" data-id="${l.id}">⇢ ${t('up.addFromLine')}</button>
          <hr><button class="danger" data-act="line.del" data-id="${l.id}">${t('common.delete')}</button>
        </div></details>
      </td>
    </tr>`).join(''));
  if (!filtering()) enableDragSort(body); // 筛选后看到的不是全部线路,拖动排序会打乱没显示的那些
}

function enableDragSort(tbody) {
  let dragged = null;
  tbody.querySelectorAll('tr').forEach(tr => {
    tr.ondragstart = () => { dragged = tr; tr.classList.add('dragging'); };
    tr.ondragend = async () => {
      tr.classList.remove('dragging');
      const ids = [...tbody.querySelectorAll('tr')].map(r => Number(r.dataset.id));
      try { await post('lines/sort', ids); await load('lines'); toast(t('line.sorted'), 'ok'); }
      catch (e) { toast(e.message, 'err'); }
    };
    tr.ondragover = e => {
      e.preventDefault();
      if (!dragged || dragged === tr) return;
      const rect = tr.getBoundingClientRect();
      tr.parentNode.insertBefore(dragged, (e.clientY - rect.top) / rect.height > 0.5 ? tr.nextSibling : tr);
    };
  });
}

// ---- 双栈:线路单独选 IP 版本 ----
const famName = f => (f === 'v6' ? 'IPv6' : 'IPv4');
function familyOf(l) { const v = parseJ(l.nodeFamily); return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
const isV4Addr = s => /^\d+\.\d+\.\d+\.\d+$/.test(s);
const bareAddr = n => String(n.addr || '').trim().replace(/^\[|\]$/g, '');
// 这台两个地址都有才算双栈(探测到的,手填的连接地址算它那一族;和后端 model.FamilyIPs 一致)
function isDualStack(n) {
  const a = bareAddr(n), pub = String(n.publicIp || '').trim(), pub6 = String(n.publicIp6 || '').trim();
  return (isV4Addr(a) || isV4Addr(pub)) && (a.includes(':') || pub6.includes(':') || pub.includes(':'));
}
// 这台两族各自的地址(和后端 model.FamilyIPs 一致):探测到的两个,手填的连接地址是哪一族就顶替哪一族
function famIPs(n) {
  const a = bareAddr(n), pub = String(n.publicIp || '').trim(), pub6 = String(n.publicIp6 || '').trim();
  let v4 = isV4Addr(pub) ? pub : '', v6 = pub6.includes(':') ? pub6 : (pub.includes(':') ? pub : '');
  if (isV4Addr(a)) v4 = a; else if (a.includes(':')) v6 = a;
  return { v4, v6 };
}
// IPv6 地址的规范写法(补全 8 段、去掉前导零、小写),不是 IPv6 返回 '':写法不同(大写、省略的零、方括号)也认得出是同一个地址
function v6Norm(s) {
  let a = String(s ?? '').trim().replace(/^\[|\]$/g, '').toLowerCase();
  if (!a.includes(':') || !/^[0-9a-f:.]+$/.test(a)) return '';
  const m = a.match(/^(.*:)(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/); // 末尾嵌着 IPv4 的写法(::ffff:1.2.3.4)
  if (m) {
    const o = m.slice(2).map(Number);
    if (o.some(x => x > 255)) return '';
    a = m[1] + ((o[0] << 8) | o[1]).toString(16) + ':' + ((o[2] << 8) | o[3]).toString(16);
  }
  const parts = a.split('::');
  if (parts.length > 2) return '';
  const head = parts[0] ? parts[0].split(':') : [], tail = parts.length === 2 && parts[1] ? parts[1].split(':') : [];
  const fill = 8 - head.length - tail.length;
  if (parts.length === 2 ? fill < 1 : fill !== 0) return '';
  const groups = [...head, ...Array(parts.length === 2 ? fill : 0).fill('0'), ...tail];
  return groups.every(g => /^[0-9a-f]{1,4}$/.test(g)) ? groups.map(g => parseInt(g, 16).toString(16)).join(':') : '';
}
// 地址是哪一族:IPv4 字面量 'v4'(IPv4 映射地址也算,和后端一致),IPv6 字面量 'v6',域名(和写错的)''
function ipFam(s) {
  const a = String(s ?? '').trim().replace(/^\[|\]$/g, '');
  if (isV4Addr(a)) return 'v4';
  const n = v6Norm(a);
  return !n ? '' : n.startsWith('0:0:0:0:0:ffff:') ? 'v4' : 'v6';
}
// 这台自己的 IPv6(和后端 runner.subAddrs.own6 一致):手填的连接地址、探测到的公网 IPv6
const own6 = n => new Set([bareAddr(n), n.publicIp6, n.publicIp].map(v6Norm).filter(a => a && !a.startsWith('0:0:0:0:0:ffff:')));
// 自定义地址里取键:不分大小写(和订阅、后端保存时的读法一致)
const keyOf = (o, k) => {
  if (!o || typeof o !== 'object') return undefined;
  for (const [kk, v] of Object.entries(o)) if (kk.toLowerCase() === k) return v;
  return undefined;
};
// 自定义地址里这台拿得到的那几条(node_id 写这台的、没写的);没填自定义地址返回 null
function customFor(l, n) {
  const list = parseJ(l.addrs);
  if (!Array.isArray(list) || !list.length) return null;
  return list.filter(c => { const id = Number(keyOf(c, 'node_id')) || 0; return !id || id === n.id; });
}
// 这条线路在这台服务器上订阅里给不给这台的 IPv6(和后端 sub.resolveAddrs、runner.lineUse 一致):
// 填了自定义地址的看这台拿得到的那几条里有没有写成这台自己 IPv6 的 —— 别的 IPv6(国内中转、CDN 的入口)不算,
// 写不写 node_id 都一样:用户连的是中转,这台不用为它放行 IPv6,IPv6 入站检测也不测它;
// 否则单独选了、且这台有那一族的地址就按选的,再否则跟服务器的默认
function lineUsesV6(l, n, f = familyOf(l)[n.id]) {
  const mine = customFor(l, n);
  if (mine) {
    const own = own6(n);
    return mine.some(c => ipFam(keyOf(c, 'server')) === 'v6' && own.has(v6Norm(keyOf(c, 'server'))));
  }
  const ips = famIPs(n);
  if (f === 'v6' && ips.v6) return true;
  if (f === 'v4' && ips.v4) return false;
  return serverFamily(n) === 'IPv6';
}
// 订阅里的节点地址设成了"域名"(设置 → 订阅):没单独选 IP 版本的线路给的是域名,客户端自己解析
const subByDomain = () => String((state.settings || {}).subServerAddr || '').toLowerCase() === 'domain';
// 没单独选时这台在订阅里实际给哪个版本(和后端 sub.EntriesFromNodes 一致):订阅给域名时由客户端解析决定('dns');
// 否则手填的连接地址优先,再按"自动地址用"挑探测到的地址,选的那个没探测到就用另一个
function serverFamily(n) {
  if (subByDomain()) return 'dns';
  const a = bareAddr(n);
  if (isV4Addr(a)) return 'IPv4';
  if (a.includes(':')) return 'IPv6';
  if (a) return ''; // 手填的是域名:也由解析决定
  const ips = famIPs(n);
  if (ips.v6 && (n.addrFamily === 'v6' || !ips.v4)) return 'IPv6';
  return ips.v4 ? 'IPv4' : famName(n.addrFamily);
}
const famSelect = (n, cur) => {
  const def = serverFamily(n);
  return `<select class="sm node-fam" data-node="${n.id}" aria-label="${esc(t('line.ipVersion'))}">
    <option value="">${esc(def === 'dns' ? t('line.famDefaultDns') : def ? t('line.famDefault', { f: def }) : t('line.famDefaultPlain'))}</option>
    <option value="v4" ${cur === 'v4' ? 'selected' : ''}>IPv4</option>
    <option value="v6" ${cur === 'v6' ? 'selected' : ''}>IPv6</option>
  </select>`;
};
// 部署到哪些服务器;双栈的那行多一个小下拉。只有一台服务器时不用勾选,是双栈才露出下拉
function serversField(l) {
  const nodes = state.nodes || [];
  const fam = familyOf(l);
  // 订阅给域名时"默认跟服务器页的设置走"不成立(服务器的"自动地址用"不起作用),换成域名模式的说明,不接在后面自相矛盾
  if (nodes.length < 2) {
    return nodes.length && isDualStack(nodes[0]) ? `<div class="full">${field(t('line.ipVersion'), famSelect(nodes[0], fam[nodes[0].id]), subByDomain() ? t('line.famPick') + ' ' + t('line.famDnsHelp') : t('line.ipVersionHelp'))}</div>` : '';
  }
  const ids = (() => { const v = l.nodeIds; if (!v) return []; try { return Array.isArray(v) ? v : JSON.parse(v); } catch { return []; } })();
  const anyDual = nodes.some(isDualStack);
  const rows = nodes.map(n => `<div class="node-row"><label><input type="checkbox" class="node-cb" value="${n.id}" ${!ids.length || ids.includes(n.id) ? 'checked' : ''}> ${esc(n.name)}${n.isLocal ? ` <span class="muted small">(${t('node.local')})</span>` : ''}</label>${isDualStack(n) ? famSelect(n, fam[n.id]) : ''}</div>`).join('');
  const famHelp = subByDomain() ? t('line.famPickMulti') + ' ' + t('line.famDnsHelp') : t('line.famHelp');
  return `<div class="full">${fieldBox(t('line.servers'), `<div class="check-list node-list">${rows}</div>`, t('line.serversHelp') + (anyDual ? ' ' + famHelp : ''))}</div>`;
}
// 给前端测试用(web/jstest/lines.test.mjs)
export { serverFamily, lineUsesV6, formV6Servers, formLine, allServersShown, v6TipsOpen, v6Norm };
// "全部服务器"的线路在列表里列出哪几台:单独选了 IP 版本的,以及这条线路在上面走 IPv6 的(纯 IPv6 机器、"自动地址用 IPv6"、
// 自定义地址写成这台自己 IPv6 的)—— 它们后面要跟 IPv6 入站结论,以前只列单独选了的,默认给 IPv6 的服务器永远看不到结论
function allServersShown(l) {
  const fam = familyOf(l);
  return (state.nodes || []).filter(n => fam[n.id] || (n.enabled !== false && lineUsesV6(l, n))).map(n => n.id);
}
// 表单里正在编辑的这条线路(算按线路的 IPv6 入站结论用):名字、协议、端口、自定义地址
function formLine() {
  let addrs = [];
  try { addrs = JSON.parse(fv('f-addrs').trim() || '[]'); } catch { addrs = []; }
  return { name: fv('f-name').trim(), protocol: fv('f-protocol'), port: Number(fv('f-port')) || 0, addrs: Array.isArray(addrs) ? addrs : [] };
}
// 按表单当前的勾选算:这条线路在哪些服务器上会给这台的 IPv6(填了自定义地址的看有没有写成那台自己的 IPv6)
function formV6Servers() {
  const nodes = state.nodes || [];
  const l = formLine();
  const cbs = [...document.querySelectorAll('.node-cb')];
  const picked = cbs.filter(c => c.checked).map(c => Number(c.value));
  const famSel = id => { const s = document.querySelector(`.node-fam[data-node="${id}"]`); return s && !s.disabled ? s.value : ''; };
  return nodes.filter(n => !picked.length || picked.includes(n.id)).filter(n => lineUsesV6(l, n, famSel(n.id)));
}
// 自检要点要不要直接展开:这条线路的端口测出不通;UDP 线路(测不了,只能照着要点抓包)也展开
function v6TipsOpen(l, servers) {
  return servers.some(n => { const st = v6ForLine(v6Of(n.id), l); return st === 'down' || st === 'udpLine'; });
}
// 线路会在某台服务器上给 IPv6 时,在表单里列出 IPv6 自检要点和这条线路在那台的 IPv6 入站结论
function syncV6Tips() {
  const box = document.getElementById('f-v6tips');
  if (!box) return;
  const v6 = formV6Servers();
  box.hidden = !v6.length;
  if (!v6.length) { box.innerHTML = ''; return; }
  const l = formLine();
  const status = v6.map(n => {
    const b = v6LineBadge(v6Of(n.id), l);
    return `<span class="nowrap">${esc(n.name)} ${b || `<span class="muted small">${esc(t('line.v6Never'))}</span>`}</span>`;
  }).join(' · ');
  const was = box.querySelector('details');
  const open = (was && was.open) || v6TipsOpen(l, v6); // 改名字、端口时重画,别把用户点开的收起来
  box.innerHTML = `<details class="adv v6-tips" ${open ? 'open' : ''}><summary>${esc(t('line.v6Tips'))}</summary>
    <p class="hint">${esc(t('line.v6Status'))} ${status}</p>
    <ol class="hint">${[1, 2, 3, 4, 5, 6].map(i => `<li>${esc(t('line.v6Tip' + i))}</li>`).join('')}</ol></details>`;
}
// 没部署到的服务器,下拉没有意义
function syncFamily() {
  const cbs = [...document.querySelectorAll('.node-cb')];
  const any = cbs.some(c => c.checked);
  document.querySelectorAll('.node-fam').forEach(s => {
    const cb = cbs.find(c => c.value === s.dataset.node);
    s.disabled = !!cb && any && !cb.checked;
  });
  syncV6Tips();
}

// ---- 表单 ----
const sel = (id, opts, cur, labels = {}) => `<select id="${id}">${opts.map(x => `<option value="${x}" ${cur === x ? 'selected' : ''}>${esc(labels[x] || x)}</option>`).join('')}</select>`;

function tlsSection(protocol, tls) {
  const spec = PROTOCOLS[protocol] || {};
  if (spec.noTls) return '';
  const mode = tls.mode || spec.tlsDefault;
  const modes = spec.tlsRequired ? ['cert', 'reality'] : ['cert', 'reality', 'none'];
  const r = tls.reality || {};
  return `<h3>${t('line.tls')}</h3><div class="form-grid">
    ${field(t('line.tlsMode'), sel('f-tlsmode', modes, mode, { cert: t('line.tls.cert'), reality: t('line.tls.reality'), none: t('line.tls.none') }), t('line.tlsHelp.' + mode))}
    ${field(t('line.fp'), sel('f-fp', ['', ...FPS], tls.fingerprint || '', { '': '默认' }))}
    <div id="f-cert-wrap" class="full" ${mode === 'cert' ? '' : 'hidden'}>${field(t('line.cert'), `<div class="row"><input type="hidden" id="f-cert" value="${esc(tls.cert || '')}"><button type="button" class="btn cert-pick-btn" data-act="line.pickCert" id="f-cert-btn">${esc(tls.cert ? certLabel(tls.cert) : t('line.certDefault'))} ▾</button></div>`, t('line.certHelp'))}</div>
    <div id="f-reality" class="form-grid full" ${mode === 'reality' ? '' : 'hidden'}>
      ${field(t('line.reality.server'), `<div class="row"><input id="f-hs" value="${esc(r.handshake_server || 'www.microsoft.com')}"><button type="button" class="btn sm" data-act="line.realityCheck">${t('line.reality.check')}</button><button type="button" class="btn sm" data-act="line.realityPick">${t('line.reality.pick')}</button></div><div id="f-hs-res" class="hint" hidden></div>`, t('line.reality.serverHelp'))}
      ${field(t('line.reality.port'), `<input id="f-hsport" type="number" value="${r.handshake_port || 443}">`)}
      <div class="full">${field(t('line.reality.private'), `<div class="row"><input id="f-priv" value="${esc(r.private_key || '')}"><button type="button" class="btn" data-act="line.genReality">${t('line.reality.gen')}</button></div>`)}</div>
      <div class="full">${field(t('line.reality.public'), `<input id="f-pub" value="${esc(r.public_key || '')}" readonly>`)}</div>
      <div class="full">${field(t('line.reality.shortIds'), `<div class="row"><input id="f-sids" value="${esc((r.short_ids || []).join(','))}"><button type="button" class="btn" data-act="line.genShortId">${t('line.gen')}</button></div>`)}</div>
    </div>
  </div>`;
}

function transportSection(protocol, tr) {
  if (!(PROTOCOLS[protocol] || {}).transport) return '';
  const type = tr.type || 'tcp';
  const host = tr.headers && tr.headers.Host ? (Array.isArray(tr.headers.Host) ? tr.headers.Host[0] : tr.headers.Host) : (Array.isArray(tr.host) ? tr.host.join(',') : (tr.host || ''));
  return `<h3>${t('line.transport')}</h3><div class="form-grid">
    ${field(t('line.transport.type'), sel('f-trtype', ['tcp', 'ws', 'grpc', 'httpupgrade', 'http'], type, { tcp: t('line.transport.tcp') }), t('line.transportHelp'))}
    <div id="f-tr-fields" class="form-grid full">${transportFields(type, tr, host)}</div>
  </div>`;
}
function transportFields(type, tr, host) {
  switch (type) {
    case 'ws': case 'httpupgrade': case 'http':
      return field(t('line.transport.path'), `<input id="f-trpath" value="${esc(tr.path || '/')}">`) + field(t('line.transport.host'), `<input id="f-trhost" value="${esc(host || '')}" placeholder="cdn.example.com">`);
    case 'grpc':
      return field(t('line.transport.service'), `<input id="f-trsvc" value="${esc(tr.service_name || 'grpc')}">`);
  }
  return '';
}

function protoSection(protocol, o) {
  let h = '';
  switch (protocol) {
    case 'vless': h = check('f-vision', t('line.vision'), o.vision !== false); break;
    case 'hysteria2':
      h = field(t('line.upMbps'), `<input id="f-up" type="number" value="${o.up_mbps || 0}">`) +
        field(t('line.downMbps'), `<input id="f-down" type="number" value="${o.down_mbps || 0}">`) +
        field(t('line.obfs'), `<input id="f-obfs" value="${esc((o.obfs && o.obfs.password) || '')}">`) +
        `<div class="full">${field(t('line.masq'), `<input id="f-masq" value="${esc(masqOf(o))}" placeholder="https://www.bing.com">`, t('line.masqHelp'))}</div>` +
        `<div class="full">${field(t('line.hop'), `<input id="f-hop" value="${esc(o.port_hopping || '')}" placeholder="20000-30000">`, t('line.hopHelp'))}</div>`; break;
    case 'tuic': h = field(t('line.cc'), sel('f-cc', ['cubic', 'bbr', 'new_reno'], o.congestion_control || 'cubic')); break;
    case 'shadowsocks':
      h = field(t('line.method'), sel('f-method', SS_METHODS, o.method || 'aes-256-gcm')) +
        field(t('line.password'), `<div class="row"><input id="f-sspw" value="${esc(o.password || '')}"><button type="button" class="btn" data-act="line.genSsPw">${t('line.gen')}</button></div>`); break;
    case 'anytls':
      h = `<div class="full">${field(t('line.padding'), `<textarea id="f-padding">${esc((o.padding_scheme || []).join('\n'))}</textarea>`)}</div>`; break;
  }
  return h ? `<h3>${t('line.proto')}</h3><div class="form-grid">${h}</div>` : '';
}

function extraOf(protocol, o) {
  const known = new Set(OPT_KEYS[protocol] || []);
  const extra = {};
  Object.entries(o).forEach(([k, v]) => { if (!known.has(k)) extra[k] = v; });
  return Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
}

function renderDynamic(l) {
  const protocol = fv('f-protocol');
  const o = parseJ(l.options), tls = parseJ(l.tls), tr = parseJ(l.transport);
  document.getElementById('f-dyn').innerHTML = tlsSection(protocol, tls) + transportSection(protocol, tr) + protoSection(protocol, o);
  document.getElementById('f-extra').value = extraOf(protocol, o);
  const tm = document.getElementById('f-tlsmode');
  if (tm) tm.addEventListener('change', e => {
    document.getElementById('f-reality').hidden = e.target.value !== 'reality';
    document.getElementById('f-cert-wrap').hidden = e.target.value !== 'cert';
    const help = tm.closest('.field').querySelector('.help');
    if (help) help.textContent = t('line.tlsHelp.' + e.target.value);
  });
  const tt = document.getElementById('f-trtype');
  if (tt) tt.addEventListener('change', e => { document.getElementById('f-tr-fields').innerHTML = transportFields(e.target.value, {}, ''); });
}

function readForm(id) {
  const protocol = fv('f-protocol');
  const spec = PROTOCOLS[protocol] || {};
  const body = {
    name: fv('f-name').trim(), protocol, port: Number(fv('f-port')),
    upstreamId: Number(fv('f-upstream')), enabled: fchk('f-enabled'),
    routeRules: rrRead(),
    forwards: fwCollect(),
  };
  // 部署到哪些服务器:全不勾或全勾 = 全部(存空)
  const nodeCbs = [...document.querySelectorAll('.node-cb')];
  const picked = nodeCbs.filter(c => c.checked).map(c => Number(c.value));
  body.nodeIds = (picked.length && picked.length < nodeCbs.length) ? picked : [];
  body.nodeFamily = {};
  document.querySelectorAll('.node-fam').forEach(s => { if (s.value && !s.disabled) body.nodeFamily[s.dataset.node] = s.value; });
  // TLS
  if (!spec.noTls) {
    const tls = { mode: fv('f-tlsmode') };
    if (fv('f-fp')) tls.fingerprint = fv('f-fp');
    if (tls.mode === 'cert' && fv('f-cert')) tls.cert = fv('f-cert');
    if (tls.mode === 'reality') {
      if (!fv('f-priv').trim()) throw new Error(t('line.reality.private') + ' ' + t('line.reality.gen'));
      tls.reality = {
        handshake_server: fv('f-hs').trim(), handshake_port: Number(fv('f-hsport')) || 443,
        private_key: fv('f-priv').trim(), public_key: fv('f-pub').trim(),
        short_ids: fv('f-sids').split(',').map(s => s.trim()).filter(Boolean),
      };
    }
    body.tls = tls;
  }
  // 传输
  if (spec.transport) {
    const type = fv('f-trtype');
    if (type && type !== 'tcp') {
      const tr = { type };
      if (type === 'ws' || type === 'httpupgrade' || type === 'http') {
        tr.path = fv('f-trpath').trim() || '/';
        const host = fv('f-trhost').trim();
        if (host) {
          if (type === 'ws') tr.headers = { Host: host };
          else if (type === 'http') tr.host = host.split(',').map(s => s.trim()).filter(Boolean);
          else tr.host = host;
        }
      }
      if (type === 'grpc') tr.service_name = fv('f-trsvc').trim() || 'grpc';
      body.transport = tr;
    } else body.transport = {};
  }
  // 协议参数
  const o = JSON.parse(fv('f-extra').trim() || '{}');
  switch (protocol) {
    case 'vless': o.vision = fchk('f-vision'); break;
    case 'hysteria2':
      if (Number(fv('f-up')) > 0) o.up_mbps = Number(fv('f-up')); else delete o.up_mbps;
      if (Number(fv('f-down')) > 0) o.down_mbps = Number(fv('f-down')); else delete o.down_mbps;
      if (fv('f-obfs')) o.obfs = { type: 'salamander', password: fv('f-obfs') }; else delete o.obfs;
      if (fv('f-hop').trim()) o.port_hopping = fv('f-hop').trim(); else delete o.port_hopping;
      { // 伪装站点:别人探测这个端口时看到的是这个网站,不是一个报错的代理
        const m = fv('f-masq').trim();
        if (m && !/^https?:\/\/[^\s/]+/i.test(m)) throw new Error(t('line.masqBad'));
        if (m) o.masquerade = { type: 'proxy', url: m, rewrite_host: true }; else delete o.masquerade;
      }
      break;
    case 'tuic': o.congestion_control = fv('f-cc'); break;
    case 'shadowsocks':
      o.method = fv('f-method'); o.password = fv('f-sspw').trim();
      if (!o.password) throw new Error(t('line.password'));
      break;
    case 'anytls': {
      const lines = fv('f-padding').split('\n').map(s => s.trim()).filter(Boolean);
      if (lines.length) o.padding_scheme = lines; else delete o.padding_scheme;
      break;
    }
  }
  body.options = o;
  return body;
}

// 一键预设:常用协议组合,随机挑一个未占用端口,Reality 自动生成密钥
const PRESETS = [
  { id: 'hy2', label: 'Hysteria2', hint: 'UDP · 抗丢包 · 需证书', protocol: 'hysteria2', tls: { mode: 'cert' } },
  { id: 'anytls', label: 'AnyTLS', hint: 'TCP · 流量特征弱 · 需证书', protocol: 'anytls', tls: { mode: 'cert' } },
  { id: 'reality', label: 'VLESS + Reality', hint: 'TCP · 无需证书/域名 · Vision', protocol: 'vless', tls: { mode: 'reality' }, options: { vision: true } },
  { id: 'trojan', label: 'Trojan', hint: 'TCP · 需证书', protocol: 'trojan', tls: { mode: 'cert' } },
  { id: 'ss2022', label: 'Shadowsocks 2022', hint: 'TCP/UDP · 无 TLS · 兼容性最好', protocol: 'shadowsocks', tls: { mode: 'none' }, options: { method: '2022-blake3-aes-128-gcm' } },
  { id: 'vmess-ws', label: 'VMess + WS', hint: 'TCP · 可套 CDN', protocol: 'vmess', tls: { mode: 'none' }, transport: { type: 'ws', path: '/ws' } },
];
// 新线路的默认端口:优先问服务端(它避开面板/订阅/代理面板端口,并真的试着监听一次);
// 拿不到就在本地挑一个没被其它线路用的五位端口。填好后仍然可以改。
async function suggestPort() {
  try {
    const r = await get('keygen?type=port');
    if (r && r.port) return r.port;
  } catch { /* 接口不可用时退回本地挑 */ }
  const used = new Set(state.lines.map(l => l.port));
  for (let i = 0; i < 50; i++) {
    const p = 10000 + Math.floor(Math.random() * 55536);
    if (!used.has(p)) return p;
  }
  return 10000 + Math.floor(Math.random() * 55536);
}
async function presetLine(pid) {
  const p = PRESETS.find(x => x.id === pid);
  if (!p) return;
  const port = await suggestPort();
  const l = { protocol: p.protocol, enabled: true, options: { ...(p.options || {}) }, upstreamId: 0, port, name: `${p.label}-${port}`, tls: p.tls, transport: p.transport };
  // Shadowsocks 2022 的密码是固定长度的密钥:和其它预设一样先生成好,打开表单就能直接保存
  if (p.protocol === 'shadowsocks' && ss2022Bytes(l.options.method)) l.options.password = ss2022Key(l.options.method);
  if (p.tls && p.tls.mode === 'reality') {
    try {
      const kp = await get('keygen?type=reality');
      const sid = await get('keygen?type=shortid');
      l.tls = { mode: 'reality', reality: { private_key: kp.privateKey || kp.private_key, public_key: kp.publicKey || kp.public_key, short_ids: [sid.shortId || sid.short_id || sid.value], handshake_server: 'www.apple.com', handshake_port: 443 } };
    } catch (e) { toast(e.message, 'err'); }
  }
  editLine(null, null, l);
}

async function editLine(id, cloneFrom, preset) {
  await load('nodes').catch(() => {}); // 服务器列表可能刚在别的页面改过,"部署到服务器"要用最新的
  const src = cloneFrom || (id ? state.lines.find(x => x.id === id) : null);
  const l = src ? { ...src } : (preset || { protocol: 'vless', enabled: true, options: {}, upstreamId: 0 });
  formIsNew = !id && !cloneFrom;
  if (cloneFrom) { l.name = t('line.cloneOf', { name: src.name }); l.port = ''; l.forwards = null; }
  if (!l.port) l.port = await suggestPort(); // 新建/克隆:给个可用端口做默认值,可改
  const rrInit = rrOf(l);
  openModal(id ? t('line.edit') : t('line.add'), `
    <h3>${t('line.basic')}</h3>
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(l.name || '')}">`, t('line.nameHelp'))}
      ${field(t('line.protocol'), sel('f-protocol', Object.keys(PROTOCOLS), l.protocol))}
      ${field(t('common.port'), `<input id="f-port" type="number" min="1" max="65535" value="${l.port || ''}">`, t('line.portHelp'))}
      ${field(t('line.upstream'), `<select id="f-upstream"><option value="0">${t('line.direct')}</option>${state.upstreams.map(u => `<option value="${u.id}" ${l.upstreamId === u.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')}</select>`, t('line.upstreamHelp'))}
      <div class="full rr-box">
        <div class="rr-top">
          ${check('f-rr-on', t('line.rr.on'), rrInit.length > 0, t('line.rr.help'))}
          <div class="rr-lookup" id="f-rr-lookup" ${rrInit.length ? '' : 'hidden'}>
            <input id="f-rr-apps" placeholder="${esc(t('line.rr.appPh'))}" aria-label="${esc(t('line.rr.appPh'))}" autocomplete="off" spellcheck="false">
            <button type="button" class="btn sm" id="f-rr-apps-go">${esc(t('line.rr.lookup'))}</button>
          </div>
        </div>
        <p class="rr-lookup-msg" id="f-rr-apps-msg" hidden></p>
        <div id="f-rr" ${rrInit.length ? '' : 'hidden'}>
          <div class="rr-row rr-head" aria-hidden="true"><span>#</span><span>${esc(t('line.rr.type'))}</span><span>${esc(t('line.rr.values'))}</span><span>${esc(t('line.rr.to'))}</span><span></span></div>
          <div id="f-rr-list"></div>
          <div class="rr-row rr-rest"><span class="rr-no">—</span><span class="rr-rest-label">${esc(t('line.rr.rest'))}</span><span class="muted small">${esc(t('line.rr.restHint'))}</span><span class="rr-rest-to" id="f-rr-rest"></span><span></span></div>
          <button type="button" class="rr-add" id="f-rr-add">${esc(t('line.rr.add'))}</button>
          <p class="hint">${esc(t('line.rr.hint'))}</p>
        </div>
      </div>
      ${check('f-enabled', t('common.enabled'), l.enabled !== false)}
      ${id ? '' : check('f-assign', t('line.assignAll'), true, t('line.assignAllHelp'))}
      ${serversField(l)}
      <div class="full" id="f-v6tips" hidden></div>
      ${fwSection()}
    </div>
    <div id="f-dyn"></div>
    <details class="adv"><summary>${t('line.advanced')}</summary><textarea id="f-extra"></textarea></details>
    <details class="adv" style="margin-top:.5rem"><summary>${t('line.addrs')}</summary>
      <textarea id="f-addrs" placeholder='[{"server":"1.2.3.4","server_port":443,"remark":"-备用"}]'>${esc(l.addrs ? (typeof l.addrs === 'string' ? l.addrs : JSON.stringify(l.addrs, null, 2)) : '')}</textarea>
      <p class="hint">${t('line.addrsHelp')}</p></details>`, async () => {
    const body = readForm(id);
    const addrs = fv('f-addrs').trim();
    if (addrs) body.addrs = JSON.parse(addrs);
    if (!id) body.assignAll = fchk('f-assign');
    const saved = id ? await put('lines/' + id, body) : await post('lines', body);
    await load('lines', 'status', 'users');
    renderRows();
    toast(id ? t('line.updated') : t('line.created'), 'ok');
    scopeWidenedNotice(saved && saved.scopeWidened);
  }, { size: 'xl' });
  renderDynamic(l);
  rrBind(rrInit);
  fwBind(fwOf(l));
  document.getElementById('f-protocol').addEventListener('change', () => { renderDynamic({ options: {}, tls: {}, transport: {} }); syncV6Tips(); });
  ['f-name', 'f-port'].forEach(id => document.getElementById(id).addEventListener('input', debounce(syncV6Tips, 400)));
  document.querySelectorAll('.node-cb').forEach(c => c.addEventListener('change', () => { syncFamily(); fwRender(fwCollect()); }));
  document.getElementById('f-port').addEventListener('input', fwHint);
  document.querySelectorAll('.node-fam').forEach(c => c.addEventListener('change', syncV6Tips));
  document.getElementById('f-addrs').addEventListener('input', debounce(syncV6Tips, 400));
  syncFamily();
  if (!reachLoaded()) loadReach().then(syncV6Tips); // 新开页面直接点"添加"时还没拿过检测结果
}

// ---- 中转入口:客户端经别的服务器(集群中转)或外部中转(自己跑的 realm、买的中转)连到这条线路,加密数据原样转过去 ----
// 后端:保存校验 web/line_forwards.go、中转服务器上的转发 render/forward.go、订阅里的节点 sub/link.go forwardAddrs。
// 一行 = 方式 / 中转服务器或地址 / 端口 / 转给哪台 / 节点名后缀 / 更多(转到地址、暂停)与删除;部署范围变了,「转给」跟着变。
function fwOf(l) { const v = parseJ(l.forwards); return Array.isArray(v) ? v : []; }
const fwNode = id => (state.nodes || []).find(n => n.id === id);
const fwNodeName = id => (fwNode(id) || {}).name || '#' + id;
// 按表单当前勾的服务器:这条线路部署在哪几台(全不勾 = 全部)
function fwDeployed() {
  const nodes = state.nodes || [];
  const picked = [...document.querySelectorAll('.node-cb')].filter(c => c.checked).map(c => Number(c.value));
  return picked.length ? picked : nodes.map(n => n.id);
}

// 列表里线路名后面的小标记:有几个中转入口(暂停的不算),悬停看明细
function fwBadge(l) {
  const fs = fwOf(l).filter(f => !f.off);
  if (!fs.length) return '';
  const tip = fs.map(f => `${f.via ? fwNodeName(f.via) : f.host}:${f.port} → ${fwNodeName(f.to)}${f.name ? ' (' + f.name + ')' : ''}`).join('\n');
  return ` <span class="badge fw-badge" title="${esc(t('line.fw.title') + '\n' + tip)}">⇄ ${fs.length}</span>`;
}

function fwSection() {
  return `<div class="full fw-box">
    <div class="fw-top"><span class="fw-title">${esc(t('line.fw.title'))}</span><span class="muted small" id="f-fw-n"></span></div>
    <p class="hint fw-intro">${esc(t('line.fw.help'))}</p>
    <div id="f-fw" hidden>
      <div class="fw-row fw-head" aria-hidden="true"><span>${esc(t('line.fw.kind'))}</span><span>${esc(t('line.fw.where'))}</span><span>${esc(t('line.fw.port'))}</span><span>${esc(t('line.fw.to'))}</span><span>${esc(t('line.fw.name'))}</span><span></span></div>
      <div id="f-fw-list"></div>
      <p class="hint" id="f-fw-hint"></p>
    </div>
    <button type="button" class="rr-add" id="f-fw-add">${esc(t('line.fw.add'))}</button>
  </div>`;
}

function fwRowHTML(f) {
  const nodes = state.nodes || [];
  const ext = !f.via;
  const deployed = fwDeployed();
  const to = deployed.includes(f.to) ? f.to : deployed.length === 1 ? deployed[0] : 0;
  const toCell = deployed.length > 1
    ? `<select class="fw-to" aria-label="${esc(t('line.fw.to'))}">${to ? '' : `<option value="0">${esc(t('line.fw.toPick'))}</option>`}${deployed.map(id => `<option value="${id}" ${id === to ? 'selected' : ''}>${esc(fwNodeName(id))}</option>`).join('')}</select>`
    : `<span class="fw-to-fixed" data-to="${to}" title="${esc(fwNodeName(to))}">→ ${esc(fwNodeName(to))}</span>`;
  const more = !!(f.target || f.off);
  return `<div class="fw-row">
    <select class="fw-kind" aria-label="${esc(t('line.fw.kind'))}"><option value="via" ${ext ? '' : 'selected'} ${nodes.length < 2 ? 'disabled' : ''}>${esc(t('line.fw.kindVia'))}</option><option value="ext" ${ext ? 'selected' : ''}>${esc(t('line.fw.kindExt'))}</option></select>
    <span class="fw-where">
      <select class="fw-via" aria-label="${esc(t('line.fw.where'))}" ${ext ? 'hidden' : ''}>${nodes.map(n => `<option value="${n.id}" ${n.id === f.via ? 'selected' : ''}>${esc(n.name)}</option>`).join('')}</select>
      <input class="fw-host" value="${esc(f.host || '')}" placeholder="${esc(t('line.fw.hostPh'))}" aria-label="${esc(t('line.fw.where'))}" spellcheck="false" autocomplete="off" ${ext ? '' : 'hidden'}>
    </span>
    <input class="fw-port" type="number" min="1" max="65535" value="${f.port || ''}" aria-label="${esc(t('line.fw.port'))}">
    ${toCell}
    <input class="fw-name" maxlength="40" value="${esc(f.name || '')}" aria-label="${esc(t('line.fw.name'))}">
    <span class="rr-ops">
      <button type="button" class="btn sm ghost${more ? ' on' : ''}" data-fw="more" title="${esc(t('line.fw.more'))}" aria-label="${esc(t('line.fw.more'))}" aria-expanded="${more}">⋯</button>
      <button type="button" class="btn sm ghost danger" data-fw="del" title="${esc(t('common.delete'))}" aria-label="${esc(t('common.delete'))}">✕</button>
    </span>
    <div class="fw-more" ${more ? '' : 'hidden'}>
      <label class="fw-target-wrap" ${ext ? 'hidden' : ''}><span>${esc(t('line.fw.target'))}</span><input class="fw-target" value="${esc(f.target || '')}" placeholder="${esc(t('line.fw.targetPh'))}" title="${esc(t('line.fw.targetHelp'))}" spellcheck="false" autocomplete="off"></label>
      <label class="fw-off-wrap"><input type="checkbox" class="fw-off" ${f.off ? 'checked' : ''}> ${esc(t('line.fw.off'))}</label>
    </div>
    <div class="fw-note" hidden></div>
  </div>`;
}

// 一行的提示:节点名默认用什么;中转服务器和出口同一台、已停用、版本和主机不一样
function fwCheckRow(row) {
  const ext = row.querySelector('.fw-kind').value === 'ext';
  const via = Number(row.querySelector('.fw-via').value) || 0;
  const toSel = row.querySelector('.fw-to'), toFixed = row.querySelector('.fw-to-fixed');
  const to = Number(toSel ? toSel.value : toFixed ? toFixed.dataset.to : 0) || 0;
  row.querySelector('.fw-name').placeholder = ext ? t('line.fw.namePhExt') : t('line.fw.namePh', { name: fwNodeName(via) });
  const note = row.querySelector('.fw-note');
  let msg = '', bad = false;
  if (!ext && via && via === to) { msg = t('line.fw.same'); bad = true; }
  else if (!ext && fwNode(via) && fwNode(via).enabled === false) msg = t('line.fw.offNode');
  else if (!ext && fwNode(via) && (fwNode(via).status || {}).versionMismatch) msg = t('line.fw.old');
  note.textContent = msg;
  note.hidden = !msg;
  note.classList.toggle('bad', bad);
}

function fwCollect() {
  return [...document.querySelectorAll('#f-fw-list .fw-row')].map(row => {
    const ext = row.querySelector('.fw-kind').value === 'ext';
    const toSel = row.querySelector('.fw-to'), toFixed = row.querySelector('.fw-to-fixed');
    const f = { port: Number(row.querySelector('.fw-port').value) || 0, to: Number(toSel ? toSel.value : toFixed ? toFixed.dataset.to : 0) || 0 };
    const name = row.querySelector('.fw-name').value.trim();
    if (name) f.name = name;
    if (ext) f.host = row.querySelector('.fw-host').value.trim();
    else {
      f.via = Number(row.querySelector('.fw-via').value) || 0;
      const target = row.querySelector('.fw-target').value.trim();
      if (target) f.target = target;
    }
    if (row.querySelector('.fw-off').checked) f.off = true;
    return f;
  });
}

function fwHint() {
  const hint = document.getElementById('f-fw-hint');
  if (hint) hint.textContent = t('line.fw.hint', { port: fv('f-port') || '?' });
}

function fwRender(list) {
  const box = document.getElementById('f-fw-list');
  if (!box) return;
  box.innerHTML = list.map(fwRowHTML).join('');
  box.querySelectorAll('.fw-row').forEach(fwCheckRow);
  document.getElementById('f-fw').hidden = !list.length;
  document.getElementById('f-fw-n').textContent = list.length ? String(list.length) : '';
  fwHint();
}

function fwBind(initial) {
  const box = document.getElementById('f-fw-list');
  fwRender(initial);
  document.getElementById('f-fw-add').addEventListener('click', () => {
    const nodes = state.nodes || [], deployed = fwDeployed();
    // 默认经集群里不部署这条线路、还没被前面几行用上的那台中转(没有就用随便一台别的;只有一台服务器只能外部中转),
    // 端口照线路的端口(按原端口转发);这台这个端口前面已经用了就空着让人填
    const cur = fwCollect(), used = new Set(cur.filter(f => f.via).map(f => f.via));
    const spare = nodes.filter(n => !deployed.includes(n.id));
    const via = (spare.find(n => !used.has(n.id)) || spare[0] || nodes.find(n => deployed.length !== 1 || n.id !== deployed[0]) || {}).id || 0;
    let port = Number(fv('f-port')) || '';
    if (cur.some(f => f.via === via && f.port === port)) port = '';
    const f = { via: nodes.length > 1 ? via : 0, port, to: deployed.length === 1 ? deployed[0] : 0 };
    fwRender([...fwCollect(), f]);
    const rows = box.querySelectorAll('.fw-row');
    const last = rows[rows.length - 1];
    (last.querySelector(f.via ? '.fw-port' : '.fw-host') || last).focus();
  });
  box.addEventListener('click', e => {
    const b = e.target.closest('[data-fw]');
    if (!b) return;
    const row = b.closest('.fw-row');
    if (b.dataset.fw === 'del') {
      const rows = [...box.querySelectorAll('.fw-row')];
      fwRender(fwCollect().filter((_, i) => rows[i] !== row));
    } else if (b.dataset.fw === 'more') {
      const m = row.querySelector('.fw-more');
      m.hidden = !m.hidden;
      b.setAttribute('aria-expanded', String(!m.hidden));
    }
  });
  box.addEventListener('change', e => {
    const row = e.target.closest('.fw-row');
    if (!row) return;
    if (e.target.classList.contains('fw-kind')) {
      const ext = e.target.value === 'ext';
      row.querySelector('.fw-via').hidden = ext;
      row.querySelector('.fw-host').hidden = !ext;
      row.querySelector('.fw-target-wrap').hidden = ext;
      (ext ? row.querySelector('.fw-host') : row.querySelector('.fw-via')).focus();
    }
    fwCheckRow(row);
  });
}

// ---- 分流规则:同一条线路按域名 / IP 段 / 端口分给不同出口,没命中的走线路的上游(后端见 render/route_rules.go) ----
const RR_TYPES = ['domain_suffix', 'domain', 'domain_keyword', 'ip_cidr', 'port'];
const RR_NEW = () => ({ type: 'domain_suffix', values: [], to: 0 });
function rrOf(l) { const v = parseJ(l.routeRules); return Array.isArray(v) ? v : []; }

// 出口:直连 / 上游(分组)/ 拦截
function rrOutOptions(to) {
  const opt = (v, label) => `<option value="${v}" ${to === v ? 'selected' : ''}>${esc(label)}</option>`;
  const ups = state.upstreams.length ? `<optgroup label="${esc(t('nav.upstreams'))}">${state.upstreams.map(u => opt(u.id, u.name)).join('')}</optgroup>` : '';
  return opt(0, t('line.direct')) + ups + opt(-1, t('line.rr.reject'));
}

function rrRowHTML(r, i, n) {
  const type = RR_TYPES.includes(r.type) ? r.type : 'domain_suffix';
  const typeSel = `<select class="rr-type" aria-label="${esc(t('line.rr.type'))}" ${r.name ? 'hidden' : ''}>${RR_TYPES.map(k => `<option value="${k}" ${type === k ? 'selected' : ''}>${esc(t('line.rr.t.' + k))}</option>`).join('')}</select>`;
  // 查应用域名得来的规则带着应用名:左边显示应用名,下面小字是匹配方式(匹配方式就不给改了)
  const named = r.name ? `<span class="rr-name" title="${esc(r.name)}"><b>${esc(r.name)}</b><small>${esc(t('line.rr.t.' + type))}</small></span>` : '';
  return `<div class="rr-row" data-name="${esc(r.name || '')}">
    <span class="rr-no">${i + 1}</span>
    ${named ? `<span class="rr-type-cell">${named}${typeSel}</span>` : typeSel}
    <textarea class="rr-values" rows="1" spellcheck="false" autocomplete="off" aria-label="${esc(t('line.rr.values'))}" placeholder="${esc(t('line.rr.ph.' + type))}">${esc(r.raw ?? (r.values || []).join(', '))}</textarea>
    <select class="rr-to" aria-label="${esc(t('line.rr.to'))}">${rrOutOptions(Number(r.to) || 0)}</select>
    <span class="rr-ops">
      <button type="button" class="btn sm ghost" data-rr="up" title="${esc(t('line.rr.up'))}" aria-label="${esc(t('line.rr.up'))}" ${i === 0 ? 'disabled' : ''}>↑</button>
      <button type="button" class="btn sm ghost" data-rr="down" title="${esc(t('line.rr.down'))}" aria-label="${esc(t('line.rr.down'))}" ${i === n - 1 ? 'disabled' : ''}>↓</button>
      <button type="button" class="btn sm ghost danger" data-rr="del" title="${esc(t('common.delete'))}" aria-label="${esc(t('common.delete'))}">✕</button>
    </span>
    <div class="rr-err" hidden></div>
  </div>`;
}

// 查应用的域名:逗号、顿号、空格隔开都行
const RR_NOT_FOUND = '没找到这个应用'; // 与后端 appdomains.ErrNotFound 一致
const rrAppNames = s => s.split(/[\s,，;；、/|]+/).filter(Boolean);

// 查域名:面板所在的服务器去公开的域名库(v2fly)里查,每个应用作为一条带应用名的规则加到列表末尾:
// 完整域名并进域名后缀(只会多匹配它自己的子域名),关键字(少见)另起一条同名的。
// 内容可以再改,出口在规则里自己选(先是直连,光标落到第一条新规则的出口上)。列表里只有一条还没填的空规则时直接用它的位置。
// 没查到的名字留在查询框里,下面写上相近的名字(点一下替换再查)。
async function rrAppsLookup() {
  const input = document.getElementById('f-rr-apps'), btn = document.getElementById('f-rr-apps-go'), msg = document.getElementById('f-rr-apps-msg');
  const say = (html, err) => { msg.innerHTML = html; msg.hidden = !html; msg.classList.toggle('danger-text', !!err); };
  if (btn.disabled) return; // 上一次还没查完(回车不受按钮变灰拦)
  const names = rrAppNames(input.value);
  if (!names.length) { say(esc(t('line.rr.appEmpty')), true); input.focus(); return; }
  btn.disabled = true;
  btn.textContent = t('line.rr.looking');
  say('');
  let res;
  try {
    res = (await post('route-apps', { names }, SLOW)).results || [];
  } catch (e) {
    say(esc(e.message), true);
    return;
  } finally {
    btn.disabled = false;
    btn.textContent = t('line.rr.lookup');
  }
  // 没这个应用(给相近的名字)与查询出错(连不上域名库之类,换个名字没用)分开说
  const found = res.filter(r => !r.error), missed = res.filter(r => r.error === RR_NOT_FOUND), failed = res.filter(r => r.error && r.error !== RR_NOT_FOUND);
  const add = [];
  found.forEach(r => {
    const dom = [...new Set([...(r.suffix || []), ...(r.full || [])])];
    if (dom.length) add.push({ name: r.query, type: 'domain_suffix', values: dom, to: 0 });
    if ((r.keyword || []).length) add.push({ name: r.query, type: 'domain_keyword', values: r.keyword, to: 0 });
  });
  let rules = rrCollect(true);
  if (rules.length === 1 && !rules[0].values.length) rules = [];
  const first = rules.length; // 第一条新规则的位置
  if (add.length) rrRender([...rules, ...add]);
  const parts = [];
  if (found.length) {
    const n = add.reduce((s, r) => s + r.values.length, 0);
    const detail = found.map(r => `${r.query} ${(r.suffix || []).length + (r.full || []).length + (r.keyword || []).length}`).join('、');
    const total = rrCollect().reduce((s, r) => s + r.values.length, 0);
    parts.push(esc(total > 5000 ? t('line.rr.appTooMany', { n: total }) : t('line.rr.appDone', { k: add.length, n, detail })));
    // 微信、Gmail 这类是从大厂列表里挑出来的,说一声可能不全
    const part = found.filter(r => r.part);
    if (part.length) parts.push(esc(t('line.rr.appPart', { names: part.map(r => r.query).join('、'), lists: [...new Set(part.flatMap(r => (r.list || '').split(',')))].join('、') })));
  }
  if (missed.length) {
    const sug = missed.flatMap(r => (r.suggest || []).map(s => `<button type="button" class="link" data-rr-sug="${esc(s)}" data-q="${esc(r.query)}">${esc(s)}</button>`));
    parts.push(esc(t('line.rr.appMissed', { names: missed.map(r => r.query).join('、') })) + (sug.length ? ' ' + esc(t('line.rr.appSuggest')) + ' ' + sug.join(' ') : ''));
  }
  if (failed.length) parts.push(esc(t('line.rr.appFailed', { detail: failed.map(r => `${r.query}: ${trErr(r.error)}`).join('; ') })));
  input.value = [...missed, ...failed].map(r => r.query).join(', '); // 查到的从框里拿掉,没查到的留着改
  say(parts.join('<br>'), !found.length);
  const to = add.length && document.querySelectorAll('#f-rr-list .rr-row')[first]?.querySelector('.rr-to');
  if (to) { to.scrollIntoView({ block: 'nearest' }); to.focus(); }
}

const rrSplit = s => s.split(/[\s,，;；]+/).filter(Boolean);

// 填的时候就地检查,和后端 render.normRouteValue 同一套规矩(后端仍是最终把关)
const rrIPv4 = s => { const p = s.split('.'); return p.length === 4 && p.every(x => /^\d{1,3}$/.test(x) && +x <= 255); };
const rrIPv6 = s => s.includes(':') && /^[0-9a-f:.]+$/i.test(s);
function rrBadValue(type, v) {
  if (type === 'domain_keyword') return '';
  if (type === 'ip_cidr') {
    const [a, p, extra] = v.split('/');
    const bits = rrIPv4(a) ? 32 : rrIPv6(a) ? 128 : 0;
    return bits && extra === undefined && (p === undefined || (/^\d{1,3}$/.test(p) && +p <= bits)) ? '' : t('line.rr.err.ip', { v });
  }
  if (type === 'port') {
    const m = v.match(/^(\d+)(?:[-:](\d+))?$/);
    const lo = m ? +m[1] : 0, hi = m && m[2] ? +m[2] : lo;
    return m && lo >= 1 && hi <= 65535 && lo <= hi ? '' : t('line.rr.err.port', { v });
  }
  if (/[/:@*?#]/.test(v)) return t('line.rr.err.domain', { v });
  if (rrIPv4(v)) return t('line.rr.err.isIP', { v });
  return '';
}

// 检查一条,把第一个问题写在这条下面;返回问题(没有 = '')
function rrRowCheck(row) {
  const type = row.querySelector('.rr-type').value;
  let msg = '';
  for (const v of rrSplit(row.querySelector('.rr-values').value)) if ((msg = rrBadValue(type, v))) break;
  const err = row.querySelector('.rr-err');
  err.textContent = msg;
  err.hidden = !msg;
  row.querySelector('.rr-values').classList.toggle('bad', !!msg);
  return msg;
}

// 从界面读回当前的规则:值用换行、逗号(中英文)、空格、分号隔开都行;raw 是原样的输入,
// 排序 / 添加 / 删除重画时照原样放回去,不把用户的写法改掉(只发 values 给后端)
function rrCollect(withRaw) {
  return [...document.querySelectorAll('#f-rr-list .rr-row')].map(row => {
    const ta = row.querySelector('.rr-values');
    const r = { type: row.querySelector('.rr-type').value, values: rrSplit(ta.value), to: Number(row.querySelector('.rr-to').value) };
    if (row.dataset.name) r.name = row.dataset.name;
    if (withRaw) r.raw = ta.value;
    return r;
  });
}

// 输入框按内容长高(换行和逗号写法自动折行都算),最多约 8 行,再多出滚动条
function rrFit(ta) {
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight + 2, 180) + 'px';
}

function rrRender(rules, focusLast) {
  const list = document.getElementById('f-rr-list');
  list.innerHTML = rules.length
    ? rules.map((r, i) => rrRowHTML(r, i, rules.length)).join('')
    : `<p class="rr-empty">${esc(t('line.rr.none'))}</p>`;
  list.querySelectorAll('.rr-row').forEach(rrRowCheck);
  list.querySelectorAll('.rr-values').forEach(rrFit);
  if (focusLast) list.querySelector('.rr-row:last-child .rr-values')?.focus();
}

// 最后一行"其余流量 → 线路的上游",跟着上面的上游下拉变
function rrRest() {
  const up = document.getElementById('f-upstream');
  document.getElementById('f-rr-rest').textContent = up && up.selectedOptions[0] ? up.selectedOptions[0].textContent : '';
}

function rrBind(initial) {
  const on = document.getElementById('f-rr-on'), box = document.getElementById('f-rr'), list = document.getElementById('f-rr-list');
  rrRender(initial);
  rrRest();
  document.getElementById('f-upstream').addEventListener('change', rrRest);
  on.addEventListener('change', () => {
    box.hidden = !on.checked;
    document.getElementById('f-rr-lookup').hidden = !on.checked;
    if (!on.checked) document.getElementById('f-rr-apps-msg').hidden = true;
    if (on.checked && !rrCollect().length) rrRender([RR_NEW()], true); // 勾上就给一条空规则,直接填
    else if (on.checked) list.querySelectorAll('.rr-values').forEach(rrFit); // 藏着时量不出高度,显示出来再量
  });
  document.getElementById('f-rr-add').addEventListener('click', () => rrRender([...rrCollect(true), RR_NEW()], true));
  // 查应用的域名:按钮或回车;点相近的名字把没找到的那个换成它再查
  const apps = document.getElementById('f-rr-apps');
  document.getElementById('f-rr-apps-go').addEventListener('click', rrAppsLookup);
  apps.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); rrAppsLookup(); } });
  document.getElementById('f-rr-apps-msg').addEventListener('click', e => {
    const sug = e.target.closest('[data-rr-sug]');
    if (!sug) return;
    apps.value = rrAppNames(apps.value).map(x => x === sug.dataset.q ? sug.dataset.rrSug : x).join(', ');
    rrAppsLookup();
  });
  list.addEventListener('click', e => {
    const b = e.target.closest('[data-rr]');
    if (!b) return;
    const i = [...list.querySelectorAll('.rr-row')].indexOf(b.closest('.rr-row'));
    const rules = rrCollect(true);
    if (b.dataset.rr === 'del') rules.splice(i, 1);
    else {
      const j = b.dataset.rr === 'up' ? i - 1 : i + 1;
      if (j < 0 || j >= rules.length) return;
      [rules[i], rules[j]] = [rules[j], rules[i]];
    }
    rrRender(rules);
  });
  list.addEventListener('change', e => {
    if (!e.target.classList.contains('rr-type')) return;
    const row = e.target.closest('.rr-row');
    row.querySelector('.rr-values').placeholder = t('line.rr.ph.' + e.target.value);
    rrRowCheck(row);
  });
  list.addEventListener('input', e => { if (e.target.classList.contains('rr-values')) rrFit(e.target); }); // 长高立刻做,检查稍等一下再做
  list.addEventListener('input', debounce(e => { const row = e.target.closest('.rr-row'); if (row) rrRowCheck(row); }, 300));
}

// 保存时用:没勾 = 不分流(存空);勾了就每条都要有内容、内容要合规
function rrRead() {
  if (!fchk('f-rr-on')) return [];
  const rows = [...document.querySelectorAll('#f-rr-list .rr-row')];
  const rules = rrCollect();
  rules.forEach((r, i) => {
    if (!r.values.length) throw new Error(t('line.rr.emptyRule', { n: i + 1 }));
    const bad = rrRowCheck(rows[i]);
    if (bad) throw new Error(t('line.rr.errAt', { n: i + 1, msg: bad }));
  });
  return rules;
}

// ---- 批量设置 ----
async function runBatch(action, extra = {}) {
  const ids = [...selected];
  try {
    const r = await post('lines/batch', { ids, action, ...extra });
    selected.clear();
    await load('lines', 'status'); renderRows();
    toast(t('line.batchDone', { n: r.affected }), 'ok');
    scopeWidenedNotice(r.scopeWidened);
  } catch (e) { toast(e.message, 'err'); }
}

// 收窄部署服务器后,只选了被撤服务器的用户 / 代理 / 套餐被改回这条线路的全部服务器:点名给管理员看
function scopeWidenedNotice(w) {
  if (!w) return;
  const sep = t('node.sep');
  const parts = [['users', 'line.scopeWidenedUsers'], ['resellers', 'line.scopeWidenedResellers'], ['plans', 'line.scopeWidenedPlans']]
    .filter(([k]) => (w[k] || []).length).map(([k, key]) => t(key, { names: w[k].join(sep) }));
  if (!parts.length) return;
  setTimeout(() => confirm([t('line.scopeWidened'), ...parts].join('\n'), { title: t('line.scopeWidenedTitle'), okText: t('node.ack'), notice: true }), 0);
}
function batchDialog(action) {
  const n = selected.size;
  if (action === 'upstream') {
    openModal(t('line.batchUpstream', { n }), `<div class="form-grid">${field(t('line.upstream'), `<select id="b-upstream"><option value="0">${t('line.direct')}</option>${state.upstreams.map(u => `<option value="${u.id}">${esc(u.name)}</option>`).join('')}</select>`, t('line.upstreamHelp'))}</div>`,
      () => runBatch('upstream', { upstreamId: Number(fv('b-upstream')) }));
    return;
  }
  openModal(t('line.batchNodes', { n }), `<div class="form-grid"><div class="full">${fieldBox(t('line.servers'), `<div class="check-list">${(state.nodes || []).map(nd => `<label><input type="checkbox" class="b-node-cb" value="${nd.id}"> ${esc(nd.name)}${nd.isLocal ? ` <span class="muted small">(${t('node.local')})</span>` : ''}</label>`).join('')}</div>`, t('line.batchNodesHelp'))}</div></div>`,
    () => runBatch('nodes', { nodeIds: [...document.querySelectorAll('.b-node-cb:checked')].map(c => Number(c.value)) }));
}

// 选这条线路的证书:列出各服务器证书库的并集,每张证书标出哪台有、哪台没有(没有的那台用自己的默认证书)
function pickLineCert() {
  const parse = v => { try { return Array.isArray(v) ? v : (v ? JSON.parse(v) : []); } catch { return []; } };
  const checks = [...document.querySelectorAll('.node-cb')];
  const picked = checks.filter(c => c.checked).map(c => Number(c.value));
  const servers = (state.nodes || []).filter(n => !picked.length || picked.includes(n.id));
  const byName = new Map();
  for (const n of servers) {
    for (const c of parse(n.certs)) {
      if (!byName.has(c.name)) byName.set(c.name, { domains: c.domains || [], selfSigned: c.selfSigned, has: new Map() });
      byName.get(c.name).has.set(n.id, c);
    }
  }
  const cur = fv('f-cert');
  const now = Date.now() / 1000;
  // 各服务器上这张证书的剩余天数:剩 14 天以内、或会自动续却过了续期时间还没续上的变黄,自动续期在失败的变红(悬停看原因)
  const cell = (n, info) => {
    const c = info.has.get(n.id);
    if (!c) return { kind: 'warn', missing: true, html: badge(n.name + ' · ' + t('line.certMissing'), 'warn') };
    const left = Math.floor((c.notAfter - now) / 86400);
    const overdue = c.autoRenew && c.renewBefore && c.notAfter - now <= c.renewBefore;
    const kind = c.autoRenew && c.renewErr ? 'danger' : left <= 14 || overdue ? 'warn' : 'ok';
    const label = n.name + ' · ' + t('cert.daysLeftN', { n: left });
    return { kind, html: c.renewErr ? `<span class="badge ${kind}" title="${esc(trErr(c.renewErr))}">${esc(label)}</span>` : badge(label, kind) };
  };
  const chips = (info, name) => {
    const cells = servers.map(n => cell(n, info));
    const have = servers.filter(n => info.has.get(n.id)).length, missing = servers.length - have;
    const soon = cells.filter(x => x.kind !== 'ok' && !x.missing).length;
    if (servers.length <= 3) return cells.map(x => x.html).join('');
    // 每个选项本身是一个按钮:里面不能再放可点的东西(折叠按钮、<details> 会被浏览器挪到按钮外面)。
    // 写摘要,只列缺失 / 要处理的前 6 台,其余的名字放在「+N」的悬停提示里
    const sum = `<span class="muted small">${esc(t('cert.srvSum', { have, missing, soon }))}</span>`;
    const bad = cells.map((x, i) => ({ ...x, name: servers[i].name })).filter(x => x.kind !== 'ok');
    const rest = bad.slice(6);
    return sum + bad.slice(0, 6).map(x => x.html).join('')
      + (rest.length ? `<span class="badge" title="${esc(rest.map(x => x.name).join(t('reach.sep')))}">+${rest.length}</span>` : '');
  };
  const opts = [{ value: '', name: t('line.certDefault'), sub: esc(t('line.certDefaultHelp')), current: !cur }];
  for (const [name, info] of byName) {
    opts.push({ value: name, name: certLabel(name), sub: info.domains.length > 1 ? info.domains.map(esc).join(', ') : (info.selfSigned ? esc(t('cert.srcSelf')) : ''), chips: servers.length ? chips(info, name) : '', current: cur === name });
  }
  // 选过的证书已经不在任何服务器上了:也列出来,不然看不出当前选的是什么
  if (cur && !byName.has(cur)) opts.push({ value: cur, name: certLabel(cur), sub: esc(t('line.certMissing')), current: true });
  if (opts.length === 1) opts[0].sub = esc(t('line.certNoneYet'));
  openCertDrawer(t('line.certPick'), opts, value => {
    document.getElementById('f-cert').value = value;
    document.getElementById('f-cert-btn').textContent = (value ? certLabel(value) : t('line.certDefault')) + ' ▾';
    return true;
  });
}

// 已有的伪装站点;新建线路给一个默认值,改已有线路时留空就是不伪装
let formIsNew = false;
function masqOf(o) {
  const m = o.masquerade;
  if (typeof m === 'string') return m;
  if (m && m.url) return m.url;
  return formIsNew ? 'https://www.bing.com' : '';
}

// ---- 线路详情:这条线路的总流量、各服务器上分开的流量、此刻在线的用户与 IP ----
// 数据多时不撑破抽屉:服务器超过 8 台在限高区里滚动(手机上不套内滚动);在线用户一次显示 40 个、可搜、可再多显示;
// 一个用户的 IP 超过 3 个折成 +N。抽屉开着时在线部分跟着 5 秒一轮刷新(只取在线,不向副机要流量)。
let ld = null; // { id, hours, server(-1 = 合计), q, more, data, seq }
let ldHours = 24;
const LD_PAGE = 40;
const ldRangeKey = h => 'dash.range.' + (h === 24 ? '24h' : h === 168 ? '7d' : '30d');
const ldEtc = names => (names.length <= 3 ? names.join(t('reach.sep')) : t('dash.etc.nodes', { list: names.slice(0, 3).join(t('reach.sep')), n: names.length, more: names.length - 3 }));
const ldAlive = () => !!ld && drawerOpen() && !!document.getElementById('ld-sum');
// 在线列表按用户名自然排序(user2 在 user10 前面),同一个人再按服务器
const ldColl = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });
const ldSort = list => (list || []).slice().sort((a, b) => ldColl.compare(a.user, b.user) || ldColl.compare(a.server || '', b.server || ''));
const ldNarrow = () => matchMedia('(max-width: 700px)').matches;
const LD_SRV_FOLD = 8; // 手机上服务器列表先露 8 台(电脑上是限高滚动区)
const ldClip = (s, n = 24) => (s.length > n ? s.slice(0, n - 1) + '…' : s);

async function showLineDetail(id) {
  const l = state.lines.find(x => x.id === id);
  if (!l) return;
  ld = { id, hours: ldHours, server: -1, q: '', more: 0, data: null, seq: 0, allSrv: false };
  openDrawer(`${esc(l.name)} ${badge(l.protocol, 'primary')} <span class="muted small num">:${l.port}</span>${l.enabled ? '' : ' ' + badge(t('common.disabled'), 'warn')}`, `
    <div class="ld-sum" id="ld-sum"></div>
    ${fwDetail(l)}
    <section>
      <div class="ld-head"><h3>${t('line.d.traffic')}</h3>
        <select class="sm" id="ld-srv" hidden aria-label="${esc(t('nav.nodes'))}"></select>
        <div class="seg sm" id="ld-range">${[24, 168, 720].map(h => `<button data-act="line.dRange" data-id="${h}" class="${h === ld.hours ? 'active' : ''}">${t(ldRangeKey(h))}</button>`).join('')}</div></div>
      <div id="ld-chart"><div class="empty">${t('common.loading')}</div></div>
      <p class="hint" id="ld-note" hidden></p>
    </section>
    <section id="ld-srv-sec" hidden><h3>${t('line.d.servers')} <span class="muted small" id="ld-srv-n"></span></h3><div id="ld-servers"></div></section>
    <section>
      <div class="ld-head"><h3>${t('line.d.online')} <span class="muted small" id="ld-on-n"></span></h3>
        <input type="search" class="sm" id="ld-q" placeholder="${esc(t('line.d.search'))}" hidden></div>
      <div id="ld-online"><span class="muted small">${t('common.loading')}</span></div>
    </section>`, { wide: true });
  document.getElementById('ld-srv').addEventListener('change', e => { if (ld) { ld.server = Number(e.target.value); renderLineDetail(); } });
  document.getElementById('ld-q').addEventListener('input', debounce(e => { if (ld) { ld.q = e.target.value; ld.more = 0; renderLineOnline(); } }));
  await loadLineDetail();
}

async function loadLineDetail() {
  const cur = ld;
  if (!cur) return;
  const seq = ++cur.seq;
  let d;
  try { d = await get(`lines/${cur.id}/detail?hours=${cur.hours}&bucket=${bucketFor(cur.hours)}&tz=${tzOffsetMinutes()}`); }
  catch (e) { if (ld === cur && seq === cur.seq) setHTML('ld-chart', `<div class="empty">${esc(e.message)}</div>`); return; }
  if (ld !== cur || seq !== cur.seq || !ldAlive()) return; // 等数据期间换了范围、关了抽屉或打开了别的
  d.onlines = ldSort(d.onlines);
  cur.data = d;
  if (cur.server !== -1 && !(d.servers || []).some(s => s.nodeId === cur.server && s.ok)) cur.server = -1;
  renderLineDetail();
}

function renderLineDetail() {
  if (!ldAlive() || !ld.data) return;
  const d = ld.data, servers = d.servers || [], bad = servers.filter(s => !s.ok && !s.disabled), off = servers.filter(s => s.disabled);
  const tot = d.total || {};
  setHTML('ld-sum', `
    <div class="ld-tile"><span class="lb">${esc(t('line.d.sumTotal', { range: t(ldRangeKey(ld.hours)) }))}</span><b class="num">${fmtBytes((tot.totalUp || 0) + (tot.totalDown || 0), 1)}</b><span class="muted small">↑ ${fmtBytes(tot.totalUp || 0, 1)} · ↓ ${fmtBytes(tot.totalDown || 0, 1)}</span></div>
    <div class="ld-tile"><span class="lb">${esc(t('line.d.sumServers'))}</span><b class="num">${servers.length}</b>${bad.length ? `<span class="small warn-text">${esc(t('line.d.failedN', { n: bad.length }))}</span>` : off.length ? `<span class="muted small">${esc(t('line.d.offN', { n: off.length }))}</span>` : ''}</div>
    <div class="ld-tile"><span class="lb">${esc(t('line.d.sumUsers'))}</span><b class="num" id="ld-sum-u">${d.onlineUsers || 0}</b></div>
    <div class="ld-tile"><span class="lb">${esc(t('line.d.sumIPs'))}</span><b class="num" id="ld-sum-ip">${d.onlineIPs || 0}</b></div>`);
  // 选看哪台:两台以上才有;没取到的那几台不能选
  const sel = document.getElementById('ld-srv');
  sel.hidden = servers.length < 2;
  setHTML(sel, sel.hidden ? '' : `<option value="-1">${esc(t('line.d.allServers'))}</option>` +
    servers.map(s => `<option value="${s.nodeId}" ${s.ok ? '' : 'disabled'}>${esc(s.name)}${s.ok ? '' : ' · ' + esc(t('line.d.notFetched'))}</option>`).join(''));
  sel.value = String(ld.server);
  const cur = ld.server === -1 ? null : servers.find(s => s.nodeId === ld.server);
  const series = cur ? cur.series : tot;
  barChart(document.getElementById('ld-chart'), (series && series.points) || [], { span: series && series.span, height: 170, upLabel: t('dash.up'), downLabel: t('dash.down'), totalLabel: cur ? esc(ldClip(cur.name)) : esc(t('dash.total')), peakLabel: t('dash.peak'), emptyText: t('dash.noTraffic') });
  const notes = [];
  if (!cur && servers.length === 1 && !servers[0].ok) notes.push(t('line.d.single', { name: servers[0].name, err: trErr(servers[0].error || '') }));
  else if (!cur && bad.length) notes.push(t('line.d.partial', { names: ldEtc(bad.map(s => s.name)) }));
  if ((state.nodes || []).some(n => n.ratio > 0 && n.ratio !== 1)) notes.push(t('line.d.raw'));
  const note = document.getElementById('ld-note');
  note.hidden = !notes.length;
  note.innerHTML = notes.map(esc).join('<br>');
  // 各服务器:一台一行(名字、占比条、合计、上下行、此刻在线),点一行只看那台,再点回到合计
  const sec = document.getElementById('ld-srv-sec');
  sec.hidden = servers.length < 2;
  setText('ld-srv-n', t('line.d.serversN', { n: servers.length }));
  if (!sec.hidden) {
    const max = Math.max(1, ...servers.filter(s => s.ok).map(s => s.up + s.down));
    const onN = s => `<span class="on-n" data-node="${s.nodeId}">${esc(t('line.d.onlineN', { n: s.online || 0 }))}</span>`;
    const fold = ldNarrow() && !ld.allSrv && servers.length > LD_SRV_FOLD + 1;
    const shownSrv = fold ? servers.slice(0, LD_SRV_FOLD) : servers;
    setHTML('ld-servers', `<div class="ld-slist${servers.length > 8 ? ' capped' : ''}">${shownSrv.map(s => s.ok
      ? `<button type="button" class="ld-srow${s.nodeId === ld.server ? ' on' : ''}" data-act="line.dServer" data-id="${s.nodeId}" title="${esc(s.name + ' · ' + t('line.d.pickTip'))}">
          <span class="nm">${esc(s.name)}</span><span class="ld-bar"><span style="width:${Math.max(2, Math.round((s.up + s.down) / max * 100))}%"></span></span>
          <span class="num val">${fmtBytes(s.up + s.down, 1)}</span><span class="muted small ud">↑ ${fmtBytes(s.up, 1)} · ↓ ${fmtBytes(s.down, 1)}</span>${onN(s)}</button>`
      : `<div class="ld-srow bad${s.disabled ? ' off' : ''}"><span class="nm" title="${esc(s.name)}">${esc(s.name)}</span><span class="err" title="${esc(trErr(s.error || ''))}">${esc(trErr(s.error || ''))}</span>${onN(s)}</div>`).join('')}</div>` +
      (fold ? `<div class="card-foot"><button class="btn sm ghost" data-act="line.dAllSrv">${esc(t('line.d.allSrv', { n: servers.length }))}</button></div>`
        : ld.allSrv && ldNarrow() ? `<div class="card-foot"><button class="btn sm ghost" data-act="line.dAllSrv">${esc(t('common.less'))}</button></div>` : ''));
  }
  renderLineOnline();
}

function renderLineOnline() {
  if (!ldAlive() || !ld.data) return;
  const d = ld.data, all = d.onlines || [];
  setText('ld-on-n', all.length ? t('line.d.onCount', { u: d.onlineUsers || 0, ip: d.onlineIPs || 0 }) : '');
  const qBox = document.getElementById('ld-q');
  if (qBox) qBox.hidden = all.length <= 8 && !ld.q;
  if (!all.length) { setHTML('ld-online', `<p class="muted small">${esc(t('line.d.noOnline'))}</p>`); return; }
  const list = ld.q ? all.filter(o => matches(ld.q, o.user, o.server, ...(o.ips || []))) : all;
  if (!list.length) { setHTML('ld-online', empty(t('dash.online.noMatch'))); return; }
  const multi = (d.servers || []).length > 1 || new Set(all.map(o => o.nodeId)).size > 1;
  const n = LD_PAGE + ld.more, shown = list.slice(0, n);
  const box = document.querySelector('#ld-online .ld-olist');
  const keep = box ? box.scrollTop : 0;
  const changed = setHTML('ld-online', `<div class="ld-olist${multi ? ' srv' : ''}${shown.length > 12 ? ' capped' : ''}">${shown.map(o => `<div class="ld-orow">
      <a class="nm" href="#/users?q=${encodeURIComponent(o.user)}" title="${esc(o.user)}">${esc(o.user)}</a>
      ${multi ? `<span class="srv" title="${esc(o.server)}">${esc(o.server)}</span>` : ''}
      <span class="ips">${chipList((o.ips || []).map(ip => { const via = serverOfIP(ip); return `<span class="chip mono${via ? ' via' : ''}" title="${esc(via ? ip + ' · ' + t('line.fw.viaIP', { name: via }) : ip)}">${via ? '⇄ ' : ''}${esc(ip)}</span>`; }), { key: `ldip-${ld.id}-${o.nodeId}-${o.user}`, max: 3 })}</span>
    </div>`).join('')}</div>` +
    (list.length > n || ld.more ? `<div class="card-foot">${list.length > n ? `<button class="btn sm ghost" data-act="line.dMore">${esc(t('line.d.more', { n: Math.min(LD_PAGE, list.length - n), left: list.length - n }))}</button>` : ''}${ld.more ? `<button class="btn sm ghost" data-act="line.dLess">${esc(t('common.less'))}</button>` : ''}</div>` : ''));
  if (changed) { const b = document.querySelector('#ld-online .ld-olist'); if (b) b.scrollTop = keep; }
}

// 线路详情顶上的中转入口:一个一枚,多了折叠
function fwDetail(l) {
  const fs = fwOf(l);
  if (!fs.length) return '';
  const chips = fs.map(f => `<span class="chip${f.off ? ' off' : ''}" title="${esc((f.name ? f.name + ' · ' : '') + (f.via ? t('line.fw.kindVia') : t('line.fw.kindExt')))}">⇄ ${esc(f.via ? fwNodeName(f.via) : f.host)}:${f.port} → ${esc(fwNodeName(f.to))}${f.off ? ' · ' + esc(t('line.fw.off')) : ''}</span>`);
  return `<div class="ld-fw"><span class="muted small">${esc(t('line.fw.title'))}</span>${chipList(chips, { key: 'ldfw-' + l.id, max: 4 })}</div>`;
}

// 5 秒一轮:抽屉还开着、还是这条线路的详情,就只刷新在线用户与 IP
let ldRefreshing = false;
async function refreshLineDetail() {
  if (!ld) return;
  if (!ldAlive()) { ld = null; return; }
  const cur = ld;
  if (!cur.data || ldRefreshing) return;
  ldRefreshing = true;
  try {
    const o = await get(`lines/${cur.id}/detail?part=online`);
    if (ld !== cur || !ldAlive()) return;
    const per = o.perNode || {};
    Object.assign(cur.data, { onlines: ldSort(o.onlines), onlineUsers: o.onlineUsers || 0, onlineIPs: o.onlineIPs || 0 });
    (cur.data.servers || []).forEach(s => { s.online = per[s.nodeId] || 0; });
    setText('ld-sum-u', String(cur.data.onlineUsers));
    setText('ld-sum-ip', String(cur.data.onlineIPs));
    document.querySelectorAll('#ld-servers .on-n[data-node]').forEach(el => { el.textContent = t('line.d.onlineN', { n: per[el.dataset.node] || 0 }); });
    renderLineOnline();
  } catch {} finally { ldRefreshing = false; }
}

const realityText = r => r.ok ? t('line.reality.ok', { ms: r.ms }) : t('line.reality.bad.' + (r.error || 'connect'));

registerActions({
  'line.proto': id => { protoFilter = id || ''; renderRows(); },
  'line.realityCheck': async (_, btn) => {
    const box = document.getElementById('f-hs-res');
    btn.disabled = true; box.hidden = false; box.textContent = t('line.reality.checking'); box.className = 'hint';
    try {
      const r = await post('reality-check', { server: fv('f-hs').trim(), port: Number(fv('f-hsport')) || 443 }, SLOW);
      box.textContent = realityText(r);
      box.className = 'hint ' + (r.ok ? 'ok' : 'bad');
    } catch (e) { box.textContent = e.message; box.className = 'hint bad'; }
    btn.disabled = false;
  },
  'line.realityPick': async (_, btn) => {
    const box = document.getElementById('f-hs-res');
    btn.disabled = true; box.hidden = false; box.textContent = t('line.reality.checking'); box.className = 'hint';
    try {
      const { results } = await post('reality-check', {}, SLOW);
      const good = results.filter(x => x.ok).slice(0, 6);
      box.innerHTML = good.length
        ? `<div class="chips">${good.map(x => `<button type="button" class="chip" data-act="line.realityUse" data-id="${esc(x.server)}">${esc(x.server)} · ${x.ms} ms</button>`).join('')}</div>`
        : esc(t('line.reality.none'));
    } catch (e) { box.textContent = e.message; box.className = 'hint bad'; }
    btn.disabled = false;
  },
  'line.realityUse': id => {
    document.getElementById('f-hs').value = id;
    document.getElementById('f-hsport').value = 443;
    document.getElementById('f-hs-res').hidden = true;
  },
  'line.pickCert': () => pickLineCert(),
  'line.sel': (id, cb) => { cb.checked ? selected.add(Number(id)) : selected.delete(Number(id)); renderRows(); },
  'line.clearSel': () => { selected.clear(); renderRows(); },
  'line.batch': async action => {
    if (!selected.size) return;
    if (action === 'enable' || action === 'disable') { await runBatch(action); return; }
    batchDialog(action);
  },
  'line.preset': id => presetLine(id),
  'line.detail': id => showLineDetail(Number(id)),
  'line.dRange': id => {
    if (!ld) return;
    ld.hours = ldHours = Number(id) || 24;
    document.querySelectorAll('#ld-range button').forEach(b => b.classList.toggle('active', Number(b.dataset.id) === ld.hours));
    setHTML('ld-chart', `<div class="empty">${t('common.loading')}</div>`);
    loadLineDetail();
  },
  'line.dServer': id => { if (!ld) return; ld.server = ld.server === Number(id) ? -1 : Number(id); renderLineDetail(); },
  'line.dAllSrv': () => { if (!ld) return; ld.allSrv = !ld.allSrv; renderLineDetail(); },
  'line.dMore': () => { if (!ld) return; ld.more += LD_PAGE; renderLineOnline(); },
  'line.dLess': () => { if (!ld) return; ld.more = 0; renderLineOnline(); },
  'line.add': () => editLine(null),
  'line.edit': id => editLine(Number(id)),
  'line.clone': id => editLine(null, state.lines.find(x => x.id === Number(id))),
  'line.asUpstream': async id => { await load('nodes').catch(() => {}); editRelayUpstream(null, Number(id)); },
  'line.del': async id => {
    const l = state.lines.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: l.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('lines/' + id); await load('lines', 'status'); renderRows(); toast(t('line.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'line.toggle': async (id, input) => {
    input.disabled = true;
    try { await post(`lines/${id}/toggle`); await load('lines'); toast(t('line.toggled'), 'ok'); }
    catch (e) { toast(e.message, 'err'); input.checked = !input.checked; }
    finally { input.disabled = false; }
  },
  'line.genReality': async () => {
    const k = await get('keygen?type=reality');
    document.getElementById('f-priv').value = k.privateKey;
    document.getElementById('f-pub').value = k.publicKey;
    if (!fv('f-sids').trim()) document.getElementById('f-sids').value = (await get('keygen?type=shortid')).shortId;
  },
  'line.genShortId': async () => {
    const cur = fv('f-sids').trim();
    const s = (await get('keygen?type=shortid')).shortId;
    document.getElementById('f-sids').value = cur ? cur + ',' + s : s;
  },
  'line.genSsPw': async () => {
    const m = fv('f-method');
    document.getElementById('f-sspw').value = ss2022Bytes(m) ? ss2022Key(m) : (await get('keygen?type=password&len=16')).password;
  },
});

// Shadowsocks 2022 算法要求 base64 的 16 / 32 字节密钥(aes-128 是 16 字节,其余 32);不是 2022 算法返回 0
function ss2022Bytes(method) {
  const m = String(method || '');
  return m.startsWith('2022-blake3-aes-128') ? 16 : m.startsWith('2022') ? 32 : 0;
}
function ss2022Key(method) {
  const bytes = new Uint8Array(ss2022Bytes(method));
  crypto.getRandomValues(bytes);
  return btoa(String.fromCharCode(...bytes));
}

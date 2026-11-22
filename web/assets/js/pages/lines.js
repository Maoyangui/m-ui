import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { keep, esc, toast, confirm, openModal, openDrawer, drawerOpen, registerActions, badge, dot, field, check, empty, fv, fchk, matches, debounce, setHTML, setText, chipList, fieldBox, fmtBytes, tzOffsetMinutes, switchRow, fmtTime, toggleBatchBar } from '../ui.js';
import { barChart, bucketFor } from '../chart.js';
import { serverOfIP } from '../relayip.js';
import { openCertDrawer, certLabel } from '../certpicker.js';
import { editRelayUpstream } from './upstreams.js';
import { loadReach, reachLoaded, v6Of, v6LineBadge, v6ForLine, groupChip, v6Chip, v6Label, v6Explain, v6Tone, verdictLabel, verdictTone } from '../reach.js';

const selected = new Set(); // 批量设置勾选的线路 id
let selFrom = null; // 刚勾 / 取消的那个勾选框:批量栏露出 / 收起时让它那一行不跳(见 ui.js toggleBatchBar)
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

// 协议的显示名:列表、筛选、详情、编辑框的下拉都用它(库里存的是小写的协议 id)
const PROTO_LABEL = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', hysteria2: 'Hysteria2', tuic: 'TUIC', anytls: 'AnyTLS', shadowsocks: 'Shadowsocks', socks: 'SOCKS5', http: 'HTTP', mixed: 'Mixed' };
const protoName = p => PROTO_LABEL[p] || p;
// 服务器按分组排:[{name, nodes}],没分组的排最后(name = '');一台都没分组时只有这一段
function nodeGroups(nodes) {
  const by = new Map();
  for (const n of nodes || []) {
    const g = String(n.group || '').trim();
    if (!by.has(g)) by.set(g, []);
    by.get(g).push(n);
  }
  const out = [...by].filter(([g]) => g).map(([name, list]) => ({ name, nodes: list }));
  if (by.has('')) out.push({ name: '', nodes: by.get('') });
  return out;
}
const hasGroups = nodes => (nodes || []).some(n => String(n.group || '').trim());

export async function render(el) {
  const nodes = state.nodes || [];
  const opt = n => `<option value="${n.id}" ${n.id === nodeFilter ? 'selected' : ''}>${esc(n.name)}</option>`;
  const nodeOpts = hasGroups(nodes)
    ? nodeGroups(nodes).map(g => `<optgroup label="${esc(g.name || t('node.ungrouped'))}">${g.nodes.map(opt).join('')}</optgroup>`).join('')
    : nodes.map(opt).join('');
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="line-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      ${nodes.length > 1 ? `<select class="sm" id="line-node" style="width:auto;max-width:14rem" aria-label="${esc(t('nav.nodes'))}"><option value="0">${esc(t('line.allNodes'))}</option>${nodeOpts}</select>` : ''}
      <span id="line-protos"></span>
      <span class="muted small" id="line-count"></span>
      <span class="grow"></span>
      <details class="menu"><summary class="btn">${t('line.quick')}</summary><div class="menu-list">
        ${PRESETS.map(p => `<button data-act="line.preset" data-id="${p.id}"><b>${esc(p.label)}</b><span class="muted small" style="display:block">${esc(t(p.hint))}</span></button>`).join('')}
      </div></details>
      <button class="btn primary" data-act="line.add">${t('line.add')}</button>
    </div>
    <div class="toolbar batch-bar" id="line-batch-bar" hidden>
      <span class="badge primary" id="line-batch-count"></span>
      <button class="btn sm" data-act="line.batch" data-id="enable">${t('line.batch.enable')}</button>
      <button class="btn sm" data-act="line.batch" data-id="disable">${t('line.batch.disable')}</button>
      <button class="btn sm" data-act="line.batch" data-id="upstream">${t('line.batch.upstream')}</button>
      ${nodes.length > 1 ? `<button class="btn sm" data-act="line.batch" data-id="nodes">${t('line.batch.nodes')}</button>` : ''}
      <button class="btn sm ghost" data-act="line.clearSel">${t('line.batch.clear')}</button>
    </div>
    <div class="table-wrap fill"><table class="grid lines-grid" data-fit="p3:1180 p2:1020 cards:900">
      <thead><tr><th></th><th style="width:1.5rem"><input type="checkbox" id="line-sel-all"></th><th>${t('common.name')}</th><th>${t('line.protocol')}</th><th>${t('common.port')}</th><th class="c-p2">${t('line.upstream')}</th><th>${t('nav.nodes')}</th><th class="c-p3">${t('line.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
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
  setHTML('line-protos', used.length > 1 ? `<div class="seg sm">${['', ...used].map(pr => `<button data-act="line.proto" data-id="${esc(pr)}" class="${pr === protoFilter ? 'active' : ''}">${esc(pr ? protoName(pr) : t('line.allProtocols'))}</button>`).join('')}</div>` : '');
}
export function tick() { renderRows(); refreshLineDetail(); }
const filtering = () => !!(query || nodeFilter || protoFilter);

function tlsModeOf(l) {
  const tls = parseJ(l.tls);
  return tls.mode || (PROTOCOLS[l.protocol] || {}).tlsDefault || 'none';
}
// 协议一格:协议名一枚,后面小字写加密方式和传输;没加密的标黄;用了哪张证书放在悬停提示里
function protoCell(l) {
  const mode = tlsModeOf(l), tls = parseJ(l.tls), tr = parseJ(l.transport);
  const bits = [];
  let plain = false;
  if (!(PROTOCOLS[l.protocol] || {}).noTls) {
    if (mode === 'reality') bits.push('Reality');
    else if (mode === 'cert') bits.push('TLS');
    else plain = true;
  }
  if (tr.type && tr.type !== 'tcp') bits.push(tr.type);
  const tip = mode === 'cert' && tls.cert ? `${t('line.cert')}: ${certLabel(tls.cert)}` : '';
  return `<span class="ln-proto"${tip ? ` title="${esc(tip)}"` : ''}>${badge(protoName(l.protocol), 'primary')}${bits.length ? `<span class="muted small">${esc(bits.join(' · '))}</span>` : ''}${plain ? `<span class="small warn-text">${esc(t('line.plain'))}</span>` : ''}</span>`;
}

// 一台服务器上因为端口被别的程序占着而没开的入站(本机看 status,副机看它上报的)
const blockedOn = n => (n ? (n.isLocal ? (state.status || {}).blocked : (n.status || {}).blocked) : null) || [];
// 这条线路没开成的入站(线路自己的,或它的中转入口):[{ node, b }]
function blockedOf(l) {
  const mine = b => b && (b.tag === l.name || String(b.tag || '').startsWith(l.name + ' ⇄'));
  const out = [];
  for (const n of state.nodes || []) for (const b of blockedOn(n)) if (mine(b)) out.push({ node: n, b });
  return out;
}
function blockedBadge(l) {
  const list = blockedOf(l);
  if (!list.length) return '';
  const tip = list.map(({ node, b }) => t('line.blockedItem', { server: node.name, port: b.port, net: (b.net || 'tcp').toUpperCase(), by: b.by ? t('node.blockedBy', { by: b.by }) : '' })).join('\n') + '\n' + t('line.blockedHelp');
  return ` <span class="badge danger" title="${esc(tip)}">${esc(t('line.blocked'))}</span>`;
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
  if (bar) { document.getElementById('line-batch-count').textContent = t('line.selected', { n: selected.size }); toggleBatchBar(bar, selected.size > 0, selFrom); } // 先填数字再露出;刚勾的那一行不跳
  selFrom = null;
  const selAll = document.getElementById('line-sel-all');
  if (selAll) selAll.checked = rows.length > 0 && rows.every(l => selected.has(l.id));
  // 库里一条线路都没有(不是搜索没结果):说清线路是什么,并给出第一步
  if (!rows.length) {
    setHTML(body, `<tr><td colspan="10">${state.lines.length ? empty() : `<div class="empty-guide"><p>${t('line.emptyFirst')}</p><button class="btn primary" data-act="line.add">${t('line.emptyFirstBtn')}</button></div>`}</td></tr>`);
    return;
  }
  const nodes = state.nodes || [];
  const serversCell = l => {
    const ids = lineNodeIds(l), fam = familyOf(l);
    // 走 IPv6 的服务器后面跟这条线路自己的 IPv6 入站结论(按它的端口和传输层,见 reach.js v6ForLine)
    const tag = (id, inAll) => {
      const n = nodes.find(x => x.id === id);
      const v6 = !!n && lineUsesV6(l, n);
      // "全部服务器"里因为默认走 IPv6 才列出来的那台,名字后面注明 IPv6(不然还没测过时看不出为什么列它)
      const fl = fam[id] ? ' · ' + famName(fam[id]) : inAll && v6 ? ' · IPv6' : '';
      return badge((n ? n.name : '#' + id) + fl, 'primary') + (v6 ? ' ' + v6LineBadge(v6Of(id), l) : '');
    };
    if (!ids.length) { // 全部服务器:写「全部 N 台」,再把单独选了 IP 版本的、以及这条线路在上面走 IPv6 的列出来
      const shown = allServersShown(l);
      if (nodes.length < 2) return shown.length ? shown.map(id => tag(id, true)).join(' ') : `<span class="muted">${esc(nodes[0] ? nodes[0].name : t('line.allServers'))}</span>`;
      return `<span class="muted">${esc(t('line.allN', { n: nodes.length }))}</span>${shown.length ? ' ' + shown.map(id => tag(id, true)).join(' ') : ''}`;
    }
    return chipList(ids.map(id => tag(id, false)), { key: 'ln-' + l.id, max: 2 }); // 服务器多时只露前 2 个,其余折成 +N(展开状态 5 秒刷新不丢)
  };
  setHTML(body, rows.map(l => `
    <tr draggable="${filtering() ? 'false' : 'true'}" data-id="${l.id}" class="${selected.has(l.id) ? 'selected' : ''}">
      <td class="handle" title="${esc(t('line.dragSort'))}"><i class="i i-grip"></i></td>
      <td><input type="checkbox" class="sel" data-change="line.sel" data-id="${l.id}" ${selected.has(l.id) ? 'checked' : ''}></td>
      <td class="primary-cell">${dot(online.has(l.name))}<a href="#" data-act="line.detail" data-id="${l.id}" title="${esc(l.name)}">${esc(l.name)}</a>${blockedBadge(l)}</td>
      <td>${protoCell(l)}</td>
      <td class="num">${l.port}</td>
      <td class="c-p2">${l.upstreamId ? `<span class="cell-clip" title="${esc(l.upstreamName)}">${esc(l.upstreamName)}</span>` : `<span class="muted">${esc(t('line.directShort'))}</span>`}${rrOf(l).length ? ' ' + badge(t('line.rr.badge', { n: rrOf(l).length }), 'primary') : ''}</td>
      <td>${serversCell(l)}${fwBadge(l)}</td>
      <td class="num c-p3">${l.userCount}</td>
      <td><label class="switch" title="${l.enabled ? t('common.enabled') : t('common.disabled')}"><input type="checkbox" data-change="line.toggle" data-id="${l.id}" ${l.enabled ? 'checked' : ''}><span></span></label></td>
      <td class="actions">
        <button class="btn sm" data-act="line.detail" data-id="${l.id}">${t('common.details')}</button>
        <button class="btn sm" data-act="line.edit" data-id="${l.id}">${t('common.edit')}</button>
        <details class="menu"><summary class="btn sm">${t('common.more')}</summary><div class="menu-list">
          <button data-act="line.clone" data-id="${l.id}">${t('line.clone')}</button>
          <button data-act="line.asUpstream" data-id="${l.id}">${t('up.addFromLine')}</button>
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
// 表单里这条线路部署到哪几台:{ all, ids }。选了「全部服务器」、或只有一台服务器(没有勾选框)= 全部
function formScope() {
  const nodes = state.nodes || [];
  const picked = [...document.querySelectorAll('.node-cb')].filter(c => c.checked).map(c => Number(c.value));
  const el = document.getElementById('f-scope');
  const all = el ? el.value === 'all' : !picked.length; // 没有范围开关的地方(只有一台服务器、测试桩):全不勾 = 全部
  return { all, ids: all ? nodes.map(n => n.id) : picked };
}
// 部署到哪些服务器。上面一排选「指定服务器 / 全部服务器」:指定 = 只在勾的这几台上开(新建线路默认这个、一台不勾,
// 以前默认全勾,每次都要一台台取消);全部 = 现有的都开,以后新加的服务器也自动开(存成空)。
// 服务器有分组时按组列,组名前的勾选框整组勾;双栈的那行多一个 IP 版本小下拉。只有一台服务器时不用选,是双栈才露出下拉。
function serversField(l, isNew) {
  const nodes = state.nodes || [];
  const fam = familyOf(l);
  // 订阅给域名时"默认跟服务器页的设置走"不成立(服务器的"自动地址用"不起作用),换成域名模式的说明,不接在后面自相矛盾
  if (nodes.length < 2) {
    return nodes.length && isDualStack(nodes[0]) ? `<div class="ln-one-fam">${field(t('line.ipVersion'), famSelect(nodes[0], fam[nodes[0].id]), subByDomain() ? [t('line.famPick'), keep(t('line.famDnsHelp'))] : t('line.ipVersionHelp'))}</div>` : '';
  }
  const ids = lineNodeIds(l);
  const all = !isNew && !ids.length;
  // 一台服务器一格,多列排开:几十台也只是几行,不会把表单拉得很长;再多就在框里滚动、用搜索找。
  // 双栈服务器的 IP 版本下拉只在勾上之后才出现在它旁边(见 syncFamily)
  const cell = n => `<div class="node-row"><label title="${esc(n.name)}"><input type="checkbox" class="node-cb" value="${n.id}" ${all || ids.includes(n.id) ? 'checked' : ''} ${all ? 'disabled' : ''}> <span class="nm">${esc(n.name)}</span>${n.isLocal ? ` <span class="muted small">${t('node.local')}</span>` : ''}${n.enabled === false ? ` <span class="muted small">${t('common.disabled')}</span>` : ''}</label>${isDualStack(n) ? famSelect(n, fam[n.id]) : ''}</div>`;
  const body = hasGroups(nodes)
    ? nodeGroups(nodes).map(g => `<div class="node-grp"><label class="node-grp-h"><input type="checkbox" class="node-gcb"> <b>${esc(g.name || t('node.ungrouped'))}</b><span class="muted small node-gn"></span></label><div class="node-cells">${g.nodes.map(cell).join('')}</div></div>`).join('')
    : `<div class="node-cells">${nodes.map(cell).join('')}</div>`;
  return `<h3 class="form-sec">${t('line.servers')}</h3>
  <div class="ln-srv">
    <div class="ln-srv-head">
      <div class="seg sm" id="f-scope-seg"><button type="button" data-scope="pick" class="${all ? '' : 'active'}">${esc(t('line.scope.pick'))}</button><button type="button" data-scope="all" class="${all ? 'active' : ''}">${esc(t('line.scope.all'))}</button></div>
      <input type="hidden" id="f-scope" value="${all ? 'all' : 'pick'}">
      ${nodes.length > 8 ? `<input type="search" class="sm ln-srv-q" id="f-srv-q" placeholder="${esc(t('line.srvSearch'))}" aria-label="${esc(t('line.srvSearch'))}">` : ''}
      <span class="grow"></span>
      <span class="muted small" id="f-srv-n"></span>
    </div>
    <div class="node-list" id="f-srv-list">${body}</div>
    <p class="hint xp" id="f-scope-help"></p>
  </div>`;
}
// 服务器选择器的联动:切范围、整组勾、搜索、右上角的已选台数
function bindServers() {
  const seg = document.getElementById('f-scope-seg');
  if (!seg) return;
  const scope = document.getElementById('f-scope'), list = document.getElementById('f-srv-list');
  const cbs = () => [...list.querySelectorAll('.node-cb')];
  const anyDual = (state.nodes || []).some(isDualStack);
  const sync = () => {
    const all = scope.value === 'all';
    seg.querySelectorAll('button').forEach(b => b.classList.toggle('active', b.dataset.scope === scope.value));
    cbs().forEach(c => { c.disabled = all; if (all) c.checked = true; });
    list.querySelectorAll('.node-grp').forEach(grp => {
      const mine = [...grp.querySelectorAll('.node-cb')], n = mine.filter(c => c.checked).length;
      const g = grp.querySelector('.node-gcb'), lab = grp.querySelector('.node-gn');
      g.disabled = all;
      g.checked = n > 0 && n === mine.length;
      g.indeterminate = n > 0 && n < mine.length;
      lab.textContent = `${n} / ${mine.length}`;
    });
    const n = cbs().filter(c => c.checked).length;
    setText('f-srv-n', all ? t('line.scope.allN', { n: cbs().length }) : t('line.scope.pickedN', { n, total: cbs().length }));
    const famHelp = !anyDual ? '' : ' ' + (subByDomain() ? t('line.famPickMulti') + ' ' + t('line.famDnsHelp') : t('line.famHelp'));
    setText('f-scope-help', t(all ? 'line.scope.allHelp' : 'line.scope.pickHelp') + famHelp);
    syncFamily();
    fwUpdateAll();
  };
  seg.addEventListener('click', e => {
    const b = e.target.closest('[data-scope]');
    if (!b || b.dataset.scope === scope.value) return;
    scope.value = b.dataset.scope; // 从「全部」切回「指定」:勾选保持全勾,去掉不要的那几台就行
    sync();
  });
  list.addEventListener('change', e => {
    const el = e.target;
    if (el.classList.contains('node-gcb')) el.closest('.node-grp').querySelectorAll('.node-row:not([hidden]) .node-cb').forEach(c => { c.checked = el.checked; });
    if (el.classList.contains('node-gcb') || el.classList.contains('node-cb')) sync();
    else if (el.classList.contains('node-fam')) { syncV6Tips(); fwUpdateAll(); } // 中转入口「转到哪个地址」跟着这里选的 IP 版本走
  });
  const q = document.getElementById('f-srv-q');
  if (q) q.addEventListener('input', () => {
    const s = q.value.trim().toLowerCase();
    list.querySelectorAll('.node-row').forEach(r => { r.hidden = !!s && !r.textContent.toLowerCase().includes(s); });
    list.querySelectorAll('.node-grp').forEach(g => { g.hidden = ![...g.querySelectorAll('.node-row')].some(r => !r.hidden); });
  });
  sync();
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
  const { ids } = formScope();
  const famSel = id => { const s = document.querySelector(`.node-fam[data-node="${id}"]`); return s && !s.disabled ? s.value : ''; };
  return nodes.filter(n => ids.includes(n.id)).filter(n => lineUsesV6(l, n, famSel(n.id)));
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
  // 最常见的一种「用不了」摆在外面:客户端自己的网络没有 IPv6(实测过:家用宽带、漫游流量卡常常没有,连上就是全断)
  box.innerHTML = `<p class="hint v6-need">${esc(t('line.v6Need'))}</p><details class="adv v6-tips" ${open ? 'open' : ''}><summary>${esc(t('line.v6Tips'))}</summary>
    <p class="hint">${esc(t('line.v6Status'))} ${status}</p>
    <ol class="hint">${[1, 2, 3, 4, 5, 6].map(i => `<li>${esc(t('line.v6Tip' + i))}</li>`).join('')}</ol></details>`;
}
// 没勾的服务器,IP 版本下拉没有意义
function syncFamily() {
  const cbs = [...document.querySelectorAll('.node-cb')];
  document.querySelectorAll('.node-fam').forEach(s => {
    const cb = cbs.find(c => c.value === s.dataset.node);
    s.disabled = !!cb && !cb.checked;
    s.hidden = s.disabled; // 勾上那台之后才出现在它旁边
  });
  syncV6Tips();
}

// ---- 表单 ----
const sel = (id, opts, cur, labels = {}) => `<select id="${id}">${opts.map(x => `<option value="${x}" ${cur === x ? 'selected' : ''}>${esc(labels[x] || x)}</option>`).join('')}</select>`;

// 加密方式与传输:一排按钮选「证书 / Reality / 不加密」,下面一行只出现这种方式要填的东西(证书或握手目标、客户端指纹、传输方式);
// Reality 的密钥没有就自动生成、收在「密钥」里。Shadowsocks 没有 TLS:这里是它必填的加密算法和密码
function dynSection(protocol, tls, tr, o) {
  const spec = PROTOCOLS[protocol] || {};
  if (spec.noTls) {
    if (protocol !== 'shadowsocks') return '';
    return `<h3 class="form-sec">${t('line.tlsMode')}</h3><div class="form-grid cols-4">
      ${field(t('line.method'), sel('f-method', SS_METHODS, o.method || 'aes-256-gcm'))}
      <div class="span-2">${field(t('line.password'), `<div class="row"><input id="f-sspw" value="${esc(o.password || '')}" spellcheck="false"><button type="button" class="btn" data-act="line.genSsPw">${t('line.gen')}</button></div>`)}</div>
    </div>`;
  }
  const mode = tls.mode || spec.tlsDefault;
  const modes = spec.tlsRequired ? ['cert', 'reality'] : ['cert', 'reality', 'none'];
  const r = tls.reality || {};
  const type = tr.type || 'tcp';
  const host = tr.headers && tr.headers.Host ? (Array.isArray(tr.headers.Host) ? tr.headers.Host[0] : tr.headers.Host) : (Array.isArray(tr.host) ? tr.host.join(',') : (tr.host || ''));
  const trFields = spec.transport ? transportFields(type, tr, host) : '';
  const trTypes = ['tcp', 'ws', 'grpc', 'httpupgrade', 'http'];
  return `<h3 class="form-sec">${t('line.tlsMode')}</h3>
    <div class="ln-tls"><div class="seg" id="f-tlsseg">${modes.map(m => `<button type="button" data-tls="${m}" class="${m === mode ? 'active' : ''}">${esc(t('line.tls.' + m))}</button>`).join('')}</div><span class="hint xp" id="f-tls-help">${esc(t('line.tlsHelp.' + mode))}</span></div>
    <input type="hidden" id="f-tlsmode" value="${esc(mode)}">
    <div class="form-grid cols-4 ln-dyn">
      <div id="f-cert-wrap" class="span-2" ${mode === 'cert' ? '' : 'hidden'}>${fieldBox(t('line.cert'), `<input type="hidden" id="f-cert" value="${esc(tls.cert || '')}"><button type="button" class="btn cert-pick-btn" data-act="line.pickCert" id="f-cert-btn" title="${esc(t('line.certHelp'))}"><span id="f-cert-label">${esc(tls.cert ? certLabel(tls.cert) : t('line.certDefault'))}</span><i class="i i-chev"></i></button>`)}</div>
      <div class="span-2 ln-reality" ${mode === 'reality' ? '' : 'hidden'}>${fieldBox(t('line.reality.server'), `<div class="row ln-hs"><input id="f-hs" value="${esc(r.handshake_server || 'www.microsoft.com')}" spellcheck="false" autocomplete="off" title="${esc(t('line.reality.serverHelp'))}"><input id="f-hsport" type="number" min="1" max="65535" value="${r.handshake_port || 443}" aria-label="${esc(t('line.reality.port'))}" title="${esc(t('line.reality.port'))}"><button type="button" class="btn" data-act="line.realityCheck">${t('line.reality.check')}</button><button type="button" class="btn" data-act="line.realityPick">${t('line.reality.pick')}</button></div><div id="f-hs-res" class="hint" hidden></div>`)}</div>
      <div id="f-fp-wrap" ${mode === 'none' ? 'hidden' : ''}>${field(t('line.fp'), sel('f-fp', ['', ...FPS], tls.fingerprint || '', { '': t('common.default') }))}</div>
      ${spec.transport ? field(t('line.transport.type'), `<select id="f-trtype" title="${esc(t('line.transportHelp'))}">${trTypes.map(x => `<option value="${x}" ${type === x ? 'selected' : ''}>${esc(x === 'tcp' ? t('line.transport.tcp') : x)}</option>`).join('')}</select>`) : ''}
      ${spec.transport ? `<div id="f-tr-fields" class="form-grid cols-2 span-2" ${trFields ? '' : 'hidden'}>${trFields}</div>` : ''}
      <details class="adv full ln-reality" ${mode === 'reality' ? '' : 'hidden'}><summary>${esc(t('line.reality.keys'))}</summary><div class="form-grid cols-4">
        <div class="span-2">${field(t('line.reality.private'), `<div class="row"><input id="f-priv" value="${esc(r.private_key || '')}" spellcheck="false" autocomplete="off"><button type="button" class="btn" data-act="line.genReality">${t('line.reality.gen')}</button></div>`)}</div>
        <div class="span-2">${field(t('line.reality.public'), `<input id="f-pub" value="${esc(r.public_key || '')}" readonly>`)}</div>
        <div class="span-2">${field(t('line.reality.shortIds'), `<div class="row"><input id="f-sids" value="${esc((r.short_ids || []).join(','))}" spellcheck="false" autocomplete="off"><button type="button" class="btn" data-act="line.genShortId">${t('line.gen')}</button></div>`)}</div>
      </div></details>
    </div>`;
}
// 切加密方式:只露出这种方式的栏;切到 Reality 还没有密钥就现生成一对
function setTlsMode(mode) {
  document.getElementById('f-tlsmode').value = mode;
  document.querySelectorAll('#f-tlsseg button').forEach(b => b.classList.toggle('active', b.dataset.tls === mode));
  document.querySelectorAll('#f-dyn .ln-reality').forEach(el => { el.hidden = mode !== 'reality'; });
  document.getElementById('f-cert-wrap').hidden = mode !== 'cert';
  document.getElementById('f-fp-wrap').hidden = mode === 'none';
  setText('f-tls-help', t('line.tlsHelp.' + mode));
  if (mode === 'reality' && !fv('f-priv').trim()) genReality().catch(e => toast(e.message, 'err'));
}
async function genReality() {
  const k = await get('keygen?type=reality');
  if (!document.getElementById('f-priv')) return; // 等结果的时候表单关了、或换了协议
  document.getElementById('f-priv').value = k.privateKey || k.private_key || '';
  document.getElementById('f-pub').value = k.publicKey || k.public_key || '';
  document.getElementById('f-priv').dispatchEvent(new Event('m-ui:keypair')); // 新的一对:以它为准(见 bindRealityPub)
  if (!fv('f-sids').trim()) {
    const s = await get('keygen?type=shortid');
    if (document.getElementById('f-sids')) document.getElementById('f-sids').value = s.shortId || s.short_id || '';
  }
}

// 传输方式选了 ws / grpc 之类才有要填的东西(TCP 没有)
function transportFields(type, tr, host) {
  switch (type) {
    case 'ws': case 'httpupgrade': case 'http':
      return field(t('line.transport.path'), `<input id="f-trpath" value="${esc(tr.path || '/')}">`) + field(t('line.transport.host'), `<input id="f-trhost" value="${esc(host || '')}" placeholder="cdn.example.com">`)
        // 实测:Xray 26 的 HTTPUpgrade 客户端发的是真的 WebSocket 握手头,这边的服务端按规范拒绝,连不上;mihomo / sing-box 系正常
        + (type === 'httpupgrade' ? `<p class="hint full tr-note">${esc(t('line.huXray'))}</p>` : '');
    case 'grpc':
      return field(t('line.transport.service'), `<input id="f-trsvc" value="${esc(tr.service_name || 'grpc')}">`);
  }
  return '';
}

// 协议自己的可选参数,收在「更多设置」里(Shadowsocks 的算法和密码是必填,在上面的加密方式里)
function advProto(protocol, o) {
  let h = '';
  switch (protocol) {
    case 'vless': h = `<div class="full">${check('f-vision', t('line.vision'), o.vision !== false)}</div>`; break;
    case 'hysteria2':
      h = `<div class="full">${field(t('line.hop'), `<input id="f-hop" value="${esc(o.port_hopping || '')}" placeholder="20000-30000">`, t('line.hopHelp'))}</div>` +
        `<div class="full">${field(t('line.masq'), `<input id="f-masq" value="${esc(masqOf(o))}" placeholder="https://www.bing.com">`, t('line.masqHelp'))}</div>` +
        field(t('line.upMbps'), `<input id="f-up" type="number" min="0" value="${o.up_mbps || 0}">`) +
        field(t('line.downMbps'), `<input id="f-down" type="number" min="0" value="${o.down_mbps || 0}">`) +
        `<div class="full">${field(t('line.obfs'), `<input id="f-obfs" value="${esc((o.obfs && o.obfs.password) || '')}">`)}</div>`; break;
    case 'tuic': h = field(t('line.cc'), sel('f-cc', ['cubic', 'bbr', 'new_reno'], o.congestion_control || 'cubic')); break;
    case 'anytls':
      h = `<div class="full">${field(t('line.padding'), `<textarea id="f-padding" spellcheck="false">${esc((o.padding_scheme || []).join('\n'))}</textarea>`)}</div>`; break;
  }
  return h ? `<div class="form-grid cols-2 ln-adv-proto">${h}</div>` : '';
}

function extraOf(protocol, o) {
  const known = new Set(OPT_KEYS[protocol] || []);
  const extra = {};
  Object.entries(o).forEach(([k, v]) => { if (!known.has(k)) extra[k] = v; });
  return Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
}

// 「更多设置」那一行右边:已经设了哪几样;一样都没设时写这里面有什么(端口跳跃这类功能收起来了也找得到)
function advSummary() {
  const el = document.getElementById('f-adv-sum');
  if (!el) return;
  const p = fv('f-protocol'), set = [];
  const has = id => { const x = document.getElementById(id); const v = x ? String(x.value || '').trim() : ''; return v !== '' && v !== '0'; };
  if (p === 'hysteria2') {
    if (has('f-hop')) set.push(t('line.hop'));
    if (has('f-masq')) set.push(t('line.masq'));
    if (has('f-up') || has('f-down')) set.push(t('line.adv.bw'));
    if (has('f-obfs')) set.push(t('line.adv.obfs'));
  }
  if (p === 'anytls' && has('f-padding')) set.push('padding');
  if (has('f-extra')) set.push(t('line.adv.extra'));
  if (has('f-addrs')) set.push(t('line.adv.addrs'));
  el.textContent = set.length ? set.join(' · ') : t(p === 'hysteria2' ? 'line.adv.hasHy2' : 'line.adv.has');
  el.classList.toggle('on', set.length > 0);
}

function renderDynamic(l) {
  const protocol = fv('f-protocol');
  const o = parseJ(l.options), tls = parseJ(l.tls), tr = parseJ(l.transport);
  document.getElementById('f-dyn').innerHTML = dynSection(protocol, tls, tr, o);
  document.getElementById('f-adv-proto').innerHTML = advProto(protocol, o);
  document.getElementById('f-extra').value = extraOf(protocol, o);
  const seg = document.getElementById('f-tlsseg');
  if (seg) seg.addEventListener('click', e => { const b = e.target.closest('[data-tls]'); if (b && b.dataset.tls !== fv('f-tlsmode')) setTlsMode(b.dataset.tls); });
  const tt = document.getElementById('f-trtype');
  if (tt) tt.addEventListener('change', e => {
    const box = document.getElementById('f-tr-fields'), h = transportFields(e.target.value, {}, '');
    box.innerHTML = h;
    box.hidden = !h;
  });
  // 新建线路默认就是 Reality:密钥现生成,不用人去点「生成密钥对」
  if (document.getElementById('f-priv') && fv('f-tlsmode') === 'reality' && !fv('f-priv').trim()) genReality().catch(e => toast(e.message, 'err'));
  bindRealityPub();
  advSummary();
}

// 手改私钥时公钥框跟着清空、写明「保存时按私钥算出」:公钥由后端按私钥算,框里留着旧公钥会让人以为还是那一对(审计 E3)。
// 点「生成密钥对」(genReality)是一对一起填,不走这里。
function bindRealityPub() {
  const priv = document.getElementById('f-priv'), pub = document.getElementById('f-pub');
  if (!priv || !pub || priv.dataset.pubBound) return;
  priv.dataset.pubBound = '1';
  let pair = { priv: priv.value.trim(), pub: pub.value };
  priv.addEventListener('input', () => {
    const changed = priv.value.trim() !== pair.priv;
    pub.value = changed ? '' : pair.pub;
    pub.placeholder = changed ? t('line.reality.pubAuto') : '';
  });
  priv.addEventListener('m-ui:keypair', () => { pair = { priv: priv.value.trim(), pub: pub.value }; pub.placeholder = ''; });
}

// 展开某个收起的分区再报错:错在收起来的地方时,人得看得见是哪一栏
function failIn(accId, msg) {
  const a = document.getElementById(accId);
  if (a) a.open = true;
  throw new Error(msg);
}

function readForm(id) {
  const protocol = fv('f-protocol');
  const spec = PROTOCOLS[protocol] || {};
  // 部署到哪些服务器:「全部服务器」存空(以后新加的也算);「指定服务器」至少勾一台
  const sc = formScope();
  if (document.getElementById('f-scope') && !sc.all && !sc.ids.length) throw new Error(t('line.scope.none'));
  const body = {
    name: fv('f-name').trim(), protocol, port: Number(fv('f-port')),
    upstreamId: Number(fv('f-upstream')), enabled: fchk('f-enabled'),
    routeRules: rrRead(),
    forwards: fwRead(),
  };
  body.nodeIds = sc.all ? [] : sc.ids;
  body.nodeFamily = {};
  document.querySelectorAll('.node-fam').forEach(s => { if (s.value && !s.disabled) body.nodeFamily[s.dataset.node] = s.value; });
  const nodeOpts = fwOnlyOpts(body.forwards);
  if (nodeOpts) body.nodeOpts = nodeOpts; // 没动过就不带:后端沿用库里的
  // TLS
  if (!spec.noTls) {
    const tls = { mode: fv('f-tlsmode') };
    if (tls.mode !== 'none' && fv('f-fp')) tls.fingerprint = fv('f-fp');
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
  let o;
  try { o = JSON.parse(fv('f-extra').trim() || '{}'); } catch { failIn('f-adv-acc', t('line.extraBad')); }
  if (!o || typeof o !== 'object' || Array.isArray(o)) failIn('f-adv-acc', t('line.extraBad'));
  switch (protocol) {
    case 'vless': o.vision = fchk('f-vision'); break;
    case 'hysteria2':
      if (Number(fv('f-up')) > 0) o.up_mbps = Number(fv('f-up')); else delete o.up_mbps;
      if (Number(fv('f-down')) > 0) o.down_mbps = Number(fv('f-down')); else delete o.down_mbps;
      if (fv('f-obfs')) o.obfs = { type: 'salamander', password: fv('f-obfs') }; else delete o.obfs;
      if (fv('f-hop').trim()) o.port_hopping = fv('f-hop').trim(); else delete o.port_hopping;
      { // 伪装站点:别人探测这个端口时看到的是这个网站,不是一个报错的代理
        const m = fv('f-masq').trim();
        if (m && !/^https?:\/\/[^\s/]+/i.test(m)) failIn('f-adv-acc', t('line.masqBad'));
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
  { id: 'hy2', label: 'Hysteria2', hint: 'line.preset.hy2', protocol: 'hysteria2', tls: { mode: 'cert' } },
  { id: 'anytls', label: 'AnyTLS', hint: 'line.preset.anytls', protocol: 'anytls', tls: { mode: 'cert' } },
  { id: 'reality', label: 'VLESS + Reality', hint: 'line.preset.reality', protocol: 'vless', tls: { mode: 'reality' }, options: { vision: true } },
  { id: 'trojan', label: 'Trojan', hint: 'line.preset.trojan', protocol: 'trojan', tls: { mode: 'cert' } },
  { id: 'ss2022', label: 'Shadowsocks 2022', hint: 'line.preset.ss2022', protocol: 'shadowsocks', tls: { mode: 'none' }, options: { method: '2022-blake3-aes-128-gcm' } },
  { id: 'vmess-ws', label: 'VMess + WS', hint: 'line.preset.vmessWs', protocol: 'vmess', tls: { mode: 'none' }, transport: { type: 'ws', path: '/ws' } },
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

// 编辑框的版式:通栏、从上到下 —— 基本信息一行(名称、协议、端口、上游)→ 加密方式 → 部署到服务器(多列网格)→ 启用 →
// 三个可展开的分区(中转入口、分流规则、更多设置):不用的就是收着的一行,行尾写着现在的状态。
// 不分左右两栏:服务器多少、加密方式要填的东西多少都不一定,两栏总有一边空着。
async function editLine(id, cloneFrom, preset) {
  await load('nodes').catch(() => {}); // 服务器列表可能刚在别的页面改过,"部署到服务器"要用最新的
  const src = cloneFrom || (id ? state.lines.find(x => x.id === id) : null);
  const l = src ? { ...src } : (preset || { protocol: 'vless', enabled: true, options: {}, upstreamId: 0 });
  formIsNew = !id && !cloneFrom;
  if (cloneFrom) { l.name = t('line.cloneOf', { name: src.name }); l.port = ''; l.forwards = null; l.nodeOpts = null; }
  if (!l.port) l.port = await suggestPort(); // 新建/克隆:给个可用端口做默认值,可改
  const rrInit = rrOf(l), fwInit = fwOf(l);
  const multi = (state.nodes || []).length > 1;
  const acc = (key, title, body, open) => `<details class="acc" id="f-${key}-acc" ${open ? 'open' : ''}>
      <summary><span class="acc-t">${esc(title)}</span><span class="acc-s" id="f-${key}-sum"></span><i class="i i-chev"></i></summary>
      <div class="acc-b">${body}</div></details>`;
  const adv = `
    <div id="f-adv-proto"></div>
    <div class="form-grid cols-2">
      ${field(t('line.advanced'), `<textarea id="f-extra" spellcheck="false"></textarea>`)}
      ${field(t('line.addrs'), `<textarea id="f-addrs" spellcheck="false" placeholder='[{"server":"1.2.3.4","server_port":443,"remark":"-备用"}]'>${esc(l.addrs ? (typeof l.addrs === 'string' ? l.addrs : JSON.stringify(l.addrs, null, 2)) : '')}</textarea>`, t('line.addrsHelp'))}
    </div>`;
  openModal(id ? t('line.edit') : t('line.add'), `<div class="ln-form">
    <div class="form-grid ln-head">
      ${field(t('common.name'), `<input id="f-name" value="${esc(l.name || '')}" placeholder="${esc(t('line.namePh'))}">`)}
      ${field(t('line.protocol'), sel('f-protocol', Object.keys(PROTOCOLS), l.protocol, PROTO_LABEL))}
      ${field(t('common.port'), `<input id="f-port" type="number" min="1" max="65535" value="${l.port || ''}" title="${esc(t('line.portHelp'))}">`)}
      ${field(t('line.upstream'), `<select id="f-upstream"><option value="0">${esc(t('line.direct'))}</option>${state.upstreams.map(u => `<option value="${u.id}" ${l.upstreamId === u.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')}</select>`)}
    </div>
    <div id="f-dyn"></div>
    ${serversField(l, formIsNew)}
    <div id="f-v6tips" hidden></div>
    <div class="ln-flags">${switchRow('f-enabled', t('common.enabled'), l.enabled !== false, t('line.enabledHelp'))}${id ? '' : switchRow('f-assign', t('line.assignAll'), true, t('line.assignAllHelp'))}</div>
    <div class="acc-list">${acc('fw', t('line.fw.on'), fwSection(), fwInit.length > 0)}${acc('rr', t('line.rr.on'), rrSection(), rrInit.length > 0)}${acc('adv', t('line.adv.title'), adv, false)}</div>
  </div>`, async () => {
    const body = readForm(id);
    const addrs = fv('f-addrs').trim();
    if (addrs) {
      try { body.addrs = JSON.parse(addrs); } catch { failIn('f-adv-acc', t('line.addrsBad')); }
    }
    if (!id) body.assignAll = fchk('f-assign');
    const saved = id ? await put('lines/' + id, body) : await post('lines', body);
    await load('lines', 'status', 'users');
    renderRows();
    toast(id ? t('line.updated') : t('line.created'), 'ok');
    scopeWidenedNotice(saved && saved.scopeWidened);
    if (id && ldAlive() && ld.id === id) showLineDetail(id); // 从详情里点的编辑:保存后详情跟着刷新
  }, multi ? { size: 'xl' } : { wide: true });
  renderDynamic(l);
  bindServers();
  rrBind(rrInit);
  fwBind(fwInit, id, l);
  const v6Later = debounce(syncV6Tips, 400);
  document.getElementById('f-protocol').addEventListener('change', () => { renderDynamic({ options: {}, tls: {}, transport: {} }); syncV6Tips(); fwUpdateAll(); }); // 转 TCP 还是 UDP 跟着协议变
  ['f-name', 'f-port'].forEach(k => document.getElementById(k).addEventListener('input', () => { fwUpdateAll(); v6Later(); })); // 节点名预览、转到的端口跟着变
  document.getElementById('f-addrs').addEventListener('input', v6Later);
  document.getElementById('f-adv-acc').addEventListener('input', debounce(advSummary, 200));
  syncFamily();
  if (!reachLoaded()) loadReach().then(syncV6Tips); // 新开页面直接点"添加"时还没拿过检测结果
}

// ---- 中转入口:客户端经别的服务器(集群中转)或外部中转(自己跑的 realm、买的中转)连到这条线路,加密数据原样转过去 ----
// 后端:保存校验 web/line_forwards.go、中转服务器上的转发 render/forward.go、订阅里的节点 sub/link.go forwardAddrs。
// 默认收起:勾上「中转入口」才出来(和分流规则一样,取消勾选并保存会清掉)。一条一块,读起来就是数据怎么走:
//   第一行:入口 [中转服务器 ▾ / 外部中转 + 地址] : [端口] → 出口 [服务器] 转到的地址:线路端口  [暂停] [删除]
//   第二行:订阅里这个节点叫什么(中转那一段可改),以及要做的事(集群中转:那台放行哪种流量的哪个端口;外部中转:在中转上转到哪)
// 端口在中转服务器上撞了(那台的面板 / 订阅端口、部署在那台的线路、别的中转入口、同一台的另一行)当场标红;
// 挑了中转服务器没改过端口的,自动给一个那台上空着的端口(先试线路自己的端口,按原端口转发)。
let fwEditId = 0;
function fwOf(l) { const v = parseJ(l.forwards); return Array.isArray(v) ? v : []; }
const fwNode = id => (state.nodes || []).find(n => n.id === id);
const fwNodeName = id => (fwNode(id) || {}).name || '#' + id;
// 按表单当前勾的服务器:这条线路部署在哪几台(全不勾 = 全部)
function fwDeployed() { return formScope().ids; }
const fwOnNode = (l, id) => { const ids = lineNodeIds(l); return !ids.length || ids.includes(id); };

// 列表「服务器」一格末尾的小标记:有几个中转入口(暂停的不算),悬停看明细
function fwBadge(l) {
  const fs = fwOf(l).filter(f => !f.off);
  if (!fs.length) return '';
  const tip = fs.map(f => `${f.via ? fwNodeName(f.via) : f.host}:${f.port} → ${fwNodeName(f.to)}${f.name ? ' (' + f.name + ')' : ''}`).join('\n');
  return ` <span class="badge fw-badge" title="${esc(t('line.fw.title') + '\n' + tip)}">${esc(t('line.fw.badge', { n: fs.length }))}</span>`;
}

// 转哪种流量(和后端 render.ForwardNetwork 一致;界面上选不出 QUIC 传输,只看协议)
function fwNet() {
  const p = fv('f-protocol');
  return p === 'hysteria2' || p === 'tuic' ? 'udp' : p === 'shadowsocks' ? 'both' : 'tcp';
}
// 出口服务器转到哪个地址(显示用,挑法和后端 render.ForwardTarget 一样,改那边要一起改):
//   这条线路在出口那台选了 IPv6(上面「部署到服务器」里选的,没选跟那台服务器的设置)就转到它的 IPv6;
//   没指定、转的只是 UDP、中转机自己在订阅里按 IPv6 用的,也先走 IPv6;其余中转有 IPv4(或不知道)先用出口的 IPv4。
function fwExitAddr(exitId, viaId) {
  const e = fwNode(exitId) || {}, v = fwNode(viaId) || {};
  const four = x => (x && !String(x).includes(':') ? x : ''), six = x => (x && String(x).includes(':') ? x : '');
  const e4 = four(e.publicIp), e6 = six(e.publicIp6) || six(e.publicIp), s4 = four(v.publicIp), s6 = six(v.publicIp6) || six(v.publicIp);
  // 这台在订阅里给的是不是 IPv6:连接地址填的是 IPv6;没填的话是「自动地址用 IPv6」(填了 IPv4 / 域名时那个设置不生效)
  const usesV6 = n => { const a = String(n.addr || '').trim().replace(/^\[|\]$/g, ''); return a ? /^[0-9a-f:.]+$/i.test(a) && a.includes(':') : n.addrFamily === 'v6'; };
  const sel = document.querySelector(`.node-fam[data-node="${exitId}"]`);
  const fam = (sel && !sel.disabled && sel.value) || (usesV6(e) ? 'v6' : '');
  if ((fam === 'v6' || (!fam && fwNet() === 'udp' && usesV6(v))) && e6 && (s6 || !s4)) return e6;
  if (e4 && (s4 || !s6)) return e4;
  return e6 || e4 || e.addr || e.domain || fwNodeName(exitId);
}
// 一台服务器自己监听的服务端口(和后端 servicePortsOf 一致:本机按设置,副机按上报,没上报按默认)
function fwServicePorts(n) {
  const label = { '面板': t('line.fw.svcPanel'), '订阅': t('line.fw.svcSub'), '代理面板': t('line.fw.svcReseller') };
  if (n.isLocal) {
    const st = state.settings || {};
    const out = [['面板', st.webPort || 2053], ['订阅', st.subPort || 2056]];
    if (String(st.resellerEnabled) !== 'false') out.push(['代理面板', st.resellerPort || 2054]);
    return out.map(([k, p]) => ({ label: label[k], port: Number(p) }));
  }
  let list = parseJ(n.servicePorts);
  if (!Array.isArray(list) || !list.length) list = [{ label: '面板', port: 2053 }, { label: '订阅', port: 2056 }];
  return list.map(p => ({ label: label[p.label] || p.label, port: Number(p.port) }));
}
// 端口在中转服务器 via 上撞了什么(没撞返回空);row 是正在查的这一行(和别的行比)
function fwPortProblem(via, port, row) {
  const n = fwNode(via);
  if (!n) return '';
  if (!(port >= 1 && port <= 65535)) return t('line.fw.e.port');
  const svc = fwServicePorts(n).find(p => p.port === port);
  if (svc) return t('line.fw.e.svc', { port, via: n.name, what: svc.label });
  if (fwDeployed().includes(via) && Number(fv('f-port')) === port) return t('line.fw.e.self', { port, via: n.name });
  const other = (state.lines || []).find(l => l.id !== fwEditId && l.port === port && fwOnNode(l, via));
  if (other) return t('line.fw.e.line', { port, via: n.name, line: other.name });
  const ofw = (state.lines || []).find(l => l.id !== fwEditId && fwOf(l).some(f => f.via === via && f.port === port));
  if (ofw) return t('line.fw.e.fw', { port, via: n.name, line: ofw.name });
  const rows = [...document.querySelectorAll('#f-fw-list .fw-item')];
  const i = rows.findIndex(r => r !== row && r.querySelector('.fw-via').value === String(via) && Number(r.querySelector('.fw-port').value) === port);
  if (i >= 0 && rows.indexOf(row) > i) return t('line.fw.e.dup', { n: i + 1, via: n.name });
  return '';
}
// 中转服务器上空着的端口:先试线路自己的端口(按原端口转发),再从 30001、40001 往后找
function fwFreePort(via, row) {
  const own = Number(fv('f-port')) || 0;
  const cands = [own];
  for (let p = 30001; p <= 30300; p++) cands.push(p);
  for (let p = 40001; p <= 40300; p++) cands.push(p);
  return cands.find(p => p >= 1 && p <= 65535 && !fwBusy.has(via + ':' + p) && !fwPortProblem(via, p, row)) || '';
}

// 分区最上面一张小图:用户 → 中转 → 出口 → 网站,一眼看懂这是干什么的
function fwSection() {
  const node = (k, cls = '') => `<span class="fw-flow-node${cls}">${esc(t(k))}</span>`;
  const arrow = '<i class="i i-arrow fw-flow-arrow"></i>';
  return `<div class="fw-box">
    <div class="fw-flow" aria-hidden="true">${node('line.fw.dUser')}${arrow}${node('line.fw.dRelay', ' relay')}${arrow}${node('line.fw.dExit', ' exit')}${arrow}${node('line.fw.dSite')}</div>
    <p class="hint acc-lead xp">${esc(t('line.fw.lead'))}</p>
    <div id="f-fw-list"></div>
    <button type="button" class="rr-add" id="f-fw-add">${esc(t('line.fw.add'))}</button>
    <label class="field check fw-only" id="f-fw-only-wrap" hidden><input type="checkbox" id="f-fw-only"><span>${esc(t('line.fw.only'))}</span><span class="help xp">${esc(t('line.fw.onlyHelp'))}</span></label>
    <p class="hint fw-foot xp" id="f-fw-foot" hidden>${esc(t('line.fw.foot'))}</p>
  </div>`;
}

// 一条中转入口的骨架;地址、节点名、要做的事、问题由 fwUpdate 按当前表单填。
// 「谁来转」:面板来转(在那台服务器上自动开转发)/ 这台上已有转发(realm、gost 之类已经占着这个端口,面板不去碰,只把入口写进订阅)
function fwItemHTML(f, i) {
  const nodes = state.nodes || [];
  const ext = !f.via;
  return `<div class="fw-item${f.off ? ' paused' : ''}" data-fid="${Number(f.id) || 0}" data-auto="${f.auto ? 1 : 0}" data-to="${Number(f.to) || 0}">
    <div class="fw-path">
      <span class="fw-no">${i + 1}</span>
      <span class="fw-lab">${esc(t('line.fw.in'))}</span>
      <span class="fw-from">
        <select class="fw-via" aria-label="${esc(t('line.fw.in'))}">${nodes.map(n => `<option value="${n.id}" ${!ext && n.id === f.via ? 'selected' : ''}>${esc(n.name)}${n.enabled === false ? ' · ' + esc(t('common.disabled')) : ''}</option>`).join('')}<option value="ext" ${ext ? 'selected' : ''}>${esc(t('line.fw.ext'))}</option></select>
        <input class="fw-host" value="${esc(f.host || '')}" placeholder="${esc(t('line.fw.hostPh'))}" aria-label="${esc(t('line.fw.hostPh'))}" spellcheck="false" autocomplete="off" ${ext ? '' : 'hidden'}>
      </span>
      <span class="fw-lab">${esc(t('line.fw.portOf'))}</span>
      <input class="fw-port" type="number" min="1" max="65535" value="${f.port || ''}" placeholder="${esc(t('line.fw.portPh'))}" aria-label="${esc(t('line.fw.portPh'))}">
      <i class="i i-arrow fw-arrow" aria-hidden="true"></i>
      <span class="fw-lab">${esc(t('line.fw.out'))}</span>
      <span class="fw-exit"></span>
      <span class="fw-dest-wrap"><button type="button" class="fw-dest" title="${esc(t('line.fw.targetHelp'))}"></button><span class="fw-paren" hidden>(</span><input class="fw-target" value="${esc(f.target || '')}" spellcheck="false" autocomplete="off" title="${esc(t('line.fw.targetHelp'))}" ${f.target && !f.manual ? '' : 'hidden'}><span class="fw-dest-port"></span></span>
      <span class="fw-tail">
        <select class="sm fw-mode" aria-label="${esc(t('line.fw.mode'))}" title="${esc(t('line.fw.modeHelp'))}" ${ext ? 'hidden' : ''}><option value="auto">${esc(t('line.fw.modeAuto'))}</option><option value="manual" ${f.manual ? 'selected' : ''}>${esc(t('line.fw.modeManual'))}</option></select>
        <label class="fw-pause" title="${esc(t('line.fw.offHelp'))}"><input type="checkbox" class="fw-off" ${f.off ? 'checked' : ''}> ${esc(t('line.fw.off'))}</label>
        <button type="button" class="btn sm ghost icon danger fw-del" title="${esc(t('common.delete'))}" aria-label="${esc(t('common.delete'))}"><i class="i i-x"></i></button>
      </span>
    </div>
    <div class="fw-result">
      <span class="fw-nn"><span class="lab">${esc(t('line.fw.nodeName'))}</span><b class="fw-nn-pre"></b><input class="fw-name" maxlength="40" value="${esc(f.name || '')}" aria-label="${esc(t('line.fw.nodeName'))}"><b class="fw-nn-ratio"></b></span>
      <span class="fw-todo"></span>
    </div>
    <div class="fw-note" hidden></div>
  </div>`;
}

// 现问中转服务器:这个端口现在有没有被别的程序占着(结果记在这一行上,回来后重画这一行的提示)。
// 面板来转的:被占 = 开不出来,给出路(那就是我做的转发 → 改成「已有转发」/ 换一个端口);
// 已有转发的:没人在听 = 提醒确认转发已经启动。问不到(副机离线、老版本)不下结论。
const fwBusy = new Set(); // 「服务器 id:端口」:问出来被别的程序占着的,自动挑端口时绕开
function fwProbe(row) {
  const v = row.querySelector('.fw-via').value, port = Number(row.querySelector('.fw-port').value) || 0;
  if (v === 'ext' || !(port >= 1 && port <= 65535)) { row.__probe = null; return; }
  const net = fwNet() === 'both' ? '' : fwNet();
  const key = `${v}:${port}:${net}`;
  if (row.__probe && row.__probe.key === key) return;
  row.__probe = { key, state: 'pending', by: '' };
  clearTimeout(row.__probeTimer);
  row.__probeTimer = setTimeout(async () => {
    let r = null;
    try { r = await post(`nodes/${v}/port-check`, { port, net }); } catch { r = null; }
    if (!row.isConnected || !row.__probe || row.__probe.key !== key) return;
    row.__probe.state = !r || !r.asked ? 'unknown' : r.free ? 'free' : r.self ? 'self' : 'busy';
    row.__probe.by = (r && r.by) || '';
    if (row.__probe.state === 'busy') fwBusy.add(`${v}:${port}`);
    fwUpdate(row);
  }, 600);
}

// 按表单当前的线路名、端口、协议、部署范围,把一条中转入口的显示和检查做一遍;返回问题(红的)
function fwUpdate(row) {
  const viaSel = row.querySelector('.fw-via'), modeSel = row.querySelector('.fw-mode');
  const ext = viaSel.value === 'ext', via = ext ? 0 : Number(viaSel.value) || 0;
  const manual = !ext && modeSel.value === 'manual';
  const fixedDest = ext || manual; // 转到哪里不归面板管:只是告诉你该转到哪
  const host = row.querySelector('.fw-host'), target = row.querySelector('.fw-target'), dest = row.querySelector('.fw-dest');
  host.hidden = !ext;
  modeSel.hidden = ext;
  const paused = row.querySelector('.fw-off').checked;
  row.classList.toggle('paused', paused);
  // 出口:部署在几台就能选几台(不能是中转自己);只有一台就是那台
  const deployed = fwDeployed().filter(id => ext || id !== via);
  const cell = row.querySelector('.fw-exit');
  const curSel = cell.querySelector('select');
  let to = Number(curSel ? curSel.value : row.dataset.to) || 0;
  if (!deployed.includes(to)) to = deployed.length === 1 ? deployed[0] : 0;
  row.dataset.to = to;
  const want = deployed.length > 1
    ? `<select class="fw-to" aria-label="${esc(t('line.fw.out'))}">${to ? '' : `<option value="0">${esc(t('line.fw.toPick'))}</option>`}${deployed.map(id => `<option value="${id}" ${id === to ? 'selected' : ''}>${esc(fwNodeName(id))}</option>`).join('')}</select>`
    : (to ? `<b class="fw-exit-name" title="${esc(fwNodeName(to))}">${esc(fwNodeName(to))}</b>` : `<span class="muted">${esc(t('line.fw.noExit'))}</span>`);
  if (cell.__html !== want) { cell.innerHTML = want; cell.__html = want; }
  // 转到的地址(放在括号里):面板来转的可点开改(内网地址);别的两种只是告诉你该转到哪
  const linePort = Number(fv('f-port')) || 0;
  const addr = to ? fwExitAddr(to, via) : '';
  const shown = addr.includes(':') ? '[' + addr + ']' : addr;
  // 平时整段「(地址:端口)」是一个按钮(能改的后面带一支笔);点开后换成「( [地址输入框] :端口)」
  if (fixedDest) target.hidden = true;
  const editing = !fixedDest && !target.hidden;
  dest.innerHTML = to ? esc('(' + shown + ':' + (linePort || '?') + ')') + (fixedDest ? '' : ' <i class="i i-pencil"></i>') : '';
  dest.disabled = fixedDest;
  dest.hidden = editing || !to;
  target.placeholder = addr;
  row.querySelector('.fw-paren').hidden = !editing;
  const dp = row.querySelector('.fw-dest-port');
  dp.hidden = !editing;
  dp.textContent = ':' + (linePort || '?') + ')';
  // 订阅里的节点名:线路名 + 出口后缀(两台以上服务器时)+ "-" + 中转名 + 倍率(和后端 forwardAddrs 一样)
  const exitNode = fwNode(to) || {};
  const multi = (state.nodes || []).filter(n => n.enabled !== false).length > 1;
  const ratio = exitNode.ratio > 0 && exitNode.ratio !== 1 ? ' x' + exitNode.ratio : '';
  row.querySelector('.fw-nn-pre').textContent = (fv('f-name').trim() || t('line.fw.lineName')) + (multi && to ? '-' + exitNode.name : '') + '-';
  row.querySelector('.fw-nn-ratio').textContent = ratio;
  const port = Number(row.querySelector('.fw-port').value) || 0;
  row.querySelector('.fw-name').placeholder = ext ? (host.value.trim() || t('line.fw.relayName')) : fwNodeName(via);
  // 要做的事
  const net = t('line.fw.net.' + fwNet());
  const destText = (addr.includes(':') ? '[' + addr + ']' : addr) + ':' + (linePort || '?');
  row.querySelector('.fw-todo').textContent = !to ? '' : ext
    ? t('line.fw.todoExt', { port: port || '?', net, dest: destText })
    : manual ? t('line.fw.todoManual', { via: fwNodeName(via), port: port || '?', net, dest: destText })
      : t('line.fw.todoVia', { via: fwNodeName(via), port: port || '?', net });
  // 问题:红的保存时拦下,黄的只提示
  let bad = '', warn = '';
  if (!to) bad = t('line.fw.e.to');
  else if (ext && !host.value.trim()) bad = t('line.fw.e.host');
  else if (ext && !(port >= 1 && port <= 65535)) bad = t('line.fw.e.port');
  else if (!ext) bad = fwPortProblem(via, port, row);
  if (!bad && !ext && fwNode(via) && fwNode(via).enabled === false) warn = t('line.fw.offNode');
  else if (!bad && !ext && fwNode(via) && (fwNode(via).status || {}).versionMismatch) warn = t('line.fw.old');
  const note = row.querySelector('.fw-note');
  // 端口占用:没有别的问题、没暂停时才去问那台服务器
  let probeHTML = '', probeOk = false;
  if (!bad && !warn && !ext && !paused) {
    fwProbe(row);
    const pr = row.__probe;
    const who = pr && pr.by ? t('line.fw.busyBy', { by: pr.by }) : '';
    if (pr && pr.state === 'busy' && !manual) {
      probeHTML = esc(t('line.fw.busy', { port, via: fwNodeName(via), who })) +
        ` <button type="button" class="link" data-fw-fix="manual">${esc(t('line.fw.fixManual'))}</button> <button type="button" class="link" data-fw-fix="port">${esc(t('line.fw.fixPort'))}</button>`;
    } else if (pr && pr.state === 'free' && manual) {
      probeHTML = esc(t('line.fw.idle', { port, via: fwNodeName(via) }));
    } else if (pr && pr.state === 'busy' && manual) {
      probeHTML = esc(t('line.fw.listening', { port, via: fwNodeName(via), who }));
      probeOk = true;
    }
  }
  if (bad || warn) note.textContent = bad || warn; else note.innerHTML = probeHTML;
  note.hidden = !(bad || warn || probeHTML);
  note.classList.toggle('bad', !!bad);
  note.classList.toggle('ok', !bad && !warn && probeOk);
  return bad;
}
const fwUpdateAll = () => { document.querySelectorAll('#f-fw-list .fw-item').forEach(fwUpdate); fwSummary(); };

// 分区那一行右边的状态,以及有入口时才出现的「只给中转入口」和说明
function fwSummary() {
  const rows = [...document.querySelectorAll('#f-fw-list .fw-item')];
  const off = rows.filter(r => r.querySelector('.fw-off').checked).length;
  const el = document.getElementById('f-fw-sum');
  if (el) {
    el.textContent = !rows.length ? t('line.acc.off') : off ? t('line.fw.countOff', { n: rows.length, off }) : t('line.fw.count', { n: rows.length });
    el.classList.toggle('on', rows.length > off);
  }
  const only = document.getElementById('f-fw-only-wrap'), foot = document.getElementById('f-fw-foot');
  if (only) only.hidden = !rows.length;
  if (foot) foot.hidden = !rows.length;
}

function fwCollect(withMeta) {
  return [...document.querySelectorAll('#f-fw-list .fw-item')].map(row => {
    const v = row.querySelector('.fw-via').value, ext = v === 'ext';
    const f = { port: Number(row.querySelector('.fw-port').value) || 0, to: Number(row.dataset.to) || 0 };
    const fid = Number(row.dataset.fid) || 0;
    if (fid) f.id = fid; // 入口的编号:节点设置、用户不要哪个节点都按它认,不能丢
    const name = row.querySelector('.fw-name').value.trim();
    if (name) f.name = name;
    if (ext) f.host = row.querySelector('.fw-host').value.trim();
    else {
      f.via = Number(v) || 0;
      if (row.querySelector('.fw-mode').value === 'manual') f.manual = true;
      else {
        const target = row.querySelector('.fw-target');
        if (!target.hidden && target.value.trim()) f.target = target.value.trim();
      }
    }
    if (row.querySelector('.fw-off').checked) f.off = true;
    if (withMeta && row.dataset.auto === '1') f.auto = true;
    return f;
  });
}

// 保存时读:有红的问题就拦下,说清是第几个(分区收着的话先展开)
function fwRead() {
  const rows = [...document.querySelectorAll('#f-fw-list .fw-item')];
  rows.forEach((row, i) => {
    const bad = fwUpdate(row);
    if (bad) failIn('f-fw-acc', t('line.fw.e.at', { n: i + 1, msg: bad }));
  });
  return fwCollect();
}

// 「订阅里只给中转入口」:写进线路的节点设置(Line.NodeOpts)—— 有生效中转入口的出口服务器,它的直连节点不输出。
// 只动这条线路的出口服务器那几个键;开关没碰过且是关着的 = 不带这个字段(返回 null,后端沿用库里的,
// 以后在别处按节点单独关的不会被这里冲掉)。
let fwOpts0 = {}, fwExits0 = [], fwOnlyDirty = false;
function fwOnlyOpts(forwards) {
  const cb = document.getElementById('f-fw-only');
  if (!cb || (!cb.checked && !fwOnlyDirty)) return null;
  const cur = JSON.parse(JSON.stringify(fwOpts0 || {}));
  const active = new Set((forwards || []).filter(f => !f.off).map(f => Number(f.to)));
  const touched = new Set([...fwExits0, ...(forwards || []).map(f => Number(f.to))].filter(Boolean));
  for (const id of touched) {
    const k = 's' + id, o = { ...(cur[k] || {}) };
    if (cb.checked && active.has(id)) o.hidden = true; else delete o.hidden;
    if (Object.keys(o).length) cur[k] = o; else delete cur[k];
  }
  return JSON.stringify(cur) === JSON.stringify(fwOpts0 || {}) ? null : cur;
}

function fwRender(list) {
  const box = document.getElementById('f-fw-list');
  if (!box) return;
  box.innerHTML = list.map(fwItemHTML).join('');
  fwUpdateAll();
}

function fwAdd() {
  const nodes = state.nodes || [], deployed = fwDeployed();
  // 默认经一台不部署这条线路、还没被前面几条用上、没停用的服务器中转(没有就别的任意一台;只有一台服务器就只能外部中转)
  const used = new Set(fwCollect().filter(f => f.via).map(f => f.via));
  const spare = nodes.filter(n => !deployed.includes(n.id) && n.enabled !== false);
  const pick = spare.find(n => !used.has(n.id)) || spare[0] || nodes.find(n => !(deployed.length === 1 && n.id === deployed[0]));
  const via = nodes.length > 1 && pick ? pick.id : 0;
  const f = { via, to: deployed.filter(id => id !== via).length === 1 ? deployed.find(id => id !== via) : 0, auto: !!via };
  fwRender([...fwCollect(true), f]);
  const rows = document.querySelectorAll('#f-fw-list .fw-item'), row = rows[rows.length - 1];
  if (via) { row.querySelector('.fw-port').value = fwFreePort(via, row); fwUpdateAll(); }
  (row.querySelector(via ? '.fw-port' : '.fw-host') || row).focus();
}

function fwBind(initial, editId, l) {
  fwEditId = editId || 0;
  const opts = parseJ(l && l.nodeOpts);
  fwOpts0 = opts && typeof opts === 'object' && !Array.isArray(opts) ? opts : {};
  fwExits0 = initial.map(f => Number(f.to)).filter(Boolean);
  fwOnlyDirty = false;
  fwBusy.clear();
  const list = document.getElementById('f-fw-list'), only = document.getElementById('f-fw-only');
  fwRender(initial);
  // 开关的初始状态:每个有生效入口的出口服务器,直连节点都已经关了输出
  const active = [...new Set(initial.filter(f => !f.off).map(f => Number(f.to)))];
  only.checked = active.length > 0 && active.every(id => (fwOpts0['s' + id] || {}).hidden);
  only.addEventListener('change', () => { fwOnlyDirty = true; });
  document.getElementById('f-fw-add').addEventListener('click', fwAdd);
  list.addEventListener('click', e => {
    const row = e.target.closest('.fw-item');
    if (!row) return;
    const fix = e.target.closest('[data-fw-fix]');
    if (fix) { // 端口被占时给的两条出路
      if (fix.dataset.fwFix === 'manual') row.querySelector('.fw-mode').value = 'manual';
      else { row.querySelector('.fw-port').value = fwFreePort(Number(row.querySelector('.fw-via').value), row); row.dataset.auto = '1'; }
      fwUpdateAll();
    } else if (e.target.closest('.fw-del')) {
      row.remove();
      document.querySelectorAll('#f-fw-list .fw-no').forEach((x, i) => { x.textContent = i + 1; });
      fwUpdateAll();
    } else if (e.target.closest('.fw-dest') && !e.target.closest('.fw-dest').disabled) { // 点开改转到的地址
      const tg = row.querySelector('.fw-target');
      tg.hidden = false;
      fwUpdate(row);
      tg.focus();
    }
  });
  list.addEventListener('change', e => {
    const row = e.target.closest('.fw-item');
    if (!row) return;
    if (e.target.classList.contains('fw-via')) {
      const ext = e.target.value === 'ext';
      // 换了中转服务器、端口还是自动给的:换成那台上空着的端口
      if (!ext && row.dataset.auto === '1') row.querySelector('.fw-port').value = fwFreePort(Number(e.target.value), row);
      if (ext) row.querySelector('.fw-host').focus();
    }
    if (e.target.classList.contains('fw-to')) row.dataset.to = e.target.value;
    fwUpdateAll(); // 一行改了,别的行的撞端口结论也可能变
  });
  list.addEventListener('input', e => {
    const row = e.target.closest('.fw-item');
    if (!row) return;
    if (e.target.classList.contains('fw-port')) row.dataset.auto = '0';
    fwUpdateAll();
  });
  // 外部中转粘贴「地址:端口」:拆开填(IPv6 要写成 [v6]:端口 才拆)
  list.addEventListener('focusout', e => {
    if (!e.target.classList.contains('fw-host')) return;
    const m = /^\[?([^[\]]+?)\]?:(\d{1,5})$/.exec(e.target.value.trim());
    if (m && (!m[1].includes(':') || e.target.value.trim().startsWith('['))) {
      e.target.value = m[1];
      const port = e.target.closest('.fw-item').querySelector('.fw-port');
      port.value = m[2];
      fwUpdateAll();
    }
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
      <button type="button" class="btn sm ghost icon danger" data-rr="del" title="${esc(t('common.delete'))}" aria-label="${esc(t('common.delete'))}"><i class="i i-x"></i></button>
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

// 分流规则分区的正文:查应用域名的一行 + 规则表(一条都没有时只剩「添加规则」)
function rrSection() {
  return `<div class="rr-box">
    <p class="hint acc-lead xp">${esc(t('line.rr.help'))}</p>
    <div class="rr-lookup" id="f-rr-lookup">
      <input id="f-rr-apps" placeholder="${esc(t('line.rr.appPh'))}" aria-label="${esc(t('line.rr.appPh'))}" autocomplete="off" spellcheck="false">
      <button type="button" class="btn" id="f-rr-apps-go">${esc(t('line.rr.lookup'))}</button>
    </div>
    <p class="rr-lookup-msg" id="f-rr-apps-msg" hidden></p>
    <div id="f-rr">
      <div class="rr-row rr-head" aria-hidden="true"><span>#</span><span>${esc(t('line.rr.type'))}</span><span>${esc(t('line.rr.values'))}</span><span>${esc(t('line.rr.to'))}</span><span></span></div>
      <div id="f-rr-list"></div>
      <div class="rr-row rr-rest"><span class="rr-no">—</span><span class="rr-rest-label">${esc(t('line.rr.rest'))}</span><span class="muted small">${esc(t('line.rr.restHint'))}</span><span class="rr-rest-to" id="f-rr-rest"></span><span></span></div>
      <button type="button" class="rr-add" id="f-rr-add">${esc(t('line.rr.add'))}</button>
      <p class="hint rr-hint xp">${esc(t('line.rr.hint'))}</p>
    </div>
  </div>`;
}

function rrRender(rules, focusLast) {
  const list = document.getElementById('f-rr-list');
  list.innerHTML = rules.map((r, i) => rrRowHTML(r, i, rules.length)).join('');
  // 一条规则都没有:表头、「其余流量」和说明都不用出现
  document.querySelectorAll('#f-rr > .rr-head, #f-rr > .rr-rest, #f-rr > .rr-hint').forEach(el => { el.hidden = !rules.length; });
  const sum = document.getElementById('f-rr-sum');
  if (sum) { sum.textContent = rules.length ? t('line.rr.count', { n: rules.length }) : t('line.acc.off'); sum.classList.toggle('on', rules.length > 0); }
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
  const list = document.getElementById('f-rr-list');
  rrRender(initial);
  rrRest();
  document.getElementById('f-upstream').addEventListener('change', rrRest);
  // 分区收着时量不出输入框的高度:展开的时候再量一次
  document.getElementById('f-rr-acc').addEventListener('toggle', e => { if (e.target.open) list.querySelectorAll('.rr-values').forEach(rrFit); });
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

// 保存时用:一条规则都没有 = 不分流(存空);有就每条都要有内容、内容要合规(分区收着的话先展开)
function rrRead() {
  const rows = [...document.querySelectorAll('#f-rr-list .rr-row')];
  const rules = rrCollect();
  rules.forEach((r, i) => {
    if (!r.values.length) failIn('f-rr-acc', t('line.rr.emptyRule', { n: i + 1 }));
    const bad = rrRowCheck(rows[i]);
    if (bad) failIn('f-rr-acc', t('line.rr.errAt', { n: i + 1, msg: bad }));
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
  // 和线路编辑框同一个说法:要么勾出几台,要么「全部服务器」(以后新加的也算)
  openModal(t('line.batchNodes', { n }), `
    ${check('b-all', t('line.scope.all'), false, t('line.scope.allHelp'))}
    <div class="check-list b-nodes">${(state.nodes || []).map(nd => `<label><input type="checkbox" class="b-node-cb" value="${nd.id}"> ${esc(nd.name)}${nd.isLocal ? ` <span class="muted small">${t('node.local')}</span>` : ''}${nd.group ? ` <span class="muted small">${esc(nd.group)}</span>` : ''}</label>`).join('')}</div>`,
    () => {
      const all = fchk('b-all');
      const ids = [...document.querySelectorAll('.b-node-cb:checked')].map(c => Number(c.value));
      if (!all && !ids.length) throw new Error(t('line.scope.none'));
      return runBatch('nodes', { nodeIds: all ? [] : ids });
    });
  document.getElementById('b-all').addEventListener('change', e => {
    document.querySelectorAll('.b-node-cb').forEach(c => { c.disabled = e.target.checked; if (e.target.checked) c.checked = true; });
  });
}

// 选这条线路的证书:列出各服务器证书库的并集,每张证书标出哪台有、哪台没有(没有的那台用自己的默认证书)
function pickLineCert() {
  const parse = v => { try { return Array.isArray(v) ? v : (v ? JSON.parse(v) : []); } catch { return []; } };
  const picked = formScope().ids; // 还一台都没勾(新线路)时按全部服务器列,不然证书列表是空的
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
    setText('f-cert-label', value ? certLabel(value) : t('line.certDefault'));
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
  openDrawer(`${esc(l.name)} ${badge(protoName(l.protocol), 'primary')} <span class="muted small num">:${l.port}</span>${l.enabled ? '' : ' ' + badge(t('common.disabled'), 'warn')}`, `
    <div class="ud-acts">
      <button class="btn sm" data-act="line.edit" data-id="${l.id}"><i class="i i-pencil"></i>${t('common.edit')}</button>
      <button class="btn sm" data-act="line.clone" data-id="${l.id}"><i class="i i-copy"></i>${t('line.clone')}</button>
      <button class="btn sm" data-act="line.asUpstream" data-id="${l.id}">${t('up.addFromLine')}</button>
      ${canLineReach() ? `<button class="btn sm" id="ld-reach-btn" data-act="line.reach" data-id="${l.id}"${l.enabled ? ` title="${esc(t('line.reach.help'))}"` : ` disabled title="${esc(t('line.reach.off'))}"`}>${t('line.reach.btn')}</button>` : ''}
    </div>
    <div class="ld-reach" id="ld-reach" hidden></div>
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
  renderLineReach(id);
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
    <div class="ld-tile"><span class="lb">${esc(t('line.d.sumTotal', { range: t(ldRangeKey(ld.hours)) }))}</span><b class="num">${fmtBytes((tot.totalUp || 0) + (tot.totalDown || 0), 1)}</b></div>
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

// ---- 线路详情:测大陆连通 ----
// 点一下,主机从大陆三网的家宽测点连这条线路在订阅里给用户的每个地址(后端 web/line_reach.go;和服务器页的定时巡检是两回事:
// 这里测的是这条线路自己的端口,不记历史、不告警)。结果要轻:一个地址一行 —— 圆点、名字、地址、三家运营商的小标
// (和服务器页同一种)、结论;解释放在悬停里。抽屉关了再开,这条线路最近一次的结果还在(只留在这次打开的页面里)。
const lineReach = new Map(); // 线路 id → { job, items, done, at, err, timer, fails }
const canLineReach = () => (state.status || {}).role !== 'node' && (state.status || {}).scope !== 'reseller';
const lrAddr = it => (String(it.target).includes(':') ? `[${it.target}]:${it.port}` : `${it.target}:${it.port}`);
const LR_CARRIERS = ['cm', 'ct', 'cu'];
// 一个地址的结论:文字、颜色、悬停里的解释、右边的小标
export function lineReachVerdict(it) {
  if (it.state === 'pending') return { label: t('reach.running'), tone: '', title: '', chips: '', wait: true };
  if (it.state === 'skipped') return { label: t('reach.v6.s.udpLine'), tone: '', title: t('reach.v6.lineUdp', { port: it.port }), chips: '' };
  if (it.v6) {
    const v = it.v6r || { state: 'error', error: it.error || '', target: it.target, port: it.port };
    return { label: v6Label(v), tone: v6Tone(v), title: v6Explain(v), chips: v6Chip(v) };
  }
  const notes = (it.note || '').split(',').filter(Boolean), gs = it.groups || [];
  const down = gs.filter(g => LR_CARRIERS.includes(g.key) && g.state === 'down').map(g => t('reach.g.' + g.key));
  const why = it.verdict === 'error' ? t('reach.x.error', { err: trErr(it.error || '') })
    : it.verdict === 'partial' ? t('reach.x.partial', { down: down.join(t('reach.sep')) || t('reach.v.partial') })
    : it.verdict === 'abroadDown' ? '' // 两边都不通:下面那条提示(bothDown)说得更准
    : it.verdict ? t('reach.x.' + it.verdict) : '';
  const none = ['error', 'noProbes'].includes(it.verdict); // 没测成 / 没有测点:没有分运营商的结果可说
  const hk = none ? null : gs.find(g => g.key === 'hk');
  const title = [why, ...notes.map(n => t('ext.reachNote.' + n)), hk && hk.total ? `${t('reach.g.hk')}(${t('reach.control')})${t('reach.okOf', { ok: hk.ok, total: hk.total })}` : ''].filter(Boolean).join(' ');
  // UDP 的线路只 ping 了地址:通的时候不写「正常」(端口和 QUIC 并没有验证到),写「ping 通」
  const label = it.verdict === 'abroadDown' ? t('ext.reachBothDown') : it.udp && it.verdict === 'ok' ? t('line.reach.pingOk') : verdictLabel(it.verdict);
  return { label, tone: verdictTone(it.verdict), title, chips: none ? '' : gs.filter(g => LR_CARRIERS.includes(g.key)).map(groupChip).join('') };
}
function renderLineReach(id) {
  const box = document.getElementById('ld-reach'), btn = document.getElementById('ld-reach-btn');
  if (!box || !ld || ld.id !== id) return;
  const r = lineReach.get(id);
  if (btn) {
    const busy = !!(r && !r.done && !r.err);
    btn.disabled = busy || !(state.lines.find(x => x.id === id) || {}).enabled;
    btn.innerHTML = busy ? `<span class="spin"></span> ${esc(t('reach.running'))}` : esc(t(r ? 'line.reach.again' : 'line.reach.btn'));
  }
  box.hidden = !r;
  if (!r) { setHTML(box, ''); return; }
  const items = r.items || [], fin = items.filter(x => x.state !== 'pending').length;
  const head = r.err ? `<span class="danger-text">${esc(r.err)}</span>`
    : r.done ? `<span>${esc(fmtTime(r.at))}</span>` : `<span>${esc(t('line.reach.progress', { done: fin, total: items.length }))}</span>`;
  const rows = items.map(it => {
    const v = lineReachVerdict(it), addr = lrAddr(it);
    return `<div class="ld-rrow"><span class="dot ${v.tone}"></span><span class="nm" title="${esc(it.label)}">${esc(it.label)}</span><span class="ad mono" title="${esc(addr)}">${esc(addr)}</span>
      <span class="cs">${v.chips}</span><span class="vd ${v.tone ? v.tone + '-text' : 'muted'}"${v.title ? ` title="${esc(v.title)}"` : ''}>${v.wait ? '<span class="spin"></span>' : esc(v.label)}</span></div>`;
  }).join('');
  // UDP 的线路只能 ping 地址:这一点不放在悬停里,明说一次
  const udp = items.some(x => x.udp && x.state !== 'skipped');
  setHTML(box, `<div class="ld-reach-h"><b>${esc(t('reach.col'))}</b>${head}</div>${rows ? `<div class="ld-rlist">${rows}</div>` : ''}${udp ? `<p class="hint">${esc(t('line.reach.udpNote'))}</p>` : ''}`);
}
async function startLineReach(id) {
  const old = lineReach.get(id);
  if (old && !old.done && !old.err) return;
  const run = { items: (old && old.items) || [], done: false, at: 0, err: '', fails: 0 };
  if (old) clearTimeout(old.timer);
  lineReach.set(id, run);
  run.items = run.items.map(x => (x.state === 'skipped' ? x : { ...x, state: 'pending' })); // 重测时上一次的行先留着,转成等待
  renderLineReach(id);
  let r;
  try { r = await post(`lines/${id}/reach`, {}, SLOW); }
  catch (e) {
    if (lineReach.get(id) !== run) return;
    // 没开始成(额度不够、别处正在测):说明原因;上一次测完的结果还留着
    run.err = trErr(e.message); run.items = (old && old.done && old.items) || []; run.at = (old && old.at) || 0;
    renderLineReach(id);
    return;
  }
  if (lineReach.get(id) !== run) return;
  run.job = r.job; run.items = r.items || [];
  renderLineReach(id);
  const started = Date.now();
  const poll = async () => {
    if (lineReach.get(id) !== run) return;
    let st;
    try { st = await get(`lines/${id}/reach/${run.job}`); run.fails = 0; }
    catch (e) {
      if (lineReach.get(id) !== run) return;
      if (e.status === 404 || ++run.fails > 5) { run.err = trErr(e.message); renderLineReach(id); return; }
      run.timer = setTimeout(poll, 2000 * 2 ** run.fails); // 出错退避重试,不是一错就停
      return;
    }
    if (lineReach.get(id) !== run) return;
    run.items = st.items || run.items;
    if (st.done || Date.now() - started > 6 * 60 * 1000) { run.done = true; run.at = Math.floor(Date.now() / 1000); renderLineReach(id); return; }
    renderLineReach(id);
    run.timer = setTimeout(poll, 2000);
  };
  run.timer = setTimeout(poll, 1500);
}

// 一个中转入口现在的状态:暂停 / 别处的中转 / 已有转发 / 生效;开不出来的写原因(端口被占、那台离线或停用、版本太旧)
function fwState(l, f) {
  if (f.off) return { kind: '', text: t('line.fw.off') };
  if (!f.via) return { kind: '', text: t('line.fw.st.ext') };
  const n = fwNode(f.via);
  if (!n) return { kind: 'danger', text: t('line.fw.st.gone') };
  if (n.enabled === false) return { kind: 'warn', text: t('line.fw.st.offNode') };
  if (f.manual) return { kind: '', text: t('line.fw.modeManual') };
  const b = blockedOn(n).find(x => x.tag === `${l.name} ⇄${f.port}`);
  if (b) return { kind: 'danger', text: t('line.fw.st.busy') + (b.by ? t('line.fw.busyBy', { by: b.by }) : '') };
  if (!n.isLocal && !(n.status || {}).ok) return { kind: 'warn', text: t('line.fw.st.offline') };
  if ((n.status || {}).versionMismatch) return { kind: 'warn', text: t('line.fw.st.old') };
  return { kind: 'ok', text: t('line.fw.st.ok') };
}
// 线路详情里的中转入口:一条一行 —— 用户连哪里、转给哪台、现在的状态
function fwDetail(l) {
  const fs = fwOf(l);
  if (!fs.length) return '';
  const opts = parseJ(l.nodeOpts);
  const rows = fs.map(f => {
    const st = fwState(l, f);
    const hidden = f.id && opts && opts['f' + f.id] && opts['f' + f.id].hidden;
    return `<div class="ld-fwrow${f.off ? ' off' : ''}">
      <span class="from mono" title="${esc(f.name || '')}">${esc(f.via ? fwNodeName(f.via) : f.host)}:${f.port}</span>
      <i class="i i-arrow"></i>
      <span class="to">${esc(fwNodeName(f.to))}</span>
      <span class="st">${hidden ? badge(t('line.fw.st.hidden')) + ' ' : ''}${st.kind ? badge(st.text, st.kind) : `<span class="muted small">${esc(st.text)}</span>`}</span>
    </div>`;
  }).join('');
  return `<section class="ld-fw"><h3>${esc(t('line.fw.title'))} <span class="muted small">${fs.length}</span></h3><div class="ld-fwlist">${rows}</div></section>`;
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
  'line.sel': (id, cb) => { cb.checked ? selected.add(Number(id)) : selected.delete(Number(id)); selFrom = cb; renderRows(); },
  'line.clearSel': () => { selected.clear(); renderRows(); },
  'line.batch': async action => {
    if (!selected.size) return;
    if (action === 'enable' || action === 'disable') { await runBatch(action); return; }
    batchDialog(action);
  },
  'line.preset': id => presetLine(id),
  'line.detail': id => showLineDetail(Number(id)),
  'line.reach': id => startLineReach(Number(id)),
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
  'line.genReality': () => genReality(),
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

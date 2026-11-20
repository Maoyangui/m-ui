// 「节点」页:订阅里的每个节点 —— 一条线路在一台服务器上的直连,或这条线路的一个中转入口 —— 逐个设置
// 倍率(盖过所在服务器的)、限速、用户容量,和要不要输出到订阅。存在线路的 nodeOpts 里(键 "s<服务器 id>" / "f<入口 id>")。
//
// 版式:一张表,一条线路一行,节点多时默认收起、不超过 12 个时默认全展开(见 ../snfold.js;用户 11-18:「节点的现在是全部展开 可以按线路(可点击展开对应的线路查看设置所有服务器/中转的)」——
// 生产上几十条线路、上百个节点,全展开时一屏只看得到一两条线路)。线路那一行的各列是它的汇总:几个节点(含几个中转)、跑在哪几台、
// 倍率范围、几个限速、在线人数(几个已满)、输出了几个;点它展开,它的节点作为子行缩进列在下面。有搜索或筛选时命中的线路自动展开,
// 只有一条线路时直接展开。节点行平时只是看:倍率一格写着现在实际生效的倍率和它是「跟随服务器」还是单独设的,
// 一眼分得出;没设的限速、容量都是淡色的「不限」。常用的「输出到订阅」开关直接摆在行里,点了就存;
// 其余几项点「设置」在弹框里改(不在表里摆一排输入框:十几个节点就是几十个框,看着累,也容易改错行)。
//
// 线路一多光靠搜索找不过来(用户 11-17:「节点页能否增加一些必要筛选」):工具栏可以按线路、按服务器、按类型(直连 / 中转)筛,
// 再加「单独设置的 / 没输出的」。要一次改很多个就勾选(用户同一天:「节点这个要支持勾选批量操作」):行首勾一个、
// 分组行勾整条线路、表头勾现在列出的全部 —— 先筛再全选,就是「某台服务器上的全部节点」这类批量。
import { expandByDefault } from '../snfold.js';
import { state } from '../app.js';
import { get, put } from '../api.js';
import { t } from '../i18n.js';
import { keep, esc, toast, confirm, openModal, registerActions, badge, field, empty, fv, fchk, matches, debounce, setHTML, switchRow } from '../ui.js';

export const title = () => t('sn.title');
export const subtitle = () => t('sn.subtitle');

let query = '';
let only = 'all'; // all / own(单独设置过的)/ off(没输出到订阅的)
let fLine = 0;    // 只看这条线路(0 = 全部)
let fServer = 0;  // 只看和这台服务器有关的:它上面的直连,经它中转的、转到它的入口(0 = 全部)
let fKind = '';   // direct / relay('' = 全部)
const selected = new Set(); // 勾选的节点:"线路 id:节点键"
const expanded = new Set(); // 展开着的线路 id(节点多时默认都收起,见 seedExpanded)
let seeded = false;         // 默认的展开状态定过没有(进页面头一次有数据时定一次,之后跟着用户点的走)
let online = {};            // 各节点此刻在线的用户数,键同上(GET lines/nodes/online;主机汇总本机和各副机报来的)

async function loadOnline() {
  try { online = (await get('lines/nodes/online')).online || {}; } catch { /* 拿不到就先不显示人数,不为它报错 */ }
}
// 定时刷新(app.js 每几秒调一次):只更新在线人数
export async function tick() { await loadOnline(); renderRows(); }

const parseJ = v => { try { return typeof v === 'string' ? JSON.parse(v) : (v || null); } catch { return null; } };
const servers = () => state.nodes || [];
const hostOf = n => (n ? (n.domain || n.addr || n.publicIp || n.publicIp6 || '') : '');
const fmtRatio = r => String(Math.round(r * 10000) / 10000);
const isOwn = o => !!o && (o.ratio > 0 || o.speed > 0 || o.userSpeed > 0 || o.cap > 0);
const idOf = (l, nd) => `${l.id}:${nd.key}`;

// nodesOf 一条线路的全部节点:部署到的每台服务器一个直连,每个中转入口一个(和后端 lineNodeKeys 同一口径)。
export function nodesOf(l) {
  const all = servers();
  const ids = parseJ(l.nodeIds) || [];
  const opts = parseJ(l.nodeOpts) || {};
  const out = all.filter(n => !ids.length || ids.includes(n.id)).map(n => ({
    key: 's' + n.id, kind: 'direct', name: n.name, exit: n, host: hostOf(n), port: l.port, opt: opts['s' + n.id] || {},
    idle: n.enabled === false ? t('sn.serverOff') : '',
  }));
  for (const f of parseJ(l.forwards) || []) {
    if (!f.id) continue;
    const exit = all.find(n => n.id === f.to), via = f.via ? all.find(n => n.id === f.via) : null;
    out.push({
      key: 'f' + f.id, kind: 'relay', name: f.name || (via ? via.name : f.host) || ('#' + f.id), exit, via, fwd: f,
      host: via ? hostOf(via) : (f.host || ''), port: f.port, opt: opts['f' + f.id] || {},
      idle: f.off ? t('sn.paused') : (exit && exit.enabled === false) || (via && via.enabled === false) ? t('sn.serverOff') : '',
    });
  }
  return out;
}
// 没单独设倍率时跟的是哪台服务器的倍率:直连是所在的那台,中转入口是出口那台(流量在出口上记)
const serverRatio = nd => (nd.exit && nd.exit.ratio > 0 ? nd.exit.ratio : 1);
const onServer = (nd, id) => (nd.exit && nd.exit.id === id) || (nd.via && nd.via.id === id);
const filtered = () => !!(query.trim() || only !== 'all' || fLine || fServer || fKind);

// visible 现在列出来的:每条线路 { 线路, 它的全部节点, 其中过了搜索和筛选的 }
function visible() {
  return state.lines.filter(l => !fLine || l.id === fLine).map(l => {
    const all = nodesOf(l);
    const list = all.filter(nd => matches(query, l.name, nd.name, nd.host, nd.exit && nd.exit.name)
      && (only === 'all' || (only === 'own' ? isOwn(nd.opt) : !!nd.opt.hidden))
      && (!fServer || onServer(nd, fServer)) && (!fKind || nd.kind === fKind));
    return { l, all, list };
  });
}

export async function render(el) {
  // 筛选记着的线路 / 服务器已经删了:回到「全部」
  if (fLine && !state.lines.some(l => l.id === fLine)) fLine = 0;
  if (fServer && !servers().some(n => n.id === fServer)) fServer = 0;
  const hasRelay = state.lines.some(l => (parseJ(l.forwards) || []).some(f => f.id));
  if (!hasRelay) fKind = '';
  const opt = (v, label, cur) => `<option value="${v}" ${String(cur) === String(v) ? 'selected' : ''}>${esc(label)}</option>`;
  el.innerHTML = `
    <div class="toolbar sn-bar">
      <input type="search" id="sn-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      ${state.lines.length > 1 ? `<select class="sm" id="sn-f-line" data-change="sn.filter" data-id="line" aria-label="${esc(t('nav.lines'))}">${opt(0, t('sn.f.allLines'), fLine)}${state.lines.map(l => opt(l.id, l.name, fLine)).join('')}</select>` : ''}
      ${servers().length > 1 ? `<select class="sm" id="sn-f-server" data-change="sn.filter" data-id="server" aria-label="${esc(t('nav.nodes'))}">${opt(0, t('sn.f.allServers'), fServer)}${servers().map(n => opt(n.id, n.name, fServer)).join('')}</select>` : ''}
      ${hasRelay ? `<select class="sm" id="sn-f-kind" data-change="sn.filter" data-id="kind" aria-label="${esc(t('sn.f.kind'))}">${opt('', t('sn.f.allKinds'), fKind)}${opt('direct', t('sn.direct'), fKind)}${opt('relay', t('sn.relay'), fKind)}</select>` : ''}
      <div class="seg" id="sn-only">${['all', 'own', 'off'].map(k => `<button type="button" data-act="sn.only" data-id="${k}" class="${only === k ? 'active' : ''}">${t('sn.filter.' + k)}</button>`).join('')}</div>
      <span class="muted small" id="sn-count"></span>
      <button type="button" class="btn sm ghost" id="sn-clear" data-act="sn.clearFilter" hidden>${t('sn.f.clear')}</button>
    </div>
    <div class="toolbar batch-bar" id="sn-batch-bar" hidden>
      <span class="badge primary" id="sn-batch-count"></span>
      <button class="btn sm" data-act="sn.batch" data-id="on">${t('sn.batch.on')}</button>
      <button class="btn sm" data-act="sn.batch" data-id="off">${t('sn.batch.off')}</button>
      <button class="btn sm" data-act="sn.batch" data-id="ratio">${t('sn.batch.ratio')}</button>
      <button class="btn sm" data-act="sn.batch" data-id="limits">${t('sn.batch.limits')}</button>
      <button class="btn sm" data-act="sn.batch" data-id="reset">${t('sn.reset')}</button>
      <button class="btn sm ghost" data-act="sn.clearSel">${t('line.batch.clear')}</button>
    </div>
    <div class="table-wrap fill"><table class="grid sn-table" data-fit="p3:980 p2:830 cards:700">
      <thead><tr><th style="width:1.5rem"><input type="checkbox" id="sn-sel-all" aria-label="${esc(t('sn.selAll'))}"></th><th>${t('sn.col.lineNode')}<button type="button" class="link-btn sn-fold" id="sn-fold" data-act="sn.foldAll" hidden></button></th><th class="c-p3">${t('sn.col.addr')}</th><th>${t('sn.col.ratio')}</th><th class="c-p2">${t('sn.col.speed')}</th><th class="c-p2">${t('sn.col.cap')}</th><th>${t('sn.col.output')}</th><th></th></tr></thead>
      <tbody id="sn-body"></tbody>
    </table></div>`;
  document.getElementById('sn-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  document.getElementById('sn-sel-all').addEventListener('change', e => {
    for (const g of visible()) for (const nd of g.list) e.target.checked ? selected.add(idOf(g.l, nd)) : selected.delete(idOf(g.l, nd));
    renderRows();
  });
  // 点线路那一行的空白处也能展开 / 收起;行里的勾选框、按钮照常用,不归这里
  document.getElementById('sn-body').addEventListener('click', e => {
    const tr = e.target.closest('tr.sn-line');
    if (tr && !e.target.closest('input, label, button, a, select')) toggleLine(Number(tr.dataset.line));
  });
  renderRows();
  loadOnline().then(renderRows); // 不等它:表先出来,人数回来再补上
}

function speedText(o) {
  if (o.speed > 0 && o.userSpeed > 0) return t('sn.speedBoth', { a: o.speed, b: o.userSpeed });
  if (o.speed > 0) return t('sn.speedTotal', { a: o.speed });
  if (o.userSpeed > 0) return t('sn.speedUser', { b: o.userSpeed });
  return '';
}

// 展开 / 收起一条线路
function toggleLine(id) {
  expanded.has(id) ? expanded.delete(id) : expanded.add(id);
  renderRows();
}

// lineFacts 卡片排法(手机)里线路卡的一行汇总:桌面上这些分在倍率、限速、在线、输出几列里,卡片里一格一行太占地方
function lineFacts(lo, hi, onl, full, limited, outN, total) {
  return [`×${fmtRatio(lo)}${hi > lo ? ` – ×${fmtRatio(hi)}` : ''}`, onl > 0 ? t('sn.onlineN', { n: onl }) : '', full ? t('sn.fullN', { n: full }) : '',
    limited ? t('sn.limitedN', { n: limited }) : '', outN < total ? t('sn.outFact', { a: outN, b: total }) : '']
    .filter(Boolean).map((x, i) => `<span>${i ? '· ' : ''}${esc(x)}</span>`).join(' ');
}

// lineRowHTML 线路那一行:各列是它现在列出来的这些节点的汇总;点一下展开 / 收起
function lineRowHTML(l, all, list, open) {
  const ids = list.map(nd => idOf(l, nd));
  const nSel = ids.filter(id => selected.has(id)).length;
  const relays = list.filter(nd => nd.kind === 'relay').length;
  const count = list.length < all.length ? t('sn.countOf', { a: list.length, b: all.length }) : all.length === 1 ? t('sn.count1') : t('sn.count', { n: all.length });
  const where = [...new Set(list.filter(nd => nd.kind === 'direct').map(nd => nd.name))].join(t('node.sep'));
  const ratios = list.map(nd => (nd.opt.ratio > 0 ? nd.opt.ratio : serverRatio(nd)));
  const lo = Math.min(...ratios), hi = Math.max(...ratios);
  const limited = list.filter(nd => nd.opt.speed > 0 || nd.opt.userSpeed > 0).length;
  const onl = ids.reduce((a, id) => a + (online[id] || 0), 0);
  const full = list.filter((nd, i) => nd.opt.cap > 0 && (online[ids[i]] || 0) >= nd.opt.cap).length;
  const outN = list.filter(nd => !nd.opt.hidden).length;
  // 有单独设了倍率的节点:范围和节点行里单独设的一样用强调色
  const ratio = `<span class="${list.some(nd => nd.opt.ratio > 0) ? 'sn-own' : 'sn-follow'}">×${esc(fmtRatio(lo))}${hi > lo ? ` – ×${esc(fmtRatio(hi))}` : ''}</span>`;
  return `<tr class="sn-line${open ? ' open' : ''}${l.enabled === false ? ' sn-idle' : ''}" data-line="${l.id}">
    <td><input type="checkbox" class="sel" data-change="sn.selLine" data-id="${l.id}" data-part="${nSel > 0 && nSel < ids.length ? 1 : 0}" ${nSel === ids.length ? 'checked' : ''} aria-label="${esc(t('sn.selLine', { name: l.name }))}"></td>
    <td class="primary-cell"><div class="sn-lhead"><button type="button" class="sn-ltog" data-act="sn.toggle" data-id="${l.id}" aria-expanded="${open}" title="${esc(t(open ? 'sn.collapse' : 'sn.expand'))}"><i class="i i-chev" aria-hidden="true"></i><b>${esc(l.name)}</b></button>${l.enabled === false ? badge(t('sn.lineOff')) : ''}</div>
      <div class="sub-cell sn-lsum">${[`${String(l.protocol || '').toUpperCase()} · ${l.port}`, count, relays ? (relays === 1 ? t('sn.relay1') : t('sn.relayN', { n: relays })) : ''].filter(Boolean).map((x, i, a) => `<span>${esc(x)}${i < a.length - 1 ? ' ·' : ''}</span>`).join(' ')}</div>
      <div class="sub-cell sn-lfacts">${lineFacts(lo, hi, onl, full, limited, outN, list.length)}</div></td>
    <td class="c-p3"><span class="ellip sn-where" title="${esc(where)}">${esc(where)}</span></td>
    <td class="num">${ratio}<span class="sn-size" aria-hidden="true"><span class="sn-follow">×1.5<span> · ${esc(t('sn.follow'))}</span></span></span></td>
    <td class="num c-p2">${limited ? esc(t('sn.limitedN', { n: limited })) : `<span class="faint">${esc(t('sn.unlimited'))}</span>`}</td>
    <td class="num c-p2"><span class="sn-cap">${onl > 0 ? `<b>${onl}</b>` : '<span class="faint">0</span>'}${full ? ` <span class="sn-full">${esc(t('sn.fullN', { n: full }))}</span>` : ''}</span><span class="sn-size" aria-hidden="true"><span class="sn-cap"><b>0</b> / ${esc(t('sn.unlimited'))}</span></span></td>
    <td class="sn-out">${outN === list.length ? `<span class="faint">${esc(t('sn.outAll'))}</span>` : `<span class="sn-outn">${esc(t('sn.outOfN', { a: outN, b: list.length }))}</span>`}</td>
    <td class="actions"><span class="btn sm sn-size-btn" aria-hidden="true">${t('sn.edit')}</span></td>
  </tr>`;
}

function rowHTML(l, nd, last) {
  const o = nd.opt, id = idOf(l, nd);
  const where = nd.kind === 'relay'
    ? `${badge(t('sn.relay'), 'primary')}<span class="pname" title="${esc(nd.name)}">${esc(nd.name)}</span><span class="sn-to">${esc(t('sn.to', { name: nd.exit ? nd.exit.name : '?' }))}</span>`
    : `<span class="pname" title="${esc(nd.name)}">${esc(nd.name)}</span>`;
  const ratio = o.ratio > 0
    ? `<b class="sn-own">×${esc(fmtRatio(o.ratio))}</b>`
    : `<span class="sn-follow">×${esc(fmtRatio(serverRatio(nd)))}<span> · ${esc(t('sn.follow'))}</span></span>`;
  const speed = speedText(o), unl = `<span class="faint">${esc(t('sn.unlimited'))}</span>`;
  const addr = nd.host ? `${nd.host}:${nd.port}` : `:${nd.port}`;
  // 在线 / 容量:几个人在线、上限多少(没设是淡色的「不限」);到上限了变色
  const n = online[id] || 0;
  const cap = o.cap > 0
    ? `<span class="sn-cap${n >= o.cap ? ' full' : ''}"><b>${n}</b> / ${o.cap}</span>`
    : `<span class="sn-cap">${n > 0 ? `<b>${n}</b>` : `<span class="faint">0</span>`}<span class="faint"> / ${esc(t('sn.unlimited'))}</span></span>`;
  return `<tr class="sn-child${last ? ' last' : ''}${nd.idle || l.enabled === false ? ' sn-idle' : ''}${selected.has(id) ? ' selected' : ''}">
    <td><input type="checkbox" class="sel" data-change="sn.sel" data-id="${id}" ${selected.has(id) ? 'checked' : ''} aria-label="${esc(nd.name)}"></td>
    <td class="primary-cell"><div class="sn-node">${where}${nd.idle ? ` <span class="muted small">${esc(nd.idle)}</span>` : ''}</div><div class="sub-cell sn-addr" title="${esc(addr)}">${esc(addr)}</div></td>
    <td class="num c-p3"><span class="ellip" title="${esc(addr)}">${esc(addr)}</span></td>
    <td class="num">${ratio}</td>
    <td class="num c-p2${speed ? '' : ' sn-def'}">${speed ? esc(speed) : unl}</td>
    <td class="num c-p2${o.cap > 0 || n > 0 ? '' : ' sn-def'}">${cap}</td>
    <td class="sn-out"><label class="switch" title="${esc(t('sn.outputHelp'))}"><input type="checkbox" data-change="sn.output" data-id="${id}" ${o.hidden ? '' : 'checked'} aria-label="${esc(t('sn.col.output'))}"><span></span></label></td>
    <td class="actions"><button class="btn sm" data-act="sn.edit" data-id="${id}">${t('sn.edit')}</button></td>
  </tr>`;
}

// seedExpanded 头一次有数据时定默认:节点不多(一屏放得下)就全展开 —— 只有两三条线路的面板不用再一条条点开;
// 多了才按线路收起(几十条线路、上百个节点全摊开找不到东西)。只定一次,之后展开 / 收起跟着用户点的走。
function seedExpanded() {
  if (seeded || !state.lines.length) return;
  seeded = true;
  if (expandByDefault(state.lines.map(l => nodesOf(l).length))) for (const l of state.lines) expanded.add(l.id);
}

function renderRows() {
  const body = document.getElementById('sn-body');
  if (!body) return;
  seedExpanded();
  const groups = visible();
  // 已经不在的节点(线路删了、部署范围或入口改了)不再算选中
  const alive = new Set(state.lines.flatMap(l => nodesOf(l).map(nd => idOf(l, nd))));
  for (const id of [...selected]) if (!alive.has(id)) selected.delete(id);
  // 有搜索 / 筛选时命中的线路都展开(不然筛完还得一条条点开);只有一条线路时也不用收
  const auto = filtered() || state.lines.length === 1;
  let shown = 0;
  const html = groups.map(({ l, all, list }) => {
    if (!list.length) return '';
    shown += list.length;
    const open = auto || expanded.has(l.id);
    return lineRowHTML(l, all, list, open) + (open ? list.map((nd, i) => rowHTML(l, nd, i === list.length - 1)).join('') : '');
  }).join('');
  const fold = document.getElementById('sn-fold');
  if (fold) {
    const lines = groups.filter(g => g.list.length);
    const allOpen = lines.length > 0 && lines.every(g => expanded.has(g.l.id));
    fold.hidden = auto || lines.length < 2;
    fold.dataset.id = allOpen ? 'collapse' : 'expand';
    fold.textContent = t(allOpen ? 'sn.collapseAll' : 'sn.expandAll');
  }
  document.getElementById('sn-count').textContent = alive.size ? `${shown} / ${alive.size}` : '';
  const clear = document.getElementById('sn-clear');
  if (clear) clear.hidden = !filtered();
  const bar = document.getElementById('sn-batch-bar');
  if (bar) { bar.hidden = selected.size === 0; document.getElementById('sn-batch-count').textContent = t('sn.selected', { n: selected.size }); }
  const selAll = document.getElementById('sn-sel-all');
  if (selAll) selAll.checked = shown > 0 && groups.every(g => g.list.every(nd => selected.has(idOf(g.l, nd))));
  if (!state.lines.length) { setHTML(body, `<tr><td colspan="8"><div class="empty-guide"><p>${esc(t('sn.empty'))}</p><a class="btn primary" href="#/lines">${esc(t('sn.goLines'))}</a></div></td></tr>`); return; }
  setHTML(body, html || `<tr><td colspan="8">${empty()}</td></tr>`);
  // 线路里只勾了一部分节点:勾选框显示「半选」(indeterminate 只能用属性设)
  body.querySelectorAll('input[data-change="sn.selLine"]').forEach(cb => { cb.indeterminate = cb.dataset.part === '1'; });
}

function find(id) {
  const [lineId, key] = String(id).split(':');
  const l = state.lines.find(x => x.id === Number(lineId));
  const nd = l ? nodesOf(l).find(x => x.key === key) : null;
  return { l, nd };
}

// 后端回来的是整条线路:把节点设置和中转入口换进 state 里
function adopt(line) {
  const i = state.lines.findIndex(x => x.id === line.id);
  if (i >= 0) state.lines[i] = { ...state.lines[i], nodeOpts: line.nodeOpts || null, forwards: line.forwards || null };
}

// 存一个节点的设置:后端整份替换这个节点的设置
async function saveNode(l, key, opt, src) {
  const body = { key, opt };
  if (src !== undefined) body.src = src;
  adopt(await put(`lines/${l.id}/node`, body));
  renderRows();
}

// 批量:勾选的节点改同样的几项(set 里没有的项不动),一次请求、后端一笔事务
async function saveBatch(set) {
  const items = [...selected].map(id => { const [line, key] = id.split(':'); return { line: Number(line), key }; });
  const res = await put('lines/nodes', { items, set });
  (res.lines || []).forEach(adopt);
  renderRows();
  toast(t('sn.batch.done', { n: items.length }), 'ok');
}

const cleanOpt = o => {
  const out = {};
  if (o.hidden) out.hidden = true;
  for (const k of ['ratio', 'speed', 'userSpeed', 'cap']) if (o[k] > 0) out[k] = o[k];
  return out;
};
const numInput = (eid, v, ph, attrs) => `<input id="${eid}" type="number" inputmode="decimal" min="0" ${attrs} value="${v > 0 ? esc(String(v)) : ''}" placeholder="${esc(ph)}">`;
const readNum = eid => { const s = fv(eid).trim(); return s === '' ? 0 : Number(s); };

function editNode(id) {
  const { l, nd } = find(id);
  if (!l || !nd) return;
  const o = nd.opt;
  const known = nd.kind === 'relay' ? (nd.via ? [nd.via.publicIp, nd.via.publicIp6, nd.via.addr] : [nd.fwd.host]).filter(x => x && /^[0-9a-f:.]+$/i.test(x)) : [];
  const who = nd.kind === 'relay' ? `${nd.name} ${t('sn.to', { name: nd.exit ? nd.exit.name : '?' })}` : nd.name;
  openModal(t('sn.editTitle'), `
    <p class="sn-who"><b>${esc(l.name)}</b><span class="muted"> · ${esc(who)}</span>${nd.kind === 'relay' ? ' ' + badge(t('sn.relay'), 'primary') : ''}</p>
    <div class="form-grid">
      ${field(t('sn.ratio'), numInput('sn-ratio', o.ratio, t('sn.ratioPh', { r: fmtRatio(serverRatio(nd)) }), 'max="100" step="0.1"'), t('sn.ratioHelp'))}
      ${field(t('sn.cap'), numInput('sn-cap', o.cap, t('sn.unlimited'), 'max="1000000" step="1"'), t('sn.capHelp'))}
      ${field(t('sn.speed'), numInput('sn-speed', o.speed, t('sn.unlimited'), 'max="100000" step="1"'), t('sn.speedHelp'))}
      ${field(t('sn.userSpeed'), numInput('sn-uspeed', o.userSpeed, t('sn.unlimited'), 'max="100000" step="1"'), t('sn.userSpeedHelp'))}
    </div>
    <div class="sn-extra">
      ${switchRow('sn-out', t('sn.output'), !o.hidden, t('sn.outputHelp'))}
      ${nd.kind === 'relay' ? `<details class="adv"${nd.fwd.src ? ' open' : ''}><summary>${esc(t('sn.srcSummary'))}</summary>
        ${field(t('sn.src'), `<input id="sn-src" value="${esc(nd.fwd.src || '')}" placeholder="192.0.2.10, 2001:db8::10" autocomplete="off" spellcheck="false">`,
          [t('sn.srcHelp'), keep(known.length ? t('sn.srcKnown', { list: known.join('、') }) : t('sn.srcNone'))])}
      </details>` : ''}
      <div><button type="button" class="btn sm sn-reset" data-act="sn.reset">${esc(t('sn.reset'))}</button></div>
    </div>`,
  async () => {
    const next = { hidden: !fchk('sn-out'), ratio: readNum('sn-ratio'), speed: readNum('sn-speed'), userSpeed: readNum('sn-uspeed'), cap: readNum('sn-cap') };
    if (!(next.ratio >= 0 && next.ratio <= 100)) throw new Error(t('sn.err.ratio'));
    for (const k of ['speed', 'userSpeed', 'cap']) if (!(Number.isInteger(next[k]) && next[k] >= 0)) throw new Error(t('sn.err.int'));
    const srcEl = document.getElementById('sn-src');
    const src = srcEl && srcEl.value.trim() !== (nd.fwd.src || '') ? srcEl.value.trim() : undefined;
    await saveNode(l, nd.key, cleanOpt(next), src);
    toast(t('sn.saved'), 'ok');
  });
}

// 批量设倍率:一个框。留空 = 这些节点都改回跟随服务器
function batchRatio() {
  openModal(t('sn.batch.ratioTitle'), `
    <p class="sn-who">${esc(t('sn.selected', { n: selected.size }))}</p>
    ${field(t('sn.ratio'), numInput('sn-b-ratio', 0, t('sn.batch.ratioPh'), 'max="100" step="0.1"'), t('sn.batch.ratioHelp'))}`,
  async () => {
    const r = readNum('sn-b-ratio');
    if (!(r >= 0 && r <= 100)) throw new Error(t('sn.err.ratio'));
    await saveBatch({ ratio: r });
  });
}

// 批量设限速 / 容量:三项各自选「不改 / 设为 / 不限」—— 留空在这里有歧义(是不动还是清掉),所以让人明说
function batchLimits() {
  const row = (key, label, max) => `<div class="sn-brow"><span class="label">${esc(label)}</span>
      <select id="sn-b-${key}-mode" data-change="sn.bmode" data-id="${key}" aria-label="${esc(label)}"><option value="keep">${esc(t('sn.keep'))}</option><option value="set">${esc(t('sn.setTo'))}</option><option value="clear">${esc(t('sn.unlimited'))}</option></select>
      <input id="sn-b-${key}" type="number" inputmode="numeric" min="1" max="${max}" step="1" disabled aria-label="${esc(label)}"></div>`;
  openModal(t('sn.batch.limitsTitle'), `
    <p class="sn-who">${esc(t('sn.selected', { n: selected.size }))}</p>
    <div class="sn-brows">
      ${row('speed', t('sn.speed'), 100000)}
      ${row('userSpeed', t('sn.userSpeed'), 100000)}
      ${row('cap', t('sn.cap'), 1000000)}
    </div>
    <p class="hint xp">${esc(t('sn.batch.limitsHelp'))}</p>`,
  async () => {
    const set = {};
    for (const key of ['speed', 'userSpeed', 'cap']) {
      const mode = fv(`sn-b-${key}-mode`);
      if (mode === 'clear') set[key] = 0;
      if (mode === 'set') {
        const v = readNum(`sn-b-${key}`);
        if (!(Number.isInteger(v) && v >= 1)) throw new Error(t('sn.err.batchInt'));
        set[key] = v;
      }
    }
    if (!Object.keys(set).length) throw new Error(t('sn.batch.none'));
    await saveBatch(set);
  });
}

registerActions({
  'sn.only': id => { only = id; document.querySelectorAll('#sn-only button').forEach(b => b.classList.toggle('active', b.dataset.id === only)); renderRows(); },
  'sn.filter': (what, el) => {
    if (what === 'line') fLine = Number(el.value) || 0;
    else if (what === 'server') fServer = Number(el.value) || 0;
    else fKind = el.value;
    renderRows();
  },
  'sn.clearFilter': () => {
    query = ''; only = 'all'; fLine = 0; fServer = 0; fKind = '';
    const page = document.getElementById('page');
    if (page) render(page);
  },
  'sn.edit': id => editNode(id),
  'sn.toggle': id => toggleLine(Number(id)),
  // 全部展开 / 全部收起(只管现在列出来的线路)
  'sn.foldAll': what => {
    for (const g of visible()) if (g.list.length) what === 'collapse' ? expanded.delete(g.l.id) : expanded.add(g.l.id);
    renderRows();
  },
  'sn.reset': () => {
    for (const eid of ['sn-ratio', 'sn-cap', 'sn-speed', 'sn-uspeed']) { const el = document.getElementById(eid); if (el) el.value = ''; }
    const out = document.getElementById('sn-out');
    if (out) out.checked = true;
  },
  // 行里的开关:点了就存;没存上把开关拨回去
  'sn.output': async (id, el) => {
    const { l, nd } = find(id);
    if (!l || !nd) return;
    const on = el.checked;
    el.disabled = true;
    try {
      await saveNode(l, nd.key, cleanOpt({ ...nd.opt, hidden: !on }));
      toast(t(on ? 'sn.outOn' : 'sn.outOff'), 'ok');
    } catch (e) {
      el.checked = !on;
      el.disabled = false;
      toast(e.message, 'err');
    }
  },
  'sn.sel': (id, cb) => { cb.checked ? selected.add(id) : selected.delete(id); renderRows(); },
  // 分组行的勾选框:这条线路现在列出来的节点全选 / 全不选
  'sn.selLine': (id, cb) => {
    const g = visible().find(x => x.l.id === Number(id));
    if (g) for (const nd of g.list) cb.checked ? selected.add(idOf(g.l, nd)) : selected.delete(idOf(g.l, nd));
    renderRows();
  },
  'sn.clearSel': () => { selected.clear(); renderRows(); },
  'sn.bmode': (key, sel) => {
    const input = document.getElementById('sn-b-' + key);
    if (!input) return;
    input.disabled = sel.value !== 'set';
    if (input.disabled) input.value = ''; else input.focus();
  },
  'sn.batch': async what => {
    if (!selected.size) return;
    try {
      if (what === 'on' || what === 'off') await saveBatch({ hidden: what === 'off' });
      else if (what === 'ratio') batchRatio();
      else if (what === 'limits') batchLimits();
      else if (what === 'reset' && await confirm(t('sn.batch.resetConfirm', { n: selected.size }))) await saveBatch({ hidden: false, ratio: 0, speed: 0, userSpeed: 0, cap: 0 });
    } catch (e) { toast(e.message, 'err'); }
  },
});

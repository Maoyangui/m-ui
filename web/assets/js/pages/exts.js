import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtRelative, toast, confirm, openModal, openDrawer, registerActions, badge, field, check, empty, fv, fchk, copy, matches, hostPort } from '../ui.js';

// 外部节点的大陆连通:两边都不通对外部节点就是用不了(服务器巡检里那是"机器自己的问题",给黄色)
const REACH_TONE = { ok: 'ok', partial: 'warn', blocked: 'danger', ipBlocked: 'danger', portBlocked: 'danger', abroadDown: 'danger', noProbes: '', error: '' };

export const title = () => t('ext.title');
export const subtitle = () => t('ext.subtitle');

// 展开面板的状态都放在模块里,不放 DOM:列表每几秒重画一次,勾选、搜索词、测速进度不能跟着丢。
// 节点一律按指纹(key)认:订阅刷新后对方增删、调换了节点,下标会变,指纹不变。
const open = new Set();  // 展开了的外部节点 id
const cache = {};        // id → 展开接口的返回 { nodes, servers, fetchedAt, sig, rev, stale }
const loading = {};      // id → 正在拉的请求(好几处同时要拉时只发一个)
const picked = {};       // id → Set(节点指纹)
const query = {};        // id → 搜索词
const results = {};      // id → { 指纹: { 服务器 id: 测速格 } }:列表刷新、面板重画都不丢
const reaches = {};      // id → { 指纹: 大陆连通结果 }
const jobs = {};         // id → { job, timer } 进行中的测速(请求还没回来时就占上,防重复点)
const reachJobs = {};    // id → { job, timer } 进行中的大陆连通(同上)
const inclHidden = {};   // id → 测速全部时也测已隐藏的
const filt = {};         // id → 筛选:all | ok(有能通的)| bad(有不通的)| hidden | sel(已选)
const sorts = {};        // id → { k: 'name' | 'delay', dir: 1 | -1 };没有 = 按订阅里的顺序
const limits = {};       // id → 当前画了多少行(节点很多时分批画,每批 PAGE 个)
const keepTop = {};      // id → 列表重画、面板重建前子表的滚动位置
let focusNext = 0;       // 刚展开的那个:画好后滚到面板顶部,整个滚动区落在视口里
let rowsSig = '';
const PAGE = 200;
// 视图:汇总(每个节点一格:最快的那台 + n/m 台通)/ 逐台(每台服务器一列)。服务器超过 6 台默认汇总,切过就记住;手机只看汇总
const LS_VIEW = 'm-ui.extView', LS_COLS = 'm-ui.extCols';
const lsGet = k => { try { return localStorage.getItem(k); } catch { return null; } };
const lsSet = (k, v) => { try { localStorage.setItem(k, v); } catch { /* 隐私模式 */ } };
const mobile = () => typeof matchMedia === 'function' && matchMedia('(max-width: 700px)').matches;
const viewOf = c => (mobile() ? 'sum' : lsGet(LS_VIEW) || ((c.servers || []).length > 6 ? 'sum' : 'grid'));
// 逐台视图显示哪几台服务器的列(存的是服务器 id;没选过、或选的都不在了 = 全部)
function colIds(c) {
  const all = (c.servers || []).map(x => x.id);
  let pick = null;
  try { pick = JSON.parse(lsGet(LS_COLS) || 'null'); } catch { pick = null; }
  if (!Array.isArray(pick)) return all;
  const set = new Set(pick), out = all.filter(id => set.has(id));
  return out.length ? out : all;
}

// 名字里带会变的数字(剩余流量、天数、到期日、倍率):这类信息节点每次刷新都改名
const DYNAMIC = /\d+(\.\d+)?\s*(GB|MB|TB|KB|G|M|T|天|日|小时|%|倍|x)|\d{4}[-/.年]\d{1,2}[-/.月]\d{1,2}/i;

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="muted small">${t('ext.help')}</span><span class="grow"></span><button class="btn primary" data-act="ext.add">${t('ext.add')}</button></div>
    <div class="table-wrap"><table class="grid ext-table">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.type')}</th><th>${t('ext.value')}</th><th>${t('ext.nodes')}</th><th>${t('ext.lastFetch')}</th><th>${t('ext.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="exts-body"></tbody>
    </table></div>`;
  rowsSig = '';
  renderRows();
  // 展开面板里的搜索框:委托在表体上,重画也不用重新绑
  el.querySelector('#exts-body').addEventListener('input', e => {
    const q = e.target.closest('input[data-q]');
    if (!q) return;
    query[q.dataset.q] = q.value;
    renderSubBody(Number(q.dataset.q));
  });
}
export async function tick() { await load('exts'); renderRows(); }

// listChanged 展开时拿到的节点列表是否已作废:看服务端记的内容指纹(刷新了但内容没变就不作废,测速结果也不丢);
// 老数据还没有指纹时退回看抓取时间。
const listChanged = (c, x) => x.contentSig ? x.contentSig !== c.sig : x.lastFetch !== c.fetchedAt;

function renderRows() {
  const body = document.getElementById('exts-body');
  if (!body) return;
  const rows = state.exts || [];
  // 内容没变就不重画:展开面板里的勾选、搜索、测速进度都在 DOM 里,整表重画会把它们抹掉。
  // 排除关键词、排序也要算进来:以前只改它们时指纹不变、面板不重画,缓存却被删了,之后点开关全都静默不保存
  const sig = rows.map(x => [x.id, x.name, x.type, x.value, x.prefix, x.remark, x.exclude, x.sort, x.nodeCount, x.nodeTotal,
    x.lastFetch, x.lastTry, x.lastError, x.contentSig, x.enabled, x.userCount].join('\u0001')).join('\u0002') + '|' + [...open].join(',');
  if (sig === rowsSig) return;
  rowsSig = sig;
  for (const id of open) { const w = document.getElementById('ext-wrap-' + id); if (w && w.scrollTop) keepTop[id] = w.scrollTop; }
  if (!rows.length) { body.innerHTML = `<tr><td colspan="8">${empty(t('ext.empty'))}</td></tr>`; return; }
  body.innerHTML = rows.map(x => rowHTML(x) + (open.has(x.id)
    ? `<tr class="ext-sub"><td colspan="8"><div class="ext-panel" id="ext-panel-${x.id}"><span class="muted small">${t('common.loading')}</span></div></td></tr>` : '')).join('');
  for (const id of open) {
    // 订阅内容变了:展开时拿的列表作废,重新拉。测速、连通结果按指纹记,进行中的任务也不停
    const x = rows.find(r => r.id === id);
    if (cache[id] && x && listChanged(cache[id], x)) delete cache[id];
    fillPanel(id);
  }
}

function rowHTML(x) {
  const on = open.has(x.id);
  const total = x.nodeTotal || 0, out = x.nodeCount || 0;
  const count = total > out
    ? `<span title="${esc(t('ext.countTitle', { out, n: total }))}">${out} <span class="muted small">/ ${total}</span></span>` : String(out);
  const fetched = x.lastError
    ? badge(t('ext.failed'), 'danger') + `<div class="sub-cell" title="${esc(trErr(x.lastError))}">${esc(trErr(x.lastError).slice(0, 60))}</div>`
      + (x.lastFetch ? `<div class="sub-cell muted">${t('ext.lastOk', { t: fmtRelative(x.lastFetch) })}</div>` : '')
    : (x.lastFetch ? fmtRelative(x.lastFetch) : '—');
  return `<tr>
    <td class="primary-cell"><div class="ext-name"><button class="btn sm ext-toggle" data-act="ext.toggle" data-id="${x.id}" title="${on ? t('ext.collapse') : t('ext.expand')}">${on ? '▾' : '▸'}</button><span>${esc(x.name)}</span></div>${x.prefix ? `<div class="sub-cell">${t('ext.prefix')}: ${esc(x.prefix)}</div>` : ''}${x.remark ? `<div class="sub-cell">${esc(x.remark)}</div>` : ''}</td>
    <td>${badge(x.type === 'sub' ? t('ext.typeSub') : t('ext.typeLink'), x.type === 'sub' ? 'primary' : '')}</td>
    <td class="mono small ext-url" title="${esc(x.value)}">${esc(x.value.length > 60 ? x.value.slice(0, 60) + '…' : x.value)}</td>
    <td class="num">${count}</td>
    <td>${fetched}</td>
    <td class="num">${x.userCount || 0}</td>
    <td>${badge(x.enabled ? t('common.enabled') : t('common.disabled'), x.enabled ? 'ok' : 'danger')}</td>
    <td class="actions">
      ${x.type === 'sub' ? `<button class="btn sm" data-act="ext.refresh" data-id="${x.id}">${t('ext.refresh')}</button>` : ''}
      <button class="btn sm" data-act="ext.edit" data-id="${x.id}">${t('common.edit')}</button>
      <button class="btn sm danger" data-act="ext.del" data-id="${x.id}">${t('common.delete')}</button>
    </td></tr>`;
}

// ---- 展开面板:节点子表 ----

// loadNodes 重新拉一遍节点列表。勾选里已经不在列表上的节点去掉。
function loadNodes(id) {
  if (!loading[id]) {
    loading[id] = get(`exts/${id}/nodes`).then(c => {
      cache[id] = c;
      const keys = new Set((c.nodes || []).map(n => n.key));
      if (picked[id]) picked[id] = new Set([...picked[id]].filter(k => keys.has(k)));
      return c;
    }).finally(() => { delete loading[id]; });
  }
  return loading[id];
}

async function fillPanel(id) {
  const box = document.getElementById('ext-panel-' + id);
  if (!box) return;
  if (!cache[id]) {
    try { await loadNodes(id); }
    catch (e) { const b = document.getElementById('ext-panel-' + id); if (b) b.innerHTML = `<div class="pc-err">${esc(e.message)}</div>`; return; }
  }
  const again = document.getElementById('ext-panel-' + id);
  if (!again || !cache[id]) return; // 等接口的功夫被收起了
  again.innerHTML = panelHTML(id, cache[id]);
  renderTable(id);
  const w = document.getElementById('ext-wrap-' + id);
  if (w && keepTop[id]) w.scrollTop = keepTop[id];
  if (focusNext === id) { focusNext = 0; if (again.scrollIntoView) again.scrollIntoView({ block: 'start' }); }
}

// 工具栏两行:第一行搜索、筛选、视图、服务器列;第二行是动作。子表放在自带滚动区里(表头吸顶、前两列吸左)
const FILTERS = ['all', 'ok', 'bad', 'hidden', 'sel'];
function panelHTML(id, c) {
  const stale = c.stale ? `<span class="muted small" title="${esc(t('ext.staleHelp'))}">${t('ext.staleN', { n: c.stale })} <a href="#" data-act="ext.cleanStale" data-id="${id}">${t('ext.cleanStale')}</a></span>` : '';
  const servers = c.servers || [], view = viewOf(c), f = filt[id] || 'all';
  const cols = new Set(colIds(c));
  const colMenu = view === 'grid' && servers.length > 1 ? `<details class="menu ext-cols" data-keep-open><summary class="btn sm">${t('ext.cols')} ▾</summary><div class="menu-list">
      <div class="row" style="padding:.2rem .4rem"><button type="button" class="btn xs" data-act="ext.colsAll" data-id="${id}">${t('cl.all')}</button><button type="button" class="btn xs" data-act="ext.colsNone" data-id="${id}">${t('cl.none')}</button></div>
      ${servers.map(x => `<label><input type="checkbox" data-change="ext.col" data-id="${id}" data-sid="${x.id}" ${cols.has(x.id) ? 'checked' : ''}> ${esc(x.name)}</label>`).join('')}
    </div></details>` : '';
  return `<div class="toolbar ext-tools">
      <input type="search" data-q="${id}" placeholder="${t('ext.search')}" value="${esc(query[id] || '')}">
      <div class="seg sm">${FILTERS.map(k => `<button data-act="ext.filter" data-id="${id}" data-f="${k}" class="${k === f ? 'active' : ''}">${t('ext.f.' + k)}</button>`).join('')}</div>
      ${servers.length > 1 ? `<div class="seg sm ext-view">${['sum', 'grid'].map(v => `<button data-act="ext.view" data-id="${id}" data-v="${v}" class="${v === view ? 'active' : ''}" title="${esc(t('ext.view.' + v + 'Help'))}">${t('ext.view.' + v)}</button>`).join('')}</div>` : ''}
      ${colMenu}
      <span class="muted small" id="ext-count-${id}"></span>${stale}<span class="grow"></span>
      <div class="ext-row2">
        <button class="btn sm" data-act="ext.hideSel" data-id="${id}" title="${esc(t('ext.hideHelp'))}">${t('ext.hideSel')}</button>
        <button class="btn sm" data-act="ext.showSel" data-id="${id}">${t('ext.showSel')}</button>
        <span class="grow"></span>
        <button class="btn sm" data-act="ext.testAll" data-id="${id}" title="${esc(t('ext.testAllHelp'))}">${t('ext.testAll')}</button>
        <label class="small muted"><input type="checkbox" data-change="ext.inclHidden" data-id="${id}" ${inclHidden[id] ? 'checked' : ''}> ${t('ext.inclHidden')}</label>
        <button class="btn sm" data-act="ext.testSel" data-id="${id}">${t('ext.testSel')}</button>
        <button class="btn sm danger" data-act="ext.testStop" data-id="${id}" id="ext-stop-${id}" ${jobs[id] && jobs[id].job ? '' : 'hidden'}>${t('ext.testStop')}</button>
        <button class="btn sm" data-act="ext.reachSel" data-id="${id}" title="${esc(t('ext.reachHelp'))}">${t('ext.reachSel')}</button>
        <button class="btn sm primary" data-act="ext.addUp" data-id="${id}">${t('ext.addUp')}</button>
      </div>
    </div>
    <div class="table-wrap fill ext-wrap" id="ext-wrap-${id}" style="--fill-reserve:9rem"></div>
    <div class="ext-more" id="ext-more-${id}" hidden></div>
    <p class="hint small muted">${t('ext.testHint')}</p>`;
}

// 视图、服务器列变了:所有展开的面板按新设置重画,子表滚动位置留着
function redrawPanels() {
  // 面板连工具栏一起重建:记下按着的是哪个按钮(切汇总 / 逐台、列菜单的全选 / 清空),重建后把焦点放回同一个按钮(在收起的菜单里就放回菜单)
  const a = document.activeElement;
  const was = a && a.dataset && a.dataset.act ? { act: a.dataset.act, id: a.dataset.id || '', v: a.dataset.v || '' } : null;
  for (const x of open) {
    const w = document.getElementById('ext-wrap-' + x);
    if (w) keepTop[x] = w.scrollTop;
    if (cache[x]) fillPanel(x);
  }
  if (was && !document.activeElement?.dataset?.act) {
    const b = [...document.querySelectorAll('[data-act]')].find(x => x.dataset.act === was.act && (x.dataset.id || '') === was.id && (x.dataset.v || '') === was.v);
    const menu = b && b.closest('details.menu:not([open])');
    (menu ? menu.querySelector('summary') : b)?.focus();
  }
}

// 表头随视图变:汇总一列「延迟」,逐台每台服务器一列(只列选中的服务器)
function renderTable(id) {
  const w = document.getElementById('ext-wrap-' + id), c = cache[id];
  if (!w || !c) return;
  const view = viewOf(c), so = sorts[id];
  const cols = view === 'grid' ? (c.servers || []).filter(x => colIds(c).includes(x.id)) : [];
  const sortTh = (k, label, cls = '') => `<th class="sortable ${cls} ${so && so.k === k ? 'sorted' : ''}" data-act="ext.sort" data-id="${id}" data-k="${k}">${label}${so && so.k === k ? (so.dir > 0 ? ' ▲' : ' ▼') : ''}</th>`;
  w.innerHTML = `<table class="grid ext-grid"><thead><tr>
      <th><input type="checkbox" data-change="ext.pickAll" data-id="${id}" title="${t('common.all')}"></th>
      ${sortTh('name', t('common.name'))}<th>${t('common.type')}</th><th>${t('ext.server')}</th>
      ${view === 'sum' ? sortTh('delay', t('ext.delay')) : cols.map(x => `<th class="num srv" title="${esc(x.name)}">${esc(x.name)}</th>`).join('')}<th>${t('ext.reachCol')}</th><th></th></tr></thead>
      <tbody id="ext-nodes-${id}"></tbody></table>`;
  renderSubBody(id);
}

// 搜索 → 筛选 → 排序。全选、测速选中都作用于筛选后的全部(不只是画出来的那一批)
const visible = (id, c) => {
  const q = (query[id] || '').trim(), f = filt[id] || 'all';
  const res = results[id] || {}, sel = picked[id] || new Set(), servers = c.servers || [];
  let list = (c.nodes || []).filter(n => !q || matches(q, n.name, n.rename || '', n.type, n.server + ':' + n.port));
  if (f !== 'all') {
    list = list.filter(n => {
      const row = res[n.key] || {};
      const cells = servers.map(x => row[x.id]).filter(Boolean);
      if (f === 'ok') return cells.some(x => x.state === 'ok');
      if (f === 'bad') return cells.some(x => x.state === 'fail');
      if (f === 'hidden') return !!n.hidden;
      return sel.has(n.key);
    });
  }
  const so = sorts[id];
  if (so) {
    const best = n => { const b = sumOf(res[n.key], servers).best; return b ? b.delayMs : Infinity; };
    list = list.slice().sort((a, b) => {
      if (so.k === 'name') return so.dir * String(a.rename || a.name).localeCompare(String(b.rename || b.name));
      const x = best(a), y = best(b); // 不通、没测的不管升序降序都排在后面
      if (x === Infinity || y === Infinity) return (x === Infinity) - (y === Infinity);
      return (x - y) * so.dir;
    });
  }
  return list;
};

// 一个节点在各服务器上的测速汇总:最快的那台、几台通、还有几台在测
function sumOf(row, servers) {
  let best = null, ok = 0, done = 0, pending = 0, tested = 0;
  for (const x of servers) {
    const c = row && row[x.id];
    if (!c) continue;
    tested++;
    if (c.state === 'pending') { pending++; continue; }
    if (c.state === 'skip' || c.state === 'cancelled' || c.state === 'unsupported') continue;
    done++;
    if (c.state === 'ok') { ok++; if (!best || c.delayMs < best.delayMs) best = { delayMs: c.delayMs, name: x.name }; }
  }
  return { best, ok, done, pending, tested };
}
// 汇总那一格:最快延迟 + 服务器名,下面小字"22/24 台通";全部不通标红;测速中显示进度。点开看各服务器延迟
function sumHTML(id, n, c) {
  const servers = c.servers || [];
  const s = sumOf((results[id] || {})[n.key], servers);
  if (!s.tested) return '<span class="muted">—</span>';
  if (s.pending) return `<span class="muted">… ${s.tested - s.pending}/${s.tested}</span>`;
  if (!s.done) return `<span class="muted">${t('ext.cellUnsupported')}</span>`;
  const top = s.best ? `<span class="badge ${delayTone(s.best.delayMs)}">${s.best.delayMs} ms</span>${servers.length > 1 ? ` <span class="muted small">${esc(s.best.name)}</span>` : ''}` : '<span class="badge danger">✗</span>';
  const sub = servers.length < 2 ? '' : s.ok ? `<span class="sub">${t('ext.okOf', { n: s.ok, m: s.done })}</span>` : `<span class="sub danger-text">${t('ext.allFail')}</span>`;
  return `<button type="button" class="link ext-sum" data-act="ext.detail" data-id="${id}" data-key="${esc(n.key)}" data-idx="${n.index}" title="${esc(t('ext.perServer'))}">${top}${sub}</button>`;
}

// 大陆连通那一格:判定 + 三网通的个数与提示放在悬停说明里
const reachHTML = r => {
  if (!r) return '<span class="muted">—</span>';
  if (r.state === 'pending') return '<span class="muted">…</span>';
  const notes = (r.note || '').split(',').filter(Boolean);
  const title = r.error ? trErr(r.error)
    : [(r.groups || []).filter(g => g.total).map(g => t('reach.g.' + g.key) + ' ' + g.ok + '/' + g.total).join(' · '), ...notes.map(n => t('ext.reachNote.' + n))].filter(Boolean).join('\n');
  let label = r.verdict === 'abroadDown' ? t('ext.reachBothDown') : t('reach.v.' + (r.verdict || 'none'));
  if (notes.includes('udpPing')) label += ' (ping)';
  else if (notes.length) label += ' *';
  return `<span class="badge ${REACH_TONE[r.verdict] || ''}" title="${esc(title)}">${esc(label)}</span>`;
};

// 测速那一格:延迟按快慢着色;测不了(协议不支持)和不通分开显示,悬停看原因
const delayTone = ms => ms <= 300 ? 'ok' : ms <= 800 ? 'warn' : 'danger';
const cellHTML = r => {
  if (!r) return '<span class="muted">—</span>';
  const why = esc(trErr(r.error || ''));
  switch (r.state) {
    case 'pending': return '<span class="muted">…</span>';
    case 'ok': return `<span class="badge ${delayTone(r.delayMs)}" title="${esc(r.at ? t('ext.testedAt', { t: fmtRelative(r.at) }) : '')}">${r.delayMs} ms</span>`;
    case 'unsupported': return `<span class="badge" title="${why}">${t('ext.cellUnsupported')}</span>`;
    case 'skip': return `<span class="muted" title="${why}">${t('ext.cellSkip')}</span>`;
    case 'cancelled': return `<span class="muted" title="${why}">${t('ext.cellCancelled')}</span>`;
  }
  return `<span class="badge danger" title="${why}">✗</span>`;
};

function renderSubBody(id) {
  const tb = document.getElementById('ext-nodes-' + id), c = cache[id];
  if (!tb || !c) return;
  const sel = picked[id] || (picked[id] = new Set());
  const all = visible(id, c), servers = c.servers || [];
  const lim = limits[id] || PAGE;
  const list = all.slice(0, lim);
  const view = viewOf(c);
  const cols = view === 'grid' ? servers.filter(x => colIds(c).includes(x.id)) : [];
  const res = results[id] || {}, rch = reaches[id] || {};
  tb.innerHTML = list.length ? list.map(n => {
    const k = esc(n.key);
    return `<tr class="${n.hidden ? 'ext-hidden' : ''}">
    <td><input type="checkbox" data-change="ext.pick" data-id="${id}" data-key="${k}" ${sel.has(n.key) ? 'checked' : ''}></td>
    <td class="primary-cell"><a href="#" data-act="ext.detail" data-id="${id}" data-key="${k}" data-idx="${n.index}">${esc(n.rename || n.name)}</a>
      <button class="btn xs ghost" data-act="ext.rename" data-id="${id}" data-key="${k}" data-idx="${n.index}" title="${t('ext.rename')}">✎</button>
      <label class="switch sm" title="${n.keyword ? t('ext.hiddenByKeyword') : t('ext.output')}"><input type="checkbox" data-change="ext.toggleNode" data-id="${id}" data-key="${k}" data-idx="${n.index}" ${n.hidden ? '' : 'checked'} ${n.keyword ? 'disabled' : ''}><span></span></label>
      ${n.rename ? `<div class="sub-cell">${t('ext.renamedFrom', { name: esc(n.name) })}</div>` : ''}
      ${n.keyword ? `<div class="sub-cell">${t('ext.hiddenByKeyword')}</div>` : ''}</td>
    <td>${badge(esc(n.type))}</td>
    <td class="mono small">${esc(hostPort(n.server, n.port))}</td>
    ${view === 'sum' ? `<td data-sum="${id}-${k}">${sumHTML(id, n, c)}</td>` : cols.map(x => `<td class="num srv" data-cell="${id}-${k}-${x.id}">${cellHTML(res[n.key] && res[n.key][x.id])}</td>`).join('')}
    <td data-reach="${id}-${k}">${reachHTML(rch[n.key])}</td>
    <td>${n.upstream ? '' : `<span class="badge warn" title="${esc(trErr(n.reason || ''))}">${t('ext.noUpstream')}</span>`}</td></tr>`;
  }).join('')
    : `<tr><td colspan="${6 + (view === 'sum' ? 1 : cols.length)}">${empty(t('ext.noNodes'))}</td></tr>`;
  const more = document.getElementById('ext-more-' + id);
  if (more) {
    more.hidden = all.length <= lim;
    more.innerHTML = all.length > lim ? `<button class="btn sm" data-act="ext.more" data-id="${id}">${t('ext.more', { n: Math.min(PAGE, all.length - lim), left: all.length - lim })}</button>` : '';
  }
  updateCount(id);
}

function updateCount(id) {
  const cnt = document.getElementById('ext-count-' + id), c = cache[id];
  if (!cnt || !c) return;
  const sel = picked[id];
  const out = (c.nodes || []).filter(n => !n.hidden).length;
  const shown = (filt[id] || 'all') !== 'all' || (query[id] || '').trim() ? visible(id, c).length : -1;
  cnt.textContent = t('ext.countOut', { n: (c.nodes || []).length, out })
    + (shown >= 0 ? ' · ' + t('ext.shown', { n: shown }) : '')
    + (sel && sel.size ? ' · ' + t('ext.selected', { n: sel.size }) : '')
    + (c.fetchedAt ? ' · ' + t('ext.lastFetch') + ' ' + fmtRelative(c.fetchedAt) : '');
}

// findNode 按指纹找节点:先看下标对不对得上(同一份列表),对不上再按指纹找(列表刷新过)。
function findNode(c, key, idx) {
  const nodes = (c && c.nodes) || [];
  const byIdx = nodes[idx];
  return byIdx && byIdx.key === key ? byIdx : nodes.find(n => n.key === key);
}

// ---- 节点详情(抽屉)----

function showDetail(id, key, idx) {
  const n = findNode(cache[id], key, idx);
  if (!n) return;
  const rows = (n.fields || []).map(f => `<tr><td class="muted">${esc(f.k)}</td><td class="mono small ext-val">${esc(f.v)}</td></tr>`).join('');
  const link = n.link
    ? `<div class="ext-link"><div class="muted small">${t('ext.link')}</div><div class="mono small ext-val">${esc(n.link)}</div><div><button class="btn sm" data-act="ext.copy" data-link="${esc(n.link)}">${t('common.copy')}</button></div></div>`
    : `<p class="muted small">${t('ext.noLink')}</p>`;
  const servers = (cache[id] && cache[id].servers) || [], row = (results[id] || {})[n.key] || {};
  const tested = servers.filter(x => row[x.id]);
  const rank = x => { const c = row[x.id]; return c.state === 'ok' ? c.delayMs : c.state === 'pending' ? 1e9 : 2e9; };
  const per = tested.length ? `<h3 style="margin:.4rem 0 .3rem">${t('ext.perServer')}</h3><div class="table-wrap"><table class="grid kv"><tbody>${tested.slice().sort((a, b) => rank(a) - rank(b)).map(x => `<tr><td>${esc(x.name)}${x.isLocal ? ` <span class="muted small">(${t('node.local')})</span>` : ''}</td><td>${cellHTML(row[x.id])}</td></tr>`).join('')}</tbody></table></div>` : '';
  openDrawer(esc(n.name), `<p class="small muted">${badge(esc(n.type))} ${esc(hostPort(n.server, n.port))}${n.upstream ? '' : ' · ' + t('ext.noUpstream') + (n.reason ? ':' + esc(trErr(n.reason)) : '')}</p>
    ${per}<div class="table-wrap"><table class="grid kv"><tbody>${rows}</tbody></table></div>${link}`, { wide: tested.length > 0 });
}

// ---- 测速:起任务,每秒取进度,一格一格填。结果按指纹记,列表刷新也不丢 ----

function syncStop(id) {
  const b = document.getElementById('ext-stop-' + id);
  if (b) b.hidden = !(jobs[id] && jobs[id].job);
}

async function startTest(id, keys) {
  if (jobs[id]) { toast(t('ext.testRunning'), 'err'); return; }
  jobs[id] = {}; // 请求还没回来也算在跑:防连点起两个
  let c, r;
  try {
    c = cache[id] || await loadNodes(id);
    r = await post(`exts/${id}/nodes/test`, { keys, includeHidden: !!inclHidden[id] }, SLOW);
  } catch (e) {
    delete jobs[id];
    toast(e.message, 'err');
    if (e.status === 409) reloadPanel(id);
    return;
  }
  const res = results[id] || (results[id] = {});
  for (const k of r.keys || []) {
    res[k] = {};
    for (const s of c.servers || []) res[k][s.id] = { state: 'pending' };
  }
  renderSubBody(id);
  toast(t('ext.testStarted', { n: (r.keys || []).length, s: (c.servers || []).length }), 'ok');
  const started = Date.now();
  let errors = 0;
  // 上一次进度回来了再约下一次:以前 setInterval 每秒一发不等返回,弱网下请求越积越多(审计 MB31)。出错退避重试几次
  const poll = async () => {
    let st;
    try { st = await get(`exts/${id}/nodes/test/${r.job}`); errors = 0; }
    catch (e) {
      if (!jobs[id] || jobs[id].job !== r.job) return;
      if (e.status === 404 || ++errors > 5) { stopJob(id); toast(e.message, 'err'); return; }
      jobs[id].timer = setTimeout(poll, 1000 * 2 ** errors);
      return;
    }
    if (!jobs[id] || jobs[id].job !== r.job) return; // 这期间被停了
    applyResults(id, st.results);
    if (st.done || Date.now() - started > 15 * 60 * 1000) {
      stopJob(id);
      renderSubBody(id); // 收尾整表重画一遍:中途重画过的面板也拿到全部结果
      toast(tallyText(st.results), tally(st.results).bad ? '' : 'ok');
      return;
    }
    jobs[id].timer = setTimeout(poll, 1000);
  };
  jobs[id] = { job: r.job, timer: setTimeout(poll, 1000) };
  syncStop(id);
}

async function stopTest(id) {
  const j = jobs[id];
  if (!j || !j.job) return;
  try {
    const st = await del(`exts/${id}/nodes/test/${j.job}`);
    applyResults(id, st.results);
    stopJob(id);
    toast(t('ext.testStopped'), 'ok');
  } catch (e) { toast(e.message, 'err'); }
}

// reloadPanel 节点列表已经变了:丢掉旧的,重新拉一遍(勾选、测速结果按指纹记,还对得上的留着)。
function reloadPanel(id) { delete cache[id]; fillPanel(id); }
function stopJob(id) { if (jobs[id]) { clearTimeout(jobs[id].timer); delete jobs[id]; } syncStop(id); }
function applyResults(id, rs) {
  const res = results[id] || (results[id] = {});
  const c = cache[id];
  for (const [key, row] of Object.entries(rs || {})) {
    res[key] = Object.assign(res[key] || {}, row);
    for (const [sid, cell] of Object.entries(row || {})) {
      document.querySelectorAll(`[data-cell="${id}-${key}-${sid}"]`).forEach(td => { td.innerHTML = cellHTML(cell); });
    }
    const n = c && (c.nodes || []).find(x => x.key === key);
    if (n) document.querySelectorAll(`[data-sum="${id}-${key}"]`).forEach(td => { td.innerHTML = sumHTML(id, n, c); });
  }
}
function tally(rs) {
  let ok = 0, bad = 0, na = 0;
  for (const row of Object.values(rs || {})) for (const c of Object.values(row || {})) {
    if (c.state === 'ok') ok++; else if (c.state === 'fail') bad++; else if (c.state === 'unsupported') na++;
  }
  return { ok, bad, na };
}
function tallyText(rs) {
  const { ok, bad, na } = tally(rs);
  return t('ext.testDone', { ok, bad }) + (na ? ' · ' + t('ext.testNA', { n: na }) : '');
}

// ---- 隐藏 / 改名:只发这一次的增量,服务端按名字 + 指纹合并 ----

// savePrefs 发增量;版本号对不上(别的标签页刚改过)就重新拉一遍再试一次。成功后以服务端返回的状态为准。
async function savePrefs(id, ops) {
  let c = cache[id] || await loadNodes(id);
  for (let attempt = 0; ; attempt++) {
    try {
      const r = await put(`exts/${id}/nodes/prefs`, { rev: c.rev, ...ops });
      cache[id] = r;
      refreshList();
      return r;
    } catch (e) {
      if (e.status !== 409 || attempt > 0) throw e;
      c = await loadNodes(id);
    }
  }
}

// refreshList 列表上的节点数是输出的个数,隐藏改名后跟着变。
function refreshList() { load('exts').then(renderRows).catch(() => { /* 下一轮轮询再说 */ }); }

// setHidden 隐藏 / 显示。缓存不在(刚编辑过、列表刚刷新)就先重新拉,按指纹找回节点再保存;
// 找不到就明说"列表已更新",绝不静默不保存 —— 以前这里直接 return,开关拨过去了却没存,下次刷新又弹回来。
async function setHidden(id, keys, hidden) {
  try {
    const c = cache[id] || await loadNodes(id);
    const nodes = (c.nodes || []).filter(n => keys.includes(n.key) && !n.keyword);
    if (!nodes.length) {
      toast(t(keys.length ? 'ext.listChanged' : 'ext.pickFirst'), 'err');
      fillPanel(id); // 开关按服务端状态画回去
      return;
    }
    const ops = nodes.map(n => ({ name: n.name, key: n.key }));
    await savePrefs(id, hidden ? { hide: ops } : { show: ops });
    fillPanel(id);
    toast(t('ext.prefsSaved') + (hidden && nodes.some(n => DYNAMIC.test(n.name)) ? ' · ' + t('ext.dynamicHint') : ''), 'ok');
  } catch (e) {
    fillPanel(id);
    toast(e.message, 'err');
  }
}

async function renameNode(id, key, idx) {
  let c;
  try { c = cache[id] || await loadNodes(id); }
  catch (e) { toast(e.message, 'err'); return; }
  const n = findNode(c, key, idx);
  if (!n) { toast(t('ext.listChanged'), 'err'); fillPanel(id); return; }
  openModal(t('ext.rename'), `<div class="form-grid"><div class="full">${field(t('ext.renameTo'), `<input id="f-rename" value="${esc(n.rename || n.name)}">`, t('ext.renameHelp', { name: esc(n.name) }))}</div></div>`, async () => {
    const to = fv('f-rename').trim();
    await savePrefs(id, { rename: [{ name: n.name, key: n.key, to: to === n.name ? '' : to }] });
    fillPanel(id);
    toast(t('ext.prefsSaved'), 'ok');
  });
}

// ---- 大陆连通:只测勾选的;一轮没结束不能再点(每次都扣测点额度) ----

async function startReach(id, keys) {
  if (!keys.length) { toast(t('ext.pickFirst'), 'err'); return; }
  if (reachJobs[id]) { toast(t('ext.reachRunning'), 'err'); return; }
  reachJobs[id] = {};
  let r;
  try { r = await post(`exts/${id}/nodes/reach`, { keys }, SLOW); }
  catch (e) { delete reachJobs[id]; toast(e.message, 'err'); if (e.status === 409) reloadPanel(id); return; }
  const rch = reaches[id] || (reaches[id] = {});
  for (const k of r.keys || []) rch[k] = { state: 'pending' };
  renderSubBody(id);
  toast(t('ext.reachStarted', { n: (r.keys || []).length }), 'ok');
  const started = Date.now();
  let errors = 0;
  const poll = async () => {
    let st;
    try { st = await get(`exts/${id}/nodes/reach/${r.job}`); errors = 0; }
    catch (e) {
      if (!reachJobs[id] || reachJobs[id].job !== r.job) return;
      if (e.status === 404 || ++errors > 5) { delete reachJobs[id]; toast(e.message, 'err'); return; }
      reachJobs[id].timer = setTimeout(poll, 2000 * 2 ** errors); // 出错退避重试,不是一错就停
      return;
    }
    if (!reachJobs[id] || reachJobs[id].job !== r.job) return;
    for (const [key, cell] of Object.entries(st.results || {})) { // 按指纹回填:列表刷新过也不会贴到别的节点上
      rch[key] = cell;
      document.querySelectorAll(`[data-reach="${id}-${key}"]`).forEach(td => { td.innerHTML = reachHTML(cell); });
    }
    if (st.done || Date.now() - started > 15 * 60 * 1000) { delete reachJobs[id]; toast(t('ext.reachDone'), 'ok'); return; }
    reachJobs[id].timer = setTimeout(poll, 2000);
  };
  reachJobs[id] = { job: r.job, timer: setTimeout(poll, 2000) };
}

// ---- 添加为上游 ----

async function addUpstreams(id) {
  const sel = picked[id];
  if (!sel || !sel.size) { toast(t('ext.pickFirst'), 'err'); return; }
  let c;
  try { c = cache[id] || await loadNodes(id); }
  catch (e) { toast(e.message, 'err'); return; }
  const ext = (state.exts || []).find(x => x.id === id) || {};
  const list = [...sel].map(k => (c.nodes || []).find(n => n.key === k)).filter(Boolean);
  if (!list.length) { toast(t('ext.listChanged'), 'err'); fillPanel(id); return; }
  const html = `<p class="hint">${t('ext.addUpHelp')}</p><div class="form-grid">` + list.map(n => n.upstream
    ? field(n.name, `<input id="ext-up-${esc(n.key)}" value="${esc((ext.name ? ext.name + '-' : '') + (n.rename || n.name))}">`)
    : field(n.name, `<span class="muted small" title="${esc(trErr(n.reason || ''))}">${t('ext.noUpstream')}</span>`)).join('') + '</div>';
  openModal(t('ext.addUp'), html, async () => {
    const items = list.filter(n => n.upstream).map(n => ({ key: n.key, name: fv(`ext-up-${n.key}`).trim() }));
    if (!items.length) throw new Error(t('ext.noneAddable'));
    if (items.some(x => !x.name)) throw new Error(t('ext.nameRequired'));
    let r;
    try { r = await post(`exts/${id}/nodes/add-upstream`, { items }, SLOW); }
    catch (e) { if (e.status === 409) reloadPanel(id); throw e; }
    const okN = (r.results || []).filter(x => x.ok).length, bad = (r.results || []).filter(x => !x.ok);
    if (okN) { try { await load('upstreams'); } catch (_) { /* 上游页没打开过也没关系 */ } picked[id] = new Set(); renderSubBody(id); }
    // 逐条结果另开一个框:成功的已经建好了,失败的连同原因列出来,别让人再提交一遍撞重名
    setTimeout(() => openModal(t('ext.addUpResult'), `<p>${t('ext.addUpDone', { n: okN })}${bad.length ? ' · ' + t('ext.addUpFailedN', { n: bad.length }) : ''}</p>
      <ul class="ext-results">${(r.results || []).map(x => `<li>${x.ok ? badge('OK', 'ok') : badge('✗', 'danger')} ${esc(x.name)}${x.error ? ` <span class="muted small">${esc(trErr(x.error))}</span>` : ''}</li>`).join('')}</ul>
      ${okN ? `<p><a href="#/upstreams">${t('ext.toUpstreams')}</a></p>` : ''}`, null), 0);
  }, { wide: true, saveText: t('ext.addUp') });
}

// ---- 新增 / 编辑 ----

function editExt(id) {
  const x = id ? state.exts.find(e => e.id === id) : { type: 'sub', enabled: true };
  openModal(id ? t('ext.edit') : t('ext.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(x.name || '')}" placeholder="${t('ext.namePh')}">`)}
      ${field(t('common.type'), `<select id="f-type"><option value="sub" ${x.type === 'sub' ? 'selected' : ''}>${t('ext.typeSub')}</option><option value="link" ${x.type === 'link' ? 'selected' : ''}>${t('ext.typeLink')}</option></select>`, t('ext.typeHelp'))}
      <div class="full">${field(t('ext.value'), `<textarea id="f-value" style="min-height:5rem">${esc(x.value || '')}</textarea>`, t('ext.valueHelp'))}</div>
      ${field(t('ext.prefix'), `<input id="f-prefix" value="${esc(x.prefix || '')}" placeholder="[中转] ">`, t('ext.prefixHelp'))}
      ${field(t('ext.exclude'), `<input id="f-exclude" value="${esc(x.exclude || '')}" placeholder="${esc(t('ext.excludePh'))}">`, t('ext.excludeHelp'))}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(x.remark || '')}">`)}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${x.sort || 0}">`)}
      ${check('f-enabled', t('common.enabled'), x.enabled !== false)}
    </div>`, async () => {
    const body = { name: fv('f-name').trim(), type: fv('f-type'), value: fv('f-value').trim(), prefix: fv('f-prefix'), exclude: fv('f-exclude').trim(), remark: fv('f-remark'), sort: Number(fv('f-sort')) || 0, enabled: fchk('f-enabled') };
    if (id) await put('exts/' + id, body); else await post('exts', body);
    // 地址、关键词可能改了:展开的列表作废、行指纹作废,一定重画(以前只删缓存不重画,面板留着、之后的开关全都不保存)
    delete cache[id];
    rowsSig = '';
    await load('exts'); renderRows();
    toast(id ? t('ext.updated') : t('ext.created'), 'ok');
  }, { wide: true });
}

registerActions({
  'ext.add': () => editExt(null),
  'ext.edit': id => editExt(Number(id)),
  'ext.refresh': async (id, btn) => {
    btn.disabled = true;
    try {
      const r = await post(`exts/${id}/refresh`, undefined, SLOW);
      toast(t(r.unchanged ? 'ext.refreshedSame' : 'ext.refreshed', { n: r.total, out: r.out }), 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; await load('exts'); rowsSig = ''; renderRows(); }
  },
  'ext.del': async id => {
    const x = state.exts.find(e => e.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: x.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('exts/' + id); open.delete(Number(id)); await load('exts', 'users'); renderRows(); toast(t('ext.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'ext.toggle': id => { id = Number(id); if (open.has(id)) open.delete(id); else { open.add(id); focusNext = id; } renderRows(); },
  'ext.filter': (id, el) => {
    id = Number(id); filt[id] = el.dataset.f;
    el.parentNode.querySelectorAll('button').forEach(b => b.classList.toggle('active', b === el));
    renderSubBody(id);
  },
  'ext.view': (id, el) => { lsSet(LS_VIEW, el.dataset.v); redrawPanels(); },
  'ext.col': (id, el) => {
    id = Number(id);
    const box = el.closest('.menu-list');
    const ids = box ? [...box.querySelectorAll('input[data-sid]')].filter(x => x.checked).map(x => Number(x.dataset.sid)) : [];
    lsSet(LS_COLS, JSON.stringify(ids));
    for (const x of open) renderTable(x);
  },
  'ext.colsAll': id => { lsSet(LS_COLS, 'null'); redrawPanels(); },
  'ext.colsNone': id => { const c = cache[Number(id)]; lsSet(LS_COLS, JSON.stringify(c && c.servers && c.servers.length ? [c.servers[0].id] : [])); redrawPanels(); },
  'ext.sort': (id, el) => {
    id = Number(id);
    const k = el.dataset.k, cur = sorts[id];
    sorts[id] = cur && cur.k === k ? (cur.dir > 0 ? { k, dir: -1 } : undefined) : { k, dir: 1 }; // 升序 → 降序 → 原顺序
    renderTable(id);
  },
  'ext.more': id => { id = Number(id); limits[id] = (limits[id] || PAGE) + PAGE; renderSubBody(id); },
  'ext.pick': (id, el) => { const s = picked[id] || (picked[id] = new Set()); if (el.checked) s.add(el.dataset.key); else s.delete(el.dataset.key); updateCount(Number(id)); },
  'ext.pickAll': (id, el) => {
    id = Number(id); const c = cache[id]; if (!c) return;
    const s = picked[id] || (picked[id] = new Set());
    for (const n of visible(id, c)) { if (el.checked) s.add(n.key); else s.delete(n.key); }
    renderSubBody(id);
  },
  'ext.detail': (id, el, e) => { if (e) e.preventDefault(); showDetail(Number(id), el.dataset.key, Number(el.dataset.idx)); },
  'ext.copy': async (id, el) => { await copy(el.dataset.link || ''); toast(t('common.copied'), 'ok'); },
  'ext.toggleNode': (id, el) => setHidden(Number(id), [el.dataset.key], !el.checked),
  'ext.hideSel': id => { id = Number(id); const s = picked[id]; if (!s || !s.size) { toast(t('ext.pickFirst'), 'err'); return; } setHidden(id, [...s], true); },
  'ext.showSel': id => { id = Number(id); const s = picked[id]; if (!s || !s.size) { toast(t('ext.pickFirst'), 'err'); return; } setHidden(id, [...s], false); },
  'ext.reachSel': id => { id = Number(id); startReach(id, [...(picked[id] || [])]); },
  'ext.rename': (id, el, e) => { if (e) e.preventDefault(); renameNode(Number(id), el.dataset.key, Number(el.dataset.idx)); },
  'ext.cleanStale': async (id, el, e) => {
    if (e) e.preventDefault();
    id = Number(id);
    if (!await confirm(t('ext.cleanStaleConfirm', { n: (cache[id] || {}).stale || 0 }))) return;
    try { await savePrefs(id, { clearStale: true }); fillPanel(id); toast(t('ext.prefsSaved'), 'ok'); }
    catch (err) { toast(err.message, 'err'); }
  },
  'ext.inclHidden': (id, el) => { inclHidden[Number(id)] = el.checked; },
  'ext.testAll': id => startTest(Number(id), []),
  'ext.testSel': id => { id = Number(id); const s = picked[id]; if (!s || !s.size) { toast(t('ext.pickFirst'), 'err'); return; } startTest(id, [...s]); },
  'ext.testStop': id => stopTest(Number(id)),
  'ext.addUp': id => addUpstreams(Number(id)),
});

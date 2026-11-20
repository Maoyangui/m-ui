// 「防火墙」页。三块,从上到下是三个不同层面的事,各管各的:
//
//   1. 系统防火墙 —— 每台服务器自带的那一层(ufw / firewalld / 镜像自带的 iptables 规则 / 自己写的 nftables)。
//      「云控制台放行了、外面还是连不上」几乎都是它。一行一台:它在不在拦、m-ui 用到的端口在它那里放行了没有、
//      面板规则落了几条;在拦的可以「放开」(可撤销)。右上角的开关:留着它时,m-ui 的端口自动在它那里放行。
//   2. 规则 —— 自己定的三种:放行端口、拉黑来源、端口只给指定来源。下发到选中的服务器,在内核里生效(代理流量也管),
//      可以设有效期,到点自动撤掉。行里的开关点了就存。
//   3. 黑白名单 —— 只管面板、订阅、外部 API 的访问(应用层直接断开),代理流量不受影响。自动拉黑是个开关,默认关。
//
// 版式上的几条(都是在别的页面上被否过的,这里从一开始就照着做):常用的开关摆在卡片标题右边,不藏进弹框;
// 展开的明细(一台服务器用到的端口)不画在单元格里,是该行下面的一条通栏;技术性的报错放悬停,展开后才写出来;
// 时间一律「多久前 / 还剩多久」,完整时间放悬停;空表不放一个大空方块,一行字带上最常用的几种做法。
import { state } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, toast, confirm, openModal, registerActions, badge, field, fieldBox, fv, setHTML, fmtDate, fmtRelative, matches, debounce } from '../ui.js';

export const title = () => t('fw.title');
export const subtitle = () => t('fw.subtitle');

let data = null;          // GET fw 的回复
let tab = 'ban';          // 黑白名单卡里现在看哪一张:ban / allow
const opened = new Set(); // 展开看端口的服务器 id
const portsOf = {};       // 服务器 id → 端口清单(副机的要点开才问;本机在总览里就带着)
const busy = new Set();   // 正在检查 / 放开 / 撤销的服务器 id
let banShown = 30;        // 黑名单先列这么多条,多的点「再显示」
let banQ = '';            // 黑名单里搜

const FRONT = { ufw: 'ufw', firewalld: 'firewalld', iptables: 'iptables', nftables: 'nftables' };
const now = () => Date.now() / 1000;
const readOnly = () => state.status.role === 'node';
const nodes = () => state.nodes || [];
const nodeName = id => (nodes().find(n => n.id === id) || (data ? data.servers.find(s => s.id === id) : null) || {}).name || '#' + id;
const idsOf = v => { try { const x = typeof v === 'string' ? JSON.parse(v) : v; return Array.isArray(x) ? x : []; } catch { return []; } };
const bare = s => String(s).replace(/\/32$/, '').replace(/\/128$/, ''); // 单个地址不带掩码给人看
const listOf = s => String(s || '').split(',').map(x => x.trim()).filter(Boolean);

// 「还剩多久」「多久之后」:不到一小时说分钟,不到两天说小时,再长说天
function span(sec) {
  if (sec < 90) return ['m', 1];
  if (sec < 3600) return ['m', Math.round(sec / 60)];
  if (sec < 48 * 3600) return ['h', Math.round(sec / 3600)];
  return ['d', Math.round(sec / 86400)];
}
const leftText = exp => { const [u, n] = span(exp - now()); return t('fw.left.' + u, { n }); };
const inText = exp => { const [u, n] = span(exp - now()); return t('fw.in.' + u, { n }); };

async function reload() { data = await get('fw'); }
export async function tick() {
  if (document.getElementById('modal') && !document.getElementById('modal').hidden) return; // 弹框开着时不重画,免得把正在填的东西冲掉
  try { await reload(); draw(); } catch { /* 这一轮没取到:留着上一轮的 */ }
}

export async function render(el) {
  el.innerHTML = `<div class="wall-page"><section class="card" id="wall-sys"></section><section class="card" id="wall-rules"></section><section class="card" id="wall-lists"></section></div>`;
  await reload();
  for (const s of data.servers) if (s.ports) portsOf[s.id] = s.ports;
  draw();
}

function draw() {
  if (!data || !document.getElementById('wall-sys')) return;
  for (const s of data.servers) if (s.ports) portsOf[s.id] = s.ports;
  setHTML('wall-sys', sysHTML());
  setHTML('wall-rules', rulesHTML());
  setHTML('wall-lists', listsHTML());
}

// ---- 1. 系统防火墙 ----

const frontName = f => FRONT[f] || f || '';

// 一台服务器「系统防火墙」那一格
function sysCell(s) {
  const st = s.state;
  if (!s.enabled) return `<span class="muted">${esc(t('common.disabled'))}</span>`;
  if (!st) return s.online ? `<span class="muted" title="${esc(t('fw.sys.noReportTip'))}">${esc(t('fw.sys.noReport'))}</span>` : `<span class="muted">${esc(t('common.offline'))}</span>`;
  if (!st.at) return `<span class="muted">${esc(t('fw.sys.checking'))}</span>`;
  if (st.opened) return `${badge(t('fw.sys.opened'), 'ok')}${st.openedFrom && st.openedFrom !== 'none' ? ` <span class="muted small">${esc(t('fw.sys.was', { name: frontName(st.openedFrom) }))}</span>` : ''}`;
  if (st.blocking) return `<span title="${esc(st.detail || '')}">${badge(t(st.frontend === 'ufw' || st.frontend === 'firewalld' ? 'fw.sys.on' : 'fw.sys.rules', { name: frontName(st.frontend) }), 'warn')}</span>`;
  return `<span class="muted" title="${esc(t('fw.sys.noneTip'))}">${esc(t('fw.sys.none'))}</span>`;
}
// 「m-ui 的端口」那一格:几个 + 在系统防火墙那里的状况;点开看是哪些
function portsCell(s) {
  const st = s.state;
  if (!st || !st.at) return '<span class="muted">—</span>';
  const how = !st.blocking ? ['muted', 'fw.ports.direct'] : st.allow === 'ok' ? ['ok-text', 'fw.ports.ok']
    : st.allow === 'fail' ? ['danger-text', 'fw.ports.fail'] : st.allow === 'manual' ? ['warn-text', 'fw.ports.manual'] : ['warn-text', 'fw.ports.off'];
  const isOpen = opened.has(s.id);
  return `<button type="button" class="link-btn wall-pbtn" data-act="fw.expand" data-id="${s.id}" aria-expanded="${isOpen}" title="${esc(st.allow === 'fail' ? trErr(st.error || '') : t('fw.ports.tip'))}">
    <b>${esc(t('fw.ports.n', { n: st.ports || 0 }))}</b><span class="${how[0]}">${esc(t(how[1]))}</span><i class="i i-chev${isOpen ? ' open' : ''}"></i></button>`;
}
function rulesCell(s) {
  const st = s.state;
  if (!st || !st.at) return '<span class="muted">—</span>';
  if (st.paused) return `<span title="${esc(t('fw.rules.pausedTip'))}">${badge(t('fw.rules.paused'), 'warn')}</span>`;
  if (!st.engine && st.rules > 0) return `<span class="danger-text" title="${esc(t('fw.d.rules.noengine'))}">${esc(t('fw.rules.noengine'))}</span>`;
  if (st.error && st.allow !== 'fail') return `<span class="danger-text" title="${esc(trErr(st.error))}">${esc(t('fw.rules.failed'))}</span>`;
  return st.rules > 0 ? esc(t('fw.rules.n', { n: st.rules })) : '<span class="muted">—</span>';
}
function actCell(s) {
  const st = s.state;
  if (readOnly() || !st || !st.at || !s.enabled) return '';
  if (busy.has(s.id)) return `<span class="muted small">${esc(t('fw.working'))}</span>`;
  if (st.opened) return `<button class="btn sm" data-act="fw.undo" data-id="${s.id}">${esc(t('fw.act.undo'))}</button>`;
  if (st.blocking && st.frontend !== 'nftables') return `<button class="btn sm" data-act="fw.open" data-id="${s.id}">${esc(t('fw.act.open'))}</button>`;
  return '';
}
// 展开的那一条通栏:这台用到的端口,和几句大白话(系统防火墙现在怎样、放行得怎样、规则落在哪)
function srvDetail(s) {
  const st = s.state || {}, list = portsOf[s.id];
  const chips = !list ? `<span class="muted small">${esc(t('fw.d.loading'))}</span>`
    : list.length ? list.map(p => `<span class="chip mono">${esc(p)}</span>`).join('') : `<span class="muted small">${esc(t('fw.d.noports'))}</span>`;
  const lines = [];
  if (st.opened) lines.push(['', t('fw.d.sys.opened', { when: st.openedAt ? fmtRelative(st.openedAt) : '', name: frontName(st.openedFrom) || '—' })]);
  else if (st.blocking) lines.push(['', t('fw.d.sys.on', { name: frontName(st.frontend) })]);
  else lines.push(['', t('fw.d.sys.none')]);
  if (st.blocking) {
    if (st.allow === 'ok') lines.push(['ok', t('fw.d.allow.ok', { n: st.allowed || 0 })]);
    else if (st.allow === 'fail') lines.push(['bad', t('fw.d.allow.fail', { msg: trErr(st.error || '') })]);
    else if (st.allow === 'manual') lines.push(['warn', t('fw.d.allow.manual')]);
    else lines.push(['warn', t('fw.d.allow.off')]);
  }
  if (st.paused) lines.push(['warn', t('fw.rules.pausedTip')]);
  else if (!st.engine && st.rules > 0) lines.push(['bad', t('fw.d.rules.noengine')]);
  else if (st.error && st.allow !== 'fail') lines.push(['bad', t('fw.d.rules.err', { msg: trErr(st.error) })]);
  else if (st.rules > 0) lines.push(['', t('fw.d.rules', { n: st.rules, where: t('fw.d.where.' + (st.engine || 'none')) })]);
  return `<div class="wall-detail">
    <div class="wall-dports"><span class="wall-dl">${esc(t('fw.d.ports'))}</span><span class="chips">${chips}</span></div>
    ${lines.map(([cls, text]) => `<p class="wall-dline${cls ? ' ' + cls : ''}">${esc(text)}</p>`).join('')}
    ${st.at ? `<p class="wall-dmeta">${esc(t('fw.d.checked', { when: fmtRelative(st.at) }))}${st.detail ? ' · ' + esc(st.detail) : ''}</p>` : ''}
  </div>`;
}
function sysHTML() {
  const ro = readOnly();
  const rows = data.servers.map(s => {
    const isOpen = opened.has(s.id);
    return `<tr class="${isOpen ? 'up-open' : ''}">
      <td class="primary-cell"><span class="pname" title="${esc(s.name)}">${esc(s.name)}</span>${s.isLocal ? ` ${badge(t('fw.local'), 'primary')}` : ''}</td>
      <td>${sysCell(s)}</td>
      <td>${portsCell(s)}</td>
      <td class="c-p2">${rulesCell(s)}</td>
      <td class="actions">${actCell(s)}</td>
    </tr>${isOpen ? `<tr class="up-sub wall-sub"><td colspan="5">${srvDetail(s)}</td></tr>` : ''}`;
  }).join('');
  return `<div class="card-head"><h2>${esc(t('fw.sys.title'))}</h2>
      <div class="wall-tools"><label class="wall-sw" title="${esc(t('fw.autoAllow.tip'))}"><span>${esc(t('fw.autoAllow'))}</span><span class="switch"><input type="checkbox" data-change="fw.setting" data-id="autoAllow" ${data.autoAllow ? 'checked' : ''} ${ro ? 'disabled' : ''}><span></span></span></label>
      <button class="btn sm" data-act="fw.recheck" ${busy.size ? 'disabled' : ''}>${esc(t('fw.recheck'))}</button></div></div>
    <p class="hint xp">${esc(t('fw.sys.hint'))}</p>
    <div class="table-wrap"><table class="grid wall-srv" data-fit="p2:640 cards:520">
      <thead><tr><th>${esc(t('fw.col.server'))}</th><th>${esc(t('fw.col.sys'))}</th><th>${esc(t('fw.col.ports'))}</th><th class="c-p2">${esc(t('fw.col.rules'))}</th><th></th></tr></thead>
      <tbody>${rows}</tbody></table></div>`;
}

// ---- 2. 规则 ----

const KIND_BADGE = { allow: 'ok', deny: 'danger', only: 'primary' };
const protoText = p => (p === 'tcp' ? 'TCP' : p === 'udp' ? 'UDP' : '');
// 一格里列几样东西:前两样写出来,多的收成「+N」(悬停看全)
function few(items, mono) {
  if (!items.length) return '';
  const head = items.slice(0, 2).map(x => `<span class="${mono ? 'mono ' : ''}wall-it">${esc(x)}</span>`).join('<span class="wall-sep">,</span>');
  return `<span class="wall-few" title="${esc(items.join('\n'))}">${head}${items.length > 2 ? `<span class="wall-more">+${items.length - 2}</span>` : ''}</span>`;
}
function ruleRow(r) {
  const ports = listOf(r.ports), srcs = listOf(r.sources).map(bare), ids = idsOf(r.nodeIds), pt = protoText(r.proto);
  const portCell = ports.length ? `${few(ports, true)}${pt ? ` <span class="wall-proto">${pt}</span>` : ''}`
    : `<span class="muted">${esc(t('fw.allPorts'))}</span>${pt ? ` <span class="wall-proto">${pt}</span>` : ''}`;
  const srcCell = r.kind === 'allow' ? `<span class="muted">${esc(t('fw.everyone'))}</span>` : few(srcs, true);
  const scope = ids.length ? few(ids.map(nodeName), false) : `<span class="muted">${esc(t('fw.allServers'))}</span>`;
  const exp = r.expiry > 0 ? `<span title="${esc(fmtDate(r.expiry))}">${esc(leftText(r.expiry))}</span>` : `<span class="muted">${esc(t('fw.forever'))}</span>`;
  // 第一格:类型的小标签,后面跟备注(一行,放不下省略)。窄了以后「服务器」「有效期」两列会收起:
  // 不是默认值的(指定了服务器、设了有效期)在这一格下面补一行小字,不让它们凭空消失
  const memo = [r.origin === 'ban' ? t('fw.fromBan') : '', r.note || ''].filter(Boolean).join(' · ');
  const fb = [ids.length ? `<span class="wall-fb-p2">${esc(ids.map(nodeName).join(', '))}</span>` : '', r.expiry > 0 ? `<span class="wall-fb-p3">${esc(leftText(r.expiry))}</span>` : ''].join('');
  return `<tr class="${r.enabled ? '' : 'is-off'}">
    <td class="primary-cell"><span class="wall-kindcell">${badge(t('fw.kind.' + r.kind + '.s'), KIND_BADGE[r.kind] || '')}${memo ? `<span class="wall-note" title="${esc(memo)}">${esc(memo)}</span>` : ''}</span>${fb ? `<div class="sub-cell wall-fb">${fb}</div>` : ''}</td>
    <td>${portCell}</td>
    <td>${srcCell}</td>
    <td class="c-p2${ids.length ? '' : ' wall-def'}">${scope}</td>
    <td class="c-p3${r.expiry > 0 ? '' : ' wall-def'}">${exp}</td>
    <td><label class="switch" title="${esc(t('fw.col.on'))}"><input type="checkbox" data-change="fw.rule.toggle" data-id="${r.id}" aria-label="${esc(t('fw.col.on'))}" ${r.enabled ? 'checked' : ''} ${readOnly() ? 'disabled' : ''}><span></span></label></td>
    <td class="actions"><button class="btn sm" data-act="fw.rule.edit" data-id="${r.id}">${esc(t('common.edit'))}</button><button class="btn sm danger" data-act="fw.rule.del" data-id="${r.id}">${esc(t('common.delete'))}</button></td>
  </tr>`;
}
function rulesHTML() {
  const list = data.rules || [];
  const body = list.length ? `<div class="table-wrap"><table class="grid wall-rtable" data-fit="p3:880 p2:740 cards:600">
      <thead><tr><th>${esc(t('fw.col.rule'))}</th><th>${esc(t('fw.col.portsC'))}</th><th>${esc(t('fw.col.src'))}</th><th class="c-p2">${esc(t('fw.col.scope'))}</th><th class="c-p3">${esc(t('fw.col.expiry'))}</th><th>${esc(t('fw.col.on'))}</th><th></th></tr></thead>
      <tbody>${list.map(ruleRow).join('')}</tbody></table></div>`
    : `<div class="wall-starter"><span class="muted">${esc(t('fw.rule.empty'))}</span>${readOnly() ? '' : ['ssh', 'deny', 'allow'].map(k => `<button class="btn sm" data-act="fw.rule.add" data-id="${k}">${esc(t('fw.starter.' + k))}</button>`).join('')}</div>`;
  return `<div class="card-head"><h2>${esc(t('fw.rule.title'))}${list.length ? `<span class="h-count">${list.length}</span>` : ''}</h2>
      <button class="btn sm primary" data-act="fw.rule.add">${esc(t('fw.rule.add'))}</button></div>
    <p class="hint xp">${esc(t('fw.rule.hint'))}</p>${body}`;
}

// ---- 3. 黑白名单 ----

function banWhy(b) {
  if (b.auto && ['login', 'sub', 'api'].includes(b.reason)) return t('fw.why.' + b.reason, { n: b.count || '' }) + (b.hits > 1 ? ' · ' + t('fw.ban.hits', { n: b.hits }) : '');
  return b.evidence || t('fw.why.manual');
}
function listsHTML() {
  const ro = readOnly();
  const bans = data.bans || [], allow = data.allow || [];
  const seg = `<div class="seg sm">${[['ban', bans.length], ['allow', allow.length]].map(([k, n]) => `<button type="button" data-act="fw.tab" data-id="${k}" class="${tab === k ? 'active' : ''}">${esc(t('fw.tab.' + k))}<span class="seg-n">${n}</span></button>`).join('')}</div>`;
  let body;
  if (tab === 'ban') {
    const list = bans.filter(b => matches(banQ, b.ip, b.evidence));
    const rows = list.slice(0, banShown).map(b => `<tr>
        <td class="primary-cell"><span class="pname mono">${esc(b.ip)}</span></td>
        <td class="wall-whycell"><span class="wall-why" title="${esc(banWhy(b))}">${esc(banWhy(b))}</span></td>
        <td class="c-p2"><span title="${esc(fmtDate(b.lastAt))}">${esc(fmtRelative(b.lastAt))}</span></td>
        <td>${b.expiry > 0 ? `<span title="${esc(fmtDate(b.expiry))}">${esc(inText(b.expiry))}</span>` : `<span class="muted">${esc(t('fw.forever'))}</span>`}</td>
        <td class="actions"><button class="btn sm" data-act="fw.ban.del" data-id="${b.id}">${esc(t('fw.ban.remove'))}</button>
          <details class="menu"><summary class="btn sm">${esc(t('common.more'))}</summary><div class="menu-list">
            <button data-act="fw.ban.toAllow" data-id="${b.id}">${esc(t('fw.ban.toAllow'))}</button>
            <button data-act="fw.ban.promote" data-id="${b.id}">${esc(t('fw.ban.promote'))}</button></div></details></td>
      </tr>`).join('');
    body = bans.length ? `<div class="table-wrap"><table class="grid wall-btable" data-fit="p2:680 cards:520">
        <thead><tr><th>${esc(t('fw.col.ip'))}</th><th>${esc(t('fw.col.why'))}</th><th class="c-p2">${esc(t('fw.col.last'))}</th><th>${esc(t('fw.col.until'))}</th><th></th></tr></thead>
        <tbody>${rows || `<tr><td colspan="5" class="muted">${esc(t('fw.ban.noMatch'))}</td></tr>`}</tbody></table></div>
        ${list.length > banShown ? `<div class="card-foot"><button class="btn sm ghost" data-act="fw.ban.more">${esc(t('fw.ban.more', { n: Math.min(50, list.length - banShown) }))}</button></div>` : ''}
        ${data.banTotal > bans.length ? `<p class="hint">${esc(t('fw.ban.capped', { n: data.banTotal }))}</p>` : ''}`
      : `<p class="wall-none">${esc(t(data.autoBan ? 'fw.ban.emptyOn' : 'fw.ban.emptyOff'))}</p>`;
  } else {
    body = allow.length ? `<div class="table-wrap"><table class="grid wall-atable" data-fit="cards:520">
        <thead><tr><th>${esc(t('fw.col.ip'))}</th><th>${esc(t('fw.col.note'))}</th><th>${esc(t('fw.col.added'))}</th><th></th></tr></thead>
        <tbody>${allow.map(a => `<tr><td class="primary-cell"><span class="pname mono">${esc(a.ip)}</span></td><td>${a.note ? esc(a.note) : '<span class="muted">—</span>'}</td>
          <td><span title="${esc(fmtDate(a.createdAt))}">${esc(fmtRelative(a.createdAt))}</span></td>
          <td class="actions"><button class="btn sm danger" data-act="fw.allow.del" data-id="${a.id}">${esc(t('common.delete'))}</button></td></tr>`).join('')}</tbody></table></div>`
      : `<p class="wall-none">${esc(t('fw.allow.empty'))}</p>`;
  }
  const tools = tab === 'ban'
    ? `${bans.length > 8 ? `<input type="search" class="sm wall-q" id="wall-ban-q" placeholder="${esc(t('common.search'))}…" value="${esc(banQ)}">` : ''}
       ${bans.length && !ro ? `<details class="menu"><summary class="btn sm">${esc(t('fw.ban.clear'))}</summary><div class="menu-list">
          ${bans.some(b => b.auto) ? `<button data-act="fw.ban.clear" data-id="auto">${esc(t('fw.clear.auto'))}</button>` : ''}<button data-act="fw.ban.clear" data-id="all">${esc(t('fw.clear.all'))}</button></div></details>` : ''}
       <button class="btn sm" data-act="fw.ban.add">${esc(t('fw.ban.add'))}</button>`
    : `<button class="btn sm" data-act="fw.allow.add">${esc(t('fw.allow.add'))}</button>`;
  return `<div class="card-head"><h2>${esc(t('fw.lists.title'))}</h2>
      <label class="wall-sw" title="${esc(t('fw.autoBan.tip'))}"><span>${esc(t('fw.autoBan'))}</span><span class="switch"><input type="checkbox" data-change="fw.setting" data-id="autoBan" ${data.autoBan ? 'checked' : ''} ${ro ? 'disabled' : ''}><span></span></span></label></div>
    <p class="hint"><span class="xp">${esc(t(tab === 'ban' ? 'fw.ban.hint' : 'fw.allow.hint'))}</span>${tab === 'ban' ? ` <span title="${esc(t('fw.autoBan.rule'))}">${esc(t(data.autoBan ? 'fw.autoBan.on' : 'fw.autoBan.off'))}</span>` : ''}</p>
    <div class="toolbar wall-bar">${seg}<span class="grow"></span>${tools}</div>${body}`;
}

// ---- 弹框:规则 ----

const EXP = [0, 3600, 6 * 3600, 86400, 7 * 86400, 30 * 86400];
function expirySelect(id, cur) { // cur:编辑时这条规则现在的到期时间(0 = 一直有效;undefined = 新建)
  const keep = cur > 0 ? `<option value="keep" selected>${esc(t('fw.exp.keep', { t: leftText(cur) }))}</option>` : '';
  return `<div class="row wall-exp"><select id="${id}">${keep}${EXP.map(s => `<option value="${s}" ${!keep && s === 0 ? 'selected' : ''}>${esc(t('fw.exp.' + s))}</option>`).join('')}<option value="custom">${esc(t('fw.exp.custom'))}</option></select>
    <span class="wall-hours" hidden><input type="number" id="${id}-h" min="1" max="87600" step="1" inputmode="numeric" value="12" aria-label="${esc(t('fw.f.hours'))}"><span>${esc(t('fw.f.hoursUnit'))}</span></span></div>`;
}
function bindExpiry(id) {
  const sel = document.getElementById(id), box = sel.parentElement.querySelector('.wall-hours');
  const sync = () => { box.hidden = sel.value !== 'custom'; };
  sel.addEventListener('change', sync);
  sync();
}
// 读「有效期」:undefined = 不改(编辑时选了「不改」);否则是秒数(0 = 一直有效)
function readExpiry(id) {
  const v = fv(id);
  if (v === 'keep') return undefined;
  if (v === 'custom') {
    const h = Number(fv(id + '-h'));
    if (!(h >= 1 && h <= 87600)) throw new Error(t('fw.err.hours'));
    return Math.round(h * 3600);
  }
  return Number(v) || 0;
}
// 应用到哪些服务器:全部(以后新加的也算)/ 指定。几十台也只是几行;有分组就能整组勾
function scopeField(ids) {
  const list = nodes();
  if (list.length < 2) return '';
  const all = !ids.length;
  const cell = n => `<div class="node-row"><label title="${esc(n.name)}"><input type="checkbox" class="node-cb" value="${n.id}" ${all || ids.includes(n.id) ? 'checked' : ''}> <span class="nm">${esc(n.name)}</span>${n.isLocal ? ` <span class="muted small">${esc(t('fw.local'))}</span>` : ''}</label></div>`;
  const grouped = list.some(n => String(n.group || '').trim());
  let body;
  if (grouped) {
    const by = new Map();
    for (const n of list) { const g = String(n.group || '').trim(); if (!by.has(g)) by.set(g, []); by.get(g).push(n); }
    body = [...by].map(([g, ns]) => `<div class="node-grp"><label class="node-grp-h"><input type="checkbox" class="node-gcb"> <b>${esc(g || t('node.ungrouped'))}</b><span class="muted small node-gn"></span></label><div class="node-cells">${ns.map(cell).join('')}</div></div>`).join('');
  } else body = `<div class="node-cells">${list.map(cell).join('')}</div>`;
  return fieldBox(t('fw.f.scope'), `<div class="ln-srv wall-scope">
    <div class="ln-srv-head"><div class="seg sm" id="wall-scope-seg"><button type="button" data-scope="all" class="${all ? 'active' : ''}">${esc(t('fw.scope.all'))}</button><button type="button" data-scope="pick" class="${all ? '' : 'active'}">${esc(t('fw.scope.pick'))}</button></div>
      <input type="hidden" id="wall-scope" value="${all ? 'all' : 'pick'}"><span class="grow"></span><span class="muted small" id="wall-scope-n"></span></div>
    <div class="node-list" id="wall-scope-list" ${all ? 'hidden' : ''}>${body}</div>
    <p class="hint xp" id="wall-scope-help"></p></div>`);
}
function bindScope() {
  const seg = document.getElementById('wall-scope-seg');
  if (!seg) return;
  const scope = document.getElementById('wall-scope'), list = document.getElementById('wall-scope-list');
  const cbs = () => [...list.querySelectorAll('.node-cb')];
  const sync = () => {
    const all = scope.value === 'all';
    seg.querySelectorAll('button').forEach(b => b.classList.toggle('active', b.dataset.scope === scope.value));
    list.hidden = all;
    list.querySelectorAll('.node-grp').forEach(grp => {
      const mine = [...grp.querySelectorAll('.node-cb')], n = mine.filter(c => c.checked).length;
      const g = grp.querySelector('.node-gcb');
      g.checked = n > 0 && n === mine.length;
      g.indeterminate = n > 0 && n < mine.length;
      grp.querySelector('.node-gn').textContent = `${n} / ${mine.length}`;
    });
    document.getElementById('wall-scope-n').textContent = all ? '' : t('fw.scope.n', { n: cbs().filter(c => c.checked).length, total: cbs().length });
    document.getElementById('wall-scope-help').textContent = all ? t('fw.scope.allHelp') : '';
  };
  seg.addEventListener('click', e => {
    const b = e.target.closest('button[data-scope]');
    if (!b) return;
    // 从「全部」切到「指定」:先全不勾,免得以为自己挑过(和新建线路同一个道理)
    if (scope.value === 'all' && b.dataset.scope === 'pick') cbs().forEach(c => { c.checked = false; });
    scope.value = b.dataset.scope;
    sync();
  });
  list.addEventListener('change', e => {
    if (e.target.classList.contains('node-gcb')) e.target.closest('.node-grp').querySelectorAll('.node-cb').forEach(c => { c.checked = e.target.checked; });
    sync();
  });
  sync();
}
function readScope() {
  const scope = document.getElementById('wall-scope');
  if (!scope || scope.value === 'all') return [];
  const ids = [...document.querySelectorAll('#wall-scope-list .node-cb:checked')].map(c => Number(c.value));
  if (!ids.length) throw new Error(t('fw.err.scope'));
  return ids;
}

// 规则弹框。seed:新建时预填的(开头那几个「常用做法」按钮);r:要编辑的规则
function ruleDialog(r, seed = {}) {
  const cur = r || { kind: seed.kind || 'deny', proto: seed.proto || '', ports: seed.ports || '', sources: seed.sources || '', note: '', nodeIds: [] };
  const myIp = data.myIp || '';
  const kinds = ['allow', 'deny', 'only'].map(k => `<label class="wall-kind"><input type="radio" name="wall-kind" value="${k}" ${cur.kind === k ? 'checked' : ''}>
      <span class="wall-kind-t">${badge(t('fw.kind.' + k + '.s'), KIND_BADGE[k])}<b>${esc(t('fw.kind.' + k))}</b></span><small class="xp">${esc(t('fw.kind.' + k + '.d'))}</small></label>`).join('');
  const panelPort = Number(state.settings.webPort) || 2053;
  const html = `<div class="wall-kinds" role="radiogroup" aria-label="${esc(t('fw.f.kind'))}">${kinds}</div>
    <div class="form-grid cols-2 wall-form">
      <div class="field full" id="wall-f-src">
        <span class="label">${esc(t('fw.f.src'))}</span>
        <textarea id="wall-src" rows="3" spellcheck="false" placeholder="${esc(t('fw.f.srcPh'))}">${esc(listOf(cur.sources).map(bare).join('\n'))}</textarea>
        ${myIp ? `<span class="wall-quick"><button type="button" class="chip more" data-act="fw.quick" data-id="src:${esc(myIp)}">+ ${esc(t('fw.f.myip', { ip: myIp }))}</button></span>` : ''}
        <span class="help xp" id="wall-src-help"></span>
      </div>
      <div class="field" id="wall-f-ports">
        <span class="label">${esc(t('fw.f.ports'))}</span>
        <input id="wall-ports" spellcheck="false" autocomplete="off" value="${esc(listOf(cur.ports).join(', '))}">
        <span class="wall-quick" data-for="only"><button type="button" class="chip more" data-act="fw.quick" data-id="port:22">+ SSH 22</button><button type="button" class="chip more" data-act="fw.quick" data-id="port:${panelPort}">+ ${esc(t('fw.f.panelPort', { p: panelPort }))}</button></span>
        <span class="wall-quick" data-for="allow"><button type="button" class="chip more" data-act="fw.quick" data-id="port:80">+ 80</button><button type="button" class="chip more" data-act="fw.quick" data-id="port:443">+ 443</button></span>
        <span class="help xp" id="wall-ports-help"></span>
      </div>
      ${field(t('fw.f.proto'), `<select id="wall-proto"><option value="" ${!cur.proto ? 'selected' : ''}>${esc(t('fw.proto.both'))}</option><option value="tcp" ${cur.proto === 'tcp' ? 'selected' : ''}>TCP</option><option value="udp" ${cur.proto === 'udp' ? 'selected' : ''}>UDP</option></select>`)}
      <div class="full">${scopeField(idsOf(cur.nodeIds))}</div>
      ${fieldBox(t('fw.f.expiry'), expirySelect('wall-exp', r ? r.expiry : undefined))}
      ${field(t('fw.f.note'), `<input id="wall-note" maxlength="200" value="${esc(cur.note || '')}">`)}
    </div>`;
  const kindNow = () => (document.querySelector('input[name="wall-kind"]:checked') || {}).value || 'deny';
  const save = async (force) => {
    const kind = kindNow();
    const body = { kind, proto: fv('wall-proto'), ports: fv('wall-ports'), sources: kind === 'allow' ? '' : fv('wall-src'), nodeIds: readScope(), note: fv('wall-note').trim(), force: !!force };
    const secs = readExpiry('wall-exp');
    if (secs !== undefined) body.seconds = secs;
    try {
      data = r ? await put('fw/rules/' + r.id, body) : await post('fw/rules', body);
    } catch (e) {
      if (e.data && e.data.code === 'self_lockout' && !force) {
        if (!await lockConfirm(e.message)) { ruleDialog(r, { kind, proto: body.proto, ports: body.ports, sources: body.sources }); return; }
        return save(true);
      }
      throw e;
    }
    draw();
    toast(t('fw.saved'), 'ok');
  };
  openModal(t(r ? 'fw.rule.edit' : 'fw.rule.new'), html, () => save(false), { wide: true });
  const sync = () => {
    const k = kindNow();
    document.getElementById('wall-f-src').hidden = k === 'allow';
    document.querySelectorAll('#wall-f-ports .wall-quick').forEach(q => { q.hidden = q.dataset.for !== k; });
    document.getElementById('wall-ports').placeholder = t(k === 'deny' ? 'fw.f.portsPh.deny' : 'fw.f.portsPh');
    document.getElementById('wall-ports-help').textContent = t('fw.f.portsHelp.' + k);
    document.getElementById('wall-src-help').textContent = k === 'allow' ? '' : t('fw.f.srcHelp.' + k);
    document.querySelectorAll('.wall-kind').forEach(l => l.classList.toggle('on', l.querySelector('input').checked));
  };
  document.querySelector('.wall-kinds').addEventListener('change', sync);
  sync();
  bindScope();
  bindExpiry('wall-exp');
}
// 会把自己挡在外面的:后端先拦下(409),这里把话说清楚,再确认一次才带 force 存
const lockConfirm = msg => confirm(msg + t('fw.lock.ask'), { title: t('fw.lock.title'), danger: true, okText: t('fw.lock.ok') });

// ---- 弹框:黑名单 / 白名单 ----

function banDialog(seedIp = '') {
  openModal(t('fw.ban.new'), `<div class="form-grid">
      <label class="field full"><span class="label">${esc(t('fw.f.ip'))}</span><input id="wall-ip" spellcheck="false" autocomplete="off" placeholder="${esc(t('fw.f.ipPh'))}" value="${esc(seedIp)}"></label>
      ${fieldBox(t('fw.f.expiry'), expirySelect('wall-bexp'))}
      ${field(t('fw.f.note'), `<input id="wall-bnote" maxlength="200">`)}
    </div><p class="hint xp">${esc(t('fw.ban.help'))}</p>`, async () => {
    const body = { ip: fv('wall-ip').trim(), note: fv('wall-bnote').trim(), seconds: readExpiry('wall-bexp') || 0 };
    const send = async force => {
      try { data = await post('fw/bans', { ...body, force }); }
      catch (e) {
        if (e.data && e.data.code === 'self_lockout' && !force) { if (!await lockConfirm(e.message)) return false; return send(true); }
        throw e;
      }
      return true;
    };
    if (await send(false)) { tab = 'ban'; draw(); toast(t('fw.ban.added'), 'ok'); }
  });
  bindExpiry('wall-bexp');
}
function allowDialog(seedIp = '', seedNote = '') {
  openModal(t('fw.allow.new'), `<div class="form-grid">
      <label class="field full"><span class="label">${esc(t('fw.f.ip'))}</span><input id="wall-aip" spellcheck="false" autocomplete="off" placeholder="${esc(t('fw.f.ipPh'))}" value="${esc(seedIp)}"></label>
      <label class="field full"><span class="label">${esc(t('fw.f.note'))}</span><input id="wall-anote" maxlength="200" value="${esc(seedNote)}"></label>
    </div><p class="hint xp">${esc(t('fw.allow.hint'))}</p>`, async () => {
    data = await post('fw/allow', { ip: fv('wall-aip').trim(), note: fv('wall-anote').trim() });
    tab = 'allow'; draw(); toast(t('fw.allow.added'), 'ok');
  });
}
// 「连代理一起挡」:给黑名单里的这一条加一条「拉黑来源」的规则
function promoteDialog(b) {
  const life = b.expiry > 0 ? leftText(b.expiry) : t('fw.forever');
  openModal(t('fw.promote.title'), `<p class="confirm-text">${esc(t('fw.promote.text', { ip: b.ip, t: life }))}</p><div class="wall-promote">${scopeField([])}</div>`, async () => {
    const body = { nodeIds: readScope() };
    const send = async force => {
      try { data = await post(`fw/bans/${b.id}/promote`, { ...body, force }); }
      catch (e) {
        if (e.data && e.data.code === 'self_lockout' && !force) { if (!await lockConfirm(e.message)) return false; return send(true); }
        throw e;
      }
      return true;
    };
    if (await send(false)) { draw(); toast(t('fw.saved'), 'ok'); }
  }, { saveText: t('fw.promote.ok') });
  bindScope();
}

// ---- 每台服务器上的动作 ----

async function serverOp(id, op) {
  busy.add(id); draw();
  try {
    const res = await post(`fw/servers/${id}/${op}`, undefined, SLOW);
    const s = data.servers.find(x => x.id === id);
    if (s) { s.state = res.state; if (res.ports) portsOf[id] = res.ports; }
    return res;
  } finally { busy.delete(id); draw(); }
}

registerActions({
  'fw.expand': async id => {
    id = Number(id);
    if (opened.has(id)) { opened.delete(id); draw(); return; }
    opened.add(id); draw();
    if (!portsOf[id]) { // 副机的端口清单要现问(顺带让它重新检查一次)
      try { await serverOp(id, 'state'); } catch (e) { portsOf[id] = []; draw(); toast(e.message, 'err'); }
    }
  },
  'fw.recheck': async () => {
    const list = data.servers.filter(s => s.enabled && (s.isLocal || s.online));
    list.forEach(s => busy.add(s.id)); draw();
    const failed = [];
    await Promise.all(list.map(async s => {
      try {
        const res = await post(`fw/servers/${s.id}/state`, undefined, SLOW);
        s.state = res.state; if (res.ports) portsOf[s.id] = res.ports;
      } catch { failed.push(s.name); } finally { busy.delete(s.id); }
    }));
    draw();
    if (failed.length) toast(t('fw.recheckFailed', { names: failed.join(', ') }), 'err'); else toast(t('fw.rechecked'), 'ok');
  },
  'fw.open': async id => {
    id = Number(id);
    const s = data.servers.find(x => x.id === id);
    if (!s || !s.state) return;
    const how = t('fw.open.' + (FRONT[s.state.frontend] ? s.state.frontend : 'iptables'), { name: s.name });
    if (!await confirm(how + t('fw.open.after'), { title: t('fw.open.title'), okText: t('fw.act.open') })) return;
    try {
      const res = await serverOp(id, 'open');
      if (res.error) toast(t('fw.act.failed', { msg: trErr(res.error) }), 'err'); else toast(t('fw.open.done', { name: s.name }), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'fw.undo': async id => {
    id = Number(id);
    const s = data.servers.find(x => x.id === id);
    if (!s || !s.state) return;
    if (!await confirm(t('fw.undo.text', { name: s.name, kind: frontName(s.state.openedFrom) || '—' }), { title: t('fw.undo.title'), okText: t('fw.undo.ok') })) return;
    try {
      const res = await serverOp(id, 'undo');
      if (res.error) toast(t('fw.act.failed', { msg: trErr(res.error) }), 'err'); else toast(t('fw.undo.done', { name: s.name }), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
  'fw.setting': async (key, el) => {
    const on = el.checked;
    if (key === 'autoBan' && on && !await confirm(t('fw.autoBan.confirm'), { title: t('fw.autoBan'), okText: t('fw.autoBan.turnOn') })) { el.checked = false; return; }
    try { data = await put('fw/settings', { [key]: on }); draw(); toast(t(`fw.${key}.${on ? 'onToast' : 'offToast'}`), 'ok'); }
    catch (e) { el.checked = !on; toast(e.message, 'err'); }
  },
  'fw.rule.add': kind => {
    const myIp = data.myIp || '';
    const seed = kind === 'ssh' ? { kind: 'only', proto: 'tcp', ports: '22', sources: myIp } : kind === 'allow' ? { kind: 'allow' } : kind === 'deny' ? { kind: 'deny' } : {};
    ruleDialog(null, seed);
  },
  'fw.rule.edit': id => { const r = data.rules.find(x => x.id === Number(id)); if (r) ruleDialog(r); },
  'fw.rule.del': async id => {
    if (!await confirm(t('fw.rule.delConfirm'), { danger: true, okText: t('common.delete') })) return;
    try { data = await del('fw/rules/' + id); draw(); toast(t('fw.deleted'), 'ok'); } catch (e) { toast(e.message, 'err'); }
  },
  'fw.rule.toggle': async (id, el) => {
    const want = el.checked;
    const send = async force => {
      try { data = await post(`fw/rules/${id}/toggle`, { force }); }
      catch (e) {
        if (e.data && e.data.code === 'self_lockout' && !force) { if (!await lockConfirm(e.message)) return false; return send(true); }
        throw e;
      }
      return true;
    };
    try { if (!await send(false)) el.checked = !want; draw(); } catch (e) { el.checked = !want; toast(e.message, 'err'); }
  },
  'fw.quick': (what, el) => {
    const [kind, val] = String(what).split(/:(.*)/s);
    if (kind === 'port') { const f = document.getElementById('wall-ports'); const have = listOf(f.value.replace(/\s+/g, ',')); if (!have.includes(val)) f.value = [...have, val].join(', '); }
    else { const f = document.getElementById('wall-src'); const have = f.value.split(/[\s,]+/).filter(Boolean); if (!have.includes(val)) f.value = [...have, val].join('\n'); }
    el.blur();
  },
  'fw.tab': id => { tab = id === 'allow' ? 'allow' : 'ban'; draw(); },
  'fw.ban.add': () => banDialog(),
  'fw.ban.more': () => { banShown += 50; draw(); },
  'fw.ban.del': async id => { try { data = await del('fw/bans/' + id); draw(); toast(t('fw.ban.removed'), 'ok'); } catch (e) { toast(e.message, 'err'); } },
  'fw.ban.toAllow': async id => {
    const b = data.bans.find(x => x.id === Number(id));
    if (!b) return;
    try { await post('fw/allow', { ip: b.ip, note: '' }); data = await del('fw/bans/' + id); draw(); toast(t('fw.ban.movedToAllow', { ip: b.ip }), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'fw.ban.promote': id => { const b = data.bans.find(x => x.id === Number(id)); if (b) promoteDialog(b); },
  'fw.ban.clear': async which => {
    const onlyAuto = which === 'auto';
    if (!await confirm(t(onlyAuto ? 'fw.clear.autoConfirm' : 'fw.clear.allConfirm'), { danger: true, okText: t('fw.ban.clear') })) return;
    try { data = await post('fw/bans/clear', { onlyAuto }); draw(); toast(t('fw.ban.cleared'), 'ok'); } catch (e) { toast(e.message, 'err'); }
  },
  'fw.allow.add': () => allowDialog(),
  'fw.allow.del': async id => { try { data = await del('fw/allow/' + id); draw(); toast(t('fw.deleted'), 'ok'); } catch (e) { toast(e.message, 'err'); } },
});

// 黑名单里搜:输入框在每次重画时会换掉,事件挂在文档上
document.addEventListener('input', debounce(e => {
  if (e.target && e.target.id === 'wall-ban-q') {
    banQ = e.target.value; banShown = 30;
    const pos = e.target.selectionStart;
    draw();
    const q = document.getElementById('wall-ban-q');
    if (q) { q.focus(); try { q.setSelectionRange(pos, pos); } catch { /* 有的浏览器 search 框不让设 */ } }
  }
}));

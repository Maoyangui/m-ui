// 概况(首页):主面板、副机、代理面板共用一个 render。
// 原则:每块都有上限(条数或限高滚动),超出写「还有 N … / 查看全部 →」,整页高度不随数据量增长;
// 每块各自加载、各自失败,一个接口失败只影响它那一块(以前一个失败整页换成报错卡);
// 展开、筛选、搜索都存在模块变量里,5 秒一轮的 setHTML 重画不会把它们收起,也不丢滚动位置。
import { state, ensure, load, isReseller, updateInfo, promptUpdate } from '../app.js';
import { get, post, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtBytes, fmtDuration, fmtRelative, fmtTime, fmtDate, fmtDay, tzOffsetMinutes, toast, registerActions, badge, dot, empty, setHTML, chipList, onChipToggle, chipOpen, auditText, daysLeft, hostPort, matches } from '../ui.js';
import { barChart, bucketFor } from '../chart.js';
import { NODE_CODES, summarizeNodes, trackUnsynced, worstCode, healthSummary, sortCerts, certSig, certCounts, certLeftDays, sortReach, reachSig, reachHard, reachStall } from '../issues.js';
import { expiringDays, quotaPct } from './users.js';
import { shownConns } from '../fair.js';

export const title = () => t('nav.dashboard');
// status / onlines 没取到时 app.js 照样调 tick:好在「需要处理」里写明"面板状态没取到"
export const tickOnError = true;

// ---- 交互状态(切页回来还在)----
let range = 24, logLevel = 'info', topHours = 24;
let attnAll = false, attnIgnoredOpen = false;
let onlineQ = '', connF = 'ok', connAll = false, connQ = '', auditScope = 'human';
// 手机(≤700px)上不做内滚区(和页面滚动抢手势):在线名单、最近连接在原地一批批展开,这里记展开到第几批
let onlineMore = 0, connMore = 0;
const MOBILE_STEP = { online: 36, conns: 20 };
let logOpen = null; // localStorage m-ui.dashLog;没存过时手机默认折起
// ---- 各数据源的缓存与错误(进页面时清空,不沿用上一次登录的)----
let nodesData = null, nodesErr = null, health = null, healthErr = null, conns = null, connsErr = null;
let dayFromChart = null, day = null, dayErr = null; // 24 小时合计:图在 24 小时档时直接用图的,否则单独取
let okAt = {};
const unsyncedSince = new Map(); // 服务器名 → 第一次看到"未同步"的时间
let ticks = 0;

const narrow = () => matchMedia('(max-width: 700px)').matches;
const isNode = () => (state.status || {}).role === 'node';
const multi = () => !isNode() && ((state.status || {}).nodes || 0) > 1;
const enc = encodeURIComponent;
const ls = k => { try { return localStorage.getItem(k); } catch { return null; } };
const lsSet = (k, v) => { try { if (v === null) localStorage.removeItem(k); else localStorage.setItem(k, v); } catch {} };
const errText = e => trErr(String((e && e.message) || e || ''));
const now = () => Date.now() / 1000;
// 名单:前 n 个 + 「等 N 台 / 张 / 条 / 人」
const etc = (names, unit, n = 3) => (names.length <= n ? names.join(t('reach.sep'))
  : t('dash.etc.' + unit, { list: names.slice(0, n).join(t('reach.sep')), n: names.length, more: names.length - n }));

// ---- 每块的加载 / 失败状态 ----
// 首次失败:块里显示"加载失败 + 重试";之前成功过:保留旧内容,标题旁挂"未能刷新 · 某时的数据"。401 照抛,交给登录流程。
async function guard(id, fn) {
  try {
    await fn();
    okAt[id] = Date.now();
    staleMark(id, null);
  } catch (e) {
    if (e && e.status === 401) throw e;
    const el = document.getElementById(id);
    if (!el) return;
    if (!okAt[id]) setHTML(el, `<div class="dash-err"><span class="msg">⚠ ${esc(t('dash.loadFailed', { err: errText(e) }))}</span><button class="btn sm" data-act="dash.retry" data-id="${id}">${esc(t('dash.retry'))}</button></div>`);
    else staleMark(id, e, okAt[id]);
  }
}
function staleMark(id, e, at) {
  const card = document.getElementById(id + '-card') || (document.getElementById(id) || { closest: () => null }).closest('.card');
  if (!card) return;
  let s = card.querySelector(':scope > .card-head .dash-stale');
  if (!e) { if (s) s.remove(); return; }
  if (!s) {
    const h = card.querySelector(':scope > .card-head h2');
    if (!h) return;
    s = document.createElement('span');
    s.className = 'badge warn dash-stale';
    h.append(s);
  }
  s.textContent = t('dash.stale', { time: fmtTime(Math.floor(at / 1000)) });
  s.title = errText(e);
}
const loading = () => `<div class="dash-loading">${esc(t('common.loading'))}</div>`;

// ---- 快速开始 / 配置完成卡 ----
// 必做项没做完:只列没做的项。必做项做完:和 Star 合成一行「✓ 基本配置已完成 · 还可以:…」,两边都没内容就整张不出。
function qsItems() {
  const s = state.status;
  return [
    { ok: !s.defaultPassword, page: 'admin', text: t('qs.password'), opt: false },
    { ok: !!s.certExists, page: 'cert', text: s.certExists && s.certSelfSigned ? t('qs.certSelf') : t('qs.cert'), opt: false },
    { ok: (s.lines || 0) > 0, page: 'lines', text: t('qs.lines'), opt: false },
    { ok: (s.users || 0) > 0, page: 'users', text: t('qs.users'), opt: false },
    { ok: !!s.domain, page: 'settings', text: t('qs.domain'), opt: true },
    { ok: (s.upstreams || 0) > 0, page: 'upstreams', text: t('qs.upstreams'), opt: true },
    { ok: !!s.tgEnabled, page: 'settings', text: t('qs.telegram'), opt: true },
    { ok: (s.nodes || 0) > 1, page: 'nodes', text: t('qs.nodes'), opt: true },
    { ok: (s.plans || 0) > 0, page: 'plans', text: t('qs.plans'), opt: true },
  ];
}
function quickStart() {
  const s = state.status;
  if (s.role === 'node') return '';
  const items = qsItems(), todo = items.filter(i => !i.ok);
  const hidden = ls('m-ui.hideQuickStart') === '1';
  if (!items.filter(i => !i.opt).every(i => i.ok)) {
    if (hidden) return '';
    // 必做的在前,可选的带「可选」
    const list = todo.slice().sort((a, b) => a.opt - b.opt);
    return `<section class="card quickstart span-all">
      <div class="card-head"><h2>${esc(t('qs.title'))} <span class="h-count">${esc(t('qs.progress', { done: items.length - todo.length, total: items.length }))}</span></h2><button class="btn ghost sm" data-act="dash.hideQs" title="${esc(t('qs.hide'))}">✕</button></div>
      <p class="hint">${esc(t('qs.help'))}</p>
      <ol class="qs-list">${list.map(i => `<li><span class="qs-mark">○</span><a href="#/${i.page}">${esc(i.text)}</a>${i.opt ? `<span class="badge">${esc(t('qs.optional'))}</span>` : ''}</li>`).join('')}</ol>
    </section>`;
  }
  const opt = hidden ? [] : todo;
  const star = s.repo && ls('m-ui.starDismissed') !== '1';
  if (!opt.length && !star) return '';
  return `<section class="card qs-done span-all" id="star-card">
    <span>✓ <b>${esc(t('qs.doneTitle'))}</b>${opt.length ? ` <span class="muted small">${esc(t('qs.alsoCan'))}</span>` : ''}</span>
    ${opt.length ? `<span class="qs-opt">${opt.slice(0, 3).map(i => `<a class="chip" href="#/${i.page}" title="${esc(i.text)}">${esc(i.text.split(/[((::]/)[0].trim())}</a>`).join('')}${opt.length > 3 ? `<span class="muted small">${esc(t('qs.moreN', { n: opt.length, more: opt.length - 3 }))}</span>` : ''}</span>` : ''}
    <span class="grow"></span>
    ${star ? `<a class="btn primary sm" href="${esc(s.repo)}" target="_blank" rel="noopener" data-act="dash.starGo" title="${esc(t('star.text'))}">★ ${esc(t('star.btn'))}</a>` : ''}
    <button class="btn ghost sm" data-act="dash.doneHide" title="${esc(t('star.dismiss'))}">✕</button>
  </section>`;
}

// ---- 页面骨架:一个 .dash-grid,DOM 顺序就是阅读顺序(规格 §2.2)----
const card = (id, cls, head, body, foot = '') => `<section class="card ${cls}" id="${id}-card"><div class="card-head">${head}</div>${body}${foot}</section>`;
const scopeTag = text => (text ? ` <span class="scope">${esc(text)}</span>` : '');

export async function render(el) {
  // 数据缓存不沿用上一次(可能是别的账号登录时的)
  nodesData = nodesErr = health = healthErr = conns = connsErr = dayFromChart = day = dayErr = null;
  okAt = {};
  ticks = 0;
  if (isReseller()) return renderReseller(el);
  if (logOpen === null) { const v = ls('m-ui.dashLog'); logOpen = v === null ? !narrow() : v === '1'; }
  const node = isNode(), many = multi();
  const allScope = many ? t('dash.scope.all') : node ? t('dash.scope.local') : '';
  el.innerHTML = `<div class="dash-grid">
    ${quickStart()}
    <section class="card span-all attn-card" id="dash-attn-card"><div class="card-head"><h2>${esc(t('dash.attn.title'))} <span class="h-count" id="dash-attn-n"></span></h2></div><div id="dash-attn">${loading()}</div></section>
    <div class="stats span-all" id="dash-stats"></div>
    ${card('dash-chart', 'span-2', `<h2>${esc(t('dash.traffic'))}${scopeTag(allScope)}</h2>
      <div class="seg" id="dash-range">${[1, 6, 24, 168].map(h => `<button data-act="dash.range" data-id="${h}" class="${h === range ? 'active' : ''}">${esc(t('dash.range.' + (h === 168 ? '7d' : h + 'h')))}</button>`).join('')}</div>`, `<div id="dash-chart">${loading()}</div>`)}
    ${card('dash-health', '', `<h2>${esc(t('dash.health'))}${scopeTag(node ? t('dash.scope.local') : '')}</h2>${node ? '' : `<button class="btn sm" data-act="dash.healthRun">${esc(t('dash.healthRun'))}</button>`}`, `<div id="dash-health">${loading()}</div>`)}
    ${card('dash-online', '', `<h2>${esc(t('dash.onlineList'))}${scopeTag(node ? t('dash.scope.local') : '')} <span class="h-count" id="dash-online-n"></span></h2><input type="search" class="sm dash-q" id="dash-online-q" placeholder="${esc(t('dash.online.search'))}" value="${esc(onlineQ)}" hidden>`, `<div id="dash-online"></div>`)}
    ${card('dash-conns', 'span-2', `<h2>${esc(t('dash.recentConns'))} <span class="help-tip" id="dash-conns-help" title="${esc(t('dash.recentConnsHelp'))}">?</span></h2><span id="dash-conn-seg"></span><input type="search" class="sm dash-q" id="dash-conn-q" placeholder="${esc(t('dash.conn.search'))}" value="${esc(connQ)}" hidden><button class="btn sm" data-act="dash.connsRefresh">${esc(t('common.refresh'))}</button>`, `<div id="dash-conns">${loading()}</div>`)}
    ${card('dash-top', '', `<h2>${esc(t('dash.topUsers'))}${scopeTag(allScope)}</h2>
      <div class="seg" id="dash-toprange">${[24, 168, 720].map(h => `<button data-act="dash.toprange" data-id="${h}" class="${h === topHours ? 'active' : ''}">${esc(t('dash.range.' + (h === 24 ? '24h' : h === 168 ? '7d' : '30d')))}</button>`).join('')}</div>`, `<div id="dash-top">${loading()}</div>`)}
    ${card('dash-audit', '', `<h2>${esc(t('dash.recentAudit'))}</h2><div class="seg sm" id="dash-audit-seg">${[['human', t('dash.audit.human')], ['all', t('common.all')]].map(([k, v]) => `<button data-act="dash.auditScope" data-id="${k}" class="${k === auditScope ? 'active' : ''}">${esc(v)}</button>`).join('')}</div>`, `<div id="dash-audit">${loading()}</div>`,
      `<div class="card-foot"><a href="#/logs?tab=audit">${esc(t('dash.audit.viewAll'))}</a></div>`)}
    ${card('dash-core', '', `<h2>${esc(many ? t('dash.coreLocal') : t('dash.core'))}</h2><button class="btn sm" data-act="dash.reload">${esc(t('dash.reload'))}</button>`, `<dl class="kv" id="dash-core"></dl>`)}
    <section class="card dash-log-card ${logOpen ? '' : 'collapsed'}" id="dash-log-card">
      <div class="card-head"><h2><button class="link-btn" data-act="dash.logToggle" aria-expanded="${logOpen}">${logOpen ? '▾' : '▸'} ${esc(t('dash.coreLog'))}</button></h2>
        <select class="sm" id="dash-loglevel" data-change="dash.loglevel" style="width:auto">${['debug', 'info', 'warning', 'error'].map(l => `<option ${l === logLevel ? 'selected' : ''}>${l}</option>`).join('')}</select></div>
      <pre class="log" id="dash-log"></pre>
      <div class="card-foot"><a href="#/logs?tab=core">${esc(t('dash.logAll'))}</a></div>
    </section>
  </div>`;
  document.getElementById('dash-online-q').addEventListener('input', e => { onlineQ = e.target.value.trim(); renderOnline(); });
  document.getElementById('dash-conn-q').addEventListener('input', e => { connQ = e.target.value.trim(); renderConns(); });
  onChipToggle('dash-online', renderOnline);
  watchChart();
  renderStats(); renderCore(); renderOnline(); renderAttn();
  // 每块各自成败:Promise.allSettled,一块失败不会把整页换成报错卡
  const jobs = [guard('dash-chart', renderChart), guard('dash-audit', renderAudit), guard('dash-top', renderTop),
    guard('dash-health', fetchHealth), guard('dash-conns', fetchConns), fetchNodes().catch(() => {})];
  if (logOpen) jobs.push(guard('dash-log', renderLog));
  if (range !== 24) jobs.push(fetchDay());
  await Promise.allSettled(jobs);
  renderStats(); renderAttn();
}

// 节奏(一轮 5 秒,操作后 12 秒内每轮 2 秒):status、onlines 由 app.js 每轮拉;
// 服务器列表每 3 轮(副机每 12 轮,只要修订号和应用时间)、连接每 2 轮、日志展开时每轮、巡检和审计每 3 轮、流量图与排行每 12 轮。
export async function tick() {
  if (isReseller()) return tickReseller();
  ticks++;
  const jobs = [];
  if (ticks % (isNode() ? 12 : 3) === 0) jobs.push(fetchNodes().catch(() => {}));
  if (ticks % 2 === 0) jobs.push(guard('dash-conns', fetchConns));
  if (logOpen) jobs.push(guard('dash-log', renderLog)); // 折起时不拉
  if (ticks % 3 === 0) jobs.push(guard('dash-health', fetchHealth), guard('dash-audit', renderAudit));
  if (ticks % 12 === 0) { jobs.push(guard('dash-chart', renderChart), guard('dash-top', renderTop)); if (range !== 24) jobs.push(fetchDay()); }
  await Promise.allSettled(jobs);
  renderStats(); renderCore(); renderOnline(); renderAttn();
}

// ---- 数据 ----
async function fetchNodes() {
  try {
    nodesData = await get('nodes');
    nodesErr = null;
    trackUnsynced(unsyncedSince, nodesData.nodes || []);
    renderConns(); // 「老副机不上报握手失败」的说明跟着服务器列表变
  } catch (e) {
    if (e && e.status === 401) throw e;
    nodesErr = e;
    throw e;
  }
}
async function fetchHealth() {
  try { health = await get('upstreams/health'); healthErr = null; }
  catch (e) { healthErr = e; throw e; }
  renderHealth();
}
async function fetchConns() {
  try {
    const got = await get('conns/recent?fails=1');
    conns = Array.isArray(got) ? got : []; // 老后端 / 演示站的假接口可能回 {}
    connsErr = null;
  } catch (e) { connsErr = e; throw e; }
  renderConns();
}
// 流量图不在 24 小时档时,24 小时流量卡单独取(以前切到 7 天再回来,卡片显示旧值或 0)
async function fetchDay() {
  try {
    const d = await get(`stats?resource=user&hours=24&bucket=3600&tz=${tzOffsetMinutes()}`);
    day = { up: d.totalUp || 0, down: d.totalDown || 0 };
    dayErr = null;
  } catch (e) { dayErr = e; }
}
const nodesSum = () => (nodesData && !isNode() ? summarizeNodes(nodesData, { unsyncedFor: trackUnsynced(unsyncedSince, nodesData.nodes || []) }) : null);

// ---- 需要处理 ----
// 只放要人去处理的持续状态。类别固定,每类一行,按 红 → 黄 → 灰、同色按类别序号排;有数据源没取到时不说"一切正常"。
const SEV = { bad: 0, warn: 1, info: 2 };
const btn = (act, id, text, cls = '') => `<button class="btn sm ${cls}" data-act="${act}"${id ? ` data-id="${id}"` : ''}>${esc(text)}</button>`;

function certWhat(a) {
  if (a.trouble === 'failing') return a.renewErr ? trErr(a.renewErr) : t('alert.certFailing');
  if (a.trouble === 'missing') return t('alert.certMissing');
  const left = certLeftDays(a);
  if (left < 0) return t('alert.certExpired');
  return t(a.method === 'manual' ? 'alert.certManual' : 'alert.certLeft', { n: left });
}

// 主机容灾(status.ha):同一时刻只出一行,取最严重的作标题,其余进分项。子字段缺了就跳过那一项。
// 兼容两种形状:精简的小结(lost 为秒数、rank、masterName 在顶层)和完整的视图(node / demoted / switch / standbys 子对象)。
function haRows(ha) {
  if (!ha || typeof ha !== 'object') return null;
  const nd = ha.node || {};
  const lostSec = typeof ha.lost === 'number' ? ha.lost : (nd.lost ? Number(nd.leaseAgo) || 0 : 0);
  const rank = ha.rank || nd.rank || 0;
  const sw = ha.switch || nd.switch || null;
  const c = [];
  if (ha.role === 'demoted') {
    const d = ha.demoted || {};
    c.push({ sev: 'bad', title: t('dash.attn.ha.demoted', { name: d.masterName || ha.masterName || '—' }),
      parts: [d.divergent > 0 && t('dash.attn.ha.divergent', { n: d.divergent })], names: d.backup ? t('dash.attn.ha.backup', { file: d.backup }) : '', go: '#/backup' });
  }
  if (sw && sw.err) c.push({ sev: 'bad', title: t('dash.attn.ha.switchFailed', { err: trErr(sw.err) }) });
  if (lostSec > 0) c.push({ sev: 'bad', title: t('dash.attn.ha.lost', { dur: fmtDuration(lostSec) }),
    parts: [rank > 0 && t('dash.attn.ha.rank', { n: rank }), nd.replicaAt && t('dash.attn.ha.replica', { ago: fmtRelative(nd.replicaAt) })] });
  if (ha.role === 'promoting') c.push({ sev: 'warn', title: t('dash.attn.ha.promoting') });
  // 写操作暂停(换主机、接任过程中):没有换主机的明细时单独成项
  if (ha.frozenUntil > now() && !(sw && !sw.err && !sw.doneAt)) c.push({ sev: 'warn', title: ha.frozenText ? trErr(ha.frozenText) : t('dash.attn.ha.frozen', { time: fmtTime(ha.frozenUntil) }) });
  if (sw && !sw.err && !sw.doneAt && (sw.stage || sw.targetName)) c.push({ sev: 'warn', title: t('dash.attn.ha.switching', { name: sw.targetName || '—' }),
    parts: [ha.frozenUntil && t('dash.attn.ha.frozen', { time: fmtTime(ha.frozenUntil) })] });
  if (ha.became && ha.former) c.push({ sev: 'warn', title: t('dash.attn.ha.took', { e: ha.epoch || '—' }),
    parts: [ha.became.replicaAt && t('dash.attn.ha.dataAt', { time: fmtTime(ha.became.replicaAt) }), t('dash.attn.ha.former', { name: ha.former.name || ha.became.formerName || '—' })] });
  if (ha.replicaErr) c.push({ sev: 'warn', title: t('dash.attn.ha.replicaErr', { err: trErr(ha.replicaErr) }) });
  const sb = Array.isArray(ha.standbys) ? ha.standbys : null;
  if (sb && ha.role === 'master' && ha.mode && ha.mode !== 'off') {
    const ok = sb.filter(x => x.eligible);
    if (!ok.length) c.push({ sev: 'warn', title: t('dash.attn.ha.noStandby') });
    const limit = now() - 3 * (Number(ha.replicaMinutes) || 5) * 60;
    const stale = ok.filter(x => x.ackErr || (x.replicaAt && x.replicaAt < limit));
    if (stale.length) c.push({ sev: 'warn', title: t('dash.attn.ha.stale'), names: etc(stale.map(x => `${x.name} ${x.ackErr ? t('dash.attn.ha.sendFailed') : fmtRelative(x.replicaAt)}`), 'nodes') });
  }
  const verifyErr = nd.verifyErr || (nd.replica && nd.replica.err);
  if (rank > 0 && verifyErr) c.push({ sev: 'warn', title: t('dash.attn.ha.verifyErr', { err: trErr(verifyErr) }) });
  if (!c.length) return null;
  c.sort((a, b) => SEV[a.sev] - SEV[b.sev]);
  const top = { ...c[0] };
  top.parts = [...(top.parts || []), ...c.slice(1).map(x => x.title)].filter(Boolean).map(esc);
  top.go = top.go || '#/nodes';
  // 失联且本机可以接任:这一行直接给按钮(概况上顶部的失联横幅不再重复显示,见 style.css)
  if (lostSec > 0 && ha.canTakeover) top.acts = btn('ha.takeover', '', t('dash.attn.ha.takeover'), 'danger');
  return top;
}

function attnItems() {
  const s = state.status || {}, node = isNode(), items = [], ignored = [];
  const add = (no, sev, r) => items.push({ no, sev, ...r, parts: (r.parts || []).filter(Boolean) });
  // 1 数据面重载失败
  const rl = s.reload;
  if (rl && rl.at && rl.ok === false) add(1, 'bad', { title: t('dash.attn.reload', { op: trErr(rl.op || '') }), names: trErr(rl.error || ''),
    acts: btn('dash.reload', '', t('alert.retryReload')), go: '#/logs?tab=core', goText: t('dash.attn.viewLog') });
  // 2 主机容灾(字段存在才有)
  const ha = haRows(s.ha);
  if (ha) add(2, ha.sev, ha);
  // 3 服务器(主面板;单服务器时只可能是下线通知没送到)
  const sum = nodesSum();
  if (sum && (sum.red || sum.yellow)) {
    const worst = worstCode(sum);
    add(3, sum.red ? 'bad' : 'warn', { title: t('dash.attn.nodes', { on: sum.online, total: sum.total }),
      parts: NODE_CODES.filter(c => sum.byCode[c].length).map(c => esc(t('dash.attn.node.' + c, { n: sum.byCode[c].length }))),
      names: etc(sum.byCode[worst] || [], 'nodes'), go: '#/nodes?f=bad' });
  }
  // 4 证书(可以「知道了」,和顶部提示条同一个签名)
  const ca = s.certAlerts || [];
  if (ca.length) {
    const n = certCounts(ca);
    const row = { title: t('dash.attn.certs', { n: ca.length }),
      parts: [n.failing && t('dash.attn.cert.failing', { n: n.failing }), n.missing && t('dash.attn.cert.missing', { n: n.missing }), n.expired && t('dash.attn.cert.expired', { n: n.expired }), n.expiring && t('dash.attn.cert.expiring', { n: n.expiring })].filter(Boolean).map(esc),
      names: etc(sortCerts(ca).map(a => `${a.node ? a.node + ' · ' : ''}${a.name} (${certWhat(a)})`), 'certs'),
      go: ca.some(a => !a.nodeId) ? '#/cert' : '#/nodes?f=bad', kind: 'cert' };
    if (certSig(ca) === ls('m-ui.certDismissed')) ignored.push(row);
    else add(4, n.failing + n.missing + n.expired ? 'bad' : 'warn', { ...row, acts: btn('dash.dismiss', 'cert', t('reach.dismiss'), 'ghost') });
  }
  // 5 大陆连通(主面板)
  const rb = (!node && s.reachBad) || [];
  if (rb.length) {
    const hard = reachHard(rb).length;
    const row = { title: t('dash.attn.reach', { n: rb.length }),
      parts: [hard && t('dash.attn.reach.blocked', { n: hard }), rb.length - hard && t('dash.attn.reach.partial', { n: rb.length - hard })].filter(Boolean).map(esc),
      names: etc(sortReach(rb).map(r => `${r.name} ${t('reach.v.' + r.verdict)}`), 'nodes'), go: '#/nodes?f=bad', kind: 'reach' };
    if (reachSig(rb) === ls('m-ui.reachDismissed')) ignored.push(row);
    else add(5, hard ? 'bad' : 'warn', { ...row, acts: btn('dash.dismiss', 'reach', t('reach.dismiss'), 'ghost') });
  }
  // 6 IPv6 入站(字段存在才有)
  if (!node && Array.isArray(s.v6Bad) && s.v6Bad.length) add(6, 'warn', { title: t('dash.attn.v6', { n: s.v6Bad.length }), parts: [esc(t('dash.attn.v6.hint'))],
    names: etc(s.v6Bad.map(x => `${x.name} ${x.port ? hostPort(x.target, x.port) : x.target || ''}`.trim()), 'nodes'), go: '#/nodes?f=bad' });
  // 7 上游
  const hs = healthSummary(health);
  if (hs && hs.bad.length) add(7, 'bad', { title: t('dash.attn.upstreams', { n: hs.bad.length }), parts: [!hs.single && esc(t('dash.attn.up.servers', { n: hs.servers.size }))],
    names: etc(hs.bad.map(b => b.name), 'ups'), go: '#/upstreams?f=bad' });
  // 8 本机记账(副机;字段存在才有)
  if (node && s.ledgerFails > 0) add(8, 'bad', { title: t('dash.attn.ledger', { n: s.ledgerFails }), names: t('node.ledgerFailsHelp', { n: s.ledgerFails }), go: '#/logs?tab=core', goText: t('dash.attn.viewLog') });
  // 8b 主机推来的配置没应用成(版本太低被拒收、数据面应用失败):主机照样在联系本机,不提就会显示成正常
  if (node && s.applyErr) add(8, 'bad', { title: t('dash.attn.applyErr', { time: fmtTime(s.applyErrAt) }), names: trErr(s.applyErr), go: '#/logs?tab=core', goText: t('dash.attn.viewLog') });
  // 9 主机多久没联系本机(副机、没开容灾;字段存在才有)
  const haOn = s.ha && (s.ha.epoch > 0 || s.ha.node);
  if (node && !haOn && s.hubSeenAt && now() - s.hubSeenAt > 120) add(9, 'warn', { title: t('dash.attn.hubSilent', { dur: fmtDuration(now() - s.hubSeenAt) }), names: t('dash.attn.hubSilentHint') });
  // 10 / 11 / 12 用户、代理、备份(主面板;status.attention 存在才有)
  const at = (!node && s.attention) || null;
  if (at) {
    const ne = at.usersExpiring || 0, nq = at.usersQuota || 0;
    if (ne + nq > 0) add(10, 'warn', { title: t('dash.attn.users', { n: ne + nq }),
      parts: [ne && `<a href="#/users?f=expiring">${esc(t('dash.attn.users.expiring', { d: at.expiringDays || expiringDays(), n: ne }))}</a>`,
        nq && `<a href="#/users?f=quota">${esc(t('dash.attn.users.quota', { p: at.quotaPct || quotaPct(), n: nq }))}</a>`] });
    const rd = at.resellersDepleted || 0, rx = at.resellersExpiring || 0, rgone = at.resellersExpired || 0;
    if (rd + rx + rgone > 0) add(11, rd || rgone ? 'bad' : 'warn', { title: t('dash.attn.resellers', { n: rd + rx + rgone }),
      parts: [rd && esc(t('dash.attn.rs.depleted', { n: rd })), rgone && esc(t('dash.attn.rs.expired', { n: rgone })), rx && esc(t('dash.attn.rs.expiring', { n: rx }))], go: '#/resellers?f=attn' });
    const b = at.backup;
    if (b && (b.lastErr || (b.auto && b.lastAt && b.lastAt < now() - 48 * 3600))) add(12, 'warn', {
      title: b.lastErr ? t('dash.attn.backupFailed') : t('dash.attn.backupStale', { ago: fmtRelative(b.lastAt) }),
      names: b.lastErr ? trErr(b.lastErr) + (b.lastErrAt ? ' · ' + fmtTime(b.lastErrAt) : '') : '', go: '#/backup' });
  }
  // 12b 检测器自己停摆:检测不跑了,"没有异常"就不可信
  const rs = !node && s.reachState;
  const stall = rs ? reachStall(rs, now()) : '';
  if (stall) {
    const title = stall === 'idle' ? t('dash.attn.reachStale', { ago: fmtRelative(rs.lastRun), n: rs.minutes })
      : stall === 'failing' && rs.lastOk ? t('dash.attn.reachFailing', { ago: fmtRelative(rs.lastOk) })
      : stall === 'failing' ? t('dash.attn.reachFailNow') : t('dash.attn.reachNever');
    const why = rs.error || rs.failErr;
    add(12, 'warn', { title, names: why ? trErr(why) : '', go: '#/nodes' });
  }
  if (hs && hs.rows.length && hs.interval > 0 && now() - hs.lastRun > 3 * hs.interval * 60) {
    add(12, 'warn', { title: t('dash.attn.healthStale', { ago: fmtRelative(hs.lastRun), n: hs.interval }), go: '#/upstreams' });
  }
  // 13 最近 10 分钟被拒的连接(灰色提示;按来源计,不是次数)
  const rj = (conns || []).filter(c => c.rejected && !c.failed && c.ts > now() - 600);
  if (rj.length) {
    const dev = rj.filter(c => c.rejected !== 'pool').length;
    add(13, 'info', { title: t('dash.attn.rejected', { n: rj.length }),
      parts: [dev && esc(t('dash.attn.rej.device', { n: dev })), rj.length - dev && esc(t('dash.attn.rej.pool', { n: rj.length - dev }))],
      names: etc([...new Set(rj.map(c => c.user).filter(Boolean))], 'users'), acts: btn('dash.showRejected', '', t('dash.attn.showRejected'), 'ghost') });
  }
  // 14 面板状态过期(status 超过 15 秒没取到)
  const sat = state.at && state.at.status;
  if (sat && Date.now() - sat > 15000) add(14, 'warn', { title: t('dash.attn.statusStale', { time: fmtTime(Math.floor(sat / 1000)) }), names: errText(state.err && state.err.status), acts: btn('dash.retry', 'status', t('dash.retry')) });
  // 15 数据源第一次就没取到:不能说"一切正常"
  const src = (key, err, data, id) => { if (err && !data) add(15, 'warn', { title: t('dash.attn.srcFail', { what: t('dash.attn.src.' + key) }), names: errText(err), acts: btn('dash.retry', id, t('dash.retry')) }); };
  if (!node) src('nodes', nodesErr, nodesData, 'nodes');
  src('health', healthErr, health, 'dash-health');
  src('conns', connsErr, conns, 'dash-conns');
  items.sort((a, b) => SEV[a.sev] - SEV[b.sev] || a.no - b.no);
  return { items, ignored };
}

function attnRow(r) {
  const parts = (r.parts || []).filter(Boolean);
  const plain = [r.title, ...parts.map(p => p.replace(/<[^>]+>/g, ''))].join(' · ');
  return `<li class="${r.sev}"><span class="sev"></span><span class="what"><span class="ttl" title="${esc(plain.replace(/&amp;/g, '&'))}"><b>${esc(r.title)}</b>${parts.length ? ` <span class="parts">${parts.join(' · ')}</span>` : ''}</span>${r.names ? `<span class="names" title="${esc(r.names)}">${esc(r.names)}</span>` : ''}</span><span class="acts">${r.acts || ''}${r.go ? `<a class="go" href="${r.go}">${esc(r.goText || t('dash.attn.go'))}</a>` : ''}</span></li>`;
}

function renderAttn() {
  const box = document.getElementById('dash-attn');
  if (!box) return;
  const { items, ignored } = attnItems();
  const serious = items.filter(i => i.sev !== 'info');
  const pending = (!isNode() && !nodesData && !nodesErr) || (!health && !healthErr) || (!conns && !connsErr);
  const ok = !serious.length && !pending;
  document.getElementById('dash-attn-card')?.classList.toggle('ok', ok);
  setHTML('dash-attn-n', serious.length ? esc(t('dash.attn.count', { n: serious.length })) : ok ? `<span class="attn-ok">${esc(t('dash.attn.ok'))}</span>` : '');
  const cap = narrow() && !attnAll ? 4 : items.length;
  let html = !items.length && pending ? `<p class="muted small">${esc(t('dash.attn.checking'))}</p>` : '';
  if (items.length) html += `<ul class="attn">${items.slice(0, cap).map(attnRow).join('')}</ul>`;
  if (items.length > cap) html += `<div class="card-foot"><button class="btn sm ghost" data-act="dash.attnAll">${esc(t('dash.moreN', { n: items.length - cap }))} ▾</button></div>`;
  if (ignored.length) {
    html += `<div class="attn-ignored"><button class="link-btn muted small" data-act="dash.attnIgnored" aria-expanded="${attnIgnoredOpen}">${esc(t('dash.attn.ignored', { n: ignored.length }))} ${attnIgnoredOpen ? '▾' : '▸'}</button>`
      + (attnIgnoredOpen ? `<ul class="attn" style="margin-top:.35rem">${ignored.map(r => attnRow({ ...r, sev: 'info', acts: btn('dash.undismiss', r.kind, t('dash.attn.restore'), 'ghost') })).join('')}</ul>` : '') + '</div>';
  }
  setHTML(box, html);
}

// ---- 统计条:固定 6 张,每张一行数值一行说明,放不下省略号、全文在 title;有去处的卡就是链接 ----
const stat = ({ k, v = '', s = '', kind = '', href = '', title = '', html = '' }) => {
  const tag = href ? 'a' : 'div';
  return `<${tag}${href ? ` href="${href}"` : ''} class="stat ${kind}"${title ? ` title="${esc(title)}"` : ''}><div class="k">${esc(k)}</div>${html || `<div class="v">${esc(v)}</div><div class="s">${esc(s) || '&nbsp;'}</div>`}</${tag}>`;
};
const pct = (a, b) => (b ? Math.round(a / b * 100) : 0);
const meter = (k, p, txt) => `<div class="meter"><span>${esc(k)}</span><div class="progress ${p >= 90 ? 'danger' : p >= 80 ? 'warn' : ''}"><div class="bar" style="width:${Math.min(100, Math.max(0, p))}%"></div></div><span>${esc(txt)}</span></div>`;

function loadCard() {
  const s = state.status;
  const mem = pct(s.memUsed, s.memTotal), cpu = s.cpu != null ? Math.round(s.cpu) : null;
  const disk = s.disk && s.disk.total ? pct(s.disk.total - s.disk.free, s.disk.total) : null;
  const worst = Math.max(cpu || 0, mem, disk || 0);
  const title = t('dash.load.title', { mem: s.memTotal ? `${fmtBytes(s.memUsed, 1)} / ${fmtBytes(s.memTotal, 1)}` : '—', disk: disk != null ? `${fmtBytes(s.disk.free, 1)} / ${fmtBytes(s.disk.total, 1)}` : '—' });
  return stat({ k: t('dash.load'), href: '#/ops', kind: worst >= 90 ? 'bad' : worst >= 80 ? 'warn' : '', title,
    html: `<div class="meters">${meter('CPU', cpu || 0, cpu != null ? cpu + '%' : '—')}${meter(t('dash.mem'), mem, s.memTotal ? mem + '%' : '—')}${disk != null ? meter(t('dash.disk'), disk, disk + '%') : ''}</div>` });
}

function renderStats() {
  const el = document.getElementById('dash-stats');
  if (!el || isReseller()) return;
  const s = state.status || {}, node = isNode(), many = multi();
  const local = node ? ' · ' + t('dash.scope.local') : '';
  const d = range === 24 ? dayFromChart : day;
  const counts = (state.onlines && state.onlines.connCounts) || {};
  const conns = Object.values(counts).reduce((a, b) => a + (Number(b) || 0), 0);
  const total = (s.users || 0) + (s.resellerUsers || 0);
  const cards = [
    stat({ k: t('dash.onlineUsers') + local, v: t('dash.people', { n: s.onlineUsers ?? 0 }), href: '#/users?f=online',
      s: node ? t('dash.chip.conns', { n: conns }) : s.resellerUsers ? t('dash.online.sub', { total, rs: s.resellerUsers }) : t('dash.online.subAll', { total }),
      title: node ? '' : t('dash.online.title', { direct: s.users ?? 0, enabled: s.enabledUsers ?? 0, rs: s.resellerUsers ?? 0 }) }),
    stat({ k: t('dash.trafficToday') + local, v: d ? fmtBytes((d.up || 0) + (d.down || 0), 1) : '—', s: d ? `↑ ${fmtBytes(d.up || 0, 1)} ↓ ${fmtBytes(d.down || 0, 1)}` : (dayErr ? errText(dayErr) : ''),
      title: many ? t('dash.scope.all') : '' }),
  ];
  if (node) cards.push(syncCard());
  else if (many) {
    const sum = nodesSum();
    const parts = sum ? [sum.offline && t('dash.attn.node.offline', { n: sum.offline }), sum.fault && t('dash.nodes.fault', { n: sum.fault }), sum.warn && t('dash.nodes.warn', { n: sum.warn })].filter(Boolean) : [];
    cards.push(stat({ k: t('nav.nodes'), href: sum && (sum.red || sum.yellow) ? '#/nodes?f=bad' : '#/nodes', kind: sum ? (sum.red ? 'bad' : sum.yellow ? 'warn' : '') : '',
      v: sum ? `${sum.online} / ${sum.total}` : String(s.nodes || '—'),
      s: !sum ? (nodesErr ? t('dash.attn.srcFail', { what: t('dash.attn.src.nodes') }) : '…') : parts.length ? parts.join(' · ') : t('dash.nodes.allOk'),
      title: sum ? etc(sum.names, 'nodes', 20) : '' }));
  } else {
    const left = s.certDaysLeft;
    cards.push(stat({ k: t('dash.cert'), href: '#/cert', kind: !s.certExists || left < 0 ? 'bad' : left < 14 ? 'warn' : '',
      v: s.certExists ? (left < 0 ? t('alert.certExpired') : t('user.daysLeft', { n: left ?? '—' })) : t('dash.cert.none'),
      s: s.certExists ? (s.certSelfSigned ? t('dash.cert.self') : t('dash.cert.ca')) : '' }));
  }
  const running = ((state.onlines && state.onlines.lines) || []).length;
  const localLines = node && s.linesLocal != null;
  cards.push(stat({ k: t('dash.lines') + (localLines ? local : ''), href: '#/lines', v: localLines ? `${s.linesLocalEnabled ?? 0} / ${s.linesLocal}` : `${s.linesEnabled ?? 0} / ${s.lines ?? 0}`,
    s: running ? t('dash.localRunning', { n: running }) : t('common.enabled'), title: localLines ? t('dash.linesLocalTitle', { n: s.lines ?? 0 }) : '' }));
  const hs = healthSummary(health);
  const localUps = node && hs;
  cards.push(stat({ k: t('dash.upstreams') + (localUps ? ' · ' + t('dash.scope.localUsed') : ''), href: hs && hs.bad.length ? '#/upstreams?f=bad' : '#/upstreams', kind: hs && hs.bad.length ? 'bad' : '', v: String(localUps ? hs.rows.length : s.upstreams ?? 0),
    title: localUps ? t('dash.upsLocalTitle', { n: s.upstreams ?? 0 }) : '',
    s: hs ? (hs.bad.length ? t('dash.up.bad', { n: hs.bad.length }) : t('dash.up.ok', { ago: fmtRelative(hs.lastRun) })) : health ? t('dash.healthNever') : '' }));
  cards.push(loadCard());
  el.classList.toggle('stale', !!(state.at && state.at.status && Date.now() - state.at.status > 15000));
  setHTML(el, cards.join(''));
}

// 副机:与主机同步(修订号 + 主机最近联系 / 最近应用)
function syncCard() {
  const s = state.status, nd = nodesData || {};
  const ha = s.ha || {}, hn = ha.node || {};
  // 主机最近一次联系本机:开了容灾时用它的租约(完整视图给 leaseAgo,小结只在失联时给 lost 秒数),否则用 hubSeenAt
  const lease = typeof hn.leaseAgo === 'number' && (ha.epoch > 0 || ha.node) ? now() - hn.leaseAgo : typeof ha.lost === 'number' && ha.lost > 0 ? now() - ha.lost : 0;
  const seen = lease || s.hubSeenAt || 0;
  const applied = Number(nd.appliedAt) || 0;
  const lost = !!hn.lost || (typeof ha.lost === 'number' && ha.lost > 0);
  const silent = seen && now() - seen > 120;
  const sub = s.applyErr ? t('dash.sync.applyErr', { err: trErr(s.applyErr) })
    : seen ? t('dash.sync.seen', { ago: fmtRelative(seen) }) + (applied ? ' · ' + t('dash.sync.applied', { ago: fmtRelative(applied) }) : '')
    : applied ? t('dash.sync.applied', { ago: fmtRelative(applied) }) : nodesData ? t('dash.sync.never') : nodesErr ? errText(nodesErr) : '…';
  return stat({ k: t('dash.sync'), kind: lost || s.applyErr ? 'bad' : silent || (nodesData && !applied) ? 'warn' : '',
    v: nd.revision ? t('dash.sync.rev', { rev: String(nd.revision).slice(0, 8) }) : '—',
    s: sub,
    title: nodesData ? [nd.revision || '—', nd.masterId ? t('role.master') + ' #' + nd.masterId : '', applied ? t('dash.sync.applied', { ago: fmtDate(applied) }) : ''].filter(Boolean).join(' · ') : '' });
}

// ---- 数据面:固定 6 行 ----
function roleBadge(s) {
  const ha = s.ha || {};
  const rank = ha.rank || (ha.node && ha.node.rank) || 0;
  if (ha.role === 'demoted') return badge(t('dash.role.demoted'), 'danger');
  if (ha.role === 'promoting') return badge(t('dash.role.promoting'), 'warn');
  if (s.role !== 'node' && ha.became && ha.former) return badge(t('dash.role.took'), 'warn');
  if (s.role === 'node' && rank > 0) return badge(t('dash.role.standby', { n: rank }), 'node');
  return s.role === 'node' ? badge(t('role.node'), 'node') : badge(t('role.master'), 'primary');
}
function renderCore() {
  const s = state.status || {}, rl = s.reload || {};
  const up = updateInfo();
  const upd = up && up.hasUpdate ? ` <button type="button" class="badge primary" data-act="dash.update" title="${esc(t('update.available', { v: up.latest }))}">${esc(t('dash.updateTo', { v: up.latest }))}</button>` : '';
  setHTML('dash-core', `
    <dt>${esc(t('common.status'))}</dt><dd class="ell">${badge(s.coreRunning ? t('dash.running') : t('dash.stopped'), s.coreRunning ? 'ok' : 'danger')} <span class="muted small" title="${esc(t('dash.uptimeHelp'))}">${esc(t('dash.uptime'))} ${esc(fmtDuration(s.uptime))}${s.bootTime ? ' · ' + esc(t('dash.boot', { ago: fmtRelative(s.bootTime) })) : ''}</span></dd>
    <dt>${esc(t('set.role.current'))}</dt><dd>${roleBadge(s)}</dd>
    <dt>${esc(t('dash.lastReload'))}</dt><dd class="ell" title="${esc(rl.error ? trErr(rl.error) : '')}">${rl.at ? `${esc(fmtRelative(rl.at))} · ${esc(trErr(rl.op || ''))} ${rl.ok ? badge(t('dash.reloadOk'), 'ok') : badge(t('dash.reloadFail'), 'danger')}` : '—'}</dd>
    <dt>${esc(t('set.version'))}</dt><dd class="ell"><span class="mono" title="m-ui ${esc(s.version || '')} · sing-box ${esc(s.singBox || '—')}">m-ui ${esc(s.version || '')} · sing-box ${esc(s.singBox || '—')}</span>${upd}</dd>
    <dt>${esc(t('set.webDomain'))}</dt><dd class="ell" title="${esc(s.domain || '')}">${esc(s.domain || '—')}</dd>
    <dt>${esc(t('dash.dbSize'))}</dt><dd class="mono" title="goroutines ${esc(s.goroutines ?? '')}">${s.dbSize ? fmtBytes(s.dbSize, 1) : '—'}</dd>`);
}

// ---- 流量走势:按容器像素宽出图,宽度变了用缓存重画(不重新请求)----
let chartRO = null;
function watchChart() {
  if (chartRO) { chartRO.disconnect(); chartRO = null; }
  const el = document.getElementById('dash-chart');
  if (!el || typeof ResizeObserver === 'undefined') return;
  let w = Math.round(el.clientWidth);
  chartRO = new ResizeObserver(() => {
    if (!el.isConnected) { chartRO.disconnect(); chartRO = null; return; }
    const nw = Math.round(el.clientWidth);
    if (!el.__chart || Math.abs(nw - w) < 24) return;
    w = nw;
    barChart(el, el.__chart.points, el.__chart.opts);
  });
  chartRO.observe(el);
}
async function renderChart() {
  const el = document.getElementById('dash-chart');
  if (!el) return;
  const d = await get(`stats?resource=user&hours=${range}&bucket=${bucketFor(range)}&tz=${tzOffsetMinutes()}`);
  if (range === 24) dayFromChart = { up: d.totalUp || 0, down: d.totalDown || 0 };
  barChart(el, d.points || [], { span: d.span, upLabel: t('dash.up'), downLabel: t('dash.down'), totalLabel: t('dash.total'), peakLabel: t('dash.peak'), emptyText: t('dash.noTraffic') });
  renderStats();
}

// ---- 当前在线:按设备数(没有时按本机连接数)排;先露一屏的量,+N 原地展开(限高滚动);人多才出搜索框 ----
function renderOnline() {
  const el = document.getElementById('dash-online');
  if (!el) return;
  const o = state.onlines || {};
  const users = o.users || [], counts = o.connCounts || {}, devs = o.devices || null, owners = o.owners || null;
  const n = users.length;
  const max = Math.min(40, Math.max(2, Math.floor((el.clientWidth || 360) / 125)) * 6);
  const rs = owners ? users.filter(u => owners[u]).length : 0;
  setHTML('dash-online-n', n ? esc(t('dash.people', { n }) + (owners ? ' · ' + t('dash.online.split', { a: n - rs, b: rs }) : '')) : '');
  const q = document.getElementById('dash-online-q');
  if (q) q.hidden = n <= max && !onlineQ;
  const oat = state.at && state.at.onlines;
  staleMark('dash-online', oat && Date.now() - oat > 15000 ? (state.err && state.err.onlines) || 'stale' : null, oat);
  if (!n) { setHTML(el, empty(t('dash.noOnline'))); return; }
  const key = u => (devs ? Number(devs[u]) || 0 : Number(counts[u]) || 0);
  let list = users.slice().sort((a, b) => key(b) - key(a) || a.localeCompare(b));
  if (onlineQ) list = list.filter(u => u.toLowerCase().includes(onlineQ.toLowerCase()));
  const chip = u => {
    const dv = devs ? Number(devs[u]) || 0 : 0, cn = Number(counts[u]) || 0;
    const tip = [u, devs ? t('dash.chip.devices', { n: dv }) : cn ? t('dash.chip.conns', { n: cn }) : '', owners && owners[u] ? owners[u] : ''].filter(Boolean).join(' · ');
    const href = owners && owners[u] && !isReseller() ? `#/resellers?q=${enc(owners[u])}` : `#/users?q=${enc(u)}`;
    const num = devs ? dv : cn;
    return `<a class="chip" href="${href}" title="${esc(tip)}">${dot(true)}<span class="cn">${esc(u)}</span>${num ? ` <span class="muted">${num}</span>` : ''}</a>`;
  };
  let body;
  const mob = narrow();
  if (onlineQ) body = list.length ? `<span class="chips ${mob ? '' : 'scroll'}">${list.slice(0, mob ? max + onlineMore : 300).map(chip).join('')}</span>` : empty(t('dash.online.noMatch'));
  else if (mob) { // 手机:原地多露一批,不开内滚区
    const shown = Math.min(list.length, max + onlineMore, 300); // 最多也只画 300 个,再多请搜索或到用户页
    const left = list.length - shown;
    body = `<span class="chips">${list.slice(0, shown).map(chip).join('')}${left && shown < 300 ? `<button type="button" class="chip more" data-act="dash.onlineMore">+${left}</button>` : ''}</span>`
      + (left && shown >= 300 ? `<p class="muted small">${esc(t('dash.online.cap', { n: left }))}</p>` : '');
  } else {
    body = chipList(list.map(chip), { key: 'dash-online', max, lazy: true, cap: 300 });
    if (chipOpen('dash-online') && n > 300) body += `<p class="muted small">${esc(t('dash.online.cap', { n: n - 300 }))}</p>`;
  }
  const folded = !onlineQ && (mob ? n > max + onlineMore : !chipOpen('dash-online') && n > max);
  setHTML(el, body + (folded ? `<div class="card-foot"><a href="#/users?f=online"${isReseller() ? '' : ` title="${esc(t('dash.online.viewAllTitle'))}"`}>${esc(t('dash.online.viewAll'))}</a></div>` : ''));
}

// ---- 上游巡检:按上游分组(不是"上游 × 服务器"),徽章一律按条数;故障的按坏的台数排,最多 5 条(手机 3 条)----
function renderHealth() {
  const el = document.getElementById('dash-health');
  if (!el) return;
  const hs = healthSummary(health);
  if (!hs) { setHTML(el, empty(t('dash.healthNever'))); return; }
  const meta = `<p class="small muted">${esc(t('dash.healthLast'))} ${esc(fmtRelative(hs.lastRun))}${hs.interval ? ' · ' + esc(t('dash.health.every', { n: hs.interval })) : ''}</p>`;
  if (!hs.rows.length) { setHTML(el, meta + empty(t('dash.health.unused'))); return; }
  const badges = `<div class="row wrap" style="margin:.35rem 0 .5rem">${badge(t('dash.health.okN', { n: hs.ok }), 'ok')}${hs.bad.length ? badge(t('dash.health.badN', { n: hs.bad.length }) + (hs.single ? '' : ' · ' + t('dash.health.servers', { n: hs.servers.size })), 'danger') : ''}${hs.pending ? badge(t('dash.health.pendN', { n: hs.pending })) : ''}</div>`;
  if (!hs.bad.length) { setHTML(el, meta + badges + `<p class="attn-ok">${esc(t('dash.health.allOk'))}</p>`); return; }
  const max = narrow() ? 3 : 5;
  const row = b => `<li><a class="nm" href="#/upstreams?f=bad" title="${esc(b.name)}">✗ ${esc(b.name)}</a>${hs.single ? '<span></span>' : `<span class="badge danger" title="${esc(etc(b.fails, 'nodes', 20))}">${esc(t('dash.health.ratio', { bad: b.fails.length, total: b.total }))}</span>`}<span class="err" title="${esc(trErr(b.err))}">${esc(trErr(b.err))}</span></li>`;
  setHTML(el, meta + badges + `<ul class="up-bad">${hs.bad.slice(0, max).map(row).join('')}</ul>` +
    (hs.bad.length > max ? `<div class="card-foot"><a href="#/upstreams?f=bad">${esc(t('dash.health.more', { n: hs.bad.length - max }))} · ${esc(t('dash.viewAll'))}</a></div>` : ''));
}

// ---- 流量排行:名字列吃掉剩余宽度,全名在 title;点名字去用户页搜这个人 ----
async function renderTop() {
  const el = document.getElementById('dash-top');
  if (!el) return;
  const rows = (await get(`stats/top?hours=${topHours}&limit=10`)) || [];
  if (!rows.length) { setHTML(el, empty(t('dash.noTop'))); return; }
  const max = Math.max(...rows.map(r => r.up + r.down)) || 1;
  setHTML(el, `<div class="top-list">${rows.map((r, i) => `<div class="top-row">
      <span class="top-rank">${i + 1}</span>
      ${r.gone ? `<span class="top-name muted" title="${esc(r.name + ' · ' + t('dash.top.gone'))}">${esc(r.name)}</span>` : `<a href="${r.owner ? '#/resellers?q=' + enc(r.owner) : '#/users?q=' + enc(r.name)}" class="top-name" title="${esc(r.owner ? r.name + ' · ' + r.owner : r.name)}">${esc(r.name)}</a>`}
      <div class="top-bar"><div style="width:${Math.max(2, Math.round((r.up + r.down) / max * 100))}%"></div></div>
      <span class="num top-val" title="↑ ${esc(fmtBytes(r.up, 1))} · ↓ ${esc(fmtBytes(r.down, 1))}">${esc(fmtBytes(r.up + r.down, 1))}</span>
    </div>`).join('')}</div>`);
}

// 在线、启用,但版本较旧不上报握手 / 认证失败的副机名(副机首页、代理面板没有服务器列表,恒为空)
const oldNodes = () => (nodesData && !isNode() && !isReseller() ? (nodesData.nodes || []).filter(n => !n.isLocal && n.enabled && n.status && n.status.ok && n.status.noConnFails).map(n => n.name) : []);

// ---- 最近入站连接:连接 / 被拒 / 握手失败三档筛选;默认 10 行(手机 6 行),「显示全部」在限高滚动区里展开 ----
function renderConns() {
  const el = document.getElementById('dash-conns');
  if (!el) return;
  const all = (Array.isArray(conns) ? conns : []).slice().sort((a, b) => (b.ts || 0) - (a.ts || 0));
  const ok = all.filter(c => !c.failed), rej = ok.filter(c => c.rejected), fails = all.filter(c => c.failed);
  const segs = [['ok', t('dash.conn.f.ok', { n: ok.length })], ['rejected', t('dash.conn.f.rejected', { n: rej.length })]];
  if (fails.length || connF === 'failed') segs.push(['failed', t('dash.conn.f.failed', { n: fails.length })]);
  setHTML('dash-conn-seg', all.length || connF !== 'ok' ? `<div class="seg sm">${segs.map(([k, v]) => `<button data-act="dash.connF" data-id="${k}" class="${k === connF ? 'active' : ''}">${esc(v)}</button>`).join('')}</div>` : '');
  const help = document.getElementById('dash-conns-help');
  if (help) help.title = t('dash.recentConnsHelp') + (fails.length ? '\n\n' + t('dash.failsHelp') : '');
  const qBox = document.getElementById('dash-conn-q');
  if (qBox) qBox.hidden = !connAll || narrow();
  // 在线但版本较旧、不上报握手 / 认证失败的副机(主机按报告里有没有 connFailsOk 标出):它们上面的问题不会出现在「握手失败」里,不能当成包没到
  const old = oldNodes();
  const oldNote = old.length ? `<p class="hint warn-text small conns-old">${esc(t('dash.failsOldNodes', { names: etc(old, 'nodes', 5) }))}</p>` : '';
  if (!all.length) { setHTML(el, empty(t('dash.noConns')) + oldNote); return; }
  const failed = connF === 'failed';
  let list = connF === 'rejected' ? rej : failed ? fails : ok;
  if (!list.length) { setHTML(el, empty(connF === 'rejected' ? t('dash.conn.noRejected', { n: ok.length }) : failed ? t('dash.conn.noFailed') : t('dash.noConns')) + oldNote); return; }
  const mob = narrow();
  const base = mob ? 6 : 10;
  const total = list.length;
  if (connAll && connQ) list = list.filter(c => matches(connQ, c.ip, c.user, c.line, c.server));
  // 桌面:「显示全部」在限高滚动区里展开;手机:原地每次多 20 条,不开内滚区。
  // 显示哪几行由 shownConns 定(每台服务器先留最近的一条,其余按时间补满):一台正被扫描的服务器挤不掉别的服务器最近那条
  const n = mob ? base + connMore : connAll ? list.length : base;
  const pick = shownConns(list, n, n);
  const shown = failed ? pick.fails : pick.rows;
  const srv = all.some(c => c.server);
  const rejB = c => c.rejected ? ` <span class="badge danger" title="${esc(t(c.rejected === 'pool' ? 'dash.rejectedPool' : 'dash.rejectedDevice') + ':' + t('dash.rejectedHelp'))}">${esc(t('dash.conn.rejected'))}</span>` : '';
  const why = c => `<span class="badge warn" title="${esc(t('dash.fail.' + c.failed + '.help') + (c.detail ? '\n' + c.detail : ''))}">${esc(t('dash.fail.' + c.failed))}</span>`;
  const wrap = el.querySelector('.conns-wrap');
  const keep = wrap ? wrap.scrollTop : 0;
  const changed = setHTML(el, `<div class="table-wrap conns-wrap ${connAll && !mob ? 'capped' : ''}"><table class="grid conns${srv ? ' srv' : ''}">
    <colgroup><col class="c-ip"><col class="c-user"><col class="c-line">${srv ? '<col class="c-srv">' : ''}<col class="c-cnt"><col class="c-last"></colgroup>
    <thead><tr><th>IP</th><th>${esc(failed ? t('dash.failWhy') : t('logs.user'))}</th><th>${esc(t('nav.lines'))}</th>${srv ? `<th>${esc(t('common.server'))}</th>` : ''}<th class="num">${esc(t('dash.connCount'))}</th><th>${esc(t('dash.connLast'))}</th></tr></thead>
    <tbody>${shown.length ? shown.map(c => `<tr class="${c.rejected ? 'rej' : ''}">
      <td class="ip"><div class="ipc"><span class="ipv mono" title="${esc(c.ip)}">${esc(c.ip)}</span>${rejB(c)}</div></td>
      <td class="user" title="${esc(failed ? '' : c.user || '')}">${failed ? why(c) : c.user ? esc(c.user) : '<span class="muted">—</span>'}</td>
      <td class="line" title="${esc(c.line)} · ${esc(c.protocol || '')}">${esc(c.line)} <span class="muted small">${esc(c.protocol || '')}</span></td>
      ${srv ? `<td class="srv" title="${esc(c.server || '')}">${esc(c.server || '')}</td>` : ''}
      <td class="num cnt">×${c.count}</td>
      <td class="mono small last" title="${esc(fmtDate(c.ts))}">${esc(fmtRelative(c.ts))}</td>
    </tr>`).join('') : `<tr><td colspan="${srv ? 6 : 5}">${empty(t('dash.online.noMatch'))}</td></tr>`}</tbody></table></div>
    ${mob ? (total > base ? `<div class="card-foot">${shown.length < list.length ? `<button class="btn sm ghost" data-act="dash.connsMore">${esc(t('dash.conn.more', { n: Math.min(MOBILE_STEP.conns, list.length - shown.length), left: list.length - shown.length }))}</button>` : ''}${connMore ? `<button class="btn sm ghost" data-act="dash.connsLess">${esc(t('common.less'))}</button>` : ''}</div>` : '')
      : total > base ? `<div class="card-foot"><button class="btn sm ghost" data-act="dash.connsAll">${esc(connAll ? t('common.less') : t('dash.conn.showAll', { n: total }))}</button></div>` : ''}${oldNote}`);
  if (changed && connAll && !mob) { const w = el.querySelector('.conns-wrap'); if (w) w.scrollTop = keep; }
}

// ---- 最近操作:默认只看人工操作(定时任务批量写的 ResetJob / DepleteJob / RuleJob 不刷屏),连续相同的合成一行 ×N ----
function auditAction(r) {
  const b = r.key === 'user' && /^batch:plan:(renew|extend|bind)(?::(.*))?$/.exec(r.action || '');
  if (b) return b[1] === 'bind' ? t('dash.audit.batchBind') : `${t('dash.audit.batch', { op: t('pc.op.' + b[1]) })} · ${b[2] || ''}`;
  if (r.key === 'user' && r.action === 'plan:bind') return t('dash.audit.bind'); // 只改所属套餐(也可能是移出套餐),额度和到期不动
  const m = r.key === 'user' && /^plan:(renew|extend|replace|stack|prorate):(.*)$/.exec(r.action || '');
  if (!m) return `${r.key}.${r.action}`;
  if (m[1] === 'renew' || m[1] === 'extend') return `${t('pc.op.' + m[1])} · ${m[2]}`;
  return `${t('dash.audit.planChange', { mode: t('pc.mode.' + m[1]) })} → ${m[2]}`;
}
async function renderAudit() {
  const el = document.getElementById('dash-audit');
  if (!el) return;
  const human = auditScope === 'human';
  const rows = (await get(human ? 'audit?limit=40&actor=human' : 'audit?limit=40')) || [];
  const list = human ? rows.filter(r => !/Job$/.test(r.actor || '')) : rows; // 老后端不认 actor 参数时也对
  const runs = [];
  for (const r of list) { const p = runs[runs.length - 1]; if (p && p.actor === r.actor && p.key === r.key && p.action === r.action) p.n++; else runs.push({ ...r, n: 1 }); }
  if (!runs.length) { setHTML(el, empty(human ? t('dash.audit.noHuman') : undefined)); return; }
  setHTML(el, `<ul class="audit-list">${runs.slice(0, 8).map(r => {
    const obj = auditText(r.obj), act = auditAction(r);
    return `<li><span class="when mono" title="${esc(fmtDate(r.dateTime))}">${esc(fmtRelative(r.dateTime))}</span><span class="what" title="${esc(`${r.actor} ${act}${r.n > 1 ? ' ×' + r.n : ''} ${auditText(r.obj, { max: 0 })}`)}"><b>${esc(r.actor)}</b> ${esc(act)}${r.n > 1 ? ` <span class="badge">×${r.n}</span>` : ''} <span class="muted">${esc(obj)}</span></span></li>`;
  }).join('')}</ul>`);
}

async function renderLog() {
  const el = document.getElementById('dash-log');
  if (!el) return;
  const lines = (await get(`logs?count=40&level=${logLevel}`)) || [];
  setHTML(el, lines.slice().reverse().map(l => {
    const cls = /ERROR/.test(l) ? 'error' : /WARN/.test(l) ? 'warn' : /DEBUG/.test(l) ? 'debug' : '';
    return `<span class="${cls}">${esc(l)}</span>`;
  }).join('\n') || esc(t('common.empty')));
}

// ---- 代理面板:需要处理 + 6 张统计卡 + 当前在线 + 即将到期 / 流量快用完两张名单 ----
function renderReseller(el) {
  el.innerHTML = `<div class="dash-grid">
    <section class="card span-all attn-card" id="dash-attn-card"><div class="card-head"><h2>${esc(t('dash.attn.title'))} <span class="h-count" id="dash-attn-n"></span></h2></div><div id="dash-attn"></div></section>
    <div class="stats span-all" id="dash-stats"></div>
    ${card('dash-online', 'span-2', `<h2>${esc(t('dash.onlineList'))} <span class="h-count" id="dash-online-n"></span></h2><input type="search" class="sm dash-q" id="dash-online-q" placeholder="${esc(t('dash.online.search'))}" value="${esc(onlineQ)}" hidden>`, `<div id="dash-online"></div>`)}
    ${card('dash-expiring', '', `<h2>${esc(t('dash.rs.expiringList'))} <span class="h-count" id="dash-expiring-n"></span></h2>`, `<div id="dash-expiring">${loading()}</div>`)}
    ${card('dash-quota', '', `<h2>${esc(t('dash.rs.quotaList'))} <span class="h-count" id="dash-quota-n"></span></h2>`, `<div id="dash-quota">${loading()}</div>`)}
  </div>`;
  document.getElementById('dash-online-q').addEventListener('input', e => { onlineQ = e.target.value.trim(); renderOnline(); });
  onChipToggle('dash-online', renderOnline);
  renderResellerStats(); renderOnline(); renderResellerAttn();
  // 名单要用户列表:不阻塞其它块
  ensure('users').then(renderResellerLists, e => {
    const msg = `<div class="dash-err"><span class="msg">⚠ ${esc(t('dash.loadFailed', { err: errText(e) }))}</span></div>`;
    setHTML('dash-expiring', msg); setHTML('dash-quota', msg);
  });
}
async function tickReseller() {
  ticks++;
  renderResellerStats(); renderOnline(); renderResellerAttn();
  if (ticks % 12 === 0) { await load('users').catch(() => {}); renderResellerLists(); }
}
function resellerNums() {
  const s = state.status || {};
  return { s, pct: s.volume ? Math.round((s.used || 0) / s.volume * 100) : 0, left: s.expiry ? daysLeft(s.expiry) : null };
}
function renderResellerStats() {
  const { s, pct: p, left } = resellerNums();
  const speed = (s.speedUp || s.speedDown) ? t('dash.rs.speed', { up: s.speedUp || '∞', down: s.speedDown || '∞' }) : t('dash.rs.noSpeed');
  const full = s.deviceLimit && s.onlineDevices >= s.deviceLimit;
  setHTML('dash-stats', [
    stat({ k: t('dash.rs.quota'), kind: s.depleted || p >= 100 ? 'bad' : p >= 80 ? 'warn' : '', v: s.volume ? `${p}%` : fmtBytes(s.used || 0, 1),
      s: s.volume ? `${fmtBytes(s.used || 0, 1)} / ${fmtBytes(s.volume, 0)}` : t('common.unlimited') }),
    stat({ k: t('dash.rs.expiry'), kind: left != null && left <= 3 ? 'bad' : left != null && left <= 7 ? 'warn' : '',
      v: left == null ? t('common.unlimited') : left < 0 ? t('user.expired') : t('user.daysLeft', { n: left }), s: s.expiry ? fmtDay(s.expiry) : '', title: s.expiry ? t('rs.expiryHelp') : '' }),
    stat({ k: t('rs.users'), href: '#/users', v: `${s.enabledUsers || 0} / ${s.users || 0}`, s: t('dash.rs.enabledAll') + (s.userLimit ? ' · ' + t('dash.rs.userLimit', { n: s.userLimit }) : '') }),
    stat({ k: t('dash.onlineUsers'), href: '#/users?f=online', v: t('dash.people', { n: s.onlineUsers || 0 }), s: t('dash.rs.traffic', { v: fmtBytes(s.trafficTotal || 0, 1) }) }),
    stat({ k: t('rs.online'), kind: full ? 'warn' : '', v: `${s.onlineDevices || 0}${s.deviceLimit ? ' / ' + s.deviceLimit : ''}`, s: t('rs.devicesSub') }),
    stat({ k: t('dash.rs.lines'), v: t('dash.rs.linesN', { n: s.lines || 0 }), s: speed }),
  ].join(''));
}
function renderResellerAttn() {
  const box = document.getElementById('dash-attn');
  if (!box) return;
  const { s, pct: p, left } = resellerNums();
  const items = [];
  if (s.depleted || (s.volume && p >= 100)) items.push({ sev: 'bad', title: t('dash.attn.rs.quotaOver') });
  else if (s.volume && p >= 80) items.push({ sev: 'warn', title: t('dash.attn.rs.quotaNear', { p }), names: t('dash.attn.rs.quotaNearHint') });
  if (left != null && left <= 7) items.push({ sev: left <= 3 ? 'bad' : 'warn', title: left < 0 ? t('dash.attn.rs.expired') : t('dash.attn.rs.expiresIn', { n: Math.max(0, left) }), names: t('rs.expiryHelp') });
  if (s.deviceLimit && s.onlineDevices >= s.deviceLimit) items.push({ sev: 'warn', title: s.poolRejects > 0 ? t('rs.poolFull', { n: s.poolRejects }) : t('dash.attn.rs.poolFull'), names: t('dash.attn.rs.poolFullHint', { a: s.onlineDevices, b: s.deviceLimit }) });
  if (s.userLimit && (s.users || 0) >= s.userLimit * 0.9) items.push({ sev: 'warn', title: t('dash.attn.rs.userLimit', { a: s.users || 0, b: s.userLimit }) });
  const sat = state.at && state.at.status;
  if (sat && Date.now() - sat > 15000) items.push({ sev: 'warn', title: t('dash.attn.statusStale', { time: fmtTime(Math.floor(sat / 1000)) }), names: errText(state.err && state.err.status), acts: btn('dash.retry', 'status', t('dash.retry')) });
  items.sort((a, b) => SEV[a.sev] - SEV[b.sev]);
  document.getElementById('dash-attn-card')?.classList.toggle('ok', !items.length);
  setHTML('dash-attn-n', items.length ? esc(t('dash.attn.count', { n: items.length })) : `<span class="attn-ok">${esc(t('dash.attn.ok'))}</span>`);
  setHTML(box, items.length ? `<ul class="attn">${items.map(attnRow).join('')}</ul>` : '');
  document.getElementById('dash-stats')?.classList.toggle('stale', !!(sat && Date.now() - sat > 15000));
}
function renderResellerLists() {
  const ts = now();
  const users = state.users || [];
  const exp = users.filter(u => u.enabled && u.expiry > ts && u.expiry - ts <= 7 * 86400).sort((a, b) => a.expiry - b.expiry);
  const ratio = u => (u.up + u.down) / u.volume;
  const quota = users.filter(u => u.enabled && u.volume > 0 && ratio(u) >= 0.8).sort((a, b) => ratio(b) - ratio(a));
  const max = narrow() ? 5 : 8;
  const li = (u, right) => `<li><a href="#/users?q=${enc(u.name)}" class="nm" title="${esc(u.name)}">${esc(u.name)}</a>${right}</li>`;
  const more = (list, f) => (list.length > max ? `<div class="card-foot"><a href="#/users?f=${f}">${esc(t('dash.rs.moreUsers', { n: list.length - max }))}</a></div>` : '');
  setHTML('dash-expiring-n', exp.length ? esc(t('dash.people', { n: exp.length })) : '');
  setHTML('dash-expiring', exp.length ? `<ul class="mini-list">${exp.slice(0, max).map(u => { const d = daysLeft(u.expiry); return li(u, badge(t('user.daysLeft', { n: d }), d <= 3 ? 'warn' : '')); }).join('')}</ul>${more(exp, 'expiring')}` : empty(t('dash.rs.noExpiring')));
  setHTML('dash-quota-n', quota.length ? esc(t('dash.people', { n: quota.length })) : '');
  setHTML('dash-quota', quota.length ? `<ul class="mini-list">${quota.slice(0, max).map(u => li(u, badge(Math.round(ratio(u) * 100) + '%', ratio(u) >= 1 ? 'danger' : 'warn'))).join('')}</ul>${more(quota, 'quota')}` : empty(t('dash.rs.noQuota')));
}

const retryFns = {
  'dash-chart': () => guard('dash-chart', renderChart), 'dash-log': () => guard('dash-log', renderLog), 'dash-audit': () => guard('dash-audit', renderAudit),
  'dash-top': () => guard('dash-top', renderTop), 'dash-health': () => guard('dash-health', fetchHealth), 'dash-conns': () => guard('dash-conns', fetchConns),
  nodes: () => fetchNodes().catch(() => {}), status: () => load('status').catch(() => {}),
};
registerActions({
  'dash.range': async id => {
    range = Number(id);
    document.querySelectorAll('#dash-range button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    await guard('dash-chart', renderChart);
    if (range !== 24 && !day) { await fetchDay(); renderStats(); }
  },
  'dash.reload': async (_, b) => {
    b.disabled = true;
    try { await post('reload', undefined, SLOW); toast(t('dash.reloaded'), 'ok'); await load('status'); renderCore(); renderAttn(); }
    catch (e) { toast(e.message, 'err'); }
    finally { b.disabled = false; }
  },
  'dash.loglevel': async (_, sel) => { logLevel = sel.value; await guard('dash-log', renderLog); },
  'dash.logToggle': async () => {
    logOpen = !logOpen;
    lsSet('m-ui.dashLog', logOpen ? '1' : '0');
    const c = document.getElementById('dash-log-card');
    if (!c) return;
    c.classList.toggle('collapsed', !logOpen);
    const b = c.querySelector('.link-btn');
    b.textContent = (logOpen ? '▾ ' : '▸ ') + t('dash.coreLog');
    b.setAttribute('aria-expanded', String(logOpen));
    if (logOpen) await guard('dash-log', renderLog);
  },
  'dash.connsRefresh': () => guard('dash-conns', fetchConns).then(renderAttn),
  'dash.attnAll': () => { attnAll = true; renderAttn(); },
  'dash.attnIgnored': () => { attnIgnoredOpen = !attnIgnoredOpen; renderAttn(); },
  // 「知道了」和顶部提示条写同一个签名:这里忽略了,别的页面也不再弹;情况变了(签名变了)自动回来
  'dash.dismiss': id => {
    const s = state.status || {};
    if (id === 'cert') lsSet('m-ui.certDismissed', certSig(s.certAlerts || []));
    if (id === 'reach') lsSet('m-ui.reachDismissed', reachSig(s.reachBad || []));
    renderAttn();
  },
  'dash.undismiss': id => { lsSet(id === 'cert' ? 'm-ui.certDismissed' : 'm-ui.reachDismissed', null); renderAttn(); },
  'dash.connF': id => { connF = id; connMore = 0; renderConns(); },
  'dash.connsMore': () => { connMore += MOBILE_STEP.conns; renderConns(); },
  'dash.connsLess': () => { connMore = 0; renderConns(); document.getElementById('dash-conns-card')?.scrollIntoView({ block: 'start' }); },
  'dash.onlineMore': () => { onlineMore += MOBILE_STEP.online; renderOnline(); },
  'dash.connsAll': () => { connAll = !connAll; if (!connAll) connQ = ''; const q = document.getElementById('dash-conn-q'); if (q && !connAll) q.value = ''; renderConns(); },
  'dash.showRejected': () => { connF = 'rejected'; renderConns(); document.getElementById('dash-conns-card')?.scrollIntoView({ block: 'start', behavior: 'smooth' }); },
  'dash.auditScope': async id => {
    auditScope = id;
    document.querySelectorAll('#dash-audit-seg button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    await guard('dash-audit', renderAudit);
  },
  'dash.retry': async id => { await (retryFns[id] || (async () => {}))(); renderStats(); renderAttn(); },
  'dash.toprange': async id => {
    topHours = Number(id);
    document.querySelectorAll('#dash-toprange button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    await guard('dash-top', renderTop);
  },
  'dash.update': () => promptUpdate(),
  // 事件委托会拦下链接的默认跳转:这里自己开新标签页
  'dash.starGo': (_, a) => { window.open(a.href, '_blank', 'noopener'); lsSet('m-ui.starDismissed', '1'); document.getElementById('star-card')?.remove(); },
  'dash.doneHide': () => { lsSet('m-ui.starDismissed', '1'); lsSet('m-ui.hideQuickStart', '1'); document.getElementById('star-card')?.remove(); },
  'dash.hideQs': () => { lsSet('m-ui.hideQuickStart', '1'); document.querySelector('.quickstart')?.remove(); },
  'dash.healthRun': async (_, b) => {
    b.disabled = true;
    try { await post('upstreams/health', undefined, SLOW); await guard('dash-health', fetchHealth); renderAttn(); renderStats(); }
    catch (e) { toast(e.message, 'err'); }
    finally { b.disabled = false; }
  },
});

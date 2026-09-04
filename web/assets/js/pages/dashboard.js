import { state, load, isReseller } from '../app.js';
import { get, post, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDuration, fmtRelative, fmtTime, tzOffsetMinutes, toast, registerActions, badge, dot, empty } from '../ui.js';
import { barChart, bucketFor } from '../chart.js';

export const title = () => t('nav.dashboard');
let range = 24, logLevel = 'info', topHours = 24;

async function renderTop() {
  const el = document.getElementById('dash-top');
  if (!el) return;
  const rows = await get(`stats/top?hours=${topHours}&limit=10`);
  if (!rows.length) { el.innerHTML = empty(t('dash.noTop')); return; }
  const max = Math.max(...rows.map(r => r.up + r.down)) || 1;
  el.innerHTML = `<div class="top-list">${rows.map((r, i) => `<div class="top-row">
      <span class="top-rank">${i + 1}</span>
      <a href="#/users" class="top-name">${esc(r.name)}</a>
      <div class="top-bar"><div style="width:${Math.max(2, Math.round((r.up + r.down) / max * 100))}%"></div></div>
      <span class="num top-val">${fmtBytes(r.up + r.down, 1)} <span class="muted small">↑${fmtBytes(r.up, 0)} ↓${fmtBytes(r.down, 0)}</span></span>
    </div>`).join('')}</div>`;
}

// 快速开始清单:关键步骤未完成时显示,可关闭(记在浏览器里)
function quickStart() {
  const s = state.status;
  if (s.role === 'node') return '';
  let hidden = false;
  try { hidden = localStorage.getItem('m-ui.hideQuickStart') === '1'; } catch {}
  const items = [
    { ok: !s.defaultPassword, page: 'admin', text: t('qs.password'), opt: false },
    { ok: !!s.domain, page: 'settings', text: t('qs.domain'), opt: false },
    { ok: s.certExists && !s.certSelfSigned, page: 'cert', text: s.certExists && s.certSelfSigned ? t('qs.certSelf') : t('qs.cert'), opt: false },
    { ok: (s.lines || 0) > 0, page: 'lines', text: t('qs.lines'), opt: false },
    { ok: (s.users || 0) > 0, page: 'users', text: t('qs.users'), opt: false },
    { ok: (s.upstreams || 0) > 0, page: 'upstreams', text: t('qs.upstreams'), opt: true },
    { ok: !!s.tgEnabled, page: 'settings', text: t('qs.telegram'), opt: true },
    { ok: (s.nodes || 0) > 1, page: 'nodes', text: t('qs.nodes'), opt: true },
    { ok: (s.plans || 0) > 0, page: 'plans', text: t('qs.plans'), opt: true },
  ];
  const requiredDone = items.filter(i => !i.opt).every(i => i.ok);
  if (hidden || (requiredDone && items.every(i => i.ok))) return '';
  return `<section class="card quickstart">
    <div class="card-head"><h2>${t('qs.title')}</h2><button class="btn ghost sm" data-act="dash.hideQs" title="${t('qs.hide')}">✕</button></div>
    <p class="hint">${t('qs.help')}</p>
    <ol class="qs-list">${items.map(i => `<li class="${i.ok ? 'done' : ''}"><span class="qs-mark">${i.ok ? '✓' : '○'}</span><a href="#/${i.page}">${esc(i.text)}</a>${i.opt ? `<span class="badge">${t('qs.optional')}</span>` : ''}</li>`).join('')}</ol>
  </section>`;
}

export async function render(el) {
  if (isReseller()) return renderReseller(el);
  el.innerHTML = `
    ${quickStart()}
    <div class="stats" id="dash-stats"></div>
    <div class="grid-2">
      <section class="card">
        <div class="card-head"><h2>${t('dash.traffic')}</h2>
          <div class="seg" id="dash-range">${[1, 6, 24, 168].map(h => `<button data-act="dash.range" data-id="${h}" class="${h === range ? 'active' : ''}">${t('dash.range.' + (h === 168 ? '7d' : h + 'h'))}</button>`).join('')}</div>
        </div>
        <div id="dash-chart"></div>
      </section>
      <section class="card">
        <div class="card-head"><h2>${t('dash.core')}</h2><button class="btn sm" data-act="dash.reload">${t('dash.reload')}</button></div>
        <dl class="kv" id="dash-core"></dl>
      </section>
    </div>
    <div class="grid-2">
      <section class="card"><h2>${t('dash.onlineList')}</h2><div id="dash-online" style="margin-top:.6rem"></div></section>
      <section class="card">
        <div class="card-head"><h2>${t('dash.health')}</h2><button class="btn sm" data-act="dash.healthRun">${t('dash.healthRun')}</button></div>
        <div id="dash-health"></div>
      </section>
    </div>
    <section class="card">
      <div class="card-head"><h2>${t('dash.topUsers')}</h2>
        <div class="seg" id="dash-toprange">${[24, 168, 720].map(h => `<button data-act="dash.toprange" data-id="${h}" class="${h === topHours ? 'active' : ''}">${t('dash.range.' + (h === 24 ? '24h' : h === 168 ? '7d' : '30d'))}</button>`).join('')}</div>
      </div>
      <div id="dash-top"></div>
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('dash.recentConns')}</h2><button class="btn sm" data-act="dash.connsRefresh">${t('common.refresh')}</button></div>
      <p class="hint">${t('dash.recentConnsHelp')}</p>
      <div id="dash-conns" style="margin-top:.5rem"></div>
    </section>
    <section class="card"><h2>${t('dash.recentAudit')}</h2><div id="dash-audit" style="margin-top:.6rem"></div></section>
    <section class="card">
      <div class="card-head"><h2>${t('dash.coreLog')}</h2>
        <select class="sm" id="dash-loglevel" data-change="dash.loglevel" style="width:auto">
          ${['debug', 'info', 'warning', 'error'].map(l => `<option ${l === logLevel ? 'selected' : ''}>${l}</option>`).join('')}
        </select>
      </div>
      <pre class="log" id="dash-log"></pre>
    </section>`;
  renderStats();
  renderCore();
  await Promise.all([renderChart(), renderOnline(), renderAudit(), renderLog(), renderHealth(), renderConns(), renderTop(), refreshNodeSummary().then(renderStats)]);
}

// 每 10 秒:状态卡、数据面、在线、最近入站连接、日志;每 30 秒:流量图(含 24h 流量卡)、排行、巡检、审计
let ticks = 0;
export async function tick() {
  if (isReseller()) { renderResellerStats(); renderOnline(); return; }
  ticks++;
  await refreshNodeSummary();
  renderStats(); renderCore(); renderOnline();
  const jobs = [renderConns(), renderLog()];
  if (ticks % 3 === 0) jobs.push(renderChart(), renderTop(), renderHealth(), renderAudit());
  await Promise.allSettled(jobs);
}

// ---- 代理面板的概览:只有自己的额度、用户与在线 ----
function renderReseller(el) {
  el.innerHTML = `
    <div class="stats" id="dash-stats"></div>
    <section class="card"><h2>${t('dash.onlineList')}</h2><div id="dash-online" style="margin-top:.6rem"></div></section>`;
  renderResellerStats();
  renderOnline();
}

function renderResellerStats() {
  const el = document.getElementById('dash-stats');
  if (!el) return;
  const s = state.status || {};
  const pct = s.volume ? Math.min(100, Math.round((s.used || 0) / s.volume * 100)) : 0;
  const card = (label, value, sub, kind) => `<div class="stat ${kind || ''}"><div class="stat-label">${label}</div>
    <div class="stat-value">${value}</div>${sub ? `<div class="stat-sub">${sub}</div>` : ''}</div>`;
  el.innerHTML = [
    card(t('rs.used'), fmtBytes(s.used || 0), s.volume ? `/ ${fmtBytes(s.volume)} · ${pct}%` : t('common.unlimited'), pct >= 100 ? 'bad' : ''),
    card(t('rs.users'), `${s.enabledUsers || 0} / ${s.users || 0}`, t('common.enabled')),
    card(t('dash.onlineUsers'), s.onlineUsers || 0, ''),
    card(t('rs.online'), `${s.onlineDevices || 0}${s.deviceLimit ? ' / ' + s.deviceLimit : ''}`, t('rs.devicesSub')),
  ].join('');
}

async function renderConns() {
  const el = document.getElementById('dash-conns');
  if (!el) return;
  const rows = await get('conns/recent');
  if (!rows.length) { el.innerHTML = empty(t('dash.noConns')); return; }
  const multi = rows.some(c => c.server);
  el.innerHTML = `<div class="table-wrap"><table class="grid conns"><thead><tr><th>IP</th><th>${t('logs.user')}</th><th>${t('nav.lines')}</th>${multi ? `<th>${t('common.server')}</th>` : ''}<th class="num">${t('dash.connCount')}</th><th>${t('dash.connLast')}</th></tr></thead><tbody>${rows.slice(0, 15).map(c =>
    `<tr>
      <td class="mono">${esc(c.ip)}</td>
      <td class="ell" title="${esc(c.user || '')}">${c.user ? esc(c.user) : '<span class="muted">—</span>'}</td>
      <td class="ell">${esc(c.line)} <span class="muted small">${esc(c.protocol)}</span></td>
      ${multi ? `<td class="ell">${esc(c.server || '')}</td>` : ''}
      <td class="num">${c.count}</td>
      <td class="mono small">${fmtTime(c.ts)}</td>
    </tr>`).join('')}</tbody></table></div>`;
}

async function renderHealth() {
  const el = document.getElementById('dash-health');
  if (!el) return;
  const h = await get('upstreams/health');
  if (!h.lastRun) { el.innerHTML = empty(t('dash.healthNever')); return; }
  const bad = (h.results || []).filter(x => !x.ok);
  el.innerHTML = `<p class="small muted">${t('dash.healthLast')} ${fmtRelative(h.lastRun)} · ${h.intervalMinutes} min</p>
    <div class="row" style="margin:.4rem 0 .6rem"><span class="badge ok">${h.results.length - bad.length} ${t('dash.healthOk')}</span><span class="badge ${bad.length ? 'danger' : ''}">${bad.length} ${t('dash.healthBad')}</span></div>
    ${bad.length ? `<div class="chips">${bad.map(x => `<a class="chip" href="#/upstreams" title="${esc(x.error)}">✗ ${esc(x.name)}</a>`).join('')}</div>` : ''}`;
}

let nodeSummary = null; // {online, total, bad:[names]},仅多服务器时填充

function renderStats() {
  const s = state.status;
  const cards = [
    [t('dash.onlineUsers'), `${s.onlineUsers ?? 0} / ${s.enabledUsers ?? 0}`, `${s.users ?? 0} ${t('common.all')}`],
    [t('dash.trafficToday'), `↑ ${fmtBytes(state._dayUp || 0, 1)}`, `↓ ${fmtBytes(state._dayDown || 0, 1)}`],
    [t('dash.lines'), `${s.linesEnabled ?? 0} / ${s.lines ?? 0}`, (state.onlines.lines || []).length ? t('dash.localRunning', { n: (state.onlines.lines || []).length }) : t('common.enabled')],
    [t('dash.upstreams'), s.upstreams ?? 0, ''],
    [t('dash.cpu'), s.cpu != null ? s.cpu.toFixed(1) + '%' : '—', ''],
    [t('dash.mem'), s.memTotal ? Math.round(s.memUsed / s.memTotal * 100) + '%' : '—', s.memTotal ? `${fmtBytes(s.memUsed, 1)} / ${fmtBytes(s.memTotal, 1)}` : ''],
  ];
  if (nodeSummary) cards.splice(3, 0, [t('nav.nodes'), `${nodeSummary.online} / ${nodeSummary.total}`, nodeSummary.bad.length ? '✗ ' + nodeSummary.bad.join(', ') : t('common.online'), 'nodes', nodeSummary.bad.length ? 'bad' : '']);
  document.getElementById('dash-stats').innerHTML = cards.map(([k, v, sub, link, kind]) =>
    `<${link ? `a href="#/${link}"` : 'div'} class="stat ${kind || ''}"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div><div class="s">${esc(sub)}</div></${link ? 'a' : 'div'}>`).join('');
}

async function refreshNodeSummary() {
  if ((state.status.nodes || 0) <= 1 || state.status.role === 'node') { nodeSummary = null; return; }
  try {
    const d = await get('nodes');
    const remote = d.nodes.filter(n => !n.isLocal && n.enabled);
    const bad = remote.filter(n => !n.status || !n.status.ok).map(n => n.name);
    nodeSummary = { online: remote.length - bad.length + 1, total: remote.length + 1, bad };
  } catch { nodeSummary = null; }
}

function renderCore() {
  const s = state.status;
  document.getElementById('dash-core').innerHTML = `
    <dt>${t('common.status')}</dt><dd>${badge(s.coreRunning ? t('dash.running') : t('dash.stopped'), s.coreRunning ? 'ok' : 'danger')}</dd>
    <dt>${t('dash.uptime')}</dt><dd>${fmtDuration(s.uptime)}</dd>
    <dt>${t('set.webDomain')}</dt><dd>${esc(s.domain || '—')}</dd>
    <dt>${t('set.role.current')}</dt><dd>${s.role === 'node' ? t('role.node') : t('role.master')}</dd>
    <dt>${t('set.version')}</dt><dd class="mono">${esc(s.version || '')}</dd>
    <dt>goroutines</dt><dd class="mono">${s.goroutines ?? ''}</dd>`;
}

async function renderChart() {
  const el = document.getElementById('dash-chart');
  if (!el) return;
  const d = await get(`stats?resource=user&hours=${range}&bucket=${bucketFor(range)}&tz=${tzOffsetMinutes()}`);
  if (range === 24) { state._dayUp = d.totalUp; state._dayDown = d.totalDown; renderStats(); }
  barChart(el, d.points, { span: d.span, upLabel: t('dash.up'), downLabel: t('dash.down'), totalLabel: t('dash.total'), peakLabel: t('dash.peak'), emptyText: t('dash.noTraffic') });
}

function renderOnline() {
  const el = document.getElementById('dash-online');
  if (!el) return;
  const o = state.onlines;
  if (!o.users.length) { el.innerHTML = empty(t('dash.noOnline')); return; }
  el.innerHTML = `<div class="chips">${o.users.map(u => {
    const usr = state.users.find(x => x.name === u); // 用户列表是进用户页才拉的,这里可能还没有
    return `<a class="chip" href="#/users" title="${usr ? (usr.onlineIps || []).length + ' IP' : ''}">${dot(true)}${esc(u)} <span class="muted">${o.connCounts[u] || 0} ${t('dash.conns')}</span></a>`;
  }).join('')}</div>`;
}

async function renderAudit() {
  const el = document.getElementById('dash-audit');
  if (!el) return;
  const rows = await get('audit?limit=8');
  if (!rows.length) { el.innerHTML = empty(); return; }
  el.innerHTML = `<dl class="kv">${rows.map(r => {
    let obj = ''; try { obj = JSON.parse(r.obj); } catch { obj = r.obj; }
    if (Array.isArray(obj)) obj = obj.join(', ');
    return `<dt class="mono">${fmtRelative(r.dateTime)}</dt><dd><b>${esc(r.actor)}</b> ${esc(r.key)}.${esc(r.action)} <span class="muted">${esc(String(obj)).slice(0, 60)}</span></dd>`;
  }).join('')}</dl>`;
}

async function renderLog() {
  const el = document.getElementById('dash-log');
  if (!el) return;
  const lines = await get(`logs?count=40&level=${logLevel}`);
  el.innerHTML = lines.slice().reverse().map(l => {
    const cls = /ERROR/.test(l) ? 'error' : /WARN/.test(l) ? 'warn' : /DEBUG/.test(l) ? 'debug' : '';
    return `<span class="${cls}">${esc(l)}</span>`;
  }).join('\n') || t('common.empty');
}

registerActions({
  'dash.range': async id => {
    range = Number(id);
    document.querySelectorAll('#dash-range button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    await renderChart();
  },
  'dash.reload': async (_, btn) => {
    btn.disabled = true;
    try { await post('reload', undefined, SLOW); toast(t('dash.reloaded'), 'ok'); await load('status'); renderCore(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'dash.loglevel': async (_, sel) => { logLevel = sel.value; await renderLog(); },
  'dash.connsRefresh': () => renderConns(),
  'dash.toprange': async id => {
    topHours = Number(id);
    document.querySelectorAll('#dash-toprange button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    await renderTop();
  },
  'dash.hideQs': () => { try { localStorage.setItem('m-ui.hideQuickStart', '1'); } catch {} document.querySelector('.quickstart')?.remove(); },
  'dash.healthRun': async (_, btn) => {
    btn.disabled = true;
    try { await post('upstreams/health'); await renderHealth(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
});

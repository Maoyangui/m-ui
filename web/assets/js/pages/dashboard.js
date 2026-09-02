import { state, load } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDuration, fmtRelative, toast, registerActions, badge, dot, empty } from '../ui.js';
import { areaChart } from '../chart.js';

export const title = () => t('nav.dashboard');
let range = 24, logLevel = 'info';

export async function render(el) {
  el.innerHTML = `
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
  await Promise.all([renderChart(), renderOnline(), renderAudit(), renderLog(), renderHealth()]);
}

export function tick() { renderStats(); renderCore(); renderOnline(); }

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

function renderStats() {
  const s = state.status;
  const cards = [
    [t('dash.onlineUsers'), `${s.onlineUsers ?? 0} / ${s.enabledUsers ?? 0}`, `${s.users ?? 0} ${t('common.all')}`],
    [t('dash.trafficToday'), `↑ ${fmtBytes(state._dayUp || 0, 1)}`, `↓ ${fmtBytes(state._dayDown || 0, 1)}`],
    [t('dash.lines'), `${(state.onlines.lines || []).length} / ${s.lines ?? 0}`, t('common.online')],
    [t('dash.upstreams'), s.upstreams ?? 0, ''],
    [t('dash.cpu'), s.cpu != null ? s.cpu.toFixed(1) + '%' : '—', ''],
    [t('dash.mem'), s.memTotal ? Math.round(s.memUsed / s.memTotal * 100) + '%' : '—', s.memTotal ? `${fmtBytes(s.memUsed, 1)} / ${fmtBytes(s.memTotal, 1)}` : ''],
  ];
  document.getElementById('dash-stats').innerHTML = cards.map(([k, v, sub]) =>
    `<div class="stat"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div><div class="s">${esc(sub)}</div></div>`).join('');
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
  const d = await get(`stats?resource=user&hours=${range}`);
  if (range === 24) { state._dayUp = d.totalUp; state._dayDown = d.totalDown; renderStats(); }
  areaChart(el, d.points, { upLabel: t('dash.up'), downLabel: t('dash.down') });
}

function renderOnline() {
  const el = document.getElementById('dash-online');
  if (!el) return;
  const o = state.onlines;
  if (!o.users.length) { el.innerHTML = empty(t('dash.noOnline')); return; }
  el.innerHTML = `<div class="chips">${o.users.map(u => {
    const usr = state.users.find(x => x.name === u);
    const ips = usr ? (usr.onlineIps || []).length : 0;
    return `<a class="chip" href="#/users" title="${ips} IP">${dot(true)}${esc(u)} <span class="muted">${o.connCounts[u] || 0} ${t('dash.conns')}</span></a>`;
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
    try { await post('reload'); toast(t('dash.reloaded'), 'ok'); await load('status'); renderCore(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'dash.loglevel': async (_, sel) => { logLevel = sel.value; await renderLog(); },
  'dash.healthRun': async (_, btn) => {
    btn.disabled = true;
    try { await post('upstreams/health'); await renderHealth(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
});

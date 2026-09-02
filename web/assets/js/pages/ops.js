import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtBytes, fmtDuration, toast, confirm, registerActions, badge, field, fv } from '../ui.js';

export const title = () => t('ops.title');
export const subtitle = () => t('ops.subtitle');
let data = null, pollTimer = null;

const yes = (ok, onText, offText) => badge(ok ? (onText || t('common.yes')) : (offText || t('common.no')), ok ? 'ok' : 'warn');

export async function render(el) {
  data = await get('ops');
  const i = data.info, w = i.warp, st = data.status, p = data.params || { swapGb: 2, noFile: 1048576, sysctl: '', defaultSysctl: '' };
  const linux = i.linux;
  const dis = linux && i.root ? '' : 'disabled';
  el.innerHTML = `
    ${!linux ? `<div class="card" style="border-color:var(--warn)">${t('ops.linuxOnly')}</div>` : (!i.root ? `<div class="card" style="border-color:var(--warn)">${t('ops.rootOnly')}</div>` : '')}
    <div class="grid-2">
      <section class="card">
        <div class="card-head"><h2>${t('ops.system')}</h2><button class="btn sm" data-act="ops.refresh">${t('common.refresh')}</button></div>
        <dl class="kv">
          <dt>${t('ops.os')}</dt><dd>${esc(i.os)} <span class="muted small">${esc(i.kernel || '')} ${esc(i.arch)}</span></dd>
          <dt>${t('ops.host')}</dt><dd class="mono">${esc(i.hostname)}</dd>
          ${linux ? `
          <dt>${t('ops.uptime')}</dt><dd>${fmtDuration(i.uptime)} <span class="muted small">load ${esc(i.load)}</span></dd>
          <dt>${t('ops.mem')}</dt><dd>${fmtBytes(i.memTotal - i.memAvail, 1)} / ${fmtBytes(i.memTotal, 1)}</dd>
          <dt>${t('ops.swap')}</dt><dd>${i.swapTotal ? `${fmtBytes(i.swapTotal - i.swapFree, 1)} / ${fmtBytes(i.swapTotal, 1)}` : badge(t('common.none'), 'warn')}</dd>
          <dt>${t('ops.disk')}</dt><dd>${i.diskTotal ? `${fmtBytes(i.diskTotal - i.diskFree, 1)} / ${fmtBytes(i.diskTotal, 1)}` : '—'}</dd>
          <dt>BBR</dt><dd>${yes(i.cc === 'bbr', 'bbr', esc(i.cc || '?'))} <span class="muted small">qdisc ${esc(i.qdisc || '?')}</span></dd>
          <dt>${t('ops.nofile')}</dt><dd>${yes(i.nofile >= 65536, String(i.nofile), String(i.nofile))}</dd>
          <dt>NTP</dt><dd>${yes(i.ntp === 'yes', t('ops.synced'), i.ntp === 'no' ? t('ops.unsynced') : '?')}</dd>` : ''}
        </dl>
      </section>
      <section class="card">
        <div class="card-head"><h2>Cloudflare WARP</h2>${w.listening ? (w.exit === 'on' || w.exit === 'plus' ? badge('warp=' + w.exit, 'ok') : badge(t('ops.warpNoExit'), 'warn')) : badge(t('ops.warpOff'), '')}</div>
        <dl class="kv">
          <dt>${t('ops.installed')}</dt><dd>${yes(w.installed)}${w.status ? ` <span class="muted small">${esc(w.status)}</span>` : ''}</dd>
          <dt>${t('ops.services')}</dt><dd class="mono">warp-svc ${esc(w.service || '—')} · warp-socks5 ${esc(w.socks || '—')}</dd>
          <dt>SOCKS5</dt><dd>${yes(w.listening, `127.0.0.1:${w.port}`, `127.0.0.1:${w.port}`)}</dd>
          <dt>${t('ops.exit')}</dt><dd>${w.listening ? `<span class="mono">${esc(w.exitIp || '—')}</span> ${esc(w.exitLoc || '')} ${esc(w.exitColo || '')} <span class="muted small">${esc(w.exit)}</span>` : '—'}</dd>
          <dt>${t('ops.upstream')}</dt><dd>${data.warpUpstream ? badge('warp', 'ok') : `<button class="btn sm" data-act="ops.warpUpstream">${t('ops.addUpstream')}</button>`}</dd>
        </dl>
        <div class="form-grid" style="margin-top:.6rem">${field(t('ops.warpPort'), `<input id="ops-port" type="number" min="1" max="65535" value="${data.warpPort}">`, t('ops.warpPortHelp'))}</div>
        <div class="row" style="margin-top:.6rem;flex-wrap:wrap">
          <button class="btn" data-act="ops.run" data-id="warp-install" ${dis}>${t('ops.t.warp-install')}</button>
          <button class="btn primary" data-act="ops.run" data-id="warp-enable" ${dis}>${t('ops.t.warp-enable')}</button>
          <button class="btn" data-act="ops.run" data-id="warp-disable" ${dis}>${t('ops.t.warp-disable')}</button>
          <button class="btn" data-act="ops.warpCheck">${t('ops.checkExit')}</button>
          <button class="btn danger" data-act="ops.run" data-id="warp-uninstall" ${dis}>${t('ops.t.warp-uninstall')}</button>
        </div>
      </section>
    </div>
    <section class="card">
      <div class="card-head"><h2>${t('ops.tune')}</h2><button class="btn primary sm" data-act="ops.run" data-id="tune-all" ${dis}>${t('ops.t.tune-all')}</button></div>
      <p class="hint" style="margin-bottom:.6rem">${t('ops.tuneHelp')}</p>
      <div class="task-grid">
        ${['swap', 'limits', 'ntp'].map(n => {
          const task = data.tasks.find(x => x.name === n);
          const done = { swap: i.swapTotal > 0, limits: i.limits, ntp: i.ntp === 'yes' }[n];
          const cur = { swap: i.swapTotal ? fmtBytes(i.swapTotal, 0) : '', limits: i.nofile ? `${t('ops.current')} ${i.nofile}` : '', ntp: '' }[n];
          return `<div class="task"><div class="task-head"><b>${esc(task.title)}</b>${linux ? yes(done, t('ops.done'), t('ops.todo')) : ''}</div><p class="hint">${esc(task.desc)}${cur ? ` · ${esc(cur)}` : ''}</p>
            <div class="row">
              ${n === 'swap' ? `<input id="ops-swap" type="number" min="1" max="64" value="${p.swapGb}" style="width:5rem"> G` : ''}
              ${n === 'limits' ? `<input id="ops-nofile" type="number" min="1024" max="4194304" step="1024" value="${p.noFile}" style="width:8rem">` : ''}
              <button class="btn sm" data-act="ops.run" data-id="${n}" ${dis}>${t('common.run')}</button></div></div>`;
        }).join('')}
      </div>
      <div class="task" style="margin-top:.75rem">
        <div class="task-head"><b>${esc(data.tasks.find(x => x.name === 'sysctl').title)}</b>${linux ? yes(i.tuned && i.cc === 'bbr', t('ops.done'), t('ops.todo')) : ''}</div>
        <p class="hint">${esc(data.tasks.find(x => x.name === 'sysctl').desc)}${linux ? ` · ${t('ops.current')} ${esc(i.cc || '?')} / ${esc(i.qdisc || '?')}` : ''}</p>
        <textarea id="ops-sysctl" class="mono" style="min-height:11rem" spellcheck="false">${esc(p.sysctl)}</textarea>
        <div class="row"><button class="btn sm ghost" data-act="ops.sysctlDefault">${t('ops.restoreDefault')}</button><span class="grow"></span><button class="btn sm" data-act="ops.run" data-id="sysctl" ${dis}>${t('common.run')}</button></div>
      </div>
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('ops.log')}</h2><div class="row">${st.running ? badge(t('ops.running', { task: st.current }), 'primary') + `<button class="btn sm danger" data-act="ops.cancel">${t('common.cancel')}</button>` : (st.last && st.last.name ? badge((st.last.ok ? '✓ ' : '✗ ') + st.last.name, st.last.ok ? 'ok' : 'danger') : '')}</div></div>
      <pre class="log" id="ops-log">${esc((st.log || []).join('\n') || t('common.empty'))}</pre>
    </section>`;
  if (st.running) startPoll();
}

export function tick() {}

function startPoll() {
  stopPoll();
  pollTimer = setInterval(async () => {
    const st = await get('ops/status').catch(() => null);
    if (!st) return;
    const log = document.getElementById('ops-log');
    if (log) { log.textContent = (st.log || []).join('\n'); log.scrollTop = log.scrollHeight; }
    if (!st.running) {
      stopPoll();
      toast(st.last && st.last.ok ? t('ops.taskDone') : t('ops.taskFailed'), st.last && st.last.ok ? 'ok' : 'err');
      if (location.hash.startsWith('#/ops')) render(document.getElementById('page'));
    }
  }, 1500);
}
function stopPoll() { if (pollTimer) clearInterval(pollTimer); pollTimer = null; }

registerActions({
  'ops.refresh': () => render(document.getElementById('page')),
  'ops.run': async (task, btn) => {
    const t2 = data.tasks.find(x => x.name === task);
    if (!await confirm(t('ops.runConfirm', { task: t2 ? t2.title : task }), { danger: !!(t2 && t2.danger) })) return;
    btn.disabled = true;
    try {
      await post('ops/run', {
        task, port: Number(fv('ops-port')) || 0, swapGb: Number(fv('ops-swap')) || 0,
        noFile: Number(fv('ops-nofile')) || 0, sysctl: (task === 'sysctl' || task === 'tune-all') ? fv('ops-sysctl') : '',
      });
      toast(t('ops.started'), 'ok');
      await render(document.getElementById('page'));
      startPoll();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'ops.cancel': async () => { await post('ops/cancel').catch(() => {}); },
  'ops.sysctlDefault': () => { document.getElementById('ops-sysctl').value = data.params.defaultSysctl; },
  'ops.warpCheck': async (_, btn) => {
    btn.disabled = true;
    try { const r = await post('ops/warp-check'); toast(`warp=${r.exit} ${r.ip || ''} ${r.loc || ''} ${r.colo || ''}`, r.exit === 'on' || r.exit === 'plus' ? 'ok' : 'err'); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'ops.warpUpstream': async (_, btn) => {
    btn.disabled = true;
    try { await post('ops/warp-upstream'); toast(t('ops.upstreamAdded'), 'ok'); render(document.getElementById('page')); }
    catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
});

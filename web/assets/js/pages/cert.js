import { state, load } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtDay, fmtRelative, toast, confirm, openModal, registerActions, badge, field, check, fv, fchk } from '../ui.js';

export const title = () => t('cert.title');
export const subtitle = () => t('cert.subtitle');
let data = null, pollTimer = null;

export async function render(el) {
  data = await get('cert');
  const i = data.info, s = data.settings;
  const st = data.status;
  el.innerHTML = `
    <div class="grid-2">
      <section class="card">
        <div class="card-head"><h2>${t('cert.current')}</h2>${certBadge(i)}</div>
        ${i.exists ? `<dl class="kv">
          <dt>${t('cert.subject')}</dt><dd>${esc(i.subject)}${(i.dnsNames || []).length ? `<div class="sub-cell">${(i.dnsNames || []).concat(i.ips || []).map(esc).join(', ')}</div>` : ''}</dd>
          <dt>${t('cert.issuer')}</dt><dd>${esc(i.issuer)} ${i.selfSigned ? badge(t('cert.selfSigned'), 'warn') : ''}</dd>
          <dt>${t('cert.validity')}</dt><dd>${fmtDay(new Date(i.notBefore).getTime() / 1000)} → ${fmtDay(new Date(i.notAfter).getTime() / 1000)}</dd>
          <dt>${t('cert.daysLeft')}</dt><dd>${i.daysLeft} ${t('common.day')}</dd>
          <dt>${t('cert.path')}</dt><dd class="mono">${esc(i.path)}</dd>
        </dl>` : `<p class="muted">${t('cert.none')}${i.error ? `<br><span class="small">${esc(i.error)}</span>` : ''}</p>`}
        <div class="chips" style="margin-top:.7rem">
          <span class="chip">${t('cert.usePanel')} ${badge(data.panelTLS ? 'HTTPS' : 'HTTP', data.panelTLS ? 'ok' : 'warn')}</span>
          <span class="chip">${t('cert.useSub')} ${badge(data.subTLS ? 'HTTPS' : 'HTTP', data.subTLS ? 'ok' : 'warn')}</span>
        </div>
        <div class="row" style="margin-top:.8rem"><button class="btn sm" data-act="cert.selfsign">${t('cert.selfsign')}</button></div>
      </section>
      <section class="card">
        <div class="card-head"><h2>${t('cert.issue')}</h2>${st.running ? badge(t('cert.running'), 'primary') : (st.lastOk ? badge(t('cert.lastOk') + ' ' + fmtRelative(st.lastOk), 'ok') : '')}</div>
        <div class="form-grid">
          ${field(t('set.webDomain'), `<input id="c-domain" value="${esc(s.acmeDomain || s.webDomain || '')}" placeholder="hk.example.com">`, t('cert.domainHelp'))}
          ${field(t('cert.email'), `<input id="c-email" value="${esc(s.acmeEmail || '')}" placeholder="admin@example.com">`, t('cert.emailHelp'))}
          ${field(t('cert.method'), `<select id="c-method" data-change="cert.method"><option value="http" ${s.acmeMethod !== 'cloudflare' ? 'selected' : ''}>${t('cert.methodHttp')}</option><option value="cloudflare" ${s.acmeMethod === 'cloudflare' ? 'selected' : ''}>${t('cert.methodCf')}</option></select>`, t('cert.methodHelp'))}
          <div id="c-cf-wrap" ${s.acmeMethod !== 'cloudflare' ? 'hidden' : ''}>${field(t('cert.cfToken'), `<input id="c-cftoken" type="password" placeholder="${s.hasCfToken ? t('cert.cfTokenKeep') : ''}">`, t('cert.cfTokenHelp'))}</div>
          ${check('c-panel', t('cert.applyPanel'), s.acmeApplyPanel)}
          ${check('c-sub', t('cert.applySub'), s.acmeApplySub)}
          ${check('c-renew', t('cert.autoRenew'), s.acmeAutoRenew, t('cert.autoRenewHelp'))}
          ${check('c-staging', t('cert.staging'), s.acmeStaging, t('cert.stagingHelp'))}
        </div>
        <div class="row" style="margin-top:.8rem;flex-wrap:wrap">
          <button class="btn" data-act="cert.precheck">${t('cert.precheck')}</button>
          <button class="btn primary" data-act="cert.issue" ${st.running ? 'disabled' : ''}>${t('cert.issueBtn')}</button>
        </div>
        <div id="c-precheck" style="margin-top:.6rem"></div>
      </section>
    </div>
    <section class="card">
      <div class="card-head"><h2>${t('cert.log')}</h2>${st.lastError ? badge(t('cert.lastFailed'), 'danger') : ''}</div>
      <pre class="log" id="c-log">${esc((st.log || []).join('\n') || t('common.empty'))}</pre>
      ${st.lastError ? `<p class="hint" style="color:var(--danger)">${esc(st.lastError)}</p>` : ''}
    </section>`;
  if (st.running) startPoll();
}

export function tick() {}

function certBadge(i) {
  if (!i.exists) return badge(t('cert.none'), 'warn');
  if (i.daysLeft < 0) return badge(t('cert.expired'), 'danger');
  if (i.daysLeft <= 14) return badge(t('cert.expiring', { n: i.daysLeft }), 'warn');
  return badge(t('cert.valid'), 'ok');
}

function startPoll() {
  stopPoll();
  pollTimer = setInterval(async () => {
    const st = await get('cert/status').catch(() => null);
    if (!st) return;
    const log = document.getElementById('c-log');
    if (log) { log.textContent = (st.log || []).join('\n'); log.scrollTop = log.scrollHeight; }
    if (!st.running) {
      stopPoll();
      toast(st.lastError ? t('cert.failed') : t('cert.done'), st.lastError ? 'err' : 'ok');
      await load('settings', 'status');
      const el = document.getElementById('page');
      if (el && location.hash.startsWith('#/cert')) render(el);
    }
  }, 1500);
}
function stopPoll() { if (pollTimer) clearInterval(pollTimer); pollTimer = null; }

registerActions({
  'cert.method': (_, sel) => { document.getElementById('c-cf-wrap').hidden = sel.value !== 'cloudflare'; },
  'cert.precheck': async (_, btn) => {
    const box = document.getElementById('c-precheck');
    btn.disabled = true;
    box.innerHTML = `<span class="muted small">${t('cert.checking')}</span>`;
    try {
      const p = await post('cert/precheck', { domain: fv('c-domain') });
      box.innerHTML = `<dl class="kv">
        <dt>${t('cert.publicIp')}</dt><dd class="mono">${esc(p.publicIp || '—')}</dd>
        ${Object.entries(p.resolved).map(([r, v]) => `<dt class="mono">${esc(r)}</dt><dd class="mono">${esc(v)} ${v === p.publicIp ? badge('✓', 'ok') : badge('✗', 'danger')}</dd>`).join('')}
        <dt>${t('cert.port80')}</dt><dd>${p.port80 === 'free' ? badge(t('cert.port80Free'), 'ok') : badge(t('cert.port80Busy'), 'danger') + ` <span class="small muted">${esc(p.port80)}</span>`}</dd>
      </dl><p class="hint">${p.dnsOk ? t('cert.dnsOk') : t('cert.dnsBad')}</p>`;
    } catch (e) { box.innerHTML = `<span class="small" style="color:var(--danger)">${esc(e.message)}</span>`; }
    finally { btn.disabled = false; }
  },
  'cert.issue': async (_, btn) => {
    const body = {
      domain: fv('c-domain').trim(), email: fv('c-email').trim(), method: fv('c-method'), cfToken: fv('c-cftoken'),
      staging: fchk('c-staging'), autoRenew: fchk('c-renew'), applyPanel: fchk('c-panel'), applySub: fchk('c-sub'),
    };
    if (!body.domain) { toast(t('cert.needDomain'), 'err'); return; }
    if (!await confirm(t('cert.issueConfirm', { domain: body.domain }))) return;
    btn.disabled = true;
    try { await post('cert/issue', body); toast(t('cert.started'), 'ok'); startPoll(); }
    catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.selfsign': () => {
    openModal(t('cert.selfsign'), `<div class="form-grid"><div class="full">${field(t('cert.hosts'), `<input id="ss-hosts" value="${esc(state.settings.webDomain || '')}" placeholder="hk.example.com, 1.2.3.4">`, t('cert.hostsHelp'))}</div></div>`, async () => {
      await post('cert/selfsign', { hosts: fv('ss-hosts') });
      toast(t('cert.done'), 'ok');
      await load('settings', 'status');
      render(document.getElementById('page'));
    });
  },
});

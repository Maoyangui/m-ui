import { state, load } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtDay, fmtRelative, toast, confirm, registerActions, badge, field, check, fv, fchk } from '../ui.js';

export const title = () => t('cert.title');
export const subtitle = () => t('cert.subtitle');
let data = null, pollTimer = null, source = '';

export async function render(el) {
  data = await get('cert');
  const i = data.info, s = data.settings, st = data.status;
  // 证书来源:有域名签发 / 无域名自签 / 服务器上已有的证书
  source = source || data.source || (s.webDomain ? 'acme' : 'selfsign');
  el.innerHTML = `
    <div class="grid-2">
      <section class="card">
        <div class="card-head"><h2>${t('cert.current')}</h2>${certBadge(i)}</div>
        ${i.exists ? `<dl class="kv">
          <dt>${t('cert.subject')}</dt><dd>${esc(i.subject)}${(i.dnsNames || []).length || (i.ips || []).length ? `<div class="sub-cell">${(i.dnsNames || []).concat(i.ips || []).map(esc).join(', ')}</div>` : ''}</dd>
          <dt>${t('cert.issuer')}</dt><dd>${esc(i.issuer)} ${sourceBadge()}</dd>
          <dt>${t('cert.validity')}</dt><dd>${fmtDay(new Date(i.notBefore).getTime() / 1000)} → ${fmtDay(new Date(i.notAfter).getTime() / 1000)}</dd>
          <dt>${t('cert.daysLeft')}</dt><dd>${i.daysLeft} ${t('common.day')}</dd>
          <dt>${t('cert.path')}</dt><dd class="mono">${esc(i.path)}</dd>
        </dl>` : `<p class="muted">${t('cert.none')}${i.error ? `<br><span class="small">${esc(i.error)}</span>` : ''}</p>`}
        <h3 class="sub-title">${t('cert.usage')}</h3>
        <div class="chips">
          <span class="chip">${t('cert.useLines')} ${badge(i.exists ? t('cert.on') : t('cert.off'), i.exists ? 'ok' : '')}</span>
          <span class="chip">${t('cert.usePanel')} ${tlsBadge(data.applyPanel, data.panelCert)}</span>
          <span class="chip">${t('cert.useSub')} ${tlsBadge(data.applySub, data.subCert)}</span>
        </div>
        <p class="hint" style="margin-top:.6rem">${i.exists && i.selfSigned ? t('cert.selfSignedNote') : t('cert.usageHelp')}</p>
        ${(data.panelCert && !data.applyPanel) || (data.subCert && !data.applySub) ? `<p class="hint" style="color:var(--warn)">${t('cert.otherCertNote')}</p>` : ''}
      </section>

      <section class="card">
        <div class="card-head"><h2>${t('cert.source')}</h2>${st.running ? badge(t('cert.running'), 'primary') : (st.lastOk ? badge(t('cert.lastOk') + ' ' + fmtRelative(st.lastOk), 'ok') : '')}</div>
        <div class="seg" id="c-source">${[['acme', t('cert.srcAcme')], ['selfsign', t('cert.srcSelf')], ['external', t('cert.srcExternal')]]
          .map(([k, label]) => `<button data-act="cert.source" data-id="${k}" class="${k === source ? 'active' : ''}">${label}</button>`).join('')}</div>
        <div id="c-form" style="margin-top:.8rem"></div>
        <h3 class="sub-title">${t('cert.applyTo')}</h3>
        <div class="form-grid">
          ${check('c-panel', t('cert.applyPanel'), data.applyPanel, t('cert.applyPanelHelp'))}
          ${check('c-sub', t('cert.applySub'), data.applySub, t('cert.applySubHelp'))}
        </div>
        <div class="row" style="margin-top:.5rem"><button class="btn sm" data-act="cert.apply">${t('cert.applySave')}</button></div>
      </section>
    </div>
    <section class="card">
      <div class="card-head"><h2>${t('cert.log')}</h2>${st.lastError ? badge(t('cert.lastFailed'), 'danger') : ''}</div>
      <pre class="log" id="c-log">${esc((st.log || []).join('\n') || t('common.empty'))}</pre>
      ${st.lastError ? `<p class="hint" style="color:var(--danger)">${esc(st.lastError)}</p>` : ''}
    </section>`;
  renderForm();
  if (st.running) startPoll();
}

export function tick() {}

// HTTPS 状态:用的是当前证书 / 设置页手填的另一张证书 / 没开
function tlsBadge(applied, path) {
  if (applied) return badge('HTTPS', 'ok');
  if (path) return badge('HTTPS · ' + t('cert.otherCert'), 'warn');
  return badge('HTTP', 'warn');
}

function sourceBadge() {
  const src = data.source;
  if (src === 'selfsign' || data.info.selfSigned) return badge(t('cert.srcSelf'), 'warn');
  if (src === 'external') return badge(t('cert.srcExternal'), 'primary');
  if (src === 'acme') return badge("Let's Encrypt", 'ok');
  return '';
}

// 三种来源各自的表单;共用下方的"应用到"勾选
function renderForm() {
  const box = document.getElementById('c-form');
  if (!box) return;
  const s = data.settings, st = data.status;
  if (source === 'acme') {
    box.innerHTML = `
      <div class="form-grid">
        ${field(t('set.webDomain'), `<input id="c-domain" value="${esc(s.acmeDomain || s.webDomain || '')}" placeholder="hk.example.com">`, t('cert.domainHelp'))}
        ${field(t('cert.email'), `<input id="c-email" value="${esc(s.acmeEmail || '')}" placeholder="admin@example.com">`, t('cert.emailHelp'))}
        ${field(t('cert.method'), `<select id="c-method" data-change="cert.method"><option value="http" ${s.acmeMethod !== 'cloudflare' ? 'selected' : ''}>${t('cert.methodHttp')}</option><option value="cloudflare" ${s.acmeMethod === 'cloudflare' ? 'selected' : ''}>${t('cert.methodCf')}</option></select>`, t('cert.methodHelp'))}
        <div id="c-cf-wrap" ${s.acmeMethod !== 'cloudflare' ? 'hidden' : ''}>${field(t('cert.cfToken'), `<input id="c-cftoken" type="password" placeholder="${s.hasCfToken ? t('cert.cfTokenKeep') : ''}">`, t('cert.cfTokenHelp'))}</div>
        ${check('c-renew', t('cert.autoRenew'), s.acmeAutoRenew, t('cert.autoRenewHelp'))}
        ${check('c-staging', t('cert.staging'), s.acmeStaging, t('cert.stagingHelp'))}
      </div>
      <div class="row" style="margin-top:.8rem;flex-wrap:wrap">
        <button class="btn" data-act="cert.precheck">${t('cert.precheck')}</button>
        <button class="btn primary" data-act="cert.issue" ${st.running ? 'disabled' : ''}>${t('cert.issueBtn')}</button>
      </div>
      <div id="c-precheck" style="margin-top:.6rem"></div>`;
  } else if (source === 'selfsign') {
    // 无域名场景不该逼用户填东西:留空即用本机探测到的公网 IP,占位里先给他看是哪些
    const auto = [state.settings.publicIp, s.webDomain].filter(Boolean).join(', ');
    box.innerHTML = `
      <p class="hint">${t('cert.selfHelp')}</p>
      <div class="form-grid" style="margin-top:.6rem">
        <div class="full">${field(t('cert.hosts'), `<input id="ss-hosts" value="" placeholder="${esc(auto || '1.2.3.4')}">`, t('cert.hostsHelp'))}</div>
      </div>
      <div class="row" style="margin-top:.8rem"><button class="btn primary" data-act="cert.selfsign">${t('cert.selfsign')}</button></div>`;
  } else {
    box.innerHTML = `
      <p class="hint">${t('cert.externalHelp')}</p>
      <div class="form-grid" style="margin-top:.6rem">
        <div class="full">${field(t('cert.certPath'), `<input id="ex-cert" value="${esc(s.certFile || '')}" placeholder="/etc/letsencrypt/live/example.com/fullchain.pem">`)}</div>
        <div class="full">${field(t('cert.keyPath'), `<input id="ex-key" value="${esc(s.keyFile || '')}" placeholder="/etc/letsencrypt/live/example.com/privkey.pem">`, t('cert.pathHelp'))}</div>
      </div>
      <div class="row" style="margin-top:.8rem"><button class="btn primary" data-act="cert.external">${t('cert.useExternal')}</button></div>`;
  }
}

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
      await reload();
    }
  }, 1500);
}
function stopPoll() { if (pollTimer) clearInterval(pollTimer); pollTimer = null; }

async function reload() {
  await load('settings', 'status');
  const el = document.getElementById('page');
  if (el && location.hash.startsWith('#/cert')) await render(el);
}

registerActions({
  'cert.source': id => {
    source = id;
    document.querySelectorAll('#c-source button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    // 自签证书不被系统信任:默认只给线路入站用,面板与订阅保持 HTTP
    const panel = document.getElementById('c-panel'), sub = document.getElementById('c-sub');
    if (id === 'selfsign') { panel.checked = false; sub.checked = false; }
    else if (!data.info.exists || data.info.selfSigned) { panel.checked = true; sub.checked = true; }
    renderForm();
  },
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
  'cert.selfsign': async (_, btn) => {
    const hosts = fv('ss-hosts').trim(); // 留空 = 后端自动用本机公网 IP
    btn.disabled = true;
    try {
      await post('cert/selfsign', { hosts, applyPanel: fchk('c-panel'), applySub: fchk('c-sub') });
      toast(t('cert.done'), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.external': async (_, btn) => {
    const body = { certFile: fv('ex-cert').trim(), keyFile: fv('ex-key').trim(), applyPanel: fchk('c-panel'), applySub: fchk('c-sub') };
    if (!body.certFile || !body.keyFile) { toast(t('cert.needPaths'), 'err'); return; }
    btn.disabled = true;
    try {
      await post('cert/external', body);
      toast(t('cert.externalOk'), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.apply': async (_, btn) => {
    btn.disabled = true;
    try {
      await post('cert/apply', { applyPanel: fchk('c-panel'), applySub: fchk('c-sub') });
      toast(t('cert.applied'), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
});

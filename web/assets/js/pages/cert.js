import { state, load } from '../app.js';
import { get, post, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtDay, fmtRelative, toast, confirm, registerActions, badge, field, check, copy, fv, fchk } from '../ui.js';
import { openCertDrawer, certLabel } from '../certpicker.js';

export const title = () => t('cert.title');
export const subtitle = () => t('cert.subtitle');
let data = null, pollTimer = null, source = '', panelChanged = false;

const certs = () => data.certs || [];
const certOf = target => certs().find(c => (target === 'lines' ? c.forLines : target === 'panel' ? c.forPanel : c.forSub));
const USES = { lines: 'cert.useLines', panel: 'cert.usePanel', sub: 'cert.useSub' };

export async function render(el) {
  data = await get('cert');
  const s = data.settings, st = data.status;
  // 证书来源:有域名签发 / 无域名自签 / 服务器上已有的证书
  source = source || data.source || (s.webDomain ? 'acme' : 'selfsign');
  el.innerHTML = `
    <div class="grid-2">
      <section class="card">
        <div class="card-head"><h2>${t('cert.inUse')}</h2>${st.running ? badge(t('cert.running'), 'primary') : (st.lastOk ? badge(t('cert.lastOk') + ' ' + fmtRelative(st.lastOk), 'ok') : '')}</div>
        <div class="cert-uses">${['lines', 'panel', 'sub'].map(useRow).join('')}</div>
        ${panelChanged ? `<div class="hint warn-text row" style="margin-top:.6rem;justify-content:space-between;flex-wrap:wrap"><span>${t('cert.panelRestart')}</span><button class="btn sm" data-act="cert.restart">${t('cert.restartBtn')}</button></div>` : ''}
        <p class="hint" style="margin-top:.6rem">${t('cert.usesHelp')}</p>
      </section>

      <section class="card">
        <div class="card-head"><h2>${t('cert.add')}</h2></div>
        <div class="seg" id="c-source">${[['acme', t('cert.srcAcme')], ['selfsign', t('cert.srcSelf')], ['external', t('cert.srcExternal')]]
          .map(([k, label]) => `<button data-act="cert.source" data-id="${k}" class="${k === source ? 'active' : ''}">${label}</button>`).join('')}</div>
        <div id="c-form" style="margin-top:.8rem"></div>
      </section>
    </div>
    <div id="c-txt"></div>
    <section class="card">
      <div class="card-head"><h2>${t('cert.store')}</h2><span class="muted small">${t('cert.storeCount', { n: certs().length })}</span></div>
      ${certs().length ? `<div class="table-wrap"><table class="grid">
        <thead><tr><th>${t('cert.subject')}</th><th>${t('cert.issuer')}</th><th>${t('cert.validity')}</th><th>${t('cert.usedBy')}</th><th></th></tr></thead>
        <tbody>${certs().map(storeRow).join('')}</tbody>
      </table></div>` : `<p class="muted">${t('cert.none')}</p>`}
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('cert.log')}</h2>${st.lastError ? badge(t('cert.lastFailed'), 'danger') : ''}</div>
      <pre class="log" id="c-log">${esc((st.log || []).join('\n') || t('common.empty'))}</pre>
      ${st.lastError ? `<p class="hint" style="color:var(--danger)">${esc(trErr(st.lastError))}</p>` : ''}
    </section>`;
  renderForm();
  renderTxt(st.txt);
  if (st.running) startPoll();
}

export function tick() {}

// 「正在使用」的一行:线路默认 / 面板 / 订阅各选各的,点「更换」开抽屉
function useRow(target) {
  const c = certOf(target);
  const path = target === 'panel' ? data.panelCert : target === 'sub' ? data.subCert : data.certPath;
  let cur;
  if (c) cur = `<b>${esc(certLabel(c.name))}</b> ${certBadge(c.info)}<div class="sub-cell">${esc(sourceText(c))}${c.info.exists ? ' · ' + fmtDay(new Date(c.info.notAfter).getTime() / 1000) : ''}</div>`;
  else if (path && (target !== 'lines' || data.info.exists)) cur = `<span class="mono small">${esc(path)}</span><div class="sub-cell">${t('cert.notInStore')}</div>`;
  else cur = `<span class="muted">${t(target === 'lines' ? 'cert.none' : 'cert.noHttps')}</span>`;
  return `<div class="cert-use">
    <span class="cert-use-label">${t(USES[target])}</span>
    <div class="cert-use-cur">${cur}</div>
    <button class="btn sm" data-act="cert.pick" data-id="${target}" ${certs().length ? '' : 'disabled'}>${t('cert.change')}</button>
  </div>`;
}

function storeRow(c) {
  const i = c.info;
  const domains = (i.dnsNames || []).concat(i.ips || []);
  const uses = [];
  if (c.forLines) uses.push(badge(t('cert.useLines'), 'primary'));
  if (c.forPanel) uses.push(badge(t('cert.usePanel'), 'primary'));
  if (c.forSub) uses.push(badge(t('cert.useSub'), 'primary'));
  if ((c.lineNames || []).length) uses.push(`<span class="badge" title="${esc(c.lineNames.join('、'))}">${t('cert.nLines', { n: c.lineNames.length })}</span>`);
  return `<tr>
    <td class="primary-cell">${esc(certLabel(c.name))}${domains.length && !(domains.length === 1 && domains[0] === c.name) ? `<div class="sub-cell">${domains.map(esc).join(', ')}</div>` : ''}</td>
    <td>${sourceBadge(c)}<div class="sub-cell">${esc(i.issuer || '')}</div></td>
    <td>${i.exists ? `${fmtDay(new Date(i.notAfter).getTime() / 1000)} ${certBadge(i)}` : `<span class="danger small">${esc(trErr(i.error || t('cert.none')))}</span>`}</td>
    <td>${uses.join(' ') || '<span class="muted">—</span>'}</td>
    <td class="actions">
      ${c.source === 'acme' ? `<button class="btn sm" data-act="cert.renew" data-id="${esc(c.name)}">${t('cert.renewBtn')}</button>` : ''}
      <button class="btn sm danger" data-act="cert.del" data-id="${esc(c.name)}">${t('common.delete')}</button>
    </td></tr>`;
}

const sourceText = c => ({ acme: "Let's Encrypt", selfsign: t('cert.srcSelf'), external: t('cert.srcExternal') })[c.source] || '';
function sourceBadge(c) {
  if (c.source === 'selfsign' || c.info.selfSigned) return badge(t('cert.srcSelf'), 'warn');
  if (c.source === 'external') return badge(t('cert.srcExternal'), 'primary');
  if (c.source === 'acme') return badge("Let's Encrypt", 'ok');
  return '';
}

// 添加之后顶替哪些用途;没勾的不动
function targetChecks() {
  const hasReal = data.info.exists && !data.info.selfSigned;
  const self = source === 'selfsign';
  return `<h3 class="sub-title">${t('cert.applyTo')}</h3>
    <div class="form-grid">
      ${check('c-lines', t('cert.forLines'), !hasReal, t('cert.forLinesHelp'))}
      ${check('c-panel', t('cert.applyPanel'), !self && !data.panelCert, t('cert.applyPanelHelp'))}
      ${check('c-sub', t('cert.applySub'), !self && !data.subCert, t('cert.applySubHelp'))}
    </div>`;
}

// 三种来源各自的表单;共用下方的"添加后用于"勾选
function renderForm() {
  const box = document.getElementById('c-form');
  if (!box) return;
  const s = data.settings, st = data.status;
  if (source === 'acme') {
    const method = ['http', 'cloudflare', 'manual'].includes(s.acmeMethod) ? s.acmeMethod : 'http';
    box.innerHTML = `
      <div class="form-grid">
        ${field(t('set.webDomain'), `<input id="c-domain" value="${esc(s.acmeDomain || s.webDomain || '')}" placeholder="hk.example.com / *.example.com">`, t('cert.domainHelp'))}
        ${field(t('cert.email'), `<input id="c-email" value="${esc(s.acmeEmail || '')}" placeholder="admin@example.com">`, t('cert.emailHelp'))}
        ${field(t('cert.method'), `<select id="c-method" data-change="cert.method">
          <option value="http" ${method === 'http' ? 'selected' : ''}>${t('cert.methodHttp')}</option>
          <option value="cloudflare" ${method === 'cloudflare' ? 'selected' : ''}>${t('cert.methodCf')}</option>
          <option value="manual" ${method === 'manual' ? 'selected' : ''}>${t('cert.methodManual')}</option></select>`, t('cert.methodHelp'))}
        <div id="c-cf-wrap" ${method !== 'cloudflare' ? 'hidden' : ''}>${field(t('cert.cfToken'), `<input id="c-cftoken" type="password" placeholder="${s.hasCfToken ? t('cert.cfTokenKeep') : ''}">`, t('cert.cfTokenHelp'))}</div>
        <div id="c-renew-wrap" ${method === 'manual' ? 'hidden' : ''}>${check('c-renew', t('cert.autoRenew'), s.acmeAutoRenew, t('cert.autoRenewHelp'))}</div>
        ${check('c-staging', t('cert.staging'), s.acmeStaging, t('cert.stagingHelp'))}
      </div>
      ${targetChecks()}
      <div class="row" style="margin-top:.8rem;flex-wrap:wrap">
        <button class="btn" data-act="cert.precheck">${t('cert.precheck')}</button>
        <button class="btn primary" data-act="cert.issue" ${st.running ? 'disabled' : ''}>${t('cert.issueBtn')}</button>
      </div>
      <div id="c-precheck" style="margin-top:.6rem"></div>`;
    document.getElementById('c-domain').addEventListener('input', syncWildcard);
    syncWildcard();
  } else if (source === 'selfsign') {
    // 无域名场景不该逼用户填东西:留空即用本机探测到的公网 IP,占位里先给他看是哪些
    const auto = [state.settings.publicIp, s.webDomain].filter(Boolean).join(', ');
    box.innerHTML = `
      <p class="hint">${t('cert.selfHelp')}</p>
      <div class="form-grid" style="margin-top:.6rem">
        <div class="full">${field(t('cert.hosts'), `<input id="ss-hosts" value="" placeholder="${esc(auto || '1.2.3.4')}">`, t('cert.hostsHelp'))}</div>
      </div>
      ${targetChecks()}
      <div class="row" style="margin-top:.8rem"><button class="btn primary" data-act="cert.selfsign">${t('cert.selfsign')}</button></div>`;
  } else {
    box.innerHTML = `
      <p class="hint">${t('cert.externalHelp')}</p>
      <div class="form-grid" style="margin-top:.6rem">
        <div class="full">${field(t('cert.certPath'), `<input id="ex-cert" value="" placeholder="/etc/letsencrypt/live/example.com/fullchain.pem">`)}</div>
        <div class="full">${field(t('cert.keyPath'), `<input id="ex-key" value="" placeholder="/etc/letsencrypt/live/example.com/privkey.pem">`, t('cert.pathHelp'))}</div>
      </div>
      ${targetChecks()}
      <div class="row" style="margin-top:.8rem"><button class="btn primary" data-act="cert.external">${t('cert.useExternal')}</button></div>`;
  }
}

// 泛域名只能走 DNS 验证:HTTP 选项灰掉,已选 HTTP 的自动换走,说明换成泛域名那句
function syncWildcard() {
  const dom = document.getElementById('c-domain'), sel = document.getElementById('c-method');
  if (!dom || !sel) return;
  const wild = dom.value.trim().startsWith('*.');
  const http = sel.querySelector('option[value=http]');
  http.disabled = wild;
  if (wild && sel.value === 'http') { sel.value = data.settings.hasCfToken ? 'cloudflare' : 'manual'; syncMethod(); }
  const help = sel.closest('.field').querySelector('.help');
  if (help) help.textContent = wild ? t('cert.wildcardHelp') : t('cert.methodHelp');
}
function syncMethod() {
  const v = fv('c-method');
  document.getElementById('c-cf-wrap').hidden = v !== 'cloudflare';
  document.getElementById('c-renew-wrap').hidden = v === 'manual';
}

// 手动 DNS:要加的记录放在最显眼的地方,等它生效
function renderTxt(recs) {
  const box = document.getElementById('c-txt');
  if (!box) return;
  if (!recs || !recs.length) { box.innerHTML = ''; return; }
  box.innerHTML = `<section class="card cert-txt">
    <div class="card-head"><h2>${t('cert.txtTitle')}</h2><span class="badge primary">${t('cert.txtWaiting')}</span></div>
    <p class="hint">${t('cert.txtHelp')}</p>
    <div class="table-wrap"><table class="grid"><thead><tr><th>${t('cert.txtName')}</th><th>${t('cert.txtValue')}</th></tr></thead><tbody>
      ${recs.map(r => `<tr><td class="mono small">${esc(r.name)} <button class="btn xs ghost" data-act="cert.copy" data-v="${esc(r.name)}">${t('common.copy')}</button></td>
        <td class="mono small">${esc(r.value)} <button class="btn xs ghost" data-act="cert.copy" data-v="${esc(r.value)}">${t('common.copy')}</button></td></tr>`).join('')}
    </tbody></table></div></section>`;
}

function certBadge(i) {
  if (!i.exists) return badge(t('cert.none'), 'warn');
  if (i.daysLeft < 0) return badge(t('cert.expired'), 'danger');
  if (i.daysLeft <= 14) return badge(t('cert.expiring', { n: i.daysLeft }), 'warn');
  return badge(t('cert.daysLeftN', { n: i.daysLeft }), 'ok');
}

// 上一次进度回来了再约下一次:setInterval 不等返回,弱网下请求叠起来,结束时还会弹两次提示、重绘两遍。
// 面板连不上时不能一直显示"申请中":连续失败一分钟就说清楚,签发本身还在后台跑。
let pollGen = 0;
function startPoll() {
  stopPoll();
  const gen = ++pollGen;
  let fails = 0, lastTxt = '';
  const step = async () => {
    const st = await get('cert/status').catch(() => null);
    if (gen !== pollGen) return; // 这期间被停了
    if (st) {
      fails = 0;
      const log = document.getElementById('c-log');
      if (log) { log.textContent = (st.log || []).join('\n'); log.scrollTop = log.scrollHeight; }
      const sig = JSON.stringify(st.txt || []);
      if (sig !== lastTxt) { lastTxt = sig; renderTxt(st.txt); }
      if (!st.running) {
        stopPoll();
        toast(st.lastError ? t('cert.failed') : t('cert.done'), st.lastError ? 'err' : 'ok');
        await reload();
        return;
      }
    } else if (++fails >= 40) {
      stopPoll();
      toast(t('cert.pollLost'), 'err');
      return;
    }
    pollTimer = setTimeout(step, 1500);
  };
  pollTimer = setTimeout(step, 1500);
}
function stopPoll() { pollGen++; if (pollTimer) clearTimeout(pollTimer); pollTimer = null; }

async function reload() {
  await load('settings', 'status');
  const el = document.getElementById('page');
  if (el && location.hash.startsWith('#/cert')) await render(el);
}

const targets = () => ({ forLines: fchk('c-lines'), applyPanel: fchk('c-panel'), applySub: fchk('c-sub') });

registerActions({
  'cert.source': id => {
    source = id;
    document.querySelectorAll('#c-source button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    renderForm();
  },
  'cert.method': () => syncMethod(),
  'cert.copy': async (_, el) => { await copy(el.dataset.v || ''); toast(t('common.copied'), 'ok'); },
  'cert.pick': target => {
    const opts = certs().map(c => ({
      value: c.name, name: certLabel(c.name), badge: certBadge(c.info),
      sub: esc(sourceText(c)) + ((c.info.dnsNames || []).length > 1 ? ' · ' + c.info.dnsNames.map(esc).join(', ') : ''),
      current: certOf(target) === c,
    }));
    if (target !== 'lines') opts.unshift({ value: '', name: t('cert.noHttps'), sub: esc(t('cert.noHttpsHelp')), current: !certOf(target) && !(target === 'panel' ? data.panelCert : data.subCert) });
    openCertDrawer(t('cert.pickFor.' + target), opts, async value => {
      if (target === 'lines' && !await confirm(t('cert.switchLinesConfirm'))) return false;
      try {
        await post('cert/use', { name: value, target }, SLOW);
        if (target === 'panel') panelChanged = true;
        toast(t('cert.useOk'), 'ok');
        await reload();
        return true;
      } catch (e) { toast(e.message, 'err'); return false; }
    });
  },
  'cert.restart': async () => {
    if (!await confirm(t('bk.restartConfirm'), { danger: true })) return;
    await post('backup/restart').catch(() => {});
    toast(t('bk.restarting'), 'ok');
  },
  'cert.renew': async (name, btn) => {
    const c = certs().find(x => x.name === name);
    if (!c) return;
    if (!await confirm(t('cert.issueConfirm', { domain: name }))) return;
    btn.disabled = true;
    // 续期只换文件内容,路径不变,用途都不动
    try {
      await post('cert/issue', { domain: name, email: data.settings.acmeEmail || '', method: c.method || 'http', staging: c.staging, autoRenew: c.autoRenew });
      toast(t('cert.started'), 'ok');
      startPoll();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.del': async name => {
    if (!await confirm(t('common.deleteConfirm', { name: certLabel(name) }), { danger: true, okText: t('common.delete') })) return;
    try { await post('cert/delete', { name }); toast(t('cert.deleted'), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'cert.precheck': async (_, btn) => {
    const box = document.getElementById('c-precheck');
    btn.disabled = true;
    box.innerHTML = `<span class="muted small">${t('cert.checking')}</span>`;
    try {
      const p = await post('cert/precheck', { domain: fv('c-domain') }, SLOW);
      if (p.wildcard) { box.innerHTML = `<p class="hint">${t('cert.wildcardHelp')}</p>`; return; }
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
      staging: fchk('c-staging'), autoRenew: fchk('c-renew'), ...targets(),
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
      await post('cert/selfsign', { hosts, ...targets() });
      toast(t('cert.done'), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'cert.external': async (_, btn) => {
    const body = { certFile: fv('ex-cert').trim(), keyFile: fv('ex-key').trim(), ...targets() };
    if (!body.certFile || !body.keyFile) { toast(t('cert.needPaths'), 'err'); return; }
    btn.disabled = true;
    try {
      await post('cert/external', body);
      toast(t('cert.externalOk'), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
});

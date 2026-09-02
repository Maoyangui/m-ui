// 管理员:登录账号、两步验证(TOTP)、外部 API 开关与令牌。
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, registerActions, field, fv, copy, badge, openModal } from '../ui.js';

export const title = () => t('adm.title');
export const subtitle = () => t('adm.subtitle');

let info = {};

export async function render(el) {
  info = await get('admin/info');
  el.innerHTML = `
    <section class="card">
      <div class="card-head"><h2>${t('adm.account')}</h2><span class="muted small">${t('adm.lastLogin')}: ${esc(info.lastLogins || '—')}</span></div>
      <div class="form-grid">
        ${field(t('set.newUser'), `<input id="pw-user" autocomplete="username" placeholder="${esc(info.username || '')}">`)}
        ${field(t('set.oldPass'), `<input id="pw-old" type="password" autocomplete="current-password">`)}
        ${field(t('set.newPass'), `<input id="pw-new" type="password" autocomplete="new-password">`)}
      </div>
      <div class="row" style="margin-top:.7rem"><button class="btn primary" data-act="adm.password">${t('set.updateAdmin')}</button></div>
    </section>
    <section class="card" id="adm-totp">${totpCard()}</section>
    <section class="card" id="adm-api">${apiCard()}</section>`;
  const sw = document.getElementById('adm-api-on');
  if (sw) sw.addEventListener('change', async e => {
    try { await post('admin/api', { enabled: e.target.checked }); toast(t('adm.apiSaved'), 'ok'); await refresh(); }
    catch (err) { toast(err.message, 'err'); e.target.checked = !e.target.checked; }
  });
  const code = document.getElementById('totp-code');
  if (code) {
    code.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); document.querySelector('[data-act="adm.totpEnable"]').click(); } });
    setTimeout(() => code.focus(), 50);
  }
}

const refresh = () => render(document.getElementById('page'));

function totpCard() {
  const head = b => `<div class="card-head"><h2>${t('adm.totp')}</h2>${b}</div>`;
  if (info.totpEnabled) {
    return `${head(badge(t('adm.totpOn'), 'ok'))}
      <p class="hint">${t('adm.totpOnHelp')}</p>
      <div class="row" style="margin-top:.7rem"><button class="btn danger sm" data-act="adm.totpDisable">${t('adm.totpDisable')}</button></div>`;
  }
  const p = info.totpPending;
  if (p) {
    return `${head(badge(t('adm.totpPending'), 'warn'))}
      <p class="hint">${t('adm.totpScanHelp')}</p>
      <div class="totp-setup">
        <img class="qr" src="./api/admin/totp/qr?_=${Date.now()}" alt="QR">
        <div class="totp-side">
          <div class="label">${t('adm.totpSecret')}</div>
          <div class="sub-box"><code>${esc(p.secret)}</code><button class="btn sm" data-act="adm.copy" data-id="${esc(p.secret)}">${t('common.copy')}</button></div>
          <div class="row" style="align-items:flex-end;gap:.6rem;margin-top:.5rem;flex-wrap:wrap">
            ${field(t('adm.totpCode'), `<input id="totp-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000" style="max-width:9rem;font-family:var(--mono);letter-spacing:.15em">`)}
            <button class="btn primary" data-act="adm.totpEnable">${t('adm.totpVerify')}</button>
            <button class="btn ghost" data-act="adm.totpCancel">${t('common.cancel')}</button>
          </div>
        </div>
      </div>`;
  }
  return `${head(badge(t('adm.totpOff')))}
    <p class="hint">${t('adm.totpHelp')}</p>
    <div class="row" style="margin-top:.7rem"><button class="btn primary sm" data-act="adm.totpGen">${t('adm.totpGen')}</button><button class="btn sm" data-act="adm.totpManual">${t('adm.totpManual')}</button></div>`;
}

const endpoints = () => [
  ['GET', '/ping', t('adm.ep.ping')],
  ['GET', '/plans', t('adm.ep.plans')],
  ['GET', '/users', t('adm.ep.users')],
  ['POST', '/users', t('adm.ep.create')],
  ['GET', '/users/{name}', t('adm.ep.get')],
  ['PATCH', '/users/{name}', t('adm.ep.update')],
  ['DELETE', '/users/{name}', t('adm.ep.delete')],
  ['POST', '/users/{name}/enable | disable', t('adm.ep.enable')],
  ['POST', '/users/{name}/reset', t('adm.ep.reset')],
  ['POST', '/users/{name}/kick', t('adm.ep.kick')],
  ['POST', '/users/{name}/plan', t('adm.ep.plan')],
  ['GET', '/users/{name}/sub', t('adm.ep.sub')],
];

const example = (base, token) => `# 开号并套用套餐
curl -X POST -H "Authorization: Bearer ${token}" -H "Content-Type: application/json" \\
  -d '{"name":"alice","plan":"月付","remark":"订单 #1001"}' ${base}/users

# 续费(再套一次套餐)
curl -X POST -H "Authorization: Bearer ${token}" -H "Content-Type: application/json" \\
  -d '{"plan":"月付","mode":"renew"}' ${base}/users/alice/plan

# 停用 / 启用 / 取订阅地址
curl -X POST -H "Authorization: Bearer ${token}" ${base}/users/alice/disable
curl -X POST -H "Authorization: Bearer ${token}" ${base}/users/alice/enable
curl -H "Authorization: Bearer ${token}" ${base}/users/alice/sub`;

function apiCard() {
  const on = !!info.apiEnabled;
  const base = info.apiBase || '';
  return `<div class="card-head"><h2>${t('adm.api')}</h2><div class="row" style="align-items:center;gap:.5rem">${badge(on ? t('common.enabled') : t('common.disabled'), on ? 'ok' : '')}<label class="switch" title="${esc(t('adm.apiToggle'))}"><input type="checkbox" id="adm-api-on" ${on ? 'checked' : ''}><span></span></label></div></div>
    <p class="hint">${t('adm.apiHelp')}</p>
    ${on ? `
    <dl class="kv" style="margin-top:.8rem">
      <dt>${t('adm.apiBase')}</dt><dd><div class="sub-box"><code>${esc(base)}</code><button class="btn sm" data-act="adm.copy" data-id="${esc(base)}">${t('common.copy')}</button></div></dd>
      <dt>${t('adm.apiToken')}</dt><dd><div class="sub-box"><code>${esc(info.apiToken || '')}</code><button class="btn sm" data-act="adm.copy" data-id="${esc(info.apiToken || '')}">${t('common.copy')}</button><button class="btn sm danger" data-act="adm.apiRotate">${t('adm.apiRotate')}</button></div></dd>
    </dl>
    <h3 class="sub-title">${t('adm.apiEndpoints')}</h3>
    <div class="table-wrap"><table class="grid api-doc"><thead><tr><th>${t('adm.apiMethod')}</th><th>${t('adm.apiPath')}</th><th>${t('adm.apiDesc')}</th></tr></thead><tbody>
      ${endpoints().map(([m, p, d]) => `<tr><td class="mono">${m}</td><td class="mono">${esc(p)}</td><td>${esc(d)}</td></tr>`).join('')}
    </tbody></table></div>
    <h3 class="sub-title">${t('adm.apiExample')}</h3>
    <pre class="log code">${esc(example(base, info.apiToken || ''))}</pre>
    <p class="hint">${t('adm.apiDocs')} <a href="https://github.com/fangjunsheng555/m-ui/blob/main/docs/API.md" target="_blank" rel="noopener">docs/API.md</a></p>` : ''}`;
}

registerActions({
  'adm.password': async () => {
    try {
      await post('password', { username: fv('pw-user'), oldPassword: fv('pw-old'), newPassword: fv('pw-new') });
      toast(t('set.adminUpdated'), 'ok');
      setTimeout(() => post('logout').then(() => location.reload()), 1200);
    } catch (e) { toast(e.message, 'err'); }
  },
  'adm.copy': id => copy(id),
  'adm.totpGen': async () => {
    try { await post('admin/totp/setup', {}); await refresh(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'adm.totpManual': () => {
    openModal(t('adm.totpManualTitle'), field(t('adm.totpSecret'), `<input id="totp-manual" autocomplete="off" spellcheck="false" placeholder="JBSW Y3DP EHPK 3PXP …">`, t('adm.totpManualHelp')), async () => {
      await post('admin/totp/setup', { secret: fv('totp-manual') });
      await refresh();
    });
  },
  'adm.totpEnable': async (_, btn) => {
    btn.disabled = true;
    try { await post('admin/totp/enable', { code: fv('totp-code') }); toast(t('adm.totpEnabled'), 'ok'); await refresh(); }
    catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'adm.totpCancel': async () => { await post('admin/totp/cancel').catch(() => {}); await refresh(); },
  'adm.totpDisable': () => {
    openModal(t('adm.totpDisableTitle'), `
      <p class="hint">${t('adm.totpDisableHelp')}</p>
      <div class="form-grid">
        ${field(t('adm.password'), `<input id="totp-pw" type="password" autocomplete="current-password">`)}
        ${field(t('adm.totpCode'), `<input id="totp-dcode" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000">`)}
      </div>`, async () => {
      await post('admin/totp/disable', { password: fv('totp-pw'), code: fv('totp-dcode') });
      toast(t('adm.totpDisabled'), 'ok');
      await refresh();
    }, { saveText: t('adm.totpDisable'), danger: true });
  },
  'adm.apiRotate': async () => {
    if (!await confirm(t('adm.apiRotateConfirm'), { danger: true })) return;
    try { await post('admin/api/rotate'); toast(t('adm.apiRotated'), 'ok'); await refresh(); }
    catch (e) { toast(e.message, 'err'); }
  },
});

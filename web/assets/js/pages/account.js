// 代理面板:我的账号(改密码 / 两步验证)与订阅页设置。
import { state, load } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, registerActions, badge, field, check, fv, fchk, fmtBytes, progress } from '../ui.js';

export const title = () => t('acct.title');
export const subtitle = () => t('acct.subtitle');

let me = {};
let pendingSecret = '';

export async function render(el) {
  me = await get('self');
  el.innerHTML = `
    <section class="card">
      <div class="card-head"><h2>${t('acct.quota')}</h2><span class="muted small">${t('rs.lastLogin')}: ${esc(me.lastLogins || '—')}</span></div>
      <dl class="kv">
        <dt>${t('rs.used')}</dt><dd>${fmtBytes(me.used)} / ${me.volume ? fmtBytes(me.volume) : t('common.unlimited')}${progress(me.used, me.volume)}</dd>
        <dt>${t('rs.devices')}</dt><dd class="num">${me.devices} / ${me.deviceLimit || '∞'}</dd>
        <dt>${t('rs.online')}</dt><dd class="num">${me.online}</dd>
        <dt>${t('rs.users')}</dt><dd class="num">${me.users}</dd>
      </dl>
    </section>

    <section class="card">
      <div class="card-head"><h2>${t('acct.password')}</h2></div>
      <div class="form-grid">
        ${field(t('set.oldPass'), `<input id="pw-old" type="password" autocomplete="current-password">`)}
        ${field(t('set.newPass'), `<input id="pw-new" type="password" autocomplete="new-password">`)}
      </div>
      <div class="row" style="margin-top:.7rem"><button class="btn primary" data-act="acct.passwd">${t('common.save')}</button></div>
    </section>

    <section class="card" id="acct-totp">${totpCard()}</section>

    <section class="card">
      <div class="card-head"><h2>${t('set.subPage')}</h2><button class="btn primary sm" data-act="acct.savePage">${t('common.save')}</button></div>
      <div class="form-grid">
        ${check('sp-enabled', t('set.subPageEnabled'), me.pageEnabled !== false, t('acct.pageHelp'))}
        ${check('sp-share', t('set.subShare'), me.shareOn !== false, t('acct.shareHelp'))}
        ${field(t('set.subPageTitle'), `<input id="sp-title" value="${esc(me.pageTitle || '')}" placeholder="${esc(t('acct.inherit'))}">`)}
        ${field(t('set.subPageSupport'), `<input id="sp-support" value="${esc(me.pageSupport || '')}" placeholder="${esc(t('acct.inherit'))}">`)}
        <div class="full">${field(t('set.subPageNotice'), `<textarea id="sp-notice" placeholder="${esc(t('acct.inherit'))}">${esc(me.pageNotice || '')}</textarea>`)}</div>
      </div>
    </section>`;
}

function totpCard() {
  const head = b => `<div class="card-head"><h2>${t('adm.totp')}</h2>${b}</div>`;
  if (me.totpEnabled) {
    return `${head(badge(t('adm.totpOn'), 'ok'))}
      <p class="hint">${t('adm.totpOnHelp')}</p>
      <div class="row" style="margin-top:.7rem"><button class="btn danger sm" data-act="acct.totpOff">${t('adm.totpDisable')}</button></div>`;
  }
  if (pendingSecret) {
    return `${head(badge(t('adm.totpPending'), 'warn'))}
      <p class="hint">${t('adm.totpScanHelp')}</p>
      <div class="totp-setup">
        <img class="qr" src="./api/self/totp/qr?_=${Date.now()}" alt="QR">
        <div class="totp-side">
          <div class="label">${t('adm.totpSecret')}</div>
          <div class="sub-box"><code>${esc(pendingSecret)}</code></div>
          ${field(t('adm.totpCode'), `<input id="totp-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000" style="max-width:9rem;font-family:var(--mono);letter-spacing:.15em">`)}
          <div class="row"><button class="btn primary" data-act="acct.totpOn">${t('adm.totpVerify')}</button>
          <button class="btn ghost" data-act="acct.totpCancel">${t('common.cancel')}</button></div>
        </div>
      </div>`;
  }
  return `${head(badge(t('adm.totpOff')))}
    <p class="hint">${t('adm.totpHelp')}</p>
    <div class="row" style="margin-top:.7rem"><button class="btn primary sm" data-act="acct.totpGen">${t('adm.totpGen')}</button></div>`;
}

async function refresh() {
  me = await get('self');
  const el = document.getElementById('acct-totp');
  if (el) el.innerHTML = totpCard();
}

// 首次登录:必须先设密码才能用面板
export function forceSetPassword() {
  const box = document.getElementById('app');
  if (!box) return;
  box.hidden = true;
  const login = document.getElementById('login');
  login.hidden = false;
  login.innerHTML = `<div class="login-card">
    <h1>${t('acct.setPassword')}</h1>
    <p class="hint">${t('acct.setPasswordHelp')}</p>
    ${field(t('set.newPass'), `<input id="first-pw" type="password" autocomplete="new-password">`)}
    <button class="btn primary block" data-act="acct.first">${t('common.save')}</button>
    <p class="err" id="first-err" hidden></p>
  </div>`;
}

registerActions({
  'acct.first': async () => {
    const err = document.getElementById('first-err');
    try {
      await post('self/password', { new: fv('first-pw') });
      location.reload();
    } catch (e) { err.hidden = false; err.textContent = e.message; }
  },
  'acct.passwd': async () => {
    try {
      await post('self/password', { old: fv('pw-old'), new: fv('pw-new') });
      toast(t('set.passChanged'), 'ok');
      document.getElementById('pw-old').value = '';
      document.getElementById('pw-new').value = '';
    } catch (e) { toast(e.message, 'err'); }
  },
  'acct.totpGen': async () => {
    try { const r = await get('self/totp'); pendingSecret = r.secret; document.getElementById('acct-totp').innerHTML = totpCard(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'acct.totpCancel': () => { pendingSecret = ''; document.getElementById('acct-totp').innerHTML = totpCard(); },
  'acct.totpOn': async () => {
    try { await post('self/totp', { code: fv('totp-code') }); pendingSecret = ''; await refresh(); toast(t('adm.totpEnabled'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'acct.totpOff': async () => {
    if (!await confirm(t('adm.totpDisableConfirm'), { danger: true })) return;
    try { await del('self/totp'); await refresh(); toast(t('adm.totpDisabled'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'acct.savePage': async () => {
    try {
      await put('self', {
        pageEnabled: fchk('sp-enabled'), shareOn: fchk('sp-share'),
        pageTitle: fv('sp-title').trim(), pageSupport: fv('sp-support').trim(), pageNotice: fv('sp-notice'),
      });
      await load('status');
      toast(t('set.saved'), 'ok');
    } catch (e) { toast(e.message, 'err'); }
  },
});

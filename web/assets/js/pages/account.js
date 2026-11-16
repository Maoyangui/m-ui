// 代理面板「账号」:我的额度一排概要 → 登录密码 + 两步验证并排 → 外部 API(密钥)→ 我的订阅页。
// 额度是来这一页最常看的,放最上面、做成一格一个数;密码和两步验证平时不动,各一张小卡片,改动在弹框里做。
import { state, load } from '../app.js';
import { get, post, put, del, totpQrUrl } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, openModal, registerActions, field, switchRow, fv, fchk, fmtBytes, fmtDay, progress, charCount, bindCharCounts, PAGE_TEXT_MAX } from '../ui.js';
import { mountApiCard } from '../apikeys.js';
import { mountTotp } from '../totp.js';
import { logoBlock, bindLogo } from '../logo.js';

export const title = () => t('acct.title');
export const subtitle = () => t('acct.subtitle');

let me = {};
let pendingSecret = ''; // 两步验证生成了、还没验证的密钥(代理面板这边只在页面里记着)

// 代理的外部 API 地址就是当前面板地址下的 api/v1
const apiBase = () => location.origin + location.pathname.replace(/[^/]*$/, '') + 'api/v1';

function lastLogin() {
  const s = String(me.lastLogins || '').trim();
  if (!s) return esc(t('adm.neverLogin'));
  const i = s.lastIndexOf(' ');
  return i > 0 ? `${esc(s.slice(0, i))}<span class="sep">·</span>${esc(s.slice(i + 1))}` : esc(s);
}

// 我的额度:流量、用户数、设备、限速、到期各一格;快到头的那一格变色
function quotaTiles() {
  const tile = (lb, v, sub = '', cls = '') => `<div class="ld-tile${cls ? ' ' + cls : ''}"><span class="lb">${esc(lb)}</span><b>${v}</b>${sub ? `<span class="sub">${sub}</span>` : ''}</div>`;
  const left = me.expiry ? Math.ceil((me.expiry * 1000 - Date.now()) / 86400000) : null;
  const pct = me.volume ? me.used / me.volume : 0;
  const speed = (me.speedUp || me.speedDown) ? `${me.speedUp || '∞'}<span class="of"> / ${me.speedDown || '∞'} Mbps</span>` : esc(t('common.unlimited'));
  return `<div class="ld-sum acct-sum">
    ${tile(t('rs.used'), fmtBytes(me.used || 0, 1), `/ ${me.volume ? fmtBytes(me.volume, 0) : esc(t('common.unlimited'))}${progress(me.used, me.volume)}`, pct >= 1 ? 'bad' : pct >= 0.9 ? 'warn' : '')}
    ${tile(t('rs.users'), `${me.users || 0}<span class="of"> / ${me.userLimit || '∞'}</span>`, '', me.userLimit && me.users >= me.userLimit ? 'warn' : '')}
    ${tile(t('rs.pool'), `${me.online || 0}<span class="of"> / ${me.deviceLimit || '∞'}</span>`, esc(me.poolRejects ? t('rs.poolFull', { n: me.poolRejects }) : `${t('rs.allocated')} ${me.devices || 0}`), me.poolRejects ? 'warn' : '')}
    ${tile(t('rs.speed'), speed, (me.speedUp || me.speedDown) ? esc(t('acct.speedSub')) : '')}
    ${tile(t('user.expiry'), me.expiry ? fmtDay(me.expiry) : esc(t('user.never')),
    left == null ? '' : left <= 0 ? esc(t('user.expired')) : esc(t('user.daysLeft', { n: left })), left != null && left <= 3 ? 'bad' : left != null && left <= 7 ? 'warn' : '')}
  </div>`;
}

function passCard() {
  return `<div class="card-head"><h2>${t('acct.password')}</h2></div>
    <div class="adm-who"><span class="adm-ava"><i class="i i-user"></i></span>
      <div><b>${esc(me.name || '')}</b><span class="muted small">${t('rs.lastLogin')} ${lastLogin()}</span></div></div>
    <div class="card-acts"><button class="btn sm" data-act="acct.editPass">${t('acct.passEdit')}</button></div>`;
}

export async function render(el) {
  me = await get('self');
  const api = await get('self/api').catch(() => ({ enabled: false }));
  el.innerHTML = `
    <section class="card">
      <div class="card-head"><h2>${t('acct.quota')}</h2></div>
      ${quotaTiles()}
    </section>

    <div class="adm-top">
      <section class="card" id="acct-pass">${passCard()}</section>
      <section class="card" id="acct-totp"></section>
    </div>

    <section class="card" id="acct-api"></section>

    <section class="card acct-page">
      <div class="card-head"><h2>${t('set.subPage')}</h2></div>
      <p class="hint">${esc(t('acct.pageLead'))}</p>
      <div class="acct-sw">
        ${switchRow('sp-enabled', t('set.subPageEnabled'), me.pageEnabled !== false, t('acct.pageHelp'))}
        ${switchRow('sp-share', t('set.subShare'), me.shareOn !== false, t('acct.shareHelp'))}
      </div>
      <div class="form-grid cols-2">
        ${field(t('set.subTitle'), `<input id="sp-profile" value="${esc(me.profileTitle || '')}" placeholder="${esc(t('acct.inherit'))}">`, t('acct.profileHelp'))}
        ${field(t('set.subPageTitle'), `<input id="sp-title" value="${esc(me.pageTitle || '')}" placeholder="${esc(t('acct.inherit'))}">`)}
        ${field(t('set.subPageSupport'), `<input id="sp-support" value="${esc(me.pageSupport || '')}" placeholder="${esc(t('acct.inherit'))}">${charCount('sp-support', PAGE_TEXT_MAX.support)}`, t('set.subPageSupportHelp'))}
        ${field(t('set.subPageBuyURL'), `<input id="sp-buy" type="url" value="${esc(me.pageBuyURL || '')}" placeholder="${esc(t('acct.inherit'))}">`, t('set.subPageBuyURLHelp'))}
        <div class="full">${field(t('set.subPageNotice'), `<textarea id="sp-notice" placeholder="${esc(t('acct.inherit'))}">${esc(me.pageNotice || '')}</textarea>${charCount('sp-notice', PAGE_TEXT_MAX.notice)}`, t('set.subPageNoticeHelp'))}</div>
      </div>
      ${logoBlock('sp-logo')}
      <div class="card-acts end"><button class="btn primary" data-act="acct.savePage">${t('acct.pageSave')}</button></div>
    </section>`;
  bindLogo('sp-logo', 'self/logo', { inherit: true }); // 没设时名下用户看到的是主面板的
  bindCharCounts(el);
  await Promise.all([
    mountTotp({
      el: 'acct-totp',
      manual: false,
      initial: { enabled: !!me.totpEnabled, pending: pendingSecret },
      state: async () => { me = await get('self'); return { enabled: !!me.totpEnabled, pending: pendingSecret }; },
      qr: () => totpQrUrl('self'),
      begin: async () => { pendingSecret = (await get('self/totp')).secret || ''; },
      enable: async code => { await post('self/totp', { code }); pendingSecret = ''; },
      cancel: async () => { pendingSecret = ''; },
      disable: (password, code) => del('self/totp', { body: JSON.stringify({ password, code }) }),
    }),
    mountApiCard({
      el: 'acct-api',
      prefix: 'self/api',
      url: apiBase(),
      enabled: !!api.enabled,
      reseller: true,
      repo: (state.status || {}).repo || '',
      toggle: on => put('self/api', { enabled: on }),
    }),
  ]);
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
  'acct.editPass': () => {
    openModal(t('acct.passEdit'), `<div class="form-grid">
        ${field(t('set.oldPass'), `<input id="pw-old" type="password" autocomplete="current-password">`)}
        ${field(t('set.newPass'), `<input id="pw-new" type="password" autocomplete="new-password">`)}
      </div>`, async () => {
      await post('self/password', { old: fv('pw-old'), new: fv('pw-new') });
      toast(t('set.passChanged'), 'ok');
    });
  },
  'acct.savePage': async (_, btn) => {
    if (btn) btn.disabled = true;
    try {
      await put('self', {
        pageEnabled: fchk('sp-enabled'), shareOn: fchk('sp-share'),
        profileTitle: fv('sp-profile').trim(),
        pageTitle: fv('sp-title').trim(), pageSupport: fv('sp-support').trim(), pageNotice: fv('sp-notice'), pageBuyURL: fv('sp-buy').trim(),
      });
      await load('status');
      toast(t('set.saved'), 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { if (btn) btn.disabled = false; }
  },
});

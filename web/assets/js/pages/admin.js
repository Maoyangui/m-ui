// 管理员:谁能进这个面板、怎么进 —— 登录账号、两步验证,以及给外部程序用的 API 密钥。
// 账号和两步验证各一张小卡片并排(看一眼就知道现在是谁、上次从哪登录、两步验证开没开),改动都在弹框里做;
// 外部 API 占一整行:总开关、接口地址、密钥列表,接口文档在右侧抽屉里。
import { state } from '../app.js';
import { get, post, totpQrUrl } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, registerActions, field, fv, openModal, badge } from '../ui.js';
import { mountApiCard } from '../apikeys.js';
import { mountTotp } from '../totp.js';

export const title = () => t('adm.title');
export const subtitle = () => t('adm.subtitle');

let info = {};

// 上次登录记的是「时间 IP」一串:拆开分别写
function lastLogin() {
  const s = String(info.lastLogins || '').trim();
  if (!s) return esc(t('adm.neverLogin'));
  const i = s.lastIndexOf(' ');
  return i > 0 ? `${esc(s.slice(0, i))}<span class="sep">·</span>${esc(s.slice(i + 1))}` : esc(s);
}

// 还在用安装时的默认密码:卡片上标出来,按钮变成醒目的「立即修改」
function accountCard() {
  const weak = !!(state.status || {}).defaultPassword;
  return `<div class="card-head"><h2>${t('adm.account')}</h2>${weak ? badge(t('adm.defaultPw'), 'warn') : ''}</div>
    <div class="adm-who"><span class="adm-ava"><i class="i i-user"></i></span>
      <div><b>${esc(info.username || '')}</b><span class="muted small">${t('adm.lastLogin')} ${lastLogin()}</span></div></div>
    ${weak ? `<p class="hint warn">${esc(t('alert.defaultPw'))}</p>` : ''}
    <div class="card-acts"><button class="btn sm${weak ? ' primary' : ''}" data-act="adm.editAccount">${t(weak ? 'alert.changeNow' : 'adm.accountEdit')}</button></div>`;
}

function editAccount() {
  openModal(t('adm.accountEdit'), `<div class="form-grid">
      <div class="full">${field(t('set.newUser'), `<input id="pw-user" autocomplete="username" placeholder="${esc(info.username || '')}">`)}</div>
      ${field(t('set.oldPass'), `<input id="pw-old" type="password" autocomplete="current-password">`)}
      ${field(t('set.newPass'), `<input id="pw-new" type="password" autocomplete="new-password">`)}
    </div>
    <p class="hint">${esc(t('adm.accountEditHelp'))}</p>`, async () => {
    await post('password', { username: fv('pw-user'), oldPassword: fv('pw-old'), newPassword: fv('pw-new') });
    toast(t('set.adminUpdated'), 'ok');
    setTimeout(() => post('logout').then(() => location.reload()), 1200);
  }, { saveText: t('set.updateAdmin') });
  const old = document.getElementById('pw-old');
  if (old) setTimeout(() => old.focus(), 60); // 多数人只改密码:焦点放在原密码上,用户名那格留空就是不改
}

export async function render(el) {
  info = await get('admin/info');
  el.innerHTML = `
    <div class="adm-top">
      <section class="card" id="adm-acct">${accountCard()}</section>
      <section class="card" id="adm-totp"></section>
    </div>
    <section class="card" id="adm-api"></section>`;
  await Promise.all([
    mountTotp({
      el: 'adm-totp',
      manual: true,
      initial: { enabled: !!info.totpEnabled, pending: info.totpPending ? info.totpPending.secret : '' },
      state: async () => { info = await get('admin/info'); return { enabled: !!info.totpEnabled, pending: info.totpPending ? info.totpPending.secret : '' }; },
      qr: () => totpQrUrl('admin'),
      begin: secret => post('admin/totp/setup', secret ? { secret } : {}),
      enable: code => post('admin/totp/enable', { code }),
      cancel: () => post('admin/totp/cancel'),
      disable: (password, code) => post('admin/totp/disable', { password, code }),
    }),
    mountApiCard({
      el: 'adm-api',
      prefix: 'admin/api',
      url: info.apiBase || '',
      enabled: !!info.apiEnabled,
      repo: (state.status || {}).repo || '',
      toggle: on => post('admin/api', { enabled: on }),
    }),
  ]);
  // 从顶部「还是默认密码」那条提示点过来的(#/admin?pw):直接弹出修改框,地址里的 ?pw 去掉(刷新不再弹)
  if (/[?&]pw\b/.test(location.hash)) {
    history.replaceState(null, '', location.pathname + location.search + '#/admin');
    editAccount();
  }
}

registerActions({
  'adm.editAccount': () => editAccount(),
});
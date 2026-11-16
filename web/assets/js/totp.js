// 两步验证卡片:管理员页与代理面板「我的账号」共用。页面给一组接口(两个面板的地址不一样),这里管三种状态和设置流程:
//   没开   → 「开启两步验证」:生成密钥 → 弹框里扫码、填验证码 → 开启
//   待验证 → 密钥已生成、还没验证(弹框中途关掉了):「继续」接着扫码,「放弃」丢掉这把密钥
//   已开   → 「关闭两步验证」:要当前密码 + 一次验证码
// 用法:mountTotp({ el, state, qr, begin, enable, cancel, disable, manual })
//   state    async () => ({ enabled, pending })   pending 是待验证的密钥,没有为 ''
//   qr       () => 二维码图片地址
//   begin    async secret => {}   生成密钥;secret 非空 = 用自己已有的那把(manual 为真的面板才有这个入口)
//   enable   async code => {}     cancel async () => {}     disable async (password, code) => {}
import { t } from './i18n.js';
import { esc, toast, openModal, registerActions, badge, field, fv, copy, setHTML } from './ui.js';

let o = null;
let st = { enabled: false, pending: '' };

function cardHTML() {
  const head = (text, kind) => `<div class="card-head"><h2>${t('adm.totp')}</h2>${badge(text, kind)}</div>`;
  if (st.enabled) {
    return `${head(t('adm.totpOn'), 'ok')}<p class="hint">${esc(t('adm.totpOnHelp'))}</p>
      <div class="card-acts"><button class="btn sm danger" data-act="totp.disable">${t('adm.totpDisable')}</button></div>`;
  }
  if (st.pending) {
    return `${head(t('adm.totpPending'), 'warn')}<p class="hint">${esc(t('adm.totpPendingHelp'))}</p>
      <div class="card-acts"><button class="btn primary sm" data-act="totp.resume">${t('adm.totpResume')}</button><button class="btn sm ghost" data-act="totp.cancel">${t('adm.totpDrop')}</button></div>`;
  }
  return `${head(t('adm.totpOff'), '')}<p class="hint">${esc(t('adm.totpHelp'))}</p>
    <div class="card-acts"><button class="btn primary sm" data-act="totp.begin">${t('adm.totpStart')}</button>${o.manual ? `<button class="btn sm ghost" data-act="totp.manual">${t('adm.totpManual')}</button>` : ''}</div>`;
}

async function sync() {
  st = await o.state();
  setHTML(o.el, cardHTML());
}

// initial:页面刚读过状态的话直接给,省一次请求
export async function mountTotp(opts) {
  o = opts;
  if (opts.initial) { st = opts.initial; setHTML(o.el, cardHTML()); } else await sync();
}

// 扫码 + 填验证码。中途关掉不丢密钥:卡片上留着「继续」,回来接着扫同一个码
function setupDialog() {
  if (!st.pending) return;
  openModal(t('adm.totpStart'), `<div class="totp-setup">
      <img class="qr" src="${esc(o.qr())}" alt="QR">
      <ol class="totp-steps">
        <li><b>${esc(t('adm.totpStep1'))}</b><span class="muted small">${esc(t('adm.totpStep1Help'))}</span>
          <div class="sub-box"><code>${esc(st.pending)}</code><button type="button" class="btn sm" data-act="totp.copy">${t('common.copy')}</button></div></li>
        <li><b>${esc(t('adm.totpStep2'))}</b>
          <input id="totp-code" class="totp-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000"></li>
      </ol>
    </div>`, async () => {
    await o.enable(fv('totp-code').trim());
    toast(t('adm.totpEnabled'), 'ok');
    await sync();
  }, { saveText: t('adm.totpVerify') });
  const code = document.getElementById('totp-code');
  if (code) code.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); document.getElementById('modal-save').click(); } });
}

registerActions({
  'totp.copy': () => copy(st.pending),
  'totp.begin': async (_, btn) => {
    if (btn) btn.disabled = true;
    try { await o.begin(''); await sync(); setupDialog(); }
    catch (e) { toast(e.message, 'err'); if (btn) btn.disabled = false; }
  },
  'totp.resume': () => setupDialog(),
  'totp.cancel': async () => { try { await o.cancel(); } catch { /* 丢不掉也当作丢了:下一次生成会换掉它 */ } await sync(); },
  'totp.manual': () => {
    openModal(t('adm.totpManualTitle'), field(t('adm.totpSecret'), `<input id="totp-manual" autocomplete="off" spellcheck="false" placeholder="JBSW Y3DP EHPK 3PXP …">`, t('adm.totpManualHelp')), async () => {
      await o.begin(fv('totp-manual'));
      await sync();
      setupDialog(); // 保存回调里开下一个弹框:openModal 认得出窗口已经换了,不会把新的关掉
    }, { saveText: t('adm.totpNext') });
  },
  'totp.disable': () => {
    openModal(t('adm.totpDisableTitle'), `<p class="hint">${esc(t('adm.totpDisableHelp'))}</p>
      <div class="form-grid">
        ${field(t('adm.password'), `<input id="totp-pw" type="password" autocomplete="current-password">`)}
        ${field(t('adm.totpCode'), `<input id="totp-dcode" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000">`)}
      </div>`, async () => {
      await o.disable(fv('totp-pw'), fv('totp-dcode').trim());
      toast(t('adm.totpDisabled'), 'ok');
      await sync();
    }, { saveText: t('adm.totpDisable'), danger: true });
  },
});

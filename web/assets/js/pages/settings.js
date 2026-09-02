import { state, load } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, registerActions, field, check, badge, fv, fchk, copy } from '../ui.js';

export const title = () => t('set.title');

// 留空时后端采用的默认值,作为输入框占位提示
const defaults = {
  webListen: '0.0.0.0', webPort: 2053, webPath: '/app/', subListen: '0.0.0.0', subPort: 2056, subPath: '/sub/', subUpdates: 12,
  tgExpiringDays: 3, tgQuotaPercent: 80, tgDailyHour: 9, upstreamCheckMinutes: 10, upstreamCheckFailThreshold: 2, extRefreshMinutes: 30,
  upstreamTestUrl: 'http://www.gstatic.com/generate_204', statsBucketSeconds: 10, trafficAge: 30,
};

const groups = () => [
  { id: 'panel', title: t('set.panel'), restart: true, fields: [
    ['webDomain', t('set.webDomain'), 'text', t('set.webDomainHelp')],
    ['webListen', t('set.listen'), 'text'], ['webPort', t('set.port'), 'number'], ['webPath', t('set.path'), 'text'],
    ['webCertFile', t('set.cert'), 'text'], ['webKeyFile', t('set.key'), 'text'],
  ]},
  { id: 'sub', title: t('set.sub'), restart: true, fields: [
    ['subListen', t('set.listen'), 'text'], ['subPort', t('set.port'), 'number'], ['subPath', t('set.path'), 'text'],
    ['subCertFile', t('set.cert'), 'text'], ['subKeyFile', t('set.key'), 'text'],
    ['subProfileTitle', t('set.subTitle'), 'text'], ['subUpdates', t('set.subUpdates'), 'number'],
    ['subEncode', t('set.subEncode'), 'bool'], ['subShowNotice', t('set.subNotice'), 'bool'],
    ['subInsecure', t('set.subInsecure'), 'select', t('set.subInsecureHelp'), [['', t('set.subInsecureAuto')], ['true', t('common.yes')], ['false', t('common.no')]]],
    ['subServerAddr', t('set.subServerAddr'), 'select', t('set.subServerAddrHelp'), [['', t('set.addrIp')], ['domain', t('set.addrDomain')]]],
    ['extRefreshMinutes', t('set.extRefresh'), 'number'],
  ]},
  { id: 'subpage', title: t('set.subPage'), fields: [
    ['subPageEnabled', t('set.subPageEnabled'), 'bool', t('set.subPageEnabledHelp')],
    ['subPageTitle', t('set.subPageTitle'), 'text', t('set.subPageTitleHelp')],
    ['subPageSupport', t('set.subPageSupport'), 'text'],
    ['subPageNotice', t('set.subPageNotice'), 'textarea'],
  ]},
  { id: 'notify', title: t('set.notify'), test: true, fields: [
    ['tgEnabled', t('set.tgEnabled'), 'bool', t('set.tgEnabledHelp')],
    ['tgToken', t('set.tgToken'), 'text', t('set.tgTokenHelp')], ['tgChatId', t('set.tgChatId'), 'text', t('set.tgChatIdHelp')],
    ['tgProxy', t('set.tgProxy'), 'text', t('set.tgProxyHelp')],
    ['tgOnLogin', t('set.tgOnLogin'), 'boolOn'], ['tgOnUserDisabled', t('set.tgOnUserDisabled'), 'boolOn'],
    ['tgOnUserExpiring', t('set.tgOnUserExpiring'), 'boolOn'], ['tgExpiringDays', t('set.tgExpiringDays'), 'number'],
    ['tgOnQuota', t('set.tgOnQuota'), 'boolOn'], ['tgQuotaPercent', t('set.tgQuotaPercent'), 'number'],
    ['tgOnUpstream', t('set.tgOnUpstream'), 'boolOn'], ['tgOnCore', t('set.tgOnCore'), 'boolOn'],
    ['tgDaily', t('set.tgDaily'), 'boolOn'], ['tgDailyHour', t('set.tgDailyHour'), 'number'],
  ]},
  { id: 'monitor', title: t('set.monitor'), fields: [
    ['upstreamCheckMinutes', t('set.checkMinutes'), 'number', t('set.checkMinutesHelp')],
    ['upstreamCheckFailThreshold', t('set.checkThreshold'), 'number', t('set.checkThresholdHelp')],
  ]},
  { id: 'core', title: t('set.core'), fields: [
    ['certFile', t('set.certFile'), 'text', t('set.certHelp')], ['keyFile', t('set.key'), 'text'],
    ['upstreamTestUrl', t('set.testUrl'), 'text', t('set.testUrlHelp')],
    ['statsBucketSeconds', t('set.bucket'), 'number'], ['trafficAge', t('set.trafficAge'), 'number', t('set.trafficAgeHelp')],
  ]},
];

export async function render(el) {
  const s = state.settings;
  const isNode = String(s.nodeMode).toLowerCase() === 'true';
  el.innerHTML = `
    <section class="card">
      <div class="card-head"><h2>${t('set.role')}</h2>${badge(isNode ? t('role.node') : t('role.master'), isNode ? 'node' : 'primary')}</div>
      <label class="field check"><input type="checkbox" id="set-nodeMode" ${isNode ? 'checked' : ''}><span>${t('set.roleSwitch')}</span></label>
      <p class="hint" id="role-hint">${isNode ? t('set.roleNode') : t('set.roleMaster')}</p>
      <div class="row" style="margin-top:.7rem"><button class="btn primary" data-act="set.saveRole">${t('common.save')}</button></div>
    </section>
    ${groups().map(g => `
      <section class="card">
        <div class="card-head"><h2>${esc(g.title)}${g.restart ? ` <span class="badge warn" title="${t('set.restartHint')}">${t('set.needRestart')}</span>` : ''}</h2><div class="row">${g.test ? `<button class="btn sm" data-act="set.notifyTest">${t('set.tgTest')}</button>` : ''}<button class="btn primary sm" data-act="set.save" data-id="${g.id}">${t('common.save')}</button></div></div>
        <div class="form-grid">${g.fields.map(([k, label, type, help, options]) =>
          type === 'bool' || type === 'boolOn'
            ? check('set-' + k, label, s[k] === undefined || s[k] === '' ? (k === 'subPageEnabled' || type === 'boolOn') : String(s[k]).toLowerCase() === 'true', help)
            : type === 'textarea'
              ? `<div class="full">${field(label, `<textarea id="set-${k}">${esc(s[k] ?? '')}</textarea>`, help)}</div>`
              : type === 'select'
                ? field(label, `<select id="set-${k}">${options.map(([v, l]) => `<option value="${esc(v)}" ${String(s[k] ?? '') === v ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select>`, help)
                : field(label, `<input id="set-${k}" type="${type}" value="${esc(s[k] ?? '')}" placeholder="${esc(defaults[k] ?? '')}">`, help)).join('')}</div>
      </section>`).join('')}
    <section class="card">
      <div class="card-head"><h2>${t('set.admin')}</h2></div>
      <div class="form-grid">
        ${field(t('set.newUser'), `<input id="pw-user" autocomplete="username">`)}
        ${field(t('set.oldPass'), `<input id="pw-old" type="password" autocomplete="current-password">`)}
        ${field(t('set.newPass'), `<input id="pw-new" type="password" autocomplete="new-password">`)}
      </div>
      <div class="row" style="margin-top:.7rem"><button class="btn" data-act="set.password">${t('set.updateAdmin')}</button></div>
    </section>
    <section class="card">
      <div class="card-head"><h2>${t('set.about')}</h2><button class="btn sm danger" data-act="set.restart">${t('set.restart')}</button></div>
      <dl class="kv">
        <dt>${t('set.version')}</dt><dd class="mono">${esc(state.status.version || '')}</dd>
        <dt>sing-box</dt><dd class="mono">1.14</dd>
        <dt>${t('set.panelUrl')}</dt><dd class="mono">${esc((state.status.panelTLS ? 'https' : 'http') + '://' + (s.webDomain || '<IP>') + ':' + (state.status.webPort || '') + (state.status.webPath || '/'))}</dd>
        <dt>${t('set.subUrl')}</dt><dd class="mono">${esc((state.status.subTLS ? 'https' : 'http') + '://' + (s.webDomain || '<IP>') + ':' + (state.status.subPort || '') + (state.status.subPath || '/sub/') + '<' + t('set.userPh') + '>?format=clash')}</dd>
        <dt>License</dt><dd>GPL-3.0 · <a href="https://github.com/fangjunsheng555/m-ui" target="_blank" rel="noopener">GitHub</a></dd>
      </dl>
      <p class="hint">${t('set.restartNote')}</p>
    </section>`;
  document.getElementById('set-nodeMode').addEventListener('change', e => {
    document.getElementById('role-hint').textContent = e.target.checked ? t('set.roleNode') : t('set.roleMaster');
  });
  if (isNode) renderPairing();
}

// 副机:展示配对信息(API 地址 + 令牌),复制到主机的"服务器"页即可接入
async function renderPairing() {
  const info = await get('agent/info').catch(() => null);
  if (!info) return;
  const role = document.querySelector('section.card');
  const card = document.createElement('section');
  card.className = 'card';
  card.innerHTML = `
    <div class="card-head"><h2>${t('set.pairing')}</h2><button class="btn sm" data-act="set.rotateToken">${t('set.rotateToken')}</button></div>
    <p class="hint">${t('set.pairingHelp')}</p>
    <dl class="kv" style="margin-top:.5rem">
      <dt>${t('node.apiUrl')}</dt><dd class="mono">${esc(info.apiUrl)}</dd>
      <dt>${t('node.token')}</dt><dd><div class="sub-box"><code id="pair-token">${esc(info.token)}</code><button class="btn sm" data-act="set.copyToken">${t('common.copy')}</button></div></dd>
      <dt>${t('node.revision')}</dt><dd class="mono">${esc(info.revision || '—')}${info.appliedAt ? ` <span class="muted small">${new Date(Number(info.appliedAt) * 1000).toLocaleString()}</span>` : ''}</dd>
    </dl>`;
  role.after(card);
}

registerActions({
  'set.save': async id => {
    const g = groups().find(x => x.id === id);
    const body = {};
    g.fields.forEach(([k, , type]) => { body[k] = (type === 'bool' || type === 'boolOn') ? String(fchk('set-' + k)) : fv('set-' + k).trim(); });
    try { const r = await post('settings', body); await load('settings', 'status'); toast(t('set.saved') + (r.note ? ' · ' + r.note : ''), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'set.saveRole': async () => {
    const toNode = fchk('set-nodeMode');
    const cur = String(state.settings.nodeMode).toLowerCase() === 'true';
    if (toNode !== cur && !await confirm(toNode ? t('set.roleToNode') : t('set.roleToMaster'), { danger: true })) return;
    try { await post('settings', { nodeMode: String(toNode) }); await load('settings', 'status'); toast(t('set.saved'), 'ok'); location.reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'set.restart': async () => {
    if (!await confirm(t('bk.restartConfirm'), { danger: true })) return;
    await post('backup/restart').catch(() => {});
    toast(t('bk.restarting'), 'ok');
    const start = Date.now();
    const poll = async () => {
      if (Date.now() - start > 60000) { toast(t('bk.restartTimeout'), 'err'); return; }
      try { await fetch('./api/status', { cache: 'no-store' }); location.reload(); } catch { setTimeout(poll, 2000); }
    };
    setTimeout(poll, 3000);
  },
  'set.copyToken': () => copy(document.getElementById('pair-token').textContent),
  'set.rotateToken': async () => {
    if (!await confirm(t('set.rotateConfirm'), { danger: true })) return;
    try { const r = await post('agent/rotate'); document.getElementById('pair-token').textContent = r.token; toast(t('set.rotated'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'set.notifyTest': async (_, btn) => {
    btn.disabled = true;
    try { await post('notify/test'); toast(t('set.tgTestOk'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'set.password': async () => {
    try {
      await post('password', { username: fv('pw-user'), oldPassword: fv('pw-old'), newPassword: fv('pw-new') });
      toast(t('set.adminUpdated'), 'ok');
      setTimeout(() => post('logout').then(() => location.reload()), 1200);
    } catch (e) { toast(e.message, 'err'); }
  },
});

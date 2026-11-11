import { state, load } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, registerActions, field, check, badge, fv, fchk, copy, urlHost, charCount, bindCharCounts, PAGE_TEXT_MAX } from '../ui.js';
import { logoBlock, bindLogo } from '../logo.js';
import { subSettingsChange } from '../subtls.js';

// boolSetting 开关类设置的显示:和后端同一个判法(model.NormBoolSetting),空值用默认。
function boolSetting(v, def) {
  const x = String(v ?? '').trim().toLowerCase();
  if (x === '') return def;
  if (['true', '1', 'on', 'yes'].includes(x)) return true;
  if (['false', '0', 'off', 'no'].includes(x)) return false;
  return def;
}

export const title = () => t('set.title');

// 留空时后端采用的默认值,作为输入框占位提示
const defaults = {
  timezone: 'Asia/Shanghai', webListen: '0.0.0.0', webPort: 2053, webPath: '/app/', subListen: '0.0.0.0', subPort: 2056, subPath: '/sub/', subUpdates: 12,
  resellerListen: '0.0.0.0', resellerPort: 2054, resellerPath: '/dl/',
  tgExpiringDays: 3, tgQuotaPercent: 80, tgDailyHour: 9, upstreamCheckMinutes: 10, upstreamCheckFailThreshold: 2, extRefreshMinutes: 30,
  upstreamTestUrl: 'http://www.gstatic.com/generate_204', statsBucketSeconds: 60, trafficAge: 30, reachMinutes: 90,
};

// 有字数上限的文案:框下面显示当前字数与上限
const textMax = { subPageNotice: PAGE_TEXT_MAX.notice, subPageSupport: PAGE_TEXT_MAX.support };

// 面板时间显示用的时区(默认 Asia/Shanghai);列表覆盖常见地区,够用且不会写错名字
const TIMEZONES = [
  'Asia/Shanghai', 'Asia/Hong_Kong', 'Asia/Taipei', 'Asia/Tokyo', 'Asia/Seoul', 'Asia/Singapore',
  'Asia/Bangkok', 'Asia/Kolkata', 'Asia/Dubai', 'Europe/Moscow', 'Europe/London', 'Europe/Berlin',
  'Europe/Paris', 'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles',
  'America/Sao_Paulo', 'Australia/Sydney', 'UTC',
];

const isNodeRole = () => String(state.settings.nodeMode).toLowerCase() === 'true';

const groups = () => [
  { id: 'panel', title: t('set.panel'), restart: true, fields: [
    ['webDomain', t('set.webDomain'), 'text', t('set.webDomainHelp')],
    ['timezone', t('set.timezone'), 'select', t('set.timezoneHelp'), TIMEZONES.map(z => [z, z])],
    ['webListen', t('set.listen'), 'text'], ['webPort', t('set.port'), 'number'], ['webPath', t('set.path'), 'text', t('set.pathHelp')],
    ['webCertFile', t('set.cert'), 'text', t('set.certManaged')], ['webKeyFile', t('set.key'), 'text'],
    ['supportOnline', t('set.supportOnline'), 'boolOn', t('set.supportOnlineHelp')],
  ]},
  { id: 'sub', title: t('set.sub'), restart: true, fields: [
    ['subListen', t('set.listen'), 'text'], ['subPort', t('set.port'), 'number'], ['subPath', t('set.path'), 'text', t('set.subPathHelp')],
    ['subCertFile', t('set.cert'), 'text', t('set.subCertHelp')], ['subKeyFile', t('set.key'), 'text'],
    ['subProfileTitle', t('set.subTitle'), 'text'], ['subUpdates', t('set.subUpdates'), 'number'],
    ['subUseUserName', t('set.subUseUserName'), 'boolOn', t('set.subUseUserNameHelp')],
    ['subEncode', t('set.subEncode'), 'bool'], ['subShowNotice', t('set.subNotice'), 'bool'],
    ['subInsecure', t('set.subInsecure'), 'select', t('set.subInsecureHelp'), [['', t('set.subInsecureAuto')], ['true', t('common.yes')], ['false', t('common.no')]]],
    ['subServerAddr', t('set.subServerAddr'), 'select', t('set.subServerAddrHelp'), [['', t('set.addrIp')], ['domain', t('set.addrDomain')]]],
    ['extRefreshMinutes', t('set.extRefresh'), 'number'],
  ]},
  { id: 'subtmpl', title: t('set.subTmpl'), masterOnly: true, extra: `<div class="row" style="margin-top:.6rem;flex-wrap:wrap"><button class="btn sm" data-act="set.tmplDefault" data-id="clash">${t('set.tmplFillClash')}</button><button class="btn sm" data-act="set.tmplDefault" data-id="singbox">${t('set.tmplFillSingbox')}</button></div>`, fields: [
    ['subAutoFormat', t('set.subAutoFormat'), 'boolOn', t('set.subAutoFormatHelp')],
    ['subClashExt', t('set.subClashExt'), 'code', t('set.subClashExtHelp')],
    ['subSingboxExt', t('set.subSingboxExt'), 'code', t('set.subSingboxExtHelp')],
  ]},
  { id: 'reseller', title: t('set.reseller'), restart: true, fields: [
    ['resellerEnabled', t('set.resellerEnabled'), 'boolOn', t('set.resellerEnabledHelp')],
    ['resellerListen', t('set.listen'), 'text'], ['resellerPort', t('set.port'), 'number'],
    ['resellerPath', t('set.path'), 'text', t('set.resellerPathHelp')],
    ['resellerCertFile', t('set.cert'), 'text', t('set.resellerCertHelp')], ['resellerKeyFile', t('set.key'), 'text'],
  ]},
  // logo 那一块上传即生效(不跟「保存」走);副机只读,随主机同步
  { id: 'subpage', title: t('set.subPage'), extra: logoBlock('set-logo', { readonly: isNodeRole() }), fields: [
    ['subPageEnabled', t('set.subPageEnabled'), 'bool', t('set.subPageEnabledHelp')],
    ['subShareEnabled', t('set.subShare'), 'boolOn', t('set.subShareHelp')],
    ['subPageBrand', t('set.subPageBrand'), 'boolOn', t('set.subPageBrandHelp')],
    ['subPageTitle', t('set.subPageTitle'), 'text', t('set.subPageTitleHelp')],
    ['subPageSupport', t('set.subPageSupport'), 'text', t('set.subPageSupportHelp')],
    ['subPageBuyURL', t('set.subPageBuyURL'), 'text', t('set.subPageBuyURLHelp')],
    ['subPageNotice', t('set.subPageNotice'), 'textarea', t('set.subPageNoticeHelp')],
  ]},
  { id: 'notify', title: t('set.notify'), test: true, fields: [
    ['tgEnabled', t('set.tgEnabled'), 'bool', t('set.tgEnabledHelp')],
    ['tgToken', t('set.tgToken'), 'text', t('set.tgTokenHelp')], ['tgChatId', t('set.tgChatId'), 'text', t('set.tgChatIdHelp')],
    ['tgProxy', t('set.tgProxy'), 'text', t('set.tgProxyHelp')],
    ['tgBot', t('set.tgBot'), 'bool', t('set.tgBotHelp')],
    ['webhookUrl', t('set.webhookUrl'), 'text', t('set.webhookUrlHelp')], ['webhookSecret', t('set.webhookSecret'), 'text', t('set.webhookSecretHelp')],
    ['tgOnLogin', t('set.tgOnLogin'), 'boolOn'], ['tgOnUserDisabled', t('set.tgOnUserDisabled'), 'boolOn'],
    ['tgOnUserExpiring', t('set.tgOnUserExpiring'), 'boolOn'], ['tgExpiringDays', t('set.tgExpiringDays'), 'number'],
    ['tgOnQuota', t('set.tgOnQuota'), 'boolOn'], ['tgQuotaPercent', t('set.tgQuotaPercent'), 'number'],
    ['tgOnRuleLimit', t('set.tgOnRuleLimit'), 'bool'],
    ['tgOnUpstream', t('set.tgOnUpstream'), 'boolOn'], ['tgOnCore', t('set.tgOnCore'), 'boolOn'], ['tgOnReach', t('set.tgOnReach'), 'boolOn'],
    ['tgOnCert', t('set.tgOnCert'), 'boolOn', t('set.tgOnCertHelp')],
    ['tgDaily', t('set.tgDaily'), 'boolOn'], ['tgDailyHour', t('set.tgDailyHour'), 'number'],
  ]},
  { id: 'monitor', title: t('set.monitor'), fields: [
    ['upstreamCheckMinutes', t('set.checkMinutes'), 'number', t('set.checkMinutesHelp')],
    ['upstreamCheckFailThreshold', t('set.checkThreshold'), 'number', t('set.checkThresholdHelp')],
  ]},
  // 大陆连通检测只在主机上跑,副机不显示这一组
  { id: 'reach', title: t('set.reach'), masterOnly: true, fields: [
    ['reachAuto', t('set.reachAuto'), 'boolOn', t('set.reachAutoHelp')],
    ['reachMinutes', t('set.reachMinutes'), 'number', t('set.reachMinutesHelp')],
    ['reachToken', t('set.reachToken'), 'text', t('set.reachTokenHelp')],
  ]},
  { id: 'core', title: t('set.core'), fields: [
    ['certFile', t('set.certFile'), 'text', t('set.certHelp')], ['keyFile', t('set.key'), 'text'],
    ['upstreamTestUrl', t('set.testUrl'), 'text', t('set.testUrlHelp')],
    ['statsBucketSeconds', t('set.bucket'), 'number'], ['trafficAge', t('set.trafficAge'), 'number', t('set.trafficAgeHelp')],
    ['allowPrivate', t('set.allowPrivate'), 'bool', t('set.allowPrivateHelp')],
    ['blockBT', t('set.blockBT'), 'bool', t('set.blockBTHelp')],
  ]},
];

export async function render(el) {
  const s = state.settings;
  const isNode = isNodeRole();
  // 重启按钮放在页头右侧,任何时候都看得到
  // 文字包一层:手机宽度下只留 ↻,给页面标题和右上角的帮助、支持按钮让地方(见 style.css .tb-label)
  document.getElementById('topbar-right').innerHTML = `<button class="btn danger sm" data-act="set.restart" title="${esc(t('set.restartHint'))}" aria-label="${esc(t('set.restart'))}">↻ <span class="tb-label">${t('set.restart')}</span></button>`;
  el.innerHTML = `
    <section class="card">
      <div class="card-head"><h2>${t('set.role')}</h2>${badge(isNode ? t('role.node') : t('role.master'), isNode ? 'node' : 'primary')}</div>
      <label class="field check"><input type="checkbox" id="set-nodeMode" ${isNode ? 'checked' : ''}><span>${t('set.roleSwitch')}</span></label>
      <p class="hint" id="role-hint">${isNode ? t('set.roleNode') : t('set.roleMaster')}</p>
      <div class="row" style="margin-top:.7rem"><button class="btn primary" data-act="set.saveRole">${t('common.save')}</button></div>
    </section>
    ${groups().filter(g => !(g.masterOnly && isNode)).map(g => `
      <section class="card">
        <div class="card-head"><h2>${esc(g.title)}${g.restart ? ` <span class="badge warn" title="${t('set.restartHint')}">${t('set.needRestart')}</span>` : ''}</h2><div class="row">${g.test ? `<button class="btn sm" data-act="set.notifyTest">${t('set.tgTest')}</button>` : ''}<button class="btn primary sm" data-act="set.save" data-id="${g.id}">${t('common.save')}</button></div></div>
        <div class="form-grid">${g.fields.map(([k, label, type, help, options]) =>
          type === 'bool' || type === 'boolOn'
            ? check('set-' + k, label, boolSetting(s[k], k === 'subPageEnabled' || type === 'boolOn'), help)
            : type === 'textarea' || type === 'code'
              ? `<div class="full">${field(label, `<textarea id="set-${k}" ${type === 'code' ? 'class="code-area" spellcheck="false" autocomplete="off"' : ''}>${esc(s[k] ?? '')}</textarea>${textMax[k] ? charCount('set-' + k, textMax[k]) : ''}`, help)}</div>`
              : type === 'select'
                ? field(label, `<select id="set-${k}">${options.map(([v, l]) => `<option value="${esc(v)}" ${String(s[k] ?? '') === v ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select>`, help)
                : field(label, `<input id="set-${k}" type="${type}" value="${esc(s[k] ?? '')}" placeholder="${esc(defaults[k] ?? '')}">${textMax[k] ? charCount('set-' + k, textMax[k]) : ''}`, help)).join('')}</div>${g.extra || ''}
      </section>`).join('')}
    <section class="card">
      <div class="card-head"><h2>${t('set.about')}</h2><a href="#/admin" class="btn sm">${t('nav.admin')} →</a></div>
      <dl class="kv">
        <dt>${t('set.version')}</dt><dd class="mono">${esc(state.status.version || '')}</dd>
        <dt>sing-box</dt><dd class="mono">${esc(state.status.singBox || '—')}</dd>
        <dt>${t('set.panelUrl')}</dt><dd class="mono">${esc((state.status.panelTLS ? 'https' : 'http') + '://' + urlHost(state.status.panelTLS ? 'https' : 'http', s.webDomain || '<IP>', state.status.webPort || '') + (state.status.webPath || '/'))}</dd>
        <dt>${t('set.subUrl')}</dt><dd class="mono">${esc((state.status.subTLS ? 'https' : 'http') + '://' + urlHost(state.status.subTLS ? 'https' : 'http', s.webDomain || '<IP>', state.status.subPort || '') + (state.status.subPath || '/sub/') + '<' + t('set.userPh') + '>')}</dd>
        <dt>License</dt><dd>GPL-3.0 · <a href="${esc(state.status.repo || '#')}" target="_blank" rel="noopener">GitHub</a></dd>
      </dl>
      <p class="hint">${t('set.restartNote')}</p>
    </section>`;
  document.getElementById('set-nodeMode').addEventListener('change', e => {
    document.getElementById('role-hint').textContent = e.target.checked ? t('set.roleNode') : t('set.roleMaster');
  });
  bindLogo('set-logo', 'sub-logo');
  bindCharCounts(el);
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
    ${/^http:\/\//i.test(info.apiUrl || '') ? `<p class="hint warn-text">${t('set.pairingPlain')}</p>` : ''}
    <dl class="kv" style="margin-top:.5rem">
      <dt>${t('node.apiUrl')}</dt><dd class="mono">${esc(info.apiUrl)}</dd>
      <dt>${t('node.token')}</dt><dd><div class="sub-box"><code id="pair-token">${esc(info.token)}</code><button class="btn sm" data-act="set.copyToken">${t('common.copy')}</button></div></dd>
      <dt>${t('node.revision')}</dt><dd class="mono">${esc(info.revision || '—')}${info.appliedAt ? ` <span class="muted small">${new Date(Number(info.appliedAt) * 1000).toLocaleString()}</span>` : ''}</dd>
    </dl>`;
  role.after(card);
}

// 设置页直接改订阅证书路径时的确认(判法见 subtls.js):scheme 不变或没有用户时直接放行。
// 用户数取 GET /cert 的 subUsers(含停用的、代理名下的);取不到时退回本页已加载的用户列表。
async function confirmSubScheme(body) {
  if (!subSettingsChange(state.settings, body, 1)) return true;
  let n;
  try { n = Number((await get('cert')).subUsers); } catch { /* 下面退回本地列表 */ }
  if (!Number.isFinite(n)) n = (state.users || []).length;
  const ch = subSettingsChange(state.settings, body, n);
  return !ch || confirm(t('cert.subSchemeConfirm', { n: ch.n, from: ch.from, to: ch.to }), { danger: true });
}

registerActions({
  'set.save': async id => {
    const g = groups().find(x => x.id === id);
    const body = {};
    g.fields.forEach(([k, , type]) => { body[k] = (type === 'bool' || type === 'boolOn') ? String(fchk('set-' + k)) : fv('set-' + k).trim(); });
    // 面板路径改动保存即生效:旧地址随即失效,保存成功后直接跳到新地址(不能再用旧前缀请求接口)
    // 订阅证书 / 私钥路径一改,订阅可能在 http ↔ https 之间切换:所有用户手里的订阅地址都会变,和证书页一样先确认
    if (id === 'sub' && !await confirmSubScheme(body)) return;
    const norm = p => { p = (p || '/app/').trim(); if (!p.startsWith('/')) p = '/' + p; if (!p.endsWith('/')) p += '/'; return p; };
    if (id === 'panel' && norm(body.webPath) !== norm(state.status.webPath)) {
      try {
        await post('settings', body);
        const np = norm(body.webPath);
        toast(t('set.pathChanged', { p: np }), 'ok');
        setTimeout(() => { location.href = np + '#/settings'; }, 900);
      } catch (e) { toast(e.message, 'err'); }
      return;
    }
    try {
      const r = await post('settings', body); await load('settings', 'status'); toast(t('set.saved') + (r.note ? ' · ' + r.note : ''), 'ok');
      // Clash 模板写了 ipv6: false:存下了,但有 IPv6 节点时订阅会改成 true,要让人知道
      if ((r.hints || []).includes('clashIPv6')) confirm(t('set.clashIPv6Hint'), { title: t('set.saved'), okText: t('node.ack'), notice: true });
    }
    catch (e) { toast(e.message, 'err'); }
  },
  'set.tmplDefault': async id => {
    const box = document.getElementById(id === 'clash' ? 'set-subClashExt' : 'set-subSingboxExt');
    if (box.value.trim() && !await confirm(t('set.tmplOverwrite'))) return;
    try { const r = await get('sub-templates'); box.value = r[id]; box.focus(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'set.saveRole': async () => {
    const toNode = fchk('set-nodeMode');
    const cur = String(state.settings.nodeMode).toLowerCase() === 'true';
    if (toNode !== cur && !await confirm(toNode ? t('set.roleToNode') : t('set.roleToMaster'), { danger: true })) return;
    try { const r = await post('settings', { nodeMode: String(toNode) }); await load('settings', 'status'); toast(r && r.note && toNode ? r.note : t('set.saved'), 'ok'); setTimeout(() => location.reload(), toNode ? 2500 : 0); }
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
});

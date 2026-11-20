import { state, load, applyUIPrefs, NAV_ORDER } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, registerActions, badge, fv, fchk, copy, urlHost, charCount, bindCharCounts, PAGE_TEXT_MAX, openModal, debounce } from '../ui.js';
import { ICONS } from '../icons.js';
import { followPanel } from '../panelmove.js';
import { logoBlock, bindLogo } from '../logo.js';
import { renderHASettings, saveHASettings } from '../ha.js';
import { subSettingsChange } from '../subtls.js';
import { refreshReward } from '../reward.js';
import { ATTN_GROUPS, attnCatsFor, normMutes } from '../attnmute.js';
import { trErr } from '../errmsg.js';
import { navHidden, normNavHidden, isConcise, NAV_LOCKED } from '../uiprefs.js';

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

// 输入框里的示例(不是默认值:留空就是空,保存条里也写「空」)
const placeholders = { camoRedirect: 'https://www.example.com' };

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
  { id: 'panel', title: t('set.panel'), fields: [
    ['webDomain', t('set.webDomain'), 'text', t('set.webDomainHelp')],
    ['timezone', t('set.timezone'), 'select', t('set.timezoneHelp'), TIMEZONES.map(z => [z, z])],
    ['webListen', t('set.listen'), 'text'], ['webPort', t('set.port'), 'number'], ['webPath', t('set.path'), 'text', t('set.pathHelp')],
    ['webCertFile', t('set.cert'), 'text', t('set.certManaged')], ['webKeyFile', t('set.key'), 'text'],
    ['trustedProxies', t('set.trustedProxies'), 'text', t('set.trustedProxiesHelp')],
    ['supportOnline', t('set.supportOnline'), 'boolOn', t('set.supportOnlineHelp')],
    // 流量奖励和功德箱、在线设备数一样是「要不要为它联网」的开关,放在一起(用户 N15);只在主机上有(第 6 项 = 只给主机)
    ['rewardEnabled', t('set.rewardEnabled'), 'boolOn', t('set.rewardEnabledHelp'), null, true],
  ]},
  { id: 'sub', title: t('set.sub'), fields: [
    ['subDomain', t('set.subDomain'), 'text', t('set.subDomainHelp')],
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
    ['subClashDefaultGroups', t('set.subClashDefaultGroups'), 'boolOn', t('set.subClashDefaultGroupsHelp')],
    ['subClashExt', t('set.subClashExt'), 'code', t('set.subClashExtHelp')],
    ['subSingboxExt', t('set.subSingboxExt'), 'code', t('set.subSingboxExtHelp')],
  ]},
  { id: 'reseller', title: t('set.reseller'), fields: [
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
  // 页面伪装(用户 N2):陌生人顺着端口、域名找过来时看到的普通网站。只在主机上设,随主机同步;下面是这台的预览(见 renderCamo)
  { id: 'camo', title: t('set.camo'), masterOnly: true, extra: '<div class="camo-box" id="camo-box" aria-live="polite"></div>', fields: [
    ['camoEnabled', t('set.camoEnabled'), 'bool', t('set.camoEnabledHelp')],
    ['camoTemplate', t('set.camoTemplate'), 'select', t('set.camoTemplateHelp'),
      [['', t('set.camoT.auto')], ...CAMO_TEMPLATES.map(x => [x, t('set.camoT.' + x)]), ['redirect', t('set.camoT.redirect')]]],
    ['camoRedirect', t('set.camoRedirect'), 'text', t('set.camoRedirectHelp')], // 只在选了「跳转到我的网站」时显示(camoMode)
    ['camoPorts', t('set.camoPorts'), 'bool', t('set.camoPortsHelp')],
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
  // 概况「需要处理」里哪几类要提(用户 N14):一组勾选框,存成一个设置(关掉的类别)
  { id: 'attn', title: t('set.attn'), fields: [
    ['attnMute', t('set.attnMute'), 'mutes', t('set.attnMuteHelp')],
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
  document.getElementById('topbar-right').innerHTML = `<button class="btn danger sm" data-act="set.restart" title="${esc(t('set.restartHint'))}" aria-label="${esc(t('set.restart'))}">${ICONS.restart}<span class="tb-label">${t('set.restart')}</span></button>`;
  // 版式:左边一列分区导航(跟着滚动高亮,点了跳过去),右边一列分区卡片;卡片里一项设置一行。
  // 保存:整页一个规则 —— 动了任何一项,窗口底部浮出保存条(见下面「未保存的改动」);分区里不再各有一个「保存」。
  // 角色那个开关例外:拨了当场确认、当场生效(它要重载页面,不和别的设置混在一起存)
  el.innerHTML = `<div class="settings">
    <nav class="set-nav" id="set-nav" aria-label="${esc(t('set.title'))}"></nav>
    <div class="set-main">
    ${orderedCards(s, isNode)}
    <section class="card set-sec" id="sec-about" data-nav="${esc(t('set.about'))}">
      <div class="card-head"><h2>${t('set.about')}</h2><a href="#/admin" class="btn sm">${t('nav.admin')} →</a></div>
      <dl class="kv">
        <dt>${t('set.version')}</dt><dd class="mono">${esc(state.status.version || '')}</dd>
        <dt>sing-box</dt><dd class="mono">${esc(state.status.singBox || '—')}</dd>
        <dt>${t('set.panelUrl')}</dt><dd class="mono">${esc((state.status.panelTLS ? 'https' : 'http') + '://' + urlHost(state.status.panelTLS ? 'https' : 'http', s.webDomain || '<IP>', state.status.webPort || '') + (state.status.webPath || '/'))}</dd>
        <dt>${t('set.subUrl')}</dt><dd class="mono">${esc((state.status.subTLS ? 'https' : 'http') + '://' + urlHost(state.status.subTLS ? 'https' : 'http', s.subDomain || s.webDomain || '<IP>', state.status.subPort || '') + (state.status.subPath || '/sub/') + '<' + t('set.userPh') + '>')}</dd>
        <dt>License</dt><dd>GPL-3.0 · <a href="${esc(state.status.repo || '#')}" target="_blank" rel="noopener">GitHub</a></dd>
      </dl>
      <p class="hint xp">${t('set.restartNote')}</p>
    </section>
    <div class="savebar" id="set-bar" hidden></div></div></div>`;
  // 角色开关:拨了就问、确认了就生效;取消或失败就拨回去
  document.getElementById('set-nodeMode').addEventListener('change', async e => {
    const sw = e.target, toNode = sw.checked;
    if (!await confirm(toNode ? t('set.roleToNode') : t('set.roleToMaster'), { danger: true })) { sw.checked = !toNode; return; }
    sw.disabled = true;
    try {
      const r = await post('settings', { nodeMode: String(toNode) });
      await load('settings', 'status');
      snapshot(); renderBar(); // 页面马上要重载:别让"有没保存的改动"拦一下
      toast(r && r.note && toNode ? r.note : t('set.saved'), 'ok');
      setTimeout(() => location.reload(), toNode ? 2500 : 0);
    } catch (err) { toast(err.message, 'err'); sw.checked = !toNode; sw.disabled = false; }
  });
  bindUI();
  bindLogo('set-logo', 'sub-logo');
  bindCharCounts(el);
  // 页面伪装:换了样式马上按它预览(还没保存也一样);撤销这一项时 setCtl 发的是 input,下拉框本身选了也会发 input
  const camoSel = document.getElementById('set-camoTemplate');
  if (camoSel) camoSel.addEventListener('input', () => { camoMode(); renderCamo(); });

  const camoUrl = document.getElementById('set-camoRedirect');
  if (camoUrl) camoUrl.addEventListener('input', debounce(() => renderCamo(), 450));
  camoMode();
  renderCamo();
  el.querySelectorAll('.mutes').forEach(box => box.addEventListener('change', () => syncMutes(box)));
  // 未保存的改动:记下现在各项的值,之后任何一项动了就重算保存条
  barOpen = false;
  snapshot();
  const main = el.querySelector('.set-main');
  const onEdit = e => {
    const el = e.target;
    if (!el || el.id === 'set-nodeMode') return;
    if ((el.id && el.id.startsWith('set-')) || (el.closest && el.closest('#ha-card'))) renderBar();
  };
  main.addEventListener('input', onEdit);
  main.addEventListener('change', onEdit);
  if (isNode) renderPairing().then(buildNav);
  // 主机容灾:紧跟在角色卡片后面(服务器页角色卡片里的「去设置」跳到这里)
  const haCard = document.createElement('section');
  haCard.className = 'card set-sec';
  haCard.id = 'ha-card';
  haCard.dataset.nav = t('ha.title');
  haCard.dataset.navg = 'multi';
  document.getElementById('sec-role').after(haCard);
  buildNav();
  renderHASettings(haCard).then(() => {
    buildNav(); // 取不到容灾状态时这张卡会被拿掉,导航跟着少一项
    haSnapshot(); // 容灾卡片画好了:记下它各项现在的值(之后的改动进保存条)
    let to = '';
    try { to = sessionStorage.getItem('m-ui.scrollTo') || ''; sessionStorage.removeItem('m-ui.scrollTo'); } catch {}
    goTo(to);
  });
}

// ---- 分区的排列(用户 11-21)----
// 按用途分组,左侧导航加组名。「角色」在最上面(用户:「角色需要在最上面 这个是多服务器的关键功能」),容灾卡、副机的配对信息插在它后面
const SET_GROUPS = [
  ['multi', ['role']],
  ['panel', ['panel', 'reseller', 'ui', 'notify', 'attn']],
  ['sub', ['sub', 'subtmpl', 'subpage', 'camo']],
  ['check', ['monitor', 'reach', 'core']],
];
function orderedCards(s, isNode) {
  const defs = new Map(groups().filter(g => !(g.masterOnly && isNode)).map(g => [g.id, g]));
  const listed = new Set(SET_GROUPS.flatMap(([, ids]) => ids));
  const rest = [...defs.keys()].filter(id => !listed.has(id)); // 以后新加的分区忘了排:放在最后一组,不会丢
  return SET_GROUPS.map(([gk, ids], i) => [...ids, ...(i === SET_GROUPS.length - 1 ? rest : [])].map(id =>
    id === 'role' ? roleCard(isNode, gk) : id === 'ui' ? uiCard(gk) : defs.has(id) ? groupCard(s, defs.get(id), isNode, gk) : '').join('')).join('');
}
function roleCard(isNode, gk) {
  return `<section class="card set-sec" id="sec-role" data-nav="${esc(t('set.role'))}" data-navg="${gk}">
      <div class="card-head"><h2>${t('set.role')} ${badge(isNode ? t('role.node') : t('role.master'), isNode ? 'node' : 'primary')}</h2></div>
      <div class="set-rows">
        <div class="set-row bool"><div class="set-info"><label class="set-label" for="set-nodeMode">${t('set.roleSwitch')}</label><p class="set-help xp" id="role-hint">${isNode ? t('set.roleNode') : t('set.roleMaster')}</p></div>
          <div class="set-ctl"><label class="switch"><input type="checkbox" id="set-nodeMode" ${isNode ? 'checked' : ''}><span></span></label></div></div>
      </div>
    </section>`;
}
function groupCard(s, g, isNode, gk) {
  return `
      <section class="card set-sec" id="sec-${g.id}" data-nav="${esc(g.title)}" data-navg="${gk}">
        <div class="card-head"><h2>${esc(g.title)}</h2>${g.test ? `<div class="row"><button class="btn sm" data-act="set.notifyTest">${t('set.tgTest')}</button></div>` : ''}</div>
        <div class="set-rows">${g.fields.filter(f => !(f[5] && isNode)).map(f => rowHTML(s, f)).join('')}</div>${g.extra || ''}
      </section>`;
}

// ---- 界面(用户 11-21)----
// 「有的用户可能有的栏目用不到，在设置里增加一个功能可隐藏栏目 点击轻量弹窗 可勾选隐藏左边的栏目（只是前端隐藏，设置不支持隐藏）」
// 「跟这个栏目隐藏的设置在一个板块里 增加一个切换详细/简洁模式」
// 两项都是即改即生效、即存:拨了马上看到效果(侧栏少了一项、说明收起来了),不用再去点底部的保存
function uiCard(gk) {
  const concise = isConcise(state.settings);
  return `<section class="card set-sec" id="sec-ui" data-nav="${esc(t('set.ui'))}" data-navg="${gk}">
      <div class="card-head"><h2>${t('set.ui')}</h2></div>
      <div class="set-rows">
        <div class="set-row bool ui-row"><div class="set-info"><span class="set-label" id="ui-nav-label">${t('set.uiNav')}</span><p class="set-help xp">${esc(t('set.uiNavHelp'))}</p></div>
          <div class="set-ctl ui-ctl"><span class="ui-nav-sum" id="ui-nav-sum"></span><button class="btn sm ui-nav-btn" type="button" id="ui-nav-btn" aria-haspopup="dialog" aria-expanded="false" aria-describedby="ui-nav-sum">${ICONS.sidebar || ''}<span>${t('set.uiNavPick')}</span></button></div></div>
        <div class="set-row bool ui-row"><div class="set-info"><span class="set-label" id="ui-mode-label">${t('set.uiMode')}</span><p class="set-help xp">${esc(t('set.uiModeHelp'))}</p></div>
          <div class="set-ctl ui-ctl"><div class="seg" role="group" aria-labelledby="ui-mode-label" id="ui-mode">
            <button type="button" data-mode="full" class="${concise ? '' : 'active'}" aria-pressed="${!concise}">${t('set.uiModeFull')}</button>
            <button type="button" data-mode="concise" class="${concise ? 'active' : ''}" aria-pressed="${concise}">${t('set.uiModeConcise')}</button></div></div></div>
      </div>
    </section>`;
}
// 侧栏的栏目(和 app.js 的 NAV_GROUPS 同一份):浮层里按同样的分组列出来
const navSum = () => {
  const n = navHidden(state.settings).size;
  return n ? t('set.uiNavSome', { n }) : t('set.uiNavAll');
};
let uiSaveTimer = 0, uiSaving = Promise.resolve(), uiPending = {};
// saveUI 存界面的一项:先在本页生效(state.settings 改了、侧栏和简洁模式跟着变),再存;存失败就按服务器上的值改回来。
// 还没发出去的几项攒在一起发:勾完栏目(等 0.4 秒再存)紧接着切简洁模式,两项都要存上,不能后一项把前一项的计时冲掉
function saveUI(key, value, delay = 0) {
  state.settings[key] = value;
  uiPending[key] = value;
  applyUIPrefs();
  clearTimeout(uiSaveTimer);
  uiSaveTimer = setTimeout(() => {
    const body = uiPending;
    uiPending = {};
    uiSaving = uiSaving.then(() => post('settings', body)).catch(async e => {
      toast(trErr(e.message), 'err');
      await load('settings').catch(() => {});
      syncUI();
    });
  }, delay);
}
// syncUI 卡片上的状态跟着设置走(存失败改回来时、换语言重画后)
function syncUI() {
  const sum = document.getElementById('ui-nav-sum');
  if (sum) sum.textContent = navSum();
  const concise = isConcise(state.settings);
  document.querySelectorAll('#ui-mode button').forEach(b => {
    const on = (b.dataset.mode === 'concise') === concise;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', String(on));
  });
  const pop = document.getElementById('ui-nav-pop');
  if (pop) {
    const hide = navHidden(state.settings);
    pop.querySelectorAll('input[data-page]').forEach(cb => { cb.checked = cb.dataset.page === NAV_LOCKED || !hide.has(cb.dataset.page); });
  }
}
function bindUI() {
  syncUI();
  const mode = document.getElementById('ui-mode');
  if (mode) mode.addEventListener('click', e => {
    const b = e.target.closest('button[data-mode]');
    if (!b || b.classList.contains('active')) return;
    saveUI('uiConcise', String(b.dataset.mode === 'concise'));
    syncUI();
  });
  const btn = document.getElementById('ui-nav-btn');
  if (btn) btn.addEventListener('click', () => (document.getElementById('ui-nav-pop') ? closeNavPop(true) : openNavPop(btn)));
}
// ---- 选栏目的浮层:挂在 body 上(不被卡片裁掉),贴着按钮;点外面、Esc、换页都收起 ----
let popCleanup = null;
function openNavPop(btn) {
  closeNavPop();
  const hide = navHidden(state.settings);
  const item = id => {
    const locked = id === NAV_LOCKED;
    return `<label class="np-item${locked ? ' locked' : ''}"><input type="checkbox" data-page="${id}" ${locked || !hide.has(id) ? 'checked' : ''} ${locked ? 'disabled' : ''}>` +
      `<span class="np-ic">${ICONS[id] || ''}</span><span class="np-t">${esc(t('nav.' + id))}</span>${locked ? `<span class="np-lock" title="${esc(t('set.uiNavLocked'))}" aria-label="${esc(t('set.uiNavLocked'))}">${ICONS.lock || ''}</span>` : ''}</label>`;
  };
  const groups = NAV_ORDER.map(g => `<div class="np-g${g.key ? '' : ' np-top'}">${g.key ? `<div class="np-h">${esc(t('nav.group.' + g.key))}</div>` : ''}${g.items.map(item).join('')}</div>`).join('');
  const pop = document.createElement('div');
  pop.className = 'np-pop';
  pop.id = 'ui-nav-pop';
  pop.setAttribute('role', 'dialog');
  pop.setAttribute('aria-labelledby', 'np-title');
  pop.innerHTML = `<div class="np-head"><b id="np-title">${esc(t('set.uiNavPopTitle'))}</b><button type="button" class="btn ghost sm" id="np-all">${esc(t('set.uiNavShowAll'))}</button></div>
    <div class="np-body">${groups}</div><p class="np-foot">${esc(t('set.uiNavPopNote'))}</p>`;
  document.body.append(pop);
  btn.setAttribute('aria-expanded', 'true');
  const place = () => {
    const r = btn.getBoundingClientRect();
    if (!r.width) { closeNavPop(); return; }
    const vw = document.documentElement.clientWidth, vh = innerHeight, m = 8;
    const w = pop.offsetWidth, h = pop.offsetHeight;
    const left = Math.min(Math.max(m, r.right - w), vw - w - m);
    const below = r.bottom + 6 + h <= vh - m || r.top - 6 - h < m;
    pop.style.left = Math.round(left) + 'px';
    pop.style.top = Math.round(below ? Math.min(r.bottom + 6, vh - h - m) : r.top - 6 - h) + 'px';
  };
  place();
  const changed = () => {
    const hidden = [...pop.querySelectorAll('input[data-page]')].filter(cb => !cb.checked).map(cb => cb.dataset.page);
    saveUI('uiNavHidden', normNavHidden(hidden, NAV_ORDER.flatMap(g => g.items)), 400);
    syncUI();
    allBtn.disabled = !hidden.length; // 一个都没藏时「全部显示」没有意义
  };
  const allBtn = pop.querySelector('#np-all');
  allBtn.disabled = !hide.size;
  pop.addEventListener('change', changed);
  allBtn.addEventListener('click', () => { pop.querySelectorAll('input[data-page]').forEach(cb => { cb.checked = true; }); changed(); });
  const outside = e => { if (!pop.contains(e.target) && !btn.contains(e.target)) closeNavPop(); };
  const key = e => { if (e.key === 'Escape') { e.stopPropagation(); closeNavPop(true); } };
  const onScroll = e => { if (!pop.contains(e.target)) place(); };
  document.addEventListener('pointerdown', outside, true);
  document.addEventListener('keydown', key, true);
  document.addEventListener('scroll', onScroll, true);
  window.addEventListener('resize', place);
  window.addEventListener('hashchange', closeNavPop);
  popCleanup = () => {
    document.removeEventListener('pointerdown', outside, true);
    document.removeEventListener('keydown', key, true);
    document.removeEventListener('scroll', onScroll, true);
    window.removeEventListener('resize', place);
    window.removeEventListener('hashchange', closeNavPop);
    btn.setAttribute('aria-expanded', 'false');
    pop.remove();
  };
  const first = pop.querySelector('input:not(:disabled)');
  if (first) first.focus({ preventScroll: true });
}
function closeNavPop(focusBtn = false) {
  if (!popCleanup) return;
  const f = popCleanup;
  popCleanup = null;
  f();
  if (focusBtn === true) document.getElementById('ui-nav-btn')?.focus();
}

// goTo 跳到设置页的某处(别处的链接经 openSettingsAt 过来):某一项(set-xxx)滚到那一行、闪一下;分区(sec-xxx、ha-card)滚到卡片顶上、闪一下
function goTo(to) {
  if (!to) return;
  if (to.startsWith('set-')) { focusRow(to); return; }
  const sec = document.getElementById(to);
  if (!sec || !sec.classList.contains('set-sec')) return;
  sec.scrollIntoView({ block: 'start' });
  sec.classList.remove('flash'); void sec.offsetWidth; sec.classList.add('flash');
  setTimeout(() => sec.classList.remove('flash'), 2800);
}
// 已经在设置页时别处要跳过来(ui.js openSettingsAt):直接滚,不重画整页
document.addEventListener('mui:settings-goto', e => { if (document.getElementById('set-nav')) goTo(e.detail); });

// 从别处跳到某一项设置(流量奖励活动页里的「去设置里关闭」):滚到那一行、闪一下,焦点落在它的开关上。
// 等容灾那张卡画完才滚(它是后插进来的,先滚的话位置会被它顶走)
function focusRow(id) {
  const ctl = document.getElementById(id);
  const row = ctl && ctl.closest('.set-row');
  if (!row) return;
  row.scrollIntoView({ block: 'center' });
  row.classList.remove('flash'); void row.offsetWidth; row.classList.add('flash');
  try { ctl.focus({ preventScroll: true }); } catch { /* 有的控件不接焦点 */ }
  setTimeout(() => row.classList.remove('flash'), 2800);
}

// ---- 未保存的改动:窗口底部的保存条 ----
// 以前每个分区右上角各有一个「保存」:开关拨了不算数,还得回到分区顶上去点;分区一长,改完要往回滚。
// 现在整页一个规则:动了任何一项(开关、输入框、下拉都一样),底部就浮出保存条 —— 几处改动、改了哪几项、放弃 / 保存;
// 点左边那段向上展开清单,逐项写着「原来 → 现在」,可以单独撤销一项。普通设置只发改过的键,一次请求、要么都成要么都不成;
// 「主机容灾」那张卡(ha.js 画的,保存要输管理员密码)也算在里面:点保存时先存普通设置,再走它自己的确认。
let orig = {};      // 键 → 画出页面时控件上的值(开关是 'true' / 'false',文字是去掉首尾空白的)
let barOpen = false;
const SECRET = /token|secret/i; // 令牌、密钥类:清单里不显示内容
const isBool = type => type === 'bool' || type === 'boolOn';
const fieldList = () => groups().filter(g => !(g.masterOnly && isNodeRole()))
  .flatMap(g => g.fields.filter(f => !(f[5] && isNodeRole())).map(([k, label, type, , options]) => ({ k, label, type, options, group: g.title, gid: g.id })));
const ctlVal = f => { const el = document.getElementById('set-' + f.k); return !el ? undefined : isBool(f.type) ? String(el.checked) : el.value.trim(); };
const dirtyFields = () => fieldList().filter(f => { const v = ctlVal(f); return v !== undefined && v !== orig[f.k]; });
function snapshot() {
  orig = {};
  fieldList().forEach(f => { const v = ctlVal(f); if (v !== undefined) orig[f.k] = v; });
}
function setCtl(f, v) {
  const el = document.getElementById('set-' + f.k);
  if (!el) return;
  if (isBool(f.type)) el.checked = v === 'true'; else el.value = v;
  if (f.k === 'camoTemplate') camoMode(); // 撤销这一项时「跳转到」那一行跟着显示 / 收起
  if (f.type === 'mutes') { // 勾选框跟着隐藏的值走(撤销这一项时)
    const off = new Set(String(v || '').split(',').filter(Boolean));
    document.querySelectorAll(`.mutes[data-for="${el.id}"] input[data-mute]`).forEach(cb => { cb.checked = !off.has(cb.dataset.mute); });
  }
  el.dispatchEvent(new Event('input', { bubbles: true })); // 字数统计跟着更新,保存条也跟着重算
}
// syncMutes 概况提醒的勾选框动了:把关掉的类别写回隐藏的那个值(这个角色看不到的类别原样留着),保存条跟着重算
function syncMutes(box) {
  const input = document.getElementById(box.dataset.for);
  if (!input) return;
  const shown = [...box.querySelectorAll('input[data-mute]')];
  const was = new Set(String(input.dataset.orig || '').split(',').filter(Boolean));
  for (const cb of shown) was.delete(cb.dataset.mute);
  for (const cb of shown) if (!cb.checked) was.add(cb.dataset.mute);
  input.value = normMutes([...was].join(','));
  input.dispatchEvent(new Event('input', { bubbles: true }));
}
const clip = v => (v.length > 40 ? v.slice(0, 38) + '…' : v);
// 清单里一个值怎么写:开关写开 / 关,下拉写选项名,空的写默认值,令牌不显示内容,长文本只写字数
function showVal(f, v) {
  if (isBool(f.type)) return t(v === 'true' ? 'set.sb.on' : 'set.sb.off');
  if (f.type === 'mutes') return v ? t('set.attnOff', { list: v.split(',').map(id => t('attn.cat.' + id)).join(t('node.sep')) }) : t('set.attnAllOn');

  if (f.type === 'select') { const o = (f.options || []).find(x => x[0] === v); return o ? o[1] : v; }
  if (v === '') return defaults[f.k] !== undefined ? t('set.sb.defaultV', { v: defaults[f.k] }) : t('set.sb.empty');
  if (SECRET.test(f.k)) return '••••••';
  if (f.type === 'textarea' || f.type === 'code') return t('set.sb.chars', { n: v.length });
  return clip(v);
}

// 容灾卡片里的控件:不认识它的字段,就按页面上实际有的控件来 —— 单选组一项、别的控件一项,名字取旁边的标签。
// 每次现找(选了别的 DNS 服务商,凭据那几栏会重画)
function haFields() {
  const card = document.getElementById('ha-card');
  if (!card) return [];
  const out = [], radios = new Set();
  const text = el => (el ? el.textContent : '').trim();
  card.querySelectorAll('input, select, textarea').forEach(el => {
    if (el.type === 'radio') {
      if (!el.name || radios.has(el.name)) return;
      radios.add(el.name);
      const group = () => [...card.querySelectorAll(`input[type="radio"][name="${el.name}"]`)];
      out.push({ k: 'ha:' + el.name, blank: '', label: (el.closest('[role="radiogroup"]') || card).getAttribute('aria-label') || el.name,
        get: () => (group().find(r => r.checked) || {}).value || '',
        set: v => { const r = group().find(x => x.value === v); if (r) { r.checked = true; r.dispatchEvent(new Event('change', { bubbles: true })); } },
        show: v => { const r = group().find(x => x.value === v); return r ? text(r.closest('label').querySelector('b')) || v : v || t('set.sb.empty'); } });
      return;
    }
    if (!el.id || el.type === 'hidden' || el.type === 'search' || el.type === 'button') return;
    const box = el.closest('.field');
    const bool = el.type === 'checkbox', pick = el.tagName === 'SELECT';
    const secret = el.type === 'password' || SECRET.test(el.id);
    out.push({ k: 'ha:' + el.id, blank: bool ? 'false' : '',
      label: text(box && (box.querySelector('.label') || box.querySelector(':scope > span:not(.help)'))) || el.id,
      get: () => (bool ? String(el.checked) : el.value.trim()),
      set: v => { if (bool) el.checked = v === 'true'; else el.value = v; el.dispatchEvent(new Event(bool || pick ? 'change' : 'input', { bubbles: true })); },
      show: v => (bool ? t(v === 'true' ? 'set.sb.on' : 'set.sb.off')
        : pick ? text([...el.options].find(o => o.value === v)) || v
          : v === '' ? t('set.sb.empty') : secret ? '••••••' : clip(v)) });
  });
  return out;
}
let haOrig = new Map();
const haWas = f => (haOrig.has(f.k) ? haOrig.get(f.k) : f.blank);
const haDirty = () => haFields().filter(f => f.get() !== haWas(f));
function haSnapshot() { haOrig = new Map(haFields().map(f => [f.k, f.get()])); }

// 现在所有没保存的改动(普通设置 + 容灾),整理成清单要显示的样子
function changes() {
  const out = dirtyFields().map(f => ({ k: f.k, group: f.group, label: f.label, from: showVal(f, orig[f.k]), to: showVal(f, ctlVal(f)) }));
  for (const f of haDirty()) out.push({ k: f.k, group: t('ha.title'), label: f.label, from: f.show(haWas(f)), to: f.show(f.get()) });
  return out;
}
function undoOne(k) {
  if (k.startsWith('ha:')) { const f = haFields().find(x => x.k === k); if (f) f.set(haWas(f)); return; }
  const f = fieldList().find(x => x.k === k);
  if (f) setCtl(f, orig[k]);
}

// 保存条:左边「几处改动」+ 最多两枚改动项的名字(其余折成 +N,放不下的整枚不显示),右边 放弃 / 保存。
// 改得再多条本身也不变长;清单限高、在里面滚动
// barHTML 保存条现在画着的内容。内容没变就不重画:改完输入框直接点「保存」时,按下鼠标那一下输入框失焦、触发 change 又走到这里,
// 重画会把鼠标底下的「保存」换成新按钮,这一下点击就丢了,要点第二下才存(v0.8.1 起就这样,11-21 全量审查时发现)
let barHTML = '';
function renderBar() {
  const bar = document.getElementById('set-bar');
  if (!bar) return;
  const list = changes();
  bar.hidden = !list.length;
  if (!list.length) { barOpen = false; bar.innerHTML = ''; barHTML = ''; return; }
  const html = `
    <div class="sb-list" ${barOpen ? '' : 'hidden'}>${list.map(c => `<div class="sb-row">
        <span class="sb-name" title="${esc(c.group + ' · ' + c.label)}"><small>${esc(c.group)}</small>${esc(c.label)}</span>
        <span class="sb-diff"><s title="${esc(c.from)}">${esc(c.from)}</s><i class="i i-arrow"></i><b title="${esc(c.to)}">${esc(c.to)}</b></span>
        <button type="button" class="btn sm ghost" data-act="set.undo" data-id="${esc(c.k)}">${t('set.sb.undo')}</button>
      </div>`).join('')}</div>
    <div class="sb-main">
      <button type="button" class="sb-sum" data-act="set.barToggle" aria-expanded="${barOpen}" title="${esc(t('set.sb.toggle'))}">
        <span class="sb-dot"></span><b>${esc(t('set.sb.n', { n: list.length }))}</b>
        <span class="sb-chips">${list.slice(0, 2).map(c => `<span class="sb-chip">${esc(c.label)}</span>`).join('')}</span>${list.length > 2 ? `<span class="sb-chip sb-more">+${list.length - 2}</span>` : ''}
        <i class="i i-chev"></i>
      </button>
      <button type="button" class="btn sm sb-discard" data-act="set.discard">${t('set.sb.discard')}</button>
      <button type="button" class="btn primary sm" data-act="set.saveAll">${t('common.save')}</button>
    </div>`;
  if (html === barHTML && bar.firstElementChild) return; // 页面整个重画过的话保存条是新的、空的,照常画
  barHTML = html;
  const keep = bar.querySelector('.sb-list') ? bar.querySelector('.sb-list').scrollTop : 0;
  bar.innerHTML = html;
  const box = bar.querySelector('.sb-list');
  if (box) box.scrollTop = keep;
}
// 换页前(路由会来问,见 app.js)和关标签页前:有没保存的改动就提醒一句
export function leaveGuard() {
  const n = changes().length;
  return n ? t('set.sb.leave', { n }) : '';
}
if (typeof window !== 'undefined') {
  window.addEventListener('beforeunload', e => {
    if (document.getElementById('set-bar') && changes().length) { e.preventDefault(); e.returnValue = ''; }
  });
}

async function saveAll() {
  const list = dirtyFields();
  // 容灾那张卡走它自己的保存(要输管理员密码);存好后它会重画,重新记一次它的起点
  const saveHA = () => { if (haDirty().length) saveHASettings(() => { haSnapshot(); renderBar(); }); };
  if (!list.length) { saveHA(); return; }
  const body = {};
  list.forEach(f => { body[f.k] = ctlVal(f); });
  // 订阅证书 / 私钥路径一改,订阅可能在 http ↔ https 之间切换:所有用户手里的订阅地址都会变,和证书页一样先确认
  if (('subCertFile' in body || 'subKeyFile' in body) && !await confirmSubScheme({ subCertFile: fv('set-subCertFile').trim(), subKeyFile: fv('set-subKeyFile').trim() })) return;
  // 和后端 normalizePath(web/server.go)一样整理:去掉两头的空白和引号,空的算 /app/,连续的斜杠并成一个,补上首尾的斜杠。
  // 原来没并斜杠:填「//abc」时后端实际是 /abc/,这里却跳到「//abc/」—— 浏览器把它当成另一台主机 abc 的地址
  const norm = p => { p = String(p || '').trim().replace(/^["']+|["']+$/g, '').trim() || '/app/'; p = p.replace(/\/{2,}/g, '/'); if (!p.startsWith('/')) p = '/' + p; if (!p.endsWith('/')) p += '/'; return p; };
  const pathChanged = 'webPath' in body && norm(body.webPath) !== norm(state.status.webPath);
  const btn = document.querySelector('[data-act="set.saveAll"]');
  if (btn) btn.disabled = true;
  try {
    const r = await post('settings', body);
    snapshot(); // 存下了:现在的值就是新的起点
    barOpen = false;
    renderBar();
    if (r && r.panelSwitch) { toast(t('set.saved'), 'ok'); followPanel(r.panelSwitch); return; }
    // 面板路径改动保存即生效:旧地址随即失效,直接跳到新地址(不能再用旧前缀请求接口)
    if (pathChanged) {
      const np = norm(body.webPath);
      toast(t('set.pathChanged', { p: np }), 'ok');
      // 前面写明本站地址:路径里再怎么写(比如带反斜杠),浏览器也只会在本站里跳
      setTimeout(() => { location.href = location.origin + np + '#/settings'; }, 900);
      return;
    }
    await load('settings', 'status');
    toast(t('set.saved') + (r && r.note ? ' · ' + r.note : ''), 'ok');
    if (list.some(f => f.gid === 'camo')) renderCamo(); // 「已开启」和 80 / 443 的状况跟着存下的值走
    if ('rewardEnabled' in body) refreshReward(); // 顶栏的金币跟着开关走
    // Clash 模板写了 ipv6: false:存下了,但有 IPv6 节点时订阅会改成 true,要让人知道
    if (((r && r.hints) || []).includes('clashIPv6')) await confirm(t('set.clashIPv6Hint'), { title: t('set.saved'), okText: t('node.ack'), notice: true });
    // 关着「Clash 自动生成默认策略组」,模板却还用着 Proxy / Auto:存下了,但订阅里仍会生成用到的组
    if (((r && r.hints) || []).includes('clashGroupsRef')) await confirm(t('set.clashGroupsRefHint'), { title: t('set.saved'), okText: t('node.ack'), notice: true });
    saveHA();
  } catch (e) {
    toast(e.message, 'err');
    const b = document.querySelector('[data-act="set.saveAll"]');
    if (b) b.disabled = false;
  }
}

// 一项设置一行:左边名称 + 说明,右边控件(开关靠右);长文本框(公告、模板)名称在上、框在下占满。
// 控件的 id 仍是 set-<键>,保存逻辑不用跟着变
// 宽屏两列时也各占一整行的:功德箱 / 在线设备数和流量奖励这两个「要不要为它联网」的开关,一上一下放在一起(用户 N15);
// 页面伪装的总开关说明长,占一整行;下面「伪装成」和 80 / 443 两项并排,选了跳转时网址那一行占整行、排在它们后面(见 style.css .camo-rd-row)
const WIDE = new Set(['supportOnline', 'rewardEnabled', 'camoEnabled', 'camoRedirect']);

function rowHTML(s, [k, label, type, help, options]) {
  const id = 'set-' + k;
  const info = `<div class="set-info"><label class="set-label" for="${id}">${esc(label)}</label>${help ? `<p class="set-help xp">${esc(help)}</p>` : ''}</div>`;
  if (type === 'bool' || type === 'boolOn') {
    const on = boolSetting(s[k], k === 'subPageEnabled' || type === 'boolOn');
    return `<div class="set-row bool${WIDE.has(k) ? ' wide' : ''}">${info}<div class="set-ctl"><label class="switch"><input type="checkbox" id="${id}" ${on ? 'checked' : ''}><span></span></label></div></div>`;
  }

  if (type === 'mutes') { // 概况提醒:分三小块的一组勾选框(勾着 = 提醒),值存在隐藏的输入框里给保存条用
    const v = normMutes(s[k]), off = new Set(v.split(',').filter(Boolean));
    const cats = attnCatsFor(isNodeRole());
    const blocks = ATTN_GROUPS.map(g => {
      const list = cats.filter(c => c.group === g);
      return list.length ? `<div class="mute-g"><div class="mute-h">${esc(t('attn.g.' + g))}</div>${list.map(c =>
        `<label class="mute-cb"><input type="checkbox" data-mute="${c.id}" ${off.has(c.id) ? '' : 'checked'}><span>${esc(t('attn.cat.' + c.id))}</span></label>`).join('')}</div>` : '';
    }).join('');
    return `<div class="set-row stack">${info}<div class="set-ctl"><input type="hidden" id="${id}" value="${esc(v)}" data-orig="${esc(v)}"><div class="mutes" data-for="${id}">${blocks}</div></div></div>`;
  }
  if (type === 'textarea' || type === 'code') {
    return `<div class="set-row stack">${info}<div class="set-ctl"><textarea id="${id}" ${type === 'code' ? 'class="code-area" spellcheck="false" autocomplete="off"' : ''}>${esc(s[k] ?? '')}</textarea>${textMax[k] ? charCount(id, textMax[k]) : ''}</div></div>`;
  }
  const ctl = type === 'select'
    ? `<select id="${id}">${options.map(([v, l]) => `<option value="${esc(v)}" ${String(s[k] ?? '') === v ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select>`
    : `<input id="${id}" type="${type}" value="${esc(s[k] ?? '')}" placeholder="${esc(placeholders[k] ?? defaults[k] ?? '')}">${textMax[k] ? charCount(id, textMax[k]) : ''}`;
  return `<div class="set-row${WIDE.has(k) ? ' wide' : ''}${k === 'camoRedirect' ? ' camo-rd-row' : ''}">${info}<div class="set-ctl">${ctl}</div></div>`;
}

// 左侧分区导航:按页面上现有的分区卡片生成(容灾、配对信息是后插进来的,插完再调一次)。
// 点了平滑滚过去;滚动时哪张卡在视口上部,哪一项高亮
let navSpy = null;
function buildNav() {
  const nav = document.getElementById('set-nav');
  if (!nav) return;
  const secs = [...document.querySelectorAll('.set-main > section.card')].filter(s => s.id && s.dataset.nav);
  // 组名:组变了就先写一行组名;没有组的(关于)前面画一条细线
  nav.innerHTML = secs.map((s, i) => {
    const g = s.dataset.navg || '', prev = i ? secs[i - 1].dataset.navg || '' : '';
    const head = g && g !== prev ? `<div class="set-nav-g">${esc(t('set.g.' + g))}</div>` : !g && prev ? '<div class="set-nav-sep"></div>' : '';
    return head + `<a href="#/settings" data-sec="${esc(s.id)}">${esc(s.dataset.nav)}</a>`;
  }).join('');
  nav.onclick = e => {
    const a = e.target.closest('a[data-sec]');
    if (!a) return;
    e.preventDefault();
    document.getElementById(a.dataset.sec)?.scrollIntoView({ block: 'start', behavior: 'smooth' });
  };
  if (navSpy) navSpy.disconnect();
  if (typeof IntersectionObserver === 'undefined') return;
  const mark = id => nav.querySelectorAll('a').forEach(a => a.classList.toggle('active', a.dataset.sec === id));
  const seen = new Set();
  navSpy = new IntersectionObserver(es => {
    es.forEach(en => (en.isIntersecting ? seen.add(en.target.id) : seen.delete(en.target.id)));
    const first = secs.find(s => seen.has(s.id));
    if (first) mark(first.id);
  }, { rootMargin: '-15% 0px -55% 0px' });
  secs.forEach(s => navSpy.observe(s));
  if (secs[0]) mark(secs[0].id);
}

// ---- 页面伪装(用户 N2):这台伪装站的预览、换一个样子、开启后在哪些地方显示 ----
// 预览按 1024 宽的桌面浏览器排版,再整体缩小放进框里(宽屏时框和右边一栏一样高,露出的页面多一些);
// 框里的页面不能点(链接也在后端拿掉了),点框放大看。
// 种子每台各自生成:这里看到的只是这台的样子,副机各是各的(设置随主机同步)。
const CAMO_TEMPLATES = ['mirror', 'downloads', 'video', 'blog'];
const CAMO_W = 1024;
let camoSeq = 0, camoData = null, camoRO = null, camoTimer = 0, camoPollFrom = 0;
const camoSaved = k => boolSetting(state.settings[k], false);

async function renderCamo() {
  const box = document.getElementById('camo-box');
  if (!box) return;
  const seq = ++camoSeq;
  clearTimeout(camoTimer);
  const sel = document.getElementById('set-camoTemplate');
  let d;
  try { d = await get('camo?template=' + encodeURIComponent(sel ? sel.value : '') + '&redirect=' + encodeURIComponent(fv('set-camoRedirect').trim())); }
  catch (e) { if (seq === camoSeq && box.isConnected) box.innerHTML = `<p class="hint danger-text">${esc(e.message)}</p>`; return; }
  if (seq !== camoSeq || !box.isConnected) return;
  camoData = d;
  box.innerHTML = camoHTML(d);
  fitCamo(box);
  // 封面照片正在后台取、处理:过两三秒再取一次预览(处理好就换成照片);
  // 刚存下「也放在 80 / 443」:端口每半分钟对一次账,过一会儿再看一眼结果
  // 最多看一分钟(GitHub 很慢时不一直每 2.5 秒传一份预览;之后照片到了,下次打开设置页就是照片)
  const ph = d.photos, loading = ph && ph.loading && ph.have < ph.want;
  if (!loading) camoPollFrom = 0;
  else if (!camoPollFrom) camoPollFrom = Date.now();
  if (loading && Date.now() - camoPollFrom < 60000) camoTimer = setTimeout(renderCamo, 2500);
  else if (camoSaved('camoEnabled') && camoSaved('camoPorts') && !(d.ports || []).length) camoTimer = setTimeout(renderCamo, 32000);
}

// 封面照片那一行(视频站、博客、部分软件下载页;没用照片的站不显示):处理好了 / 正在取 / 没取全(悬停看原因)/
// 页面伪装没开(off:没开时不为预览去连 GitHub,开启后才取 —— 自审 G3)
function camoPhotoLine(p) {
  if (!p || !p.want) return '';
  let k = 'ok', s = t('camo.ph.ready', { n: p.want }), tip = '';
  if (p.have < p.want) {
    const at = p.retry ? new Date(p.retry * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';
    if (p.off) { k = 'off'; s = t('camo.ph.off', { n: p.want }); }
    else if (p.error && !p.loading) { k = 'warn'; s = t(at ? 'camo.ph.failedAt' : 'camo.ph.failed', { have: p.have, n: p.want, time: at }); tip = p.error; }
    else { k = 'wait'; s = t('camo.ph.loading', { have: p.have, n: p.want }); }
  }
  return `<div class="camo-ph ${k}"${tip ? ` title="${esc(tip)}"` : ''}>${esc(s)}</div>`;
}


// 「跳转到」那一行只在选了「跳转到我的网站」时显示(存着的网址不动,换回伪装站时也还在)
function camoMode() {
  const sel = document.getElementById('set-camoTemplate');
  const row = document.getElementById('set-camoRedirect')?.closest('.set-row');
  if (row) row.hidden = !sel || sel.value !== 'redirect';
}

// 浏览器窗口的样子:三个圆点 + 地址栏(订阅域名、面板域名、公网 IP 里先有的那个;都没有写站名)
const camoBar = d => `<span class="camo-bar" aria-hidden="true"><i></i><i></i><i></i><span class="camo-url">${esc(d.host || d.name)}</span></span>`;
const camoFrame = (d, cls) => `<iframe class="${cls}" sandbox="" referrerpolicy="no-referrer" title="${esc(d.name)}" srcdoc="${esc(d.html)}"${cls === 'camo-thumb' ? ' tabindex="-1" aria-hidden="true"' : ''}></iframe>`;

// 「开启后在这些地方显示」:三行 —— 端口、线路、80 / 443;值用小标签,状况用颜色点
function camoWhereHTML(d) {
  const e = d.entries || {};
  const tag = (s, k = 'ok', tip = '') => `<span class="camo-tag ${k}"${tip ? ` title="${esc(tip)}"` : ''}>${esc(s)}</span>`;
  const ports = [tag(t('camo.p.panel', { n: e.panel })), tag(t('camo.p.sub', { n: e.sub }))];
  if (e.reseller) ports.push(tag(t('camo.p.reseller', { n: e.reseller })));
  const lines = [];
  if (e.anytls) lines.push(tag(t('camo.l.anytls', { n: e.anytls })));
  if (e.trojan) lines.push(tag(t('camo.l.trojan', { n: e.trojan })));
  if (e.hy2) lines.push(tag(t('camo.l.hy2', { n: e.hy2 })));
  const linesNote = lines.length ? t('camo.k.linesNote') + (e.hy2Own ? t('node.sep') + t('camo.l.hy2Own', { n: e.hy2Own }) : '') : t('camo.l.none');
  if (!lines.length) lines.push(tag(t('camo.l.zero'), 'off'));
  const web = camoPortRows(d).map(([k, s, tip]) => tag(s, k, tip));
  const row = (k, vals, note) => `<dt>${esc(t(k))}</dt><dd><div class="camo-tags">${vals.join('')}</div>${note ? `<small>${esc(note)}</small>` : ''}</dd>`;
  return `<h4 class="camo-wh">${esc(t('camo.where'))}</h4>
    <dl class="camo-dl">${row('camo.k.ports', ports, t('camo.k.portsNote'))}${row('camo.k.lines', lines, linesNote)}${row('camo.k.web', web, '')}</dl>`;
}

function camoHTML(d) {
  const on = camoSaved('camoEnabled');
  const rd = d.mode === 'redirect';
  const kind = rd ? '' : t('set.camoT.' + d.template) + (d.auto ? ' · ' + t('camo.autoPicked') : '');
  // 跳转模式:左边画「有人打开 → 302 → 你的网站」,右边是网址和打开它的链接;伪装站模式:左边缩略图,右边站名和两个按钮
  const left = rd
    ? `<div class="camo-win camo-rd">${camoBar(d)}<div class="camo-rdbody">
        <div class="camo-rdrow"><span class="camo-rdk">${esc(t('camo.rd.req'))}</span><code>${esc((d.host || '…') + '/…')}</code></div>
        <div class="camo-rdarrow"><i class="i i-arrow"></i>302</div>
        <div class="camo-rdrow"><span class="camo-rdk">${esc(t('camo.rd.to'))}</span><code class="${d.target ? '' : 'muted'}">${esc(d.target || t('camo.rd.none'))}</code></div>
      </div></div>`
    : `<div class="camo-win">${camoBar(d)}<span class="camo-shot">${camoFrame(d, 'camo-thumb')}</span>
        <button type="button" class="camo-hit" data-act="set.camoZoom" title="${esc(t('camo.zoom'))}" aria-label="${esc(t('camo.zoom') + ' · ' + d.name)}"></button></div>`;
  const top = rd
    ? `<div class="camo-name">${esc(t('camo.rd.title'))}</div>
        ${d.error ? `<div class="camo-kind danger-text">${esc(trErr(d.error))}</div>` : `<div class="camo-kind camo-target">${esc(d.target || t('camo.rd.none'))}</div>`}
        ${d.target ? `<div class="camo-acts"><a class="btn sm" href="${esc(d.target)}" target="_blank" rel="noopener noreferrer">${esc(t('camo.rd.open'))} ↗</a></div>` : ''}
        <p class="camo-note xp">${esc(t('camo.rd.note'))}</p>`
    : `<div class="camo-name">${esc(d.name)}</div>
        <div class="camo-kind">${esc(kind)}</div>
        ${camoPhotoLine(d.photos)}
        <div class="camo-acts">
          <button type="button" class="btn sm" data-act="set.camoReseed"><i class="i i-refresh"></i>${esc(t('camo.reseed'))}</button>
          <button type="button" class="btn sm" data-act="set.camoZoom">${esc(t('camo.zoom'))}</button>
          ${d.live ? `<a class="btn sm" href="${esc(d.live)}" target="_blank" rel="noopener noreferrer" title="${esc(t('camo.openHint'))}">${esc(t('camo.open'))} ↗</a>` : ''}
        </div>
        <p class="camo-note xp">${esc(t('camo.reseedHelp'))}</p>`;
  return `<div class="camo-head"><h3>${esc(t(rd ? 'camo.rd.head' : 'camo.preview'))}</h3>${badge(t(on ? 'camo.on' : 'camo.off'), on ? 'ok' : '')}</div>
    <div class="camo-grid">
      ${left}
      <div class="camo-info">
        ${top}
        ${camoWhereHTML(d)}
      </div>
    </div>
    <p class="camo-foot">${esc(t('camo.w.others'))}</p>`;
}

// 80 / 443 这一项:按存下的设置说;开着的话逐个端口写现在的状况,没开的写原因(别的程序占着时悬停看原文)
function camoPortRows(d) {
  if (!camoSaved('camoEnabled') || !camoSaved('camoPorts')) return [['off', t('camo.s.off')]];
  const ports = d.ports || [];
  if (!ports.length) return [['wait', t('camo.s.pending')]];
  const on443 = ports.some(p => p.on && p.port === 443);
  return ports.map(p => (p.on
    ? ['ok', t(d.mode === 'redirect' ? 'camo.s.onRd' : p.port === 80 && on443 ? 'camo.s.on80s' : 'camo.s.on', { port: p.port })]
    : ['warn', t('camo.s.no', { port: p.port, why: t('camo.why.' + p.why) }), p.detail || '']));
}

// 缩略图跟着框的宽度缩放
function fitCamo(box) {
  const shot = box.querySelector('.camo-shot');
  if (!shot) return;
  const fit = () => shot.style.setProperty('--camo-s', String(shot.clientWidth / CAMO_W));
  fit();
  if (camoRO) camoRO.disconnect();
  camoRO = null;
  if (typeof ResizeObserver === 'undefined') return;
  camoRO = new ResizeObserver(() => {
    if (!shot.isConnected) { camoRO.disconnect(); camoRO = null; return; }
    fit();
  });
  camoRO.observe(shot);
}

// 副机:展示配对信息(API 地址 + 令牌),复制到主机的"服务器"页即可接入
async function renderPairing() {
  const info = await get('agent/info').catch(() => null);
  if (!info) return;
  const role = document.getElementById('sec-role');
  if (!role) return;
  const card = document.createElement('section');
  card.className = 'card set-sec';
  card.id = 'sec-pairing';
  card.dataset.nav = t('set.pairing');
  card.dataset.navg = 'multi';
  card.innerHTML = `
    <div class="card-head"><h2>${t('set.pairing')}</h2><button class="btn sm" data-act="set.rotateToken">${t('set.rotateToken')}</button></div>
    <p class="hint xp">${t('set.pairingHelp')}</p>
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
  'set.saveAll': () => saveAll(),
  'set.barToggle': () => { barOpen = !barOpen; renderBar(); },
  'set.undo': k => { undoOne(k); renderBar(); },
  'set.discard': () => { changes().forEach(c => undoOne(c.k)); barOpen = false; renderBar(); },
  'set.tmplDefault': async id => {
    const box = document.getElementById(id === 'clash' ? 'set-subClashExt' : 'set-subSingboxExt');
    if (box.value.trim() && !await confirm(t('set.tmplOverwrite'))) return;
    try {
      const r = await get('sub-templates');
      box.value = r[id];
      box.dispatchEvent(new Event('input', { bubbles: true })); // 算一处改动:要点保存才生效
      box.focus();
    } catch (e) { toast(e.message, 'err'); }
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
  'set.camoReseed': async (_, btn) => {
    btn.disabled = true;
    try { await post('camo'); await renderCamo(); toast(t('camo.reseeded'), 'ok'); }
    catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  },
  'set.camoZoom': () => {
    const d = camoData;
    if (!d || d.mode === 'redirect') return;
    openModal(t('camo.previewTitle') + ' · ' + d.name, `<div class="camo-zoom">${camoBar(d)}${camoFrame(d, 'camo-full')}</div><p class="hint camo-zoom-hint xp">${esc(t('camo.zoomHint'))}</p>`,
      null, { size: 'xl', cancelText: t('common.close') });
  },
  'set.copyToken': () => copy(document.getElementById('pair-token').textContent),
  'set.rotateToken': async () => {
    if (!await confirm(t('set.rotateConfirm'), { danger: true })) return;
    try { const r = await post('agent/rotate'); document.getElementById('pair-token').textContent = r.token; toast(t('set.rotated'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'set.notifyTest': async (_, btn) => {
    // 测试用的是已经保存的设置:这一组有没保存的改动时先提醒,免得以为测的是刚填的
    if (dirtyFields().some(f => f.gid === 'notify')) { toast(t('set.sb.saveFirst'), 'warn'); return; }
    btn.disabled = true;
    try { await post('notify/test'); toast(t('set.tgTestOk'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
});

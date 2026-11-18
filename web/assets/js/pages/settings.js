import { state, load } from '../app.js';
import { get, post } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, registerActions, badge, fv, fchk, copy, urlHost, charCount, bindCharCounts, PAGE_TEXT_MAX } from '../ui.js';
import { ICONS } from '../icons.js';
import { followPanel } from '../panelmove.js';
import { logoBlock, bindLogo } from '../logo.js';
import { renderHASettings, saveHASettings } from '../ha.js';
import { subSettingsChange } from '../subtls.js';
import { refreshReward } from '../reward.js';
import { ATTN_GROUPS, attnCatsFor, normMutes } from '../attnmute.js';

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
    <section class="card set-sec" id="sec-role" data-nav="${esc(t('set.role'))}">
      <div class="card-head"><h2>${t('set.role')} ${badge(isNode ? t('role.node') : t('role.master'), isNode ? 'node' : 'primary')}</h2></div>
      <div class="set-rows">
        <div class="set-row bool"><div class="set-info"><label class="set-label" for="set-nodeMode">${t('set.roleSwitch')}</label><p class="set-help" id="role-hint">${isNode ? t('set.roleNode') : t('set.roleMaster')}</p></div>
          <div class="set-ctl"><label class="switch"><input type="checkbox" id="set-nodeMode" ${isNode ? 'checked' : ''}><span></span></label></div></div>
      </div>
    </section>
    ${groups().filter(g => !(g.masterOnly && isNode)).map(g => `
      <section class="card set-sec" id="sec-${g.id}" data-nav="${esc(g.title)}">
        <div class="card-head"><h2>${esc(g.title)}</h2>${g.test ? `<div class="row"><button class="btn sm" data-act="set.notifyTest">${t('set.tgTest')}</button></div>` : ''}</div>
        <div class="set-rows">${g.fields.filter(f => !(f[5] && isNode)).map(f => rowHTML(s, f)).join('')}</div>${g.extra || ''}
      </section>`).join('')}
    <section class="card set-sec" id="sec-about" data-nav="${esc(t('set.about'))}">
      <div class="card-head"><h2>${t('set.about')}</h2><a href="#/admin" class="btn sm">${t('nav.admin')} →</a></div>
      <dl class="kv">
        <dt>${t('set.version')}</dt><dd class="mono">${esc(state.status.version || '')}</dd>
        <dt>sing-box</dt><dd class="mono">${esc(state.status.singBox || '—')}</dd>
        <dt>${t('set.panelUrl')}</dt><dd class="mono">${esc((state.status.panelTLS ? 'https' : 'http') + '://' + urlHost(state.status.panelTLS ? 'https' : 'http', s.webDomain || '<IP>', state.status.webPort || '') + (state.status.webPath || '/'))}</dd>
        <dt>${t('set.subUrl')}</dt><dd class="mono">${esc((state.status.subTLS ? 'https' : 'http') + '://' + urlHost(state.status.subTLS ? 'https' : 'http', s.subDomain || s.webDomain || '<IP>', state.status.subPort || '') + (state.status.subPath || '/sub/') + '<' + t('set.userPh') + '>')}</dd>
        <dt>License</dt><dd>GPL-3.0 · <a href="${esc(state.status.repo || '#')}" target="_blank" rel="noopener">GitHub</a></dd>
      </dl>
      <p class="hint">${t('set.restartNote')}</p>
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
  bindLogo('set-logo', 'sub-logo');
  bindCharCounts(el);
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
  document.querySelector('section.card').after(haCard);
  buildNav();
  renderHASettings(haCard).then(() => {
    buildNav(); // 取不到容灾状态时这张卡会被拿掉,导航跟着少一项
    haSnapshot(); // 容灾卡片画好了:记下它各项现在的值(之后的改动进保存条)
    let to = '';
    try { to = sessionStorage.getItem('m-ui.scrollTo') || ''; sessionStorage.removeItem('m-ui.scrollTo'); } catch {}
    if (to === 'ha-card') haCard.scrollIntoView({ block: 'start' });
    else if (to.startsWith('set-')) focusRow(to);
  });
}

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
function renderBar() {
  const bar = document.getElementById('set-bar');
  if (!bar) return;
  const list = changes();
  bar.hidden = !list.length;
  if (!list.length) { barOpen = false; bar.innerHTML = ''; return; }
  const keep = bar.querySelector('.sb-list') ? bar.querySelector('.sb-list').scrollTop : 0;
  bar.innerHTML = `
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
  const norm = p => { p = (p || '/app/').trim(); if (!p.startsWith('/')) p = '/' + p; if (!p.endsWith('/')) p += '/'; return p; };
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
      setTimeout(() => { location.href = np + '#/settings'; }, 900);
      return;
    }
    await load('settings', 'status');
    toast(t('set.saved') + (r && r.note ? ' · ' + r.note : ''), 'ok');
    if ('rewardEnabled' in body) refreshReward(); // 顶栏的金币跟着开关走
    // Clash 模板写了 ipv6: false:存下了,但有 IPv6 节点时订阅会改成 true,要让人知道
    if (((r && r.hints) || []).includes('clashIPv6')) await confirm(t('set.clashIPv6Hint'), { title: t('set.saved'), okText: t('node.ack'), notice: true });
    saveHA();
  } catch (e) {
    toast(e.message, 'err');
    const b = document.querySelector('[data-act="set.saveAll"]');
    if (b) b.disabled = false;
  }
}

// 一项设置一行:左边名称 + 说明,右边控件(开关靠右);长文本框(公告、模板)名称在上、框在下占满。
// 控件的 id 仍是 set-<键>,保存逻辑不用跟着变
// 宽屏两列时也各占一整行的:功德箱 / 在线设备数和流量奖励这两个「要不要为它联网」的开关,一上一下放在一起(用户 N15)
const WIDE = new Set(['supportOnline', 'rewardEnabled']);

function rowHTML(s, [k, label, type, help, options]) {
  const id = 'set-' + k;
  const info = `<div class="set-info"><label class="set-label" for="${id}">${esc(label)}</label>${help ? `<p class="set-help">${esc(help)}</p>` : ''}</div>`;
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
    : `<input id="${id}" type="${type}" value="${esc(s[k] ?? '')}" placeholder="${esc(defaults[k] ?? '')}">${textMax[k] ? charCount(id, textMax[k]) : ''}`;
  return `<div class="set-row">${info}<div class="set-ctl">${ctl}</div></div>`;
}

// 左侧分区导航:按页面上现有的分区卡片生成(容灾、配对信息是后插进来的,插完再调一次)。
// 点了平滑滚过去;滚动时哪张卡在视口上部,哪一项高亮
let navSpy = null;
function buildNav() {
  const nav = document.getElementById('set-nav');
  if (!nav) return;
  const secs = [...document.querySelectorAll('.set-main > section.card')].filter(s => s.id && s.dataset.nav);
  nav.innerHTML = secs.map(s => `<a href="#/settings" data-sec="${esc(s.id)}">${esc(s.dataset.nav)}</a>`).join('');
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

// 副机:展示配对信息(API 地址 + 令牌),复制到主机的"服务器"页即可接入
async function renderPairing() {
  const info = await get('agent/info').catch(() => null);
  if (!info) return;
  const role = document.querySelector('section.card');
  const card = document.createElement('section');
  card.className = 'card set-sec';
  card.id = 'sec-pairing';
  card.dataset.nav = t('set.pairing');
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

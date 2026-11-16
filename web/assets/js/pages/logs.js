// 日志页:三种日志各有各的用处,各排各的。
//   订阅访问 —— 排查「某个人订阅更新不了」「有没有人在扫订阅地址」:一行一次访问,客户端写成认得出的名字(Clash Meta 2.11 · Android),
//     内容和结果用标签(成功 / 没找到 / 出错 / 地址不对 ×N),可以按用户搜、只看失败的。以前状态是个光秃秃的数字、格式是 page / link 这类内部词、客户端是一整串 User-Agent。
//   核心日志 —— 排查连不上:一屏滚动的原始日志,上面一排是「记不记」「记多细」(存下来的设置),和「现在只看哪些」(级别、关键字,只影响这一屏)分开放;
//     以前两个没有标签的下拉框挨在一起,分不清哪个是设置、哪个是筛选。可以打开自动刷新盯着看。
//   操作审计 —— 查「谁在什么时候改了什么」:对象和动作译成界面上的话(用户 · 批量停用),可以只看人做的、按对象筛。以前是 user / batch:disable 这样的原词。
import { state, load, takeHashParams } from '../app.js';
import { get, post, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtDate, fmtRelative, registerActions, empty, debounce, confirm, toast, auditText, badge, setHTML } from '../ui.js';
import { auditAction, auditObject, JOB_ACTORS } from '../audit.js';

export const title = () => t('logs.title');
export const subtitle = () => t('logs.subtitle');
let tab = 'sub', userFilter = '', subOnly = 'all', level = 'info', coreQ = '', coreLive = false, auditScope = 'all', auditKey = '';
let subRows = [], coreLines = [], auditRows = [];

const logOn = () => String(state.settings.logEnabled ?? 'true') !== 'false';
const coreLevel = () => ['warn', 'info', 'debug', 'error'].includes(state.settings.coreLogLevel) ? state.settings.coreLogLevel : 'warn';
const clearBtn = () => `<button class="btn sm danger" data-act="logs.clear">${t('logs.clear')}</button>`;
const seg = (id, act, cur, opts) => `<div class="seg sm" id="${id}">${opts.map(([k, v]) => `<button type="button" data-act="${act}" data-id="${k}" class="${k === cur ? 'active' : ''}">${esc(v)}</button>`).join('')}</div>`;

// 自动清理:订阅访问日志与审计日志存在数据库里,会一直累积,给它们各自一个保留天数。
// 空 = 保持原来的行为(订阅跟随「流量记录保留」,审计不清理),不会因为上线这个功能就删掉已有记录。
const RETENTION = { sub: 'subLogAge', audit: 'auditAge' };
function retentionSelect(kind) {
  const key = RETENTION[kind];
  if (!key) return '';
  const cur = String(state.settings[key] ?? '');
  const opts = [
    ['', kind === 'sub' ? t('logs.keepFollow') : t('logs.keepNever')],
    ['1', t('logs.keep1d')], ['7', t('logs.keep7d')], ['30', t('logs.keep30d')],
  ];
  if (kind === 'sub') opts.push(['0', t('logs.keepNever')]);
  return `<label class="lg-opt">${t('logs.keep')}
    <select class="sm" id="logs-keep">${opts.map(([v, label]) => `<option value="${v}" ${v === cur ? 'selected' : ''}>${esc(label)}</option>`).join('')}</select></label>`;
}
function bindRetention(kind) {
  const sel = document.getElementById('logs-keep');
  if (!sel) return;
  sel.addEventListener('change', async e => {
    const v = e.target.value;
    try { await post('settings', { [RETENTION[kind]]: v }); await load('settings'); toast(t('logs.keepSaved'), 'ok'); }
    catch (err) { toast(err.message, 'err'); }
  });
}

// ---- 订阅访问 ----
// clientOf 把 User-Agent 认成客户端的名字和版本;认不出的原样返回(截短在样式里做)
const CLIENTS = [
  [/godusevpn[/ ]?([\d.]+)?/i, '佛跳墙'], [/ClashMetaForAndroid[/ ]?([\d.]+)?/i, 'Clash Meta', 'Android'], [/clash[- ]?verge[/ ]?v?([\d.]+)?/i, 'Clash Verge'],
  [/FlClash[/ ]?v?([\d.]+)?/i, 'FlClash'], [/mihomo[/ ]?v?([\d.]+)?/i, 'mihomo'], [/Stash[/ ]?([\d.]+)?/i, 'Stash'], [/Shadowrocket[/ ]?([\d.]+)?/i, 'Shadowrocket'],
  [/Quantumult%20X[/ ]?([\d.]+)?/i, 'Quantumult X'], [/Loon[/ ]?([\d.]+)?/i, 'Loon'], [/Surge[/ ]?([\d.]+)?/i, 'Surge'], [/Hiddify[/ ]?v?([\d.]+)?/i, 'Hiddify'],
  [/v2rayNG[/ ]?([\d.]+)?/i, 'v2rayNG', 'Android'], [/v2rayN[/ ]?([\d.]+)?/i, 'v2rayN'], [/NekoBox[/ ]?([\d.]+)?/i, 'NekoBox'], [/Karing[/ ]?([\d.]+)?/i, 'Karing'],
  [/\bSFA[/ ]?([\d.]+)?/, 'sing-box', 'Android'], [/\bSFI[/ ]?([\d.]+)?/, 'sing-box', 'iPhone'], [/\bSFM[/ ]?([\d.]+)?/, 'sing-box', 'macOS'], [/sing-box[/ ]?([\d.]+)?/i, 'sing-box'],
  [/clash/i, 'Clash'], [/curl[/ ]?([\d.]+)?/i, 'curl'], [/Wget[/ ]?([\d.]+)?/i, 'wget'], [/Go-http-client[/ ]?([\d.]+)?/i, 'Go'], [/python-requests[/ ]?([\d.]+)?/i, 'Python'],
];
// Darwin 不当成 macOS:iPhone 上的客户端(CFNetwork/… Darwin/…)也带这个词
const OSES = [[/iPhone|iOS/i, 'iPhone'], [/iPad/i, 'iPad'], [/Android/i, 'Android'], [/Windows/i, 'Windows'], [/Mac OS X|Macintosh|macOS/i, 'macOS'], [/Linux/i, 'Linux']];
const BROWSERS = [[/Edg\/([\d]+)/, 'Edge'], [/Firefox\/([\d]+)/, 'Firefox'], [/Chrome\/([\d]+)/, 'Chrome'], [/Version\/([\d]+).*Safari/, 'Safari']];
export function clientOf(ua) {
  ua = String(ua || '');
  if (!ua) return { name: '—', extra: '' };
  const os = (OSES.find(([re]) => re.test(ua)) || [])[1] || '';
  for (const [re, name, fixedOs] of CLIENTS) {
    const m = re.exec(ua);
    if (m) return { name: name + (m[1] ? ' ' + m[1].replace(/\.$/, '') : ''), extra: fixedOs || os };
  }
  if (/Mozilla\//.test(ua)) {
    for (const [re, name] of BROWSERS) { const m = re.exec(ua); if (m) return { name: `${name} ${m[1]}`, extra: os, browser: true }; }
  }
  return { name: ua, extra: '', raw: true };
}
const FORMATS = { link: 'logs.f.link', clash: 'logs.f.clash', json: 'logs.f.json', page: 'logs.f.page' };
function formatCell(f) {
  f = String(f || '');
  if (f === 'miss') return `<span class="muted">—</span>`;
  const shared = f.endsWith('-share'), base = shared ? f.slice(0, -6) : f;
  return `${esc(FORMATS[base] ? t(FORMATS[base]) : base)}${shared ? ` ${badge(t('logs.f.share'))}` : ''}`;
}
const subFailed = l => l.status >= 400;
function resultCell(l) {
  if (l.format === 'miss') return badge(t('logs.r.miss'), 'danger');
  if (l.status >= 500) return badge(t('logs.r.error'), 'danger');
  if (l.status === 404) return badge(t('logs.r.none'), 'warn');
  if (l.status >= 400) return badge(String(l.status), 'warn');
  return badge(t('logs.r.ok'), 'ok');
}
function subRowsHTML() {
  const rows = subOnly === 'fail' ? subRows.filter(subFailed) : subRows;
  const cnt = document.getElementById('logs-count');
  if (cnt) cnt.textContent = t('logs.count', { n: rows.length }) + (subRows.length >= 300 ? ' · ' + t('logs.latestOnly', { n: 300 }) : '');
  if (!rows.length) return `<tr><td colspan="6">${empty(subOnly === 'fail' && subRows.length ? t('logs.noFail') : undefined)}</td></tr>`;
  return rows.map(l => {
    const miss = l.format === 'miss', c = miss ? null : clientOf(l.ua);
    // 地址不对的那几行(同一个 IP 的 404 合成一行):没有用户、没有客户端,原来塞在 UA 里的「404 ×N(多久内,最近 xxx)」当说明
    const client = miss ? `<span class="lg-ua" title="${esc(l.ua)}">${esc(l.ua)}</span>`
      : `<span class="lg-client${c.raw ? ' raw' : ''}" title="${esc(l.ua)}">${esc(c.name)}</span>${c.extra ? ` <span class="muted small">${esc(c.extra)}</span>` : ''}`;
    return `<tr>
      <td class="num lg-time" title="${esc(fmtRelative(l.ts))}">${fmtDate(l.ts)}</td>
      <td>${miss || l.user === '-' ? '<span class="muted">—</span>' : `<a href="#/users?q=${encodeURIComponent(l.user)}">${esc(l.user)}</a>`}</td>
      <td class="num">${esc(l.ip)}</td>
      <td class="lg-cl">${client}</td>
      <td>${formatCell(l.format)}</td>
      <td>${resultCell(l)}</td></tr>`;
  }).join('');
}
async function loadSub() {
  subRows = (await get('sublogs?limit=300' + (userFilter ? '&user=' + encodeURIComponent(userFilter) : ''))) || [];
  setHTML('logs-rows', subRowsHTML());
}

// ---- 核心日志 ----
function coreHTML() {
  const q = coreQ.trim().toLowerCase();
  const lines = q ? coreLines.filter(l => l.toLowerCase().includes(q)) : coreLines;
  const cnt = document.getElementById('logs-count');
  if (cnt) cnt.textContent = q ? t('logs.matched', { n: lines.length, m: coreLines.length }) : t('logs.lines', { n: coreLines.length });
  return lines.map(l => {
    const cls = /ERROR/.test(l) ? 'error' : /WARN/.test(l) ? 'warn' : /DEBUG/.test(l) ? 'debug' : '';
    return `<span class="${cls}">${esc(l)}</span>`;
  }).join('\n') || esc(q ? t('logs.noMatch') : t('common.empty'));
}
async function loadCore() {
  coreLines = ((await get(`logs?count=500&level=${level}`)) || []).slice().reverse(); // 顺序和以前一样:接口给的倒过来排
  const el = document.getElementById('logs-core');
  if (el) el.innerHTML = coreHTML();
}

// ---- 操作审计 ----
const AUDIT_KEYS = ['user', 'line', 'upstream', 'ext', 'node', 'plan', 'rule', 'reseller', 'cert', 'backup', 'settings', 'admin', 'ops', 'ha'];
function auditRowsHTML() {
  const cnt = document.getElementById('logs-count');
  if (cnt) cnt.textContent = t('logs.count', { n: auditRows.length }) + (auditRows.length >= 300 ? ' · ' + t('logs.latestOnly', { n: 300 }) : '');
  if (!auditRows.length) return `<tr><td colspan="5">${empty()}</td></tr>`;
  return auditRows.map(r => {
    // obj 是任意 JSON:字符串原样,对象 / 数组里的对象写成 JSON(以前显示成 [object Object]);日志页不截断
    const obj = String(auditText(r.obj, { max: 0 }));
    const job = JOB_ACTORS.includes(r.actor);
    return `<tr>
      <td class="num lg-time" title="${esc(fmtRelative(r.dateTime))}">${fmtDate(r.dateTime)}</td>
      <td>${esc(r.actor)}${job ? ` <span class="badge" title="${esc(t('logs.jobTitle'))}">${esc(t('logs.job'))}</span>` : ''}</td>
      <td title="${esc(r.key)}">${esc(auditObject(r.key))}</td>
      <td title="${esc(r.action)}">${esc(auditAction(r))}</td>
      <td class="lg-obj" title="${esc(obj)}">${esc(obj)}</td></tr>`;
  }).join('');
}
async function loadAudit() {
  const qs = ['limit=300'];
  if (auditScope === 'human') qs.push('actor=human');
  if (auditKey) qs.push('key=' + encodeURIComponent(auditKey));
  const rows = (await get('audit?' + qs.join('&'))) || [];
  auditRows = auditScope === 'human' ? rows.filter(r => !JOB_ACTORS.includes(r.actor)) : rows; // 老后端不认 actor 参数时也对
  setHTML('logs-rows', auditRowsHTML());
}

export async function render(el) {
  // 概况的「查看全部操作记录 / 完整日志」:#/logs?tab=audit|core
  const hp = takeHashParams('logs');
  if (['sub', 'core', 'audit'].includes(hp.get('tab'))) tab = hp.get('tab');
  el.innerHTML = `
    <div class="tabs">${['sub', 'core', 'audit'].map(k => `<button data-act="logs.tab" data-id="${k}" class="${k === tab ? 'active' : ''}">${t('logs.' + k)}</button>`).join('')}</div>
    <div id="logs-body"></div>`;
  await renderTab();
}

// 自动刷新只管核心日志那一屏(打开了才刷);别的两张表点「刷新」才重新取,免得正看着的行跳走
export async function tick() { if (tab === 'core' && coreLive && document.getElementById('logs-core')) await loadCore().catch(() => {}); }

async function renderTab() {
  const el = document.getElementById('logs-body');
  if (!el) return;
  document.querySelectorAll('.tabs button').forEach(b => b.classList.toggle('active', b.dataset.id === tab));
  if (tab === 'sub') {
    el.innerHTML = `
      <div class="toolbar lg-bar">
        <input type="search" id="logs-user" placeholder="${t('logs.filterUser')}" value="${esc(userFilter)}">
        ${seg('logs-only', 'logs.subOnly', subOnly, [['all', t('common.all')], ['fail', t('logs.onlyFail')]])}
        <span class="muted small" id="logs-count"></span>
        <span class="grow"></span>${retentionSelect('sub')}<button class="btn sm" data-act="logs.refresh">${t('common.refresh')}</button>${clearBtn()}
      </div>
      <div class="table-wrap fill"><table class="grid lg-rows lg-sub" data-fit="cards:760">
        <thead><tr><th>${t('logs.time')}</th><th>${t('logs.user')}</th><th>${t('logs.ip')}</th><th>${t('logs.client')}</th><th>${t('logs.format')}</th><th>${t('logs.result')}</th></tr></thead>
        <tbody id="logs-rows"></tbody></table></div>`;
    document.getElementById('logs-user').addEventListener('input', debounce(e => { userFilter = e.target.value.trim(); loadSub(); }));
    bindRetention('sub');
    await loadSub();
  } else if (tab === 'core') {
    el.innerHTML = `
      <div class="lg-core-set">
        <label class="lg-sw" title="${esc(t('logs.enabledHelp'))}"><span class="switch"><input type="checkbox" id="logs-on" ${logOn() ? 'checked' : ''}><span></span></span><span>${t('logs.enabled')}</span></label>
        <label class="lg-opt" title="${esc(t('logs.coreLevelHelp'))}">${t('logs.coreLevel')}<select class="sm" id="logs-core-level">${['warn', 'info', 'debug', 'error'].map(l => `<option ${l === coreLevel() ? 'selected' : ''}>${l}</option>`).join('')}</select></label>
        <span class="muted small lg-note">${esc(logOn() ? t('logs.coreMemHelp') : t('logs.enabledHelp'))}</span>
      </div>
      <div class="toolbar lg-bar">
        <input type="search" id="logs-q" placeholder="${esc(t('logs.coreFilter'))}" value="${esc(coreQ)}">
        <label class="lg-opt">${t('logs.show')}<select class="sm" id="logs-level">${['debug', 'info', 'warning', 'error'].map(l => `<option value="${l}" ${l === level ? 'selected' : ''}>${esc(t('logs.lv.' + l))}</option>`).join('')}</select></label>
        <span class="muted small" id="logs-count"></span>
        <span class="grow"></span>
        <label class="lg-sw"><span class="switch"><input type="checkbox" id="logs-live" ${coreLive ? 'checked' : ''}><span></span></span><span>${t('logs.live')}</span></label>
        <button class="btn sm" data-act="logs.refresh">${t('common.refresh')}</button>${clearBtn()}
      </div>
      <pre class="log lg-core" id="logs-core"></pre>`;
    document.getElementById('logs-level').addEventListener('change', e => { level = e.target.value; loadCore(); });
    document.getElementById('logs-q').addEventListener('input', debounce(e => { coreQ = e.target.value; const box = document.getElementById('logs-core'); if (box) box.innerHTML = coreHTML(); }, 120));
    document.getElementById('logs-live').addEventListener('change', e => { coreLive = e.target.checked; if (coreLive) loadCore(); });
    document.getElementById('logs-core-level').addEventListener('change', async e => {
      try { await post('logs', { enabled: String(logOn()), level: e.target.value }); await load('settings'); toast(t('logs.coreLevelSaved'), 'ok'); }
      catch (err) { toast(err.message, 'err'); e.target.value = coreLevel(); }
    });
    document.getElementById('logs-on').addEventListener('change', async e => {
      const on = e.target.checked;
      try { await post('logs', { enabled: String(on) }); await load('settings'); toast(t('logs.toggled'), 'ok'); await renderTab(); }
      catch (err) { toast(err.message, 'err'); e.target.checked = !on; }
    });
    await loadCore();
  } else {
    el.innerHTML = `
      <div class="toolbar lg-bar">
        ${seg('logs-scope', 'logs.auditScope', auditScope, [['all', t('common.all')], ['human', t('logs.onlyHuman')]])}
        <label class="lg-opt"><select class="sm" id="logs-key" aria-label="${esc(t('logs.object'))}"><option value="">${esc(t('logs.allObjects'))}</option>${AUDIT_KEYS.map(k => `<option value="${k}" ${k === auditKey ? 'selected' : ''}>${esc(auditObject(k))}</option>`).join('')}</select></label>
        <span class="muted small" id="logs-count"></span>
        <span class="grow"></span>${retentionSelect('audit')}<button class="btn sm" data-act="logs.refresh">${t('common.refresh')}</button>${clearBtn()}
      </div>
      <div class="table-wrap fill"><table class="grid lg-rows lg-audit" data-fit="cards:760">
        <thead><tr><th>${t('logs.time')}</th><th>${t('logs.actor')}</th><th>${t('logs.object')}</th><th>${t('logs.action')}</th><th>${t('common.details')}</th></tr></thead>
        <tbody id="logs-rows"></tbody></table></div>`;
    document.getElementById('logs-key').addEventListener('change', e => { auditKey = e.target.value; loadAudit(); });
    bindRetention('audit');
    await loadAudit();
  }
}

registerActions({
  'logs.tab': async id => { tab = id; await renderTab(); },
  'logs.refresh': async () => { await renderTab(); },
  'logs.subOnly': id => {
    subOnly = id;
    document.querySelectorAll('#logs-only button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    setHTML('logs-rows', subRowsHTML());
  },
  'logs.auditScope': id => {
    auditScope = id;
    document.querySelectorAll('#logs-scope button').forEach(b => b.classList.toggle('active', b.dataset.id === id));
    loadAudit();
  },
  'logs.clear': async () => {
    if (!await confirm(t('logs.clearConfirm'), { danger: true })) return;
    try { await del('logs?kind=' + tab); toast(t('logs.cleared'), 'ok'); await renderTab(); }
    catch (e) { toast(e.message, 'err'); }
  },
});

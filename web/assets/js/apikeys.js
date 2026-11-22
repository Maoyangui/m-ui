// 外部 API 卡片:总开关、接口地址、密钥列表(每把单独的权限与允许的来源)。管理员页与代理面板「我的账号」共用。
// 用法:页面里放一个空的 <section class="card" id="…">,调 mountApiCard({ el, prefix, url, enabled, toggle, reseller, repo })。
//   prefix  密钥接口的前缀:主面板 'admin/api',代理 'self/api'
//   url     外部 API 的地址前缀(给人复制)
//   toggle  async on => {}  打开 / 关闭外部 API(两个面板的接口不一样,由页面给)
// 升级前就有的那一把(「默认密钥」)和别的密钥在同一张表里,一样能改名、改权限、限来源、停用、重新生成、删除。
import { get, post, put, del } from './api.js';
import { t } from './i18n.js';
import { esc, toast, confirm, openModal, registerActions, field, fieldBox, fv, copy, fmtTime, chipList, setHTML } from './ui.js';
import { openApiDocs } from './apidoc.js';
import { trErr } from './errmsg.js';

const PERMS = ['users.read', 'users.create', 'users.update', 'users.delete', 'billing', 'plans.read', 'panel.read'];
const READ_PERMS = ['users.read', 'plans.read', 'panel.read'];

let cfg = { el: '', prefix: '', url: '', enabled: false, reseller: false, repo: '', toggle: async () => {} };
let keys = [], you = '', loadErr = '';

const byId = id => keys.find(k => String(k.id) === String(id));
const nameOf = k => k.name || t('api.keyDefault');
const masked = tok => (tok && tok.length > 14 ? tok.slice(0, 6) + '…' + tok.slice(-4) : tok || '');
const ipItems = k => String(k.ipList || '').split(/[\s,]+/).filter(Boolean);

function permsCell(k) {
  const have = k.permList || [];
  if (have.length === PERMS.length) return `<span class="chip all">${esc(t('api.permAll'))}</span>`;
  return chipList(have.map(p => `<span class="chip">${esc(t('api.perm.' + p))}</span>`), { key: 'apikey-' + k.id, max: 3 });
}

// 允许的来源:不限 / 只有一个就直接写出来 / 多个写个数,悬停看全部
function fromCell(k) {
  const ips = ipItems(k);
  if (!k.ipAllow || !ips.length) return `<span class="muted">${esc(t('api.fromAny'))}</span>`;
  if (ips.length === 1) return `<code class="key-ip">${esc(ips[0])}</code>`;
  return `<span class="key-ipn" title="${esc(ips.join('\n'))}">${esc(t('api.fromN', { n: ips.length }))}</span>`;
}

function keysTable() {
  if (loadErr) return `<p class="hint bad">${esc(loadErr)}</p>`;
  if (!keys.length) return `<div class="api-empty"><p>${esc(t('api.keysNone'))}</p><button class="btn primary sm" data-act="apikey.add">${t('api.keyAdd')}</button></div>`;
  return `<div class="table-wrap"><table class="grid api-keys"><thead><tr>
      <th>${t('api.keyName')}</th><th>${t('api.keyPerms')}</th><th>${t('api.from')}</th><th>${t('api.keyLastUsed')}</th><th>${t('api.keyToken')}</th><th></th></tr></thead><tbody>
    ${keys.map(k => `<tr class="${k.enabled ? '' : 'off'}">
      <td class="primary-cell"><span class="pname" title="${esc(nameOf(k))}">${esc(nameOf(k))}</span></td>
      <td>${permsCell(k)}</td>
      <td>${fromCell(k)}</td>
      <td class="small">${k.lastUsedAt ? `${esc(fmtTime(k.lastUsedAt))}<div class="sub-cell muted">${esc(k.lastUsedIp || '')}</div>` : `<span class="muted">${esc(t('api.keyNever'))}</span>`}</td>
      <td><span class="key-tok"><code>${esc(masked(k.token))}</code><button class="btn xs" data-act="apikey.copy" data-id="${k.id}">${t('common.copy')}</button></span></td>
      <td class="actions">
        <label class="switch sm" title="${esc(t(k.enabled ? 'api.keyOnTip' : 'api.keyOffTip'))}"><input type="checkbox" data-change="apikey.toggle" data-id="${k.id}" ${k.enabled ? 'checked' : ''}><span></span></label>
        <button class="btn sm" data-act="apikey.edit" data-id="${k.id}">${t('common.edit')}</button>
        <button class="btn sm${k.logOff ? ' muted' : ''}" data-act="apikey.logs" data-id="${k.id}" title="${esc(t(k.logOff ? 'api.logOffTip' : 'api.logTip'))}">${t('api.log')}</button>
        <details class="menu"><summary class="btn sm">${t('common.more')}</summary><div class="menu-list">
          <button data-act="apikey.rotate" data-id="${k.id}">${t('api.keyRotate')}</button>
          <button class="danger" data-act="apikey.del" data-id="${k.id}">${t('common.delete')}</button>
        </div></details>
      </td></tr>`).join('')}
  </tbody></table></div>`;
}

function cardHTML() {
  const on = !!cfg.enabled;
  const head = `<div class="card-head"><h2>${t('adm.api')}</h2><label class="switch" title="${esc(t('adm.apiToggle'))}"><input type="checkbox" id="api-on" ${on ? 'checked' : ''}><span></span></label></div>
    <p class="hint xp">${esc(t(cfg.reseller ? 'acct.apiHelp' : 'adm.apiHelp'))}</p>`;
  // 没开时只留一句话和文档入口:先看清能做什么,再决定开不开
  if (!on) return `${head}<div class="api-off"><button class="btn sm" data-act="api.docs">${t('adm.apiDocsBtn')}</button><span class="muted small">${esc(t('api.offHint'))}</span></div>`;
  return `${head}
    <div class="api-url"><span class="lbl">${t('adm.apiBase')}</span><code>${esc(cfg.url)}</code>
      <button class="btn sm" data-act="api.copyUrl">${t('common.copy')}</button><button class="btn sm" data-act="api.docs">${t('adm.apiDocsBtn')}</button></div>
    <div class="api-keys-head"><h3>${t('api.keys')}${keys.length ? `<span class="n">${keys.length}</span>` : ''}</h3>${keys.length ? `<button class="btn primary sm" data-act="apikey.add">${t('api.keyAdd')}</button>` : ''}</div>
    ${keysTable()}
    <p class="hint xp">${esc(t('api.keysHelp'))}</p>`;
}

function draw() {
  setHTML(cfg.el, cardHTML());
  const sw = document.getElementById('api-on');
  if (sw && !sw.__bound) {
    sw.__bound = true;
    sw.addEventListener('change', async e => {
      const on = e.target.checked;
      try {
        await cfg.toggle(on);
        cfg.enabled = on;
        toast(t(on ? 'api.turnedOn' : 'api.turnedOff'), 'ok');
        if (on) await reload(); else draw();
      } catch (err) { toast(err.message, 'err'); e.target.checked = !on; }
    });
  }
}

async function reload() {
  try {
    const r = await get(cfg.prefix + '/keys');
    keys = r.keys || [];
    you = r.you || '';
    loadErr = '';
  } catch (e) { keys = []; loadErr = e.message; }
  draw();
}

export async function mountApiCard(opts) {
  cfg = { ...cfg, reseller: false, repo: '', ...opts };
  keys = []; you = ''; loadErr = '';
  if (cfg.enabled) await reload(); else draw();
}

// ---- 新建 / 编辑一把 ----
function keyForm(k) {
  const have = new Set(k ? k.permList : ['users.read']);
  const restrict = !!(k && k.ipAllow);
  const ips = k ? ipItems(k).join('\n') : '';
  const name = k && k.isDefault
    ? field(t('api.keyName'), `<input id="ak-name" maxlength="40" value="${esc(k.name || '')}" placeholder="${esc(t('api.keyDefault'))}">`, t('api.keyDefaultHelp'))
    : field(t('api.keyName'), `<input id="ak-name" maxlength="40" value="${esc(k ? k.name : '')}" placeholder="${esc(t('api.keyNamePh'))}">`);
  return `<div class="ak-form">
    ${name}
    ${k ? fieldBox(t('api.keyToken'), `<div class="sub-box ak-tok"><code>${esc(k.token)}</code><button type="button" class="btn sm" data-act="apikey.copy" data-id="${k.id}">${t('common.copy')}</button></div>`) : ''}
    <div class="ak-sec"><b>${t('api.keyPerms')}</b><span class="ak-quick"><button type="button" class="link-btn" data-act="apikey.pick" data-id="all">${t('api.pickAll')}</button><button type="button" class="link-btn" data-act="apikey.pick" data-id="read">${t('api.pickRead')}</button></span></div>
    <div class="perm-grid">${PERMS.map(p => `<label class="perm-item"><input type="checkbox" class="ak-perm" value="${p}" ${have.has(p) ? 'checked' : ''}>
      <span><b>${esc(t('api.perm.' + p))}</b><small class="muted xp">${esc(t('api.permHelp.' + p))}</small></span></label>`).join('')}</div>
    <div class="ak-sec"><b>${t('api.from')}</b></div>
    <div class="seg" id="ak-from"><button type="button" data-v="0" class="${restrict ? '' : 'active'}">${t('api.fromAny')}</button><button type="button" data-v="1" class="${restrict ? 'active' : ''}">${t('api.fromOnly')}</button></div>
    <div id="ak-ipbox" ${restrict ? '' : 'hidden'}>
      <textarea id="ak-ips" rows="3" spellcheck="false" placeholder="${esc(t('api.ipPlaceholder'))}">${esc(ips)}</textarea>
      ${you ? `<div class="ak-you"><span>${esc(t('api.ipYou', { ip: you }))}</span><button type="button" class="btn xs" data-act="apikey.addMe">${t('api.ipAddMe')}</button></div>` : ''}
      <p class="hint xp">${esc(t('api.fromHelp'))}</p>
    </div>
  </div>`;
}

const restricted = () => { const b = document.querySelector('#ak-from button.active'); return !!b && b.dataset.v === '1'; };

function formBody() {
  const body = { name: fv('ak-name').trim(), perms: [...document.querySelectorAll('.ak-perm:checked')].map(x => x.value), ipAllow: restricted(), ipList: fv('ak-ips') };
  // 这两条后端也会拒,在这里先说清楚,不用等一个来回
  if (!body.perms.length) throw new Error(t('api.needPerm'));
  if (body.ipAllow && !body.ipList.trim()) throw new Error(t('api.fromEmpty'));
  return body;
}

function editKey(k) {
  openModal(t(k ? 'api.keyEdit' : 'api.keyAdd'), keyForm(k), async () => {
    const body = formBody();
    if (k) { await put(`${cfg.prefix}/keys/${k.id}`, body); toast(t('api.keySaved'), 'ok'); }
    else { const nk = await post(cfg.prefix + '/keys', body); toast(t('api.keyCreated', { name: nk.name }), 'ok'); }
    await reload();
  });
  const seg = document.getElementById('ak-from');
  if (seg) seg.addEventListener('click', e => {
    const b = e.target.closest('button');
    if (!b) return;
    seg.querySelectorAll('button').forEach(x => x.classList.toggle('active', x === b));
    const box = document.getElementById('ak-ipbox');
    box.hidden = b.dataset.v !== '1';
    if (!box.hidden) document.getElementById('ak-ips').focus();
  });
}

// ---- 调用日志(用户 11-22:「对应的密钥行里增加一个日志 点击可显示查看调用日志(白话版看得懂的 鼠标悬浮显示具体执行指令
// 代理面板也一样) 可设置不开启日志 / 自动删除 / 一键删除」) ----
// 一次调用一行大白话(「新建用户 alice」「给 bob 续费」)、结果和失败原因;鼠标停在一行上看具体执行的请求(方法、路径、参数、请求体),
// 手机上没有悬停,点一下那一行在下面展开。上面一排是这把密钥的日志设置:记不记、自动删除几天前的、一键清空 —— 改了就存,不用再点保存。
const LOG_ACTIONS = new Set(['ping', 'overview', 'servers', 'lines', 'onlines', 'plans', 'users.list', 'user.create', 'user.lookup', 'users.batch',
  'user.get', 'user.update', 'user.delete', 'user.traffic', 'user.sub', 'user.planOps', 'user.enable', 'user.disable', 'user.reset', 'user.kick',
  'user.rotate', 'user.renew', 'user.change']);
let logKey = null, logItems = [], logMore = false, logTotal = 0, logCfg = { off: false, days: 7, choices: [1, 7, 30, 90] };

const logText = x => (LOG_ACTIONS.has(x.action) ? t('api.log.a.' + x.action, { name: x.target || t('api.log.noName'), n: x.target || '?' }) : `${x.method} ${x.path}`);
const logRaw = x => `${x.method} ${x.path}${x.query ? '?' + x.query : ''}${x.body ? '\n' + x.body : ''}\n→ ${x.status} · ${x.ms} ms`;
function logRow(x) {
  const ok = x.status < 400, err = x.err ? trErr(x.err) : '';
  return `<li class="alog-row" tabindex="0" title="${esc(logRaw(x))}">
    <div class="alog-main"><span class="alog-what">${esc(logText(x))}</span>${ok ? `<span class="badge ok">${esc(t('api.log.ok'))}</span>` : `<span class="badge danger">${esc(t('api.log.fail'))}</span>`}</div>
    <div class="alog-sub muted small"><span>${esc(fmtTime(x.at))}</span><span class="mono">${esc(x.ip)}</span>${!ok ? `<span class="alog-err">${esc(err || 'HTTP ' + x.status)}</span>` : ''}</div>
    <pre class="alog-raw">${esc(logRaw(x))}</pre></li>`;
}
function logsHTML() {
  const set = `<div class="alog-set">
      <label class="alog-on"><span class="switch sm"><input type="checkbox" id="alog-on" ${logCfg.off ? '' : 'checked'}><span></span></span><span>${esc(t('api.log.record'))}</span></label>
      <label class="alog-days"><span>${esc(t('api.log.keep'))}</span><select id="alog-days" class="sm">${logCfg.choices.map(d => `<option value="${d}" ${d === logCfg.days ? 'selected' : ''}>${esc(t('api.log.days', { n: d }))}</option>`).join('')}</select></label>
      <span class="alog-tot muted small">${esc(t('api.log.total', { n: logTotal }))}</span>
      <button class="btn sm danger" data-act="apikey.logsClear" ${logTotal ? '' : 'disabled'}>${esc(t('api.log.clear'))}</button>
    </div>`;
  const off = logCfg.off ? `<p class="hint warn-text">${esc(t('api.log.offHint'))}</p>` : '';
  const list = logItems.length
    ? `<ul class="alog-list">${logItems.map(logRow).join('')}</ul>${logMore ? `<div class="alog-more"><button class="btn sm" data-act="apikey.logsMore">${esc(t('api.log.more'))}</button></div>` : ''}`
    : `<div class="alog-empty muted">${esc(t('api.log.empty'))}</div>`;
  return set + off + list + `<p class="hint xp">${esc(t('api.log.help'))}</p>`;
}
function drawLogs() {
  const box = document.getElementById('alog-box');
  if (!box) return;
  box.innerHTML = logsHTML();
  const putSet = async (body, undo) => {
    try { await put(`${cfg.prefix}/keys/${logKey.id}/logs`, body); toast(t('api.log.saved'), 'ok'); await loadLogs(false); reload(); }
    catch (e) { toast(e.message, 'err'); undo(); }
  };
  const on = document.getElementById('alog-on'), days = document.getElementById('alog-days');
  if (on) on.addEventListener('change', () => putSet({ off: !on.checked }, () => { on.checked = !on.checked; }));
  if (days) days.addEventListener('change', () => { const was = logCfg.days; putSet({ days: Number(days.value) }, () => { days.value = String(was); }); });
}
async function loadLogs(more) {
  if (!logKey) return;
  const last = more && logItems.length ? logItems[logItems.length - 1].id : 0;
  try {
    const r = await get(`${cfg.prefix}/keys/${logKey.id}/logs?limit=50${last ? '&before=' + last : ''}`);
    logItems = more ? logItems.concat(r.items || []) : (r.items || []);
    logMore = !!r.more;
    logTotal = r.total || 0;
    logCfg = { off: !!r.logOff, days: r.logDays || 7, choices: r.dayChoices || [1, 7, 30, 90] };
    drawLogs();
  } catch (e) {
    // 「加载更多」没取到:已经列出来的留着,按钮恢复可点,报个错;第一页就没取到才把框里换成出错的原因
    if (more) { toast(e.message, 'err'); drawLogs(); return; }
    const box = document.getElementById('alog-box');
    if (box) box.innerHTML = `<p class="hint bad">${esc(e.message)}</p>`;
  }
}
function openLogs(k) {
  logKey = k; logItems = []; logMore = false; logTotal = 0;
  openModal(t('api.log.title', { name: nameOf(k) }), `<div id="alog-box"><p class="muted small">${esc(t('common.loading'))}</p></div>`, null, { wide: true, cancelText: t('common.close') });
  loadLogs(false);
}

registerActions({
  'api.copyUrl': () => copy(cfg.url),
  'api.docs': () => openApiDocs({ base: cfg.url, reseller: cfg.reseller, repo: cfg.repo, keys: cfg.enabled ? keys : [] }),
  'apikey.add': () => editKey(null),
  'apikey.logs': id => { const k = byId(id); if (k) openLogs(k); },
  'apikey.logsMore': async (_, btn) => { btn.disabled = true; await loadLogs(true); },
  // 清空:确认框和日志框是同一个弹窗,确认完再把日志框打开
  'apikey.logsClear': async () => {
    const k = logKey;
    if (!k || !await confirm(t('api.log.clearConfirm', { name: nameOf(k) }), { danger: true, okText: t('api.log.clear') })) { if (k) openLogs(k); return; }
    try { await del(`${cfg.prefix}/keys/${k.id}/logs`); toast(t('api.log.cleared'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
    openLogs(k);
  },
  'apikey.edit': id => { const k = byId(id); if (k) editKey(k); },
  'apikey.copy': id => { const k = byId(id); if (k) copy(k.token); },
  'apikey.pick': which => {
    const want = which === 'all' ? PERMS : READ_PERMS;
    document.querySelectorAll('.ak-perm').forEach(x => { x.checked = want.includes(x.value); });
  },
  'apikey.addMe': () => {
    const el = document.getElementById('ak-ips');
    if (!el || !you) return;
    const have = el.value.split(/[\s,，;]+/).filter(Boolean);
    if (!have.includes(you)) el.value = [...have, you].join('\n');
  },
  'apikey.toggle': async (id, el) => {
    const k = byId(id);
    if (!k) return;
    try {
      await put(`${cfg.prefix}/keys/${k.id}`, { enabled: el.checked });
      toast(t(el.checked ? 'api.keyOn' : 'api.keyOff', { name: nameOf(k) }), 'ok');
      await reload();
    } catch (e) { toast(e.message, 'err'); el.checked = !el.checked; }
  },
  'apikey.rotate': async id => {
    const k = byId(id);
    if (!k || !await confirm(t('api.keyRotateConfirm', { name: nameOf(k) }), { danger: true, okText: t('api.keyRotate') })) return;
    try { await post(`${cfg.prefix}/keys/${k.id}/rotate`); toast(t('api.keyRotated'), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'apikey.del': async id => {
    const k = byId(id);
    if (!k || !await confirm(t('api.keyDeleteConfirm', { name: nameOf(k) }), { danger: true, okText: t('common.delete') })) return;
    try { await del(`${cfg.prefix}/keys/${k.id}`); toast(t('common.deleted'), 'ok'); await reload(); }
    catch (e) { toast(e.message, 'err'); }
  },
});

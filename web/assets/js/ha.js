// 主机容灾与设为主机的界面:服务器列表状态栏里的角色标签 + 小卡片、行内「更多」里的「设为主机…」、
// 设为主机确认框(带进度、完成后给新面板地址)、备用接任、设置页的「主机容灾」卡片、页面顶部的一次性提示。
// 都收在这一个模块里,服务器页只调 roleChip / haMoreMenu,方便和别的界面改动合并。
import { get, post } from './api.js';
import { t } from './i18n.js';
import { trErr } from './errmsg.js';
import { esc, fmtRelative, fmtTime, toast, openModal, closeModal, registerActions, field } from './ui.js';

let view = null;
let rows = [];
let refresh = () => {};

// loadHA 取容灾状态(服务器页每次刷新一起取;取不到就当没有,角色标签退回原来的样子)。
export async function loadHA() {
  try { view = await get('ha'); } catch { view = null; }
  return view;
}
export const haView = () => view;
// setHARefresh 动作之后让服务器页重画。
export function setHARefresh(fn) { refresh = fn; }

const isMasterView = () => view && view.role === 'master';
const standbyIds = () => (isMasterView() ? view.standbyIds : (view && view.node && view.node.standbys)) || [];
const infoOf = id => ((view && view.standbys) || []).find(s => s.id === id);

// roleOf 一台服务器在主机容灾里的角色:master / standby(rank)/ node / former(失联的原主机)。
function roleOf(n, masterId) {
  if (!view) return null;
  if (isMasterView()) {
    if (n.isLocal) return { kind: 'master', local: true };
    if (view.former && view.former.id === n.id) return { kind: 'former' };
    const rank = standbyIds().indexOf(n.id) + 1;
    return rank ? { kind: 'standby', rank } : { kind: 'node' };
  }
  const mid = view.masterId || masterId;
  if (n.id === mid) return { kind: 'master', lost: !!(view.node && view.node.lost) };
  const rank = standbyIds().indexOf(n.id) + 1;
  return { kind: rank ? 'standby' : 'node', rank, local: n.isLocal };
}

function chipText(r) {
  const local = r.local ? ' · ' + t('ha.local') : '';
  switch (r.kind) {
    case 'master': return t('ha.role.master') + (r.lost ? ' · ' + t('ha.lost') : local);
    case 'former': return t('ha.role.former') + ' · ' + t('ha.lost');
    case 'standby': return t('ha.role.standby', { n: r.rank }) + local;
  }
  return t('ha.role.node') + local;
}

const chipTone = r => (r.kind === 'former' || r.lost ? 'k-lost' : r.kind === 'master' ? 'k-master' : r.kind === 'standby' ? 'k-standby' : '');

// roleChip 状态栏里的角色标签(可点,打开小卡片)。data 是服务器页的数据(用它的 masterId)。
export function roleChip(n, data) {
  rows = (data && data.nodes) || rows;
  const r = roleOf(n, data && data.masterId);
  if (!r) return '';
  return `<button type="button" class="role-chip ${chipTone(r)}" data-act="ha.card" data-id="${n.id}" aria-haspopup="dialog" aria-expanded="false" title="${esc(t('ha.chipHint'))}">${esc(chipText(r))}<span class="caret" aria-hidden="true">▾</span></button>`;
}

// haMoreMenu 行内「更多」:设为主机…(第二入口:窄屏下状态栏被藏起来时也能用)。只在主机上、对副机有。
export function haMoreMenu(n) {
  if (!isMasterView() || n.isLocal) return '';
  return `<details class="menu ha-more"><summary class="btn sm" title="${esc(t('common.more'))}" aria-label="${esc(t('common.more'))}">⋯</summary><div class="menu-list">
    <button data-act="ha.setMaster" data-id="${n.id}">${t('ha.setMaster')}…</button></div></details>`;
}

// ---- 小卡片 ----

let pop = null;
let popChip = null;

function closeCard(focusBack) {
  if (!pop) return;
  pop.remove();
  pop = null;
  if (popChip) {
    popChip.setAttribute('aria-expanded', 'false');
    if (focusBack) popChip.focus();
  }
  popChip = null;
}

function placeCard(chip) {
  const r = chip.getBoundingClientRect();
  const w = pop.offsetWidth, h = pop.offsetHeight;
  const vw = window.innerWidth, vh = window.innerHeight;
  let left = Math.min(Math.max(8, r.left), vw - w - 8);
  let top = r.bottom + 6;
  if (top + h > vh - 8 && r.top - 6 - h > 8) top = r.top - 6 - h; // 下面放不下就往上翻
  pop.style.left = left + 'px';
  pop.style.top = Math.max(8, top) + 'px';
}

document.addEventListener('click', e => {
  if (pop && !pop.contains(e.target) && !e.target.closest('.role-chip')) closeCard(false);
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && pop) { e.stopPropagation(); closeCard(true); }
}, true);
['scroll', 'resize'].forEach(ev => window.addEventListener(ev, () => { if (pop && popChip) placeCard(popChip); }, true));

const modeText = m => t('ha.mode.' + (m || 'off'));

function standbyLine(info, rank) {
  const rep = info && info.replica;
  let s = t('ha.card.standby', { n: rank });
  if (rep && rep.asOf) s += ' ' + t('ha.card.replicaAt', { t: fmtTime(rep.asOf) }) + (rep.err ? ' ' + t('ha.card.replicaBad') : rep.verifiedAt ? ' ' + t('ha.card.replicaOk') : '');
  return s;
}

const reasonText = code => t('ha.why.' + code);

async function openCard(id, chip) {
  if (pop && popChip === chip) { closeCard(true); return; }
  closeCard(false);
  const n = rows.find(x => x.id === Number(id)) || { id: Number(id), name: '#' + id };
  const r = roleOf(n, view && view.masterId) || { kind: 'node' };
  pop = document.createElement('div');
  pop.className = 'ha-pop';
  pop.setAttribute('role', 'dialog');
  pop.setAttribute('aria-label', n.name + ' · ' + chipText(r));
  pop.tabIndex = -1;
  popChip = chip;
  chip.setAttribute('aria-expanded', 'true');
  pop.innerHTML = cardHTML(n, r);
  document.body.appendChild(pop);
  placeCard(chip);
  (pop.querySelector('button:not([disabled]), a') || pop).focus();
  // 设为主机能不能点:问一次主机(在线、同步、版本、磁盘、是不是正在换)
  const btn = pop.querySelector('[data-act="ha.setMaster"]');
  if (btn && isMasterView()) {
    try {
      const plan = await get('ha/plan?id=' + n.id);
      if (!pop || popChip !== chip) return;
      const why = pop.querySelector('.why');
      if (plan.ok) { btn.disabled = false; if (why) why.textContent = ''; }
      else { btn.disabled = true; if (why) why.textContent = (plan.blockers || []).map(b => planText(b)).join(t('reach.sep')); }
    } catch (e) {
      if (pop && popChip === chip) { const why = pop.querySelector('.why'); if (why) why.textContent = e.message; }
    }
    if (pop) placeCard(chip);
  }
}

function cardHTML(n, r) {
  const head = `<h4>${esc(n.name)} · ${esc(chipText(r))}</h4>`;
  const setBtn = `<button class="btn sm primary" data-act="ha.setMaster" data-id="${n.id}" disabled>${t('ha.setMaster')}</button>`;
  const checking = `<p class="why">${t('ha.checking')}</p>`;
  if (isMasterView()) {
    if (r.kind === 'master') {
      const c = view.config || {};
      const names = standbyIds().map(id => (rows.find(x => x.id === id) || {}).name || '#' + id);
      const b = view.became;
      return head + `<p>${t('ha.card.master')}</p>
        <p>${t('ha.card.mode')}${esc(modeText(c.mode))}${c.mode && c.mode !== 'off' ? (names.length ? t('ha.card.standbys', { names: names.join(t('reach.sep')) }) : t('ha.card.noStandby')) : ''}</p>
        ${c.pauseUntil > Date.now() / 1000 ? `<p class="warn-text">${t('ha.card.paused', { t: fmtTime(c.pauseUntil) })}</p>` : ''}
        ${b ? `<p>${esc(t(b.how === 'takeover' ? 'ha.card.tookOver' : 'ha.card.setBy', { t: fmtTime(b.at), name: b.formerName }))}</p>` : ''}
        <p>${t('ha.card.howSwitch')}</p>
        <div class="actions"><a class="btn sm" href="#/settings" data-act="ha.toSettings">${t('ha.toSettings')}</a></div>`;
    }
    if (r.kind === 'former') {
      const f = view.former || {};
      return head + `<p>${esc(t('ha.card.former', { name: n.name, t: fmtRelative(f.since) }))}</p>
        ${n.apiUrl ? '' : `<p class="warn-text">${t('ha.card.formerNoLink')}</p>`}`;
    }
    const info = infoOf(n.id);
    const pubChanged = info && (info.reasons || []).includes('pubChanged');
    return head + `<p>${r.kind === 'standby' ? esc(standbyLine(info, r.rank)) : t('ha.card.node')}</p>
      ${info && !info.eligible && (view.config || {}).mode !== 'off' && r.kind !== 'standby' && (info.reasons || []).length ? `<p class="muted small">${t('ha.card.notStandby')}${esc((info.reasons || []).map(reasonText).join(t('reach.sep')))}</p>` : ''}
      <div class="actions">${setBtn}${pubChanged ? `<button class="btn sm" data-act="ha.repin" data-id="${n.id}">${t('ha.repin')}</button>` : ''}</div>${checking}`;
  }
  // 副机上看
  const nv = (view && view.node) || {};
  if (r.kind === 'master') {
    return head + `<p>${r.lost ? esc(t('ha.card.masterLost', { m: Math.floor((nv.leaseAgo || 0) / 60) })) : t('ha.card.masterOk')}</p>
      <p>${t('ha.card.mode')}${esc(modeText(view.mode))}</p>`;
  }
  if (!n.isLocal) return head + `<p>${r.kind === 'standby' ? esc(t('ha.card.standbyOther', { n: r.rank })) : t('ha.card.nodeOther')}</p>`;
  const lines = [`<p>${r.kind === 'standby' ? esc(standbyLine({ replica: nv.replica && { asOf: nv.replica.asOf, verifiedAt: nv.replica.verifiedAt, err: nv.replica.verifyErr } }, r.rank)) : t('ha.card.nodeSelf')}</p>`];
  if (nv.canTakeover) lines.push(`<div class="actions"><button class="btn sm danger" data-act="ha.takeover">${t('ha.takeover')}</button></div>`);
  else if (nv.lost && r.kind === 'standby' && nv.why) lines.push(`<p class="why">${esc(t('ha.cant.' + nv.why))}</p>`);
  if (nv.evidence && nv.evidence.reason) lines.push(`<p class="muted small">${esc(t('ha.ev.' + nv.evidence.reason))}</p>`);
  return head + lines.join('');
}

function planText(it) {
  const a = it.args || {};
  return t('ha.plan.' + it.code, a);
}

// ---- 设为主机 ----

const STAGES = ['prepare', 'export', 'send', 'grant', 'commit', 'done'];

async function openSetMaster(id) {
  closeCard(false);
  let plan;
  try { plan = await get('ha/plan?id=' + id); } catch (e) { toast(e.message, 'err'); return; }
  const target = plan.targetName, self = plan.selfName;
  const dns = !plan.subHost ? '' : plan.subKept
    ? (plan.dnsAuto ? t('ha.sm.dnsAuto', { names: (plan.dnsNames || []).join(', '), target, ip: plan.targetIp || '?' })
      : t('ha.sm.dns', { host: plan.subHost, target, ip: plan.targetIp || '?' })) : '';
  const items = [
    t('ha.sm.data', { target, self }),
    t('ha.sm.self', { self }),
    t('ha.sm.users', { self, target }),
    t('ha.sm.panel', { url: plan.newPanel }),
  ];
  if (dns) items.push(dns);
  const warn = (plan.warnings || []).map(w => `<li>${esc(planText(w))}</li>`).join('');
  const block = (plan.blockers || []).map(b => `<li>${esc(planText(b))}</li>`).join('');
  openModal(t('ha.sm.title', { target }), `
    <ul class="ha-plan">${items.map(x => `<li>${esc(x)}</li>`).join('')}</ul>
    ${warn ? `<div class="ha-note warn"><b>${t('ha.sm.warn')}</b><ul>${warn}</ul></div>` : ''}
    ${block ? `<div class="ha-note bad"><b>${t('ha.sm.blocked')}</b><ul>${block}</ul></div>` : ''}
    <div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-pw" type="password" autocomplete="current-password">`, t('ha.passwordHelp'))}
      ${plan.totp ? field(t('ha.code'), `<input id="ha-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>`, async () => {
    const r = await post('ha/set-master', { id: Number(id), password: document.getElementById('ha-pw').value, code: (document.getElementById('ha-code') || {}).value || '' });
    showProgress(r, plan);
    const e = new Error('');
    e.keep = true;
    throw e; // 不关窗口:接着显示进度
  }, { saveText: t('ha.setMaster'), danger: false });
  const save = document.getElementById('modal-save');
  if (!plan.ok) save.disabled = true;
}

function stepsHTML(st, plan) {
  const cur = STAGES.indexOf(st.stage);
  const label = s => t('ha.stage.' + s, { target: plan.targetName, self: plan.selfName });
  return `<ol class="ha-steps">${STAGES.map((s, i) => {
    let cls = 'pending', icon = '○';
    if (st.stage === 'failed') { cls = i === 0 ? 'fail' : 'pending'; icon = i === 0 ? '✗' : '○'; }
    else if (i < cur || st.stage === 'done') { cls = 'done'; icon = '✓'; }
    else if (i === cur) { cls = 'cur'; icon = '<span class="spin"></span>'; }
    return `<li class="${cls}"><span class="ic">${icon}</span>${esc(label(s))}</li>`;
  }).join('')}</ol>`;
}

function showProgress(st, plan) {
  document.getElementById('modal-save').hidden = true;
  document.getElementById('modal-cancel').textContent = t('common.close');
  const body = document.getElementById('modal-body');
  const render = (s, extra = '') => {
    if (!document.body.contains(body)) return;
    let html = `<p class="hint">${t('ha.sm.running')}</p>` + stepsHTML(s, plan) + extra;
    if (s.stage === 'failed') html = `<div class="ha-note bad"><b>${t('ha.sm.failed')}</b><p>${esc(trErr(s.err || ''))}</p></div><p>${t('ha.sm.failedKept')}</p>`;
    if (s.stage === 'done') {
      html = `<div class="ha-note ok"><b>${t('ha.sm.done', { target: plan.targetName })}</b></div>
        <p>${t('ha.sm.newPanel')} <a href="${esc(s.newPanel)}" target="_blank" rel="noopener">${esc(s.newPanel)}</a></p>
        <p class="muted small">${t('ha.sm.relogin')}</p>`;
      const left = [];
      if (s.subKept && !s.dnsAuto) left.push(t('ha.sm.leftDns', { host: s.subHost, ip: s.targetIp || '?' }));
      if (left.length) html += `<p><b>${t('ha.sm.left')}</b></p><ul class="ha-plan">${left.map(x => `<li>${esc(x)}</li>`).join('')}</ul>`;
    }
    body.innerHTML = html;
  };
  render(st);
  const started = Date.now();
  const poll = async () => {
    if (!document.body.contains(body) || document.getElementById('modal').hidden) return;
    let s = null;
    try {
      const v = await get('ha', { timeout: 8000 });
      s = v.switch || (v.node && v.node.switch) || null;
    } catch { /* 本机正在重启(变成副机),过一会儿再问 */ }
    if (s && s.id === st.id) {
      render(s);
      if (s.stage === 'done' || s.stage === 'failed') return;
    } else if (!s) {
      render(st, `<p class="muted small">${t('ha.sm.restarting')}</p>`);
    }
    if (Date.now() - started > 20 * 60 * 1000) return;
    setTimeout(poll, 1500);
  };
  setTimeout(poll, 1000);
}

// ---- 备用:接任为主机 ----

function openTakeover() {
  closeCard(false);
  const nv = (view && view.node) || {};
  const at = nv.replica ? fmtTime(nv.replica.asOf) : '—';
  openModal(t('ha.to.title'), `
    <ul class="ha-plan">
      <li>${esc(t('ha.to.data', { t: at, name: view.masterName || '' }))}</li>
      <li>${esc(t('ha.to.restart'))}</li>
      <li>${esc(t('ha.to.login'))}</li>
      <li>${esc(t('ha.to.former'))}</li>
    </ul>
    <div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-pw" type="password" autocomplete="current-password">`, t('ha.passwordHelpLocal'))}
      ${view.totp ? field(t('ha.code'), `<input id="ha-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>`, async () => {
    await post('ha/takeover', { password: document.getElementById('ha-pw').value, code: (document.getElementById('ha-code') || {}).value || '' });
    toast(t('ha.to.started'), 'ok');
  }, { saveText: t('ha.takeover'), danger: true });
}

// ---- 二次确认(改主机容灾设置) ----

function askPassword(title, onOk) {
  openModal(title, `<div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-pw" type="password" autocomplete="current-password">`, t('ha.passwordHelp'))}
      ${view && view.totp ? field(t('ha.code'), `<input id="ha-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>`, async () => onOk(document.getElementById('ha-pw').value, (document.getElementById('ha-code') || {}).value || ''),
  { saveText: t('common.confirm') });
}

registerActions({
  'ha.card': (id, el) => openCard(id, el),
  'ha.setMaster': id => openSetMaster(id),
  'ha.takeover': async () => { if (!view) await loadHA(); if (view) openTakeover(); }, // 顶部提示条上的按钮:别的页面没取过状态
  'ha.toSettings': () => { closeCard(false); try { sessionStorage.setItem('m-ui.scrollTo', 'ha-card'); } catch {} location.hash = '#/settings'; },
  'ha.repin': async id => {
    try { await post('ha/repin', { id: Number(id) }); closeCard(false); toast(t('ha.repinned'), 'ok'); await loadHA(); refresh(); }
    catch (e) { toast(e.message, 'err'); }
  },
  'ha.notice': async (id, btn) => {
    btn.disabled = true;
    try { await post('ha/notice', { id }); } catch {}
    const bar = btn.closest('.alert-bar');
    if (bar) bar.remove();
  },
  'ha.pause': async min => {
    try { await post('ha/pause', { minutes: Number(min) }); await loadHA(); renderHASettings(); toast(t('set.saved'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'ha.save': () => saveSettings(),
  'ha.dnsTest': async (_, btn) => {
    btn.disabled = true;
    const out = document.getElementById('ha-dns-out');
    try {
      const r = await post('ha/dns-test', { token: (document.getElementById('ha-dnsToken') || {}).value || '', names: (document.getElementById('ha-dnsNames') || {}).value || '' });
      if (out) out.innerHTML = dnsTestHTML(r);
    } catch (e) { if (out) out.innerHTML = `<p class="danger-text">${esc(e.message)}</p>`; }
    finally { btn.disabled = false; }
  },
});

// ---- 页面顶部的提示 ----

// haNotices 由 app.js 在每次刷新状态后调用:一次性提示(点"知道了"清掉)+ 进行中的状态(冻结、主机失联)。
export function haNotices(status) {
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  const ha = (status && status.ha) || null;
  const want = new Map();
  if (ha) {
    if (ha.frozenUntil) want.set('ha-frozen', { cls: 'warn', html: `<span><b>${esc(trErr(ha.frozenText || ''))}</b></span>` });
    if (ha.lost && ha.mode && ha.mode !== 'off') {
      const m = Math.floor(ha.lost / 60);
      want.set('ha-lost', {
        cls: '', html: `<span><b>${esc(t('ha.bar.lost', { name: ha.masterName || '', m }))}</b> ${esc(ha.rank ? t('ha.bar.lostStandby', { n: ha.rank }) : '')}</span>${ha.canTakeover ? `<button class="btn sm danger" data-act="ha.takeover">${t('ha.takeover')}</button>` : ''}`,
      });
    }
    for (const n of ha.notices || []) want.set('ha-n-' + n.id, { cls: n.kind === 'switchFailed' || n.kind === 'demoted' ? 'warn' : 'ha-info', html: `<span>${noticeHTML(n)}</span><button class="btn sm ghost" data-act="ha.notice" data-id="${esc(n.id)}">${t('reach.dismiss')}</button>` });
  }
  document.querySelectorAll('.alert-bar[data-ha]').forEach(b => { if (!want.has(b.dataset.ha)) b.remove(); });
  for (const [key, v] of want) {
    let bar = document.querySelector(`.alert-bar[data-ha="${key}"]`);
    if (!bar) {
      bar = document.createElement('div');
      bar.dataset.ha = key;
      main.insertBefore(bar, page);
    }
    bar.className = 'alert-bar ' + v.cls;
    if (bar.dataset.html !== v.html) { bar.dataset.html = v.html; bar.innerHTML = v.html; }
  }
}

function noticeHTML(n) {
  const warn = (n.warn || []).length ? ' ' + (n.warn || []).map(w => esc(trErr(w))).join(' ') : '';
  switch (n.kind) {
    case 'became': {
      let s = `<b>${esc(t(n.how === 'takeover' ? 'ha.n.tookOver' : 'ha.n.became', { name: n.name }))}</b>`;
      if (n.host && !/^[\d.:]+$/.test(n.host)) s += ' ' + esc(n.dns ? t('ha.n.dnsDone', { host: n.host, ip: n.ip }) : t('ha.n.dnsTodo', { host: n.host, ip: n.ip }));
      return s + warn;
    }
    case 'formerBack': return `<b>${esc(t('ha.n.formerBack', { name: n.name }))}</b>`;
    case 'demoted': return `<b>${esc(t('ha.n.demoted', { name: n.name }))}</b>${n.n ? ' ' + esc(t('ha.n.divergent', { n: n.n, file: n.file || '' })) : ''}`;
    case 'joined': return `<b>${esc(t('ha.n.joined'))}</b>`;
    case 'switched': return `<b>${esc(t('ha.n.switched', { name: n.name }))}</b> ${esc(t('ha.sm.newPanel'))} <a href="${esc(n.url)}" target="_blank" rel="noopener">${esc(n.url)}</a>${n.host && !n.dns ? ' ' + esc(t('ha.sm.leftDns', { host: n.host, ip: n.ip || '?' })) : ''}`;
    case 'switchFailed': return `<b>${esc(t('ha.n.switchFailed', { name: n.name }))}</b>${warn}`;
  }
  return esc(n.kind);
}

// ---- 设置页:主机容灾 ----

const MODES = ['off', 'confirm', 'auto'];

// renderHASettings 设置页里的「主机容灾」卡片(只在主机上;副机上说明一句到主机改)。
export async function renderHASettings(container) {
  const host = container || document.getElementById('ha-card');
  if (!host) return;
  const v = await get('ha?full=1').catch(() => null);
  if (!v) { host.remove(); return; }
  view = v;
  if (v.role !== 'master') {
    host.innerHTML = `<div class="card-head"><h2>${t('ha.title')}</h2></div><p class="hint">${esc(t('ha.set.nodeHint', { mode: modeText(v.mode) }))}</p>`;
    return;
  }
  const c = v.config || {};
  const ready = (v.ready || []).map(ch => `<li class="lv-${ch.level}"><span class="ic">${{ ok: '✓', warn: '!', bad: '✗', info: '·' }[ch.level] || '·'}</span><span>${esc(t('ha.ready.' + ch.code + '.' + ch.level, ch.args || {}))}</span></li>`).join('');
  const paused = c.pauseUntil > Date.now() / 1000;
  host.innerHTML = `
    <div class="card-head"><h2>${t('ha.title')}</h2><button class="btn primary sm" data-act="ha.save">${t('common.save')}</button></div>
    <p class="hint">${t('ha.set.intro')}</p>
    <div class="ha-modes" role="radiogroup" aria-label="${esc(t('ha.title'))}">
      ${MODES.map(m => `<label class="${(c.mode || 'off') === m ? 'sel' : ''}"><input type="radio" name="ha-mode" value="${m}" ${(c.mode || 'off') === m ? 'checked' : ''}>
        <span><b>${esc(t('ha.mode.' + m))}</b>${m === 'confirm' ? ` <span class="badge ok">${t('ha.recommended')}</span>` : ''}<br><span class="muted small">${esc(t('ha.modeHelp.' + m, { c: c.confirmMinutes || 3, a: c.autoMinutes || 10 }))}</span></span></label>`).join('')}
    </div>
    <div class="form-grid">
      ${field(t('ha.set.standbys'), `<select id="ha-standbys">${[1, 2, 3].map(n => `<option value="${n}" ${(c.standbys || 2) === n ? 'selected' : ''}>${n}</option>`).join('')}</select>`, t('ha.set.standbysHelp'))}
    </div>
    <p class="hint">${t('ha.set.lostRule')}</p>
    <h4 class="ha-sub">${t('ha.set.ready')}</h4>
    <ul class="ha-check">${ready}</ul>
    <div class="row ha-pause">
      ${paused ? `<span class="warn-text small">${esc(t('ha.card.paused', { t: fmtTime(c.pauseUntil) }))}</span> <button class="btn sm" data-act="ha.pause" data-id="0">${t('ha.resume')}</button>`
    : `<span class="muted small">${t('ha.set.pause')}</span> ${[60, 360, 1440].map(m => `<button class="btn sm" data-act="ha.pause" data-id="${m}">${t('ha.pauseFor', { h: m / 60 })}</button>`).join(' ')}`}
    </div>
    <details class="ha-adv"><summary>${t('ha.set.advanced')}</summary>
      <div class="form-grid">
        ${field(t('ha.set.confirmMin'), `<input id="ha-confirmMinutes" type="number" min="2" max="30" value="${c.confirmMinutes || 3}">`, t('ha.set.confirmMinHelp'))}
        ${field(t('ha.set.autoMin'), `<input id="ha-autoMinutes" type="number" min="5" max="60" value="${c.autoMinutes || 10}">`, t('ha.set.autoMinHelp'))}
        ${field(t('ha.set.rankStep'), `<input id="ha-rankStepMinutes" type="number" min="1" max="10" value="${c.rankStepMinutes || 3}">`, t('ha.set.rankStepHelp'))}
        ${field(t('ha.set.replicaMin'), `<input id="ha-replicaMinutes" type="number" min="1" max="60" value="${c.replicaMinutes || 5}">`, t('ha.set.replicaMinHelp'))}
        <label class="field check"><input type="checkbox" id="ha-witness" ${c.witness !== false ? 'checked' : ''}><span>${t('ha.set.witness')}</span><span class="help">${esc(t('ha.set.witnessHelp'))}</span></label>
      </div>
      <h4 class="ha-sub">${t('ha.set.dns')}</h4>
      <p class="hint">${t('ha.set.dnsHelp')}</p>
      <div class="form-grid">
        ${field(t('ha.set.dnsToken'), `<input id="ha-dnsToken" type="password" autocomplete="off" placeholder="${esc(c.dnsTokenSet ? t('ha.set.dnsTokenKeep') : '')}">`, t('ha.set.dnsTokenHelp'))}
        ${field(t('ha.set.dnsNames'), `<input id="ha-dnsNames" value="${esc(c.dnsNames || '')}" placeholder="sub.example.com">`, t('ha.set.dnsNamesHelp'))}
      </div>
      <div class="row"><button class="btn sm" data-act="ha.dnsTest">${t('ha.set.dnsTest')}</button>${c.dnsTokenSet ? ` <label class="field check inline"><input type="checkbox" id="ha-dnsClear"><span>${t('ha.set.dnsClear')}</span></label>` : ''}</div>
      <div id="ha-dns-out"></div>
    </details>`;
  host.querySelectorAll('input[name="ha-mode"]').forEach(r => r.addEventListener('change', () => {
    host.querySelectorAll('.ha-modes label').forEach(l => l.classList.toggle('sel', l.querySelector('input').checked));
  }));
}

function dnsTestHTML(r) {
  if (!r || !(r.records || []).length) return `<p class="muted small">${t('ha.dns.none')}</p>`;
  return `<ul class="ha-check">${r.records.map(x => `<li class="lv-${x.match ? 'ok' : 'info'}"><span class="ic">${x.match ? '✓' : '·'}</span><span>${esc(x.name)} ${esc(x.type)} ${esc(x.content)}${x.proxied ? ' (proxied)' : ''} — ${esc(x.match ? t('ha.dns.match') : t('ha.dns.skip'))}</span></li>`).join('')}</ul>${r.error ? `<p class="danger-text small">${esc(trErr(r.error))}</p>` : ''}`;
}

function saveSettings() {
  const num = id => Number((document.getElementById(id) || {}).value) || 0;
  const body = {
    mode: (document.querySelector('input[name="ha-mode"]:checked') || {}).value || 'off',
    standbys: num('ha-standbys') || 2,
    confirmMinutes: num('ha-confirmMinutes') || 3, autoMinutes: num('ha-autoMinutes') || 10,
    rankStepMinutes: num('ha-rankStepMinutes') || 3, replicaMinutes: num('ha-replicaMinutes') || 5,
    witness: !!(document.getElementById('ha-witness') || {}).checked,
    dnsNames: (document.getElementById('ha-dnsNames') || {}).value || '',
    dnsToken: (document.getElementById('ha-dnsToken') || {}).value || '',
    clearDnsToken: !!(document.getElementById('ha-dnsClear') || {}).checked,
  };
  askPassword(t('ha.set.confirmSave'), async (password, code) => {
    await post('ha', { ...body, password, code });
    closeModal();
    toast(t('set.saved'), 'ok');
    renderHASettings();
  });
}

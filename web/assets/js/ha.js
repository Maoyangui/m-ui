// 主机容灾与设为主机的界面:服务器列表状态栏里的角色标签 + 小卡片、行内「更多」里的「设为主机…」、
// 设为主机确认框(带进度、完成后给新面板地址)、备用接任、设置页的「主机容灾」卡片、页面顶部的提示。
// 都收在这一个模块里,服务器页只调 roleChip / haMoreMenu,方便和别的界面改动合并。
import { get, post, SLOW, LONG } from './api.js';
import { t } from './i18n.js';
import { trErr } from './errmsg.js';
import { esc, fmtRelative, fmtTime, toast, openModal, closeModal, registerActions, field, modalGen, cfTokenLink } from './ui.js';

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
const nameOf = id => (rows.find(x => x.id === id) || {}).name || '#' + id;

// 名单一律按界面语言的分隔符拼(后端给数组,不在后端拼「、」)
const joinNames = list => (list || []).join(t('reach.sep'));
// has 这条文案在字典里有没有(t() 查不到时原样返回键)
const has = k => t(k) !== k;
// argsOf 后端给的代码 + 参数:names 数组拼成一句;svc(面板 / 代理面板 / WARP)按界面语言翻译;err 是后端原文,再翻一层
function argsOf(it) {
  const a = { ...(it.args || {}) };
  if ((it.names || []).length) a.names = joinNames(it.names);
  if (a.svc) a.svc = t('ha.svc.' + a.svc);
  if (a.err) a.err = trErr(a.err);
  return a;
}

// fill 后端给的代码配上文案;后端少给了参数时不把 {占位} 原样露给用户
const fill = (key, args) => t(key, args).replace(/\{[A-Za-z0-9_]+\}/g, '—');

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
// 表格每几秒整体重画一次:打开着卡片的那台照样输出 aria-expanded="true"。
export function roleChip(n, data) {
  rows = (data && data.nodes) || rows;
  const r = roleOf(n, data && data.masterId);
  if (!r) return '';
  return `<button type="button" class="role-chip ${chipTone(r)}" data-act="ha.card" data-id="${n.id}" aria-haspopup="dialog" aria-expanded="${popId === n.id ? 'true' : 'false'}" title="${esc(t('ha.chipHint'))}">${esc(chipText(r))}<span class="caret" aria-hidden="true">▾</span></button>`;
}

// haMoreMenu 行内「更多」:设为主机…(第二入口:窄屏下状态栏被藏起来时也能用)。只在主机上、对副机有。
export function haMoreMenu(n) {
  if (!isMasterView() || n.isLocal) return '';
  return `<details class="menu ha-more"><summary class="btn sm" title="${esc(t('common.more'))}" aria-label="${esc(t('common.more'))}">⋯</summary><div class="menu-list">
    <button data-act="ha.setMaster" data-id="${n.id}">${t('ha.setMaster')}…</button></div></details>`;
}

// ---- 小卡片 ----
// 卡片挂在 body 上,按服务器 id 找它的标签(不记元素:服务器表每几秒重画,标签元素会被换掉)。

let pop = null;
let popId = null;
let cardSeq = 0; // 每开一张卡片加一:卡片里异步取回的结果只写给还开着的那张

const chipOf = id => document.querySelector(`.role-chip[data-id="${id}"]`);

function closeCard(focusBack) {
  if (!pop) return;
  pop.remove();
  pop = null;
  const chip = popId == null ? null : chipOf(popId);
  popId = null;
  cardSeq++;
  if (chip) {
    chip.setAttribute('aria-expanded', 'false');
    if (focusBack) chip.focus();
  }
}

// placeCard 贴着标签放;标签不在了(那一行被删、换了页面)就收起
function placeCard() {
  if (!pop) return;
  const chip = chipOf(popId);
  if (!chip) { closeCard(false); return; }
  if (chip.getAttribute('aria-expanded') !== 'true') chip.setAttribute('aria-expanded', 'true');
  const r = chip.getBoundingClientRect();
  const w = pop.offsetWidth, h = pop.offsetHeight;
  const vw = window.innerWidth, vh = window.innerHeight;
  const left = Math.min(Math.max(8, r.left), vw - w - 8);
  let top = r.bottom + 6;
  if (top + h > vh - 8 && r.top - 6 - h > 8) top = r.top - 6 - h; // 下面放不下就往上翻
  pop.style.left = left + 'px';
  pop.style.top = Math.max(8, top) + 'px';
}

document.addEventListener('click', e => {
  if (pop && !pop.contains(e.target) && !(e.target.closest && e.target.closest('.role-chip'))) closeCard(false);
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && pop) { e.stopPropagation(); closeCard(true); }
}, true);
['scroll', 'resize'].forEach(ev => window.addEventListener(ev, () => placeCard(), true));

const modeText = m => t('ha.mode.' + (m || 'off'));

// standbyLine 备用的一句话:手里没有副本时不说「保存着」
function standbyLine(info, rank) {
  const rep = info && info.replica;
  if (!rep || !rep.asOf) return t('ha.card.standbyNoReplica', { n: rank });
  return t('ha.card.standby', { n: rank }) + ' ' + t('ha.card.replicaAt', { t: fmtTime(rep.asOf) })
    + (rep.err ? ' ' + t('ha.card.replicaBad') : rep.verifiedAt ? ' ' + t('ha.card.replicaOk') : '');
}

const reasonText = code => t('ha.why.' + code);
// whyText 不能接任的原因:证据代码(ha.ev.*)优先,其次接任检查的代码(ha.cant.*)
const whyText = code => (has('ha.ev.' + code) ? t('ha.ev.' + code) : t('ha.cant.' + code));

async function openCard(id, chip) {
  id = Number(id);
  if (pop && popId === id) { closeCard(true); return; }
  closeCard(false);
  const n = rows.find(x => x.id === id) || { id, name: '#' + id };
  const r = roleOf(n, view && view.masterId) || { kind: 'node' };
  pop = document.createElement('div');
  pop.className = 'ha-pop';
  pop.setAttribute('role', 'dialog');
  pop.setAttribute('aria-label', n.name + ' · ' + chipText(r));
  pop.tabIndex = -1;
  popId = id;
  const seq = ++cardSeq;
  if (chip) chip.setAttribute('aria-expanded', 'true');
  pop.innerHTML = cardHTML(n, r);
  document.body.appendChild(pop);
  placeCard();
  if (!pop) return;
  (pop.querySelector('button:not([disabled]), a') || pop).focus();
  // 设为主机能不能点:问一次主机(在线、同步、版本、磁盘、是不是正在换)
  const btn = pop.querySelector('[data-act="ha.setMaster"]');
  if (btn && isMasterView()) {
    let plan = null, err = '';
    try { plan = await get('ha/plan?id=' + id); } catch (e) { err = e.message; }
    if (!pop || seq !== cardSeq) return; // 卡片已经关了或换成别的了
    const why = pop.querySelector('.why');
    if (plan && plan.ok) { btn.disabled = false; if (why) why.textContent = ''; }
    else if (plan) { btn.disabled = true; if (why) why.textContent = (plan.blockers || []).map(planText).join(t('reach.sep')); }
    else if (why) why.textContent = err;
    placeCard();
  }
}

export function cardHTML(n, r) {
  const head = `<h4>${esc(n.name)} · ${esc(chipText(r))}</h4>`;
  const setBtn = `<button class="btn sm primary" data-act="ha.setMaster" data-id="${n.id}" disabled>${t('ha.setMaster')}</button>`;
  const checking = `<p class="why">${t('ha.checking')}</p>`;
  if (isMasterView()) {
    const c = view.config || {};
    const on = c.mode && c.mode !== 'off';
    if (r.kind === 'master') {
      const names = standbyIds().map(nameOf);
      const b = view.became;
      return head + `<p>${t('ha.card.master')}</p>
        <p>${esc(t('ha.card.mode') + modeText(c.mode) + (on ? (names.length ? t('ha.card.standbys', { names: joinNames(names) }) : t('ha.card.noStandby')) : ''))}</p>
        ${c.pauseUntil > Date.now() / 1000 ? `<p class="warn-text">${esc(t('ha.card.paused', { t: fmtTime(c.pauseUntil) }))}</p>` : ''}
        ${on && view.replicaErr ? `<p class="warn-text small">${esc(t('ha.card.replicaErr', { err: trErr(view.replicaErr) }))}</p>` : ''}
        ${b ? `<p>${esc(t(b.how === 'takeover' ? 'ha.card.tookOver' : 'ha.card.setBy', { t: fmtTime(b.at), name: b.formerName || '' }))}</p>` : ''}
        ${view.signFp ? `<p class="muted small fp">${esc(t('ha.card.signFp', { fp: view.signFp }))}</p>` : ''}
        <p>${t('ha.card.howSwitch')}</p>
        <div class="actions"><a class="btn sm" href="#/settings" data-act="ha.toSettings">${t('ha.toSettings')}</a></div>`;
    }
    if (r.kind === 'former') {
      const f = view.former || {};
      return head + `<p>${esc(t('ha.card.former', { name: n.name, t: fmtRelative(f.since) }))}</p>
        ${n.apiUrl ? '' : `<p class="warn-text">${t('ha.card.formerNoLink')}</p>`}`;
    }
    const info = infoOf(n.id);
    const reasons = (info && info.reasons) || [];
    const pubChanged = reasons.includes('pubChanged');
    const lines = [`<p>${esc(r.kind === 'standby' ? standbyLine(info, r.rank) : t('ha.card.node'))}</p>`];
    if (on && reasons.length && (r.kind === 'standby' || !info.eligible)) {
      lines.push(`<p class="muted small">${esc(t(r.kind === 'standby' ? 'ha.card.standbyNote' : 'ha.card.notStandby') + joinNames(reasons.map(reasonText)))}</p>`);
    }
    const ack = info && info.ack;
    if (ack && ack.err) lines.push(`<p class="warn-text small">${esc(t('ha.card.ackErr', { t: fmtTime(ack.errAt), err: trErr(ack.err) }))}</p>`);
    if (info && pubChanged && info.pinnedFp) lines.push(`<p class="muted small fp">${esc(t('ha.card.fpChanged', { old: info.pinnedFp, now: info.pubFp || '—' }))}</p>`);
    else if (info && (info.pinnedFp || info.pubFp)) lines.push(`<p class="muted small fp">${esc(t('ha.card.fp', { fp: info.pinnedFp || info.pubFp }))}</p>`);
    return head + lines.join('') + `<div class="actions">${setBtn}${pubChanged ? `<button class="btn sm" data-act="ha.repin" data-id="${n.id}">${t('ha.repin')}</button>` : ''}</div>${checking}`;
  }
  // 副机上看
  const nv = (view && view.node) || {};
  if (r.kind === 'master') {
    return head + `<p>${r.lost ? esc(t('ha.card.masterLost', { m: Math.floor((nv.leaseAgo || 0) / 60) })) : t('ha.card.masterOk')}</p>
      <p>${esc(t('ha.card.mode') + modeText(view.mode))}</p>`;
  }
  if (!n.isLocal) return head + `<p>${r.kind === 'standby' ? esc(t('ha.card.standbyOther', { n: r.rank })) : t('ha.card.nodeOther')}</p>`;
  const rep = nv.replica && { asOf: nv.replica.asOf, verifiedAt: nv.replica.verifiedAt, err: nv.replica.verifyErr };
  const lines = [`<p>${esc(r.kind === 'standby' ? standbyLine({ replica: rep }, r.rank) : t('ha.card.nodeSelf'))}</p>`];
  // 只有服务端判定「该接任了」(确认模式到了确认时间、证据齐全)才给按钮;否则写原因
  if (nv.canTakeover) lines.push(`<div class="actions"><button class="btn sm danger" data-act="ha.takeover">${t('ha.takeover')}</button></div>`);
  else if (r.kind === 'standby' && nv.why && (nv.lost || nv.why === 'oldReplica')) lines.push(`<p class="why">${esc(whyText(nv.why))}</p>`);
  if (nv.evidence && nv.evidence.reason && nv.evidence.reason !== nv.why) lines.push(`<p class="muted small">${esc(t('ha.ev.' + nv.evidence.reason))}</p>`);
  if (nv.pubFp && r.kind === 'standby') lines.push(`<p class="muted small fp">${esc(t('ha.card.selfFp', { fp: nv.pubFp }))}</p>`);
  return head + lines.join('');
}

export function planText(it) {
  if (it.code === 'text') return trErr((it.args || {}).text || '');
  return fill('ha.plan.' + it.code, argsOf(it));
}

// ---- 设为主机 ----

const STAGES = ['prepare', 'export', 'send', 'grant', 'commit', 'done'];
const TERMINAL = ['done', 'failed', 'fenced'];

async function openSetMaster(id) {
  closeCard(false);
  let plan;
  try { plan = await get('ha/plan?id=' + id); } catch (e) { toast(e.message, 'err'); return; }
  const target = plan.targetName, self = plan.selfName;
  // 订阅地址能不能保持,以后端的结论(subKept:订阅主机名是域名、端口也能沿用)为准
  const dns = !(plan.subHost && plan.subKept) ? ''
    : plan.dnsAuto ? t('ha.sm.dnsAuto', { names: joinNames(plan.dnsNames), target, ip: plan.targetIp || '?' })
      : t('ha.sm.dns', { host: plan.subHost, target, ip: plan.targetIp || '?' });
  const items = [
    t('ha.sm.data', { target, self }),
    t('ha.sm.self', { self }),
    t('ha.sm.users', { self, target }),
    t('ha.sm.panel', { url: plan.newPanel }),
  ];
  if (plan.newReseller) items.push(t('ha.sm.reseller', { url: plan.newReseller }));
  if (dns) items.push(dns);
  const warn = (plan.warnings || []).map(w => `<li>${esc(planText(w))}</li>`).join('');
  const block = (plan.blockers || []).map(b => `<li>${esc(planText(b))}</li>`).join('');
  openModal(t('ha.sm.title', { target }), `
    <ul class="ha-plan">${items.map(x => `<li>${esc(x)}</li>`).join('')}</ul>
    ${warn ? `<div class="ha-note warn"><b>${t('ha.sm.warn')}</b><ul>${warn}</ul></div>` : ''}
    ${block ? `<div class="ha-note bad"><b>${t('ha.sm.blocked')}</b><ul>${block}</ul></div>` : ''}
    ${plan.plainAck ? `<label class="field check ha-ack"><input type="checkbox" id="ha-plain"><span>${esc(t('ha.sm.plainAck'))}</span></label>` : ''}
    <div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-pw" type="password" autocomplete="current-password">`, t('ha.passwordHelp'))}
      ${plan.totp ? field(t('ha.code'), `<input id="ha-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>`, async () => {
    const plain = document.getElementById('ha-plain');
    const r = await post('ha/set-master', {
      id: Number(id), password: document.getElementById('ha-pw').value, code: (document.getElementById('ha-code') || {}).value || '',
      allowPlain: !!(plain && plain.checked),
    }, SLOW);
    showProgress(r, plan);
    const e = new Error('');
    e.keep = true;
    throw e; // 不关窗口:接着显示进度
  }, { saveText: t('ha.setMaster'), danger: false });
  const save = document.getElementById('modal-save');
  const plain = document.getElementById('ha-plain');
  const sync = () => { save.disabled = !plan.ok || !!(plan.plainAck && !(plain && plain.checked)); };
  sync();
  if (plain) plain.addEventListener('change', sync);
}

function stepsHTML(st, plan) {
  const stage = st.stage === 'unknown' ? 'commit' : st.stage;
  const cur = STAGES.indexOf(stage);
  const label = s => t('ha.stage.' + s, { target: plan.targetName, self: plan.selfName });
  return `<ol class="ha-steps">${STAGES.map((s, i) => {
    let cls = 'pending', icon = '○';
    if (i < cur || stage === 'done') { cls = 'done'; icon = '✓'; }
    else if (i === cur) { cls = st.stage === 'unknown' ? 'cur warn' : 'cur'; icon = st.stage === 'unknown' ? '?' : '<span class="spin"></span>'; }
    return `<li class="${cls}"><span class="ic">${icon}</span>${esc(label(s))}</li>`;
  }).join('')}</ol>`;
}

// freezeMinutes 这次换主机写操作最长暂停多久(冻结时长按库大小算,后端给截止时间)
const freezeMinutes = s => (s.freezeUntil && s.startedAt ? Math.max(1, Math.ceil((s.freezeUntil - s.startedAt) / 60)) : 15);

// progressHTML 进度框正文(不含「放弃」那一块:它有输入框,不能跟着每次刷新重画)
export function progressHTML(s, plan, extra = '') {
  if (s.stage === 'failed') {
    return `<div class="ha-note bad"><b>${t('ha.sm.failed')}</b><p>${esc(trErr(s.err || ''))}</p></div><p>${t('ha.sm.failedKept')}</p>`;
  }
  if (s.stage === 'fenced') {
    return `<div class="ha-note warn"><b>${esc(t('ha.sm.fenced', { target: plan.targetName }))}</b><p>${esc(t('ha.sm.fencedHow', { target: plan.targetName }))}</p></div>`;
  }
  if (s.stage === 'done') {
    let html = `<div class="ha-note ok"><b>${esc(t('ha.sm.done', { target: plan.targetName }))}</b></div>
      <p>${t('ha.sm.newPanel')} <a href="${esc(s.newPanel)}" target="_blank" rel="noopener">${esc(s.newPanel)}</a></p>`;
    if (s.newReseller) html += `<p>${t('ha.sm.newReseller')} <a href="${esc(s.newReseller)}" target="_blank" rel="noopener">${esc(s.newReseller)}</a></p>`;
    html += `<p class="muted small">${t('ha.sm.relogin')}</p>`;
    const left = [];
    if (s.subKept && s.subHost && !s.dnsAuto) left.push(t('ha.sm.leftDns', { host: s.subHost, ip: s.targetIp || '?' }));
    if (left.length) html += `<p><b>${t('ha.sm.left')}</b></p><ul class="ha-plan">${left.map(x => `<li>${esc(x)}</li>`).join('')}</ul>`;
    return html;
  }
  let html = `<p class="hint">${esc(t('ha.sm.running', { m: freezeMinutes(s) }))}</p>` + stepsHTML(s, plan);
  if (s.stage === 'unknown' || s.unknown) {
    html += `<div class="ha-note warn"><b>${esc(t('ha.sm.unknown', { target: plan.targetName }))}</b>${s.err ? `<p>${esc(trErr(s.err))}</p>` : ''}
      <p>${esc(t('ha.sm.unknownHow', { t: fmtTime(s.freezeUntil), target: plan.targetName }))}</p></div>`;
  } else if (s.err) {
    // 还没到终态,但上一步出过错(比如提交没回话、正在重试):把原因和最晚什么时候有结论写出来
    html += `<div class="ha-note warn"><p>${esc(trErr(s.err))}</p>${s.freezeUntil ? `<p class="muted small">${esc(t('ha.sm.frozenUntil', { t: fmtTime(s.freezeUntil) }))}</p>` : ''}</div>`;
  }
  return html + extra;
}

function abandonHTML(plan) {
  return `<div class="ha-abandon"><p class="muted small">${esc(t('ha.sm.abandonHelp', { target: plan.targetName }))}</p>
    <div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-ab-pw" type="password" autocomplete="current-password">`)}
      ${plan.totp ? field(t('ha.code'), `<input id="ha-ab-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>
    <div class="row"><button class="btn sm danger" data-act="ha.abandon">${t('ha.sm.abandon')}</button></div><p class="danger-text small ha-ab-err"></p></div>`;
}

let progTok = 0; // 每开一个进度框加一:旧的轮询看到对不上就停
let progSwitch = null;

export function showProgress(st, plan) {
  const tok = ++progTok;
  const gen = modalGen();
  progSwitch = st;
  const modal = document.getElementById('modal');
  document.getElementById('modal-save').hidden = true;
  document.getElementById('modal-cancel').textContent = t('common.close');
  const body = document.getElementById('modal-body');
  body.innerHTML = '';
  const prog = document.createElement('div');
  const ab = document.createElement('div');
  body.appendChild(prog);
  body.appendChild(ab);
  // 进度框关掉、或者窗口换成了别的对话框,就不再写(#modal 是所有对话框共用的)
  const alive = () => tok === progTok && gen === modalGen() && !modal.hidden;
  const render = (s, extra = '') => {
    if (!alive()) return;
    const html = progressHTML(s, plan, extra);
    if (prog.innerHTML !== html) prog.innerHTML = html;
    const wantAb = (s.stage === 'unknown' || !!s.unknown) && !TERMINAL.includes(s.stage);
    if (wantAb && !ab.dataset.on) { ab.dataset.on = '1'; ab.innerHTML = abandonHTML(plan); }
    else if (!wantAb && ab.dataset.on) { delete ab.dataset.on; ab.innerHTML = ''; }
  };
  render(st);
  const started = Date.now();
  // 最长等到冻结截止后再多 10 分钟(冻结时长按库大小算,最长一小时),至少 20 分钟
  const limit = Math.max(20 * 60 * 1000, ((st.freezeUntil || 0) * 1000 - started) + 10 * 60 * 1000);
  const poll = async () => {
    if (!alive()) return;
    let s = null;
    try {
      const v = await get('ha', { timeout: 8000 });
      s = v.switch || (v.node && v.node.switch) || null;
    } catch { /* 本机正在重启(变成副机),过一会儿再问 */ }
    if (!alive()) return; // 等回话的时候窗口关了或换了
    if (s && s.id === st.id) {
      progSwitch = s;
      render(s);
      if (TERMINAL.includes(s.stage)) return;
    } else if (!s) {
      render(progSwitch, `<p class="muted small">${t('ha.sm.restarting')}</p>`);
    }
    if (Date.now() - started > limit) return;
    setTimeout(poll, 1500);
  };
  setTimeout(poll, 1000);
  return { alive };
}

// ---- 备用:接任为主机 ----

function openTakeover() {
  closeCard(false);
  const nv = (view && view.node) || {};
  const at = nv.replica ? fmtTime(nv.replica.asOf) : '—';
  openModal(t('ha.to.title'), `
    <ul class="ha-plan">
      <li>${esc(t('ha.to.data', { t: at, name: view.masterName || '' }))}</li>
      <li>${esc(t('ha.to.lost'))}</li>
      <li>${esc(t('ha.to.restart'))}</li>
      <li>${esc(t('ha.to.login'))}</li>
      <li>${esc(t('ha.to.former'))}</li>
    </ul>
    <div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-pw" type="password" autocomplete="current-password">`, t('ha.passwordHelpLocal'))}
      ${view.totp ? field(t('ha.code'), `<input id="ha-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>`, async () => {
    // 服务端在请求里同步组好新库才回话,大库要几分钟
    await post('ha/takeover', { password: document.getElementById('ha-pw').value, code: (document.getElementById('ha-code') || {}).value || '' }, LONG);
    toast(t('ha.to.started'), 'ok');
  }, { saveText: t('ha.takeover'), danger: true });
}

// ---- 二次确认(改主机容灾设置、重新信任) ----

function askPassword(title, onOk, intro = '') {
  openModal(title, `${intro}<div class="form-grid">
      ${field(t('ha.password'), `<input id="ha-pw" type="password" autocomplete="current-password">`, t('ha.passwordHelp'))}
      ${view && view.totp ? field(t('ha.code'), `<input id="ha-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6">`) : ''}
    </div>`, async () => onOk(document.getElementById('ha-pw').value, (document.getElementById('ha-code') || {}).value || ''),
  { saveText: t('common.confirm') });
}

function openRepin(id) {
  closeCard(false);
  const info = infoOf(id) || {};
  const intro = `<p>${esc(t('ha.repinIntro', { name: nameOf(id), old: info.pinnedFp || '—', now: info.pubFp || '—' }))}</p>`;
  askPassword(t('ha.repinTitle', { name: nameOf(id) }), async (password, code) => {
    const r = await post('ha/repin', { id, password, code }, SLOW);
    await loadHA();
    const fp = (r && r.pinnedFp) || (infoOf(id) || {}).pinnedFp || '';
    toast(fp ? t('ha.repinnedFp', { fp }) : t('ha.repinned'), 'ok');
    refresh();
  }, intro);
}

registerActions({
  'ha.card': (id, el) => openCard(id, el),
  'ha.setMaster': id => openSetMaster(id),
  'ha.takeover': async () => { if (!view) await loadHA(); if (view) openTakeover(); }, // 顶部提示条上的按钮:别的页面没取过状态
  'ha.toSettings': () => { closeCard(false); try { sessionStorage.setItem('m-ui.scrollTo', 'ha-card'); } catch {} location.hash = '#/settings'; },
  'ha.repin': id => openRepin(Number(id)),
  'ha.abandon': async (_, btn) => {
    const box = btn.closest('.ha-abandon');
    const errEl = box && box.querySelector('.ha-ab-err');
    btn.disabled = true;
    try {
      await post('ha/switch/abandon', {
        id: (progSwitch && progSwitch.id) || '',
        password: (document.getElementById('ha-ab-pw') || {}).value || '', code: (document.getElementById('ha-ab-code') || {}).value || '',
      }, SLOW);
      if (errEl) errEl.textContent = '';
      toast(t('ha.sm.abandoned'), 'ok');
    } catch (e) { if (errEl) errEl.textContent = e.message; }
    finally { btn.disabled = false; }
  },
  'ha.notice': async (id, btn) => {
    btn.disabled = true;
    // 合并显示的一条带着同类的全部 id,一起标为已读
    await Promise.all(String(id).split(',').filter(Boolean).map(x => post('ha/notice', { id: x }).catch(() => {})));
    const bar = btn.closest('.alert-bar');
    if (bar) bar.remove();
  },
  'ha.noticesMore': () => { noticesOpen = !noticesOpen; haNotices(lastStatus); },
  'ha.pause': async min => {
    try { await post('ha/pause', { minutes: Number(min) }); await loadHA(); renderHASettings(); toast(t('set.saved'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'ha.save': () => saveSettings(),
  'ha.dnsTest': async (_, btn) => {
    btn.disabled = true;
    const out = document.getElementById('ha-dns-out');
    try {
      // 服务端最多等 Cloudflare 60 秒
      const r = await post('ha/dns-test', { token: (document.getElementById('ha-dnsToken') || {}).value || '', names: (document.getElementById('ha-dnsNames') || {}).value || '' }, SLOW);
      if (out) out.innerHTML = dnsTestHTML(r);
    } catch (e) { if (out) out.innerHTML = `<p class="danger-text">${esc(e.message)}</p>`; }
    finally { btn.disabled = false; }
  },
});

// ---- 页面顶部的提示 ----

const MAX_BARS = 3; // 一次性提示最多显示几条,其余折叠
const WARN_KINDS = ['switchFailed', 'demoted', 'fenced', 'versionKeep'];
let noticesOpen = false;
let lastStatus = null;

// groupNotices 同类提示合并成一条(显示最近一条,带次数);已有未读的「变为副机」时,「已作为副机接入」并进它。
// 要警示的排在前面,其余按时间新到旧。
export function groupNotices(list) {
  const ns = (list || []).slice().sort((a, b) => (b.at || 0) - (a.at || 0));
  const hasDemoted = ns.some(n => n.kind === 'demoted');
  const groups = new Map();
  for (const n of ns) {
    const kind = n.kind === 'joined' && hasDemoted ? 'demoted' : n.kind;
    let g = groups.get(kind);
    if (!g) { g = { kind, latest: null, ids: [], count: 0 }; groups.set(kind, g); }
    g.ids.push(n.id);
    if (n.kind !== kind) continue; // 并进来的「已接入」不单算一次
    if (!g.latest) g.latest = n;
    g.count += Math.max(1, Number(n.count) || 1);
  }
  const warnFirst = g => (WARN_KINDS.includes(g.kind) ? 0 : 1);
  return [...groups.values()].filter(g => g.latest)
    .sort((a, b) => warnFirst(a) - warnFirst(b) || (b.latest.at || 0) - (a.latest.at || 0));
}

// haNotices 由 app.js 在每次刷新状态后调用:一次性提示(点"知道了"清掉)+ 进行中的状态(冻结、主机失联)。
export function haNotices(status) {
  lastStatus = status;
  const main = document.querySelector('.main'), page = document.getElementById('page');
  if (!main || !page) return;
  const ha = (status && status.ha) || null;
  const want = new Map();
  if (ha) {
    if (ha.frozenUntil) {
      const txt = ha.frozenTarget ? t('ha.bar.frozen', { name: ha.frozenTarget, t: fmtTime(ha.frozenUntil) }) : trErr(ha.frozenText || '');
      want.set('ha-frozen', { cls: 'warn', html: `<span><b>${esc(txt)}</b></span>` });
    }
    if (ha.lost && ha.mode && ha.mode !== 'off') {
      const m = Math.floor(ha.lost / 60);
      // 接任按钮只在服务端判定可以接任时给(确认模式到了确认时间、证据齐全);否则写为什么不接任
      const why = !ha.canTakeover && ha.why ? ` <span class="muted">${esc(whyText(ha.why))}</span>` : '';
      want.set('ha-lost', {
        cls: '', html: `<span><b>${esc(t('ha.bar.lost', { name: ha.masterName || '', m }))}</b> ${esc(ha.rank ? t('ha.bar.lostStandby', { n: ha.rank }) : '')}${why}</span>${ha.canTakeover ? `<button class="btn sm danger" data-act="ha.takeover">${t('ha.takeover')}</button>` : ''}`,
      });
    }
    const groups = groupNotices(ha.notices);
    const shown = noticesOpen ? groups : groups.slice(0, MAX_BARS);
    for (const g of shown) {
      const n = g.latest;
      const times = g.count > 1 ? ` <span class="muted">${esc(t('ha.n.times', { n: g.count }))}</span>` : '';
      want.set('ha-n-' + g.kind, {
        cls: WARN_KINDS.includes(g.kind) ? 'warn' : 'ha-info',
        html: `<span>${noticeHTML(n)}${times}</span><button class="btn sm ghost" data-act="ha.notice" data-id="${esc(g.ids.join(','))}">${t('reach.dismiss')}</button>`,
      });
    }
    if (groups.length > MAX_BARS) {
      want.set('ha-more', {
        cls: 'ha-info', html: `<span>${esc(noticesOpen ? t('ha.bar.fold') : t('ha.bar.more', { n: groups.length - MAX_BARS }))}</span><button class="btn sm ghost" data-act="ha.noticesMore">${noticesOpen ? t('ha.bar.foldBtn') : t('ha.bar.moreBtn')}</button>`,
      });
    }
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

// changesText DNS 改动逐条「名字 类型:原值 → 新值」,多了截断
function changesText(list, max = 4) {
  const cs = list || [];
  const s = cs.slice(0, max).map(c => `${c.name} ${c.type}: ${c.from || '—'} → ${c.to || '—'}`).join(t('reach.sep'));
  return cs.length > max ? s + ' ' + t('ha.n.andMore', { n: cs.length - max }) : s;
}

// dnsHTML 提示里 DNS 那一句:按后端给的三态(全部改好 / 部分 / 失败)写,并列出原值 → 新值;
// 订阅地址能不能保持(subKept)也用后端的结论,不在前端猜主机名是不是 IP
export function dnsHTML(n) {
  const state = n.dnsState || (n.dns ? 'done' : '');
  const changes = (n.changes || []).length ? ' ' + t('ha.n.dnsChanges', { list: changesText(n.changes) }) : '';
  switch (state) {
    case 'done': return esc((n.subKept && n.host ? t('ha.n.dnsDone', { host: n.host, ip: n.ip || '' }) : t('ha.n.dnsDoneOther')) + changes);
    case 'partial': return esc(t('ha.n.dnsPartial') + changes + (n.subKept && n.host && !n.subDone ? ' ' + t('ha.n.dnsTodo', { host: n.host, ip: n.ip || '' }) : ''));
    case 'failed': return esc(t('ha.n.dnsFailed') + changes + (n.subKept && n.host ? ' ' + t('ha.n.dnsTodo', { host: n.host, ip: n.ip || '' }) : ''));
  }
  return n.subKept && n.host ? esc(t('ha.n.dnsTodo', { host: n.host, ip: n.ip || '' })) : '';
}

// idwText 身份迁移告警(后端给代码 + 参数;code 为 text 的是旧写法,原文再翻译)
const idwText = it => (it.code === 'text' ? trErr((it.args || {}).text || '') : fill('ha.idw.' + it.code, argsOf(it)));

const link = url => `<a href="${esc(url)}" target="_blank" rel="noopener">${esc(url)}</a>`;

export function noticeHTML(n) {
  const ws = [...(n.warn || []).map(w => trErr(w)), ...(n.items || []).map(idwText)];
  const warn = ws.length ? ' ' + ws.map(esc).join(' ') : '';
  const dns = dnsHTML(n);
  const reseller = n.reseller ? ` ${esc(t('ha.n.reseller'))} ${link(n.reseller)}` : '';
  switch (n.kind) {
    case 'became': {
      const take = n.how === 'takeover' || n.reason === 'takenOver';
      let s = `<b>${esc(t(take ? 'ha.n.tookOver' : 'ha.n.became', { name: n.name || '' }))}</b>`;
      if (take) s += ' ' + esc(t('ha.n.tookOverData'));
      return s + (dns ? ' ' + dns : '') + reseller + warn;
    }
    case 'formerBack': return `<b>${esc(t('ha.n.formerBack', { name: n.name || '' }))}</b>`;
    case 'demoted': {
      const why = n.reason && has('ha.n.demoted.' + n.reason) ? n.reason : n.yield ? 'yielded' : 'takenOver';
      let s = `<b>${esc(t('ha.n.demoted.' + why, { name: n.name || '' }))}</b>`;
      if (Number(n.n) < 0) s += ' ' + esc(t('ha.n.divergentUnknown', { file: n.file || '' }));
      else if (n.n) s += ' ' + esc(t(why === 'yielded' ? 'ha.n.divergentYield' : 'ha.n.divergent', { n: n.n, file: n.file || '' }));
      return s + (dns ? ' ' + dns : '') + warn;
    }
    case 'rolledBack': return `<b>${esc(t('ha.n.rolledBack', { n: n.epoch || '' }))}</b>${warn}`;
    case 'joined': return `<b>${esc(t('ha.n.joined'))}</b>`;
    case 'switched': return `<b>${esc(t('ha.n.switched', { name: n.name || '' }))}</b> ${esc(t('ha.sm.newPanel'))} ${link(n.url || '')}${reseller}${dns ? ' ' + dns : ''}${warn}`;
    case 'switchFailed': return `<b>${esc(t(n.reason === 'fenced' ? 'ha.n.fenced' : 'ha.n.switchFailed', { name: n.name || '' }))}</b>${warn}`;
    case 'fenced': return `<b>${esc(t('ha.n.fenced', { name: n.name || '' }))}</b>${warn}`;
    case 'dnsReverted': return `<b>${esc(t('ha.n.dnsReverted'))}</b> ${esc(changesText(n.changes))}${warn}`;
    case 'standbyNew': return `<b>${esc(t('ha.n.standbyNew', { name: n.name || '' }))}</b>${warn}`;
    case 'versionKeep': return `<b>${esc(t('ha.n.versionKeep', { name: n.name || '' }))}</b>${warn}`;
  }
  return esc(n.kind);
}

// ---- 设置页:主机容灾 ----

const MODES = ['off', 'confirm', 'auto'];
const LEVEL_ICON = { ok: '✓', warn: '!', bad: '✗', info: '·' };

// readyText 就绪检查一条:键是 ha.ready.<代码>.<级别>,名单(names)按界面语言拼
export const readyText = ch => fill('ha.ready.' + ch.code + '.' + ch.level, argsOf(ch));

// renderHASettings 设置页里的「主机容灾」卡片(只在主机上;副机上说明一句到主机改,并显示本机的备用公钥指纹)。
export async function renderHASettings(container) {
  const host = container || document.getElementById('ha-card');
  if (!host) return;
  const v = await get('ha?full=1').catch(() => null);
  if (!v) { host.remove(); return; }
  view = v;
  const override = v.dnsApiOverride ? `<p class="ha-note warn">${esc(t('ha.set.dnsOverride', { url: v.dnsApiOverride }))}</p>` : '';
  if (v.role !== 'master') {
    const fp = v.node && v.node.pubFp;
    host.innerHTML = `<div class="card-head"><h2>${t('ha.title')}</h2></div><p class="hint">${esc(t('ha.set.nodeHint', { mode: modeText(v.mode) }))}</p>
      ${fp ? `<p class="hint fp">${esc(t('ha.set.nodeFp', { fp }))}</p>` : ''}${override}`;
    return;
  }
  const c = v.config || {};
  const ready = (v.ready || []).map(ch => `<li class="lv-${esc(ch.level)}"><span class="ic">${LEVEL_ICON[ch.level] || '·'}</span><span>${esc(readyText(ch))}</span></li>`).join('');
  const paused = c.pauseUntil > Date.now() / 1000;
  host.innerHTML = `
    <div class="card-head"><h2>${t('ha.title')}</h2><button class="btn primary sm" data-act="ha.save">${t('common.save')}</button></div>
    <p class="hint">${t('ha.set.intro')}</p>
    ${override}
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
    ${(v.dropPending || []).length ? `<p class="warn-text small">${esc(t('ha.set.dropPending', { names: joinNames(v.dropPending.map(d => d.name || '#' + d.id)) }))}</p>` : ''}
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
        <label class="field check"><input type="checkbox" id="ha-allowPlain" ${c.allowPlain ? 'checked' : ''}><span>${t('ha.set.allowPlain')}</span><span class="help">${esc(t('ha.set.allowPlainHelp'))}</span></label>
      </div>
      <h4 class="ha-sub">${t('ha.set.dns')}</h4>
      <p class="hint">${t('ha.set.dnsHelp')}</p>
      <div class="form-grid">
        ${field(t('ha.set.dnsToken'), `<input id="ha-dnsToken" type="password" autocomplete="off" placeholder="${esc(c.dnsTokenSet ? t('ha.set.dnsTokenKeep') : '')}">${cfTokenLink()}`, t('ha.set.dnsTokenHelp'))}
        ${field(t('ha.set.dnsNames'), `<input id="ha-dnsNames" value="${esc(c.dnsNames || '')}" placeholder="sub.example.com">`, t('ha.set.dnsNamesHelp'))}
      </div>
      <div class="row"><button class="btn sm" data-act="ha.dnsTest">${t('ha.set.dnsTest')}</button>${c.dnsTokenSet ? ` <label class="field check inline"><input type="checkbox" id="ha-dnsClear"><span>${t('ha.set.dnsClear')}</span></label>` : ''}</div>
      <div id="ha-dns-out"></div>
    </details>`;
  host.querySelectorAll('input[name="ha-mode"]').forEach(r => r.addEventListener('change', () => {
    host.querySelectorAll('.ha-modes label').forEach(l => l.classList.toggle('sel', l.querySelector('input').checked));
  }));
}

// dnsTestHTML「测试令牌与记录」的结果:有错误先写错误(令牌无效、区域找不到时记录本来就是空的,不能说成「没找到记录」)
export function dnsTestHTML(r) {
  const recs = (r && r.records) || [];
  const err = r && r.error ? `<p class="danger-text small">${esc(trErr(r.error))}</p>` : '';
  if (!recs.length) return err || `<p class="muted small">${t('ha.dns.none')}</p>`;
  return err + `<ul class="ha-check">${recs.map(x => `<li class="lv-${x.match ? 'ok' : 'info'}"><span class="ic">${x.match ? '✓' : '·'}</span><span>${esc(x.name)} ${esc(x.type)} ${esc(x.content)}${x.proxied ? ' (proxied)' : ''} — ${esc(x.match ? t('ha.dns.match') : t('ha.dns.skip'))}</span></li>`).join('')}</ul>`;
}

function saveSettings() {
  const num = id => Number((document.getElementById(id) || {}).value) || 0;
  const body = {
    mode: (document.querySelector('input[name="ha-mode"]:checked') || {}).value || 'off',
    standbys: num('ha-standbys') || 2,
    confirmMinutes: num('ha-confirmMinutes') || 3, autoMinutes: num('ha-autoMinutes') || 10,
    rankStepMinutes: num('ha-rankStepMinutes') || 3, replicaMinutes: num('ha-replicaMinutes') || 5,
    witness: !!(document.getElementById('ha-witness') || {}).checked,
    allowPlain: !!(document.getElementById('ha-allowPlain') || {}).checked,
    dnsNames: (document.getElementById('ha-dnsNames') || {}).value || '',
    dnsToken: (document.getElementById('ha-dnsToken') || {}).value || '',
    clearDnsToken: !!(document.getElementById('ha-dnsClear') || {}).checked,
  };
  askPassword(t('ha.set.confirmSave'), async (password, code) => {
    // 首次开启要进纪元、推状态给全部副机,可能要一会儿
    await post('ha', { ...body, password, code }, SLOW);
    toast(t('set.saved'), 'ok');
    renderHASettings();
  });
}

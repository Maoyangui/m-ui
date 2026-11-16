import { state, load, isReseller } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { lineItems, keysFromRefs, linePicker } from '../linepicker.js';
import { showUsersOfPlan } from './users.js';
import { esc, toast, confirm, openModal, registerActions, badge, field, empty, fv, fchk, switchRow } from '../ui.js';

export const title = () => t('plan.title');
export const subtitle = () => t('plan.subtitle');

// 套餐页:一个套餐一张卡。套餐就那么几个,是拿来卖、拿来套用的"商品",不是一行行的记录 ——
// 以前是一张十列的表,大半格子是「∞」「不改动」,一眼看不出每个套餐到底给多少。
// 卡片上:名字和说明 → 流量、时长两个大数 → 设备 / 限速 / 重置 / 线路 / 规则几项 → 几个人在用、编辑、删除。
export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="muted small" id="plans-count"></span><span class="grow"></span><button class="btn primary" data-act="plan.add"><i class="i i-plus"></i>${t('plan.add')}</button></div>
    <div class="plan-grid" id="plans-body"></div>`;
  if (!isReseller()) rules = await get('rules').catch(() => []) || [];
  renderRows();
}

let rules = []; // 主面板的限速规则(套餐可以选跟哪几条)

const ruleChips = p => (p.ruleIds || []).map(id => rules.find(r => r.id === id)).filter(Boolean).map(r => badge(r.name, r.enabled ? 'primary' : '')).join(' ');
const planLineIds = p => (Array.isArray(p.lineIds) ? p.lineIds : (p.lineIds ? JSON.parse(p.lineIds) : []));

function planCard(p) {
  const ids = planLineIds(p);
  const names = ids.map(id => (state.lines.find(l => l.id === id) || {}).name).filter(Boolean);
  const big = (n, unit) => (n ? `${n}<small>${esc(unit)}</small>` : esc(t('common.unlimited')));
  const speed = (p.speedUp || p.speedDown) ? `↑ ${p.speedUp || '∞'} · ↓ ${p.speedDown || '∞'} Mbps` : t('common.unlimited');
  const row = (k, v) => `<dt>${esc(k)}</dt><dd>${v}</dd>`;
  const chips = ruleChips(p);
  return `<article class="plan-card">
    <header><div class="pc-name"><b title="${esc(p.name)}">${esc(p.name)}</b>${p.desc ? `<span title="${esc(p.desc)}">${esc(p.desc)}</span>` : ''}</div></header>
    <div class="pc-main">
      <div><b>${big(p.volumeGb, 'GB')}</b><span>${esc(t('plan.quota'))}</span></div>
      <div><b>${big(p.days, t('common.day'))}</b><span>${esc(t('plan.c.days'))}${p.days && p.firstUse ? ' · ' + esc(t('plan.firstUseShort')) : ''}</span></div>
    </div>
    <dl class="pc-attrs">
      ${row(t('plan.f.device'), p.deviceLimit ? esc(t('plan.c.devN', { n: p.deviceLimit })) : esc(t('common.unlimited')))}
      ${row(t('user.speed'), esc(speed))}
      ${row(t('plan.c.reset'), esc(p.autoReset ? t('plan.c.resetN', { n: p.resetDays }) : t('plan.c.noReset')))}
      ${row(t('plan.lines'), names.length ? `<span title="${esc(names.join('\n'))}">${esc(names.slice(0, 2).join('、'))}${names.length > 2 ? ` <span class="muted">+${names.length - 2}</span>` : ''}</span>` : `<span class="muted">${esc(t('plan.keepLines'))}</span>`)}
      ${isReseller() || !chips ? '' : row(t('plan.rules'), chips)}
      ${p.price > 0 ? row(t('plan.c.price'), esc(String(p.price))) : ''}
    </dl>
    <footer>
      ${p.userCount ? `<a href="#" data-act="plan.users" data-id="${p.id}">${esc(t('plan.c.usersN', { n: p.userCount }))}</a>` : `<span class="muted">${esc(t('plan.c.noUsers'))}</span>`}
      <span class="grow"></span>
      <button class="btn sm" data-act="plan.edit" data-id="${p.id}">${t('common.edit')}</button>
      <button class="btn sm danger" data-act="plan.del" data-id="${p.id}">${t('common.delete')}</button>
    </footer>
  </article>`;
}

function renderRows() {
  const body = document.getElementById('plans-body');
  if (!body) return;
  const cnt = document.getElementById('plans-count');
  if (cnt) cnt.textContent = state.plans.length ? t('plan.c.count', { n: state.plans.length }) : '';
  if (!state.plans.length) {
    body.innerHTML = `<div class="empty-guide plan-empty"><p>${esc(t('plan.empty'))}</p><button class="btn primary" data-act="plan.add">${t('plan.add')}</button></div>`;
    return;
  }
  body.innerHTML = state.plans.map(planCard).join('');
}

// 规则的条件写成一句话,勾选的时候看得出是什么规则
const ruleCond = r => r.kind === 'burst'
  ? t('rule.condBurst', { win: r.windowMin, gb: r.thresholdGb, pen: r.penaltyMin })
  : `${r.start} – ${r.end}`;

export async function editPlan(id) {
  if (!isReseller()) rules = await get('rules').catch(() => rules) || [];
  const p = id ? state.plans.find(x => x.id === id) : { volumeGb: 100, days: 30, resetDays: 30, ruleIds: [] };
  const ids = Array.isArray(p.lineIds) ? p.lineIds : (p.lineIds ? JSON.parse(p.lineIds) : []);
  let nodesBy = {};
  try { nodesBy = typeof p.lineNodes === 'string' ? JSON.parse(p.lineNodes || '{}') : (p.lineNodes || {}); } catch { nodesBy = {}; }
  const refs = ids.map(id => ({ lineId: id, nodeIds: nodesBy[String(id)] || [] }));
  let picker = null;
  // 版式:通栏从上到下 —— 名称与说明一行;「额度与限速」六个数一行(0 = 不限只说一遍,不在每栏下面各写一行);
  // 两个开关各是一行带说明的开关(周期重置的天数就在那一行里);然后是线路、限速规则。
  // 以前所有栏挤在一个四列格子里,每栏下面的小字长短不一,勾选框和输入框混排,上下对不齐
  const ruleRow = r => `<label class="${r.enabled ? '' : 'muted'}"><input type="checkbox" class="plan-rule" value="${r.id}" ${r.allUsers ? 'checked disabled' : ((p.ruleIds || []).includes(r.id) ? 'checked' : '')}> ${esc(r.name)} <span class="muted small">${esc(ruleCond(r))} · ${r.upMbps || '—'}/${r.downMbps || '—'} M${r.enabled ? '' : ' · ' + t('common.disabled')}${r.allUsers ? ' · ' + t('plan.ruleAll') : ''}</span></label>`;
  openModal(id ? t('plan.edit') : t('plan.add'), `<div class="pl-form">
    <div class="form-grid pl-head">
      ${field(t('common.name'), `<input id="f-name" value="${esc(p.name || '')}" placeholder="${t('plan.namePh')}">`)}
      ${field(t('plan.desc'), `<input id="f-desc" value="${esc(p.desc || '')}">`)}
    </div>
    <h3 class="form-sec">${t('user.tab.quota')}</h3>
    <div class="form-grid pl-nums">
      ${field(t('plan.quota') + ' GB', `<input id="f-vol" type="number" min="0" step="any" value="${p.volumeGb || 0}">`)}
      ${field(t('plan.days'), `<input id="f-days" type="number" min="0" value="${p.days || 0}">`)}
      ${field(t('plan.f.device'), `<input id="f-device" type="number" min="0" value="${p.deviceLimit || 0}">`)}
      ${field(t('plan.f.up'), `<input id="f-up" type="number" min="0" value="${p.speedUp || 0}">`)}
      ${field(t('plan.f.down'), `<input id="f-down" type="number" min="0" value="${p.speedDown || 0}">`)}
      ${field(t('plan.price'), `<input id="f-price" type="number" min="0" step="any" value="${p.price || 0}" title="${esc(t('plan.priceHelp'))}">`)}
    </div>
    <p class="hint">${esc(t('plan.numsHelp'))}</p>
    <div class="pl-opts">
      <div class="sw-row"><div><b>${esc(t('user.f.autoReset'))}</b><small>${esc(t('plan.autoResetHelp'))}</small></div>
        <span class="pl-every">${esc(t('plan.everyA'))}<input id="f-resetdays" type="number" min="1" value="${p.resetDays || 30}" aria-label="${esc(t('user.f.resetDays'))}">${esc(t('plan.everyB'))}</span>
        <label class="switch"><input type="checkbox" id="f-autoreset" ${p.autoReset ? 'checked' : ''}><span></span></label></div>
      ${switchRow('f-firstuse', t('plan.firstUse'), !!p.firstUse, t('plan.firstUseHelp'))}
    </div>
    <h3 class="form-sec">${t('plan.lines')}</h3>
    <div id="f-lines"></div>
    <p class="hint">${esc(t('plan.linesHelp'))}</p>
    ${isReseller() ? '' : `<h3 class="form-sec">${t('plan.rules')}</h3>
      ${rules.length ? `<div class="check-list one-col">${rules.map(ruleRow).join('')}</div>` : `<p class="muted small">${t('plan.rulesNone')}</p>`}
      <p class="hint">${esc(t('plan.rulesHelp'))}</p>`}
  </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), desc: fv('f-desc'), volumeGb: Number(fv('f-vol')), days: Number(fv('f-days')),
      deviceLimit: Number(fv('f-device')), speedUp: Number(fv('f-up')), speedDown: Number(fv('f-down')),
      autoReset: fchk('f-autoreset'), resetDays: Number(fv('f-resetdays')), firstUse: fchk('f-firstuse'),
      price: Math.max(0, Number(fv('f-price')) || 0),
    };
    if (!isReseller()) body.ruleIds = [...document.querySelectorAll('.plan-rule:checked:not(:disabled)')].map(c => Number(c.value));
    const picked = picker.read();
    body.lineIds = picked.map(r => r.lineId);
    const nodes = {};
    picked.forEach(r => { if (r.nodeIds && r.nodeIds.length) nodes[String(r.lineId)] = r.nodeIds; });
    body.lineNodes = Object.keys(nodes).length ? nodes : null;
    if (id) await put('plans/' + id, body); else await post('plans', body);
    await load('plans');
    if (!isReseller()) rules = await get('rules').catch(() => rules) || [];
    renderRows();
    toast(id ? t('plan.updated') : t('plan.created'), 'ok');
  }, { size: 'xl' });
  // 周期重置没开时,天数那一格是灰的
  const ar = document.getElementById('f-autoreset'), rd = document.getElementById('f-resetdays');
  const syncReset = () => { rd.disabled = !ar.checked; };
  ar.addEventListener('change', syncReset);
  syncReset();
  const nodes = (Array.isArray(state.nodes) && state.nodes.length) ? state.nodes : (Array.isArray(state.status.nodes) ? state.status.nodes : []);
  const items = lineItems(state.lines, nodes, state.status.scope === 'reseller' ? (state.status.grants || []) : null);
  picker = linePicker(document.getElementById('f-lines'), { items, selected: keysFromRefs(refs, items) });
}

registerActions({
  'plan.add': () => editPlan(null),
  'plan.edit': id => editPlan(Number(id)),
  'plan.users': (id, _, e) => {
    if (e) e.preventDefault();
    const p = state.plans.find(x => x.id === Number(id));
    if (p) { showUsersOfPlan(p.id); location.hash = '#/users'; }
  },
  'plan.del': async id => {
    const p = state.plans.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: p.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('plans/' + id); await load('plans'); renderRows(); toast(t('plan.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});

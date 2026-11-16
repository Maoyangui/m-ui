// 规则页:时段限速 / 突发限速。规则由主机判定,命中时给用户加一条临时限速状态并同步到副机;
// 条件消失(走出时段 / 到期 / 规则停用)自动退回用户自己的限速。
import { state } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { userPicker, targetText } from '../userpicker.js';
import { esc, toast, confirm, openModal, openDrawer, registerActions, badge, field, empty, fv, fchk, fmtTime, fmtRelative, fieldBox, switchRow } from '../ui.js';

export const title = () => t('rule.title');
export const subtitle = () => t('rule.subtitle');
let resellers = [], plans = [];

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar intro"><p class="page-hint">${t('rule.hint')}</p><button class="btn primary" data-act="rule.add">${t('rule.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('rule.kind')}</th><th>${t('rule.cond')}</th><th>${t('rule.limit')}</th><th>${t('rule.targets')}</th><th>${t('rule.active')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="rules-body"></tbody>
    </table></div>`;
  await refresh();
}

async function refresh() {
  const [rules, rs, ps] = await Promise.all([get('rules').catch(() => []), get('resellers').catch(() => []), get('plans?all=1').catch(() => [])]);
  state.rules = rules || [];
  resellers = rs || [];
  plans = ps || [];
  renderRows();
}
export async function tick() { await refresh(); }

const daysText = days => {
  const list = String(days || '').split(',').filter(Boolean);
  if (!list.length || list.length === 7) return t('rule.everyDay');
  return t('rule.weekPrefix') + list.map(d => t('rule.day.' + d)).join('、');
};
const condText = r => r.kind === 'burst'
  ? t('rule.condBurst', { win: r.windowMin, gb: r.thresholdGb, pen: r.penaltyMin })
  : `${daysText(r.days)} ${r.start} – ${r.end}`;
const limitText = r => `${r.upMbps || '—'} / ${r.downMbps || '—'} M${r.tightenOnly ? ` <span class="muted small">· ${t('rule.tightenOnlyShort')}</span>` : ''}`;

function renderRows() {
  const body = document.getElementById('rules-body');
  if (!body) return;
  if (!state.rules.length) { body.innerHTML = `<tr><td colspan="8">${empty(t('rule.empty'))}</td></tr>`; return; }
  body.innerHTML = state.rules.map(r => `<tr class="${r.enabled ? '' : 'muted'}">
      <td class="primary-cell">${esc(r.name)}${r.remark ? `<div class="sub-cell">${esc(r.remark)}</div>` : ''}</td>
      <td>${badge(t('rule.kind.' + r.kind), r.kind === 'burst' ? 'warn' : 'primary')}</td>
      <td>${esc(condText(r))}</td>
      <td class="num">${limitText(r)}</td>
      <td>${esc(targetText(r, resellers, plans))}<div class="sub-cell">${t('rule.targetCount', { n: r.targetCount })}</div></td>
      <td class="num">${r.activeCount ? `<button class="btn sm" data-act="rule.states" data-id="${r.id}">${badge(r.activeCount, 'warn')} ${t('rule.viewActive')}</button>` : '<span class="muted">0</span>'}</td>
      <td><label class="switch" title="${r.enabled ? t('common.enabled') : t('common.disabled')}"><input type="checkbox" data-change="rule.toggle" data-id="${r.id}" ${r.enabled ? 'checked' : ''}><span></span></label></td>
      <td class="actions">
        <button class="btn sm" data-act="rule.edit" data-id="${r.id}">${t('common.edit')}</button>
        <button class="btn sm danger" data-act="rule.del" data-id="${r.id}">${t('common.delete')}</button>
      </td></tr>`).join('');
}

function editRule(id) {
  const r = id ? state.rules.find(x => x.id === id) : { kind: 'schedule', enabled: true, tightenOnly: true, days: '', start: '19:00', end: '23:00', windowMin: 10, thresholdGb: 1, penaltyMin: 20, upMbps: 0, downMbps: 20, userIds: [], resellerIds: [], planIds: [] };
  let kind = r.kind || 'schedule';
  let picker = null;
  const days = new Set(String(r.days || '').split(',').filter(Boolean).map(Number));
  // 版式:最上面一行选类型(右边是「启用」开关),下面按一句话的顺序分三段 —— 什么时候限、限到多少、对谁生效。
  // 星期是七个可点的小块(不是七个勾选框排一排);每段的说明只写一遍,放在那一段下面
  openModal(id ? t('rule.edit') : t('rule.add'), `<div class="rl-form">
    <div class="rl-top">
      <div class="seg" id="f-kind">${['schedule', 'burst'].map(k => `<button type="button" data-kind="${k}" class="${k === kind ? 'active' : ''}">${t('rule.kind.' + k)}</button>`).join('')}</div>
      <span class="hint" id="f-kind-help"></span>
      <label class="rl-on"><span>${t('common.enabled')}</span><span class="switch"><input type="checkbox" id="f-enabled" ${r.enabled !== false ? 'checked' : ''}><span></span></span></label>
    </div>
    <div class="form-grid cols-2">
      ${field(t('common.name'), `<input id="f-name" value="${esc(r.name || '')}" placeholder="${t('rule.namePh')}">`)}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(r.remark || '')}">`)}
    </div>
    <h3 class="form-sec">${t('rule.sec.when')}</h3>
    <div id="sec-schedule">
      <div class="rl-when">
        ${fieldBox(t('rule.days'), `<div class="day-pills">${[1, 2, 3, 4, 5, 6, 7].map(d => `<label class="day-pill"><input type="checkbox" class="f-day" value="${d}" ${!days.size || days.has(d) ? 'checked' : ''}><span>${t('rule.day.' + d)}</span></label>`).join('')}</div>`)}
        ${field(t('rule.start'), `<input id="f-start" type="time" value="${esc(r.start || '19:00')}">`)}
        ${field(t('rule.end'), `<input id="f-end" type="time" value="${esc(r.end || '23:00')}">`)}
      </div>
      <p class="hint">${esc(t('rule.daysHelp'))};${esc(t('rule.timeHelp'))}</p>
    </div>
    <div id="sec-burst">
      <div class="form-grid cols-3">
        ${field(t('rule.windowMin'), `<input id="f-window" type="number" min="1" max="1440" value="${r.windowMin || 10}">`)}
        ${field(t('rule.thresholdGb'), `<input id="f-threshold" type="number" min="0.01" step="0.1" value="${r.thresholdGb || 1}">`)}
        ${field(t('rule.penaltyMin'), `<input id="f-penalty" type="number" min="1" max="10080" value="${r.penaltyMin || 20}">`)}
      </div>
      <p class="hint">${esc(t('rule.burstHelp'))}</p>
    </div>
    <h3 class="form-sec">${t('rule.sec.limit')}</h3>
    <div class="rl-limit">
      ${field(t('rule.up'), `<input id="f-up" type="number" min="0" value="${r.upMbps || 0}">`)}
      ${field(t('rule.down'), `<input id="f-down" type="number" min="0" value="${r.downMbps || 0}">`)}
      ${switchRow('f-tighten', t('rule.tightenOnly'), r.tightenOnly !== false, t('rule.tightenOnlyHelp'))}
    </div>
    <p class="hint">${esc(t('rule.limitHelp'))}</p>
    <h3 class="form-sec">${t('rule.sec.who')}</h3>
    <div id="f-targets"></div>
    <p class="hint">${esc(t('rule.targetsHelp'))}</p>
  </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), remark: fv('f-remark'), kind, enabled: fchk('f-enabled'), tightenOnly: fchk('f-tighten'),
      upMbps: Number(fv('f-up')), downMbps: Number(fv('f-down')),
      days: [...document.querySelectorAll('.f-day:checked')].map(x => x.value).join(','),
      start: fv('f-start'), end: fv('f-end'),
      windowMin: Number(fv('f-window')), thresholdGb: Number(fv('f-threshold')), penaltyMin: Number(fv('f-penalty')),
      ...picker.read(),
    };
    if (id) await put('rules/' + id, body); else await post('rules', body);
    await refresh();
    toast(id ? t('rule.updated') : t('rule.created'), 'ok');
  }, { size: 'xl' });
  const showKind = () => {
    document.getElementById('sec-schedule').hidden = kind !== 'schedule';
    document.getElementById('sec-burst').hidden = kind !== 'burst';
    document.getElementById('f-kind-help').textContent = t('rule.kindHelp.' + kind);
    document.querySelectorAll('#f-kind [data-kind]').forEach(b => b.classList.toggle('active', b.dataset.kind === kind));
  };
  document.getElementById('f-kind').addEventListener('click', e => {
    const b = e.target.closest('[data-kind]');
    if (!b) return;
    kind = b.dataset.kind;
    showKind();
  });
  showKind();
  picker = userPicker(document.getElementById('f-targets'), { users: state.users || [], resellers, plans, selected: { allUsers: !!r.allUsers, userIds: r.userIds || [], resellerIds: r.resellerIds || [], planIds: r.planIds || [] } });
}

async function showStates(id) {
  const r = state.rules.find(x => x.id === id);
  const list = await get(`rules/${id}/states`).catch(() => []);
  const now = Math.floor(Date.now() / 1000);
  openDrawer(`${esc(r ? r.name : '')} · ${t('rule.active')}`, list.length
    ? `<div class="table-wrap"><table class="grid"><thead><tr><th>${t('user.title')}</th><th>${t('rule.reason')}</th><th>${t('rule.since')}</th><th>${t('rule.until')}</th></tr></thead><tbody>${list.map(s => `<tr>
        <td class="primary-cell">${esc(s.userName)}</td><td>${esc(s.reason || '')}</td><td>${fmtTime(s.since)} <span class="muted small">${fmtRelative(s.since)}</span></td>
        <td>${s.until ? `${fmtTime(s.until)} <span class="muted small">${t('rule.remaining', { n: Math.max(0, Math.ceil((s.until - now) / 60)) })}</span>` : t('rule.untilWindowEnd')}</td></tr>`).join('')}</tbody></table></div>`
    : `<p class="muted">${t('rule.activeNone')}</p>`, { wide: true });
}

registerActions({
  'rule.add': () => editRule(null),
  'rule.edit': id => editRule(Number(id)),
  'rule.states': id => showStates(Number(id)),
  'rule.toggle': async (id, input) => {
    const r = state.rules.find(x => x.id === Number(id));
    if (!r) return;
    if (input) input.disabled = true;
    try {
      await put('rules/' + id, { ...r, enabled: !r.enabled });
      await refresh();
      toast(t(r.enabled ? 'rule.disabledOk' : 'rule.enabledOk'), 'ok');
    } catch (e) {
      toast(e.message, 'err');
      if (input) { input.checked = !!r.enabled; input.disabled = false; } // 没改成:开关拨回去
    }
  },
  'rule.del': async id => {
    const r = state.rules.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: r.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('rules/' + id); await refresh(); toast(t('rule.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});

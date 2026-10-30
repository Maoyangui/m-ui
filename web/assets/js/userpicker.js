// 用户勾选器(规则的目标):顶部按归属分组——主面板一组、每个代理一组,勾选状态存在 Set 里,切换分组不会丢。
// 四种目标可叠加:最上面"全部用户"(含以后新建);"按套餐"(现有的和以后套用的)→ planIds;
// 代理分组头部"该代理名下全部用户"(含以后新建)→ resellerIds;逐个勾选 → userIds。读出来是 {allUsers, userIds, resellerIds, planIds}。
import { t } from './i18n.js';
import { esc } from './ui.js';

export function userPicker(host, { users, resellers, plans, selected }) {
  const planIds = new Set((selected && selected.planIds) || []);
  const planLabel = p => (p.resellerName ? p.resellerName + ' · ' : '') + p.name;
  const userIds = new Set((selected && selected.userIds) || []);
  const resellerIds = new Set((selected && selected.resellerIds) || []);
  let allUsers = !!(selected && selected.allUsers);
  let tab = 0; // 0 = 主面板
  let q = '';
  const tabs = [{ id: 0, name: t('up.master') }, ...(resellers || []).map(r => ({ id: r.id, name: r.name }))];
  const inTab = u => (tab === 0 ? !u.resellerId : u.resellerId === tab);
  const hit = u => !q || (u.name || '').toLowerCase().includes(q); // 只按用户名找,列表也只显示用户名(备注可能是多行订单信息)
  const visible = () => users.filter(u => inTab(u) && hit(u));
  const countText = () => t('up.count', { users: userIds.size, resellers: resellerIds.size, plans: planIds.size });

  const listHTML = () => {
    const rows = visible();
    const whole = tab !== 0 && resellerIds.has(tab);
    if (!rows.length) return `<p class="hint">${t('up.none')}</p>`;
    // 已经被选中的套餐覆盖的用户:和"该代理名下全部用户"一样,勾着、不用再单独选
    const covered = u => whole || (u.planId && planIds.has(u.planId));
    return `<div class="check-list lp-list">${rows.map(u => `<label class="${!u.enabled ? 'muted' : ''}"><input type="checkbox" class="up-cb" data-id="${u.id}" ${userIds.has(u.id) || covered(u) ? 'checked' : ''} ${covered(u) ? 'disabled' : ''}> ${esc(u.name)}${!u.enabled ? ` <span class="muted small">(${t('common.disabled')})</span>` : ''}</label>`).join('')}</div>`;
  };
  const render = () => {
    host.innerHTML = `<div class="lp up">
      <label class="up-all" style="display:flex;align-items:center;gap:.4rem;margin-bottom:.5rem"><input type="checkbox" id="up-all" ${allUsers ? 'checked' : ''}> <b>${t('up.all')}</b></label>
      <div class="up-body" ${allUsers ? 'style="opacity:.45;pointer-events:none"' : ''}>
        ${(plans || []).length ? `<div class="up-plans"><div class="up-plans-title"><b>${t('up.plans')}</b></div><div class="check-list">${plans.map(p => `<label><input type="checkbox" class="up-plan" data-id="${p.id}" ${planIds.has(p.id) ? 'checked' : ''}> ${esc(planLabel(p))} <span class="muted small">${t('up.planUsers', { n: p.userCount || 0 })}</span></label>`).join('')}</div></div>` : ''}
        <div class="lp-head">
          <div class="seg lp-tabs">${tabs.map(n => `<button type="button" data-up-tab="${n.id}" class="${n.id === tab ? 'active' : ''}">${esc(n.name)}${n.id ? ` <span class="muted small">(${t('up.reseller')})</span>` : ''}</button>`).join('')}</div>
          <span class="grow"></span>
          <input class="up-search" placeholder="${t('up.search')}" value="${esc(q)}" style="max-width:11rem">
          <button type="button" class="btn sm ghost" data-up="all">${t('lp.selectGroup')}</button>
          <button type="button" class="btn sm ghost" data-up="none">${t('lp.clearGroup')}</button>
          <span class="muted small up-count">${countText()}</span>
        </div>
        ${tab ? `<label class="up-whole" style="display:flex;align-items:center;gap:.4rem;margin:.3rem 0"><input type="checkbox" class="up-rs" ${resellerIds.has(tab) ? 'checked' : ''}> ${t('up.wholeReseller')}</label>` : ''}
        <div class="up-list-wrap">${listHTML()}</div>
      </div>
    </div>`;
  };
  const refreshList = () => {
    const wrap = host.querySelector('.up-list-wrap');
    if (wrap) wrap.innerHTML = listHTML();
    const c = host.querySelector('.up-count');
    if (c) c.textContent = countText();
  };
  host.addEventListener('click', e => {
    const tb = e.target.closest('[data-up-tab]');
    if (tb) { tab = Number(tb.dataset.upTab); render(); return; }
    const act = e.target.closest('[data-up]');
    if (!act) return;
    for (const u of visible()) act.dataset.up === 'all' ? userIds.add(u.id) : userIds.delete(u.id);
    if (act.dataset.up === 'none' && tab) resellerIds.delete(tab);
    render();
  });
  host.addEventListener('change', e => {
    if (e.target.id === 'up-all') { allUsers = e.target.checked; render(); return; }
    if (e.target.classList.contains('up-plan')) {
      const id = Number(e.target.dataset.id);
      e.target.checked ? planIds.add(id) : planIds.delete(id);
      refreshList();
      return;
    }
    if (e.target.classList.contains('up-rs')) { e.target.checked ? resellerIds.add(tab) : resellerIds.delete(tab); refreshList(); return; }
    const cb = e.target.closest('.up-cb');
    if (!cb) return;
    cb.checked ? userIds.add(Number(cb.dataset.id)) : userIds.delete(Number(cb.dataset.id));
    const c = host.querySelector('.up-count');
    if (c) c.textContent = countText();
  });
  host.addEventListener('input', e => {
    if (!e.target.classList.contains('up-search')) return;
    q = e.target.value.trim().toLowerCase();
    refreshList();
  });
  render();
  return { read: () => ({ allUsers, userIds: [...userIds], resellerIds: [...resellerIds], planIds: [...planIds] }) };
}

// targetText 把目标写成一句话:全部用户 / 套餐 + N 人 + M 个代理
export function targetText(rule, resellers, plans) {
  if (rule.allUsers) return t('rule.allUsers');
  const parts = [];
  for (const id of rule.planIds || []) {
    const p = (plans || []).find(x => x.id === id);
    if (p) parts.push(p.name + ' (' + t('up.plan') + ')');
  }
  if ((rule.userIds || []).length) parts.push(t('rule.nUsers', { n: rule.userIds.length }));
  for (const id of rule.resellerIds || []) {
    const r = (resellers || []).find(x => x.id === id);
    parts.push((r ? r.name : '#' + id) + ' (' + t('up.reseller') + ')');
  }
  return parts.join(' + ') || '—';
}

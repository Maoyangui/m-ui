// 线路 × 服务器选择器:一条线路部署在几台服务器上,对用户来说就是几个入口,按入口勾选。
// 顶部按服务器分组(全部 / A / B…),每组可一键全选 / 清空;勾选状态存在 Set 里,切换分组不会丢。
// 用户、套餐、代理授权三处共用;代理面板里只列授权范围内的入口。
import { t } from './i18n.js';
import { esc } from './ui.js';

// 一条线路部署到的服务器:nodeIds 空 = 全部服务器
function nodeIdsOf(line, nodes) {
  const v = line.nodeIds;
  let ids = [];
  try { ids = Array.isArray(v) ? v : (v ? JSON.parse(v) : []); } catch { ids = []; }
  if (!ids.length) return nodes.map(n => n.id);
  return nodes.filter(n => ids.includes(n.id)).map(n => n.id);
}

// lineItems 把线路展开成入口:[{key, lineId, nodeId, lineName, nodeName, local}]
// grants(可选)= 授权范围 [{lineId, nodeIds}]:只列授权过的入口(nodeIds 空 = 该线路全部)
export function lineItems(lines, nodes, grants) {
  nodes = nodes || [];
  const byNode = new Map(nodes.map(n => [n.id, n]));
  const allowed = grants ? new Map(grants.map(g => [g.lineId, g.nodeIds || []])) : null;
  const out = [];
  for (const l of lines) {
    if (allowed && !allowed.has(l.id)) continue;
    let ids = nodes.length ? nodeIdsOf(l, nodes) : [0];
    if (allowed && allowed.get(l.id).length) ids = ids.filter(id => allowed.get(l.id).includes(id));
    for (const nid of ids) {
      const n = byNode.get(nid);
      out.push({ key: `${l.id}:${nid}`, lineId: l.id, nodeId: nid, lineName: l.name, nodeName: n ? n.name : '', local: !!(n && n.isLocal) });
    }
  }
  return out;
}

// keysFromRefs 把已有分配 [{lineId, nodeIds}] 换成勾选集合(nodeIds 空 = 该线路的全部入口)
export function keysFromRefs(refs, items) {
  const keys = new Set();
  for (const r of refs || []) {
    for (const it of items) {
      if (it.lineId !== r.lineId) continue;
      if (!r.nodeIds || !r.nodeIds.length || r.nodeIds.includes(it.nodeId)) keys.add(it.key);
    }
  }
  return keys;
}

// refsFromKeys 把勾选集合收回成分配:一条线路的入口全勾 = 全部(以后新加的服务器自动包含)
export function refsFromKeys(keys, items) {
  const byLine = new Map();
  for (const it of items) {
    if (!byLine.has(it.lineId)) byLine.set(it.lineId, { total: 0, picked: [] });
    const b = byLine.get(it.lineId);
    b.total++;
    if (keys.has(it.key)) b.picked.push(it.nodeId);
  }
  const refs = [];
  for (const [lineId, b] of byLine) {
    if (!b.picked.length) continue;
    refs.push(b.picked.length === b.total ? { lineId } : { lineId, nodeIds: b.picked });
  }
  return refs;
}

// linePicker(host, {items, selected}) 渲染到 host(已存在的元素),返回 {read(): refs, keys: Set}
export function linePicker(host, { items, selected }) {
  const keys = selected || new Set();
  const nodeTabs = [];
  const seen = new Set();
  for (const it of items) if (it.nodeId && !seen.has(it.nodeId)) { seen.add(it.nodeId); nodeTabs.push({ id: it.nodeId, name: it.nodeName, local: it.local }); }
  let tab = 0; // 0 = 全部
  const visible = () => items.filter(it => !tab || it.nodeId === tab);
  const render = () => {
    const rows = visible();
    host.innerHTML = `<div class="lp">
      <div class="lp-head">
        ${nodeTabs.length > 1 ? `<div class="seg lp-tabs">${[{ id: 0, name: t('lp.all') }, ...nodeTabs].map(n => `<button type="button" data-lp-tab="${n.id}" class="${n.id === tab ? 'active' : ''}">${esc(n.name)}${n.local ? ` <span class="muted small">(${t('node.local')})</span>` : ''}</button>`).join('')}</div>` : ''}
        <span class="grow"></span>
        <button type="button" class="btn sm ghost" data-lp="all">${t('lp.selectGroup')}</button>
        <button type="button" class="btn sm ghost" data-lp="none">${t('lp.clearGroup')}</button>
        <span class="muted small lp-count">${t('lp.count', { n: keys.size, total: items.length })}</span>
      </div>
      ${items.length ? `<div class="check-list lp-list">${rows.map(it => `<label><input type="checkbox" class="lp-cb" data-key="${it.key}" ${keys.has(it.key) ? 'checked' : ''}> ${esc(it.lineName)}${nodeTabs.length > 1 && it.nodeName ? ` <span class="muted small">· ${esc(it.nodeName)}</span>` : ''}</label>`).join('')}</div>` : `<p class="hint">${t('lp.none')}</p>`}
    </div>`;
  };
  const count = () => { const c = host.querySelector('.lp-count'); if (c) c.textContent = t('lp.count', { n: keys.size, total: items.length }); };
  host.addEventListener('click', e => {
    const tb = e.target.closest('[data-lp-tab]');
    if (tb) { tab = Number(tb.dataset.lpTab); render(); return; }
    const act = e.target.closest('[data-lp]');
    if (!act) return;
    for (const it of visible()) act.dataset.lp === 'all' ? keys.add(it.key) : keys.delete(it.key);
    render();
  });
  host.addEventListener('change', e => {
    const cb = e.target.closest('.lp-cb');
    if (!cb) return;
    cb.checked ? keys.add(cb.dataset.key) : keys.delete(cb.dataset.key);
    count();
  });
  render();
  return { keys, read: () => refsFromKeys(keys, items), set: refs => { keys.clear(); keysFromRefs(refs, items).forEach(k => keys.add(k)); render(); } };
}

// refLabels 把分配渲染成可读的名字:全部入口 = 线路名;部分 = 线路名 · 服务器名
export function refLabels(refs, lines, nodes) {
  nodes = nodes || [];
  const out = [];
  for (const r of refs || []) {
    const l = lines.find(x => x.id === r.lineId);
    if (!l) continue;
    if (!r.nodeIds || !r.nodeIds.length || nodes.length < 2) { out.push(l.name); continue; }
    for (const nid of r.nodeIds) { const n = nodes.find(x => x.id === nid); out.push(`${l.name} · ${n ? n.name : '#' + nid}`); }
  }
  return out;
}

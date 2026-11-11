// 线路 × 服务器选择器:一条线路部署在几台服务器上,对用户来说就是几个入口,按入口勾选。
// 用户、套餐、代理授权三处共用;代理面板里只列授权范围内的入口。
// 界面:搜索(线路名 / 服务器名)+ 服务器筛选(≤8 台是可换行的按钮,更多换成下拉)+ 只看已选 + 全选 / 清空可见;
// 「全部服务器」时按线路分组:每条线路一个三态勾选框,后面是可点的服务器 chip(服务器多时折叠)。
// 服务器再多也是纵向增长、能搜,不会横向溢出;勾选存在 Set 里,切换筛选不会丢。
import { t } from './i18n.js';
import { esc, chipList } from './ui.js';

// 一条线路部署到的服务器:nodeIds 空 = 全部服务器
function nodeIdsOf(line, nodes) {
  const v = line.nodeIds;
  let ids = [];
  try { ids = Array.isArray(v) ? v : (v ? JSON.parse(v) : []); } catch { ids = []; }
  if (!ids.length) return nodes.map(n => n.id);
  return nodes.filter(n => ids.includes(n.id)).map(n => n.id);
}

// lineItems 把线路展开成入口:[{key, lineId, nodeId, lineName, nodeName, local, narrowed}]
// grants(可选)= 授权范围 [{lineId, nodeIds}]:只列授权过的入口(nodeIds 空 = 该线路全部)。
// 授权收窄过的线路标 narrowed:这里看到的不是它的全部入口,勾满也只能按具体服务器写回。
export function lineItems(lines, nodes, grants) {
  nodes = nodes || [];
  const byNode = new Map(nodes.map(n => [n.id, n]));
  const allowed = grants ? new Map(grants.map(g => [g.lineId, g.nodeIds || []])) : null;
  const out = [];
  for (const l of lines) {
    if (allowed && !allowed.has(l.id)) continue;
    let ids = nodes.length ? nodeIdsOf(l, nodes) : [0];
    const narrowed = !!(allowed && allowed.get(l.id).length);
    if (narrowed) ids = ids.filter(id => allowed.get(l.id).includes(id));
    for (const nid of ids) {
      const n = byNode.get(nid);
      out.push({ key: `${l.id}:${nid}`, lineId: l.id, nodeId: nid, lineName: l.name, nodeName: n ? n.name : '', local: !!(n && n.isLocal), narrowed });
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

// refsFromKeys 把勾选集合收回成分配:一条线路的入口全勾 = 全部(以后新加的服务器自动包含);
// 授权收窄过的线路例外,勾满也只写具体服务器(代理看到的本来就只是一部分)。
export function refsFromKeys(keys, items) {
  const byLine = new Map();
  for (const it of items) {
    if (!byLine.has(it.lineId)) byLine.set(it.lineId, { total: 0, picked: [], narrowed: false });
    const b = byLine.get(it.lineId);
    b.total++;
    if (it.narrowed) b.narrowed = true;
    if (keys.has(it.key)) b.picked.push(it.nodeId);
  }
  const refs = [];
  for (const [lineId, b] of byLine) {
    if (!b.picked.length) continue;
    refs.push(!b.narrowed && b.picked.length === b.total ? { lineId } : { lineId, nodeIds: b.picked });
  }
  return refs;
}

const CHIP_OPEN_MAX = 6;  // 一条线路部署到的服务器不超过这么多:chip 直接摊开;再多折起来,点 ▾ 展开
const TABS_MAX = 8;       // 服务器不超过这么多:筛选用可换行的按钮;再多换成下拉框

// linePicker(host, {items, selected}) 渲染到 host(已存在的元素),返回 {keys, read(): refs, set(refs)}
export function linePicker(host, { items, selected }) {
  const keys = selected || new Set();
  const nodeTabs = [];
  const seen = new Set();
  for (const it of items) if (it.nodeId && !seen.has(it.nodeId)) { seen.add(it.nodeId); nodeTabs.push({ id: it.nodeId, name: it.nodeName, local: it.local }); }
  const multi = nodeTabs.length > 1;
  // 按线路分组(保持线路顺序)
  const groups = [], byLine = new Map();
  for (const it of items) {
    let g = byLine.get(it.lineId);
    if (!g) { g = { id: it.lineId, name: it.lineName, items: [], narrowed: it.narrowed }; byLine.set(it.lineId, g); groups.push(g); }
    g.items.push(it);
  }
  let q = '', node = 0, onlySel = false;
  const expanded = new Set(); // 手动展开了 chip 的线路
  const lc = s => String(s || '').toLowerCase();
  // 一条线路在当前搜索 / 筛选下露出来的入口;byServer = 是不是因为服务器名命中才露出来的(这时自动展开、只列命中的 chip)
  const visibleOf = g => {
    let list = g.items;
    if (node) list = list.filter(it => it.nodeId === node);
    if (onlySel) list = list.filter(it => keys.has(it.key));
    let byServer = false;
    if (q && !lc(g.name).includes(q)) { list = list.filter(it => lc(it.nodeName).includes(q)); byServer = true; }
    return { list, byServer };
  };
  const visibleItems = () => groups.flatMap(g => visibleOf(g).list);
  const flat = () => !multi || node !== 0;
  const nodeName = n => esc(n.name) + (n.local ? ` <span class="muted small">(${esc(t('node.local'))})</span>` : '');
  const pickedOn = id => items.filter(it => it.nodeId === id && keys.has(it.key)).length;
  const totalOn = id => items.filter(it => it.nodeId === id).length;

  const filterHTML = () => {
    if (!multi) return '';
    if (nodeTabs.length <= TABS_MAX) {
      return `<div class="lp-nodetabs">${[{ id: 0, name: t('lp.allNodes') }, ...nodeTabs].map(n => `<button type="button" data-lp-node="${n.id}" class="${n.id === node ? 'active' : ''}">${n.id ? nodeName(n) : esc(n.name)}</button>`).join('')}</div>`;
    }
    return `<select class="sm lp-node" aria-label="${esc(t('common.server'))}"><option value="0">${esc(t('lp.allNodes'))}</option>${nodeTabs.map(n => `<option value="${n.id}" ${n.id === node ? 'selected' : ''}>${esc(t('lp.nodeOpt', { name: n.name + (n.local ? ` (${t('node.local')})` : ''), n: pickedOn(n.id), total: totalOn(n.id) }))}</option>`).join('')}</select>`;
  };

  // 一条线路一行:三态勾选框 + 名字 + "n/m 台" + (整条勾满且没被授权收窄)「全部(含以后新增)」+ 服务器 chip
  const lineHTML = g => {
    const { list, byServer } = visibleOf(g);
    if (!list.length) return '';
    const on = list.filter(it => keys.has(it.key)).length;
    const pickedAll = g.items.filter(it => keys.has(it.key)).length;
    // 整条勾满且没被授权收窄:写回的是整条线路(以后新加的服务器也算进来),只部署在一台上的线路也一样要说清楚
    const whole = pickedAll === g.items.length && !g.narrowed;
    const many = g.items.length > 1;
    const one = !many && g.items[0];
    const open = byServer || expanded.has(g.id) || g.items.length <= CHIP_OPEN_MAX;
    const chip = it => `<button type="button" class="chip lp-chip ${keys.has(it.key) ? 'on' : ''}" data-key="${it.key}" aria-pressed="${keys.has(it.key)}">${esc(it.nodeName || '—')}${it.local ? ` <span class="small">(${esc(t('node.local'))})</span>` : ''}</button>`;
    return `<div class="lp-line" data-line="${g.id}">
      <label title="${esc(g.name)}"><input type="checkbox" class="lp-lcb" data-line="${g.id}" ${on && on === list.length ? 'checked' : ''} ${on && on < list.length ? 'data-ind="1"' : ''}> <span class="lp-name">${esc(g.name)}</span></label>
      <span class="lp-n">${many ? esc(t('lp.nOf', { n: pickedAll, m: g.items.length })) : one ? `<span class="muted small">${esc(one.nodeName || '—')}${one.local ? ` (${esc(t('node.local'))})` : ''}</span>` : ''}${whole ? ' ' + `<span class="badge primary" title="${esc(t('lp.wholeLineHelp'))}">${esc(t('lp.wholeLine'))}</span>` : ''}</span>
      ${many && !byServer && g.items.length > CHIP_OPEN_MAX ? `<button type="button" class="lp-x" data-lp-x="${g.id}" aria-expanded="${open}" title="${esc(t(open ? 'common.less' : 'common.more'))}">${open ? '▴' : '▾'}</button>` : '<span></span>'}
      ${many && open ? `<div class="lp-nodes">${list.map(chip).join('')}</div>` : ''}
    </div>`;
  };

  const bodyHTML = () => {
    if (!items.length) return `<p class="hint">${esc(t('lp.none'))}</p>`;
    if (flat()) {
      const rows = visibleItems();
      if (!rows.length) return `<p class="hint">${esc(t('lp.noMatch'))}</p>`;
      return `<div class="check-list lp-list">${rows.map(it => `<label title="${esc(it.lineName)}"><input type="checkbox" class="lp-cb" data-key="${it.key}" ${keys.has(it.key) ? 'checked' : ''}> ${esc(it.lineName)}</label>`).join('')}</div>`;
    }
    const html = groups.map(lineHTML).join('');
    return html || `<p class="hint">${esc(t('lp.noMatch'))}</p>`;
  };

  const markInd = root => root.querySelectorAll('.lp-lcb[data-ind]').forEach(cb => { cb.indeterminate = true; });
  const count = () => {
    const c = host.querySelector('.lp-count');
    if (c) c.textContent = t('lp.count', { n: keys.size, total: items.length });
    const sel = host.querySelector('select.lp-node');
    if (sel) [...sel.options].forEach(o => { const id = Number(o.value); if (id) { const n = nodeTabs.find(x => x.id === id); o.textContent = t('lp.nodeOpt', { name: n.name + (n.local ? ` (${t('node.local')})` : ''), n: pickedOn(id), total: totalOn(id) }); } });
  };
  // 只重画正文(搜索框保留焦点);滚动位置留着
  const renderBody = () => {
    const b = host.querySelector('.lp-body');
    if (!b) return;
    const top = b.scrollTop;
    b.innerHTML = bodyHTML();
    markInd(b);
    b.scrollTop = top;
  };
  const renderLine = id => {
    const row = host.querySelector(`.lp-line[data-line="${id}"]`);
    const g = byLine.get(id);
    if (!row || !g) { renderBody(); return; }
    const html = lineHTML(g);
    if (!html) { row.remove(); return; }
    row.insertAdjacentHTML('afterend', html);
    const next = row.nextElementSibling;
    row.remove();
    markInd(next);
  };
  const render = () => {
    host.innerHTML = `<div class="lp">
      <div class="lp-head">
        ${items.length > 6 ? `<input type="search" class="sm lp-q" placeholder="${esc(t('lp.search'))}" aria-label="${esc(t('lp.search'))}" value="${esc(q)}">` : ''}
        <span class="lp-filter">${filterHTML()}</span>
        <label class="lp-only"><input type="checkbox" class="lp-onlysel" ${onlySel ? 'checked' : ''}> ${esc(t('lp.onlySel'))}</label>
        <span class="grow"></span>
        <button type="button" class="btn sm ghost" data-lp="all">${esc(t('lp.selVisible'))}</button>
        <button type="button" class="btn sm ghost" data-lp="none">${esc(t('lp.clearVisible'))}</button>
        <span class="muted small lp-count"></span>
      </div>
      <div class="lp-body"></div>
    </div>`;
    renderBody();
    count();
  };

  let qTimer;
  host.addEventListener('input', e => {
    if (!e.target.classList.contains('lp-q')) return;
    clearTimeout(qTimer);
    qTimer = setTimeout(() => { q = lc(e.target.value.trim()); renderBody(); }, 150);
  });
  host.addEventListener('click', e => {
    const tb = e.target.closest('[data-lp-node]');
    if (tb) {
      node = Number(tb.dataset.lpNode);
      host.querySelectorAll('[data-lp-node]').forEach(b => b.classList.toggle('active', Number(b.dataset.lpNode) === node));
      renderBody();
      return;
    }
    const x = e.target.closest('[data-lp-x]');
    if (x) { const id = Number(x.dataset.lpX); if (expanded.has(id)) expanded.delete(id); else expanded.add(id); renderLine(id); return; }
    const chip = e.target.closest('.lp-chip');
    if (chip) {
      const k = chip.dataset.key;
      if (keys.has(k)) keys.delete(k); else keys.add(k);
      renderLine(Number(k.split(':')[0]));
      count();
      return;
    }
    const act = e.target.closest('[data-lp]');
    if (!act) return;
    for (const it of visibleItems()) act.dataset.lp === 'all' ? keys.add(it.key) : keys.delete(it.key);
    renderBody();
    count();
  });
  host.addEventListener('change', e => {
    const el = e.target;
    if (el.classList.contains('lp-node')) { node = Number(el.value) || 0; renderBody(); return; }
    if (el.classList.contains('lp-onlysel')) { onlySel = el.checked; renderBody(); return; }
    if (el.classList.contains('lp-lcb')) {
      const id = Number(el.dataset.line);
      for (const it of visibleOf(byLine.get(id)).list) el.checked ? keys.add(it.key) : keys.delete(it.key);
      renderLine(id);
      count();
      return;
    }
    if (el.classList.contains('lp-cb')) {
      el.checked ? keys.add(el.dataset.key) : keys.delete(el.dataset.key);
      count();
    }
  });
  render();
  return { keys, read: () => refsFromKeys(keys, items), set: refs => { keys.clear(); keysFromRefs(refs, items).forEach(k => keys.add(k)); renderBody(); count(); } };
}

// refGroups 把分配按线路聚合:[{name, full, servers:[服务器名], n, m}]。整条线路(或只有一台服务器)= full。
function refGroups(refs, lines, nodes) {
  nodes = nodes || [];
  const out = [];
  for (const r of refs || []) {
    const l = lines.find(x => x.id === r.lineId);
    if (!l) continue;
    const all = nodes.length ? nodeIdsOf(l, nodes) : [];
    if (!r.nodeIds || !r.nodeIds.length || nodes.length < 2) { out.push({ name: l.name, full: true, servers: [], n: all.length, m: all.length }); continue; }
    // 只算线路现在还部署着的服务器:线路撤下某台后,用户分配里可能还留着那台的 id(改部署时后端不清),不能因此把部分分配算成整条
    const eff = all.length ? r.nodeIds.filter(id => all.includes(id)) : r.nodeIds;
    const names = eff.map(nid => { const n = nodes.find(x => x.id === nid); return n ? n.name : '#' + nid; });
    out.push({ name: l.name, full: all.length > 0 && eff.length === all.length, servers: names, n: eff.length, m: all.length || eff.length });
  }
  return out;
}

// refLabels 把分配渲染成可读的名字:整条线路写线路名;只分了部分服务器写「线路名 · 3/24 台」。
// servers: true 时部分分配写出服务器名(按名字排序):换套餐预览靠字符串比较标出变化,同一线路换了同样台数的服务器也要看得出来
export function refLabels(refs, lines, nodes, { servers = false } = {}) {
  return refGroups(refs, lines, nodes).map(g => (g.full ? g.name
    : `${g.name} · ${servers ? [...g.servers].sort().join(t('reach.sep')) || t('lp.nOf', { n: g.n, m: g.m }) : t('lp.nOf', { n: g.n, m: g.m })}`));
}

// refChips 同上,做成 chip(服务器名放 title),多了折成「+N」;展开状态按 key 记住
export function refChips(refs, lines, nodes, { key = '', max = 20 } = {}) {
  const list = refGroups(refs, lines, nodes);
  if (!list.length) return '';
  return chipList(list.map(g => `<span class="chip"${g.servers.length ? ` title="${esc(g.servers.join(t('reach.sep')))}"` : ''}>${esc(g.full ? g.name : `${g.name} · ${t('lp.nOf', { n: g.n, m: g.m })}`)}</span>`), { key, max });
}

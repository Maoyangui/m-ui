import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtRelative, toast, confirm, openModal, openDrawer, registerActions, badge, field, check, empty, fv, fchk, copy, matches } from '../ui.js';

export const title = () => t('ext.title');
export const subtitle = () => t('ext.subtitle');

// 展开面板的状态都放在模块里,不放 DOM:列表每几秒重画一次,勾选、搜索词、测速进度不能跟着丢。
const open = new Set(); // 展开了的外部节点 id
const cache = {};       // id → { nodes, servers, fetchedAt }
const picked = {};      // id → Set(节点下标)
const query = {};       // id → 搜索词
const jobs = {};        // id → { job, timer } 进行中的测速
let rowsSig = '';

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar"><span class="muted small">${t('ext.help')}</span><span class="grow"></span><button class="btn primary" data-act="ext.add">${t('ext.add')}</button></div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.type')}</th><th>${t('ext.value')}</th><th>${t('ext.nodes')}</th><th>${t('ext.lastFetch')}</th><th>${t('ext.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="exts-body"></tbody>
    </table></div>`;
  rowsSig = '';
  renderRows();
  // 展开面板里的搜索框:委托在表体上,重画也不用重新绑
  el.querySelector('#exts-body').addEventListener('input', e => {
    const q = e.target.closest('input[data-q]');
    if (!q) return;
    query[q.dataset.q] = q.value;
    renderSubBody(Number(q.dataset.q));
  });
}
export async function tick() { await load('exts'); renderRows(); }

function renderRows() {
  const body = document.getElementById('exts-body');
  if (!body) return;
  const rows = state.exts || [];
  // 内容没变就不重画:展开面板里的勾选、搜索、测速进度都在 DOM 里,整表重画会把它们抹掉
  const sig = rows.map(x => [x.id, x.name, x.type, x.value, x.prefix, x.remark, x.nodeCount, x.lastFetch, x.lastError, x.enabled, x.userCount].join('')).join('') + '|' + [...open].join(',');
  if (sig === rowsSig) return;
  rowsSig = sig;
  if (!rows.length) { body.innerHTML = `<tr><td colspan="8">${empty(t('ext.empty'))}</td></tr>`; return; }
  body.innerHTML = rows.map(x => rowHTML(x) + (open.has(x.id)
    ? `<tr class="ext-sub"><td colspan="8"><div class="ext-panel" id="ext-panel-${x.id}"><span class="muted small">${t('common.loading')}</span></div></td></tr>` : '')).join('');
  for (const id of open) fillPanel(id);
}

function rowHTML(x) {
  const on = open.has(x.id);
  return `<tr>
    <td class="primary-cell"><button class="btn sm ext-toggle" data-act="ext.toggle" data-id="${x.id}" title="${on ? t('ext.collapse') : t('ext.expand')}">${on ? '▾' : '▸'}</button>${esc(x.name)}${x.prefix ? `<div class="sub-cell">${t('ext.prefix')}: ${esc(x.prefix)}</div>` : ''}${x.remark ? `<div class="sub-cell">${esc(x.remark)}</div>` : ''}</td>
    <td>${badge(x.type === 'sub' ? t('ext.typeSub') : t('ext.typeLink'), x.type === 'sub' ? 'primary' : '')}</td>
    <td class="mono small" title="${esc(x.value)}">${esc(x.value.length > 60 ? x.value.slice(0, 60) + '…' : x.value)}</td>
    <td class="num">${x.nodeCount || 0}</td>
    <td>${x.lastError ? badge(t('ext.failed'), 'danger') + `<div class="sub-cell" title="${esc(x.lastError)}">${esc(x.lastError.slice(0, 60))}</div>` : (x.lastFetch ? fmtRelative(x.lastFetch) : '—')}</td>
    <td class="num">${x.userCount || 0}</td>
    <td>${badge(x.enabled ? t('common.enabled') : t('common.disabled'), x.enabled ? 'ok' : 'danger')}</td>
    <td class="actions">
      ${x.type === 'sub' ? `<button class="btn sm" data-act="ext.refresh" data-id="${x.id}">${t('ext.refresh')}</button>` : ''}
      <button class="btn sm" data-act="ext.edit" data-id="${x.id}">${t('common.edit')}</button>
      <button class="btn sm danger" data-act="ext.del" data-id="${x.id}">${t('common.delete')}</button>
    </td></tr>`;
}

// ---- 展开面板:节点子表 ----

async function fillPanel(id) {
  const box = document.getElementById('ext-panel-' + id);
  if (!box) return;
  if (!cache[id]) {
    try { cache[id] = await get(`exts/${id}/nodes`); }
    catch (e) { const b = document.getElementById('ext-panel-' + id); if (b) b.innerHTML = `<div class="pc-err">${esc(e.message)}</div>`; return; }
  }
  const again = document.getElementById('ext-panel-' + id);
  if (!again) return; // 等接口的功夫被收起了
  again.innerHTML = panelHTML(id, cache[id]);
  renderSubBody(id);
}

function panelHTML(id, c) {
  return `<div class="toolbar ext-tools">
      <input type="search" data-q="${id}" placeholder="${t('ext.search')}" value="${esc(query[id] || '')}">
      <span class="muted small" id="ext-count-${id}"></span><span class="grow"></span>
      <button class="btn sm" data-act="ext.testAll" data-id="${id}">${t('ext.testAll')}</button>
      <button class="btn sm" data-act="ext.testSel" data-id="${id}">${t('ext.testSel')}</button>
      <button class="btn sm primary" data-act="ext.addUp" data-id="${id}">${t('ext.addUp')}</button>
    </div>
    <div class="table-wrap"><table class="grid ext-grid"><thead><tr>
      <th><input type="checkbox" data-change="ext.pickAll" data-id="${id}" title="${t('common.all')}"></th>
      <th>${t('common.name')}</th><th>${t('common.type')}</th><th>${t('ext.server')}</th>
      ${(c.servers || []).map(s => `<th class="num" title="${esc(s.name)}">${esc(s.name)}</th>`).join('')}<th></th></tr></thead>
      <tbody id="ext-nodes-${id}"></tbody></table></div>
    <p class="hint small muted">${t('ext.testHint')}</p>`;
}

const visible = (id, c) => {
  const q = (query[id] || '').trim();
  return (c.nodes || []).filter(n => !q || matches(q, n.name, n.type, n.server + ':' + n.port));
};

const cellHTML = r => !r ? '<span class="muted">—</span>'
  : r.state === 'pending' ? '<span class="muted">…</span>'
    : r.state === 'ok' ? badge(r.delayMs + ' ms', 'ok')
      : `<span class="badge danger" title="${esc(r.error || '')}">✗</span>`;

function renderSubBody(id) {
  const tb = document.getElementById('ext-nodes-' + id), c = cache[id];
  if (!tb || !c) return;
  const sel = picked[id] || (picked[id] = new Set());
  const list = visible(id, c), servers = c.servers || [];
  tb.innerHTML = list.length ? list.map(n => `<tr>
    <td><input type="checkbox" data-change="ext.pick" data-id="${id}" data-idx="${n.index}" ${sel.has(n.index) ? 'checked' : ''}></td>
    <td class="primary-cell"><a href="#" data-act="ext.detail" data-id="${id}" data-idx="${n.index}">${esc(n.name)}</a></td>
    <td>${badge(esc(n.type))}</td>
    <td class="mono small">${esc(n.server)}:${n.port}</td>
    ${servers.map(s => `<td class="num" data-cell="${id}-${n.index}-${s.id}">${cellHTML(n.results && n.results[s.id])}</td>`).join('')}
    <td>${n.upstream ? '' : badge(t('ext.noUpstream'), 'warn')}</td></tr>`).join('')
    : `<tr><td colspan="${5 + servers.length}">${empty(t('ext.noNodes'))}</td></tr>`;
  updateCount(id);
}

function updateCount(id) {
  const cnt = document.getElementById('ext-count-' + id), c = cache[id];
  if (!cnt || !c) return;
  const sel = picked[id];
  cnt.textContent = t('ext.count', { n: (c.nodes || []).length })
    + (sel && sel.size ? ' · ' + t('ext.selected', { n: sel.size }) : '')
    + (c.fetchedAt ? ' · ' + t('ext.lastFetch') + ' ' + fmtRelative(c.fetchedAt) : '');
}

// ---- 节点详情(抽屉)----

function showDetail(id, idx) {
  const c = cache[id], n = c && (c.nodes || [])[idx];
  if (!n) return;
  const rows = (n.fields || []).map(f => `<tr><td class="muted">${esc(f.k)}</td><td class="mono small ext-val">${esc(f.v)}</td></tr>`).join('');
  const link = n.link
    ? `<div class="ext-link"><div class="muted small">${t('ext.link')}</div><div class="mono small ext-val">${esc(n.link)}</div><div><button class="btn sm" data-act="ext.copy" data-link="${esc(n.link)}">${t('common.copy')}</button></div></div>`
    : `<p class="muted small">${t('ext.noLink')}</p>`;
  openDrawer(esc(n.name), `<p class="small muted">${badge(esc(n.type))} ${esc(n.server)}:${n.port}${n.upstream ? '' : ' · ' + t('ext.noUpstream')}</p>
    <div class="table-wrap"><table class="grid kv"><tbody>${rows}</tbody></table></div>${link}`);
}

// ---- 测速:起任务,每秒取进度,一格一格填 ----

async function startTest(id, indexes) {
  if (jobs[id]) { toast(t('ext.testRunning'), 'err'); return; }
  const c = cache[id];
  if (!c) return;
  let r;
  try { r = await post(`exts/${id}/nodes/test`, { indexes }, SLOW); }
  catch (e) { toast(e.message, 'err'); return; }
  const want = indexes.length ? new Set(indexes) : null;
  for (const n of c.nodes || []) {
    if (want && !want.has(n.index)) continue;
    n.results = {};
    for (const s of c.servers || []) n.results[s.id] = { state: 'pending' };
  }
  renderSubBody(id);
  toast(t('ext.testStarted', { n: want ? want.size : (c.nodes || []).length, s: (c.servers || []).length }), 'ok');
  const started = Date.now();
  const timer = setInterval(async () => {
    let st;
    try { st = await get(`exts/${id}/nodes/test/${r.job}`); }
    catch (e) { stopJob(id); toast(e.message, 'err'); return; }
    applyResults(id, st.results);
    if (st.done || Date.now() - started > 20 * 60 * 1000) {
      stopJob(id);
      const { ok, bad } = tally(st.results);
      toast(t('ext.testDone', { ok, bad }), bad ? '' : 'ok');
    }
  }, 1000);
  jobs[id] = { job: r.job, timer };
}
function stopJob(id) { if (jobs[id]) { clearInterval(jobs[id].timer); delete jobs[id]; } }
function applyResults(id, results) {
  const c = cache[id];
  if (!c) return;
  for (const [idx, row] of Object.entries(results || {})) {
    const n = (c.nodes || [])[Number(idx)];
    if (!n) continue;
    n.results = n.results || {};
    for (const [sid, cell] of Object.entries(row || {})) {
      n.results[sid] = cell;
      const td = document.querySelector(`[data-cell="${id}-${idx}-${sid}"]`);
      if (td) td.innerHTML = cellHTML(cell);
    }
  }
}
function tally(results) {
  let ok = 0, bad = 0;
  for (const row of Object.values(results || {})) for (const c of Object.values(row || {})) { if (c.state === 'ok') ok++; else if (c.state === 'fail') bad++; }
  return { ok, bad };
}

// ---- 添加为上游 ----

async function addUpstreams(id) {
  const c = cache[id], sel = picked[id];
  if (!c || !sel || !sel.size) { toast(t('ext.pickFirst'), 'err'); return; }
  const ext = (state.exts || []).find(x => x.id === id) || {};
  const list = [...sel].sort((a, b) => a - b).map(i => (c.nodes || [])[i]).filter(Boolean);
  const html = `<p class="hint">${t('ext.addUpHelp')}</p><div class="form-grid">` + list.map(n => n.upstream
    ? field(esc(n.name), `<input id="ext-up-${n.index}" value="${esc((ext.name ? ext.name + '-' : '') + n.name)}">`)
    : field(esc(n.name), `<span class="muted small">${t('ext.noUpstream')}</span>`)).join('') + '</div>';
  openModal(t('ext.addUp'), html, async () => {
    const items = list.filter(n => n.upstream).map(n => ({ index: n.index, name: fv(`ext-up-${n.index}`).trim() }));
    if (!items.length) throw new Error(t('ext.noneAddable'));
    if (items.some(x => !x.name)) throw new Error(t('ext.nameRequired'));
    const r = await post(`exts/${id}/nodes/add-upstream`, items, SLOW);
    const okN = (r.results || []).filter(x => x.ok).length, bad = (r.results || []).filter(x => !x.ok);
    if (okN) { try { await load('upstreams'); } catch (_) { /* 上游页没打开过也没关系 */ } picked[id] = new Set(); renderSubBody(id); }
    // 逐条结果另开一个框:成功的已经建好了,失败的连同原因列出来,别让人再提交一遍撞重名
    setTimeout(() => openModal(t('ext.addUpResult'), `<p>${t('ext.addUpDone', { n: okN })}${bad.length ? ' · ' + t('ext.addUpFailedN', { n: bad.length }) : ''}</p>
      <ul class="ext-results">${(r.results || []).map(x => `<li>${x.ok ? badge('OK', 'ok') : badge('✗', 'danger')} ${esc(x.name)}${x.error ? ` <span class="muted small">${esc(x.error)}</span>` : ''}</li>`).join('')}</ul>
      ${okN ? `<p><a href="#/upstreams">${t('ext.toUpstreams')}</a></p>` : ''}`, null), 0);
  }, { wide: true, saveText: t('ext.addUp') });
}

// ---- 新增 / 编辑 ----

function editExt(id) {
  const x = id ? state.exts.find(e => e.id === id) : { type: 'sub', enabled: true };
  openModal(id ? t('ext.edit') : t('ext.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(x.name || '')}" placeholder="${t('ext.namePh')}">`)}
      ${field(t('common.type'), `<select id="f-type"><option value="sub" ${x.type === 'sub' ? 'selected' : ''}>${t('ext.typeSub')}</option><option value="link" ${x.type === 'link' ? 'selected' : ''}>${t('ext.typeLink')}</option></select>`, t('ext.typeHelp'))}
      <div class="full">${field(t('ext.value'), `<textarea id="f-value" style="min-height:5rem">${esc(x.value || '')}</textarea>`, t('ext.valueHelp'))}</div>
      ${field(t('ext.prefix'), `<input id="f-prefix" value="${esc(x.prefix || '')}" placeholder="[中转] ">`, t('ext.prefixHelp'))}
      ${field(t('user.f.remark'), `<input id="f-remark" value="${esc(x.remark || '')}">`)}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${x.sort || 0}">`)}
      ${check('f-enabled', t('common.enabled'), x.enabled !== false)}
    </div>`, async () => {
    const body = { name: fv('f-name').trim(), type: fv('f-type'), value: fv('f-value').trim(), prefix: fv('f-prefix'), remark: fv('f-remark'), sort: Number(fv('f-sort')) || 0, enabled: fchk('f-enabled') };
    if (id) await put('exts/' + id, body); else await post('exts', body);
    delete cache[id]; // 地址可能改了,展开时重新拉
    await load('exts'); renderRows();
    toast(id ? t('ext.updated') : t('ext.created'), 'ok');
  }, { wide: true });
}

registerActions({
  'ext.add': () => editExt(null),
  'ext.edit': id => editExt(Number(id)),
  'ext.refresh': async (id, btn) => {
    btn.disabled = true;
    try { const r = await post(`exts/${id}/refresh`, undefined, SLOW); toast(t('ext.refreshed', { n: r.clash }), 'ok'); delete cache[Number(id)]; }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; await load('exts'); rowsSig = ''; renderRows(); }
  },
  'ext.del': async id => {
    const x = state.exts.find(e => e.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: x.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('exts/' + id); open.delete(Number(id)); await load('exts', 'users'); renderRows(); toast(t('ext.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'ext.toggle': id => { id = Number(id); if (open.has(id)) open.delete(id); else open.add(id); renderRows(); },
  'ext.pick': (id, el) => { const s = picked[id] || (picked[id] = new Set()); const i = Number(el.dataset.idx); if (el.checked) s.add(i); else s.delete(i); updateCount(Number(id)); },
  'ext.pickAll': (id, el) => {
    id = Number(id); const c = cache[id]; if (!c) return;
    const s = picked[id] || (picked[id] = new Set());
    for (const n of visible(id, c)) { if (el.checked) s.add(n.index); else s.delete(n.index); }
    renderSubBody(id);
  },
  'ext.detail': (id, el, e) => { if (e) e.preventDefault(); showDetail(Number(id), Number(el.dataset.idx)); },
  'ext.copy': async (id, el) => { await copy(el.dataset.link || ''); toast(t('common.copied'), 'ok'); },
  'ext.testAll': id => startTest(Number(id), []),
  'ext.testSel': id => { id = Number(id); const s = picked[id]; if (!s || !s.size) { toast(t('ext.pickFirst'), 'err'); return; } startTest(id, [...s]); },
  'ext.addUp': id => addUpstreams(Number(id)),
});

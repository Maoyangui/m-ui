import { state, load } from '../app.js';
import { post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, openModal, registerActions, badge, dot, field, check, empty, fv, fchk, matches, debounce } from '../ui.js';

export const title = () => t('line.title');
export const subtitle = () => t('line.subtitle');
let query = '';

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="line-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <span class="muted small" id="line-count"></span>
      <span class="grow"></span>
      <button class="btn primary" data-act="line.add">${t('line.add')}</button>
    </div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th></th><th>${t('common.name')}</th><th>${t('line.protocol')}</th><th>${t('common.port')}</th><th>${t('line.upstream')}</th><th>${t('line.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="lines-body"></tbody>
    </table></div>`;
  document.getElementById('line-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  renderRows();
}
export function tick() { renderRows(); }

function renderRows() {
  const body = document.getElementById('lines-body');
  if (!body) return;
  const online = new Set(state.onlines.lines || []);
  const rows = state.lines.filter(l => matches(query, l.name, l.protocol, l.port, l.upstreamName));
  document.getElementById('line-count').textContent = `${rows.length} / ${state.lines.length}`;
  if (!rows.length) { body.innerHTML = `<tr><td colspan="8">${empty()}</td></tr>`; return; }
  body.innerHTML = rows.map(l => `
    <tr draggable="${query ? 'false' : 'true'}" data-id="${l.id}">
      <td class="handle" title="拖动排序">⠿</td>
      <td class="primary-cell">${dot(online.has(l.name))}${esc(l.name)}</td>
      <td>${badge(l.protocol, l.protocol === 'hysteria2' ? 'primary' : '')}</td>
      <td class="num">${l.port}</td>
      <td>${esc(l.upstreamName)}</td>
      <td class="num">${l.userCount}</td>
      <td><label class="switch" title="${l.enabled ? t('common.enabled') : t('common.disabled')}"><input type="checkbox" data-change="line.toggle" data-id="${l.id}" ${l.enabled ? 'checked' : ''}><span></span></label></td>
      <td class="actions">
        <button class="btn sm" data-act="line.edit" data-id="${l.id}">${t('common.edit')}</button>
        <button class="btn sm danger" data-act="line.del" data-id="${l.id}">${t('common.delete')}</button>
      </td>
    </tr>`).join('');
  if (!query) enableDragSort(body);
}

function enableDragSort(tbody) {
  let dragged = null;
  tbody.querySelectorAll('tr').forEach(tr => {
    tr.ondragstart = () => { dragged = tr; tr.classList.add('dragging'); };
    tr.ondragend = async () => {
      tr.classList.remove('dragging');
      const ids = [...tbody.querySelectorAll('tr')].map(r => Number(r.dataset.id));
      try { await post('lines/sort', ids); await load('lines'); toast(t('line.sorted'), 'ok'); }
      catch (e) { toast(e.message, 'err'); }
    };
    tr.ondragover = e => {
      e.preventDefault();
      if (!dragged || dragged === tr) return;
      const rect = tr.getBoundingClientRect();
      tr.parentNode.insertBefore(dragged, (e.clientY - rect.top) / rect.height > 0.5 ? tr.nextSibling : tr);
    };
  });
}

function editLine(id) {
  const l = id ? state.lines.find(x => x.id === id) : { protocol: 'hysteria2', enabled: true, options: {}, upstreamId: 0 };
  const optText = typeof l.options === 'string' ? l.options : JSON.stringify(l.options ?? {}, null, 2);
  const addrText = l.addrs ? (typeof l.addrs === 'string' ? l.addrs : JSON.stringify(l.addrs, null, 2)) : '';
  openModal(id ? t('line.edit') : t('line.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(l.name || '')}">`, t('line.nameHelp'))}
      ${field(t('line.protocol'), `<select id="f-protocol">${['hysteria2', 'anytls', 'shadowsocks'].map(p => `<option value="${p}" ${l.protocol === p ? 'selected' : ''}>${p}</option>`).join('')}</select>`)}
      ${field(t('common.port'), `<input id="f-port" type="number" min="1" max="65535" value="${l.port || ''}">`, t('line.portHelp'))}
      ${field(t('line.upstream'), `<select id="f-upstream"><option value="0">${t('line.direct')}</option>${state.upstreams.map(u => `<option value="${u.id}" ${l.upstreamId === u.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')}</select>`, t('line.upstreamHelp'))}
      ${check('f-enabled', t('common.enabled'), l.enabled !== false)}
      <div class="full">${field(t('line.options'), `<textarea id="f-options">${esc(optText)}</textarea>`, t('line.optionsHelp'))}</div>
      <div class="full">${field(t('line.addrs'), `<textarea id="f-addrs">${esc(addrText)}</textarea>`, t('line.addrsHelp'))}</div>
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), protocol: fv('f-protocol'), port: Number(fv('f-port')),
      upstreamId: Number(fv('f-upstream')), enabled: fchk('f-enabled'),
      options: JSON.parse(fv('f-options').trim() || '{}'),
    };
    const addrs = fv('f-addrs').trim();
    if (addrs) body.addrs = JSON.parse(addrs);
    if (id) await put('lines/' + id, body); else await post('lines', body);
    await load('lines', 'status');
    renderRows();
    toast(id ? t('line.updated') : t('line.created'), 'ok');
  });
}

registerActions({
  'line.add': () => editLine(null),
  'line.edit': id => editLine(Number(id)),
  'line.del': async id => {
    const l = state.lines.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: l.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('lines/' + id); await load('lines', 'status'); renderRows(); toast(t('line.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'line.toggle': async (id, input) => {
    input.disabled = true;
    try { await post(`lines/${id}/toggle`); await load('lines'); toast(t('line.toggled'), 'ok'); }
    catch (e) { toast(e.message, 'err'); input.checked = !input.checked; }
    finally { input.disabled = false; }
  },
});

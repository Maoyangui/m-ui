import { state, load } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtRelative, fmtDuration, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk } from '../ui.js';

export const title = () => t('node.title');
export const subtitle = () => t('node.subtitle');
let data = { nodes: [], revision: '' };

export async function render(el) {
  data = await get('nodes');
  el.innerHTML = `
    <div class="toolbar">
      <span class="muted small">${t('node.revision')} <code>${esc(data.revision || '—')}</code></span>
      <span class="grow"></span>
      <button class="btn primary" data-act="node.add">${t('node.add')}</button>
    </div>
    <p class="hint" style="margin-bottom:.8rem">${t('node.howto')}</p>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('node.domain')}</th><th>${t('common.status')}</th><th>${t('node.sync')}</th><th>${t('node.core')}</th><th>${t('node.online')}</th><th></th></tr></thead>
      <tbody id="nodes-body"></tbody>
    </table></div>`;
  renderRows();
}

export async function tick() { if (!document.getElementById('nodes-body')) return; data = await get('nodes'); renderRows(); }

function statusCell(n) {
  if (n.isLocal) return badge(t('node.local'), 'primary');
  if (!n.enabled) return badge(t('common.disabled'));
  const s = n.status;
  if (!s) return badge(t('node.pending'), 'warn');
  if (s.ok) return `${badge(t('common.online'), 'ok')} <span class="muted small">${s.version ? 'v' + esc(s.version) : ''} ${s.hostname ? esc(s.hostname) : ''}</span>`;
  return `${badge(t('common.offline'), 'danger')}<div class="sub-cell" title="${esc(s.error || '')}">${esc((s.error || '').slice(0, 70))}${s.lastSeen ? ` · ${t('node.lastSeen')} ${fmtRelative(s.lastSeen)}` : ''}</div>`;
}

function renderRows() {
  const body = document.getElementById('nodes-body');
  if (!body) return;
  if (!data.nodes.length) { body.innerHTML = `<tr><td colspan="7">${empty()}</td></tr>`; return; }
  body.innerHTML = data.nodes.map(n => {
    const s = n.status || {};
    return `<tr>
      <td class="primary-cell">${esc(n.name)}${n.apiUrl ? `<div class="sub-cell mono">${esc(n.apiUrl)}</div>` : ''}</td>
      <td class="mono">${esc(n.domain || (n.isLocal ? state.settings.webDomain || '' : ''))}</td>
      <td>${statusCell(n)}</td>
      <td>${n.isLocal ? '—' : (s.ok ? (s.synced ? badge(t('node.synced'), 'ok') : badge(t('node.unsynced'), 'warn')) + (s.lastPush ? ` <span class="muted small">${fmtRelative(s.lastPush)}</span>` : '') : '—')}</td>
      <td>${n.isLocal ? badge(state.status.coreRunning ? t('dash.running') : t('dash.stopped'), state.status.coreRunning ? 'ok' : 'danger') : (s.ok ? badge(s.coreRunning ? t('dash.running') : t('dash.stopped'), s.coreRunning ? 'ok' : 'danger') + (s.uptime ? ` <span class="muted small">${fmtDuration(s.uptime)}</span>` : '') : '—')}${!n.isLocal && s.ok && s.certDays !== undefined ? ` <span class="muted small" title="${t('cert.daysLeft')}">🔒 ${s.certDays}d</span>` : ''}</td>
      <td class="num">${n.isLocal ? (state.status.onlineUsers ?? '—') : (s.ok ? s.onlineUsers : '—')}</td>
      <td class="actions">
        <button class="btn sm" data-act="node.test" data-id="${n.id}">${t('common.test')}</button>
        ${n.isLocal ? '' : `<button class="btn sm" data-act="node.push" data-id="${n.id}">${t('node.push')}</button>`}
        <button class="btn sm" data-act="node.edit" data-id="${n.id}">${t('common.edit')}</button>
        ${n.isLocal ? '' : `<button class="btn sm danger" data-act="node.del" data-id="${n.id}">${t('common.delete')}</button>`}
      </td></tr>`;
  }).join('');
}

function editNode(id) {
  const n = id ? data.nodes.find(x => x.id === id) : { enabled: true, insecure: true };
  openModal(id ? t('node.edit') : t('node.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(n.name || '')}" placeholder="台湾">`, t('node.nameHelp'))}
      ${field(t('node.domain'), `<input id="f-domain" value="${esc(n.domain || '')}" placeholder="tw.example.com">`, t('node.domainHelp'))}
      ${n.isLocal ? '' : `
      <div class="full">${field(t('node.apiUrl'), `<input id="f-api" value="${esc(n.apiUrl || '')}" placeholder="https://tw.example.com:2053/ad/">`, t('node.apiUrlHelp'))}</div>
      <div class="full">${field(t('node.token'), `<input id="f-token" type="password" placeholder="${n.hasToken ? t('node.tokenKeep') : ''}">`, t('node.tokenHelp'))}</div>
      ${check('f-insecure', t('node.insecure'), n.insecure !== false, t('node.insecureHelp'))}
      ${check('f-enabled', t('common.enabled'), n.enabled !== false)}`}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${n.sort || 0}">`)}
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), domain: fv('f-domain').trim(), sort: Number(fv('f-sort')) || 0,
      apiUrl: n.isLocal ? '' : fv('f-api').trim(), token: n.isLocal ? '' : fv('f-token'),
      insecure: n.isLocal ? false : fchk('f-insecure'), enabled: n.isLocal ? true : fchk('f-enabled'),
    };
    if (id) await put('nodes/' + id, body); else await post('nodes', body);
    await load('settings');
    toast(t('set.saved'), 'ok');
    render(document.getElementById('page'));
  }, { wide: true });
}

registerActions({
  'node.add': () => editNode(null),
  'node.edit': id => editNode(Number(id)),
  'node.test': async (id, btn) => {
    btn.disabled = true;
    try {
      const r = await post(`nodes/${id}/test`);
      if (r.ok) toast(t('node.testOk', { v: r.version || '', core: r.coreRunning ? t('dash.running') : t('dash.stopped') }), 'ok');
      else toast(r.error, 'err');
    } catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'node.push': async (id, btn) => {
    btn.disabled = true;
    try { await post(`nodes/${id}/push`); toast(t('node.pushed'), 'ok'); data = await get('nodes'); renderRows(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'node.del': async id => {
    const n = data.nodes.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: n.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('nodes/' + id); render(document.getElementById('page')); toast(t('common.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});

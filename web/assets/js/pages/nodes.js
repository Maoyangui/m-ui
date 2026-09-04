import { state, load } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, fmtRelative, fmtDuration, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk } from '../ui.js';

export const title = () => t('node.title');
export const subtitle = () => t('node.subtitle');
let data = { nodes: [], revision: '', role: 'master', masterId: 0, appliedAt: '' };
const isNodeView = () => data.role === 'node';

export async function render(el) {
  data = await get('nodes');
  el.innerHTML = `
    <div class="toolbar">
      <span class="muted small">${t('node.revision')} <code>${esc(data.revision || '—')}</code></span>
      <span class="grow"></span>
      ${isNodeView() ? '' : `<button class="btn primary" data-act="node.add">${t('node.add')}</button>`}
    </div>
    <p class="hint" style="margin-bottom:.8rem">${isNodeView() ? t('node.nodeView') : t('node.howto')}</p>
    <div class="table-wrap"><table class="grid tight nodes">
      <thead><tr><th>${t('common.name')}</th><th>${t('node.domain')}</th><th>${t('common.status')}</th><th>${t('node.sync')}</th><th>${t('node.core')}</th><th>${t('node.online')}</th><th></th></tr></thead>
      <tbody id="nodes-body"></tbody>
    </table></div>`;
  renderRows();
}

export async function tick() { if (!document.getElementById('nodes-body')) return; data = await get('nodes'); renderRows(); }

// 副机视角:自己是"本机",主机那行标"主机 · 最近同步",其它副机标"主机管理";副机不探测别的机器
function statusCell(n) {
  if (n.isLocal) return badge(t('node.local'), 'primary');
  if (isNodeView()) {
    if (n.id === data.masterId) return `${badge(t('role.master'), 'primary')}${data.appliedAt ? ` <span class="muted small">${t('node.lastSync')} ${fmtRelative(Number(data.appliedAt))}</span>` : ''}`;
    return badge(t('node.byMaster'));
  }
  if (!n.enabled) return badge(t('common.disabled'));
  const s = n.status;
  if (!s) return badge(t('node.pending'), 'warn');
  if (s.ok) return `${badge(t('common.online'), 'ok')}${s.version ? ` <span class="muted small">v${esc(s.version)}</span>` : ''}${s.hostname ? `<div class="sub-cell ellip" title="${esc(s.hostname)}">${esc(s.hostname)}</div>` : ''}`;
  return `${badge(t('common.offline'), 'danger')}<div class="sub-cell ellip" title="${esc(s.error || '')}">${esc(shortErr(s.error))}${s.lastSeen ? ` · ${t('node.lastSeen')} ${fmtRelative(s.lastSeen)}` : ''}</div>`;
}

// 副机报错原文是 Go 的网络错误,又长又带完整 URL(列表里会把表格撑出横向滚动条)。
// 这里只显示原因,完整内容留在 title 里。
function shortErr(raw) {
  const e = String(raw || '');
  if (!e) return t('node.errUnknown');
  // 用整词匹配状态码:端口 4031、IP 段里都可能出现 "403" 这三个字
  const map = [
    [/context deadline exceeded|timeout/, 'node.errTimeout'],
    [/connection refused/, 'node.errRefused'],
    [/no such host|dns/, 'node.errDns'],
    [/certificate|x509/, 'node.errCert'],
    [/\b40[13]\b/, 'node.errToken'],
  ];
  for (const [re, key] of map) if (re.test(e.toLowerCase())) return t(key);
  const tail = e.split(': ').pop().trim();
  return tail.length > 48 ? tail.slice(0, 48) + '…' : tail;
}

function syncCell(n) {
  if (n.isLocal) return '—';
  if (isNodeView()) return n.id === data.masterId && data.appliedAt ? badge(t('node.synced'), 'ok') : '—';
  const s = n.status || {};
  if (!s.ok) return '—';
  return (s.synced ? badge(t('node.synced'), 'ok') : badge(t('node.unsynced'), 'warn')) + (s.lastPush ? ` <span class="muted small">${fmtRelative(s.lastPush)}</span>` : '');
}

function coreCell(n) {
  if (n.isLocal) return badge(state.status.coreRunning ? t('dash.running') : t('dash.stopped'), state.status.coreRunning ? 'ok' : 'danger');
  const s = n.status || {};
  if (!s.ok) return '—';
  return badge(s.coreRunning ? t('dash.running') : t('dash.stopped'), s.coreRunning ? 'ok' : 'danger')
    + (s.uptime ? ` <span class="muted small">${fmtDuration(s.uptime)}</span>` : '')
    + (s.certDays !== undefined ? ` <span class="muted small" title="${t('cert.daysLeft')}">🔒 ${s.certDays}d</span>` : '');
}

function actionsCell(n) {
  if (isNodeView()) return '';
  return `<button class="btn sm" data-act="node.test" data-id="${n.id}">${t('common.test')}</button>
        ${n.isLocal ? '' : `<button class="btn sm" data-act="node.push" data-id="${n.id}">${t('node.push')}</button>`}
        <button class="btn sm" data-act="node.edit" data-id="${n.id}">${t('common.edit')}</button>
        ${n.isLocal ? '' : `<button class="btn sm danger" data-act="node.del" data-id="${n.id}">${t('common.delete')}</button>`}`;
}

function renderRows() {
  const body = document.getElementById('nodes-body');
  if (!body) return;
  if (!data.nodes.length) { body.innerHTML = `<tr><td colspan="7">${empty()}</td></tr>`; return; }
  body.innerHTML = data.nodes.map(n => {
    const s = n.status || {};
    const domain = n.domain || (n.isLocal ? state.settings.webDomain || '' : '');
    const addr = n.addr || n.publicIp || '';
    return `<tr>
      <td class="primary-cell">${esc(n.name)}${n.ratio && n.ratio !== 1 ? ' ' + badge('x' + n.ratio, 'warn') : ''}${n.apiUrl && !isNodeView() ? `<div class="sub-cell mono ellip" title="${esc(n.apiUrl)}">${esc(n.apiUrl)}</div>` : ''}</td>
      <td class="mono"><span class="ellip" title="${esc(domain)}">${esc(domain || '—')}</span>${addr ? `<div class="sub-cell mono">${esc(addr)}${n.addr ? ' · ' + t('node.addrManual') : ''}</div>` : ''}</td>
      <td>${statusCell(n)}</td>
      <td>${syncCell(n)}</td>
      <td>${coreCell(n)}</td>
      <td class="num">${n.isLocal ? (state.status.onlineUsers ?? '—') : (s.ok ? s.onlineUsers : '—')}</td>
      <td class="actions">${actionsCell(n)}</td></tr>`;
  }).join('');
}

function editNode(id) {
  const n = id ? data.nodes.find(x => x.id === id) : { enabled: true, insecure: true };
  openModal(id ? t('node.edit') : t('node.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(n.name || '')}" placeholder="台湾">`, t('node.nameHelp'))}
      ${field(t('node.domain'), `<input id="f-domain" value="${esc(n.domain || '')}" placeholder="tw.example.com">`, t('node.domainHelp'))}
      ${field(t('node.addr'), `<input id="f-addr" value="${esc(n.addr || '')}" placeholder="${esc(n.publicIp || t('node.addrAuto'))}">`, t('node.addrHelp'))}
      ${field(t('node.ratio'), `<input id="f-ratio" type="number" min="0" max="100" step="0.1" value="${n.ratio || 1}">`, t('node.ratioHelp'))}
      ${n.isLocal ? '' : `
      <div class="full">${field(t('node.apiUrl'), `<input id="f-api" value="${esc(n.apiUrl || '')}" placeholder="https://tw.example.com:2053/app/">`, t('node.apiUrlHelp'))}</div>
      <div class="full">${field(t('node.token'), `<input id="f-token" type="password" placeholder="${n.hasToken ? t('node.tokenKeep') : ''}">`, t('node.tokenHelp'))}</div>
      ${check('f-insecure', t('node.insecure'), n.insecure !== false, t('node.insecureHelp'))}
      ${check('f-enabled', t('common.enabled'), n.enabled !== false)}`}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${n.sort || 0}">`)}
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), domain: fv('f-domain').trim(), sort: Number(fv('f-sort')) || 0,
      addr: fv('f-addr').trim(), ratio: Number(fv('f-ratio')) || 1,
      apiUrl: n.isLocal ? '' : fv('f-api').trim(), token: n.isLocal ? '' : fv('f-token'),
      insecure: n.isLocal ? false : fchk('f-insecure'), enabled: n.isLocal ? true : fchk('f-enabled'),
    };
    if (id) await put('nodes/' + id, body); else await post('nodes', body);
    await load('settings', 'nodes'); // 线路编辑器里的"部署到服务器"依赖 state.nodes
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
    try { await del('nodes/' + id); await load('nodes'); render(document.getElementById('page')); toast(t('common.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});

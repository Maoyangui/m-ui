import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtRelative, fmtDuration, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk, setHTML } from '../ui.js';
import { loadReach, reachCell, reachToolbar, refreshDetail, setNodes, setReachListener } from '../reach.js';

export const title = () => t('node.title');
export const subtitle = () => t('node.subtitle');
let data = { nodes: [], revision: '', role: 'master', masterId: 0, appliedAt: '' };
const isNodeView = () => data.role === 'node';

setNodes(() => data.nodes);
setReachListener(() => { renderReachBar(); renderRows(); refreshDetail(); });

export async function render(el) {
  [data] = await Promise.all([get('nodes'), loadReach()]);
  el.innerHTML = `
    <div class="toolbar">
      <span class="muted small">${t('node.revision')} <code>${esc(data.revision || '—')}</code></span>
      <span class="grow"></span>
      <span class="row" id="reach-bar"></span>
      ${isNodeView() ? '' : `<button class="btn primary" data-act="node.add">${t('node.add')}</button>`}
    </div>
    <p class="hint" style="margin-bottom:.8rem">${isNodeView() ? t('node.nodeView') : t('node.howto')}</p>
    <div class="table-wrap"><table class="grid tight nodes">
      <thead><tr><th>${t('common.name')}</th><th>${t('node.domain')}</th><th>${t('common.status')}</th><th title="${esc(t('reach.colHint'))}">${t('reach.col')}</th><th>${t('node.sync')}</th><th>${t('node.core')}</th><th>${t('node.online')}</th><th></th></tr></thead>
      <tbody id="nodes-body"></tbody>
    </table></div>`;
  renderReachBar();
  renderRows();
}

export async function tick() {
  if (!document.getElementById('nodes-body')) return;
  [data] = await Promise.all([get('nodes'), loadReach()]);
  renderReachBar();
  renderRows();
  refreshDetail();
}

function renderReachBar() { setHTML('reach-bar', reachToolbar(isNodeView())); }

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
  // 在线但有话要说(配置推不下去、数据面没应用成功):这台机器并没有离线,但管理员必须看见
  if (s.ok) return `${badge(t('common.online'), 'ok')}${s.version ? ` <span class="muted small">v${esc(s.version)}</span>` : ''}${s.versionMismatch ? ' ' + badge(t('node.versionMismatch'), 'warn') : ''}${s.error ? `<div class="sub-cell ellip warn-text" title="${esc(trErr(s.error))}">${esc(shortErr(s.error))}</div>` : (s.hostname ? `<div class="sub-cell ellip" title="${esc(s.hostname)}">${esc(s.hostname)}</div>` : '')}`;
  return `${badge(t('common.offline'), 'danger')}<div class="sub-cell ellip" title="${esc(trErr(s.error || ''))}">${esc(shortErr(s.error))}${s.lastSeen ? ` · ${t('node.lastSeen')} ${fmtRelative(s.lastSeen)}` : ''}</div>`;
}

// 副机报错原文是 Go 的网络错误,又长又带完整 URL(列表里会把表格撑出横向滚动条)。
// 这里只显示原因,完整内容留在 title 里。
function shortErr(raw) {
  const e = trErr(String(raw || ''));
  if (!e) return t('node.errUnknown');
  // 用整词匹配状态码:端口 4031、IP 段里都可能出现 "403" 这三个字
  const map = [
    [/context deadline exceeded|timeout/, 'node.errTimeout'],
    [/connection refused/, 'node.errRefused'],
    [/no such host|dns/, 'node.errDns'],
    [/指纹|fingerprint/, 'node.errFp'],
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
  // 时间显示"最后一次联系"(每 5 秒一轮)。推送只在配置真的变了才发,拿它当时间会让人以为同步停了。
  const push = s.lastPush ? `${t('node.pushedAt')} ${fmtRelative(s.lastPush)}` : t('node.neverPushed');
  return (s.synced ? badge(t('node.synced'), 'ok') : badge(t('node.unsynced'), 'warn')) +
    (s.lastSeen ? ` <span class="muted small" title="${esc(push)}">${fmtRelative(s.lastSeen)}</span>` : '');
}

// 重载失败:线上还是旧配置,要在这一行里看得见
const reloadBadge = err => err ? ` <span class="badge danger" title="${esc(err)}">${t('node.reloadFailed')}</span>` : '';

function coreCell(n) {
  if (n.isLocal) {
    const rl = state.status.reload || {};
    return badge(state.status.coreRunning ? t('dash.running') : t('dash.stopped'), state.status.coreRunning ? 'ok' : 'danger')
      + reloadBadge(rl.at && !rl.ok ? `${trErr(rl.op)}: ${trErr(rl.error)}` : '');
  }
  const s = n.status || {};
  if (!s.ok) return '—';
  return badge(s.coreRunning ? t('dash.running') : t('dash.stopped'), s.coreRunning ? 'ok' : 'danger')
    + (s.uptime ? ` <span class="muted small">${fmtDuration(s.uptime)}</span>` : '')
    + (s.certDays !== undefined ? ` <span class="muted small" title="${t('cert.daysLeft')}">🔒 ${s.certDays}d</span>` : '')
    + reloadBadge(s.reloadError);
}

function actionsCell(n) {
  if (isNodeView()) return '';
  return `<button class="btn sm" data-act="node.test" data-id="${n.id}">${t('common.test')}</button>
        ${n.isLocal ? '' : `<button class="btn sm" data-act="node.push" data-id="${n.id}">${t('node.push')}</button>`}
        ${n.isLocal || !n.certFp ? '' : `<button class="btn sm" data-act="node.resetCert" data-id="${n.id}" title="${esc(n.certFp)}">${t('node.resetCert')}</button>`}
        <button class="btn sm" data-act="node.edit" data-id="${n.id}">${t('common.edit')}</button>
        ${n.isLocal ? '' : `<button class="btn sm danger" data-act="node.del" data-id="${n.id}">${t('common.delete')}</button>`}`;
}

function renderRows() {
  const body = document.getElementById('nodes-body');
  if (!body) return;
  if (!data.nodes.length) { setHTML(body, `<tr><td colspan="8">${empty()}</td></tr>`); return; }
  // 每 5 秒刷新一次:内容没变就不动 DOM(整表重画会让鼠标下的按钮、提示闪一下)
  setHTML(body, data.nodes.map(n => {
    const s = n.status || {};
    const domain = n.domain || (n.isLocal ? state.settings.webDomain || '' : '');
    const addr = n.addr || n.publicIp || '';
    return `<tr>
      <td class="primary-cell">${esc(n.name)}${n.ratio && n.ratio !== 1 ? ' ' + badge('x' + n.ratio, 'warn') : ''}${n.apiUrl && !isNodeView() ? `${isPlain(n.apiUrl) ? ` <span title="${esc(t('node.plainHelp'))}">${badge(t('node.plain'), 'warn')}</span>` : ''}<div class="sub-cell mono ellip" title="${esc(n.apiUrl)}">${esc(n.apiUrl)}</div>` : ''}</td>
      <td class="mono"><span class="ellip" title="${esc(domain)}">${esc(domain || '—')}</span>${addr ? `<div class="sub-cell mono nowrap">${esc(addr)}${n.addr ? ' · ' + t('node.addrManual') : ''}</div>` : ''}</td>
      <td>${statusCell(n)}</td>
      <td class="reach-td">${reachCell(n, isNodeView())}</td>
      <td>${syncCell(n)}</td>
      <td>${coreCell(n)}</td>
      <td class="num">${n.isLocal ? (state.status.onlineUsers ?? '—') : (s.ok ? s.onlineUsers : '—')}</td>
      <td class="actions">${actionsCell(n)}</td></tr>`;
  }).join(''));
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
      <div class="full">${field(t('node.apiUrl'), `<input id="f-api" value="${esc(n.apiUrl || '')}" placeholder="https://tw.example.com:2053/app/">`, t('node.apiUrlHelp'))}<p class="hint warn-text" id="f-api-plain" ${isPlain(n.apiUrl) ? '' : 'hidden'}>${esc(t('node.plainHelp'))}</p></div>
      <div class="full">${field(t('node.token'), `<input id="f-token" type="password" placeholder="${n.hasToken ? t('node.tokenKeep') : ''}">`, t('node.tokenHelp'))}</div>
      ${check('f-insecure', t('node.insecure'), n.insecure !== false, t('node.insecureHelp'))}
      ${check('f-enabled', t('common.enabled'), n.enabled !== false, t('node.enabledHelp'))}`}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${n.sort || 0}">`)}
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), domain: fv('f-domain').trim(), sort: Number(fv('f-sort')) || 0,
      addr: fv('f-addr').trim(), ratio: Number(fv('f-ratio')) || 1,
      apiUrl: n.isLocal ? '' : fv('f-api').trim(), token: n.isLocal ? '' : fv('f-token'),
      insecure: n.isLocal ? false : fchk('f-insecure'), enabled: n.isLocal ? true : fchk('f-enabled'),
    };
    const r = id ? await put('nodes/' + id, body, SLOW) : await post('nodes', body); // 停用时主机要先给它推空用户表
    await load('settings', 'nodes'); // 线路编辑器里的"部署到服务器"依赖 state.nodes
    toast(t('set.saved'), 'ok');
    render(document.getElementById('page'));
    if (r && r.decommissionError) notice(t('node.decomFailTitle'), t('node.decomFail', { name: body.name, err: r.decommissionError }));
  }, { wide: true });
  const api = document.getElementById('f-api');
  if (api) api.addEventListener('input', () => { document.getElementById('f-api-plain').hidden = !isPlain(api.value); });
}

// 必须让人看见的结果(下线通知没发到、线路被一并停用):弹窗,不用两秒多就消失的 toast。等当前弹窗关掉再开
const notice = (title, msg) => setTimeout(() => confirm(msg, { title, okText: t('node.ack'), notice: true }), 0);

// 副机 API 地址是 http:// 时,令牌与用户凭据走明文:不拦,但要看得见
const isPlain = url => /^http:\/\//i.test(String(url || '').trim());

registerActions({
  'node.add': () => editNode(null),
  'node.edit': id => editNode(Number(id)),
  'node.test': async (id, btn) => {
    btn.disabled = true;
    try {
      const r = await post(`nodes/${id}/test`, undefined, SLOW);
      if (r.ok) toast(t('node.testOk', { v: r.version || '', core: r.coreRunning ? t('dash.running') : t('dash.stopped') }), 'ok');
      else toast(trErr(r.error), 'err');
    } catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'node.push': async (id, btn) => {
    btn.disabled = true;
    try { await post(`nodes/${id}/push`, undefined, SLOW); toast(t('node.pushed'), 'ok'); data = await get('nodes'); renderRows(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'node.resetCert': async (id, btn) => {
    if (!await confirm(t('node.resetCertConfirm'), { okText: t('node.resetCert') })) return;
    btn.disabled = true;
    try { await post(`nodes/${id}/resetcert`); toast(t('node.resetCertDone'), 'ok'); data = await get('nodes'); renderRows(); }
    catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'node.del': async id => {
    const n = data.nodes.find(x => x.id === Number(id));
    if (!await confirm(t('node.delConfirm', { name: n.name }), { danger: true, okText: t('common.delete') })) return;
    try {
      const r = await del('nodes/' + id, SLOW); // 主机要先给它推空用户表
      await load('nodes'); render(document.getElementById('page')); toast(t('common.deleted'), 'ok');
      const msgs = [];
      if ((r.disabledLines || []).length) msgs.push(t('node.linesDisabled', { names: r.disabledLines.join(t('node.sep')) }));
      const revoked = [...(r.revokedUsers || []), ...(r.revokedResellers || [])];
      if (revoked.length) msgs.push(t('node.scopeRevoked', { names: revoked.join(t('node.sep')) }));
      if (r.decommissionError) msgs.push(t('node.decomFail', { name: n.name, err: r.decommissionError }));
      if (msgs.length) notice(t(r.decommissionError ? 'node.decomFailTitle' : 'common.deleted'), msgs.join('\n'));
    } catch (e) { toast(e.message, 'err'); }
  },
});

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

// 探测到的 IPv4 / IPv6(publicIp 有 IPv4 时是 IPv4,老版本副机在纯 IPv6 机器上报的是 v6)
const isV4 = s => /^\d{1,3}(\.\d{1,3}){3}$/.test(s);
function detected(n) {
  const pub = (n.publicIp || '').trim(), pub6 = (n.publicIp6 || '').trim();
  return { v4: isV4(pub) ? pub : '', v6: pub6.includes(':') ? pub6 : (pub.includes(':') ? pub : '') };
}
// 连接地址留空时订阅里用哪个(与后端 model.AutoIP 一致):选 IPv6 优先 v6,否则优先 v4;选的那个没有就用另一个
function autoAddr(n, fam = n.addrFamily) {
  const { v4, v6 } = detected(n);
  if (v6 && (fam === 'v6' || !v4)) return { ip: v6, fam: 'v6' };
  return v4 ? { ip: v4, fam: 'v4' } : { ip: '', fam: '' };
}
const detectedText = n => { const d = detected(n); return t('node.detected', { v4: d.v4 || t('node.none'), v6: d.v6 || t('node.none') }); };

// 服务器列表里域名下面那行地址:手填的标"手动";自动的标出 IPv6、或选了 IPv6 却没有 IPv6
function addrCell(n) {
  const auto = autoAddr(n);
  const addr = n.addr || auto.ip;
  if (!addr) return '';
  const tag = n.addr ? t('node.addrManual')
    : n.addrFamily === 'v6' && auto.fam === 'v4' ? t('node.v6Fallback')
    : auto.fam === 'v6' ? 'IPv6' : '';
  // IPv4 不从中间折;IPv6 太长时只在冒号后折行
  const ip = addr.includes(':') ? esc(addr).replace(/:/g, ':<wbr>') : `<span class="nowrap">${esc(addr)}</span>`;
  return `<div class="sub-cell mono" title="${esc(detectedText(n))}">${ip}${tag ? ` <span class="nowrap">· ${esc(tag)}</span>` : ''}</div>`;
}

function renderRows() {
  const body = document.getElementById('nodes-body');
  if (!body) return;
  if (!data.nodes.length) { setHTML(body, `<tr><td colspan="8">${empty()}</td></tr>`); return; }
  // 每 5 秒刷新一次:内容没变就不动 DOM(整表重画会让鼠标下的按钮、提示闪一下)
  setHTML(body, data.nodes.map(n => {
    const s = n.status || {};
    const domain = n.domain || (n.isLocal ? state.settings.webDomain || '' : '');
    return `<tr>
      <td class="primary-cell">${esc(n.name)}${n.ratio && n.ratio !== 1 ? ' ' + badge('x' + n.ratio, 'warn') : ''}${n.apiUrl && !isNodeView() ? `${isPlain(n.apiUrl) ? ` <span title="${esc(t('node.plainHelp'))}">${badge(t('node.plain'), 'warn')}</span>` : ''}<div class="sub-cell mono ellip" title="${esc(n.apiUrl)}">${esc(n.apiUrl)}</div>` : ''}</td>
      <td class="mono"><span class="ellip" title="${esc(domain)}">${esc(domain || '—')}</span>${addrCell(n)}</td>
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
      ${field(t('node.addr'), `<input id="f-addr" value="${esc(n.addr || '')}" placeholder="${esc(autoAddr(n).ip || t('node.addrAuto'))}">`, t('node.addrHelp'))}
      ${field(t('node.family'), `<select id="f-family"><option value="">${t('node.familyV4')}</option><option value="v6" ${n.addrFamily === 'v6' ? 'selected' : ''}>IPv6</option></select>`, t('node.familyHelp') + (id ? ' ' + detectedText(n) : ''))}
      ${field(t('node.ratio'), `<input id="f-ratio" type="number" min="0" max="100" step="0.1" value="${n.ratio || 1}">`, t('node.ratioHelp'))}
      ${field(t('common.sort'), `<input id="f-sort" type="number" value="${n.sort || 0}">`)}
      ${n.isLocal ? '' : `
      <div class="full">${field(t('node.apiUrl'), `<input id="f-api" value="${esc(n.apiUrl || '')}" placeholder="https://tw.example.com:2053/app/">`, t('node.apiUrlHelp'))}<p class="hint warn-text" id="f-api-plain" ${isPlain(n.apiUrl) ? '' : 'hidden'}>${esc(t('node.plainHelp'))}</p></div>
      <div class="full">${field(t('node.token'), `<input id="f-token" type="password" placeholder="${n.hasToken ? t('node.tokenKeep') : ''}">`, t('node.tokenHelp'))}</div>
      ${check('f-insecure', t('node.insecure'), n.insecure !== false, t('node.insecureHelp'))}
      ${check('f-enabled', t('common.enabled'), n.enabled !== false, t('node.enabledHelp'))}`}
    </div>`, async () => {
    const body = {
      name: fv('f-name').trim(), domain: fv('f-domain').trim(), sort: Number(fv('f-sort')) || 0,
      addr: fv('f-addr').trim(), addrFamily: fv('f-family'), ratio: Number(fv('f-ratio')) || 1,
      apiUrl: n.isLocal ? '' : fv('f-api').trim(), token: n.isLocal ? '' : fv('f-token'),
      insecure: n.isLocal ? false : fchk('f-insecure'), enabled: n.isLocal ? true : fchk('f-enabled'),
    };
    const r = id ? await put('nodes/' + id, body, SLOW) : await post('nodes', body); // 停用时主机要先给它推空用户表
    await load('settings', 'nodes'); // 线路编辑器里的"部署到服务器"依赖 state.nodes
    toast(t('set.saved'), 'ok');
    render(document.getElementById('page'));
    if (r && r.decommissionError) notice(t('node.decomFailTitle'), t('node.decomFail', { name: body.name, err: trErr(r.decommissionError) }));
  }, { wide: true });
  const api = document.getElementById('f-api');
  if (api) api.addEventListener('input', () => { document.getElementById('f-api-plain').hidden = !isPlain(api.value); });
  // 换了地址族,连接地址框的灰字跟着换成会用的那个地址
  document.getElementById('f-family').addEventListener('change', e => {
    document.getElementById('f-addr').placeholder = autoAddr(n, e.target.value).ip || t('node.addrAuto');
  });
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
      if (r.decommissionError) msgs.push(t('node.decomFail', { name: n.name, err: trErr(r.decommissionError) }));
      if (msgs.length) notice(t(r.decommissionError ? 'node.decomFailTitle' : 'common.deleted'), msgs.join('\n'));
    } catch (e) { toast(e.message, 'err'); }
  },
});

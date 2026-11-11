import { state, load, takeHashParams } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, fmtRelative, fmtDuration, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk, setHTML, matches, debounce } from '../ui.js';
import { loadReach, reachCell, reachToolbar, refreshDetail, setNodes, setReachListener, reach, isBad, v6Of } from '../reach.js';
import { nodeIssues, trackUnsynced } from '../issues.js';
import { loadHA, roleChip, haMenuItems, setHARefresh } from '../ha.js';

export const title = () => t('node.title');
export const subtitle = () => t('node.subtitle');
let data = { nodes: [], revision: '', role: 'master', masterId: 0, appliedAt: '' };
const isNodeView = () => data.role === 'node';
let query = '', onlyBad = false;
const unsyncedSince = new Map(); // 服务器名 → 第一次看到"未同步"的时间(一次保存后副机会有几秒未同步,不到 30 秒不算异常)
const MANY = 10; // 服务器超过这么多才给搜索框和「只看异常」(从概况点进来时不论多少台都给)

setNodes(() => data.nodes);
setReachListener(() => { renderReachBar(); renderRows(); refreshDetail(); });
setHARefresh(() => renderRows());

export async function render(el) {
  const hp = takeHashParams('nodes'); // 概况、提示条的「查看」:#/nodes?f=bad
  if (hp.has('f')) { onlyBad = hp.get('f') === 'bad'; query = ''; }
  [data] = await Promise.all([get('nodes'), loadReach(), loadHA()]);
  trackUnsynced(unsyncedSince, data.nodes);
  const tools = data.nodes.length > MANY || onlyBad;
  el.innerHTML = `
    <div class="toolbar">
      <span class="muted small">${t('node.revision')} <code>${esc(data.revision || '—')}</code></span>
      ${tools ? `<input type="search" id="node-q" class="sm" style="max-width:13rem" placeholder="${t('common.search')}…" value="${esc(query)}">
      <label class="small muted" style="display:inline-flex;align-items:center;gap:.3rem" title="${esc(t('node.onlyBadHelp'))}"><input type="checkbox" id="node-onlybad" ${onlyBad ? 'checked' : ''}> ${t('node.onlyBad')}</label>
      <span class="muted small" id="node-count"></span>` : ''}
      <span class="grow"></span>
      <span class="row" id="reach-bar"></span>
      ${isNodeView() ? '' : `<button class="btn primary" data-act="node.add">${t('node.add')}</button>`}
    </div>
    <p class="hint" style="margin-bottom:.8rem">${isNodeView() ? t('node.nodeView') : t('node.howto')}</p>
    <div id="decom-gone"></div>
    <div class="table-wrap fill"><table class="grid tight nodes">
      <thead><tr><th>${t('common.name')}</th><th>${t('node.domain')}</th><th>${t('common.status')}</th><th class="c-reach" title="${esc(t('reach.colHint'))}">${t('reach.col')}</th><th class="c-sync c-p3">${t('node.sync')}</th><th class="c-core">${t('node.core')}</th><th class="c-online c-p3">${t('node.online')}</th><th class="c-act"></th></tr></thead>
      <tbody id="nodes-body"></tbody>
    </table></div>`;
  const q = document.getElementById('node-q');
  if (q) q.addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  const ob = document.getElementById('node-onlybad');
  if (ob) ob.addEventListener('change', e => { onlyBad = e.target.checked; renderRows(); });
  renderReachBar();
  renderRows();
  renderDecomGone();
}

export async function tick() {
  if (!document.getElementById('nodes-body')) return;
  [data] = await Promise.all([get('nodes'), loadReach(), loadHA()]);
  trackUnsynced(unsyncedSince, data.nodes);
  renderReachBar();
  renderRows();
  renderDecomGone();
  refreshDetail();
}

// 已经删掉、但还没收到下线通知的服务器:列表里没有它们那一行了,单独提示(主机一直在重试)
function renderDecomGone() {
  const gone = data.decommissions || [];
  setHTML('decom-gone', gone.length && !isNodeView()
    ? `<p class="hint warn-text" style="margin-bottom:.8rem">${esc(t('node.decomPendingGone', { names: gone.map(d => d.name).join(t('node.sep')) }))}</p>` : '');
}

// 停用的副机还没收到下线通知:它仍按旧配置为所有用户服务,得让管理员看见
const decomBadge = n => n.decommission
  ? ` <span title="${esc(t('node.decomPendingHelp', { n: n.decommission.attempts || 0, err: trErr(n.decommission.lastError || '—') }))}">${badge(t('node.decomPending'), 'warn')}</span>` : '';

// 副机自己报的记账失败、主机发现的账本回退(24 小时内):在线也要看得见
function ledgerBadges(s) {
  let out = '';
  if (s.ledgerFails) out += ` <span title="${esc(t('node.ledgerFailsHelp', { n: s.ledgerFails }))}">${badge(t('node.ledgerFails', { n: s.ledgerFails }), 'danger')}</span>`;
  if (s.ledgerRegressAt && Date.now() / 1000 - s.ledgerRegressAt < 86400) {
    out += ` <span title="${esc(t('node.ledgerRegressHelp', { n: s.ledgerRegressUsers || 0, time: fmtRelative(s.ledgerRegressAt) }))}">${badge(t('node.ledgerRegress'), 'warn')}</span>`;
  }
  return out;
}

// 订阅里的节点地址设成了"域名"(设置 → 订阅):连接地址、自动地址用只对单独选了 IP 版本的线路生效,没填域名的副机不进订阅
const subByDomain = () => String((state.settings || {}).subServerAddr || '').toLowerCase() === 'domain';

function renderReachBar() { setHTML('reach-bar', reachToolbar(isNodeView())); }

// 副机视角:自己是"本机",主机那行标"主机 · 最近同步",其它副机标"主机管理";副机不探测别的机器。
// 第一个标签是主机容灾里的角色(主机 · 本机 / 备用 N / 副机 / 原主机 · 失联),点开是小卡片(见 ha.js)
function statusCell(n) {
  const chip = roleChip(n, data);
  if (n.isLocal) return chip || badge(t('node.local'), 'primary');
  if (isNodeView()) {
    if (n.id === data.masterId) return `${chip || badge(t('role.master'), 'primary')}${data.appliedAt ? ` <span class="muted small">${t('node.lastSync')} ${fmtRelative(Number(data.appliedAt))}</span>` : ''}`;
    return chip || badge(t('node.byMaster'));
  }
  if (!n.enabled) return chip + badge(t('common.disabled')) + decomBadge(n);
  const s = n.status;
  if (!s) return chip + badge(t('node.pending'), 'warn');
  // 在线但有话要说(配置推不下去、数据面没应用成功、记账出问题):这台机器并没有离线,但管理员必须看见
  if (s.ok) return `${chip}${badge(t('common.online'), 'ok')}${s.version ? ` <span class="muted small">v${esc(s.version)}</span>` : ''}${s.versionMismatch ? ' ' + badge(t('node.versionMismatch'), 'warn') : ''}${s.noConnFails ? ` <span title="${esc(t('node.noConnFailsHelp'))}">${badge(t('node.noConnFails'))}</span>` : ''}${ledgerBadges(s)}${s.error ? `<div class="sub-cell ellip warn-text" title="${esc(trErr(s.error))}">${esc(shortErr(s.error))}</div>` : (s.hostname ? `<div class="sub-cell ellip" title="${esc(s.hostname)}">${esc(s.hostname)}</div>` : '')}`;
  return `${chip}${badge(t('common.offline'), 'danger')}<div class="sub-cell ellip" title="${esc(trErr(s.error || ''))}">${esc(shortErr(s.error))}${s.lastSeen ? ` · ${t('node.lastSeen')} ${fmtRelative(s.lastSeen)}` : ''}</div>`;
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

// 证书自动续期在失败:红色徽标,悬停看是哪张、为什么(副机一般没配通知,以前只看得到锁后面的天数)
function certFailBadge(n) {
  let list = [];
  try { list = Array.isArray(n.certs) ? n.certs : (n.certs ? JSON.parse(n.certs) : []); } catch { list = []; }
  const bad = list.filter(c => c.autoRenew && c.renewErr);
  if (!bad.length) return '';
  const why = bad.map(c => `${c.name}: ${trErr(c.renewErr)}`).join('\n');
  return ` <span class="badge danger" title="${esc(why)}">${t('node.certFail')}</span>`;
}
// 锁后面的天数(正用着的证书里最少的):剩 14 天以内变黄、7 天以内变红
const certDaysCell = d => ` <span class="small ${d <= 7 ? 'danger-text' : d <= 14 ? 'warn-text' : 'muted'}" title="${t('cert.daysLeft')}">🔒 ${d}d</span>`;

function coreCell(n) {
  if (n.isLocal) {
    const rl = state.status.reload || {};
    return badge(state.status.coreRunning ? t('dash.running') : t('dash.stopped'), state.status.coreRunning ? 'ok' : 'danger')
      + reloadBadge(rl.at && !rl.ok ? `${trErr(rl.op)}: ${trErr(rl.error)}` : '') + certFailBadge(n);
  }
  const s = n.status || {};
  if (!s.ok) return '—';
  return badge(s.coreRunning ? t('dash.running') : t('dash.stopped'), s.coreRunning ? 'ok' : 'danger')
    + (s.uptime ? ` <span class="muted small">${fmtDuration(s.uptime)}</span>` : '')
    + (s.certDays !== undefined ? certDaysCell(s.certDays) : '')
    + reloadBadge(s.reloadError) + certFailBadge(n);
}

function actionsCell(n) {
  if (isNodeView()) return '';
  // 更多菜单里的项:一项一个按钮;容灾等以后加的操作也往这里放
  const more = [
    n.isLocal || !n.enabled ? '' : `<button data-act="node.push" data-id="${n.id}">${t('node.push')}</button>`,
    n.isLocal || !n.certFp ? '' : `<button data-act="node.resetCert" data-id="${n.id}" title="${esc(n.certFp)}">${t('node.resetCert')}</button>`,
    haMenuItems(n),
    n.isLocal ? '' : `<hr><button class="danger" data-act="node.del" data-id="${n.id}">${t('common.delete')}</button>`,
  ].filter(Boolean);
  return `<button class="btn sm" data-act="node.test" data-id="${n.id}">${t('common.test')}</button>
        <button class="btn sm" data-act="node.edit" data-id="${n.id}">${t('common.edit')}</button>
        ${more.length ? `<details class="menu"><summary class="btn sm">${t('common.more')} ▾</summary><div class="menu-list">${more.join('').replace(/^<hr>/, '')}</div></details>` : ''}`;
}

// 「只看异常」的口径(和概况「需要处理」同一份,见 issues.js):离线、数据面停止、重载失败、下线通知未送达、记账失败、
// 未同步超过 30 秒、版本不一致;再加上大陆连通不通 / 部分不通、证书有问题、IPv6 入站不通(有检测结果时)
function nodeBad(n) {
  if (!n.isLocal && nodeIssues(n, { unsyncedFor: trackUnsynced(unsyncedSince, data.nodes)(n.name), now: data.now || Date.now() / 1000 }).length) return true;
  if (n.isLocal) {
    const rl = state.status.reload || {};
    if ((rl.at && !rl.ok) || !state.status.coreRunning) return true;
  }
  if (n.status && n.status.ledgerRegressAt && Date.now() / 1000 - n.status.ledgerRegressAt < 86400) return true; // 和这一行的「账本回退」徽章一致
  const r = ((reach && reach.results) || []).find(x => x.nodeId === n.id);
  if (r && isBad(r.verdict)) return true;
  const v6 = v6Of(n.id);
  if (v6 && v6.state === 'down') return true;
  if ((state.status.certAlerts || []).some(a => (a.nodeId || 0) === (n.isLocal ? 0 : n.id))) return true;
  return certFailBadge(n) !== '';
}
function visibleNodes() {
  return data.nodes.filter(n => matches(query, n.name, n.domain, n.addr, n.publicIp, n.publicIp6) && (!onlyBad || nodeBad(n)));
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
  const rows = visibleNodes();
  const cnt = document.getElementById('node-count');
  if (cnt) cnt.textContent = `${rows.length} / ${data.nodes.length}`;
  if (!rows.length) { setHTML(body, `<tr><td colspan="8">${empty(onlyBad && data.nodes.length ? t('node.noBad') : undefined)}</td></tr>`); return; }
  // 每 5 秒刷新一次:内容没变就不动 DOM(整表重画会让鼠标下的按钮、提示闪一下)
  setHTML(body, rows.map(n => {
    const s = n.status || {};
    const domain = n.domain || (n.isLocal ? state.settings.webDomain || '' : '');
    return `<tr>
      <td class="primary-cell">${esc(n.name)}${n.ratio && n.ratio !== 1 ? ' ' + badge('x' + n.ratio, 'warn') : ''}${n.apiUrl && !isNodeView() ? `${isPlain(n.apiUrl) ? ` <span title="${esc(t('node.plainHelp'))}">${badge(t('node.plain'), 'warn')}</span>` : ''}<div class="sub-cell mono ellip" title="${esc(n.apiUrl)}">${esc(n.apiUrl)}</div>` : ''}</td>
      <td class="mono"><span class="ellip" title="${esc(domain)}">${esc(domain || '—')}</span>${addrCell(n)}</td>
      <td>${statusCell(n)}</td>
      <td class="reach-td c-reach">${reachCell(n, isNodeView())}</td>
      <td class="c-sync c-p3">${syncCell(n)}</td>
      <td class="c-core">${coreCell(n)}</td>
      <td class="num c-online c-p3">${n.isLocal ? (state.status.onlineUsers ?? '—') : (s.ok ? s.onlineUsers : '—')}</td>
      <td class="actions c-act">${actionsCell(n)}</td></tr>`;
  }).join(''));
}

function editNode(id) {
  const n = id ? data.nodes.find(x => x.id === id) : { enabled: true, insecure: true };
  openModal(id ? t('node.edit') : t('node.add'), `
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(n.name || '')}" placeholder="台湾">`, t('node.nameHelp'))}
      ${field(t('node.domain'), `<input id="f-domain" value="${esc(n.domain || '')}" placeholder="tw.example.com">`, t('node.domainHelp') + (subByDomain() ? ' ' + t('node.domainDnsHelp') : ''))}
      ${field(t('node.addr'), `<input id="f-addr" value="${esc(n.addr || '')}" placeholder="${esc(autoAddr(n).ip || t('node.addrAuto'))}">`, t('node.addrHelp') + (subByDomain() ? ' ' + t('node.addrDnsHelp') : ''))}
      ${field(t('node.family'), `<select id="f-family"><option value="">${t('node.familyV4')}</option><option value="v6" ${n.addrFamily === 'v6' ? 'selected' : ''}>IPv6</option></select>`, t('node.familyHelp') + (id ? ' ' + detectedText(n) : '') + (subByDomain() ? ' ' + t('node.familyDnsHelp') : ''))}
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

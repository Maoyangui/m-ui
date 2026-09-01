'use strict';

const API = './api/';
let LINES = [], UPSTREAMS = [], USERS = [], SETTINGS = {};

// ---- 基础工具 ----
async function api(path, opts = {}) {
  const res = await fetch(API + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (res.status === 401) { showLogin(); throw new Error('未登录'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ('请求失败 ' + res.status));
  return data;
}

function toast(msg) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(el._t);
  el._t = setTimeout(() => { el.hidden = true; }, 2600);
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function fmtBytes(n) {
  n = Number(n) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i === 0 ? 0 : 2) + u[i];
}

function fmtDate(ts) {
  if (!ts) return '—';
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false });
}

function fmtDuration(sec) {
  sec = Number(sec) || 0;
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return (d ? d + '天' : '') + (h ? h + '小时' : '') + m + '分';
}

// ---- 登录 ----
function showLogin() {
  document.getElementById('login').hidden = false;
  document.getElementById('app').hidden = true;
}

async function doLogin(ev) {
  ev.preventDefault();
  const err = document.getElementById('lg-err');
  err.textContent = '';
  try {
    await api('login', {
      method: 'POST',
      body: JSON.stringify({
        username: document.getElementById('lg-user').value,
        password: document.getElementById('lg-pass').value,
      }),
    });
    document.getElementById('lg-pass').value = '';
    enterApp();
  } catch (e) { err.textContent = e.message; }
}

async function doLogout() {
  await api('logout', { method: 'POST' }).catch(() => {});
  showLogin();
}

function enterApp() {
  document.getElementById('login').hidden = true;
  document.getElementById('app').hidden = false;
  loadAll();
}

// ---- 导航 ----
document.querySelectorAll('nav a').forEach(a => {
  a.onclick = () => {
    document.querySelectorAll('nav a').forEach(x => x.classList.remove('active'));
    a.classList.add('active');
    document.querySelectorAll('main section').forEach(s => s.hidden = true);
    document.getElementById('tab-' + a.dataset.tab).hidden = false;
    if (a.dataset.tab === 'logs') loadLogs();
  };
});

async function loadAll() {
  await Promise.all([loadStatus(), loadLines(), loadUpstreams(), loadUsers(), loadSettings()]);
}

// ---- 概览 ----
async function loadStatus() {
  const s = await api('status');
  document.getElementById('role-badge').textContent = s.role === 'node' ? '副 · 台湾' : '主 · 香港';
  document.getElementById('role-badge').className = 'badge' + (s.role === 'node' ? ' node' : '');

  const cards = [
    ['线路', s.lines],
    ['上游', s.upstreams],
    ['用户', `${s.enabledUsers}/${s.users}`],
    ['上行流量', fmtBytes(s.trafficUp)],
    ['下行流量', fmtBytes(s.trafficDown)],
    ['CPU', s.cpu != null ? s.cpu.toFixed(1) + '%' : '—'],
    ['内存', s.memTotal ? fmtBytes(s.memUsed) + ' / ' + fmtBytes(s.memTotal) : '—'],
  ];
  document.getElementById('stat-cards').innerHTML = cards
    .map(([k, v]) => `<div class="stat"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join('');

  document.getElementById('core-info').innerHTML =
    `<p>状态:<span class="tag ${s.coreRunning ? 'on' : 'off'}">${s.coreRunning ? '运行中' : '已停止'}</span>
     &nbsp; 已运行:${fmtDuration(s.uptime)} &nbsp; 域名:${esc(s.domain || '未设置')}</p>`;
}

async function reloadCore() {
  try { await api('reload', { method: 'POST' }); toast('数据面已重载'); loadStatus(); }
  catch (e) { toast('重载失败:' + e.message); }
}

// ---- 线路 ----
async function loadLines() {
  LINES = await api('lines');
  const body = document.getElementById('lines-body');
  body.innerHTML = LINES.map(l => `
    <tr draggable="true" data-id="${l.id}">
      <td class="handle">⠿</td>
      <td>${esc(l.name)}</td>
      <td>${esc(l.protocol)}</td>
      <td class="mono">${l.port}</td>
      <td>${esc(l.upstreamName)}</td>
      <td>${l.userCount}</td>
      <td><span class="tag ${l.enabled ? 'on' : 'off'}">${l.enabled ? '启用' : '停用'}</span></td>
      <td>
        <button onclick="editLine(${l.id})">编辑</button>
        <button class="danger" onclick="delLine(${l.id})">删除</button>
      </td>
    </tr>`).join('');
  enableDragSort(body);
}

function enableDragSort(tbody) {
  let dragged = null;
  tbody.querySelectorAll('tr').forEach(tr => {
    tr.ondragstart = () => { dragged = tr; tr.classList.add('dragging'); };
    tr.ondragend = async () => {
      tr.classList.remove('dragging');
      const ids = [...tbody.querySelectorAll('tr')].map(r => Number(r.dataset.id));
      try { await api('lines/sort', { method: 'POST', body: JSON.stringify(ids) }); toast('顺序已保存'); }
      catch (e) { toast('保存顺序失败:' + e.message); }
    };
    tr.ondragover = e => {
      e.preventDefault();
      if (!dragged || dragged === tr) return;
      const rect = tr.getBoundingClientRect();
      const after = (e.clientY - rect.top) / rect.height > 0.5;
      tr.parentNode.insertBefore(dragged, after ? tr.nextSibling : tr);
    };
  });
}

function editLine(id) {
  const l = id ? LINES.find(x => x.id === id) : { protocol: 'hysteria2', enabled: true, options: {} };
  const optText = typeof l.options === 'string' ? l.options : JSON.stringify(l.options ?? {}, null, 2);
  const addrText = l.addrs ? (typeof l.addrs === 'string' ? l.addrs : JSON.stringify(l.addrs, null, 2)) : '';
  openModal(id ? '编辑线路' : '新增线路', `
    <div class="form-grid">
      <label>名称(订阅中显示)<input id="f-name" value="${esc(l.name || '')}"></label>
      <label>协议<select id="f-protocol">
        ${['hysteria2', 'anytls', 'shadowsocks'].map(p =>
          `<option value="${p}" ${l.protocol === p ? 'selected' : ''}>${p}</option>`).join('')}
      </select></label>
      <label>端口<input id="f-port" type="number" value="${l.port || ''}"></label>
      <label>上游<select id="f-upstream">
        <option value="0">direct(本机直出)</option>
        ${UPSTREAMS.map(u => `<option value="${u.id}" ${l.upstreamId === u.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')}
      </select></label>
      <label class="check"><input type="checkbox" id="f-enabled" ${l.enabled !== false ? 'checked' : ''}> 启用</label>
      <label class="full">协议参数(JSON)<textarea id="f-options">${esc(optText)}</textarea></label>
      <label class="full">对外地址(JSON,留空=用面板域名)<textarea id="f-addrs" placeholder='[{"server":"1.2.3.4","server_port":443,"remark":"-备用"}]'>${esc(addrText)}</textarea></label>
    </div>`, async () => {
    const body = {
      name: document.getElementById('f-name').value,
      protocol: document.getElementById('f-protocol').value,
      port: Number(document.getElementById('f-port').value),
      upstreamId: Number(document.getElementById('f-upstream').value),
      enabled: document.getElementById('f-enabled').checked,
      options: JSON.parse(document.getElementById('f-options').value || '{}'),
    };
    const addrs = document.getElementById('f-addrs').value.trim();
    if (addrs) body.addrs = JSON.parse(addrs);
    await api(id ? 'lines/' + id : 'lines', { method: id ? 'PUT' : 'POST', body: JSON.stringify(body) });
    await loadLines(); await loadStatus();
    toast(id ? '线路已更新' : '线路已创建');
  });
}

async function delLine(id) {
  const l = LINES.find(x => x.id === id);
  if (!confirm(`确定删除线路「${l.name}」?订阅中的该节点会消失。`)) return;
  try { await api('lines/' + id, { method: 'DELETE' }); await loadLines(); await loadStatus(); toast('已删除'); }
  catch (e) { toast('删除失败:' + e.message); }
}

// ---- 上游 ----
const UP_TYPE_LABELS = { tuic: 'TUIC', hysteria2: 'Hysteria2', shadowsocks: 'Shadowsocks', socks: 'SOCKS5(含 WARP 本地代理)' };
const SS_METHODS = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305',
  '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305', 'none'];
// 表单直接管理的键;其余键保留在"高级参数"里原样回写,编辑不丢字段
const UP_FORM_KEYS = {
  tuic: ['server', 'server_port', 'uuid', 'password', 'congestion_control', 'udp_relay_mode', 'zero_rtt_handshake', 'udp_over_stream', 'tls'],
  hysteria2: ['server', 'server_port', 'password', 'tls', 'up_mbps', 'down_mbps', 'obfs'],
  shadowsocks: ['server', 'server_port', 'method', 'password'],
  socks: ['server', 'server_port', 'version', 'username', 'password'],
};
let UP_TEST = {}; // id → 最近一次测试结果

function parseOpts(o) { try { return typeof o === 'string' ? JSON.parse(o) : (o || {}); } catch { return {}; } }
const fv = id => ((document.getElementById(id) || {}).value || '');
const fchk = id => !!(document.getElementById(id) || {}).checked;

function upResultHTML(id) {
  const r = UP_TEST[id];
  if (!r) return '<span class="tag">未测</span>';
  if (r.testing) return '<span class="tag">测试中…</span>';
  if (r.ok) return `<span class="tag on">${r.delayMs} ms</span>${r.method === 'tcp' ? ' <span class="tag" title="数据面未运行,仅端口探测">TCP</span>' : ''}`;
  return `<span class="tag off">故障</span><div class="sub-url">${esc(r.error)}</div>`;
}

async function loadUpstreams() {
  UPSTREAMS = await api('upstreams');
  renderUpstreams();
}

function renderUpstreams() {
  document.getElementById('upstreams-body').innerHTML = UPSTREAMS.map(u => {
    const o = parseOpts(u.options);
    const srv = o.server ? o.server + (o.server_port ? ':' + o.server_port : '') : '';
    return `<tr>
      <td>${esc(u.name)}</td><td>${esc(UP_TYPE_LABELS[u.type] || u.type)}</td><td class="mono">${esc(srv)}</td>
      <td id="up-res-${u.id}">${upResultHTML(u.id)}</td>
      <td>
        <button onclick="testUpstream(${u.id})">测试</button>
        <button onclick="editUpstream(${u.id})">编辑</button>
        <button class="danger" onclick="delUpstream(${u.id})">删除</button>
      </td></tr>`;
  }).join('');
}

function setUpResult(id, r) {
  UP_TEST[id] = r;
  const cell = document.getElementById('up-res-' + id);
  if (cell) cell.innerHTML = upResultHTML(id);
}

async function testUpstream(id) {
  setUpResult(id, { testing: true });
  try { setUpResult(id, await api('upstreams/' + id + '/test', { method: 'POST' })); }
  catch (e) { setUpResult(id, { ok: false, error: e.message }); }
}

async function testAllUpstreams() {
  const btn = document.getElementById('btn-test-all');
  btn.disabled = true; btn.textContent = '测试中…';
  UPSTREAMS.forEach(u => setUpResult(u.id, { testing: true }));
  try {
    const results = await api('upstreams/test', { method: 'POST' });
    results.forEach(r => setUpResult(r.id, r));
    const bad = results.filter(r => !r.ok).length;
    toast(bad ? `测试完成:${bad} 个上游故障` : '测试完成:全部正常');
  } catch (e) { toast('测试失败:' + e.message); }
  btn.disabled = false; btn.textContent = '测试全部';
}

// 各类型的可视化字段(参照现有上游的真实配置项)
function upstreamFieldsHTML(type, o) {
  const tls = o.tls || {};
  const alpn = Array.isArray(tls.alpn) ? tls.alpn.join(',') : (tls.alpn || 'h3');
  const common = `
    <label>服务器地址<input id="f-server" value="${esc(o.server || '')}" placeholder="域名或 IP"></label>
    <label>端口<input id="f-port" type="number" value="${o.server_port || ''}"></label>`;
  const tlsFields = `
    <label>SNI(留空=服务器地址)<input id="f-sni" value="${esc(tls.server_name || '')}"></label>
    <label>ALPN<input id="f-alpn" value="${esc(alpn)}"></label>
    <label class="check"><input type="checkbox" id="f-insecure" ${tls.insecure ? 'checked' : ''}> 跳过证书验证</label>`;
  switch (type) {
    case 'tuic': return common + `
      <label>UUID<input id="f-uuid" value="${esc(o.uuid || '')}"></label>
      <label>密码<input id="f-password" value="${esc(o.password || '')}"></label>
      <label>拥塞控制<select id="f-cc">${['cubic', 'bbr', 'new_reno'].map(x => `<option ${(o.congestion_control || 'cubic') === x ? 'selected' : ''}>${x}</option>`).join('')}</select></label>
      <label>UDP 中继模式<select id="f-relay"><option value="">默认</option>${['native', 'quic'].map(x => `<option value="${x}" ${o.udp_relay_mode === x ? 'selected' : ''}>${x}</option>`).join('')}</select></label>
      ${tlsFields}
      <label class="check"><input type="checkbox" id="f-zrtt" ${o.zero_rtt_handshake ? 'checked' : ''}> 0-RTT 握手</label>
      <label class="check"><input type="checkbox" id="f-uos" ${o.udp_over_stream ? 'checked' : ''}> UDP over stream</label>`;
    case 'hysteria2': return common + `
      <label>密码<input id="f-password" value="${esc(o.password || '')}"></label>
      ${tlsFields}
      <label>上行 Mbps(0=不设)<input id="f-up" type="number" value="${o.up_mbps || 0}"></label>
      <label>下行 Mbps(0=不设)<input id="f-down" type="number" value="${o.down_mbps || 0}"></label>
      <label>混淆密码(salamander,留空=不用)<input id="f-obfs" value="${esc((o.obfs && o.obfs.password) || '')}"></label>`;
    case 'shadowsocks': return common + `
      <label>加密方式<select id="f-method">${SS_METHODS.map(x => `<option ${(o.method || 'aes-256-gcm') === x ? 'selected' : ''}>${x}</option>`).join('')}</select></label>
      <label>密码<input id="f-password" value="${esc(o.password || '')}"></label>`;
    case 'socks': return common + `
      <label>版本<select id="f-version">${['5', '4a', '4'].map(x => `<option ${(o.version || '5') === x ? 'selected' : ''}>${x}</option>`).join('')}</select></label>
      <label>用户名(可空)<input id="f-username" value="${esc(o.username || '')}"></label>
      <label>密码(可空)<input id="f-password" value="${esc(o.password || '')}"></label>`;
  }
  return common;
}

function readUpstreamFields(type) {
  const o = { server: fv('f-server').trim(), server_port: Number(fv('f-port')) };
  const tlsOf = () => {
    const t = { enabled: true, server_name: fv('f-sni').trim() || o.server };
    const alpn = fv('f-alpn').split(',').map(s => s.trim()).filter(Boolean);
    if (alpn.length) t.alpn = alpn;
    if (fchk('f-insecure')) t.insecure = true;
    return t;
  };
  switch (type) {
    case 'tuic':
      Object.assign(o, { uuid: fv('f-uuid').trim(), password: fv('f-password'), congestion_control: fv('f-cc'), tls: tlsOf() });
      if (fv('f-relay')) o.udp_relay_mode = fv('f-relay');
      if (fchk('f-zrtt')) o.zero_rtt_handshake = true;
      if (fchk('f-uos')) o.udp_over_stream = true;
      break;
    case 'hysteria2':
      Object.assign(o, { password: fv('f-password'), tls: tlsOf() });
      if (Number(fv('f-up')) > 0) o.up_mbps = Number(fv('f-up'));
      if (Number(fv('f-down')) > 0) o.down_mbps = Number(fv('f-down'));
      if (fv('f-obfs')) o.obfs = { type: 'salamander', password: fv('f-obfs') };
      break;
    case 'shadowsocks':
      Object.assign(o, { method: fv('f-method'), password: fv('f-password') });
      break;
    case 'socks':
      o.version = fv('f-version');
      if (fv('f-username')) { o.username = fv('f-username'); o.password = fv('f-password'); }
      break;
  }
  return o;
}

function renderUpstreamFields(type, o) {
  document.getElementById('f-fields').innerHTML = upstreamFieldsHTML(type, o || {});
  const known = new Set(UP_FORM_KEYS[type] || []);
  const extra = {};
  Object.entries(o || {}).forEach(([k, val]) => { if (!known.has(k)) extra[k] = val; });
  document.getElementById('f-extra').value = Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
}

function onUpstreamTypeChange() {
  // 换类型时保留服务器/端口,其余按新类型重建
  renderUpstreamFields(fv('f-type'), { server: fv('f-server'), server_port: Number(fv('f-port')) || undefined });
}

async function importUpstreamLink() {
  const link = fv('f-link').trim();
  if (!link) return;
  try {
    const p = await api('upstreams/parse', { method: 'POST', body: JSON.stringify({ link }) });
    document.getElementById('f-type').value = p.type;
    if (!fv('f-name').trim()) document.getElementById('f-name').value = p.name || '';
    renderUpstreamFields(p.type, p.options);
    document.getElementById('modal-err').textContent = '';
    toast('已解析为 ' + (UP_TYPE_LABELS[p.type] || p.type) + ',请核对后保存');
  } catch (e) { document.getElementById('modal-err').textContent = '解析失败:' + e.message; }
}

function editUpstream(id) {
  const u = id ? UPSTREAMS.find(x => x.id === id) : { type: 'tuic', options: {} };
  const o = parseOpts(u.options);
  openModal(id ? '编辑上游' : '新增上游', `
    <div class="form-grid">
      ${id ? '' : `<label class="full">从分享链接导入(tuic:// hysteria2:// ss:// socks5://)
        <div class="row"><input id="f-link" placeholder="粘贴机场/服务商给的链接"><button type="button" onclick="importUpstreamLink()">解析填表</button></div></label>`}
      <label>名称<input id="f-name" value="${esc(u.name || '')}"></label>
      <label>类型<select id="f-type" onchange="onUpstreamTypeChange()">
        ${Object.entries(UP_TYPE_LABELS).map(([t, l]) => `<option value="${t}" ${u.type === t ? 'selected' : ''}>${l}</option>`).join('')}
      </select></label>
      <div id="f-fields" class="form-grid full"></div>
      <details class="full"><summary>高级参数(JSON,合并进配置,一般留空)</summary><textarea id="f-extra"></textarea></details>
    </div>
    <p class="hint">WARP 本地代理:类型 SOCKS5,服务器 127.0.0.1,端口 40000,无用户名密码。</p>`,
  async () => {
    const type = fv('f-type');
    const fields = readUpstreamFields(type);
    if (!fields.server || !fields.server_port) throw new Error('服务器地址和端口必填');
    if (type === 'tuic' && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(fields.uuid)) {
      throw new Error('UUID 格式不正确(应为 8-4-4-4-12 位十六进制)');
    }
    const extra = JSON.parse(fv('f-extra').trim() || '{}');
    await api(id ? 'upstreams/' + id : 'upstreams', {
      method: id ? 'PUT' : 'POST',
      body: JSON.stringify({ name: fv('f-name').trim(), type, options: Object.assign({}, extra, fields) }),
    });
    await loadUpstreams(); await loadLines(); await loadStatus();
    toast(id ? '上游已更新' : '上游已创建');
  });
  renderUpstreamFields(u.type, o);
}

async function delUpstream(id) {
  if (!confirm('确定删除该上游?')) return;
  try { await api('upstreams/' + id, { method: 'DELETE' }); await loadUpstreams(); toast('已删除'); }
  catch (e) { toast('删除失败:' + e.message); }
}

// ---- 用户 ----
async function loadUsers() {
  USERS = await api('users');
  document.getElementById('users-body').innerHTML = USERS.map(u => {
    const used = (u.up || 0) + (u.down || 0);
    const quota = u.volume ? fmtBytes(u.volume) : '不限';
    return `<tr>
      <td>${esc(u.name)}<div class="sub-url">${esc(u.subUrl)}</div></td>
      <td><span class="tag ${u.enabled ? 'on' : 'off'}">${u.enabled ? '启用' : '停用'}</span></td>
      <td>${fmtBytes(used)} / ${quota}</td>
      <td>${u.expiry ? fmtDate(u.expiry) : '不限'}</td>
      <td>${u.deviceLimit || '不限'}</td>
      <td>${(u.speedUp || u.speedDown) ? `${u.speedUp || 0}/${u.speedDown || 0} Mbps` : '不限'}</td>
      <td>${(u.onlineIps || []).length}</td>
      <td>
        <button onclick="editUser(${u.id})">编辑</button>
        <button class="danger" onclick="delUser(${u.id})">删除</button>
      </td></tr>`;
  }).join('');
}

function editUser(id) {
  const u = id ? USERS.find(x => x.id === id) : { enabled: true, lineIds: [] };
  const lineIds = new Set(u.lineIds || []);
  const expiryVal = u.expiry ? new Date(u.expiry * 1000).toISOString().slice(0, 10) : '';
  openModal(id ? '编辑用户' : '新增用户', `
    <div class="form-grid">
      <label>用户名(即订阅地址)<input id="f-name" value="${esc(u.name || '')}"></label>
      <label>备注<input id="f-remark" value="${esc(u.remark || '')}"></label>
      <label>流量配额 GB(0=不限)<input id="f-volume" type="number" value="${u.volume ? (u.volume / 1073741824).toFixed(0) : 0}"></label>
      <label>到期日(留空=不限)<input id="f-expiry" type="date" value="${expiryVal}"></label>
      <label>同时在线设备(0=不限)<input id="f-device" type="number" value="${u.deviceLimit || 0}"></label>
      <label>上行限速 Mbps(0=不限)<input id="f-up" type="number" value="${u.speedUp || 0}"></label>
      <label>下行限速 Mbps(0=不限)<input id="f-down" type="number" value="${u.speedDown || 0}"></label>
      <label class="check"><input type="checkbox" id="f-enabled" ${u.enabled !== false ? 'checked' : ''}> 启用</label>
      <label class="check"><input type="checkbox" id="f-autoreset" ${u.autoReset ? 'checked' : ''}> 周期重置流量</label>
      <label>重置周期(天)<input id="f-resetdays" type="number" value="${u.resetDays || 30}"></label>
      <label class="full">可用线路
        <div style="max-height:12rem;overflow:auto;border:1px solid var(--border);border-radius:6px;padding:.5rem">
          <label class="check" style="margin-bottom:.4rem">
            <input type="checkbox" onchange="document.querySelectorAll('.ln-cb').forEach(c=>c.checked=this.checked)"> 全选
          </label>
          ${LINES.map(l => `<label class="check"><input type="checkbox" class="ln-cb" value="${l.id}" ${lineIds.has(l.id) ? 'checked' : ''}> ${esc(l.name)}</label>`).join('')}
        </div>
      </label>
    </div>`, async () => {
    const expiry = document.getElementById('f-expiry').value;
    const body = {
      name: document.getElementById('f-name').value,
      remark: document.getElementById('f-remark').value,
      enabled: document.getElementById('f-enabled').checked,
      volume: Math.round(Number(document.getElementById('f-volume').value) * 1073741824),
      expiry: expiry ? Math.floor(new Date(expiry + 'T23:59:59').getTime() / 1000) : 0,
      deviceLimit: Number(document.getElementById('f-device').value),
      speedUp: Number(document.getElementById('f-up').value),
      speedDown: Number(document.getElementById('f-down').value),
      autoReset: document.getElementById('f-autoreset').checked,
      resetDays: Number(document.getElementById('f-resetdays').value),
      lineIds: [...document.querySelectorAll('.ln-cb:checked')].map(c => Number(c.value)),
    };
    await api(id ? 'users/' + id : 'users', { method: id ? 'PUT' : 'POST', body: JSON.stringify(body) });
    await loadUsers(); await loadLines(); await loadStatus();
    toast(id ? '用户已更新' : '用户已创建');
  });
}

async function delUser(id) {
  const u = USERS.find(x => x.id === id);
  if (!confirm(`确定删除用户「${u.name}」?其订阅将立即失效。`)) return;
  try { await api('users/' + id, { method: 'DELETE' }); await loadUsers(); await loadStatus(); toast('已删除'); }
  catch (e) { toast('删除失败:' + e.message); }
}

// ---- 订阅日志 ----
async function loadLogs() {
  const user = document.getElementById('log-filter').value.trim();
  const logs = await api('sublogs?limit=300' + (user ? '&user=' + encodeURIComponent(user) : ''));
  document.getElementById('logs-body').innerHTML = logs.map(l => `<tr>
    <td>${fmtDate(l.ts)}</td><td>${esc(l.user)}</td><td class="mono">${esc(l.ip)}</td>
    <td>${esc(l.format)}</td><td>${l.status}</td>
    <td class="sub-url">${esc(l.ua)}</td></tr>`).join('') || '<tr><td colspan="6">暂无记录</td></tr>';
}

// ---- 设置 ----
const SETTING_KEYS = ['webDomain', 'webListen', 'webPort', 'webPath', 'webCertFile', 'webKeyFile',
  'subListen', 'subPort', 'subPath', 'subCertFile', 'subKeyFile', 'subProfileTitle', 'subUpdates'];
const SETTING_BOOLS = ['subEncode', 'subShowNotice', 'nodeMode'];

async function loadSettings() {
  SETTINGS = await api('settings');
  SETTING_KEYS.forEach(k => {
    const el = document.getElementById('set-' + k);
    if (el) el.value = SETTINGS[k] ?? '';
  });
  SETTING_BOOLS.forEach(k => {
    const el = document.getElementById('set-' + k);
    if (el) el.checked = String(SETTINGS[k]).toLowerCase() === 'true';
  });
  updateRoleHint();
  document.getElementById('set-nodeMode').onchange = updateRoleHint;
}

function updateRoleHint() {
  const isNode = document.getElementById('set-nodeMode').checked;
  document.getElementById('role-hint').textContent = isNode
    ? '副服务器:由主面板统一下发配置与用户,本机不做配额判定。'
    : '主服务器:在此管理线路、上游与用户,并向副服务器同步。';
}

async function saveSettings() {
  const body = {};
  SETTING_KEYS.forEach(k => {
    const el = document.getElementById('set-' + k);
    if (el) body[k] = el.value;
  });
  SETTING_BOOLS.forEach(k => {
    const el = document.getElementById('set-' + k);
    if (el) body[k] = String(el.checked);
  });
  if (body.nodeMode !== String(SETTINGS.nodeMode).toLowerCase()) {
    const toNode = body.nodeMode === 'true';
    const msg = toNode
      ? '切换为副服务器后,本机将不再判定用户配额,配置由主面板下发。确认?'
      : '切换为主服务器前请确认原主面板已停用,避免出现两个主。确认?';
    if (!confirm(msg)) return;
  }
  try {
    const r = await api('settings', { method: 'POST', body: JSON.stringify(body) });
    toast('设置已保存 · ' + (r.note || ''));
    await loadSettings(); await loadStatus();
  } catch (e) { toast('保存失败:' + e.message); }
}

async function changePassword() {
  const body = {
    username: document.getElementById('pw-user').value,
    oldPassword: document.getElementById('pw-old').value,
    newPassword: document.getElementById('pw-new').value,
  };
  try {
    await api('password', { method: 'POST', body: JSON.stringify(body) });
    document.getElementById('pw-old').value = '';
    document.getElementById('pw-new').value = '';
    toast('账号已更新,请重新登录');
    setTimeout(doLogout, 1200);
  } catch (e) { toast('更新失败:' + e.message); }
}

// ---- 弹窗 ----
function openModal(title, html, onSave) {
  document.getElementById('modal-title').textContent = title;
  document.getElementById('modal-body').innerHTML = html;
  document.getElementById('modal-err').textContent = '';
  document.getElementById('modal').hidden = false;
  document.getElementById('modal-save').onclick = async () => {
    try { await onSave(); closeModal(); }
    catch (e) { document.getElementById('modal-err').textContent = e.message; }
  };
}

function closeModal() { document.getElementById('modal').hidden = true; }

// ---- 启动 ----
api('status').then(enterApp).catch(showLogin);
setInterval(() => { if (!document.getElementById('app').hidden) loadStatus().catch(() => {}); }, 15000);

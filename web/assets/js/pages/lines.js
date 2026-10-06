import { state, load } from '../app.js';
import { get, post, put, del, SLOW } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, openModal, registerActions, badge, dot, field, check, empty, fv, fchk, matches, debounce, setHTML } from '../ui.js';

const selected = new Set(); // 批量设置勾选的线路 id
export const title = () => t('line.title');
export const subtitle = () => t('line.subtitle');
let query = '';

// 与后端 render.Protocols 对应:TLS 是否必需、默认模式、是否支持传输层
const PROTOCOLS = {
  vless: { tlsRequired: false, tlsDefault: 'reality', transport: true },
  vmess: { tlsRequired: false, tlsDefault: 'none', transport: true },
  trojan: { tlsRequired: false, tlsDefault: 'cert', transport: true },
  hysteria2: { tlsRequired: true, tlsDefault: 'cert', transport: false },
  tuic: { tlsRequired: true, tlsDefault: 'cert', transport: false },
  anytls: { tlsRequired: true, tlsDefault: 'cert', transport: false },
  shadowsocks: { tlsRequired: false, tlsDefault: 'none', transport: false, noTls: true },
  socks: { tlsRequired: false, tlsDefault: 'none', transport: false, noTls: true },
  http: { tlsRequired: false, tlsDefault: 'cert', transport: false },
  mixed: { tlsRequired: false, tlsDefault: 'none', transport: false, noTls: true },
};
const SS_METHODS = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305', '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305', 'none'];
const FPS = ['chrome', 'firefox', 'safari', 'ios', 'android', 'edge', 'random'];
// 表单接管的 Options 键,其余进"高级参数"
const OPT_KEYS = {
  hysteria2: ['up_mbps', 'down_mbps', 'obfs', 'port_hopping'], tuic: ['congestion_control'], shadowsocks: ['method', 'password'],
  anytls: ['padding_scheme'], vless: ['vision'],
};
const parseJ = o => { try { return typeof o === 'string' ? JSON.parse(o) : (o || {}); } catch { return {}; } };

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="line-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <span class="muted small" id="line-count"></span>
      <span class="grow"></span>
      <details class="menu"><summary class="btn">${t('line.quick')} ▾</summary><div class="menu-list">
        ${PRESETS.map(p => `<button data-act="line.preset" data-id="${p.id}"><b>${esc(p.label)}</b><span class="muted small" style="display:block">${esc(p.hint)}</span></button>`).join('')}
      </div></details>
      <button class="btn primary" data-act="line.add">${t('line.add')}</button>
    </div>
    <div class="toolbar batch-bar" id="line-batch-bar" hidden>
      <span class="badge primary" id="line-batch-count"></span>
      <button class="btn sm" data-act="line.batch" data-id="enable">${t('line.batch.enable')}</button>
      <button class="btn sm" data-act="line.batch" data-id="disable">${t('line.batch.disable')}</button>
      <button class="btn sm" data-act="line.batch" data-id="upstream">${t('line.batch.upstream')}</button>
      ${(state.nodes || []).length > 1 ? `<button class="btn sm" data-act="line.batch" data-id="nodes">${t('line.batch.nodes')}</button>` : ''}
      <button class="btn sm ghost" data-act="line.clearSel">${t('line.batch.clear')}</button>
    </div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th></th><th style="width:1.5rem"><input type="checkbox" id="line-sel-all"></th><th>${t('common.name')}</th><th>${t('line.protocol')}</th><th>${t('common.port')}</th><th>${t('line.upstream')}</th><th>${t('nav.nodes')}</th><th>${t('line.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="lines-body"></tbody>
    </table></div>`;
  document.getElementById('line-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  document.getElementById('line-sel-all').addEventListener('change', e => {
    visibleRows().forEach(l => e.target.checked ? selected.add(l.id) : selected.delete(l.id));
    renderRows();
  });
  renderRows();
}
function visibleRows() { return state.lines.filter(l => matches(query, l.name, l.protocol, l.port, l.upstreamName)); }
export function tick() { renderRows(); }

function tlsModeOf(l) {
  const tls = parseJ(l.tls);
  return tls.mode || (PROTOCOLS[l.protocol] || {}).tlsDefault || 'none';
}
function protoBadges(l) {
  const mode = tlsModeOf(l);
  const tr = parseJ(l.transport);
  let h = badge(l.protocol, 'primary');
  if (!(PROTOCOLS[l.protocol] || {}).noTls) h += ' ' + badge(mode === 'reality' ? 'Reality' : mode === 'cert' ? 'TLS' : 'plain', mode === 'reality' ? 'ok' : mode === 'cert' ? '' : 'warn');
  if (tr.type && tr.type !== 'tcp') h += ' ' + badge(tr.type);
  return h;
}

function renderRows() {
  const body = document.getElementById('lines-body');
  if (!body) return;
  const online = new Set(state.onlines.lines || []);
  const rows = visibleRows();
  document.getElementById('line-count').textContent = `${rows.length} / ${state.lines.length}`;
  for (const id of [...selected]) if (!state.lines.some(l => l.id === id)) selected.delete(id); // 被删掉的线路不再算选中
  const bar = document.getElementById('line-batch-bar');
  if (bar) { bar.hidden = selected.size === 0; document.getElementById('line-batch-count').textContent = t('line.selected', { n: selected.size }); }
  const selAll = document.getElementById('line-sel-all');
  if (selAll) selAll.checked = rows.length > 0 && rows.every(l => selected.has(l.id));
  // 库里一条线路都没有(不是搜索没结果):说清线路是什么,并给出第一步
  if (!rows.length) {
    setHTML(body, `<tr><td colspan="10">${state.lines.length ? empty() : `<div class="empty-guide"><p>${t('line.emptyFirst')}</p><button class="btn primary" data-act="line.add">${t('line.emptyFirstBtn')}</button></div>`}</td></tr>`);
    return;
  }
  const nodeIdsOf = l => { const v = l.nodeIds; if (!v) return []; try { return Array.isArray(v) ? v : JSON.parse(v); } catch { return []; } };
  const serversCell = l => {
    const ids = nodeIdsOf(l);
    if (!ids.length) return `<span class="muted">${t('line.allServers')}</span>`;
    return ids.map(id => { const n = (state.nodes || []).find(x => x.id === id); return badge(n ? n.name : '#' + id, 'primary'); }).join(' ');
  };
  setHTML(body, rows.map(l => `
    <tr draggable="${query ? 'false' : 'true'}" data-id="${l.id}" class="${selected.has(l.id) ? 'selected' : ''}">
      <td class="handle" title="拖动排序">⠿</td>
      <td><input type="checkbox" class="sel" data-change="line.sel" data-id="${l.id}" ${selected.has(l.id) ? 'checked' : ''}></td>
      <td class="primary-cell">${dot(online.has(l.name))}${esc(l.name)}</td>
      <td>${protoBadges(l)}</td>
      <td class="num">${l.port}</td>
      <td>${esc(l.upstreamName)}${rrOf(l).length ? ' ' + badge(t('line.rr.badge', { n: rrOf(l).length }), 'primary') : ''}</td>
      <td>${serversCell(l)}</td>
      <td class="num">${l.userCount}</td>
      <td><label class="switch" title="${l.enabled ? t('common.enabled') : t('common.disabled')}"><input type="checkbox" data-change="line.toggle" data-id="${l.id}" ${l.enabled ? 'checked' : ''}><span></span></label></td>
      <td class="actions">
        <button class="btn sm" data-act="line.edit" data-id="${l.id}">${t('common.edit')}</button>
        <button class="btn sm" data-act="line.clone" data-id="${l.id}" title="${t('line.clone')}">⧉</button>
        <button class="btn sm danger" data-act="line.del" data-id="${l.id}">${t('common.delete')}</button>
      </td>
    </tr>`).join(''));
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

// ---- 表单 ----
const sel = (id, opts, cur, labels = {}) => `<select id="${id}">${opts.map(x => `<option value="${x}" ${cur === x ? 'selected' : ''}>${esc(labels[x] || x)}</option>`).join('')}</select>`;

function tlsSection(protocol, tls) {
  const spec = PROTOCOLS[protocol] || {};
  if (spec.noTls) return '';
  const mode = tls.mode || spec.tlsDefault;
  const modes = spec.tlsRequired ? ['cert', 'reality'] : ['cert', 'reality', 'none'];
  const r = tls.reality || {};
  return `<h3>${t('line.tls')}</h3><div class="form-grid">
    ${field(t('line.tlsMode'), sel('f-tlsmode', modes, mode, { cert: t('line.tls.cert'), reality: t('line.tls.reality'), none: t('line.tls.none') }), t('line.tlsHelp.' + mode))}
    ${field(t('line.fp'), sel('f-fp', ['', ...FPS], tls.fingerprint || '', { '': '默认' }))}
    <div id="f-reality" class="form-grid full" ${mode === 'reality' ? '' : 'hidden'}>
      ${field(t('line.reality.server'), `<input id="f-hs" value="${esc(r.handshake_server || 'www.microsoft.com')}">`, t('line.reality.serverHelp'))}
      ${field(t('line.reality.port'), `<input id="f-hsport" type="number" value="${r.handshake_port || 443}">`)}
      <div class="full">${field(t('line.reality.private'), `<div class="row"><input id="f-priv" value="${esc(r.private_key || '')}"><button type="button" class="btn" data-act="line.genReality">${t('line.reality.gen')}</button></div>`)}</div>
      <div class="full">${field(t('line.reality.public'), `<input id="f-pub" value="${esc(r.public_key || '')}" readonly>`)}</div>
      <div class="full">${field(t('line.reality.shortIds'), `<div class="row"><input id="f-sids" value="${esc((r.short_ids || []).join(','))}"><button type="button" class="btn" data-act="line.genShortId">${t('line.gen')}</button></div>`)}</div>
    </div>
  </div>`;
}

function transportSection(protocol, tr) {
  if (!(PROTOCOLS[protocol] || {}).transport) return '';
  const type = tr.type || 'tcp';
  const host = tr.headers && tr.headers.Host ? (Array.isArray(tr.headers.Host) ? tr.headers.Host[0] : tr.headers.Host) : (Array.isArray(tr.host) ? tr.host.join(',') : (tr.host || ''));
  return `<h3>${t('line.transport')}</h3><div class="form-grid">
    ${field(t('line.transport.type'), sel('f-trtype', ['tcp', 'ws', 'grpc', 'httpupgrade', 'http'], type, { tcp: t('line.transport.tcp') }), t('line.transportHelp'))}
    <div id="f-tr-fields" class="form-grid full">${transportFields(type, tr, host)}</div>
  </div>`;
}
function transportFields(type, tr, host) {
  switch (type) {
    case 'ws': case 'httpupgrade': case 'http':
      return field(t('line.transport.path'), `<input id="f-trpath" value="${esc(tr.path || '/')}">`) + field(t('line.transport.host'), `<input id="f-trhost" value="${esc(host || '')}" placeholder="cdn.example.com">`);
    case 'grpc':
      return field(t('line.transport.service'), `<input id="f-trsvc" value="${esc(tr.service_name || 'grpc')}">`);
  }
  return '';
}

function protoSection(protocol, o) {
  let h = '';
  switch (protocol) {
    case 'vless': h = check('f-vision', t('line.vision'), o.vision !== false); break;
    case 'hysteria2':
      h = field(t('line.upMbps'), `<input id="f-up" type="number" value="${o.up_mbps || 0}">`) +
        field(t('line.downMbps'), `<input id="f-down" type="number" value="${o.down_mbps || 0}">`) +
        field(t('line.obfs'), `<input id="f-obfs" value="${esc((o.obfs && o.obfs.password) || '')}">`) +
        `<div class="full">${field(t('line.hop'), `<input id="f-hop" value="${esc(o.port_hopping || '')}" placeholder="20000-30000">`, t('line.hopHelp'))}</div>`; break;
    case 'tuic': h = field(t('line.cc'), sel('f-cc', ['cubic', 'bbr', 'new_reno'], o.congestion_control || 'cubic')); break;
    case 'shadowsocks':
      h = field(t('line.method'), sel('f-method', SS_METHODS, o.method || 'aes-256-gcm')) +
        field(t('line.password'), `<div class="row"><input id="f-sspw" value="${esc(o.password || '')}"><button type="button" class="btn" data-act="line.genSsPw">${t('line.gen')}</button></div>`); break;
    case 'anytls':
      h = `<div class="full">${field(t('line.padding'), `<textarea id="f-padding">${esc((o.padding_scheme || []).join('\n'))}</textarea>`)}</div>`; break;
  }
  return h ? `<h3>${t('line.proto')}</h3><div class="form-grid">${h}</div>` : '';
}

function extraOf(protocol, o) {
  const known = new Set(OPT_KEYS[protocol] || []);
  const extra = {};
  Object.entries(o).forEach(([k, v]) => { if (!known.has(k)) extra[k] = v; });
  return Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
}

function renderDynamic(l) {
  const protocol = fv('f-protocol');
  const o = parseJ(l.options), tls = parseJ(l.tls), tr = parseJ(l.transport);
  document.getElementById('f-dyn').innerHTML = tlsSection(protocol, tls) + transportSection(protocol, tr) + protoSection(protocol, o);
  document.getElementById('f-extra').value = extraOf(protocol, o);
  const tm = document.getElementById('f-tlsmode');
  if (tm) tm.addEventListener('change', e => {
    document.getElementById('f-reality').hidden = e.target.value !== 'reality';
    const help = tm.closest('.field').querySelector('.help');
    if (help) help.textContent = t('line.tlsHelp.' + e.target.value);
  });
  const tt = document.getElementById('f-trtype');
  if (tt) tt.addEventListener('change', e => { document.getElementById('f-tr-fields').innerHTML = transportFields(e.target.value, {}, ''); });
}

function readForm(id) {
  const protocol = fv('f-protocol');
  const spec = PROTOCOLS[protocol] || {};
  const body = {
    name: fv('f-name').trim(), protocol, port: Number(fv('f-port')),
    upstreamId: Number(fv('f-upstream')), enabled: fchk('f-enabled'),
    routeRules: rrRead(),
  };
  // 部署到哪些服务器:全不勾或全勾 = 全部(存空)
  const nodeCbs = [...document.querySelectorAll('.node-cb')];
  const picked = nodeCbs.filter(c => c.checked).map(c => Number(c.value));
  body.nodeIds = (picked.length && picked.length < nodeCbs.length) ? picked : [];
  // TLS
  if (!spec.noTls) {
    const tls = { mode: fv('f-tlsmode') };
    if (fv('f-fp')) tls.fingerprint = fv('f-fp');
    if (tls.mode === 'reality') {
      if (!fv('f-priv').trim()) throw new Error(t('line.reality.private') + ' ' + t('line.reality.gen'));
      tls.reality = {
        handshake_server: fv('f-hs').trim(), handshake_port: Number(fv('f-hsport')) || 443,
        private_key: fv('f-priv').trim(), public_key: fv('f-pub').trim(),
        short_ids: fv('f-sids').split(',').map(s => s.trim()).filter(Boolean),
      };
    }
    body.tls = tls;
  }
  // 传输
  if (spec.transport) {
    const type = fv('f-trtype');
    if (type && type !== 'tcp') {
      const tr = { type };
      if (type === 'ws' || type === 'httpupgrade' || type === 'http') {
        tr.path = fv('f-trpath').trim() || '/';
        const host = fv('f-trhost').trim();
        if (host) {
          if (type === 'ws') tr.headers = { Host: host };
          else if (type === 'http') tr.host = host.split(',').map(s => s.trim()).filter(Boolean);
          else tr.host = host;
        }
      }
      if (type === 'grpc') tr.service_name = fv('f-trsvc').trim() || 'grpc';
      body.transport = tr;
    } else body.transport = {};
  }
  // 协议参数
  const o = JSON.parse(fv('f-extra').trim() || '{}');
  switch (protocol) {
    case 'vless': o.vision = fchk('f-vision'); break;
    case 'hysteria2':
      if (Number(fv('f-up')) > 0) o.up_mbps = Number(fv('f-up')); else delete o.up_mbps;
      if (Number(fv('f-down')) > 0) o.down_mbps = Number(fv('f-down')); else delete o.down_mbps;
      if (fv('f-obfs')) o.obfs = { type: 'salamander', password: fv('f-obfs') }; else delete o.obfs;
      if (fv('f-hop').trim()) o.port_hopping = fv('f-hop').trim(); else delete o.port_hopping;
      break;
    case 'tuic': o.congestion_control = fv('f-cc'); break;
    case 'shadowsocks':
      o.method = fv('f-method'); o.password = fv('f-sspw').trim();
      if (!o.password) throw new Error(t('line.password'));
      break;
    case 'anytls': {
      const lines = fv('f-padding').split('\n').map(s => s.trim()).filter(Boolean);
      if (lines.length) o.padding_scheme = lines; else delete o.padding_scheme;
      break;
    }
  }
  body.options = o;
  return body;
}

// 一键预设:常用协议组合,随机挑一个未占用端口,Reality 自动生成密钥
const PRESETS = [
  { id: 'hy2', label: 'Hysteria2', hint: 'UDP · 抗丢包 · 需证书', protocol: 'hysteria2', tls: { mode: 'cert' } },
  { id: 'anytls', label: 'AnyTLS', hint: 'TCP · 流量特征弱 · 需证书', protocol: 'anytls', tls: { mode: 'cert' } },
  { id: 'reality', label: 'VLESS + Reality', hint: 'TCP · 无需证书/域名 · Vision', protocol: 'vless', tls: { mode: 'reality' }, options: { vision: true } },
  { id: 'trojan', label: 'Trojan', hint: 'TCP · 需证书', protocol: 'trojan', tls: { mode: 'cert' } },
  { id: 'ss2022', label: 'Shadowsocks 2022', hint: 'TCP/UDP · 无 TLS · 兼容性最好', protocol: 'shadowsocks', tls: { mode: 'none' }, options: { method: '2022-blake3-aes-128-gcm' } },
  { id: 'vmess-ws', label: 'VMess + WS', hint: 'TCP · 可套 CDN', protocol: 'vmess', tls: { mode: 'none' }, transport: { type: 'ws', path: '/ws' } },
];
// 新线路的默认端口:优先问服务端(它避开面板/订阅/代理面板端口,并真的试着监听一次);
// 拿不到就在本地挑一个没被其它线路用的五位端口。填好后仍然可以改。
async function suggestPort() {
  try {
    const r = await get('keygen?type=port');
    if (r && r.port) return r.port;
  } catch { /* 接口不可用时退回本地挑 */ }
  const used = new Set(state.lines.map(l => l.port));
  for (let i = 0; i < 50; i++) {
    const p = 10000 + Math.floor(Math.random() * 55536);
    if (!used.has(p)) return p;
  }
  return 10000 + Math.floor(Math.random() * 55536);
}
async function presetLine(pid) {
  const p = PRESETS.find(x => x.id === pid);
  if (!p) return;
  const port = await suggestPort();
  const l = { protocol: p.protocol, enabled: true, options: p.options || {}, upstreamId: 0, port, name: `${p.label}-${port}`, tls: p.tls, transport: p.transport };
  if (p.tls && p.tls.mode === 'reality') {
    try {
      const kp = await get('keygen?type=reality');
      const sid = await get('keygen?type=shortid');
      l.tls = { mode: 'reality', reality: { private_key: kp.privateKey || kp.private_key, public_key: kp.publicKey || kp.public_key, short_ids: [sid.shortId || sid.short_id || sid.value], handshake_server: 'www.apple.com', handshake_port: 443 } };
    } catch (e) { toast(e.message, 'err'); }
  }
  editLine(null, null, l);
}

async function editLine(id, cloneFrom, preset) {
  await load('nodes').catch(() => {}); // 服务器列表可能刚在别的页面改过,"部署到服务器"要用最新的
  const src = cloneFrom || (id ? state.lines.find(x => x.id === id) : null);
  const l = src ? { ...src } : (preset || { protocol: 'vless', enabled: true, options: {}, upstreamId: 0 });
  if (cloneFrom) { l.name = t('line.cloneOf', { name: src.name }); l.port = ''; }
  if (!l.port) l.port = await suggestPort(); // 新建/克隆:给个可用端口做默认值,可改
  const rrInit = rrOf(l);
  openModal(id ? t('line.edit') : t('line.add'), `
    <h3>${t('line.basic')}</h3>
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(l.name || '')}">`, t('line.nameHelp'))}
      ${field(t('line.protocol'), sel('f-protocol', Object.keys(PROTOCOLS), l.protocol))}
      ${field(t('common.port'), `<input id="f-port" type="number" min="1" max="65535" value="${l.port || ''}">`, t('line.portHelp'))}
      ${field(t('line.upstream'), `<select id="f-upstream"><option value="0">${t('line.direct')}</option>${state.upstreams.map(u => `<option value="${u.id}" ${l.upstreamId === u.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')}</select>`, t('line.upstreamHelp'))}
      <div class="full rr-box">
        <div class="rr-top">
          ${check('f-rr-on', t('line.rr.on'), rrInit.length > 0, t('line.rr.help'))}
          <div class="rr-lookup" id="f-rr-lookup" ${rrInit.length ? '' : 'hidden'}>
            <input id="f-rr-apps" placeholder="${esc(t('line.rr.appPh'))}" aria-label="${esc(t('line.rr.appPh'))}" autocomplete="off" spellcheck="false">
            <button type="button" class="btn sm" id="f-rr-apps-go">${esc(t('line.rr.lookup'))}</button>
          </div>
        </div>
        <p class="rr-lookup-msg" id="f-rr-apps-msg" hidden></p>
        <div id="f-rr" ${rrInit.length ? '' : 'hidden'}>
          <div class="rr-row rr-head" aria-hidden="true"><span>#</span><span>${esc(t('line.rr.type'))}</span><span>${esc(t('line.rr.values'))}</span><span>${esc(t('line.rr.to'))}</span><span></span></div>
          <div id="f-rr-list"></div>
          <div class="rr-row rr-rest"><span class="rr-no">—</span><span class="rr-rest-label">${esc(t('line.rr.rest'))}</span><span class="muted small">${esc(t('line.rr.restHint'))}</span><span class="rr-rest-to" id="f-rr-rest"></span><span></span></div>
          <button type="button" class="rr-add" id="f-rr-add">${esc(t('line.rr.add'))}</button>
          <p class="hint">${esc(t('line.rr.hint'))}</p>
        </div>
      </div>
      ${check('f-enabled', t('common.enabled'), l.enabled !== false)}
      ${id ? '' : check('f-assign', t('line.assignAll'), true, t('line.assignAllHelp'))}
      ${(state.nodes || []).length > 1 ? `<div class="full">${field(t('line.servers'), `<div class="check-list">${(state.nodes || []).map(n => {
        const ids = (() => { const v = l.nodeIds; if (!v) return []; try { return Array.isArray(v) ? v : JSON.parse(v); } catch { return []; } })();
        return `<label><input type="checkbox" class="node-cb" value="${n.id}" ${!ids.length || ids.includes(n.id) ? 'checked' : ''}> ${esc(n.name)}${n.isLocal ? ` <span class="muted small">(${t('node.local')})</span>` : ''}</label>`;
      }).join('')}</div>`, t('line.serversHelp'))}</div>` : ''}
    </div>
    <div id="f-dyn"></div>
    <details class="adv"><summary>${t('line.advanced')}</summary><textarea id="f-extra"></textarea></details>
    <details class="adv" style="margin-top:.5rem"><summary>${t('line.addrs')}</summary>
      <textarea id="f-addrs" placeholder='[{"server":"1.2.3.4","server_port":443,"remark":"-备用"}]'>${esc(l.addrs ? (typeof l.addrs === 'string' ? l.addrs : JSON.stringify(l.addrs, null, 2)) : '')}</textarea>
      <p class="hint">${t('line.addrsHelp')}</p></details>`, async () => {
    const body = readForm(id);
    const addrs = fv('f-addrs').trim();
    if (addrs) body.addrs = JSON.parse(addrs);
    if (!id) body.assignAll = fchk('f-assign');
    if (id) await put('lines/' + id, body); else await post('lines', body);
    await load('lines', 'status', 'users');
    renderRows();
    toast(id ? t('line.updated') : t('line.created'), 'ok');
  }, { wide: true });
  renderDynamic(l);
  rrBind(rrInit);
  document.getElementById('f-protocol').addEventListener('change', () => renderDynamic({ options: {}, tls: {}, transport: {} }));
}

// ---- 分流规则:同一条线路按域名 / IP 段 / 端口分给不同出口,没命中的走线路的上游(后端见 render/route_rules.go) ----
const RR_TYPES = ['domain_suffix', 'domain', 'domain_keyword', 'ip_cidr', 'port'];
const RR_NEW = () => ({ type: 'domain_suffix', values: [], to: 0 });
function rrOf(l) { const v = parseJ(l.routeRules); return Array.isArray(v) ? v : []; }

// 出口:直连 / 上游(分组)/ 拦截
function rrOutOptions(to) {
  const opt = (v, label) => `<option value="${v}" ${to === v ? 'selected' : ''}>${esc(label)}</option>`;
  const ups = state.upstreams.length ? `<optgroup label="${esc(t('nav.upstreams'))}">${state.upstreams.map(u => opt(u.id, u.name)).join('')}</optgroup>` : '';
  return opt(0, t('line.direct')) + ups + opt(-1, t('line.rr.reject'));
}

function rrRowHTML(r, i, n) {
  const type = RR_TYPES.includes(r.type) ? r.type : 'domain_suffix';
  const typeSel = `<select class="rr-type" aria-label="${esc(t('line.rr.type'))}" ${r.name ? 'hidden' : ''}>${RR_TYPES.map(k => `<option value="${k}" ${type === k ? 'selected' : ''}>${esc(t('line.rr.t.' + k))}</option>`).join('')}</select>`;
  // 查应用域名得来的规则带着应用名:左边显示应用名,下面小字是匹配方式(匹配方式就不给改了)
  const named = r.name ? `<span class="rr-name" title="${esc(r.name)}"><b>${esc(r.name)}</b><small>${esc(t('line.rr.t.' + type))}</small></span>` : '';
  return `<div class="rr-row" data-name="${esc(r.name || '')}">
    <span class="rr-no">${i + 1}</span>
    ${named ? `<span class="rr-type-cell">${named}${typeSel}</span>` : typeSel}
    <textarea class="rr-values" rows="1" spellcheck="false" autocomplete="off" aria-label="${esc(t('line.rr.values'))}" placeholder="${esc(t('line.rr.ph.' + type))}">${esc(r.raw ?? (r.values || []).join(', '))}</textarea>
    <select class="rr-to" aria-label="${esc(t('line.rr.to'))}">${rrOutOptions(Number(r.to) || 0)}</select>
    <span class="rr-ops">
      <button type="button" class="btn sm ghost" data-rr="up" title="${esc(t('line.rr.up'))}" aria-label="${esc(t('line.rr.up'))}" ${i === 0 ? 'disabled' : ''}>↑</button>
      <button type="button" class="btn sm ghost" data-rr="down" title="${esc(t('line.rr.down'))}" aria-label="${esc(t('line.rr.down'))}" ${i === n - 1 ? 'disabled' : ''}>↓</button>
      <button type="button" class="btn sm ghost danger" data-rr="del" title="${esc(t('common.delete'))}" aria-label="${esc(t('common.delete'))}">✕</button>
    </span>
    <div class="rr-err" hidden></div>
  </div>`;
}

// 查应用的域名:逗号、顿号、空格隔开都行
const rrAppNames = s => s.split(/[\s,，;；、/|]+/).filter(Boolean);

// 查域名:面板所在的服务器去公开的域名库(v2fly)里查,每个应用作为一条带应用名的规则加到列表末尾:
// 完整域名并进域名后缀(只会多匹配它自己的子域名),关键字(少见)另起一条同名的。
// 内容可以再改,出口在规则里自己选(先是直连,光标落到第一条新规则的出口上)。列表里只有一条还没填的空规则时直接用它的位置。
// 没查到的名字留在查询框里,下面写上相近的名字(点一下替换再查)。
async function rrAppsLookup() {
  const input = document.getElementById('f-rr-apps'), btn = document.getElementById('f-rr-apps-go'), msg = document.getElementById('f-rr-apps-msg');
  const say = (html, err) => { msg.innerHTML = html; msg.hidden = !html; msg.classList.toggle('danger-text', !!err); };
  const names = rrAppNames(input.value);
  if (!names.length) { say(esc(t('line.rr.appEmpty')), true); input.focus(); return; }
  btn.disabled = true;
  btn.textContent = t('line.rr.looking');
  say('');
  let res;
  try {
    res = (await post('route-apps', { names }, SLOW)).results || [];
  } catch (e) {
    say(esc(e.message), true);
    return;
  } finally {
    btn.disabled = false;
    btn.textContent = t('line.rr.lookup');
  }
  const found = res.filter(r => !r.error), missed = res.filter(r => r.error);
  const add = [];
  found.forEach(r => {
    const dom = [...new Set([...(r.suffix || []), ...(r.full || [])])];
    if (dom.length) add.push({ name: r.query, type: 'domain_suffix', values: dom, to: 0 });
    if ((r.keyword || []).length) add.push({ name: r.query, type: 'domain_keyword', values: r.keyword, to: 0 });
  });
  let rules = rrCollect(true);
  if (rules.length === 1 && !rules[0].values.length) rules = [];
  const first = rules.length; // 第一条新规则的位置
  if (add.length) rrRender([...rules, ...add]);
  const parts = [];
  if (found.length) {
    const n = add.reduce((s, r) => s + r.values.length, 0);
    const detail = found.map(r => `${r.query} ${(r.suffix || []).length + (r.full || []).length + (r.keyword || []).length}`).join('、');
    const total = rrCollect().reduce((s, r) => s + r.values.length, 0);
    parts.push(esc(total > 5000 ? t('line.rr.appTooMany', { n: total }) : t('line.rr.appDone', { k: add.length, n, detail })));
  }
  if (missed.length) {
    const sug = missed.flatMap(r => (r.suggest || []).map(s => `<button type="button" class="link" data-rr-sug="${esc(s)}" data-q="${esc(r.query)}">${esc(s)}</button>`));
    parts.push(esc(t('line.rr.appMissed', { names: missed.map(r => r.query).join('、') })) + (sug.length ? ' ' + esc(t('line.rr.appSuggest')) + ' ' + sug.join(' ') : ''));
  }
  input.value = missed.map(r => r.query).join(', '); // 查到的从框里拿掉,没查到的留着改
  say(parts.join('<br>'), missed.length > 0 && !found.length);
  const to = add.length && document.querySelectorAll('#f-rr-list .rr-row')[first]?.querySelector('.rr-to');
  if (to) { to.scrollIntoView({ block: 'nearest' }); to.focus(); }
}

const rrSplit = s => s.split(/[\s,，;；]+/).filter(Boolean);

// 填的时候就地检查,和后端 render.normRouteValue 同一套规矩(后端仍是最终把关)
const rrIPv4 = s => { const p = s.split('.'); return p.length === 4 && p.every(x => /^\d{1,3}$/.test(x) && +x <= 255); };
const rrIPv6 = s => s.includes(':') && /^[0-9a-f:.]+$/i.test(s);
function rrBadValue(type, v) {
  if (type === 'domain_keyword') return '';
  if (type === 'ip_cidr') {
    const [a, p, extra] = v.split('/');
    const bits = rrIPv4(a) ? 32 : rrIPv6(a) ? 128 : 0;
    return bits && extra === undefined && (p === undefined || (/^\d{1,3}$/.test(p) && +p <= bits)) ? '' : t('line.rr.err.ip', { v });
  }
  if (type === 'port') {
    const m = v.match(/^(\d+)(?:[-:](\d+))?$/);
    const lo = m ? +m[1] : 0, hi = m && m[2] ? +m[2] : lo;
    return m && lo >= 1 && hi <= 65535 && lo <= hi ? '' : t('line.rr.err.port', { v });
  }
  if (/[/:@*?#]/.test(v)) return t('line.rr.err.domain', { v });
  if (rrIPv4(v)) return t('line.rr.err.isIP', { v });
  return '';
}

// 检查一条,把第一个问题写在这条下面;返回问题(没有 = '')
function rrRowCheck(row) {
  const type = row.querySelector('.rr-type').value;
  let msg = '';
  for (const v of rrSplit(row.querySelector('.rr-values').value)) if ((msg = rrBadValue(type, v))) break;
  const err = row.querySelector('.rr-err');
  err.textContent = msg;
  err.hidden = !msg;
  row.querySelector('.rr-values').classList.toggle('bad', !!msg);
  return msg;
}

// 从界面读回当前的规则:值用换行、逗号(中英文)、空格、分号隔开都行;raw 是原样的输入,
// 排序 / 添加 / 删除重画时照原样放回去,不把用户的写法改掉(只发 values 给后端)
function rrCollect(withRaw) {
  return [...document.querySelectorAll('#f-rr-list .rr-row')].map(row => {
    const ta = row.querySelector('.rr-values');
    const r = { type: row.querySelector('.rr-type').value, values: rrSplit(ta.value), to: Number(row.querySelector('.rr-to').value) };
    if (row.dataset.name) r.name = row.dataset.name;
    if (withRaw) r.raw = ta.value;
    return r;
  });
}

// 输入框按内容长高(换行和逗号写法自动折行都算),最多约 8 行,再多出滚动条
function rrFit(ta) {
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight + 2, 180) + 'px';
}

function rrRender(rules, focusLast) {
  const list = document.getElementById('f-rr-list');
  list.innerHTML = rules.length
    ? rules.map((r, i) => rrRowHTML(r, i, rules.length)).join('')
    : `<p class="rr-empty">${esc(t('line.rr.none'))}</p>`;
  list.querySelectorAll('.rr-row').forEach(rrRowCheck);
  list.querySelectorAll('.rr-values').forEach(rrFit);
  if (focusLast) list.querySelector('.rr-row:last-child .rr-values')?.focus();
}

// 最后一行"其余流量 → 线路的上游",跟着上面的上游下拉变
function rrRest() {
  const up = document.getElementById('f-upstream');
  document.getElementById('f-rr-rest').textContent = up && up.selectedOptions[0] ? up.selectedOptions[0].textContent : '';
}

function rrBind(initial) {
  const on = document.getElementById('f-rr-on'), box = document.getElementById('f-rr'), list = document.getElementById('f-rr-list');
  rrRender(initial);
  rrRest();
  document.getElementById('f-upstream').addEventListener('change', rrRest);
  on.addEventListener('change', () => {
    box.hidden = !on.checked;
    document.getElementById('f-rr-lookup').hidden = !on.checked;
    if (!on.checked) document.getElementById('f-rr-apps-msg').hidden = true;
    if (on.checked && !rrCollect().length) rrRender([RR_NEW()], true); // 勾上就给一条空规则,直接填
    else if (on.checked) list.querySelectorAll('.rr-values').forEach(rrFit); // 藏着时量不出高度,显示出来再量
  });
  document.getElementById('f-rr-add').addEventListener('click', () => rrRender([...rrCollect(true), RR_NEW()], true));
  // 查应用的域名:按钮或回车;点相近的名字把没找到的那个换成它再查
  const apps = document.getElementById('f-rr-apps');
  document.getElementById('f-rr-apps-go').addEventListener('click', rrAppsLookup);
  apps.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); rrAppsLookup(); } });
  document.getElementById('f-rr-apps-msg').addEventListener('click', e => {
    const sug = e.target.closest('[data-rr-sug]');
    if (!sug) return;
    apps.value = rrAppNames(apps.value).map(x => x === sug.dataset.q ? sug.dataset.rrSug : x).join(', ');
    rrAppsLookup();
  });
  list.addEventListener('click', e => {
    const b = e.target.closest('[data-rr]');
    if (!b) return;
    const i = [...list.querySelectorAll('.rr-row')].indexOf(b.closest('.rr-row'));
    const rules = rrCollect(true);
    if (b.dataset.rr === 'del') rules.splice(i, 1);
    else {
      const j = b.dataset.rr === 'up' ? i - 1 : i + 1;
      if (j < 0 || j >= rules.length) return;
      [rules[i], rules[j]] = [rules[j], rules[i]];
    }
    rrRender(rules);
  });
  list.addEventListener('change', e => {
    if (!e.target.classList.contains('rr-type')) return;
    const row = e.target.closest('.rr-row');
    row.querySelector('.rr-values').placeholder = t('line.rr.ph.' + e.target.value);
    rrRowCheck(row);
  });
  list.addEventListener('input', e => { if (e.target.classList.contains('rr-values')) rrFit(e.target); }); // 长高立刻做,检查稍等一下再做
  list.addEventListener('input', debounce(e => { const row = e.target.closest('.rr-row'); if (row) rrRowCheck(row); }, 300));
}

// 保存时用:没勾 = 不分流(存空);勾了就每条都要有内容、内容要合规
function rrRead() {
  if (!fchk('f-rr-on')) return [];
  const rows = [...document.querySelectorAll('#f-rr-list .rr-row')];
  const rules = rrCollect();
  rules.forEach((r, i) => {
    if (!r.values.length) throw new Error(t('line.rr.emptyRule', { n: i + 1 }));
    const bad = rrRowCheck(rows[i]);
    if (bad) throw new Error(t('line.rr.errAt', { n: i + 1, msg: bad }));
  });
  return rules;
}

// ---- 批量设置 ----
async function runBatch(action, extra = {}) {
  const ids = [...selected];
  try {
    const r = await post('lines/batch', { ids, action, ...extra });
    selected.clear();
    await load('lines', 'status'); renderRows();
    toast(t('line.batchDone', { n: r.affected }), 'ok');
  } catch (e) { toast(e.message, 'err'); }
}
function batchDialog(action) {
  const n = selected.size;
  if (action === 'upstream') {
    openModal(t('line.batchUpstream', { n }), `<div class="form-grid">${field(t('line.upstream'), `<select id="b-upstream"><option value="0">${t('line.direct')}</option>${state.upstreams.map(u => `<option value="${u.id}">${esc(u.name)}</option>`).join('')}</select>`, t('line.upstreamHelp'))}</div>`,
      () => runBatch('upstream', { upstreamId: Number(fv('b-upstream')) }));
    return;
  }
  openModal(t('line.batchNodes', { n }), `<div class="form-grid"><div class="full">${field(t('line.servers'), `<div class="check-list">${(state.nodes || []).map(nd => `<label><input type="checkbox" class="b-node-cb" value="${nd.id}"> ${esc(nd.name)}${nd.isLocal ? ` <span class="muted small">(${t('node.local')})</span>` : ''}</label>`).join('')}</div>`, t('line.batchNodesHelp'))}</div></div>`,
    () => runBatch('nodes', { nodeIds: [...document.querySelectorAll('.b-node-cb:checked')].map(c => Number(c.value)) }));
}

registerActions({
  'line.sel': (id, cb) => { cb.checked ? selected.add(Number(id)) : selected.delete(Number(id)); renderRows(); },
  'line.clearSel': () => { selected.clear(); renderRows(); },
  'line.batch': async action => {
    if (!selected.size) return;
    if (action === 'enable' || action === 'disable') { await runBatch(action); return; }
    batchDialog(action);
  },
  'line.preset': id => presetLine(id),
  'line.add': () => editLine(null),
  'line.edit': id => editLine(Number(id)),
  'line.clone': id => editLine(null, state.lines.find(x => x.id === Number(id))),
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
  'line.genReality': async () => {
    const k = await get('keygen?type=reality');
    document.getElementById('f-priv').value = k.privateKey;
    document.getElementById('f-pub').value = k.publicKey;
    if (!fv('f-sids').trim()) document.getElementById('f-sids').value = (await get('keygen?type=shortid')).shortId;
  },
  'line.genShortId': async () => {
    const cur = fv('f-sids').trim();
    const s = (await get('keygen?type=shortid')).shortId;
    document.getElementById('f-sids').value = cur ? cur + ',' + s : s;
  },
  'line.genSsPw': async () => {
    const m = fv('f-method');
    const n = m.startsWith('2022-blake3-aes-128') ? 16 : m.startsWith('2022') ? 32 : 16;
    if (m.startsWith('2022')) {
      // 2022 算法要求 base64 的 16/32 字节密钥
      const bytes = new Uint8Array(n); crypto.getRandomValues(bytes);
      document.getElementById('f-sspw').value = btoa(String.fromCharCode(...bytes));
    } else document.getElementById('f-sspw').value = (await get('keygen?type=password&len=16')).password;
  },
});

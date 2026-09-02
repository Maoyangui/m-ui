import { state, load } from '../app.js';
import { get, post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, openModal, registerActions, badge, dot, field, check, empty, fv, fchk, matches, debounce } from '../ui.js';

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
    <div class="table-wrap"><table class="grid">
      <thead><tr><th></th><th>${t('common.name')}</th><th>${t('line.protocol')}</th><th>${t('common.port')}</th><th>${t('line.upstream')}</th><th>${t('nav.nodes')}</th><th>${t('line.users')}</th><th>${t('common.status')}</th><th></th></tr></thead>
      <tbody id="lines-body"></tbody>
    </table></div>`;
  document.getElementById('line-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  renderRows();
}
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
  const rows = state.lines.filter(l => matches(query, l.name, l.protocol, l.port, l.upstreamName));
  document.getElementById('line-count').textContent = `${rows.length} / ${state.lines.length}`;
  if (!rows.length) { body.innerHTML = `<tr><td colspan="9">${empty()}</td></tr>`; return; }
  const nodeIdsOf = l => { const v = l.nodeIds; if (!v) return []; try { return Array.isArray(v) ? v : JSON.parse(v); } catch { return []; } };
  const serversCell = l => {
    const ids = nodeIdsOf(l);
    if (!ids.length) return `<span class="muted">${t('line.allServers')}</span>`;
    return ids.map(id => { const n = (state.nodes || []).find(x => x.id === id); return badge(n ? n.name : '#' + id, 'primary'); }).join(' ');
  };
  body.innerHTML = rows.map(l => `
    <tr draggable="${query ? 'false' : 'true'}" data-id="${l.id}">
      <td class="handle" title="拖动排序">⠿</td>
      <td class="primary-cell">${dot(online.has(l.name))}${esc(l.name)}</td>
      <td>${protoBadges(l)}</td>
      <td class="num">${l.port}</td>
      <td>${esc(l.upstreamName)}</td>
      <td>${serversCell(l)}</td>
      <td class="num">${l.userCount}</td>
      <td><label class="switch" title="${l.enabled ? t('common.enabled') : t('common.disabled')}"><input type="checkbox" data-change="line.toggle" data-id="${l.id}" ${l.enabled ? 'checked' : ''}><span></span></label></td>
      <td class="actions">
        <button class="btn sm" data-act="line.edit" data-id="${l.id}">${t('common.edit')}</button>
        <button class="btn sm" data-act="line.clone" data-id="${l.id}" title="${t('line.clone')}">⧉</button>
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
function randomFreePort() {
  const used = new Set(state.lines.map(l => l.port));
  for (let i = 0; i < 50; i++) { const p = 20000 + Math.floor(Math.random() * 40000); if (!used.has(p)) return p; }
  return 20000 + Math.floor(Math.random() * 40000);
}
async function presetLine(pid) {
  const p = PRESETS.find(x => x.id === pid);
  if (!p) return;
  const port = randomFreePort();
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

function editLine(id, cloneFrom, preset) {
  const src = cloneFrom || (id ? state.lines.find(x => x.id === id) : null);
  const l = src ? { ...src } : (preset || { protocol: 'vless', enabled: true, options: {}, upstreamId: 0 });
  if (cloneFrom) { l.name = t('line.cloneOf', { name: src.name }); l.port = ''; }
  openModal(id ? t('line.edit') : t('line.add'), `
    <h3>${t('line.basic')}</h3>
    <div class="form-grid">
      ${field(t('common.name'), `<input id="f-name" value="${esc(l.name || '')}">`, t('line.nameHelp'))}
      ${field(t('line.protocol'), sel('f-protocol', Object.keys(PROTOCOLS), l.protocol))}
      ${field(t('common.port'), `<input id="f-port" type="number" min="1" max="65535" value="${l.port || ''}">`, t('line.portHelp'))}
      ${field(t('line.upstream'), `<select id="f-upstream"><option value="0">${t('line.direct')}</option>${state.upstreams.map(u => `<option value="${u.id}" ${l.upstreamId === u.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')}</select>`, t('line.upstreamHelp'))}
      ${check('f-enabled', t('common.enabled'), l.enabled !== false)}
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
    if (id) await put('lines/' + id, body); else await post('lines', body);
    await load('lines', 'status');
    renderRows();
    toast(id ? t('line.updated') : t('line.created'), 'ok');
  }, { wide: true });
  renderDynamic(l);
  document.getElementById('f-protocol').addEventListener('change', () => renderDynamic({ options: {}, tls: {}, transport: {} }));
}

registerActions({
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

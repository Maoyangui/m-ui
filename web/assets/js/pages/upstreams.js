import { state, load } from '../app.js';
import { post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk, matches, debounce } from '../ui.js';

export const title = () => t('up.title');
export const subtitle = () => t('up.subtitle');

const TYPES = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', tuic: 'TUIC', hysteria2: 'Hysteria2', shadowsocks: 'Shadowsocks', socks: 'SOCKS5' };
const SS_METHODS = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305', '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305', 'none'];
const FPS = ['', 'chrome', 'firefox', 'safari', 'ios', 'android', 'edge', 'random'];
const FORM_KEYS = {
  vless: ['server', 'server_port', 'uuid', 'flow', 'tls', 'transport'],
  vmess: ['server', 'server_port', 'uuid', 'alter_id', 'security', 'tls', 'transport'],
  trojan: ['server', 'server_port', 'password', 'tls', 'transport'],
  tuic: ['server', 'server_port', 'uuid', 'password', 'congestion_control', 'udp_relay_mode', 'zero_rtt_handshake', 'udp_over_stream', 'tls'],
  hysteria2: ['server', 'server_port', 'password', 'tls', 'up_mbps', 'down_mbps', 'obfs'],
  shadowsocks: ['server', 'server_port', 'method', 'password'],
  socks: ['server', 'server_port', 'version', 'username', 'password'],
};
const HAS_TRANSPORT = { vless: 1, vmess: 1, trojan: 1 };
const results = {};
let query = '';

const parseOpts = o => { try { return typeof o === 'string' ? JSON.parse(o) : (o || {}); } catch { return {}; } };
const sel = (id, opts, cur, labels = {}) => `<select id="${id}">${opts.map(x => `<option value="${x}" ${cur === x ? 'selected' : ''}>${esc(labels[x] ?? x)}</option>`).join('')}</select>`;

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="up-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <span class="muted small" id="up-count"></span>
      <span class="grow"></span>
      <button class="btn" id="btn-test-all" data-act="up.testAll">${t('common.testAll')}</button>
      <button class="btn primary" data-act="up.add">${t('up.add')}</button>
    </div>
    <div class="table-wrap"><table class="grid">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.type')}</th><th>${t('common.server')}</th><th>${t('up.linesUsing')}</th><th>${t('up.latency')}</th><th></th></tr></thead>
      <tbody id="up-body"></tbody>
    </table></div>`;
  document.getElementById('up-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  renderRows();
}

function resultHTML(id) {
  const r = results[id];
  if (!r) return badge(t('up.untested'));
  if (r.testing) return badge(t('up.testing'));
  if (r.ok) return badge(r.delayMs + ' ms', 'ok') + (r.method === 'tcp' ? ' ' + badge('TCP') : '');
  return `${badge(t('up.fault'), 'danger')}<div class="sub-cell" title="${esc(r.error)}">${esc(r.error).slice(0, 80)}</div>`;
}

function typeBadges(u) {
  const o = parseOpts(u.options);
  let h = badge(TYPES[u.type] || u.type);
  if (o.tls && o.tls.reality && o.tls.reality.enabled) h += ' ' + badge('Reality', 'ok');
  else if (o.tls && o.tls.enabled) h += ' ' + badge('TLS');
  if (o.transport && o.transport.type) h += ' ' + badge(o.transport.type);
  return h;
}

function renderRows() {
  const body = document.getElementById('up-body');
  if (!body) return;
  const usage = {};
  state.lines.forEach(l => { if (l.upstreamId) usage[l.upstreamId] = (usage[l.upstreamId] || 0) + 1; });
  const online = new Set(state.onlines.upstreams || []);
  const rows = state.upstreams.filter(u => { const o = parseOpts(u.options); return matches(query, u.name, u.type, o.server); });
  document.getElementById('up-count').textContent = `${rows.length} / ${state.upstreams.length}`;
  if (!rows.length) { body.innerHTML = `<tr><td colspan="6">${empty()}</td></tr>`; return; }
  body.innerHTML = rows.map(u => {
    const o = parseOpts(u.options);
    const srv = o.server ? o.server + (o.server_port ? ':' + o.server_port : '') : '';
    return `<tr>
      <td class="primary-cell">${online.has(u.name) ? '<span class="dot on"></span>' : ''}${esc(u.name)}</td>
      <td>${typeBadges(u)}</td>
      <td class="num">${esc(srv)}</td>
      <td class="num">${usage[u.id] || 0}</td>
      <td id="up-res-${u.id}">${resultHTML(u.id)}</td>
      <td class="actions">
        <button class="btn sm" data-act="up.test" data-id="${u.id}">${t('common.test')}</button>
        <button class="btn sm" data-act="up.edit" data-id="${u.id}">${t('common.edit')}</button>
        <button class="btn sm danger" data-act="up.del" data-id="${u.id}">${t('common.delete')}</button>
      </td></tr>`;
  }).join('');
}

function setResult(id, r) {
  results[id] = r;
  const cell = document.getElementById('up-res-' + id);
  if (cell) cell.innerHTML = resultHTML(id);
}

// ---- 表单片段 ----
function tlsClientFields(o, opts = {}) {
  const tls = o.tls || {};
  const reality = tls.reality || {};
  const mode = reality.enabled ? 'reality' : tls.enabled ? 'tls' : 'none';
  const modes = opts.required ? ['tls'] : opts.reality ? ['none', 'tls', 'reality'] : ['none', 'tls'];
  const fp = (tls.utls && tls.utls.fingerprint) || '';
  const alpn = Array.isArray(tls.alpn) ? tls.alpn.join(',') : (tls.alpn || (opts.h3 ? 'h3' : ''));
  return `
    ${opts.required ? '' : field(t('line.tlsMode'), sel('f-tlsmode', modes, mode, { none: t('line.tls.none'), tls: 'TLS', reality: 'Reality' }))}
    ${field(t('up.f.sni'), `<input id="f-sni" value="${esc(tls.server_name || '')}">`)}
    ${field(t('up.f.alpn'), `<input id="f-alpn" value="${esc(alpn)}">`)}
    ${field(t('line.fp'), sel('f-fp', FPS, fp, { '': '默认' }))}
    ${check('f-insecure', t('up.f.insecure'), !!tls.insecure)}
    <div id="f-reality-c" class="form-grid full" ${mode === 'reality' ? '' : 'hidden'}>
      ${field(t('line.reality.public'), `<input id="f-pbk" value="${esc(reality.public_key || '')}">`)}
      ${field('Short ID', `<input id="f-sid" value="${esc(reality.short_id || '')}">`)}
    </div>`;
}
function readClientTLS(server, opts = {}) {
  const mode = opts.required ? 'tls' : fv('f-tlsmode');
  if (mode === 'none') return null;
  const tls = { enabled: true, server_name: fv('f-sni').trim() || server };
  const alpn = fv('f-alpn').split(',').map(s => s.trim()).filter(Boolean);
  if (alpn.length) tls.alpn = alpn;
  if (fchk('f-insecure')) tls.insecure = true;
  let fp = fv('f-fp');
  if (mode === 'reality') {
    if (!fp) fp = 'chrome';
    tls.reality = { enabled: true, public_key: fv('f-pbk').trim(), short_id: fv('f-sid').trim() };
  }
  if (fp) tls.utls = { enabled: true, fingerprint: fp };
  return tls;
}
function transportFields(o) {
  const tr = o.transport || {};
  const type = tr.type || 'tcp';
  const host = tr.headers && tr.headers.Host ? tr.headers.Host : (Array.isArray(tr.host) ? tr.host.join(',') : (tr.host || ''));
  return `
    ${field(t('line.transport.type'), sel('f-trtype', ['tcp', 'ws', 'grpc', 'httpupgrade', 'http'], type, { tcp: t('line.transport.tcp') }))}
    <div id="f-tr-c" class="form-grid full">${transportSub(type, tr, host)}</div>`;
}
function transportSub(type, tr, host) {
  if (type === 'grpc') return field(t('line.transport.service'), `<input id="f-trsvc" value="${esc(tr.service_name || '')}">`);
  if (type === 'ws' || type === 'httpupgrade' || type === 'http')
    return field(t('line.transport.path'), `<input id="f-trpath" value="${esc(tr.path || '/')}">`) + field(t('line.transport.host'), `<input id="f-trhost" value="${esc(host || '')}">`);
  return '';
}
function readTransport() {
  const type = fv('f-trtype');
  if (!type || type === 'tcp') return null;
  const tr = { type };
  if (type === 'grpc') tr.service_name = fv('f-trsvc').trim();
  else {
    tr.path = fv('f-trpath').trim() || '/';
    const host = fv('f-trhost').trim();
    if (host) {
      if (type === 'ws') tr.headers = { Host: host };
      else if (type === 'http') tr.host = host.split(',').map(s => s.trim()).filter(Boolean);
      else tr.host = host;
    }
  }
  return tr;
}

function fieldsHTML(type, o) {
  const common = field(t('up.f.server'), `<input id="f-server" value="${esc(o.server || '')}" placeholder="example.com / 1.2.3.4">`) +
    field(t('up.f.port'), `<input id="f-port" type="number" min="1" max="65535" value="${o.server_port || ''}">`);
  switch (type) {
    case 'vless': return common +
      field(t('up.f.uuid'), `<input id="f-uuid" value="${esc(o.uuid || '')}">`) +
      field('Flow', sel('f-flow', ['', 'xtls-rprx-vision'], o.flow || '', { '': '无' })) +
      tlsClientFields(o, { reality: true }) + transportFields(o);
    case 'vmess': return common +
      field(t('up.f.uuid'), `<input id="f-uuid" value="${esc(o.uuid || '')}">`) +
      field('AlterId', `<input id="f-aid" type="number" value="${o.alter_id || 0}">`) +
      tlsClientFields(o) + transportFields(o);
    case 'trojan': return common +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`) +
      tlsClientFields(o, { required: true }) + transportFields(o);
    case 'tuic': return common +
      field(t('up.f.uuid'), `<input id="f-uuid" value="${esc(o.uuid || '')}">`) +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`) +
      field(t('up.f.cc'), sel('f-cc', ['cubic', 'bbr', 'new_reno'], o.congestion_control || 'cubic')) +
      field(t('up.f.relay'), sel('f-relay', ['', 'native', 'quic'], o.udp_relay_mode || '', { '': '默认' })) +
      tlsClientFields(o, { required: true, h3: true }) +
      check('f-zrtt', t('up.f.zrtt'), !!o.zero_rtt_handshake) + check('f-uos', t('up.f.uos'), !!o.udp_over_stream);
    case 'hysteria2': return common +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`) +
      tlsClientFields(o, { required: true, h3: true }) +
      field(t('up.f.up'), `<input id="f-up" type="number" value="${o.up_mbps || 0}">`) +
      field(t('up.f.down'), `<input id="f-down" type="number" value="${o.down_mbps || 0}">`) +
      field(t('up.f.obfs'), `<input id="f-obfs" value="${esc((o.obfs && o.obfs.password) || '')}">`);
    case 'shadowsocks': return common +
      field(t('up.f.method'), sel('f-method', SS_METHODS, o.method || 'aes-256-gcm')) +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`);
    case 'socks': return common +
      field(t('up.f.version'), sel('f-version', ['5', '4a', '4'], o.version || '5')) +
      field(t('up.f.username'), `<input id="f-username" value="${esc(o.username || '')}">`) +
      field(t('up.f.password2'), `<input id="f-password" value="${esc(o.password || '')}">`);
  }
  return common;
}

function readFields(type) {
  const o = { server: fv('f-server').trim(), server_port: Number(fv('f-port')) };
  const setTLS = opts => { const tls = readClientTLS(o.server, opts); if (tls) o.tls = tls; };
  const setTr = () => { const tr = readTransport(); if (tr) o.transport = tr; };
  switch (type) {
    case 'vless':
      o.uuid = fv('f-uuid').trim(); if (fv('f-flow')) o.flow = fv('f-flow');
      setTLS({ reality: true }); setTr(); break;
    case 'vmess':
      o.uuid = fv('f-uuid').trim(); o.alter_id = Number(fv('f-aid')) || 0; o.security = 'auto';
      setTLS(); setTr(); break;
    case 'trojan':
      o.password = fv('f-password'); setTLS({ required: true }); setTr(); break;
    case 'tuic':
      Object.assign(o, { uuid: fv('f-uuid').trim(), password: fv('f-password'), congestion_control: fv('f-cc') });
      if (fv('f-relay')) o.udp_relay_mode = fv('f-relay');
      if (fchk('f-zrtt')) o.zero_rtt_handshake = true;
      if (fchk('f-uos')) o.udp_over_stream = true;
      setTLS({ required: true }); break;
    case 'hysteria2':
      o.password = fv('f-password'); setTLS({ required: true });
      if (Number(fv('f-up')) > 0) o.up_mbps = Number(fv('f-up'));
      if (Number(fv('f-down')) > 0) o.down_mbps = Number(fv('f-down'));
      if (fv('f-obfs')) o.obfs = { type: 'salamander', password: fv('f-obfs') };
      break;
    case 'shadowsocks': Object.assign(o, { method: fv('f-method'), password: fv('f-password') }); break;
    case 'socks':
      o.version = fv('f-version');
      if (fv('f-username')) { o.username = fv('f-username'); o.password = fv('f-password'); }
      break;
  }
  return o;
}

function renderFields(type, o) {
  document.getElementById('f-fields').innerHTML = fieldsHTML(type, o || {});
  const known = new Set(FORM_KEYS[type] || []);
  const extra = {};
  Object.entries(o || {}).forEach(([k, v]) => { if (!known.has(k)) extra[k] = v; });
  document.getElementById('f-extra').value = Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
  const tm = document.getElementById('f-tlsmode');
  if (tm) tm.addEventListener('change', e => { const r = document.getElementById('f-reality-c'); if (r) r.hidden = e.target.value !== 'reality'; });
  const tt = document.getElementById('f-trtype');
  if (tt) tt.addEventListener('change', e => { document.getElementById('f-tr-c').innerHTML = transportSub(e.target.value, {}, ''); });
}

function editUpstream(id) {
  const u = id ? state.upstreams.find(x => x.id === id) : { type: 'vless', options: {} };
  const o = parseOpts(u.options);
  openModal(id ? t('up.edit') : t('up.add'), `
    <div class="form-grid">
      ${id ? '' : `<div class="full">${field(t('up.import'), `<div class="row"><input id="f-link" placeholder="${t('up.importPh')}"><button type="button" class="btn" data-act="up.parse">${t('up.parse')}</button></div>`)}</div>`}
      ${field(t('common.name'), `<input id="f-name" value="${esc(u.name || '')}">`)}
      ${field(t('common.type'), sel('f-type', Object.keys(TYPES), u.type, TYPES))}
      <div id="f-fields" class="form-grid full"></div>
      <details class="adv full"><summary>${t('up.advanced')}</summary><textarea id="f-extra"></textarea></details>
    </div>
    <p class="hint">${t('up.warpHint')}</p>`, async () => {
    const type = fv('f-type');
    const fields = readFields(type);
    if (!fields.server || !fields.server_port) throw new Error(t('up.f.server') + ' / ' + t('up.f.port'));
    if ((type === 'tuic' || type === 'vless' || type === 'vmess') && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(fields.uuid)) throw new Error('UUID: 8-4-4-4-12 hex');
    const extra = JSON.parse(fv('f-extra').trim() || '{}');
    const body = { name: fv('f-name').trim(), type, options: Object.assign({}, extra, fields) };
    if (id) await put('upstreams/' + id, body); else await post('upstreams', body);
    await load('upstreams', 'lines', 'status');
    renderRows();
    toast(id ? t('up.updated') : t('up.created'), 'ok');
  }, { wide: true });
  renderFields(u.type, o);
  document.getElementById('f-type').addEventListener('change', e => renderFields(e.target.value, { server: fv('f-server'), server_port: Number(fv('f-port')) || undefined }));
}

registerActions({
  'up.add': () => editUpstream(null),
  'up.edit': id => editUpstream(Number(id)),
  'up.del': async id => {
    const u = state.upstreams.find(x => x.id === Number(id));
    if (!await confirm(t('common.deleteConfirm', { name: u.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('upstreams/' + id); await load('upstreams', 'status'); renderRows(); toast(t('up.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'up.test': async id => {
    setResult(id, { testing: true });
    try { setResult(id, await post(`upstreams/${id}/test`)); }
    catch (e) { setResult(id, { ok: false, error: e.message }); }
  },
  'up.testAll': async (_, btn) => {
    btn.disabled = true;
    state.upstreams.forEach(u => setResult(u.id, { testing: true }));
    try {
      const rs = await post('upstreams/test');
      rs.forEach(r => setResult(r.id, r));
      const bad = rs.filter(r => !r.ok).length;
      toast(bad ? t('up.testDoneBad', { n: bad }) : t('up.testDone'), bad ? 'err' : 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { btn.disabled = false; }
  },
  'up.parse': async () => {
    const link = fv('f-link').trim();
    if (!link) return;
    try {
      const p = await post('upstreams/parse', { link });
      document.getElementById('f-type').value = p.type;
      if (!fv('f-name').trim()) document.getElementById('f-name').value = p.name || '';
      renderFields(p.type, p.options);
      document.getElementById('modal-err').textContent = '';
      toast(t('up.parsed', { type: TYPES[p.type] || p.type }), 'ok');
    } catch (e) { document.getElementById('modal-err').textContent = e.message; }
  },
});

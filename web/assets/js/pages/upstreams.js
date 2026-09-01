import { state, load } from '../app.js';
import { post, put, del } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, confirm, openModal, registerActions, badge, field, check, empty, fv, fchk, matches, debounce } from '../ui.js';

export const title = () => t('up.title');
export const subtitle = () => t('up.subtitle');

const TYPES = { tuic: 'TUIC', hysteria2: 'Hysteria2', shadowsocks: 'Shadowsocks', socks: 'SOCKS5' };
const SS_METHODS = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305', '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305', 'none'];
const FORM_KEYS = {
  tuic: ['server', 'server_port', 'uuid', 'password', 'congestion_control', 'udp_relay_mode', 'zero_rtt_handshake', 'udp_over_stream', 'tls'],
  hysteria2: ['server', 'server_port', 'password', 'tls', 'up_mbps', 'down_mbps', 'obfs'],
  shadowsocks: ['server', 'server_port', 'method', 'password'],
  socks: ['server', 'server_port', 'version', 'username', 'password'],
};
const results = {}; // id → 测试结果(内存,页面切换保留)
let query = '';

const parseOpts = o => { try { return typeof o === 'string' ? JSON.parse(o) : (o || {}); } catch { return {}; } };

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
      <td>${badge(TYPES[u.type] || u.type)}</td>
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

// ---- 表单 ----
function fieldsHTML(type, o) {
  const tls = o.tls || {};
  const alpn = Array.isArray(tls.alpn) ? tls.alpn.join(',') : (tls.alpn || 'h3');
  const common = field(t('up.f.server'), `<input id="f-server" value="${esc(o.server || '')}" placeholder="example.com / 1.2.3.4">`) +
    field(t('up.f.port'), `<input id="f-port" type="number" min="1" max="65535" value="${o.server_port || ''}">`);
  const tlsF = field(t('up.f.sni'), `<input id="f-sni" value="${esc(tls.server_name || '')}">`) +
    field(t('up.f.alpn'), `<input id="f-alpn" value="${esc(alpn)}">`) +
    check('f-insecure', t('up.f.insecure'), !!tls.insecure);
  const sel = (id, opts, cur, emptyLabel) => `<select id="${id}">${emptyLabel ? `<option value="">${emptyLabel}</option>` : ''}${opts.map(x => `<option value="${x}" ${cur === x ? 'selected' : ''}>${x}</option>`).join('')}</select>`;
  switch (type) {
    case 'tuic': return common +
      field(t('up.f.uuid'), `<input id="f-uuid" value="${esc(o.uuid || '')}" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx">`) +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`) +
      field(t('up.f.cc'), sel('f-cc', ['cubic', 'bbr', 'new_reno'], o.congestion_control || 'cubic')) +
      field(t('up.f.relay'), sel('f-relay', ['native', 'quic'], o.udp_relay_mode || '', '默认')) +
      tlsF + check('f-zrtt', t('up.f.zrtt'), !!o.zero_rtt_handshake) + check('f-uos', t('up.f.uos'), !!o.udp_over_stream);
    case 'hysteria2': return common +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`) + tlsF +
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
  const tlsOf = () => {
    const tl = { enabled: true, server_name: fv('f-sni').trim() || o.server };
    const alpn = fv('f-alpn').split(',').map(s => s.trim()).filter(Boolean);
    if (alpn.length) tl.alpn = alpn;
    if (fchk('f-insecure')) tl.insecure = true;
    return tl;
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
}

function editUpstream(id) {
  const u = id ? state.upstreams.find(x => x.id === id) : { type: 'tuic', options: {} };
  const o = parseOpts(u.options);
  openModal(id ? t('up.edit') : t('up.add'), `
    <div class="form-grid">
      ${id ? '' : `<div class="full">${field(t('up.import'), `<div class="row"><input id="f-link" placeholder="${t('up.importPh')}"><button type="button" class="btn" data-act="up.parse">${t('up.parse')}</button></div>`)}</div>`}
      ${field(t('common.name'), `<input id="f-name" value="${esc(u.name || '')}">`)}
      ${field(t('common.type'), `<select id="f-type">${Object.entries(TYPES).map(([k, v]) => `<option value="${k}" ${u.type === k ? 'selected' : ''}>${v}</option>`).join('')}</select>`)}
      <div id="f-fields" class="form-grid full"></div>
      <details class="adv full"><summary>${t('up.advanced')}</summary><textarea id="f-extra"></textarea></details>
    </div>
    <p class="hint">${t('up.warpHint')}</p>`, async () => {
    const type = fv('f-type');
    const fields = readFields(type);
    if (!fields.server || !fields.server_port) throw new Error(t('up.f.server') + ' / ' + t('up.f.port'));
    if (type === 'tuic' && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(fields.uuid)) throw new Error('UUID: 8-4-4-4-12 hex');
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

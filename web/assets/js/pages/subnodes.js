// 「节点」页:订阅里的每个节点 —— 一条线路在一台服务器上的直连,或这条线路的一个中转入口 —— 逐个设置
// 倍率(盖过所在服务器的)、限速、用户容量,和要不要输出到订阅。存在线路的 nodeOpts 里(键 "s<服务器 id>" / "f<入口 id>")。
//
// 版式:一张表,按线路分组,一行一个节点。平时只是看:倍率一格写着现在实际生效的倍率和它是「跟随服务器」还是单独设的,
// 一眼分得出;没设的限速、容量都是淡色的「不限」。常用的「输出到订阅」开关直接摆在行里,点了就存;
// 其余几项点「设置」在弹框里改(不在表里摆一排输入框:十几个节点就是几十个框,看着累,也容易改错行)。
import { state } from '../app.js';
import { put } from '../api.js';
import { t } from '../i18n.js';
import { esc, toast, openModal, registerActions, badge, field, empty, fv, fchk, matches, debounce, setHTML, switchRow } from '../ui.js';

export const title = () => t('sn.title');
export const subtitle = () => t('sn.subtitle');

let query = '';
let only = 'all'; // all / own(单独设置过的)/ off(没输出到订阅的)

const parseJ = v => { try { return typeof v === 'string' ? JSON.parse(v) : (v || null); } catch { return null; } };
const servers = () => state.nodes || [];
const hostOf = n => (n ? (n.domain || n.addr || n.publicIp || n.publicIp6 || '') : '');
const fmtRatio = r => String(Math.round(r * 10000) / 10000);
const isOwn = o => !!o && (o.ratio > 0 || o.speed > 0 || o.userSpeed > 0 || o.cap > 0);

// nodesOf 一条线路的全部节点:部署到的每台服务器一个直连,每个中转入口一个(和后端 lineNodeKeys 同一口径)。
export function nodesOf(l) {
  const all = servers();
  const ids = parseJ(l.nodeIds) || [];
  const opts = parseJ(l.nodeOpts) || {};
  const out = all.filter(n => !ids.length || ids.includes(n.id)).map(n => ({
    key: 's' + n.id, kind: 'direct', name: n.name, exit: n, host: hostOf(n), port: l.port, opt: opts['s' + n.id] || {},
    idle: n.enabled === false ? t('sn.serverOff') : '',
  }));
  for (const f of parseJ(l.forwards) || []) {
    if (!f.id) continue;
    const exit = all.find(n => n.id === f.to), via = f.via ? all.find(n => n.id === f.via) : null;
    out.push({
      key: 'f' + f.id, kind: 'relay', name: f.name || (via ? via.name : f.host) || ('#' + f.id), exit, via, fwd: f,
      host: via ? hostOf(via) : (f.host || ''), port: f.port, opt: opts['f' + f.id] || {},
      idle: f.off ? t('sn.paused') : (exit && exit.enabled === false) || (via && via.enabled === false) ? t('sn.serverOff') : '',
    });
  }
  return out;
}
// 没单独设倍率时跟的是哪台服务器的倍率:直连是所在的那台,中转入口是出口那台(流量在出口上记)
const serverRatio = nd => (nd.exit && nd.exit.ratio > 0 ? nd.exit.ratio : 1);

export async function render(el) {
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="sn-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <div class="seg" id="sn-only">${['all', 'own', 'off'].map(k => `<button type="button" data-act="sn.only" data-id="${k}" class="${only === k ? 'active' : ''}">${t('sn.filter.' + k)}</button>`).join('')}</div>
      <span class="muted small" id="sn-count"></span>
    </div>
    <div class="table-wrap fill"><table class="grid sn-table" data-fit="p3:940 p2:800 cards:700">
      <thead><tr><th>${t('sn.col.node')}</th><th class="c-p3">${t('sn.col.addr')}</th><th>${t('sn.col.ratio')}</th><th class="c-p2">${t('sn.col.speed')}</th><th class="c-p2">${t('sn.col.cap')}</th><th>${t('sn.col.output')}</th><th></th></tr></thead>
      <tbody id="sn-body"></tbody>
    </table></div>`;
  document.getElementById('sn-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  renderRows();
}

function speedText(o) {
  if (o.speed > 0 && o.userSpeed > 0) return t('sn.speedBoth', { a: o.speed, b: o.userSpeed });
  if (o.speed > 0) return t('sn.speedTotal', { a: o.speed });
  if (o.userSpeed > 0) return t('sn.speedUser', { b: o.userSpeed });
  return '';
}

function rowHTML(l, nd) {
  const o = nd.opt, id = `${l.id}:${nd.key}`;
  const where = nd.kind === 'relay'
    ? `${badge(t('sn.relay'), 'primary')}<span class="pname" title="${esc(nd.name)}">${esc(nd.name)}</span><span class="sn-to">${esc(t('sn.to', { name: nd.exit ? nd.exit.name : '?' }))}</span>`
    : `<span class="pname" title="${esc(nd.name)}">${esc(nd.name)}</span>`;
  const ratio = o.ratio > 0
    ? `<b class="sn-own">×${esc(fmtRatio(o.ratio))}</b>`
    : `<span class="sn-follow">×${esc(fmtRatio(serverRatio(nd)))}<span> · ${esc(t('sn.follow'))}</span></span>`;
  const speed = speedText(o), unl = `<span class="faint">${esc(t('sn.unlimited'))}</span>`;
  const addr = nd.host ? `${esc(nd.host)}:${nd.port}` : `:${nd.port}`;
  return `<tr class="${nd.idle || l.enabled === false ? 'sn-idle' : ''}">
    <td class="primary-cell"><div class="sn-node">${where}${nd.idle ? ` <span class="muted small">${esc(nd.idle)}</span>` : ''}</div><div class="sub-cell sn-addr" title="${esc(addr)}">${esc(addr)}</div></td>
    <td class="num c-p3"><span class="ellip" title="${esc(addr)}">${esc(addr)}</span></td>
    <td class="num">${ratio}</td>
    <td class="num c-p2${speed ? '' : ' sn-def'}">${speed ? esc(speed) : unl}</td>
    <td class="num c-p2${o.cap > 0 ? '' : ' sn-def'}">${o.cap > 0 ? esc(t('sn.capN', { n: o.cap })) : unl}</td>
    <td class="sn-out"><label class="switch" title="${esc(t('sn.outputHelp'))}"><input type="checkbox" data-change="sn.output" data-id="${id}" ${o.hidden ? '' : 'checked'} aria-label="${esc(t('sn.col.output'))}"><span></span></label></td>
    <td class="actions"><button class="btn sm" data-act="sn.edit" data-id="${id}">${t('sn.edit')}</button></td>
  </tr>`;
}

function renderRows() {
  const body = document.getElementById('sn-body');
  if (!body) return;
  let total = 0, shown = 0;
  const html = state.lines.map(l => {
    const all = nodesOf(l);
    total += all.length;
    const list = all.filter(nd => matches(query, l.name, nd.name, nd.host, nd.exit && nd.exit.name)
      && (only === 'all' || (only === 'own' ? isOwn(nd.opt) : !!nd.opt.hidden)));
    if (!list.length) return '';
    shown += list.length;
    return `<tr class="grp-row"><td colspan="7">${esc(l.name)} <span class="muted">${esc(String(l.protocol || '').toUpperCase())} · ${l.port}</span>${l.enabled === false ? ` ${badge(t('sn.lineOff'))}` : ''}<span class="muted">${esc(all.length === 1 ? t('sn.count1') : t('sn.count', { n: all.length }))}</span></td></tr>`
      + list.map(nd => rowHTML(l, nd)).join('');
  }).join('');
  document.getElementById('sn-count').textContent = total ? `${shown} / ${total}` : '';
  if (!state.lines.length) { setHTML(body, `<tr><td colspan="7"><div class="empty-guide"><p>${esc(t('sn.empty'))}</p><a class="btn primary" href="#/lines">${esc(t('sn.goLines'))}</a></div></td></tr>`); return; }
  setHTML(body, html || `<tr><td colspan="7">${empty()}</td></tr>`);
}

function find(id) {
  const [lineId, key] = String(id).split(':');
  const l = state.lines.find(x => x.id === Number(lineId));
  const nd = l ? nodesOf(l).find(x => x.key === key) : null;
  return { l, nd };
}

// 存一个节点的设置:后端整份替换这个节点的设置,回来的是整条线路,换进 state 里
async function saveNode(l, key, opt, src) {
  const body = { key, opt };
  if (src !== undefined) body.src = src;
  const line = await put(`lines/${l.id}/node`, body);
  const i = state.lines.findIndex(x => x.id === l.id);
  if (i >= 0) state.lines[i] = { ...state.lines[i], nodeOpts: line.nodeOpts || null, forwards: line.forwards || null };
  renderRows();
}

const cleanOpt = o => {
  const out = {};
  if (o.hidden) out.hidden = true;
  for (const k of ['ratio', 'speed', 'userSpeed', 'cap']) if (o[k] > 0) out[k] = o[k];
  return out;
};

function editNode(id) {
  const { l, nd } = find(id);
  if (!l || !nd) return;
  const o = nd.opt;
  const num = (eid, v, ph, attrs) => `<input id="${eid}" type="number" inputmode="decimal" min="0" ${attrs} value="${v > 0 ? esc(String(v)) : ''}" placeholder="${esc(ph)}">`;
  const known = nd.kind === 'relay' ? (nd.via ? [nd.via.publicIp, nd.via.publicIp6, nd.via.addr] : [nd.fwd.host]).filter(x => x && /^[0-9a-f:.]+$/i.test(x)) : [];
  const who = nd.kind === 'relay' ? `${nd.name} ${t('sn.to', { name: nd.exit ? nd.exit.name : '?' })}` : nd.name;
  openModal(t('sn.editTitle'), `
    <p class="sn-who"><b>${esc(l.name)}</b><span class="muted"> · ${esc(who)}</span>${nd.kind === 'relay' ? ' ' + badge(t('sn.relay'), 'primary') : ''}</p>
    <div class="form-grid">
      ${field(t('sn.ratio'), num('sn-ratio', o.ratio, t('sn.ratioPh', { r: fmtRatio(serverRatio(nd)) }), 'max="100" step="0.1"'), t('sn.ratioHelp'))}
      ${field(t('sn.cap'), num('sn-cap', o.cap, t('sn.unlimited'), 'max="1000000" step="1"'), t('sn.capHelp'))}
      ${field(t('sn.speed'), num('sn-speed', o.speed, t('sn.unlimited'), 'max="100000" step="1"'), t('sn.speedHelp'))}
      ${field(t('sn.userSpeed'), num('sn-uspeed', o.userSpeed, t('sn.unlimited'), 'max="100000" step="1"'), t('sn.userSpeedHelp'))}
    </div>
    <div class="sn-extra">
      ${switchRow('sn-out', t('sn.output'), !o.hidden, t('sn.outputHelp'))}
      ${nd.kind === 'relay' ? `<details class="adv"${nd.fwd.src ? ' open' : ''}><summary>${esc(t('sn.srcSummary'))}</summary>
        ${field(t('sn.src'), `<input id="sn-src" value="${esc(nd.fwd.src || '')}" placeholder="192.0.2.10, 2001:db8::10" autocomplete="off" spellcheck="false">`,
          t('sn.srcHelp') + (known.length ? ' ' + t('sn.srcKnown', { list: known.join('、') }) : ' ' + t('sn.srcNone')))}
      </details>` : ''}
      <div><button type="button" class="btn sm sn-reset" data-act="sn.reset">${esc(t('sn.reset'))}</button></div>
    </div>`,
  async () => {
    const f = eid => { const s = fv(eid).trim(); return s === '' ? 0 : Number(s); };
    const next = { hidden: !fchk('sn-out'), ratio: f('sn-ratio'), speed: f('sn-speed'), userSpeed: f('sn-uspeed'), cap: f('sn-cap') };
    if (!(next.ratio >= 0 && next.ratio <= 100)) throw new Error(t('sn.err.ratio'));
    for (const k of ['speed', 'userSpeed', 'cap']) if (!(Number.isInteger(next[k]) && next[k] >= 0)) throw new Error(t('sn.err.int'));
    const srcEl = document.getElementById('sn-src');
    const src = srcEl && srcEl.value.trim() !== (nd.fwd.src || '') ? srcEl.value.trim() : undefined;
    await saveNode(l, nd.key, cleanOpt(next), src);
    toast(t('sn.saved'), 'ok');
  });
}

registerActions({
  'sn.only': id => { only = id; document.querySelectorAll('#sn-only button').forEach(b => b.classList.toggle('active', b.dataset.id === only)); renderRows(); },
  'sn.edit': id => editNode(id),
  'sn.reset': () => {
    for (const eid of ['sn-ratio', 'sn-cap', 'sn-speed', 'sn-uspeed']) { const el = document.getElementById(eid); if (el) el.value = ''; }
    const out = document.getElementById('sn-out');
    if (out) out.checked = true;
  },
  // 行里的开关:点了就存;没存上把开关拨回去
  'sn.output': async (id, el) => {
    const { l, nd } = find(id);
    if (!l || !nd) return;
    const on = el.checked;
    el.disabled = true;
    try {
      await saveNode(l, nd.key, cleanOpt({ ...nd.opt, hidden: !on }));
      toast(t(on ? 'sn.outOn' : 'sn.outOff'), 'ok');
    } catch (e) {
      el.checked = !on;
      el.disabled = false;
      toast(e.message, 'err');
    }
  },
});

// 流量明细弹框:概况页点「流量」那一格打开。回答「这段时间的流量是谁用的、走了哪些线路、落在哪些服务器上」。
// 版式:最上面选时间范围(24 小时 / 7 天 / 30 天)→ 一排四个数(合计、下行、上行、有流量的用户)→ 三栏排行并排(窄了上下排):
//   用户 / 线路 / 服务器,每行 名次、名字、一条两色的条(深色下行、浅色上行,长度按这一栏里最大的那个算)、数值、占合计的百分比。
// 三栏是同一个合计的三种分法(都按倍率计);线路的流量要临时向各副机问,问不到的服务器那部分记成「没归到线路」并写明是哪几台。
import { get } from './api.js';
import { t } from './i18n.js';
import { esc, fmtBytes, openModal, registerActions, setHTML, modalGen } from './ui.js';

const RANGES = [[24, '24h'], [168, '7d'], [720, '30d']];
const TOP = 8; // 每栏先列前 8 个,其余合成一行
let hours = 24, data = null, loadErr = '', gen = 0;
const enc = encodeURIComponent;
const sum = x => (x.up || 0) + (x.down || 0);

// 一栏:rows [{name, up, down, href?, title?, muted?}],total 是整个合计(算占比),rest 是「其余」那一行(可无)
function column(title, sub, rows, total, restLabel) {
  if (!rows.length) return `<section class="tb-col"><h3>${esc(title)}${sub ? `<span class="muted">${esc(sub)}</span>` : ''}</h3><p class="tb-none">${esc(t('dash.bd.none'))}</p></section>`;
  // 前 8 个照排名列,排在后面的合成一行;「其余」「没归到线路」这类补差额的行(noRank)不占名次、永远排在最后
  const ranked = rows.filter(r => !r.noRank), tail = rows.filter(r => r.noRank);
  const shown = ranked.slice(0, TOP), more = ranked.slice(TOP);
  const list = shown.slice();
  if (more.length) list.push({ name: restLabel(more.length), up: more.reduce((a, x) => a + x.up, 0), down: more.reduce((a, x) => a + x.down, 0), rest: true });
  list.push(...tail);
  const max = Math.max(...list.map(sum)) || 1;
  // 占比按「合计」算;这一栏自己加起来比合计还多时(删掉的用户流量记录跟着删了,线路、服务器那边的还在)按这一栏的和算,不出现加起来超过 100% 的数
  const denom = Math.max(total, list.reduce((a, x) => a + sum(x), 0)) || 1;
  const row = (r, i) => {
    const v = sum(r), share = v / denom * 100;
    const label = esc(r.name) + (r.tag ? ` <small class="tb-tag">${esc(r.tag)}</small>` : ''); // 「本机」「已不在」跟在名字后面,不另起一行
    const name = r.href ? `<a class="tb-name" href="${r.href}" title="${esc(r.title || r.name)}">${label}</a>` : `<span class="tb-name${r.rest || r.muted ? ' muted' : ''}" title="${esc(r.title || r.name)}">${label}</span>`;
    // 条的总长按这一栏最大的算;里面下行、上行各占自己的份额
    const w = Math.max(1.5, v / max * 100), dw = v ? r.down / v * 100 : 0;
    return `<li class="tb-row${r.rest ? ' rest' : ''}">
      <span class="tb-rank">${r.rest || r.noRank ? '' : i + 1}</span>${name}
      <span class="tb-val" title="↓ ${esc(fmtBytes(r.down, 1))} · ↑ ${esc(fmtBytes(r.up, 1))}">${esc(fmtBytes(v, 1))}</span>
      <span class="tb-share">${share >= 0.95 ? Math.round(share) + '%' : share > 0 ? '<1%' : ''}</span>
      <span class="tb-bar"><i style="width:${w.toFixed(1)}%"><b style="width:${dw.toFixed(1)}%"></b></i></span>
    </li>`;
  };
  return `<section class="tb-col"><h3>${esc(title)}${sub ? `<span class="muted">${esc(sub)}</span>` : ''}</h3><ol class="tb-list">${list.map(row).join('')}</ol></section>`;
}

function bodyHTML() {
  if (loadErr) return `<p class="hint bad">${esc(loadErr)}</p>`;
  if (!data) return `<p class="tb-none">${esc(t('common.loading'))}</p>`;
  const total = sum(data.total || {});
  if (!total) return `<p class="tb-none">${esc(t('dash.bd.empty'))}</p>`;
  const days = hours / 24;
  const tile = (lb, v, s = '') => `<div class="ld-tile"><span class="lb">${esc(lb)}</span><b>${esc(v)}</b>${s ? `<span class="sub">${esc(s)}</span>` : ''}</div>`;
  const users = (data.users || []).map(u => ({ ...u, href: u.gone ? '' : (u.owner ? '#/resellers?q=' + enc(u.owner) : '#/users?q=' + enc(u.name)), muted: u.gone,
    title: u.gone ? u.name + ' · ' + t('dash.top.gone') : u.owner ? u.name + ' · ' + u.owner : u.name }));
  // 用户只带回前 50 名:其余的人数和用量用合计减出来
  const usersShown = users.slice(0, TOP), usersRestN = (data.userCount || users.length) - usersShown.length;
  const uUp = usersShown.reduce((a, x) => a + x.up, 0), uDown = usersShown.reduce((a, x) => a + x.down, 0);
  const userRows = usersShown.slice();
  if (usersRestN > 0) userRows.push({ name: t('dash.bd.restUsers', { n: usersRestN }), up: Math.max(0, data.total.up - uUp), down: Math.max(0, data.total.down - uDown), rest: true, noRank: true });
  let cols = column(t('dash.bd.users'), '', userRows, total, n => t('dash.bd.restUsers', { n }));
  if (data.nodes) {
    // 线路:各条线路加起来比合计少的那一截归不到具体线路(有服务器没问到,或者那台已经删了、停用了),单列一行写明,不让这一栏悄悄少一块
    const lines = (data.lines || []).map(l => ({ ...l, href: '#/lines?q=' + enc(l.name) }));
    const missing = data.linesMissing || [];
    const gapUp = Math.max(0, data.total.up - lines.reduce((a, x) => a + x.up, 0)), gapDown = Math.max(0, data.total.down - lines.reduce((a, x) => a + x.down, 0));
    if (gapUp + gapDown > total * 0.005) {
      lines.push({ name: t('dash.bd.unassigned'), up: gapUp, down: gapDown, muted: true, noRank: true,
        title: missing.length ? t('dash.bd.missing', { n: missing.length, names: missing.join('、') }) : t('dash.bd.unassignedWhy') });
    }
    const nodes = (data.nodes || []).map(n => ({ ...n, href: n.gone ? '' : '#/nodes', muted: n.gone, tag: n.local ? t('node.local') : n.gone ? t('dash.bd.gone') : '' }));
    cols += column(t('dash.bd.lines'), '', lines, total, n => t('dash.bd.rest', { n }))
      + column(t('dash.bd.nodes'), '', nodes, total, n => t('dash.bd.rest', { n }));
  }
  const missing = (data.linesMissing || []).length ? `<p class="hint warn">${esc(t('dash.bd.missing', { n: data.linesMissing.length, names: data.linesMissing.join('、') }))}</p>` : '';
  return `<div class="ld-sum tb-sum">
      ${tile(t('dash.bd.total'), fmtBytes(total, 1), days > 1 ? t('dash.bd.perDay', { v: fmtBytes(total / days, 1) }) : '')}
      ${tile(t('dash.bd.down'), fmtBytes(data.total.down, 1), Math.round(data.total.down / total * 100) + '%')}
      ${tile(t('dash.bd.up'), fmtBytes(data.total.up, 1), Math.round(data.total.up / total * 100) + '%')}
      ${tile(t('dash.bd.activeUsers'), t('dash.people', { n: data.userCount || 0 }))}
    </div>
    <div class="tb-cols${data.nodes ? '' : ' one'}">${cols}</div>
    ${missing}
    <p class="hint tb-legend"><span class="tb-key down"></span>${esc(t('dash.bd.down'))}<span class="tb-key up"></span>${esc(t('dash.bd.up'))}<span class="xp tb-note"><span class="sep">·</span>${esc(t(data.nodes ? 'dash.bd.note' : 'dash.bd.noteLocal'))}</span></p>`;
}

const shell = () => `<div class="tb">
    <div class="tb-head"><div class="seg" id="tb-range">${RANGES.map(([h, k]) => `<button type="button" data-act="traffic.range" data-id="${h}" class="${h === hours ? 'active' : ''}">${esc(t('dash.range.' + k))}</button>`).join('')}</div></div>
    <div id="tb-body">${bodyHTML()}</div>
  </div>`;

async function loadRange() {
  const my = ++gen, modal = modalGen();
  data = null; loadErr = '';
  setHTML('tb-body', bodyHTML());
  try { const r = await get('stats/breakdown?hours=' + hours); if (my !== gen) return; data = r; }
  catch (e) { if (my !== gen) return; loadErr = e.message; }
  if (modal === modalGen()) setHTML('tb-body', bodyHTML()); // 弹框还是这一个才写(期间关了、换了别的就不动)
}

// openTraffic 打开流量明细(h:先看哪个范围,默认 24 小时)
export function openTraffic(h = 24) {
  hours = RANGES.some(([x]) => x === h) ? h : 24;
  data = null; loadErr = '';
  openModal(t('dash.bd.title'), shell(), null, { size: 'xl', cancelText: t('common.close') });
  loadRange();
}

registerActions({
  'traffic.range': id => {
    hours = Number(id) || 24;
    document.querySelectorAll('#tb-range button').forEach(b => b.classList.toggle('active', Number(b.dataset.id) === hours));
    loadRange();
  },
});

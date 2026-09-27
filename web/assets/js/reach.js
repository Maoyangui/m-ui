// 大陆连通检测:服务器页的一列、详情抽屉、顶部工具条文案。数据来自 GET reach(主机内存里的最近结果)。
import { get, post } from './api.js';
import { t } from './i18n.js';
import { esc, fmtRelative, fmtTime, toast, openDrawer, drawerOpen, setHTML, registerActions } from './ui.js';

export let reach = null;            // 最近一次 GET reach 的结果;副机上是 { node: true }
let detailId = 0;                   // 抽屉里正在看的服务器
let onChange = () => {};            // 结果变了之后让服务器页重画这一列

export function setReachListener(fn) { onChange = fn; }

export async function loadReach() {
  try { reach = await get('reach'); } catch { reach = null; }
  return reach;
}

const GROUPS = ['cm', 'ct', 'cu'];
const BAD = new Set(['blocked', 'ipBlocked', 'portBlocked']);
// 结论 → 徽章颜色
const TONE = { ok: 'ok', partial: 'warn', blocked: 'danger', ipBlocked: 'danger', portBlocked: 'danger', abroadDown: 'danger', noProbes: '', error: '' };

export const isBad = v => BAD.has(v) || v === 'partial';
const resultOf = id => ((reach && reach.results) || []).find(r => r.nodeId === id);
const runningFor = id => !!(reach && reach.running && (reach.runningIds || []).includes(id));

function groupChip(g) {
  const label = t('reach.g.' + g.key + '.short');
  const title = `${t('reach.g.' + g.key)} ${g.total ? t('reach.okOf', { ok: g.ok, total: g.total }) : t('reach.noProbe')}${g.ok && g.avgMs ? ' · ' + Math.round(g.avgMs) + ' ms' : ''}`;
  const val = !g.total ? '–' : g.ok ? (g.avgMs ? Math.round(g.avgMs) : '✓') : '✕';
  return `<span class="rc ${g.state}" title="${esc(title)}">${esc(label)}<b>${esc(String(val))}</b></span>`;
}

// 服务器页"大陆连通"那一格
export function reachCell(n, nodeView) {
  if (nodeView || (reach && reach.node)) return `<span class="muted small">${t('reach.byMaster')}</span>`;
  if (!n.enabled && !n.isLocal) return '—';
  const r = resultOf(n.id);
  if (runningFor(n.id)) {
    return `<button class="reach-cell" data-act="reach.detail" data-id="${n.id}"><span class="badge primary"><span class="spin"></span> ${t('reach.running')}</span>${r ? `<div class="sub-cell">${t('reach.last')} ${esc(verdictLabel(r.verdict))}</div>` : ''}</button>`;
  }
  if (!r) return `<button class="reach-cell" data-act="reach.one" data-id="${n.id}"><span class="badge">${t('reach.never')}</span><div class="sub-cell link">${t('reach.clickToCheck')}</div></button>`;
  const chips = r.verdict === 'error' || r.verdict === 'noProbes'
    ? `<span class="muted small ellip" title="${esc(r.error || '')}">${esc(shortReason(r))}</span>`
    : (r.groups || []).filter(g => GROUPS.includes(g.key)).map(groupChip).join('');
  const warn = r.attempt ? ` <span class="warn-text small" title="${esc(t('reach.attemptFailed', { t: fmtTime(r.attempt.at), err: r.attempt.error || '' }))}">⚠</span>` : '';
  return `<button class="reach-cell" data-act="reach.detail" data-id="${n.id}" title="${esc(t('reach.openDetail'))}">
    <span class="badge ${TONE[r.verdict] || ''}">${esc(verdictLabel(r.verdict))}</span> <span class="muted small">${fmtRelative(r.at)}</span>${warn}
    <div class="rc-row">${chips}</div></button>`;
}

function shortReason(r) {
  if (r.verdict === 'noProbes') return t('reach.v.noProbes');
  const e = String(r.error || '');
  return e.length > 26 ? e.slice(0, 26) + '…' : (e || t('reach.v.error'));
}

export const verdictLabel = v => t('reach.v.' + (v || 'none'));

// 页头工具条右侧:自动巡检的节奏 + "检测全部"按钮
export function reachToolbar(nodeView) {
  if (nodeView || !reach || reach.node) return '';
  const auto = reach.auto
    ? t('reach.autoOn', { m: reach.minutes, next: reach.nextRun ? fmtTime(reach.nextRun) : '—' })
    : t('reach.autoOff');
  return `<span class="muted small reach-sum" title="${esc(t('reach.autoHint'))}">${esc(auto)} · <a href="#/settings">${t('reach.settings')}</a></span>
    <button class="btn" data-act="reach.all" ${reach.running ? 'disabled' : ''}>${reach.running ? `<span class="spin"></span> ${t('reach.running')}` : t('reach.checkAll')}</button>`;
}

function explain(r) {
  if (r.verdict === 'partial') {
    const down = (r.groups || []).filter(g => GROUPS.includes(g.key) && g.state === 'down').map(g => t('reach.g.' + g.key));
    return t('reach.x.partial', { down: down.join(t('reach.sep')) });
  }
  if (r.verdict === 'error') return t('reach.x.error', { err: r.error || '' });
  return t('reach.x.' + r.verdict);
}

function portText(r) {
  if (!r.port) return t('reach.port.none');
  if (r.portFrom === 'line') return t('reach.port.line', { port: r.port, name: r.portLine || '' });
  if (r.portFrom === 'sub') return t('reach.port.sub', { port: r.port });
  if (r.portFrom === 'api') return t('reach.port.api', { port: r.port });
  return String(r.port);
}

function groupBlock(g) {
  const name = t('reach.g.' + g.key) + (g.key === 'hk' ? ` <span class="muted small">${t('reach.control')}</span>` : '');
  const stateBadge = !g.total ? `<span class="badge">${t('reach.noProbe')}</span>`
    : `<span class="badge ${{ ok: 'ok', weak: 'warn', down: 'danger' }[g.state] || ''}">${t('reach.okOf', { ok: g.ok, total: g.total })}</span>`;
  const probes = (g.probes || []).map(p => `<li><span class="ellip">${esc(p.city || '?')} · ${esc(p.network || '')} <span class="muted">AS${p.asn}</span></span>
      <span class="${p.offline ? 'muted' : p.ok ? 'ok-text' : 'danger-text'}">${p.offline ? t('reach.offline') : p.ok ? (p.avgMs ? Math.round(p.avgMs) + ' ms' : '✓') : t('reach.fail')}</span></li>`).join('');
  return `<div class="reach-g">
      <div class="reach-g-head"><span class="reach-g-name">${name}</span>${stateBadge}<span class="muted small num">${g.ok && g.avgMs ? Math.round(g.avgMs) + ' ms' : ''}</span></div>
      ${probes ? `<details><summary>${t('reach.probes', { n: (g.probes || []).length })}</summary><ul class="reach-probes">${probes}</ul></details>` : ''}
    </div>`;
}

function historyStrip(id) {
  const h = ((reach && reach.history) || {})[id] || [];
  if (!h.length) return '';
  return `<section><h3>${t('reach.history', { n: h.length })}</h3><div class="reach-hist">${h.map(p =>
    `<i class="${TONE[p.verdict] || 'none'}" title="${esc(fmtTime(p.at) + ' · ' + verdictLabel(p.verdict) + ' · ' + t(p.auto ? 'reach.auto' : 'reach.manual'))}"></i>`).join('')}</div></section>`;
}

function detailHTML(n) {
  const r = resultOf(n.id);
  const running = runningFor(n.id);
  const q = reach && reach.quota;
  const head = r ? `${r.attempt ? `<p class="hint warn-text" style="margin:0 0 .6rem">${esc(t('reach.attemptFailed', { t: fmtTime(r.attempt.at), err: r.attempt.error || '' }))}</p>` : ''}<div class="reach-hero ${TONE[r.verdict] || ''}">
      <div class="v">${esc(verdictLabel(r.verdict))}${r.rechecked ? ` <span class="badge">${t('reach.rechecked')}</span>` : ''}</div>
      <p>${esc(explain(r))}</p>
    </div>
    <dl class="kv reach-meta">
      <dt>${t('reach.target')}</dt><dd class="mono">${esc(r.target || '—')}${r.port ? ':' + r.port : ''}</dd>
      <dt>${t('reach.portFrom')}</dt><dd>${esc(portText(r))}</dd>
      <dt>${t('reach.at')}</dt><dd>${fmtTime(r.at)} · ${t(r.auto ? 'reach.auto' : 'reach.manual')}</dd>
    </dl>${r.portFrom === 'sub' || r.portFrom === 'api' || !r.port ? `<p class="hint">${t(r.port ? 'reach.noTcpLine' : 'reach.pingOnly')}</p>` : ''}` : `<p class="hint">${t('reach.neverHint')}</p>`;
  const groups = r && r.verdict !== 'error' ? `<section><h3>${t('reach.byCarrier')}</h3>${(r.groups || []).map(groupBlock).join('')}</section>` : '';
  const icmp = r && r.icmp ? `<p class="hint">${esc(t('reach.icmp', { cn: `${r.icmp.cnOk}/${r.icmp.cnTotal}`, hk: `${r.icmp.hkOk}/${r.icmp.hkTotal}` }))}</p>`
    : r && r.icmpError ? `<p class="hint">${esc(t('reach.icmpFail', { err: r.icmpError }))}</p>` : '';
  const quota = q ? `<p class="hint">${esc(t('reach.quota', { r: q.remaining, l: q.limit, n: reach.perCheck || 15 }))}${q.resetAt ? ' · ' + esc(t('reach.quotaReset', { t: fmtTime(q.resetAt) })) : ''}</p>` : '';
  return `${head}
    <div class="row" style="margin:.8rem 0 1rem"><button class="btn primary" data-act="reach.one" data-id="${n.id}" ${running || (reach && reach.running) ? 'disabled' : ''}>${running ? `<span class="spin"></span> ${t('reach.running')}` : t('reach.checkNow')}</button></div>
    ${groups}${icmp}${historyStrip(n.id)}
    <section><p class="hint">${t('reach.note')}</p>${quota}${reach && reach.error ? `<p class="hint danger-text">${esc(reach.error)}</p>` : ''}</section>`;
}

let nodesRef = () => [];
export function setNodes(fn) { nodesRef = fn; }

function openDetail(id) {
  const n = nodesRef().find(x => x.id === id);
  if (!n) return;
  detailId = id;
  const html = detailHTML(n);
  openDrawer(esc(t('reach.drawerTitle', { name: n.name })), html);
  const body = document.getElementById('drawer-body');
  if (body) { body.__muiHTML = html; delete body.dataset.reachSig; }
}

// 刷新时抽屉开着就原地更新(内容没变不动 DOM,展开的明细不会合上)
export function refreshDetail() {
  if (!detailId || !drawerOpen()) { detailId = 0; return; }
  const n = nodesRef().find(x => x.id === detailId);
  if (!n) return;
  const body = document.getElementById('drawer-body');
  if (body && body.querySelector('details[open]')) {
    // 有展开的明细:只在结果真的变了时重画
    const r = resultOf(detailId);
    const sig = r ? r.at + r.verdict + runningFor(detailId) : String(runningFor(detailId));
    if (body.dataset.reachSig === sig) return;
    body.dataset.reachSig = sig;
  }
  setHTML(body, detailHTML(n));
}

async function run(nodeId, btn) {
  if (btn) btn.disabled = true;
  try {
    await post('reach/run', { nodeId: nodeId || 0 });
    toast(t('reach.started'), 'ok');
  } catch (e) { toast(e.message, 'err'); }
  await loadReach();
  onChange();
  document.dispatchEvent(new CustomEvent('mui:acted'));
}

registerActions({
  'reach.all': (_, btn) => run(0, btn),
  'reach.one': (id, btn) => run(Number(id), btn),
  'reach.detail': id => openDetail(Number(id)),
});

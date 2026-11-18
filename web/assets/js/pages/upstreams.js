import { state, load, takeHashParams } from '../app.js';
import { get, post, put, del, SLOW, LONG } from '../api.js';
import { t } from '../i18n.js';
import { trErr } from '../errmsg.js';
import { esc, toast, confirm, openModal, closeModal, registerActions, badge, dot, field, check, empty, fv, fchk, matches, debounce, setHTML, fmtDate, fmtRelative, chipList, fieldBox } from '../ui.js';
import { upstreamBad } from '../issues.js';
// 展开着逐台明细的上游 id(数字):平时一格只有状态点和一句结论,点开才逐台列出来(见 detailHTML)
const expanded = new Set();

export const title = () => t('up.title');
export const subtitle = () => t('up.subtitle');

const TYPES = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', anytls: 'AnyTLS', tuic: 'TUIC', hysteria2: 'Hysteria2', shadowsocks: 'Shadowsocks', socks: 'SOCKS5' };
const SS_METHODS = ['aes-256-gcm', 'aes-128-gcm', 'chacha20-ietf-poly1305', '2022-blake3-aes-128-gcm', '2022-blake3-aes-256-gcm', '2022-blake3-chacha20-poly1305', 'none'];
const FPS = ['', 'chrome', 'firefox', 'safari', 'ios', 'android', 'edge', 'random'];
const FORM_KEYS = {
  vless: ['server', 'server_port', 'uuid', 'flow', 'tls', 'transport'],
  vmess: ['server', 'server_port', 'uuid', 'alter_id', 'security', 'tls', 'transport'],
  trojan: ['server', 'server_port', 'password', 'tls', 'transport'],
  anytls: ['server', 'server_port', 'password', 'tls'],
  tuic: ['server', 'server_port', 'uuid', 'password', 'congestion_control', 'udp_relay_mode', 'zero_rtt_handshake', 'udp_over_stream', 'tls'],
  hysteria2: ['server', 'server_port', 'password', 'tls', 'up_mbps', 'down_mbps', 'obfs'],
  shadowsocks: ['server', 'server_port', 'method', 'password'],
  socks: ['server', 'server_port', 'version', 'username', 'password'],
};
const HAS_TRANSPORT = { vless: 1, vmess: 1, trojan: 1 };
const GROUP = 'urltest';
const results = {};
let query = '';
let onlyBad = false; // 只看故障:在真正用它的服务器上有失败的(和概况「需要处理」同一口径)

const parseOpts = o => { try { return typeof o === 'string' ? JSON.parse(o) : (o || {}); } catch { return {}; } };
const sel = (id, opts, cur, labels = {}) => `<select id="${id}">${opts.map(x => `<option value="${x}" ${cur === x ? 'selected' : ''}>${esc(labels[x] ?? x)}</option>`).join('')}</select>`;

export async function render(el) {
  const hp = takeHashParams('upstreams'); // 概况的「查看全部」:#/upstreams?f=bad
  if (hp.has('f')) { onlyBad = hp.get('f') === 'bad'; query = ''; }
  await load('nodes').catch(() => {}); // 线路上游要显示服务器名
  el.innerHTML = `
    <div class="toolbar">
      <input type="search" id="up-q" placeholder="${t('common.search')}…" value="${esc(query)}">
      <label class="small muted" style="display:inline-flex;align-items:center;gap:.3rem"><input type="checkbox" id="up-onlybad" ${onlyBad ? 'checked' : ''}> ${t('up.onlyBad')}</label>
      <span class="muted small" id="up-count"></span>
      <span class="grow"></span>
      <button class="btn" id="btn-test-all" data-act="up.testAll">${t('common.testAll')}</button>
      <details class="menu"><summary class="btn primary">${t('up.add')}</summary><div class="menu-list">
        <button data-act="up.add"><b>${esc(t('up.menu.manual'))}</b><span class="muted small" style="display:block">${esc(t('up.menu.manualHelp'))}</span></button>
        <button data-act="up.addLine"><b>${esc(t('up.addFromLine'))}</b><span class="muted small" style="display:block">${esc(t('up.menu.lineHelp'))}</span></button>
        <button data-act="up.addGroup"><b>${esc(t('up.addGroup'))}</b><span class="muted small" style="display:block">${esc(t('up.menu.groupHelp'))}</span></button>
      </div></details>
    </div>
    <div class="table-wrap fill"><table class="grid" data-fit="p3:980 cards:820">
      <thead><tr><th>${t('common.name')}</th><th>${t('common.type')}</th><th>${t('common.server')}</th><th class="c-p3">${t('up.linesUsing')}</th><th>${t('up.latency')}</th><th></th></tr></thead>
      <tbody id="up-body"></tbody>
    </table></div>`;
  document.getElementById('up-q').addEventListener('input', debounce(e => { query = e.target.value; renderRows(); }));
  document.getElementById('up-onlybad').addEventListener('change', e => { onlyBad = e.target.checked; renderRows(); });
  renderRows();
  // 用定时巡检结果填充尚未手动测试过的行
  try {
    const h = await get('upstreams/health');
    (h.results || []).forEach(x => { if (!results[x.id] || results[x.id].scheduled) results[x.id] = { ...x, scheduled: true }; });
    renderRows();
  } catch {}
  // 巡检结果会随定时刷新更新,不必自己再开一个定时器
}

// resultHTML 延迟列:一条上游一行,里面按服务器并排 —— "专线倍率 128ms · 高带宽 96ms"。
// 上游通不通只在真正跑这条线路的机器上量才算数,所以这里显示的是各机各自的结果。
const stateText = sv => (sv.state === 'ok' ? sv.delayMs + ' ms' : sv.state === 'fail' ? t('up.fault') : sv.state === 'stale' ? t('up.stale') : t('up.pending'));
const stateCls = sv => (sv.state === 'ok' ? ' ok' : sv.state === 'fail' ? ' bad' : sv.state === 'stale' ? ' warn' : '');
function resultHTML(id) {
  id = Number(id);
  const r = results[id];
  const idle = (label, cls = '') => `<div class="hsum"><span class="hdots"><i class="hd${cls}"></i></span><span class="muted">${esc(label)}</span></div>`;
  const inCard = detailHTML(id, 'in-card'); // 卡片排法时明细画在卡片里;表格排法时这一份藏着,看的是下面那条通栏
  if (!r) return idle(t('up.untested'));
  if (r.testing) return idle(t('up.testing'), ' busy') + inCard;
  if (r.unused) return idle(t('up.unused')) + `<div class="sub-cell muted">${t('up.unusedHint')}</div>`;
  const servers = r.servers || [];
  if (!servers.length) return idle(t('up.untested'));
  // 这一格要回答的是「通不通、多快」:在用它的服务器每台一个状态点(绿 = 通、红 = 不通;悬停看是哪台、多少毫秒或什么错),
  // 后面跟一句结论;只有一台时把服务器名直接写在后面(是哪台测出来的要一眼看到,不能只放在悬停提示里)。
  // 点这一行任何地方(点、字、箭头都行)展开逐台的结果(见 detailHTML);一台也能点开,不用记哪种能点哪种不能。
  // 以前是「服务器名 + 故障」的标签一个接一个排开,三台就占满一格
  const dots = servers.slice(0, 16).map(sv => `<i class="hd${stateCls(sv)}" title="${esc(sv.name + ' · ' + stateText(sv) + (sv.error ? ' · ' + trErr(sv.error) : ''))}"></i>`).join('')
    + (servers.length > 16 ? `<span class="muted small">+${servers.length - 16}</span>` : '');
  const ok = servers.filter(sv => sv.state === 'ok'), fail = servers.filter(sv => sv.state === 'fail'), stale = servers.filter(sv => sv.state === 'stale');
  const delays = ok.map(sv => sv.delayMs).sort((a, b) => a - b);
  let kind = '', text = t('up.pending');
  if (ok.length === servers.length) { kind = 'ok'; text = delays.length === 1 || delays[0] === delays[delays.length - 1] ? delays[0] + ' ms' : t('up.h.allOk', { a: delays[0], b: delays[delays.length - 1] }); }
  else if (!ok.length && fail.length) { kind = 'bad'; text = servers.length === 1 ? t('up.fault') : t('up.h.allFail', { n: servers.length }); }
  else if (fail.length || stale.length) { kind = 'warn'; text = t('up.okOf', { n: ok.length, m: servers.length }) + (delays.length ? ' · ' + delays[0] + ' ms' : ''); }
  const tcp = ok.length && ok.every(sv => sv.method === 'tcp') ? ' ' + badge('TCP') : '';
  const via = viaLine(servers);
  const open = expanded.has(id);
  const one = servers.length === 1 ? `<span class="hname" title="${esc(t('up.testedOn', { name: servers[0].name }))}">${esc(servers[0].name)}${servers[0].isLocal ? ` <span class="muted">${t('node.local')}</span>` : ''}</span>` : '';
  // 结论、服务器名和箭头包在一起不拆开:点多了要折行时,整句一起到下一行,不会剩一个箭头孤零零掉下去
  const inner = `<span class="hdots">${dots}</span><span class="hconc"><b class="${kind}">${esc(text)}</b>${one}${tcp}<i class="i i-chev${open ? ' open' : ''}"></i></span>`;
  // 没测通的原因不在页面上占地方(它只是「这次没测通」的技术细节):鼠标停在结论上看 —— 每台一行「服务器 · 原因」。
  // 以前折叠时结论下面还挂一行报错预览、展开后每台再列一遍;用户:「这不就是测试 ping 失败吗,至于非要明显列出吗」
  const bad = servers.filter(sv => sv.error);
  const tip = t('up.detail') + (bad.length ? '\n' + bad.slice(0, 8).map(sv => sv.name + ' · ' + trErr(sv.error).slice(0, 160)).join('\n') : '');
  const sum = `<button type="button" class="hsum hbtn" data-act="up.expand" data-id="${id}" title="${esc(tip)}" aria-expanded="${open}">${inner}</button>`;
  // 从外部节点加来的:最近一次尝试跟随的结论,一句话放在这一格里(它解释的正是这一格的状态)
  const up = state.upstreams.find(x => x.id === id), fn = up ? followNote(up.source) : null;
  const follow = fn ? `<div class="sub-cell up-note${fn.cls}" title="${esc(fn.tip || '')}">${esc(fn.text)}</div>` : '';
  // 上游组那句「现在走 X」:展开后每台那一行自己写着(→ 成员),这一句不再重复
  return `${sum}${open ? '' : via}${inCard}${follow}`;
}

// detailHTML 展开后的逐台明细:每台服务器固定一行 ——「● 服务器名  结果  多久前查的」,上游组再多一格「→ 现在走的成员」。
// 四格各有固定的位置(名字一格最宽 10rem,放不下省略号;结果、时间两格定宽),所以多台上下排时结果对得齐;
// 结果紧跟在名字后面(试过把结果推到这一栏最右边:并排几栏时它离下一台的圆点比离自己的名字还近,看着像是下一台的)。
// 三台以内在表格排法里总是并排成一行,用不着对齐,各格按内容收紧(.few)。
// 不管窗口多宽、有几台,一台永远只占一行。
// 放在哪由排法决定,不放在「状态」那一格里撑(同一段 HTML 画两处,CSS 按排法只露一处):
//   表格排法 → 这一行下面的一条通栏(tr.up-sub,跨满整张表;够宽就并排几栏,三台一行排完,十几台也只占三四行);
//   卡片排法 → 这张卡片自己里面(where = 'in-card',状态那一格下面)。
// 以前明细画在「状态」那一格里。那一列多宽取决于窗口多宽、别的列占多少,880–1000 宽时只剩两百多像素:
// 先是「检查于 11/17 02:20:00」被挤成一个字一行;改成按格宽分行后又成了每台三四行(名字、时间、报错各占一行)。
// 用户三次截图指出 —— 根子是不该去赌那一格的宽度。
// 故障原因不列出来:鼠标停在那一行上看(title,连同检查时间);触屏没有悬停,点一下那一行(它能拿到焦点)才在下面露出来,
// 用的是 CSS :focus,不用记状态。什么时候查的写成「3 分钟前」:要知道的只是结果新不新。
function detailHTML(id, where = '') {
  const r = results[id];
  const servers = (r && !r.unused && r.servers) || [];
  if (!expanded.has(id) || !servers.length) return '';
  const viaOf = sv => (String(sv.method || '').startsWith('via:') ? t('up.groupNow', { name: sv.method.slice(4) }) : '');
  const grp = servers.some(viaOf);
  // 名字那一格给多宽:按这条上游里最长的名字估(汉字按一个字宽、其余按 0.62 个字宽,再多留一个字),最多 12.5 个字宽(= 10rem)。
  // 名字都短的时候结果就紧挨着名字,不会因为统一留 10rem 而隔出一大段空;估大了无非空一点,估不小(留了余量,真放不下还有省略号兜底)
  const nameEm = s => [...s].reduce((w, ch) => w + (/[⺀-鿿가-힯豈-﫿＀-￯]/.test(ch) ? 1 : 0.62), 0);
  const hname = Math.min(12.5, Math.ceil(Math.max(...servers.map(sv => nameEm(sv.name + (sv.isLocal ? ' ' + t('node.local') : '')))) + 1));
  const rows = servers.map(sv => {
    const via = viaOf(sv), why = sv.error ? trErr(sv.error).slice(0, 300) : '', c = stateCls(sv).trim();
    const at = sv.checkedAt ? t('up.checkedAt') + ' ' + fmtDate(sv.checkedAt) : '';
    const name = `<span class="nm"${why ? '' : ` title="${esc(sv.name)}"`}>${esc(sv.name)}${sv.isLocal ? ` <span class="muted">${t('node.local')}</span>` : ''}</span>`;
    // 每一格都画(没有内容就空着):各行的格数一样,位置才对得上
    const when = `<span class="hwhen"${why || !at ? '' : ` title="${esc(at)}"`}>${sv.checkedAt ? esc(fmtRelative(sv.checkedAt)) : ''}</span>`;
    const viaCell = grp ? `<span class="hvia"${via ? ` title="${esc(via)}"` : ''}>${via ? '→ ' + esc(sv.method.slice(4)) : ''}</span>` : '';
    const row = `<div class="hrow"><i class="hd${stateCls(sv)}"></i>${name}<b${c ? ` class="${c}"` : ''}>${esc(stateText(sv))}</b>${viaCell}${when}</div>`;
    return why
      ? `<div class="hitem why" tabindex="0" title="${esc(t('up.why', { msg: why }) + (at ? '\n' + at : ''))}">${row}<div class="hwhy">${esc(why)}</div></div>`
      : `<div class="hitem">${row}</div>`;
  }).join('');
  return `<div class="hdetail${where ? ' ' + where : ''}${grp ? ' grp' : ''}${servers.length <= 3 ? ' few' : ''}${r.testing ? ' busy' : ''}" style="--hname:${hname}em">${rows}</div>`;
}

// viaLine 上游组在各服务器上此刻走的成员,合成一行:都走同一条就只写一次「现在走 X」(以前每台后面各写一遍,
// 三台就重复三遍、把这一格撑成好几行);各台不一样时按成员归并「现在走 X(2 台)· Y(1 台)」,哪几台放在悬停提示里。
// 展开看逐台时每台那一行自己写着,这里不再重复。
function viaLine(servers) {
  const by = new Map();
  servers.forEach(sv => {
    const m = String(sv.method || '');
    if (sv.state !== 'ok' || !m.startsWith('via:')) return;
    const name = m.slice(4);
    if (!by.has(name)) by.set(name, []);
    by.get(name).push(sv.name);
  });
  if (!by.size) return '';
  const groups = [...by].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]));
  if (groups.length === 1) {
    return `<div class="sub-cell muted" title="${esc(groups[0][1].join('、'))}">${esc(t('up.groupNow', { name: groups[0][0] }))}</div>`;
  }
  const shown = groups.slice(0, 3).map(([name, who]) => `<span title="${esc(who.join('、'))}">${esc(name)} ${esc(t('up.viaServers', { n: who.length }))}</span>`);
  const rest = groups.length - shown.length;
  return `<div class="sub-cell muted">${esc(t('up.groupNow', { name: '' }).trim())} ${shown.join(' · ')}${rest > 0 ? ` · +${rest}` : ''}</div>`;
}

// 线路上游:写明是哪条线路、哪台服务器
const relayNames = u => {
  const l = state.lines.find(x => x.id === u.fromLine), n = (state.nodes || []).find(x => x.id === u.fromNode);
  return (l ? l.name : '#' + u.fromLine) + ' @ ' + (n ? n.name : '#' + u.fromNode);
};

function typeBadges(u) {
  const o = parseOpts(u.options);
  if (u.type === GROUP) return badge(t('up.group'), 'primary');
  let h = u.fromLine ? badge(t('up.fromLineBadge'), 'primary') + ' ' + badge(TYPES[u.type] || u.type) : badge(TYPES[u.type] || u.type);
  if (o.tls && o.tls.reality && o.tls.reality.enabled) h += ' ' + badge('Reality', 'ok');
  else if (o.tls && o.tls.enabled) h += ' ' + badge('TLS');
  if (o.transport && o.transport.type) h += ' ' + badge(o.transport.type);
  return h;
}

// upstreamLines 每条上游被哪些线路用到(上游 id → 线路名的集合):线路自己选的上游 + 分流规则指向的,
// 和后端判断「有没有线路在用」同一口径(render.LineUpstreams)。以前只数自己选的:只被分流规则用到的上游这一列写 0,
// 延迟那一格却有服务器在测,删的时候后端又说有线路在用。线路用上游组的,组的成员也算在用。
function upstreamLines() {
  const by = new Map();
  const add = (id, name) => { if (!id) return; if (!by.has(id)) by.set(id, new Set()); by.get(id).add(name); };
  state.lines.forEach(l => {
    add(l.upstreamId, l.name);
    let rr = l.routeRules;
    if (typeof rr === 'string') { try { rr = JSON.parse(rr); } catch { rr = []; } }
    (Array.isArray(rr) ? rr : []).forEach(r => { if (Number(r.to) > 0) add(Number(r.to), l.name); });
  });
  state.upstreams.filter(u => u.type === GROUP).forEach(g => {
    const mine = by.get(g.id);
    if (!mine) return;
    (parseOpts(g.options).outbounds || []).forEach(name => {
      const m = state.upstreams.find(x => x.name === name);
      if (m) mine.forEach(ln => add(m.id, ln));
    });
  });
  return by;
}

// 从外部节点加来的上游,界面上分三处,每处只放一行(试过把来源、结论、按钮全堆在名字下面:名称列窄,折成五行,一行顶别人三行高):
//   名字下面:一行很短的来源「↻ 订阅名 · 节点名」,放不下就省略号;点它开一个小框看详情;
//   状态那一列:最近一次尝试跟随的结论,一句话(跟着改过 / 订阅里没变 / 找不到对应节点),细节放悬停提示和小框里;
//   小框里:完整的说明、上一次的结论和改了什么、「不再跟随」按钮。
// 「没变」「没抓下来」这类结论只在一天内显示,「跟着改过」显示一周:过了这段时间就只是历史,不占地方。
const followAge = at => (at ? Date.now() / 1000 - at : Infinity);
function followNote(s, long = false) {
  if (!s || !s.note || s.stop) return null; // 停止跟随的不再挂以前的结论:那是停下之前的事
  if (s.note === 'followed') {
    if (!long && followAge(s.followedAt) >= 7 * 86400) return null;
    const detail = s.detail || t('up.fn.params');
    return { cls: ' ok', text: long ? t('up.fn.followedLong', { when: fmtRelative(s.followedAt), detail }) : t('up.fn.followed', { when: fmtRelative(s.followedAt) }), tip: detail };
  }
  if (!['unchanged', 'gone', 'ambiguous', 'fetch', 'invalid'].includes(s.note)) return null;
  if (!long && followAge(s.triedAt) >= 86400) return null;
  const tip = s.detail ? trErr(s.detail) : '';
  return { cls: s.note === 'unchanged' ? '' : ' warn', text: t('up.fn.' + s.note) + (long ? ' · ' + fmtRelative(s.triedAt) + (tip ? ' · ' + tip : '') : ''), tip };
}
// 名字下面那一行来源:跟随着是「↻ 订阅名 · 节点名」;停了跟随的去掉转圈的图标、写明「已停止跟随」,一眼分得出。两种都能点开。
function sourceHTML(u) {
  const s = u.source;
  if (!s) return '';
  const ext = s.extName || '#' + s.extId;
  const tip = t('up.follow', { ext, node: s.node });
  if (s.stop) return `<button type="button" class="up-srcline off" data-act="up.followInfo" data-id="${u.id}" title="${esc(t('up.followStopped') + ' · ' + tip)}"><span>${esc(t('up.followStopped') + ' · ' + ext)}</span></button>`;
  return `<button type="button" class="up-srcline" data-act="up.followInfo" data-id="${u.id}" title="${esc(tip)}"><i class="i i-refresh"></i><span>${esc(ext + ' · ' + s.node)}</span></button>`;
}
// 跟随是一个开关(用户 11-17:「点击了不在跟随 可以保留现在上游信息 只是不在跟随订阅自动跟随新配置 并且可以重新打开跟随」):
// 关掉不是把来源删掉 —— 上游现在的配置原样留着,只是不再自动换,随时能再打开。点了就存,不用再点保存。
function followInfo(u) {
  const s = u.source, n = followNote(s, true);
  const state2 = s.stop === 'edited' ? `<p class="hint warn">${esc(t('up.follow.stopEdited'))}</p>`
    : s.stop ? `<p class="hint">${esc(t('up.follow.stopUser'))}</p>`
    : n ? `<p class="up-note${n.cls}">${esc(t('up.followLast'))} ${esc(n.text)}</p>` : '';
  openModal(t('up.followTitle'), `<p><b>${esc(t('up.follow', { ext: s.extName || '#' + s.extId, node: s.node }))}</b></p>
    <label class="sw-row up-follow-sw"><div><b>${esc(t('up.followSwitch'))}</b><small>${esc(t('up.followHelp'))}</small></div>
      <span class="switch"><input type="checkbox" id="up-follow" data-change="up.followSet" data-id="${u.id}" ${s.stop ? '' : 'checked'}><span></span></span></label>
    ${state2}`,
  null, { cancelText: t('common.close') });
}

// upstreamUsage 这条上游现在被谁用着(删之前给人看):直接把它当出站的线路、分流规则指向它的线路、把它当成员的上游组。
// 和后端删上游时的拦截同一口径(lineNamesUsingUpstream + groupsWith):这里先说清楚,不让人点了「删除」才吃一个报错。
function upstreamUsage(u) {
  const lines = [];
  state.lines.forEach(l => {
    let rr = l.routeRules;
    if (typeof rr === 'string') { try { rr = JSON.parse(rr); } catch { rr = []; } }
    const out = l.upstreamId === u.id, rule = (Array.isArray(rr) ? rr : []).some(r => Number(r.to) === u.id);
    if (out || rule) lines.push({ name: l.name, out, rule, off: l.enabled === false });
  });
  const groups = state.upstreams.filter(g => g.type === GROUP && g.id !== u.id && (parseOpts(g.options).outbounds || []).includes(u.name)).map(g => g.name);
  return { lines, groups };
}
function inUseDialog(u, use) {
  const row = l => `<li><span class="nm" title="${esc(l.name)}">${esc(l.name)}</span>${l.off ? `<span class="muted small">${esc(t('common.disabled'))}</span>` : ''}<span class="grow"></span>${l.out ? badge(t('up.inUse.out'), 'primary') : ''}${l.rule ? badge(t('up.inUse.rule')) : ''}</li>`;
  const html = (use.lines.length ? `<p class="confirm-text">${esc(t('up.inUse.lines', { n: use.lines.length }))}</p><ul class="up-inuse">${use.lines.map(row).join('')}</ul>` : '')
    + (use.groups.length ? `<p class="confirm-text">${esc(t('up.inUse.groups', { g: use.groups.join(t('up.inUse.sep')) }))}</p>` : '');
  // 有线路在用:主按钮带去线路页;只是在上游组里:就在这一页处理,不用按钮
  openModal(t('up.inUse.title', { name: u.name }), html, use.lines.length ? async () => { location.hash = '#/lines'; } : null,
    { saveText: t('up.inUse.go'), cancelText: t('common.close') });
}

function renderRows() {
  const body = document.getElementById('up-body');
  if (!body) return;
  const using = upstreamLines();
  const online = new Set(state.onlines.upstreams || []);
  const rows = state.upstreams.filter(u => { const o = parseOpts(u.options); return matches(query, u.name, u.type, o.server) && (!onlyBad || upstreamBad(results[u.id])); });
  document.getElementById('up-count').textContent = `${rows.length} / ${state.upstreams.length}`;
  if (!rows.length) { setHTML(body, `<tr><td colspan="6">${empty()}</td></tr>`); return; }
  setHTML(body, rows.map(u => {
    const o = parseOpts(u.options);
    // 上游组的成员可能有一二十条:收成标签、超过 3 条折起来(+N 点开),不再拼成一长串把这一列撑宽
    const srv = u.type === GROUP
      ? chipList((o.outbounds || []).map(m => `<span class="chip">${esc(m)}</span>`), { key: 'up-mem-' + u.id, max: 3 })
      : esc(o.server ? o.server + (o.server_port ? ':' + o.server_port : '') : '');
    // 展开着的:下面跟一条通栏(tr.up-sub)放逐台明细;卡片排法时这条通栏藏着,明细在卡片里(resultHTML 里那一份)
    const sub = detailHTML(u.id);
    return `<tr${sub ? ' class="up-open"' : ''}>
      <td class="primary-cell">${dot(online.has(u.name))}<span class="pname" title="${esc(u.name)}">${esc(u.name)}</span>${u.fromLine ? `<div class="sub-cell">${esc(relayNames(u))}${u.relayProblem ? ` · <span class="danger">${esc(trErr(u.relayProblem))}</span>` : ''}</div>` : ''}${sourceHTML(u)}</td>
      <td>${typeBadges(u)}</td>
      <td class="num">${srv}</td>
      <td class="num c-p3"${using.has(u.id) ? ` title="${esc([...using.get(u.id)].join('、'))}"` : ''}>${using.has(u.id) ? using.get(u.id).size : 0}</td>
      <td id="up-res-${u.id}">${resultHTML(u.id)}</td>
      <td class="actions">
        <button class="btn sm" data-act="up.test" data-id="${u.id}">${t('common.test')}</button>
        <button class="btn sm" data-act="up.edit" data-id="${u.id}">${t('common.edit')}</button>
        <button class="btn sm danger" data-act="up.del" data-id="${u.id}">${t('common.delete')}</button>
      </td></tr>${sub ? `<tr class="up-sub" id="up-sub-${u.id}"><td colspan="6">${sub}</td></tr>` : ''}`;
  }).join(''));
}

function setResult(id, r) {
  id = Number(id);
  // 重测展开着的那一条:明细先留着上一次的结果(变淡),测完再换 —— 不然通栏一收一放,下面的行跟着跳一下
  const prev = results[id];
  if (r && r.testing && prev && !prev.unused && prev.servers && prev.servers.length) r = { ...r, servers: prev.servers };
  results[id] = r;
  // 展开着的带着下面那条通栏(另一行),整表重画;其余的只换这一格
  if (expanded.has(id) || document.getElementById('up-sub-' + id)) { renderRows(); return; }
  const body = document.getElementById('up-body');
  if (setHTML('up-res-' + id, resultHTML(id)) && body) body.__muiHTML = null; // 这一格单独换过了:整表下次重画不能再拿旧的那份比对
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
    case 'anytls': return common +
      field(t('up.f.password'), `<input id="f-password" value="${esc(o.password || '')}">`) +
      tlsClientFields(o, { required: true });
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
    case 'anytls':
      o.password = fv('f-password'); setTLS({ required: true }); break;
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

// 线路编辑里部署到的服务器:空 = 全部
const lineNodes = l => {
  const v = l.nodeIds;
  let ids = [];
  try { ids = Array.isArray(v) ? v : (v ? JSON.parse(v) : []); } catch { ids = []; }
  return (state.nodes || []).filter(n => !ids.length || ids.includes(n.id));
};

// 新建 / 修改线路上游:选一条线路和它在哪台服务器上的入口。preset 从线路页点进来时带着线路
export function editRelayUpstream(id, preset) {
  const u = id ? state.upstreams.find(x => x.id === id) : { fromLine: preset || 0, fromNode: 0 };
  const lineOpts = state.lines.map(l => `<option value="${l.id}" ${u.fromLine === l.id ? 'selected' : ''}>${esc(l.name)} (${esc(l.protocol)} · ${l.port})</option>`).join('');
  const nodeOpts = lineId => {
    const l = state.lines.find(x => x.id === Number(lineId));
    return (l ? lineNodes(l) : []).map(n => `<option value="${n.id}" ${u.fromNode === n.id ? 'selected' : ''}>${esc(n.name)}</option>`).join('');
  };
  openModal(id ? t('up.edit') : t('up.addFromLine'), `
    <div class="form-grid">
      <div class="full">${field(t('up.fromLine'), `<select id="f-rline">${lineOpts}</select>`, t('up.fromLineHelp'))}</div>
      ${field(t('up.fromNode'), `<select id="f-rnode">${nodeOpts(u.fromLine || (state.lines[0] || {}).id)}</select>`, t('up.fromNodeHelp'))}
      ${field(t('common.name'), `<input id="f-name" value="${esc(u.name || '')}">`)}
    </div>`, async () => {
    const body = { name: fv('f-name').trim(), fromLine: Number(fv('f-rline')), fromNode: Number(fv('f-rnode')) };
    if (!body.fromLine || !body.fromNode) throw new Error(t('up.fromNode'));
    if (id) await put('upstreams/' + id, body); else await post('upstreams', body);
    await load('upstreams', 'lines', 'status');
    renderRows();
    toast(id ? t('up.updated') : t('up.created'), 'ok');
  });
  const lineSel = document.getElementById('f-rline'), nodeSel = document.getElementById('f-rnode'), name = document.getElementById('f-name');
  const suggest = () => { // 名字没动过就跟着选择走
    if (id && u.name) return;
    const l = state.lines.find(x => x.id === Number(lineSel.value)), n = (state.nodes || []).find(x => x.id === Number(nodeSel.value));
    if (l && n && (!name.value || name.dataset.auto === name.value)) { name.value = l.name + '@' + n.name; name.dataset.auto = name.value; }
  };
  lineSel.addEventListener('change', () => { nodeSel.innerHTML = nodeOpts(lineSel.value); suggest(); });
  nodeSel.addEventListener('change', suggest);
  suggest();
}

// 上游组:选几条上游,内核定时测,用延迟最低的那条能通的,挂了自动换
function editGroup(id) {
  const u = id ? state.upstreams.find(x => x.id === id) : {};
  const cur = new Set(parseOpts(u.options).outbounds || []);
  const others = state.upstreams.filter(x => x.type !== GROUP && x.id !== id);
  if (others.length < 2) { toast(t('up.groupNeedTwo'), 'err'); return; }
  openModal(id ? t('up.edit') : t('up.addGroup'), `
    <div class="form-grid">
      <div class="full">${field(t('common.name'), `<input id="f-name" value="${esc(u.name || '')}">`)}</div>
      <div class="full">${fieldBox(t('up.groupMembers'), `<div class="check-list">${others.map(x => `<label><input type="checkbox" class="grp-cb" value="${esc(x.name)}" ${cur.has(x.name) ? 'checked' : ''}> ${esc(x.name)}</label>`).join('')}</div>`, t('up.groupHelp'))}</div>
    </div>`, async () => {
    const outbounds = [...document.querySelectorAll('.grp-cb:checked')].map(c => c.value);
    if (outbounds.length < 2) throw new Error(t('up.groupNeedTwo'));
    const body = { name: fv('f-name').trim(), type: GROUP, options: { outbounds } };
    if (id) await put('upstreams/' + id, body); else await post('upstreams', body);
    await load('upstreams', 'lines', 'status');
    renderRows();
    toast(id ? t('up.updated') : t('up.created'), 'ok');
  });
}

function editUpstream(id) {
  if (id && (state.upstreams.find(x => x.id === id) || {}).type === GROUP) { editGroup(id); return; }
  const u = id ? state.upstreams.find(x => x.id === id) : { type: 'vless', options: {} };
  if (u.fromLine) { editRelayUpstream(id); return; }
  const o = parseOpts(u.options);
  openModal(id ? t('up.edit') : t('up.add'), `
    <div class="form-grid">
      ${id ? '' : `<div class="full">${fieldBox(t('up.import'), `<div class="row"><input id="f-link" placeholder="${t('up.importPh')}"><button type="button" class="btn" data-act="up.parse">${t('up.parse')}</button></div>`, t('up.importHelp'))}</div>`}
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
  'up.addGroup': () => editGroup(null),
  'up.addLine': () => {
    if (!state.lines.length) { toast(t('up.noLines'), 'err'); return; }
    editRelayUpstream(null);
  },
  'up.edit': id => editUpstream(Number(id)),
  'up.del': async id => {
    const u = state.upstreams.find(x => x.id === Number(id));
    if (!u) return;
    const use = upstreamUsage(u);
    if (use.lines.length || use.groups.length) { inUseDialog(u, use); return; } // 还在用:不让删,列出是谁在用(后端同样会拦)
    if (!await confirm(t('common.deleteConfirm', { name: u.name }), { danger: true, okText: t('common.delete') })) return;
    try { await del('upstreams/' + id); await load('upstreams', 'status'); renderRows(); toast(t('up.deleted'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
  'up.followInfo': id => { const u = state.upstreams.find(x => x.id === Number(id)); if (u && u.source) followInfo(u); },
  'up.followSet': async (id, el) => {
    const on = el.checked;
    el.disabled = true;
    try {
      await put(`upstreams/${id}/source`, { follow: on });
      await load('upstreams');
      renderRows();
      toast(t(on ? 'up.followOn' : 'up.followOff'), 'ok');
      const u = state.upstreams.find(x => x.id === Number(id));
      if (u && u.source && !document.getElementById('modal').hidden && document.getElementById('up-follow')) followInfo(u); // 框还开着:换成新的状态说明
    } catch (e) { el.checked = !on; el.disabled = false; toast(e.message, 'err'); }
  },
  'up.expand': (id, _el, e) => {
    const n = Number(id);
    expanded.has(n) ? expanded.delete(n) : expanded.add(n);
    renderRows();
    // 整表重画后原来那个按钮换成了新的。用键盘按出来的(click 事件的 detail 是 0)把焦点还给新按钮,接着按回车能收起;
    // 鼠标点的不用还 —— 还了反倒在按钮外面留一圈焦点框
    const b = e && e.detail === 0 ? document.querySelector(`#up-res-${n} button.hbtn`) : null;
    if (b) b.focus({ preventScroll: true });
  },
  'up.test': async id => {
    setResult(id, { testing: true });
    try { setResult(id, await post(`upstreams/${id}/test`, undefined, SLOW)); }
    catch (e) { setResult(id, { servers: [{ name: '—', state: 'fail', error: e.message }] }); }
  },
  'up.testAll': async (_, btn) => {
    btn.disabled = true;
    state.upstreams.forEach(u => setResult(u.id, { testing: true }));
    try {
      const rs = await post('upstreams/test', undefined, LONG);
      rs.forEach(r => setResult(r.id, r));
      const bad = rs.filter(r => (r.servers || []).some(sv => sv.state === 'fail')).length;
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

// 流量奖励计划。顶栏(圣诞树左边)一枚金币:整枚硬币从灰一点点灌成金色 —— 灌满就是又跑满 1 TB、得 1 积分,
// 然后回到灰色重新灌;右上角的小数字是已经拿到的积分。没有外圈,进度就是硬币自己。
// 点开是一张「活动页」,有自己的一套样子(白底、留白、细线,金色只在硬币和钱数上),不跟面板的版式走:
//
//   头部    一句话说清玩法;当前奖池(大数字)、1 积分约值多少、已登记多少;「捐赠奖池」入口和离发放日的倒计时;
//           右边是那枚大金币 —— 就是我自己的进度。
//   我的    一条:积分、真实流量、离下一个积分还差多少、登记的 Solana 地址(没登记就是登记入口)。
//   四页    奖池明细 —— 奖池从哪来:一条按来源分两段的色条 + 两行(订单的 40%、留名捐赠)+ 合计;逐笔入账(数额、链、付款地址、留名),可筛;
//           积分榜   —— 已登记的地址和积分,每页十条 + 按地址查;被系统剔除的照样列着、写明原因;
//           捐赠榜   —— 上面是捐赠:填「留名 + 金额」,给出要转的数和 60 分钟的期限,期限内这个数到账就计入奖池、以这个名字上榜。
//                       只有这样捐的才计入奖池(没登记就往地址转的钱对不上号,不算),所以奖池地址只在这一步给出。下面是按合计排的榜;
//           规则     —— 五条,外加「会上传什么」。
//
// 轻提醒:刚装好的面板第一次进来、以及没登记时每满 1 TB,金币下面冒一个小气泡(一句话 + 两个按钮),不是弹框,点「以后再说」就收起。
// 设置里关掉这个功能后金币不显示(本机照样累计,重新打开不丢)。
// 数据都从面板自己的接口来(GET reward、reward/ledger、reward/board、reward/donors):面板去读公开账本,这里不直接连外面。
// 版式按活动页自己的宽度排(容器查询),不看窗口:面板的侧栏、浏览器的窄窗格都不会把它挤坏。
import { get, post, rewardQrUrl } from './api.js';
import { t, getLang } from './i18n.js';
import { trErr } from './errmsg.js';
import { esc, fmtBytes, toast, registerActions, copy, setHTML, debounce, closeMenus } from './ui.js';

let st = null;            // GET reward 的回复;null = 还没拿到
let slot = null;          // 顶栏里的位置
let timer = 0, cdTimer = 0, fastTimer = 0;
let tipHidden = false;    // 这一次提醒已经点过「以后再说」
let isOpen = false;
let tab = 'pool';
let bindOpen = false, bindBusy = false, bindErr = '';
let donBusy = false, donErr = ''; // 捐赠:提交中 / 表单的报错
let lastFocus = null, lastPoints = -1;
const led = { kind: '', chain: '', page: 1, data: null, loading: false };
const brd = { q: '', page: 1, data: null, loading: false };
const dnr = { page: 1, data: null, loading: false };
const PAGE = 10;
const SUB_URL = 'https://godusevpn.org/subscription/';
const QUICK = [5, 10, 20, 50, 100];

const TB = () => (st && st.tb) || 1099511627776;
const uid = (() => { let n = 0; return () => 'rw' + (++n); })();
const reduced = () => { try { return matchMedia('(prefers-reduced-motion: reduce)').matches; } catch { return false; } };

// ---- 数字和地址怎么写 ----
const nf = (n, d = 2) => Number(n || 0).toLocaleString(getLang() === 'zh' ? 'zh-CN' : 'en-US', { minimumFractionDigits: d, maximumFractionDigits: d });
const usd = n => nf(n, 2);
// 留名捐赠要转的确切金额:带零头(到第 3 位小数),一位都不能四舍五入掉;不加千分位,好让人照着敲、照着贴
const exactAmt = v => { const s = Number(v || 0).toFixed(6).replace(/0+$/, ''); return s + '0'.repeat(Math.max(0, 2 - (s.length - 1 - s.indexOf('.')))); };
// 被剔除的原因:登记服务给了规则代号、而且这一版认识它,就按界面的语言说;不然原样显示它写的那句
const why = x => { const k = 'rw.why.' + ((x && x.rule) || '-'), s = t(k); return s !== k ? s : (x && x.reason) || ''; };
const cap = s => s ? s[0].toUpperCase() + s.slice(1) : s;
const pts = n => Number(n || 0).toLocaleString('en-US');
const short = (a, n = 4) => { a = String(a || ''); return a.length > n * 2 + 3 ? a.slice(0, n) + '…' + a.slice(-n) : a; };
const p2 = x => String(x).padStart(2, '0');
const dayOf = ts => { const d = new Date(ts * 1000); return `${d.getFullYear()}‑${p2(d.getMonth() + 1)}‑${p2(d.getDate())}`; }; // 连字符用不断行的那个:窄屏上日期不会被拆成两截
const timeOf = ts => { const d = new Date(ts * 1000); return `${dayOf(ts)} ${p2(d.getHours())}:${p2(d.getMinutes())}`; };
const validSol = a => /^[1-9A-HJ-NP-Za-km-z]{32,44}$/.test(a);
const CHAIN = { solana: 'Solana', tron: 'TRON' };
const addrUrl = (chain, a) => chain === 'tron' ? `https://tronscan.org/#/address/${encodeURIComponent(a)}` : `https://solscan.io/account/${encodeURIComponent(a)}`;
const txUrl = (chain, tx) => chain === 'tron' ? `https://tronscan.org/#/transaction/${encodeURIComponent(tx)}` : `https://solscan.io/tx/${encodeURIComponent(tx)}`;
// 文案里的 [[sub]] 换成「godusevpn 节点订阅」的链接(其余照常转义)
const withSub = s => esc(s).replace('[[sub]]', `<a class="rw-link in" href="${SUB_URL}" target="_blank" rel="noopener">${esc(t('rw.sub'))}</a>`);

// ---- 图标 ----
const SOL = () => { const id = uid(); return `<svg class="rw-chain" viewBox="0 0 20 16" aria-hidden="true"><defs><linearGradient id="${id}" x1="0" y1="16" x2="20" y2="0" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="#9945ff"/><stop offset=".5" stop-color="#5497d5"/><stop offset="1" stop-color="#14f195"/></linearGradient></defs><path fill="url(#${id})" d="M3.9 11.9a.6.6 0 0 1 .4-.2h15.2c.3 0 .4.3.2.5l-3 3.1a.6.6 0 0 1-.4.2H1.1c-.3 0-.4-.3-.2-.5l3-3.1Zm0-11.4a.6.6 0 0 1 .4-.2h15.2c.3 0 .4.3.2.5l-3 3.1a.6.6 0 0 1-.4.2H1.1c-.3 0-.4-.3-.2-.5l3-3.1Zm12.8 5.6a.6.6 0 0 0-.4-.2H1.1c-.3 0-.4.3-.2.5l3 3.1c.1.1.3.2.4.2h15.2c.3 0 .4-.3.2-.5l-3-3.1Z"/></svg>`; };
const TRX = () => '<svg class="rw-chain trx" viewBox="0 0 20 20" aria-hidden="true"><path fill="#ff3d47" d="M18.6 5.5 15.5 3 1.9.6a.5.5 0 0 0-.5.8l8 17.5c.2.4.7.4.9.1L18.7 6.3a.6.6 0 0 0-.1-.8ZM14.6 3.9l1.8 1.4-3.4.6 1.6-2Zm-4.3 5.8-.9 6L3.3 2.7l7 7Zm.9-.6L4.9 2.6l8.2 1.4-1.9 5.1Zm.2 1.2 4.7-.9-5.8 8.6 1.1-7.7Z"/></svg>';
const chainIcon = c => c === 'tron' ? TRX() : SOL();
const sv = d => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${d}</svg>`;
const IC = {
  copy: sv('<rect x="9" y="9" width="11" height="11" rx="2.2"/><path d="M5 15V6.2A2.2 2.2 0 0 1 7.2 4H15"/>'),
  out: sv('<path d="M14 5h5v5M19 5l-8 8M11 6H7a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h9a2 2 0 0 0 2-2v-4"/>'),
  ok: sv('<path d="m5 12.5 4.5 4.5L19 7.5"/>'),
  warn: sv('<path d="M12 4 3 19.5h18L12 4Z"/><path d="M12 10v4.5M12 17.2v.1"/>'),
  x: sv('<path d="M6 6l12 12M18 6 6 18"/>'),
  heart: sv('<path d="M12 20s-7-4.3-7-9.5A4 4 0 0 1 12 8a4 4 0 0 1 7 2.5C19 15.7 12 20 12 20Z"/>'),
  clock: sv('<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>'),
  bill: sv('<path d="M6.5 3.5h11v17l-2.75-1.8L12 20.5l-2.75-1.8L6.5 20.5v-17Z"/><path d="M9.5 8.5h5M9.5 12h5"/>'),
};

// ---- 金币 ----
// 正面看的一枚硬币:底下露一点厚度,外缘一圈凸边(大号的带一圈齿纹),中间凹下去的币面上压一颗五角星,左上一道高光。
// 灰的一枚打底,金的一枚盖在上面、从币底往上按比例露出来 —— 进度就是整枚硬币自己由灰变金。
const starPath = (cx, cy, R, r) => { let d = ''; for (let i = 0; i < 10; i++) { const a = -Math.PI / 2 + i * Math.PI / 5, q = i % 2 ? r : R; d += (i ? 'L' : 'M') + (cx + q * Math.cos(a)).toFixed(2) + ' ' + (cy + q * Math.sin(a)).toFixed(2); } return d + 'Z'; };
const STAR = starPath(16, 16.25, 6.1, 2.55), STAR_SH = starPath(16.55, 16.95, 6.1, 2.55);
const PAL = {
  y: { face: ['#fff8cf', '#ffdb5c', '#f7b21d', '#e2900a'], field: ['#efa312', '#ffcc3e', '#ffe98f'], star: ['#fffdf0', '#ffe175'], edge: '#b46d08', reed: 'rgba(150,88,0,.34)', groove: 'rgba(158,96,2,.62)', shade: 'rgba(168,100,2,.8)' },
  g: { face: ['#ffffff', '#e7eaef', '#c4cad4', '#a3abb8'], field: ['#b1b8c4', '#d2d7df', '#eef1f5'], star: ['#ffffff', '#dde2e9'], edge: '#7b8493', reed: 'rgba(88,98,114,.28)', groove: 'rgba(94,104,120,.56)', shade: 'rgba(100,110,126,.72)' },
};
function coinLayer(k, id, big) {
  const P = PAL[k];
  return `<defs>
<linearGradient id="${id}${k}f" x1="4" y1="3" x2="28" y2="29" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="${P.face[0]}"/><stop offset=".36" stop-color="${P.face[1]}"/><stop offset=".78" stop-color="${P.face[2]}"/><stop offset="1" stop-color="${P.face[3]}"/></linearGradient>
<linearGradient id="${id}${k}i" x1="7" y1="6" x2="25" y2="26" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="${P.field[0]}"/><stop offset=".55" stop-color="${P.field[1]}"/><stop offset="1" stop-color="${P.field[2]}"/></linearGradient>
<linearGradient id="${id}${k}s" x1="12" y1="10" x2="20" y2="22" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="${P.star[0]}"/><stop offset="1" stop-color="${P.star[1]}"/></linearGradient>
</defs>
<circle cx="16" cy="17.35" r="13.5" fill="${P.edge}"/>
<circle cx="16" cy="16" r="13.5" fill="url(#${id}${k}f)"/>
${big ? `<circle cx="16" cy="16" r="12.2" fill="none" stroke="${P.reed}" stroke-width="1.5" stroke-dasharray=".5 1.033"/>` : ''}
<circle cx="16" cy="16" r="10.55" fill="url(#${id}${k}i)"/>
<circle cx="16" cy="16" r="10.55" fill="none" stroke="${P.groove}" stroke-width="${big ? 0.5 : 0.75}"/>
<path d="${STAR_SH}" fill="${P.shade}"/>
<path d="${STAR}" fill="url(#${id}${k}s)"/>
<path d="M5.7 11.9a11.2 11.2 0 0 1 6.1-5.9" fill="none" stroke="#fff" stroke-opacity=".92" stroke-width="${big ? 1.15 : 1.5}" stroke-linecap="round"/>
<path d="M26.4 20.3a11.2 11.2 0 0 1-5.3 5.5" fill="none" stroke="#fff" stroke-opacity=".3" stroke-width="${big ? 0.8 : 1}" stroke-linecap="round"/>`;
}
// frac = 这一枚灌了多少(0–1)
function coin(frac, big = false) {
  const id = uid();
  const f = Math.max(0, Math.min(1, frac || 0));
  const top = 2.5 + 28.35 * (1 - f); // 硬币(连厚度)从 y=2.5 到 30.85;金色从底往上露到这条线
  return `<svg class="rw-coin-svg${big ? ' big' : ''}" viewBox="0 0 32 33" aria-hidden="true">
<g>${coinLayer('g', id, big)}</g>
${f > 0 ? `<clipPath id="${id}c"><rect x="0" y="${top.toFixed(2)}" width="32" height="33"/></clipPath><g clip-path="url(#${id}c)">${coinLayer('y', id, big)}</g>` : ''}
</svg>`;
}

// ---- 顶栏 ----
const frac = () => st ? (st.bytes % TB()) / TB() : 0;
const left = () => st ? TB() - (st.bytes % TB()) : TB();
const pct = () => Math.floor(frac() * 100);
function hoverText() {
  if (!st) return t('rw.title');
  const p = st.points || 0;
  return p > 0 ? t('rw.hover.some', { n: pts(p), left: fmtBytes(left(), 1), pct: pct() }) : t('rw.hover.none', { left: fmtBytes(left(), 1), pct: pct() });
}
// 气泡只在概览页冒(别的页面右上角常有按钮,不去挡);十几秒后自己收起,点别处也收起 —— 收起后金币上留一个小点。
const onDash = () => { const h = location.hash.replace('#', '').replace('/', '').split('?')[0].split('/')[0]; return !h || h === 'dashboard'; };
const wantsBind = () => !!(st && st.remind && !st.address);
let tipTimer = 0;
function tipHTML() {
  if (!wantsBind() || tipHidden || isOpen || !onDash()) return '';
  const msg = st.remind === 'tb' ? t('rw.tip.tb', { n: pts(st.points) }) : t('rw.tip.fresh');
  return `<div class="rw-tip" role="status">
<div class="rw-tip-t">${SOL()}<span>${esc(msg)}</span></div>
<div class="rw-tip-a"><button class="btn primary sm" type="button" data-act="rw.bindNow">${esc(t('rw.bind'))}</button><button class="btn ghost sm" type="button" data-act="rw.later">${esc(t('rw.later'))}</button></div>
</div>`;
}
function renderSlot() {
  if (!slot) return;
  if (!st || st.enabled === false) { setHTML(slot, ''); return; }
  const p = st.points || 0;
  const pop = lastPoints >= 0 && p > lastPoints; // 刚又满了 1 TB:硬币翻一圈
  lastPoints = p;
  const tip = tipHTML();
  setHTML(slot, `<button class="rw-coin${pop ? ' pop' : ''}" type="button" data-act="rw.open" title="${esc(hoverText() + (wantsBind() ? ' · ' + t('rw.hover.bind') : ''))}" aria-label="${esc(t('rw.title') + ' · ' + hoverText())}">${coin(frac())}${p > 0 ? `<span class="rw-n">${p > 99 ? '99+' : p}</span>` : ''}${wantsBind() && !tip ? '<span class="rw-dot"></span>' : ''}</button>${tip}`);
  if (tip && !tipTimer) tipTimer = setTimeout(() => { tipTimer = 0; tipHidden = true; renderSlot(); }, 14000);
}

// 还在等结果的那一笔:等到账(pending),或者倒计时走完了、登记服务还没核对完(settling)
const pending = () => !!(st && st.donation && (st.donation.status === 'pending' || st.donation.status === 'settling'));
async function load() {
  const before = st && st.donation ? st.donation.status : '';
  try { st = await get('reward'); } catch { /* 取不到就保持上一次的样子:金币不该因为一次请求失败闪没 */ }
  renderSlot();
  if (isOpen) {
    const now = st && st.donation ? st.donation.status : '';
    if ((before === 'pending' || before === 'settling') && now === 'paid') { led.data = null; dnr.data = null; if (tab === 'pool') loadLedger(); if (tab === 'donate') loadDonors(); } // 刚到账:明细和捐赠榜都要重取
    paint();
  }
  clearInterval(fastTimer); fastTimer = 0;
  if (isOpen && pending()) fastTimer = setInterval(() => { if (!document.hidden) load(); }, 5000); // 等到账的时候查得勤一点
}
// 气泡开着时点了别处:收起(这一下点击照常落到它该去的地方)
document.addEventListener('pointerdown', e => {
  if (tipHidden || !slot || !slot.querySelector('.rw-tip')) return;
  if (e.target && e.target.closest && e.target.closest('.rw-tip, .rw-coin')) return;
  tipHidden = true; renderSlot();
}, true);
window.addEventListener('hashchange', () => renderSlot());
export function mountReward(el) {
  slot = el;
  if (slot) slot.__muiHTML = null; // 外壳重画过(换语言),按手头的数据重新画上
  renderSlot();
  if (isOpen) paintAll();
  if (!timer) { load(); timer = setInterval(() => { if (!document.hidden && !fastTimer) load(); }, 60000); }
}
// 设置里开关了这个功能之后叫一下
export function refreshReward() { load(); }

// ---- 头部 ----
function poolTopHTML() {
  const p = st.pool;
  if (!p) return `<div class="rw-pool-k">${esc(t('rw.pool.total'))}</div><div class="rw-pool-v"><b class="na">—</b></div><div class="rw-chips"><span class="rw-chip">${esc(t('rw.pool.none'))}</span></div>`;
  const per = p.points > 0 ? p.total / p.points : 0;
  return `<div class="rw-pool-k">${esc(t('rw.pool.total'))}${p.stale ? `<span class="rw-stale" title="${esc(t('rw.pool.stale', { at: timeOf(p.updatedAt) }))}">${IC.warn}</span>` : ''}</div>
<div class="rw-pool-v"><b id="rw-pool-n" data-v="${Number(p.total) || 0}">${usd(p.total)}</b><span>USDT</span></div>
<div class="rw-chips">${p.points > 0 ? `<span class="rw-chip gold" title="${esc(t('rw.pool.perHelp'))}">${esc(t('rw.pool.perChip', { v: usd(per) }))}</span>` : ''}<span class="rw-chip">${esc(p.points > 0 ? t('rw.pool.regChip', { n: pts(p.points), a: pts(p.addresses) }) : t('rw.pool.perNone'))}</span></div>`;
}
function cdHTML() {
  const p = st.pool;
  if (!p || !p.payoutAt) return '';
  const s = Math.max(0, Math.floor(p.payoutAt - Date.now() / 1000));
  if (s <= 0) return `<span class="rw-cd-k">${esc(t('rw.cd.due', { day: dayOf(p.payoutAt) }))}</span>`;
  return `<span class="rw-cd-k">${esc(t('rw.cd.k'))}</span><b>${Math.floor(s / 86400)}</b><i>${esc(t('rw.cd.d'))}</i><b>${p2(Math.floor(s % 86400 / 3600))}</b><i class="c">:</i><b>${p2(Math.floor(s % 3600 / 60))}</b><i class="c">:</i><b>${p2(s % 60)}</b>`;
}
function heroCoinHTML() {
  const f = frac();
  return `<div class="rw-art${f > 0.02 ? ' lit' : ''}"><i class="s1"></i><i class="s2"></i><i class="s3"></i>${coin(f, true)}<span class="rw-art-sh"></span></div>
<div class="rw-art-c"><b>${pct()}<small>%</small></b><span>${esc(t('rw.mine.left', { v: fmtBytes(left(), 1) }))}</span></div>`;
}

// ---- 我的 ----
function meHTML() {
  const p = st.points || 0, f = frac();
  const bound = !!st.address;
  const credited = Math.min(p, st.credited || 0), wait = Math.max(0, p - credited);
  let sub = '';
  if (!bound) sub = p > 0 ? t('rw.mine.unbound') : '';
  else if (!st.excluded) sub = wait > 0 ? t('rw.mine.pending', { a: pts(credited), b: pts(wait) }) : (p > 0 ? t('rw.mine.credited') : '');
  let addr;
  if (st.address) {
    addr = `<div class="rw-me-c addr"><span class="rw-k">${SOL()}${esc(t('rw.addr.bound'))}</span>
<div class="rw-addr-v"><code class="mono" title="${esc(st.address)}">${esc(short(st.address, 5))}</code><button class="rw-ib" type="button" data-act="rw.copy" data-v="${esc(st.address)}" title="${esc(t('common.copy'))}" aria-label="${esc(t('common.copy'))}">${IC.copy}</button><button class="rw-link" type="button" data-act="rw.bindNow">${esc(t('rw.addr.changeBtn'))}</button></div></div>`;
  } else {
    addr = bindOpen ? '' : `<div class="rw-me-c addr cta"><button class="rw-btn ink" type="button" data-act="rw.bindNow">${SOL()}<span>${esc(t('rw.bind'))}</span></button></div>`;
  }
  return `<div class="rw-me-row">
<div class="rw-me-c pts"><span class="rw-k">${esc(t('rw.mine.k'))}</span><div class="rw-me-v"><b>${pts(p)}</b>${sub ? `<em${wait > 0 && bound ? ` title="${esc(t('rw.mine.pendingHelp'))}" class="q"` : ''}>${esc(sub)}</em>` : ''}</div></div>
<div class="rw-me-c tfc"><span class="rw-k">${esc(t('rw.mine.traffic'))}</span><div class="rw-me-v"><b>${esc(fmtBytes(st.bytes, 2))}</b></div></div>
<div class="rw-me-c grow"><span class="rw-k">${esc(t('rw.mine.next'))}<span class="rw-sp"></span><span class="rw-pc">${pct()}%</span></span>
<div class="rw-bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${pct()}"><i style="width:${Math.max(f > 0 ? 1.5 : 0, f * 100).toFixed(2)}%"></i></div>
<span class="rw-me-s">${esc(t('rw.mine.left', { v: fmtBytes(left(), 1) }))}</span></div>
${addr}
</div>
${st.excluded ? `<div class="rw-note bad">${IC.warn}<span>${esc(t('rw.excluded.me', { reason: why(st.excluded) }))}</span></div>` : ''}
${bindOpen ? `<div class="rw-bind">
<label class="rw-k" for="rw-addr-in">${SOL()}${esc(t(st.address ? 'rw.addr.change' : 'rw.addr.bindTitle'))}</label>
<div class="rw-bind-r"><input id="rw-addr-in" type="text" class="rw-in mono" spellcheck="false" autocomplete="off" autocapitalize="off" maxlength="44" placeholder="${esc(t('rw.addr.ph'))}"><button class="rw-btn ink" type="button" data-act="rw.bindSave"${bindBusy ? ' disabled' : ''}>${esc(t('rw.addr.save'))}</button><button class="rw-btn" type="button" data-act="rw.bindCancel">${esc(t('common.cancel'))}</button></div>
<div class="rw-bind-err" id="rw-bind-err">${esc(bindErr)}</div>
<p class="rw-hint">${esc(t('rw.addr.warn'))}</p>
</div>` : (!st.address ? `<p class="rw-hint rw-me-h">${esc(t('rw.addr.noneHelp'))}</p>` : '')}`;
}

// ---- 奖池明细 ----
// 奖池从哪来:像一张小清单 —— 一路钱一行(小图标、叫什么、一句怎么算的;右边是「+ 进了奖池多少」),
// 最后一行金色的合计带「= 当前奖池」,就是页头那个数。只有三个数,加起来对得上,不用再看别的。
// 两路钱的颜色(订单金、捐赠紫)和下面逐笔入账里的小标是同一套。
function sourcesHTML(p) {
  const rate = Math.round((p.orderRate || 0.4) * 100), rest = 100 - rate;
  const row = (cls, icon, title, desc, v) => `<div class="rw-src-row"><span class="rw-src-i ${cls}">${icon}</span>
<div class="rw-src-t"><b>${esc(title)}</b><span>${desc}</span></div>
<b class="rw-src-v"><i>+</i>${usd(v)}</b></div>`;
  return `<div class="rw-src">
${row('ord', IC.bill, t('rw.pool.ordT', { rate }), withSub(p.orderCount > 0 ? t('rw.pool.ordD', { n: pts(p.orderCount), v: usd(p.orderAmount), rest }) : t('rw.pool.ordNone', { rest })), p.orderShare)}
${row('don', IC.heart, t('rw.pool.donT'), esc(p.donateCount > 0 ? t('rw.pool.donD', { n: pts(p.donateCount) }) : t('rw.pool.donNone')), p.donateAmount)}
<div class="rw-src-sum"><span class="rw-src-eq">=</span><b class="rw-src-st">${esc(t('rw.pool.total'))}</b><span class="rw-src-sv"><b>${usd(p.total)}</b><i>USDT</i></span></div>
</div>`;
}
function poolHTML() {
  const p = st.pool;
  if (!p) return `<div class="rw-empty">${esc(t('rw.pool.none'))}</div>`;
  return `<h3 class="rw-h">${esc(t('rw.pool.how'))}</h3>
${sourcesHTML(p)}

<div class="rw-led-head"><h3 class="rw-h">${esc(t('rw.led.title'))}</h3>
<div class="rw-filters">
<div class="rw-seg">${[['', 'rw.led.all'], ['order', 'rw.led.order'], ['donation', 'rw.led.donation']].map(([k, key]) => `<button type="button" class="${led.kind === k ? 'active' : ''}" data-act="rw.ledKind" data-v="${k}">${esc(t(key))}</button>`).join('')}</div>
<div class="rw-seg">${[['', 'rw.led.allChains'], ['solana', ''], ['tron', '']].map(([k, key]) => `<button type="button" class="${led.chain === k ? 'active' : ''}" data-act="rw.ledChain" data-v="${k}">${k ? chainIcon(k) + esc(CHAIN[k]) : esc(t(key))}</button>`).join('')}</div>
</div></div>
<div id="rw-led">${ledgerHTML()}</div>`;
}
function ledgerHTML() {
  const d = led.data;
  if (!d) return `<div class="rw-empty">${esc(t('common.loading'))}</div>`;
  if (!d.items.length) return `<div class="rw-empty">${esc(t(led.kind || led.chain ? 'rw.led.noneFiltered' : 'rw.led.none'))}</div>`;
  const rate = Math.round(((st.pool || {}).orderRate || 0.4) * 100);
  return `<div class="rw-list rw-led${led.loading ? ' busy' : ''}">
<div class="rw-row head"><span>${esc(t('rw.led.from'))}</span><span>${esc(t('rw.led.kind'))}</span><span>${esc(t('rw.led.time'))}</span><span class="r">${esc(t('rw.led.amount'))}</span><span class="r">${esc(t('rw.led.counted'))}</span></div>
${d.items.map(x => `<div class="rw-row${x.void ? ' void' : ''}">
<span class="c-from"><span class="rw-ci" title="${esc(CHAIN[x.chain] || x.chain)}">${chainIcon(x.chain)}</span>${x.from ? `<button class="rw-a mono" type="button" data-act="rw.copy" data-v="${esc(x.from)}" title="${esc(x.from + ' · ' + t('rw.clickCopy'))}">${esc(short(x.from, 6))}</button>` : `<span class="c-nofrom">—</span>`}${x.tx ? `<a class="rw-ib sm" href="${esc(txUrl(x.chain, x.tx))}" target="_blank" rel="noopener noreferrer" title="${esc(t('rw.led.tx'))}" aria-label="${esc(t('rw.led.tx'))}">${IC.out}</a>` : ''}</span>
<span class="c-kind"><span class="rw-tag ${x.kind === 'donation' ? 'don' : 'ord'}">${esc(t(x.kind === 'donation' ? 'rw.led.donation' : 'rw.led.order'))}</span>${x.kind === 'donation' && x.name ? `<span class="c-name" title="${esc(x.name)}">${esc(x.name)}</span>` : ''}${x.void ? `<span class="rw-tag off" title="${esc(t('rw.led.voidTip'))}">${esc(t('rw.led.void'))}</span>` : x.note === 'manual' ? `<span class="rw-tag man" title="${esc(t('rw.led.manualTip'))}">${esc(t('rw.led.manual'))}</span>` : ''}</span>
<span class="c-time" title="${esc(timeOf(x.ts))}">${esc(dayOf(x.ts))}</span>
<span class="c-amt r" data-k="${esc(t('rw.led.amount'))}">${usd(x.amount)}</span>
<span class="c-cnt r">${x.void ? `<b class="none" title="${esc(t('rw.led.voidTip'))}">${esc(t('rw.led.notCounted'))}</b>` : `<b>+${usd(x.share)}</b>${x.kind === 'donation' ? '' : `<i>${rate}%</i><small>${usd(x.amount)} × ${rate}%</small>`}`}</span>
</div>`).join('')}
</div>${pagerHTML('rw.ledPage', led.page, d.total)}`;
}
function pagerHTML(act, page, total) {
  const pages = Math.max(1, Math.ceil(total / PAGE));
  if (pages <= 1) return `<div class="rw-pager one">${esc(t('rw.count', { n: pts(total) }))}</div>`;
  return `<div class="rw-pager"><span>${esc(t('rw.count', { n: pts(total) }))}</span><span class="rw-sp"></span>
<button class="rw-btn sm" type="button" data-act="${act}" data-v="${page - 1}"${page <= 1 ? ' disabled' : ''}>${esc(t('rw.prev'))}</button><span class="pg">${page} / ${pages}</span><button class="rw-btn sm" type="button" data-act="${act}" data-v="${page + 1}"${page >= pages ? ' disabled' : ''}>${esc(t('rw.next'))}</button></div>`;
}

// ---- 积分榜 ----
function boardHTML() {
  const p = st.pool || {};
  return `<div class="rw-brd-head">
<input id="rw-q" class="rw-in" type="search" placeholder="${esc(t('rw.brd.search'))}" value="${esc(brd.q)}" spellcheck="false" autocomplete="off">
<span class="rw-hint">${esc(t('rw.brd.sum', { a: pts(p.addresses), n: pts(p.points) }))}</span>
<span class="rw-sp"></span>
${p.ledgerUrl ? `<a class="rw-link" href="${esc(p.ledgerUrl)}" target="_blank" rel="noopener noreferrer">${esc(t('rw.brd.public'))}${IC.out}</a>` : ''}
</div><div id="rw-brd">${boardRows()}</div>`;
}
const medal = rank => rank <= 3 ? `<i class="rw-medal m${rank}">${rank}</i>` : rank;
function boardRows() {
  const d = brd.data;
  if (!d) return `<div class="rw-empty">${esc(t('common.loading'))}</div>`;
  if (!d.items.length) return `<div class="rw-empty">${esc(t(brd.q ? 'rw.brd.noneQ' : 'rw.brd.none'))}</div>`;
  const total = (st.pool || {}).points || 0, pool = (st.pool || {}).total || 0;
  const top = d.top || d.items.reduce((m, x) => Math.max(m, x.excluded ? 0 : x.points), 0) || 1;
  return `<div class="rw-list rw-brd${brd.loading ? ' busy' : ''}">
<div class="rw-row head"><span>#</span><span>${esc(t('rw.brd.addr'))}</span><span class="r">${esc(t('rw.brd.points'))}</span><span>${esc(t('rw.brd.share'))}</span><span class="r">${esc(t('rw.brd.value'))}</span></div>
${d.items.map(x => {
    const share = !x.excluded && total > 0 ? x.points / total : 0;
    return `<div class="rw-row${x.me ? ' me' : ''}${x.excluded ? ' out' : ''}">
<span class="c-rank">${x.excluded ? '—' : medal(x.rank)}</span>
<span class="c-addr"><button class="rw-a mono" type="button" data-act="rw.copy" data-v="${esc(x.address)}" title="${esc(x.address + ' · ' + t('rw.clickCopy'))}">${esc(short(x.address, 5))}</button>${x.me ? `<span class="rw-tag me">${esc(t('rw.brd.me'))}</span>` : ''}${x.panels > 1 ? `<span class="rw-tag" title="${esc(t('rw.brd.panelsHelp'))}">${esc(t('rw.brd.panels', { n: x.panels }))}</span>` : ''}${x.excluded ? `<span class="rw-tag bad">${esc(t('rw.brd.excluded'))}</span>` : ''}</span>
<span class="c-pts r"><b>${pts(x.points)}</b><i>${esc(t(x.points === 1 ? 'rw.unit1' : 'rw.unit'))}</i></span>
${x.excluded ? `<span class="c-why">${esc(cap(why(x)))}</span>` : `<span class="c-share"><span class="rw-meter"><i style="width:${Math.max(2, x.points / top * 100).toFixed(1)}%"></i></span><em>${(share * 100).toFixed(share < 0.1 ? 2 : 1)}%</em></span>
<span class="c-val r">≈ ${usd(share * pool)}</span>`}
</div>`;
  }).join('')}
</div>${pagerHTML('rw.brdPage', brd.page, d.total)}`;
}

// ---- 捐赠 + 捐赠榜 ----
const dnLeft = () => { const d = st.donation; return d ? Math.max(0, Math.floor(d.expiresAt - Date.now() / 1000)) : 0; };
const mmss = s => `${p2(Math.floor(s / 60))}:${p2(s % 60)}`;
// 登记服务查账落后了多久(秒):它答复时的钟 − 它上一次把链上看全的时间。平时两分钟一轮,落后不超过两三分钟;
// 超过 6 分钟就是查链的节点不通了 —— 这时要明说钱不会丢,不能让人对着一个不动的页面猜。
const dnLag = () => { const d = st.donation; return d && d.scanAt > 0 && d.now > 0 ? Math.max(0, d.now - d.scanAt) : 0; };
const dnStuck = () => dnLag() > 360;
const stuckNote = () => dnStuck() ? `<div class="rw-note">${IC.clock}<span>${esc(t('rw.dn.stuck', { n: Math.round(dnLag() / 60) }))}</span></div>` : '';
// 等到账时的那一块:① 要转的确切金额 ② 奖池地址(这一笔登记时给的那个)+ 二维码。奖池地址只在这里出现。
function payBlock(d) {
  return `<div class="rw-dn-pay">
<div class="rw-dn-steps">
<div class="rw-dn-step"><span class="rw-k"><i>1</i>${esc(t('rw.dn.s1'))}</span><div class="rw-dn-amt"><b>${exactAmt(d.exact)}</b><span>USDT</span><button class="rw-btn sm" type="button" data-act="rw.copy" data-v="${esc(exactAmt(d.exact))}">${IC.copy}<span>${esc(t('rw.dn.copyAmt'))}</span></button></div></div>
<div class="rw-dn-step"><span class="rw-k"><i>2</i>${esc(t('rw.dn.s2'))}</span>
<div class="rw-dn-addr"><span class="rw-chain-i">${SOL()}</span><code class="mono">${esc(d.address)}</code></div>
<div class="rw-dn-btns"><button class="rw-btn sm" type="button" data-act="rw.copy" data-v="${esc(d.address)}">${IC.copy}<span>${esc(t('rw.dn.copyAddr'))}</span></button><a class="rw-btn sm" href="${esc(addrUrl('solana', d.address))}" target="_blank" rel="noopener noreferrer">${IC.out}<span>${esc(t('rw.onchain'))}</span></a></div></div>
</div>
<div class="rw-dn-qr"><img src="${esc(rewardQrUrl('solana', d.address))}" alt="${esc(t('rw.dn.qr'))}" width="120" height="120"><span>${esc(t('rw.dn.qr'))}</span></div>
</div>${st.pool.demo ? `<div class="rw-note bad">${IC.warn}<span>${esc(t('rw.dn.demo'))}</span></div>` : ''}`;
}
function donateCardHTML() {
  const d = st.donation, min = (st.pool || {}).donateMin || 1;
  const head = (title, right = '') => `<div class="rw-dn-h"><h3>${IC.heart}${esc(title)}</h3>${right}</div>`;
  if (d && d.status === 'pending' && d.address) {
    return `<section class="rw-dn wait">${head(t('rw.dn.waitTitle'), `<span class="rw-dn-timer" title="${esc(t('rw.dn.timerHelp'))}">${IC.clock}<b id="rw-dn-t">${mmss(dnLeft())}</b></span>`)}
<p class="rw-dn-lead">${esc(t('rw.dn.waitLead', { name: d.name, amount: exactAmt(d.exact) }))}</p>
${payBlock(d)}
${stuckNote()}
<p class="rw-hint rw-dn-late">${esc(t('rw.dn.late'))}</p>
<div class="rw-dn-foot"><span class="rw-dn-live"><i></i>${esc(t(dnStuck() ? 'rw.dn.liveStuck' : st.pool.demo ? 'rw.dn.liveDemo' : 'rw.dn.live'))}</span><span class="rw-sp"></span><button class="rw-link" type="button" data-act="rw.dnCancel" title="${esc(t('rw.dn.cancelHelp'))}">${esc(t('rw.dn.cancel'))}</button></div>
</section>`;
  }
  if (d && d.status === 'settling') { // 倒计时走完了,登记服务还没把这段时间的到账核对完:不说「没成立」
    return `<section class="rw-dn wait">${head(t('rw.dn.settleTitle'))}
<p class="rw-dn-lead">${esc(t('rw.dn.settleLead', { name: d.name, amount: exactAmt(d.exact) }))}</p>
${stuckNote()}
<div class="rw-dn-foot"><span class="rw-dn-live"><i></i>${esc(t(dnStuck() ? 'rw.dn.liveStuck' : 'rw.dn.settleLive'))}</span><span class="rw-sp"></span><button class="rw-link" type="button" data-act="rw.dnAgain" title="${esc(t('rw.dn.settleSkipHelp'))}">${esc(t('rw.dn.settleSkip'))}</button></div>
</section>`;
  }
  if (d && d.status === 'paid') {
    return `<section class="rw-dn done">${head(t('rw.dn.paidTitle'))}
<p class="rw-dn-lead ok">${IC.ok}<span>${esc(t('rw.dn.paidLead', { name: d.name, amount: exactAmt(d.exact) }))}</span></p>
<div class="rw-dn-foot"><button class="rw-btn ink" type="button" data-act="rw.dnAgain">${esc(t('rw.dn.again'))}</button></div>
</section>`;
  }
  if (!(st.pool || {}).donate) return `<section class="rw-dn">${head(t('rw.dn.title'))}<div class="rw-note">${IC.clock}<span>${esc(t('rw.dn.off'))}</span></div></section>`; // 登记服务这会儿给不出奖池地址
  const expired = d && d.status === 'expired';
  return `<section class="rw-dn">${head(t('rw.dn.title'))}
<p class="rw-dn-lead">${esc(t('rw.dn.lead'))}</p>
${expired ? `<div class="rw-note">${IC.clock}<span>${esc(t('rw.dn.expired', { amount: exactAmt(d.exact) }))}</span></div>` : ''}
<div class="rw-dn-form">
<label class="rw-f"><span class="rw-k">${esc(t('rw.dn.name'))}</span><input id="rw-dn-name" class="rw-in" type="text" maxlength="20" autocomplete="off" placeholder="${esc(t('rw.dn.namePh'))}" value="${esc(expired ? d.name : '')}"></label>
<label class="rw-f amt"><span class="rw-k">${esc(t('rw.dn.amount'))}</span><span class="rw-amt"><input id="rw-dn-amt" class="rw-in" type="text" inputmode="decimal" autocomplete="off" placeholder="${esc(t('rw.dn.amountPh', { min }))}" value="${esc(expired ? String(d.amount) : '')}"><i>USDT</i></span></label>
<button class="rw-btn ink" type="button" data-act="rw.dnSubmit"${donBusy ? ' disabled' : ''}>${esc(t('rw.dn.submit'))}</button>
</div>
<div class="rw-dn-quick"><span>${esc(t('rw.dn.quick'))}</span>${QUICK.map(v => `<button type="button" data-act="rw.dnQuick" data-v="${v}">${v}</button>`).join('')}</div>
<div class="rw-bind-err" id="rw-dn-err">${esc(donErr)}</div>
<p class="rw-hint">${esc(t('rw.dn.notes'))}</p>
</section>`;
}
function donateHTML() {
  if (!st.pool) return `<div class="rw-empty">${esc(t('rw.pool.none'))}</div>`;
  return `<div id="rw-dn">${donateCardHTML()}</div>
<div class="rw-led-head"><h3 class="rw-h">${esc(t('rw.dnr.title'))}</h3><span class="rw-hint" id="rw-dnr-sum">${donorsSum()}</span></div>
<div id="rw-dnr">${donorRows()}</div>`;
}
function donorsSum() {
  const d = dnr.data;
  if (!d) return '';
  const n = d.named || {};
  return n.count ? esc(t('rw.dnr.sum', { p: pts(n.donors), c: pts(n.count), v: usd(n.amount) })) : '';
}
function donorRows() {
  const d = dnr.data;
  if (!d) return `<div class="rw-empty">${esc(t('common.loading'))}</div>`;
  if (!d.items.length) return `<div class="rw-empty">${esc(t('rw.dnr.none'))}</div>`;
  const top = d.top || d.items.reduce((m, x) => Math.max(m, x.amount), 0) || 1;
  return `<div class="rw-list rw-dnr${dnr.loading ? ' busy' : ''}">
<div class="rw-row head"><span>#</span><span>${esc(t('rw.dnr.name'))}</span><span class="r">${esc(t('rw.dnr.count'))}</span><span>${esc(t('rw.dnr.last'))}</span><span class="r">${esc(t('rw.dnr.total'))}</span></div>
${d.items.map(x => `<div class="rw-row${x.me ? ' me' : ''}">
<span class="c-rank">${medal(x.rank)}</span>
<span class="c-dname"><b title="${esc(x.name)}">${esc(x.name)}</b><span class="rw-meter"><i style="width:${Math.max(2, x.amount / top * 100).toFixed(1)}%"></i></span></span>
<span class="c-dcnt r">${esc(t('rw.dnr.times', { n: pts(x.count) }))}</span>
<span class="c-time" title="${esc(timeOf(x.lastAt))}">${esc(dayOf(x.lastAt))}</span>
<span class="c-dsum r"><b>${usd(x.amount)}</b><i>USDT</i></span>
</div>`).join('')}
</div>${pagerHTML('rw.dnrPage', dnr.page, d.total)}`;
}

// ---- 规则 ----
function rulesHTML() {
  const p = st.pool || {};
  const rate = Math.round((p.orderRate || 0.4) * 100);
  const day = p.payoutAt ? dayOf(p.payoutAt) : '2027‑09‑03';
  const li = (n, k, v) => `<li><i>${n}</i><div><b>${esc(t(k + '.t', v))}</b><span>${withSub(t(k + '.d', v))}</span></div></li>`;
  return `<ol class="rw-rules">
${li(1, 'rw.rule.1')}${li(2, 'rw.rule.2')}${li(3, 'rw.rule.3', { rate })}${li(4, 'rw.rule.4', { day })}${li(5, 'rw.rule.5')}
</ol>
<div class="rw-priv"><h3 class="rw-h">${esc(t('rw.priv.title'))}</h3>
<ul><li>${esc(t('rw.priv.1'))}</li><li>${esc(t('rw.priv.2'))}</li><li>${esc(t('rw.priv.3'))}</li><li>${esc(t('rw.priv.4'))}</li></ul>
<p class="rw-hint">${esc(t('rw.priv.off'))} <a class="rw-link in" href="#/settings" data-act="rw.toSettings">${esc(t('rw.priv.offLink'))}</a></p></div>`;
}
const TABS = [['pool', 'rw.tab.pool'], ['board', 'rw.tab.board'], ['donate', 'rw.tab.donate'], ['rules', 'rw.tab.rules']];
const paneHTML = () => tab === 'pool' ? poolHTML() : tab === 'board' ? boardHTML() : tab === 'donate' ? donateHTML() : rulesHTML();
const tabsHTML = () => TABS.map(([k, key]) => `<button type="button" role="tab" aria-selected="${tab === k}" class="${tab === k ? 'active' : ''}" data-act="rw.tab" data-v="${k}">${esc(t(key))}</button>`).join('');

// ---- 活动页:搭一次架子,之后只换有变化的那几块(倒计时每秒走,金币的动画不该被整页重画打断)----
function shellHTML() {
  const day = st && st.pool && st.pool.payoutAt ? dayOf(st.pool.payoutAt) : '2027‑09‑03';
  return `<div class="rw-sheet" role="dialog" aria-modal="true" aria-label="${esc(t('rw.title'))}">
<button class="rw-x" type="button" data-act="rw.close" aria-label="${esc(t('common.close'))}" title="${esc(t('common.close'))}">${IC.x}</button>
<div class="rw-scroll" id="rw-scroll"><div class="rw-in-w">
<header class="rw-hero">
<div class="rw-hd"><div class="rw-kicker"><span class="rw-spark">✦</span><b>${esc(t('rw.title'))}</b><i>${esc(t('rw.kicker'))}</i></div>
<h2 class="rw-slogan">${esc(t('rw.slogan'))}</h2></div>
<p class="rw-lead">${esc(t('rw.lead', { day }))}</p>
<div id="rw-pool-top" class="rw-pool"></div>
<div class="rw-acts"><button class="rw-btn gold" type="button" data-act="rw.toDonate">${IC.heart}<span>${esc(t('rw.donate'))}</span></button><div class="rw-cd" id="rw-cd"></div></div>
<div class="rw-hero-r" id="rw-art"></div>
</header>
<section class="rw-me" id="rw-me"></section>
<nav class="rw-tabs" id="rw-tabs" role="tablist"></nav>
<div class="rw-pane" id="rw-pane"></div>
</div></div></div>`;
}
function keepInputs(ids, fn) { // 重画一块时,里面正在输入的框(地址、搜索、留名、金额)不丢字、不丢光标
  const keep = ids.map(id => { const el = document.getElementById(id); return el ? { id, el, v: el.value, f: document.activeElement === el, s: el.selectionStart, e: el.selectionEnd } : null; }).filter(Boolean);
  fn();
  for (const k of keep) {
    const now = document.getElementById(k.id);
    if (!now || now === k.el) continue;
    now.value = k.v;
    if (k.f) { now.focus(); try { now.setSelectionRange(k.s, k.e); } catch { /* search 框不支持就算了 */ } }
  }
}
function paint() {
  if (!isOpen || !st) return;
  setHTML('rw-pool-top', poolTopHTML());
  setHTML('rw-cd', cdHTML());
  setHTML('rw-art', heroCoinHTML());
  keepInputs(['rw-addr-in'], () => setHTML('rw-me', meHTML()));
  setHTML('rw-tabs', tabsHTML());
  keepInputs(['rw-q', 'rw-dn-name', 'rw-dn-amt'], () => setHTML('rw-pane', paneHTML()));
  wire();
}
function paintAll() { // 换了语言:架子上的字也要换
  const ov = document.getElementById('rw-ov');
  if (!ov) return;
  const top = (document.getElementById('rw-scroll') || {}).scrollTop || 0;
  ov.innerHTML = shellHTML();
  paint();
  const sc = document.getElementById('rw-scroll'); if (sc) sc.scrollTop = top;
}
function wire() {
  const q = document.getElementById('rw-q');
  if (q && !q.__rw) { q.__rw = 1; q.addEventListener('input', debounce(() => { brd.q = q.value.trim(); brd.page = 1; loadBoard(); }, 250)); }
  const on = (id, fn) => { const el = document.getElementById(id); if (el && !el.__rw) { el.__rw = 1; el.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); fn(); } }); } };
  on('rw-addr-in', bindSave); on('rw-dn-name', donateSubmit); on('rw-dn-amt', donateSubmit);
}
function tick() { // 每秒:发放倒计时、等到账的倒计时
  if (!isOpen || !st) return;
  setHTML('rw-cd', cdHTML());
  const el = document.getElementById('rw-dn-t');
  if (el) { const s = dnLeft(); el.textContent = mmss(s); if (s <= 0 && pending()) load(); }
}
function countUp() { // 打开时奖池数字从 0 数上来(系统设了「减少动态效果」就不数)
  const el = document.getElementById('rw-pool-n');
  if (!el || reduced()) return;
  const to = Number(el.dataset.v) || 0, t0 = performance.now(), dur = 900;
  if (to <= 0) return;
  const step = now => {
    if (!isOpen || !el.isConnected) return;
    const k = Math.min(1, (now - t0) / dur), e = 1 - Math.pow(1 - k, 3);
    el.textContent = usd(to * e);
    if (k < 1) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}

async function loadLedger() {
  led.loading = true;
  const qs = `reward/ledger?page=${led.page}&size=${PAGE}${led.kind ? '&kind=' + led.kind : ''}${led.chain ? '&chain=' + led.chain : ''}`;
  try { led.data = await get(qs); } catch (e) { if (isOpen) toast(trErr(e.message), 'err'); }
  led.loading = false;
  if (isOpen && tab === 'pool') setHTML('rw-led', ledgerHTML());
}
async function loadBoard() {
  brd.loading = true;
  try { brd.data = await get(`reward/board?page=${brd.page}&size=${PAGE}${brd.q ? '&q=' + encodeURIComponent(brd.q) : ''}`); } catch (e) { if (isOpen) toast(trErr(e.message), 'err'); }
  brd.loading = false;
  if (isOpen && tab === 'board') setHTML('rw-brd', boardRows());
}
async function loadDonors() {
  dnr.loading = true;
  try { dnr.data = await get(`reward/donors?page=${dnr.page}&size=${PAGE}`); } catch (e) { if (isOpen) toast(trErr(e.message), 'err'); }
  dnr.loading = false;
  if (isOpen && tab === 'donate') { setHTML('rw-dnr', donorRows()); setHTML('rw-dnr-sum', donorsSum()); }
}
function ensureTab() {
  if (tab === 'pool' && !led.data) loadLedger();
  if (tab === 'board' && !brd.data) loadBoard();
  if (tab === 'donate' && !dnr.data) loadDonors();
}
function showTabs() { // 切页时让页签贴到顶上,新的一页从头看
  const sc = document.getElementById('rw-scroll'), tb = document.getElementById('rw-tabs');
  if (sc && tb && tb.offsetTop < sc.scrollTop) sc.scrollTop = tb.offsetTop;
}

function onKey(e) {
  if (!isOpen) return;
  if (e.key === 'Escape') {
    e.preventDefault();
    if (bindOpen) { bindOpen = false; bindErr = ''; paint(); } else close();
    return;
  }
  if (e.key === 'Tab') { // 焦点留在活动页里
    const f = [...document.querySelectorAll('#rw-ov button:not([disabled]), #rw-ov a[href], #rw-ov input')].filter(x => x.offsetParent !== null);
    if (!f.length) return;
    const first = f[0], last = f[f.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
}
function open(focusBind = false) {
  if (!st) return;
  closeMenus();
  tipHidden = true; // 点开了就不用再冒气泡
  bindOpen = focusBind && !st.address;
  bindErr = ''; donErr = '';
  led.page = brd.page = dnr.page = 1; led.data = brd.data = dnr.data = null; brd.q = '';
  let ov = document.getElementById('rw-ov');
  if (!ov) {
    ov = document.createElement('div');
    ov.id = 'rw-ov'; ov.className = 'rw-ov';
    ov.addEventListener('mousedown', e => { ov.__down = e.target === ov; });
    ov.addEventListener('click', e => { if (e.target === ov && ov.__down) close(); }); // 在页内按下、拖到外面松手不算点空白
    document.body.appendChild(ov);
    document.addEventListener('keydown', onKey);
  }
  lastFocus = document.activeElement;
  ov.innerHTML = shellHTML();
  ov.hidden = false;
  ov.classList.remove('out');
  isOpen = true;
  document.documentElement.classList.add('rw-lock');
  paint();
  countUp();
  renderSlot();
  clearInterval(cdTimer);
  cdTimer = setInterval(tick, 1000);
  load();
  ensureTab();
  setTimeout(() => { const el = bindOpen ? document.getElementById('rw-addr-in') : ov.querySelector('.rw-x'); if (el) el.focus({ preventScroll: true }); }, 60);
}
function close() {
  const ov = document.getElementById('rw-ov');
  if (!ov || !isOpen) return;
  isOpen = false;
  clearInterval(cdTimer); clearInterval(fastTimer); fastTimer = 0;
  document.documentElement.classList.remove('rw-lock');
  ov.classList.add('out');
  setTimeout(() => { if (!isOpen) { ov.hidden = true; ov.innerHTML = ''; } }, reduced() ? 0 : 180);
  renderSlot();
  if (lastFocus && lastFocus.isConnected) { try { lastFocus.focus({ preventScroll: true }); } catch { /* 原来的按钮已经不在了 */ } }
}

async function bindSave() {
  const el = document.getElementById('rw-addr-in');
  const v = (el ? el.value : '').trim();
  const show = m => { bindErr = m; const e = document.getElementById('rw-bind-err'); if (e) e.textContent = m; };
  if (!validSol(v)) { show(t(v ? 'rw.addr.bad' : 'rw.addr.empty')); if (el) el.focus(); return; }
  if (bindBusy) return;
  bindBusy = true; show('');
  try {
    st = await post('reward/bind', { address: v });
    bindOpen = false;
    toast(t('rw.addr.saved'), 'ok');
    brd.data = null; brd.page = 1;
    if (tab === 'board') loadBoard();
  } catch (e) { show(trErr(e.message)); }
  bindBusy = false;
  renderSlot(); paint();
}
async function donateSubmit() {
  const nameEl = document.getElementById('rw-dn-name'), amtEl = document.getElementById('rw-dn-amt');
  const show = m => { donErr = m; const e = document.getElementById('rw-dn-err'); if (e) e.textContent = m; };
  const name = (nameEl ? nameEl.value : '').trim().replace(/\s+/g, ' ');
  const raw = (amtEl ? amtEl.value : '').trim().replace(/,/g, '');
  const min = (st.pool || {}).donateMin || 1, max = (st.pool || {}).donateMax || 100000;
  if (!name) { show(t('rw.dn.errName')); if (nameEl) nameEl.focus(); return; }
  if ([...name].length > 20) { show(t('rw.dn.errNameLong')); if (nameEl) nameEl.focus(); return; }
  if (!/^\d+(\.\d{1,2})?$/.test(raw) || !(Number(raw) >= min) || Number(raw) > max) { show(t('rw.dn.errAmount', { min, max: pts(max) })); if (amtEl) amtEl.focus(); return; }
  if (donBusy) return;
  donBusy = true; show('');
  try { st = await post('reward/donate', { name, amount: Number(raw) }); } catch (e) { show(trErr(e.message)); }
  donBusy = false;
  paint();
  clearInterval(fastTimer); fastTimer = 0;
  if (pending()) fastTimer = setInterval(() => { if (!document.hidden) load(); }, 5000);
}
// 「去设置里关闭」:到设置页,滚到「流量奖励」那一行并闪一下(设置页自己接着做,见 settings.js)
function toSettings() {
  close();
  try { sessionStorage.setItem('m-ui.scrollTo', 'set-rewardEnabled'); } catch { /* 存不了就只跳到设置页 */ }
  if (location.hash.replace(/\?.*$/, '') === '#/settings') { location.hash = '#/dashboard'; setTimeout(() => { location.hash = '#/settings'; }, 0); } // 已经在设置页:重进一次才会走到那一行
  else location.hash = '#/settings';
}

registerActions({
  'rw.open': () => open(false),
  'rw.close': () => close(),
  'rw.bindNow': () => {
    if (!isOpen) { open(true); return; }
    bindOpen = true; bindErr = ''; paint();
    const el = document.getElementById('rw-addr-in');
    if (el) { el.focus(); el.scrollIntoView({ block: 'nearest', behavior: reduced() ? 'auto' : 'smooth' }); }
  },
  'rw.bindCancel': () => { bindOpen = false; bindErr = ''; paint(); },
  'rw.bindSave': () => bindSave(),
  'rw.toDonate': () => { tab = 'donate'; paint(); ensureTab(); const tb = document.getElementById('rw-tabs'); if (tb) tb.scrollIntoView({ block: 'start', behavior: reduced() ? 'auto' : 'smooth' }); },
  'rw.dnSubmit': () => donateSubmit(),
  'rw.dnQuick': (_, el) => { const a = document.getElementById('rw-dn-amt'); if (a) { a.value = el.dataset.v; a.focus(); } },
  'rw.dnCancel': async () => { try { st = await post('reward/donate/cancel', {}); } catch (e) { toast(trErr(e.message), 'err'); } paint(); },
  'rw.dnAgain': async () => { try { st = await post('reward/donate/clear', {}); } catch (e) { toast(trErr(e.message), 'err'); } donErr = ''; paint(); },
  'rw.later': async () => { tipHidden = true; renderSlot(); try { await post('reward/remind', { dismiss: true }); } catch { /* 下次满 1 TB 再提醒,这次没记上也无妨 */ } },
  'rw.copy': (_, el) => copy(el.dataset.v),
  'rw.tab': (_, el) => { tab = el.dataset.v; paint(); ensureTab(); showTabs(); },
  'rw.ledKind': (_, el) => { led.kind = el.dataset.v; led.page = 1; paint(); loadLedger(); },
  'rw.ledChain': (_, el) => { led.chain = el.dataset.v; led.page = 1; paint(); loadLedger(); },
  'rw.ledPage': (_, el) => { led.page = Math.max(1, Number(el.dataset.v) || 1); loadLedger(); },
  'rw.brdPage': (_, el) => { brd.page = Math.max(1, Number(el.dataset.v) || 1); loadBoard(); },
  'rw.dnrPage': (_, el) => { dnr.page = Math.max(1, Number(el.dataset.v) || 1); loadDonors(); },
  'rw.toSettings': () => toSettings(),
});

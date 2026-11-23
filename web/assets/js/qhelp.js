// 简洁模式里的「?」说明(用户 11-21):
//   「切换成简洁模式后把文字介绍全部隐藏,所有的功能标题旁边一个轻量的？图标 鼠标悬浮显示文字介绍 点击？常驻介绍
//    再次点击或者点击介绍外部隐藏文字介绍」
//   「部分 hint影响面板运行或者必要的简洁模式也保留」
// 做法:模板里把「功能说明」标上 class="xp"(警告、状态、出错、空状态、操作结果这类不标,两种模式都照常显示)。
// 这里盯着页面上新出现的 .xp,给它找到所属的标题,在标题旁挂一个「?」;简洁模式下样式藏起挂上了「?」的说明、露出「?」。
//   - 找不到标题的说明不藏(没有「?」就再也看不到了),照常显示 —— 宁可多露一句,不能把说明弄丢;
//   - 一个标题下有几段说明时共用一个「?」,浮层里按页面上的顺序都列出来;
//   - 说明是空的(还没填上内容的动态说明)时「?」也藏着;
//   - 浮层的内容每次打开时现取,说明改了(换语言、动态说明换了一句)看到的就是新的。
// 不在详细模式里挂也行,但挂着不显示更省事:切模式只是换个属性,不用重新走一遍页面。

const HEAD = 'h1,h2,h3,h4,h5,h6';
// 标题里这些东西前面插「?」(紧跟标题文字,不排到徽章、按钮后面去)
const AFTER_TEXT = '.badge, .btn, button:not(.qh), .tag, .chip, .count, .pill, .row, details, select, input';
const ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9.2"/><path d="M9.7 9.4a2.4 2.4 0 0 1 4.65.8c0 1.6-2.35 2.1-2.35 3.55"/><circle cx="12" cy="16.85" r=".55" fill="currentColor" stroke="none"/></svg>';

let label = '说明';
export function setQhLabel(text) { label = text || label; document.querySelectorAll('.qh').forEach(b => b.setAttribute('aria-label', label)); }

// anchorFor 说明所属的标题:
//   - 写了 data-qh-for="元素 id" 的按它来;
//   - 设置页的一行(.set-info 里的 .set-label)、表单的一格(.field 里的 .label)、开关行(.sw-row 里的 <b>)各按结构找;
//   - 其余的往前找同级的标题(h1–h6,或卡片头 / 折叠块标题里的),找不到就上一层接着找,最多四层,不出卡片 / 弹窗 / 抽屉;
//     弹窗、抽屉正文里找不到的,挂到弹窗 / 抽屉的标题上。
export function anchorFor(xp) {
  if (xp.dataset.qhFor) return document.getElementById(xp.dataset.qhFor);
  if (xp.id === 'page-subtitle') return document.getElementById('page-title');
  const info = xp.closest('.set-info');
  if (info) return info.querySelector('.set-label');
  const field = xp.closest('.field');
  if (field) return field.querySelector(':scope > .label') || field.querySelector(':scope > span:not(.help):not(.xp)');
  const sw = xp.closest('.sw-row');
  if (sw) return sw.querySelector('b');
  // 选项卡片:「<b>标题</b> 徽章 <br> <小字说明>」或「<span>徽章 <b>标题</b></span><小字说明>」
  for (let p = xp.previousElementSibling; p; p = p.previousElementSibling) {
    if (p.matches('br, .badge, .tag')) continue;
    if (p.matches('b, strong')) return p;
    if (p.matches('span') && p.lastElementChild && p.lastElementChild.matches('b, strong')) return p.lastElementChild;
    break;
  }
  let el = xp;
  for (let depth = 0; depth < 4 && el; depth++) {
    for (let p = el.previousElementSibling; p; p = p.previousElementSibling) {
      if (p.matches(HEAD)) return p;
      if (p.matches('.card-head, .ld-head, .sec-head, .acc-head, header, summary')) {
        const h = p.querySelector(HEAD + ', .acc-t'); // 折叠块的标题(线路弹框「中转入口」「分流规则」)是 summary 里的 .acc-t
        if (h) return h;
      }
    }
    const parent = el.parentElement;
    if (!parent) break;
    if (parent.id === 'modal-body') return document.getElementById('modal-title');
    if (parent.id === 'drawer-body') return document.getElementById('drawer-title');
    if (parent.matches('summary')) { const h = parent.querySelector(HEAD); if (h) return h; }
    if (parent.matches('.card, .modal-box, .drawer, #page, body')) break;
    el = parent;
  }
  return null;
}

const hasText = xp => xp.isConnected && xp.textContent.trim() !== '';

// 标签正好写满一行时「?」不单独掉到下一行:「?」是行内小块,浏览器在它前面总能断行(英文的长标签上实测到)。
// 这几类标签和卡片头的标题把紧挨着「?」的最后一个词和「?」一起包进不断行的 .qh-glue,「?」总跟着最后一个词;
// 含中文、或者词超过 12 个字时只带最后两个字,免得一长串不能折行把标签撑出去。按固定的选择器认,不按当时的布局。
// 设置页分区标题自己是弹性布局(包一层会拆成两格、中间多出间距),弹窗、顶栏的标题各有自己的排法(见 style.css),都不碰。
// (试过给这几类标签加 text-wrap: balance:WebKit 在「?」刚显示出来的那次排版里没把它算进去,单行标签的「?」反而全掉到第二行。)
const GLUE = '.set-label, .field > .label, .sw-row b, .bk-steps b, .wall-kind-t b, .card-head > h2';
const NO_GLUE = '.set-sec > .card-head > h2';
function glue(anchor, b) {
  const t = b.previousSibling;
  if (!anchor.matches(GLUE) || anchor.matches(NO_GLUE) || !t || t.nodeType !== 3) return;
  const m = t.data.match(/(\S+)(\s*)$/);
  if (!m) return;
  const chars = [...m[1]];
  const word = /[\u2E80-\u9FFF\uF900-\uFAFF\uFF00-\uFFEF]/.test(m[1]) || chars.length > 12 ? chars.slice(-2).join('') : m[1];
  const cut = t.data.length - m[2].length - word.length;
  const tail = cut > 0 ? t.splitText(cut) : t;
  const g = document.createElement('span');
  g.className = 'qh-glue';
  tail.before(g);
  g.append(tail, b);
}

function buttonFor(anchor) {
  const after = anchor.id === 'page-title'; // 顶栏标题每换一页整个重写文字:「?」放在它后面,不放在里面
  let b = after ? anchor.parentElement.querySelector(':scope > .qh') : anchor.querySelector(':scope > .qh, :scope > .qh-glue > .qh');
  if (b) return b;
  // 不用 <button>:「?」常在 <label> 里(开关行、表单格),<label> 会把排在前面的 button 当成它的控件,点整行就点到了「?」上
  b = document.createElement('span');
  b.className = 'qh';
  b.setAttribute('role', 'button');
  b.tabIndex = 0;
  b.setAttribute('aria-label', label);
  b.setAttribute('aria-expanded', 'false');
  b.innerHTML = ICON;
  b.__xps = new Set();
  if (after) anchor.after(b);
  else {
    const stop = [...anchor.children].find(c => c.matches(AFTER_TEXT));
    anchor.insertBefore(b, stop || null);
    glue(anchor, b);
  }
  return b;
}

// scan 给还没挂上「?」的说明挂上;收拾没有说明了的「?」
function scan() {
  document.querySelectorAll('.xp:not(.has-qh):not([data-qh-none])').forEach(xp => {
    const a = anchorFor(xp);
    if (!a || a.closest('.xp') === xp) { xp.dataset.qhNone = ''; return; }
    const b = buttonFor(a);
    b.__xps.add(xp);
    xp.__qh = b;
    xp.classList.add('has-qh');
    if (xp.classList.contains('xp-always')) b.classList.add('qh-always'); // 两种模式都只在「?」里
  });
  document.querySelectorAll('.qh').forEach(b => {
    if (!b.__xps) return;
    for (const xp of b.__xps) if (!xp.isConnected || xp.__qh !== b) b.__xps.delete(xp);
    const empty = ![...b.__xps].some(hasText);
    if (b.hidden !== empty) b.hidden = empty;
    if (empty && open === b) hide();
  });
}
let queued = false;
const queue = () => {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => { queued = false; scan(); });
};

// ---- 浮层 ----
let tip = null, open = null, pinned = false, hideTimer = 0;
function ensureTip() {
  if (tip) return tip;
  tip = document.createElement('div');
  tip.className = 'qh-tip';
  tip.id = 'qh-tip';
  tip.setAttribute('role', 'tooltip');
  tip.hidden = true;
  tip.addEventListener('pointerenter', () => clearTimeout(hideTimer));
  tip.addEventListener('pointerleave', e => { if (!pinned && e.pointerType === 'mouse') later(); });
  document.body.append(tip);
  return tip;
}
function contentOf(b) {
  if (b.dataset.qhText) return `<div class="qh-p">${b.dataset.qhText.replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))}</div>`; // 圆点这类:内容就写在自己身上
  // 按页面上的先后排(Set 里是挂上的先后,动态说明后挂的可能排到前面去)
  const xps = [...b.__xps].filter(hasText).sort((x, y) => (x.compareDocumentPosition(y) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1));
  return xps.map(xp => {
    const c = xp.cloneNode(true);
    c.querySelectorAll('[id]').forEach(n => n.removeAttribute('id'));
    c.querySelectorAll('.qh').forEach(n => n.remove());
    return `<div class="qh-p">${c.innerHTML}</div>`;
  }).join('');
}
function place() {
  if (!open || !tip || tip.hidden) return;
  const r = open.getBoundingClientRect();
  if (!r.width || r.bottom < 0 || r.top > innerHeight) { hide(); return; } // 标题滚出去了 / 被藏了
  const vw = document.documentElement.clientWidth, vh = innerHeight, gap = 8, m = 8;
  // 先放回最左边再量:浮层宽度随内容伸缩,在上一次靠右的位置量会被屏幕右边挤窄,按窄的宽度挪过去以后它又撑开,
  // 右边就顶到了屏幕边上(手机宽度下看得出来,11-21 第五遍补测发现)。放在最左边量到的才是它真正的宽度
  tip.style.left = m + 'px';
  tip.style.top = '0px';
  const w = tip.offsetWidth, h = tip.offsetHeight;
  const cx = r.left + r.width / 2;
  let left = Math.round(Math.min(Math.max(m, cx - 22), vw - w - m));
  const below = r.bottom + gap + h <= vh - m || r.top - gap - h < m;
  const top = Math.round(below ? r.bottom + gap : r.top - gap - h);
  tip.style.left = left + 'px';
  tip.style.top = top + 'px';
  tip.dataset.side = below ? 'below' : 'above';
  tip.style.setProperty('--qh-x', Math.round(Math.min(Math.max(12, cx - left), w - 12)) + 'px');
}
function show(b, pin) {
  clearTimeout(hideTimer);
  const html = contentOf(b);
  if (!html) return;
  ensureTip();
  if (open && open !== b) open.setAttribute('aria-expanded', 'false');
  open = b;
  pinned = !!pin;
  tip.innerHTML = html;
  tip.classList.toggle('pinned', pinned);
  tip.hidden = false;
  b.setAttribute('aria-expanded', 'true');
  b.setAttribute('aria-describedby', 'qh-tip');
  place();
}
export function hide() {
  clearTimeout(hideTimer);
  if (open) { open.setAttribute('aria-expanded', 'false'); open.removeAttribute('aria-describedby'); }
  open = null;
  pinned = false;
  if (tip) tip.hidden = true;
}
const later = () => { clearTimeout(hideTimer); hideTimer = setTimeout(hide, 160); };
export const qhOpen = () => !!open;
// inQh 这个元素是不是「?」或说明浮层(抽屉、弹窗的「点外面收起」要放过它们)
export const inQh = el => !!(el && el.closest && el.closest('.qh, .qh-dot, .qh-tip'));

export function initQh() {
  // 在 DOM 变化的同一时刻(渲染之前)就挂上「?」:不等下一帧,简洁模式下说明不会先露出来再收起;后台标签页里也照常挂
  new MutationObserver(scan).observe(document.body, { childList: true, subtree: true });
  queue();
  // 鼠标移上去显示,移开收起(移到浮层上不收,好选字、点链接);点一下固定住,再点一下或点外面收起
  document.addEventListener('pointerover', e => {
    if (e.pointerType !== 'mouse' || pinned) return;
    const b = e.target.closest && e.target.closest('.qh, .qh-dot');
    if (b && !b.hidden) show(b, false);
  }, true);
  document.addEventListener('pointerout', e => {
    if (e.pointerType !== 'mouse' || pinned || !open) return;
    const b = e.target.closest && e.target.closest('.qh, .qh-dot');
    if (b && b === open && !(e.relatedTarget && (b.contains(e.relatedTarget) || (tip && tip.contains(e.relatedTarget))))) later();
  }, true);
  document.addEventListener('click', e => {
    const b = e.target.closest && e.target.closest('.qh, .qh-dot');
    if (b) {
      // 「?」挂在 <label> / <summary> / 表格行这些自己会响应点击的东西里:不能让这一下切开关、展开折叠块、打开详情
      e.preventDefault();
      e.stopPropagation();
      if (open === b && pinned) hide();
      else show(b, true);
      return;
    }
    if (open && !(tip && tip.contains(e.target))) hide();
  }, true);
  document.addEventListener('keydown', e => {
    const q = (e.key === 'Enter' || e.key === ' ') && e.target.closest && e.target.closest('.qh');
    if (q) { e.preventDefault(); e.stopPropagation(); if (open === q && pinned) hide(); else show(q, true); return; } // 键盘:和点一下一样
    if (e.key === 'Escape' && open) { e.stopPropagation(); e.preventDefault(); const b = open, inTip = !!(tip && tip.contains(document.activeElement)); hide(); if (inTip) b.focus(); }
  }, true);
  document.addEventListener('scroll', () => { if (open) place(); }, true);
  window.addEventListener('resize', () => { if (open) place(); });
}

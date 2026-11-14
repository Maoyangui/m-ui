// 圣诞彩蛋(限时):顶栏右上角一棵小圣诞树(没有外框),点一下全屏放一段动画 ——
// 夜空、月亮、光斑;三层雪(近处是会转的六角雪花);正中一棵大圣诞树逐层长出、金色彩带画上、彩球弹出、彩灯轮流闪、
// 顶上的星星带光芒点亮;雪橇和驯鹿从月亮前飞过、拖一串金色星光;「Merry Xmas」金箔字逐字浮现,下面「guys」。
// 约 9 秒后自动淡出,右上角 ×、点空白处、按 Esc 都能提前关。
// 到 12 月 31 日为止(按浏览器本地日期),过了自动不显示;以后每年 12 月也会出现。
// 自带样式,不依赖 style.css:删掉这个文件和 app.js 里的引用就干净了。prefers-reduced-motion:不飘不飞,只淡入。

const START = new Date(2026, 10, 14); // 2026-11-14 起
export function xmasActive(now = new Date()) {
  if (now.getMonth() === 11) return true; // 每年 12 月
  return now.getFullYear() === 2026 && now >= START; // 今年从 11-14 开始
}

const CSS = `
.xmas-egg { position: relative; width: 30px; height: 32px; border: 0; background: none; padding: 0; margin: 0 .1rem; cursor: pointer; flex: none;
  display: inline-grid; place-items: center; transition: transform .25s cubic-bezier(.3,1.6,.5,1), filter .25s ease; }
.xmas-egg svg { width: 28px; height: 30px; overflow: visible; }
.xmas-egg:hover { transform: translateY(-2px) rotate(-4deg) scale(1.06); filter: drop-shadow(0 3px 8px rgba(255, 196, 70, .55)); }
.xmas-egg:active { transform: scale(.94); }
.xmas-egg:focus-visible { outline: 2px solid #e8b04a; outline-offset: 3px; border-radius: 8px; }
.xmas-egg .xb-star { transform-box: fill-box; transform-origin: 50% 50%; animation: xb-star 2.4s ease-in-out infinite; }
.xmas-egg .xb-l { animation: xb-blink 1.6s ease-in-out infinite; }
.xmas-egg .xb-l:nth-of-type(2) { animation-delay: .4s; } .xmas-egg .xb-l:nth-of-type(3) { animation-delay: .8s; } .xmas-egg .xb-l:nth-of-type(4) { animation-delay: 1.2s; }
@keyframes xb-star { 0%, 100% { transform: scale(1) rotate(0); filter: drop-shadow(0 0 0 rgba(255,214,90,0)); } 50% { transform: scale(1.25) rotate(14deg); filter: drop-shadow(0 0 3px rgba(255,214,90,.95)); } }
@keyframes xb-blink { 0%, 100% { opacity: .35; } 50% { opacity: 1; } }

.xmas-show { position: fixed; inset: 0; z-index: 9999; overflow: hidden; cursor: pointer; color: #fff;
  background: radial-gradient(120% 90% at 50% 15%, #13254d 0%, #0a1430 45%, #04070f 100%);
  opacity: 0; animation: xs-in .9s ease forwards; }
/* 整体出场 = 浮现:整个画面淡入,内容只轻轻上移一点(不加模糊,里面逐项出场的动画照常看得清);整体消失 = 隐退:内容上移变淡,画面随后淡出 */
.xmas-stage { animation: xs-lift 1s cubic-bezier(.2,.7,.2,1) both; }
@keyframes xs-lift { from { transform: translateY(14px); } to { transform: none; } }
.xmas-show.out { animation: xs-out 1.1s ease .15s both; }
.xmas-show.out .xmas-stage { animation: xs-recede .9s cubic-bezier(.4,0,.6,1) forwards; }
@keyframes xs-recede { from { opacity: 1; transform: none; } to { opacity: 0; transform: translateY(-16px) scale(.985); } }
.xmas-show.out .xmas-x, .xmas-show.out .xmas-hint { opacity: 0; transition: opacity .4s ease; }
@keyframes xs-in { from { opacity: 0; } to { opacity: 1; } }
@keyframes xs-out { from { opacity: 1; } to { opacity: 0; } }
.xmas-show canvas { position: absolute; inset: 0; width: 100%; height: 100%; pointer-events: none; }
.xmas-stage { position: absolute; inset: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 1.2vh; pointer-events: none; padding: 16px; }
.xmas-tree { height: min(48vh, 64vw); width: auto; overflow: visible; filter: drop-shadow(0 18px 30px rgba(0,0,0,.45)); }
.xmas-tree .t { transform-box: fill-box; transform-origin: 50% 100%; transform: scale(.2, 0); opacity: 0; animation: xs-grow .9s cubic-bezier(.2,1.5,.4,1) forwards; }
@keyframes xs-grow { to { transform: scale(1, 1); opacity: 1; } }
.xmas-tree .g { stroke-dasharray: 220; stroke-dashoffset: 220; animation: xs-draw 1s ease-out forwards; }
@keyframes xs-draw { to { stroke-dashoffset: 0; } }
.xmas-tree .b { transform-box: fill-box; transform-origin: 50% 50%; transform: scale(0); animation: xs-pop .55s cubic-bezier(.2,1.9,.4,1) forwards; }
@keyframes xs-pop { to { transform: scale(1); } }
.xmas-tree .l { opacity: 0; animation: xs-on .3s ease forwards, xs-tw 1.4s ease-in-out infinite; }
@keyframes xs-on { to { opacity: 1; } }
@keyframes xs-tw { 0%, 100% { opacity: .35; } 50% { opacity: 1; } }
.xmas-tree .s { transform-box: fill-box; transform-origin: 50% 50%; transform: scale(0) rotate(-90deg); animation: xs-star 1s cubic-bezier(.2,1.7,.4,1) forwards; }
@keyframes xs-star { to { transform: scale(1) rotate(0); } }
.xmas-tree .rays { transform-box: fill-box; transform-origin: 50% 50%; opacity: 0; animation: xs-rays-in .8s ease forwards, xs-spin 12s linear infinite; }
@keyframes xs-rays-in { to { opacity: .85; } }
@keyframes xs-spin { to { transform: rotate(360deg); } }
.xmas-tree .gift { transform-box: fill-box; transform-origin: 50% 100%; transform: translateY(30px) scale(.6); opacity: 0; animation: xs-gift .7s cubic-bezier(.2,1.6,.4,1) forwards; }
@keyframes xs-gift { to { transform: none; opacity: 1; } }
.xmas-title { text-align: center; line-height: 1; }
.xmas-line { font: italic 700 clamp(2.4rem, 8.5vw, 5.6rem)/1.02 Georgia, "Times New Roman", "Songti SC", serif; letter-spacing: .01em; white-space: nowrap;
  filter: drop-shadow(0 0 18px rgba(255, 200, 90, .35)); }
.xmas-line span { display: inline-block; opacity: 0; transform: translateY(26px); filter: blur(8px);
  background: linear-gradient(100deg, #a8741f 0%, #f6d27a 22%, #fff8de 34%, #ffd36b 46%, #c9922e 62%, #ffe9a8 80%, #a8741f 100%); background-size: 260% 100%;
  -webkit-background-clip: text; background-clip: text; color: transparent;
  animation: xs-rise .9s cubic-bezier(.2,.9,.3,1) forwards, xs-foil 3.2s linear infinite; }
.xmas-line span.sp { width: .3em; }
@keyframes xs-rise { to { opacity: 1; transform: none; filter: blur(0); } }
@keyframes xs-foil { from { background-position: 130% 0; } to { background-position: -130% 0; } }
.xmas-sub { display: inline-block; margin-top: .55rem; font: 600 clamp(1.05rem, 3.6vw, 1.7rem)/1.3 "Trebuchet MS", system-ui, sans-serif; letter-spacing: .3em; text-transform: lowercase;
  color: #fff4dc; opacity: 0; transform: translateY(10px); animation: xs-rise .8s ease forwards; padding: 0 .1em .35rem .4em; position: relative; }
.xmas-sub::after { content: ""; position: absolute; left: 0; right: .3em; bottom: 0; height: 5px; border-radius: 3px;
  background: repeating-linear-gradient(-45deg, #e23b3b 0 7px, #fff 7px 14px); background-size: 40px 100%; animation: xs-cane 1.2s linear infinite; }
@keyframes xs-cane { to { background-position: 40px 0; } }
.xmas-x { position: absolute; top: 16px; right: 16px; width: 42px; height: 42px; border-radius: 50%; border: 1px solid rgba(255,255,255,.3); background: rgba(255,255,255,.1);
  color: #fff; font: 300 28px/1 system-ui, sans-serif; display: grid; place-items: center; cursor: pointer; backdrop-filter: blur(6px); transition: background .2s ease, transform .2s ease; z-index: 2; }
.xmas-x:hover { background: rgba(255,255,255,.22); transform: rotate(90deg); }
.xmas-x:focus-visible { outline: 2px solid #ffd36b; outline-offset: 2px; }
.xmas-hint { position: absolute; bottom: 16px; left: 0; right: 0; text-align: center; color: rgba(255,255,255,.5); font: 500 .8rem system-ui, sans-serif; pointer-events: none; }
@media (max-height: 520px) { .xmas-tree { height: 40vh; } }
@media (prefers-reduced-motion: reduce) {
  .xmas-egg .xb-star, .xmas-egg .xb-l { animation: none; opacity: 1; }
  .xmas-tree .t, .xmas-tree .g, .xmas-tree .b, .xmas-tree .l, .xmas-tree .s, .xmas-tree .gift, .xmas-line span, .xmas-sub { animation: xs-on .6s ease forwards !important; transform: none !important; filter: none !important; stroke-dashoffset: 0 !important; }
  .xmas-tree .rays, .xmas-sub::after { animation: none !important; }
  .xmas-stage, .xmas-show.out .xmas-stage { transform: none !important; filter: none !important; }
}
`;

function injectCSS() {
  if (document.getElementById('xmas-css')) return;
  const st = document.createElement('style');
  st.id = 'xmas-css';
  st.textContent = CSS;
  document.head.appendChild(st);
}

// 顶栏的小树:三层渐变枝叶 + 雪边 + 四颗轮流闪的彩灯 + 会闪的星;没有外框
const MINI = `<svg viewBox="0 0 28 30" aria-hidden="true">
  <defs>
    <linearGradient id="xmb-g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#3fbf73"/><stop offset="1" stop-color="#11633a"/></linearGradient>
    <linearGradient id="xmb-s" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fff3b8"/><stop offset="1" stop-color="#f2b632"/></linearGradient>
  </defs>
  <rect x="12" y="24.2" width="4" height="4.2" rx="1" fill="#8b5a33"/>
  <path d="M14 15.4 6 25.2c2.6-1.1 5.3-1 8 .2 2.7-1.2 5.4-1.3 8-.2Z" fill="url(#xmb-g)"/>
  <path d="M14 10.2 7.6 18.8c2.1-.9 4.3-.8 6.4.2 2.1-1 4.3-1.1 6.4-.2Z" fill="url(#xmb-g)"/>
  <path d="M14 5.6 9.2 12.4c1.6-.7 3.2-.6 4.8.2 1.6-.8 3.2-.9 4.8-.2Z" fill="url(#xmb-g)"/>
  <path d="M6 25.2c2.6-1.1 5.3-1 8 .2 2.7-1.2 5.4-1.3 8-.2-2.6-.4-5.3 0-8 1-2.7-1-5.4-1.4-8-1Z" fill="#fff" opacity=".9"/>
  <path d="M7.6 18.8c2.1-.9 4.3-.8 6.4.2 2.1-1 4.3-1.1 6.4-.2-2.1-.3-4.3.1-6.4.9-2.1-.8-4.3-1.2-6.4-.9Z" fill="#fff" opacity=".85"/>
  <circle class="xb-l" cx="11" cy="21.6" r="1.1" fill="#ff5d5d"/><circle class="xb-l" cx="17.2" cy="20.6" r="1.1" fill="#ffd54a"/>
  <circle class="xb-l" cx="12.6" cy="14.8" r="1" fill="#62b6ff"/><circle class="xb-l" cx="16" cy="13.6" r="1" fill="#ff8ad0"/>
  <path class="xb-star" d="m14 .6 1.25 2.55 2.8.4-2.03 1.98.48 2.8L14 7.0l-2.5 1.33.48-2.8-2.03-1.98 2.8-.4Z" fill="url(#xmb-s)" stroke="#e1a12a" stroke-width=".4"/>
</svg>`;

// 大树:四层枝叶逐层长出,雪边、金色彩带、彩球、彩灯、星与光芒、树下礼物。各部件的出场时间写在 style 的 animation-delay 里。
function bigTree() {
  const tiers = [
    // [路径, 雪边路径, 出场秒]
    ['M100 140 30 214c23-10 47-11 70 1 23-12 47-11 70-1Z', 'M30 214c23-10 47-11 70 1 23-12 47-11 70-1-23-3-47 1-70 9-23-8-47-12-70-9Z', .25],
    ['M100 100 42 170c19-9 39-9 58 1 19-10 39-10 58-1Z', 'M42 170c19-9 39-9 58 1 19-10 39-10 58-1-19-3-39 1-58 8-19-7-39-11-58-8Z', .45],
    ['M100 64 55 126c15-7 30-7 45 1 15-8 30-8 45-1Z', 'M55 126c15-7 30-7 45 1 15-8 30-8 45-1-15-2-30 1-45 7-15-6-30-9-45-7Z', .65],
    ['M100 30 69 84c10-5 21-5 31 1 10-6 21-6 31-1Z', 'M69 84c10-5 21-5 31 1 10-6 21-6 31-1-10-2-21 1-31 6-10-5-21-8-31-6Z', .85],
  ];
  const tierSVG = tiers.map(([p, snow, d]) => `<g class="t" style="animation-delay:${d}s"><path d="${p}" fill="url(#xmo-leaf)"/><path d="${p}" fill="url(#xmo-shade)"/><path d="${snow}" fill="url(#xmo-snow)"/></g>`).join('');
  const garland = [['M70 96c18 12 40 13 62-4', 1.15], ['M58 138c24 16 54 17 84-6', 1.35], ['M45 180c30 20 70 20 110-8', 1.55]]
    .map(([p, d]) => `<path class="g" d="${p}" fill="none" stroke="url(#xmo-gold)" stroke-width="3.2" stroke-linecap="round" style="animation-delay:${d}s"/>`).join('');
  const baubles = [[78, 102, 'r'], [118, 98, 'au'], [98, 108, 'b'], [66, 146, 'au'], [102, 152, 'r'], [134, 140, 's'], [86, 150, 'b'], [56, 190, 'r'], [92, 196, 's'], [128, 192, 'au'], [148, 182, 'b'], [112, 200, 'r']]
    .map(([x, y, c], i) => `<circle class="b" cx="${x}" cy="${y}" r="${c === 'au' ? 6.2 : 5.6}" fill="url(#xmo-${c})" style="animation-delay:${(1.7 + i * 0.06).toFixed(2)}s"/>`).join('');
  const lights = [[84, 88], [110, 86], [72, 122], [96, 128], [124, 120], [60, 164], [86, 172], [114, 170], [140, 160], [50, 206], [76, 210], [104, 214], [132, 208], [156, 200]]
    .map(([x, y], i) => `<circle class="l" cx="${x}" cy="${y}" r="2.4" fill="${['#fff3b0', '#ffb3b3', '#b3e1ff', '#c9ffb8'][i % 4]}" filter="url(#xmo-glow)" style="animation-delay:${(2.0 + i * 0.05).toFixed(2)}s, ${(2.3 + (i % 5) * 0.27).toFixed(2)}s"/>`).join('');
  const gift = (x, y, w, h, body, ribbon, d) => `<g class="gift" style="animation-delay:${d}s"><rect x="${x}" y="${y}" width="${w}" height="${h}" rx="2.5" fill="${body}"/>
    <rect x="${x + w / 2 - 2.2}" y="${y}" width="4.4" height="${h}" fill="${ribbon}"/><rect x="${x}" y="${y + h * 0.36}" width="${w}" height="4" fill="${ribbon}"/>
    <path d="M${x + w / 2} ${y}c-6-7-12-2-7 1Zm0 0c6-7 12-2 7 1Z" fill="none" stroke="${ribbon}" stroke-width="2.4" stroke-linecap="round"/></g>`;
  return `<svg class="xmas-tree" viewBox="0 0 200 250" aria-hidden="true">
  <defs>
    <linearGradient id="xmo-leaf" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#3ccf7c"/><stop offset=".55" stop-color="#1b8a4b"/><stop offset="1" stop-color="#0c4d2a"/></linearGradient>
    <linearGradient id="xmo-shade" x1="0" y1="0" x2="1" y2="0"><stop offset=".5" stop-color="#000" stop-opacity="0"/><stop offset="1" stop-color="#000" stop-opacity=".28"/></linearGradient>
    <linearGradient id="xmo-snow" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#d9ecff"/></linearGradient>
    <linearGradient id="xmo-gold" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="#b8862b"/><stop offset=".5" stop-color="#ffe9a8"/><stop offset="1" stop-color="#c9922e"/></linearGradient>
    <radialGradient id="xmo-r" cx=".35" cy=".3" r=".8"><stop offset="0" stop-color="#ffd0d0"/><stop offset=".35" stop-color="#ff4b4b"/><stop offset="1" stop-color="#8f1414"/></radialGradient>
    <radialGradient id="xmo-au" cx=".35" cy=".3" r=".8"><stop offset="0" stop-color="#fff6cf"/><stop offset=".4" stop-color="#ffcc4d"/><stop offset="1" stop-color="#9c6a12"/></radialGradient>
    <radialGradient id="xmo-b" cx=".35" cy=".3" r=".8"><stop offset="0" stop-color="#e2f3ff"/><stop offset=".4" stop-color="#4aa8ff"/><stop offset="1" stop-color="#14467d"/></radialGradient>
    <radialGradient id="xmo-s" cx=".35" cy=".3" r=".8"><stop offset="0" stop-color="#ffffff"/><stop offset=".45" stop-color="#c9d3df"/><stop offset="1" stop-color="#5d6b7c"/></radialGradient>
    <radialGradient id="xmo-starfill" cx=".5" cy=".4" r=".7"><stop offset="0" stop-color="#fffbe6"/><stop offset=".5" stop-color="#ffd54a"/><stop offset="1" stop-color="#e09a1c"/></radialGradient>
    <radialGradient id="xmo-halo" cx=".5" cy=".5" r=".5"><stop offset="0" stop-color="#ffe9a0" stop-opacity=".9"/><stop offset="1" stop-color="#ffe9a0" stop-opacity="0"/></radialGradient>
    <filter id="xmo-glow" x="-200%" y="-200%" width="500%" height="500%"><feGaussianBlur stdDeviation="1.6" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
  </defs>
  <ellipse cx="100" cy="238" rx="78" ry="7" fill="#000" opacity=".28"/>
  <rect x="91" y="212" width="18" height="24" rx="3" fill="#7a4a29"/><rect x="91" y="212" width="6" height="24" rx="3" fill="#9a6439" opacity=".8"/>
  ${tierSVG}
  ${garland}
  ${baubles}
  ${lights}
  ${gift(48, 216, 34, 24, '#d63a3a', '#ffd36b', 2.2)}${gift(118, 220, 30, 20, '#2f6fd6', '#e8eef7', 2.35)}${gift(84, 226, 20, 14, '#2a9d5c', '#ffd36b', 2.5)}
  <circle class="rays" cx="100" cy="30" r="30" fill="url(#xmo-halo)" style="animation-delay:2.55s, 2.55s"/>
  <g class="rays" style="animation-delay:2.55s, 2.55s">${Array.from({ length: 12 }, (_, i) => `<rect x="99.2" y="-6" width="1.6" height="22" rx=".8" fill="#ffe9a0" opacity=".7" transform="rotate(${i * 30} 100 30)"/>`).join('')}</g>
  <path class="s" d="m100 12 5.3 10.8 11.9 1.7-8.6 8.4 2 11.8L100 39.1l-10.6 5.6 2-11.8-8.6-8.4 11.9-1.7Z" fill="url(#xmo-starfill)" stroke="#f7c04a" stroke-width="1" style="animation-delay:2.45s"/>
</svg>`;
}

// mountXmasEgg 在 target(顶栏按钮区)最前面放彩蛋;不在活动期内什么都不放。title 是悬停提示。
export function mountXmasEgg(target, title = 'Merry Xmas!') {
  if (!target || !xmasActive() || target.querySelector('.xmas-egg')) return;
  injectCSS();
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'xmas-egg';
  b.title = title;
  b.setAttribute('aria-label', title);
  b.innerHTML = MINI;
  b.addEventListener('click', () => playXmas(b));
  target.prepend(b);
}

// playXmas 放一遍;origin 是星光炸开的位置(按钮)。约 9 秒后自动淡出,×、点空白处、Esc 都能提前关。
export function playXmas(origin) {
  injectCSS();
  if (document.querySelector('.xmas-show')) return;
  const reduce = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
  const zh = (document.documentElement.lang || navigator.language || '').toLowerCase().startsWith('zh');
  const box = document.createElement('div');
  box.className = 'xmas-show';
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-label', 'Merry Xmas guys');
  const letters = [...'Merry Xmas'].map((c, i) => c === ' ' ? '<span class="sp"></span>' : `<span style="animation-delay:${(2.6 + i * 0.08).toFixed(2)}s, ${(3.6 + i * 0.08).toFixed(2)}s">${c}</span>`).join('');
  box.innerHTML = `<canvas class="xm-back"></canvas>
    <div class="xmas-stage">${bigTree()}<div class="xmas-title"><div class="xmas-line">${letters}</div><div class="xmas-sub" style="animation-delay:3.5s">guys</div></div></div>
    <canvas class="xm-front"></canvas>
    <button type="button" class="xmas-x" aria-label="${zh ? '关闭' : 'Close'}" title="${zh ? '关闭' : 'Close'}">×</button>
    <div class="xmas-hint">${zh ? '会自动消失,也可以点 × 或按 Esc 关掉' : 'Closes by itself — or click × / press Esc'}</div>`;
  document.body.appendChild(box);

  const back = box.querySelector('.xm-back'), front = box.querySelector('.xm-front');
  const bx = back.getContext('2d'), fx = front.getContext('2d');
  const dpr = Math.min(window.devicePixelRatio || 1, 2);
  let W = 0, H = 0;
  const size = () => {
    W = box.clientWidth; H = box.clientHeight;
    for (const [cv, cx] of [[back, bx], [front, fx]]) { cv.width = W * dpr; cv.height = H * dpr; cx.setTransform(dpr, 0, 0, dpr, 0, 0); }
  };
  size();
  window.addEventListener('resize', size);
  const rnd = (a, b) => a + Math.random() * (b - a);
  const area = W * H;

  // 背景:星星、月亮、漂浮的暖色光斑
  const stars = Array.from({ length: Math.round(Math.min(140, area / 9000)) }, () => ({ x: rnd(0, W), y: rnd(0, H * 0.6), r: rnd(.4, 1.3), p: rnd(0, 6.28), v: rnd(.6, 1.8) }));
  const bokeh = Array.from({ length: 16 }, () => ({ x: rnd(0, W), y: rnd(H * 0.2, H), r: rnd(18, 60), vy: rnd(-.25, -.08), vx: rnd(-.1, .1), c: ['255,196,90', '255,110,110', '120,220,160', '150,190,255'][Math.floor(rnd(0, 4))], a: rnd(.05, .14) }));
  const moon = { x: W * 0.8, y: H * 0.17, r: Math.max(22, Math.min(W, H) * 0.06) };
  // 前景:三层雪(远、中、近;近处画六角雪花)
  const flakes = reduce ? [] : Array.from({ length: Math.round(Math.min(220, area / 6500)) }, () => {
    const layer = Math.random() < .5 ? 0 : Math.random() < .7 ? 1 : 2;
    return { x: rnd(0, W), y: rnd(-H, H), layer, r: [rnd(.6, 1.4), rnd(1.4, 2.6), rnd(4, 7)][layer], vy: [rnd(.3, .6), rnd(.7, 1.2), rnd(1.2, 2)][layer], s: rnd(0, 6.28), rot: rnd(0, 6.28), vr: rnd(-.02, .02) };
  });
  // 按钮处炸开的星光与小彩球
  const o = origin && origin.getBoundingClientRect ? origin.getBoundingClientRect() : { left: W / 2, top: H / 2, width: 0, height: 0 };
  const ox = o.left + o.width / 2, oy = o.top + o.height / 2;
  const COLORS = ['#ffd54a', '#ff5d5d', '#62d18c', '#62b6ff', '#fff6d0', '#ff9ad5'];
  const burst = reduce ? [] : Array.from({ length: 70 }, () => {
    const a = rnd(Math.PI * .1, Math.PI * .9), sp = rnd(3, 9);
    return { x: ox, y: oy, vx: -Math.cos(a) * sp * (Math.random() < .5 ? -1 : 1), vy: Math.sin(a) * sp * .6 + rnd(0, 2), r: rnd(1.6, 4), c: COLORS[Math.floor(rnd(0, COLORS.length))], life: 1, star: Math.random() < .55, rot: rnd(0, 6), vr: rnd(-.2, .2) };
  });
  // 雪橇:2.6 秒起从左到右飞过月亮前,拖一串金色星光
  const trail = [];
  const sleighAt = t => { const k = (t - 2.6) / 4.2; return k < 0 || k > 1 ? null : { k, x: -160 + (W + 320) * k, y: moon.y + Math.sin(k * Math.PI * 1.2) * H * 0.06 - k * H * 0.04 }; };

  const starPath = (c, x, y, r, rot) => { c.beginPath(); for (let i = 0; i < 10; i++) { const rr = i % 2 ? r * .45 : r, a = rot + i * Math.PI / 5; c.lineTo(x + Math.cos(a) * rr, y + Math.sin(a) * rr); } c.closePath(); };
  const crystal = (c, x, y, r, rot) => {
    c.save(); c.translate(x, y); c.rotate(rot); c.beginPath();
    for (let i = 0; i < 6; i++) { c.rotate(Math.PI / 3); c.moveTo(0, 0); c.lineTo(0, -r); c.moveTo(0, -r * .55); c.lineTo(r * .28, -r * .8); c.moveTo(0, -r * .55); c.lineTo(-r * .28, -r * .8); }
    c.stroke(); c.restore();
  };
  const drawSleigh = (c, x, y) => {
    c.save(); c.translate(x, y); c.fillStyle = 'rgba(255,244,222,.95)'; c.strokeStyle = 'rgba(255,244,222,.95)'; c.lineWidth = 1.4;
    c.shadowColor = 'rgba(255,214,120,.85)'; c.shadowBlur = 10; // 发光的浅色剪影:在夜空里看得见,飞过月亮时也清楚
    for (let i = 0; i < 3; i++) { // 驯鹿
      const dx = 40 + i * 30, bob = Math.sin((performance.now() / 160) + i) * 2;
      c.beginPath(); c.ellipse(dx, bob, 11, 5, 0, 0, Math.PI * 2); c.fill();
      c.beginPath(); c.moveTo(dx + 9, bob - 2); c.lineTo(dx + 15, bob - 9); c.lineTo(dx + 19, bob - 8); c.lineTo(dx + 13, bob - 1); c.fill();
      c.beginPath(); c.moveTo(dx + 15, bob - 9); c.lineTo(dx + 13, bob - 15); c.moveTo(dx + 16, bob - 10); c.lineTo(dx + 19, bob - 15); c.stroke();
      c.beginPath(); c.moveTo(dx - 7, bob + 3); c.lineTo(dx - 10, bob + 10); c.moveTo(dx + 6, bob + 3); c.lineTo(dx + 9, bob + 10); c.stroke();
      if (i === 2) { c.fillStyle = '#ff4b4b'; c.beginPath(); c.arc(dx + 20, bob - 7.5, 1.8, 0, Math.PI * 2); c.fill(); c.fillStyle = 'rgba(255,244,222,.95)'; }
    }
    c.beginPath(); c.moveTo(28, 0); c.lineTo(-2, -4); c.stroke(); // 缰绳
    c.beginPath(); c.moveTo(-40, -10); c.lineTo(-2, -10); c.quadraticCurveTo(4, -10, 4, -2); c.lineTo(-30, 2); c.quadraticCurveTo(-42, 2, -40, -10); c.fill(); // 车身
    c.beginPath(); c.moveTo(-44, 6); c.lineTo(2, 6); c.quadraticCurveTo(8, 6, 8, 1); c.stroke(); // 滑板
    c.beginPath(); c.arc(-24, -16, 6, 0, Math.PI * 2); c.fill(); // 圣诞老人
    c.fillStyle = '#ff4b4b'; c.beginPath(); c.moveTo(-30, -18); c.quadraticCurveTo(-24, -30, -16, -20); c.fill();
    c.restore();
  };

  let raf = 0;
  const t0 = performance.now();
  const frame = now => {
    const t = (now - t0) / 1000;
    // 背景层
    bx.clearRect(0, 0, W, H);
    for (const s of stars) { s.p += .03 * s.v; bx.globalAlpha = .25 + .55 * Math.abs(Math.sin(s.p)); bx.fillStyle = '#fff'; bx.beginPath(); bx.arc(s.x, s.y, s.r, 0, 6.283); bx.fill(); }
    const mg = bx.createRadialGradient(moon.x, moon.y, moon.r * .2, moon.x, moon.y, moon.r * 3.2);
    mg.addColorStop(0, 'rgba(255,248,220,.35)'); mg.addColorStop(1, 'rgba(255,248,220,0)');
    bx.globalAlpha = 1; bx.fillStyle = mg; bx.beginPath(); bx.arc(moon.x, moon.y, moon.r * 3.2, 0, 6.283); bx.fill();
    bx.fillStyle = '#fff6da'; bx.beginPath(); bx.arc(moon.x, moon.y, moon.r, 0, 6.283); bx.fill();
    bx.fillStyle = 'rgba(210,200,170,.35)'; bx.beginPath(); bx.arc(moon.x - moon.r * .3, moon.y - moon.r * .15, moon.r * .18, 0, 6.283); bx.arc(moon.x + moon.r * .25, moon.y + moon.r * .3, moon.r * .12, 0, 6.283); bx.fill();
    for (const b of bokeh) {
      b.x += b.vx; b.y += b.vy; if (b.y < -80) { b.y = H + 80; b.x = rnd(0, W); }
      const g = bx.createRadialGradient(b.x, b.y, 0, b.x, b.y, b.r);
      g.addColorStop(0, `rgba(${b.c},${b.a})`); g.addColorStop(1, `rgba(${b.c},0)`);
      bx.fillStyle = g; bx.beginPath(); bx.arc(b.x, b.y, b.r, 0, 6.283); bx.fill();
    }
    // 前景层
    fx.clearRect(0, 0, W, H);
    for (const f of flakes) {
      f.y += f.vy; f.s += .015; f.rot += f.vr; f.x += Math.sin(f.s) * (f.layer + 1) * .35;
      if (f.y > H + 10) { f.y = -10; f.x = rnd(0, W); }
      if (f.layer === 2) { fx.globalAlpha = .85; fx.strokeStyle = '#fff'; fx.lineWidth = 1.1; crystal(fx, f.x, f.y, f.r, f.rot); }
      else { fx.globalAlpha = f.layer ? .8 : .45; fx.fillStyle = '#fff'; fx.beginPath(); fx.arc(f.x, f.y, f.r, 0, 6.283); fx.fill(); }
    }
    const sl = reduce ? null : sleighAt(t);
    if (sl) {
      for (let i = 0; i < 3; i++) trail.push({ x: sl.x - 44 + rnd(-4, 4), y: sl.y + 4 + rnd(-3, 3), r: rnd(.8, 2.4), life: 1, vy: rnd(.1, .6) });
      drawSleigh(fx, sl.x, sl.y);
    }
    for (const p of trail) {
      if (p.life <= 0) continue;
      p.life -= .012; p.y += p.vy;
      fx.globalAlpha = Math.max(0, p.life); fx.fillStyle = '#ffd36b';
      starPath(fx, p.x, p.y, p.r * 1.5, p.life * 3); fx.fill();
    }
    for (const p of burst) {
      if (p.life <= 0) continue;
      p.x += p.vx; p.y += p.vy; p.vy += .12; p.vx *= .985; p.rot += p.vr; p.life -= .008;
      fx.globalAlpha = Math.max(0, p.life); fx.fillStyle = p.c;
      if (p.star) { starPath(fx, p.x, p.y, p.r * 1.6, p.rot); fx.fill(); }
      else { fx.beginPath(); fx.arc(p.x, p.y, p.r, 0, 6.283); fx.fill(); fx.globalAlpha *= .6; fx.fillStyle = '#fff'; fx.beginPath(); fx.arc(p.x - p.r * .35, p.y - p.r * .35, p.r * .3, 0, 6.283); fx.fill(); }
    }
    fx.globalAlpha = 1;
    if (t < 11) raf = requestAnimationFrame(frame);
  };
  raf = requestAnimationFrame(frame);

  let closed = false;
  const esc = e => { if (e.key === 'Escape') close(); };
  const close = () => {
    if (closed) return;
    closed = true;
    box.classList.add('out');
    setTimeout(() => { cancelAnimationFrame(raf); window.removeEventListener('resize', size); document.removeEventListener('keydown', esc); box.remove(); }, 1350);
  };
  box.addEventListener('click', close); // 点空白处、点 ×、按 Esc 都能关
  box.querySelector('.xmas-x').addEventListener('click', e => { e.stopPropagation(); close(); });
  document.addEventListener('keydown', esc);
  setTimeout(() => box.querySelector('.xmas-x')?.focus({ preventScroll: true }), 60);
  setTimeout(close, 9200);
}

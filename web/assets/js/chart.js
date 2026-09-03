// 零依赖 SVG 流量图:按时段聚合的堆叠柱状图(下行在下、上行在上),悬停显示该时段明细,
// 顶部汇总 总量/峰值,Y 轴取整刻度,X 轴按桶宽选择 时:分 / 月/日 标签。
import { fmtBytes, tzDate } from './ui.js';

const pad2 = n => String(n).padStart(2, '0');

// 取"好看"的刻度上限:1/2/5 × 10^k 的字节数
function niceMax(v) {
  if (v <= 0) return 1024;
  const units = [1, 1024, 1024 ** 2, 1024 ** 3, 1024 ** 4];
  let unit = units[0];
  for (const u of units) if (v >= u) unit = u;
  const x = v / unit;
  const steps = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10, 15, 20, 25, 30, 40, 50, 60, 80, 100, 150, 200, 250, 300, 400, 500, 600, 800, 1000];
  for (const s of steps) if (s >= x) return s * unit;
  return Math.ceil(x) * unit;
}

// 时间一律按面板时区取墙上时间(tzDate 平移后用 getUTC*)
const day = d => `${d.getUTCMonth() + 1}/${d.getUTCDate()}`;
const hm = d => `${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}`;

function labelFor(t, span, showDate) {
  const d = tzDate(t);
  if (span >= 86400) return day(d);
  if (showDate) return `${day(d)} ${hm(d)}`;
  return hm(d);
}

function rangeText(t, span) {
  const a = tzDate(t), b = tzDate(t + span);
  if (span >= 86400) return day(a);
  return `${day(a)} ${hm(a)} – ${hm(b)}`;
}

// barChart(container, points, {span, height, upLabel, downLabel, totalLabel, peakLabel, emptyText})
export function barChart(container, points, opts = {}) {
  const W = 800, H = opts.height || 220, PL = 58, PR = 12, PT = 10, PB = 26;
  const iw = W - PL - PR, ih = H - PT - PB;
  const n = points.length;
  const upLabel = opts.upLabel || '上行', downLabel = opts.downLabel || '下行';
  const span = opts.span || (n > 1 ? points[1].t - points[0].t : 3600);
  const totalUp = points.reduce((s, p) => s + p.up, 0), totalDown = points.reduce((s, p) => s + p.down, 0);
  if (!n || totalUp + totalDown === 0) {
    container.innerHTML = `<div class="empty">${opts.emptyText || '该时段没有流量'}</div>`;
    return;
  }
  const sums = points.map(p => p.up + p.down);
  const peakIdx = sums.indexOf(Math.max(...sums));
  const max = niceMax(Math.max(...sums));
  const y = v => PT + ih - v / max * ih;
  const slot = iw / n, gap = Math.min(6, slot * 0.25), bw = Math.max(2, slot - gap);
  const x = i => PL + i * slot + gap / 2;

  let grid = '';
  for (let k = 0; k <= 4; k++) {
    const v = max * k / 4, yy = y(v).toFixed(1);
    grid += `<line x1="${PL}" x2="${W - PR}" y1="${yy}" y2="${yy}" class="grid"/>`;
    grid += `<text x="${PL - 8}" y="${yy}" class="ylab" text-anchor="end" dominant-baseline="middle">${fmtBytes(v, v >= 1024 ** 3 ? 1 : 0)}</text>`;
  }
  let bars = '';
  for (let i = 0; i < n; i++) {
    const p = points[i];
    const yD = y(p.down), yU = y(p.down + p.up);
    bars += `<g class="bar" data-i="${i}">
      <rect class="hit" x="${(PL + i * slot).toFixed(1)}" y="${PT}" width="${slot.toFixed(1)}" height="${ih}"/>
      <rect class="seg down" x="${x(i).toFixed(1)}" y="${yD.toFixed(1)}" width="${bw.toFixed(1)}" height="${Math.max(0, PT + ih - yD).toFixed(1)}" rx="1"/>
      <rect class="seg up" x="${x(i).toFixed(1)}" y="${yU.toFixed(1)}" width="${bw.toFixed(1)}" height="${Math.max(0, yD - yU).toFixed(1)}" rx="1"/>
    </g>`;
  }
  // X 轴标签:跨多天且桶宽小于一天时,只在每天零点处标日期(天数多则隔天标);否则均匀取 ≤8 个标签
  const totalSpan = points[n - 1].t - points[0].t + span;
  const multiDay = totalSpan > 36 * 3600 && span < 86400;
  let xl = '';
  const put = (i, text) => { xl += `<text x="${(x(i) + bw / 2).toFixed(1)}" y="${H - 8}" class="xlab" text-anchor="middle">${text}</text>`; };
  if (multiDay) {
    const dayStarts = [];
    for (let i = 0; i < n; i++) {
      const d = tzDate(points[i].t);
      if (d.getUTCHours() === 0 && d.getUTCMinutes() === 0) dayStarts.push(i);
    }
    const step = Math.max(1, Math.ceil(dayStarts.length / 8));
    dayStarts.forEach((i, k) => { if (k % step === 0) put(i, labelFor(points[i].t, 86400)); });
    if (!dayStarts.length) put(0, labelFor(points[0].t, span, true));
  } else {
    const every = Math.max(1, Math.ceil(n / 8));
    for (let i = 0; i < n; i += every) put(i, labelFor(points[i].t, span, false));
  }
  container.innerHTML = `
    <div class="chart-wrap">
      <div class="chart-summary">
        <span><b>${opts.totalLabel || '合计'}</b> <span class="num">${fmtBytes(totalUp + totalDown, 1)}</span> <span class="muted small">↑ ${fmtBytes(totalUp, 1)} · ↓ ${fmtBytes(totalDown, 1)}</span></span>
        <span class="muted small">${opts.peakLabel || '峰值'} ${fmtBytes(sums[peakIdx], 1)} · ${rangeText(points[peakIdx].t, span)}</span>
        <span class="chart-legend"><span class="lg up">${upLabel}</span><span class="lg down">${downLabel}</span></span>
      </div>
      <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" class="chart bars">${grid}${bars}${xl}</svg>
      <div class="chart-tip" hidden></div>
    </div>`;
  const tip = container.querySelector('.chart-tip');
  const svg = container.querySelector('svg');
  svg.addEventListener('mousemove', e => {
    const g = e.target.closest('g.bar');
    if (!g) { tip.hidden = true; return; }
    svg.querySelectorAll('g.bar.hot').forEach(b => b.classList.remove('hot'));
    g.classList.add('hot');
    const p = points[Number(g.dataset.i)];
    const r = svg.getBoundingClientRect();
    tip.innerHTML = `<div class="tt">${rangeText(p.t, span)}</div><div>↑ ${fmtBytes(p.up)}</div><div>↓ ${fmtBytes(p.down)}</div><div class="muted small">= ${fmtBytes(p.up + p.down)}</div>`;
    tip.hidden = false;
    const left = e.clientX - r.left;
    tip.style.left = Math.min(left + 12, r.width - 170) + 'px';
    tip.style.top = Math.max(0, e.clientY - r.top - 70) + 'px';
  });
  svg.addEventListener('mouseleave', () => { tip.hidden = true; svg.querySelectorAll('g.bar.hot').forEach(b => b.classList.remove('hot')); });
}

// 旧名兼容
export const areaChart = barChart;

// 按时间范围选择合适的桶宽(秒):1h→5min, 6h→30min, 24h→1h, 7d→6h, 30d→1d
export function bucketFor(hours) {
  if (hours <= 1) return 300;
  if (hours <= 6) return 1800;
  if (hours <= 24) return 3600;
  if (hours <= 168) return 21600;
  return 86400;
}

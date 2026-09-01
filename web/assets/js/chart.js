// 零依赖 SVG 面积图:上/下行两条序列,时间轴、字节轴、悬停提示。
import { fmtBytes } from './ui.js';

export function areaChart(container, points, opts = {}) {
  const W = 800, H = opts.height || 220, PL = 56, PR = 12, PT = 12, PB = 28;
  const iw = W - PL - PR, ih = H - PT - PB;
  const n = points.length;
  if (!n) { container.innerHTML = '<div class="empty">暂无流量数据</div>'; return; }

  const max = Math.max(1, ...points.map(p => Math.max(p.up, p.down)));
  const x = i => PL + (n === 1 ? iw / 2 : i / (n - 1) * iw);
  const y = v => PT + ih - v / max * ih;

  const line = key => points.map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(p[key]).toFixed(1)}`).join(' ');
  const area = key => `${line(key)} L${x(n - 1).toFixed(1)},${(PT + ih).toFixed(1)} L${x(0).toFixed(1)},${(PT + ih).toFixed(1)} Z`;

  // Y 轴刻度(4 档)
  let grid = '';
  for (let k = 0; k <= 4; k++) {
    const v = max * k / 4, yy = y(v).toFixed(1);
    grid += `<line x1="${PL}" x2="${W - PR}" y1="${yy}" y2="${yy}" class="grid"/>`;
    grid += `<text x="${PL - 6}" y="${yy}" class="ylab" text-anchor="end" dominant-baseline="middle">${fmtBytes(v, 0)}</text>`;
  }
  // X 轴刻度(约 6 个)
  const span = points[n - 1].t - points[0].t;
  const showDate = span > 36 * 3600;
  let xl = '';
  const step = Math.max(1, Math.floor(n / 6));
  for (let i = 0; i < n; i += step) {
    const d = new Date(points[i].t * 1000);
    const label = showDate
      ? `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:00`
      : `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
    xl += `<text x="${x(i).toFixed(1)}" y="${H - 8}" class="xlab" text-anchor="middle">${label}</text>`;
  }

  container.innerHTML = `
    <div class="chart-wrap">
      <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" class="chart">
        ${grid}
        <path d="${area('down')}" class="area down"/>
        <path d="${area('up')}" class="area up"/>
        <path d="${line('down')}" class="line down"/>
        <path d="${line('up')}" class="line up"/>
        ${xl}
        <line class="cursor" x1="0" x2="0" y1="${PT}" y2="${PT + ih}" hidden/>
      </svg>
      <div class="chart-tip" hidden></div>
      <div class="chart-legend"><span class="lg up">${opts.upLabel || '上行'}</span><span class="lg down">${opts.downLabel || '下行'}</span></div>
    </div>`;

  const svg = container.querySelector('svg');
  const tip = container.querySelector('.chart-tip');
  const cursor = svg.querySelector('.cursor');
  svg.addEventListener('mousemove', e => {
    const r = svg.getBoundingClientRect();
    const px = (e.clientX - r.left) / r.width * W;
    let i = Math.round((px - PL) / iw * (n - 1));
    i = Math.max(0, Math.min(n - 1, i));
    const p = points[i];
    cursor.setAttribute('x1', x(i)); cursor.setAttribute('x2', x(i)); cursor.hidden = false;
    const d = new Date(p.t * 1000);
    tip.innerHTML = `<div class="tt">${d.toLocaleString('zh-CN', { hour12: false })}</div><div>↑ ${fmtBytes(p.up)}</div><div>↓ ${fmtBytes(p.down)}</div>`;
    tip.hidden = false;
    const left = (e.clientX - r.left);
    tip.style.left = Math.min(left + 12, r.width - 150) + 'px';
    tip.style.top = (e.clientY - r.top - 10) + 'px';
  });
  svg.addEventListener('mouseleave', () => { tip.hidden = true; cursor.hidden = true; });
}

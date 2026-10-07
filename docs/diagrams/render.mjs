// 把 docs/diagrams/*.mmd(Mermaid 源文件)渲染成 README 里用的图片,明暗各一张。开发工具,不参与发布二进制。
//
// 为什么不直接在 README 里写 ```mermaid:GitHub 的手机 App 不渲染 Mermaid,只显示源码;网页版又会因为
// 量字宽的字体和显示的字体不一致把中文标签截掉。预先渲染成图片,哪里看都一样。
//
// 依赖 playwright-core(不下载浏览器,用本机 / CI 上已安装的 Chrome):
//   npm i --no-save playwright-core && node docs/diagrams/render.mjs
// 改了 .mmd 就重跑一次,把生成的 png 一起提交。
import { chromium } from 'playwright-core';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const DIR = dirname(fileURLToPath(import.meta.url));
const MERMAID = 'https://cdn.jsdelivr.net/npm/mermaid@11.17.2/dist/mermaid.esm.min.mjs'; // 固定版本,出图可复现
const THEMES = {
  light: { theme: 'default', bg: '#ffffff' },
  dark: { theme: 'dark', bg: '#0d1117' }, // GitHub 暗色页面的底色:图贴在页面上看不出边
};
const SCALE = 2; // 两倍图,高分屏上不糊

const only = process.argv[2]; // 可选:只渲染名字里带这个字符串的
const files = readdirSync(DIR).filter(f => f.endsWith('.mmd') && (!only || f.includes(only))).sort();
if (!files.length) throw new Error('docs/diagrams 里没有 .mmd');

const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1600, height: 1000 }, deviceScaleFactor: SCALE });
const sizes = {};
for (const f of files) {
  const code = readFileSync(join(DIR, f), 'utf8').replace(/\r\n/g, '\n');
  for (const [mode, t] of Object.entries(THEMES)) {
    await page.setContent(`<!doctype html><meta charset="utf-8">
<style>body{margin:0;background:${t.bg}} #w{display:inline-block;padding:12px 16px;background:${t.bg}} #w svg{display:block}</style>
<div id="w"><pre class="mermaid"></pre></div>`);
    await page.$eval('pre.mermaid', (el, c) => { el.textContent = c; }, code);
    // 参数与 GitHub 渲染 Mermaid 时一致(图里的 frontmatter 照样生效),只把流程图四周的留白从 48 收到 12:
    // 图要在手机上缩小显示,留白越少字越大
    const err = await page.evaluate(async ({ url, theme }) => {
      const { default: mermaid } = await import(url);
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme, flowchart: { diagramPadding: 12 }, sequence: { diagramMarginY: 40 } });
      try { await mermaid.run({ querySelector: 'pre.mermaid' }); return ''; } catch (e) { return String(e && e.message || e); }
    }, { url: MERMAID, theme: t.theme });
    if (err) throw new Error(`${f}(${mode})渲染失败:${err}`);
    // Mermaid 给 svg 设了 width:100% 与 max-width:<自然宽度>;按自然宽度出图,不被视口压缩
    await page.$eval('#w svg', svg => { const w = parseFloat(svg.style.maxWidth) || svg.getBoundingClientRect().width; svg.style.width = w + 'px'; svg.style.maxWidth = 'none'; });
    const out = f.replace(/\.mmd$/, `-${mode}.png`);
    const el = await page.$('#w');
    const box = await el.boundingBox();
    writeFileSync(join(DIR, out), await el.screenshot({ type: 'png' }));
    sizes[f] = Math.round(box.width);
    console.log(`${out}  ${Math.round(box.width)}×${Math.round(box.height)}`);
  }
}
await browser.close();
// README 里 <img width> 填的就是这里的宽度(CSS 像素;文件是它的两倍)
writeFileSync(join(DIR, 'sizes.json'), JSON.stringify(sizes, null, 2) + '\n');

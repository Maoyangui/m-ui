// README 与文档站用的面板截图:对着演示站(site/dist/demo,和线上的 Live Demo 是同一份界面与数据)拍。
// 不起任何服务:页面文件由本脚本从 site/dist 回填,演示数据都是 example.com 与文档保留地址,不用再抹。
//
//   node site/build.mjs                       # 先生成 site/dist
//   npm i --no-save playwright-core           # 用本机 / CI 上已装的 Chrome,不下载浏览器
//   node docs/screenshots/demo-shots.mjs      # 输出到 docs/screenshots/(OUT 可改)
//
// 拍的是:概览、线路、服务器、用户、用户详情、线路编辑里的分流规则、手机宽度的线路页,中英各一套。
// 落地页(landing*.png)是服务端渲染的,演示站里没有,要对着一台真的面板拍(演示用户 + 文档域名)。
// 演示站自己的横幅、限时彩蛋、积分提示、快速开始条不进截图。
import { chromium } from 'playwright-core';
import { readFileSync, existsSync, statSync, mkdirSync } from 'node:fs';
import { join, extname, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const DIST = process.env.DIST || join(ROOT, 'site', 'dist');
const OUT = process.env.OUT || join(ROOT, 'docs', 'screenshots');
if (!existsSync(join(DIST, 'demo', 'index.html'))) { console.error('没有 ' + join(DIST, 'demo') + ':先运行 node site/build.mjs'); process.exit(2); }
mkdirSync(OUT, { recursive: true });

const BASE = '/m-ui/', origin = 'https://demo.invalid';
const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.png': 'image/png', '.svg': 'image/svg+xml' };
const HIDE = '#demo-bar,.rw-tip,#xmas-slot,#toast{display:none!important}';
// 打开带分流规则的那条线路的编辑框,展开「分流规则」
const openRouteRules = async page => {
  await page.evaluate(() => {
    const S = globalThis.__demoState || {};
    const l = (S.lines || []).find(x => x.routeRules && x.routeRules.length);
    const b = (l && document.querySelector(`[data-act="line.edit"][data-id="${l.id}"]`)) || document.querySelector('[data-act="line.edit"]');
    if (!b) throw new Error('线路页上没有编辑按钮');
    b.click();
  });
  await page.waitForTimeout(1200);
  await page.evaluate(() => {
    const a = document.getElementById('f-rr-acc');
    if (!a) throw new Error('线路编辑框里没有分流规则那一块');
    a.open = true;
    a.scrollIntoView({ block: 'center' });
  });
  await page.waitForTimeout(800);
};
const SHOTS = [
  { name: 'dashboard', route: 'dashboard' },
  { name: 'lines', route: 'lines' },
  { name: 'nodes', route: 'nodes' },
  { name: 'users', route: 'users' },
  { name: 'user-detail', route: 'users', before: async page => { await page.locator('[data-act="user.detail"]').first().click(); await page.waitForTimeout(1500); } },
  { name: 'route-rules', route: 'lines', before: openRouteRules, clip: '#f-rr-acc' },
  { name: 'mobile-lines', route: 'lines', w: 390, h: 844, dsf: 2 },
];

const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });
let failed = 0;
for (const lang of ['zh', 'en']) {
  for (const s of SHOTS) {
    const ctx = await browser.newContext({ viewport: { width: s.w || 1440, height: s.h || 900 }, deviceScaleFactor: s.dsf || 1, colorScheme: 'light', timezoneId: 'Asia/Tokyo', locale: lang === 'zh' ? 'zh-CN' : 'en-US' });
    await ctx.route('**/*', route => {
      const url = new URL(route.request().url());
      if (url.origin !== origin) return route.abort('blockedbyclient'); // 不联网:字体、统计之类一律不要
      let file = join(DIST, decodeURIComponent(url.pathname).slice(BASE.length));
      if (existsSync(file) && statSync(file).isDirectory()) file = join(file, 'index.html');
      if (!existsSync(file)) return route.fulfill({ status: 404, body: 'not found' });
      let body = readFileSync(file);
      // 演示站的内存状态挂出来,给「找带分流规则的那条线路」用(只在回填时加这一行,站点文件不变)
      if (url.pathname.endsWith('/demo/js/demo.js')) body = body.toString('utf8') + '\n;globalThis.__demoState = S;\n';
      return route.fulfill({ status: 200, contentType: TYPES[extname(file)] || 'application/octet-stream', body });
    });
    await ctx.addInitScript(l => {
      try {
        localStorage.setItem('m-ui-lang', l);
        localStorage.setItem('m-ui-theme', 'light');
        localStorage.setItem('m-ui.hideQuickStart', '1');
        localStorage.setItem('m-ui.starDismissed', '1');
        localStorage.setItem('m-ui-xmas-seen', '1');
      } catch { /* 存不了就按默认 */ }
    }, lang);
    const page = await ctx.newPage();
    const file = join(OUT, s.name + (lang === 'zh' ? '' : '-en') + '.png');
    try {
      await page.goto(`${origin}${BASE}demo/#/${s.route}`, { waitUntil: 'networkidle' });
      await page.waitForTimeout(1500);
      await page.addStyleTag({ content: HIDE });
      if (s.before) await s.before(page);
      if (s.clip) await page.locator(s.clip).first().screenshot({ path: file });
      else await page.screenshot({ path: file });
      console.log('拍好', s.name + (lang === 'zh' ? '' : '-en'));
    } catch (e) {
      failed++;
      console.error('没拍成', s.name, lang, String(e).slice(0, 200));
    }
    await ctx.close();
  }
}
await browser.close();
process.exit(failed ? 1 : 0);

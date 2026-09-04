// 用无头 Chrome 给面板拍演示截图。开发工具,不参与发布二进制。
//
// 依赖 playwright-core(不下载浏览器,直接用本机 / CI 上已安装的 Chrome):
//   npm i --no-save playwright-core && node docs/screenshots/shoot.mjs
// 环境变量:
//   MUI_URL   面板地址(默认 http://127.0.0.1:19053/app/)
//   MUI_SUB   订阅地址前缀(默认 http://127.0.0.1:19056/sub/)
//   MUI_PASS  管理员密码(用户名 admin)
//   OUT       输出目录(默认 docs/screenshots)
//   SCRUB     逗号分隔的字符串,截图前会从页面文字里替换掉(本机名、真实 IP 之类)
import { chromium } from 'playwright-core';
import { mkdirSync } from 'node:fs';

const BASE = process.env.MUI_URL || 'http://127.0.0.1:19053/app/';
const SUB = process.env.MUI_SUB || 'http://127.0.0.1:19056/sub/';
const OUT = process.env.OUT || 'docs/screenshots';
const PASS = process.env.MUI_PASS || '';
const SCRUB = (process.env.SCRUB || '').split(',').map(s => s.trim()).filter(Boolean);
mkdirSync(OUT, { recursive: true });

const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });

// 把页面里不该出现在公开截图里的文字换掉:本机主机名、探测到的公网 IP(IPv6 一律换成文档地址)
async function scrub(page) {
  await page.evaluate(({ words }) => {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    const v6 = /\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}\b/gi;
    let n;
    while ((n = walker.nextNode())) {
      let s = n.nodeValue;
      for (const w of words) if (w) s = s.split(w).join('demo');
      s = s.replace(v6, m => (m.includes('::') || m.split(':').length > 3) ? '2001:db8::10' : m);
      if (s !== n.nodeValue) n.nodeValue = s;
    }
  }, { words: SCRUB });
}

async function panel(lang) {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, locale: lang === 'zh' ? 'zh-CN' : 'en-US' });
  const page = await ctx.newPage();
  await page.goto(BASE);
  await page.evaluate(async ({ pass, lang }) => {
    localStorage.setItem('m-ui-lang', lang);
    localStorage.setItem('m-ui-theme', 'light');
    localStorage.setItem('m-ui.hideQuickStart', '1');
    localStorage.setItem('m-ui.starDismissed', '1');
    await fetch('api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'admin', password: pass }) });
  }, { pass: PASS, lang });
  const suffix = lang === 'zh' ? '' : '-en';
  const shot = async (hash, name, before) => {
    await page.goto(BASE + '#/' + hash);
    await page.waitForTimeout(1800);
    if (before) await before();
    await scrub(page);
    await page.screenshot({ path: `${OUT}/${name}${suffix}.png` });
    console.log('拍好', name + suffix);
  };
  await page.goto(BASE + '#/dashboard');
  await page.reload();
  await page.waitForTimeout(2500);
  await shot('dashboard', 'dashboard');
  await shot('lines', 'lines');
  await shot('nodes', 'nodes');
  await shot('users', 'users');
  await shot('users', 'user-detail', async () => {
    await page.click('#users-body tr:first-child [data-act="user.detail"]');
    await page.waitForTimeout(1500);
  });
  await ctx.close();
}

async function landing(lang) {
  const ctx = await browser.newContext({ viewport: { width: 430, height: 900 }, deviceScaleFactor: 2, locale: lang === 'zh' ? 'zh-CN' : 'en-US' });
  const page = await ctx.newPage();
  await page.goto(SUB + 'demo-alice', { waitUntil: 'networkidle' });
  await page.waitForTimeout(800);
  await scrub(page);
  await page.screenshot({ path: `${OUT}/landing${lang === 'zh' ? '' : '-en'}.png`, fullPage: false });
  console.log('拍好 landing' + (lang === 'zh' ? '' : '-en'));
  await ctx.close();
}

await panel('zh');
await panel('en');
await landing('zh');
await landing('en');
await browser.close();

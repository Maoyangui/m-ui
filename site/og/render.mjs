// 把 og.html 拍成 1200×630 的 site/assets/og.png(OpenGraph / Twitter Card 用)。
// 需要 playwright-core 与本机 Chrome:npm i --no-save playwright-core && node site/og/render.mjs
import { chromium } from 'playwright-core';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const browser = await chromium.launch({ channel: process.env.CHROME_CHANNEL || 'chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
await page.goto(pathToFileURL(join(here, 'og.html')).href, { waitUntil: 'load' });
await page.waitForTimeout(500);
await page.screenshot({ path: join(here, '..', 'assets', 'og.png'), type: 'png' });
await browser.close();
console.log('已生成 site/assets/og.png');

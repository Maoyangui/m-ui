// 落地页 logo 上传(web/assets/js/logo.js)的前端测试:用桩模块加载真实的 logo.js。
// 运行:node --test web/jstest/logo.test.mjs(CI 里跑)。文件放在 web/assets 之外:那个目录整个嵌进二进制、对外提供。
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = process.env.LOGO_JS || path.join(here, '..', 'assets', 'js', 'logo.js');

// ---- 桩模块:logo.js 只从这几个模块取东西 ----
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'logo-test-'));
fs.copyFileSync(src, path.join(dir, 'logo.js'));
const stub = (name, body) => fs.writeFileSync(path.join(dir, name), body);
stub('api.js', `export const get = async () => ({});
export const post = async () => ({});
export const del = async () => ({});`);
stub('i18n.js', `export const t = (k) => k;`);
stub('ui.js', `export const esc = (s) => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
export const toast = () => {};
export const confirm = async () => true;
export const registerActions = () => {};
export const fmtBytes = (n) => String(n);`);
const { svgLength, logoBlock } = await import(pathToFileURL(path.join(dir, 'logo.js')).href);
after(() => fs.rmSync(dir, { recursive: true, force: true }));

const near = (got, want) => assert.ok(Math.abs(got - want) < 1e-6, `${got} 应为 ${want}`);

// width="100mm" 又没有 viewBox 的 SVG(老版 Inkscape 等导出的):内容按 378px 画,以前被当成没写尺寸、
// 补成 0 0 300 150 的 viewBox,右边裁掉一截、下面多出空白。绝对单位要按 CSS 换算(1in = 96px)。
test('SVG 宽高的绝对单位按 CSS 换算成 px', () => {
  near(svgLength('120'), 120);
  near(svgLength('120px'), 120);
  near(svgLength(' 120.5PX '), 120.5);
  near(svgLength('100mm'), 9600 / 25.4);
  near(svgLength('30mm'), 2880 / 25.4);
  near(svgLength('2in'), 192);
  near(svgLength('72pt'), 96);
  near(svgLength('6pc'), 96);
  near(svgLength('2.54cm'), 96);
  near(svgLength('40Q'), 9600 / 254);
  near(svgLength('.5in'), 48);
  near(svgLength('1e2'), 100);
  near(svgLength('+3px'), 3);
});

test('相对单位与写错的按没写尺寸处理(返回 NaN)', () => {
  for (const v of ['100%', '1em', '2rem', '50vw', '100 mm', '-10', 'auto', '', null, undefined, 'abc', '10px20', '1e999', '.']) {
    assert.ok(Number.isNaN(svgLength(v)), `${JSON.stringify(v)} 应为 NaN`);
  }
});

// 落地页跟随访客系统的深色模式:预览并排画浅色、深色两种背景,上传的人看得出深色 / 白色 logo 在哪种底色上看不清。
test('logo 预览并排画落地页的浅色、深色两种背景', () => {
  for (const readonly of [false, true]) {
    const html = logoBlock('x', { readonly });
    assert.match(html, /<figure class="lp light" hidden><img id="x-img" alt=""><figcaption>set\.logoLight<\/figcaption><\/figure>/);
    assert.match(html, /<figure class="lp dark" hidden><img id="x-img-dark" alt=""><figcaption>set\.logoDark<\/figcaption><\/figure>/);
    assert.equal(html.includes('data-act="logo.pick"'), !readonly, '副机只读:没有上传按钮');
  }
});

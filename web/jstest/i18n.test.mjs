// 界面文案(web/assets/js/i18n.js):中英两份的键要一一对应、占位一致;同一句里一个占位可以出现几次,
// t() 每一处都要换掉(以前只换第一处,第二处会原样显示 {port})。后端返回的中文错误另由 web/errmsg_test.go 核对 errmsg.js。
// 运行:node --test web/jstest/i18n.test.mjs(CI 里和其它前端测试一起跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = process.env.I18N_JS || path.join(here, '..', 'assets', 'js', 'i18n.js');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'i18n-test-'));
// 只取两本字典(文件末尾的 t / setLang 要读 localStorage,这里用不着)
const text = fs.readFileSync(src, 'utf8');
fs.writeFileSync(path.join(dir, 'dicts.mjs'), text.slice(0, text.indexOf('const dicts')) + '\nexport { zh, en };\n');
const { zh, en } = await import(pathToFileURL(path.join(dir, 'dicts.mjs')).href);

const holes = s => (String(s).match(/\{[A-Za-z0-9_]+\}/g) || []);

test('中英两份的键一一对应', () => {
  assert.deepEqual(Object.keys(zh).filter(k => !(k in en)), [], '只有中文的键');
  assert.deepEqual(Object.keys(en).filter(k => !(k in zh)), [], '只有英文的键');
});

test('中英占位一致', () => {
  const bad = Object.keys(zh).filter(k => k in en && holes(zh[k]).sort().join() !== holes(en[k]).sort().join());
  assert.deepEqual(bad, []);
});

test('t() 把同一个占位的每一处都换掉(同一句里一个占位可以出现几次)', async () => {
  // 整个 i18n.js 跑一遍:末尾读 localStorage / navigator,给个最小替身
  // (新版 Node 自带一个没配存储文件就不能用的 localStorage,直接盖掉)
  Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => null, setItem() {} }, configurable: true, writable: true });
  if (!globalThis.navigator) Object.defineProperty(globalThis, 'navigator', { value: { language: 'zh-CN' }, configurable: true });
  fs.writeFileSync(path.join(dir, 'full.mjs'), text);
  const { t, setLang } = await import(pathToFileURL(path.join(dir, 'full.mjs')).href);
  let multi = 0;
  for (const [name, d] of [['zh', zh], ['en', en]]) {
    setLang(name);
    for (const [k, v] of Object.entries(d)) {
      const h = holes(v);
      if (!h.length) continue;
      if (new Set(h).size !== h.length) multi++;
      const vars = Object.fromEntries(h.map(x => [x.slice(1, -1), '@@']));
      const out = t(k, vars);
      assert.ok(!h.some(x => out.includes(x)), `${name}.${k} 还剩占位: ${out}`);
    }
  }
  assert.ok(multi > 0, '至少有一句用了重复的占位(否则这条测不到什么)');
});

// 模板里直接插 ${t('…')}(没经过 esc)的文案进的是 innerHTML:里面的 "<字母" 会被当成标签吞掉(审查:英文概览的
// "tcpdump -ni any port <port>"、"udp portrange <from>-<to>" 显示成 "port ,"、"portrange -")。
// 这类键(含 ${t('前缀.' + …)} 拼出来的整组)中英两份都不能有;要写尖括号占位的文案,用的地方包 esc()。
test('直接插进 HTML 的文案里没有会被当成标签的 "<字母"', () => {
  const root = path.join(here, '..', 'assets', 'js');
  const walk = d => fs.readdirSync(d, { withFileTypes: true }).flatMap(e => e.isDirectory() ? walk(path.join(d, e.name)) : [path.join(d, e.name)]);
  const tagLike = /<[A-Za-z!/?]/;
  const bad = [];
  for (const f of walk(root).filter(f => f.endsWith('.js') && path.basename(f) !== 'i18n.js')) {
    const code = fs.readFileSync(f, 'utf8');
    for (const m of code.matchAll(/\$\{\s*t\(\s*'([^']*)'\s*(\+)?/g)) {
      const keys = m[2] ? Object.keys(en).filter(k => k.startsWith(m[1])) : [m[1]];
      for (const k of keys) {
        for (const [name, d] of [['zh', zh], ['en', en]]) {
          if (tagLike.test(String(d[k] ?? ''))) bad.push(`${path.relative(root, f)}: ${name}.${k}`);
        }
      }
    }
  }
  assert.deepEqual([...new Set(bad)], []);
});

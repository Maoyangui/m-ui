// 界面文案(web/assets/js/i18n.js):中英两份的键要一一对应、占位一致,同一句里一个占位只能出现一次
// (t() 只替换第一处,第二处会原样显示 {port})。后端返回的中文错误另由 web/errmsg_test.go 核对 errmsg.js。
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

test('同一句里一个占位只出现一次(t() 只替换第一处)', () => {
  const bad = [];
  for (const [name, d] of [['zh', zh], ['en', en]]) {
    for (const [k, v] of Object.entries(d)) {
      const h = holes(v);
      if (new Set(h).size !== h.length) bad.push(`${name}.${k}`);
    }
  }
  assert.deepEqual(bad, []);
});

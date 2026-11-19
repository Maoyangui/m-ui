// 节点设置页默认展开还是收起(审查后调整:原来一律收起,只有两三条线路的面板也要多点一次)。
import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const { expandByDefault, SMALL_SETUP } = await import(pathToFileURL(path.join(here, '..', 'assets', 'js', 'snfold.js')).href);

test('节点不多时默认全展开,多了才按线路收起', () => {
  assert.equal(SMALL_SETUP, 12);
  assert.equal(expandByDefault([3, 3, 3]), true, '三条线路各三个节点:一屏放得下,全展开');
  assert.equal(expandByDefault([4, 4, 4]), true, '正好 12 个:还算放得下');
  assert.equal(expandByDefault([4, 4, 5]), false, '13 个:按线路收起');
  assert.equal(expandByDefault(Array(72).fill(2)), false, '生产量级(72 条线路):收起');
  assert.equal(expandByDefault([]), true, '还没有线路:无所谓,不报错');
});

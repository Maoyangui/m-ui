// 设置 → 界面(用户 11-21):侧栏隐藏的栏目、简洁模式的判断(web/assets/js/uiprefs.js,纯函数,直接用真的)。
// 运行:node --test web/jstest/uiprefs.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { navHidden, normNavHidden, visibleNav, isConcise, NAV_LOCKED } from '../assets/js/uiprefs.js';

const ORDER = ['dashboard', 'lines', 'subnodes', 'upstreams', 'exts', 'nodes', 'users', 'resellers', 'plans', 'rules', 'cert', 'backup', 'ops', 'fw', 'logs', 'admin', 'settings'];

test('隐藏的栏目:写法同后端(小写字母数字),「设置」永远不算隐藏', () => {
  assert.equal(NAV_LOCKED, 'settings');
  assert.deepEqual([...navHidden({ uiNavHidden: ' Exts ,settings,rules,x-y,,rules' })], ['exts', 'rules']);
  assert.deepEqual([...navHidden({})], []);
  assert.deepEqual([...navHidden(null)], []);
});

test('存进设置:按侧栏顺序、去重、去掉「设置」和不认得的', () => {
  assert.equal(normNavHidden(['rules', 'exts', 'settings', 'nope', 'exts'], ORDER), 'exts,rules');
  assert.equal(normNavHidden([], ORDER), '');
});

test('侧栏露出来的栏目:藏光了也至少留「设置」', () => {
  assert.deepEqual(visibleNav(ORDER, { uiNavHidden: 'dashboard,lines' })[0], 'subnodes');
  const all = ORDER.filter(x => x !== 'settings').join(',');
  assert.deepEqual(visibleNav(ORDER, { uiNavHidden: all + ',settings' }), ['settings']);
});

test('简洁模式:和后端开关同一个判法,默认关', () => {
  for (const v of ['true', 'TRUE', '1', 'on', 'yes']) assert.equal(isConcise({ uiConcise: v }), true, v);
  for (const v of ['false', '0', '', 'off', undefined]) assert.equal(isConcise({ uiConcise: v }), false, String(v));
  assert.equal(isConcise(undefined), false);
});

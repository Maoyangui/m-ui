// 概览"最近入站连接"按服务器公平显示(web/assets/js/fair.js):一台正被扫描的服务器不能把别的服务器的记录挤出前几行
// (审查:后端 fairMerge 只在超过接口上限时才轮流取,界面又只显示前 15 / 30 行)。
// 运行:node --test web/jstest/fair.test.mjs(CI 里和其它前端测试一起跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const { fairPick } = await import(pathToFileURL(process.env.FAIR_JS || path.join(here, '..', 'assets', 'js', 'fair.js')).href);

const scan = (n, node, server, from) => Array.from({ length: n }, (_, i) => ({ ip: `198.51.100.${i + 1}`, nodeId: node, server, ts: from - i, failed: 'tls' }));

test('审查的场景:A 有 40 条更新的扫描失败,B 几分钟前只有 1 条:前 15 行里有 B 那条', () => {
  const now = 1_800_000_000;
  const quiet = { ip: '2001:db8::5', nodeId: 2, server: 'B', ts: now - 300, failed: 'auth' };
  const list = [...scan(40, 1, 'A', now), quiet]; // 接口按时间排:B 那条在第 41 名
  assert.ok(!list.slice(0, 15).includes(quiet)); // 以前按时间截断:看不到
  const got = fairPick(list, 15);
  assert.equal(got.length, 15);
  assert.ok(got.includes(quiet));
  assert.equal(got.filter(c => c.nodeId === 1).length, 14); // A 补满其余的,取的是 A 最近的 14 条
  assert.deepEqual(got.filter(c => c.nodeId === 1).map(c => c.ts), scan(14, 1, 'A', now).map(c => c.ts));
  assert.deepEqual(got.map(c => c.ts), got.map(c => c.ts).slice().sort((a, b) => b - a)); // 显示仍按时间从新到旧
});

test('只有一台服务器、或总数没超过要显示的行数:和按时间取前 n 条一样', () => {
  const one = scan(20, 1, '', 1000);
  assert.deepEqual(fairPick(one, 15), one.slice(0, 15));
  const few = [...scan(3, 1, 'A', 1000), ...scan(2, 2, 'B', 500)].sort((a, b) => b.ts - a.ts);
  assert.deepEqual(fairPick(few, 15), few);
  assert.deepEqual(fairPick([], 15), []);
  assert.deepEqual(fairPick(undefined, 15), []);
});

test('几台都有很多:轮流取,每台分到的条数差不超过一条,最近活动过的服务器先取', () => {
  const list = [...scan(20, 1, 'A', 1000), ...scan(20, 2, 'B', 2000), ...scan(20, 3, 'C', 500)];
  const got = fairPick(list, 10);
  const per = id => got.filter(c => c.nodeId === id).length;
  assert.deepEqual([per(2), per(1), per(3)], [4, 3, 3]); // B 最近,多分到余下的那一条
  // 没带 nodeId 的(老数据)按服务器名分组
  const named = [...scan(20, undefined, 'A', 1000), ...scan(1, undefined, 'B', 1)];
  assert.ok(fairPick(named, 5).some(c => c.server === 'B'));
});

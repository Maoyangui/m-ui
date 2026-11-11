// 概览"最近入站连接"按服务器公平显示(web/assets/js/fair.js):一台正被扫描的服务器不能把别的服务器的记录挤出前几行
// (审查:后端 fairMerge 只在超过接口上限时才筛,界面又只显示前 15 / 30 行);也不能反过来让别的服务器几天前的旧记录
// 挤掉忙的那台刚产生的记录(审查:严格轮流时三台服务器的主表里忙的那台只剩 10 行)。
// 运行:node --test web/jstest/fair.test.mjs(CI 里和其它前端测试一起跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const { fairPick, shownConns, CONN_ROWS, FAIL_ROWS } = await import(pathToFileURL(process.env.FAIR_JS || path.join(here, '..', 'assets', 'js', 'fair.js')).href);

const scan = (n, node, server, from, step = 1) => Array.from({ length: n }, (_, i) => ({ ip: `198.51.100.${i + 1}`, nodeId: node, server, ts: from - i * step, failed: 'tls' }));
const ok = list => list.map(c => ({ ...c, failed: undefined, user: 'u', line: 'l' }));

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

test('几台都有很多:每台先留最近的一条,其余按时间取', () => {
  const list = [...scan(20, 1, 'A', 1000), ...scan(20, 2, 'B', 2000), ...scan(20, 3, 'C', 500)];
  const got = fairPick(list, 10);
  const per = id => got.filter(c => c.nodeId === id).length;
  assert.deepEqual([per(2), per(1), per(3)], [8, 1, 1]); // A、C 各留最近那条,其余都是 B 的(B 的记录最新)
  assert.deepEqual(got.filter(c => c.nodeId === 2).map(c => c.ts), scan(8, 2, 'B', 2000).map(c => c.ts));
  assert.ok(got.some(c => c.nodeId === 1 && c.ts === 1000) && got.some(c => c.nodeId === 3 && c.ts === 500));
  // 服务器比行数还多:先给最近活动过的服务器
  const many = Array.from({ length: 5 }, (_, i) => scan(3, i + 1, 'S' + i, 100 * (i + 1))).flat();
  assert.deepEqual(fairPick(many, 3).map(c => c.nodeId), [5, 4, 3]);
  // 没带 nodeId 的(老数据)按服务器名分组
  const named = [...scan(20, undefined, 'A', 1000), ...scan(1, undefined, 'B', 1)];
  assert.ok(fairPick(named, 5).some(c => c.server === 'B'));
});

test('审查的场景:A 最近 40 分钟有 40 条,B、C 各 20 条都是两天前的:前 30 行里 A 最近的记录都在,B、C 各留最近一条', () => {
  const now = 1_800_000_000, twoDays = 2 * 86400;
  const a = ok(scan(40, 1, 'A', now, 60)); // 每分钟一条
  const list = [...a, ...ok(scan(20, 2, 'B', now - twoDays)), ...ok(scan(20, 3, 'C', now - twoDays))].sort((x, y) => y.ts - x.ts);
  const got = fairPick(list, 30);
  assert.equal(got.length, 30);
  assert.deepEqual(got.filter(c => c.nodeId === 1).map(c => c.ts), a.slice(0, 28).map(c => c.ts)); // A 最近的 28 条一条不少
  assert.ok(got.some(c => c.nodeId === 1 && c.ts === now - 11 * 60)); // 11 分钟前那条(按时间排第 12)看得到
  assert.deepEqual([2, 3].map(id => got.filter(c => c.nodeId === id).map(c => c.ts)), [[now - twoDays], [now - twoDays]]);
});

test('概览两张表各显示哪几行(shownConns):通过认证的取 30 行、到了没过的取 15 行,两张表各自按服务器公平取', () => {
  assert.equal(CONN_ROWS, 30);
  assert.equal(FAIL_ROWS, 15);
  const now = 1_800_000_000;
  const quietOk = { ip: '2001:db8::5', nodeId: 2, server: 'B', ts: now - 300, user: 'u', line: 'l' };
  const quietFail = { ip: '2001:db8::6', nodeId: 2, server: 'B', ts: now - 300, failed: 'auth' };
  // 接口给的:A 有 40 条更新的连接和 40 条更新的扫描失败,B 各只有一条,都排在第 41 名
  const all = [...ok(scan(40, 1, 'A', now)), quietOk, ...scan(40, 1, 'A', now), quietFail];
  const { rows, fails } = shownConns(all);
  assert.equal(rows.length, 30);
  assert.equal(fails.length, 15);
  assert.ok(rows.includes(quietOk) && !rows.some(c => c.failed));
  assert.ok(fails.includes(quietFail) && fails.every(c => c.failed));
  assert.deepEqual(shownConns(undefined), { rows: [], fails: [] });
  assert.deepEqual(shownConns({}), { rows: [], fails: [] }); // 官网演示的假接口回的是 {}
});

test('概览的两张表都经过 shownConns,不自己按时间截断(审查:合并改版的概览时换回 slice 测试也照样全过)', () => {
  const src = fs.readFileSync(process.env.DASHBOARD_JS || path.join(here, '..', 'assets', 'js', 'pages', 'dashboard.js'), 'utf8');
  assert.ok(/import\s*\{[^}]*\bshownConns\b[^}]*\}\s*from\s*'\.\.\/fair\.js'/.test(src), 'dashboard.js 没从 fair.js 引入 shownConns');
  const start = src.search(/(?:async\s+)?function\s+renderConns\s*\(/);
  assert.ok(start >= 0, '找不到 renderConns');
  const body = src.slice(start, src.indexOf('\n}\n', start));
  assert.ok(/\bshownConns\(/.test(body), 'renderConns 没用 shownConns 取两张表的行');
  assert.ok(!/\.slice\(\s*\d/.test(body), 'renderConns 里不能再按时间截断(要多显示几行就把行数传给 shownConns)');
});

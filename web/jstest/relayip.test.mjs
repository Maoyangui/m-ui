// 中转入口的来源地址(中转机的地址)不是用户的设备:面板数「在线设备」时不算(和后端 runner.relaySources 同一套)。
// 运行:node --test web/jstest/relayip.test.mjs
import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'relayip-test-'));
fs.copyFileSync(path.join(here, '..', 'assets', 'js', 'relayip.js'), path.join(dir, 'relayip.js'));
fs.writeFileSync(path.join(dir, 'app.js'), 'export const state = { nodes: [], lines: [] };');
after(() => fs.rmSync(dir, { recursive: true, force: true }));
const { state } = await import(pathToFileURL(path.join(dir, 'app.js')).href);
const { relayIPs, deviceIPs, serverOfIP } = await import(pathToFileURL(path.join(dir, 'relayip.js')).href);

test('中转入口的来源:集群中转机的地址、别处的中转填的地址、手填的来源;暂停的入口、停用的线路、域名不算', () => {
  state.nodes = [
    { id: 1, name: 'Tokyo', publicIp: '203.0.113.1' },
    { id: 2, name: 'Singapore', publicIp: '198.51.100.2', publicIp6: '2001:DB8:2::1', addr: 'relay.example.net' },
    { id: 3, name: 'Frankfurt', publicIp: '198.51.100.3', addr: '[2001:db8:3::9]' },
    { id: 4, name: 'Seoul', publicIp: '198.51.100.4' },
  ];
  state.lines = [
    { id: 1, enabled: true, forwards: JSON.stringify([
      { id: 1, via: 2, port: 30001, to: 1 },
      { id: 2, via: 0, host: '192.0.2.50', port: 30002, to: 1, src: '192.0.2.51, 192.0.2.52' },
      { id: 3, via: 4, port: 30003, to: 1, off: true },
      { id: 4, via: 0, host: 'relay.example.org', port: 30004, to: 1 },
    ]) },
    { id: 2, enabled: true, forwards: [{ id: 1, via: 3, port: 30005, to: 2 }] }, // 已经解析成数组的也认
    { id: 3, enabled: false, forwards: JSON.stringify([{ id: 1, via: 4, port: 30006, to: 1 }]) },
    { id: 4, enabled: true, forwards: 'not json' },
  ];
  assert.deepEqual([...relayIPs()].sort(), ['192.0.2.50', '192.0.2.51', '192.0.2.52', '198.51.100.2', '198.51.100.3', '2001:db8:2::1', '2001:db8:3::9'].sort());
});

test('数在线设备:去掉中转机的地址;没有中转入口时原样', () => {
  assert.deepEqual(deviceIPs(['203.0.113.77', '198.51.100.2']), ['203.0.113.77']);
  assert.deepEqual(deviceIPs(['198.51.100.2', '2001:DB8:2::1']), [], '只经中转在线的人不占设备名额');
  assert.deepEqual(deviceIPs(['198.51.100.4']), ['198.51.100.4'], '暂停的入口那台不算中转');
  assert.deepEqual(deviceIPs(null), []);
  state.lines = [];
  assert.deepEqual(deviceIPs(['198.51.100.2']), ['198.51.100.2']);
  assert.equal(serverOfIP('198.51.100.2'), 'Singapore', '标「⇄ 服务器名」的那套不变');
});

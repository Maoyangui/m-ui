// 大陆连通 / IPv6 入站的前端(web/assets/js/reach.js):用桩模块加载真实的 reach.js,核对按线路给的 IPv6 入站结论
// (审查第 1、15、20 条:UDP 线路、没测到的端口不能显示整台服务器的"通")、服务器列表那一格和详情抽屉。
// 运行:node --test web/jstest/reach.test.mjs(CI 里和其它前端测试一起跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = process.env.REACH_JS || path.join(here, '..', 'assets', 'js', 'reach.js');

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reach-test-'));
fs.copyFileSync(src, path.join(dir, 'reach.js'));
const stub = (name, body) => fs.writeFileSync(path.join(dir, name), body);
stub('api.js', `export const get = async () => globalThis.__reach; export const post = async () => ({});`);
// t 把参数也写进去,好核对用的是哪句、填了什么
stub('i18n.js', `export const t = (k, v) => v ? k + JSON.stringify(v) : k;`);
stub('errmsg.js', `export const trErr = (m) => m;`);
stub('ui.js', `export const esc = (s) => String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
export const fmtRelative = () => 'rel'; export const fmtTime = () => 'time'; export const toast = () => {};
export const openDrawer = (title, html) => { globalThis.__drawer = html; }; export const drawerOpen = () => false; export const setHTML = () => {};
export const registerActions = (a) => { globalThis.__actions = a; };`);
globalThis.document = { getElementById: () => null };
const reach = await import(pathToFileURL(path.join(dir, 'reach.js')).href);

// 服务器 1:双栈,走 IPv6 的有 trojan tj6(8443,被测的那条)、vless vl6(9443)、hysteria2 hy6(8444)
const ok = { target: '2001:db8::1', state: 'ok', port: 8443, portLine: 'tj6', udpLines: ['hy6'], at: 1, ok: 4, total: 4 };
const tj6 = { name: 'tj6', protocol: 'trojan', port: 8443 };
const vl6 = { name: 'vl6', protocol: 'vless', port: 9443 };
const hy6 = { name: 'hy6', protocol: 'hysteria2', port: 8444 };

test('按线路的结论:只有被测的那条线路给通 / 不通;UDP 线路"未测(UDP)";别的 TCP 线路"未测到此端口"', () => {
  assert.equal(reach.v6ForLine(ok, tj6), 'ok');
  assert.equal(reach.v6ForLine(ok, vl6), 'otherPort');
  assert.equal(reach.v6ForLine(ok, hy6), 'udpLine');
  assert.equal(reach.v6ForLine({ ...ok, state: 'down' }, tj6), 'down');
  assert.equal(reach.v6ForLine({ ...ok, state: 'down' }, vl6), 'otherPort'); // 反过来:别的端口不通也不借过来
  // 线路改了名、端口没变:还是这条
  assert.equal(reach.v6ForLine(ok, { ...tj6, name: 'tj6-new' }), 'ok');
  // 自定义地址的线路测的可能是 server_port:只按名字认
  assert.equal(reach.v6ForLine(ok, { ...tj6, name: 'c6', addrs: [{ server: '2001:db8::9' }] }), 'otherPort');
  assert.equal(reach.v6ForLine({ ...ok, port: 23444, portLine: 'c6' }, { name: 'c6', protocol: 'anytls', port: 7443, addrs: '[{"server":"2001:db8::9","server_port":23444}]' }), 'ok');
  // 没测成、没有测点:整台都一样,照实说
  assert.equal(reach.v6ForLine({ ...ok, state: 'error', port: 0 }, vl6), 'error');
  assert.equal(reach.v6ForLine({ ...ok, state: 'none' }, vl6), 'none');
  // 上次检测时这台走 IPv6 的只有 UDP 线路:这条 TCP 线路没测到
  assert.equal(reach.v6ForLine({ target: '2001:db8::1', state: 'udp', udpLines: ['hy6'], at: 1 }, vl6), 'otherPort');
  // 还没测过:TCP 线路没有结论;UDP 线路照样注明测不了
  assert.equal(reach.v6ForLine(null, vl6), null);
  assert.equal(reach.v6ForLine(null, hy6), 'udpLine');
});

test('徽章:被测的那条是绿色"通";UDP 线路、别的端口是灰色并在悬停提示里说清', () => {
  assert.match(reach.v6LineBadge(ok, tj6), /class="badge ok"/);
  const u = reach.v6LineBadge(ok, hy6);
  assert.match(u, /class="badge"/);
  assert.match(u, /reach\.v6\.s\.udpLine/);
  assert.match(u, /reach\.v6\.lineUdp.*8444/);
  const o = reach.v6LineBadge(ok, vl6);
  assert.match(o, /class="badge"/);
  assert.match(o, /reach\.v6\.s\.otherPort/);
  assert.match(o, /reach\.v6\.lineOther.*9443.*tj6.*8443/);
  assert.doesNotMatch(o, /badge ok/);
  assert.match(reach.v6LineBadge({ ...ok, state: 'down' }, tj6), /class="badge danger"/);
  assert.equal(reach.v6LineBadge(null, vl6), '');
});

test('IPv6 入站最近一次没有可用测点:说明用的是上一次的结果,不写成"没测成:"后面空着', () => {
  const s = reach.v6Explain({ ...ok, state: 'down', attempt: { at: 2, verdict: 'none' } });
  assert.match(s, /reach\.v6\.attemptNone/);
  assert.doesNotMatch(s, /reach\.v6\.attempt\{/);
  assert.match(reach.v6Explain({ ...ok, attempt: { at: 2, verdict: 'error', error: '测试额度已用完' } }), /reach\.v6\.attempt\{.*额度/);
});

test('无可测端口:服务器列表那一格灰色、写原因,不画三网的小格;没发请求的详情里不列运营商', async () => {
  globalThis.__reach = { results: [
    { nodeId: 1, name: 'v6only', verdict: 'noPort', error: '这台的线路在订阅里都走 IPv6,大陆连通检测暂只支持 IPv4', at: 1, groups: [{ key: 'cm', state: 'none', total: 0 }] },
    { nodeId: 2, name: 'api', verdict: 'noPort', error: '面板 API 端口连香港测点也连不上', at: 1, port: 2053, portFrom: 'api', measurements: ['m1'], groups: [{ key: 'cm', state: 'down', ok: 0, total: 2 }] },
  ], runningIds: [] };
  await reach.loadReach();
  const c1 = reach.reachCell({ id: 1, enabled: true });
  assert.match(c1, /class="badge "/);
  assert.match(c1, /reach\.v\.noPort/);
  assert.match(c1, /都走 IPv6/);
  assert.doesNotMatch(c1, /class="rc /);
  assert.equal(reach.isBad('noPort'), false);
  // 详情抽屉:没发请求的不列运营商、端口写"没测",也不提"只测了 ping";借 API 端口测过的照列,并说明 API 端口常只对主机放行
  reach.setNodes(() => [{ id: 1, name: 'v6only' }, { id: 2, name: 'api' }]);
  globalThis.__actions['reach.detail']('1');
  assert.match(globalThis.__drawer, /reach.x.noPort/);
  assert.match(globalThis.__drawer, /reach.port.untested/);
  assert.doesNotMatch(globalThis.__drawer, /reach.byCarrier|reach.pingOnly/);
  globalThis.__actions['reach.detail']('2');
  assert.match(globalThis.__drawer, /reach.byCarrier/);
  assert.match(globalThis.__drawer, /reach.apiPortNote/);
});

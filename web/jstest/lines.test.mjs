// 线路页(web/assets/js/pages/lines.js)里 IP 版本的判断:用桩模块加载真实的 lines.js,核对它和后端
// (sub.EntriesFromNodes / sub.resolveAddrs / model.FamilyIPs)给出的地址族一致。
// 运行:node --test web/jstest/lines.test.mjs(CI 里和 exts.test.mjs 一起跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = process.env.LINES_JS || path.join(here, '..', 'assets', 'js', 'pages', 'lines.js');

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lines-test-'));
fs.mkdirSync(path.join(dir, 'pages'));
fs.copyFileSync(src, path.join(dir, 'pages', 'lines.js'));
const stub = (name, body) => fs.writeFileSync(path.join(dir, name), body);
stub('app.js', `export const state = globalThis.__state; export async function load() {}`);
stub('api.js', `export const SLOW = {}; export const get = async () => ({}); export const post = get; export const put = get; export const del = get;`);
stub('i18n.js', `export const t = (k) => k;`);
stub('errmsg.js', `export const trErr = (m) => m;`);
stub('ui.js', `export const esc = (s) => String(s ?? ''); export const toast = () => {}; export const confirm = async () => true;
export const openModal = () => {}; export const registerActions = () => {}; export const badge = (s) => String(s); export const dot = () => '';
export const field = () => ''; export const check = () => ''; export const empty = () => ''; export const fv = (id) => (globalThis.__fv || {})[id] ?? '';
export const fchk = () => true; export const matches = () => true; export const debounce = (f) => f; export const setHTML = () => {};
export const chipList = (a) => a.join('');`);
stub('certpicker.js', `export const openCertDrawer = () => {}; export const certLabel = (s) => s;`);
stub('reach.js', `export const loadReach = async () => null; export const reachLoaded = () => true; export const v6Of = (id) => (globalThis.__v6 || {})[id] || null;
export const v6LineBadge = () => ''; export const v6ForLine = (v, l) => ((l.protocol === 'hysteria2' || l.protocol === 'tuic') ? 'udpLine' : v ? v.state : null);`);
stub(path.join('pages', 'upstreams.js'), `export const editRelayUpstream = () => {};`);

globalThis.__state = { settings: {}, nodes: [], lines: [] };
let cbs = [], fams = {};
globalThis.document = {
  getElementById: () => null,
  querySelectorAll: (sel) => (sel === '.node-cb' ? cbs : []),
  querySelector: (sel) => { const m = sel.match(/data-node="(\d+)"/); return m && fams[m[1]] !== undefined ? { value: fams[m[1]], disabled: false } : null; },
};
const lines = await import(pathToFileURL(path.join(dir, 'pages', 'lines.js')).href);

const dual = { id: 1, name: '双栈', publicIp: '203.0.113.1', publicIp6: '2001:db8::1' };
const dualV6 = { id: 2, name: '默认v6', publicIp: '203.0.113.2', publicIp6: '2001:db8::2', addrFamily: 'v6' };
const v6Missing = { id: 3, name: '选v6没有', publicIp: '203.0.113.3', addrFamily: 'v6' };
const manual6 = { id: 4, name: '手填v6', addr: '[2001:db8::4]', publicIp: '203.0.113.4' };
const pure6 = { id: 5, name: '纯v6', publicIp: '2001:db8::5', publicIp6: '2001:db8::5' };

test('没单独选时的地址族:和订阅一致(选了 IPv6 却没探测到就用 IPv4)', () => {
  globalThis.__state.settings = {};
  assert.equal(lines.serverFamily(dual), 'IPv4');
  assert.equal(lines.serverFamily(dualV6), 'IPv6');
  assert.equal(lines.serverFamily(v6Missing), 'IPv4');
  assert.equal(lines.serverFamily(manual6), 'IPv6');
  assert.equal(lines.serverFamily(pure6), 'IPv6');
});

test('订阅地址用域名时:默认由 DNS 决定,服务器的"自动地址用 IPv6"不算;单独选了的照样给 IP', () => {
  globalThis.__state.settings = { subServerAddr: 'domain' };
  try {
    assert.equal(lines.serverFamily(dualV6), 'dns');
    assert.equal(lines.lineUsesV6({}, dualV6), false);
    assert.equal(lines.lineUsesV6({ nodeFamily: { 1: 'v6' } }, dual), true);
  } finally { globalThis.__state.settings = {}; }
});

test('线路在这台给不给 IPv6:单独选了且有那个地址才算;自定义地址看这台拿得到的那几条里有没有写成这台自己的 IPv6', () => {
  assert.equal(lines.lineUsesV6({ nodeFamily: { 1: 'v6' } }, dual), true);
  assert.equal(lines.lineUsesV6({ nodeFamily: { 3: 'v6' } }, v6Missing), false); // 没有 IPv6:订阅里退回 IPv4
  assert.equal(lines.lineUsesV6({ nodeFamily: { 2: 'v4' } }, dualV6), false);
  assert.equal(lines.lineUsesV6({}, dualV6), true);
  assert.equal(lines.lineUsesV6({ nodeFamily: '{"1":"v6"}' }, dual), true); // 接口里是 JSON 字符串
  // 自定义地址整体替换服务器地址:写成 IPv4 / 域名的不算(单独选了 v6 也不算),写成这台自己 IPv6 的算(审查第 9 条),
  // 写法不同(大写、补零、方括号)也认
  assert.equal(lines.lineUsesV6({ nodeFamily: { 1: 'v6' }, addrs: [{ server: '1.2.3.4' }] }, dual), false);
  assert.equal(lines.lineUsesV6({ addrs: [{ server: 'hk.example.com' }] }, dualV6), false);
  assert.equal(lines.lineUsesV6({ addrs: [{ server: '::ffff:1.2.3.4' }] }, dual), false); // IPv4 映射地址按 IPv4
  assert.equal(lines.lineUsesV6({ addrs: '[{"server":"[2001:DB8:0::1]"}]' }, dual), true);
  assert.equal(lines.lineUsesV6({ addrs: [{ server: '2001:db8::4' }] }, manual6), true); // 手填的连接地址也是这台的
  // 别的 IPv6(国内中转、CDN 的入口)不算这台的,写了 node_id 也一样:这台不用为它放行 IPv6
  assert.equal(lines.lineUsesV6({ addrs: [{ server: '2001:db8::9' }] }, dual), false);
  assert.equal(lines.lineUsesV6({ addrs: [{ server: '240e:1:2::1', server_port: 30000, node_id: 1 }] }, dual), false);
  assert.equal(lines.lineUsesV6({ addrs: [{ server: '2001:db8::5' }] }, v6Missing), false); // 这台根本没有 IPv6
  // node_id 写了别的服务器的地址这台拿不到;键名不分大小写(老数据)
  assert.equal(lines.lineUsesV6({ addrs: [{ server: '2001:db8::1', node_id: 2 }] }, dual), false);
  assert.equal(lines.lineUsesV6({ addrs: [{ Server: '2001:db8::1', Node_Id: 1 }] }, dual), true);
});

test('IPv6 规范写法:补全、去前导零、小写;不是 IPv6 的返回空', () => {
  assert.equal(lines.v6Norm('[2001:DB8:0::1]'), '2001:db8:0:0:0:0:0:1');
  assert.equal(lines.v6Norm('2001:0db8::0001'), lines.v6Norm('2001:db8::1'));
  assert.equal(lines.v6Norm('::'), '0:0:0:0:0:0:0:0');
  assert.equal(lines.v6Norm('::ffff:1.2.3.4'), '0:0:0:0:0:ffff:102:304');
  assert.equal(lines.v6Norm('1:2:3:4:5:6:7:8'), '1:2:3:4:5:6:7:8');
  for (const bad of ['', '1.2.3.4', 'hk.example.com', '1:2', '1::2::3', '1:2:3:4:5:6:7:8::', '12345::1', '::1.2.3.256']) assert.equal(lines.v6Norm(bad), '', bad);
});

test('"全部服务器"的线路:单独选了 IP 版本的、以及线路在上面走 IPv6 的服务器都列出来(审查第 15 条)', () => {
  globalThis.__state.nodes = [dual, dualV6, v6Missing, pure6, { ...pure6, id: 6, enabled: false }];
  assert.deepEqual(lines.allServersShown({}), [2, 5]); // 默认给 IPv6 的两台(停用的那台不列)
  assert.deepEqual(lines.allServersShown({ nodeFamily: { 1: 'v6', 3: 'v4' } }), [1, 2, 3, 5]);
  assert.deepEqual(lines.allServersShown({ addrs: [{ server: '1.2.3.4' }] }), []);
  assert.deepEqual(lines.allServersShown({ addrs: [{ server: '2001:db8::5' }, { server: '240e::1' }] }), [5]); // 只有 5 自己的 IPv6
});

test('表单里的这条线路:名字、协议、端口、自定义地址;UDP 线路和测出不通的直接展开自检要点', () => {
  globalThis.__fv = { 'f-name': ' hy6 ', 'f-protocol': 'hysteria2', 'f-port': '8444', 'f-addrs': '' };
  const l = lines.formLine();
  assert.deepEqual(l, { name: 'hy6', protocol: 'hysteria2', port: 8444, addrs: [] });
  globalThis.__v6 = { 1: { state: 'ok' }, 2: { state: 'down' } };
  assert.equal(lines.v6TipsOpen(l, [dual]), true); // UDP:测不了,只能照着要点抓包
  assert.equal(lines.v6TipsOpen({ ...l, protocol: 'trojan' }, [dual]), false);
  assert.equal(lines.v6TipsOpen({ ...l, protocol: 'trojan' }, [dual, dualV6]), true);
  globalThis.__v6 = {};
});

test('表单:按勾选的服务器与下拉算出哪些服务器走 IPv6(全不勾 = 全部;填了自定义地址的看自定义地址)', () => {
  globalThis.__state.nodes = [dual, dualV6, v6Missing];
  cbs = [{ value: '1', checked: true }, { value: '2', checked: false }, { value: '3', checked: true }];
  fams = { 1: 'v6', 3: 'v6' };
  globalThis.__fv = { 'f-addrs': '' };
  assert.deepEqual(lines.formV6Servers().map(n => n.id), [1]);
  cbs = cbs.map(c => ({ ...c, checked: false }));
  assert.deepEqual(lines.formV6Servers().map(n => n.id), [1, 2]);
  globalThis.__fv = { 'f-addrs': '[{"server":"2001:db8::2"}]' };
  assert.deepEqual(lines.formV6Servers().map(n => n.id), [2]); // 没写 node_id:每台都拿到这个地址,但它只是 2 自己的 IPv6
  globalThis.__fv = { 'f-addrs': '[{"server":"2001:db8::9"}]' };
  assert.deepEqual(lines.formV6Servers(), []); // 中转的 IPv6:哪台都不用为它放行 IPv6
  globalThis.__fv = { 'f-addrs': '[{"server":"2001:db8::1","node_id":1},{"server":"240e::1","node_id":3}]' };
  assert.deepEqual(lines.formV6Servers().map(n => n.id), [1]);
  globalThis.__fv = { 'f-addrs': '[{"server":"1.2.3.4"}]' };
  assert.deepEqual(lines.formV6Servers(), []);
  globalThis.__fv = {};
});

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
export const fchk = () => true; export const matches = () => true; export const debounce = (f) => f; export const setHTML = () => {};`);
stub('certpicker.js', `export const openCertDrawer = () => {}; export const certLabel = (s) => s;`);
stub('reach.js', `export const loadReach = async () => null; export const reachLoaded = () => true; export const v6Of = () => null; export const v6Badge = () => '';`);
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

test('线路在这台给不给 IPv6:单独选了且有那个地址才算;自定义地址不算', () => {
  assert.equal(lines.lineUsesV6({ nodeFamily: { 1: 'v6' } }, dual), true);
  assert.equal(lines.lineUsesV6({ nodeFamily: { 3: 'v6' } }, v6Missing), false); // 没有 IPv6:订阅里退回 IPv4
  assert.equal(lines.lineUsesV6({ nodeFamily: { 2: 'v4' } }, dualV6), false);
  assert.equal(lines.lineUsesV6({}, dualV6), true);
  assert.equal(lines.lineUsesV6({ nodeFamily: '{"1":"v6"}' }, dual), true); // 接口里是 JSON 字符串
  assert.equal(lines.lineUsesV6({ nodeFamily: { 1: 'v6' }, addrs: [{ server: '1.2.3.4' }] }, dual), false);
});

test('表单:按勾选的服务器与下拉算出哪些服务器走 IPv6(全不勾 = 全部;填了自定义地址就没有)', () => {
  globalThis.__state.nodes = [dual, dualV6, v6Missing];
  cbs = [{ value: '1', checked: true }, { value: '2', checked: false }, { value: '3', checked: true }];
  fams = { 1: 'v6', 3: 'v6' };
  globalThis.__fv = { 'f-addrs': '' };
  assert.deepEqual(lines.formV6Servers().map(n => n.id), [1]);
  cbs = cbs.map(c => ({ ...c, checked: false }));
  assert.deepEqual(lines.formV6Servers().map(n => n.id), [1, 2]);
  globalThis.__fv = { 'f-addrs': '[{"server":"2001:db8::9"}]' };
  assert.deepEqual(lines.formV6Servers(), []);
});

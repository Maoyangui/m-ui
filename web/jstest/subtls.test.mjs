// 订阅 http ↔ https 切换的提示(web/assets/js/subtls.js):订阅从 http 改成 https(或反过来)时,所有用户手里的订阅地址都会变、
// 旧地址失效,客户端要重新导入。所以已有用户时「用于订阅 HTTPS」默认不勾,勾了或在「正在使用」里换成 / 换掉证书时要明确提示。
// 运行:node --test web/jstest/subtls.test.mjs(CI 里和其它前端测试一起跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const { subDefaultOn, subSchemeChange } = await import(pathToFileURL(process.env.SUBTLS_JS || path.join(here, '..', 'assets', 'js', 'subtls.js')).href);

test('首次签正式证书:还没有用户时默认勾「用于订阅 HTTPS」,已有用户时默认不勾', () => {
  assert.equal(subDefaultOn({ subCert: '', subUsers: 0 }, false), true);
  assert.equal(subDefaultOn({ subCert: '', subUsers: 3 }, false), false);
  assert.equal(subDefaultOn({ subCert: '', subUsers: 0 }, true), false, '自签证书不默认用于订阅');
  assert.equal(subDefaultOn({ subCert: '/x/sub.crt', subUsers: 0 }, false), false, '订阅已经是 https 时不再默认顶替');
  assert.equal(subDefaultOn({ subCert: '' }, false), true, '老后端不给 subUsers 时按没有用户算(和以前一样)');
});

test('勾了「用于订阅 HTTPS」:订阅还是 http 且已有用户时要提示,给出变化方向', () => {
  assert.deepEqual(subSchemeChange({ subCert: '', subUsers: 5 }, true), { from: 'http', to: 'https', n: 5 });
  assert.equal(subSchemeChange({ subCert: '', subUsers: 0 }, true), null, '没有用户不用提示');
  assert.equal(subSchemeChange({ subCert: '', subUsers: 5 }, false), null, '没勾不用提示');
  assert.equal(subSchemeChange({ subCert: '/x/sub.crt', subUsers: 5 }, true), null, '已经是 https,只是换一张证书,地址不变');
});

test('「正在使用」里给订阅换证书:http → https、https → 不用 HTTPS 都要提示,换成另一张证书不提示', () => {
  assert.deepEqual(subSchemeChange({ subCert: '', subUsers: 2 }, 'le.example.com'), { from: 'http', to: 'https', n: 2 });
  assert.deepEqual(subSchemeChange({ subCert: '/x/sub.crt', subUsers: 2 }, ''), { from: 'https', to: 'http', n: 2 });
  assert.equal(subSchemeChange({ subCert: '/x/sub.crt', subUsers: 2 }, 'other.example.com'), null);
  assert.equal(subSchemeChange({ subCert: '', subUsers: 2 }, ''), null);
});

test('设置页「订阅」组直接改证书 / 私钥路径:http ↔ https 一换就要提示,只换路径不提示', async () => {
  const { subSettingsChange } = await import(pathToFileURL(process.env.SUBTLS_JS || path.join(here, '..', 'assets', 'js', 'subtls.js')).href);
  assert.equal(typeof subSettingsChange, 'function', 'subtls.js 应导出 subSettingsChange');
  const http = { subCertFile: '', subKeyFile: '' };
  const https = { subCertFile: '/x/sub.crt', subKeyFile: '/x/sub.key' };
  assert.deepEqual(subSettingsChange(http, https, 4), { from: 'http', to: 'https', n: 4 }, '填上证书和私钥:http → https');
  assert.deepEqual(subSettingsChange(https, http, 4), { from: 'https', to: 'http', n: 4 }, '清空:https → http');
  assert.deepEqual(subSettingsChange(https, { subCertFile: '/x/sub.crt', subKeyFile: '' }, 4), { from: 'https', to: 'http', n: 4 },
    '只清空私钥也会退回 http(订阅服务要证书和私钥都在才走 HTTPS)');
  assert.equal(subSettingsChange(http, { subCertFile: '/x/sub.crt', subKeyFile: '' }, 4), null, '只填证书不填私钥,仍是 http');
  assert.equal(subSettingsChange(https, { subCertFile: '/y/a.crt', subKeyFile: '/y/a.key' }, 4), null, '换成另一张证书,地址不变');
  assert.equal(subSettingsChange(http, https, 0), null, '没有用户不用提示');
  assert.equal(subSettingsChange({}, { subCertFile: ' /x/sub.crt ', subKeyFile: ' /x/sub.key ' }, 2)?.to, 'https', '设置值缺省 / 带空白也按同一个判法');
});

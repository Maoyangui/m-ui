// 从跑着的演示实例导出官网 Live Demo 用的 fixture:接口响应 + 二维码,写到 site/demo/fixtures.json。
// 由 docs/screenshots/make.sh 在拍完截图后调用;单独跑需要环境变量:
//   MUI_URL=http://127.0.0.1:19053/app  MUI_COOKIE=<curl cookie 文件>  MUI_TOKEN=<副机令牌>  SCRUB=<逗号分隔要抹掉的字符串>  FIX=site/demo/fixtures.json
// 里面只有 example.com 与文档保留 IP 的演示数据;主机名、探测到的公网 IP、令牌会再抹一遍。
import { execFileSync } from 'node:child_process';
import { writeFileSync } from 'node:fs';
const BS = String.fromCharCode(92);

const M = process.env.MUI_URL, C = process.env.MUI_COOKIE, TOKEN = process.env.MUI_TOKEN || '', SCRUB = (process.env.SCRUB || '').split(',').filter(Boolean), FIX = process.env.FIX || 'site/demo/fixtures.json';
const curl = (p, bin) => { try { return execFileSync('curl', ['-s', '-b', C, '-m', '20', `${M}/api/${p}`], { encoding: bin ? 'buffer' : 'utf8', maxBuffer: 1 << 26 }); } catch { return bin ? null : ''; } };
const json = p => { try { return JSON.parse(curl(p)); } catch { return null; } };

const keys = ['status', 'settings', 'lines', 'upstreams', 'users', 'plans', 'nodes', 'exts', 'onlines', 'resellers', 'update', 'cert', 'ops', 'self',
  'audit?limit=8', 'logs?count=40&level=info', 'upstreams/health', 'conns/recent', 'sublogs', 'cert/status', 'ops/status', 'backup/list', 'admin/info', 'agent/info',
  'stats/top?hours=24&limit=10', 'stats/top?hours=168&limit=10', 'stats/top?hours=720&limit=10',
  'stats?resource=user&hours=1&bucket=60', 'stats?resource=user&hours=6&bucket=300', 'stats?resource=user&hours=24&bucket=3600', 'stats?resource=user&hours=168&bucket=3600'];
const fixtures = {};
for (const k of keys) fixtures['GET ' + k] = json(k);
// 订阅日志里的 UA 是拍截图的无头浏览器 / curl,换成常见客户端的样子;IP 已是文档保留段
const UAS = ['ClashMetaForAndroid/2.11.12.Meta', 'Shadowrocket/2.2.60 (iPhone; iOS 18.5)', 'sing-box/1.12.4', 'clash-verge/v2.3.2', 'Hiddify/2.5.7 (Android)', 'Stash/3.1.0 (iOS)'];
for (const [i, l] of (fixtures['GET sublogs'] || []).entries()) if (l && typeof l === 'object' && 'ua' in l) l.ua = UAS[i % UAS.length];
const users = fixtures['GET users'] || [];
for (const u of users) {
  fixtures[`GET users/${u.id}/sub`] = json(`users/${u.id}/sub`);
  for (const h of [24, 168, 720]) fixtures[`GET stats?resource=user&tag=${u.name}&hours=${h}&bucket=3600`] = json(`stats?resource=user&tag=${encodeURIComponent(u.name)}&hours=${h}&bucket=3600`);
}
const qr = {};
for (const u of users) for (const f of ['clash', 'link']) { const b = curl(`users/${u.id}/qr?format=${f}`, true); if (b && b.length > 100) qr[`${u.id}:${f}`] = 'data:image/png;base64,' + b.toString('base64'); }

// 密钥类字段一律换成占位:演示实例的密钥本来就是一次性的,但也不该原样出现在官网
const SECRET = /private_key|privatekey|password|secret|apitoken|agenttoken|totp|(^|_)token$/i;
const scrub = (v, k = '') => Array.isArray(v) ? v.map(x => scrub(x)) : (v && typeof v === 'object') ? Object.fromEntries(Object.entries(v).map(([kk, vv]) => [kk, scrub(vv, kk)])) : (typeof v === 'string' && k !== 'subToken' && SECRET.test(k) && v.length > 3) ? 'demo-' + k.replace(/[^a-z]/gi, '').toLowerCase() : v;
// 演示实例跑在 19053 / 19056 / 19054 这类临时端口和 127.0.0.1 上,输出按默认端口与各自域名呈现;版本、主机名也换成演示口径
const VER = (() => { try { return execFileSync('git', ['describe', '--tags', '--abbrev=0'], { encoding: 'utf8' }).trim().replace(/^v/, ''); } catch { return 'demo'; } })();
for (const n of ((fixtures['GET nodes'] || {}).nodes || [])) {
  if (!n.isLocal) n.apiUrl = `https://${n.domain || n.addr}:2053/app/`;
  n.publicIp = n.addr;
  if (n.status) { n.status.version = VER; n.status.hostname = String(n.name || 'node').toLowerCase(); if (!n.status.certDays) n.status.certDays = (fixtures['GET status'] || {}).certDaysLeft || 0; }
}
if (fixtures['GET status']) fixtures['GET status'].version = VER;
let out = JSON.stringify({ generated: new Date().toISOString().slice(0, 10), generatedAt: Math.floor(Date.now() / 1000), fixtures: scrub(fixtures), qr });
out = out.replace(/\b1905([346])\b/g, '205$1'); // 19053/19056/19054 → 2053/2056/2054(数字与字符串两种形式都在)
for (const w of [TOKEN, ...SCRUB]) if (w && w.length > 3) out = out.split(w).join('demo');
// 导出实例跑在临时目录里,证书等路径会带本机目录:统一换成安装后的 /etc/m-ui(JSON 里反斜杠是转义过的)
if (process.env.MUI_TMP) {
  const forms = new Set([process.env.MUI_TMP, process.env.MUI_TMP.replace(/\//g, BS)].map(x => JSON.stringify(x).slice(1, -1)));
  for (const f of forms) if (f.length > 5) out = out.split(f).join('/etc/m-ui');
  out = out.replace(/\/etc\/m-ui((?:\\\\[A-Za-z0-9._~-]+)+)/g, (m, tail) => '/etc/m-ui' + tail.split(BS + BS).join('/'));
}
out = out.replace(/\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}\b/gi, m => (m.includes('::') || m.split(':').length > 3) ? '2001:db8::10' : m);
writeFileSync(FIX, out);
console.log(`fixture:${Object.keys(fixtures).length} 个接口,${Object.keys(qr).length} 张二维码,${Math.round(out.length / 1024)} KB → ${FIX}`);

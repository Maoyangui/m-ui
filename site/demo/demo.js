// 官网的静态 Live Demo:同一套面板前端(web/assets 原样复制),只把 api.js 的传输层换成
// 内存里的演示数据。不发任何网络请求,没有后端,改动只在本页内存里,刷新即恢复。
//
// 演示数据来自 docs/screenshots/make.sh 跑起来的真实实例(fixtures.json),所以接口形状和生产一致;
// 这里只做两件事:按 "METHOD path" 找 fixture,以及把增删改落到内存里的几张表上。
import { setTransport, setQrUrl, ApiError } from './api.js';
import { toast } from './ui.js';
import { trErr } from './errmsg.js';

const SITE = 'https://maoyangui.github.io/m-ui/';
const GITHUB = 'https://github.com/Maoyangui/m-ui';
const data = await (await fetch('js/fixtures.json')).json();
const F = data.fixtures || {};
// 把导出那一刻的时间戳整体平移到现在:流量图窗口、"几分钟前同步"、到期剩余天数都保持导出当天的样子,演示不会随时间变旧
const GEN = data.generatedAt || Math.floor(Date.parse((data.generated || '2026-01-01') + 'T12:00:00Z') / 1000);
const DELTA = Math.floor(Date.now() / 1000) - GEN;
const TS_KEYS = new Set(['bootTime', 'expiry', 'createdAt', 'onlineAt', 'lastSeen', 'lastPush', 'claimBefore', 'checkedAt', 'dateTime', 'lastRun', 'ts', 'end', 't', 'start', 'shareAt', 'nextReset', 'updatedAt']);
const shift = (v, k = '') => Array.isArray(v) ? v.map(x => shift(x)) : (v && typeof v === 'object') ? Object.fromEntries(Object.entries(v).map(([kk, vv]) => [kk, shift(vv, kk)])) : (typeof v === 'number' && TS_KEYS.has(k) && v > 1e9 && v < 2e9) ? v + DELTA : v;
for (const k of Object.keys(F)) F[k] = shift(F[k]);
const clone = v => JSON.parse(JSON.stringify(v));
const fx = k => clone(F[k] ?? null);
// 上游测试结果:上游组写明现在走哪一条
const demoUpResult = u => ({ name: 'Demo', isLocal: true, state: 'ok', delayMs: 40 + Math.floor(Math.random() * 80), method: u.type === 'urltest' ? 'via:' + ((u.options && u.options.outbounds) || [''])[0] : '' });
// 演示里的故障要跟着操作走:处理掉了,提醒就得消失(以前巡检结果是导出时的一份死数据 ——
// 在上游页测出来是通的、甚至把那条上游删了,概况的「需要处理」还报它故障)。
// demoUpTest 测一条上游并把结果记进巡检结果:一开始就不通的(S.upBroken)一直不通,直到在演示里编辑保存过(算修好了)或删掉
function demoUpTest(u) {
  const h = (S.health.results || []).find(r => r.id === u.id);
  const servers = h && (h.servers || []).length ? h.servers : [{ nodeId: 1, name: 'Tokyo', isLocal: true }];
  const via = u.type === 'urltest' ? 'via:' + ((u.options && u.options.outbounds) || [''])[0] : '';
  const out = servers.map(s => (S.upBroken.has(u.id)
    ? { ...s, state: 'fail', delayMs: 0, error: s.error || 'Head "http://www.gstatic.com/generate_204": EOF', checkedAt: now() }
    : { ...s, state: 'ok', delayMs: 40 + Math.floor(Math.random() * 80), method: via || s.method || 'urltest', error: '', checkedAt: now() }));
  if (h) h.servers = out; else (S.health.results = S.health.results || []).push({ id: u.id, name: u.name, unused: false, servers: out });
  return clone(out);
}
// 巡检结果只留还在的上游,名字跟着改名走
function demoHealth() {
  S.health.results = (S.health.results || []).filter(r => S.upstreams.some(u => u.id === r.id));
  for (const r of S.health.results) r.name = S.upstreams.find(u => u.id === r.id).name;
  return clone(S.health);
}
// 「端口被占」跟着线路走:那个中转入口改了端口、暂停或删了,线路停了或删了,服务器上这一条就没有了
function demoSyncBlocked() {
  for (const n of S.nodes.nodes || []) {
    const st = n.status;
    if (!st || !Array.isArray(st.blocked)) continue;
    st.blocked = st.blocked.filter(b => S.lines.some(l => l.enabled !== false && (l.forwards || []).some(f => f.via === n.id && Number(f.port) === Number(b.port) && !f.off)));
  }
}

// 内存状态:从 fixture 复制一份,增删改都落在这里
const S = {
  settings: fx('GET settings') || {},
  lines: fx('GET lines') || [],
  upstreams: fx('GET upstreams') || [],
  users: fx('GET users') || [],
  plans: fx('GET plans') || [],
  nodes: (fx('GET nodes') || { nodes: [] }),
  exts: fx('GET exts') || [],
  resellers: fx('GET resellers') || [],
  onlines: fx('GET onlines') || { users: [], lines: [], upstreams: [], connCounts: {} },
  // 上游巡检结果:从导出的那一份起步,之后跟着演示里的操作变(见 demoUpTest / demoHealth)
  health: fx('GET upstreams/health') || { intervalMinutes: 10, lastRun: 0, results: [] },
};
// 导出时就不通的上游:在演示里编辑保存过(算修好了)或删掉之前,测试和巡检一直报不通
S.upBroken = new Set((S.health.results || []).filter(r => (r.servers || []).length && r.servers.every(s => s.state === 'fail')).map(r => r.id));
// 让演示像在跑:两个用户在线,各带文档保留段里的 IP
const online = { 'demo-alice': ['203.0.113.5', '198.51.100.7'], 'demo-bob': ['192.0.2.44'] };
for (const u of S.users) if (online[u.name]) { u.onlineIps = online[u.name]; u.onlineAt = Math.floor(Date.now() / 1000) - 60; }
S.onlines.users = Object.keys(online).filter(n => S.users.some(u => u.name === n));
S.onlines.connCounts = Object.fromEntries(S.onlines.users.map((n, i) => [n, 3 + i * 2]));
S.onlines.lines = S.lines.slice(0, 2).map(l => l.name);
for (const n of S.nodes.nodes || []) if (n.status) n.status.uptime = (n.status.uptime || 0) + DELTA; // 副机运行时长随导出日期累加
// 磁盘占用:第二台副机快满了(服务器页的 💾 标记、概况「需要处理」、清理磁盘都看得到),其余宽裕
{ const G = 1073741824, at = Math.floor(Date.now() / 1000); let k = 0;
  for (const n of S.nodes.nodes || []) { if (n.isLocal || !n.status) continue; k++;
    n.status.disk = k === 2 ? { path: '/etc/m-ui', total: 9.6 * G, used: 8.8 * G, free: 0.8 * G, logs: 4.1 * G, cache: 0.3 * G, swap: 4 * G, opsAgent: true, at }
      : { path: '/etc/m-ui', total: 40 * G, used: (9 + 4 * k) * G, free: (31 - 4 * k) * G, logs: 0.4 * G, cache: 0.2 * G, swap: 2 * G, at }; } }

// 外部 API 的密钥(主面板 admin、代理面板 self 各一套;只在本页内存里)。第一把是默认密钥,和别的在同一张表里
const API_PERMS = ['users.read', 'users.create', 'users.update', 'users.delete', 'billing', 'plans.read', 'panel.read'];
{ const at = Math.floor(Date.now() / 1000);
  S.apiKeys = {
    admin: [
      { id: 1, resellerId: 0, name: '', isDefault: true, token: 'demo-defaulttoken0for0the0demo0only', perms: API_PERMS.join(','), enabled: true, ipAllow: false, ipList: '', createdAt: at - 86400 * 40, lastUsedAt: at - 95, lastUsedIp: '203.0.113.80' },
      { id: 2, resellerId: 0, name: 'telegram-bot', isDefault: false, token: 'demo-telegrambot0key0for0the0demo0only', perms: 'users.read,panel.read', enabled: true, ipAllow: true, ipList: '198.51.100.23, 198.51.100.24', createdAt: at - 86400 * 9, lastUsedAt: at - 420, lastUsedIp: '198.51.100.23' },
    ],
    self: [],
  }; }
function demoApiKeys(scope, method, id, sub, body) {
  const list = S.apiKeys[scope];
  const view = k => ({ ...k, permList: API_PERMS.filter(p => k.perms.split(',').includes(p)) });
  const fill = (k, b) => {
    if (!b) return k;
    if (b.name != null) k.name = String(b.name).trim() || (k.isDefault ? '' : k.name);
    if (Array.isArray(b.perms)) k.perms = API_PERMS.filter(p => b.perms.includes(p)).join(',');
    if (b.enabled != null) k.enabled = !!b.enabled;
    if (b.ipAllow != null) k.ipAllow = !!b.ipAllow;
    if (b.ipList != null) k.ipList = String(b.ipList).split(/[\s,;，]+/).filter(Boolean).join(', ');
    return k;
  };
  if (!id && method === 'POST') { const k = fill({ id: nextId(list), resellerId: 0, name: 'key', isDefault: false, token: 'demo-' + rnd(32), perms: 'users.read', enabled: true, ipAllow: false, ipList: '', createdAt: Math.floor(Date.now() / 1000), lastUsedAt: 0, lastUsedIp: '' }, body); list.push(k); return view(k); }
  const k = list.find(x => String(x.id) === String(id));
  if (!k) return { error: 'not found' };
  if (method === 'DELETE') { list.splice(list.indexOf(k), 1); return ok(); }
  if (sub === 'rotate') { k.token = 'demo-' + rnd(32); return view(k); }
  return view(fill(k, body));
}

const nextId = list => list.reduce((m, x) => Math.max(m, x.id || 0), 0) + 1;
// 外部订阅展开:节点明细 / 逐台测速 / 大陆连通 / 隐藏改名 / 添加为上游(GET 与 POST 都走这里)。
// 和真面板一样按节点指纹(key)认节点;隐藏 / 改名只收增量,带版本号。
function extNodes(id, seg, method, body) {
  let servers = ((S.nodes && S.nodes.nodes) || (Array.isArray(S.nodes) ? S.nodes : [])).filter(n => n.enabled !== false).map(n => ({ id: n.id, name: n.name, isLocal: !!n.isLocal }));
  if (!servers.length) servers = [{ id: 1, name: '主机', isLocal: true }, { id: 2, name: '副机', isLocal: false }];
  const nodes = demoExtNodes(id);
  const ext = S.exts.find(e => e.id === id) || {};
  ext.hiddenSet = ext.hiddenSet || {};
  ext.renameSet = ext.renameSet || {};
  ext.prefsRev = ext.prefsRev || 0;
  const annotate = () => {
    const words = String(ext.exclude || '').split(',').map(s => s.trim()).filter(Boolean);
    for (const n of nodes) {
      n.hidden = n.keyword = false;
      if (words.some(w => n.name.includes(w))) { n.hidden = true; n.keyword = true; }
      else if (ext.hiddenSet[n.key]) n.hidden = true;
      n.rename = ext.renameSet[n.key] || undefined;
    }
    ext.nodeCount = nodes.filter(n => !n.hidden).length;
    ext.nodeTotal = nodes.length;
  };
  annotate();
  const payload = () => ({ nodes, servers, fetchedAt: now() - 600, sig: 'demo', rev: ext.prefsRev, stale: 0 });
  if (!seg[3]) return payload();
  if (seg[3] === 'prefs') {
    if (!body || body.rev !== ext.prefsRev) throw new ApiError(409, '节点的隐藏 / 改名设置已在别处修改,请刷新后重试');
    for (const x of body.hide || []) ext.hiddenSet[x.key] = true;
    for (const x of body.show || []) delete ext.hiddenSet[x.key];
    for (const x of body.rename || []) { if (x.to) ext.renameSet[x.key] = x.to; else delete ext.renameSet[x.key]; }
    ext.prefsRev++;
    annotate();
    return payload();
  }
  const keysOf = b => (b && b.keys && b.keys.length) ? b.keys : nodes.filter(n => !n.hidden || (b && b.includeHidden)).map(n => n.key);
  if (seg[3] === 'reach' && !seg[4]) { S.reachKeys = keysOf(body); return { job: 'demo-reach-' + id, keys: S.reachKeys }; }
  if (seg[3] === 'reach') {
    // 演示用:第一个被墙,其余通;UDP 协议只 ping
    const results = {};
    (S.reachKeys || []).forEach((k, i) => {
      const n = nodes.find(x => x.key === k) || {};
      const udp = ['hysteria2', 'tuic'].includes(n.type);
      results[k] = { state: 'done', verdict: i === 0 ? 'blocked' : 'ok', note: udp ? 'udpPing' : '', groups: [{ key: 'ct', ok: i === 0 ? 0 : 3, total: 3 }, { key: 'cu', ok: i === 0 ? 0 : 3, total: 3 }, { key: 'cm', ok: i === 0 ? 0 : 3, total: 3 }] };
    });
    return { done: true, results };
  }
  if (seg[3] === 'test' && !seg[4]) { S.testKeys = keysOf(body); return { job: 'demo-' + id, keys: S.testKeys }; }
  if (seg[3] === 'test') {
    const results = {};
    for (const k of S.testKeys || []) {
      results[k] = {};
      for (const s of servers) results[k][s.id] = Math.random() < 0.8 ? { state: 'ok', delayMs: 60 + Math.floor(Math.random() * 300), method: 'urltest', at: now() } : { state: 'fail', error: 'timeout', at: now() };
    }
    const total = (S.testKeys || []).length * servers.length;
    return { done: true, total, finished: total, results };
  }
  if (seg[3] === 'add-upstream') return { results: ((body && body.items) || []).map((x, i) => ({ key: x.key, name: x.name, ok: true, id: 900 + i })) };
  return ok();
}
// 演示用的外部订阅节点:三种协议各两个,带完整参数
const demoExtNodes = id => [
  ['trojan', '官网 www.example.net', 'www.example.net', 443, [['password', 'x' + id]]],
  ['hysteria2', 'HK-1', 'hk1.example.net', 443, [['password', 'p' + id + 'a'], ['sni', 'hk1.example.net'], ['skip-cert-verify', 'false'], ['alpn', '["h3"]']]],
  ['hysteria2', 'HK-2', 'hk2.example.net', 443, [['password', 'p' + id + 'b'], ['sni', 'hk2.example.net'], ['up', '100'], ['down', '500']]],
  ['tuic', 'JP-1', 'jp1.example.net', 8443, [['uuid', '3f2a1c9e-0000-4000-8000-00000000' + String(id).padStart(4, '0')], ['password', 'tp' + id], ['sni', 'jp1.example.net'], ['congestion-controller', 'bbr'], ['udp-relay-mode', 'native']]],
  ['tuic', 'JP-2', 'jp2.example.net', 8443, [['uuid', '3f2a1c9e-0000-4000-8000-00000000' + String(id + 1).padStart(4, '0')], ['password', 'tq' + id], ['sni', 'jp2.example.net']]],
  ['anytls', 'SG-1', 'sg1.example.net', 8444, [['password', 'ap' + id], ['sni', 'sg1.example.net'], ['client-fingerprint', 'chrome']]],
  ['anytls', 'US-1', 'us1.example.net', 8444, [['password', 'aq' + id], ['sni', 'us1.example.net'], ['skip-cert-verify', 'true']]],
].map(([type, name, server, port, extra], i) => ({
  index: i, key: 'd' + id + 'k' + i, name, type, server, port, upstream: true,
  fields: [['name', name], ['type', type], ['server', server], ['port', String(port)], ...extra].map(([k, v]) => ({ k, v })),
  link: type + '://' + encodeURIComponent(extra[0][1]) + '@' + server + ':' + port + '?sni=' + server + '#' + encodeURIComponent(name),
}));
const now = () => Math.floor(Date.now() / 1000);
const subBase = (S.settings.webDomain || 'panel.example.com');
const subUrl = u => `https://${subBase}:2056/sub/${u.subToken || u.name}`;
const notFound = () => { throw new ApiError(404, 'not found'); };

// 线路分流规则的"查域名":真面板由服务器去 v2fly 域名库查,演示只认几个常见应用(各取几个代表域名)
const DEMO_APPS = {
  douyin: ['douyin.com', 'douyinvod.com', 'douyincdn.com', 'douyinpic.com', 'amemv.com', 'snssdk.com', 'zijieapi.com'],
  netflix: ['netflix.com', 'netflix.net', 'nflxvideo.net', 'nflximg.net', 'nflxext.com', 'nflxso.net', 'fast.com'],
  openai: ['openai.com', 'chatgpt.com', 'oaistatic.com', 'oaiusercontent.com', 'sora.com'],
  tiktok: ['tiktok.com', 'tiktokv.com', 'tiktokcdn.com', 'byteoversea.com'],
  wechat: ['wechat.com', 'servicewechat.com', 'weixin.com', 'qpic.cn', 'qlogo.cn', 'tenpay.com', 'weixin.qq.com', 'wx.qq.com'],
  gmail: ['gmail.com', 'googlemail.com', 'mail.google.com'],
};
const DEMO_PART = { wechat: 'tencent', gmail: 'google' }; // 没有单独列表、从大厂列表里挑的
const DEMO_ALIAS = { '抖音': 'douyin', '奈飞': 'netflix', '网飞': 'netflix', chatgpt: 'openai', gpt: 'openai', '微信': 'wechat', weixin: 'wechat', '谷歌邮箱': 'gmail' };
const demoApps = names => (names || []).map(q => {
  const k = String(q).trim().toLowerCase(), list = DEMO_ALIAS[k] || k;
  if (DEMO_APPS[list]) return DEMO_PART[list] ? { query: q, list: DEMO_PART[list], part: true, suffix: DEMO_APPS[list] } : { query: q, list, suffix: DEMO_APPS[list] };
  return { query: q, error: '没找到这个应用', suggest: Object.keys(DEMO_APPS).filter(n => n.includes(k.slice(-4)) || k.includes(n)) };
});
const ok = (extra = {}) => ({ ok: '1', ...extra });
const rnd = n => Array.from(crypto.getRandomValues(new Uint8Array(n)), b => 'abcdefghijklmnopqrstuvwxyz0123456789'[b % 36]).join('');
// 规则页的演示数据(导出实例还没有这一页):一条晚高峰时段规则、一条突发规则,alice 正被突发规则限着
S.rules = [
  { id: 1, name: '晚高峰 Evening peak', enabled: true, kind: 'schedule', allUsers: true, userIds: [], resellerIds: [], days: '1,2,3,4,5', start: '19:00', end: '23:00', windowMin: 0, thresholdGb: 0, penaltyMin: 0, upMbps: 0, downMbps: 30, tightenOnly: true, remark: '', sort: 0, createdAt: now() - 86400 * 20 },
  { id: 2, name: '突发惩罚 Burst penalty', enabled: true, kind: 'burst', allUsers: false, userIds: [1, 2], resellerIds: [], days: '', start: '', end: '', windowMin: 10, thresholdGb: 1, penaltyMin: 20, upMbps: 0, downMbps: 20, tightenOnly: true, remark: '', sort: 0, createdAt: now() - 86400 * 3 },
];
S.limitStates = [{ id: 1, ruleId: 2, ruleName: '突发惩罚 Burst penalty', userId: 1, userName: 'demo-alice', upMbps: 0, downMbps: 20, tightenOnly: true, since: now() - 300, until: now() + 900, reason: '10 分钟内 1.2 GB' }];
// 选了套餐的规则:Basic 套餐的用户(现有和以后的)晚上限速
S.rules.push({ ...S.rules[0], id: 3, name: 'Basic 套餐晚间 Basic plan nights', allUsers: false, planIds: [1], downMbps: 50 });
for (const u of S.users) if (u.name === 'demo-alice' || u.name === 'demo-charlie') u.planId = 1; else if (u.name === 'demo-bob') u.planId = 2;
const planView = p => ({ ...p, userCount: S.users.filter(u => u.planId === p.id).length, ruleIds: S.rules.filter(r => (r.planIds || []).includes(p.id)).map(r => r.id), resellerName: '' });
const setPlanRules = (planId, ruleIds) => { if (!ruleIds) return; for (const r of S.rules) { const on = ruleIds.includes(r.id); r.planIds = (r.planIds || []).filter(x => x !== planId); if (on) r.planIds.push(planId); } };
// 证书库(导出实例只有一张自签):一张域名证书给线路默认、面板、订阅用,一张泛域名给单独选了它的线路用,一张自签闲着
const DAY = 86400;
// 续期状态和后端 CertView 一样:能不能自动续、为什么、从哪天起续
const renewOf = (source, method, days, self) => {
  if (self || source === 'selfsign') return { renewable: false, reason: 'selfsign', renewBefore: 30 * DAY };
  if (source !== 'acme') return { renewable: false, reason: 'external', renewBefore: 30 * DAY };
  if (method === 'manual') return { renewable: false, reason: 'manual', renewBefore: 30 * DAY };
  const at = now() + (days - 30) * DAY;
  return { renewable: true, renewBefore: 30 * DAY, renewAt: at, nextCheckAt: at, overdue: days <= 30, lastRenewOk: now() - 30 * DAY };
};
const certOf = (id, name, source, method, names, days, self, lineNames) => ({
  id, name, source, method, autoRenew: source === 'acme' && method !== 'manual', staging: false, createdAt: now() - 30 * DAY, ...renewOf(source, method, days, self),
  certFile: '/etc/m-ui/certs/' + name.replace('*', '_') + '.crt', keyFile: '/etc/m-ui/certs/' + name.replace('*', '_') + '.key',
  info: { exists: true, path: '/etc/m-ui/certs/' + name.replace('*', '_') + '.crt', subject: names[0], dnsNames: names, ips: null, issuer: self ? names[0] : "Let's Encrypt", notBefore: new Date((now() - 30 * DAY) * 1000).toISOString(), notAfter: new Date((now() + days * DAY) * 1000).toISOString(), daysLeft: days, selfSigned: !!self },
  forLines: false, forPanel: false, forSub: false, lineNames,
});
S.certs = [
  certOf(1, 'panel.example.com', 'acme', 'http', ['panel.example.com'], 61, false, []),
  certOf(2, '*.example.net', 'acme', 'manual', ['*.example.net', 'example.net'], 74, false, ['Tokyo-HY2']),
  certOf(3, 'self-signed', 'selfsign', '', ['panel.example.com'], 3640, true, []),
];
S.certUse = { lines: S.certs[0].certFile, panel: S.certs[0].certFile, sub: S.certs[0].certFile };
const syncCertUse = () => { for (const c of S.certs) { c.forLines = c.certFile === S.certUse.lines; c.forPanel = c.certFile === S.certUse.panel; c.forSub = c.certFile === S.certUse.sub; } };
syncCertUse();
// 线路选证书的抽屉按服务器列:本机三张都有,副机没有泛域名那张
const briefs = list => JSON.stringify(list.map(c => ({ name: c.name, domains: c.info.dnsNames, selfSigned: c.info.selfSigned, notAfter: now() + c.info.daysLeft * DAY, default: c.forLines })));
for (const n of S.nodes.nodes || []) n.certs = briefs(n.isLocal ? S.certs : S.certs.filter(c => c.id !== 2));
const tlsOf = l => { try { return typeof l.tls === 'string' ? JSON.parse(l.tls || '{}') : (l.tls || {}); } catch { return {}; } };
for (const l of S.lines) if (l.name === 'Tokyo-HY2' && tlsOf(l).mode === 'cert') l.tls = JSON.stringify({ ...tlsOf(l), cert: '*.example.net' });
// 外部订阅(导出实例没有):关键词已排除带"官网"的,展开能隐藏、改名、测大陆连通
if (!S.exts.length) S.exts.push({ id: 1, name: 'Demo Sub', type: 'sub', value: 'https://sub.example.net/demo', prefix: '', enabled: true, sort: 1, remark: '', lastFetch: now() - 600, lastError: '', nodeCount: 6, nodeTotal: 7, exclude: '官网' });
const ruleView = r => ({ ...r, targetCount: r.allUsers ? S.users.filter(u => u.enabled).length : new Set([...(r.userIds || []), ...S.users.filter(u => (r.resellerIds || []).includes(u.resellerId) || (r.planIds || []).includes(u.planId)).map(u => u.id)]).size, activeCount: S.limitStates.filter(s => s.ruleId === r.id).length });

// 换套餐 / 续 N 期的演示版:和面板同一套规则的简化(不处理首次连接计时),只改内存;套餐记录也记在内存里
const GB = 1073741824;
for (const p of S.plans) if (p.price === undefined) p.price = ({ Basic: 5, Pro: 20 })[p.name] || 0; // 演示用的参考价
S.planOps = [];
const planStateOf = (u, plan) => ({ planId: u.planId || 0, plan: plan ? plan.name : '', volume: u.volume || 0, used: (u.up || 0) + (u.down || 0), expiry: u.expiry || 0,
  activateDays: u.activateDays || 0, autoReset: !!u.autoReset, resetDays: u.resetDays || 0, nextReset: u.nextReset || 0, deviceLimit: u.deviceLimit || 0,
  speedUp: u.speedUp || 0, speedDown: u.speedDown || 0, enabled: u.enabled !== false, disabledReason: u.disabledReason || '', lineRefs: (u.lineIds || []).map(id => ({ lineId: id })) });
const demoErr = (status, zh) => new ApiError(status, trErr(zh)); // 英文界面也看得懂
const demoWarn = (code, args, block) => ({ code, text: code, args, block: !!block });
function demoPlanOp(u, p, op, n, usage, dry, remark) {
  const t = now(), v = Math.round((p.volumeGb || 0) * GB), used = (u.up || 0) + (u.down || 0);
  const old = S.plans.find(x => x.id === u.planId);
  if (!usage || usage === 'auto') usage = op === 'stack' && u.volume ? 'keep' : 'reset';
  if (op === 'renew') usage = 'reset'; else if (op === 'extend') usage = 'keep';
  let credit = null, creditSecs = 0;
  if (op === 'prorate') {
    if (!old) throw demoErr(400, '用户没有所属套餐,不能按价值折算');
    if (old.id === p.id) throw demoErr(400, '目标套餐就是当前套餐,不能折算:要从现在重新开始用 replace,要带结转续费用 stack');
    if (!(old.price > 0) || !(p.price > 0)) throw demoErr(400, `套餐「${!(old.price > 0) ? old.name : p.name}」没填参考价,不能按价值折算`);
    const ratio = old.days && u.expiry ? Math.max(0, u.expiry - t) / (old.days * DAY) : 0;
    const periods = ratio * old.price / p.price;
    creditSecs = Math.round(periods * (p.days || 0) * DAY);
    credit = { fromPrice: old.price, toPrice: p.price, ratio, value: ratio * old.price, periods, seconds: creditSecs, bytes: 0 };
  }
  const nx = { ...u, planId: p.id, deviceLimit: p.deviceLimit || 0, speedUp: p.speedUp || 0, speedDown: p.speedDown || 0, autoReset: !!p.autoReset, resetDays: p.resetDays || 0, enabled: true, disabledReason: '' };
  const fromNow = op === 'replace' || op === 'prorate';
  // 叠加:已过期的旧订阅不结转;目标不限期 / 不限量时,原来有限的到期 / 配额照旧保留(和面板一样)
  const expired = !u.activateDays && u.expiry > 0 && u.expiry <= t, vleft = u.volume ? Math.max(0, u.volume - used) : 0;
  const keepEnd = op === 'stack' && !p.days && !expired && (u.expiry || u.activateDays), keepVol = op === 'stack' && !v && u.volume && !expired;
  nx.expiry = keepEnd ? u.expiry : !p.days ? 0 : (fromNow ? t : Math.max(t, u.expiry || 0)) + n * p.days * DAY + creditSecs;
  nx.volume = keepVol ? (usage === 'reset' ? Math.max(1, vleft) : u.volume) : !v ? 0 : p.autoReset || op === 'extend' ? v
    : op === 'stack' && u.volume ? (expired ? (usage === 'reset' ? 0 : used) : (usage === 'reset' ? vleft : u.volume)) + n * v : n * v;
  if (usage === 'reset') { nx.up = 0; nx.down = 0; }
  nx.nextReset = p.autoReset ? (usage === 'keep' && u.nextReset > t ? Math.min(u.nextReset, t + p.resetDays * DAY) : t + p.resetDays * DAY) : 0;
  if (p.lineIds && p.lineIds.length) nx.lineIds = [...p.lineIds];
  const warnings = [];
  const nUsed = (nx.up || 0) + (nx.down || 0);
  if (nx.volume && usage === 'keep' && nUsed >= nx.volume) warnings.push(demoWarn('over_quota', { used: nUsed, volume: nx.volume, nextReset: nx.nextReset || 0 }, true));
  if (nx.expiry && (!u.expiry || nx.expiry < u.expiry - 60)) warnings.push(demoWarn('expiry_earlier', { from: u.expiry || 0, to: nx.expiry }, true));
  if (nx.volume && (!u.volume || nx.volume < u.volume)) warnings.push(demoWarn('volume_lower', { from: u.volume || 0, to: nx.volume }));
  if (op === 'stack' && v && expired && vleft) warnings.push(demoWarn('carry_expired', { lost: vleft }));
  if (op === 'stack' && p.autoReset && v) warnings.push(demoWarn('per_period_not_stacked', { lost: expired ? 0 : vleft }));
  if (keepEnd) warnings.push(demoWarn('expiry_kept', { expiry: u.expiry || 0, activateDays: u.activateDays || 0 }));
  if (keepVol) warnings.push(demoWarn('volume_kept', { left: vleft }));
  const left = u.expiry ? Math.max(0, u.expiry - t) : null;
  const res = { op, direction: (p.volumeGb || 0) > ((old && old.volumeGb) || 0) ? 'upgrade' : old && old.id === p.id ? 'same' : 'downgrade', periods: n, usage, dryRun: !!dry, replayed: false,
    from: planStateOf(u, old), to: planStateOf(nx, p), remaining: { seconds: left, bytes: u.volume ? (expired ? 0 : vleft) : null, timeRatio: null, trafficRatio: null, ratio: null },
    credit, warnings, needsForce: warnings.some(w => w.block) };
  if (dry) return { res };
  S.planOps.unshift({ id: S.planOps.length + 1, createdAt: t, actor: 'admin', userId: u.id, op, fromPlanId: u.planId || 0, fromPlan: old ? old.name : '', toPlanId: p.id, toPlan: p.name, periods: n, usage, remark: remark || '', before: res.from, after: res.to, warnings });
  Object.assign(u, nx);
  res.opId = S.planOps.length;
  return { res };
}

// 大陆连通 + IPv6 入站检测(导出实例没有这项):三台三网都通;法兰克福默认走 IPv6,境外 IPv6 测点连得进来
const probe = (city, network, asn, ms, country) => ({ ...(country ? { country } : {}), city, asn, network, ok: true, avgMs: ms, loss: 0 });
const cnGroups = base => [
  { key: 'cm', state: 'ok', ok: 3, total: 3, avgMs: base + 18, probes: [probe('Guangzhou', 'China Mobile', 9808, base + 15), probe('Shanghai', 'China Mobile', 56040, base + 20), probe('Beijing', 'China Mobile', 56048, base + 19)] },
  { key: 'ct', state: 'ok', ok: 3, total: 3, avgMs: base + 9, probes: [probe('Shanghai', 'China Telecom', 4812, base + 7), probe('Guangzhou', 'China Telecom', 4134, base + 10), probe('Chengdu', 'China Telecom', 4134, base + 11)] },
  { key: 'cu', state: 'ok', ok: 3, total: 3, avgMs: base + 14, probes: [probe('Beijing', 'China Unicom', 4808, base + 12), probe('Tianjin', 'China Unicom', 4837, base + 15), probe('Shenyang', 'China Unicom', 4837, base + 16)] },
  { key: 'hk', state: 'ok', ok: 2, total: 2, avgMs: 8, probes: [probe('Hong Kong', 'Example HK', 64510, 8), probe('Hong Kong', 'Example HK', 64511, 9)] },
];
S.reach = { auto: true, minutes: 90, nextRun: now() + 50 * 60, running: false, runningIds: [], perCheck: 15, quota: { remaining: 210, limit: 250, resetAt: now() + 40 * 60 }, history: {}, results: [
  { nodeId: 1, name: 'Tokyo', target: '192.0.2.10', port: 24443, portFrom: 'sub', at: now() - 40 * 60, auto: true, verdict: 'ok', groups: cnGroups(52) },
  { nodeId: 2, name: 'Singapore', target: '198.51.100.10', port: 28443, portFrom: 'line', portLine: 'Singapore-AnyTLS', at: now() - 40 * 60, auto: true, verdict: 'ok', groups: cnGroups(68) },
  { nodeId: 3, name: 'Frankfurt', target: '203.0.113.30', port: 24433, portFrom: 'line', portLine: 'Frankfurt-Reality', at: now() - 40 * 60, auto: true, verdict: 'ok', groups: cnGroups(160),
    // IPv6 入站:先用大陆有 IPv6 的家宽测点(这里连上了,所以没再问境外测点)
    v6: { state: 'ok', scope: 'cn', target: '2001:db8:7::30', port: 24433, portLine: 'Frankfurt-Reality', ok: 4, total: 4, cnOk: 4, cnTotal: 4, at: now() - 40 * 60,
      probes: [{ ...probe('Wuhan', 'CHINA UNICOM China169 Backbone', 4837, 182, 'CN'), group: 'cu' }, { ...probe('Guilin', 'China Telecom', 4134, 171, 'CN'), group: 'ct' },
        { ...probe('Ningbo', 'China Mobile Communications', 56041, 165, 'CN'), group: 'cm' }, { ...probe('Guangzhou', 'China Mobile Communications Group', 9808, 176, 'CN'), group: 'cm' }] } },
] };
for (const r of S.reach.results) S.reach.history[r.nodeId] = Array.from({ length: 8 }, (_, i) => ({ at: r.at - (8 - i) * 90 * 60, verdict: 'ok', auto: true }));
// 概览「最近入站连接」:通过握手与认证的、被设备数上限拒的,以及到了但没过握手 / 认证的(都用文档保留地址)
const recentConns = () => [
  { ip: '203.0.113.5', user: 'demo-alice', line: 'Tokyo-HY2', protocol: 'hysteria2', count: 14, ts: now() - 60, server: 'Tokyo', nodeId: 1, family: 'v4' },
  { ip: '2001:db8:1:2::8', user: 'demo-alice', line: 'Frankfurt-Reality', protocol: 'vless', count: 6, ts: now() - 180, server: 'Frankfurt', nodeId: 3, family: 'v6' },
  { ip: '198.51.100.7', user: 'demo-alice', line: 'Singapore-AnyTLS', protocol: 'anytls', count: 3, ts: now() - 420, server: 'Singapore', nodeId: 2, family: 'v4' },
  { ip: '192.0.2.44', user: 'demo-bob', line: 'Global-SS2022', protocol: 'shadowsocks', count: 9, ts: now() - 240, server: 'Tokyo', nodeId: 1, family: 'v4' },
  { ip: '192.0.2.77', user: 'demo-bob', line: 'Global-SS2022', protocol: 'shadowsocks', count: 2, ts: now() - 900, server: 'Singapore', nodeId: 2, family: 'v4', rejected: 'device' },
  { ip: '198.51.100.23', line: 'Frankfurt-Reality', protocol: 'vless', count: 4, ts: now() - 600, server: 'Frankfurt', nodeId: 3, family: 'v4', failed: 'tls', detail: 'REALITY: processed invalid connection' },
  { ip: '203.0.113.99', line: 'Global-TUIC', protocol: 'tuic', count: 2, ts: now() - 1500, server: 'Tokyo', nodeId: 1, family: 'v4', failed: 'auth', detail: 'authentication failed' },
];
// 「换机还原」的顶部提示只在地址带 ?restore 时演示(正常演示不挂一条警告)
const showRestore = /[?&]restore\b/.test(location.search);

// 线路详情:各服务器的流量按全站流量图的形状摊开(每条线路、每台服务器占比不同),在线的是上面那两位
const lineIdsOf = l => { try { const v = typeof l.nodeIds === 'string' ? JSON.parse(l.nodeIds || '[]') : (l.nodeIds || []); return Array.isArray(v) ? v : []; } catch { return []; } };
function lineDetail(l, q) {
  const all = (S.nodes && S.nodes.nodes) || [];
  const ids = lineIdsOf(l);
  const nodes = all.filter(n => !ids.length || ids.includes(n.id));
  const onl = S.onlines.lines.includes(l.name) ? Object.entries(online).filter(([u]) => S.onlines.users.includes(u)) : [];
  const onlines = onl.map(([user, ips], i) => { const n = nodes[i % Math.max(1, nodes.length)] || { id: 1, name: '主机' }; return { user, nodeId: n.id, server: n.name, ips: [...ips].sort() }; });
  onlines.sort((x, y) => (x.user < y.user ? -1 : x.user > y.user ? 1 : x.server < y.server ? -1 : x.server > y.server ? 1 : 0));
  const perNode = {}; onlines.forEach(o => { perNode[o.nodeId] = (perNode[o.nodeId] || 0) + 1; });
  const head = { onlines, onlineUsers: new Set(onlines.map(o => o.user)).size, onlineIPs: onlines.reduce((a, o) => a + o.ips.length, 0) };
  if (q.get('part') === 'online') return { ...head, perNode };
  // 流量序列按请求的时间范围与分桶现算(和真面板一样:分桶起点按浏览器时区对齐整点 / 整天,最后一格是正在走的这一格),
  // 不再借用录制的全站数据 —— 那份只有 24 小时与 7 天(逐小时),30 天会退回 24 小时,时间也没对齐整点。
  const hours = Number(q.get('hours')) || 24;
  const span = Number(q.get('bucket')) || (hours <= 24 ? 3600 : hours <= 168 ? 21600 : 86400);
  const off = (Number(q.get('tz')) || 0) * 60;
  const now = Math.floor(Date.now() / 1000);
  const end = Math.floor((now + off) / span) * span - off;
  const n = Math.max(1, Math.round(hours * 3600 / span));
  const lf = 0.25 + ((l.id * 37) % 60) / 100; // 每条线路的量级不同
  const weights = [0.5, 0.3, 0.12, 0.08, 0.05];
  const noise = (a, b) => { const x = Math.sin(a * 12.9898 + b * 78.233) * 43758.5453; return x - Math.floor(x); };
  // 一小时的流量(字节):晚上高、凌晨低,按线路与小时有起伏;start 起的 [start, end) 按小时累加,当前这一格只算已经过去的部分
  const hourBytes = (h, w) => { const loc = new Date((h * 3600 + off) * 1000).getUTCHours(); const day = 0.35 + 0.65 * Math.max(0, Math.sin((loc - 6) / 24 * 2 * Math.PI)) ** 1.4; return 160e6 * lf * w * day * (0.7 + 0.6 * noise(l.id * 7 + w * 1000, h)); };
  const series = (w) => {
    const pts = [];
    for (let i = 0; i <= n; i++) {
      const t = end - (n - i) * span, stop = Math.min(t + span, now);
      let bytes = 0;
      for (let s = t; s < stop; s += 3600) bytes += hourBytes(Math.floor(s / 3600), w) * Math.min(1, (stop - s) / 3600);
      const upShare = 0.14 + 0.06 * noise(l.id, Math.floor(t / span));
      pts.push({ t, up: Math.round(bytes * upShare), down: Math.round(bytes * (1 - upShare)) });
    }
    return { points: pts, span, start: end - n * span, end: now, totalUp: pts.reduce((a, p) => a + p.up, 0), totalDown: pts.reduce((a, p) => a + p.down, 0) };
  };
  const servers = nodes.map((n, i) => n.enabled === false || (n.status && n.status.ok === false)
    ? { nodeId: n.id, name: n.name, isLocal: !!n.isLocal, ok: false, disabled: n.enabled === false, error: n.enabled === false ? '服务器已停用' : '联系不上这台服务器', up: 0, down: 0, series: { points: [] }, online: perNode[n.id] || 0 }
    : (() => { const sr = series(weights[i] || 0.04); return { nodeId: n.id, name: n.name, isLocal: !!n.isLocal, ok: true, up: sr.totalUp, down: sr.totalDown, series: sr, online: perNode[n.id] || 0 }; })())
    .sort((a, b) => (b.up + b.down) - (a.up + a.down));
  const okOnes = servers.filter(x => x.ok);
  const total = okOnes.length ? { ...clone(okOnes[0].series), points: okOnes[0].series.points.map((p, i) => ({ t: p.t, up: okOnes.reduce((a, x) => a + ((x.series.points[i] || {}).up || 0), 0), down: okOnes.reduce((a, x) => a + ((x.series.points[i] || {}).down || 0), 0) })) } : { points: [], span: 3600 };
  total.totalUp = (total.points || []).reduce((a, p) => a + p.up, 0); total.totalDown = (total.points || []).reduce((a, p) => a + p.down, 0);
  return { line: { id: l.id, name: l.name, protocol: l.protocol, port: l.port, enabled: l.enabled }, total, servers, ...head };
}

function status() {
  const s = fx('GET status') || {};
  s.users = S.users.length; s.enabledUsers = S.users.filter(u => u.enabled).length; s.lines = S.lines.length; s.linesEnabled = S.lines.filter(l => l.enabled).length;
  s.upstreams = S.upstreams.length; s.onlineUsers = S.onlines.users.length; s.version = 'demo'; s.repo = GITHUB; s.defaultPassword = false;
  s.ha = { role: 'master', epoch: HA.epoch, mode: HA.config.mode, notices: [] };
  if (showRestore && !S.restoreAcked) s.restore = { at: now() - 300, copies: [{ from: '/etc/letsencrypt/live/panel.example.com/fullchain.pem', to: '/etc/m-ui/cert/restored/panel.example.com.crt', names: ['panel.example.com'], notAfter: now() + 61 * DAY }] };
  return s;
}

// 落地页 logo 只存在内存里:上传前浏览器已经转成 PNG,宽高从 PNG 头里读
function logoOf(data) {
  const b64 = String(data || '').split(',')[1] || '';
  const head = atob(b64.slice(0, 44));
  const u32 = i => ((head.charCodeAt(i) << 24) | (head.charCodeAt(i + 1) << 16) | (head.charCodeAt(i + 2) << 8) | head.charCodeAt(i + 3)) >>> 0;
  return { set: true, sum: rnd(16), mime: 'image/png', width: u32(16), height: u32(20), size: Math.floor(b64.length * 3 / 4), data };
}

// 主机容灾:演示站是一台开着「先确认再接任」的主机,服务器列表里前两台副机当备用,手里各有一份刚自检过的副本。
// 设置页的模式、台数、高级里的数字和暂停在本页内存里生效;设为主机、接任、重新信任这类不真做(卡片里写明演示模式)。
const haFp = seed => Array.from({ length: 16 }, (_, i) => '0123456789abcdef'[(seed * 7 + i * 5) % 16]).join('');
const HA = {
  epoch: 3,
  config: { mode: 'confirm', standbys: 2, replicaMinutes: 5, confirmMinutes: 3, autoMinutes: 10, rankStepMinutes: 3, witness: true, pauseUntil: 0, dnsNames: 'sub.example.com', dnsTokenSet: false, allowPlain: false, takeSub: false, dnsProvider: 'cloudflare' },
};
const haNodes = () => ((S.nodes && S.nodes.nodes) || []).filter(n => n.enabled !== false);
function haStandbys() {
  const c = HA.config, t0 = now();
  return haNodes().filter(n => !n.isLocal).map((n, i) => {
    const rank = c.mode !== 'off' && i < c.standbys ? i + 1 : 0;
    const fp = haFp(n.id);
    const replica = rank ? { epoch: HA.epoch, seq: 412 + n.id, at: t0 - 75, size: 18 * 1048576 + n.id * 4096, asOf: t0 - 20, verifiedAt: t0 - 60 } : undefined;
    return { id: n.id, name: n.name, rank, eligible: true, version: 'demo', pubFp: fp, pinnedFp: fp, replica,
      ack: rank ? { seq: 412 + n.id, sentAt: t0 - 75, asOf: t0 - 20, size: replica.size } : undefined,
      history: rank ? { epoch: HA.epoch, seq: 398 + n.id, at: t0 - 1260, size: 41 * 1048576 } : undefined,
      histAck: rank ? { asOf: t0 - 1260, size: 41 * 1048576 } : undefined,
      leaseAgo: 3, diskFree: 24 * 1073741824, peers: { ok: 2, total: 2, at: t0 - 40 }, epoch: HA.epoch, webPort: 2053, resellerPort: 2054 };
  });
}
function haView(full) {
  const me = haNodes().find(n => n.isLocal) || {};
  const c = HA.config, on = c.mode !== 'off';
  const sb = haStandbys(), picked = sb.filter(x => x.rank);
  const v = { role: 'master', epoch: HA.epoch, selfId: me.id || 1, selfName: me.name || '', masterId: me.id || 1, masterName: me.name || '', version: 'demo', mode: c.mode,
    notices: [], config: clone(c), standbys: on || full ? sb : [], standbyIds: on ? picked.map(x => x.id) : [], signFp: haFp(1) };
  if (full) {
    v.ready = on ? [
      { code: 'standbys', level: picked.length >= c.standbys ? 'ok' : 'warn', args: { n: String(picked.length), want: String(c.standbys) } },
      { code: 'versions', level: 'ok' }, { code: 'replica', level: 'ok' }, { code: 'https', level: 'ok' },
      { code: 'selfLink', level: 'ok', args: { url: `https://${subBase}:2053/app/` } },
      ...(c.takeSub ? [{ code: 'subDomain', level: 'ok', args: { host: 'sub.example.com' } },
        c.dnsTokenSet ? { code: 'takeSubDns', level: 'ok', names: c.dnsNames.split(/[\s,]+/).filter(Boolean), args: { provider: c.dnsProvider || 'cloudflare' } } : { code: 'takeSubDns', level: 'bad' },
        { code: 'takeSubCert', level: 'ok', args: { days: '71' } }] : [{ code: 'takeSub', level: 'info', args: { host: 'sub.example.com' } }]),
      { code: 'telegram', level: 'ok' }, { code: 'peers', level: 'ok' },
      c.witness ? { code: 'witness', level: 'ok' } : { code: 'witness', level: c.mode === 'auto' && haNodes().length <= 2 ? 'bad' : 'info' },
    ] : [{ code: 'standbys', level: 'info' }, { code: 'replica', level: 'info' }];
    if (!on) v.standbys = sb.map(x => ({ ...x, replica: undefined, ack: undefined, history: undefined, histAck: undefined }));
  }
  return v;
}
// 设为主机的检查:演示站不真的换主机,卡片里写明原因(code 为 text 的原文照样显示)
const haDemoBlock = () => ({ ok: false, blockers: [{ code: 'text', args: { text: zh ? '演示模式:不会真的换主机。装好以后在这里一键把主机换到这台。' : 'Demo mode: the master is not actually moved. On a real install this hands the master over to this server in one step.' } }] });

// 清理磁盘(演示):开始后两秒多显示「正在清理」,之后给出完整输出,那台的磁盘占用降下来(输出照真机的样子)
const dcLines = ['[12:00:00] 开始 disk-clean', '== 清理磁盘 ==', '清理前:已用 9011 MB / 共 9830 MB,可用 819 MB(92%)', '已清空 apt 软件包缓存',
  'journald 回收到 200MB 以内,现在:198.0M', '截断 /var/log/syslog:原 3812MB,只留最后 5MB', '删掉轮转下来的旧日志 14 个,共 286MB',
  '已装 m-ui-syslog-cap.timer:每小时看一次,syslog 超过 200MB 就立刻轮转', '清理后:已用 4720 MB / 共 9830 MB,可用 5110 MB(49%)', 'm-ui 的数据目录和备份没有动', '[12:00:04] 完成'];
function demoDiskClean(id, start) {
  const n = (S.nodes.nodes || []).find(x => x.id === id) || {}; // 不能用 find:顶层的 find 是浏览器的 window.find
  if (start) S.dc = { id, at: Date.now() };
  const st = S.dc && S.dc.id === id ? S.dc : null;
  const d = n.isLocal ? null : (n.status || {}).disk;
  if (!st) return { running: false, log: [], disk: d };
  if (Date.now() - st.at < 2500) return { running: true, current: 'disk-clean', log: dcLines.slice(0, 4), disk: d };
  if (d && !st.applied) { st.applied = true; const used = d.used * 0.5; Object.assign(d, { used, free: d.total - used, logs: 0.2 * 1073741824, cache: 0 }); }
  return { running: false, log: dcLines, last: { name: 'disk-clean', ok: true, time: now() }, disk: d };
}

// 一条路由:method + 路径(去掉 ./api/ 与查询串),query 单独给
function route(method, path, query, body) {
  const seg = path.split('/');
  const id = Number(seg[1]);
  const find = (list, i) => list.find(x => x.id === i) || notFound();
  const listByPrefix = k => { const exact = F[`GET ${path}${query ? '?' + query : ''}`]; if (exact !== undefined) return clone(exact); const key = Object.keys(F).find(x => x.startsWith(`GET ${path}?`) || x === `GET ${path}`); return key ? clone(F[key]) : null; };

  if (method === 'GET') {
    switch (seg[0]) {
      case 'status': return status();
      case 'settings': return clone(S.settings);
      case 'lines': if (seg[2] === 'detail') return lineDetail(find(S.lines, id), new URLSearchParams(query)); return clone(S.lines);
      case 'upstreams': if (seg[1] === 'health') return demoHealth(); return clone(S.upstreams);
      case 'users': if (seg[2] === 'plan-ops') return clone(S.planOps.filter(o => o.userId === id).slice(0, Number(new URLSearchParams(query).get('limit')) || 20));
        if (seg[2] === 'sub') { const u = find(S.users, id); return { link: subUrl(u), clash: subUrl(u) + '?format=clash', json: subUrl(u) + '?format=json' }; } return clone(S.users);
      case 'plans': return S.plans.map(planView);
      case 'nodes': if (seg[2] === 'disk-clean') return demoDiskClean(id, false); return clone(S.nodes);
      // 主机容灾(见 haView;没有这一条时界面拿到 {},会把每台都标成「副机」)
      case 'ha': if (seg[1] === 'plan') return haDemoBlock(); if (seg[1]) return notFound();
        return haView(/(^|&)full=1(&|$)/.test(query));
      case 'exts': if (seg[2] === 'nodes') return extNodes(id, seg, 'GET', null); return clone(S.exts);
      case 'onlines': return clone(S.onlines);
      case 'rules': if (seg[2] === 'states') return clone(S.limitStates.filter(s => s.ruleId === id)); if (seg[1]) return ruleView(find(S.rules, id)); return S.rules.map(ruleView);
      case 'limitstates': return clone(S.limitStates);
      case 'resellers': if (seg[2] === 'users') return S.users.filter(u => u.resellerId === id); return clone(S.resellers);
      case 'update': return { current: 'demo', latest: '', hasUpdate: false, canUpdate: false };
      case 'ops': { if (seg[1] === 'status') return fx('GET ops/status') || { running: false, log: [] }; const o = fx('GET ops') || { info: {}, status: {}, params: {} };
        // 导出实例跑在 Windows 上,系统信息是空的;演示里给一台像样的 Linux 主机
        o.info = { ...(o.info || {}), linux: true, root: true, os: 'linux', kernel: '6.8.0-45-generic', arch: 'amd64', hostname: 'tokyo', uptime: 37 * 86400 + 4 * 3600, load: '0.12 0.09 0.05',
          memTotal: 2 * 1073741824, memAvail: 1.3 * 1073741824, swapTotal: 2 * 1073741824, swapFree: 2 * 1073741824, diskTotal: 40 * 1073741824, diskFree: 29 * 1073741824,
          cc: 'bbr', qdisc: 'fq', nofile: 1048576, ntp: 'yes', tuned: true, limits: true,
          warp: { ...((o.info || {}).warp || {}), installed: true, service: 'active', socks: 'active', port: 40000, listening: true, status: 'Connected', exit: 'on', exitIp: '104.28.0.1', exitLoc: 'JP', exitColo: 'NRT' } };
        o.status = o.status || { running: false, log: [] };
        if (!(o.tasks || []).some(x => x.name === 'disk-clean')) o.tasks = [...(o.tasks || []), { name: 'disk-clean', title: '清理磁盘', desc: '清掉软件包缓存、把 journald 回收到上限、超过 100MB 的文本日志只留最后 5MB、删掉一天前轮转下来的旧日志,并装一个每小时检查的定时器(syslog 超过 200MB 就立刻轮转)。不碰 m-ui 的数据和备份' }];
        o.disk = { path: '/etc/m-ui', total: 40 * 1073741824, used: 11 * 1073741824, free: 29 * 1073741824, logs: 1.2 * 1073741824, cache: 0.3 * 1073741824, swap: 2 * 1073741824, at: now() };
        return o; }
      case 'cert': if (seg[1] === 'status') return fx('GET cert/status') || { running: false, log: [] }; { const c = fx('GET cert') || { settings: S.settings, status: {} }; const use = S.certs.find(x => x.certFile === S.certUse.lines); return { ...c, notifyConfigured: true, certs: clone(S.certs), certPath: S.certUse.lines, panelCert: S.certUse.panel, subCert: S.certUse.sub, panelTLS: !!S.certUse.panel, subTLS: !!S.certUse.sub, source: use ? use.source : '', info: use ? clone(use.info) : { exists: false } }; }
      case 'sub-logo': return clone(S.logo || { set: false });
      case 'reach': return clone(S.reach);
      case 'conns': return recentConns();
      case 'sub-templates': return { clash: '# 只写想加的部分,节点和分组由面板填\nrules:\n  - DOMAIN-SUFFIX,example.org,DIRECT\n  - MATCH,Proxy\n', singbox: '{\n  "route": {\n    "rules": [{ "domain_suffix": ["example.org"], "outbound": "direct" }],\n    "final": "proxy"\n  }\n}\n' };
      case 'stats': { const q = new URLSearchParams(query); const tag = q.get('tag'); const hours = q.get('hours') || '24'; const bucket = q.get('bucket') || '3600';
        if (seg[1] === 'top') return listByPrefix('stats/top') || [];
        const k = tag ? `GET stats?resource=user&tag=${tag}&hours=${hours}&bucket=${bucket}` : `GET stats?resource=user&hours=${hours}&bucket=${bucket}`;
        return fx(k) ?? listByPrefix('stats') ?? []; }
      case 'keygen': { const t = new URLSearchParams(query).get('type');
        if (t === 'uuid') return { uuid: crypto.randomUUID() };
        if (t === 'port') return { port: 10000 + Math.floor(Math.random() * 55000) };
        if (t === 'shortid') return { shortId: rnd(8) };
        if (t === 'password') return { password: rnd(Number(new URLSearchParams(query).get('len')) || 16) };
        return { privateKey: 'demo-private-key-' + rnd(24), publicKey: 'demo-public-key-' + rnd(24) }; }
      case 'admin': case 'self':
        if (seg[1] === 'api' && seg[2] === 'keys') return { keys: S.apiKeys[seg[0]].map(k => demoApiKeys(seg[0], 'GET', k.id)), perms: API_PERMS, you: '203.0.113.5' };
        if (seg[0] === 'admin' && seg[1] === 'info') { // 演示里外部 API 开着:看得到密钥列表(默认密钥和一把只读、限了来源的);两步验证可以走一遍开启流程
          const i = listByPrefix(path) ?? {};
          return { ...i, apiEnabled: S.apiOn !== false, totpEnabled: !!S.totpOn, totpPending: S.totpPending ? { secret: S.totpPending } : undefined };
        }
        return listByPrefix(path) ?? {};
      default: return listByPrefix(path) ?? {};
    }
  }
  // 写操作:只改内存
  switch (seg[0]) {
    case 'login': return { username: 'admin' };
    case 'logout': return ok();
    case 'settings': Object.assign(S.settings, body || {}); return ok({ note: 'Demo mode' });
    case 'sub-logo': if (method === 'DELETE') { S.logo = null; return { set: false }; } S.logo = logoOf(body && body.data); return clone(S.logo);
    case 'users': {
      if (seg[1] === 'batch') { const { ids = [], action, days = 0 } = body || {}; let n = 0;
        for (const uid of ids) { const u = S.users.find(x => x.id === uid); if (!u) continue; n++;
          if (action === 'enable') u.enabled = true; else if (action === 'disable') u.enabled = false; else if (action === 'reset') { u.up = 0; u.down = 0; }
          else if (action === 'extend') u.expiry = Math.max(u.expiry || 0, now()) + days * 86400; else if (action === 'delete') S.users = S.users.filter(x => x.id !== uid);
          else if (action === 'plan' && body.mode !== 'bind') { const p = S.plans.find(x => x.id === body.planId); if (p) demoPlanOp(u, p, body.mode === 'extend' ? 'extend' : 'renew', Math.max(1, body.periods || 1)); } }
        return { affected: n }; }
      if (seg[1] === 'bulk') { const { prefix = 'user', count = 1 } = body || {}; const made = [];
        for (let i = 0; i < Math.min(count, 50); i++) { const u = { id: nextId(S.users), name: `${prefix}-${rnd(5)}`, enabled: true, volume: body.volume || 0, expiry: 0, up: 0, down: 0, deviceLimit: 0, lineIds: body.lineIds || S.lines.map(l => l.id), onlineIps: [], createdAt: now() }; u.subUrl = subUrl(u); S.users.push(u); made.push({ name: u.name, link: u.subUrl + '?format=clash' }); }
        return made; }
      if (method === 'POST' && !seg[1]) { const u = { id: nextId(S.users), enabled: true, up: 0, down: 0, onlineIps: [], createdAt: now(), ...body }; u.lineIds = body.lineIds || []; u.subUrl = subUrl(u); S.users.push(u); return clone(u); }
      const u = find(S.users, id);
      if (method === 'DELETE' && !seg[2]) { S.users = S.users.filter(x => x.id !== id); return ok(); }
      if (method === 'PUT') { Object.assign(u, body, { id }); u.subUrl = subUrl(u); return clone(u); }
      if (seg[2] === 'reset') { u.up = 0; u.down = 0; return ok(); }
      if (seg[2] === 'kick') { const n = (u.onlineIps || []).length; u.onlineIps = []; S.onlines.users = S.onlines.users.filter(n2 => n2 !== u.name); return { closed: n }; }
      if (seg[2] === 'plan') { const b = body || {}; if (b.mode === 'bind') { u.planId = b.planId || 0; return ok(); }
        const p = S.plans.find(x => x.id === (b.planId || u.planId)); if (!p) throw demoErr(400, '用户没有所属套餐,请给 planId 或 plan');
        const { res } = demoPlanOp(u, p, b.mode === 'extend' ? 'extend' : 'renew', Math.max(1, b.periods || 1), '', b.dryRun, b.remark); return b.dryRun ? res : ok({ expiry: u.expiry }); }
      if (seg[2] === 'change') { const b = body || {}; const p = S.plans.find(x => x.id === b.planId); if (!p) throw demoErr(400, '缺少 planId 或 plan');
        const pre = demoPlanOp(u, p, b.mode, Math.max(b.mode === 'prorate' ? 0 : 1, b.periods ?? 1), b.usage, true).res;
        if (b.dryRun) return pre;
        if (pre.needsForce && !b.force) { const e = demoErr(409, '这次变更有需要确认的影响(见 warnings),确认后带 force: true 重试'); e.data = { code: 'needs_force', warnings: pre.warnings, preview: pre }; throw e; }
        return demoPlanOp(u, p, b.mode, Math.max(b.mode === 'prorate' ? 0 : 1, b.periods ?? 1), b.usage, false, b.remark).res; }
      if (seg[2] === 'share') { if (method === 'DELETE') { u.shareUrl = ''; return ok(); } u.shareUrl = subUrl(u).replace(/\/sub\/.*$/, '/sub/share-' + rnd(20)); return { url: u.shareUrl }; }
      return ok();
    }
    case 'route-apps': return { results: demoApps(body && body.names) };
    case 'reality-check': {
      const sites = ['www.microsoft.com', 'www.apple.com', 'www.cloudflare.com', 'www.amazon.com', 'www.nvidia.com', 'www.samsung.com'];
      if (body && body.server) return { server: body.server, port: body.port || 443, ok: true, ms: 12 + Math.floor(Math.random() * 40) };
      return { results: sites.map((server, i) => ({ server, port: 443, ok: true, ms: 8 + i * 7 })) };
    }
    case 'lines': {
      if (seg[1] === 'sort') { const ids = body || []; ids.forEach((lid, i) => { const l = S.lines.find(x => x.id === lid); if (l) l.sort = i + 1; }); S.lines.sort((a, b) => (a.sort || 0) - (b.sort || 0)); return ok(); }
      if (method === 'POST' && !seg[1]) { const l = { id: nextId(S.lines), enabled: true, sort: S.lines.length + 1, userCount: 0, ...body }; l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; if (body.assignAll) { l.userCount = S.users.length; S.users.forEach(u => { u.lineIds = [...(u.lineIds || []), l.id]; }); } S.lines.push(l); return clone(l); }
      const l = find(S.lines, id);
      if (seg[2] === 'toggle') { l.enabled = !l.enabled; demoSyncBlocked(); return ok(); }
      if (method === 'DELETE') { S.lines = S.lines.filter(x => x.id !== id); S.users.forEach(u => { u.lineIds = (u.lineIds || []).filter(x => x !== id); }); demoSyncBlocked(); return ok(); }
      if (method === 'PUT') { Object.assign(l, body, { id }); l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; demoSyncBlocked(); return clone(l); }
      return ok();
    }
    case 'upstreams': {
      if (seg[1] === 'test') return S.upstreams.map(u => ({ id: u.id, servers: demoUpTest(u) }));
      if (seg[1] === 'health') { S.upstreams.forEach(demoUpTest); S.health.lastRun = now(); return ok(); } // 立即巡检
      if (seg[1] === 'parse') return { name: 'Parsed upstream', type: 'shadowsocks', options: { server: '203.0.113.20', server_port: 8388, method: 'aes-256-gcm', password: 'demo' } };
      if (method === 'POST' && !seg[1]) { const u = { id: nextId(S.upstreams), sort: S.upstreams.length + 1, ...body }; S.upstreams.push(u); return clone(u); }
      const u = find(S.upstreams, id);
      if (seg[2] === 'test') return { id, servers: demoUpTest(u) };
      if (method === 'DELETE') { S.upstreams = S.upstreams.filter(x => x.id !== id); S.upBroken.delete(id); return ok(); }
      if (method === 'PUT') { Object.assign(u, body, { id }); S.upBroken.delete(id); demoUpTest(u); return clone(u); } // 改过就算修好了,顺手复查一次
      return ok();
    }
    case 'nodes': {
      const list = S.nodes.nodes;
      if (method === 'POST' && !seg[1]) { const n = { id: nextId(list), enabled: true, isLocal: false, status: { ok: true, version: 'demo', synced: true, coreRunning: true, uptime: 60, onlineUsers: 0, lastPush: now(), lastSeen: now() }, ...body }; delete n.token; n.hasToken = true; list.push(n); return clone(n); }
      if (method === 'POST' && seg[1] === 'group') { // 改组名:整组一起改
        const from = String((body || {}).from || '').trim(), to = String((body || {}).to || '').trim();
        let changed = 0;
        for (const x of list) if ((x.group || '').trim() === from) { x.group = to; changed++; }
        return ok({ changed });
      }
      const n = find(list, id);
      if (seg[2] === 'test') return { ok: true, version: 'demo', hostname: 'demo', coreRunning: true };
      // 编辑中转入口时现问端口:演示里新加坡那台的 30001 被 realm 占着(和它上报的「端口被占」是同一件事)
      if (seg[2] === 'port-check') return id === 2 && Number((body || {}).port) === 30001 ? { asked: true, free: false, self: false, by: 'realm' } : { asked: true, free: true, self: false, by: '' };
      if (seg[2] === 'push') return ok();
      if (seg[2] === 'disk-clean') return demoDiskClean(id, true);
      if (method === 'DELETE') { S.nodes.nodes = list.filter(x => x.id !== id); return ok({ disabledLines: [] }); }
      if (method === 'PUT') { const { token, ...rest } = body || {}; Object.assign(n, rest, { id }); return clone(n); }
      return ok();
    }
    case 'plans': {
      if (method === 'POST' && !seg[1]) { const { ruleIds, ...rest } = body || {}; const p = { id: nextId(S.plans), ...rest }; S.plans.push(p); setPlanRules(p.id, ruleIds); return planView(p); }
      const p = find(S.plans, id);
      if (method === 'DELETE') { S.plans = S.plans.filter(x => x.id !== id); for (const u of S.users) if (u.planId === id) u.planId = 0; setPlanRules(id, []); return ok(); }
      if (method === 'PUT') { const { ruleIds, ...rest } = body || {}; Object.assign(p, rest, { id }); setPlanRules(id, ruleIds); return planView(p); }
      return ok();
    }
    case 'resellers': {
      if (method === 'POST' && !seg[1]) { const r = { id: nextId(S.resellers), enabled: true, used: 0, ...body }; S.resellers.push(r); return clone(r); }
      const r = find(S.resellers, id);
      if (seg[2] === 'passwd') return { password: rnd(12) };
      if (seg[2] === 'reset') return ok();
      if (method === 'DELETE') { S.resellers = S.resellers.filter(x => x.id !== id); return ok(); }
      if (method === 'PUT') { Object.assign(r, body, { id }); return clone(r); }
      return ok();
    }
    case 'exts': {
      if (method === 'POST' && !seg[1]) { const e = { id: nextId(S.exts), enabled: true, nodeCount: 3, ...body }; S.exts.push(e); return clone(e); }
      const e = find(S.exts, id);
      if (seg[2] === 'refresh') return { clash: 7, links: 7, total: 7, out: e.nodeCount || 7, unchanged: true };
      if (seg[2] === 'preview') return { nodes: [] };
      if (seg[2] === 'nodes') return extNodes(id, seg, method, body);
      if (method === 'DELETE') { S.exts = S.exts.filter(x => x.id !== id); return ok(); }
      if (method === 'PUT') { Object.assign(e, body, { id }); return clone(e); }
      return ok();
    }
    case 'rules': {
      if (method === 'POST' && !seg[1]) { const r = { id: nextId(S.rules), enabled: true, tightenOnly: true, userIds: [], resellerIds: [], createdAt: now(), ...body }; S.rules.push(r); return ruleView(r); }
      const r = find(S.rules, id);
      if (method === 'DELETE') { S.rules = S.rules.filter(x => x.id !== id); S.limitStates = S.limitStates.filter(s => s.ruleId !== id); return ok(); }
      if (method === 'PUT') { Object.assign(r, body, { id }); if (!r.enabled) S.limitStates = S.limitStates.filter(s => s.ruleId !== id); return ruleView(r); }
      return ok();
    }
    case 'update': throw new ApiError(400, 'Demo mode: nothing to update here');
    case 'ha':
      if (!seg[1]) { // 保存设置
        const b = body || {}, c = HA.config;
        if (b.takeSub && !b.dnsToken && !c.dnsTokenSet) throw new ApiError(400, zh ? '接管订阅要先填 DNS 接口(Cloudflare 令牌,或阿里云、腾讯云 DNSPod 的密钥):接任后靠它把订阅域名改指到新主机' : 'Taking over subscriptions needs a DNS interface (a Cloudflare token, or Alibaba Cloud / Tencent Cloud DNSPod keys): after a takeover it points the subscription domain to the new master');
        for (const k of ['mode', 'standbys', 'replicaMinutes', 'confirmMinutes', 'autoMinutes', 'rankStepMinutes', 'witness', 'allowPlain', 'dnsNames', 'takeSub', 'dnsProvider']) if (b[k] !== undefined) c[k] = b[k];
        if (c.takeSub && !/(^|[\s,])sub\.example\.com([\s,]|$)/.test(c.dnsNames || '')) c.dnsNames = ['sub.example.com', c.dnsNames].filter(Boolean).join(',');
        if (b.dnsToken) c.dnsTokenSet = true;
        if (b.clearDnsToken) c.dnsTokenSet = false;
        return ok();
      }
      if (seg[1] === 'pause') { HA.config.pauseUntil = body && body.minutes > 0 ? now() + body.minutes * 60 : 0; return ok(); }
      if (seg[1] === 'notice') return ok();
      if (seg[1] === 'dns-test') return { records: [], error: zh ? '演示模式:不连 DNS 服务商' : 'Demo mode: the DNS provider is not contacted' };
      throw new ApiError(400, zh ? '演示模式:不会真的换主机或接任' : 'Demo mode: the master is not actually moved or taken over');
    case 'backup': if (seg[1] === 'restore-ack') { S.restoreAcked = true; return ok(); } if (seg[1] === 'run') return { name: 'm-ui-demo-' + new Date().toISOString().slice(0, 10) + '.zip' }; if (seg[1] === 'inspect') return { users: S.users.length, lines: S.lines.length, note: 'Demo mode' }; return ok();
    case 'cert':
      if (seg[1] === 'use') { const c = S.certs.find(x => x.name === body.name); S.certUse[body.target] = c ? c.certFile : ''; syncCertUse(); return ok(); }
      if (seg[1] === 'delete') { S.certs = S.certs.filter(x => x.name !== body.name); syncCertUse(); return ok(); }
      if (seg[1] === 'renew') { const c = S.certs.find(x => x.name === body.name); if (c && c.renewable) { c.lastRenewOk = now(); c.renewFails = 0; c.lastRenewErr = ''; } return ok({ note: 'Demo mode' }); }
      if (seg[1] === 'autorenew') { const c = S.certs.find(x => x.name === body.name); if (c && c.source === 'acme' && c.method !== 'manual') { c.autoRenew = !!body.on; Object.assign(c, body.on ? renewOf(c.source, c.method, c.info.daysLeft, c.info.selfSigned) : { renewable: false, reason: 'disabled' }); } return ok(); }
      return ok({ note: 'Demo mode' });
    case 'reach': { const ids = body && body.nodeId ? [body.nodeId] : S.reach.results.map(r => r.nodeId);
      for (const r of S.reach.results) if (ids.includes(r.nodeId)) { r.at = now(); r.auto = false; if (r.v6) r.v6.at = now(); (S.reach.history[r.nodeId] = S.reach.history[r.nodeId] || []).push({ at: now(), verdict: r.verdict, auto: false }); }
      return ok(); }
    case 'ops': if (seg[1] === 'warp-check') return { exit: 'on', ip: '104.28.0.1', loc: 'US', colo: 'LAX' }; return ok();
    case 'agent': if (seg[1] === 'rotate') return { token: 'demo-' + rnd(32) }; return ok();
    case 'admin': case 'self':
      if (seg[1] === 'api' && seg[2] === 'keys') return demoApiKeys(seg[0], method, seg[3], seg[4], body);
      if (seg[0] === 'admin' && seg[1] === 'api' && !seg[2]) { S.apiOn = !!(body && body.enabled); return { enabled: S.apiOn }; }
      if (seg[0] === 'admin' && seg[1] === 'totp') {
        if (seg[2] === 'setup') { S.totpPending = 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP'; return { secret: S.totpPending }; }
        if (seg[2] === 'cancel') { S.totpPending = ''; return ok(); }
        if (seg[2] === 'enable') { if (!/^\d{6}$/.test(String((body && body.code) || ''))) throw demoErr(400, '验证码不正确,请确认认证器已添加此密钥且手机时间准确'); S.totpOn = true; S.totpPending = ''; return ok(); }
        if (seg[2] === 'disable') { S.totpOn = false; return ok(); }
      }
      return ok();
    case 'reload': case 'password': case 'notify': return ok();
    default: return ok();
  }
}

setTransport(async (rawPath, opts = {}) => {
  const method = (opts.method || 'GET').toUpperCase();
  const [path, query = ''] = rawPath.split('?');
  let body = null; if (opts.body) { try { body = JSON.parse(opts.body); } catch { body = null; } }
  await new Promise(r => setTimeout(r, 40)); // 像一次真实请求
  const res = route(method, path, query, body);
  if (method !== 'GET' && !(body && body.dryRun) && !READONLY_POST.test(path)) demoNote();
  return res;
});
// 两步验证的二维码(id 为 'totp')演示里没有真的:拿一张现成的订阅二维码顶上,样子对就行
setQrUrl((id, fmt) => (data.qr || {})[`${id}:${fmt}`] || (id === 'totp' && Object.values(data.qr || {})[0]) || 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7');

// 写操作在本页内存里"成功"后提示一句:面板自己的提示(已保存、已续费……)后面接上"演示模式,刷新即恢复";
// 面板这次没弹提示就单独弹一条。只算真正改东西的请求:测速、预览、解析、生成密钥这类不算。
const READONLY_POST = /^(login|logout|upstreams\/(parse|test|\d+\/test)|exts\/\d+\/(preview|nodes\/(test|reach))|nodes\/\d+\/(test|port-check)|reality-check|route-apps|cert\/precheck|backup\/inspect|ops\/warp-check|keygen)/;
let noteWanted = 0, noteTimer = 0;
const noteText = () => ((localStorage.getItem('m-ui-lang') || navigator.language || 'en').startsWith('zh') ? '演示模式,刷新即恢复' : 'Demo mode: resets on refresh');
function demoNote() {
  noteWanted = Date.now();
  clearTimeout(noteTimer);
  noteTimer = setTimeout(() => { if (noteWanted) { noteWanted = 0; toast(noteText(), 'ok'); } }, 700);
}
const toastEl = document.getElementById('toast');
if (toastEl) new MutationObserver(() => {
  if (!noteWanted || toastEl.hidden || Date.now() - noteWanted > 3000) return;
  const note = noteText();
  noteWanted = 0; clearTimeout(noteTimer);
  if (!toastEl.textContent.includes(note)) toastEl.textContent += ' · ' + note;
}).observe(toastEl, { childList: true, characterData: true, subtree: true, attributes: true, attributeFilter: ['hidden', 'class'] });

// 演示提示条 + 把"支持"链接指到官网;登录框不会出现(status 直接返回),退出登录后随便填也能进
const zh = (localStorage.getItem('m-ui-lang') || navigator.language || 'en').startsWith('zh');
const bar = document.createElement('div');
bar.className = 'alert-bar info';
bar.id = 'demo-bar';
bar.innerHTML = zh
  ? `<span><b>演示模式</b> —— 改动只在本页内存里,刷新即恢复;没有真实服务器,订阅地址与二维码不可用。</span><a class="btn sm" href="${SITE}zh/getting-started/">五分钟装一个</a><a class="btn sm ghost" href="${GITHUB}" rel="noopener">GitHub</a>`
  : `<span><b>Demo mode</b> — changes are local and reset on refresh. No real server behind this: subscription links and QR codes are placeholders.</span><a class="btn sm" href="${SITE}getting-started/">Install in 5 minutes</a><a class="btn sm ghost" href="${GITHUB}" rel="noopener">GitHub</a>`;
const mount = () => { const main = document.querySelector('.main'); const page = document.getElementById('page'); if (main && page && !document.getElementById('demo-bar')) main.insertBefore(bar, page); };
for (const id of ['btn-support', 'login-support']) { const a = document.getElementById(id); if (a) a.href = GITHUB + '/discussions'; }
document.title = zh ? 'm-ui 在线演示 · sing-box 面板' : 'm-ui live demo · sing-box panel';

await import('./app.js');
mount();
new MutationObserver(mount).observe(document.getElementById('app'), { childList: true });
setTimeout(() => toast(zh ? '演示模式:可以随便点,刷新即恢复' : 'Demo mode: click anything, refresh to reset', 'ok'), 800);

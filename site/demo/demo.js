// 官网的静态 Live Demo:同一套面板前端(web/assets 原样复制),只把 api.js 的传输层换成
// 内存里的演示数据。不发任何网络请求,没有后端,改动只在本页内存里,刷新即恢复。
//
// 演示数据来自 docs/screenshots/make.sh 跑起来的真实实例(fixtures.json),所以接口形状和生产一致;
// 这里只做两件事:按 "METHOD path" 找 fixture,以及把增删改落到内存里的几张表上。
import { setTransport, setQrUrl, ApiError } from './api.js';
import { toast } from './ui.js';

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
};
// 让演示像在跑:两个用户在线,各带文档保留段里的 IP
const online = { 'demo-alice': ['203.0.113.5', '198.51.100.7'], 'demo-bob': ['192.0.2.44'] };
for (const u of S.users) if (online[u.name]) { u.onlineIps = online[u.name]; u.onlineAt = Math.floor(Date.now() / 1000) - 60; }
S.onlines.users = Object.keys(online).filter(n => S.users.some(u => u.name === n));
S.onlines.connCounts = Object.fromEntries(S.onlines.users.map((n, i) => [n, 3 + i * 2]));
S.onlines.lines = S.lines.slice(0, 2).map(l => l.name);
for (const n of S.nodes.nodes || []) if (n.status) n.status.uptime = (n.status.uptime || 0) + DELTA; // 副机运行时长随导出日期累加

const nextId = list => list.reduce((m, x) => Math.max(m, x.id || 0), 0) + 1;
// 外部订阅展开:节点明细 / 逐台测速 / 添加为上游(GET 与 POST 都走这里)
function extNodes(id, seg, method, body) {
  let servers = ((S.nodes && S.nodes.nodes) || (Array.isArray(S.nodes) ? S.nodes : [])).filter(n => n.enabled !== false).map(n => ({ id: n.id, name: n.name, isLocal: !!n.isLocal }));
  if (!servers.length) servers = [{ id: 1, name: '主机', isLocal: true }, { id: 2, name: '副机', isLocal: false }];
  const nodes = demoExtNodes(id);
  const ext = S.exts.find(e => e.id === id) || {};
  const words = String(ext.exclude || '').split(',').map(s => s.trim()).filter(Boolean);
  for (const n of nodes) {
    if (words.some(w => n.name.includes(w))) { n.hidden = true; n.keyword = true; }
    else if ((ext.hidden || []).includes(n.name)) n.hidden = true;
    if ((ext.renames || {})[n.name]) n.rename = ext.renames[n.name];
  }
  if (!seg[3]) return { nodes, servers, fetchedAt: now() - 600, sig: 'demo' };
  if (seg[3] === 'prefs') { ext.hidden = (body && body.hidden) || []; ext.renames = (body && body.renames) || {}; return ok(); }
  if (seg[3] === 'reach' && !seg[4]) { S.reachIdx = (body && body.indexes) || []; return { job: 'demo-reach-' + id }; }
  if (seg[3] === 'reach') {
    // 演示用:第一个被墙,其余通
    const results = {};
    (S.reachIdx || []).forEach((i, k) => { results[i] = { state: 'done', verdict: k === 0 ? 'blocked' : 'ok', groups: [{ key: 'ct', ok: k === 0 ? 0 : 3, total: 3 }, { key: 'cu', ok: k === 0 ? 0 : 3, total: 3 }, { key: 'cm', ok: k === 0 ? 0 : 3, total: 3 }] }; });
    return { done: true, results };
  }
  if (seg[3] === 'test' && !seg[4]) return { job: 'demo-' + id };
  if (seg[3] === 'test') {
    const results = {};
    for (const n of nodes) { results[n.index] = {}; for (const s of servers) results[n.index][s.id] = Math.random() < 0.8 ? { state: 'ok', delayMs: 60 + Math.floor(Math.random() * 300), method: 'urltest' } : { state: 'fail', error: 'timeout' }; }
    return { done: true, total: nodes.length * servers.length, finished: nodes.length * servers.length, results };
  }
  if (seg[3] === 'add-upstream') return { results: (body || []).map(x => ({ index: x.index, name: x.name, ok: true, id: 900 + x.index })) };
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
  index: i, name, type, server, port, upstream: true,
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
const certOf = (id, name, source, method, names, days, self, lineNames) => ({
  id, name, source, method, autoRenew: source === 'acme' && method !== 'manual', staging: false, createdAt: now() - 30 * DAY,
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
if (!S.exts.length) S.exts.push({ id: 1, name: 'Demo Sub', type: 'sub', value: 'https://sub.example.net/demo', prefix: '', enabled: true, sort: 1, remark: '', lastFetch: now() - 600, lastError: '', nodeCount: 7, exclude: '官网', hidden: [], renames: {} });
const ruleView = r => ({ ...r, targetCount: r.allUsers ? S.users.filter(u => u.enabled).length : new Set([...(r.userIds || []), ...S.users.filter(u => (r.resellerIds || []).includes(u.resellerId) || (r.planIds || []).includes(u.planId)).map(u => u.id)]).size, activeCount: S.limitStates.filter(s => s.ruleId === r.id).length });

function status() {
  const s = fx('GET status') || {};
  s.users = S.users.length; s.enabledUsers = S.users.filter(u => u.enabled).length; s.lines = S.lines.length; s.linesEnabled = S.lines.filter(l => l.enabled).length;
  s.upstreams = S.upstreams.length; s.onlineUsers = S.onlines.users.length; s.version = 'demo'; s.repo = GITHUB; s.defaultPassword = false;
  return s;
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
      case 'lines': return clone(S.lines);
      case 'upstreams': if (seg[1] === 'health') return fx('GET upstreams/health') || {}; return clone(S.upstreams);
      case 'users': if (seg[2] === 'sub') { const u = find(S.users, id); return { link: subUrl(u), clash: subUrl(u) + '?format=clash', json: subUrl(u) + '?format=json' }; } return clone(S.users);
      case 'plans': return S.plans.map(planView);
      case 'nodes': return clone(S.nodes);
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
        o.status = o.status || { running: false, log: [] }; return o; }
      case 'cert': if (seg[1] === 'status') return fx('GET cert/status') || { running: false, log: [] }; { const c = fx('GET cert') || { settings: S.settings, status: {} }; const use = S.certs.find(x => x.certFile === S.certUse.lines); return { ...c, certs: clone(S.certs), certPath: S.certUse.lines, panelCert: S.certUse.panel, subCert: S.certUse.sub, panelTLS: !!S.certUse.panel, subTLS: !!S.certUse.sub, source: use ? use.source : '', info: use ? clone(use.info) : { exists: false } }; }
      case 'sub-templates': return { clash: '# 只写想加的部分,节点和分组由面板填\nrules:\n  - DOMAIN-SUFFIX,example.org,DIRECT\n  - MATCH,PROXY\n', singbox: '{\n  "route": {\n    "rules": [{ "domain_suffix": ["example.org"], "outbound": "direct" }],\n    "final": "proxy"\n  }\n}\n' };
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
      default: return listByPrefix(path) ?? {};
    }
  }
  // 写操作:只改内存
  switch (seg[0]) {
    case 'login': return { username: 'admin' };
    case 'logout': return ok();
    case 'settings': Object.assign(S.settings, body || {}); return ok({ note: 'Demo mode' });
    case 'users': {
      if (seg[1] === 'batch') { const { ids = [], action, days = 0 } = body || {}; let n = 0;
        for (const uid of ids) { const u = S.users.find(x => x.id === uid); if (!u) continue; n++;
          if (action === 'enable') u.enabled = true; else if (action === 'disable') u.enabled = false; else if (action === 'reset') { u.up = 0; u.down = 0; }
          else if (action === 'extend') u.expiry = Math.max(u.expiry || 0, now()) + days * 86400; else if (action === 'delete') S.users = S.users.filter(x => x.id !== uid); }
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
      if (seg[2] === 'plan') { const p = S.plans.find(x => x.id === (body && body.planId)); if (p) { u.volume = (p.volumeGb || 0) * 1073741824; u.expiry = p.days ? now() + p.days * 86400 : 0; u.deviceLimit = p.deviceLimit || 0; } return ok(); }
      if (seg[2] === 'share') { if (method === 'DELETE') { u.shareUrl = ''; return ok(); } u.shareUrl = subUrl(u).replace(/\/sub\/.*$/, '/sub/share-' + rnd(20)); return { url: u.shareUrl }; }
      return ok();
    }
    case 'route-apps': return { results: demoApps(body && body.names) };
    case 'lines': {
      if (seg[1] === 'sort') { const ids = body || []; ids.forEach((lid, i) => { const l = S.lines.find(x => x.id === lid); if (l) l.sort = i + 1; }); S.lines.sort((a, b) => (a.sort || 0) - (b.sort || 0)); return ok(); }
      if (method === 'POST' && !seg[1]) { const l = { id: nextId(S.lines), enabled: true, sort: S.lines.length + 1, userCount: 0, ...body }; l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; if (body.assignAll) { l.userCount = S.users.length; S.users.forEach(u => { u.lineIds = [...(u.lineIds || []), l.id]; }); } S.lines.push(l); return clone(l); }
      const l = find(S.lines, id);
      if (seg[2] === 'toggle') { l.enabled = !l.enabled; return ok(); }
      if (method === 'DELETE') { S.lines = S.lines.filter(x => x.id !== id); S.users.forEach(u => { u.lineIds = (u.lineIds || []).filter(x => x !== id); }); return ok(); }
      if (method === 'PUT') { Object.assign(l, body, { id }); l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; return clone(l); }
      return ok();
    }
    case 'upstreams': {
      if (seg[1] === 'test') return { ok: true, results: S.upstreams.map(u => ({ id: u.id, ok: true, latencyMs: 40 + Math.floor(Math.random() * 80) })) };
      if (seg[1] === 'parse') return { name: 'Parsed upstream', type: 'shadowsocks', options: { server: '203.0.113.20', server_port: 8388, method: 'aes-256-gcm', password: 'demo' } };
      if (method === 'POST' && !seg[1]) { const u = { id: nextId(S.upstreams), sort: S.upstreams.length + 1, ...body }; S.upstreams.push(u); return clone(u); }
      const u = find(S.upstreams, id);
      if (seg[2] === 'test') return { ok: true, latencyMs: 40 + Math.floor(Math.random() * 80), exitIp: '203.0.113.20' };
      if (method === 'DELETE') { S.upstreams = S.upstreams.filter(x => x.id !== id); return ok(); }
      if (method === 'PUT') { Object.assign(u, body, { id }); return clone(u); }
      return ok();
    }
    case 'nodes': {
      const list = S.nodes.nodes;
      if (method === 'POST' && !seg[1]) { const n = { id: nextId(list), enabled: true, isLocal: false, status: { ok: true, version: 'demo', synced: true, coreRunning: true, uptime: 60, onlineUsers: 0, lastPush: now(), lastSeen: now() }, ...body }; delete n.token; n.hasToken = true; list.push(n); return clone(n); }
      const n = find(list, id);
      if (seg[2] === 'test') return { ok: true, version: 'demo', hostname: 'demo', coreRunning: true };
      if (seg[2] === 'push') return ok();
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
      if (seg[2] === 'refresh') return { clash: 3, link: 3 };
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
    case 'backup': if (seg[1] === 'run') return { name: 'm-ui-demo-' + new Date().toISOString().slice(0, 10) + '.zip' }; if (seg[1] === 'inspect') return { users: S.users.length, lines: S.lines.length, note: 'Demo mode' }; return ok();
    case 'cert':
      if (seg[1] === 'use') { const c = S.certs.find(x => x.name === body.name); S.certUse[body.target] = c ? c.certFile : ''; syncCertUse(); return ok(); }
      if (seg[1] === 'delete') { S.certs = S.certs.filter(x => x.name !== body.name); syncCertUse(); return ok(); }
      return ok({ note: 'Demo mode' });
    case 'ops': if (seg[1] === 'warp-check') return { exit: 'on', ip: '104.28.0.1', loc: 'US', colo: 'LAX' }; return ok();
    case 'agent': if (seg[1] === 'rotate') return { token: 'demo-' + rnd(32) }; return ok();
    case 'admin': if (seg[1] === 'api' && seg[2] === 'rotate') return { token: 'demo-' + rnd(32) }; if (seg[1] === 'totp' && seg[2] === 'setup') return { secret: 'DEMOSECRET', url: 'otpauth://totp/m-ui:admin?secret=DEMOSECRET', qr: '' }; return ok();
    case 'self': return ok();
    case 'reload': case 'password': case 'notify': return ok();
    default: return ok();
  }
}

setTransport(async (rawPath, opts = {}) => {
  const method = (opts.method || 'GET').toUpperCase();
  const [path, query = ''] = rawPath.split('?');
  let body = null; if (opts.body) { try { body = JSON.parse(opts.body); } catch { body = null; } }
  await new Promise(r => setTimeout(r, 40)); // 像一次真实请求
  return route(method, path, query, body);
});
setQrUrl((id, fmt) => (data.qr || {})[`${id}:${fmt}`] || 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7');

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

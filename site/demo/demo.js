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
    v6: { state: 'ok', target: '2001:db8:7::30', port: 24433, portLine: 'Frankfurt-Reality', ok: 4, total: 4, at: now() - 40 * 60,
      probes: [probe('Amsterdam', 'Example Net', 64500, 9, 'NL'), probe('Paris', 'Example Net', 64501, 14, 'FR'), probe('New York', 'Example Net', 64502, 82, 'US'), probe('Tokyo', 'Example Net', 64503, 230, 'JP')] } },
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

function status() {
  const s = fx('GET status') || {};
  s.users = S.users.length; s.enabledUsers = S.users.filter(u => u.enabled).length; s.lines = S.lines.length; s.linesEnabled = S.lines.filter(l => l.enabled).length;
  s.upstreams = S.upstreams.length; s.onlineUsers = S.onlines.users.length; s.version = 'demo'; s.repo = GITHUB; s.defaultPassword = false;
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
      case 'users': if (seg[2] === 'plan-ops') return clone(S.planOps.filter(o => o.userId === id).slice(0, Number(new URLSearchParams(query).get('limit')) || 20));
        if (seg[2] === 'sub') { const u = find(S.users, id); return { link: subUrl(u), clash: subUrl(u) + '?format=clash', json: subUrl(u) + '?format=json' }; } return clone(S.users);
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
      if (seg[2] === 'toggle') { l.enabled = !l.enabled; return ok(); }
      if (method === 'DELETE') { S.lines = S.lines.filter(x => x.id !== id); S.users.forEach(u => { u.lineIds = (u.lineIds || []).filter(x => x !== id); }); return ok(); }
      if (method === 'PUT') { Object.assign(l, body, { id }); l.upstreamName = (S.upstreams.find(x => x.id === l.upstreamId) || {}).name || 'direct'; return clone(l); }
      return ok();
    }
    case 'upstreams': {
      if (seg[1] === 'test') return S.upstreams.map(u => ({ id: u.id, servers: [demoUpResult(u)] }));
      if (seg[1] === 'parse') return { name: 'Parsed upstream', type: 'shadowsocks', options: { server: '203.0.113.20', server_port: 8388, method: 'aes-256-gcm', password: 'demo' } };
      if (method === 'POST' && !seg[1]) { const u = { id: nextId(S.upstreams), sort: S.upstreams.length + 1, ...body }; S.upstreams.push(u); return clone(u); }
      const u = find(S.upstreams, id);
      if (seg[2] === 'test') return { id, servers: [demoUpResult(u)] };
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
  const res = route(method, path, query, body);
  if (method !== 'GET' && !(body && body.dryRun) && !READONLY_POST.test(path)) demoNote();
  return res;
});
setQrUrl((id, fmt) => (data.qr || {})[`${id}:${fmt}`] || 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7');

// 写操作在本页内存里"成功"后提示一句:面板自己的提示(已保存、已续费……)后面接上"演示模式,刷新即恢复";
// 面板这次没弹提示就单独弹一条。只算真正改东西的请求:测速、预览、解析、生成密钥这类不算。
const READONLY_POST = /^(login|logout|upstreams\/(parse|test|\d+\/test)|exts\/\d+\/(preview|nodes\/(test|reach))|nodes\/\d+\/test|reality-check|route-apps|cert\/precheck|backup\/inspect|ops\/warp-check|keygen)/;
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

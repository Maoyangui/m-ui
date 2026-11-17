// 流量奖励的登记服务(Cloudflare Worker + D1)。面板这一侧在 reward/、runner/reward.go、web/reward_api.go。
//
// 它做四件事:
//   1. 收面板的上报(POST /v1/report):验签名、认面板、按上限把新增的流量算进去。每个面板一把 Ed25519 密钥,
//      上报只有 公钥、Solana 地址、累计流量(GB)、时间、签名 —— 这里也只存这些;来源 IP 只存加盐的哈希。
//   2. 每天给每个地址最多入账 1 积分(每满 1 TB 一个;多出来的顺延,不作废),当天要有面板在上报。
//   3. 自动识别作弊:按固定的几条规则判(见 RULES 和 findCheats),命中的那台面板的上报不再计入、写明原因,
//      条件消失 14 天后自动恢复。全程没有人工的步骤;每一次不计入 / 恢复 / 作废都记在公开账本里。
//      判的是「面板」不是「地址」:谁都能把别人的地址绑到自己的面板上,要是按地址判,就能拿它去害人。
//   4. 奖池:
//      - 订单:只读订阅站的订单库,取「监听成功」的付款(payments.status = 'credited',两条链都算),按 40% 计入。
//      - 捐赠:只认留名捐赠。先登记「留名 + 金额」,拿到一个带零头的确切金额,60 分钟内这个数转到奖池的 Solana 地址,
//        就全额计入奖池并记上名字。没登记就直接转进来的、过了期限才到的,一律不计入 —— 这个地址上还有别的资金流动,
//        只有对得上一笔登记的才能确定是给奖池的。到没到账由登记服务自己看链(订阅站只在有待付订单时才扫链)。
//        订单的金额第 6 位小数一定不是 0(订阅站的规则),留名捐赠最多 3 位小数,两者永远不会相同。
//
// 对外的接口:
//   POST /v1/report              面板上报
//   GET  /v1/export              公开账本(仓库 rewards 分支上的 ledger.json 就是它,由 ledger-workflow.yml 定时提交)
//   POST /v1/donate              {name, amount} → 要转的确切金额和期限
//   GET  /v1/donate/<id>         这一笔到没到账
//   POST /v1/donate/<id>/cancel  不捐了(面板上这一笔就此收起。但钱要是真的在期限内按那个数转来了,照样计入、照样记名:
//                                钱不能因为点了一下取消就白转;那个金额在期限过完之前也不会发给别人)
//   GET  /v1/health              定时任务最近一次跑的情况
//
// 定时(每 2 分钟):同步订单、判作弊、入账积分、看链上的新转入对留名、重建账本。
// 免费套餐一次调用最多 50 个子请求,定时任务按预算干活(见 metered),干不完的留给下一轮。
//
// 绑定:DB = 自己的库(schema.sql);COMMERCE = 订阅站的订单库(只 SELECT —— 句柄在代码里套了一层只放行查询,见 readOnly;而且只读这几张表:payments、
// 两张收款地址表、solana_amount_reservations);SALT = 给来源做哈希的盐;SOLANA_RPC = 自己的 Solana 节点(可选,可以写多个、逗号隔开,排在内置的公共节点前面)。
//
// 做不到的事说在前面:面板是开源自建的,累计值由面板自己报,改过的程序想报多少报多少 —— 签名只证明「是同一台面板报的」。
// 所以防的不是「数字造假」本身,而是让造假不划算。能保证的只有一条,但很硬:
//   每天入账的积分总数 ≤ 当天有面板在上报的「来源」的个数。
// 因为每个地址每天最多 1 积分、入账当天要有计入中的面板在上报、而一处来源(一个公网 IPv4 地址;只有 IPv6 的按 /48)
// 只认一个地址。想多拿就得多一处来源,也就是多租一台有独立公网地址的机器 —— 那是要花钱的。
// 另外两条规则抓的是偷懒的造假:同一份流量登记两次(曲线一模一样)、每天涨得一模一样。

export const CFG = {
  gbPerPoint: 1024,                                  // 1 TB
  capGbPerDay: 25 * 1024,                            // 一台面板一天最多算进这么多(约等于 2.4 Gbps 跑满一整天);超出的不算
  programStart: Date.UTC(2026, 10, 17) / 1000,       // 计划开始:首次登记能带进来的历史累计 = 这以后的天数 × 上限
  payoutAt: Date.UTC(2027, 8, 2, 16) / 1000,         // 2027-09-03 00:00(UTC+8)
  orderRate: 0.4,                                    // 订单金额计入奖池的比例
  tsSkew: 900,                                       // 上报里的时间和这边差得不能超过这么多秒
  reportGap: 240,                                    // 同一台面板两次上报至少隔这么久(更勤的只回现状,不处理)
  touchGap: 3600,                                    // 没有新流量的上报:至少隔这么久才刷新一次「最后出现」
  rebindCooldown: 7 * 86400,                         // 换地址的冷却期
  newPanelsPerSrcDay: 5,                             // 一处来源一天最多新登记这么多台面板
  creditActive: 86400,                               // 入账要求:这么久之内有计入中的面板上报过
  sampleKeep: 60,                                    // 每天一条的记录留这么多天
  // 留名捐赠
  intentWindow: 3600,                                // 这么久之内到账才算
  intentGrace: 300,                                  // 链上时间和登记时间之间留的余量
  intentsPerIpHour: 6,
  donateMin: 1_000_000, donateMax: 100_000_000_000,  // 留名捐赠的金额范围:1 – 100000 USDT(单位:百万分之一)
  // 自动识别作弊的门槛
  restoreAfter: 14 * 86400,                          // 不计入的面板连续这么久没有再命中任何规则:自动恢复
  activeWindow: 3 * 86400,                           // 「同时在用」= 这么久之内都上报过
  youngPanel: 86400,                                 // 登记不满这么久就被判的面板:它带进来的历史累计一并作废
  robotDays: 14, robotMinGb: 50, robotSpread: 0.002, // 连续这么多天,每天涨的量几乎一模一样(最大最小差不到 0.2% 或 1 GB)
  twinDays: 7, twinTolerance: 0.005,                 // 两台面板连续这么多天每天涨的量都一样(差不到 0.5% 或 1 GB)
};

// 自动判定的规则:代号 → 公开写在账本里的原因。
export const RULES = {
  same_src: '这处来源(公网 IP)上已经有别的地址在登记:一处来源只认先到的那个地址',
  twins: '和另一台面板的流量曲线一模一样:同一份流量只能登记一次,只认先登记的那台',
  robot: '每天的流量增长一模一样,不像真实流量',
};

export const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';

// ---- 小工具 ----
const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
export function b58len(s) { // base58 解出来有多少字节;不是 base58 返回 -1
  if (typeof s !== 'string' || !s) return -1;
  let n = 0n;
  for (const ch of s) { const d = B58.indexOf(ch); if (d < 0) return -1; n = n * 58n + BigInt(d); }
  let size = 0;
  for (; n > 0n; n >>= 8n) size++;
  for (let i = 0; i < s.length && s[i] === '1'; i++) size++;
  return size;
}
export const validSolana = a => typeof a === 'string' && a.length >= 32 && a.length <= 44 && b58len(a) === 32;
export const validTron = a => typeof a === 'string' && a.length === 34 && a[0] === 'T' && b58len(a) === 25;
export const chainOf = a => validTron(a) ? 'tron' : validSolana(a) ? 'solana' : '';
export const canonical = r => `m-ui-reward-v1\n${r.pub}\n${r.address}\n${r.gb}\n${r.ts}`; // 和 reward.Canonical 一字不差
const b64u = s => { s = String(s).replace(/-/g, '+').replace(/_/g, '/'); while (s.length % 4) s += '='; return Uint8Array.from(atob(s), c => c.charCodeAt(0)); };
const hex = buf => [...new Uint8Array(buf)].map(b => b.toString(16).padStart(2, '0')).join('');
export const dayOf = ts => Math.floor((ts + 8 * 3600) / 86400);  // 按 UTC+8 的自然日
export const usd = m => Math.round(Number(m) / 1e4) / 100;       // 百万分之一 → 两位小数
const sec = t => { t = Number(t) || 0; return t > 1e12 ? Math.floor(t / 1000) : Math.floor(t); }; // 订单库里的时间是毫秒
export function micros(v) { // 金额 → 百万分之一;不是数返回 NaN
  const n = typeof v === 'number' ? v : typeof v === 'string' && /^\d+(\.\d+)?$/.test(v.trim()) ? Number(v) : NaN;
  return Number.isFinite(n) ? Math.round(n * 1e6) : NaN;
}

export async function verifyReport(r) {
  try {
    const pub = b64u(r.pub), sig = b64u(r.sig);
    if (pub.length !== 32 || sig.length !== 64) return false;
    const data = new TextEncoder().encode(canonical(r));
    for (const alg of [{ name: 'Ed25519' }, { name: 'NODE-ED25519', namedCurve: 'NODE-ED25519' }]) { // 后一个是 Workers 早先的叫法
      let key;
      try { key = await crypto.subtle.importKey('raw', pub, alg, false, ['verify']); } catch { continue; }
      return await crypto.subtle.verify(alg, key, sig, data);
    }
    return false;
  } catch { return false; }
}

// ---- 来源 ----
// 「一处来源」:IPv4 是那个地址;IPv6 是它所在的 /48(一台机器、一条隧道随手就能有一整个 /48,不能一个 /64 算一处)。
// 面板上报时优先走 IPv4(见 runner/reward.go),所以只有没有 IPv4 的机器才会落到 /48 这一档。
// 只拿去做加盐哈希,不存地址本身。写法看不懂的返回空(空的不参与任何规则)。
function v4octets(ip) {
  const o = String(ip).split('.');
  if (o.length !== 4 || o.some(x => !/^\d{1,3}$/.test(x) || Number(x) > 255)) return null;
  return o.map(Number);
}
function v6groups(ip) {
  const parts = String(ip).toLowerCase().split('%')[0].split('::');
  if (parts.length > 2) return null;
  const left = parts[0] ? parts[0].split(':') : [], right = parts.length === 2 && parts[1] ? parts[1].split(':') : [];
  if (parts.length === 1 && left.length !== 8) return null;
  const fill = parts.length === 2 ? 8 - left.length - right.length : 0;
  if (fill < 0) return null;
  const g = [...left, ...Array(fill).fill('0'), ...right];
  if (g.length !== 8 || g.some(x => !/^[0-9a-f]{1,4}$/.test(x))) return null;
  return g.map(x => parseInt(x, 16).toString(16));
}
function splitIp(ip) {
  ip = String(ip || '').trim();
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(ip);
  if (mapped) ip = mapped[1];
  const o = v4octets(ip);
  if (o) return { v4: o };
  const g = v6groups(ip);
  return g ? { v6: g } : {};
}
export function srcKey(ip) { const p = splitIp(ip); return p.v4 ? p.v4.join('.') : p.v6 ? p.v6.slice(0, 3).join(':') + '::/48' : ''; }
export async function hashOf(salt, what) {
  if (!what) return '';
  return hex(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(String(salt || '') + '|' + what))).slice(0, 24);
}

// 留名怎么收(和 reward.CleanName 一样):并空白、不许控制字符和网址、最多 20 个字
export function cleanName(s) {
  s = String(s ?? '').split(/\s+/).filter(Boolean).join(' ');
  if (!s) return { err: '请填写留名', code: 'name_empty' };
  const chars = [...s];
  if (chars.some(c => { const n = c.codePointAt(0); return n < 0x20 || n === 0x7f || (n >= 0x80 && n < 0xa0) || n === 0x200b || n === 0x202e || n === 0xfeff; })) return { err: '留名里有不能用的字符', code: 'name_chars' };
  if (chars.length > 20) return { err: '留名最多 20 个字', code: 'name_long' };
  const low = s.toLowerCase();
  if (low.includes('://') || low.includes('www.')) return { err: '留名不能是网址', code: 'name_url' };
  return { name: s };
}
// 不让上榜的字眼(NAME_BLOCK:逗号隔开,不分大小写):命中的这笔照样是留名捐赠、照样进奖池,只是名字不显示。
export const HIDDEN_NAME = '已隐藏';
function shownName(env, name) {
  const low = String(name || '').toLowerCase();
  for (const w of String(env.NAME_BLOCK || '').toLowerCase().split(',').map(x => x.trim()).filter(Boolean)) if (low.includes(w)) return HIDDEN_NAME;
  return name || HIDDEN_NAME;
}

// ---- 数据库(D1)----
// 订单库只读。D1 的绑定没有「只读」这个选项(绑上就能写),所以在代码里套一层:拿到的句柄只有 prepare,而且只收单独一条 SELECT;
// 没有 run、batch、exec、dump。以后哪段代码想改订单库,在这里就被拒了,语句到不了库。所有读订单库的地方都经 commerce(env) 拿句柄。
const WRITES = /\b(INSERT|UPDATE|DELETE|REPLACE|CREATE|DROP|ALTER|ATTACH|DETACH|PRAGMA|VACUUM|REINDEX|TRUNCATE)\b/i;
export function readOnly(db) {
  if (!db || db.readOnly === true) return db;
  return {
    readOnly: true,
    prepare(sql) {
      const text = String(sql);
      if (!/^\s*SELECT\b/i.test(text) || text.includes(';') || WRITES.test(text)) throw new Error('订单库只读,拒绝执行:' + text.slice(0, 60));
      const mk = st => ({ bind: (...a) => mk(st.bind(...a)), all: () => st.all(), first: (...a) => st.first(...a) });
      return mk(db.prepare(text));
    },
  };
}
const commerce = env => readOnly(env.COMMERCE);

const q = (db, sql, ...args) => db.prepare(sql).bind(...args);
const all = async (db, sql, ...args) => (await q(db, sql, ...args).all()).results || [];
const one = (db, sql, ...args) => q(db, sql, ...args).first();
const run = (db, sql, ...args) => q(db, sql, ...args).run();
const kvGet = async (db, k) => { const r = await one(db, 'SELECT v FROM kv WHERE k = ?', k); return r ? r.v : ''; };
const kvSet = (db, k, v) => run(db, 'INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v', k, String(v));
const kvJSON = async (db, k, dflt) => { try { const v = await kvGet(db, k); return v ? JSON.parse(v) : dflt; } catch { return dflt; } };
// 分批执行;返回一共改动了多少行
async function batched(db, stmts) {
  let changed = 0;
  for (let i = 0; i < stmts.length; i += 50) for (const r of await db.batch(stmts.slice(i, i + 50))) changed += (r && r.meta && r.meta.changes) || 0;
  return changed;
}
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' } });
// 错:一句给人看的话 + 一个代号。面板认识的代号会换成它自己的说法(这样英文界面有译文),不认识的原样显示这句话。
const fail = (status, error, code = '', extra = {}) => json({ error, ...(code ? { code } : {}), ...extra }, status);

async function stateOf(db, pub) {
  const p = await one(db, 'SELECT gb, address, excluded, rule, reason, excluded_at FROM panels WHERE pub = ?', pub);
  if (!p) return null;
  const a = await one(db, 'SELECT credited FROM addresses WHERE address = ?', p.address);
  return { gb: p.gb, credited: a ? a.credited : 0, ...(p.excluded ? { excluded: { rule: p.rule, reason: p.reason, at: p.excluded_at } } : {}) };
}

// ---- 上报 ----
export async function handleReport(env, body, ip, now) {
  const db = env.DB;
  const r = body || {};
  if (r.v !== 1 || typeof r.pub !== 'string' || typeof r.sig !== 'string' || !Number.isInteger(r.gb) || !Number.isInteger(r.ts) || r.gb < 0 || r.gb > 1e9) return fail(400, '上报的格式不对', 'format');
  if (!validSolana(r.address)) return fail(400, '这不是 Solana 地址', 'address');
  if (Math.abs(now - r.ts) > CFG.tsSkew) return fail(400, '面板的时间和登记服务差得太多,请先校准服务器时间', 'clock');
  if (!await verifyReport(r)) return fail(400, '签名不对', 'sig');
  const src = await hashOf(env.SALT, srcKey(ip));
  const today = dayOf(now);
  const sample = gb => q(db, 'INSERT INTO samples(pub, day, gb) VALUES(?,?,?) ON CONFLICT(pub, day) DO UPDATE SET gb = MAX(samples.gb, excluded.gb)', r.pub, today, gb);

  const p = await one(db, 'SELECT * FROM panels WHERE pub = ?', r.pub);
  if (!p) {
    // 新面板。能带进来的历史累计有上限(计划开始以来的天数 × 每天的上限),多出来的不算。
    if (src) {
      const recent = await one(db, 'SELECT COUNT(*) AS n FROM panels WHERE ip_hash = ? AND first_seen > ?', src, now - 86400);
      if (recent && recent.n >= CFG.newPanelsPerSrcDay) return fail(429, '这处来源今天登记的面板太多了,明天再试', 'busy_src');
    }
    const days = Math.max(1, Math.ceil((now - CFG.programStart) / 86400));
    await db.batch([
      q(db, 'INSERT INTO addresses(address, first_seen) VALUES(?, ?) ON CONFLICT(address) DO NOTHING', r.address, now),
      q(db, 'INSERT INTO panels(pub, address, gb, counted_gb, base_gb, first_seen, last_seen, bound_at, ip_hash, src_since) VALUES(?,?,?,?,0,?,?,?,?,?)',
        r.pub, r.address, r.gb, Math.min(r.gb, days * CFG.capGbPerDay), now, now, now, src, now),
      sample(r.gb),
    ]);
    if (src) await judgeSource(db, src, now); // 这处来源上要是已经有别的地址,当场就判,答复里带着原因
    return json(await stateOf(db, r.pub));
  }
  let moved = false;
  if (r.address !== p.address) {
    // 换地址:有冷却期。这台面板到此为止的贡献留给原地址(记在 settled_gb),之后新增的才算到新地址名下。
    const left = p.bound_at + CFG.rebindCooldown - now;
    if (left > 0) return fail(400, `换地址还在冷却期,${Math.ceil(left / 86400)} 天后再试`, 'cooldown', { days: Math.ceil(left / 86400) });
    await db.batch([
      q(db, 'INSERT INTO addresses(address, first_seen) VALUES(?, ?) ON CONFLICT(address) DO NOTHING', r.address, now),
      q(db, 'UPDATE addresses SET settled_gb = settled_gb + ? WHERE address = ?', p.counted_gb - p.base_gb, p.address),
      q(db, 'UPDATE panels SET address = ?, base_gb = counted_gb, bound_at = ? WHERE pub = ?', r.address, now, r.pub),
    ]);
    moved = true;
  } else if (now - p.last_seen < CFG.reportGap) {
    return json(await stateOf(db, r.pub)); // 报得太勤:只回现状
  }
  // 只增不减:报上来的比记着的小(面板是接任 / 还原回来的)不处理,把记着的数回给它,它自己会往上接。
  // 不计入期间:累计值照样跟着记(恢复之后不会把这段补算进来),只是不往 counted_gb 里加。
  const delta = r.gb - p.gb, srcChanged = src !== p.ip_hash, since = srcChanged ? now : p.src_since;
  if (delta > 0) {
    const allow = Math.max(1, Math.ceil((now - p.last_seen) / 86400 * CFG.capGbPerDay));
    await db.batch([
      q(db, 'UPDATE panels SET gb = ?, counted_gb = counted_gb + ?, last_seen = ?, ip_hash = ?, src_since = ? WHERE pub = ?', r.gb, p.excluded ? 0 : Math.min(delta, allow), now, src, since, r.pub),
      sample(r.gb),
    ]);
  } else if (now - p.last_seen >= CFG.touchGap || srcChanged || dayOf(p.last_seen) !== today) {
    await db.batch([
      q(db, 'UPDATE panels SET last_seen = ?, ip_hash = ?, src_since = ? WHERE pub = ?', now, src, since, r.pub),
      sample(p.gb),
    ]);
  }
  if (src && (srcChanged || moved)) await judgeSource(db, src, now);
  return json(await stateOf(db, r.pub));
}

// ---- 每天入账 ----
// 每个地址每天最多入账 1 积分:挣到的比入账的多、今天还没入过、名下有计入中的面板最近一天上报过,就入一个。
// 多出来的留着,以后的日子接着入。挣到的 = (换绑走的面板留下的 + 名下各台面板的贡献) 每满 1 TB 一个。
export async function creditPoints(db, now) {
  const today = dayOf(now);
  const rows = await all(db, `SELECT a.address, a.credited, a.settled_gb + COALESCE(SUM(p.counted_gb - p.base_gb), 0) AS gb,
      MAX(CASE WHEN p.excluded = 0 AND p.last_seen > ? THEN 1 ELSE 0 END) AS active
    FROM addresses a JOIN panels p ON p.address = a.address WHERE a.credit_day < ? GROUP BY a.address`, now - CFG.creditActive, today);
  const stmts = [];
  for (const r of rows) {
    if (r.active && Math.floor(r.gb / CFG.gbPerPoint) > r.credited) stmts.push(q(db, 'UPDATE addresses SET credited = credited + 1, credit_day = ? WHERE address = ? AND credit_day < ?', today, r.address, today));
  }
  await batched(db, stmts);
  return stmts.length;
}

// ---- 自动识别作弊 ----
// 返回 面板公钥 → 命中的规则代号(一台面板只记最先命中的那条)。纯读,不改库;judge() 拿它去标记 / 恢复。
export async function findCheats(db, now) {
  const hit = new Map();
  const mark = (pub, rule) => { if (!hit.has(pub)) hit.set(pub, rule); };

  // 1. 一处来源只认一个地址:先在这处来源上报的那个。后来的不管绑的是谁的地址,都不计入 ——
  //    所以拿一个老地址来「抢位置」没有用,把别人的地址绑到这里来也害不到那个人(他自己的面板在别处,照常算)。
  const bySrc = await all(db, "SELECT ip_hash AS k, pub, address FROM panels WHERE ip_hash != '' AND last_seen > ? ORDER BY ip_hash, src_since, first_seen, pub", now - CFG.activeWindow);
  for (const g of groupsOf(bySrc)) for (const x of g) if (x.address !== g[0].address) mark(x.pub, 'same_src');

  // 2、3 看每天涨多少(今天还没过完,不看今天)
  const today = dayOf(now), span = Math.max(CFG.robotDays, CFG.twinDays) + 1;
  const rows = await all(db, 'SELECT s.pub, p.first_seen, s.day, s.gb FROM samples s JOIN panels p ON p.pub = s.pub WHERE s.day >= ? AND s.day < ? ORDER BY s.pub, s.day', today - span, today);
  const panels = groupsOf(rows.map(s => ({ ...s, k: s.pub }))).map(g => ({ pub: g[0].pub, born: g[0].first_seen, byDay: new Map(g.map(s => [s.day, s.gb])) }));
  const lastDeltas = (p, n) => { // 到昨天为止连续 n 天每天涨的量;中间缺一天就不算
    const d = [];
    for (let day = today - n; day < today; day++) { if (!p.byDay.has(day) || !p.byDay.has(day - 1)) return null; d.push(p.byDay.get(day) - p.byDay.get(day - 1)); }
    return d;
  };
  const sum = d => d.reduce((s, v) => s + v, 0);
  // 2. 每天涨的量一模一样:真流量每天都不一样
  for (const p of panels) { const d = lastDeltas(p, CFG.robotDays); if (d && tooRegular(d)) mark(p.pub, 'robot'); }
  // 3. 两台面板的曲线一模一样:同一份流量登记了两次(比如把一台的数据复制到另一台,再绑别的地址)。只认先登记的那台。
  const cand = panels.map(p => ({ p, d: lastDeltas(p, CFG.twinDays) })).filter(x => x.d && sum(x.d) / x.d.length >= CFG.robotMinGb)
    .map(x => ({ ...x, sum: sum(x.d) })).sort((a, b) => a.sum - b.sum);
  const parent = cand.map((_, i) => i), find = i => parent[i] === i ? i : (parent[i] = find(parent[i]));
  for (let i = 0; i < cand.length; i++) {
    for (let j = i + 1; j < cand.length && cand[j].sum - cand[i].sum <= cand[i].sum * CFG.twinTolerance + CFG.twinDays; j++) {
      if (sameCurve(cand[i].d, cand[j].d)) parent[find(j)] = find(i);
    }
  }
  const clusters = new Map();
  cand.forEach((c, i) => { const r = find(i); if (!clusters.has(r)) clusters.set(r, []); clusters.get(r).push(c.p); });
  for (const list of clusters.values()) {
    list.sort((a, b) => a.born - b.born || (a.pub < b.pub ? -1 : 1));
    for (const p of list.slice(1)) mark(p.pub, 'twins');
  }
  return hit;
}
function groupsOf(rows) { // 按 k 相邻分组(查询已经按 k 排好)
  const out = []; let cur = null;
  for (const r of rows) { if (!cur || cur[0].k !== r.k) out.push(cur = []); cur.push(r); }
  return out;
}
// 每天涨的量是不是规整得不像真的:够大(很小的面板每天涨的量本来就接近),而且最大最小几乎没差
export function tooRegular(d) {
  const mean = d.reduce((s, x) => s + x, 0) / d.length;
  if (mean < CFG.robotMinGb) return false;
  return Math.max(...d) - Math.min(...d) <= Math.max(1, mean * CFG.robotSpread);
}
export function sameCurve(a, b) {
  for (let i = 0; i < a.length; i++) if (Math.abs(a[i] - b[i]) > Math.max(1, Math.max(a[i], b[i]) * CFG.twinTolerance)) return false;
  return true;
}
// 把命中的面板标成不计入(已经标着的只刷新命中时间),并作废它不该算的那部分:
//   - 靠看曲线判出来的(造假、重复登记):这台面板报过的全部作废;
//   - 一处来源上后到的:刚登记(不满一天)的,带进来的全部作废;早就登记、后来才搬到这处来源的,以前在别处跑的照旧算。
// 作废之后,地址已入账的积分要是比挣到的多,多出来的作废(别人冒用你的地址去作弊,你被作废的只是他送的那部分)。
async function applyHits(db, hits, now, env) {
  const touched = new Set(), stmts = [];
  let excluded = 0;
  for (const [pub, rule] of hits) {
    if (env && !roomFor(env, 4 + touched.size * 2)) break;
    const p = await one(db, 'SELECT address, excluded, first_seen, counted_gb, base_gb FROM panels WHERE pub = ?', pub);
    if (!p) continue;
    if (p.excluded) { stmts.push(q(db, 'UPDATE panels SET hit_at = ? WHERE pub = ?', now, pub)); continue; }
    const base = rule !== 'same_src' || now - p.first_seen < CFG.youngPanel ? p.counted_gb : p.base_gb;
    stmts.push(q(db, 'UPDATE panels SET excluded = 1, rule = ?, reason = ?, excluded_at = ?, hit_at = ?, base_gb = ? WHERE pub = ?', rule, RULES[rule], now, now, base, pub));
    stmts.push(q(db, "INSERT INTO events(ts, address, panel, kind, rule, reason) VALUES(?,?,?,'excluded',?,?)", now, p.address, pub.slice(0, 8), rule, RULES[rule]));
    touched.add(p.address);
    excluded++;
  }
  await batched(db, stmts);
  for (const address of touched) {
    const a = await one(db, `SELECT a.credited, a.settled_gb + COALESCE((SELECT SUM(p.counted_gb - p.base_gb) FROM panels p WHERE p.address = a.address), 0) AS gb
      FROM addresses a WHERE a.address = ?`, address);
    if (!a) continue;
    const earned = Math.max(0, Math.floor(a.gb / CFG.gbPerPoint));
    if (a.credited > earned) await db.batch([
      q(db, 'UPDATE addresses SET credited = ? WHERE address = ?', earned, address),
      q(db, "INSERT INTO events(ts, address, kind, reason) VALUES(?,?,'voided',?)", now, address, `作废 ${a.credited - earned} 积分`),
    ]);
  }
  return excluded;
}
// 上报时当场看一眼这处来源(新登记、换了来源、换了地址的时候),不用等下一轮定时
async function judgeSource(db, src, now) {
  const g = await all(db, 'SELECT pub, address FROM panels WHERE ip_hash = ? AND last_seen > ? ORDER BY src_since, first_seen, pub', src, now - CFG.activeWindow);
  const hits = new Map();
  for (const x of g) if (x.address !== g[0].address) hits.set(x.pub, 'same_src');
  if (hits.size) await applyHits(db, hits, now);
}
// 标记命中的、恢复不再命中的(连续 restoreAfter 没再命中才恢复)。每一次变动记进 events。
export async function judge(db, now, env) {
  const hits = await findCheats(db, now);
  const excluded = await applyHits(db, hits, now, env);
  const stmts = [];
  let restored = 0;
  for (const p of await all(db, 'SELECT pub, address, rule, hit_at FROM panels WHERE excluded = 1')) {
    if (hits.has(p.pub) || now - p.hit_at < CFG.restoreAfter) continue;
    stmts.push(q(db, "UPDATE panels SET excluded = 0, rule = '', reason = '', excluded_at = 0 WHERE pub = ?", p.pub));
    stmts.push(q(db, "INSERT INTO events(ts, address, panel, kind, rule) VALUES(?,?,?,'restored',?)", now, p.address, p.pub.slice(0, 8), p.rule));
    restored++;
  }
  await batched(db, stmts);
  return { hit: hits.size, excluded, restored };
}

// ---- 预算 ----
// Cloudflare 免费套餐一次调用最多 50 个子请求(查库、访问外部都算)。定时任务按预算干活:
// 每一步先看余量,干不完的留给下一轮;最后出账本和存状态的那几次一定留着。
// 各处的进度(看到哪一笔交易、订单同步到哪、上次查余额是什么时候…)合成一份状态存在 kv 里,一轮只读一次、写一次。
const BUDGET = 44, RESERVE = 9; // RESERVE:出账本 6 次 + 留名过期 1 次 + 存状态 1 次,留 1 次余量
function metered(env, limit = BUDGET) {
  const m = { used: 0, limit };
  const wrap = db => ({
    prepare(sql) {
      const mk = st => ({ raw: st, bind: (...a) => mk(st.bind(...a)), all: () => (m.used++, st.all()), first: () => (m.used++, st.first()), run: () => (m.used++, st.run()) });
      return mk(db.prepare(sql));
    },
    batch(list) { m.used++; return db.batch(list.map(s => s.raw || s)); },
  });
  const f = env.FETCH || fetch;
  return { ...env, DB: wrap(env.DB), COMMERCE: wrap(readOnly(env.COMMERCE)), FETCH: (...a) => (m.used++, f(...a)), meter: m, room: n => m.limit - m.used >= n + RESERVE };
}
const roomFor = (env, n) => !env.room || env.room(n);
const loadState = db => kvJSON(db, 'state', {});

// ---- 奖池:订单(只读订单库)----
// 订单库的 payments 表里 status = 'credited' 的,就是对上了订单、订阅站认了的付款(「监听成功」)。两条链都在这张表里;
// Solana 的 tx_id 前面带 'solana:'。这种记录在订单库里不会再变,这边也只加不删。
export async function syncOrders(env, st, now) {
  const db = env.DB;
  // 平时只看最近变动的(按核对时间),每天全量过一遍
  let since = Number(st.ordCur || 0);
  if (now - Number(st.ordFull || 0) > 86400) { since = 0; st.ordFull = now; }
  // 只取这几样:链上交易号、收款地址、金额、时间、付款方地址(从链上事件里单拿这一项)。订单号、订单的哈希、套餐、订阅这些一概不读 ——
  // 订单号是订阅的使用凭证,这里不碰它,账本里自然也不会有。
  const rows = await all(commerce(env), `SELECT tx_id, address, amount_micros, block_timestamp,
    CASE WHEN json_valid(event_json) THEN json_extract(event_json, '$.from') END AS payer, COALESCE(checked_at, created_at) AS at
    FROM payments WHERE status = 'credited' AND COALESCE(checked_at, created_at) >= ?`, Math.max(0, since - 3600_000));
  const stmts = [];
  let top = since;
  for (const p of rows) {
    top = Math.max(top, Number(p.at) || 0);
    const tx = String(p.tx_id || '').replace(/^solana:/, ''), chain = chainOf(p.address), m = Number(p.amount_micros), ts = sec(p.block_timestamp);
    if (!tx || tx.length > 128 || !chain || !Number.isSafeInteger(m) || m <= 0 || !ts) continue;
    const from = typeof p.payer === 'string' && chainOf(p.payer) ? p.payer : ''; // 读不出付款方就空着
    stmts.push(q(db, `INSERT INTO deposits(tx, chain, address, ts, from_addr, micros, kind, name, seen_at) VALUES(?,?,?,?,?,?,'order','',?)
      ON CONFLICT(tx) DO UPDATE SET kind = 'order', name = '' WHERE deposits.kind != 'order'`, tx, chain, p.address, ts, from, m, now));
  }
  const added = await batched(db, stmts); // 已经记过的订单不会再改动,所以这就是新记下的笔数
  st.ordCur = top;
  return added;
}

// ---- 奖池:捐赠(自己看链上)----
// 查账的原则:
//   - 没有留名在等到账,就一次链都不查。有的时候,每一轮(2 分钟)最多问两三次:拉一页新交易号,把时间落在留名期限里的那几笔一次问清金额。
//   - 节点:自己配的(SOLANA_RPC,逗号隔开,排在前面)+ 内置的几个公共节点。公共节点会限速、有的不给某些调用:
//     出错 / 被限速的歇一会儿(1、2、4…分钟,最多半小时),不给某种调用的一天之内不拿那种调用问它,下一个顶上。
//   - 不用「查某个地址名下有哪些账户」这类受限的查询:收款用的那个账户地址是算得出来的(见 ataOf)。
const PUBLIC_RPC = ['https://api.mainnet-beta.solana.com', 'https://public.rpc.solanavibestation.com', 'https://solana-rpc.publicnode.com'];
export function rpcList(env) {
  const own = String(env.SOLANA_RPC || '').split(',').map(x => x.trim()).filter(Boolean);
  return [...new Set([...own, ...PUBLIC_RPC])];
}
const hostOf = url => { try { return new URL(url).host; } catch { return String(url); } };
// 问一圈节点,直到有一个答上来。isBatch = 这是一个批量请求(body 是数组),答复也得是数组。
async function rpcAsk(env, st, now, method, body, isBatch) {
  st.rpc = st.rpc || {};
  const state = url => { const h = hostOf(url); return st.rpc[h] || (st.rpc[h] = { until: 0, fails: 0, no: {} }); };
  const barred = url => { const h = state(url); return (h.no[method] || 0) > now || (isBatch && (h.no.batch || 0) > now); };
  const list = rpcList(env).filter(url => !barred(url));
  let order = list.filter(url => state(url).until <= now);
  if (!order.length) order = [...list].sort((a, b) => state(a).until - state(b).until).slice(0, 1); // 都在歇:挑最快该醒的那个试一次
  let last = null;
  for (const url of order.slice(0, 3)) {
    if (!roomFor(env, 1)) break;
    const h = state(url);
    try {
      const r = await (env.FETCH || fetch)(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (r.status === 401 || r.status === 403) { h.no[method] = now + 86400; throw new Error(`${hostOf(url)} 不给这种查询(${r.status})`); }
      if (!r.ok) throw new Error(`${hostOf(url)} 答复 ${r.status}`);
      const j = await r.json();
      if (isBatch) {
        if (!Array.isArray(j)) { h.no.batch = now + 86400; last = new Error(`${hostOf(url)} 不认批量请求`); continue; } // 不算它出错,换下一个
        h.fails = 0; h.until = 0;
        return j;
      }
      if (j && j.error) {
        const code = j.error.code, msg = String(j.error.message || '');
        if (code === -32601 || /not available|paid plan|personal token|not supported|not allowed|upgrade/i.test(msg)) { h.no[method] = now + 86400; throw new Error(`${hostOf(url)} 不给这种查询`); }
        throw new Error(`${hostOf(url)}: ${msg.slice(0, 80) || code}`);
      }
      h.fails = 0; h.until = 0;
      return j ? j.result : null;
    } catch (e) {
      last = e;
      if (!((h.no[method] || 0) > now)) { h.fails = (h.fails || 0) + 1; h.until = now + Math.min(1800, 60 * 2 ** Math.min(h.fails - 1, 5)); }
    }
  }
  throw last || new Error('没有可用的 Solana 节点');
}
const rpc = (env, st, now, method, params) => rpcAsk(env, st, now, method, { jsonrpc: '2.0', id: 1, method, params }, false);
// 一次请求问好几件同样的事(JSON-RPC 的批量请求):只算一个子请求。返回和 calls 对齐的结果,没答上来的那几条是 undefined。
// 没有节点认批量(或者都在限速)就退回一件一件问,问到预算用完为止。
async function rpcBatch(env, st, now, method, paramsList) {
  const out = new Array(paramsList.length).fill(undefined);
  if (paramsList.length > 1) {
    try {
      const j = await rpcAsk(env, st, now, method, paramsList.map((params, i) => ({ jsonrpc: '2.0', id: i, method, params })), true);
      for (const x of j) if (x && Number.isInteger(x.id) && x.id >= 0 && x.id < out.length && !x.error) out[x.id] = x.result;
      return out;
    } catch { /* 退回一件一件问 */ }
  }
  let failed = 0;
  for (let i = 0; i < paramsList.length && roomFor(env, 2) && failed < 2; i++) {
    try { out[i] = await rpc(env, st, now, method, paramsList[i]); } catch { failed++; }
  }
  return out;
}

// 订阅站现在启用着的收款地址(两条链);另外,账上有过入账的老地址也列出来。
export async function poolAddresses(env) {
  const out = [], seen = new Set();
  const add = (address, chain) => { if (address && !seen.has(address) && chainOf(address) === chain) { seen.add(address); out.push({ chain, address }); } };
  for (const r of await all(commerce(env), 'SELECT address FROM solana_payment_addresses WHERE enabled = 1 ORDER BY position, address')) add(r.address, 'solana');
  for (const r of await all(commerce(env), 'SELECT address FROM payment_addresses WHERE enabled = 1 ORDER BY position, address')) add(r.address, 'tron');
  for (const r of await all(env.DB, 'SELECT DISTINCT address, chain FROM deposits ORDER BY chain, address')) add(r.address, r.chain);
  return out;
}
// 定时任务里用的:十分钟查一次,其余时候用记着的
async function poolList(env, st, now) {
  if (!st.pool || now - st.pool.at >= 600) st.pool = { at: now, list: await poolAddresses(env) };
  return st.pool.list;
}

// 一个钱包地址收 USDT 用的那个账户(关联代币账户)的地址。转账是转进这个账户,交易也挂在它名下。
// 它由 钱包地址 + 代币程序 + 币种 三样算出来:依次试 255、254… 这个尾数,取第一个「不在 ed25519 曲线上」的哈希。不用问链。
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA', ATA_PROGRAM = 'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL';
function b58decode(s) {
  let n = 0n;
  for (const ch of s) { const d = B58.indexOf(ch); if (d < 0) throw new Error('不是 base58'); n = n * 58n + BigInt(d); }
  const out = new Uint8Array(32);
  for (let i = 31; i >= 0; i--) { out[i] = Number(n & 255n); n >>= 8n; }
  if (n > 0n) throw new Error('超过 32 字节');
  return out;
}
function b58encode(bytes) {
  let n = 0n;
  for (const b of bytes) n = n * 256n + BigInt(b);
  let s = '';
  while (n > 0n) { s = B58[Number(n % 58n)] + s; n /= 58n; }
  for (const b of bytes) { if (b !== 0) break; s = '1' + s; }
  return s;
}
const P25519 = (1n << 255n) - 19n;
const modpow = (b, e, m) => { let r = 1n; b %= m; while (e > 0n) { if (e & 1n) r = r * b % m; b = b * b % m; e >>= 1n; } return r; };
const D25519 = (P25519 - 121665n) * modpow(121666n, P25519 - 2n, P25519) % P25519;
// 这 32 字节是不是曲线上一个点的压缩写法:由 y 解 x² = (y² − 1) / (d·y² + 1),有解就是
export function onCurve(bytes) {
  let y = 0n;
  for (let i = 31; i >= 0; i--) y = (y << 8n) | BigInt(i === 31 ? bytes[i] & 127 : bytes[i]);
  y %= P25519;
  const y2 = y * y % P25519, u = (y2 - 1n + P25519) % P25519, v = (D25519 * y2 + 1n) % P25519;
  const x2 = u * modpow(v, P25519 - 2n, P25519) % P25519;
  return x2 === 0n || modpow(x2, (P25519 - 1n) / 2n, P25519) === 1n;
}
export async function ataOf(owner, mint = USDT_MINT) {
  const seeds = [b58decode(owner), b58decode(TOKEN_PROGRAM), b58decode(mint)], prog = b58decode(ATA_PROGRAM), tail = new TextEncoder().encode('ProgramDerivedAddress');
  for (let bump = 255; bump >= 0; bump--) {
    const data = new Uint8Array(32 * 3 + 1 + 32 + tail.length);
    let o = 0;
    for (const x of seeds) { data.set(x, o); o += 32; }
    data[o++] = bump; data.set(prog, o); o += 32; data.set(tail, o);
    const h = new Uint8Array(await crypto.subtle.digest('SHA-256', data));
    if (!onCurve(h)) return b58encode(h);
  }
  throw new Error('算不出收款账户');
}

// 一笔交易里,owner 名下的 USDT 净增了多少、是谁转出的。
export function usdtMoves(tx, owner) {
  const pre = new Map(), post = new Map(), who = new Map();
  const meta = (tx && tx.meta) || {};
  for (const b of meta.preTokenBalances || []) if (b.mint === USDT_MINT) { pre.set(b.accountIndex, BigInt(b.uiTokenAmount.amount)); who.set(b.accountIndex, b.owner || ''); }
  for (const b of meta.postTokenBalances || []) if (b.mint === USDT_MINT) { post.set(b.accountIndex, BigInt(b.uiTokenAmount.amount)); who.set(b.accountIndex, b.owner || ''); }
  let got = 0n, most = 0n, payer = '';
  for (const [i, o] of who) {
    const d = (post.get(i) || 0n) - (pre.get(i) || 0n);
    if (o === owner) got += d;
    else if (-d > most) { most = -d; payer = o; }
  }
  return { got, payer: validSolana(payer) ? payer : '' };
}
// 把一个账户上 from 之后的新交易记进 sigs:从新到旧一页一页拉,拉到上次拉到的地方、或者比 from 还早的交易为止。
// 一轮里每一段只拉一页:没拉完的那一段记成缺口,下一轮接着。返回「拉全了没有」。
const SIG_PAGE = 50;
async function pullSignatures(env, st, owner, account, now, from) {
  const db = env.DB;
  st.cur = st.cur || {};
  const cur = st.cur[account] || { head: '', gaps: [] };
  const stmts = [];
  const page = async (until, before, since) => { // 返回 { head, rest }:rest 不为空 = 这一段还没拉完,下回从它往前拉
    const rows = await rpc(env, st, now, 'getSignaturesForAddress', [account, { limit: SIG_PAGE, commitment: 'finalized', ...(until ? { until } : {}), ...(before ? { before } : {}) }]);
    if (!Array.isArray(rows) || !rows.length) return { head: '', rest: '' };
    let old = false;
    for (const r of rows) {
      if (typeof r.signature !== 'string' || r.signature.length > 128) continue;
      if (r.blockTime != null && r.blockTime < since) { old = true; continue; }
      if (r.err === null || r.err === undefined) stmts.push(q(db, "INSERT INTO sigs(sig, owner, ts, state, seen_at) VALUES(?,?,?,'new',?) ON CONFLICT(sig) DO NOTHING", r.signature, owner, r.blockTime || 0, now));
    }
    return { head: rows[0].signature, rest: old || rows.length < SIG_PAGE ? '' : rows[rows.length - 1].signature };
  };
  const gaps = [...(cur.gaps || [])];
  let reached = false;
  try {
    if (roomFor(env, 2)) {
      const top = await page(cur.head, '', from);
      if (top.rest) gaps.push({ until: cur.head, before: top.rest, from });
      if (top.head) cur.head = top.head;
      reached = true;
    }
    if (gaps.length && roomFor(env, 2)) { // 最老的那个缺口往前补一页
      const g = gaps.shift(), r = await page(g.until, g.before, g.from || from);
      if (r.rest) gaps.unshift({ ...g, before: r.rest });
    }
  } finally { // 中途出错(节点不通):已经拉到的照样记下,缺口留着下回补
    await batched(db, stmts);
    st.cur[account] = { head: cur.head, gaps: gaps.slice(0, 20) };
  }
  return reached && gaps.length === 0;
}
// 看链上的新转入里有没有留名捐赠:金额正好是某一笔登记的确切金额、到账时间落在它的期限里,才算。
// 对不上任何一笔登记的转入一律不管(不计入、不列出):这个地址上还有订单付款和别的资金流动。
const TX_BATCH = 20, TX_PER_TICK = 60;
export async function scanSolana(env, st, now) {
  const db = env.DB;
  // 还没兑现的留名(在等到账的、刚过期的)。一笔留名「查完了」= 它的期限(连余量)过后,链上的交易又拉全过一次(scanOk)。
  // 库里还有没看的交易时,把近两天的都带上:它们里面可能有哪一笔的钱。
  const backlog = await one(db, "SELECT COUNT(*) AS n FROM sigs WHERE state = 'new'");
  const horizon = backlog && backlog.n > 0 ? now - 2 * 86400 : Number(st.scanOk || 0) - CFG.intentGrace;
  const live = await all(db, "SELECT id, name, address, exact, created_at, expires_at FROM intents WHERE status IN ('pending', 'expired', 'cancelled') AND expires_at >= ? ORDER BY created_at", horizon);
  if (!live.length) { // 没有留名在等到账:没拉完的交易号也不用再拉了
    for (const c of Object.values(st.cur || {})) if (c) c.gaps = [];
    if (!(backlog && backlog.n > 0)) { st.scanOk = now; return 0; } // 一次链都不查
  } else {
    const from = Math.min(...live.map(i => i.created_at)) - CFG.intentGrace;
    const owners = [...new Set(live.map(i => i.address))].slice(0, 3);
    let complete = true;
    for (const owner of owners) {
      if (!roomFor(env, 3)) { complete = false; break; }
      st.ata = st.ata || {};
      const account = st.ata[owner] || (st.ata[owner] = await ataOf(owner));
      complete = await pullSignatures(env, st, owner, account, now, from) && complete;
    }
    if (complete) st.scanOk = now;
  }
  if (!roomFor(env, 5)) return 0;
  // 时间落不进任何一笔留名期限的交易,肯定对不上:一条语句全划掉,不用去链上看
  await run(db, `UPDATE sigs SET state = 'skip' WHERE state = 'new' AND ts > 0 AND NOT EXISTS (
    SELECT 1 FROM intents i WHERE i.address = sigs.owner AND i.status IN ('pending', 'expired', 'cancelled') AND i.created_at - ? <= sigs.ts AND i.expires_at + ? >= sigs.ts)`, CFG.intentGrace, CFG.intentGrace);
  // 剩下的要去链上看金额:一次请求问一批,预算用完就停,下一轮接着
  const todo = await all(db, "SELECT sig, owner, ts FROM sigs WHERE state = 'new' ORDER BY ts, sig LIMIT ?", TX_PER_TICK);
  const stmts = [];
  let found = 0;
  for (let k = 0; k < todo.length && roomFor(env, 3); k += TX_BATCH) {
    const part = todo.slice(k, k + TX_BATCH);
    const txs = await rpcBatch(env, st, now, 'getTransaction', part.map(x => [x.sig, { encoding: 'jsonParsed', commitment: 'finalized', maxSupportedTransactionVersion: 0 }]));
    for (let n = 0; n < part.length; n++) {
      const x = part[n], tx = txs[n];
      const mark = state => stmts.push(q(db, 'UPDATE sigs SET state = ? WHERE sig = ?', state, x.sig));
      if (tx === undefined) continue; // 这一条没问到(节点不通、预算不够):原样留着,下一轮再问
      if (!tx || !tx.meta) { stmts.push(q(db, "UPDATE sigs SET tries = tries + 1, state = CASE WHEN tries >= 30 THEN 'gone' ELSE 'new' END WHERE sig = ?", x.sig)); continue; }
      if (tx.meta.err) { mark('failed'); continue; }
      const { got, payer } = usdtMoves(tx, x.owner);
      const ts = Number(tx.blockTime) || x.ts || now;
      if (got <= 0n || got > BigInt(Number.MAX_SAFE_INTEGER)) { mark('skip'); continue; } // 转出的
      const m = Number(got);
      const it = live.find(i => !i.used && i.address === x.owner && i.exact === m && i.created_at - CFG.intentGrace <= ts && ts <= i.expires_at + CFG.intentGrace);
      if (!it) { mark('skip'); continue; } // 对不上任何一笔登记:不是留名捐赠
      // 万一订阅站把同一笔认成了订单付款(留名的金额和订单金额不会相同,不该发生):按订单算,不重复记
      if (await one(commerce(env), "SELECT 1 AS x FROM payments WHERE tx_id = ? AND status = 'credited'", 'solana:' + x.sig)) { mark('order'); continue; }
      it.used = true;
      stmts.push(q(db, "INSERT INTO deposits(tx, chain, address, ts, from_addr, micros, kind, name, seen_at) VALUES(?, 'solana', ?, ?, ?, ?, 'donation', ?, ?) ON CONFLICT(tx) DO NOTHING",
        x.sig, x.owner, ts, payer, m, shownName(env, it.name), now));
      mark('donation');
      stmts.push(q(db, "UPDATE intents SET status = 'paid', tx = ? WHERE id = ?", x.sig, it.id));
      found++;
    }
  }
  await batched(db, stmts);
  return found;
}
// Solana 的订单付款,订单库里没记付款方:去链上看这笔交易补上(十分钟补一两笔;查不到的记成 '-',不再问)
export async function fillPayers(env, st, now, limit = 2) {
  const rows = await all(env.DB, "SELECT tx, address FROM deposits WHERE chain = 'solana' AND from_addr = '' ORDER BY ts DESC LIMIT ?", limit);
  for (const d of rows) {
    if (!roomFor(env, 2)) break;
    try {
      const tx = await rpc(env, st, now, 'getTransaction', [d.tx, { encoding: 'jsonParsed', commitment: 'finalized', maxSupportedTransactionVersion: 0 }]);
      if (!tx) continue;
      await run(env.DB, 'UPDATE deposits SET from_addr = ? WHERE tx = ?', usdtMoves(tx, d.address).payer || '-', d.tx);
    } catch { break; /* 节点不通:这一轮不再问,下一轮再来 */ }
  }
}

// ---- 留名捐赠 ----
// 只有它计入奖池(没登记就转进来的不算)。要转的确切金额 = 填的数 + 一点零头(0.001–0.099):靠它认出是哪一笔,同一时间填了一样数的两个人也分得开。
// 它最多 3 位小数,而订单金额的第 6 位小数一定不是 0,所以不会和任何订单的金额相同(另外还对一遍订单占用的金额,以防万一)。
export async function handleDonate(env, body, ip, now) {
  const db = env.DB;
  const c = cleanName(body && body.name);
  if (c.err) return fail(400, c.err, c.code);
  const m = micros(body && body.amount);
  if (!(m >= CFG.donateMin) || m > CFG.donateMax || m % 10000 !== 0) return fail(400, '金额要在 1 到 100000 之间,最多两位小数', 'amount');
  const ipHash = await hashOf(env.SALT, srcKey(ip));
  if (ipHash) {
    const mine = await one(db, 'SELECT COUNT(*) AS n FROM intents WHERE ip_hash = ? AND created_at > ?', ipHash, now - 3600);
    if (mine && mine.n >= CFG.intentsPerIpHour) return fail(429, '提交得太勤了,过一会儿再试', 'busy');
  }
  const st = await loadState(db);
  const sol = ((st.pool && st.pool.list) || await poolAddresses(env)).find(a => a.chain === 'solana');
  if (!sol) return fail(503, '现在没有可用的奖池地址,请稍后再试', 'no_pool');
  // 还占着的金额:期限(含余量)还没过完的留名
  const taken = new Set((await all(db, "SELECT exact FROM intents WHERE address = ? AND status IN ('pending', 'expired', 'cancelled') AND expires_at + ? > ? AND exact > ? AND exact < ?",
    sol.address, 2 * CFG.intentGrace, now, m, m + 100000)).map(x => x.exact));
  for (const r of await all(commerce(env), 'SELECT amount_micros AS m FROM solana_amount_reservations WHERE address = ? AND amount_micros > ? AND amount_micros < ?', sol.address, m, m + 100000)) taken.add(Number(r.m));
  let exact = 0;
  const seed = crypto.getRandomValues(new Uint32Array(1))[0] % 99;
  for (let i = 0; i < 99 && !exact; i++) { const x = m + (1 + (seed + i) % 99) * 1000; if (!taken.has(x)) exact = x; }
  if (!exact) return fail(503, '这个金额现在排不开,换个数再试', 'no_slot');
  const it = { id: hex(crypto.getRandomValues(new Uint8Array(12))), name: c.name, micros: m, exact, address: sol.address, created_at: now, expires_at: now + CFG.intentWindow, status: 'pending' };
  await run(db, "INSERT INTO intents(id, name, micros, exact, address, created_at, expires_at, status, ip_hash) VALUES(?,?,?,?,?,?,?,'pending',?)", it.id, it.name, it.micros, it.exact, it.address, it.created_at, it.expires_at, ipHash);
  return json(intentView(it, now));
}
const intentView = (i, now) => ({
  id: i.id, name: i.name, amount: i.micros / 1e6, exact: i.exact / 1e6, chain: 'solana', address: i.address, createdAt: i.created_at, expiresAt: i.expires_at,
  status: i.status === 'paid' ? 'paid' : i.status === 'pending' && now <= i.expires_at ? 'pending' : 'expired',
});

// ---- 公开账本 ----
// st 是定时任务手上的那份状态(收款地址、余额都在里面);单独调用时自己读一份。
export async function buildExport(env, now, st) {
  const db = env.DB;
  if (!st) st = await loadState(db);
  const sums = { order: { n: 0, m: 0 }, donation: { n: 0, m: 0 } };
  for (const r of await all(db, 'SELECT kind, COUNT(*) AS n, COALESCE(SUM(micros), 0) AS m FROM deposits GROUP BY kind')) if (sums[r.kind]) sums[r.kind] = { n: r.n, m: r.m };
  const got = new Map((await all(db, 'SELECT address, SUM(micros) AS m FROM deposits GROUP BY address')).map(r => [r.address, r.m]));
  const share = Math.round(sums.order.m * CFG.orderRate);
  // 积分榜:有积分的,和名下的面板现在全都被剔除着的(写明原因)。还没挣到积分的地址不列(面板自己知道自己的情况)。
  // 「全都被剔除」才标出来:名下混进一台别人冒绑的作弊面板,不会让这个地址看起来像被判了(他自己的面板还在算)。
  // 被标出来的地址,剔除期间不参加分配:积分留着(恢复后照旧),但不算进合计。
  const board = (await all(db, `SELECT a.address, a.credited AS points,
      (SELECT COUNT(*) FROM panels p WHERE p.address = a.address) AS panels,
      (SELECT COUNT(*) FROM panels p WHERE p.address = a.address AND p.excluded = 1) AS bad,
      (SELECT p.rule FROM panels p WHERE p.address = a.address AND p.excluded = 1 ORDER BY p.excluded_at DESC, p.pub LIMIT 1) AS rule
    FROM addresses a ORDER BY a.credited DESC, a.first_seen, a.address LIMIT 50000`))
    .map(a => ({ address: a.address, points: a.points, panels: a.panels, out: a.panels > 0 && a.bad === a.panels, rule: a.rule }))
    .filter(a => a.points > 0 || a.out)
    .map(a => ({ address: a.address, points: a.points, panels: a.panels, ...(a.out ? { excluded: true, rule: a.rule, reason: RULES[a.rule] || '' } : {}) }));
  const live = board.filter(b => !b.excluded && b.points > 0);
  const addrs = (st.pool && st.pool.list) || await poolAddresses(env);
  const sol = addrs.find(a => a.chain === 'solana');
  const deps = await all(db, 'SELECT tx, chain, address, ts, from_addr, micros, kind, name FROM deposits ORDER BY ts DESC, tx LIMIT 5000');
  const events = await all(db, 'SELECT ts, address, panel, kind, rule, reason FROM events ORDER BY id DESC LIMIT 500');
  const ledger = {
    v: 1, updatedAt: now, payoutAt: CFG.payoutAt, orderRate: CFG.orderRate,
    pool: {
      total: usd(share + sums.donation.m), orderCount: sums.order.n, orderAmount: usd(sums.order.m), orderShare: usd(share),
      donateCount: sums.donation.n, donateAmount: usd(sums.donation.m), points: live.reduce((s, b) => s + b.points, 0), addresses: live.length,
    },
    chains: addrs.map(a => ({ chain: a.chain, address: a.address, received: usd(got.get(a.address) || 0) })), // received = 计入奖池的那些入账的合计(订单按全额记)
    donate: { chain: 'solana', address: sol ? sol.address : '', min: CFG.donateMin / 1e6, max: CFG.donateMax / 1e6 },
    deposits: deps.map(d => ({ ts: d.ts, kind: d.kind, chain: d.chain, address: d.address, amount: d.micros / 1e6, from: d.from_addr === '-' ? '' : d.from_addr, tx: d.tx, ...(d.name ? { name: d.name } : {}) })),
    board, events,
  };
  await kvSet(db, 'export', JSON.stringify(ledger));
  return ledger;
}

// ---- 定时 ----
// 一步出错不拖累别的步骤(链上节点一时不通,积分照样入账、账本照样出);错在哪一步记下来,/v1/health 看得到。
// 顺序有讲究:先判再入账(刚冒出来的作弊面板拿不到头一分);看链上转入排在后面,它最费预算,有多少余量干多少。
export async function tick(rawEnv, now) {
  const env = metered(rawEnv), db = env.DB, errs = {};
  const st = await loadState(db);
  const step = async (name, need, fn) => {
    if (!env.room(need)) { errs[name] = '这一轮预算不够,留给下一轮'; return; }
    try { await fn(); } catch (e) { errs[name] = String((e && e.message) || e).slice(0, 200); }
  };
  const due = (key, every) => { if (now - Number(st[key] || 0) < every) return false; st[key] = now; return true; };
  await step('orders', 2, () => syncOrders(env, st, now));
  await step('judge', 6, () => judge(db, now, env));
  await step('credit', 2, () => creditPoints(db, now));
  if (due('payAt', 600)) await step('payers', 3, () => fillPayers(env, st, now));
  if (due('gcAt', 86400)) await step('gc', 3, async () => {
    await run(db, 'DELETE FROM samples WHERE day < ?', dayOf(now) - CFG.sampleKeep);
    await run(db, "DELETE FROM intents WHERE status != 'paid' AND expires_at < ?", now - 7 * 86400);
    await run(db, "DELETE FROM sigs WHERE state != 'new' AND seen_at < ?", now - 30 * 86400);
  });
  await step('chain', 3, () => scanSolana(env, st, now));
  // 下面这几步用的是留出来的那部分预算
  let ledger = null;
  try { await run(db, "UPDATE intents SET status = 'expired' WHERE status = 'pending' AND expires_at < ?", now); } catch (e) { errs.intents = String((e && e.message) || e).slice(0, 200); }
  try { ledger = await buildExport(env, now, st); } catch (e) { errs.export = String((e && e.message) || e).slice(0, 200); }
  st.tick = { at: now, errs, used: env.meter.used + 1 };
  await kvSet(db, 'state', JSON.stringify(st));
  return ledger;
}

export default {
  async fetch(req, env) {
    const url = new URL(req.url), now = Math.floor(Date.now() / 1000);
    const ip = req.headers.get('CF-Connecting-IP') || '';
    const path = url.pathname.replace(/\/+$/, '');
    try {
      if (req.method === 'POST' && path === '/v1/report') return await handleReport(env, await readJSON(req), ip, now);
      if (req.method === 'GET' && path === '/v1/export') {
        const cached = await kvGet(env.DB, 'export');
        return new Response(cached || JSON.stringify(await buildExport(env, now)), { headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'public, max-age=60', 'Access-Control-Allow-Origin': '*' } });
      }
      if (req.method === 'POST' && path === '/v1/donate') return await handleDonate(env, await readJSON(req), ip, now);
      if (req.method === 'GET' && path === '/v1/health') return json((await loadState(env.DB)).tick || { at: 0, errs: {} });
      const m = /^\/v1\/donate\/([0-9a-f]{24})(\/cancel)?$/.exec(path);
      if (m) {
        const it = await one(env.DB, 'SELECT * FROM intents WHERE id = ?', m[1]);
        if (!it) return fail(404, '没有这一笔', 'not_found');
        if (m[2]) {
          if (req.method !== 'POST') return fail(405, '方法不对');
          await run(env.DB, "UPDATE intents SET status = 'cancelled' WHERE id = ? AND status = 'pending'", m[1]);
          return json({ ok: true });
        }
        if (req.method !== 'GET') return fail(405, '方法不对');
        return json(intentView(it, now));
      }
      return fail(404, '没有这个接口');
    } catch (e) {
      if (e && e.badRequest) return fail(400, '请求的格式不对', 'format');
      return fail(500, '登记服务出错了,请稍后再试', 'internal');
    }
  },
  async scheduled(event, env, ctx) {
    ctx.waitUntil(tick(env, Math.floor(Date.now() / 1000)));
  },
};
async function readJSON(req) {
  const bad = () => Object.assign(new Error('bad request'), { badRequest: true });
  const text = await req.text();
  if (text.length > 4096) throw bad();
  let v;
  try { v = JSON.parse(text || '{}'); } catch { throw bad(); }
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw bad();
  return v;
}

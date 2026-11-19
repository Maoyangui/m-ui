// 测试和本地试跑(dev-server.mjs)共用的替身:拿 Node 自带的 sqlite 充当 D1、订单库里用得到的那几张表、一条模拟的链。
// 线上不用这个文件。
import { DatabaseSync } from 'node:sqlite';
import { USDT_MINT, ataOf } from './worker.mjs';

// ---- D1:只实现 worker.mjs 用到的那几个调用 ----
// onCall:每发生一次调用(一条查询,或一整个 batch)叫一下,测试拿它数子请求。
export function d1(db, onCall = () => {}) {
  const stmt = (sql, args = []) => ({
    bind: (...a) => stmt(sql, a),
    all: async () => { onCall(); return { results: db.prepare(sql).all(...args).map(r => ({ ...r })) }; },
    first: async () => { onCall(); const r = db.prepare(sql).get(...args); return r ? { ...r } : null; },
    run: async () => { onCall(); const r = db.prepare(sql).run(...args); return { meta: { changes: Number(r.changes) } }; },
    exec: () => { const r = db.prepare(sql).run(...args); return { meta: { changes: Number(r.changes) } }; },
  });
  return {
    prepare: sql => stmt(sql),
    async batch(list) { // D1 的 batch 是一个事务,算一次调用
      onCall();
      db.exec('BEGIN');
      try { const out = []; for (const s of list) out.push(s.exec()); db.exec('COMMIT'); return out; } catch (e) { db.exec('ROLLBACK'); throw e; }
    },
  };
}
export const openDB = (file = ':memory:', options = {}) => new DatabaseSync(file, options);

// 订单库里登记服务会读到的那几张表(只有读到的列;真库的表更宽,这里够用)。
export const COMMERCE_SCHEMA = `
CREATE TABLE IF NOT EXISTS payments (
 tx_id TEXT PRIMARY KEY, address TEXT NOT NULL, amount_micros TEXT NOT NULL, block_timestamp INTEGER NOT NULL,
 event_json TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'observed', order_hash TEXT,
 confirmations INTEGER NOT NULL DEFAULT 0, reason TEXT, checked_at INTEGER, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS orders (order_hash TEXT PRIMARY KEY, amount_micros INTEGER NOT NULL, address TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, fulfilled_at INTEGER);
CREATE TABLE IF NOT EXISTS order_payment_options (order_hash TEXT NOT NULL, network TEXT NOT NULL, address TEXT NOT NULL, amount_micros INTEGER NOT NULL, PRIMARY KEY(order_hash, network));
CREATE TABLE IF NOT EXISTS payment_addresses (address TEXT PRIMARY KEY, enabled INTEGER NOT NULL, position INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS solana_payment_addresses (address TEXT PRIMARY KEY, enabled INTEGER NOT NULL, position INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS solana_amount_reservations (address TEXT NOT NULL, amount_micros INTEGER NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(address, amount_micros));
`;

// ---- base58 ----
const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
export function b58(bytes) {
  let n = 0n; for (const b of bytes) n = n * 256n + BigInt(b);
  let s = ''; while (n > 0n) { s = B58[Number(n % 58n)] + s; n /= 58n; }
  for (const b of bytes) { if (b !== 0) break; s = '1' + s; }
  return s;
}

// ---- 一条模拟的链:Solana 节点的三个调用 + TronGrid 查余额 ----
// gen.sol() / gen.sig() 给出新的地址、交易号(测试里是固定序列,试跑时是随机的)。
export const USDT_TRC20 = 'TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t';
export function newChain(gen) {
  const accounts = new Map(); // 地址 → [{ pubkey, amount }]
  const sigs = new Map();     // 装币的账户 → [{ signature, blockTime, err }](新的在前)
  const txs = new Map();
  const c = {
    calls: [], asked: [], fetches: 0, down: false, noBatch: false, tron: new Map(), // noBatch:装成不认批量请求的节点
    // short:哪些节点只留近期的记录(主机名 → 留多少秒;要先把 now 设成当前时间)。这种节点上:比这更早的交易它不列出来、
    // 也查不到;拿一个它已经不记得的交易号当 until / before 去问,它整个报「查不到这笔交易」(2026-11 两个公共节点就是这样)。
    short: new Map(), now: 0, untilAsked: 0,
    // rule(host, method, isBatch):测试给某个节点定规矩 —— 返回一个 Response 就拿它当答复(限速、不给这种查询…),不返回就照常答
    rule: null,
    open(owner) { const pubkey = gen.sol(); accounts.set(owner, [...(accounts.get(owner) || []), { pubkey, amount: 0 }]); sigs.set(pubkey, []); return pubkey; },
    // 往 owner 转进 m(负数 = 转出)。opts: { failed, payer, account }
    pay(owner, m, at, opts = {}) {
      const acc = accounts.get(owner)[opts.account || 0], signature = gen.sig(), payer = opts.payer || gen.sol();
      const pre = acc.amount, other = 50_000_000_000;
      if (!opts.failed) acc.amount += m;
      const bal = (i, o, amount) => ({ accountIndex: i, mint: USDT_MINT, owner: o, uiTokenAmount: { amount: String(amount), decimals: 6 } });
      txs.set(signature, { blockTime: at, slot: txs.size + 1, meta: {
        err: opts.failed ? { InstructionError: [0, 'Custom'] } : null,
        preTokenBalances: [bal(1, payer, other), bal(2, owner, pre)],
        postTokenBalances: [bal(1, payer, other - (opts.failed ? 0 : m)), bal(2, owner, acc.amount)],
      } });
      sigs.get(acc.pubkey).unshift({ signature, blockTime: at, err: opts.failed ? { InstructionError: [0, 'Custom'] } : null, confirmationStatus: 'finalized' });
      return { signature, payer };
    },
    fetch: async (url, init) => {
      c.fetches++;
      if (String(url).startsWith('http://tron.test')) {
        const a = String(url).split('/').pop();
        return Response.json({ data: [{ trc20: [{ TFakeOtherToken: '5' }, { [USDT_TRC20]: String(c.tron.get(a) || 0) }] }] });
      }
      if (c.down) return new Response('down', { status: 503 });
      const body = JSON.parse(init.body), host = new URL(url).host, isBatch = Array.isArray(body);
      c.asked.push(host + ' ' + (isBatch ? 'batch:' + body[0].method : body.method));
      const special = c.rule && c.rule(host, isBatch ? body[0].method : body.method, isBatch);
      if (special) return special;
      if (isBatch) { // 批量请求
        if (c.noBatch) return Response.json({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'batch requests are not supported' } });
        const out = [];
        for (const x of body) out.push({ jsonrpc: '2.0', id: x.id, ...(await answer(x.method, x.params, host)) });
        return Response.json(out);
      }
      return Response.json({ jsonrpc: '2.0', id: 1, ...(await answer(body.method, body.params, host)) });
    },
  };
  const ataCache = new Map(); // 收款账户 → 钱包地址
  async function ownerOf(account) {
    for (const owner of accounts.keys()) { if (!ataCache.has(owner)) ataCache.set(owner, await ataOf(owner)); if (ataCache.get(owner) === account) return owner; }
    return '';
  }
  async function answer(method, params, host) {
      c.calls.push(method);
      let result = null;
      const floor = c.short.has(host) ? c.now - c.short.get(host) : -Infinity; // 这个节点最早还记得什么时候的交易
      const forgot = sig => { const t = txs.get(sig); return !t || t.blockTime < floor; };
      const notFound = sig => ({ error: { code: -32020, message: `Transaction ${sig} not found` } });
      if (method === 'getTokenAccountsByOwner') {
        result = { value: (accounts.get(params[0]) || []).map(a => ({ pubkey: a.pubkey, account: { data: { parsed: { info: { owner: params[0], mint: USDT_MINT, tokenAmount: { amount: String(a.amount), decimals: 6 } } } } } })) };
      } else if (method === 'getSignaturesForAddress') {
        const owner = await ownerOf(params[0]); // 交易挂在钱包的收款账户(算出来的那个地址)名下
        let list = (owner && sigs.get(accounts.get(owner)[0].pubkey)) || [];
        const { limit = 1000, until, before } = params[1] || {};
        if (until) c.untilAsked++;
        if (c.short.has(host)) {
          if (until && forgot(until)) return notFound(until);
          if (before && forgot(before)) return notFound(before);
          list = list.filter(s => s.blockTime >= floor);
        }
        if (before) { const i = list.findIndex(s => s.signature === before); list = i >= 0 ? list.slice(i + 1) : []; }
        if (until) { const i = list.findIndex(s => s.signature === until); if (i >= 0) list = list.slice(0, i); }
        result = list.slice(0, limit);
      } else if (method === 'getTransaction') result = c.short.has(host) && forgot(params[0]) ? null : txs.get(params[0]) || null;
      return { result };
  }
  return c;
}

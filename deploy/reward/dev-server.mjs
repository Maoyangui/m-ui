// 本地试跑登记服务,给开发和联调用。面板那边:M_UI_REWARD_API=http://127.0.0.1:8787/v1(再加 M_UI_REWARD_LEDGER=…/v1/export)。
//
//   node deploy/reward/dev-server.mjs [端口]
//
// 什么真实的东西都不连:订单库是一份假的,链是模拟的,奖池地址是启动时随手生成的 —— 没有人有它的私钥,千万别往里转钱。
// 定时任务每 15 秒跑一次(线上是 2 分钟)。RW_DB=文件 可以把登记服务自己的库留在磁盘上(默认在内存里,关了就没)。
//
// 另外有几个只在这里才有的开发接口(线上的 worker.mjs 里没有):
//   POST /dev/pay    {"amount": 10.037}                往奖池地址转一笔(模拟链上到账)
//   POST /dev/order  {"amount": 9.99, "chain": "tron"} 订阅站认了一笔订单(chain 省略 = solana)
//   POST /dev/tick                                     马上跑一轮定时任务
//   GET  /dev/state                                    库里现在的面板、地址、留名、最近的记录
// 请求头 X-Dev-IP 可以冒充来源地址(试「一处来源只认一个地址」时用)。
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import worker, { tick } from './worker.mjs';
import { d1, openDB, COMMERCE_SCHEMA, newChain, b58 } from './devkit.mjs';

const port = Number(process.argv[2] || process.env.PORT || 8787);
const here = path.dirname(fileURLToPath(import.meta.url));
const rnd = n => crypto.getRandomValues(new Uint8Array(n));
const gen = { sol: () => b58(rnd(32)), sig: () => b58(rnd(64)) };
const now = () => Math.floor(Date.now() / 1000);

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rw-dev-'));
const commerceFile = path.join(dir, 'commerce.db');
const commerce = openDB(commerceFile); commerce.exec(COMMERCE_SCHEMA);
const db = openDB(process.env.RW_DB || ':memory:'); db.exec(fs.readFileSync(path.join(here, 'schema.sql'), 'utf8'));
const chain = newChain(gen);
const solAddr = gen.sol(), tronAddr = b58(Uint8Array.from([0x41, ...rnd(24)]));
commerce.prepare('INSERT INTO solana_payment_addresses VALUES(?,1,0,0,0)').run(solAddr);
commerce.prepare('INSERT INTO payment_addresses VALUES(?,1,0,0,0)').run(tronAddr);
chain.open(solAddr);
const env = { DB: d1(db), COMMERCE: d1(openDB(commerceFile, { readOnly: true })), SALT: 'dev', SOLANA_RPC: 'http://rpc.test', TRON_API: 'http://tron.test', FETCH: chain.fetch };

// 订阅站认了一笔订单:一张已发放的订单 + 挂在它上面的一笔监听到账的付款。金额照订阅站的样子带一截零头(第 6 位小数不是 0)。
function order(amount, where = 'solana') {
  let m = Math.round(amount * 1e6) + 1 + rnd(1)[0] * 37 % 9999;
  if (m % 10 === 0) m += 3;
  const at = now(), hash = Buffer.from(rnd(32)).toString('hex');
  commerce.prepare("INSERT INTO orders VALUES(?,?,?,'fulfilled',?,?)").run(hash, m, tronAddr, at * 1000 - 60000, at * 1000 + 60000);
  if (where === 'tron') {
    const tx = Buffer.from(rnd(32)).toString('hex');
    commerce.prepare("INSERT INTO payments(tx_id,address,amount_micros,block_timestamp,event_json,status,order_hash,checked_at,created_at) VALUES(?,?,?,?,?,'credited',?,?,?)")
      .run(tx, tronAddr, String(m), at * 1000, JSON.stringify({ from: b58(Uint8Array.from([0x41, ...rnd(24)])), to: tronAddr, value: String(m) }), hash, at * 1000, at * 1000);
    chain.tron.set(tronAddr, (chain.tron.get(tronAddr) || 0) + m);
    return { chain: 'tron', tx, amount: m / 1e6 };
  }
  commerce.prepare('INSERT OR IGNORE INTO solana_amount_reservations VALUES(?,?,?)').run(solAddr, m, at);
  const paid = chain.pay(solAddr, m, at);
  commerce.prepare("INSERT INTO payments(tx_id,address,amount_micros,block_timestamp,event_json,status,order_hash,checked_at,created_at) VALUES(?,?,?,?,?,'credited',?,?,?)")
    .run('solana:' + paid.signature, solAddr, String(m), at * 1000, JSON.stringify({ network: 'SOLANA', signature: paid.signature }), hash, at * 1000, at * 1000);
  return { chain: 'solana', tx: paid.signature, amount: m / 1e6 };
}
const rows = sql => db.prepare(sql).all().map(r => ({ ...r }));

async function dev(method, pathname, body) {
  if (method === 'POST' && pathname === '/dev/pay') {
    const m = Math.round(Number(body.amount) * 1e6);
    if (!(m > 0)) return [400, { error: 'amount?' }];
    const paid = chain.pay(solAddr, m, now());
    return [200, { tx: paid.signature, from: paid.payer, amount: m / 1e6 }];
  }
  if (method === 'POST' && pathname === '/dev/order') {
    if (!(Number(body.amount) > 0)) return [400, { error: 'amount?' }];
    return [200, order(Number(body.amount), body.chain)];
  }
  if (method === 'POST' && pathname === '/dev/tick') { const l = await tick(env, now()); return [200, { pool: l && l.pool }]; }
  if (method === 'GET' && pathname === '/dev/state') {
    return [200, {
      pool: { solana: solAddr, tron: tronAddr },
      panels: rows('SELECT substr(pub, 1, 8) AS panel, address, gb, counted_gb, base_gb, last_seen, excluded, rule FROM panels ORDER BY first_seen'),
      addresses: rows('SELECT * FROM addresses ORDER BY first_seen'),
      intents: rows('SELECT id, name, micros, exact, status, created_at, expires_at FROM intents ORDER BY created_at DESC LIMIT 20'),
      events: rows('SELECT * FROM events ORDER BY id DESC LIMIT 20'),
      deposits: rows('SELECT kind, chain, micros, name, substr(tx, 1, 12) AS tx FROM deposits ORDER BY ts DESC LIMIT 20'),
    }];
  }
  return [404, { error: 'no such dev endpoint' }];
}

http.createServer(async (req, res) => {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const text = Buffer.concat(chunks).toString('utf8');
  const url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
  try {
    if (url.pathname.startsWith('/dev/')) {
      let body = {};
      try { body = text ? JSON.parse(text) : {}; } catch { /* 当成空的 */ }
      const [status, out] = await dev(req.method, url.pathname, body);
      res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8' });
      res.end(JSON.stringify(out, null, 1));
      return;
    }
    const ip = req.headers['x-dev-ip'] || String(req.socket.remoteAddress || '').replace(/^::ffff:/, '');
    const r = await worker.fetch(new Request(url, { method: req.method, headers: { 'CF-Connecting-IP': ip, 'Content-Type': 'application/json' }, body: ['GET', 'HEAD'].includes(req.method) ? undefined : text }), env);
    const out = await r.text();
    res.writeHead(r.status, Object.fromEntries(r.headers));
    res.end(out);
    console.log(new Date().toISOString().slice(11, 19), req.method, url.pathname, r.status, req.method === 'GET' ? '' : out.slice(0, 160));
  } catch (e) {
    res.writeHead(500); res.end(String(e && e.stack || e));
  }
}).listen(port, '127.0.0.1', async () => {
  order(9.99, 'tron'); order(19.99); // 两笔演示订单,奖池一开始不是空的
  await tick(env, now());
  console.log(`登记服务(试跑)在 http://127.0.0.1:${port}/v1 —— 假的订单库、模拟的链,奖池地址是假的:${solAddr}`);
});
setInterval(() => tick(env, now()).catch(e => console.error('tick', e)), 15000).unref?.();
process.on('SIGINT', () => { try { db.close(); commerce.close(); fs.rmSync(dir, { recursive: true, force: true }); } catch { /* 退出时清不干净不要紧 */ } process.exit(0); });

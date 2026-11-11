// 建议与支持页(web/assets/support.html)里"留名后等到账"那段脚本的前端测试:按 Go 模板的写法渲染出中文 / 英文两版,
// 用桩 DOM、假的 fetch、时钟和定时器跑真实的脚本,不联网、不监听端口。
// 运行:node --test web/jstest/support.test.mjs(CI 里和 deploy/donors 的测试一样跑)。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const TPL = fs.readFileSync(process.env.SUPPORT_HTML || path.join(here, '..', 'assets', 'support.html'), 'utf8');

// 够这一页用的 Go 模板:{{if eq .X "v"}} / {{if .X}} / {{else}} / {{end}} / {{.X}}。脚本里的动作只有这几种。
function render(tpl, data) {
  const toks = [];
  let last = 0;
  for (const m of tpl.matchAll(/\{\{(.*?)\}\}/gs)) {
    toks.push({ text: tpl.slice(last, m.index) }, { act: m[1].trim() });
    last = m.index + m[0].length;
  }
  toks.push({ text: tpl.slice(last) });
  let i = 0;
  const cond = c => {
    let m;
    if ((m = /^eq \.(\w+) "([^"]*)"$/.exec(c))) return data[m[1]] === m[2];
    if ((m = /^\.(\w+)$/.exec(c))) return !!data[m[1]];
    throw new Error('没见过的模板条件: ' + c);
  };
  const block = () => {
    let out = '';
    while (i < toks.length) {
      const t = toks[i++];
      if (t.text !== undefined) { out += t.text; continue; }
      if (t.act === 'else' || t.act === 'end') return { out, stop: t.act };
      let m;
      if ((m = /^if (.+)$/.exec(t.act))) {
        const yes = block(), no = yes.stop === 'else' ? block() : { out: '' };
        out += cond(m[1]) ? yes.out : no.out;
      } else if ((m = /^\.(\w+)$/.exec(t.act))) out += String(data[m[1]]);
      else throw new Error('没见过的模板写法: ' + t.act);
    }
    return { out, stop: null };
  };
  return block().out;
}

const T0 = 1_800_000_000; // 留名时的服务器时间(秒)
const H = 3600e3, MIN = 60e3; // 毫秒
const flush = async () => { for (let i = 0; i < 50; i++) await null; }; // 让 fetch 的 then 链跑完

// 打开一页:浏览器里已经记着一笔 T0 留的名(60 分钟后到期),这台电脑的钟和服务器一样准;页面在留名后 start 毫秒打开。
// onCheck(url, reply) 决定每次 support/pledge-check 怎么回(返回 Promise;reply(body) 是带上服务器时间的正常响应)。
async function openPage(lang, onCheck, { start = 500 } = {}) {
  const html = render(TPL, { Lang: lang, Online: true, Version: '0.0.0', Telegram: 'tg', TelegramURL: '#', Tron: 'TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm', TronHead: 'TDoU', TronTail: 'P1Cm', Repo: '#', Site: '#' });
  const script = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]).find(s => s.includes('support/pledge-check'));
  assert.ok(script, '找不到留名的脚本');
  const clock = { ms: T0 * 1000 + start };
  const pg = { lang, clock, checks: [], els: {}, store: new Map() };

  // 桩 DOM:按 id 取元素(没有就建)
  const el = id => (pg.els[id] ||= {
    id, hidden: false, textContent: '', title: '', value: '', attrs: {}, listeners: {},
    classList: { set: new Set(), toggle(c, on) { if (on) this.set.add(c); else this.set.delete(c); }, add(c) { this.set.add(c); }, remove(c) { this.set.delete(c); } },
    addEventListener(ev, fn) { (this.listeners[ev] ||= []).push(fn); },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return this.attrs[k] ?? null; },
    querySelector(sel) { return el(id + ' ' + sel); },
    appendChild() {},
  });
  pg.el = el;
  el('p-hint').textContent = /<p class="phint" id="p-hint">([\s\S]*?)<\/p>/.exec(html)[1];
  for (const id of ['board', 'p-wait', 'p-done', 'devs']) el(id).hidden = true;
  const document = { getElementById: el, querySelectorAll: () => [], createElement: tag => el('new-' + tag + '-' + Math.random()) };

  // 假定时器:时间只在测试推的时候走
  const timers = new Map();
  let ids = 0;
  const setInterval = (fn, ms) => { timers.set(++ids, { fn, ms, due: clock.ms + ms, every: true }); return ids; };
  const setTimeout = (fn, ms = 0) => { timers.set(++ids, { fn, ms, due: clock.ms + ms, every: false }); return ids; };
  const clear = id => { timers.delete(id); };
  // 正常往前走 ms:到点的定时器按时间先后一个个跑
  pg.advance = async ms => {
    const end = clock.ms + ms;
    for (;;) {
      let next = null;
      for (const e of timers) if (e[1].due <= end && (!next || e[1].due < next[1].due)) next = e;
      if (!next) break;
      const [id, t] = next;
      clock.ms = Math.max(clock.ms, t.due);
      if (t.every) t.due += t.ms; else timers.delete(id);
      t.fn();
      await flush();
    }
    clock.ms = end;
  };
  // 标签页被冻结(或电脑睡眠)ms:这段时间什么都不跑;回来后每个过了点的定时器各跑一次,按到点先后(浏览器解冻时就是这样)
  pg.freeze = async ms => {
    clock.ms += ms;
    const due = [...timers].filter(([, t]) => t.due <= clock.ms).sort((a, b) => a[1].due - b[1].due);
    for (const [id, t] of due) {
      if (!timers.has(id)) continue;
      if (t.every) t.due = clock.ms + t.ms; else timers.delete(id);
      t.fn();
      await flush();
    }
  };

  const serverNow = () => Math.floor(clock.ms / 1000);
  pg.reply = body => Promise.resolve({ ok: true, json: async () => ({ ...body, now: serverNow() }) });
  const fetch = url => {
    url = String(url);
    if (url === 'support/donors') return pg.reply({ donors: [], devices: 0, devicesAt: 0 });
    if (url.startsWith('support/pledge-check?')) { pg.checks.push(clock.ms); return onCheck(url, pg.reply); }
    return Promise.reject(new Error('意外的请求 ' + url));
  };
  const localStorage = { getItem: k => (pg.store.has(k) ? pg.store.get(k) : null), setItem: (k, v) => pg.store.set(k, String(v)), removeItem: k => pg.store.delete(k) };
  pg.store.set('m-ui-pledge', JSON.stringify({ amount: '10.037', name: 'Larry', at: T0, expiresAt: T0 + 3600, address: 'T', now: T0, skew: 0 }));
  const FakeDate = class extends Date { static now() { return clock.ms; } };

  new Function('document', 'localStorage', 'fetch', 'setInterval', 'clearInterval', 'setTimeout', 'clearTimeout', 'Date', 'navigator', script)(
    document, localStorage, fetch, setInterval, clear, setTimeout, clear, FakeDate, {});
  await flush();
  assert.equal(el('p-wait').hidden, false, '应该在等到账');
  return pg;
}

const RECEIVED = { zh: /收到了,谢谢 Larry!/, en: /Received, thank you Larry! [A-Z]/ };
const NOT_ARRIVED = { zh: /超过 60 分钟未到账/, en: /Not received within 60 minutes/ };
const UNKNOWN = { zh: /暂时无法确认是否到账/, en: /couldn't confirm whether it arrived/ };
const ended = pg => pg.el('p-wait').hidden && !pg.store.has('m-ui-pledge');
// 上榜靠 GitHub 的定时任务,常常晚几个小时,两次运行隔太久时留名还会丢:不能承诺"几小时内上榜",要给出超过一天的办法
const SOFT = { zh: /通常几小时内上榜;超过一天还没上榜,请在 Telegram 联系我/, en: /usually appear on the list within a few hours\. If you're not on it after a day, message me on Telegram/ };
const HARD = { zh: /会在几小时内上榜/, en: /you'll appear on the list within/ };

for (const lang of ['zh', 'en']) {
  test(`[${lang}] 标签页冻结一个多小时后回来:先再核对一次,冻结期间到账了就说收到了,不拿冻结前的结果断言没到账`, async () => {
    let body = { known: true, arrived: false };
    const pg = await openPage(lang, (url, reply) => reply(body));
    await pg.advance(10 * MIN); // 正常等了 10 分钟,每 20 秒核对一次
    const before = pg.checks.length;
    assert.ok(before >= 30, String(before));
    body = { known: true, arrived: true }; // 冻结期间到账了
    await pg.freeze(70 * MIN);
    assert.ok(pg.checks.length > before, '回来后要先再核对一次');
    assert.ok(ended(pg));
    assert.equal(pg.el('p-done').hidden, false);
    assert.match(pg.el('p-done').textContent, RECEIVED[lang]);
    assert.doesNotMatch(pg.el('p-hint').textContent, NOT_ARRIVED[lang]);
  });

  test(`[${lang}] 冻结后回来再核对:确实没到,才说超过 60 分钟未到账`, async () => {
    const pg = await openPage(lang, (url, reply) => reply({ known: true, arrived: false }));
    await pg.advance(10 * MIN);
    const before = pg.checks.length;
    await pg.freeze(70 * MIN);
    assert.equal(pg.checks.length, before + 1, '回来后核对一次');
    assert.ok(ended(pg));
    assert.match(pg.el('p-hint').textContent, NOT_ARRIVED[lang]);
    assert.equal(pg.el('p-done').hidden, true);
  });

  test(`[${lang}] 冻结后回来再核对失败或一直没回:说暂时无法确认,不说没到账`, async () => {
    let mode = 'ok';
    const pg = await openPage(lang, (url, reply) => mode === 'ok' ? reply({ known: true, arrived: false }) : mode === 'fail' ? Promise.reject(new TypeError('Failed to fetch')) : new Promise(() => {}));
    await pg.advance(10 * MIN);
    mode = 'fail';
    await pg.freeze(70 * MIN);
    assert.ok(ended(pg));
    assert.match(pg.el('p-hint').textContent, UNKNOWN[lang]);

    mode = 'ok';
    const pg2 = await openPage(lang, (url, reply) => mode === 'ok' ? reply({ known: true, arrived: false }) : new Promise(() => {}));
    await pg2.advance(10 * MIN);
    mode = 'hang';
    const before = pg2.checks.length;
    await pg2.freeze(70 * MIN);
    assert.equal(pg2.checks.length, before + 1);
    assert.equal(ended(pg2), false, '先等这次核对');
    await pg2.advance(20e3); // 最多等十几秒
    assert.ok(ended(pg2));
    assert.match(pg2.el('p-hint').textContent, UNKNOWN[lang]);
  });

  test(`[${lang}] 冻结前发出、回来才处理的那次核对是旧的:不等它也不信它,另查一次再下结论`, async () => {
    let hold = null, body = { known: true, arrived: false };
    const pg = await openPage(lang, (url, reply) => hold ? new Promise(r => { hold.resolve = r; }) : reply(body));
    await pg.advance(10 * MIN);
    hold = {};
    await pg.advance(20e3); // 这一次刚发出去,还没回来标签页就被冻结了
    const sent = pg.checks.length, late = hold;
    assert.ok(late.resolve);
    hold = null;
    body = { known: true, arrived: true }; // 冻结期间到账了
    await pg.freeze(70 * MIN);
    assert.equal(pg.checks.length, sent + 1, '另查一次');
    assert.ok(ended(pg));
    assert.match(pg.el('p-done').textContent, RECEIVED[lang]);
    late.resolve({ ok: true, json: async () => ({ known: true, arrived: false, now: T0 + 620 }) }); // 冻结前查的(没到),这时才回来
    await flush();
    assert.equal(pg.el('p-done').hidden, false, '旧的结果改不了结论');
    assert.match(pg.el('p-done').textContent, RECEIVED[lang]);
  });

  test(`[${lang}] 到期时刚发出的那次核对还在路上:等它回来再下结论`, async () => {
    let hold = null;
    // 留名 10.5 秒后打开:之后每 20 秒核对一次,最后一次在到期前 9.5 秒
    const pg = await openPage(lang, (url, reply) => hold ? new Promise(r => { hold.resolve = r; }) : reply({ known: true, arrived: false }), { start: 10500 });
    await pg.advance(H - 10500 - 11e3);
    hold = {};
    await pg.advance(2e3); // 到期前最后一次核对发出去了,还没回来
    assert.ok(hold.resolve);
    const sent = pg.checks.length;
    await pg.advance(10e3); // 到期了
    assert.equal(ended(pg), false, '先等它');
    assert.equal(pg.checks.length, sent, '不另查');
    hold.resolve({ ok: true, json: async () => ({ known: true, arrived: true, now: T0 + 3601 }) });
    await flush();
    assert.ok(ended(pg));
    assert.match(pg.el('p-done').textContent, RECEIVED[lang]);
  });

  test(`[${lang}] 某次核对一直没回:不卡住之后的核对`, async () => {
    let hang = false;
    const pg = await openPage(lang, (url, reply) => hang ? new Promise(() => {}) : reply({ known: true, arrived: false }));
    await pg.advance(5 * MIN);
    hang = true;
    await pg.advance(20e3);
    const n = pg.checks.length;
    hang = false;
    await pg.advance(60e3);
    assert.ok(pg.checks.length >= n + 2, `一分钟里应照常核对: ${pg.checks.length - n}`);
    assert.equal(ended(pg), false);
  });

  test(`[${lang}] 一直开着(没冻结):到期时最近一次核对不到 20 秒,直接下结论,不多查`, async () => {
    const pg = await openPage(lang, (url, reply) => reply({ known: true, arrived: false }));
    await pg.advance(H + 5e3);
    assert.ok(ended(pg));
    assert.match(pg.el('p-hint').textContent, NOT_ARRIVED[lang]);
    assert.equal(pg.checks.filter(t => t >= (T0 + 3600) * 1000).length, 0, '到期之后没有多余的核对');
    assert.ok(pg.checks.length >= 170);
  });

  test(`[${lang}] 冻结前发出的核对回来时带的是旧的服务器时间:不拿它算钟差,倒计时不往回跳`, async () => {
    let hold = null;
    const pg = await openPage(lang, (url, reply) => hold ? new Promise(r => { hold.resolve = r; }) : reply({ known: true, arrived: false }));
    await pg.advance(10 * MIN);
    hold = {};
    await pg.advance(20e3);
    const late = hold, sentAt = pg.checks.at(-1);
    hold = null;
    await pg.freeze(5 * MIN); // 还没到期
    late.resolve({ ok: true, json: async () => ({ known: true, arrived: false, now: Math.floor(sentAt / 1000) }) });
    await flush();
    await pg.advance(1000);
    const left = T0 + 3600 - Math.floor(pg.clock.ms / 1000);
    assert.equal(pg.el('p-left').textContent, Math.floor(left / 60) + ':' + String(left % 60).padStart(2, '0'));
  });

  test(`[${lang}] 到账、无法确认时的提示不做硬承诺:通常几小时内上榜,超过一天还没上榜就联系`, async () => {
    let n = 0;
    const pg = await openPage(lang, (url, reply) => reply({ known: true, arrived: n++ > 0 }));
    await pg.advance(20e3); // 第二次核对时到账了
    assert.ok(ended(pg));
    assert.match(pg.el('p-done').textContent, RECEIVED[lang]);
    assert.match(pg.el('p-done').textContent, SOFT[lang]);
    assert.doesNotMatch(pg.el('p-done').textContent, HARD[lang]);

    const pg2 = await openPage(lang, () => Promise.reject(new TypeError('Failed to fetch')));
    await pg2.advance(H + 20e3);
    assert.ok(ended(pg2));
    assert.match(pg2.el('p-hint').textContent, UNKNOWN[lang]);
    assert.match(pg2.el('p-hint').textContent, SOFT[lang]);
    assert.doesNotMatch(pg2.el('p-hint').textContent, HARD[lang]);
  });
}

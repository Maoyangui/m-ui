import test from 'node:test';
import assert from 'node:assert/strict';
import { settle, parsePledge, validName, toMicros, deviceCount, publicBoard, ADDRESS, USDT } from './settle.mjs';

const tx = (id, usdt, at, extra = {}) => ({ transaction_id: id, value: String(Math.round(usdt * 1e6)), block_timestamp: at * 1000, to: ADDRESS, type: 'Transfer', token_info: { address: USDT }, ...extra });
const pledge = (id, name, usdt, at) => ({ id, name, micros: Math.round(usdt * 1e6), at });

test('名字规则和面板一致', () => {
  assert.equal(validName(' Larry  Wang '), 'Larry Wang');
  assert.equal(validName('张三'), '张三');
  for (const bad of ['', 'http://a.com', '加我 t.me', '<b>x</b>', 'a'.repeat(25), 12]) assert.equal(validName(bad), '', String(bad));
});

test('留名消息只信 ntfy 的时间,乱写的不要', () => {
  const ok = parsePledge({ event: 'message', id: 'x', time: 100, message: '{"n":"Larry","a":"10.037"}' });
  assert.deepEqual(ok, { id: 'x', name: 'Larry', micros: 10037000, at: 100 });
  for (const m of [{ event: 'open', id: 'x', time: 1 }, { event: 'message', id: 'x', time: 1, message: 'not json' }, { event: 'message', id: 'x', time: 1, message: '{"n":"http://a.com","a":"1"}' }, { event: 'message', id: 'x', time: 1, message: '{"n":"a","a":"0.5"}' }]) assert.equal(parsePledge(m), null);
  assert.equal(toMicros('32.83885'), 32838850);
});

test('60 分钟内金额相同才算,累计到名字上', () => {
  const r = settle({}, [pledge('p1', 'Larry', 10.037, 1000)], [tx('t1', 10.037, 1500)], 2000);
  assert.equal(r.changed, true);
  assert.equal(r.state.donors.Larry, 10037000);
  assert.equal(r.state.pending.length, 0);
  const r2 = settle(r.state, [pledge('p2', 'Larry', 5.001, 3000)], [tx('t1', 10.037, 1500), tx('t2', 5.001, 3100)], 3200);
  assert.equal(r2.state.donors.Larry, 15038000, '同一笔不能算两次,同名累加');
  assert.deepEqual(publicBoard(r2.state, 7, 9), { updatedAt: 9, devices: 7, donors: [{ name: 'Larry', total: 15.038 }] });
});

test('对不上的都不算', () => {
  for (const [name, p, t] of [
    ['金额差一点', pledge('a', 'X', 10.037, 1000), tx('t', 10.038, 1100)],
    ['转账在留名之前', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 900)],
    ['超过 60 分钟', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1000 + 3601)],
    ['转给别的地址', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1100, { to: 'TOther' })],
    ['别的币', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1100, { token_info: { address: 'TFake' } })],
    ['授权不是转账', pledge('a', 'X', 10.037, 1000), tx('t', 10.037, 1100, { type: 'Approval' })],
  ]) {
    const r = settle({}, [p], [t], 1200);
    assert.equal(r.changed, false, name);
    assert.deepEqual(r.state.donors, {}, name);
  }
});

test('同样的金额两个人留名:先到先得,第二笔给第二个人', () => {
  const r = settle({}, [pledge('b', 'B', 10.037, 1100), pledge('a', 'A', 10.037, 1000)], [tx('t1', 10.037, 1200), tx('t2', 10.037, 1300)], 1400);
  assert.equal(r.state.donors.A, 10037000);
  assert.equal(r.state.donors.B, 10037000);
});

test('留名消息反复读到也只算一次,对过账的不会再进待对账', () => {
  const r = settle({}, [pledge('a', 'A', 1.5, 1000)], [tx('t1', 1.5, 1100)], 1200);
  const r2 = settle(r.state, [pledge('a', 'A', 1.5, 1000)], [tx('t1', 1.5, 1100), tx('t2', 1.5, 1150)], 1300);
  assert.equal(r2.state.pending.length, 0);
  assert.equal(r2.state.donors.A, 1500000);
});

test('过期的待对账清掉,屏蔽名单生效', () => {
  const r = settle({}, [pledge('a', 'A', 1.5, 1000)], [], 1000 + 7300);
  assert.equal(r.state.pending.length, 0);
  const r2 = settle({ blocked: ['bad guy'] }, [pledge('b', 'Bad Guy', 1.5, 1000)], [tx('t', 1.5, 1100)], 1200);
  assert.deepEqual(r2.state.donors, {});
});

test('运行中的面板数:下载计数 24 小时涨了多少', () => {
  const day = 24 * 3600;
  let r = deviceCount([], 100, 1000);
  assert.equal(r.devices, 0, '不到 24 小时先不给');
  r = deviceCount(r.points, 150, 1000 + day + 10);
  assert.equal(r.devices, 50);
  r = deviceCount(r.points, 160, 1000 + day + 20);
  assert.equal(r.points.length, 2, '一小时内不重复记点');
  assert.equal(r.devices, 60);
});

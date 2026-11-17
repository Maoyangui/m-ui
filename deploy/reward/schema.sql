-- 流量奖励登记服务自己的库(D1)。订阅站的订单库是另一个绑定、只读,这里一张它的表都没有。

-- 面板:公钥就是身份。gb = 它报上来的累计值(只增);counted_gb = 算进积分的部分(增长有上限,不计入期间不涨);
-- base_gb = 不算数的那部分(换绑之前的、被作废的)。这台面板给它现在的地址贡献 counted_gb - base_gb。
-- 来源只存加盐的哈希;src_since = 从什么时候起在这处来源上报。
-- excluded = 这台面板的上报现在不计入(自动判的,原因在 reason;hit_at = 最近一次命中规则的时间)。
CREATE TABLE IF NOT EXISTS panels (
  pub TEXT PRIMARY KEY,
  address TEXT NOT NULL,
  gb INTEGER NOT NULL,
  counted_gb INTEGER NOT NULL,
  base_gb INTEGER NOT NULL DEFAULT 0,
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  bound_at INTEGER NOT NULL,
  ip_hash TEXT NOT NULL DEFAULT '',
  src_since INTEGER NOT NULL DEFAULT 0,
  excluded INTEGER NOT NULL DEFAULT 0,
  rule TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  excluded_at INTEGER NOT NULL DEFAULT 0,
  hit_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS panels_address ON panels(address);
CREATE INDEX IF NOT EXISTS panels_ip ON panels(ip_hash, last_seen);
CREATE INDEX IF NOT EXISTS panels_excluded ON panels(excluded);

-- 每台面板每天一条(当天最后一次报上来的累计值,留 60 天):自动识别作弊时看每天涨多少。
CREATE TABLE IF NOT EXISTS samples (
  pub TEXT NOT NULL,
  day INTEGER NOT NULL,
  gb INTEGER NOT NULL,
  PRIMARY KEY (pub, day)
);
CREATE INDEX IF NOT EXISTS samples_day ON samples(day);

-- 地址:已入账的积分。settled_gb = 以前绑在这个地址、后来换绑走了的面板留下的流量。
-- 挣到的 = (settled_gb + 名下各台面板的贡献) 每满 1 TB 一个;入账每天最多 1 个。
CREATE TABLE IF NOT EXISTS addresses (
  address TEXT PRIMARY KEY,
  first_seen INTEGER NOT NULL,
  credited INTEGER NOT NULL DEFAULT 0,
  credit_day INTEGER NOT NULL DEFAULT 0,
  settled_gb INTEGER NOT NULL DEFAULT 0
);

-- 不计入 / 恢复 / 作废积分的记录(公开在账本里)。panel 是面板公钥的头几位。
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  address TEXT NOT NULL,
  panel TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  rule TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT ''
);

-- 入账(只加不删):order = 订阅站认了的订单付款(从订单库读);donation = 链上看到的、不是订单付款的转入。
-- 捐赠对上了留名的带名字。tx 是链上的交易号,谁都能拿去查。
CREATE TABLE IF NOT EXISTS deposits (
  tx TEXT PRIMARY KEY,
  chain TEXT NOT NULL,
  address TEXT NOT NULL,
  ts INTEGER NOT NULL,
  from_addr TEXT NOT NULL DEFAULT '',
  micros INTEGER NOT NULL,
  kind TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS deposits_ts ON deposits(ts);
CREATE INDEX IF NOT EXISTS deposits_kind ON deposits(kind);

-- 留名捐赠:给出要转的确切金额(在填的数上加一点零头),期限内这个数到账就以这个名字记上。
CREATE TABLE IF NOT EXISTS intents (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  micros INTEGER NOT NULL,
  exact INTEGER NOT NULL,
  address TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  status TEXT NOT NULL,
  tx TEXT NOT NULL DEFAULT '',
  ip_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS intents_exact ON intents(address, exact);
CREATE INDEX IF NOT EXISTS intents_ip ON intents(ip_hash, created_at);

-- 奖池地址上新出现的交易(还没看的排在这里,看完就标记;只为找捐赠)。
CREATE TABLE IF NOT EXISTS sigs (
  sig TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  ts INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'new',
  tries INTEGER NOT NULL DEFAULT 0,
  seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sigs_state ON sigs(state, ts);

CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);

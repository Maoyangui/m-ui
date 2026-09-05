# 外部 API(v1)

供商城、计费系统、Telegram 机器人等外部程序调用,与面板登录会话无关。

## 开启与鉴权

1. 面板 → **管理员** → **外部 API**,打开开关,复制令牌。
2. 每个请求带令牌,两种写法任选其一:
   - `Authorization: Bearer <令牌>`
   - `X-API-Key: <令牌>`
3. 地址前缀:`https://<面板域名或IP>:<面板端口><面板路径>api/v1`,管理员页会直接显示可复制的完整前缀。

令牌泄露时在管理员页点"重新生成",旧令牌立即失效。关闭开关后所有 v1 接口返回 401。

所有响应为 JSON;失败返回 4xx 与 `{"error": "原因"}`。时间戳单位为秒,流量单位为字节。

## 代理的外部 API

代理也有同一套接口:代理面板 → **我的账号** → **外部 API**,打开开关,复制令牌。

- 地址前缀:`https://<面板域名或IP>:<代理面板端口><代理面板路径>api/v1`(默认 `:2054/dl/api/v1`),账号页会直接显示。
- 鉴权方式与主面板相同(`Authorization: Bearer <令牌>` 或 `X-API-Key`),令牌只对应这个代理,主面板入口不认代理令牌,反之亦然。
- 作用域和代理在面板里能做的完全一致:只看得到、改得动自己名下的用户;`/plans` 只列自己建的套餐;线路只能用主面板授权给他的(`lineIds` 含未授权线路返回 400);不指定线路时分配授权的全部线路;`extIds` 不可用;用户数上限、流量额度同样生效。
- 代理被停用、到期,或在账号页关掉开关后,令牌立即返回 401。

## 接口一览

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/ping` | 连通性检查,返回版本与角色 |
| GET | `/plans` | 套餐列表 |
| GET | `/users` | 用户列表,可选 `?q=关键字`(匹配名称/备注)、`?enabled=true\|false` |
| POST | `/users` | 创建用户 |
| GET | `/users/{name\|id}` | 用户详情(含订阅地址、在线 IP) |
| PATCH / PUT | `/users/{name\|id}` | 修改用户(只改给出的字段) |
| DELETE | `/users/{name\|id}` | 删除用户 |
| POST | `/users/{name\|id}/enable` | 启用 |
| POST | `/users/{name\|id}/disable` | 停用(并踢下线) |
| POST | `/users/{name\|id}/reset` | 本周期用量清零并启用 |
| POST | `/users/{name\|id}/kick` | 踢下线,返回关闭的连接数 |
| POST | `/users/{name\|id}/plan` | 套用套餐(续费/延期) |
| GET | `/users/{name\|id}/sub` | 订阅地址 `{link, clash}` |

`{name|id}` 先按用户名匹配,匹配不到且为数字时按 id。

## 用户对象

```json
{
  "id": 12, "name": "alice", "enabled": true,
  "volume": 107374182400, "used": 1234567, "up": 100, "down": 1234467,
  "totalUp": 0, "totalDown": 0,
  "expiry": 1760000000, "expired": false,
  "autoReset": true, "resetDays": 30, "nextReset": 1758000000,
  "deviceLimit": 3, "speedUp": 0, "speedDown": 0,
  "remark": "订单 #1001", "desc": "",
  "createdAt": 1756000000, "onlineAt": 1756800000, "onlineIps": ["1.2.3.4"],
  "lineIds": [1, 2], "extIds": [],
  "subLink": "https://example.com:2056/sub/alice",
  "subClash": "https://example.com:2056/sub/alice?format=clash",
  "subJson": "https://example.com:2056/sub/alice?format=json"
}
```

## 创建 / 修改 的请求字段

> 线路分配有两种写法:(整条线路,即该线路部署到的全部服务器,包括以后新加的)和 (线路 × 服务器,如 , 省略 = 该线路全部服务器)。两者同时给时以  为准;用户对象里两个字段都会返回。代理令牌下  只能落在主面板授权给该代理的服务器内。


全部可选(创建时 `name` 必填),没给的字段不改动:

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 用户名(不能含空格与 `/ ? # &`) |
| `enabled` | bool | 启停 |
| `planId` / `plan` | number / string | 套餐 id 或名称。创建时按"新建"套用;修改时按 `mode` 套用 |
| `mode` | string | 套餐套用方式:`renew`(默认,用量清零、到期顺延)或 `extend`(保留用量) |
| `volumeGb` / `volume` | number | 配额,GB 或字节;0 = 不限 |
| `days` | number | 创建:自现在起 N 天;修改:在原到期(未过期)基础上再加 N 天;≤0 = 不限 |
| `expiry` | number | 到期时间戳,优先于 `days`;0 = 不限 |
| `deviceLimit` | number | 同时在线设备数,0 = 不限 |
| `speedUp` / `speedDown` | number | 限速 Mbps,0 = 不限 |
| `autoReset` / `resetDays` | bool / number | 周期重置用量 |
| `remark` / `desc` | string | 备注 / 说明 |
| `lineIds` | number[] | 可用线路;创建时不给且套餐未指定 → 分配全部线路 |
| `extIds` | number[] | 可用外部节点 |

显式字段优先级高于套餐:例如 `{"plan": "月付", "days": 60}` 会先套用月付套餐,再把到期改为 60 天后。

## 示例

```bash
BASE="https://panel.example.com:2053/app/api/v1"
TOKEN="你的令牌"

# 连通性
curl -s -H "Authorization: Bearer $TOKEN" "$BASE/ping"

# 按套餐名开号(30 天、100 GB 之类都由套餐决定)
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"alice","plan":"月付","remark":"订单 #1001"}' "$BASE/users"

# 不用套餐,直接给参数
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"bob","volumeGb":200,"days":90,"deviceLimit":3}' "$BASE/users"

# 续费:再套一次套餐(用量清零、到期顺延)
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"plan":"月付","mode":"renew"}' "$BASE/users/alice/plan"

# 停用 / 启用
curl -s -X POST -H "Authorization: Bearer $TOKEN" "$BASE/users/alice/disable"
curl -s -X POST -H "Authorization: Bearer $TOKEN" "$BASE/users/alice/enable"

# 只改到期:再加 7 天
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"days":7}' "$BASE/users/alice"

# 取订阅地址
curl -s -H "Authorization: Bearer $TOKEN" "$BASE/users/alice/sub"
```

## 注意

- 所有写操作都记入面板"操作审计",操作人显示为 `api`。
- 接口只在主服务器上有意义:副服务器的用户由主机下发,在副机调用会被下一次同步覆盖。
- 令牌等同管理员权限,请只放在服务端程序里,不要写进网页前端或客户端。

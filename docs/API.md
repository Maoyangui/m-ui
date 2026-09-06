# 外部 API(v1)

供商城、计费系统、Telegram 机器人等外部程序调用,与面板登录会话无关:凭令牌即可建号、套用套餐、续费、启停、踢下线、重置订阅链接、取订阅地址。

同一套接口有两个入口:

| 入口 | 谁用 | 令牌在哪 | 地址前缀(默认端口 / 路径) | 能看到、改动的范围 |
|---|---|---|---|---|
| 主面板 | 站长 | 面板 → **管理员** → **外部 API** | `https://<域名或IP>:2053/app/api/v1` | 全部用户、主面板的套餐、全部线路与外部节点 |
| 代理面板 | 代理 | 代理面板 → **我的账号** → **外部 API** | `https://<域名或IP>:2054/dl/api/v1` | 只有自己名下的用户、自己建的套餐、主面板授权给他的线路(含服务器范围) |

两个入口的路径、字段、返回完全一样,只是作用域不同;下文凡是"代理令牌下"的说明,都指第二个入口。主面板入口不认代理令牌,代理入口也不认主面板令牌(都返回 401)。

## 1. 开启与鉴权

### 主面板

1. 面板 → **管理员** → **外部 API**:打开开关,复制令牌。管理员页会直接显示可复制的完整前缀。
2. 也可以在服务器上用命令行开启(改完即时生效,不用重启):
   ```bash
   m-ui set -db /etc/m-ui/m-ui.db apiEnabled=true apiToken=<一串至少 16 位的随机字符>
   ```
   关闭:`m-ui set -db /etc/m-ui/m-ui.db apiEnabled=false apiToken=`
3. 令牌泄露时在管理员页点"重新生成",旧令牌立即失效。关闭开关后所有 v1 接口返回 401。

### 代理面板

1. 代理面板 → **我的账号** → **外部 API**:打开开关,首次开启自动生成令牌;账号页显示完整前缀。
2. "重新生成"后旧令牌立即失效。
3. 以下任一情况令牌立即返回 401:代理在账号页关掉开关、代理被主面板停用、代理到期。

### 请求头

每个请求带令牌,两种写法任选其一(同时给时以 `Authorization` 为准):

```
Authorization: Bearer <令牌>
X-API-Key: <令牌>
```

带请求体的接口请发 JSON(建议加 `Content-Type: application/json`)。

## 2. 通用约定

- 所有响应为 JSON,`Cache-Control: no-store`。
- 时间戳单位为**秒**(Unix 时间);流量单位为**字节**;`volumeGb` 按 1 GB = 1073741824 字节换算;限速单位 Mbps。
- `0` 在配额、到期、设备数、限速上都表示**不限**。
- `{name|id}` 先按用户名精确匹配;匹配不到且形如数字时按用户 id。代理令牌下只在自己名下找,找不到一律 404。
- 失败返回 4xx 与 `{"error": "原因"}`(原因是中文,给人看的;判断请看状态码):

| 状态码 | 含义 |
|---|---|
| 400 | 请求体不是合法 JSON、字段不合法、名称重复、超出代理的授权 / 上限 / 额度、套餐不存在 |
| 401 | 外部 API 未开启、令牌错误、代理已停用 / 到期、用错入口 |
| 404 | 用户不存在(或不在代理名下)、接口不存在 |
| 405 | 方法不对(如对 `/enable` 发 GET) |

- 所有写操作记入面板"操作审计":主面板令牌的操作人显示为 `api`,代理令牌的显示为 `api:<代理名>`。
- 接口只在**主服务器**上调用:副服务器的用户由主机每 5 秒下发,在副机上改动会被下一次同步覆盖。`/ping` 返回的 `role` 是 `node` 就说明调错了机器:那台机器上建的用户几秒内就会被主机的快照抹掉,套餐、线路授权也不存在。
- 令牌等同该入口的全部权限,只放在服务端程序里,不要写进网页前端或客户端。令牌泄露就到面板里重新生成一个(旧的立即失效)。
- 频率与安全:接口本身不限流,由调用方控制节奏(建号、续费这类操作按需调用即可,不要轮询 `/users` 拉全表做同步);面板端口建议只对调用方的 IP 放行或挂在反向代理后面。列表接口没有分页,用户很多时用 `q` / `enabled` 缩小范围。
- 用户的启停有三种原因:手动、超量、到期(用户对象里的 `disabledReason`)。自动停用的用户在 `/reset`、`/plan` 或周期重置后自动恢复;手动停用的只有 `/enable`(或显式 `enabled: true`)才恢复。

### 代理令牌下的边界

| 项目 | 主面板令牌 | 代理令牌 |
|---|---|---|
| 用户 | 全部 | 只有 `resellerId` 等于自己的;主面板用户与其它代理的用户按名字、按 id 都找不到(404) |
| 套餐(`/plans`、`planId` / `plan`) | 主面板的套餐 | 自己建的套餐 |
| 线路(`lineIds` / `lineRefs`) | 任意 | 只能用主面板授权给他的;授权收窄到了某几台服务器的线路,只能用 `lineRefs` 指定其中的服务器,给整条(全部服务器)返回 400 |
| 不指定线路时 | 分配全部线路 | 分配授权的全部线路(含服务器范围) |
| 外部节点 `extIds` | 可用 | 忽略(代理不能分配外部节点) |
| 用户数上限 | 无 | 达到主面板设的上限后创建返回 400 |
| 代理流量额度 | 无 | 名下用户全时用量用尽后,创建与修改返回 400 |
| 单个用户的设备数 / 限速 | 任意 | 任意(不再限制"之和");运行时按代理的设备池 / 带宽池执行 |
| 新用户的订阅地址 | 按设置(用户名或随机令牌) | 总是随机令牌 |

## 3. 接口一览

| 方法 | 路径 | 说明 | 返回 |
|---|---|---|---|
| GET | `/ping` | 连通性检查 | `{ok, version, role, time}` |
| GET | `/plans` | 套餐列表 | 套餐对象数组 |
| GET | `/users` | 用户列表,可选 `?q=`、`?enabled=` | 用户对象数组 |
| POST | `/users` | 创建用户(可直接套用套餐) | 用户对象 |
| GET | `/users/{name\|id}` | 用户详情 | 用户对象 |
| PATCH / PUT | `/users/{name\|id}` | 修改用户(只改给出的字段) | 用户对象 |
| DELETE | `/users/{name\|id}` | 删除用户 | `{ok: true}` |
| POST | `/users/{name\|id}/enable` | 启用 | 用户对象 |
| POST | `/users/{name\|id}/disable` | 停用并踢下线 | 用户对象 |
| POST | `/users/{name\|id}/reset` | 本周期用量清零并启用 | 用户对象 |
| POST | `/users/{name\|id}/kick` | 踢下线 | `{closed}` |
| POST | `/users/{name\|id}/rotate` | 重置订阅链接与凭据 | 用户对象 |
| POST | `/users/{name\|id}/plan` | 套用套餐(续费 / 延期) | 用户对象 |
| GET | `/users/{name\|id}/sub` | 订阅地址 | `{link, clash, json[, share]}` |

## 4. 各接口说明

### GET /ping

连通性与身份检查,也是排查令牌是否配对的最快办法。

```json
{"ok": true, "version": "0.4.21", "role": "master", "time": 1788700000}
```

- `role`:主面板令牌下为 `master`(主服务器)或 `node`(这台是副服务器,不该在这里调接口:副机上的改动会被主机下一次同步覆盖,先用它确认调对了机器);代理令牌下固定为 `reseller`。

### GET /plans

套餐列表,按面板里的排序返回。主面板令牌只列主面板的套餐,代理令牌只列该代理自己建的。字段见 [套餐对象](#套餐对象)。

### GET /users

用户列表,按 id 升序。查询参数:

| 参数 | 说明 |
|---|---|
| `q` | 关键字,模糊匹配用户名或备注 |
| `enabled` | `true` 只列启用的,`false` 只列停用的;不给列全部 |

返回用户对象数组。列表和详情的字段完全一样(含在线 IP 与订阅地址),用户多时一次列表即可,不必逐个查详情。

### POST /users

创建用户。请求字段见 [创建 / 修改的请求字段](#5-创建--修改的请求字段),只有 `name` 必填。

行为:

- 新用户默认**启用**,各协议凭据自动生成。
- 给了 `planId` / `plan` 就先按"新建"方式套用套餐(配额、天数、设备数、限速、周期重置、套餐指定的线路),再用请求里显式给出的字段覆盖。
- 没给 `lineIds` / `lineRefs`、套餐也没指定线路:主面板令牌分配全部线路,代理令牌分配授权的全部线路(含服务器范围)。
- 订阅地址:主面板令牌建的用户按面板设置(默认用用户名作地址;设置里关掉后为随机令牌);代理令牌建的用户总是随机令牌。
- 用户名重复返回 400 `用户名已存在`。

返回创建后的用户对象,`subLink` / `subClash` / `subJson` 可直接发给用户。

### GET /users/{name|id}

用户详情,即完整的用户对象。

### PATCH / PUT /users/{name|id}

修改用户:请求体里**给出的字段才改**,没给的保持原样(`PUT` 与 `PATCH` 行为相同,都是部分更新)。

- 可以改名(`name`);主面板下按用户名作地址的用户改名后订阅地址也随之变化,客户端要重新导入。
- 给了 `planId` / `plan` 就先按 `mode` 套用套餐,再用其它显式字段覆盖。
- `days` 在修改时的语义是"再加 N 天":原到期未过就从原到期算,已过期或不限就从现在算。
- 凭据、累计流量、订阅令牌不经由这个接口改动(要换凭据用 `/rotate`,要清用量用 `/reset`)。

### DELETE /users/{name|id}

删除用户:断开其全部连接,删除线路与外部节点分配,数据面热更新。代理令牌下,该用户的全时用量会先结转到代理头上(删号不能洗额度)。返回 `{"ok": true}`。

### POST /users/{name|id}/enable · /disable

- `enable`:`enabled = true`、`disabledReason = ""`,数据面热更新后客户端立即可用。对超量或到期的用户调它也会启用,但下一分钟的判定会再次停用;先 `/reset` 或 `/plan`。
- `disable`:`enabled = false`、`disabledReason = "manual"`,同时断开该用户在主服务器上的连接;副服务器在下一次同步(≤ 5 秒)时把他撤下并断开。手动停用的用户不会被重置或续期自动恢复。

都返回更新后的用户对象。

### POST /users/{name|id}/reset

本周期用量清零:`up` / `down` 并入 `totalUp` / `totalDown` 后归零;因超量被停用的用户(`disabledReason = "quota"`)重新启用(常用于"超量停用后补量"),手动停用或已到期的保持原状。不改到期、不改周期重置日。返回用户对象。

### POST /users/{name|id}/kick

断开该用户在**主服务器**上的全部连接,返回 `{"closed": <连接数>}`。客户端凭据没变,可以立刻重连;要让他连不上请用 `/disable`,要让旧凭据彻底失效请用 `/rotate`。副服务器上的连接不受这个接口影响。

### POST /users/{name|id}/rotate

重置订阅链接:

- 订阅地址换成一串**新的随机令牌**(不管面板是否设置了用用户名作地址);
- 全部协议的凭据重新生成;
- 用户自助生成的临时共享地址一并收回;
- 旧地址、旧凭据、旧共享地址立即失效:主服务器热更新后断开已连接的设备,副服务器在下一次同步时同样断开旧凭据上的连接。

返回用户对象,`subLink` 等已是新地址,把它发给用户重新导入即可。

### POST /users/{name|id}/plan

给已有用户套用套餐(续费 / 延期)。`/renew` 是它的别名。套用套餐是明确的"续费"动作:不管之前因什么原因停用,套用后都置为启用(`disabledReason` 清空)。

请求体:

| 字段 | 说明 |
|---|---|
| `planId` / `plan` | 套餐 id 或名称,二选一,必填 |
| `mode` | `renew`(默认)或 `extend`,规则见下表 |

套用规则(创建用户时给 `plan` 走的是 `new`):

| 项目 | `new`(创建) | `renew`(续费,默认) | `extend`(延期) |
|---|---|---|---|
| 配额、设备数、限速、周期重置开关与天数 | 按套餐 | 按套餐 | 按套餐 |
| 到期 | 现在 + 套餐天数 | 原到期未过则**原到期 + 套餐天数**,否则现在 + 天数 | 同 renew |
| 本周期用量 | 从 0 起 | **清零**(并入历史累计) | **保留** |
| 周期重置日 | 现在 + 重置天数 | 现在 + 重置天数 | 当前周期未过则保留 |
| 线路 | 套餐指定了就按套餐,否则按请求 / 默认 | 套餐指定了就替换成套餐的,否则不动 | 同 renew |
| 启用状态 | 启用 | 启用 | 启用 |

套餐天数为 0 时到期改为不限。代理令牌下只能用自己的套餐,且套餐的线路必须在授权范围内。返回用户对象。

### GET /users/{name|id}/sub

订阅地址:

```json
{
  "link": "https://example.com:2056/sub/alice",
  "clash": "https://example.com:2056/sub/alice?format=clash",
  "json": "https://example.com:2056/sub/alice?format=json",
  "share": "https://example.com:2056/sub/<共享令牌>"
}
```

- `link`:通用链接订阅(Shadowrocket、Surge、Quantumult X、Loon、Karing 等);
- `clash`:Clash / Mihomo 系(Clash Verge、FlClash、Stash、Nextin、Hiddify);
- `json`:sing-box 远程配置(SFA / SFI);
- `share`:只在用户自己生成了临时共享地址时才有。

用户对象里的 `subLink` / `subClash` / `subJson` 与前三项相同。

## 5. 创建 / 修改的请求字段

全部可选(创建时 `name` 必填),没给的字段不改动:

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 用户名;不能为空,不能含空格与 `/ ? # &`(它可能是订阅地址的一部分),全站唯一 |
| `enabled` | bool | 启停;显式给 `false` 记为手动停用,之后重置 / 续期不会自动恢复 |
| `planId` / `plan` | number / string | 套餐 id 或名称,二选一。创建时按 `new` 套用;修改时按 `mode` 套用 |
| `mode` | string | 和套餐一起用:`renew`(默认)或 `extend`,规则见上表 |
| `volumeGb` / `volume` | number | 配额,GB 或字节;两者都给以 `volume` 为准;0 = 不限 |
| `days` | number | 创建:自现在起 N 天;修改:在原到期(未过期时)基础上再加 N 天;≤ 0 = 不限 |
| `expiry` | number | 到期时间戳(秒),优先于 `days`;0 = 不限 |
| `deviceLimit` | number | 同时在线设备数(按源 IP 计,跨服务器合并),0 = 不限 |
| `speedUp` / `speedDown` | number | 上行 / 下行限速 Mbps,该用户全部连接共享,0 = 不限 |
| `autoReset` / `resetDays` | bool / number | 周期重置用量:开启时 `resetDays` 必须 > 0;首次开启时下次重置日 = 现在 + 天数;关闭后重置日清零 |
| `remark` / `desc` | string | 备注 / 说明(备注会出现在列表搜索里) |
| `lineIds` | number[] | 可用线路(整条线路 = 它部署到的全部服务器,包括以后新加的) |
| `lineRefs` | `[{lineId, nodeIds}]` | 线路 × 服务器;`nodeIds` 省略或为空 = 该线路全部服务器。给了它就以它为准,`lineIds` 忽略 |
| `extIds` | number[] | 可用外部节点(只主面板令牌有效) |

优先级:显式字段 > 套餐。例如 `{"plan": "月付", "days": 60}` 先套用月付套餐,再把到期改为 60 天后;`{"plan": "月付", "lineIds": [1]}` 用套餐的配额但只给线路 1。

数值字段(配额、到期、设备数、限速)为负返回 400。

### 线路分配的两种写法

- `lineIds: [1, 2]`:整条线路 1 和 2,部署到几台服务器就有几个入口,以后线路加到新服务器也自动包含。
- `lineRefs: [{"lineId": 1}, {"lineId": 2, "nodeIds": [3]}]`:线路 1 全部服务器;线路 2 只要 3 号服务器上的入口。

服务端会整理:同一线路多条合并、没部署该线路的服务器忽略、勾满了全部服务器就收成"全部"。用户对象里两个字段都会返回,`lineIds` 是 `lineRefs` 去掉服务器后的形式,兼容老程序。

## 6. 用户对象

```json
{
  "id": 12, "name": "alice", "enabled": true, "disabledReason": "",
  "volume": 107374182400, "used": 1234567, "up": 100, "down": 1234467,
  "totalUp": 0, "totalDown": 0,
  "expiry": 1760000000, "expired": false,
  "autoReset": true, "resetDays": 30, "nextReset": 1758000000,
  "deviceLimit": 3, "speedUp": 0, "speedDown": 0,
  "remark": "订单 #1001", "desc": "",
  "createdAt": 1756000000, "onlineAt": 1756800000,
  "onlineIps": ["203.0.113.9"],
  "onlineLines": {"203.0.113.9": ["香港1-主机", "香港1-高带宽"]},
  "lineIds": [1, 2], "lineRefs": [{"lineId": 1}, {"lineId": 2, "nodeIds": [3]}], "extIds": [],
  "subLink": "https://example.com:2056/sub/alice",
  "subClash": "https://example.com:2056/sub/alice?format=clash",
  "subJson": "https://example.com:2056/sub/alice?format=json"
}
```

| 字段 | 说明 |
|---|---|
| `id`、`name`、`enabled` | 用户 id、用户名、是否启用 |
| `disabledReason` | 停用原因:`manual`(手动 / 显式 `enabled: false`)、`quota`(超量)、`expired`(到期);启用时为空 |
| `volume` | 配额(字节),0 = 不限 |
| `used` | 本周期已用 = `up + down` |
| `up` / `down` | 本周期上行 / 下行(字节),所有服务器合计,按各服务器的流量倍率计入 |
| `totalUp` / `totalDown` | 历史累计:每次重置(周期重置、`/reset`、`renew`)前的用量并入这里 |
| `expiry` / `expired` | 到期时间戳(0 = 不限)/ 是否已过期 |
| `autoReset` / `resetDays` / `nextReset` | 周期重置开关 / 天数 / 下次重置时间戳(0 = 未开启) |
| `deviceLimit` | 同时在线设备数上限,0 = 不限 |
| `speedUp` / `speedDown` | 限速 Mbps,0 = 不限 |
| `remark` / `desc` | 备注 / 说明 |
| `createdAt` / `onlineAt` | 创建时间 / 最近一次有流量的时间(0 = 从未在线) |
| `onlineIps` | 当前在线的源 IP(所有服务器合并去重) |
| `onlineLines` | 每个在线 IP 正在使用的线路,名字带服务器后缀 `线路名-服务器名`;没人在线时省略 |
| `lineIds` / `lineRefs` | 可用线路 / 线路 × 服务器,见上节 |
| `extIds` | 可用外部节点 id |
| `subLink` / `subClash` / `subJson` | 订阅地址(通用 / Clash / sing-box) |

用户对象不含任何凭据;临时共享地址只在 `/sub` 里返回。

## 7. 套餐对象

```json
{
  "id": 3, "resellerId": 0, "name": "月付",
  "volumeGb": 100, "days": 30, "deviceLimit": 3, "speedUp": 0, "speedDown": 0,
  "autoReset": true, "resetDays": 30,
  "lineIds": [1, 2], "lineNodes": {"2": [3]},
  "desc": "100 GB / 30 天", "sort": 1
}
```

| 字段 | 说明 |
|---|---|
| `resellerId` | 0 = 主面板的套餐;否则是该代理自己建的 |
| `volumeGb` / `days` | 配额(GB)/ 天数,0 = 不限 |
| `deviceLimit`、`speedUp`、`speedDown`、`autoReset`、`resetDays` | 套用后写入用户的对应字段 |
| `lineIds` | 套餐指定的线路;没有这个字段 = 套用时不改动用户线路 |
| `lineNodes` | 线路 → 服务器范围,如 `{"2": [3]}` 表示线路 2 只给 3 号服务器;没写的线路 = 全部服务器 |
| `desc` / `sort` | 说明 / 排序 |

套餐只能在面板里增删改,接口只读。

## 8. 示例

### 主面板令牌

```bash
BASE="https://panel.example.com:2053/app/api/v1"
TOKEN="你的令牌"
H=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json")

# 连通性
curl -s "${H[@]}" "$BASE/ping"

# 套餐列表
curl -s "${H[@]}" "$BASE/plans"

# 按套餐名开号(30 天、100 GB 之类都由套餐决定),备注写订单号
curl -s -X POST "${H[@]}" -d '{"name":"alice","plan":"月付","remark":"订单 #1001"}' "$BASE/users"

# 不用套餐,直接给参数:200 GB、90 天、3 台设备、只给线路 1 和线路 2 在 3 号服务器上的入口
curl -s -X POST "${H[@]}" \
  -d '{"name":"bob","volumeGb":200,"days":90,"deviceLimit":3,"lineRefs":[{"lineId":1},{"lineId":2,"nodeIds":[3]}]}' "$BASE/users"

# 查详情 / 搜索 / 只看停用的
curl -s "${H[@]}" "$BASE/users/alice"
curl -s "${H[@]}" "$BASE/users?q=订单"
curl -s "${H[@]}" "$BASE/users?enabled=false"

# 续费:再套一次套餐(用量清零、到期顺延)
curl -s -X POST "${H[@]}" -d '{"plan":"月付","mode":"renew"}' "$BASE/users/alice/plan"

# 延期但保留用量
curl -s -X POST "${H[@]}" -d '{"plan":"月付","mode":"extend"}' "$BASE/users/alice/plan"

# 只改到期:再加 7 天;只改备注
curl -s -X PATCH "${H[@]}" -d '{"days":7}' "$BASE/users/alice"
curl -s -X PATCH "${H[@]}" -d '{"remark":"订单 #1002"}' "$BASE/users/alice"

# 停用 / 启用 / 补量清零 / 踢下线
curl -s -X POST "${H[@]}" "$BASE/users/alice/disable"
curl -s -X POST "${H[@]}" "$BASE/users/alice/enable"
curl -s -X POST "${H[@]}" "$BASE/users/alice/reset"
curl -s -X POST "${H[@]}" "$BASE/users/alice/kick"

# 订阅地址泄露:重置订阅链接与凭据,返回的对象里就是新地址
curl -s -X POST "${H[@]}" "$BASE/users/alice/rotate"

# 取订阅地址 / 删除
curl -s "${H[@]}" "$BASE/users/alice/sub"
curl -s -X DELETE "${H[@]}" "$BASE/users/alice"
```

### 代理令牌

```bash
BASE="https://panel.example.com:2054/dl/api/v1"
TOKEN="代理的令牌"
H=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json")

# ping 会标明 role=reseller
curl -s "${H[@]}" "$BASE/ping"

# 只列得到自己建的套餐
curl -s "${H[@]}" "$BASE/plans"

# 开号:不指定线路就分配主面板授权给他的全部线路(含服务器范围)
curl -s -X POST "${H[@]}" -d '{"name":"dl-001","plan":"代理月付"}' "$BASE/users"

# 授权收窄到了某台服务器的线路,要用 lineRefs 指定那台;给整条会被 400 拒绝
curl -s -X POST "${H[@]}" -d '{"name":"dl-002","volumeGb":50,"days":30,"lineRefs":[{"lineId":2,"nodeIds":[3]}]}' "$BASE/users"

# 名下用户照常续费 / 停用 / 重置订阅链接;主面板的用户一律 404
curl -s -X POST "${H[@]}" -d '{"plan":"代理月付"}' "$BASE/users/dl-001/plan"
curl -s -X POST "${H[@]}" "$BASE/users/dl-001/rotate"
```

### 常见错误

| 返回 | 原因 |
|---|---|
| 401 `外部 API 未开启或令牌错误` | 开关没开、令牌不对、令牌用错入口、代理已停用 / 到期 |
| 400 `缺少 name` | 创建时没给用户名 |
| 400 `用户名已存在` | 用户名全站重复(包括别的代理的用户) |
| 400 `用户名不能包含空格或 / ? # &` | 用户名字符不合法 |
| 400 `套餐不存在: ...` | 套餐 id / 名称不在这个令牌能看到的范围内 |
| 400 `含未授权的线路` / `含未授权的服务器` | 代理令牌给了授权外的线路或服务器 |
| 400 `线路 #N 只授权了部分服务器,不能分配全部` | 代理令牌给整条线路,但授权只有其中几台;改用 `lineRefs` |
| 400 `已达到用户数上限 N,不能再建` | 主面板给代理设的用户数上限 |
| 400 `代理流量已用尽` | 代理名下用户的全时用量已达代理额度 |
| 400 `autoReset 需要 resetDays > 0` | 开周期重置没给天数 |
| 400 `数值字段不能为负` | 配额 / 到期 / 设备数 / 限速为负 |
| 404 `用户不存在` | 用户名和 id 都匹配不到,或不在代理名下 |
| 404 `接口不存在` | 路径不对 |
| 405 `方法不允许` | 方法不对 |

## 9. 注意

- 令牌等同该入口的全部权限:主面板令牌能改任何用户,代理令牌能改该代理的全部用户。只放在服务端程序里。
- 创建、修改、套餐、启停、重置都会热更新数据面,不会断开无关用户的连接;副服务器在下一次同步(≤ 5 秒)后生效。
- 用户名作订阅地址的用户(主面板默认)改名后订阅地址随之变化;要换地址又不改名,用 `/rotate`。
- 接口没有分页:用户量很大时请缓存 `/users` 的结果,或用 `?q=` 缩小范围。

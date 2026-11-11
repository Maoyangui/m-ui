# 外部 API(v1)

供商城、计费系统、Telegram 机器人等外部程序调用,与面板登录会话无关:凭令牌即可建号、续费(一次 N 期)、换套餐(立即替换 / 叠加 / 按价值折算)、启停、踢下线、重置订阅链接、取订阅地址。套餐操作都能先预览再执行,并支持幂等键,商城重试不会重复加时。

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
3. 以下任一情况令牌立即返回 401:代理在账号页关掉开关、代理被主面板停用、代理到期、主面板重置了代理密码。

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
- 数值上限:配额最多 2^62 字节(约 4.6 EB,即 4294967296 GB),叠加、续 N 期、折算算出来更大的按上限算;天数(`days`、`activateDays`、`resetDays`、套餐的天数与重置天数、批量延期)最多 3650000(约 1 万年),请求里给得更大返回 400。到期时间同样饱和计算,不会溢出。
- `{name|id}` 先按用户名精确匹配;匹配不到且形如数字时按用户 id。代理令牌下只在自己名下找,找不到一律 404。
- 失败返回 4xx 与 `{"error": "原因"}`(原因是中文,给人看的;判断请看状态码):

| 状态码 | 含义 |
|---|---|
| 400 | 请求体不是合法 JSON、字段不合法、名称重复、超出代理的授权 / 上限 / 额度、套餐不存在 |
| 401 | 外部 API 未开启、令牌错误、代理已停用 / 到期、用错入口 |
| 404 | 用户不存在(或不在代理名下)、接口不存在 |
| 405 | 方法不对(如对 `/enable` 发 GET) |
| 409 | 套餐操作:有影响要带 `force` 确认、`fromPlanId` 不符、幂等键已用于另一笔请求 |
| 429 | 同一来源(IPv6 按所在 /64)10 分钟内令牌错了 10 次,暂停 5 分钟(期间带对的令牌也是 429;两个入口共用一个计数)。只有对不上任何令牌才计数:令牌是对的、只是外部 API 开关关着、代理已停用 / 到期的,照常 401 但不计入。第一次失败和被暂停各记一条操作审计 |
| 503 | 正在换主机(设为主机进行中),写操作暂停,稍后重试 |

- 套餐操作(`/plan`、`/renew`、`/change`)的错误还带一个固定的 `code`(如 `bad_periods`、`needs_force`,见[常见错误](#常见错误)),程序请按 `code` 判断;其它接口的错误格式不变。
- 所有写操作记入面板"操作审计":主面板令牌的操作人显示为 `api`,代理令牌的显示为 `api:<代理名>`。套餐操作另记一条[套餐记录](#get-usersnameidplan-ops)。
- 接口只在**主服务器**上调用:副服务器的用户由主机下发,副机上的写操作(`/kick` 除外)一律返回 403。`/ping` 返回的 `role` 是 `node` 就说明调错了机器。套餐与套餐记录只在主机上(副机提升为主机后要重建套餐,之前的幂等键也不会跟过去;用面板上的「设为主机」或主机容灾接任换主机时,套餐、套餐记录与幂等键随数据一起过去)。
- **换主机期间**(管理员在面板上「设为主机」,通常一两分钟,最长按库的大小 15–60 分钟;提交结果不明时一直到截止或管理员放弃)写操作返回 **503** 与 `{"error": "正在把主机换到 …,写操作暂停…"}`,读照常;稍后重试。换完以后主机是另一台:用新主机的地址(同一个令牌,随数据一起迁过去)。主机容灾接任后同理。设为主机、接任、改主机容灾都不在外部 API 里,只能由登录的管理员在面板上发起(或服务器上的 root 用命令行),代理令牌、外部 API 令牌都触发不了。
- 令牌等同该入口的全部权限,只放在服务端程序里,不要写进网页前端或客户端。令牌泄露就到面板里重新生成一个(旧的立即失效)。
- 频率与安全:接口本身不限流,由调用方控制节奏(建号、续费这类操作按需调用即可,不要轮询 `/users` 拉全表做同步);面板端口建议只对调用方的 IP 放行或挂在反向代理后面。列表接口没有分页,用户很多时用 `q` / `enabled` 缩小范围。
- 用户的启停有三种原因:手动、超量、到期(用户对象里的 `disabledReason`)。自动停用的用户在 `/reset` 或周期重置后自动恢复;`/reset`、周期重置都不会恢复手动停用的用户。
- **`/plan`、`/renew`、`/change`(以及带了套餐的 `PATCH` / `PUT`)会启用手动停用的用户**:套用套餐是明确的续费 / 换套餐动作,不管之前因什么停用,一律置为启用、`disabledReason` 清空。不想让他恢复,就别对他调这些接口,或者在带套餐的 `PATCH` 里同时显式给 `enabled: false`。面板里续费 / 换套餐时会先提示「这个用户是手动停用的」,接口不提示(预览里有 `enables_manual`)。

### 幂等(重试安全)

续费、换套餐重试时最怕重复加时。套餐操作(`/plan`、`/renew`、`/change`,以及同时带了套餐的 `PATCH` / `PUT /users/{name|id}`)都可以带一个幂等键,建议直接用订单号:

- 写在请求体的 `requestId`,或请求头 `Idempotency-Key`(都给时以请求体为准)。最长 64 个字符,只能含字母、数字和 `. _ : -`,否则 400 `bad_request_id`。
- **同一个键、同样的内容**再发一次:不再执行,返回 200,响应和第一次是**同一个结构**,响应头带 `Idempotent-Replayed: true`:`/change` 返回第一次的结果对象(变更前后、折算、告警原样,`replayed: true`)加上当前的用户对象;`/plan`、`/renew` 和带套餐的 `PATCH` 返回当前的用户对象(第一次也是用户对象)。网络超时、拿不准成没成功时,用同一个键重试即可。
- 重放排在找套餐、校验期数、代理校验之前:第一次已经成功的请求,之后套餐改了名或删了、天数改了、代理额度用完了,同键同内容重试照样拿到 200 重放,不会变成 4xx。
- **同一个键、不同的内容**(套餐的写法、方式、期数、用量处理、`fromPlanId`、`force`、`remark` 任一不同,或换了用户 / 接口):409 `idempotency_conflict`。指纹按请求里的写法算:`{"plan":"月付"}` 和 `{"planId":3}` 即使是同一个套餐也算不同的内容;不带套餐(续当前套餐)本身就是一种写法。
- 只记成功的写操作:预览(`dryRun`)、4xx、409 都不占用键,改好之后可以用同一个键再试。
- 键的作用域是调用方:主面板(面板与主令牌共用)和每个代理(代理面板与他的令牌共用)各自一套,互不冲突。键随套餐记录保留 365 天;记录条数按作用域各自封顶,别的代理刷再多记录也挤不掉你的键。
- 同一个键的并发请求会排队执行:只有一个真正生效,其余都是重放。

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
| 换套餐(`/change`)的目标套餐 | 按**用户的归属**找:主面板用户找主面板的套餐,代理用户找那个代理的套餐 | 只能换成自己的套餐,套餐带的线路要在授权内;额度用尽时返回 400 `reseller_denied`。代理停用 / 到期时令牌本身就失效,所有接口都是 401(见[鉴权](#代理面板));代理面板里操作返回 400 `reseller_denied` |
| 别人的套餐 | — | 别人的套餐 id 和不存在的 id 返回同一个 400 `plan_not_found`;用户现在的套餐是主面板给的,预览和套餐记录里不显示它的名字 |
| 按价值折算的参考价 | 套餐自己的参考价 | 用自己套餐的参考价 |
| 预览里的额度信息 | 代理用户也带 | 带 `pool`(代理的流量额度、已用、剩余)和 `reseller_pool_low` / `reseller_expires_first` 提示;套餐记录里主面板的操作人显示为 `主面板` |

续费、换套餐对代理额度**不预扣、不退还**:代理额度按名下用户的全时用量算,续费 / 换套餐只是把本期用量挪进历史累计,额度不变;流量照常按实际使用扣。

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
| POST | `/users/{name\|id}/kick` | 踢下线(所有服务器) | `{closed, sessions, failed, servers}` |
| POST | `/users/{name\|id}/rotate` | 重置订阅链接与凭据 | 用户对象 |
| POST | `/users/{name\|id}/plan` | 续费 / 延期 N 期(不给套餐就续当前套餐),可预览、可幂等 | 用户对象(预览时是结果对象) |
| POST | `/users/{name\|id}/renew` | 同 `/plan`(别名) | 同上 |
| POST | `/users/{name\|id}/change` | 换套餐:立即替换 / 叠加 / 按价值折算,可预览、可幂等 | [结果对象](#结果--预览对象) |
| GET | `/users/{name\|id}/plan-ops` | 套餐记录(续费、换套餐的历史) | 记录数组 |
| GET | `/users/{name\|id}/sub` | 订阅地址 | `{link, clash, json[, share]}` |

面板里的管理员页与代理面板「我的账号」页有同一份接口一览(没开外部 API 时折叠显示)。

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
- 给了 `planId` / `plan` 就先按 `mode` 套用套餐(一期),再用其它显式字段覆盖。这等同一次续费,和 `/plan` 走同一个执行器:在事务里重新读用户再算、记一条[套餐记录](#get-usersnameidplan-ops)(`op` 为 `renew` / `extend`,审计仍是 `update:plan:<套餐名>`),认 `requestId` / `Idempotency-Key`([幂等](#幂等重试安全),重放返回当前的用户对象)。请求与返回的格式不变。没带套餐的修改不记套餐记录,也不看幂等键。
- `days` 在修改时的语义是"再加 N 天":原到期未过就从原到期算,已过期或不限就从现在算。
- 凭据、累计流量、订阅令牌不经由这个接口改动(要换凭据用 `/rotate`,要清用量用 `/reset`)。

### DELETE /users/{name|id}

删除用户:断开其全部连接,删除线路与外部节点分配,数据面热更新。代理令牌下,该用户的全时用量会先结转到代理头上(删号不能洗额度)。返回 `{"ok": true}`。

### POST /users/{name|id}/enable · /disable

- `enable`:`enabled = true`、`disabledReason = ""`,数据面热更新后客户端立即可用。对超量或到期的用户调它也会启用,但下一分钟的判定会再次停用;先 `/reset` 或 `/plan`。
- `disable`:`enabled = false`、`disabledReason = "manual"`,同时断开该用户在主服务器上的连接;副服务器在下一次同步(≤ 5 秒)时把他撤下并断开。手动停用的用户不会被 `/reset`、周期重置自动恢复;但 `/plan`、`/renew`、`/change` 会启用他(见[通用约定](#2-通用约定))。

都返回更新后的用户对象。

### POST /users/{name|id}/reset

本周期用量清零:`up` / `down` 并入 `totalUp` / `totalDown` 后归零;因超量被停用的用户(`disabledReason = "quota"`)重新启用(常用于"超量停用后补量"),手动停用或已到期的保持原状。不改到期、不改周期重置日。返回用户对象。

### POST /users/{name|id}/kick

断开该用户在**所有服务器**上的全部连接:主服务器本机立刻断,同时并发派发到每台副服务器(每台最多等 3 秒)。hysteria2 / TUIC / AnyTLS 这些"一条会话里开很多子连接"的协议会把整条会话一起关掉,客户端不能靠已鉴权的会话继续开新流。返回:

```json
{
  "closed": 5,
  "sessions": 2,
  "failed": 1,
  "servers": [
    {"local": true, "closed": 3, "sessions": 1},
    {"id": 2, "name": "hk", "closed": 2, "sessions": 1},
    {"id": 3, "name": "tw", "closed": 0, "sessions": 0, "error": "副机版本过旧,没有踢线接口", "outdated": true}
  ]
}
```

- `closed`:断开的连接数,所有服务器合计;`sessions`:关掉的整条会话数(hysteria2 / TUIC / AnyTLS),合计;`failed`:没派发成功的副服务器数。
- `servers`:每台的明细。`local: true` 是主服务器本机;副服务器带 `id` / `name`。部分副服务器失联仍返回 200,看 `failed` 与各项的 `error`;`outdated: true` 表示那台副服务器版本过旧(还没有踢线接口),升级后即可;`unconfigured: true` 表示那台副服务器没填 API 地址或令牌,没法派发。
- 代理令牌调用时 `error` 只给 `failed`,不含副服务器地址与错误原文。

客户端凭据没变,可以立刻重连;要让他连不上请用 `/disable`,要让旧凭据彻底失效请用 `/rotate`。停用 / 删除 / 重置链接不走这个派发:它们改的是用户表,快照 5 秒内推到副服务器,副服务器热换用户表时自己关掉被移除或换了凭据的会话。

### POST /users/{name|id}/rotate

重置订阅链接:

- 订阅地址换成一串**新的随机令牌**(不管面板是否设置了用用户名作地址);
- 全部协议的凭据重新生成;
- 用户自助生成的临时共享地址一并收回;
- 旧地址、旧凭据、旧共享地址立即失效:主服务器热更新后断开已连接的设备,副服务器在下一次同步时同样断开旧凭据上的连接。

返回用户对象,`subLink` 等已是新地址,把它发给用户重新导入即可。

### POST /users/{name|id}/plan

续费 / 延期,一次可以续 N 期。`/renew` 是它的别名。套用套餐是明确的"续费"动作:不管之前因什么原因停用,套用后都置为启用(`disabledReason` 清空),**手动停用的(`manual`)也会被启用**。

请求体(都可选;空请求体或 `{}` = 续当前套餐一期;请求体给了就要是 JSON 对象):

| 字段 | 说明 |
|---|---|
| `planId` / `plan` | 套餐 id 或名称,二选一。**请求里完全没有这两个字段才续用户现在的套餐**(用户没有所属套餐时 400 `plan_required`);带了字段却是 `0`、空串或 `null`,和以前一样 400 `plan_required`。续当前套餐时用的是执行那一刻的当前套餐:这期间被别的操作换了套餐,就续换过之后的那个,不会把用户改回旧套餐 |
| `mode` | `renew`(默认)或 `extend`,规则见下表;其它值按 `renew` 处理(兼容老调用方) |
| `periods` | 期数,1–120,默认 1;季付、年付按月计的套餐填 3、12。期数 × 套餐天数不超过 36500 |
| `dryRun` | `true` = 只算不改,返回[结果对象](#结果--预览对象);也可以写在查询串 `?dryRun=true` |
| `requestId` | 幂等键(也可以用请求头 `Idempotency-Key`),见[幂等](#幂等重试安全) |
| `fromPlanId` | 前置条件:用户现在的 `planId` 必须等于它(0 = 没有套餐),否则 409 `plan_changed` |
| `remark` | 只记进套餐记录,不改用户备注,最多 200 字 |

套用规则(创建用户时给 `plan` 走的是 `new`;N 期就是 N 倍的天数):

| 项目 | `new`(创建) | `renew`(续费,默认) | `extend`(延期) |
|---|---|---|---|
| 配额 | 按套餐 | 一次性额度(不周期重置、有上限)= **N × 套餐流量**;周期重置的是每期额度,不乘 N | 一期的套餐流量(不乘 N) |
| 设备数、限速、周期重置开关与天数 | 按套餐 | 按套餐 | 按套餐 |
| 到期 | 现在 + 套餐天数 | 原到期未过则**原到期 + N × 套餐天数**,否则现在 + N × 天数 | 同 renew |
| 本周期用量 | 从 0 起 | **清零**(并入历史累计) | **保留** |
| 周期重置日 | 现在 + 重置天数 | 现在 + 重置天数 | 当前周期未过则保留 |
| 线路 | 套餐指定了就按套餐,否则按请求 / 默认 | 套餐指定了就替换成套餐的,否则不动 | 同 renew |
| 启用状态 | 启用 | 启用(手动停用的也启用) | 同左 |

- 套餐天数为 0 时到期改为不限。从首次连接开始计时的套餐:还没开始计时的用户加到等待天数上;已过期或不限期的用户改成等首次连接(N × 天数)。
- 代理令牌下只能用自己的套餐,且套餐的线路必须在授权范围内;别人的套餐和不存在的套餐是同一个 400 `plan_not_found`(先核对归属,再校验期数)。
- 正式执行返回用户对象(和以前一样);幂等重放同样返回用户对象,带响应头 `Idempotent-Replayed: true`;`dryRun` 返回结果对象。续费不会因为影响(比如延期到更小的套餐后立即超量)被拦下,但预览里照样给出这些告警。
- 换了套餐会立刻重判"按套餐选人"的限速规则。

**续费建议**:月付这类**周期重置**套餐提前续费请用 `extend`。`renew` 会截断当前周期、立刻给满新的一期,结尾不满一个周期的零头还会再碰上一次重置 —— 提前 5 天续费,等于 35 天里拿到两期流量。已经超量或到期的用户才用 `renew`。

### POST /users/{name|id}/change

换套餐。`mode` 必填,没有默认值。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `planId` / `plan` | number / string | 必填 | 目标套餐,按**用户的归属**找:主面板用户找主面板的套餐,代理用户找那个代理的套餐(主令牌操作代理用户也一样;`/plan` 则按调用方的作用域找) |
| `mode` | string | 必填 | `replace` 立即替换 / `stack` 叠加 / `prorate` 按价值折算 |
| `periods` | int | 1 | 1–120;`prorate` 可以为 0;期数 × 套餐天数不超过 36500 |
| `usage` | string | `auto` | 本周期用量:`auto` / `keep`(保留,接着算)/ `reset`(清零,并入历史) |
| `force` | bool | false | 有要确认的影响(立即超量、到期提前)时必须为 true 才执行 |
| `dryRun` | bool | false | 只算不改;也可以 `?dryRun=true` |
| `requestId` | string | — | 幂等键(或请求头 `Idempotency-Key`) |
| `fromPlanId` | number | — | 前置条件:用户现在的 `planId` 必须等于它(0 = 没有套餐),否则 409 `plan_changed` |
| `remark` | string | — | 只记进套餐记录,最多 200 字 |

三种方式(N = 期数;目标套餐每期 D 天、V 流量、每 R 天重置):

| 项目 | `replace` 立即替换 | `stack` 叠加 | `prorate` 按价值折算 |
|---|---|---|---|
| 一句话 | 新套餐从现在开始算 N 期,旧订阅剩的时间和流量作废(要补偿就按结果里的 `remaining` 自己算钱) | 保留旧订阅剩的时间(和一次性流量)再加 N 期;已经过期的旧订阅没有可保留的,等于从现在开一份新的 | 旧订阅剩余的价值按两个套餐的**参考价**折成新套餐的时长,新套餐从现在开始算 N 期(N 可以为 0) |
| 到期 | 现在 + N×D;不限期的套餐 → 不限;首次连接计时的套餐 → 等首次连接,`activateDays` = N×D | 和续费一样:还没开始计时的加到等待天数上,其余从"现在与原到期中较晚者"顺延 N×D。目标不限期(D = 0)时,原来有限的到期(或等首次连接的天数)**保留不变**,提示 `expiry_kept`;原来就不限期、或已经过期的,按目标套餐变成不限期 | 现在 + N×D + 折算时长;首次连接计时的套餐:`activateDays` = N×D + 折算的整天数 |
| 配额 | 一次性额度:N×V;周期重置:V;V = 0 不限 | 一次性额度且原来有上限:`keep` 时 原配额 + N×V,`reset` 时 max(0, 原配额 − 已用) + N×V(用 SQL 原子结算,封顶见[通用约定](#2-通用约定));旧订阅**已过期**时剩的流量作废、按 0 结转(`keep`:已用 + N×V,`reset`:N×V),提示 `carry_expired`;原来不限量:N×V;周期重置:V(每期额度不叠加);目标不限量(V = 0)时原来有限的配额**保留不变**(`reset` 时配额 = 原来剩下的,至少 1 字节),提示 `volume_kept`,原来就不限量或已过期的变成不限 | 一次性额度:N×V + 折算流量;周期重置:V |
| `usage=auto` | 清零 | 保留(原来不限量时清零) | 清零 |
| 下次重置(周期重置套餐) | 清零:现在 + R;保留:当前周期没过就取 min(原重置日, 现在 + R) | 同左 | 同左 |
| 设备、限速、重置开关与天数、所属套餐 | 换成新套餐的 | 同左 | 同左 |
| 线路 | 新套餐指定了线路就替换,否则不动 | 同左 | 同左 |
| 启用 | 一律启用(手动停用的也启用) | 同左 | 同左 |
| 目标就是现在的套餐 | 可以:从现在重新开始 | 可以:等于带结转的续费 | 400 `same_plan` |

**周期重置套餐的本期用量**

- `keep`:当前周期继续,已用量按新配额算;重置日不往后推,新周期更短时提前到 现在 + R(换到 7 天重置的套餐不用再等原来的 30 天)。
- `reset`:已用量并入历史累计,新周期从现在开始。
- 叠加到周期重置套餐时流量不叠加,结果里有 `per_period_not_stacked` 提示,带上本期丢掉的剩余量。
- 降到每期额度更小的套餐又保留用量,可能立刻超量(`over_quota`,要 `force`)。三种处理:改用 `usage=reset` 直接给新的一期;带 `force` 让他本期停用到下次重置(大约 10 秒内的下一轮判定就会停);或者等 `nextReset` 之后再换。
- 已知限制:重置按"每 R 天"滚动,不按自然月或到期日对齐;年付(365 天)配每 30 天重置时,年末会剩一个 5 天的小周期。

**按价值折算(prorate)**

- 前提:用户有所属套餐 P(和用户同一归属),P 与目标套餐 Q 都填了**参考价**(面板 → 套餐 → 参考价),且 P ≠ Q。不满足时分别返回 400 `no_current_plan` / `no_price` / `same_plan`。没填参考价时面板不替你定价,只能用 `replace` / `stack`;想同价互换,就给两个套餐填相同的参考价。
- 旧订阅还值几期(ratio):
  - P 是一次性额度或不限量:时间比例 = 剩余时间 ÷ P 的天数;清零用量时再算流量比例 = 剩余流量 ÷ P 的流量;ratio 取两者较小的。保留用量时只按时间算(已用量会带进新套餐,不能再按流量算一次)。
  - P 是周期重置:本周期按剩余流量的比例折(只在清零用量时),之后的整期按时间:ratio = [(剩余时间 − 本期剩余时间) + min(本期剩余时间, 本期剩余流量比例 × R)] ÷ P 的天数。
  - 已经过期的订阅:剩余时间和流量都按 0 算,ratio = 0。
  - 按时间估不出(用户不限期,或 P 没有天数)、按流量也估不出(不限量,或保留用量时只按时间估值)时:400 `unmeasurable`。比如用户有配额但不限期,改用 `usage=reset` 就能按剩余流量折。
- 价值 = ratio × P 的参考价,折成 Q 的 c = 价值 ÷ Q 的参考价 期:
  - Q 有天数:折算时长 = c × Q 的天数(最多 3650 天);Q 是一次性额度时,一期 = D 天加 V 的流量,所以同时折 c × V 的流量;
  - Q 不限期但是一次性流量包:只折流量;
  - 其余 400 `unmeasurable`。
- Q 从首次连接开始计时:折算时长向下取整到整天,舍去的部分在 `credit_rounded` 提示里。0 期又折不满一天时直接给到期时间。
- 期数为 0 又折不出东西:400 `nothing_to_credit`。
- 这只是按**当前套餐定义**的估值:改过套餐参数、手工改过用户额度,结果都会跟着变。结果里的 `credit` 写明了每一步。

示例:月付(100 GB / 30 天 / 每 30 天重置,参考价 30)还剩 20 天,本期用了 30 GB,折算到高级月付(300 GB / 30 天,参考价 60),N = 1。本期剩 20 天、剩 70% 的流量 → min(20 天, 0.7 × 30 天) = 20 天 → ratio = 2/3 → 价值 20 → 1/3 期 → 折 10 天。结果:到期 = 现在 + 40 天,配额 300 GB 从 0 起。

**影响提示与护栏**

结果里的 `warnings` 是一组 `{code, text, args, block}`,`text` 是给人看的中文,程序按 `code` 判断:

| code | 要 `force` | 什么时候 | args |
|---|---|---|---|
| `over_quota` | 是 | 变更后有上限,保留的已用量已达到新配额(下一轮判定就会停用;新套餐周期重置的到 `nextReset` 恢复,不重置的要续费或清零用量) | `used`、`volume`、`nextReset`(0 = 不重置) |
| `expiry_earlier` | 是 | 变更后的终点早于现在的终点超过 60 秒(包括不限期变成有期限、等首次连接的天数变少) | `from`(0 = 原来不限期)、`to` |
| `volume_lower` / `fewer_devices` / `slower` | 否 | 配额 / 设备数 / 限速变少 | `from`、`to`(限速是 `[上行, 下行]`) |
| `lines_removed` | 否 | 套餐会替换线路,少了几条 | `lineIds` |
| `per_period_not_stacked` | 否 | 叠加到周期重置套餐 | `lost`(本期丢掉的旧剩余字节;过期作废的算在 `carry_expired` 里) |
| `carry_unlimited` | 否 | 叠加:原来不限量、新套餐有上限,没有可结转的,新配额就是新套餐给的 N 期流量(保留用量时已用量照算) | `volume`(新配额) |
| `carry_expired` | 否 | 叠加:旧订阅已经过期,剩下的流量随之作废,不结转 | `lost`(作废的字节) |
| `expiry_kept` | 否 | 叠加到不限期的套餐:原来有限的到期保留,不变成不限(要不限请用 `replace`) | `expiry`、`activateDays`(原来的到期 / 等首次连接的天数) |
| `volume_kept` | 否 | 叠加到不限量的套餐:原来有限的配额保留,不变成不限(要不限请用 `replace`) | `left`(原来剩下的字节) |
| `credit_rounded` | 否 | 折算到首次连接才计时的套餐,舍去不足一天的部分 | `seconds` |
| `enables_manual` | 否 | 手动停用的用户会被启用 | — |
| `reseller_pool_low` | 否 | 代理用户:变更后用户剩的配额多于代理剩余的流量额度 | `remaining`(null = 不限)、`pool` |
| `reseller_expires_first` | 否 | 代理用户:新到期晚于代理自己的到期 | `resellerExpiry` |

- 要 `force` 的只在 `/change` 正式执行时生效:没带 `force: true` 返回 409 `needs_force`,响应里附带 `warnings` 与完整的 `preview`(结果对象),确认后带 `force: true` 重试(可以用同一个幂等键)。
- `/plan`、`/renew` 为了兼容不拦,预览里照样给出这些提示。
- 不主动踢线;设备数下调只影响新连接。

**返回**:200,正式执行与幂等重放都是结果对象,外加 `user`(用户对象,与 `GET /users/{name|id}` 相同);`dryRun` 时 `dryRun: true`,没有 `opId` 与 `user`。需要确认时 409:

```json
{"error": "这次变更有需要确认的影响(见 warnings),确认后带 force: true 重试", "code": "needs_force", "warnings": [ … ], "preview": { …结果对象… }}
```

老版本面板对这个路径返回 404 `接口不存在`,调用方可以据此判断面板是否支持换套餐。

### 结果 / 预览对象

`/change` 的返回,以及 `/plan`、`/renew` 带 `dryRun` 时的返回:

```json
{
  "op": "stack", "direction": "upgrade", "periods": 1, "usage": "keep",
  "dryRun": false, "replayed": false, "opId": 42, "requestId": "order-1001",
  "from": {"planId": 3, "plan": "月付", "volume": 107374182400, "used": 32212254720, "expiry": 1795000000, "activateDays": 0,
           "autoReset": true, "resetDays": 30, "nextReset": 1795000000, "deviceLimit": 3, "speedUp": 0, "speedDown": 0,
           "enabled": true, "disabledReason": "", "lineRefs": [{"lineId": 1}]},
  "to":   {"planId": 5, "plan": "高级月付", "volume": 322122547200, "used": 32212254720, "expiry": 1797592000, "…": "字段同上"},
  "remaining": {"seconds": 1728000, "bytes": 75161927680, "timeRatio": 0.667, "trafficRatio": 1, "ratio": 0.667, "value": 20},
  "credit": null,
  "pool": {"volume": 0, "used": 0, "remaining": 0},
  "warnings": [{"code": "per_period_not_stacked", "text": "周期重置套餐的流量是每期额度,叠加只延长时间", "args": {"lost": 75161927680}}],
  "needsForce": false,
  "user": {"…": "用户对象(只在正式执行和重放时返回)"}
}
```

| 字段 | 说明 |
|---|---|
| `op` | `renew` / `extend` / `replace` / `stack` / `prorate` |
| `direction` | 按每 30 天流量、设备数、上下行限速比较(0 视为不限):`upgrade` / `downgrade` / `same` / `mixed`;只用于展示与审计 |
| `usage` | 实际的用量处理:`keep` / `reset`(`auto` 已解析) |
| `from` / `to` | 变更前 / 后的套餐状态;`to.used` 为 0 表示本期用量已清零 |
| `remaining` | 旧订阅还剩的秒数、字节,以及按旧套餐折成的份额(`ratio` = 还值几期;`null` = 不限或无从估算;已过期的订阅剩余都按 0 算);`value` = ratio × 旧套餐参考价,只有旧套餐填了参考价才有,商城可以据此按自己的价目表算差价,再调 `replace` |
| `credit` | `prorate` 的折算明细:`{fromPrice, toPrice, ratio, value, periods, seconds, bytes}`;其它方式为 `null` |
| `pool` | 只有代理用户才有:代理的流量额度、已用(全时用量)、剩余;`volume` 0 = 不限 |
| `warnings` / `needsForce` | 影响提示;`needsForce` 为真表示有要 `force` 的影响 |
| `opId` / `requestId` / `replayed` | 套餐记录 id、幂等键、是否重放 |

预览算出的数字以预览那一刻为准;预览到执行之间产生的流量会让 `used` 有几 MB 的出入 —— 正式执行在事务里重新读取用户再计算。

### GET /users/{name|id}/plan-ops

套餐记录(续费、换套餐的历史),最新的在前。`?limit=` 默认 20,最多 100。

```json
[{"id": 42, "createdAt": 1793000000, "actor": "api:代理A", "userId": 12, "userName": "alice",
  "op": "stack", "usage": "keep", "direction": "upgrade", "periods": 1,
  "fromPlanId": 3, "fromPlan": "月付", "toPlanId": 5, "toPlan": "高级月付",
  "requestId": "order-1001", "remark": "", "before": {…}, "after": {…}, "remaining": {…}, "credit": null, "warnings": […]}]
```

- `before` / `after` 与结果对象里的 `from` / `to` 相同;套餐名随记录存一份,套餐以后改名或删了照样看得懂。
- 记录保留 365 天,条数按作用域各自封顶:主面板与每个代理各最多 10 万条,超出时只删那个作用域里最旧的。清空审计日志不影响它。
- 代理令牌下,不是这个代理自己(代理面板或他的令牌)操作的记录,`actor` 一律显示为 `主面板`;记录里不是这个代理的套餐(比如主面板给他的用户套了主面板的套餐)只留 id,`fromPlan` / `toPlan` 与 `before.plan` / `after.plan` 为空。

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
| `enabled` | bool | 启停;显式给 `false` 记为手动停用,之后 `/reset`、周期重置不会自动恢复(套用套餐会启用,见 `/plan`;同一个请求里带套餐又给了 `false` 的,保持停用) |
| `planId` / `plan` | number / string | 套餐 id 或名称,二选一。创建时按 `new` 套用;修改时按 `mode` 套用 |
| `mode` | string | 和套餐一起用:`renew`(默认)或 `extend`,规则见上表 |
| `volumeGb` / `volume` | number | 配额,GB(可带小数,如 `0.1`)或字节;两者都给以 `volume` 为准;0 = 不限 |
| `days` | number | 创建:自现在起 N 天;修改:在原到期(未过期时)基础上再加 N 天;≤ 0 = 不限 |
| `expiry` | number | 到期时间戳(秒),优先于 `days`;0 = 不限 |
| `activateDays` | number | 首次连接后 N 天到期:给了就不看 `expiry` / `days`,第一次产生流量时才开始计时;0 = 不用 |
| `deviceLimit` | number | 同时在线设备数(按源 IP 计,跨服务器合并),0 = 不限 |
| `speedUp` / `speedDown` | number | 上行 / 下行限速 Mbps,该用户全部连接共享,0 = 不限 |
| `autoReset` / `resetDays` | bool / number | 周期重置用量:开启时 `resetDays` 必须 > 0;首次开启时下次重置日 = 现在 + 天数;关闭后重置日清零 |
| `remark` / `desc` | string | 备注 / 说明(备注会出现在列表搜索里) |
| `lineIds` | number[] | 可用线路(整条线路 = 它部署到的全部服务器,包括以后新加的) |
| `lineRefs` | `[{lineId, nodeIds}]` | 线路 × 服务器;`nodeIds` 省略或为空 = 该线路全部服务器。给了它就以它为准,`lineIds` 忽略 |
| `extIds` | number[] | 可用外部节点(只主面板令牌有效) |
| `requestId` | string | 幂等键(也可以用请求头 `Idempotency-Key`):只有修改时同时带了 `planId` / `plan` 才认,见[幂等](#幂等重试安全) |

优先级:显式字段 > 套餐。例如 `{"plan": "月付", "days": 60}` 套用月付套餐,但到期按 60 天算、不用套餐自带的天数(创建时自现在起,修改时自原到期起,同 `days` 本身的规则);`{"plan": "月付", "lineIds": [1]}` 用套餐的配额但只给线路 1。

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
  "activeLimits": [{"rule": "晚高峰", "upMbps": 0, "downMbps": 30, "since": 1758000000, "until": 0, "reason": "时段 19:00 到 23:00"}],
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
| `planId` | 所属套餐 id,0 = 没有 |
| `activateDays` | 还没开始计时的"首次连接后 N 天";开始计时后变回 0,到期看 `expiry` |
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
| `activeLimits` | 生效中的规则限速(只读,面板"规则"页配置):`rule` 规则名、`upMbps` / `downMbps`(0 = 该方向不改)、`since`、`until`(0 = 时段规则,随时段结束)、`reason`;没有时省略。只给站长的系统看,用户落地页不显示 |
| `subLink` / `subClash` / `subJson` | 订阅地址(通用 / Clash / sing-box) |

用户对象不含任何凭据;临时共享地址只在 `/sub` 里返回。

## 7. 套餐对象

```json
{
  "id": 3, "resellerId": 0, "name": "月付",
  "volumeGb": 100, "days": 30, "deviceLimit": 3, "speedUp": 0, "speedDown": 0,
  "autoReset": true, "resetDays": 30,
  "lineIds": [1, 2], "lineNodes": {"2": [3]},
  "desc": "100 GB / 30 天", "sort": 1, "price": 30
}
```

| 字段 | 说明 |
|---|---|
| `resellerId` | 0 = 主面板的套餐;否则是该代理自己建的 |
| `volumeGb` / `days` | 配额(GB,可带小数)/ 天数,0 = 不限 |
| `firstUse` | true = 套用后从首次连接开始计时(已经在计时的用户续费照常顺延) |
| `deviceLimit`、`speedUp`、`speedDown`、`autoReset`、`resetDays` | 套用后写入用户的对应字段 |
| `lineIds` | 套餐指定的线路;没有这个字段 = 套用时不改动用户线路 |
| `lineNodes` | 线路 → 服务器范围,如 `{"2": [3]}` 表示线路 2 只给 3 号服务器;没写的线路 = 全部服务器 |
| `desc` / `sort` | 说明 / 排序 |
| `price` | 参考价:只用于换套餐时按剩余价值折算(`prorate`)和预览里估值,不收钱,也不给终端用户看;0 = 没填(不能折算),填了要在 0.01 到 1000000000 之间。老版本升级上来的套餐都是 0;按老格式修改套餐(请求里没有 `price`)时保留原来的参考价 |

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

# 延期但保留用量(月付这类周期重置套餐提前续费用它)
curl -s -X POST "${H[@]}" -d '{"plan":"月付","mode":"extend"}' "$BASE/users/alice/plan"

# 续当前套餐 3 期(季付),带订单号做幂等:重试不会重复加时
curl -s -X POST "${H[@]}" -H "Idempotency-Key: order-1004" -d '{"periods":3,"mode":"extend"}' "$BASE/users/alice/renew"

# 预览:叠加升级到「高级月付」;确认后正式执行(fromPlanId 防止这期间别人已经改过)
curl -s -X POST "${H[@]}" -d '{"plan":"高级月付","mode":"stack","dryRun":true}' "$BASE/users/alice/change"
curl -s -X POST "${H[@]}" -H "Idempotency-Key: order-1002" -d '{"plan":"高级月付","mode":"stack","fromPlanId":3}' "$BASE/users/alice/change"

# 从现在起换成年付,按参考价把旧订阅剩的价值折成时长
curl -s -X POST "${H[@]}" -d '{"plan":"年付","mode":"prorate","requestId":"order-1005"}' "$BASE/users/alice/change"

# 降级会立即超量:改成清零本期(或者看清影响后带 "force":true)
curl -s -X POST "${H[@]}" -d '{"plan":"轻量","mode":"replace","usage":"reset","requestId":"order-1003"}' "$BASE/users/alice/change"

# 套餐记录
curl -s "${H[@]}" "$BASE/users/alice/plan-ops?limit=10"

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

# 只能换成自己的套餐;预览里带代理额度(pool)
curl -s -X POST "${H[@]}" -d '{"plan":"代理高级月付","mode":"stack","dryRun":true}' "$BASE/users/dl-001/change"
```

### 常见错误

| 返回 | 原因 |
|---|---|
| 401 `外部 API 未开启或令牌错误` | 开关没开、令牌不对、令牌用错入口、代理已停用 / 到期 |
| 429 `失败次数过多,请 5 分钟后再试` | 这个来源 10 分钟内令牌错了 10 次,5 分钟后再试;检查调用方配的令牌 |
| 400 `请求体不是合法 JSON: 请求内容过大,超出上限` | 请求体超过 1 MB |
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
| 400 `天数不能超过 3650000(约 1 万年)` | `days` / `activateDays` / `resetDays`(或套餐的天数)超过上限 |
| 400 `请求体要是 JSON 对象` | 套餐操作的请求体给了,但不是 `{…}`(比如 `null`、数组) |
| 400 `plan_required` | `/change` 没给套餐;`/plan`、`/renew` 没给套餐而用户也没有所属套餐;带了 `planId` / `plan` 字段却是 0、空串或 `null` |
| 400 `plan_not_found` | 套餐不在用户的归属(`/change`)或调用方的作用域(`/plan`)里;代理拿别人的套餐和拿不存在的套餐是同一个错误 |
| 400 `bad_mode` / `bad_periods` / `bad_usage` / `bad_request_id` | `mode` 只能是 replace / stack / prorate;期数 1–120(折算可以 0)且总天数不超过 36500;`usage` 只能是 auto / keep / reset;幂等键最长 64 个字符,只能含字母、数字和 `. _ : -` |
| 400 `no_current_plan` / `same_plan` / `no_price` / `unmeasurable` / `nothing_to_credit` | 按价值折算的前提不满足,见[按价值折算](#post-usersnameidchange) |
| 400 `reseller_denied` | 代理令牌:代理流量已用尽,或套餐的线路超出授权(原因在 `error` 里)。代理停用 / 到期时令牌在鉴权这一步就失效,返回的是 401;在代理面板里操作才会因停用 / 到期得到这个 400 |
| 409 `needs_force` | 换套餐有要确认的影响(立即超量、到期提前),看清 `warnings` / `preview` 后带 `force: true` 重试 |
| 409 `plan_changed` | 用户现在的套餐和 `fromPlanId` 不符(这期间被别人改过);或续当前套餐时用户的套餐一直在被别的操作换(按最新的重试了几次仍对不上) |
| 409 `idempotency_conflict` | 这个幂等键已经用于另一笔内容不同的请求 |
| 404 `用户不存在` | 用户名和 id 都匹配不到,或不在代理名下 |
| 404 `接口不存在` | 路径不对 |
| 405 `方法不允许` | 方法不对 |

## 9. 事件推送(Webhook)

不想轮询的话,在**设置 → 通知**填 Webhook 地址,勾选的事件会 POST 过去:

```json
{"event":"login","text":"🔐 面板登录:admin\nIP:203.0.113.8\n2026-10-31 02:57:49","time":1793386669}
```

`event` 是事件类别(`login`、`userdisabled`、`userexpiring`、`quota`、`upstream`、`core`、`cert`、`reach`、`rulelimit`、`daily`,测试按钮发的是 `test`),`text` 是和 Telegram 通知同样的文字。填了密钥就带 `X-M-UI-Signature: sha256=<正文的 HMAC-SHA256 十六进制>`,收到后用同一个密钥算一遍比对。10 秒超时、不重试,返回 2xx 才算收到。

## 10. 注意

- 令牌等同该入口的全部权限:主面板令牌能改任何用户,代理令牌能改该代理的全部用户。只放在服务端程序里。
- 创建、修改、套餐、启停、重置都会热更新数据面,不会断开无关用户的连接;副服务器在下一次同步(≤ 5 秒)后生效。换套餐只改额度和到期,不主动踢线;设备数下调只影响新连接。
- 用户名作订阅地址的用户(主面板默认)改名后订阅地址随之变化;要换地址又不改名,用 `/rotate`。
- 接口没有分页:用户量很大时请缓存 `/users` 的结果,或用 `?q=` 缩小范围。

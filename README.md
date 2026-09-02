# m-ui

内嵌 [sing-box](https://github.com/SagerNet/sing-box) 的多服务器代理面板。**单二进制、单数据库文件**,主/副角色一键切换,面板内完成证书、WARP、备份、换机等全部运维。

> 数据面部分移植自 [alireza0/s-ui](https://github.com/alireza0/s-ui),并可从 s-ui 数据库一键导入(端口、凭据、订阅 URL 全部保留)。License: GPL-3.0。

## 特性

**线路与协议**
- 一条**线路** = 入口协议 + 端口 → 上游落地,一页管理(替代入站 / 出站 / 路由三张表)
- 入站协议:Hysteria2、AnyTLS、TUIC、Trojan、VLESS(Reality / Vision)、VMess、Shadowsocks(含 2022)、SOCKS、HTTP、Mixed;传输层 WS / gRPC / HTTPUpgrade / HTTP
- 上游:VLESS / VMess / Trojan / TUIC / Hysteria2 / Shadowsocks / SOCKS,可视化编辑或粘贴分享链接导入;一键测试延迟、定时巡检、故障/恢复告警
- 保存前 sing-box 干跑校验:一条坏配置不会断掉所有用户

**用户与订阅**
- 配额、到期、周期重置、同时在线设备数(按源 IP)、上下行限速;超量/到期自动禁用并即时踢线
- 套餐模板:按套餐建号、续费、延期;批量生成、批量启停/延期/重置/删除、CSV 导出
- 订阅:`https://域名:端口/sub/用户名`(链接)与 `?format=clash`(mihomo / Clash / Shadowrocket / Stash);浏览器打开显示落地页(用量、到期、一键导入、二维码)
- 自签证书(无域名、纯 IP)时订阅自动带 `insecure` / `skip-cert-verify`

**双服务器**
- 主机把线路 / 上游 / 用户推送给每台副机,回收副机流量并集中判定配额;设备数按双机并集计算
- 订阅里每条线路按服务器各出一个节点,Clash 生成 url-test 组,客户端按延迟自动选入口
- 副机失联 / 恢复告警;副机证书各自签发

**运维**
- 证书页:Let's Encrypt 签发(HTTP-01 / Cloudflare DNS-01)、到期前自动续期、证书热更新无需重启、预检 DNS 与端口、自签
- 运维页:Cloudflare WARP 一键安装 / 启用(本地 SOCKS5,MASQUE)/ 停用 / 卸载并验证出口;swap、内核参数 + BBR、文件句柄上限、NTP,参数可按机器调整
- 备份页:一个 zip 含数据库一致快照与证书;上传即整机还原并自动重启;定时备份、轮转、推送到 Telegram
- Telegram 通知:登录、用户到期 / 超量 / 被禁用、上游故障、数据面停止、每日报告
- 审计日志、订阅访问日志、流量时序图、在线用户与 IP

## 安装

支持 Linux amd64 / arm64(systemd)。从 Releases 下载对应架构的二进制,然后:

```bash
bash deploy/install.sh ./m-ui-linux-amd64
```

`install.sh` 会把二进制装到 `/usr/local/bin/m-ui`,数据库放在 `/etc/m-ui/m-ui.db`,写入并启动 `m-ui.service`。首次启动会创建管理员 `admin` 并把随机密码打到日志:

```bash
journalctl -u m-ui -n 30 --no-pager | grep -E "初始密码|面板"
```

默认面板地址 `http://<IP>:2053/app/`,订阅入口 `:2056/sub/`。登录后按概览页的**快速开始**清单依次完成:域名 → 证书 → 线路 → 用户。

### 从 s-ui 迁移

```bash
bash deploy/install.sh ./m-ui-linux-amd64 --import /usr/local/s-ui/db/s-ui.db
```

导入保留全部入站端口、用户凭据、面板路径与订阅 URL,客户端无需改任何东西;导入报告会列出核对结果。

### 换机 / 还原

旧机备份页下载 zip(或 `m-ui backup -db /etc/m-ui/m-ui.db`),新机:

```bash
bash deploy/install.sh ./m-ui-linux-amd64 --restore m-ui-20260901-120000.zip
```

也可以在新机面板的备份页直接上传 zip,m-ui 会自动重启并整机还原(含证书)。之后把域名解析指向新机即可。

## 双服务器接入

1. 副机同样安装 m-ui,登录后在 **设置 → 角色** 打开"作为副服务器运行"
2. 副机 **设置 → 配对信息** 里复制 API 地址与令牌
3. 主机 **服务器** 页新增:名称(如"台湾",作为订阅节点后缀)、域名、API 地址、令牌
4. 几秒内主机完成推送并显示"已同步";用户订阅自动出现两组入口,Clash 每条线路自动选延迟低的一端

副机的线路 / 上游 / 用户完全由主机下发覆盖;副机自己只需要维护证书(证书页)与运维项。

## 命令行

```
m-ui run -db <m-ui.db>                    启动
m-ui import -from <s-ui.db> -to <m-ui.db> 从 s-ui 导入
m-ui backup -db <m-ui.db> -out <zip>      生成备份
m-ui restore -db <m-ui.db> -from <zip|db> 还原备份(服务停止时)
m-ui selfsign -hosts <域名,IP>            生成自签证书
m-ui render -db <m-ui.db>                 打印并校验 sing-box 配置
```

## 开发

需要 Go 1.22+。Reality 依赖 `with_utls` 构建标签,所有构建与测试都要带上:

```bash
make build          # 本机
make linux          # CGO_ENABLED=0 GOOS=linux 静态二进制
make test           # go test -tags with_utls ./...
```

前端是零构建的 ES modules(`web/assets/`),直接编辑刷新即可;内嵌资源按内容哈希生成 ETag,升级后浏览器自动拿到新文件。

目录结构:

```
core/      内嵌 sing-box 生命周期、流量统计、连接跟踪、限速与设备数
render/    线路 → sing-box 配置渲染与校验
sub/       订阅生成(链接 / clash)、落地页、订阅服务
hub/       主副机同步(快照推送、流量回收、在线聚合)
jobs/      统计、配额判定、清理定时任务
monitor/   上游巡检、看门狗、用户预警、日报
acme/      Let's Encrypt 签发(HTTP-01 / Cloudflare DNS-01)
backup/    备份与还原
ops/       WARP 与系统优化脚本
web/       面板 HTTP API 与前端
importer/  s-ui 数据库导入
```

## 常见问题

**没有域名能用吗?** 可以。证书页"生成自签证书"填服务器 IP,订阅会自动带允许不安全;有域名后再签发正式证书即可,客户端只需刷新订阅。

**改了端口 / 监听地址不生效?** 这些改动需要重启 m-ui(设置页"关于"里有重启按钮)。证书续期不需要重启。

**WARP 启用失败?** 运维页任务输出会给出原因;常见是 40000 端口被占或 Ubuntu 以外的系统。WARP 只作为本地 SOCKS5 上游,线路选择上游 `warp` 即可走 WARP 出口。

**副机显示离线?** 服务器页会显示原因(令牌错误 / 连接超时 / 证书校验失败)。副机面板用自签证书时勾选"跳过证书校验"。

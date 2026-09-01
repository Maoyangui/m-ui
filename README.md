# m-ui

内嵌 sing-box 的多服务器代理面板。单二进制、单数据库文件,主/副角色一键切换。

- **线路**抽象:一条线路 = 入口协议+端口 → 上游落地,一页管理(替代 入站+出站+路由 三张表)
- 双入口(HK/TW)配置自动同步,clash 订阅按线路生成 url-test 组,客户端按延迟自动选入口
- 用户配额/到期/月重置集中判定,流量双机聚合;单用户设备数上限与限速
- 面板内证书签发续期(ACME)、WARP 一键开关、系统优化
- 一键备份/还原/换机;从 s-ui 数据库一键导入(端口/凭据/订阅 URL 全保留)

```bash
m-ui import -from s-ui.db -to m-ui.db -order docs/line-order.txt -title maoyang
m-ui run
```

详见 [docs/PLAN.md](docs/PLAN.md)。License: GPL-3.0(数据面部分移植自 [alireza0/s-ui](https://github.com/alireza0/s-ui))。

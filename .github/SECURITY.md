# 安全策略 · Security policy

[中文](#中文) · [English](#english)

## 中文

### 支持的版本

只有[最新发布的版本](https://github.com/Maoyangui/m-ui/releases/latest)会收到安全修复。用旧版的请先升级(`m-ui update`,或面板里的一键更新),再看问题还在不在。

### 报告漏洞

**请不要在 Issue、Discussions 或群里公开漏洞细节。**

- 首选:在本仓库 [Security → Report a vulnerability](https://github.com/Maoyangui/m-ui/security/advisories/new) 私下提交,只有维护者看得到。
- 也可以在 Telegram 私信 [@yangweii](https://t.me/yangweii):先只说「有安全问题要报告」,细节等回复后再发。

请尽量写清:版本号、部署方式(单台,或主机 + 副机)、复现步骤、影响(能读到什么、能改什么),有修法的建议也欢迎一起写上。

### 之后会怎样

- 会尽快回复,确认问题,并商量什么时候公开。
- 修复随新版本发布,发布说明里写明修了什么;你愿意的话,会在里面致谢。
- 问题出在内嵌的 sing-box 本身的,也请同时报告给 sing-box 上游。

### 范围

面板、代理面板、订阅服务与落地页、多服务器同步与主机容灾、安装与更新脚本、更新包的签名校验,都在范围内。
自己服务器上的配置问题(弱密码、面板没开 HTTPS 却对公网开放等)不算漏洞,但欢迎提建议。

## English

### Supported versions

Only the [latest release](https://github.com/Maoyangui/m-ui/releases/latest) receives security fixes. If you run an older version, upgrade first (`m-ui update`, or the one-click update in the panel) and check whether the problem is still there.

### Reporting a vulnerability

**Please do not post vulnerability details in Issues, Discussions or group chats.**

- Preferred: report it privately through [Security → Report a vulnerability](https://github.com/Maoyangui/m-ui/security/advisories/new) in this repository. Only the maintainer can see it.
- Or message [@yangweii](https://t.me/yangweii) on Telegram. Say only that you have a security issue to report at first, and send the details after you get a reply.

Please include the version, how it is deployed (a single server, or a master with nodes), steps to reproduce and the impact (what can be read or changed). Suggestions for a fix are welcome too.

### What happens next

- You will get a reply as soon as possible to confirm the issue and agree on when to disclose it.
- The fix ships in a new release, and the release notes say what was fixed. If you like, you will be credited there.
- If the problem is in the bundled sing-box itself, please report it upstream to sing-box as well.

### Scope

The panel, the reseller panel, the subscription service and landing page, multi-server sync and master failover, the install and update scripts, and update-package signature checks are all in scope.
Configuration problems on your own server (weak passwords, a panel exposed to the internet without HTTPS, and so on) are not vulnerabilities, but suggestions are welcome.

// Package ha 主机容灾与「设为主机」。
//
//   - 设为主机:对某台副机执行,把主机的全部数据与主机身份(订阅地址相关的设置与证书)经现有的服务器链接加密推过去,
//     它以新纪元接手、所有副机改听它,原来的主机自动变为普通副机。计划内换主机、全新 VPS 接手、原主机回来后重新当主机,
//     都是这一种操作。
//   - 主机容灾:开启后按服务器顺序排在前面的几台副机(备用)保存加密的数据副本;主机失联(多方证据 + 时间阈值)时
//     由排第一的备用接任为主机(确认后接任,或自动接任),之后它就是主机,可以长期担任。原主机回来后自动变为副机。
//
// 两者都靠纪元(epoch)防脑裂:任何时刻副机只听一台主机。没开过容灾、也没换过主机的集群(纪元为 0)
// 主副机的请求、推送内容、修订号都和以前一样。
package ha

import "strings"

// 设置分两类:
//   - 本机设置:每台机器自己的监听、端口、路径、域名、证书、角色、同步状态、运维参数、容灾的本机状态。
//     不进副本;接任 / 设为主机时保留新主机自己的值,只有订阅地址相关的几项随主机身份迁移(见 identity.go)。
//   - 全局设置:其余全部(业务开关、通知、外部 API 令牌、两步验证、并账纪元……),随副本复制。
//
// 两张表都要显式列出:web 包的测试扫描源码里所有读写过的设置键,每个都必须归了类,新加设置时就得想清楚它属于哪边。

// localKeys 本机设置。
var localKeys = setOf(
	// 监听、端口、路径、域名、证书
	"webListen", "webPort", "webPath", "webDomain", "webCertFile", "webKeyFile",
	"certFile", "keyFile", "certSource",
	"subListen", "subPort", "subPath", "subCertFile", "subKeyFile", "subDomain", "subServerAddr", "subInsecure", "subRateLimit",
	"resellerListen", "resellerPort", "resellerPath", "resellerCertFile", "resellerKeyFile",
	// 本机探测值
	"publicIp", "publicIp6",
	// 角色、同步与待处理状态
	"nodeMode", "nodeToken", "agentLedgerEpoch", "userReloadPending", "secureReloadPending", "subRestartPending",
	// 运维参数
	"warpPort", "logEnabled", "coreLogLevel", "backupDir", "statsPingAt",
	// 容灾的本机状态
	KeyEpoch, KeyRole, KeyMasterId, KeyMasterPub, KeySignKey, KeyVote, KeyGrant, KeyState, KeyNotify, KeyPeerKey,
	KeyDemoted, KeyBecame, KeyFreezeUntil, KeySwitch, KeyIncoming, KeyReplicaSeq, KeyStandbyList, KeyFormer,
	KeyNotices, KeyCmd, KeyCmdResult, KeyDnsChanged,
)

// localPrefixes 按前缀归为本机的设置:hub*(主副机同步的本机状态)、ops*(运维页)、acme*(证书签发)。
var localPrefixes = []string{"hub", "ops", "acme"}

// globalKeys 全局设置(随副本复制)。
var globalKeys = setOf(
	"timezone", "allowPrivate", "blockBT", "supportOnline",
	"upstreamTestUrl", "upstreamCheckMinutes", "upstreamCheckFailThreshold",
	"subProfileTitle", "subEncode", "subShowNotice", "subClashExt", "subSingboxExt", "subAutoFormat", "subUpdates",
	"subUseUserName", "subPageEnabled", "subPageTitle", "subPageSupport", "subPageNotice", "subShareEnabled",
	"subPageBuyURL", "subPageBrand", "extRefreshMinutes",
	"tgEnabled", "tgToken", "tgChatId", "tgProxy", "tgApiBase", "tgBot", "tgOnLogin", "tgOnUserDisabled", "tgOnUserExpiring",
	"tgExpiringDays", "tgOnQuota", "tgQuotaPercent", "tgOnRuleLimit", "tgOnUpstream", "tgOnCore", "tgOnReach", "tgOnCert",
	"tgDaily", "tgDailyHour", "webhookUrl", "webhookSecret", "notifyOnce",
	"reachAuto", "reachMinutes", "reachToken",
	"statsBucketSeconds", "trafficAge", "subLogAge", "auditAge",
	"resellerEnabled", "backupHour", "backupKeep", "backupTelegram",
	"totpEnabled", "totpSecret", "apiEnabled", "apiToken", "adminDefault", "sessionMaxAge",
	// 容灾的全局设置
	KeyMode, KeyStandbys, KeyReplicaMinutes, KeyConfirmMinutes, KeyAutoMinutes, KeyRankStepMinutes,
	KeyWitness, KeyClusterKey, KeyPauseUntil, KeyDnsToken, KeyDnsNames,
)

// globalPrefixes 按前缀归为全局的设置:主机记下的各副机账本纪元、钉住的备用公钥。
var globalPrefixes = []string{"ledgerEpoch:", KeyPubPrefix}

// IsLocal 这个设置是不是本机的(不进副本)。
func IsLocal(key string) bool {
	if localKeys[key] {
		return true
	}
	for _, p := range localPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// IsGlobal 这个设置是不是显式归为全局的。
func IsGlobal(key string) bool {
	if globalKeys[key] {
		return true
	}
	for _, p := range globalPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Classified 归了类没有(测试用)。
func Classified(key string) bool { return IsLocal(key) || IsGlobal(key) }

// Hidden 设置接口不发给浏览器的容灾键:一律按 ha 前缀隐藏(里面有签名私钥、集群密钥、Cloudflare 令牌……),
// 容灾页走专用接口。
func Hidden(key string) bool { return strings.HasPrefix(key, "ha") }

func setOf(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// 容灾设置键。
const (
	// 全局(随副本复制)
	KeyMode            = "haMode"            // off | confirm | auto
	KeyStandbys        = "haStandbys"        // 备用台数 1–3,默认 2
	KeyReplicaMinutes  = "haReplicaMinutes"  // 定时副本间隔,默认 5
	KeyConfirmMinutes  = "haConfirmMinutes"  // 确认后接任:失联多久发确认提示,默认 3
	KeyAutoMinutes     = "haAutoMinutes"     // 自动接任:失联多久接任,默认 10
	KeyRankStepMinutes = "haRankStepMinutes" // 备用 2 比备用 1 多等多久,默认 3
	KeyWitness         = "haWitness"         // 境外测点见证(Globalping),默认开
	KeyClusterKey      = "haClusterKey"      // 副机之间互相确认与投票的 HMAC 密钥
	KeyPauseUntil      = "haPauseUntil"      // 暂停接任到何时(unix 秒)
	KeyPubPrefix       = "haPub:"            // 钉住的备用公钥(haPub:<服务器 id>)
	KeyDnsToken        = "haDnsToken"        // Cloudflare 令牌(Zone.DNS:Edit),主机变更后自动改 DNS;不复用签证书的令牌
	KeyDnsNames        = "haDnsNames"        // 要跟着主机走的域名,逗号分隔

	// 本机
	KeyEpoch       = "haEpoch"       // 本机纪元;0 = 没开过容灾、没换过主机
	KeyRole        = "haRole"        // "" | promoting(接任 / 设为主机进行中)| demoted(原主机回来后自动变成的副机,等新主机来接)
	KeyMasterId    = "haMasterId"    // 副机:当前纪元主机的服务器 id
	KeyMasterPub   = "haMasterPub"   // 副机:当前纪元主机的签名公钥
	KeySignKey     = "haSignKey"     // 主机:签名私钥
	KeyVote        = "haVote"        // 副机:本机投过的票
	KeyGrant       = "haGrant"       // 副机:设为主机时收到的交接标记
	KeyState       = "haState"       // 副机:最近一次收到的容灾状态
	KeyNotify      = "haNotify"      // 备用:加密的通知块(Telegram / Webhook 设置)
	KeyPeerKey     = "haPeerKey"     // 副机:从容灾状态里收到的集群密钥
	KeyDemoted     = "haDemoted"     // 原主机:什么时候、因为谁接任变成了副机
	KeyBecame      = "haBecame"      // 主机:本机怎么成为主机的(接任 / 设为主机)、原主机是谁
	KeyFreezeUntil = "haFreezeUntil" // 主机:设为主机进行中,写操作暂停到何时
	KeySwitch      = "haSwitch"      // 主机 / 已变为副机的原主机:设为主机的进度与结果
	KeyIncoming    = "haIncoming"    // 被设为主机的副机:收到的数据暂存到哪一步
	KeyReplicaSeq  = "haReplicaSeq"  // 主机:最近一份副本的序号(只增)
	KeyStandbyList = "haStandbyList" // 主机:上次算出的备用名单(刚启动、状态还没拉齐时先用它)
	KeyFormer      = "haFormer"      // 接任的主机:失联的原主机,等它回来自动变为副机
	KeyNotices     = "haNotices"     // 面板顶部的一次性提示
	KeyCmd         = "haCmd"         // 命令行交给运行中的服务的请求
	KeyCmdResult   = "haCmdResult"   // 上面请求的结果
	KeyDnsChanged  = "haDnsChanged"  // 最近一次自动改 DNS 的记录(原值、新值)
)

// 角色(haRole)。
const (
	RolePromoting = "promoting"
	RoleDemoted   = "demoted"
)

// 模式。
const (
	ModeOff     = "off"
	ModeConfirm = "confirm"
	ModeAuto    = "auto"
)

// MinVersion 认得纪元与容灾接口的最低版本:低于它的副机不能当备用、不能被设为主机,也不计入降级判定。
const MinVersion = "0.7.17"

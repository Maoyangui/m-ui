// Package ha 主机容灾:候选副机保存加密的整份副本,主机失联时由候选接管为临时主机;纪元(epoch)防脑裂,
// 旧主机回来自动降为只读副机;之后可以从临时主机迁回新主机(拉取或推送两个方向),临时主机退回副机。
//
// 默认关闭:没开过容灾的集群(纪元为 0)主副机的行为、推送内容、修订号都和以前一样。
package ha

import "strings"

// 设置分两类:
//   - 本机设置:每台机器自己的监听、端口、路径、域名、证书、角色、同步状态、运维参数、容灾的本机状态。
//     不进副本;接管时保留候选自己的值;迁回时按"沿用原主机身份 / 用本机设置"二选一。
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
	KeyEpoch, KeyRole, KeyMasterId, KeyMasterPub, KeySignKey, KeyVote, KeyGrant, KeyState, KeyNotify,
	KeyFencedBy, KeyPromotedFrom, KeyExportCode, KeyRecvCode, KeyHandover, KeyFreezeUntil, KeyRecoveryKEK,
	KeyCommit, KeyImport, KeyNotified, KeyClusterKeyLocal, KeyHandoverGrant, KeyReplicaSeq, KeyRollback,
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
	KeyMode, KeyCandidates, KeyExclude, KeyReplicaMinutes, KeyConfirmMinutes, KeyAutoMinutes, KeyRankStepMinutes,
	KeyWitness, KeyRecovery, KeyIncludeCerts, KeyDisableFormer, KeyClusterKey, KeyPauseUntil,
)

// globalPrefixes 按前缀归为全局的设置:主机记下的各副机账本纪元、钉住的候选公钥。
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

// Hidden 设置接口不发给浏览器的容灾键:一律按 ha 前缀隐藏(里面有签名私钥、集群密钥、迁回码哈希……),
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
	KeyMode            = "haMode"            // off | manual | confirm | auto
	KeyCandidates      = "haCandidates"      // 候选台数 1–3,默认 2
	KeyExclude         = "haExclude"         // 不当候选的服务器 id(JSON 数组)
	KeyReplicaMinutes  = "haReplicaMinutes"  // 定时副本间隔,默认 5
	KeyConfirmMinutes  = "haConfirmMinutes"  // 确认模式:失联多久发确认提示,默认 3
	KeyAutoMinutes     = "haAutoMinutes"     // 自动模式:失联多久接管,默认 10
	KeyRankStepMinutes = "haRankStepMinutes" // 名次间隔,默认 3
	KeyWitness         = "haWitness"         // Globalping 见证,默认开
	KeyRecovery        = "haRecovery"        // 恢复码模式:候选自己解不开副本,接管必须人工输码
	KeyIncludeCerts    = "haIncludeCerts"    // 副本带主机证书,默认开
	KeyDisableFormer   = "haDisableFormerMaster"
	KeyClusterKey      = "haClusterKey" // 副机之间互相确认与投票的 HMAC 密钥
	KeyPauseUntil      = "haPauseUntil" // 暂停自动接管到何时(unix 秒)
	KeyPubPrefix       = "haPub:"       // 钉住的候选公钥(haPub:<服务器 id>)

	// 本机
	KeyEpoch           = "haEpoch"        // 本机纪元;0 = 没开过容灾
	KeyRole            = "haRole"         // "" | temp | fenced | promoting
	KeyMasterId        = "haMasterId"     // 副机:当前纪元主机的服务器 id
	KeyMasterPub       = "haMasterPub"    // 副机:当前纪元主机的签名公钥
	KeySignKey         = "haSignKey"      // 主机:签名私钥
	KeyVote            = "haVote"         // 副机:本机投过的票
	KeyGrant           = "haGrant"        // 副机:迁回时收到的交接标记
	KeyState           = "haState"        // 副机:最近一次收到的容灾状态
	KeyNotify          = "haNotify"       // 候选:加密的通知块
	KeyFencedBy        = "haFencedBy"     // 已降级:被谁、在哪个纪元接管
	KeyPromotedFrom    = "haPromotedFrom" // 临时主机:从哪台、哪份副本接管的
	KeyExportCode      = "haExportCode"   // 临时主机:迁回码(只存哈希)
	KeyRecvCode        = "haRecvCode"     // 新主机:接收码(只存哈希)
	KeyHandover        = "haHandover"     // 临时主机:进行中的迁回
	KeyFreezeUntil     = "haFreezeUntil"  // 临时主机:迁回冻结到何时
	KeyRecoveryKEK     = "haRecoveryKEK"  // 主机:恢复码派生的密钥(不存恢复码)
	KeyCommit          = "haCommit"       // 新主机:待提交的迁回
	KeyImport          = "haImport"       // 新主机:迁入后的检查单
	KeyNotified        = "haNotified"     // 已发过的一次性通知(接管完成、迁回完成)
	KeyClusterKeyLocal = "haPeerKey"      // 副机:从容灾状态里收到的集群密钥
	KeyHandoverGrant   = "haHandoverTo"   // 临时主机:提交迁回时随容灾状态推给副机的交接标记
	KeyReplicaSeq      = "haReplicaSeq"   // 主机:最近一份副本的序号(只增)
	KeyRollback        = "haRollback"     // 候选:接管前的纪元与主机(回滚接管时恢复)
)

// 角色。
const (
	RoleTemp      = "temp"
	RoleFenced    = "fenced"
	RolePromoting = "promoting"
)

// 模式。
const (
	ModeOff     = "off"
	ModeManual  = "manual"
	ModeConfirm = "confirm"
	ModeAuto    = "auto"
)

// MinVersion 认得纪元与容灾接口的最低版本:低于它的副机不能当候选,也不计入降级判定。
const MinVersion = "0.7.17"

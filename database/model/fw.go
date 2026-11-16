package model

import "encoding/json"

// FwRule 面板防火墙的一条规则。
//
//	Kind allow:这些端口对所有人放行。系统防火墙开着的服务器上,在它那里加放行(只增减 m-ui 自己加的);没开着的用不着。
//	Kind deny:这些来源进不来。不填端口 = 整台机器都不让进(连代理流量一起挡);填了端口只挡这些端口。
//	Kind only:这些端口只让这些来源进,别人一律拒绝(比如 SSH、面板端口只给自己的 IP)。
//
// 后两种落在各台服务器上 m-ui 自己的那张表里(见 fw 包),不动系统防火墙原有的规则。
// 规则由主机下发到 NodeIds 指的那些服务器(空 = 全部);到期(Expiry)自动撤掉。
type FwRule struct {
	Id      uint            `json:"id" gorm:"primaryKey;autoIncrement"`
	Kind    string          `json:"kind"`
	Proto   string          `json:"proto"`                    // "" = TCP 和 UDP,或 tcp / udp
	Ports   string          `json:"ports"`                    // 规整后的端口列表,如 "22,8000-9000";空 = 全部端口
	Sources string          `json:"sources"`                  // 规整后的来源,如 "203.0.113.9/32,2001:db8::/32"
	NodeIds json.RawMessage `json:"nodeIds" gorm:"type:text"` // 应用到哪些服务器;空 = 全部
	Expiry  int64           `json:"expiry"`                   // 到期时间(Unix 秒);0 = 一直有效
	Note    string          `json:"note"`                     // 备注
	Origin  string          `json:"origin"`                   // manual = 手动加的;ban = 从黑名单升上来的
	// Enabled 关掉 = 留着不生效。故意不写 gorm 的 default:true —— 写了的话,存一条关着的(副机收快照整表重建时)会被悄悄改成开着
	Enabled   bool  `json:"enabled"`
	CreatedAt int64 `json:"createdAt" gorm:"autoCreateTime:false"`
}

// 黑名单的来由。
const (
	BanLogin  = "login"  // 面板 / 代理面板登录反复失败
	BanSub    = "sub"    // 订阅口上试了很多个不存在的地址
	BanAPI    = "api"    // 外部 API 上试了很多个错的密钥
	BanManual = "manual" // 手动加的
)

// FwBan 应用层黑名单的一条:这个来源连不上面板、代理面板、订阅口和外部 API(各监听直接断开连接)。
// 代理流量不受影响 —— 同一个出口 IP 后面可能还有正常用户;要连代理一起挡,把它升成一条 FwRule。
type FwBan struct {
	Id       uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Ip       string `json:"ip" gorm:"uniqueIndex"` // IP 或网段;自动拉黑的 IPv6 记所在的 /64
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"` // 触发的依据,给人看(10 分钟内登录失败 12 次……);手动加的是备注
	Count    int    `json:"count"`    // 自动拉黑时数到的那个数(失败了几次、试了几个):界面按 Reason + Count 用自己的语言说
	Hits     int    `json:"hits"`     // 第几次被拉黑(屡犯的封得久)
	FirstAt  int64  `json:"firstAt"`
	LastAt   int64  `json:"lastAt"`
	Expiry   int64  `json:"expiry"` // 到期自动解除;0 = 一直有效
	Auto     bool   `json:"auto"`   // 自动拉黑的(手动加的为 false)
}

// FwAllow 白名单:自动拉黑永远不碰这些来源(自己的办公网、监控探针……)。
type FwAllow struct {
	Id        uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Ip        string `json:"ip" gorm:"uniqueIndex"` // IP 或网段
	Note      string `json:"note"`
	CreatedAt int64  `json:"createdAt" gorm:"autoCreateTime:false"`
}

// AppliesTo 这条规则管不管服务器 nodeId:NodeIds 为空 = 全部服务器;nodeId 为 0(本机还没有服务器记录)按全部算。
func (r FwRule) AppliesTo(nodeId uint) bool {
	if len(r.NodeIds) == 0 || nodeId == 0 {
		return true
	}
	var ids []uint
	if json.Unmarshal(r.NodeIds, &ids) != nil || len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if id == nodeId {
			return true
		}
	}
	return false
}

// FwState 一台服务器的防火墙状况:各台自己量(runner 的 fwTick),副机随报告报给主机,防火墙页按它显示。
type FwState struct {
	Frontend   string `json:"frontend"`             // 系统防火墙的种类:none / ufw / firewalld / iptables / nftables(fw.Front*)
	Blocking   bool   `json:"blocking"`             // 现在会不会把没放行的入站挡掉
	Detail     string `json:"detail,omitempty"`     // 判断的依据,给人看的一句
	Opened     bool   `json:"opened,omitempty"`     // 放开过(面板、安装脚本或命令行做的,可撤销)
	OpenedAt   int64  `json:"openedAt,omitempty"`   // 什么时候放开的
	OpenedFrom string `json:"openedFrom,omitempty"` // 放开之前是哪种系统防火墙
	Engine     string `json:"engine"`               // 拉黑 / 只允许落在哪:nft / iptables / 空(这台上落不了)
	Rules      int    `json:"rules"`                // 这台上生效着的面板规则条数(三种都算)
	Paused     bool   `json:"paused,omitempty"`     // 这台用命令行暂停了面板规则(m-ui firewall pause)
	Allow      string `json:"allow,omitempty"`      // 系统防火墙还拦着时,m-ui 的端口在它那里放行得怎么样(FwAllow*);没在拦为空
	Ports      int    `json:"ports"`                // 这台上 m-ui 自己用到几个端口
	Allowed    int    `json:"allowed,omitempty"`    // 系统防火墙里由 m-ui 加的放行有几条
	Error      string `json:"error,omitempty"`      // 最近一次落规则 / 放行端口没成的原因
	At         int64  `json:"at"`                   // 什么时候量的(0 = 刚启动,第一轮还没量完)
}

// 系统防火墙还拦着时,m-ui 的端口在它那里的状况(FwState.Allow)。
const (
	FwAllowOK     = "ok"     // 已经放行
	FwAllowOff    = "off"    // 自动放行关着:m-ui 不管,由管理员自己在系统防火墙里放行
	FwAllowManual = "manual" // 用户自己写的 nftables 规则:面板不替他改,要自己放行
	FwAllowFail   = "fail"   // 去放行了,没成(原因在 Error 里)
)

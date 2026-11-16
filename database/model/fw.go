package model

import "encoding/json"

// FwRule 面板防火墙的一条规则。落在各台服务器上 m-ui 自己的那张表里(见 fw 包),不动系统防火墙原有的规则。
//
//	Kind deny:这些来源进不来。不填端口 = 整台机器都不让进(连代理流量一起挡);填了端口只挡这些端口。
//	Kind only:这些端口只让这些来源进,别人一律拒绝(比如 SSH、面板端口只给自己的 IP)。
//
// 规则由主机下发到 NodeIds 指的那些服务器(空 = 全部);到期(Expiry)自动撤掉。
type FwRule struct {
	Id        uint            `json:"id" gorm:"primaryKey"`
	Kind      string          `json:"kind"`
	Proto     string          `json:"proto"`                       // "" = TCP 和 UDP,或 tcp / udp
	Ports     string          `json:"ports"`                       // 规整后的端口列表,如 "22,8000-9000";空 = 全部端口
	Sources   string          `json:"sources"`                     // 规整后的来源,如 "203.0.113.9/32,2001:db8::/32"
	NodeIds   json.RawMessage `json:"nodeIds" gorm:"type:text"`    // 应用到哪些服务器;空 = 全部
	Expiry    int64           `json:"expiry"`                      // 到期时间(Unix 秒);0 = 一直有效
	Note      string          `json:"note"`                        // 备注
	Origin    string          `json:"origin"`                      // manual = 手动加的;ban = 从黑名单升上来的
	Enabled   bool            `json:"enabled" gorm:"default:true"` // 关掉 = 留着不生效
	CreatedAt int64           `json:"createdAt" gorm:"autoCreateTime:false"`
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
	Id       uint   `json:"id" gorm:"primaryKey"`
	Ip       string `json:"ip" gorm:"uniqueIndex"` // IP 或网段;自动拉黑的 IPv6 记所在的 /64
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"` // 触发的依据,给人看(10 分钟内登录失败 12 次……)
	Hits     int    `json:"hits"`     // 第几次被拉黑(屡犯的封得久)
	FirstAt  int64  `json:"firstAt"`
	LastAt   int64  `json:"lastAt"`
	Expiry   int64  `json:"expiry"` // 到期自动解除;0 = 一直有效
	Auto     bool   `json:"auto"`   // 自动拉黑的(手动加的为 false)
}

// FwAllow 白名单:自动拉黑永远不碰这些来源(自己的办公网、监控探针……)。
type FwAllow struct {
	Id        uint   `json:"id" gorm:"primaryKey"`
	Ip        string `json:"ip" gorm:"uniqueIndex"` // IP 或网段
	Note      string `json:"note"`
	CreatedAt int64  `json:"createdAt" gorm:"autoCreateTime:false"`
}

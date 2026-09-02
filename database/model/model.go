package model

import "encoding/json"

// Setting 面板设置,key/value。角色开关也存这里:nodeMode=false 为主(HK),true 为副(TW)。
type Setting struct {
	Id    uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Key   string `json:"key" gorm:"uniqueIndex"`
	Value string `json:"value"`
}

// Admin 面板登录账号(bcrypt 哈希,从旧 s-ui 原样迁入)。
type Admin struct {
	Id         uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Username   string `json:"username" gorm:"uniqueIndex"`
	Password   string `json:"-"`
	LastLogins string `json:"lastLogins"`
}

// Upstream 上游落地(旧 s-ui 的出站)。warp 本地代理也是一个 Upstream(type=socks 指向 127.0.0.1:40000)。
// direct 不入库,渲染时内置。
type Upstream struct {
	Id      uint            `json:"id" gorm:"primaryKey;autoIncrement"`
	Name    string          `json:"name" gorm:"uniqueIndex"`
	Type    string          `json:"type"`
	Options json.RawMessage `json:"options"`
	Sort    int             `json:"sort"`
}

// Line 线路 = 用户在订阅里看到的一个节点。
// 合并了旧 s-ui 的"入站 + 路由规则":一条线路即 一个入口协议+端口 → 一个上游。
// 端口在所有入口服务器上一致,配置由 Hub 按服务器渲染下发。
type Line struct {
	Id         uint            `json:"id" gorm:"primaryKey;autoIncrement"`
	Name       string          `json:"name" gorm:"uniqueIndex"`
	Protocol   string          `json:"protocol"` // hysteria2 | anytls | shadowsocks
	Port       int             `json:"port" gorm:"uniqueIndex"`
	UpstreamId uint            `json:"upstreamId"` // 0 = direct
	Options    json.RawMessage `json:"options"`    // 协议入站附加参数(obfs、method、padding_scheme 等)
	// Addrs 订阅里对外公布的连接地址列表 [{server, server_port?, remark?, sni?}]。
	// 空 = 用入口节点主机 + 本线路端口(常态);非空 = 逐地址各出一条链接/代理。
	// 也是 HK/TW 双入口的承载:每个入口一条 addr。
	Addrs json.RawMessage `json:"addrs,omitempty"`
	// NodeIds 该线路部署到哪些服务器 [1,2];空 = 全部服务器。渲染与订阅都按此过滤。
	NodeIds json.RawMessage `json:"nodeIds,omitempty"`
	// Tls 线路 TLS 配置 {mode: cert|reality|none, reality:{private_key, public_key, short_ids, handshake_server, handshake_port}}。
	// 空 = 按协议默认(hy2/anytls/tuic/trojan 用节点证书,vless 用 reality,其余无)。
	Tls json.RawMessage `json:"tls,omitempty"`
	// Transport vless/vmess/trojan 的传输层(sing-box transport 对象:ws/grpc/httpupgrade/http);空 = TCP。
	Transport json.RawMessage `json:"transport,omitempty"`
	Enabled   bool            `json:"enabled" gorm:"default:true"`
	Sort      int             `json:"sort"`
}

// Node 入口服务器。IsLocal 标记 Hub 所在机;副机通过 ApiUrl+Token 管理。
type Node struct {
	Id      uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Name    string `json:"name" gorm:"uniqueIndex"`
	Domain  string `json:"domain"`
	ApiUrl   string `json:"apiUrl"`
	Token    string `json:"-"`
	Insecure bool   `json:"insecure"` // 副机面板用自签证书时跳过校验
	IsLocal  bool   `json:"isLocal"`
	Enabled  bool   `json:"enabled" gorm:"default:true"`
	Sort     int    `json:"sort"`
	// Addr 订阅里节点的连接地址(可填 IP);空 = 自动用该服务器探测到的公网 IP。域名仍作 SNI,绕开大陆 DNS 污染。
	Addr string `json:"addr"`
	// PublicIP 该服务器自动探测到的公网 IP(本机由 runner 写入,副机随报告上报)。
	PublicIP string `json:"publicIp"`
	// Ratio 流量倍率:经该服务器的流量按倍率计入用户用量(2 = 双倍扣量);0/1 = 原样。
	Ratio float64 `json:"ratio" gorm:"default:1"`
}

// User 订阅用户。配额/流量为两台服务器聚合值,判定只在主端进行。
type User struct {
	Id          uint            `json:"id" gorm:"primaryKey;autoIncrement"`
	Enabled     bool            `json:"enabled" gorm:"default:true"`
	Name        string          `json:"name" gorm:"uniqueIndex"`
	Credentials json.RawMessage `json:"credentials,omitempty"` // 按协议存凭据,从旧库原样保留
	Volume      int64           `json:"volume"`                // 字节,0=不限
	Expiry      int64           `json:"expiry"`                // unix 秒,0=不限
	Up          int64           `json:"up"`
	Down        int64           `json:"down"`
	TotalUp     int64           `json:"totalUp"`
	TotalDown   int64           `json:"totalDown"`
	AutoReset   bool            `json:"autoReset"`
	ResetDays   int             `json:"resetDays"`
	NextReset   int64           `json:"nextReset"`
	DeviceLimit int             `json:"deviceLimit"` // 同时在线设备(源IP)上限,0=不限
	SpeedUp     int             `json:"speedUp"`     // 上行 Mbps,0=不限
	SpeedDown   int             `json:"speedDown"`   // 下行 Mbps,0=不限
	Remark      string          `json:"remark"`
	Desc        string          `json:"desc"`
	CreatedAt   int64           `json:"createdAt" gorm:"default:0;not null"`
	OnlineAt    int64           `json:"onlineAt" gorm:"default:0;not null"`
}

// Plan 套餐:配额/时长/设备/限速/线路的模板,用于建号、续费与批量操作。
type Plan struct {
	Id          uint            `json:"id" gorm:"primaryKey;autoIncrement"`
	Name        string          `json:"name" gorm:"uniqueIndex"`
	VolumeGB    int             `json:"volumeGb"` // 0=不限
	Days        int             `json:"days"`     // 0=不限
	DeviceLimit int             `json:"deviceLimit"`
	SpeedUp     int             `json:"speedUp"`
	SpeedDown   int             `json:"speedDown"`
	AutoReset   bool            `json:"autoReset"`
	ResetDays   int             `json:"resetDays"`
	LineIds     json.RawMessage `json:"lineIds,omitempty"` // 空=不改动用户线路
	Desc        string          `json:"desc"`
	Sort        int             `json:"sort"`
}

// ExtNode 外部节点:一条分享链接,或一个外部订阅地址;分配给用户后聚合进该用户的订阅。
type ExtNode struct {
	Id        uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string `json:"name" gorm:"uniqueIndex"`
	Type      string `json:"type"`   // link | sub
	Value     string `json:"value"`  // 分享链接 或 订阅 URL
	Prefix    string `json:"prefix"` // 节点名前缀(可空),如 "[中转] "
	Enabled   bool   `json:"enabled" gorm:"default:true"`
	Sort      int    `json:"sort"`
	Remark    string `json:"remark"`
	LastFetch int64  `json:"lastFetch"`
	LastError string `json:"lastError"`
	NodeCount int    `json:"nodeCount"`
	Cache     string `json:"cache,omitempty"` // 外部订阅抓取到的内容(主机抓取,随快照下发副机)
}

// UserExt 用户-外部节点 多对多。
type UserExt struct {
	UserId uint `json:"userId" gorm:"primaryKey;autoIncrement:false"`
	ExtId  uint `json:"extId" gorm:"primaryKey;autoIncrement:false"`
}

// UserLine 用户-线路 多对多。
type UserLine struct {
	UserId uint `json:"userId" gorm:"primaryKey;autoIncrement:false"`
	LineId uint `json:"lineId" gorm:"primaryKey;autoIncrement:false"`
}

// SubLog 订阅访问日志。
type SubLog struct {
	Id     uint64 `json:"id" gorm:"primaryKey;autoIncrement"`
	Ts     int64  `json:"ts" gorm:"index"`
	User   string `json:"user" gorm:"index"`
	Ip     string `json:"ip"`
	Ua     string `json:"ua"`
	Format string `json:"format"` // link | clash
	Status int    `json:"status"`
}

// Stats 流量时序(bucket 聚合,沿用 s-ui 的四元唯一键+累加合并模型,天然支持多机合并)。
type Stats struct {
	Id        uint64 `json:"id" gorm:"primaryKey;autoIncrement"`
	DateTime  int64  `json:"dateTime" gorm:"uniqueIndex:idx_stats_bucket,priority:3"`
	Resource  string `json:"resource" gorm:"uniqueIndex:idx_stats_bucket,priority:1"` // line | upstream | user
	Tag       string `json:"tag" gorm:"uniqueIndex:idx_stats_bucket,priority:2"`
	Direction bool   `json:"direction" gorm:"uniqueIndex:idx_stats_bucket,priority:4"`
	Traffic   int64  `json:"traffic"`
}

// TrafficCursor 主端:对某副机某用户已计入聚合的单调计数快照。
// 副机计数器回绕(换机重装)时快照归零重认,保证不重复计、不漏计。
type TrafficCursor struct {
	NodeId   uint   `json:"nodeId" gorm:"primaryKey;autoIncrement:false"`
	UserName string `json:"userName" gorm:"primaryKey"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// AgentCounter 副机端:本机每用户只增不减的流量账本(不直接写 User 表,避免与主端下发冲突)。
type AgentCounter struct {
	UserName string `json:"userName" gorm:"primaryKey"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// Change 审计日志。
type Change struct {
	Id       uint64          `json:"id" gorm:"primaryKey;autoIncrement"`
	DateTime int64           `json:"dateTime"`
	Actor    string          `json:"actor"`
	Key      string          `json:"key"`
	Action   string          `json:"action"`
	Obj      json.RawMessage `json:"obj"`
}

// All 供 AutoMigrate 使用。
func All() []interface{} {
	return []interface{}{
		&Setting{}, &Admin{}, &Upstream{}, &Line{}, &Node{}, &User{}, &UserLine{}, &Plan{}, &ExtNode{}, &UserExt{},
		&SubLog{}, &Stats{}, &TrafficCursor{}, &AgentCounter{}, &Change{},
	}
}

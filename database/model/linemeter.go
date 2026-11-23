package model

// LineMeter 一台服务器这次开始计数以来「用户 × 线路」的流量累计。只在有启用的突发规则勾了「忽略部分线路」时才记、才报
// (默认不记);主机据此算这种规则的用量 —— 只加没被忽略的那些线路(见 rules.LineUsage)。
// Boot 每次从头记(进程重启、功能关掉又打开)都换一个新的,主机看它分辨计数是不是从 0 重来了。
type LineMeter struct {
	Boot string         `json:"boot"`
	Rows []LineMeterRow `json:"rows"`
}

// LineMeterRow 一个用户在一条线路上的累计,和记账同一个分法(见 jobs 的 userBytes):
// Raw 是按服务器倍率计费的原始字节(主机按这台的倍率乘);Billed 是走了单独设倍率的节点、已经按节点倍率乘好的字节。
type LineMeterRow struct {
	User   string `json:"u"`
	Line   uint   `json:"l"`
	Raw    int64  `json:"r,omitempty"`
	Billed int64  `json:"b,omitempty"`
}

package model

// UpstreamSource 一条上游是从哪个外部订阅的哪个节点加来的。
//
// 只给主机的「跟随」用:这条上游在所有用它的服务器上都测不通时,去订阅里看这个节点是不是换了端口 / 参数,换了就把上游跟着改
// (见 web/upstream_follow.go)。通着的上游不会被动。单独一张表、不进推给副机的快照:副机用不到,记的结论变了也不该触发一轮推送。
type UpstreamSource struct {
	UpstreamId uint   `json:"upstreamId" gorm:"primaryKey;autoIncrement:false"`
	ExtId      uint   `json:"extId" gorm:"index"`
	Key        string `json:"key"`        // 节点的身份指纹(加的时候的;跟随之后换成新的)
	Name       string `json:"name"`       // 节点在订阅里的名字(找回节点时用)
	TriedAt    int64  `json:"triedAt"`    // 最近一次尝试跟随的时间
	FollowedAt int64  `json:"followedAt"` // 最近一次真的跟着改了的时间
	Note       string `json:"note"`       // 最近一次的结论,见 web 的 follow* 常量
	Detail     string `json:"detail"`     // 结论的补充(跟着改了什么:旧地址 → 新地址)
}

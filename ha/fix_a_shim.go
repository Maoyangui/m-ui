package ha

// 临时适配(修复者 A):下面这些是设计 3.2 约定由 B / C 提供的接口,本分支先按约定的签名放一份保持现状的版本,
// 让 A 的代码能编译、能测。合并时:B、C 的分支里有了真实现,就删掉这里对应的那一项(编译会报重复定义,照着删即可)。
//
//	TakeoverNeed、replicaDBSize、metaStandbys、peerTakeoverErrAt   → C(replicator.go、store.go、replica.go、peer.go)
//	applyFormerRow、switchTargetUp、revertDNS、becameDNS、DnsOutcome、dnsText、identityWarnText、identityItems → B

// TakeoverNeed 接任要的磁盘空间(C 的 replicator.go;A0 口径)。
func TakeoverNeed(zipSize, dbSize int64) int64 { return 4*zipSize + 64<<20 }

// replicaDBSize 副本里库的大小(C 合入后 = ri.DBSize)。
func replicaDBSize(ri ReplicaInfo) int64 { return 0 }

// metaStandbys 生成副本时的备用名单(C 合入后 = meta.Standbys)。
func metaStandbys(meta ReplicaMeta) []uint { return nil }

// peerTakeoverErrAt 同伴最近一次接任失败的时间(C 合入后 = ps.TakeoverErrAt)。
func peerTakeoverErrAt(ps PeerStatus) int64 { return 0 }

// identityItems 身份告警(B 把 identityResult.Warnings 改成 []PlanItem 之后 = r.Warnings)。
func identityItems(r identityResult) []PlanItem { return r.Warnings }

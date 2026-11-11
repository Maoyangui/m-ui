package ha

// 临时适配(修复者 A):下面这些是设计 3.2 约定由 B / C 提供的接口,本分支先按约定的签名放一份保持现状的版本,
// 让 A 的代码能编译、能测。合并时:B、C 的分支里有了真实现,就删掉这里对应的那一项(编译会报重复定义,照着删即可)。
//
//	TakeoverNeed、replicaDBSize、metaStandbys、peerTakeoverErrAt   → C(replicator.go、store.go、replica.go、peer.go)
//	applyFormerRow、switchTargetUp、revertDNS、becameDNS、DnsOutcome、dnsText、identityWarnText、identityItems → B

import (
	"fmt"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"

	"gorm.io/gorm"
)

// TakeoverNeed 接任要的磁盘空间(C 的 replicator.go;A0 口径)。
func TakeoverNeed(zipSize, dbSize int64) int64 { return 4*zipSize + 64<<20 }

// replicaDBSize 副本里库的大小(C 合入后 = ri.DBSize)。
func replicaDBSize(ri ReplicaInfo) int64 { return 0 }

// metaStandbys 生成副本时的备用名单(C 合入后 = meta.Standbys)。
func metaStandbys(meta ReplicaMeta) []uint { return nil }

// peerTakeoverErrAt 同伴最近一次接任失败的时间(C 合入后 = ps.TakeoverErrAt)。
func peerTakeoverErrAt(ps PeerStatus) int64 { return 0 }

// applyFormerRow 新库里原主机那一行填上"作为副机时怎么连"(B 的 identity.go;这里是从 promote.go 原样搬来的现状)。
func (s *Service) applyFormerRow(tx *gorm.DB, formerId, selfId uint, id Identity, how string) error {
	up := map[string]interface{}{"is_local": false}
	if l := id.Self; l.ApiUrl != "" && l.Token != "" {
		up["api_url"], up["token"], up["cert_fp"] = l.ApiUrl, l.Token, l.CertFP
		up["insecure"] = strings.HasPrefix(strings.ToLower(l.ApiUrl), "https://")
	}
	if how == HowTakeover {
		up["enabled"] = false // 失联期间不进订阅;它回来自动变为副机时再启用
	}
	return tx.Model(&model.Node{}).Where("id = ?", formerId).Updates(up).Error
}

// switchTargetUp 设为主机的目标已经以 epoch 起来了:按"换主机完成"收尾(B 的 switch.go;A0:不处理)。
func (s *Service) switchTargetUp(target uint, epoch uint64) bool { return false }

// revertDNS 让出主机时把本任期自动改过的 DNS 改回去(B 的 dns.go;A0:不改)。
func (s *Service) revertDNS(b Became) ([]DnsChange, error) { return nil, nil }

// DnsOutcome 成为主机后自动改 DNS 的结果(B 的 dns.go)。
type DnsOutcome struct {
	State            string // done | partial | failed | todo | ""
	SubDone          bool   // 订阅主机名本身的匹配记录全部改好
	Changes, Skipped []DnsChange
	Err              string
}

// becameDNS 刚成为主机:按设置改 DNS,结果写进 b(B 的 dns.go;A0:包装现有的 updateDNS)。
func (s *Service) becameDNS(b *Became, ip string) DnsOutcome {
	changes, err := s.updateDNS(b.FormerIP, b.FormerIP6)
	b.Dns = changes
	out := DnsOutcome{Changes: changes}
	switch {
	case err != nil:
		b.DnsErr = err.Error()
		out.State, out.Err = "failed", b.DnsErr
		logger.Warning("容灾:自动改 DNS 失败: ", err)
	case len(changes) > 0:
		out.State = "done"
	}
	return out
}

// dnsText 订阅地址要不要改 DNS 的那一句(B 的 dns.go;这里是从 became.go 原样搬来的现状)。
func (s *Service) dnsText(b Became, ip string) string {
	host := b.Identity.SubHost
	switch {
	case host == "" || isIPHost(host) || !b.Identity.SubURLKept:
		return ""
	case len(b.Dns) > 0:
		var names []string
		for _, c := range b.Dns {
			names = append(names, c.Name)
		}
		return fmt.Sprintf("DNS:已自动把 %s 改指向本机(%s),用户的订阅地址不变。\n", notify.Esc(strings.Join(names, "、")), notify.Esc(ip))
	case b.DnsErr != "":
		return fmt.Sprintf("DNS:自动修改失败(%s)。请手动把 %s 改指向本机 IP %s,用户的订阅地址不变。\n", notify.Esc(b.DnsErr), notify.Esc(host), notify.Esc(ip))
	}
	return fmt.Sprintf("DNS:把 %s 改指向本机 IP %s,用户的订阅地址就不变(改好之前用户刷新订阅会失败,已连上的线路照常可用)。\n", notify.Esc(host), notify.Esc(ip))
}

// identityWarnText 身份告警的中文(B 的 identity.go;A0:Code 为 text 的原文)。
func identityWarnText(it PlanItem) string { return it.Args["text"] }

// identityItems 身份告警(B 把 identityResult.Warnings 改成 []PlanItem 之后 = r.Warnings)。
func identityItems(r identityResult) []PlanItem {
	var out []PlanItem
	for _, w := range r.Warnings {
		out = append(out, PlanItem{Code: "text", Args: map[string]string{"text": w}})
	}
	return out
}

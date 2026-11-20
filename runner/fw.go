package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/fw"
	"github.com/Maoyangui/m-ui/guard"
	"github.com/Maoyangui/m-ui/hop"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/safe"

	"gorm.io/gorm"
)

// 防火墙(面板的「防火墙」一栏,设计见 DESIGN_0718.md C 节)在每台服务器上的那一半:
//
//   - 面板规则(fw_rules):拉黑来源 / 端口只给指定来源 —— 每台自己落成内核规则:m-ui 自己的一张 nft 表,
//     没有 nft 用 iptables 自建链(fw.Apply),不动系统防火墙原有的规则。放行端口 —— 系统防火墙开着的机器上,
//     在它那里放行(fw.SyncAllow,只增减 m-ui 自己加的)。都可设到期、可指定服务器。
//     主机的规则随配置快照发到副机,副机存进自己的 fw_rules,之后和主机一样按库对账:主机离线,规则照样到期。
//   - 系统防火墙(ufw / firewalld / iptables / 原生 nftables)现在什么状况,和一个动作:「放开」(可撤销,fw.Open / fw.Undo)。
//     没放开的,m-ui 自己用到的端口自动在它那里放行(设置 fwAutoAllow,默认开)。
//   - 应用层黑名单 guard.Guard(面板、订阅、外部 API 的监听在最外层查它)也由这里拿着,跟同一个定时器重读。
//
// 要轻:每 30 秒那一轮只读库(规则变没变、黑名单谁到期了);真去跑系统命令只在规则变了、有人点了按钮,
// 或者每 5 分钟核对一次(别人清空了规则、重启后 iptables 从存档恢复,要补回去)。
//
// 两条保命的:集群里别的服务器、主机实际连过来的地址,在 m-ui 自己用的端口上永远不受面板规则影响(fw.KindTrust);
// 把自己关在门外了,在那台机器上执行 m-ui firewall pause,这台就不再落任何面板规则(设置 fwPaused)。

const (
	fwTickEvery    = 30 * time.Second
	fwRecheckEvery = 5 * time.Minute
	fwOpenedKey    = "fwOpened"    // 本机「放开系统防火墙」的记录(fw.Opened 的 JSON);空 = 没放开过
	fwAllowedKey   = "fwAllowed"   // 本机系统防火墙里由 m-ui 加的放行(fw.Allowed 的 JSON)
	fwAutoAllowKey = "fwAutoAllow" // 系统防火墙开着时自动放行 m-ui 用到的端口;只有明确设成 false 才不放
	fwAutoBanKey   = "fwAutoBan"   // 自动拉黑;默认关,设成 true 才开
	fwPausedKey    = "fwPaused"    // 本机暂停面板规则(命令行的退路);true = 这台不落拉黑、只允许这两种规则;别的值都算没暂停(见 fwPausedValue)
	fwPeersKey     = "fwPeers"     // 副机:主机最近是从哪些地址连过来的(JSON 数组,最多 4 个)
	fwMaxPeers     = 4
)

type fwSvc struct {
	run     sync.Mutex // 一次只跑一轮对账(里面要等系统命令)
	mu      sync.Mutex // 护着下面这些;只在读写它们的一瞬间拿
	exec    fw.Runner
	state   model.FwState
	sig     string    // 上一次对过账的「规则 + 要放行的端口 + 开关」的指纹
	applied string    // 上一次成功落下去的规则的指纹(只为了在日志里说一声「更新了」)
	checked time.Time // 上一次真去核对系统的时间
	peer    string    // 主机最近一次连过来的地址(只为了不必每次都读库)
}

func (r *Runner) fwExec() fw.Runner {
	r.fw.mu.Lock()
	defer r.fw.mu.Unlock()
	if r.fw.exec == nil {
		r.fw.exec = fw.Exec{}
	}
	return r.fw.exec
}

// SetFwExec 换掉执行系统命令的那一层(测试用:别的包的测试要看防火墙对账的结果时,塞一个假的进来)。
func (r *Runner) SetFwExec(ex fw.Runner) {
	r.fw.mu.Lock()
	r.fw.exec = ex
	r.fw.mu.Unlock()
}

// Guard 应用层黑名单(面板、订阅、外部 API 的监听用)。
func (r *Runner) Guard() *guard.Guard { return r.guard }

func (r *Runner) initGuard() {
	r.guard = guard.New(r.db, func() bool { return strings.EqualFold(r.setting(fwAutoBanKey), "true") }, r.clusterAddr)
}

// fwCluster 集群里每台服务器的地址(公网 v4 / v6、连接地址是 IP 的),加上主机实际连过来的那几个。排好序、去重。
func fwCluster(db *gorm.DB) []netip.Prefix {
	seen := map[netip.Addr]bool{}
	var out []netip.Prefix
	add := func(s string) {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return
		}
		a = a.Unmap().WithZone("")
		if a.IsUnspecified() || a.IsLoopback() || seen[a] {
			return
		}
		seen[a] = true
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	var nodes []model.Node
	db.Select("public_ip, public_ip6, addr").Find(&nodes)
	for _, n := range nodes {
		add(n.PublicIP)
		add(n.PublicIP6)
		add(model.BareHost(n.Addr))
	}
	for _, p := range fwPeersOf(db) {
		add(p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr().Less(out[j].Addr()) })
	return out
}

func fwPeersOf(db *gorm.DB) []string {
	var raw string
	db.Raw("SELECT value FROM settings WHERE key = ?", fwPeersKey).Scan(&raw)
	var out []string
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// clusterAddr 这个地址是不是集群里某台服务器的:自动拉黑永远不碰自己人。
// 每次现查库:只有在某个来源快被拉黑时才会问到这里,不在热路径上。
func (r *Runner) clusterAddr(a netip.Addr) bool {
	for _, p := range fwCluster(r.db) {
		if p.Addr() == a {
			return true
		}
	}
	return false
}

// FwNotePeer 副机:主机刚从这个地址连过来(带对了配对令牌)。记下来,面板规则永远不挡它 ——
// 主机的出口地址不一定是它登记的公网 IP(多 IP 的机器、NAT 后面的主机)。每 5 秒就来一次,地址没变时什么都不做。
func (r *Runner) FwNotePeer(ip string) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return
	}
	a = a.Unmap().WithZone("")
	if a.IsLoopback() || a.IsUnspecified() {
		return
	}
	s := a.String()
	r.fw.mu.Lock()
	same := r.fw.peer == s
	r.fw.peer = s
	r.fw.mu.Unlock()
	if same {
		return
	}
	peers := fwPeersOf(r.db)
	for _, p := range peers {
		if p == s {
			return
		}
	}
	peers = append([]string{s}, peers...)
	if len(peers) > fwMaxPeers {
		peers = peers[:fwMaxPeers]
	}
	b, _ := json.Marshal(peers)
	if r.setSetting(fwPeersKey, string(b)) == nil {
		r.FwKick()
	}
}

// fwLoop 启动后先对一次账(开机时把规则、放开时加的那条补回去),之后每 30 秒一轮。
// 由 keepRunning 带着跑(见 Run):对账里出了意外只记日志、一分钟后重来,不会把面板和数据面带走。
func (r *Runner) fwLoop(stop <-chan struct{}) {
	r.fwTick(true)
	t := time.NewTicker(fwTickEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.fwTick(false)
		}
	}
}

// fwPlan 这台服务器此刻该有的样子。
type fwPlan struct {
	rules []fw.Rule      // 落进 m-ui 自己那张表的:拉黑、只允许;有这两种时前面带上「集群服务器放行」
	allow []fw.PortProto // 「放行端口」的规则要在系统防火墙里放行的
	count int            // 这台上生效着的面板规则(三种都算)
	bad   []string       // 解析不了的(跳过,报在状态里)
}

// fwPlanNow 按库里的规则算:启用着、没到期、应用范围里有这台的才算。ports 是这台上 m-ui 自己用的端口。
func (r *Runner) fwPlanNow(now int64, ports []fw.PortProto) fwPlan {
	var rows []model.FwRule
	r.db.Where("enabled = ?", true).Order("id asc").Find(&rows)
	self := render.LocalNodeID(r.db)
	var p fwPlan
	for _, row := range rows {
		if (row.Expiry > 0 && row.Expiry <= now) || !row.AppliesTo(self) {
			continue
		}
		prs, err := fw.ParsePorts(row.Ports)
		if err != nil {
			p.bad = append(p.bad, fmt.Sprintf("#%d %v", row.Id, err))
			continue
		}
		srcs, err := fw.ParseSources(row.Sources)
		if err != nil {
			p.bad = append(p.bad, fmt.Sprintf("#%d %v", row.Id, err))
			continue
		}
		x := fw.Rule{ID: row.Id, Kind: row.Kind, Proto: row.Proto, Ports: prs, Sources: srcs}
		if err := x.Check(); err != nil {
			p.bad = append(p.bad, fmt.Sprintf("#%d %v", row.Id, err))
			continue
		}
		p.count++
		if x.Kind != fw.KindAllow {
			p.rules = append(p.rules, x)
			continue
		}
		for _, proto := range []string{"tcp", "udp"} {
			if x.Proto != "" && x.Proto != proto {
				continue
			}
			for _, pr := range prs {
				p.allow = append(p.allow, fw.PortProto{Port: pr, Proto: proto})
			}
		}
	}
	if fw.Effective(p.rules) {
		p.rules = append(fwTrust(fwCluster(r.db), ports), p.rules...)
	}
	return p
}

// fwTrust 集群里的服务器在 m-ui 自己用的端口上放行(TCP、UDP 各一条;没有地址或没有端口就没有)。
func fwTrust(cluster []netip.Prefix, ports []fw.PortProto) []fw.Rule {
	if len(cluster) == 0 {
		return nil
	}
	var out []fw.Rule
	for _, proto := range []string{"tcp", "udp"} {
		var prs []fw.PortRange
		for _, p := range ports {
			if p.Proto == proto {
				prs = append(prs, p.Port)
			}
		}
		if len(prs) > 0 {
			out = append(out, fw.Rule{Kind: fw.KindTrust, Proto: proto, Ports: prs, Sources: cluster})
		}
	}
	return out
}

func fwHash(rules []fw.Rule) string {
	sum := sha256.Sum256([]byte(fw.NFTScript(rules)))
	return hex.EncodeToString(sum[:8])
}

func fwSetting(db *gorm.DB, key string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return strings.TrimSpace(v)
}

func fwOpenedOf(db *gorm.DB) (fw.Opened, bool) {
	raw := fwSetting(db, fwOpenedKey)
	if raw == "" {
		return fw.Opened{}, false
	}
	var o fw.Opened
	if json.Unmarshal([]byte(raw), &o) != nil {
		return fw.Opened{}, false
	}
	return o, true
}

func fwAllowedOf(db *gorm.DB) fw.Allowed {
	var a fw.Allowed
	if raw := fwSetting(db, fwAllowedKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &a)
	}
	return a
}

func sameAllowed(a, b fw.Allowed) bool {
	return a.Frontend == b.Frontend && strings.Join(a.Ports, ",") == strings.Join(b.Ports, ",")
}

// fwTick 一轮对账。force = 不管规则变没变都去核对系统(开机、有人点了按钮、防火墙页要最新状态)。
func (r *Runner) fwTick(force bool) model.FwState {
	r.fw.run.Lock()
	defer r.fw.run.Unlock()
	now := time.Now()
	if !r.IsNode() {
		// 到期的规则只有主机删(主机那份是底):删掉后快照的修订号变了,副机跟着收到。副机自己靠 Expiry 提前不再生效
		r.db.Where("expiry > 0 AND expiry <= ?", now.Unix()).Delete(&model.FwRule{})
	}
	if r.guard != nil {
		r.guard.Reload() // 黑名单到期靠它生效
	}
	ports := r.fwPorts()
	plan := r.fwPlanNow(now.Unix(), ports)
	pausedRaw := r.setting(fwPausedKey)
	paused := strings.EqualFold(pausedRaw, "true")
	if paused {
		plan.rules = nil // 暂停:拉黑、只允许都不落(放行端口不受影响,那不会把人关在门外)
	}
	autoAllow := !strings.EqualFold(r.setting(fwAutoAllowKey), "false")
	want := append([]fw.PortProto(nil), plan.allow...)
	if autoAllow {
		want = append(want, ports...)
	}
	hash := fwHash(plan.rules)
	// 「放开」的记录也算进去:命令行(m-ui firewall open / undo)改了它,下一轮就要重新看系统,不等 5 分钟
	// 暂停那一项把原值也算进去:命令行每恢复一次存的值都不一样(fwPausedValue),哪怕服务一轮都没赶上「暂停着」,恢复后的这一轮也会重建
	sig := hash + "|" + fmt.Sprint(want) + "|" + fmt.Sprint(paused, autoAllow, len(ports), plan.count, plan.bad) + "|" + fwSetting(r.db, fwOpenedKey) + "|" + pausedRaw

	r.fw.mu.Lock()
	fresh := !force && sig == r.fw.sig && now.Sub(r.fw.checked) < fwRecheckEvery
	st0, applied := r.fw.state, r.fw.applied
	r.fw.mu.Unlock()
	if fresh {
		return st0
	}

	ex := r.fwExec()
	st := fw.Detect(ex)
	errs := append([]string(nil), plan.bad...)
	// 1) 拉黑、只允许。没有这两种规则、机器上又没有 nft / iptables 的(容器、非 Linux)什么都不做,不算错
	if fw.Effective(plan.rules) || st.Engine != fw.EngineNone {
		if err := fw.Apply(ex, plan.rules); err != nil {
			errs = append(errs, err.Error())
		} else {
			if hash != applied && (fw.Effective(plan.rules) || applied != "") {
				logger.Info("防火墙:面板规则已更新,这台上生效 ", plan.count, " 条")
			}
			applied = hash
		}
	}
	// 2) 放开过系统防火墙的:那条全放行不在了就补回去(重启后 iptables 从发行版的存档恢复,没有它)。
	//    补完还在拦 = 放开之后有人又把系统防火墙开回来了:以现在为准,那条记录作废
	opened, wasOpened := fwOpenedOf(r.db)
	if wasOpened {
		fw.EnsureOpen(ex, opened)
		if st = fw.Detect(ex); st.Blocking {
			_ = r.setSetting(fwOpenedKey, "")
			logger.Info("防火墙:系统防火墙(", st.Frontend, ")又被开回来了,之前「放开」的记录作废")
			opened, wasOpened = fw.Opened{}, false
		}
	}
	// 3) 系统防火墙还拦着:m-ui 用到的端口(开着自动放行时)和「放行端口」的规则,在它那里放行;用不着了的收回
	allow, had := "", fwAllowedOf(r.db)
	if st.Blocking {
		got, err := fw.SyncAllow(ex, st, want, had)
		if !sameAllowed(got, had) {
			b, _ := json.Marshal(got)
			_ = r.setSetting(fwAllowedKey, string(b))
		}
		had = got
		switch {
		case st.Frontend == fw.FrontNFTables && (err != nil || !autoAllow):
			allow = model.FwAllowManual // 用户自己写的 nftables 规则:面板不替他改
		case err != nil:
			allow = model.FwAllowFail
			errs = append(errs, err.Error())
		case !autoAllow:
			allow = model.FwAllowOff
		default:
			allow = model.FwAllowOK
		}
	}
	out := model.FwState{
		Frontend: st.Frontend, Blocking: st.Blocking, Detail: st.Detail,
		Opened: st.Opened || wasOpened, OpenedAt: opened.At, OpenedFrom: opened.Frontend,
		Engine: st.Engine, Rules: plan.count, Paused: paused, Allow: allow, Ports: len(ports), Allowed: len(had.Ports),
		Error: strings.Join(errs, ";"), At: now.Unix(),
	}
	r.fw.mu.Lock()
	r.fw.state, r.fw.sig, r.fw.applied, r.fw.checked = out, sig, applied, now
	r.fw.mu.Unlock()
	return out
}

// FwState 这台的防火墙状况。fresh = 现在就去核对一次(要等系统命令);否则给上一轮量到的(刚启动、第一轮还没量完时 At 为 0)。
func (r *Runner) FwState(fresh bool) model.FwState {
	if fresh {
		return r.fwTick(true)
	}
	r.fw.mu.Lock()
	defer r.fw.mu.Unlock()
	return r.fw.state
}

// FwKick 规则刚改过(主机保存、副机收到新快照):马上对一次账,不等下一个 30 秒。
func (r *Runner) FwKick() { safe.Go("防火墙对账", func() { r.fwTick(false) }) }

// fwOpenDB 放开系统防火墙并把记录写进库。已经放开过的只把那条全放行补回去。did = 这一次真的动了系统。
func fwOpenDB(db *gorm.DB, ex fw.Runner, dataDir string) (o fw.Opened, did bool, err error) {
	if old, was := fwOpenedOf(db); was {
		fw.EnsureOpen(ex, old)
		return old, false, nil
	}
	o, err = fw.Open(ex, dataDir, time.Now())
	if err != nil || len(o.Actions) == 0 {
		return o, false, err
	}
	b, _ := json.Marshal(o)
	if err := saveSettings(db, map[string]string{fwOpenedKey: string(b)}); err != nil {
		return o, true, fmt.Errorf("已经放开,但没能记下来(撤销不了): %w", err)
	}
	return o, true, nil
}

// fwUndoDB 撤销「放开」:按记下的动作反着做。库里没有记录、但 INPUT 最前面有 m-ui 那条全放行的(换过库、从备份还原过),照样删掉。
func fwUndoDB(db *gorm.DB, ex fw.Runner) (o fw.Opened, did bool, err error) {
	o, was := fwOpenedOf(db)
	if !was {
		if !fw.Detect(ex).Opened {
			return o, false, nil
		}
		o = fw.Opened{Frontend: fw.FrontIPTables, Actions: []string{fw.ActIPTables, fw.ActIP6Tables}}
	}
	if err := fw.Undo(ex, o); err != nil {
		return o, false, err
	}
	return o, true, saveSettings(db, map[string]string{fwOpenedKey: ""})
}

// FwOpen 放开这台的系统防火墙(可撤销)。改之前的规则备份在数据目录的 fw-backup-<时间>/ 里。
func (r *Runner) FwOpen() (model.FwState, error) {
	o, did, err := fwOpenDB(r.db, r.fwExec(), filepath.Dir(r.dbPath))
	if did && err == nil {
		logger.Info("防火墙:已放开系统防火墙(", o.Frontend, "),原规则备份在 ", o.Backup)
	}
	return r.fwTick(true), err
}

// FwUndo 撤销「放开」,系统防火墙回到放开之前的样子。
func (r *Runner) FwUndo() (model.FwState, error) {
	o, did, err := fwUndoDB(r.db, r.fwExec())
	if did && err == nil {
		logger.Info("防火墙:已撤销放开,系统防火墙(", o.Frontend, ")恢复")
	}
	return r.fwTick(true), err
}

// fwPorts 这台服务器上 m-ui 要对外开的端口:面板 / 订阅 / 代理面板、部署在这台的线路(连同端口跳跃的范围)、
// 由这台来转的中转入口。按「端口 + 协议」排好序去重,给 fw.SyncAllow 用,也在防火墙页列给人看。
func (r *Runner) fwPorts() []fw.PortProto {
	seen := map[string]bool{}
	var out []fw.PortProto
	add := func(from, to int, proto string) {
		if from < 1 || to > 65535 || from > to {
			return
		}
		k := fmt.Sprintf("%d-%d/%s", from, to, proto)
		if !seen[k] {
			seen[k] = true
			out = append(out, fw.PortProto{Port: fw.PortRange{From: from, To: to}, Proto: proto})
		}
	}
	both := func(p int, network string) {
		if network != "udp" {
			add(p, p, "tcp")
		}
		if network != "tcp" {
			add(p, p, "udp")
		}
	}
	atoi := func(key string, def int) int {
		var n int
		if _, err := fmt.Sscan(strings.TrimSpace(r.setting(key)), &n); err != nil || n < 1 || n > 65535 {
			return def
		}
		return n
	}
	add(atoi("webPort", 2053), atoi("webPort", 2053), "tcp")
	add(atoi("subPort", 2056), atoi("subPort", 2056), "tcp")
	if strings.EqualFold(r.setting("resellerEnabled"), "true") {
		if p := atoi("resellerPort", 0); p > 0 {
			add(p, p, "tcp")
		}
	}
	for _, p := range r.camoFirewallPorts() { // 页面伪装占着 80 / 443 时(设置 camoPorts,用户 N2)
		add(p, p, "tcp")
	}
	self := render.LocalNodeID(r.db)
	var lines []model.Line
	r.db.Where("enabled = ?", true).Find(&lines)
	for _, l := range lines {
		if render.LineOnNode(l, self) && l.Port > 0 {
			switch {
			case render.UDPOnly(l.Protocol):
				both(l.Port, "udp")
			case l.Protocol == "shadowsocks":
				both(l.Port, "")
			default:
				both(l.Port, render.ForwardNetwork(l)) // 走 QUIC 传输的只有 UDP,其余 TCP
			}
			var o struct {
				PortHopping string `json:"port_hopping"`
			}
			if l.Protocol == "hysteria2" && json.Unmarshal(l.Options, &o) == nil && o.PortHopping != "" {
				if a, b, err := hop.ParseRange(o.PortHopping); err == nil {
					add(a, b, "udp")
				}
			}
		}
		for _, f := range model.ParseForwards(l.Forwards) {
			if !f.Off && f.Via == self && f.Managed() && f.Port > 0 {
				both(f.Port, render.ForwardNetwork(l))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port.From != out[j].Port.From {
			return out[i].Port.From < out[j].Port.From
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

// FwPorts 同 fwPorts,给面板看的写法("443/tcp"、"20000-30000/udp")。
func (r *Runner) FwPorts() []string {
	ps := r.fwPorts()
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

// ---- 命令行(m-ui firewall … / m-ui unban …)----
// 不经运行中的服务,直接对本机动手,结果记进库里:服务下一轮对账(30 秒内)就按新的来。

// FwLocal 命令行看到的本机防火墙状况。
type FwLocal struct {
	Status fw.Status
	Opened bool       // 放开过(库里有记录,或 INPUT 最前面有 m-ui 那条全放行)
	Record *fw.Opened // 放开的记录(什么时候、原来是哪种、备份在哪);没有记录为 nil
	Paused bool       // 这台暂停了面板规则
	Rules  int64      // 库里启用着的面板规则条数
	Bans   int64      // 黑名单里还有几条
}

func fwLocalDo(dbPath string, fn func(db *gorm.DB) error) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close(db)
	return fn(db)
}

// FwLocalStatus 本机防火墙现在的样子。
func FwLocalStatus(dbPath string) (FwLocal, error) {
	var out FwLocal
	err := fwLocalDo(dbPath, func(db *gorm.DB) error {
		out.Status = fw.Detect(fw.Exec{})
		if o, was := fwOpenedOf(db); was {
			out.Record, out.Opened = &o, true
		}
		out.Opened = out.Opened || out.Status.Opened
		out.Paused = strings.EqualFold(fwSetting(db, fwPausedKey), "true")
		now := time.Now().Unix()
		db.Model(&model.FwRule{}).Where("enabled = ? AND (expiry = 0 OR expiry > ?)", true, now).Count(&out.Rules)
		db.Model(&model.FwBan{}).Where("expiry = 0 OR expiry > ?", now).Count(&out.Bans)
		return nil
	})
	return out, err
}

// FwLocalPeek 只读地看本机防火墙(m-ui doctor 用):不建库、不迁移、不删到期的黑名单。
// ip 不为空时顺便看它在不在黑名单里(命令行里拿 SSH 连进来的地址问:是不是把自己拉黑了)。
func FwLocalPeek(dbPath, ip string) (out FwLocal, banned bool, err error) {
	db, err := database.OpenReadOnly(dbPath)
	if err != nil {
		return out, false, err
	}
	if sq, e := db.DB(); e == nil {
		defer sq.Close()
	}
	out, banned = fwPeekDB(db, fw.Exec{}, ip, time.Now().Unix())
	return out, banned, nil
}

func fwPeekDB(db *gorm.DB, ex fw.Runner, ip string, now int64) (out FwLocal, banned bool) {
	out.Status = fw.Detect(ex)
	if o, was := fwOpenedOf(db); was {
		out.Record, out.Opened = &o, true
	}
	out.Opened = out.Opened || out.Status.Opened
	out.Paused = strings.EqualFold(fwSetting(db, fwPausedKey), "true")
	db.Model(&model.FwRule{}).Where("enabled = ? AND (expiry = 0 OR expiry > ?)", true, now).Count(&out.Rules)
	var bans []model.FwBan
	db.Where("expiry = 0 OR expiry > ?", now).Find(&bans)
	out.Bans = int64(len(bans))
	if addr, e := netip.ParseAddr(strings.TrimSpace(ip)); e == nil {
		addr = addr.Unmap()
		for _, b := range bans {
			if banCovers(b.Ip, addr) {
				banned = true
				break
			}
		}
	}
	return out, banned
}

// banCovers 黑名单里的一条(IP 或网段)盖不盖得住这个地址。
func banCovers(entry string, addr netip.Addr) bool {
	entry = strings.TrimSpace(entry)
	if p, err := netip.ParsePrefix(entry); err == nil {
		return p.Masked().Contains(addr)
	}
	if a, err := netip.ParseAddr(entry); err == nil {
		return a.Unmap() == addr
	}
	return false
}

// FwTeardown 卸载时防火墙收尾做了什么。
type FwTeardown struct {
	Ports  []string // 从系统防火墙里收回的放行端口(m-ui 自己加的那些)
	Opened bool     // 系统防火墙还处在「m-ui 放开过」的状态:这里不撤,卸载时单独问
	Errs   []string // 没做成的(不挡卸载,如实说)
}

// FwLocalTeardown 卸载时的防火墙收尾。服务已经停了才调:不然 30 秒内又会被对账加回去。
// 撤掉 m-ui 落在内核里的那张表 / 那条链(面板规则、黑名单),把它在系统防火墙(ufw / firewalld / iptables)里自动放行的端口收回来。
// 「放开系统防火墙」不在这里撤销:那是装的时候选的,撤不撤由卸载时问。
func FwLocalTeardown(dbPath string) (out FwTeardown, err error) {
	err = fwLocalDo(dbPath, func(db *gorm.DB) error {
		out = fwTeardownDB(db, fw.Exec{})
		return nil
	})
	return out, err
}

func fwTeardownDB(db *gorm.DB, ex fw.Runner) (out FwTeardown) {
	st := fw.Detect(ex)
	if st.Engine != fw.EngineNone {
		if e := fw.Apply(ex, nil); e != nil {
			out.Errs = append(out.Errs, "撤掉面板的防火墙规则: "+e.Error())
		}
	}
	if had := fwAllowedOf(db); len(had.Ports) > 0 {
		got, e := fw.SyncAllow(ex, st, nil, had)
		if e != nil {
			out.Errs = append(out.Errs, "收回放行的端口: "+e.Error())
		}
		left := map[string]bool{}
		for _, p := range got.Ports {
			left[p] = true
		}
		for _, p := range had.Ports {
			if !left[p] {
				out.Ports = append(out.Ports, p)
			}
		}
		if !sameAllowed(got, had) {
			b, _ := json.Marshal(got)
			_ = saveSettings(db, map[string]string{fwAllowedKey: string(b)})
		}
	}
	_, was := fwOpenedOf(db)
	out.Opened = was || st.Opened
	return out
}

// FwLocalOpen 放开本机的系统防火墙(安装脚本、命令行菜单用)。did = 这一次真的动了系统。
func FwLocalOpen(dbPath string) (o fw.Opened, did bool, err error) {
	err = fwLocalDo(dbPath, func(db *gorm.DB) error {
		var e error
		o, did, e = fwOpenDB(db, fw.Exec{}, filepath.Dir(dbPath))
		return e
	})
	return o, did, err
}

// FwLocalUndo 撤销放开。
func FwLocalUndo(dbPath string) (o fw.Opened, did bool, err error) {
	err = fwLocalDo(dbPath, func(db *gorm.DB) error {
		var e error
		o, did, e = fwUndoDB(db, fw.Exec{})
		return e
	})
	return o, did, err
}

// fwPausedValue 设置 fwPaused 里存什么:暂停 = "true";恢复 = 一个每次都不一样的值(只要不是 true 就算没暂停)。
// 恢复时不存空串:命令行暂停是当场把表撤掉的,要是紧接着就恢复、服务一轮都没赶上,存回空串的话服务看到的设置和撤表之前
// 一模一样,以为规则还在,要等 fwRecheckEvery 那一次核对才补回去(实机测出来的)。存一个新值,对账的签名变了,下一轮必定重建。
func fwPausedValue(on bool) string {
	if on {
		return "true"
	}
	return "resumed-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// FwLocalPause 暂停 / 恢复本机的面板规则。暂停时当场把 m-ui 自己那张表撤掉,不等服务的下一轮。
func FwLocalPause(dbPath string, on bool) error {
	return fwLocalDo(dbPath, func(db *gorm.DB) error {
		if err := saveSettings(db, map[string]string{fwPausedKey: fwPausedValue(on)}); err != nil {
			return err
		}
		if on {
			if ex := (fw.Exec{}); fw.Detect(ex).Engine != fw.EngineNone {
				return fw.Apply(ex, nil)
			}
		}
		return nil
	})
}

// FwLocalUnban 把一个来源移出黑名单(all = 清空);返回移出了几条。运行中的服务 30 秒内重读名单。
func FwLocalUnban(dbPath, ip string, all bool) (n int64, err error) {
	err = fwLocalDo(dbPath, func(db *gorm.DB) error {
		g := guard.New(db, nil, nil)
		if all {
			n = g.Clear(false)
			return nil
		}
		ip = strings.TrimSpace(ip)
		if n = g.Remove(0, ip); n == 0 {
			// 自动拉黑的 IPv6 记的是所在的 /64:给的是其中一个地址也认
			if k, ok := guard.Key(ip); ok {
				s := k.String()
				if k.Addr().Is4() {
					s = k.Addr().String()
				}
				n = g.Remove(0, s)
			}
		}
		return nil
	})
	return n, err
}

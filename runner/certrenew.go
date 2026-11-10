package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/monitor"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/tz"
)

// 证书自动续期。
//
// 每台服务器续自己的证书(证书不随快照同步)。启动几分钟后先查一次,之后每小时查一次证书库;该不该续、能不能续
// 都看库里那一行和证书文件本身,结果(上次尝试、失败原因、连续失败次数、下次重试时间)写回那一行:重启不会让
// 续期从头等起,证书页和主机也都看得见。以前每 12 小时才查一次、启动时不查、失败只记日志、去重只在内存里,
// 进程隔三差五重启的机器上证书一直没续,剩 23 天了面板还是绿的(审计 I2)。
//
//   - 能自动续:面板签发的(acme)、开着自动续期、不是手动 DNS。有效期的最后三分之一(最多 30 天)开始续;
//     失败按 1、2、4 小时退避,最长 6 小时(剩不到 7 天时最长 2 小时),成功后清零。
//   - 不能自动续的照样提醒(只提醒正用着的):手动 DNS 剩 30 天起每天一次,剩 7、3、1 天各升级一次;
//     服务器上已有的证书、关了自动续期的剩 14 天起每天一次;证书文件读不出来的马上提醒。
//   - 通知的去重记录落库(notify.OnceDurable),重启不重复;副机一般没配通知,由主机代发(见 hub.nodeCertAlerts)。

// certFirstCheck 启动后多久做第一次续期检查(别和刚起来的数据面抢 80 端口、拖慢启动);之后每 certCheckEvery 查一次。
// 测试里调短。
var certFirstCheck, certCheckEvery = 3 * time.Minute, time.Hour

// http01Addr HTTP 验证临时监听的地址(测试里换成随机端口)。
var http01Addr = ":80"

const certDay = 24 * time.Hour

// renewMaxBefore 最多提前多久续:90 天的证书剩 30 天时续(Let's Encrypt 建议在有效期约三分之二处续)。
const renewMaxBefore = 30 * certDay

// certLoop 启动 certFirstCheck 后先查一遍证书库,之后每 certCheckEvery 一遍;被别的签发挡住的续期,那边一结束马上补一轮。
func (r *Runner) certLoop(stop <-chan struct{}) {
	first := time.NewTimer(certFirstCheck)
	defer first.Stop()
	select {
	case <-first.C:
	case <-r.cert.kick:
	case <-stop:
		return
	}
	r.renewDue()
	t := time.NewTicker(certCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.renewDue()
		case <-r.cert.kick:
			r.renewDue()
		case <-stop:
			return
		}
	}
}

// kickRenew 让续期检查马上再跑一轮,不等下一个钟头;已经排着一轮就不再排。
func (r *Runner) kickRenew() {
	select {
	case r.cert.kick <- struct{}{}:
	default:
	}
}

// renewBefore 到期前多久开始续:有效期的三分之一,最多 30 天 —— 90 天的证书剩 30 天、64 天的剩约 21 天、
// 45 天的剩 15 天开始续,几天的短证书也不会每小时重签一次。有效期读不出来的按 30 天。
func renewBefore(info acme.CertInfo) time.Duration {
	life := info.Lifetime()
	if life <= 0 || life/3 > renewMaxBefore {
		return renewMaxBefore
	}
	return life / 3
}

// renewBackoff 连续第 fails 次失败之后隔多久再试:1、2、4 小时……最长 6 小时,剩不到 7 天时最长 2 小时。
// 抖动只往后加(至多一成):CA 对同一账户同一域名每小时最多容忍 5 次失败的验证,间隔不能低于一小时。
func renewBackoff(fails int, left time.Duration) time.Duration {
	limit := 6 * time.Hour
	if left <= 7*certDay {
		limit = 2 * time.Hour
	}
	d := time.Hour
	for i := 1; i < fails && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d + rand.N(d/10+1)
}

// 证书不能自动续期的原因(证书页按它写说明)。
const (
	renewManual   = "manual"   // 手动 DNS 验证:每次续期都要人去加 TXT 记录
	renewExternal = "external" // 服务器上已有的证书:由签发它的工具(certbot 等)续
	renewSelfSign = "selfsign" // 自签证书:十年有效,不用续
	renewDisabled = "disabled" // 关了自动续期
	renewMissing  = "missing"  // 证书文件读不出来
)

// RenewState 一张证书的续期情况(证书页每一行显示)。
type RenewState struct {
	Renewable   bool   `json:"renewable"`             // 面板会自动续这张
	Reason      string `json:"reason,omitempty"`      // 不能自动续的原因,见上
	RenewBefore int64  `json:"renewBefore"`           // 到期前多少秒开始续
	RenewAt     int64  `json:"renewAt,omitempty"`     // 从什么时候起续(能自动续的)
	NextCheckAt int64  `json:"nextCheckAt,omitempty"` // 下一次自动尝试不早于这个时间(续期窗口、失败退避都算上)
	Overdue     bool   `json:"overdue,omitempty"`     // 已经到了该续的时候,还没续上
	Warn        string `json:"warn,omitempty"`        // 预见到的问题:HTTP 验证要用的 80 端口被线路占着
}

// renewStateOf 按库里那一行和证书文件判断这张证书能不能自动续、什么时候续。
func renewStateOf(c model.Cert, info acme.CertInfo, now time.Time) RenewState {
	before := renewBefore(info)
	st := RenewState{RenewBefore: int64(before / time.Second)}
	switch {
	case !info.Exists:
		st.Reason = renewMissing
	case c.Source == "selfsign" || info.SelfSigned:
		st.Reason = renewSelfSign
	case c.Source != "acme":
		st.Reason = renewExternal
	case c.Method == acme.MethodManual:
		st.Reason = renewManual
	case !c.AutoRenew:
		st.Reason = renewDisabled
	default:
		st.Renewable = true
		at := info.NotAfter.Add(-before)
		st.RenewAt, st.NextCheckAt = at.Unix(), at.Unix()
		if c.NextRenewAt > st.NextCheckAt {
			st.NextCheckAt = c.NextRenewAt
		}
		st.Overdue = !now.Before(at)
	}
	return st
}

// renewDue 查一遍证书库:进了续期窗口、过了退避期的自动续;不能自动续的、正用着的按需提醒;最后把本机证书清单
// 写回服务器记录(certbot 等外部工具续过的证书,线路编辑框里的剩余天数跟着变)。
func (r *Runner) renewDue() {
	var rows []model.Cert
	if err := r.db.Order("name asc").Find(&rows).Error; err != nil {
		logger.Warning("续期检查读证书库失败: ", err)
		return
	}
	now := time.Now()
	inUse := r.certsInUse()
	busy := false
	for _, c := range rows {
		info := acme.Info(c.CertFile)
		st := renewStateOf(c, info, now)
		if !st.Renewable {
			if inUse[c.Name] {
				r.remindNotRenewable(c, info, st.Reason, now)
			}
			continue
		}
		if busy || now.Unix() < st.NextCheckAt {
			continue
		}
		busy = !r.autoRenew(c, info)
	}
	r.syncLocalCerts()
}

// autoRenew 自动续一张。返回 false 表示有别的签发正在进行(比如有人在等手动 DNS 记录):这次让开,不算失败、
// 不占重试名额,那边一结束就补跑(以前占掉 20 小时的去重名额,当天不再试,也不通知)。
func (r *Runner) autoRenew(c model.Cert, info acme.CertInfo) bool {
	if err := r.startIssueAuto(true); err != nil {
		logger.Info("证书 ", c.Name, " 该续期了,但有别的签发正在进行,等它结束再续")
		return false
	}
	logger.Info("证书 ", c.Name, " 剩余 ", info.DaysLeft, " 天,自动续期")
	r.cert.logf("自动续期 %s:剩余 %d 天", c.Name, info.DaysLeft) // startIssue 刚清过日志,这一行是这次的第一行
	err := r.finishIssue(IssueRequest{Domain: c.Name, Method: c.Method, Staging: c.Staging, AutoRenew: true, Renew: true, Auto: true})
	if err != nil {
		logger.Warning("证书 ", c.Name, " 自动续期失败: ", err)
	}
	return true
}

// noteRenewResult 把这次签发的结果写回证书行:最近一次尝试、成功时间、失败原因、连续失败次数、下次重试时间
// (自动的和手点的都记;用户取消的不算;第一次签就失败的,库里还没有这张、没处记)。自动续期的通知也在这里发。
func (r *Runner) noteRenewResult(req IssueRequest, err error) {
	if errors.Is(err, errIssueCancelled) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Domain))
	var c model.Cert
	if name == "" || r.db.Where("name = ?", name).First(&c).Error != nil || c.Source != "acme" {
		return
	}
	now := time.Now()
	info := acme.Info(c.CertFile)
	upd := map[string]interface{}{"last_renew_at": now.Unix()}
	var next time.Time
	if err == nil {
		upd["last_renew_ok"], upd["last_renew_err"], upd["renew_fails"], upd["next_renew_at"] = now.Unix(), "", 0, 0
	} else {
		next = now.Add(renewBackoff(c.RenewFails+1, info.NotAfter.Sub(now)))
		upd["last_renew_err"], upd["renew_fails"], upd["next_renew_at"] = err.Error(), c.RenewFails+1, next.Unix()
	}
	if e := r.db.Model(&model.Cert{}).Where("id = ?", c.Id).Updates(upd).Error; e != nil {
		logger.Warning("记录证书 ", name, " 的续期结果失败: ", e)
	}
	if err == nil {
		r.notifier.Forget("cert:" + c.Name) // 好了:失败、到期提醒的去重记录一并清掉,下次再出事第一时间说
	}
	if req.Auto {
		r.notifyRenewResult(c, info, err, now, next)
	}
}

// notifyRenewResult 自动续期的通知:成功说一声(之前失败过的说"已恢复");失败第一次马上说,之后每天一次,
// 剩不到 7 天时每次失败都说。c 是这次之前的那一行(RenewFails 是之前连续失败的次数)。
func (r *Runner) notifyRenewResult(c model.Cert, info acme.CertInfo, err error, now, next time.Time) {
	loc := tz.Location(r.setting("timezone"))
	name := notify.Esc(c.Name)
	if err == nil {
		if c.RenewFails > 0 {
			r.notifier.Event("tgOnCert", fmt.Sprintf("🟢 <b>证书续期已恢复</b>:%s(之前连续失败 %d 次),新证书到期 %s", name, c.RenewFails, info.NotAfter.In(loc).Format("2006-01-02")))
		} else {
			r.notifier.Event("tgOnCert", fmt.Sprintf("🟢 <b>证书已自动续期</b>:%s,新证书到期 %s", name, info.NotAfter.In(loc).Format("2006-01-02")))
		}
		return
	}
	key := "cert:" + c.Name + ":renew"
	if c.RenewFails == 0 {
		r.notifier.Forget(key) // 新一轮失败的第一次:一定要说
	}
	left := info.NotAfter.Sub(now)
	if !r.notifier.OnceDurable(key, 23*time.Hour) && left > 7*certDay {
		return
	}
	r.notifier.Event("tgOnCert", fmt.Sprintf("🔴 <b>证书自动续期失败</b>:%s %s,已连续失败 %d 次,%s 再试\n%s",
		name, notify.LeftText(left), c.RenewFails+1, next.In(loc).Format("01-02 15:04"), notify.Esc(err.Error())))
}

// manualStage 手动 DNS 提醒的档位:剩 30 天起一档,7、3、1 天各升一档;换档时马上再提醒一次。
func manualStage(left time.Duration) string {
	switch days := int(left / certDay); {
	case days <= 1:
		return "1"
	case days <= 3:
		return "3"
	case days <= 7:
		return "7"
	}
	return "30"
}

// remindNotRenewable 面板续不了的(正用着的)证书:手动 DNS 剩 30 天起每天提醒,7 / 3 / 1 天各升级一次;
// 服务器上已有的、关了自动续期的剩 14 天起每天提醒;文件读不出来的马上提醒。自签的十年有效,不提醒。
func (r *Runner) remindNotRenewable(c model.Cert, info acme.CertInfo, reason string, now time.Time) {
	if reason == renewMissing {
		if r.notifier.OnceDurable("cert:"+c.Name+":missing", 23*time.Hour) {
			logger.Warning("证书 ", c.Name, " 的文件读不出来: ", info.Error)
			r.notifier.Event("tgOnCert", fmt.Sprintf("🔴 <b>证书文件读不出来</b>:%s(%s)\n%s\n重新签发或换一张;不用了的话到证书页删掉它",
				notify.Esc(c.Name), notify.Esc(c.CertFile), notify.Esc(info.Error)))
		}
		return
	}
	name, key := notify.Esc(c.Name), "cert:"+c.Name+":"
	left := info.NotAfter.Sub(now)
	days := int(left / certDay) // 和证书页、通知里说的"剩 N 天"一个口径
	icon := "🟡"
	if days <= 7 {
		icon = "🔴"
	}
	var text string
	switch reason {
	case renewManual:
		if days > 30 || !r.notifier.OnceDurable(key+"manual:"+manualStage(left), 23*time.Hour) {
			return
		}
		text = fmt.Sprintf("%s <b>证书要手动续期</b>:%s %s。它用手动 DNS 验证,不能自动续期:到证书页点「续期」,按提示加 TXT 记录;改用 Cloudflare 验证重新签发,以后就能自动续期", icon, name, notify.LeftText(left))
	case renewExternal, renewDisabled:
		if days > 14 || !r.notifier.OnceDurable(key+"expiring", 23*time.Hour) {
			return
		}
		why := "它是服务器上已有的证书,由 certbot 等外部工具续期,面板不替它续:请确认已经续上(覆盖原文件后面板自动换用)"
		if reason == renewDisabled {
			why = "它的自动续期已关闭:到证书页打开自动续期,或者点「续期」"
		}
		text = fmt.Sprintf("%s <b>证书快到期</b>:%s %s。%s", icon, name, notify.LeftText(left), why)
	default:
		return
	}
	logger.Warning("证书 ", c.Name, " 需要处理(", reason, "):", notify.LeftText(left))
	r.notifier.Event("tgOnCert", text)
}

// certsInUse 正用着的证书名:线路默认、面板、订阅、代理面板,以及线路单独选的。
func (r *Runner) certsInUse() map[string]bool {
	files := map[string]bool{}
	for _, k := range []string{"certFile", "webCertFile", "subCertFile", "resellerCertFile"} {
		if f := strings.TrimSpace(r.setting(k)); f != "" {
			files[f] = true
		}
	}
	out := map[string]bool{}
	var rows []model.Cert
	r.db.Select("name", "cert_file").Find(&rows)
	for _, c := range rows {
		if files[c.CertFile] {
			out[c.Name] = true
		}
	}
	for name := range r.linesByCert() {
		out[name] = true
	}
	return out
}

// CertDaysInUse 正用着的证书(线路默认、线路单独选的、面板、订阅、代理面板)里剩余天数最少的那张;
// 一张都没有时退回线路默认证书。副机报给主机,服务器页锁图标后的天数就是它(以前只看线路默认证书)。
func (r *Runner) CertDaysInUse() int {
	inUse := r.certsInUse()
	var rows []model.Cert
	r.db.Select("name", "cert_file").Find(&rows)
	best, found := 0, false
	for _, c := range rows {
		if !inUse[c.Name] {
			continue
		}
		if info := acme.Info(c.CertFile); info.Exists && (!found || info.DaysLeft < best) {
			best, found = info.DaysLeft, true
		}
	}
	if !found {
		return r.CertInfo().DaysLeft
	}
	return best
}

// http01Port HTTP 验证用的端口(80;测试里换成随机端口)。
func http01Port() int {
	_, p, _ := net.SplitHostPort(http01Addr)
	n, _ := strconv.Atoi(p)
	return n
}

// port80Line 部署在本机、占着 HTTP 验证端口(TCP 80)的启用线路;没有返回空。只走 UDP 的 hy2 / tuic 不占 TCP。
func (r *Runner) port80Line() string {
	var lines []model.Line
	r.db.Select("id", "name", "protocol", "port", "node_ids").Where("port = ? AND enabled = ?", http01Port(), true).Order("sort asc, id asc").Find(&lines)
	self := r.LocalNodeId()
	for _, l := range lines {
		if !render.UDPOnly(l.Protocol) && render.LineOnNode(l, self) {
			return l.Name
		}
	}
	return ""
}

// errPort80Line HTTP 验证要用的 80 端口被这条线路占着。
func errPort80Line(line string) error {
	return fmt.Errorf("线路「%s」占着 80 端口,HTTP 验证没法进行:把这条线路换个端口,或者改用 DNS 验证重新签发", line)
}

// port80Problem HTTP 验证前先试着占一下 80 端口:被线路占着就点它的名,被别的程序占着也说清楚,都不去打扰 CA。
func (r *Runner) port80Problem() error {
	ln, err := net.Listen("tcp", http01Addr)
	if err == nil {
		ln.Close()
		return nil
	}
	if line := r.port80Line(); line != "" {
		return errPort80Line(line)
	}
	return fmt.Errorf("80 端口被本机其它程序占用(%v),HTTP 验证没法进行:停掉占用 80 端口的程序,或者改用 DNS 验证重新签发", err)
}

// RenewCert 证书库的「续期」按钮:按库里这张的验证方式、测试环境与自动续期设置重签(异步,进度看 CertStatus),
// 不改签发表单的全局默认(以前续一张手动 DNS 证书,之后新签证书的自动续期默认就不勾了)。
func (r *Runner) RenewCert(name string) error {
	var c model.Cert
	if err := r.db.Where("name = ?", name).First(&c).Error; err != nil {
		return errors.New("证书库里没有这张证书")
	}
	switch c.Source {
	case "acme":
	case "selfsign":
		return errors.New("自签证书不用续期,要换地址就重新生成")
	default:
		return errors.New("这是服务器上已有的证书,由签发它的工具(certbot 等)续期,面板续不了")
	}
	return r.IssueCert(IssueRequest{Domain: c.Name, Method: c.Method, Staging: c.Staging, AutoRenew: c.AutoRenew, Renew: true})
}

// SetCertAutoRenew 打开 / 关掉一张证书的自动续期。手动 DNS、服务器上已有的、自签的不能打开,说清楚原因。
// 重新打开时不等退避:马上查一轮,该续的就续。
func (r *Runner) SetCertAutoRenew(name string, on bool) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	var c model.Cert
	if err := r.db.Where("name = ?", name).First(&c).Error; err != nil {
		return errors.New("证书库里没有这张证书")
	}
	if on {
		switch {
		case c.Source == "selfsign":
			return errors.New("自签证书不用续期")
		case c.Source != "acme":
			return errors.New("这是服务器上已有的证书,由签发它的工具(certbot 等)续期,面板不能替它续")
		case c.Method == acme.MethodManual:
			return errors.New("手动 DNS 验证的证书不能自动续期:每次续期都要重新加 TXT 记录。改用 Cloudflare 验证重新签发,以后就能自动续期")
		}
	}
	upd := map[string]interface{}{"auto_renew": on}
	if on {
		upd["next_renew_at"] = 0
	}
	if err := r.db.Model(&model.Cert{}).Where("id = ?", c.Id).Updates(upd).Error; err != nil {
		return err
	}
	r.syncLocalCerts()
	if on {
		r.cert.logf("已打开 %s 的自动续期", name)
		r.kickRenew()
	} else {
		r.cert.logf("已关闭 %s 的自动续期", name)
	}
	return nil
}

// CertAlert 页面顶部要提醒的一张证书:自动续期在失败,或者正用着、面板续不了、快到期 / 已过期 / 文件读不出来。
type CertAlert struct {
	NodeId   uint   `json:"nodeId,omitempty"` // 0 = 本机
	Node     string `json:"node,omitempty"`   // 副机名
	Name     string `json:"name"`
	NotAfter int64  `json:"notAfter,omitempty"`
	Trouble  string `json:"trouble"`            // failing / expiring / missing
	Method   string `json:"method,omitempty"`   // manual:要手动续
	RenewErr string `json:"renewErr,omitempty"` // 自动续期失败的原因
}

// CertAlerts 页面顶部常驻提示用:本机看自己的证书库;主机再看各副机报上来的清单(副机一般没配通知,
// 它的证书出了问题只能在主机面板上看见)。判断口径和主机代副机告警相同(model.CertBrief.Trouble)。
func (r *Runner) CertAlerts() []CertAlert {
	now := time.Now()
	out := []CertAlert{}
	add := func(id uint, node string, b model.CertBrief) {
		if t := b.Trouble(now); t != "" {
			out = append(out, CertAlert{NodeId: id, Node: node, Name: b.Name, NotAfter: b.NotAfter, Trouble: t, Method: b.Method, RenewErr: b.RenewErr})
		}
	}
	for _, b := range r.CertBriefs() {
		add(0, "", b)
	}
	inUse := r.certsInUse()
	var rows []model.Cert
	r.db.Order("name asc").Find(&rows)
	for _, c := range rows {
		if inUse[c.Name] && !acme.Info(c.CertFile).Exists {
			out = append(out, CertAlert{Name: c.Name, Trouble: renewMissing})
		}
	}
	if r.IsNode() {
		return out
	}
	for _, n := range r.remoteCertLists() {
		for _, b := range n.certs {
			add(n.id, n.name, b)
		}
	}
	return out
}

type nodeCerts struct {
	id    uint
	name  string
	certs []model.CertBrief
}

// remoteCertLists 各副机最近报上来的证书清单(主机上才有)。
func (r *Runner) remoteCertLists() []nodeCerts {
	var nodes []model.Node
	r.db.Select("id", "name", "certs").Where("is_local = ?", false).Order("sort asc, id asc").Find(&nodes)
	out := make([]nodeCerts, 0, len(nodes))
	for _, n := range nodes {
		var list []model.CertBrief
		if len(n.Certs) == 0 || json.Unmarshal(n.Certs, &list) != nil {
			continue
		}
		out = append(out, nodeCerts{n.Id, n.Name, list})
	}
	return out
}

// certSummary 日报里的证书一行:各服务器正用着的证书里 30 天内到期的、自动续期在失败的、要手动续的手动 DNS 证书。
func (r *Runner) certSummary() monitor.CertSummary {
	now := time.Now()
	var s monitor.CertSummary
	count := func(list []model.CertBrief) {
		for _, b := range list {
			if b.SelfSigned {
				continue
			}
			soon := int(time.Unix(b.NotAfter, 0).Sub(now)/certDay) <= 30
			if b.Trouble(now) == model.CertFailing {
				s.Failing++
			}
			if !b.InUse && !(b.Source == "" && b.Default) {
				continue
			}
			if soon {
				s.Expiring++
			}
			if soon && b.Source == "acme" && b.Method == acme.MethodManual {
				s.Manual++
			}
		}
	}
	count(r.CertBriefs())
	if !r.IsNode() {
		for _, n := range r.remoteCertLists() {
			count(n.certs)
		}
	}
	return s
}

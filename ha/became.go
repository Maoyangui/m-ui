package ha

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// ---- 面板顶部的一次性提示 ----

// Notice 一条一次性提示(面板顶部显示,点"知道了"清掉;界面按 Kind 翻译)。
type Notice struct {
	Id   string   `json:"id"`
	Kind string   `json:"kind"` // became | formerBack | demoted | joined | switched | switchFailed
	How  string   `json:"how,omitempty"`
	Name string   `json:"name,omitempty"`
	URL  string   `json:"url,omitempty"`
	Host string   `json:"host,omitempty"`
	IP   string   `json:"ip,omitempty"`
	Dns  bool     `json:"dns,omitempty"` // DNS 已经自动改好
	N    int64    `json:"n,omitempty"`
	File string   `json:"file,omitempty"`
	Warn []string `json:"warn,omitempty"`
	At   int64    `json:"at"`
}

func randID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// addNotice 加一条提示(最多留 10 条)。
func (s *Service) addNotice(n Notice) {
	if n.Id == "" {
		n.Id = randID()
	}
	if n.At == 0 {
		n.At = s.now().Unix()
	}
	var list []Notice
	s.getJSON(KeyNotices, &list)
	list = append(list, n)
	if len(list) > 10 {
		list = list[len(list)-10:]
	}
	_ = s.set(map[string]string{KeyNotices: mustJSON(list)})
}

// Notices 还没清掉的提示。
func (s *Service) Notices() []Notice {
	var list []Notice
	s.getJSON(KeyNotices, &list)
	if list == nil {
		list = []Notice{}
	}
	return list
}

// DismissNotice 清掉一条提示(id 为空 = 全清)。
func (s *Service) DismissNotice(id string) error {
	var keep []Notice
	if id != "" {
		for _, n := range s.Notices() {
			if n.Id != id {
				keep = append(keep, n)
			}
		}
	}
	if keep == nil {
		keep = []Notice{}
	}
	return s.set(map[string]string{KeyNotices: mustJSON(keep)})
}

// ---- 刚成为主机 ----

// afterBecame 刚接任 / 刚被设为主机后第一次启动:按设置改 DNS,发通知,面板顶部留一条提示。只做一次。
func (s *Service) afterBecame() {
	if s.IsNode() {
		return
	}
	var b Became
	if !s.getJSON(KeyBecame, &b) || b.Notified {
		return
	}
	b.Notified = true
	if err := s.set(map[string]string{KeyBecame: mustJSON(b)}); err != nil { // 先记下:改 DNS、发通知只做一次
		return
	}
	changes, err := s.updateDNS(b.FormerIP, b.FormerIP6)
	b.Dns = changes
	if err != nil {
		b.DnsErr = err.Error()
		logger.Warning("容灾:自动改 DNS 失败: ", err)
	}
	_ = s.set(map[string]string{KeyBecame: mustJSON(b)})
	ip := s.setting("publicIp")
	if ip == "" {
		ip = s.setting("publicIp6")
	}
	n := Notice{Kind: "became", How: b.How, Name: b.FormerName, URL: s.panelURL(), Host: b.Identity.SubHost, IP: ip,
		Dns: len(b.Dns) > 0, Warn: b.Identity.Warnings}
	s.addNotice(n)
	s.notify(s.becameText(b, ip))
	logger.Info("容灾:本机已成为主机(", b.How, "),纪元 ", b.Epoch)
}

// becameText 成为主机后的 Telegram 通知:会发生了什么、新面板地址、数据以哪台为准、DNS 要不要改。
func (s *Service) becameText(b Became, ip string) string {
	var sb strings.Builder
	self := notify.Esc(s.selfName())
	former := notify.Esc(b.FormerName)
	if b.How == HowTakeover {
		fmt.Fprintf(&sb, "🟠 <b>「%s」已接任为主机</b>(原主机「%s」失联)\n", self, former)
	} else {
		fmt.Fprintf(&sb, "✅ <b>「%s」已成为主机</b>,「%s」已变为副机\n", self, former)
	}
	if u := s.panelURL(); u != "" {
		fmt.Fprintf(&sb, "面板:%s(账号、密码、两步验证不变,需要重新登录)\n", notify.Esc(u))
	}
	if u := s.resellerURL(); u != "" && !strings.EqualFold(s.setting("resellerEnabled"), "false") {
		fmt.Fprintf(&sb, "代理面板:%s\n", notify.Esc(u))
	}
	if b.How == HowTakeover {
		fmt.Fprintf(&sb, "数据截至 %s;之后在原主机上做的改动没带过来,副机线路上的流量已自动补回。\n",
			time.Unix(b.ReplicaAt, 0).In(s.loc()).Format("2006-01-02 15:04:05"))
	}
	sb.WriteString(s.dnsText(b, ip))
	for _, w := range b.Identity.Warnings {
		sb.WriteString("⚠️ " + notify.Esc(w) + "\n")
	}
	if b.How == HowTakeover {
		sb.WriteString("原主机回来后会自动变为副机;想让它重新当主机,在服务器列表里对它「设为主机」。")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// dnsText 订阅地址要不要改 DNS 的那一句。
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

func (s *Service) loc() *time.Location {
	if z := strings.TrimSpace(s.setting("timezone")); z != "" {
		if l, err := time.LoadLocation(z); err == nil {
			return l
		}
	}
	if l, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return l
	}
	return time.Local
}

// ---- 原主机回来 ----

// formerTick 接任后的主机每 30 秒:原主机回来没有?回来了(它认了本机的纪元、变成了副机)就重新启用它那一行,
// 照常当副机管起来,面板顶部和 Telegram 各提示一次。
func (s *Service) formerTick() {
	if s.IsNode() || s.d.Hub == nil || s.Epoch() == 0 {
		return
	}
	var f Former
	if !s.getJSON(KeyFormer, &f) || f.Id == 0 {
		return
	}
	var n model.Node
	if s.d.DB.Where("id = ?", f.Id).Limit(1).Find(&n).Error != nil || n.Id == 0 || n.Enabled {
		_ = s.set(map[string]string{KeyFormer: ""}) // 删掉了(原主机不要了),或管理员自己启用了
		return
	}
	if strings.TrimSpace(n.ApiUrl) == "" || n.Token == "" {
		return // 不知道怎么连它:面板上提示到服务器编辑里补上 API 地址和令牌
	}
	body, rev := s.StateFor(n)
	if rev == "" {
		return
	}
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/state", strings.NewReader(string(body)), int64(len(body)), nil, 10*time.Second, nil); err != nil {
		return // 还没回来,或回来了还当自己是主机(它会在多数副机回 stale 后变为副机)
	}
	if err := s.d.DB.Model(&model.Node{}).Where("id = ?", n.Id).Update("enabled", true).Error; err != nil {
		logger.Warning("容灾:启用原主机那一行失败: ", err)
		return
	}
	_ = s.set(map[string]string{KeyFormer: ""})
	s.invalidate()
	s.rep.kick()
	logger.Info("容灾:原主机 ", n.Name, " 已恢复,作为副机运行")
	s.addNotice(Notice{Kind: "formerBack", Name: n.Name})
	s.notify("🟢 <b>原主机「" + notify.Esc(n.Name) + "」已恢复</b>,已作为副机运行。\n想让它重新当主机,在服务器列表里对它「设为主机」。")
}

// ForgetNode 删除 / 停用服务器时:它要是还记着是失联的原主机,忘掉。
func (s *Service) ForgetNode(id uint) {
	var f Former
	if s.getJSON(KeyFormer, &f) && f.Id == id {
		_ = s.set(map[string]string{KeyFormer: ""})
	}
}

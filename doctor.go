package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/fw"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/runner"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// m-ui doctor / 菜单「一键体检」:面板打不开、节点连不上时,在 SSH 里一条命令看清卡在哪一步、下一步做什么。
// 面板自己查不了「面板打不开」,所以这件事只能放在命令行里。
// 只读:不改库、不改系统;服务在跑、没在跑都能用。输出里不带密码、令牌、订阅地址,可以整段贴给别人看。

type lvl int

const (
	lvOK   lvl = iota // 正常
	lvInfo            // 说明,不用处理
	lvWarn            // 要留意
	lvBad             // 有问题
)

// finding 体检的一条结论。
type finding struct {
	Lvl  lvl
	Text string
	Fix  string   // 怎么办;空 = 不用处理
	More []string // 附带的原始信息(最近几行报错),缩进打印
}

type section struct {
	Title string
	Items []finding
}

func fOK(text string) finding        { return finding{Lvl: lvOK, Text: text} }
func fInfo(text string) finding      { return finding{Lvl: lvInfo, Text: text} }
func fWarn(text, fix string) finding { return finding{Lvl: lvWarn, Text: text, Fix: fix} }
func fBad(text, fix string) finding  { return finding{Lvl: lvBad, Text: text, Fix: fix} }

const fixPorts = "菜单「修改端口 / 路径 / 监听地址」"

// ---- 服务 ----

type svcInfo struct {
	Known    bool   // 问得到 systemd
	Loaded   bool   // 单元文件在
	Active   string // active / activating / inactive / failed …
	Sub      string // running / auto-restart / dead …(auto-restart = 起不来,系统在等着重试)
	Enabled  string // enabled / disabled …
	Restarts int
	Result   string // success / exit-code / oom-kill / signal / start-limit-hit …
	MainPID  int
}

func parseSystemctlShow(out string) svcInfo {
	v := svcInfo{Known: true}
	for _, line := range strings.Split(out, "\n") {
		k, val, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch k {
		case "LoadState":
			v.Loaded = val == "loaded"
		case "ActiveState":
			v.Active = val
		case "SubState":
			v.Sub = val
		case "UnitFileState":
			v.Enabled = val
		case "NRestarts":
			v.Restarts, _ = strconv.Atoi(val)
		case "Result":
			v.Result = val
		case "MainPID":
			v.MainPID, _ = strconv.Atoi(val)
		}
	}
	return v
}

func probeService() svcInfo {
	if runtime.GOOS != "linux" {
		return svcInfo{}
	}
	out, err := exec.Command("systemctl", "show", "m-ui", "--property=LoadState,ActiveState,SubState,UnitFileState,NRestarts,Result,MainPID").Output()
	if err != nil {
		return svcInfo{}
	}
	return parseSystemctlShow(string(out))
}

func checkService(v svcInfo, errLines []string) []finding {
	if !v.Known {
		return []finding{fInfo("这台问不到 systemd(不是 Linux,或没有 systemctl),服务状态看不了")}
	}
	if !v.Loaded {
		return []finding{fBad("没有找到 m-ui 的服务(m-ui.service)", "重新运行一次安装脚本就能补上,数据不会丢")}
	}
	var out []finding
	switch v.Active {
	case "active":
		out = append(out, fOK("服务在运行"))
		if v.Restarts >= 3 {
			f := fWarn(fmt.Sprintf("服务自动重启过 %d 次,可能在反复出错退出", v.Restarts), "看报错:m-ui logs -e")
			f.More = errLines
			out = append(out, f)
		}
	case "activating", "reloading":
		if v.Sub == "auto-restart" || v.Restarts >= 3 {
			// 一启动就退出,systemd 隔几秒重试一次:这不是「正在启动」,是起不来
			f := fBad(fmt.Sprintf("服务起不来:一启动就退出,系统在反复重试(已经重试 %d 次)", v.Restarts), "原因看下面几行报错(完整的:m-ui logs -e);这次体检里别的 ✗ 多半就是原因")
			f.More = errLines
			out = append(out, f)
			break
		}
		out = append(out, fWarn("服务正在启动(状态 "+v.Active+")", "等几秒再体检一次;一直这样就看报错:m-ui logs -e"))
	default:
		why, fix := "", "启动:m-ui start;起不来就看报错:m-ui logs -e"
		switch v.Result {
		case "exit-code":
			why = ",上次是出错退出的"
		case "oom-kill":
			why, fix = ",上次是内存不够被系统杀掉的", "启动:m-ui start;内存太小就到面板「运维」页加交换空间"
		case "signal", "core-dump":
			why = ",上次是被信号杀掉的"
		case "start-limit-hit":
			why, fix = ",连续启动失败太多次,系统不再重试", "先 systemctl reset-failed m-ui,再 m-ui start;还起不来看报错:m-ui logs -e"
		case "timeout":
			why = ",上次启动超时"
		}
		f := fBad("服务没在运行(状态 "+orDash(v.Active)+why+")", fix)
		f.More = errLines
		out = append(out, f)
	}
	if v.Enabled != "" && v.Enabled != "enabled" && v.Enabled != "enabled-runtime" {
		out = append(out, fWarn("没有设成开机自启("+v.Enabled+"):服务器重启后 m-ui 不会自己起来", "systemctl enable m-ui"))
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// ---- 端口 ----

type portWant struct {
	Name   string // 面板 / 订阅 / 代理面板
	Port   int
	Listen string // 设置里的监听地址;空 = 全部地址
}

type listenerInfo struct {
	Up   bool   // 有进程在监听
	Proc string // 进程名;问不到为空
	PID  int
}

func settingPort(s map[string]string, key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s[key])); err == nil && n >= 1 && n <= 65535 {
		return n
	}
	return def
}

func isTrue(v string) bool  { return strings.EqualFold(strings.TrimSpace(v), "true") }
func isFalse(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "false") }

// resellerListens 代理面板会不会监听:关掉了或本机是副机都不监听(和面板的判断一致)。
func resellerListens(s map[string]string) bool {
	return !isFalse(s["resellerEnabled"]) && !isTrue(s["nodeMode"])
}

// wantedPorts 这台上 m-ui 自己要监听的端口(线路的端口归数据面,不在这里)。
func wantedPorts(s map[string]string) []portWant {
	out := []portWant{
		{"面板", settingPort(s, "webPort", 2053), strings.TrimSpace(s["webListen"])},
		{"订阅", settingPort(s, "subPort", 2056), strings.TrimSpace(s["subListen"])},
	}
	if resellerListens(s) {
		out = append(out, portWant{"代理面板", settingPort(s, "resellerPort", 2054), strings.TrimSpace(s["resellerListen"])})
	}
	return out
}

var reSSUsers = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

// parseSS 从 ss -ltnp 的输出里找监听这个端口的进程;同一个端口有几条(v4 / v6 各一条)时优先报 m-ui 自己。
func parseSS(out string, port int) listenerInfo {
	suffix := ":" + strconv.Itoa(port)
	var got listenerInfo
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "LISTEN" || !strings.HasSuffix(f[3], suffix) {
			continue
		}
		li := listenerInfo{Up: true}
		if m := reSSUsers.FindStringSubmatch(line); m != nil {
			li.Proc = m[1]
			li.PID, _ = strconv.Atoi(m[2])
		}
		if li.Proc == "m-ui" {
			return li
		}
		if !got.Up {
			got = li
		}
	}
	return got
}

// probeListeners 谁在监听:有 ss 用 ss(看得到进程名),没有就连一下本机看通不通。
func probeListeners() func(portWant) listenerInfo {
	var out []byte
	err := errors.New("no ss")
	if runtime.GOOS == "linux" {
		if out, err = exec.Command("ss", "-H", "-ltnp").Output(); err != nil {
			out, err = exec.Command("ss", "-ltnp").Output()
		}
	}
	return func(w portWant) listenerInfo {
		if err == nil {
			return parseSS(string(out), w.Port)
		}
		c, e := net.DialTimeout("tcp", net.JoinHostPort(selfupdate.LocalHost(w.Listen), strconv.Itoa(w.Port)), time.Second)
		if e != nil {
			return listenerInfo{}
		}
		c.Close()
		return listenerInfo{Up: true}
	}
}

func normIP(s string) string {
	a, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(s), "[]"))
	if err != nil {
		return ""
	}
	return a.Unmap().String()
}

func isLoopbackListen(l string) bool {
	l = strings.Trim(strings.TrimSpace(l), "[]")
	if strings.EqualFold(l, "localhost") {
		return true
	}
	ip := net.ParseIP(l)
	return ip != nil && ip.IsLoopback()
}

// checkPorts 面板 / 订阅 / 代理面板各自的端口:设置里有没有撞、监听地址在不在这台上、现在是谁在听。
// localAddrs 是这台网卡上的地址(判断监听地址填错了没有);mainPID 是服务的主进程号。
func checkPorts(wants []portWant, look func(portWant) listenerInfo, svcActive bool, mainPID int, localAddrs []string) []finding {
	var out []finding
	seen := map[int]string{}
	for _, w := range wants {
		if other, dup := seen[w.Port]; dup {
			out = append(out, fBad(fmt.Sprintf("%s和%s的端口都是 %d:后起来的那个监听不上", other, w.Name, w.Port), "改掉其中一个:"+fixPorts))
			continue
		}
		seen[w.Port] = w.Name
	}
	local := map[string]bool{}
	for _, a := range localAddrs {
		if n := normIP(a); n != "" {
			local[n] = true
		}
	}
	for _, w := range wants {
		if ip := normIP(w.Listen); ip != "" && len(local) > 0 && !local[ip] {
			if a, _ := netip.ParseAddr(ip); !a.IsUnspecified() && !a.IsLoopback() {
				out = append(out, fBad(fmt.Sprintf("%s的监听地址 %s 不在这台机器的网卡上:%s起不来", w.Name, w.Listen, w.Name),
					"把监听地址清空(= 全部地址)或改成这台的地址:"+fixPorts))
				continue
			}
		}
		li := look(w)
		mine := li.Up && (li.Proc == "" || li.Proc == "m-ui" || (mainPID > 0 && li.PID == mainPID))
		where := ""
		if w.Listen != "" {
			where = "(只在 " + w.Listen + " 上)"
		}
		switch {
		case li.Up && !mine:
			out = append(out, fBad(fmt.Sprintf("%s端口 %d 被别的程序占着(%s,进程号 %d):m-ui 监听不上", w.Name, w.Port, li.Proc, li.PID),
				"停掉那个程序,或给"+w.Name+"换个端口:"+fixPorts))
		case li.Up:
			out = append(out, fOK(fmt.Sprintf("%s %d/tcp 在监听%s", w.Name, w.Port, where)))
		case svcActive:
			out = append(out, fBad(fmt.Sprintf("%s端口 %d 没在监听(服务在运行)", w.Name, w.Port),
				"刚改过端口或监听地址的话重启一次:m-ui restart;还不行看报错:m-ui logs -e"))
		}
		if w.Name == "面板" && isLoopbackListen(w.Listen) {
			out = append(out, fInfo("面板只监听本机("+w.Listen+"):外面直接打不开,要经反向代理或 SSH 隧道。想直接打开,把监听地址清空:"+fixPorts))
		}
	}
	return out
}

// ---- 面板与数据面 ----

// healthInfo 是面板 api/health 的返回(只对本机开放)。
type healthInfo struct {
	OK       bool `json:"ok"`
	Core     bool `json:"core"`
	Inbounds int  `json:"inbounds"`
	Lines    int  `json:"lines"`
	Reload   struct {
		At    int64  `json:"at"`
		Op    string `json:"op"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"reload"`
	Version   string `json:"version"`
	Role      string `json:"role"`
	StartedAt int64  `json:"startedAt"`
	Paired    bool   `json:"paired"`
	HubSeenAt int64  `json:"hubSeenAt"`
	Nodes     int    `json:"nodes"`
	NodesDown int    `json:"nodesDown"`
}

type panelProbe struct {
	URL    string
	Err    error       // 首页连不上
	Code   int         // 首页的状态码
	Health *healthInfo // nil = 健康接口没有(旧版本)或读不出来
}

func probePanel(url string) panelProbe {
	p := panelProbe{URL: url}
	if url == "" {
		p.Err = errors.New("读不到面板的设置")
		return p
	}
	c := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(url)
	if err != nil {
		p.Err = err
		return p
	}
	resp.Body.Close()
	p.Code = resp.StatusCode
	hr, err := c.Get(url + "api/health")
	if err != nil {
		return p
	}
	defer hr.Body.Close()
	var h healthInfo
	if b, _ := io.ReadAll(io.LimitReader(hr.Body, 64<<10)); json.Unmarshal(b, &h) == nil && h.Version != "" {
		p.Health = &h
	}
	return p
}

// shortErr 把 Go 的网络错误压成一句人话。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝(没有程序在监听)"
	case strings.Contains(s, "Client.Timeout"), strings.Contains(s, "i/o timeout"), strings.Contains(s, "deadline exceeded"):
		return "超时没有应答"
	case strings.Contains(s, "no such host"):
		return "没有这个域名"
	}
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}

// spanText 一段时长的说法:45 秒、12 分钟、3 小时 5 分钟、2 天 4 小时。
func spanText(d time.Duration) string {
	sec := int64(d / time.Second)
	switch {
	case sec < 0:
		return "0 秒"
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分钟", sec/60)
	case sec < 86400:
		if m := sec % 3600 / 60; m > 0 {
			return fmt.Sprintf("%d 小时 %d 分钟", sec/3600, m)
		}
		return fmt.Sprintf("%d 小时", sec/3600)
	}
	if h := sec % 86400 / 3600; h > 0 {
		return fmt.Sprintf("%d 天 %d 小时", sec/86400, h)
	}
	return fmt.Sprintf("%d 天", sec/86400)
}

func checkPanel(p panelProbe, cliVersion string, svcActive bool, now time.Time) []finding {
	if p.Err != nil {
		if !svcActive {
			return nil // 服务没在跑,上面已经说了
		}
		return []finding{fBad("面板在本机打不开("+p.URL+"):"+shortErr(p.Err), "看报错:m-ui logs -e")}
	}
	var out []finding
	if p.Code >= 200 && p.Code < 400 {
		out = append(out, fOK("面板在本机能打开("+p.URL+")"))
	} else {
		out = append(out, fBad(fmt.Sprintf("面板在本机应答 %d(%s)", p.Code, p.URL), "看报错:m-ui logs -e"))
	}
	h := p.Health
	if h == nil {
		return out
	}
	if h.StartedAt > 0 && now.Unix() >= h.StartedAt {
		out = append(out, fInfo("运行中的版本 v"+h.Version+",本次已运行 "+spanText(now.Sub(time.Unix(h.StartedAt, 0)))))
	}
	if cliVersion != "" && h.Version != cliVersion {
		out = append(out, fWarn(fmt.Sprintf("运行中的是 v%s,磁盘上的程序是 v%s:重启之后才换上", h.Version, cliVersion), "m-ui restart"))
	}
	const fixCore = "m-ui render >/dev/null 校验配置,会点出是哪条线路;或到面板「线路」页看报错"
	var f finding
	switch {
	case h.Lines == 0:
		f = fInfo("这台没有部署线路,数据面不用跑")
	case h.OK:
		f = fOK(fmt.Sprintf("数据面正常:%d 条线路都在监听", h.Lines))
	case !h.Core:
		f = fBad(fmt.Sprintf("数据面(sing-box)没在跑:这台的 %d 条线路都连不上", h.Lines), fixCore)
	default:
		f = fBad(fmt.Sprintf("数据面只监听上 %d 个入站,应当有 %d 个:有线路没起来", h.Inbounds, h.Lines), fixCore)
	}
	if h.Reload.At > 0 && !h.Reload.OK && h.Reload.Error != "" && f.Lvl == lvBad {
		f.More = []string{"最近一次重载失败(" + time.Unix(h.Reload.At, 0).Format("01-02 15:04") + "):" + h.Reload.Error}
	}
	return append(out, f)
}

// ---- 多台服务器 ----

func checkRole(s map[string]string, h *healthInfo, now time.Time) []finding {
	var out []finding
	const fixNode = "到主机的「服务器」页看这台的状态(令牌 / 网络 / 证书指纹);主机在升级、重启时会短暂这样"
	if !isTrue(s["nodeMode"]) {
		switch {
		case h != nil && h.Nodes > 0 && h.NodesDown > 0:
			out = append(out, fWarn(fmt.Sprintf("这台是主机:%d 台副机里有 %d 台联系不上", h.Nodes, h.NodesDown), "到面板「服务器」页看是哪台、什么原因"))
		case h != nil && h.Nodes > 0:
			out = append(out, fOK(fmt.Sprintf("这台是主机:%d 台副机都联系得上", h.Nodes)))
		default:
			out = append(out, fInfo("这台是主机"))
		}
	} else {
		paired := strings.TrimSpace(s["hubRevision"]) != ""
		switch {
		case !paired:
			out = append(out, fInfo("这台是副机,还没有主机来接入。接入信息(地址和令牌)在菜单「主机 / 副机」里,填到主机的「服务器」页"))
		case h == nil:
			out = append(out, fInfo("这台是副机,已有主机接入"))
		case h.HubSeenAt > 0 && now.Unix()-h.HubSeenAt <= 600:
			out = append(out, fOK("这台是副机,主机 "+spanText(now.Sub(time.Unix(h.HubSeenAt, 0)))+"前联系过"))
		case h.HubSeenAt > 0:
			out = append(out, fWarn("这台是副机,主机已经 "+spanText(now.Sub(time.Unix(h.HubSeenAt, 0)))+"没联系这台", fixNode))
		case h.StartedAt > 0 && now.Unix()-h.StartedAt > 600:
			out = append(out, fWarn("这台是副机,本次启动以来("+spanText(now.Sub(time.Unix(h.StartedAt, 0)))+")主机还没联系过", fixNode))
		default:
			out = append(out, fInfo("这台是副机,刚启动,等主机来联系"))
		}
	}
	if e, _ := strconv.ParseUint(strings.TrimSpace(s[ha.KeyEpoch]), 10, 64); e > 0 {
		switch strings.TrimSpace(s[ha.KeyMode]) {
		case ha.ModeConfirm:
			out = append(out, fInfo("主机容灾开着(主机失联后先确认再接任)。细节:m-ui ha status"))
		case ha.ModeAuto:
			out = append(out, fInfo("主机容灾开着(主机失联后自动接任)。细节:m-ui ha status"))
		}
	}
	return out
}

// ---- 证书 ----

type certWant struct{ Name, Cert, Key string }

type certState struct {
	Err      error
	NotAfter time.Time
}

// wantedCerts 设置里配着的证书;几处用同一张的合成一行。
func wantedCerts(s map[string]string) []certWant {
	var out []certWant
	add := func(name, c, k string) {
		c, k = strings.TrimSpace(c), strings.TrimSpace(k)
		if c == "" {
			return
		}
		for i := range out {
			if out[i].Cert == c && out[i].Key == k {
				out[i].Name += "、" + name
				return
			}
		}
		out = append(out, certWant{name, c, k})
	}
	add("面板", s["webCertFile"], s["webKeyFile"])
	add("订阅", s["subCertFile"], s["subKeyFile"])
	if resellerListens(s) {
		add("代理面板", s["resellerCertFile"], s["resellerKeyFile"])
	}
	add("线路", s["certFile"], s["keyFile"])
	return out
}

func probeCert(c certWant) certState {
	if c.Key != "" {
		if err := certutil.Verify(c.Cert, c.Key); err != nil {
			return certState{Err: err}
		}
	}
	t, err := certutil.Expiry(c.Cert)
	return certState{Err: err, NotAfter: t}
}

func checkCerts(s map[string]string, stat func(certWant) certState, now time.Time) []finding {
	var out []finding
	if strings.TrimSpace(s["webCertFile"]) == "" || strings.TrimSpace(s["webKeyFile"]) == "" {
		out = append(out, fInfo("面板走的是 HTTP(没配证书):密码明文过网络。建议到面板「证书」页给面板配一张"))
	}
	for _, c := range wantedCerts(s) {
		st := stat(c)
		day := st.NotAfter.Format("2006-01-02")
		left := int(st.NotAfter.Sub(now).Hours() / 24)
		switch {
		case st.Err != nil:
			out = append(out, fBad(c.Name+"的证书用不了:"+st.Err.Error(), "到面板「证书」页重新签发或换一张(面板会先用临时自签证书顶着,浏览器会提示不受信任)"))
		case !st.NotAfter.After(now):
			out = append(out, fBad(c.Name+"的证书已经过期("+day+" 到期)", "到面板「证书」页续期;自动续期失败的原因也写在那里"))
		case left < 15:
			out = append(out, fWarn(fmt.Sprintf("%s的证书还有 %d 天到期(%s)", c.Name, left, day), "正常会自动续期;到面板「证书」页确认自动续期开着、上次没有报错"))
		default:
			out = append(out, fOK(fmt.Sprintf("%s的证书有效,还有 %d 天(到 %s)", c.Name, left, day)))
		}
	}
	return out
}

// ---- 域名 ----

// wantedDomains 面板域名、订阅域名(去重;填的是 IP 就不用查解析)。
func wantedDomains(s map[string]string) []string {
	var out []string
	for _, k := range []string{"webDomain", "subDomain"} {
		d := strings.ToLower(strings.TrimSpace(s[k]))
		if d == "" || normIP(d) != "" {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == d
		}
		if !dup {
			out = append(out, d)
		}
	}
	return out
}

func joinSome(a []string, n int) string {
	if len(a) > n {
		return strings.Join(a[:n], "、") + " 等 " + strconv.Itoa(len(a)) + " 个"
	}
	return strings.Join(a, "、")
}

// publicOnly 留下公网地址(给人看「这台的地址」时不列内网、链路本地的)。
func publicOnly(addrs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range addrs {
		a, err := netip.ParseAddr(normIP(s))
		if err != nil || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		out = append(out, a.String())
	}
	return out
}

// checkDomains 域名解析到的地址是不是这台。self 是这台的地址(设置里的公网 IP、网卡上的、探测到的出口地址)。
func checkDomains(domains []string, lookup func(string) ([]string, error), self []string) []finding {
	mine := map[string]bool{}
	for _, a := range self {
		if n := normIP(a); n != "" {
			mine[n] = true
		}
	}
	var out []finding
	for _, d := range domains {
		addrs, err := lookup(d)
		if err != nil || len(addrs) == 0 {
			why := ""
			if err != nil {
				why = ":" + shortErr(err)
			}
			out = append(out, fBad("域名 "+d+" 解析不出来"+why, "到 DNS 服务商确认有这条记录;刚加的记录等几分钟再试"))
			continue
		}
		hit := ""
		for _, a := range addrs {
			if mine[normIP(a)] {
				hit = normIP(a)
				break
			}
		}
		pub := publicOnly(self)
		switch {
		case hit != "":
			out = append(out, fOK("域名 "+d+" 解析到这台("+hit+")"))
		case len(pub) == 0:
			out = append(out, fInfo("域名 "+d+" 解析到 "+joinSome(addrs, 3)+"(没探测到这台的公网地址,没法比对)"))
		default:
			out = append(out, fWarn("域名 "+d+" 解析到 "+joinSome(addrs, 3)+",不是这台的地址("+joinSome(pub, 3)+")",
				"套了 CDN / 反向代理、或这个域名本来就指向别的服务器的,可以不管;否则到 DNS 服务商把记录改成这台"))
		}
	}
	return out
}

// ---- 系统 ----

// parseDF 取 df -Pk 的第二行:总量与可用(字节)。
func parseDF(out string) (free, total int64, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, 0, false
	}
	t, err1 := strconv.ParseInt(f[1], 10, 64)
	a, err2 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a << 10, t << 10, true
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}

func checkDisk(free, total int64, known bool) []finding {
	if !known || total <= 0 {
		return nil
	}
	const fix = "腾出空间:面板「运维」页有磁盘清理;或 journalctl --vacuum-size=200M 清日志"
	switch {
	case free < 200<<20:
		return []finding{fBad("数据目录所在的磁盘只剩 "+sizeText(free)+"(共 "+sizeText(total)+"):库写不进去,面板会报错、流量记不上", fix)}
	case free < 1<<30 || free*20 < total:
		return []finding{fWarn("数据目录所在的磁盘只剩 "+sizeText(free)+"(共 "+sizeText(total)+")", fix)}
	}
	return []finding{fOK("磁盘还剩 " + sizeText(free) + "(共 " + sizeText(total) + ")")}
}

func checkClock(synced, known bool) []finding {
	if !known {
		return nil
	}
	if synced {
		return []finding{fOK("系统时间已经和网络对过时")}
	}
	return []finding{fWarn("系统时间没有和网络对时:时间差大了,两步验证码会不对,VMess、Reality 这类协议会连不上",
		"timedatectl set-ntp true(没有对时服务的装一个 chrony)")}
}

// ---- 防火墙 ----

type fwProbe struct {
	Skip      string // 看不了的原因(不是 root)
	Status    fw.Status
	Opened    bool
	Paused    bool
	Bans      int64
	SSHIP     string // SSH 连进来的地址(认不出来为空)
	SSHBanned bool
	Ports     []portWant
}

func checkFirewall(p fwProbe) []finding {
	if p.Skip != "" {
		return []finding{fInfo(p.Skip)}
	}
	var out []finding
	ports := make([]string, 0, len(p.Ports))
	for _, w := range p.Ports {
		ports = append(ports, strconv.Itoa(w.Port))
	}
	switch {
	case p.Opened:
		out = append(out, fInfo("系统防火墙已经放开(m-ui 放开的,恢复:m-ui firewall undo)"))
	case p.Status.Blocking && p.Status.Frontend == fw.FrontNFTables:
		out = append(out, fWarn("系统防火墙是手写的 nftables 规则,在拦没放行的入站:m-ui 不会去改它",
			"自己在规则里放行 TCP "+strings.Join(ports, "、")+" 和各条线路的端口;或整个放开:m-ui firewall open(可撤销)"))
	case p.Status.Blocking:
		out = append(out, fInfo("系统防火墙("+frontName(p.Status.Frontend)+")在拦没放行的入站;m-ui 用到的端口会自动在它那里放行"))
	default:
		out = append(out, fInfo("系统防火墙没有在拦入站"))
	}
	if p.Paused {
		out = append(out, fWarn("这台暂停了面板「防火墙」页的规则(拉黑、端口只给指定来源都没在生效)", "把有问题的那条规则改掉之后恢复:m-ui firewall resume"))
	}
	switch {
	case p.SSHIP != "" && p.SSHBanned:
		out = append(out, fBad("你现在的 IP "+p.SSHIP+" 在黑名单里:用它打开面板、订阅、接口都会被拒绝", "移出:m-ui unban "+p.SSHIP))
	case p.SSHIP != "" && p.Bans > 0:
		out = append(out, fOK(fmt.Sprintf("你现在的 IP %s 不在黑名单里(名单里有 %d 条)", p.SSHIP, p.Bans)))
	case p.Bans > 0:
		out = append(out, fInfo(fmt.Sprintf("黑名单里有 %d 条:某个 IP 打不开面板 / 订阅时,先看是不是被拉黑了(m-ui unban <IP>)", p.Bans)))
	}
	out = append(out, fInfo("云厂商的安全组 / 防火墙在这里看不到:上面都正常、外面还是连不上时,到云控制台确认放行了 TCP "+strings.Join(ports, "、")+" 和各条线路的端口"))
	return out
}

var reWhoHost = regexp.MustCompile(`\(([0-9A-Fa-f:.]+)(%[^)]*)?\)\s*$`)

// sshIPFromEnviron 从 /proc/<pid>/environ 的内容(一串以 0 结尾的 KEY=VALUE)里找 SSH 连进来的地址。
func sshIPFromEnviron(data []byte) string {
	for _, kv := range strings.Split(string(data), "\x00") {
		for _, k := range []string{"SSH_CONNECTION=", "SSH_CLIENT="} {
			if strings.HasPrefix(kv, k) {
				if f := strings.Fields(kv[len(k):]); len(f) > 0 {
					if ip := normIP(f[0]); ip != "" {
						return ip
					}
				}
			}
		}
	}
	return ""
}

// procParent /proc/<pid>/stat 里的父进程号(第 4 项;第 2 项是括号里的进程名,里面可以有空格和括号,所以从最后一个右括号往后数)。
func procParent(stat string) int {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// sshClientIP 这次 SSH 是从哪个地址连进来的。先看自己的环境变量;sudo 默认会把它丢掉,所以再顺着父进程往上找
// (sudo、su 上面那个登录的 shell 里还有,root 读得到);最后问 who -m。认不出来返回空。
func sshClientIP() string {
	for _, k := range []string{"SSH_CONNECTION", "SSH_CLIENT"} {
		if f := strings.Fields(os.Getenv(k)); len(f) > 0 {
			if ip := normIP(f[0]); ip != "" {
				return ip
			}
		}
	}
	if runtime.GOOS != "linux" {
		return ""
	}
	for pid, hops := os.Getppid(), 0; pid > 1 && hops < 10; hops++ {
		if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
			if ip := sshIPFromEnviron(data); ip != "" {
				return ip
			}
		}
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			break
		}
		pid = procParent(string(stat))
	}
	out, err := exec.Command("who", "-m").Output()
	if err != nil {
		return ""
	}
	return whoHost(string(out))
}

func whoHost(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if m := reWhoHost.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if ip := normIP(m[1]); ip != "" {
				return ip
			}
		}
	}
	return ""
}

// ---- 日志里的报错 ----

var reLogErr = regexp.MustCompile(`ERROR|FATAL|panic:|启动失败|address already in use`)

// errLinesOf 从日志文本里挑出最近 n 行报错(去掉重复的)。
func errLinesOf(text string, n int) []string {
	var out []string
	seen := map[string]bool{}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || !reLogErr.MatchString(l) {
			continue
		}
		key := l // 去掉日志前缀(时间、主机名、进程号)再比:同一个错反复打的只留最近一条
		if j := strings.Index(key, "]: "); j >= 0 {
			key = key[j+3:]
		}
		if j := strings.Index(key, " - "); j >= 0 {
			key = key[j:]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		for i := range l { // 太长的截掉;按字符边界截,别把一个汉字劈成两半
			if i > 300 {
				l = l[:i] + "…"
				break
			}
		}
		out = append([]string{l}, out...)
	}
	return out
}

func journalText(lines int) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	out, _ := exec.Command("journalctl", "-u", "m-ui", "-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso").Output()
	return string(out)
}

// ---- 汇总与打印 ----

func localAddrs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP.String())
		}
	}
	return out
}

// doctorSections 真的去探一遍,返回各段结论。
func doctorSections(dbPath string) []section {
	now := time.Now()
	root := runtime.GOOS != "linux" || os.Geteuid() == 0
	svc := probeService()
	active := svc.Active == "active"
	var errLines []string
	if svc.Known && (!active || svc.Restarts >= 3) {
		errLines = errLinesOf(journalText(400), 5)
	}
	secs := []section{{"服务", checkService(svc, errLines)}}
	if !root {
		// 数据目录只有 root 进得去,端口是谁在听、防火墙也都要 root 才看得到:别给出一份半截的、看着像出了问题的结果
		secs = append(secs, section{"权限", []finding{fBad("现在不是 root:数据库、端口、防火墙都看不了,体检做不完", "sudo m-ui doctor")}})
		return secs
	}

	st, statErr := os.Stat(dbPath)
	if statErr != nil || st.IsDir() {
		secs = append(secs, section{"数据", []finding{fBad("没有找到数据库 "+dbPath,
			"m-ui 还没装好的话重新运行安装脚本;库在别的位置就用 M_UI_DB=路径 m-ui doctor")}})
		return secs
	}
	s, err := settingsRO(dbPath)
	if err != nil {
		secs = append(secs, section{"数据", []finding{fBad("数据库打不开("+dbPath+"):"+err.Error(), "从备份还原:菜单「还原备份」")}})
		return secs
	}
	wants := wantedPorts(s)
	addrs := localAddrs()
	secs = append(secs, section{"端口", checkPorts(wants, probeListeners(), active, svc.MainPID, addrs)})

	panel := probePanel(localPanelURL(dbPath))
	secs = append(secs, section{"面板与数据面", checkPanel(panel, version, active, now)})
	secs = append(secs, section{"多台服务器", checkRole(s, panel.Health, now)})
	secs = append(secs, section{"证书", checkCerts(s, probeCert, now)})

	if domains := wantedDomains(s); len(domains) > 0 {
		self := append([]string{s["publicIp"], s["publicIp6"]}, addrs...)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if ip := acme.PublicIP(ctx); ip != "" {
			self = append(self, ip)
		}
		cancel()
		lookup := func(host string) ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			return net.DefaultResolver.LookupHost(ctx, host)
		}
		secs = append(secs, section{"域名", checkDomains(domains, lookup, self)})
	}

	fp := fwProbe{Ports: wants, SSHIP: sshClientIP()}
	if l, banned, err := runner.FwLocalPeek(dbPath, fp.SSHIP); err != nil {
		fp.Skip = "防火墙状态读不出来:" + err.Error()
	} else {
		fp.Status, fp.Opened, fp.Paused, fp.Bans, fp.SSHBanned = l.Status, l.Opened, l.Paused, l.Bans, banned
	}
	secs = append(secs, section{"防火墙与黑名单", checkFirewall(fp)})

	sys := []finding{fOK("数据库读得出来(" + dbPath + "," + sizeText(st.Size()) + ")")}
	if runtime.GOOS == "linux" {
		if out, err := exec.Command("df", "-Pk", filepath.Dir(dbPath)).Output(); err == nil {
			free, total, ok := parseDF(string(out))
			sys = append(sys, checkDisk(free, total, ok)...)
		}
		if out, err := exec.Command("timedatectl", "show", "-p", "NTPSynchronized", "--value").Output(); err == nil {
			v := strings.TrimSpace(string(out))
			sys = append(sys, checkClock(v == "yes", v == "yes" || v == "no")...)
		}
	}
	if up := selfupdate.ReadStatus(selfupdate.StatusPath(filepath.Dir(dbPath))); up != nil && !up.OK && up.At > 0 && now.Unix()-up.At < 14*86400 {
		sys = append(sys, fWarn("上次更新("+up.From+" → "+up.To+","+time.Unix(up.At, 0).Format("01-02 15:04")+")没成功:"+up.Message,
			"看当时的报错:journalctl -u m-ui --since \""+time.Unix(up.At, 0).Add(-5*time.Minute).Format("2006-01-02 15:04")+"\";想再试就重新更新"))
	}
	secs = append(secs, section{"系统", sys})
	return secs
}

// countLevels 数一数:正常 / 要留意 / 有问题 各几条。
func countLevels(secs []section) (okN, warnN, badN int) {
	for _, sec := range secs {
		for _, f := range sec.Items {
			switch f.Lvl {
			case lvOK:
				okN++
			case lvWarn:
				warnN++
			case lvBad:
				badN++
			}
		}
	}
	return
}

// printDoctor 把结论打出来。color = 带不带颜色(输出不是终端时不带,方便贴给别人、存成文件)。
func printDoctor(w io.Writer, secs []section, color bool) (badN int) {
	paint := func(c, s string) string {
		if !color {
			return s
		}
		return c + s + colorReset
	}
	mark := map[lvl]string{lvOK: paint(colorGreen, "✓"), lvInfo: paint(colorGray, "·"), lvWarn: paint(colorYellow, "!"), lvBad: paint(colorRed, "✗")}
	fmt.Fprintln(w)
	fmt.Fprintln(w, paint(colorBold, "  m-ui 体检")+paint(colorGray, "  v"+version+"  "+time.Now().Format("2006-01-02 15:04")))
	for _, sec := range secs {
		if len(sec.Items) == 0 {
			continue
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, paint(colorBold, "  "+sec.Title))
		for _, f := range sec.Items {
			text := f.Text
			if f.Lvl == lvBad {
				text = paint(colorRed, text)
			}
			fmt.Fprintln(w, "   "+mark[f.Lvl]+" "+text)
			if f.Fix != "" {
				fmt.Fprintln(w, "     "+paint(colorCyan, "→ ")+f.Fix)
			}
			for _, m := range f.More {
				fmt.Fprintln(w, "       "+paint(colorGray, m))
			}
		}
	}
	okN, warnN, badN := countLevels(secs)
	fmt.Fprintln(w)
	sum := fmt.Sprintf("  结论:%d 项正常", okN)
	if warnN > 0 {
		sum += fmt.Sprintf(",%d 项要留意", warnN)
	}
	if badN > 0 {
		sum += fmt.Sprintf(",%d 项有问题(看带 → 的那几行怎么处理)", badN)
	}
	if warnN == 0 && badN == 0 {
		sum += ",没发现问题"
	}
	c := colorGreen
	if badN > 0 {
		c = colorRed
	} else if warnN > 0 {
		c = colorYellow
	}
	fmt.Fprintln(w, paint(c, sum))
	fmt.Fprintln(w, paint(colorGray, "  上面没有密码、令牌和订阅地址,可以整段贴给别人看(域名、IP 介意的话自己打码)。"))
	return badN
}

// isTerminal 标准输出是不是终端(重定向到文件、管道时不带颜色)。
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}

// runDoctor m-ui doctor / 菜单「一键体检」。返回有问题的条数。
func runDoctor(dbPath string) int {
	return printDoctor(os.Stdout, doctorSections(dbPath), isTerminal(os.Stdout))
}

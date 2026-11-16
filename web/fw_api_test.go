package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// fwCmds 假的系统命令执行器(这台「机器」上 ufw 开着、有 nft);ufw 自己那张放行表也照着记。
type fwCmds struct {
	mu    sync.Mutex
	ufw   bool
	open  []string // ufw 里放行着的:"2053/tcp"
	calls []string
}

func (f *fwCmds) Has(name string) bool { return name == "ufw" || name == "nft" }

func (f *fwCmds) Run(stdin, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	switch {
	case line == "ufw status":
		if f.ufw {
			return "Status: active\n", nil
		}
		return "Status: inactive\n", nil
	case line == "ufw disable":
		f.ufw = false
	case line == "ufw --force enable":
		f.ufw = true
	case line == "ufw show added":
		out := "Added user rules (see 'ufw status' for running firewall):\n"
		for _, p := range f.open {
			out += "ufw allow " + p + " comment 'm-ui'\n"
		}
		return out, nil
	case strings.HasPrefix(line, "ufw allow "):
		f.open = append(f.open, strings.Fields(line)[2])
	case strings.HasPrefix(line, "ufw delete allow "):
		var keep []string
		for _, p := range f.open {
			if p != strings.Fields(line)[3] {
				keep = append(keep, p)
			}
		}
		f.open = keep
	case strings.HasPrefix(line, "firewall-cmd"), strings.HasPrefix(line, "iptables"):
		return "", errors.New("not installed")
	}
	return "", nil
}

// opened ufw 里现在放行着哪些端口。
func (f *fwCmds) opened() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.open, ",")
}

func (f *fwCmds) did(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type fwResp struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	AutoBan   bool   `json:"autoBan"`
	AutoAllow bool   `json:"autoAllow"`
	MyIp      string `json:"myIp"`
	Servers   []struct {
		Id      uint           `json:"id"`
		Name    string         `json:"name"`
		IsLocal bool           `json:"isLocal"`
		Online  bool           `json:"online"`
		State   *model.FwState `json:"state"`
		Ports   []string       `json:"ports"`
	} `json:"servers"`
	Rules    []model.FwRule  `json:"rules"`
	Bans     []model.FwBan   `json:"bans"`
	BanTotal int64           `json:"banTotal"`
	Allow    []model.FwAllow `json:"allow"`
	State    *model.FwState  `json:"state"`
	Ports    []string        `json:"ports"`
}

func fwTestServer(t *testing.T) (*Server, *fwCmds, func(method, path, body string) (int, fwResp)) {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "fw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	cmds := &fwCmds{ufw: true}
	run.SetFwExec(cmds)
	s, db := NewServer(run), run.DB()
	var local model.Node
	if db.Where("is_local = ?", true).First(&local).Error != nil {
		db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1, PublicIP: "198.51.100.10"})
	} else {
		db.Model(&model.Node{}).Where("id = ?", local.Id).Update("public_ip", "198.51.100.10")
	}
	db.Create(&model.Node{Name: "tw", Enabled: true, Sort: 2, PublicIP: "198.51.100.20", PublicIP6: "2001:db8:c::20", ApiUrl: "http://127.0.0.1:1/x/", Token: "tok"})
	do := func(method, path, body string) (int, fwResp) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "http://x/app/api/fw"+path, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.5:40000" // 操作的人现在用的 IP
		s.handleFw(w, r)
		var out fwResp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s 的回复不是 JSON: %s", method, path, w.Body.String())
		}
		return w.Code, out
	}
	return s, cmds, do
}

// 规则的增删改:校验、三种做法、有效期、指定服务器、两条防呆(别把自己关在门外、别拉黑集群里的服务器)。
func TestFwRulesAPI(t *testing.T) {
	s, _, do := fwTestServer(t)
	db := s.db
	code, out := do("GET", "", "")
	if code != 200 || out.MyIp != "203.0.113.5" || !out.AutoAllow || out.AutoBan || len(out.Servers) != 2 || !out.Servers[0].IsLocal || !out.Servers[0].Online {
		t.Fatalf("总览不对: %d %+v", code, out)
	}
	if out.Rules == nil || out.Bans == nil || out.Allow == nil {
		t.Fatal("空表也要是 [](不是 null),界面直接遍历")
	}

	// 拉黑一个来源(整台):写法规整成网段;默认启用、一直有效、所有服务器
	code, out = do("POST", "/rules", `{"kind":"deny","sources":"203.0.113.9、 2001:db8:bad::1","note":"扫描器"}`)
	if code != 200 || len(out.Rules) != 1 {
		t.Fatalf("新建拉黑规则: %d %+v", code, out)
	}
	r1 := out.Rules[0]
	if r1.Kind != "deny" || r1.Sources != "2001:db8:bad::1/128,203.0.113.9/32" || r1.Ports != "" || !r1.Enabled || r1.Expiry != 0 || string(r1.NodeIds) != "null" || r1.Origin != "manual" || r1.Note != "扫描器" || r1.CreatedAt == 0 {
		t.Fatalf("存下来的规则不对: %+v", r1)
	}
	// 端口只给指定来源:带协议、端口区间、有效期、指定服务器
	code, out = do("POST", "/rules", `{"kind":"only","proto":"TCP","ports":"8000-9000, 3306","sources":"198.51.100.7","nodeIds":[2,1],"seconds":3600}`)
	if code != 200 || len(out.Rules) != 2 {
		t.Fatalf("新建只允许规则: %d %+v", code, out)
	}
	r2 := out.Rules[0] // 新的排前面
	if r2.Kind != "only" || r2.Proto != "tcp" || r2.Ports != "3306,8000-9000" || string(r2.NodeIds) != "[1,2]" || r2.Expiry < time.Now().Unix()+3500 {
		t.Fatalf("存下来的规则不对: %+v", r2)
	}
	// 放行端口:不要来源
	code, out = do("POST", "/rules", `{"kind":"allow","proto":"udp","ports":"51820"}`)
	if code != 200 || out.Rules[0].Kind != "allow" || out.Rules[0].Sources != "" || out.Rules[0].Ports != "51820" {
		t.Fatalf("新建放行规则: %d %+v", code, out)
	}
	r3 := out.Rules[0]

	for name, body := range map[string]string{
		"拉黑没填来源":     `{"kind":"deny","ports":"22"}`,
		"只允许没填端口":    `{"kind":"only","sources":"198.51.100.7"}`,
		"只允许没填来源":    `{"kind":"only","ports":"22"}`,
		"放行没填端口":     `{"kind":"allow"}`,
		"放行却填了来源":    `{"kind":"allow","ports":"80","sources":"198.51.100.7"}`,
		"不认识的做法":     `{"kind":"drop","sources":"198.51.100.7"}`,
		"协议不对":       `{"kind":"deny","proto":"icmp","sources":"198.51.100.7"}`,
		"端口不对":       `{"kind":"deny","ports":"70000","sources":"198.51.100.7"}`,
		"来源不是 IP":    `{"kind":"deny","sources":"example.com"}`,
		"来源是全部地址":    `{"kind":"deny","sources":"0.0.0.0/0"}`,
		"拉黑回环":       `{"kind":"deny","sources":"127.0.0.1"}`,
		"服务器不存在":     `{"kind":"deny","sources":"198.51.100.7","nodeIds":[99]}`,
		"有效期为负":      `{"kind":"deny","sources":"198.51.100.7","seconds":-1}`,
		"备注太长":       `{"kind":"deny","sources":"198.51.100.7","note":"` + strings.Repeat("长", 201) + `"}`,
		"拉黑了集群里的服务器": `{"kind":"deny","sources":"198.51.100.0/24"}`,
		"拉黑了集群的 v6":  `{"kind":"deny","sources":"2001:db8:c::/48"}`,
		"请求体不是 JSON": `{`,
	} {
		if code, out := do("POST", "/rules", body); code != 400 || out.Error == "" || out.Code != "" {
			t.Errorf("%s:应该 400,得到 %d %+v", name, code, out)
		}
	}
	if _, out := do("POST", "/rules", `{"kind":"deny","sources":"198.51.100.0/24"}`); !strings.Contains(out.Error, "tw") && !strings.Contains(out.Error, "主机") {
		t.Errorf("拉黑集群服务器的报错要点名是哪台: %q", out.Error)
	}

	// 防呆:来源里有自己现在的 IP → 409,带 force 才存
	code, out = do("POST", "/rules", `{"kind":"deny","sources":"203.0.113.0/24"}`)
	if code != 409 || out.Code != "self_lockout" || !strings.Contains(out.Error, "203.0.113.5") {
		t.Fatalf("拉黑自己应该先拦下: %d %+v", code, out)
	}
	if code, out = do("POST", "/rules", `{"kind":"deny","sources":"203.0.113.0/24","force":true}`); code != 200 || len(out.Rules) != 4 {
		t.Fatalf("确认过的应该存: %d %+v", code, out)
	}
	r4 := out.Rules[0]
	// 防呆:只允许管到了 SSH / 面板端口,名单里却没有自己
	for _, ports := range []string{"22", "2000-3000"} { // 2053 是面板端口
		if code, out = do("POST", "/rules", `{"kind":"only","ports":"`+ports+`","sources":"198.51.100.7"}`); code != 409 || out.Code != "self_lockout" {
			t.Fatalf("只允许 %s 却没把自己算进去,应该先拦下: %d %+v", ports, code, out)
		}
	}
	if code, _ = do("POST", "/rules", `{"kind":"only","ports":"22","sources":"198.51.100.7,203.0.113.5"}`); code != 200 {
		t.Fatalf("名单里有自己就该直接存: %d", code)
	}
	if code, _ = do("POST", "/rules", `{"kind":"only","proto":"udp","ports":"22","sources":"198.51.100.7"}`); code != 200 {
		t.Fatalf("只管 UDP 的碰不到 SSH / 面板: %d", code)
	}
	if code, _ = do("POST", "/rules", `{"kind":"only","ports":"3306","sources":"198.51.100.7"}`); code != 200 {
		t.Fatalf("别的端口不用拦: %d", code)
	}

	// 改:不带 seconds 不动到期时间;带 0 改成一直有效;换来源、换服务器范围
	id2 := strconv.FormatUint(uint64(r2.Id), 10)
	code, out = do("PUT", "/rules/"+id2, `{"kind":"only","proto":"tcp","ports":"3306","sources":"198.51.100.8","note":"数据库"}`)
	if code != 200 {
		t.Fatalf("改规则: %d %+v", code, out)
	}
	// 每次读进一个新的结构体:往带着主键的结构体里再 First,GORM 会把旧主键也当成条件 —— 查不到,留着的还是上一条
	load := func(id uint) model.FwRule {
		t.Helper()
		var r model.FwRule
		if err := db.First(&r, id).Error; err != nil {
			t.Fatal(err)
		}
		return r
	}
	got := load(r2.Id)
	if got.Expiry != r2.Expiry || got.Sources != "198.51.100.8/32" || got.Ports != "3306" || (len(got.NodeIds) != 0 && string(got.NodeIds) != "null") || got.Note != "数据库" || got.CreatedAt != r2.CreatedAt {
		t.Fatalf("改完不对: %+v(原 %+v)", got, r2)
	}
	if code, _ = do("PUT", "/rules/"+id2, `{"kind":"only","proto":"tcp","ports":"3306","sources":"198.51.100.8","seconds":0}`); code != 200 {
		t.Fatal(code)
	}
	got = load(r2.Id)
	if got.Expiry != 0 {
		t.Fatalf("seconds 为 0 应改成一直有效: %+v", got)
	}

	// 开关:关掉不问;重新打开要再过一遍防呆
	id4 := strconv.FormatUint(uint64(r4.Id), 10)
	if code, _ = do("POST", "/rules/"+id4+"/toggle", ``); code != 200 {
		t.Fatal(code)
	}
	got = load(r4.Id)
	if got.Enabled {
		t.Fatal("应该关掉了")
	}
	if code, out = do("POST", "/rules/"+id4+"/toggle", ``); code != 409 || out.Code != "self_lockout" {
		t.Fatalf("重新打开会拉黑自己的规则,应该先拦下: %d %+v", code, out)
	}
	if code, _ = do("POST", "/rules/"+id4+"/toggle", `{"force":true}`); code != 200 {
		t.Fatal(code)
	}
	got = load(r4.Id)
	if !got.Enabled {
		t.Fatal("确认后应该打开")
	}

	// 删
	if code, out = do("DELETE", "/rules/"+strconv.FormatUint(uint64(r3.Id), 10), ``); code != 200 {
		t.Fatalf("删规则: %d %+v", code, out)
	}
	for _, r := range out.Rules {
		if r.Id == r3.Id {
			t.Fatal("删掉的规则还在")
		}
	}
	if code, _ = do("DELETE", "/rules/9999", ``); code != 404 {
		t.Fatalf("不存在的规则应 404: %d", code)
	}
	if code, _ = do("PATCH", "/rules/"+id2, ``); code != 405 {
		t.Fatalf("不支持的方法应 405: %d", code)
	}
	var n int64
	db.Model(&model.Change{}).Where("key = ? AND action LIKE ?", "fw", "rule:%").Count(&n)
	if n < 8 {
		t.Fatalf("规则的改动要进审计: 只有 %d 条", n)
	}
}

// 黑名单(面板 / 订阅 / API 的访问)与白名单;黑名单里的一条可以升成防火墙规则。
func TestFwBansAPI(t *testing.T) {
	s, _, do := fwTestServer(t)
	db := s.db
	code, out := do("POST", "/bans", `{"ip":"203.0.113.77","note":"手动","seconds":7200}`)
	if code != 200 || len(out.Bans) != 1 || out.BanTotal != 1 {
		t.Fatalf("手动拉黑: %d %+v", code, out)
	}
	b := out.Bans[0]
	if b.Ip != "203.0.113.77" || b.Auto || b.Reason != model.BanManual || b.Evidence != "手动" || b.Expiry < time.Now().Unix()+7000 {
		t.Fatalf("黑名单条目不对: %+v", b)
	}
	if !s.guardOf().Blocked("203.0.113.77") || s.guardOf().Blocked("203.0.113.78") {
		t.Fatal("拉黑应当场生效,且只管这一个地址")
	}
	for name, body := range map[string]string{
		"内网地址":    `{"ip":"10.0.0.8"}`,
		"全部地址":    `{"ip":"0.0.0.0/0"}`,
		"不是 IP":   `{"ip":"example.com"}`,
		"一次填了两个":  `{"ip":"203.0.113.1,203.0.113.2"}`,
		"集群里的服务器": `{"ip":"198.51.100.20"}`,
		"有效期不对":   `{"ip":"203.0.113.80","seconds":-5}`,
	} {
		if code, out := do("POST", "/bans", body); code != 400 || out.Error == "" {
			t.Errorf("%s:应该 400,得到 %d %+v", name, code, out)
		}
	}
	// 别把自己拉黑:409,确认了才存
	if code, out = do("POST", "/bans", `{"ip":"203.0.113.0/29"}`); code != 409 || out.Code != "self_lockout" {
		t.Fatalf("拉黑自己应先拦下: %d %+v", code, out)
	}
	if code, _ = do("POST", "/bans", `{"ip":"203.0.113.0/29","force":true}`); code != 200 || !s.guardOf().Blocked("203.0.113.5") {
		t.Fatalf("确认过的应该存: %d", code)
	}

	// 黑名单里的来源:面板最外层直接断开(这里的记录器接不过连接,回 403);没在名单里的照常
	hit := 0
	h := s.guardWrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit++; w.WriteHeader(204) }))
	for ip, want := range map[string]int{"203.0.113.77": 403, "203.0.113.5": 403, "203.0.113.90": 204} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://x/app/", nil)
		r.RemoteAddr = ip + ":50000"
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s 应得到 %d,得到 %d", ip, want, w.Code)
		}
	}
	if hit != 1 {
		t.Fatalf("被拉黑的请求不该走到里面的处理: %d", hit)
	}

	// 升成防火墙规则:来源、剩余时间带过去,标明是从黑名单来的;黑名单里那条还在
	code, out = do("POST", "/bans/"+strconv.FormatUint(uint64(b.Id), 10)+"/promote", `{"nodeIds":[1]}`)
	if code != 200 || len(out.Rules) != 1 {
		t.Fatalf("升成规则: %d %+v", code, out)
	}
	if r := out.Rules[0]; r.Kind != "deny" || r.Sources != "203.0.113.77/32" || r.Origin != "ban" || r.Expiry < time.Now().Unix()+7000 || string(r.NodeIds) != "[1]" || len(out.Bans) != 2 {
		t.Fatalf("升出来的规则不对: %+v", r)
	}

	// 移出;清空
	if code, out = do("DELETE", "/bans/"+strconv.FormatUint(uint64(b.Id), 10), ``); code != 200 || len(out.Bans) != 1 || s.guardOf().Blocked("203.0.113.77") {
		t.Fatalf("移出黑名单: %d %+v", code, out)
	}
	if code, _ = do("DELETE", "/bans/9999", ``); code != 404 {
		t.Fatalf("不在名单里的应 404: %d", code)
	}
	db.Create(&model.FwBan{Ip: "192.0.2.50", Auto: true, Reason: model.BanLogin, Expiry: time.Now().Unix() + 600})
	db.Create(&model.FwBan{Ip: "192.0.2.51", Auto: true, Reason: model.BanSub, Expiry: time.Now().Unix() - 5}) // 已到期:不该列出来
	if _, out = do("GET", "", ""); len(out.Bans) != 2 || out.BanTotal != 2 {
		t.Fatalf("到期的不该列出来: %+v", out.Bans)
	}
	if code, out = do("POST", "/bans/clear", `{"onlyAuto":true}`); code != 200 || len(out.Bans) != 1 || out.Bans[0].Auto {
		t.Fatalf("只清自动拉黑的: %d %+v", code, out.Bans)
	}
	if code, out = do("POST", "/bans/clear", ``); code != 200 || len(out.Bans) != 0 || s.guardOf().Blocked("203.0.113.5") {
		t.Fatalf("清空: %d %+v", code, out.Bans)
	}

	// 白名单:一次一个 IP 或网段;重复加是改备注;删
	if code, out = do("POST", "/allow", `{"ip":"198.51.100.0/24","note":"办公室"}`); code != 200 || len(out.Allow) != 1 || out.Allow[0].Ip != "198.51.100.0/24" {
		t.Fatalf("加白名单: %d %+v", code, out)
	}
	if code, out = do("POST", "/allow", `{"ip":"198.51.100.0/24","note":"办公网"}`); code != 200 || len(out.Allow) != 1 || out.Allow[0].Note != "办公网" {
		t.Fatalf("重复加应改备注: %d %+v", code, out.Allow)
	}
	if code, out = do("POST", "/allow", `{"ip":"198.51.100.9"}`); code != 200 || len(out.Allow) != 2 || out.Allow[0].Ip != "198.51.100.9" {
		t.Fatalf("单个 IP 不带掩码: %d %+v", code, out.Allow)
	}
	for _, body := range []string{`{"ip":"::/0"}`, `{"ip":"a.b"}`, `{"ip":"198.51.100.1 198.51.100.2"}`} {
		if code, _ = do("POST", "/allow", body); code != 400 {
			t.Errorf("%s 应 400: %d", body, code)
		}
	}
	if code, out = do("DELETE", "/allow/"+strconv.FormatUint(uint64(out.Allow[0].Id), 10), ``); code != 200 || len(out.Allow) != 1 {
		t.Fatalf("删白名单: %d %+v", code, out.Allow)
	}
	if code, _ = do("DELETE", "/allow/9999", ``); code != 404 {
		t.Fatal(code)
	}
}

// 两个开关、每台服务器上的动作。
func TestFwServersAPI(t *testing.T) {
	s, cmds, do := fwTestServer(t)
	// 开关:默认 自动拉黑关、自动放行开
	if code, out := do("PUT", "/settings", `{"autoBan":true}`); code != 200 || !out.AutoBan || !out.AutoAllow || s.setting("fwAutoBan") != "true" {
		t.Fatalf("开自动拉黑: %d %+v", code, out)
	}
	if code, out := do("PUT", "/settings", `{"autoAllow":false,"autoBan":false}`); code != 200 || out.AutoBan || out.AutoAllow || s.setting("fwAutoAllow") != "false" {
		t.Fatalf("关两个开关: %d %+v", code, out)
	}
	if code, _ := do("POST", "/settings", `{}`); code != 405 {
		t.Fatal(code)
	}

	// 本机:现在核对一次 —— ufw 开着;自动放行关着,m-ui 不去放行
	code, out := do("POST", "/servers/1/state", ``)
	if code != 200 || out.State == nil || out.State.Frontend != "ufw" || !out.State.Blocking || out.State.Allow != model.FwAllowOff || len(out.Ports) != 2 {
		t.Fatalf("核对本机: %d %+v %+v", code, out.State, out.Ports)
	}
	if cmds.opened() != "" {
		t.Fatalf("自动放行关着不该去放行: %s", cmds.opened())
	}
	// 打开自动放行:面板、订阅端口放行
	if code, _ = do("PUT", "/settings", `{"autoAllow":true}`); code != 200 {
		t.Fatal(code)
	}
	if code, out = do("POST", "/servers/1/state", ``); code != 200 || out.State.Allow != model.FwAllowOK || cmds.opened() != "2053/tcp,2056/tcp" || cmds.did("ufw allow") != 2 {
		t.Fatalf("自动放行: %d %+v(现在放行着 %s,加了 %d 次)", code, out.State, cmds.opened(), cmds.did("ufw allow"))
	}
	// 放开系统防火墙 → 撤销
	if code, out = do("POST", "/servers/1/open", ``); code != 200 || out.Error != "" || !out.State.Opened || out.State.Blocking || cmds.did("ufw disable") != 1 {
		t.Fatalf("放开: %d %+v", code, out)
	}
	if code, out = do("POST", "/servers/1/undo", ``); code != 200 || out.Error != "" || out.State.Opened || !out.State.Blocking || cmds.did("ufw --force enable") != 1 {
		t.Fatalf("撤销: %d %+v", code, out)
	}
	// 总览里本机带着状态和端口
	if _, out = do("GET", "", ""); out.Servers[0].State == nil || out.Servers[0].State.Frontend != "ufw" || len(out.Servers[0].Ports) != 2 || out.Servers[1].State != nil || out.Servers[1].Online {
		t.Fatalf("总览里的服务器不对: %+v", out.Servers)
	}

	// 副机:联系不上给出能看懂的话;停用的不去联系;不存在的 404;不认识的动作 404
	if code, out = do("POST", "/servers/2/open", ``); code != 400 || out.Error == "" {
		t.Fatalf("联系不上的副机: %d %+v", code, out)
	}
	s.db.Model(&model.Node{}).Where("id = ?", 2).Update("enabled", false)
	if code, out = do("POST", "/servers/2/state", ``); code != 400 {
		t.Fatalf("停用的副机: %d %+v", code, out)
	}
	if code, _ = do("POST", "/servers/99/state", ``); code != 404 {
		t.Fatal(code)
	}
	if code, _ = do("POST", "/servers/1/allow", ``); code != 404 {
		t.Fatalf("已经没有 allow 这个动作: %d", code)
	}
	if code, _ = do("GET", "/servers/1/state", ``); code != 405 {
		t.Fatal(code)
	}
	if code, _ = do("GET", "/nope", ``); code != 404 {
		t.Fatal(code)
	}
	var n int64
	s.db.Model(&model.Change{}).Where("key = ? AND action IN ?", "fw", []string{"server:open", "server:undo", "autoban:on", "autoallow:off"}).Count(&n)
	if n != 4 {
		t.Fatalf("开关和放开 / 撤销要进审计(核对状态不用): %d", n)
	}
}

// 副机上的 agent/fw:主机转过来的动作在本机做,回状态和端口。
func TestAgentFw(t *testing.T) {
	s, cmds, _ := fwTestServer(t)
	call := func(method, body string) (int, map[string]json.RawMessage) {
		w := httptest.NewRecorder()
		s.handleAgentFw(w, httptest.NewRequest(method, "http://x/app/api/agent/fw", strings.NewReader(body)))
		var out map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, _ := call("GET", ``); code != 405 {
		t.Fatal(code)
	}
	for _, bad := range []string{`{`, `{"op":"allow"}`, `{"op":""}`} {
		if code, _ := call("POST", bad); code != 400 {
			t.Fatalf("%s 应 400: %d", bad, code)
		}
	}
	code, out := call("POST", `{"op":"open"}`)
	var st model.FwState
	_ = json.Unmarshal(out["state"], &st)
	if code != 200 || !st.Opened || cmds.did("ufw disable") != 1 || string(out["ports"]) == "null" {
		t.Fatalf("副机放开: %d %+v %s", code, st, out["ports"])
	}
	code, out = call("POST", `{"op":"undo"}`)
	var st2 model.FwState // 新的一个:省掉的字段(opened 为假时不带)不能沿用上一次解出来的值
	_ = json.Unmarshal(out["state"], &st2)
	if code != 200 || st2.Opened || !st2.Blocking || cmds.did("ufw --force enable") != 1 {
		t.Fatalf("副机撤销: %d %+v", code, st2)
	}
	if code, _ = call("POST", `{"op":"state"}`); code != 200 {
		t.Fatal(code)
	}
}

// 删服务器:规则的「应用到」里点了它名的把它去掉;只管它一台的规则整条删掉,不能留成「全部服务器」。
func TestFwRulesPrunedOnNodeDelete(t *testing.T) {
	s, _, _ := fwTestServer(t)
	db := s.db
	mk := func(sources string, ids string) uint {
		r := model.FwRule{Kind: "deny", Sources: sources, Enabled: true, Origin: "manual"}
		if ids != "" {
			r.NodeIds = json.RawMessage(ids)
		}
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
		return r.Id
	}
	only2, both, all := mk("203.0.113.1/32", "[2]"), mk("203.0.113.2/32", "[1,2]"), mk("203.0.113.3/32", "")
	w := httptest.NewRecorder()
	s.handleNodeItem(w, httptest.NewRequest("DELETE", "http://x/app/api/nodes/2", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"removedFwRules":1`) {
		t.Fatalf("删服务器: %d %s", w.Code, w.Body.String())
	}
	var left []model.FwRule
	db.Order("id asc").Find(&left)
	if len(left) != 2 || left[0].Id != both || string(left[0].NodeIds) != "[1]" || left[1].Id != all || (len(left[1].NodeIds) != 0 && string(left[1].NodeIds) != "null") {
		t.Fatalf("规则的范围没按删掉的服务器收拾好(只管它一台的 #%d 应删掉): %+v", only2, left)
	}
	// 再加一条新规则:编号不复用删掉的那一个
	if id := mk("203.0.113.4/32", ""); id <= all {
		t.Fatalf("规则编号不该复用: 新的 %d,原来最大 %d", id, all)
	}
}

package web

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// 面板换监听不重启进程:改端口 —— 新端口起来、旧端口收掉;只开 HTTPS(端口不变)—— 原地换成 TLS;新端口被占 —— 报错、旧的照常;
// 设置页保存时回复里带新地址;一样的设置不折腾。
func TestPanelRelisten(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	set := func(k, v string) {
		db.Where("key = ?", k).Delete(&model.Setting{})
		db.Create(&model.Setting{Key: k, Value: v})
	}
	a, b := freePort(t), freePort(t)
	set("webListen", "127.0.0.1")
	set("webPort", strconv.Itoa(a))
	s.panelHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("panel")) })
	pl := s.wantPanelListen()
	ln, err := s.openPanelListener(pl)
	if err != nil {
		t.Fatal(err)
	}
	s.panelMu.Lock()
	s.servePanel(ln, pl)
	s.panelMu.Unlock()
	defer func() { s.Stop() }()

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
	up := func(url string) bool {
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	}
	if !up("http://127.0.0.1:" + strconv.Itoa(a) + "/") {
		t.Fatal("前提:面板在 A 端口")
	}
	if sw := s.planPanelSwitch(10 * time.Millisecond); sw != nil {
		t.Fatalf("设置没变不该换: %+v", sw)
	}

	// 改端口:回复里的新地址、换好后新端口通、旧端口断
	set("webPort", strconv.Itoa(b))
	sw := s.planPanelSwitch(20 * time.Millisecond)
	if sw == nil || sw.Scheme != "http" || sw.Port != b || sw.Path != "/app/" || sw.Host != "" {
		t.Fatalf("新地址不对: %+v", sw)
	}
	if s.panelPending.Load() == nil {
		t.Fatal("换之前要能查到排着的新地址(证书进度靠它)")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !up("http://127.0.0.1:"+strconv.Itoa(b)+"/") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !up("http://127.0.0.1:" + strconv.Itoa(b) + "/") {
		t.Fatal("换好后新端口应能访问")
	}
	time.Sleep(200 * time.Millisecond)
	if up("http://127.0.0.1:" + strconv.Itoa(a) + "/") {
		t.Fatal("旧端口应已收掉")
	}
	if s.panelPending.Load() != nil {
		t.Fatal("换好后排着的新地址应清掉")
	}

	// 端口不变、开 HTTPS:原地换成 TLS,有面板域名就跳到域名
	crt, key := filepath.Join(dir, "p.crt"), filepath.Join(dir, "p.key")
	if err := certutil.GenerateSelfSigned([]string{"panel.test"}, crt, key, 7); err != nil {
		t.Fatal(err)
	}
	set("webCertFile", crt)
	set("webKeyFile", key)
	set("webDomain", "panel.test")
	sw = s.planPanelSwitch(10 * time.Millisecond)
	if sw == nil || sw.Scheme != "https" || sw.Host != "panel.test" || sw.Port != b {
		t.Fatalf("开 HTTPS 的新地址不对: %+v", sw)
	}
	deadline = time.Now().Add(3 * time.Second)
	for !up("https://127.0.0.1:"+strconv.Itoa(b)+"/") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !up("https://127.0.0.1:" + strconv.Itoa(b) + "/") {
		t.Fatal("同一端口应换成 HTTPS")
	}
	if up("http://127.0.0.1:" + strconv.Itoa(b) + "/") {
		t.Fatal("换成 HTTPS 后明文请求不该成功")
	}

	// 新端口被别的程序占着:报错,旧的照常
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	set("webPort", strconv.Itoa(busy.Addr().(*net.TCPAddr).Port))
	if err := s.relistenPanel(); err == nil || !strings.Contains(err.Error(), "还在原地址") {
		t.Fatalf("新端口被占应报错并留在原地址: %v", err)
	}
	if !up("https://127.0.0.1:" + strconv.Itoa(b) + "/") {
		t.Fatal("新端口起不来时旧的应照常可用")
	}
	set("webPort", strconv.Itoa(b))

	// 设置页保存:监听设置真变了才在回复里带 panelSwitch
	c := freePort(t)
	post := func(body map[string]string) map[string]json.RawMessage {
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		s.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/app/api/settings", bytes.NewReader(raw)))
		if rec.Code != 200 {
			t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := post(map[string]string{"timezone": "Asia/Shanghai"}); out["panelSwitch"] != nil || out["note"] != nil {
		t.Fatalf("和监听无关的保存不该带新地址或重启提示: %v", out)
	}
	out := post(map[string]string{"webPort": strconv.Itoa(c)})
	var got panelSwitch
	if json.Unmarshal(out["panelSwitch"], &got) != nil || got.Port != c || got.Scheme != "https" {
		t.Fatalf("改了面板端口,回复应带新地址: %s", out["panelSwitch"])
	}
	deadline = time.Now().Add(4 * time.Second)
	for !up("https://127.0.0.1:"+strconv.Itoa(c)+"/") && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !up("https://127.0.0.1:" + strconv.Itoa(c) + "/") {
		t.Fatal("保存后约 1.5 秒应换到新端口")
	}
}

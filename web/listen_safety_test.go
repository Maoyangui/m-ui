package web

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 审计 A3:监听地址要能在本机绑上;留空 / 0.0.0.0 / :: 表示所有地址。
func TestValidateListenAddr(t *testing.T) {
	for _, ok := range []string{"", " ", "0.0.0.0", "::", "[::]", "127.0.0.1", "localhost"} {
		if err := validateListenAddr("面板", ok); err != nil {
			t.Fatalf("%q 应接受: %v", ok, err)
		}
	}
	// 203.0.113.0/24 是文档保留地址,不会在测试机的网卡上
	for _, bad := range []string{"203.0.113.9", "not-a-host.invalid"} {
		if err := validateListenAddr("面板", bad); err == nil {
			t.Fatalf("%q 绑不上应拒绝", bad)
		}
	}
}

func panelTestServer(t *testing.T) (*Server, func(k, v string)) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	s := &Server{db: db}
	set := func(k, v string) {
		db.Where("key = ?", k).Delete(&model.Setting{})
		db.Create(&model.Setting{Key: k, Value: v})
	}
	s.panelHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("panel")) })
	return s, set
}

func startPanel(t *testing.T, s *Server) {
	t.Helper()
	pl := s.wantPanelListen()
	ln, err := s.openPanelListener(pl)
	if err != nil {
		t.Fatal(err)
	}
	s.panelMu.Lock()
	s.servePanel(ln, pl)
	s.panelMu.Unlock()
	t.Cleanup(func() { s.Stop() })
}

func panelUp(port int) bool {
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func waitPanel(port int) bool {
	for i := 0; i < 60; i++ {
		if panelUp(port) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// 审计 E4:0.0.0.0 → 127.0.0.1、端口不变。以前按「地址变了」先起新监听,Linux 上通配地址还在听时同端口绑不上,
// 换监听必然失败、面板留在对外地址;现在先停旧的再起新的。
func TestPanelRelistenSamePortNewAddress(t *testing.T) {
	s, set := panelTestServer(t)
	port := freePort(t)
	set("webListen", "0.0.0.0")
	set("webPort", strconv.Itoa(port))
	startPanel(t, s)
	if !waitPanel(port) {
		t.Fatal("前提:面板在 0.0.0.0 上")
	}
	set("webListen", "127.0.0.1")
	if err := s.relistenPanel(); err != nil {
		t.Fatalf("同端口只换地址应能换过去: %v", err)
	}
	s.panelMu.Lock()
	addr := s.listener.Addr().String()
	s.panelMu.Unlock()
	if addr != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
		t.Fatalf("现在应只在 127.0.0.1 上听,实际 %s", addr)
	}
	if !waitPanel(port) {
		t.Fatal("换好后本机应能打开面板")
	}
}

// 审计 A3:换监听没成功时,库里的设置要改回实际在用的,下次重启不会按绑不上的设置起。
func TestPanelRelistenFailureRevertsSettings(t *testing.T) {
	s, set := panelTestServer(t)
	port := freePort(t)
	set("webListen", "127.0.0.1")
	set("webPort", strconv.Itoa(port))
	startPanel(t, s)

	// 同端口、本机没有的地址(绕过保存时的校验,直接改库)
	set("webListen", "203.0.113.9")
	if err := s.relistenPanel(); err == nil {
		t.Fatal("绑不上应报错")
	}
	if !waitPanel(port) {
		t.Fatal("应按原样起回原地址")
	}
	if got := s.setting("webListen"); got != "127.0.0.1" {
		t.Fatalf("设置应改回 127.0.0.1,实际 %q", got)
	}

	// 换端口、新端口被别的程序占着
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	set("webPort", strconv.Itoa(busy.Addr().(*net.TCPAddr).Port))
	if err := s.relistenPanel(); err == nil {
		t.Fatal("新端口被占应报错")
	}
	if !panelUp(port) {
		t.Fatal("旧端口应照常")
	}
	if got := s.setting("webPort"); got != strconv.Itoa(port) {
		t.Fatalf("端口设置应改回 %d,实际 %q", port, got)
	}
}

// 审计 A3:保存设置时,本机绑不上的监听地址直接拒绝,不存。
func TestSettingsRejectUnbindableListen(t *testing.T) {
	s, _ := panelTestServer(t)
	for _, key := range []string{"webListen", "subListen", "resellerListen"} {
		raw, _ := json.Marshal(map[string]string{key: "203.0.113.9"})
		rec := httptest.NewRecorder()
		s.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/app/api/settings", bytes.NewReader(raw)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "绑不上") {
			t.Fatalf("%s 填本机没有的地址应被拒: %d %s", key, rec.Code, rec.Body.String())
		}
		if got := s.setting(key); got == "203.0.113.9" {
			t.Fatalf("%s 被拒时不该存下", key)
		}
	}
}

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/runner"
)

// 容灾的设置分类(设计 3.2):本机设置不进副本、接任时保留新主机自己的;其余全局,随副本复制。
// 扫一遍源码里所有读写过的设置键,每个都必须归了类 —— 新加设置时就得想清楚它是哪边的。
func TestEverySettingIsClassified(t *testing.T) {
	re := regexp.MustCompile(`(?:[sS]etting|settingInt|SetSetting|settingOr\(in,)\(\s*"([A-Za-z][A-Za-z0-9]*)"`)
	seen := map[string]string{}
	err := filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == ".git" || n == "node_modules" || n == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if _, ok := seen[m[1]]; !ok {
				seen[m[1]] = p
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 50 {
		t.Fatalf("只扫到 %d 个设置键:读设置的写法变了,这条测试得跟着改", len(seen))
	}
	// 设置页能写的、同步给副机的、开关类、掩码显示的,也都得归类
	for k := range panelSettings {
		seen[k] = "panelSettings"
	}
	for k := range hiddenSettings {
		seen[k] = "hiddenSettings"
	}
	for k := range maskedSettings {
		seen[k] = "maskedSettings"
	}
	for k := range model.BoolSettings {
		seen[k] = "BoolSettings"
	}
	for _, k := range hub.SyncedSettings {
		seen[k] = "SyncedSettings"
		if ha.IsLocal(k) {
			t.Errorf("%s 随快照同步给副机,却归成了本机设置", k)
		}
	}
	for k, at := range seen {
		if !ha.Classified(k) {
			t.Errorf("设置 %s(%s)没有归类:在 ha/settings.go 里加进本机或全局", k, at)
		}
		if ha.IsLocal(k) && ha.IsGlobal(k) {
			t.Errorf("设置 %s 同时归成了本机与全局", k)
		}
	}
	for _, k := range []string{"webPort", "nodeMode", "nodeToken", "hubRevision", "publicIp", ha.KeyEpoch, ha.KeySignKey, hub.LedgerEpochKey} {
		if !ha.IsLocal(k) {
			t.Errorf("%s 应是本机设置", k)
		}
	}
	for _, k := range []string{"apiToken", "totpSecret", "tgToken", hub.LedgerEpochSetting(3), ha.KeyMode, ha.KeyPubPrefix + "2", ha.KeyDnsToken} {
		if !ha.IsGlobal(k) {
			t.Errorf("%s 应是全局设置", k)
		}
	}
}

// GET /api/settings 不发任何 ha 开头的键:签名私钥、集群密钥、Cloudflare 令牌都在里面。
func TestSettingsHideHAKeys(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	s := NewServer(run)
	if err := run.SetSettings(map[string]string{ha.KeySignKey: "secret-seed", ha.KeyClusterKey: "k", ha.KeyDnsToken: "cf-secret", "timezone": "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	tok := s.newSession("admin")
	req := httptest.NewRequest(http.MethodGet, "/app/api/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	w := httptest.NewRecorder()
	s.auth(s.handleSettings)(w, req)
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for k := range out {
		if strings.HasPrefix(k, "ha") {
			t.Errorf("设置接口发出了容灾键 %s", k)
		}
	}
	if out["timezone"] != "Asia/Shanghai" {
		t.Fatalf("普通设置照常返回: %d %s", w.Code, w.Body.String())
	}
}

// agent 鉴权顺序:令牌 → 纪元 → 副机模式。接任后的主机保留了副机令牌,原主机来请求收到 409 stale_epoch 而不是 403;
// 没开过容灾的主机照旧 403;副机响应头带本机纪元(只在用到纪元时)。
func TestAgentAuthEpochOrder(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	s := NewServer(run)
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", s.handleAgent)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	call := func(tok string, epoch uint64, master uint) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/app/api/agent/report", nil)
		req.Header.Set("X-Agent-Token", tok)
		ha.SetHeader(req.Header, epoch, master)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get(ha.HeaderNode)
	}
	// 普通主机,没开过容灾:和以前一样 403,不带纪元响应头
	if code, body, h := call("x", 0, 0); code != http.StatusForbidden || h != "" {
		t.Fatalf("普通主机应 403 且不带纪元头: %d %s %q", code, body, h)
	}
	// 接任后的主机:纪元 2,保留了当副机时的令牌
	if err := run.SetSettings(map[string]string{"nodeToken": "tok", ha.KeyEpoch: "2"}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := call("bad", 1, 1); code != http.StatusUnauthorized {
		t.Fatalf("令牌错先 401: %d %s", code, body)
	}
	code, body, h := call("tok", 1, 1)
	if code != http.StatusConflict || !strings.Contains(body, ha.CodeStale) || h != "2" {
		t.Fatalf("原主机带纪元 1 来请求接任后的主机应 409 stale_epoch: %d %s %q", code, body, h)
	}
	// 切成副机:纪元 0 的老主机(不带请求头)来拉报告 → stale;纪元相同但主机 id 对不上 → stale
	if err := run.SetSettings(map[string]string{"nodeMode": "true", ha.KeyRole: "", ha.KeyMasterId: "4"}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := call("tok", 0, 0); code != http.StatusConflict || !strings.Contains(body, ha.CodeStale) {
		t.Fatalf("不带纪元的老主机应被当成旧主机: %d %s", code, body)
	}
	if code, body, _ := call("tok", 2, 9); code != http.StatusConflict || !strings.Contains(body, ha.CodeStale) {
		t.Fatalf("同纪元不同主机应 stale: %d %s", code, body)
	}
	if code, body, h := call("tok", 2, 4); code != http.StatusOK || h != "2" {
		t.Fatalf("当前主机应放行并带纪元头: %d %s %q", code, body, h)
	}
}

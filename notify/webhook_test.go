package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type hookCall struct {
	body []byte
	sig  string
	ua   string
}

func hookServer(t *testing.T, code int) (*httptest.Server, func() []hookCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []hookCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, hookCall{b, r.Header.Get("X-M-UI-Signature"), r.Header.Get("User-Agent")})
		mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []hookCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]hookCall(nil), calls...)
	}
}

func settings(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func waitCalls(t *testing.T, get func() []hookCall, n int) []hookCall {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c := get(); len(c) >= n {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到 %d 次调用: %d", n, len(get()))
	return nil
}

// 事件推给 Webhook:不依赖 Telegram 有没有配;正文是 JSON,HTML 标记去掉;带密钥就有能验的签名。
func TestWebhookEvent(t *testing.T) {
	srv, calls := hookServer(t, 200)
	n := New(settings(map[string]string{"webhookUrl": srv.URL, "webhookSecret": "s3cret", "tgOnCert": "true"}))
	n.Event("tgOnCert", "🟢 <b>证书已签发</b>:a&lt;b.com")
	got := waitCalls(t, calls, 1)[0]
	var body struct {
		Event string `json:"event"`
		Text  string `json:"text"`
		Time  int64  `json:"time"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Event != "cert" || body.Text != "🟢 证书已签发:a<b.com" || body.Time == 0 {
		t.Fatalf("正文: %+v", body)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(got.body)
	if got.sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("签名: %s", got.sig)
	}
	if got.ua != "m-ui" {
		t.Fatalf("User-Agent: %s", got.ua)
	}
}

func TestWebhookRespectsToggleAndSecretOptional(t *testing.T) {
	srv, calls := hookServer(t, 200)
	n := New(settings(map[string]string{"webhookUrl": srv.URL, "tgOnLogin": "false"}))
	n.Event("tgOnLogin", "x")
	time.Sleep(100 * time.Millisecond)
	if c := calls(); len(c) != 0 {
		t.Fatalf("事件开关关了就不发: %d", len(c))
	}
	n.Event("tgOnCore", "y")
	if c := waitCalls(t, calls, 1); c[0].sig != "" {
		t.Fatalf("没配密钥就不带签名: %s", c[0].sig)
	}
}

// 没配 Webhook 时什么都不发;只配 Webhook 时「测试」不要求 Telegram;对方报错要说出来。
func TestWebhookTest(t *testing.T) {
	srv, calls := hookServer(t, 200)
	n := New(settings(map[string]string{"webhookUrl": srv.URL}))
	if err := n.Test(); err != nil {
		t.Fatalf("只配 Webhook 也能测: %v", err)
	}
	if c := calls(); len(c) != 1 || !strings.Contains(string(c[0].body), `"event":"test"`) {
		t.Fatalf("测试消息: %v", c)
	}
	bad, _ := hookServer(t, 500)
	n = New(settings(map[string]string{"webhookUrl": bad.URL}))
	if err := n.Test(); err == nil || !strings.Contains(err.Error(), "对方返回 500") {
		t.Fatalf("对方报错要说出来: %v", err)
	}
	n = New(settings(map[string]string{}))
	if err := n.Test(); err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("什么都没配: %v", err)
	}
}

// 长轮询:解析消息;网络错误里不能带 Token(错误会进日志)。
func TestPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTK/getUpdates" || r.URL.Query().Get("offset") != "7" || r.URL.Query().Get("timeout") != "3" {
			t.Errorf("请求: %s", r.URL)
		}
		w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"message":{"text":"/status","chat":{"id":-100123}}},{"update_id":8}]}`))
	}))
	defer srv.Close()
	n := New(settings(map[string]string{"tgToken": "TK", "tgApiBase": srv.URL + "/"}))
	ups, err := n.Poll(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].ID != 7 || ups[0].ChatID != "-100123" || ups[0].Text != "/status" || ups[1].Text != "" {
		t.Fatalf("解析: %+v", ups)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.URL
	dead.Close()
	n = New(settings(map[string]string{"tgToken": "SECRET-TOKEN", "tgApiBase": addr}))
	if _, err := n.Poll(context.Background(), 0, 0); err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("错误里不能有 Token: %v", err)
	}

	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"ok":false,"description":"Conflict"}`))
	}))
	defer refused.Close()
	n = New(settings(map[string]string{"tgToken": "TK", "tgApiBase": refused.URL}))
	if _, err := n.Poll(context.Background(), 0, 0); err == nil || !strings.Contains(err.Error(), "Conflict") {
		t.Fatalf("接口报错要带原因: %v", err)
	}
}

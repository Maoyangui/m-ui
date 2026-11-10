package ha

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 假的 Cloudflare:一个区域、几条 A / AAAA 记录,记下改了哪条、改成了什么。
type fakeCF struct {
	mu      sync.Mutex
	srv     *httptest.Server
	records map[string]map[string]interface{} // id → 记录
	patches []string
	auth    string
}

func newFakeCF(t *testing.T) *fakeCF {
	f := &fakeCF{records: map[string]map[string]interface{}{
		"r1": {"id": "r1", "type": "A", "name": "sub.example.com", "content": "198.51.100.1", "proxied": true, "ttl": 1},
		"r2": {"id": "r2", "type": "A", "name": "sub.example.com", "content": "198.51.100.99", "proxied": false, "ttl": 300},
		"r3": {"id": "r3", "type": "AAAA", "name": "sub.example.com", "content": "2001:db8::1", "proxied": false, "ttl": 60},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCF) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	ok := func(result interface{}) {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "result": result})
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/zones":
		if strings.HasSuffix(r.URL.Query().Get("name"), "example.com") && r.URL.Query().Get("name") == "example.com" {
			ok([]map[string]string{{"id": "z1", "name": "example.com"}})
			return
		}
		ok([]map[string]string{})
	case r.Method == http.MethodGet && r.URL.Path == "/zones/z1/dns_records":
		var out []map[string]interface{}
		for _, rec := range f.records {
			if rec["name"] == r.URL.Query().Get("name") && rec["type"] == r.URL.Query().Get("type") {
				out = append(out, rec)
			}
		}
		ok(out)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/zones/z1/dns_records/")
		b, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(b, &body)
		f.patches = append(f.patches, id+" "+string(b))
		if rec, found := f.records[id]; found {
			for k, v := range body {
				rec[k] = v
			}
		}
		ok(f.records[id])
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"success":false,"errors":[{"message":"not found"}]}`))
	}
}

// 主机变了:只把指向原主机 IP 的 A / AAAA 改到本机,别的记录不动,只发 content(橙云、TTL 原样);
// 刚成为主机的那次启动里改,改了哪些写进通知、面板提示与记录。
func TestUpdateDNSAfterBecame(t *testing.T) {
	cf := newFakeCF(t)
	var notes []string
	s := testService(t, false, map[string]string{KeyEpoch: "2", KeyDnsToken: "cf-secret", KeyDnsNames: "sub.example.com",
		"publicIp": "203.0.113.7", "publicIp6": "2001:db8::7"})
	s.d.DNSAPI = cf.srv.URL
	s.d.Notify = func(_, text string) { notes = append(notes, text) }
	s.d.DB.Create(&model.Node{Name: "TW", IsLocal: true, Enabled: true})
	s.set(map[string]string{KeyBecame: mustJSON(Became{How: HowTakeover, Epoch: 2, FormerName: "HK", FormerIP: "198.51.100.1", FormerIP6: "2001:db8::1",
		Identity: identityResult{SubURLKept: true, SubHost: "sub.example.com"}})})
	s.afterBecame()
	cf.mu.Lock()
	patches := append([]string(nil), cf.patches...)
	auth := cf.auth
	cf.mu.Unlock()
	if len(patches) != 2 || !strings.Contains(strings.Join(patches, ";"), `r1 {"content":"203.0.113.7"}`) || !strings.Contains(strings.Join(patches, ";"), `r3 {"content":"2001:db8::7"}`) {
		t.Fatalf("只改指向原主机的那两条,只发 content: %v", patches)
	}
	if auth != "Bearer cf-secret" {
		t.Fatal("用主机容灾自己的令牌")
	}
	var b Became
	s.getJSON(KeyBecame, &b)
	if !b.Notified || len(b.Dns) != 2 {
		t.Fatalf("改动要记下: %+v", b)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "已自动把") || strings.Contains(notes[0], "cf-secret") {
		t.Fatalf("通知要写明改了哪些记录、不带令牌: %q", notes)
	}
	if n := s.Notices(); len(n) != 1 || !n[0].Dns {
		t.Fatalf("面板提示要说 DNS 已经改好: %+v", n)
	}
	// 只做一次
	s.afterBecame()
	if len(notes) != 1 {
		t.Fatal("成为主机后的收尾只做一次")
	}
	// 测试令牌与记录:只读,标出哪些指向本机
	res, err := s.DNSTest("", "")
	if err != nil || len(res.Records) != 3 {
		t.Fatalf("应列出 3 条记录: %+v %v", res, err)
	}
	match := 0
	for _, r := range res.Records {
		if r.Match {
			match++
		}
	}
	if match != 2 {
		t.Fatalf("指向本机的两条要标出来: %+v", res.Records)
	}
	cf.mu.Lock()
	if len(cf.patches) != 2 {
		t.Fatal("测试不能改记录")
	}
	cf.mu.Unlock()
	if _, err := s.DNSTest("", "not a domain!"); err == nil {
		t.Fatal("域名格式不对应报错")
	}
}

// 没配令牌或域名:什么都不做;Cloudflare 出错只告警,通知里写手动改。
func TestUpdateDNSOffOrFailing(t *testing.T) {
	s := testService(t, false, map[string]string{KeyEpoch: "2", "publicIp": "203.0.113.7"})
	if ch, err := s.updateDNS("198.51.100.1", ""); ch != nil || err != nil {
		t.Fatal("没配就不动")
	}
	var notes []string
	s.d.Notify = func(_, text string) { notes = append(notes, text) }
	s.d.DNSAPI = "http://127.0.0.1:1" // 连不上
	s.set(map[string]string{KeyDnsToken: "x", KeyDnsNames: "sub.example.com", KeyBecame: mustJSON(Became{How: HowSetMaster, Epoch: 2, FormerName: "HK",
		FormerIP: "198.51.100.1", Identity: identityResult{SubURLKept: true, SubHost: "sub.example.com"}})})
	s.afterBecame()
	if len(notes) != 1 || !strings.Contains(notes[0], "自动修改失败") || !strings.Contains(notes[0], "手动") {
		t.Fatalf("改 DNS 失败要说清要手动改: %q", notes)
	}
}

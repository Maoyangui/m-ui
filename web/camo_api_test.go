package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 设置页的页面伪装接口(用户 N2):按页面上还没保存的样式预览(图片、样式表内联,链接拿掉);入口统计按启用的线路算
// (Trojan 只算没配传输层的,Hysteria2 分填没填自己的伪装站点);跳转模式给规整后的网址或错误;「换一个样子」换本机种子。
func TestCamoAPI(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	s := NewServer(run)
	add := func(name, proto, opts, transport string, on bool) {
		l := model.Line{Name: name, Protocol: proto, Port: 20000 + len(name)*7, Enabled: true, Options: json.RawMessage(opts)}
		if transport != "" {
			l.Transport = json.RawMessage(transport)
		}
		if err := s.db.Create(&l).Error; err != nil {
			t.Fatal(err)
		}
		if !on { // gorm 的 default:true 会把 false 当成没填:另外改
			s.db.Model(&l).Update("enabled", false)
		}
	}
	add("at1", "anytls", `{}`, "", true)
	add("at22", "anytls", `{}`, "", true)
	add("at333", "anytls", `{}`, "", false)
	add("tj1", "trojan", `{}`, "", true)
	add("tjws22", "trojan", `{}`, `{"type":"ws","path":"/x"}`, true)
	add("hy1", "hysteria2", `{}`, "", true)
	add("hyown22", "hysteria2", `{"masquerade":{"type":"proxy","url":"https://a.example"}}`, "", true)
	add("hystr333", "hysteria2", `{"masquerade":"https://b.example"}`, "", true)
	add("vl1", "vless", `{}`, "", true)

	call := func(method, q string) (int, camoView) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleCamo(w, httptest.NewRequest(method, "/app/api/camo"+q, nil))
		var v camoView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	code, v := call(http.MethodGet, "?template=video")
	if code != 200 || v.Mode != "site" || v.Template != "video" || v.Auto || v.Name == "" {
		t.Fatalf("预览视频站: %d %+v", code, v.Entries)
	}
	if strings.Contains(v.HTML, ` href="`) || !strings.Contains(v.HTML, "data:image/svg+xml;base64,") || !strings.Contains(v.HTML, "<style>") {
		t.Fatal("预览要内联图片和样式表、拿掉链接")
	}
	if e := v.Entries; e.AnyTLS != 2 || e.Trojan != 1 || e.Hy2 != 1 || e.Hy2Own != 2 || e.Panel != 2053 || e.Sub != 2056 || e.Reseller != 2054 {
		t.Fatalf("入口统计: %+v", e)
	}
	// 封面照片:测试里不去取(runner 在测试里不连外网),视频站用到的(种子随机,同一类不够时会重复,6~12 张)一张没有、也不算正在取;
	// 页面伪装还没开:标 Off(设置页写「开启后才取」,自审 G3);镜像站不用照片
	if p := v.Photos; p == nil || p.Want < 6 || p.Want > 12 || p.Have != 0 || p.Loading || !p.Off {
		t.Fatalf("视频站的封面照片: %+v", p)
	}
	if _, v := call(http.MethodGet, "?template=mirror"); v.Photos != nil {
		t.Fatalf("镜像站不用照片: %+v", v.Photos)
	}
	// 「在新标签页打开」:没开时没有;开了是订阅端口的根目录
	if v.Live != "" {
		t.Fatalf("没开时没有正在回应的伪装站: %q", v.Live)
	}
	for k, val := range map[string]string{"camoEnabled": "true", "subDomain": "sub.example.com"} {
		if err := s.db.Create(&model.Setting{Key: k, Value: val}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, v := call(http.MethodGet, "?template=video"); v.Live != "http://sub.example.com:2056/" || v.Photos == nil || v.Photos.Off {
		t.Fatalf("开了应给订阅端口的根目录、照片不再标 Off: %q %+v", v.Live, v.Photos)
	}
	if _, v := call(http.MethodGet, ""); v.Mode != "site" || !v.Auto {
		t.Fatalf("没存过样式时是自动: %+v", v)
	}
	if _, v := call(http.MethodGet, "?template=redirect&redirect=www.example.com"); v.Mode != "redirect" || v.Target != "https://www.example.com" || v.HTML != "" {
		t.Fatalf("跳转模式: %+v", v)
	}
	if _, v := call(http.MethodGet, "?template=redirect&redirect=ftp://x"); v.Target != "" || v.Error == "" {
		t.Fatalf("网址填错要说原因: %+v", v)
	}
	if code, _ := call(http.MethodGet, "?template=nope"); code != 400 {
		t.Fatalf("认不出的样式应 400: %d", code)
	}
	seedOf := func() string {
		var v string
		s.db.Raw("SELECT value FROM settings WHERE key = ?", "camoSeed").Scan(&v)
		return v
	}
	before := seedOf()
	if before == "" {
		t.Fatal("预览时就该生成本机种子")
	}
	if code, _ := call(http.MethodPost, ""); code != 200 || seedOf() == before {
		t.Fatalf("换一个样子应换本机种子: %d", code)
	}
}

// 保存设置时:样式只认内置的和 redirect(大小写规整);跳转网址规整后存(只写域名的补 https://),填错拒收;
// 选了跳转却没有网址(没填、或把存着的清空)拒收;开关照常规整成 true / false。
func TestCamoSettingsValidation(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	call := func(body string) int {
		r := httptest.NewRequest("POST", "http://x/app/api/settings", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSettings(w, r)
		return w.Code
	}
	for _, c := range []struct {
		body string
		code int
		key  string
		want string
	}{
		{`{"camoTemplate":"VIDEO","camoEnabled":"on","camoPorts":"0"}`, 200, "camoTemplate", "video"},
		{`{"camoTemplate":"nope"}`, 400, "camoTemplate", "video"},
		{`{"camoTemplate":"redirect"}`, 400, "camoTemplate", "video"},
		{`{"camoTemplate":"redirect","camoRedirect":"www.example.com"}`, 200, "camoRedirect", "https://www.example.com"},
		{`{"camoRedirect":"ftp://x"}`, 400, "camoRedirect", "https://www.example.com"},
		{`{"camoRedirect":""}`, 400, "camoRedirect", "https://www.example.com"},
		{`{"camoTemplate":"auto","camoRedirect":""}`, 200, "camoTemplate", ""},
	} {
		if code := call(c.body); code != c.code {
			t.Fatalf("%s: %d,应为 %d", c.body, code, c.code)
		}
		if got := s.setting(c.key); got != c.want {
			t.Fatalf("%s 之后 %s = %q,应为 %q", c.body, c.key, got, c.want)
		}
	}
	if s.setting("camoEnabled") != "true" || s.setting("camoPorts") != "false" {
		t.Fatal("页面伪装的开关要规整成 true / false")
	}
	// 库里已经是「跳转 + 没有网址」(命令行直接改的):改别的设置不受这一条拦(审查 F-B2);动这两项时照样要先把网址填上
	db.Where("key IN ?", []string{"camoTemplate", "camoRedirect"}).Delete(&model.Setting{})
	db.Create(&model.Setting{Key: "camoTemplate", Value: "redirect"})
	if code := call(`{"timezone":"Asia/Tokyo"}`); code != 200 || s.setting("timezone") != "Asia/Tokyo" {
		t.Fatalf("无关的保存不该被「要填跳转的网址」拦下: %d", code)
	}
	if code := call(`{"camoTemplate":"redirect","timezone":"Asia/Seoul"}`); code != 400 || s.setting("timezone") != "Asia/Tokyo" {
		t.Fatalf("动到伪装方式时照样要有网址: %d", code)
	}
}

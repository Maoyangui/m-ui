package sub

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// testLogo 给 owner(0 = 主面板,> 0 = 代理)存一张 w×h 的 PNG 作落地页 logo,和面板上传后库里的样子一样。
func testLogo(t *testing.T, db *gorm.DB, owner uint, w, h int, shade uint8) model.PageLogo {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.NRGBA{R: shade, G: 100, B: 200, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	row := model.PageLogo{ResellerId: owner, Mime: "image/png", Data: b.Bytes(), Sum: hex.EncodeToString(sum[:8]), Width: w, Height: h, UpdatedAt: 1}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func logoReq(s *Server, target string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://hk.example:2056"+target, nil)
	r.Header.Set("User-Agent", browserUA)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.handle()(w, r)
	return w
}

const builtinIcon = "data:image/svg+xml;base64,"

// iconPage 取中文页面并还原字符实体:html/template 会把属性值里的 + 写成 &#43;(浏览器读到的是同一个地址),
// 查图标地址和类型前先还原。
func iconPage(s *Server, target string) string {
	return html.UnescapeString(doReqZh(s, target).Body.String())
}

// 没传 logo:标签图标与页头都是内联的内置 SVG,和以前一样。
func TestLandingLogoBuiltinByDefault(t *testing.T) {
	s, _ := shareServer(t)
	body := iconPage(s, "/sub/alice")
	if strings.Count(body, builtinIcon) != 2 || !strings.Contains(body, `type="image/svg+xml"`) || strings.Contains(body, "/logo?v=") {
		t.Fatal("没传 logo 时标签图标与页头应是内置 SVG")
	}
	if !strings.Contains(body, `<img class="logo" src="`+builtinIcon) {
		t.Fatal("内置 logo 不带 custom(深色模式下不垫浅色底,它本身两种底色都看得清)")
	}
	if cl := iconPage(s, "/sub/alice?clients=1"); !strings.Contains(cl, `<link rel="icon" href="`+builtinIcon) || !strings.Contains(cl, `type="image/svg+xml"`) {
		t.Fatal("客户端下载页的标签图标应是内置 SVG")
	}
}

// 主面板传了 logo:落地页的标签图标与页头、客户端下载页的标签图标都指向 ./<地址>/logo?v=<摘要>,类型取库里的;
// 页面里不再内联内置 SVG。
func TestLandingLogoFromMaster(t *testing.T) {
	s, db := shareServer(t)
	lg := testLogo(t, db, 0, 480, 160, 30)
	want := "./alice/logo?v=" + lg.Sum
	body := iconPage(s, "/sub/alice")
	if !strings.Contains(body, `<link rel="icon" href="`+want+`" type="image/png">`) || !strings.Contains(body, `<img class="logo custom" src="`+want+`"`) {
		t.Fatalf("落地页应引用主面板的 logo %s(带 custom:深色模式下垫浅色底)", want)
	}
	if strings.Contains(body, builtinIcon) {
		t.Fatal("有自定义 logo 时不该再内联内置 SVG")
	}
	if cl := doReqZh(s, "/sub/alice?clients=1").Body.String(); !strings.Contains(cl, `<link rel="icon" href="`+want+`" type="image/png">`) {
		t.Fatal("客户端下载页的标签图标应和落地页一样")
	}
	// 停用的用户照常看到落地页,logo 也一样
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)
	if body := doReqZh(s, "/sub/alice").Body.String(); !strings.Contains(body, `src="`+want+`"`) {
		t.Fatal("停用用户的落地页也应显示自定义 logo")
	}
}

// 代理名下的用户先用代理自己的 logo,代理没传用主面板的,都没传用内置的;主面板的用户不受代理 logo 影响。
func TestLandingLogoResellerFallback(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Reseller{Name: "dl1", Enabled: true, PageEnabled: true, ShareOn: true})
	db.Create(&model.User{Name: "bob", Enabled: true, ResellerId: 1, SubToken: "tok123", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)})

	if body := iconPage(s, "/sub/tok123"); !strings.Contains(body, builtinIcon) || strings.Contains(body, "/logo?v=") {
		t.Fatal("主面板与代理都没传 logo 时应是内置 SVG")
	}
	master := testLogo(t, db, 0, 120, 40, 10)
	if body := doReqZh(s, "/sub/tok123").Body.String(); !strings.Contains(body, `src="./tok123/logo?v=`+master.Sum+`"`) {
		t.Fatal("代理没传 logo 时名下用户应看到主面板的")
	}
	own := testLogo(t, db, 1, 120, 40, 90)
	body := doReqZh(s, "/sub/tok123").Body.String()
	if !strings.Contains(body, `src="./tok123/logo?v=`+own.Sum+`"`) || strings.Contains(body, master.Sum) {
		t.Fatal("代理传了 logo 时名下用户应看到代理自己的")
	}
	if body := doReqZh(s, "/sub/alice").Body.String(); !strings.Contains(body, `src="./alice/logo?v=`+master.Sum+`"`) || strings.Contains(body, own.Sum) {
		t.Fatal("主面板的用户不该看到代理的 logo")
	}
	// /logo 路由给的也是各自的那张
	if w := logoReq(s, "/sub/tok123/logo?v="+own.Sum, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), own.Data) {
		t.Fatalf("代理用户的 /logo 应是代理的图,得 %d", w.Code)
	}
	if w := logoReq(s, "/sub/alice/logo?v="+master.Sum, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), master.Data) {
		t.Fatalf("主面板用户的 /logo 应是主面板的图,得 %d", w.Code)
	}
}

// 共享地址的落地页:logo 地址里是共享令牌,不暴露本人用户名;/logo 也按共享令牌给。
func TestSharedLandingLogoUsesShareToken(t *testing.T) {
	s, db := shareServer(t)
	lg := testLogo(t, db, 0, 120, 40, 50)
	doReq(s, "POST", "/sub/alice?share=on", browserUA)
	tok := token(t, db)
	if tok == "" {
		t.Fatal("前提:生成共享")
	}
	body := doReqZh(s, "/sub/"+tok).Body.String()
	if !strings.Contains(body, `src="./`+tok+`/logo?v=`+lg.Sum+`"`) || strings.Contains(body, "alice") {
		t.Fatal("共享落地页的 logo 地址应只含共享令牌,不带本人用户名")
	}
	if w := logoReq(s, "/sub/"+tok+"/logo?v="+lg.Sum, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), lg.Data) {
		t.Fatalf("共享令牌的 /logo 应 200,得 %d", w.Code)
	}
}

// /logo:原图加几道兜底的头(禁嗅探、整个响应进沙箱、只许同源引用);?v= 对上摘要时长缓存,对不上或没带就每次来问;
// ETag 命中 304;HEAD 只给头;别的方法一律 404(不会被当成生成 / 取消共享);和二维码一样不记订阅日志。
func TestLogoRouteHeaders(t *testing.T) {
	s, db := shareServer(t)
	lg := testLogo(t, db, 0, 480, 160, 70)

	w := logoReq(s, "/sub/alice/logo?v="+lg.Sum, nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), lg.Data) {
		t.Fatalf("/logo 应原样输出图片,得 %d(%d 字节)", w.Code, w.Body.Len())
	}
	for k, v := range map[string]string{
		"Content-Type":                 "image/png",
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'; sandbox",
		"Cross-Origin-Resource-Policy": "same-origin",
		"ETag":                         `"` + lg.Sum + `"`,
		"Cache-Control":                "private, max-age=2592000, immutable",
		"Content-Length":               strconv.Itoa(len(lg.Data)),
	} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s 应为 %q,得 %q", k, v, got)
		}
	}
	for _, q := range []string{"", "?v=0000000000000000"} { // 没带版本号,或页面是换图之前打开的
		if c := logoReq(s, "/sub/alice/logo"+q, nil).Header().Get("Cache-Control"); c != "no-cache" {
			t.Fatalf("版本号对不上(%q)时不该长缓存,得 %q", q, c)
		}
	}
	for _, inm := range []string{`"` + lg.Sum + `"`, `W/"` + lg.Sum + `"`, `"x", "` + lg.Sum + `"`, "*"} {
		if r := logoReq(s, "/sub/alice/logo", map[string]string{"If-None-Match": inm}); r.Code != 304 || r.Body.Len() != 0 {
			t.Fatalf("If-None-Match %s 应 304,得 %d", inm, r.Code)
		}
	}
	if r := logoReq(s, "/sub/alice/logo", map[string]string{"If-None-Match": `"0000000000000000"`}); r.Code != 200 {
		t.Fatalf("ETag 对不上应 200,得 %d", r.Code)
	}
	if h := doReq(s, "HEAD", "/sub/alice/logo", browserUA); h.Code != 200 || h.Body.Len() != 0 || h.Header().Get("Content-Length") != strconv.Itoa(len(lg.Data)) {
		t.Fatalf("HEAD 应 200、只给头,得 %d(正文 %d 字节)", h.Code, h.Body.Len())
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if c := doReq(s, m, "/sub/alice/logo?share=on", browserUA).Code; c != 404 {
			t.Fatalf("%s /logo 应 404,得 %d", m, c)
		}
	}
	if token(t, db) != "" {
		t.Fatal("POST /logo?share=on 不该生成共享")
	}
	if c := doReq(s, "GET", "/sub/alice/logo", "curl/8.4.0").Code; c != 200 {
		t.Fatalf("图片和 UA 无关,客户端 UA 也应 200,得 %d", c)
	}
	// 和别的后缀不混:/logo/qr、/qr/logo 都不是合法地址
	for _, p := range []string{"/sub/alice/logo/qr", "/sub/alice/qr/logo", "/sub/alice/logo/logo"} {
		if c := doReq(s, "GET", p, "curl/8.4.0").Code; c != 404 {
			t.Fatalf("%s 应 404,得 %d", p, c)
		}
	}
	s.logMu.Lock()
	n := len(s.logBuf)
	s.logMu.Unlock()
	if n != 0 {
		t.Fatalf("/logo 不该记订阅日志,记了 %d 条", n)
	}
}

// 没有自定义 logo(或页面打开期间刚被删掉)时 /logo 给内置 SVG,同样带安全头。
// 库里的类型不是 PNG / JPEG / GIF(手改过库)就当没有:页面不引用它,/logo 也不输出它。
func TestLogoRouteBuiltinFallback(t *testing.T) {
	s, db := shareServer(t)
	w := logoReq(s, "/sub/alice/logo?v=0123456789abcdef", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" || !bytes.Equal(w.Body.Bytes(), brand.SVG) {
		t.Fatalf("没有自定义 logo 时应输出内置 SVG,得 %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
		t.Fatalf("内置 SVG 也要带安全头、不长缓存: %v", w.Header())
	}
	if r := logoReq(s, "/sub/alice/logo", map[string]string{"If-None-Match": w.Header().Get("ETag")}); r.Code != 304 {
		t.Fatalf("内置 SVG 的 ETag 命中应 304,得 %d", r.Code)
	}

	evil := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/svg+xml", Data: evil, Sum: "0123456789abcdef", Width: 1, Height: 1})
	if body := iconPage(s, "/sub/alice"); strings.Contains(body, "0123456789abcdef") || !strings.Contains(body, builtinIcon) {
		t.Fatal("库里的 SVG 不该被落地页引用")
	}
	if w := logoReq(s, "/sub/alice/logo?v=0123456789abcdef", nil); !bytes.Equal(w.Body.Bytes(), brand.SVG) || bytes.Contains(w.Body.Bytes(), []byte("<script>")) {
		t.Fatal("库里的 SVG 不该被输出")
	}
}

// /logo 跟落地页同一套开关:订阅页整体关掉、代理关了自己的订阅页时 404;代理关了共享,共享令牌的 /logo 也 404;
// 对不上任何人的地址 404。停用的用户照样给(他的落地页照常出)。
func TestLogoRouteFollowsPageSwitches(t *testing.T) {
	s, db := shareServer(t)
	testLogo(t, db, 0, 60, 20, 1)
	if c := logoReq(s, "/sub/nobody-here/logo", nil).Code; c != 404 {
		t.Fatalf("对不上任何人的地址应 404,得 %d", c)
	}
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)
	if c := logoReq(s, "/sub/alice/logo", nil).Code; c != 200 {
		t.Fatalf("停用用户的落地页照常出,logo 也应 200,得 %d", c)
	}
	db.Create(&model.Setting{Key: "subPageEnabled", Value: "false"})
	if c := logoReq(s, "/sub/alice/logo", nil).Code; c != 404 {
		t.Fatalf("订阅页关掉后 /logo 应 404,得 %d", c)
	}
	db.Model(&model.Setting{}).Where("key = ?", "subPageEnabled").Update("value", "true")

	rs := model.Reseller{Name: "dl1", Enabled: true, PageEnabled: true, ShareOn: true}
	db.Create(&rs)
	db.Create(&model.User{Name: "bob", Enabled: true, ResellerId: rs.Id, SubToken: "tok123", ShareToken: "shr-bob-abcdefghijklmnop",
		Credentials: []byte(`{"hysteria2":{"password":"p"}}`), ShareCreds: []byte(`{"hysteria2":{"password":"s"}}`)})
	for _, k := range []string{"tok123", "shr-bob-abcdefghijklmnop"} {
		if c := logoReq(s, "/sub/"+k+"/logo", nil).Code; c != 200 {
			t.Fatalf("%s 的 /logo 应 200,得 %d", k, c)
		}
	}
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("share_on", false)
	if c := logoReq(s, "/sub/shr-bob-abcdefghijklmnop/logo", nil).Code; c != 404 {
		t.Fatalf("代理关了共享,共享令牌的 /logo 应 404,得 %d", c)
	}
	if c := logoReq(s, "/sub/tok123/logo", nil).Code; c != 200 {
		t.Fatalf("关共享不影响本人地址的 /logo,得 %d", c)
	}
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("page_enabled", false)
	if c := logoReq(s, "/sub/tok123/logo", nil).Code; c != 404 {
		t.Fatalf("代理关了自己的订阅页,名下用户的 /logo 应 404,得 %d", c)
	}
}

// 状态卡、公告、页脚的联系方式与无效地址页都经 linkify:文字(链接)只显示文字、Tg / WhatsApp 写法变成链接,
// 页面上不出现原始地址;尖括号照常转义;链接不带样式,字号跟随段落。
func TestLandingPageLinkifiesNoticeAndSupport(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageSupport", Value: "Tg: @MaoyangSupport(https://t.me/MaoyangSupport) / Wa: +34 671143339(https://wa.me/34671143339)"})
	db.Create(&model.Setting{Key: "subPageNotice", Value: "教程:[Clash/Mihomo 教程](https://example.com/guide)\n<script>alert(1)</script>"})
	tg := `<a href="https://t.me/MaoyangSupport" target="_blank" rel="noopener noreferrer nofollow">@MaoyangSupport</a>`
	wa := `<a href="https://wa.me/34671143339" target="_blank" rel="noopener noreferrer nofollow">+34 671143339</a>`
	guide := `<a href="https://example.com/guide" target="_blank" rel="noopener noreferrer nofollow">Clash/Mihomo 教程</a>`

	check := func(name, body string, links int) {
		t.Helper()
		if strings.Count(body, tg) != links || strings.Count(body, wa) != links || strings.Count(body, guide) != 1 {
			t.Fatalf("%s:联系方式应出现 %d 次链接、公告里的链接 1 次", name, links)
		}
		for _, raw := range []string{"(https://t.me/MaoyangSupport)", "(https://wa.me/34671143339)", "](https://example.com/guide)", "<script>alert(1)"} {
			if strings.Contains(body, raw) {
				t.Fatalf("%s:页面上不该出现 %q", name, raw)
			}
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Fatalf("%s:公告里的标签应转义成文字", name)
		}
		if strings.Count(body, `href="https://t.me/MaoyangSupport"`) != strings.Count(body, "https://t.me/MaoyangSupport") {
			t.Fatalf("%s:地址只能出现在 href 里", name)
		}
	}
	check("正常落地页(页脚)", doReqZh(s, "/sub/alice").Body.String(), 1)
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)
	check("停用的落地页(状态卡 + 页脚)", doReqZh(s, "/sub/alice").Body.String(), 2)
	w := doReqZh(s, "/sub/nobody-here")
	if w.Code != 404 {
		t.Fatalf("无效地址应 404,得 %d", w.Code)
	}
	check("无效地址页", w.Body.String(), 1)
}

// 临时共享卡的提示:已生成时说明对方用浏览器打开就是一键导入页、也能直接粘到客户端;没生成时说明对方用浏览器打开即可导入。
// 借用者那一页不出这张卡。
func TestShareCardHintMentionsBrowserImport(t *testing.T) {
	s, db := shareServer(t)
	zh := doReqZh(s, "/sub/alice").Body.String()
	en := doReq(s, "GET", "/sub/alice", browserUA).Body.String()
	if !strings.Contains(zh, "对方用浏览器打开即可一键导入") || !strings.Contains(en, "they open it in a browser and import it in one tap") {
		t.Fatal("未生成时的提示应说明对方用浏览器打开即可导入(中英)")
	}
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != 200 {
		t.Fatalf("生成共享失败: %d", w.Code)
	}
	zh = doReqZh(s, "/sub/alice").Body.String()
	en = doReq(s, "GET", "/sub/alice", browserUA).Body.String()
	if !strings.Contains(zh, "用浏览器打开就是一键导入页") || !strings.Contains(zh, "也能直接粘贴到客户端") ||
		!strings.Contains(en, "opened in a browser it is a one-tap import page") || !strings.Contains(en, "pasted straight into an app") {
		t.Fatal("已生成时的提示应说明浏览器打开是一键导入页、也能粘到客户端(中英)")
	}
	if shared := doReqZh(s, "/sub/"+token(t, db)).Body.String(); strings.Contains(shared, "把这个地址发给对方") || strings.Contains(shared, `id="share"`) {
		t.Fatal("借用者的落地页不该出现共享卡")
	}
}

// 深色模式下透明底的深色 logo 看不见:上传的 logo 带 custom,深色时垫一块浅色底(不改原图);内置的不垫。
// 窄屏上宽幅 logo 加不带空格的长标题:文字这一栏能收窄、长词能断开,logo 也按屏宽封顶,页头不撑出屏幕。
func TestLandingLogoDarkBackdropAndNarrowHeader(t *testing.T) {
	s, db := shareServer(t)
	testLogo(t, db, 0, 480, 80, 30)
	body := doReqZh(s, "/sub/alice").Body.String()
	for _, want := range []string{
		`@media(prefers-color-scheme:dark){.logo.custom{background:#f4f6f9}}`,
		`.logo.custom{height:32px;padding:2px 4px;border-radius:6px;max-width:min(168px,44vw)}`,
		`max-width:min(160px,42vw)`,
		`.brand>div{min-width:0}`,
		`.brand h1,.brand .u{overflow-wrap:anywhere;word-break:break-word}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("落地页样式里缺 %s", want)
		}
	}
	if !strings.Contains(body, `<img class="logo custom" src="./alice/logo?v=`) {
		t.Fatal("上传的 logo 应带 custom")
	}
}

// 「订阅地址无效」页的标签图标与页头 logo 和落地页同一取法:代理名下(反查得到代理,例如代理关了共享后的共享地址)
// 先用代理的,再用主面板的,都没传用内置的。地址对不上任何人时 ./<地址>/logo 路由是 404,
// 所以这页把图内联成 data URI(上传上限 64 KiB),类型取库里的。
func TestNotFoundPageLogoFollowsLanding(t *testing.T) {
	s, db := shareServer(t)
	dataURI := func(lg model.PageLogo) string {
		return "data:" + lg.Mime + ";base64," + base64.StdEncoding.EncodeToString(lg.Data)
	}
	body := iconPage(s, "/sub/nobody-here")
	if !strings.Contains(body, `<link rel="icon" href="`+builtinIcon) || !strings.Contains(body, `type="image/svg+xml"`) ||
		!strings.Contains(body, `<img class="logo" src="`+builtinIcon) {
		t.Fatal("没传 logo 时无效页的标签图标与页头应是内置 SVG")
	}

	master := testLogo(t, db, 0, 120, 40, 10)
	body = iconPage(s, "/sub/nobody-here")
	if !strings.Contains(body, `<link rel="icon" href="`+dataURI(master)+`" type="image/png">`) ||
		!strings.Contains(body, `<img class="logo custom" src="`+dataURI(master)+`"`) || strings.Contains(body, builtinIcon) {
		t.Fatal("主面板传了 logo:无效页的标签图标与页头应是主面板的 logo")
	}

	// 代理关了共享:共享地址落到无效页,反查得到代理 → 先用代理的 logo
	rs := model.Reseller{Name: "dl1", Enabled: true, PageEnabled: true, ShareOn: true}
	db.Create(&rs)
	db.Create(&model.User{Name: "bob", Enabled: true, ResellerId: rs.Id, SubToken: "tok123", ShareToken: "shr-bob-abcdefghijklmnop",
		Credentials: []byte(`{"hysteria2":{"password":"p"}}`), ShareCreds: []byte(`{"hysteria2":{"password":"s"}}`)})
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("share_on", false)
	if body := iconPage(s, "/sub/shr-bob-abcdefghijklmnop"); !strings.Contains(body, `src="`+dataURI(master)+`"`) {
		t.Fatal("代理没传 logo 时应回落到主面板的")
	}
	own := testLogo(t, db, rs.Id, 120, 40, 90)
	body = iconPage(s, "/sub/shr-bob-abcdefghijklmnop")
	if !strings.Contains(body, `<link rel="icon" href="`+dataURI(own)+`" type="image/png">`) || !strings.Contains(body, `src="`+dataURI(own)+`"`) || strings.Contains(body, dataURI(master)) {
		t.Fatal("代理传了 logo 时无效页应用代理的")
	}
	if body := iconPage(s, "/sub/nobody-here"); strings.Contains(body, dataURI(own)) {
		t.Fatal("对不上任何人的地址不该看到代理的 logo")
	}
}

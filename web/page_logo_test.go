package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

func logoImg(w, h int, shade uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.NRGBA{R: shade, G: 100, B: 200, A: 255})
	}
	return img
}

func logoPNG(t *testing.T, w, h int, shade uint8) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, logoImg(w, h, shade)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// noisePNG 随机噪点压不动:200×200 的也有一百多 KB。
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rand.New(rand.NewSource(1)).Read(img.Pix)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func dataURI(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func logoSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

func logoTestServer(t *testing.T) (*Server, *gorm.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return &Server{db: db}, db
}

type logoResp struct {
	Set    bool   `json:"set"`
	Sum    string `json:"sum"`
	Mime   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Size   int    `json:"size"`
	Data   string `json:"data"`
	Error  string `json:"error"`
}

func readLogoResp(t *testing.T, w *httptest.ResponseRecorder) logoResp {
	t.Helper()
	var v logoResp
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("返回不是 JSON: %d %s", w.Code, w.Body.String())
	}
	return v
}

func logoBody(data string) string {
	b, _ := json.Marshal(map[string]string{"data": data})
	return string(b)
}

func logoRows(t *testing.T, db *gorm.DB) map[uint]model.PageLogo {
	t.Helper()
	var rows []model.PageLogo
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]model.PageLogo{}
	for _, r := range rows {
		out[r.ResellerId] = r
	}
	return out
}

// 服务端不信任前端:只认 PNG / JPEG / GIF 的文件头(data URI 里写的类型不算数),≤ 64 KB,宽高 1~1024,并完整解码一次。
func TestDecodeLogoAcceptsPNGJPEGGIF(t *testing.T) {
	img := logoImg(480, 160, 30)
	var pngB, jpgB, gifB bytes.Buffer
	if err := png.Encode(&pngB, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpgB, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifB, img, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, in, mime string
		raw            []byte
	}{
		{"PNG data URI", dataURI("image/png", pngB.Bytes()), "image/png", pngB.Bytes()},
		{"纯 base64", base64.StdEncoding.EncodeToString(pngB.Bytes()), "image/png", pngB.Bytes()},
		{"JPEG", dataURI("image/jpeg", jpgB.Bytes()), "image/jpeg", jpgB.Bytes()},
		{"GIF", dataURI("image/gif", gifB.Bytes()), "image/gif", gifB.Bytes()},
		{"类型按文件头认", dataURI("image/gif", pngB.Bytes()), "image/png", pngB.Bytes()},
		{"前后空白", "  " + dataURI("image/png", pngB.Bytes()) + "\n", "image/png", pngB.Bytes()},
		{"大写的 BASE64 标记", "data:image/png;BASE64," + base64.StdEncoding.EncodeToString(pngB.Bytes()), "image/png", pngB.Bytes()},
	} {
		got, err := decodeLogo(c.in)
		if err != nil {
			t.Fatalf("%s:应收下,得 %v", c.name, err)
		}
		if got.mime != c.mime || got.width != 480 || got.height != 160 || !bytes.Equal(got.data, c.raw) || got.sum != logoSum(c.raw) {
			t.Fatalf("%s:解析不对 mime=%s %dx%d sum=%s", c.name, got.mime, got.width, got.height, got.sum)
		}
	}
	if len(pngB.Bytes()) > 8<<10 {
		t.Fatalf("480×160 的简单 PNG 应只有几 KB,实际 %d", pngB.Len())
	}
}

func TestDecodeLogoRejects(t *testing.T) {
	ok := logoPNG(t, 480, 160, 30)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	big := noisePNG(t, 200, 200)
	if len(big) <= maxLogoBytes {
		t.Fatalf("前提:噪点图应超过 64 KB,实际 %d", len(big))
	}
	for _, c := range []struct{ name, in, err string }{
		{"SVG", dataURI("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)), "只支持 PNG / JPEG / GIF 图片"},
		{"冒充 PNG 的 SVG", dataURI("image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)), "只支持 PNG / JPEG / GIF 图片"},
		{"HTML", b64("<html><script>alert(1)</script></html>"), "只支持 PNG / JPEG / GIF 图片"},
		{"WebP", b64("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00"), "只支持 PNG / JPEG / GIF 图片"},
		{"BMP", b64("BM\x36\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00"), "只支持 PNG / JPEG / GIF 图片"},
		{"超过 64 KB", dataURI("image/png", big), "logo 不能超过 64 KB"},
		{"超尺寸", dataURI("image/png", logoPNG(t, 2000, 10, 1)), "图片尺寸要在 1024×1024 以内"},
		{"太高", dataURI("image/png", logoPNG(t, 10, 1100, 1)), "图片尺寸要在 1024×1024 以内"},
		{"截断的 PNG", dataURI("image/png", ok[:len(ok)/2]), "图片数据不完整或已损坏"},
		{"只有文件头", dataURI("image/png", []byte("\x89PNG\r\n\x1a\n")), "图片数据不完整或已损坏"},
		{"JPEG 头后面是垃圾", dataURI("image/jpeg", []byte("\xff\xd8\xff\xe0garbage-garbage")), "图片数据不完整或已损坏"},
		{"非 base64", "data:image/png;base64,@@@@", "图片数据格式不对"},
		{"data URI 不是 base64", "data:image/png,abc", "图片数据格式不对"},
		{"data URI 没有逗号", "data:image/png;base64", "图片数据格式不对"},
		{"空", "", "图片数据格式不对"},
	} {
		if _, err := decodeLogo(c.in); err == nil || err.Error() != c.err {
			t.Fatalf("%s:应报 %q,得 %v", c.name, c.err, err)
		}
	}
}

// 主面板:上传立即生效(reseller_id = 0 这一行),同一张图再传还是一行、摘要不变;换图摘要跟着变;
// 恢复默认删掉那一行;读接口带 no-store 与可直接预览的 data URI;审计记 settings/logo、settings/logo-reset。
func TestPageLogoAPIRoundTrip(t *testing.T) {
	s, db := logoTestServer(t)
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://x/app/api/sub-logo", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSubLogo(w, r)
		return w
	}
	if w := call("GET", ""); w.Code != 200 || readLogoResp(t, w).Set || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("没传时应返回 set:false(no-store),得 %d %s", w.Code, w.Body.String())
	}

	a := logoPNG(t, 480, 160, 30)
	w := call("POST", logoBody(dataURI("image/png", a)))
	v := readLogoResp(t, w)
	if w.Code != 200 || !v.Set || v.Sum != logoSum(a) || v.Mime != "image/png" || v.Width != 480 || v.Height != 160 || v.Size != len(a) ||
		v.Data != dataURI("image/png", a) {
		t.Fatalf("上传应返回新 logo,得 %d %+v", w.Code, v)
	}
	rows := logoRows(t, db)
	if len(rows) != 1 || rows[0].Sum != logoSum(a) || !bytes.Equal(rows[0].Data, a) || rows[0].UpdatedAt == 0 {
		t.Fatalf("库里应只有主面板(reseller_id = 0)这一行: %+v", rows)
	}
	if g := readLogoResp(t, call("GET", "")); !g.Set || g.Sum != logoSum(a) || g.Data != dataURI("image/png", a) {
		t.Fatalf("读回来的应是刚传的: %+v", g)
	}
	firstID := rows[0].Id

	if w := call("POST", logoBody(dataURI("image/png", a))); w.Code != 200 {
		t.Fatalf("同一张图再传应成功,得 %d", w.Code)
	}
	if rows := logoRows(t, db); len(rows) != 1 || rows[0].Id != firstID || rows[0].Sum != logoSum(a) {
		t.Fatalf("同一张图再传应原地更新,不多一行: %+v", rows)
	}
	b := logoPNG(t, 300, 100, 99)
	if w := call("PUT", logoBody(dataURI("image/png", b))); w.Code != 200 || readLogoResp(t, w).Sum != logoSum(b) {
		t.Fatalf("PUT 也能换图,得 %d %s", w.Code, w.Body.String())
	}
	if rows := logoRows(t, db); len(rows) != 1 || rows[0].Sum != logoSum(b) || rows[0].Width != 300 || rows[0].Height != 100 {
		t.Fatalf("换图后库里应是新图: %+v", rows)
	}

	if w := call("DELETE", ""); w.Code != 200 || readLogoResp(t, w).Set {
		t.Fatalf("恢复默认应返回 set:false,得 %d %s", w.Code, w.Body.String())
	}
	if rows := logoRows(t, db); len(rows) != 0 {
		t.Fatalf("恢复默认后库里不该有行: %+v", rows)
	}
	if readLogoResp(t, call("GET", "")).Set {
		t.Fatal("恢复默认后读接口应返回 set:false")
	}
	if w := call("PATCH", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("别的方法应 405,得 %d", w.Code)
	}
	var ups, resets int64
	db.Model(&model.Change{}).Where("key = ? AND action = ?", "settings", "logo").Count(&ups)
	db.Model(&model.Change{}).Where("key = ? AND action = ?", "settings", "logo-reset").Count(&resets)
	if ups != 3 || resets != 1 {
		t.Fatalf("审计应记 3 次上传、1 次恢复默认,实际 %d / %d", ups, resets)
	}
}

// 不合格的上传一律 400,带对应的中文错误,库里原来的 logo 不动。
func TestPageLogoAPIRejectsBadUploads(t *testing.T) {
	s, db := logoTestServer(t)
	call := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://x/app/api/sub-logo", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSubLogo(w, r)
		return w
	}
	good := logoPNG(t, 480, 160, 30)
	if w := call(logoBody(dataURI("image/png", good))); w.Code != 200 {
		t.Fatalf("前提:先传一张好的,得 %d %s", w.Code, w.Body.String())
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct{ name, body, err string }{
		{"SVG", logoBody(dataURI("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))), "只支持 PNG / JPEG / GIF 图片"},
		{"HTML", logoBody(b64("<!doctype html><script>alert(1)</script>")), "只支持 PNG / JPEG / GIF 图片"},
		{"WebP", logoBody(dataURI("image/webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00"))), "只支持 PNG / JPEG / GIF 图片"},
		{"超过 64 KB", logoBody(dataURI("image/png", noisePNG(t, 150, 150))), "logo 不能超过 64 KB"},
		{"超尺寸", logoBody(dataURI("image/png", logoPNG(t, 2000, 10, 1))), "图片尺寸要在 1024×1024 以内"},
		{"截断", logoBody(dataURI("image/png", good[:len(good)/2])), "图片数据不完整或已损坏"},
		{"非 base64", logoBody("data:image/png;base64,@@@@"), "图片数据格式不对"},
		{"没有 data", `{}`, "图片数据格式不对"},
		{"不是 JSON", `data=xyz`, "图片数据格式不对"},
		{"请求体超过 128 KB", `{"data":"` + strings.Repeat("A", 140<<10) + `"}`, "logo 不能超过 64 KB"},
	} {
		w := call(c.body)
		if w.Code != 400 || readLogoResp(t, w).Error != c.err {
			t.Fatalf("%s:应 400 %q,得 %d %s", c.name, c.err, w.Code, w.Body.String())
		}
	}
	if rows := logoRows(t, db); len(rows) != 1 || rows[0].Sum != logoSum(good) {
		t.Fatalf("被拒的上传不该动库里的 logo: %+v", rows)
	}
}

// 副机只读:logo 随主机的快照来,写请求被 masterOnly 拦下;读照常(设置页显示主机同步来的那张)。
func TestPageLogoNodeReadOnly(t *testing.T) {
	s, db := logoTestServer(t)
	db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: logoPNG(t, 60, 20, 1), Sum: "from-master", Width: 60, Height: 20})
	h := s.masterOnly(s.handleSubLogo)
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(m, "http://x/app/api/sub-logo", strings.NewReader(logoBody(dataURI("image/png", logoPNG(t, 30, 10, 2)))))
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusForbidden || readLogoResp(t, w).Error != nodeReadOnly {
			t.Fatalf("副机上 %s 应 403,得 %d %s", m, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "http://x/app/api/sub-logo", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if v := readLogoResp(t, w); w.Code != 200 || !v.Set || v.Sum != "from-master" {
		t.Fatalf("副机读应照常,得 %d %+v", w.Code, v)
	}
	if rows := logoRows(t, db); len(rows) != 1 || rows[0].Sum != "from-master" {
		t.Fatalf("副机上的 logo 不该被改: %+v", rows)
	}
}

// 代理面板:/api/self/logo 只动当前代理自己的那一行(归属取会话里的代理,请求里塞 resellerId 没用),
// 读不到、删不掉主面板和别的代理的;代理会话也进不了主面板的 sub-logo;删代理时他的 logo 一并删掉。
func TestResellerLogoScopedToSelf(t *testing.T) {
	s, db := logoTestServer(t)
	a := model.Reseller{Name: "dl-a", Enabled: true}
	b := model.Reseller{Name: "dl-b", Enabled: true}
	db.Create(&a)
	db.Create(&b)
	master := logoPNG(t, 120, 40, 1)
	db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: master, Sum: logoSum(master), Width: 120, Height: 40})

	call := func(rs model.Reseller, method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://x/dl/api/self/logo", strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), scopeKey, rs.Id))
		w := httptest.NewRecorder()
		s.handleResellerSelfSub(w, r)
		return w
	}
	if v := readLogoResp(t, call(a, "GET", "")); v.Set {
		t.Fatalf("代理没传时读到的应是 set:false(不是主面板的那张): %+v", v)
	}
	own := logoPNG(t, 240, 80, 7)
	body, _ := json.Marshal(map[string]interface{}{"data": dataURI("image/png", own), "resellerId": 0, "reseller_id": 0})
	if w := call(a, "POST", string(body)); w.Code != 200 || readLogoResp(t, w).Sum != logoSum(own) {
		t.Fatalf("代理上传应成功,得 %d %s", w.Code, w.Body.String())
	}
	rows := logoRows(t, db)
	if len(rows) != 2 || rows[a.Id].Sum != logoSum(own) || rows[0].Sum != logoSum(master) {
		t.Fatalf("代理的 logo 应进他自己那一行,主面板的不动: %+v", rows)
	}
	if v := readLogoResp(t, call(b, "GET", "")); v.Set {
		t.Fatalf("别的代理读不到这张: %+v", v)
	}
	if w := call(b, "DELETE", ""); w.Code != 200 {
		t.Fatalf("代理 b 恢复默认(他本来就没有)应 200,得 %d", w.Code)
	}
	if rows := logoRows(t, db); len(rows) != 2 {
		t.Fatalf("代理 b 恢复默认不该删掉别人的: %+v", rows)
	}
	var n int64
	db.Model(&model.Change{}).Where("actor = ? AND key = ? AND action = ?", a.Name, "reseller", "logo").Count(&n)
	if n != 1 {
		t.Fatalf("审计应以代理名记一次上传,实际 %d", n)
	}

	// 代理会话直接打主面板的接口:拒绝
	r := httptest.NewRequest("POST", "http://x/app/api/sub-logo", strings.NewReader(logoBody(dataURI("image/png", own))))
	r = r.WithContext(context.WithValue(r.Context(), scopeKey, a.Id))
	w := httptest.NewRecorder()
	s.handleSubLogo(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("代理会话不能改主面板的 logo,得 %d", w.Code)
	}
	if rows := logoRows(t, db); rows[0].Sum != logoSum(master) {
		t.Fatal("主面板的 logo 不该被代理改动")
	}

	if w := call(a, "DELETE", ""); w.Code != 200 {
		t.Fatalf("代理恢复默认应成功,得 %d", w.Code)
	}
	if rows := logoRows(t, db); len(rows) != 1 || rows[0].Sum != logoSum(master) {
		t.Fatalf("代理恢复默认只删他自己的: %+v", rows)
	}

	// 删代理:他的 logo 一并删掉,主面板的留着
	call(b, "POST", logoBody(dataURI("image/png", own)))
	req := httptest.NewRequest("DELETE", "http://x/app/api/resellers/"+strconv.Itoa(int(b.Id)), nil)
	w = httptest.NewRecorder()
	s.handleResellerItem(w, req)
	if w.Code != 200 {
		t.Fatalf("删代理应成功: %d %s", w.Code, w.Body.String())
	}
	if rows := logoRows(t, db); len(rows) != 1 || rows[0].Sum != logoSum(master) {
		t.Fatalf("删代理后他的 logo 应一并删掉、主面板的留着: %+v", rows)
	}
}

// 公告、联系方式有长度上限(按字数,不是字节):主面板设置与代理面板保存时都拦,错误里写明上限,被拒的不落库。
func TestPageTextLimits(t *testing.T) {
	s, db := logoTestServer(t)
	settings := func(k, v string) (int, string) {
		body, _ := json.Marshal(map[string]string{k: v})
		r := httptest.NewRequest("POST", "http://x/app/api/settings", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSettings(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := settings("subPageNotice", strings.Repeat("公", maxPageNotice+1)); code != 400 || !strings.Contains(body, "公告太长了,上限 4000 字") {
		t.Fatalf("超长公告应被拒,得 %d %s", code, body)
	}
	if s.setting("subPageNotice") != "" {
		t.Fatal("被拒的公告不该落库")
	}
	if code, body := settings("subPageNotice", strings.Repeat("公", maxPageNotice)); code != 200 {
		t.Fatalf("正好 4000 字应能保存(按字数算,不是字节),得 %d %s", code, body)
	}
	if code, body := settings("subPageSupport", strings.Repeat("联", maxPageSupport+1)); code != 400 || !strings.Contains(body, "联系方式太长了,上限 300 字") {
		t.Fatalf("超长联系方式应被拒,得 %d %s", code, body)
	}
	if code, _ := settings("subPageSupport", "Tg: @MaoyangSupport(https://t.me/MaoyangSupport)"); code != 200 {
		t.Fatalf("正常的联系方式应能保存,得 %d", code)
	}

	rs := model.Reseller{Name: "dl", Enabled: true}
	db.Create(&rs)
	put := func(notice, support string) (int, string) {
		body, _ := json.Marshal(map[string]interface{}{"pageEnabled": true, "shareOn": true, "pageNotice": notice, "pageSupport": support})
		r := httptest.NewRequest("PUT", "http://x/dl/api/self", bytes.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), scopeKey, rs.Id))
		w := httptest.NewRecorder()
		s.handleResellerSelf(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := put(strings.Repeat("公", maxPageNotice+1), ""); code != 400 || !strings.Contains(body, "公告太长了,上限 4000 字") {
		t.Fatalf("代理超长公告应被拒,得 %d %s", code, body)
	}
	if code, body := put("", strings.Repeat("联", maxPageSupport+1)); code != 400 || !strings.Contains(body, "联系方式太长了,上限 300 字") {
		t.Fatalf("代理超长联系方式应被拒,得 %d %s", code, body)
	}
	var cur model.Reseller
	db.First(&cur, rs.Id)
	if cur.PageNotice != "" || cur.PageSupport != "" {
		t.Fatal("被拒的文案不该落库")
	}
	if code, body := put("本店公告", "Tg: @shop_support"); code != 200 {
		t.Fatalf("正常文案应能保存,得 %d %s", code, body)
	}
	db.First(&cur, rs.Id)
	if cur.PageNotice != "本店公告" || cur.PageSupport != "Tg: @shop_support" {
		t.Fatalf("文案应已保存: %+v", cur)
	}
}

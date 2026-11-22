package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 网页和样式不让浏览器存、gzip 压缩着发(用户 11-22 手机上「恢复标签页」时浏览器拿了缓存里的旧样式,新脚本画出来的对勾撑满整张卡);
// 脚本、图片照旧按内容哈希 ETag 校验。压缩只压 200 的正文,解压后和内嵌的文件一字不差;没要 gzip、HEAD、404 都原样。
func TestAssetsNoStoreAndGzip(t *testing.T) {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		t.Fatal(err)
	}
	h := assetCache(sub, http.StripPrefix("/app", http.FileServer(http.FS(sub))))
	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	gz := map[string]string{"Accept-Encoding": "gzip, deflate, br"}
	for path, file := range map[string]string{"/app/style.css": "style.css", "/app/": "index.html", "/app/help/zh.html": "help/zh.html"} {
		want, err := fs.ReadFile(sub, file)
		if err != nil {
			t.Fatal(err)
		}
		w := do("GET", path, gz)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("ETag") != "" {
			t.Fatalf("%s:应 200、no-store、gzip、不带 ETag,得 %d %q %q %q", path, w.Code, w.Header().Get("Cache-Control"), w.Header().Get("Content-Encoding"), w.Header().Get("ETag"))
		}
		if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") || w.Header().Get("Content-Length") != "" {
			t.Fatalf("%s:压缩时要 Vary: Accept-Encoding、不能带压缩前的 Content-Length,得 %v", path, w.Header())
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatalf("%s:不是 gzip: %v", path, err)
		}
		got, _ := io.ReadAll(zr)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s:解压后和内嵌的文件不一样(%d / %d 字节)", path, len(got), len(want))
		}
		// 没要 gzip:原样、照样不存
		w = do("GET", path, nil)
		if w.Code != 200 || w.Header().Get("Content-Encoding") != "" || w.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(w.Body.Bytes(), want) {
			t.Fatalf("%s:不要 gzip 时应原样发,得 %d %q", path, w.Code, w.Header().Get("Content-Encoding"))
		}
	}
	if ct := do("GET", "/app/style.css", gz).Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("样式的 Content-Type 不对:%s", ct)
	}
	// 按字节区间要样式:整份给
	if w := do("GET", "/app/style.css", map[string]string{"Range": "bytes=0-9"}); w.Code != 200 || w.Body.Len() < 1000 {
		t.Fatalf("样式按区间要也应整份给,得 %d,%d 字节", w.Code, w.Body.Len())
	}
	// HEAD、404:不压缩
	if w := do("HEAD", "/app/style.css", gz); w.Header().Get("Content-Encoding") != "" || w.Code != 200 {
		t.Fatalf("HEAD 不该压缩,得 %d %q", w.Code, w.Header().Get("Content-Encoding"))
	}
	if w := do("GET", "/app/nope.css", gz); w.Code != 404 || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("不存在的文件应 404 且不压缩,得 %d %q", w.Code, w.Header().Get("Content-Encoding"))
	}
	// 脚本:照旧 ETag + 每次校验,不压缩
	w := do("GET", "/app/js/app.js", gz)
	etag := w.Header().Get("ETag")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || etag == "" || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("脚本应 no-cache + ETag、不压缩,得 %d %q %q %q", w.Code, w.Header().Get("Cache-Control"), etag, w.Header().Get("Content-Encoding"))
	}
	if w2 := do("GET", "/app/js/app.js", map[string]string{"If-None-Match": etag}); w2.Code != http.StatusNotModified {
		t.Fatalf("脚本带 If-None-Match 应 304,得 %d", w2.Code)
	}
}

package runner

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/camo"
)

func testPhoto(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withManifest 测试期间把图包清单里的校验和都换成 photo 的(测试服务器对哪个名字都回这张图),结束时换回来
func withManifest(t *testing.T, photo []byte) {
	t.Helper()
	saved := map[string]string{}
	for k, v := range camo.MediaFiles {
		saved[k] = v
		camo.MediaFiles[k] = sha256Hex(photo)
	}
	t.Cleanup(func() {
		for k, v := range saved {
			camo.MediaFiles[k] = v
		}
	})
}

// 取一张原图:先试的地址取不到、取到的内容和清单对不上都换下一个;核对过才存进数据目录,下次直接用、不再连网;
// 磁盘上那份被改坏了不用;超过上限的不要;清单里没有的名字、都取不到时报错。
func TestCamoMediaFile(t *testing.T) {
	photo := testPhoto(t)
	withManifest(t, photo)
	var hits atomic.Int32
	serve := func(body []byte, code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(code)
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/"
	}
	missing, wrong, good := serve(nil, 404), serve([]byte("not the photo"), 200), serve(photo, 200)
	big := serve(bytes.Repeat([]byte{1}, camo.MediaMaxBytes+10), 200)
	ctx, dir, name := context.Background(), t.TempDir(), "sea-day-01.jpg"
	b, err := camoMediaFile(ctx, []string{missing, wrong, good}, dir, name)
	if err != nil || !bytes.Equal(b, photo) {
		t.Fatalf("应换到第三个地址取到: %v", err)
	}
	if disk, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(disk, photo) {
		t.Fatal("核对过的应存进数据目录")
	}
	n := hits.Load()
	if b, err := camoMediaFile(ctx, []string{missing}, dir, name); err != nil || !bytes.Equal(b, photo) || hits.Load() != n {
		t.Fatalf("存过的应直接用、不再连网: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := camoMediaFile(ctx, []string{missing}, dir, name); err == nil {
		t.Fatal("磁盘上那份坏了、又取不到时应报错")
	}
	if _, err := camoMediaFile(ctx, []string{big}, t.TempDir(), name); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("超过上限的不要: %v", err)
	}
	if _, err := camoMediaFile(ctx, []string{good}, t.TempDir(), "nope-day-01.jpg"); err == nil {
		t.Fatal("清单里没有的名字应报错")
	}
}

// waitMedia 等后台取图、处理结束。上限给到 2 分钟:CI 开着 -race 时,视频站 12 张照片按 4 种尺寸处理(含 JPEG 编解码)
// 要十来秒,以前 10 秒的上限会误判(44369375776)。正常跑一两秒就完。
func waitMedia(t *testing.T, r *Runner) {
	t.Helper()
	for i := 0; i < 4800; i++ {
		r.camo.media.mu.Lock()
		running := r.camo.media.running
		r.camo.media.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("取照片一直没结束")
}

// 伪装站用到照片时在后台取、处理,处理好就重新生成成照片,设置页的状况跟着变;原图存进数据目录。
// 取不到时照旧画插图、记下原因和下次再试的时间,到点之前不再去取。
func TestCamoMediaRun(t *testing.T) {
	photo := testPhoto(t)
	withManifest(t, photo)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(photo)
	}))
	defer srv.Close()

	r := newCamoRunner(t)
	r.camo.media.src = []string{srv.URL + "/"}
	mustSet(t, r, "camoEnabled", "true", "camoTemplate", "video")
	r.CamoApply()
	first := r.camoSource().(*camo.Site)
	if len(first.Wanted()) == 0 {
		t.Fatal("一开始还没有照片")
	}
	waitMedia(t, r)
	r.CamoApply()
	site := r.camoSource().(*camo.Site)
	if len(site.Wanted()) != 0 || !strings.Contains(string(site.Home()), ".jpg") {
		t.Fatalf("处理好以后应换成照片: 还缺 %d", len(site.Wanted()))
	}
	n := len(site.PhotoNames()) // 种子是随机的:同一类照片不够时会挑到同一张,不一定正好 12 张
	if _, st := r.CamoPreview("video"); st == nil || st.Have != n || st.Want != n || n < 6 || st.Error != "" || st.Loading {
		t.Fatalf("状况: %+v", st)
	}
	files, _ := os.ReadDir(filepath.Join(r.DataDir(), "camo-media"))
	if len(files) != n || int(hits.Load()) != n {
		t.Fatalf("原图应存下 %d 张、各取一次: %d 张,取了 %d 次", n, len(files), hits.Load())
	}

	r2 := newCamoRunner(t)
	r2.camo.media.src = []string{"http://127.0.0.1:9/"} // 连不上
	mustSet(t, r2, "camoEnabled", "true", "camoTemplate", "blog")
	r2.CamoApply()
	_ = r2.camoSource()
	waitMedia(t, r2)
	site2, st := r2.CamoPreview("blog")
	if len(site2.Wanted()) == 0 || st == nil || st.Have != 0 || st.Error == "" || st.Retry <= time.Now().Unix() {
		t.Fatalf("取不到时应记下原因和下次再试的时间: %+v", st)
	}
	r2.camo.media.mu.Lock()
	running := r2.camo.media.running
	r2.camo.media.mu.Unlock()
	if running {
		t.Fatal("没到再试的时间不该又去取")
	}
	// 到了再试的时间:伪装站再被访问(读设置)时接着取,不用等改设置、开设置页
	r2.camo.media.mu.Lock()
	r2.camo.media.retryAt = time.Now().Add(-time.Second)
	r2.camo.media.src = []string{srv.URL + "/"}
	r2.camo.media.mu.Unlock()
	r2.camo.mu.Lock()
	r2.camo.at = time.Time{} // 10 秒的缓存过了
	r2.camo.mu.Unlock()
	_ = r2.camoSource()
	waitMedia(t, r2)
	r2.CamoApply()
	if s := r2.camoSource().(*camo.Site); len(s.Wanted()) != 0 {
		t.Fatalf("到点以后应自己接着取完: 还缺 %d", len(s.Wanted()))
	}
}

// 没开页面伪装时,设置页的预览不去取照片(只是打开设置页,面板不该去连外网;自审 G3):状况标 Off、不算正在取;
// 开了以后预览照常去取。
func TestCamoPreviewOffNoFetch(t *testing.T) {
	photo := testPhoto(t)
	withManifest(t, photo)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(photo)
	}))
	defer srv.Close()

	r := newCamoRunner(t)
	r.camo.media.src = []string{srv.URL + "/"}
	site, st := r.CamoPreview("video")
	r.camo.media.mu.Lock()
	running := r.camo.media.running
	r.camo.media.mu.Unlock()
	if len(site.Wanted()) == 0 || st == nil || !st.Off || st.Loading || st.Have != 0 || running || hits.Load() != 0 {
		t.Fatalf("没开时预览不该去取: %+v running=%v hits=%d", st, running, hits.Load())
	}
	mustSet(t, r, "camoEnabled", "true")
	if _, st := r.CamoPreview("video"); st == nil || st.Off {
		t.Fatalf("开了以后不该再标 Off: %+v", st)
	}
	waitMedia(t, r)
	if _, st := r.CamoPreview("video"); hits.Load() == 0 || st == nil || st.Have != st.Want || st.Off {
		t.Fatalf("开了以后预览应去取: %+v hits=%d", st, hits.Load())
	}
}

// 处理照片时 panic(审查 F-D2):进程不能跟着崩 —— 面板和全部线路在同一个进程里,同一个种子、同一张图每次都会走到同一处。
// 记下原因、放掉「正在取」、按没取全退避,照片的位置照旧是插图;之后处理恢复正常、到了再试的时间,照常取完。
// (反向对照:把 camoMediaRun 开头的 recover 去掉,这个测试会让整个测试进程崩掉。)
func TestCamoMediaRunSurvivesPanic(t *testing.T) {
	photo := testPhoto(t)
	withManifest(t, photo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(photo) }))
	defer srv.Close()
	old := camoProcess
	camoProcess = func([]byte, uint64, string, int, int) ([]byte, error) { panic("boom") }
	defer func() { camoProcess = old }()

	r := newCamoRunner(t)
	r.camo.media.src = []string{srv.URL + "/"}
	mustSet(t, r, "camoEnabled", "true", "camoTemplate", "video")
	r.CamoApply()
	if s := r.camoSource().(*camo.Site); len(s.Wanted()) == 0 {
		t.Fatal("一开始还没有照片")
	}
	waitMedia(t, r)
	site, st := r.CamoPreview("video")
	if len(site.Wanted()) == 0 || st == nil || st.Have != 0 || st.Loading || !strings.Contains(st.Error, "boom") || st.Retry <= time.Now().Unix() {
		t.Fatalf("处理出错时应记下原因、不算正在取、定下再试的时间,照片的位置还是插图: %+v", st)
	}

	camoProcess = old
	r.camo.media.mu.Lock()
	r.camo.media.retryAt = time.Now().Add(-time.Second)
	r.camo.media.mu.Unlock()
	r.CamoApply()
	_ = r.camoSource()
	waitMedia(t, r)
	r.CamoApply()
	if s := r.camoSource().(*camo.Site); len(s.Wanted()) != 0 {
		t.Fatalf("处理恢复正常、到点以后应照常取完: 还缺 %d", len(s.Wanted()))
	}
}

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

func waitMedia(t *testing.T, r *Runner) {
	t.Helper()
	for i := 0; i < 400; i++ {
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
}

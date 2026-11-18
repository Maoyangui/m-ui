package camo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// testJPEG 一张 w×h 的渐变图(测试用的「原图」)
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), uint8((x + y) % 256), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// 图包清单:文件名都是「类别-天色-两位序号.jpg」、校验和是 64 位十六进制;视频的每个地点(限定了天色的按限定)、
// 博客的每个题材都挑得出照片,挑出来的天色对得上;录音棚、片场、作坊三类棚内照片都挑得到;同一个站里同一类别挑满之前不重样。
func TestMediaManifest(t *testing.T) {
	nameRe := regexp.MustCompile(`^[a-z]+-(dawn|day|dusk|night|rain|any)-\d\d\.jpg$`)
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if len(MediaFiles) < 80 {
		t.Fatalf("图包只有 %d 张", len(MediaFiles))
	}
	for n, h := range MediaFiles {
		if !nameRe.MatchString(n) || !hexRe.MatchString(h) {
			t.Fatalf("清单里这一项不对: %s %s", n, h)
		}
	}
	for _, p := range videoPlaces {
		ph := photoFor(1, 0, p.kind, p.tod, map[string]bool{})
		if ph == "" {
			t.Fatalf("视频地点 %s(%s)挑不出照片", p.name, p.kind)
		}
		if p.tod != "" && todOf(ph) != p.tod {
			t.Fatalf("%s 要 %s 的照片,挑到了 %s", p.name, p.tod, ph)
		}
	}
	for _, tp := range blogTopics {
		k, tod, _ := strings.Cut(tp.kind, ":")
		if ph := photoFor(1, 0, k, tod, map[string]bool{}); ph == "" || (tod != "" && todOf(ph) != tod) {
			t.Fatalf("博客题材 %q(%s)挑出 %q", tp.text, tp.kind, ph)
		}
	}
	kinds := map[string]bool{}
	for i := 0; i < 64; i++ {
		kinds[studioKind(photoFor(uint64(i), i, "studio", "", map[string]bool{}))] = true
	}
	if len(kinds) != 3 {
		t.Fatalf("录音棚、片场、作坊三类都该挑得到: %v", kinds)
	}
	used := map[string]bool{}
	for i := range mediaByKind["sea"] {
		n := photoFor(7, i, "sea", "", used)
		if used[n] {
			t.Fatalf("还有没用过的海边照片,却挑了用过的 %s", n)
		}
		used[n] = true
	}
	if photoFor(7, 0, "nope", "", used) != "" {
		t.Fatal("没有的类别应挑不出")
	}
}

// 处理:出来的是要的尺寸、合法的 JPEG;同一种子同一张图每次一样;换种子、换文件名都不一样;
// 尺寸不对、不是图片、原图太小的报错。
func TestProcess(t *testing.T) {
	orig := testJPEG(t, 1280, 720)
	a, err := Process(orig, 7, "sea-day-01.jpg", 480, 270)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(a)); err != nil || cfg.Width != 480 || cfg.Height != 270 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if b, _ := Process(orig, 7, "sea-day-01.jpg", 480, 270); !bytes.Equal(a, b) {
		t.Fatal("同一种子同一张图应一样")
	}
	c, _ := Process(orig, 8, "sea-day-01.jpg", 480, 270)
	d, _ := Process(orig, 7, "sea-day-02.jpg", 480, 270)
	if bytes.Equal(a, c) || bytes.Equal(a, d) {
		t.Fatal("换种子、换文件名应不一样")
	}
	for _, sz := range [][2]int{{960, 480}, {1280, 560}, {96, 54}, {360, 240}} {
		out, err := Process(orig, 7, "x.jpg", sz[0], sz[1])
		if err != nil {
			t.Fatal(err)
		}
		if cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out)); cfg.Width != sz[0] || cfg.Height != sz[1] {
			t.Fatalf("%v: %+v", sz, cfg)
		}
	}
	if _, err := Process(orig, 7, "x.jpg", 0, 10); err == nil {
		t.Fatal("尺寸不对应报错")
	}
	if _, err := Process([]byte("not a jpeg"), 7, "x.jpg", 100, 100); err == nil {
		t.Fatal("不是图片应报错")
	}
	if _, err := Process(testJPEG(t, 40, 30), 7, "x.jpg", 100, 100); err == nil {
		t.Fatal("原图太小应报错")
	}
}

type fakePhotos struct{ b []byte }

func (f fakePhotos) Photo(name string, w, h int) []byte { return f.b }

// 有照片时:视频站、博客的封面换成照片(jpg,能打开),一张不缺;挑的照片和没有照片时是同一批,站上的文字一模一样
// (只是图片地址不同);设置页预览里内联小图、没有脚本;整站不超过 3 MB。没有照片时画插图,用到的照片都记进 Wanted。
func TestBuildWithPhotos(t *testing.T) {
	small, err := Process(testJPEG(t, 1280, 720), 1, "x.jpg", 96, 54)
	if err != nil {
		t.Fatal(err)
	}
	imgRe := regexp.MustCompile(` src="[^"]+\.(?:svg|jpg)"`)
	jpgRe := regexp.MustCompile(` src="(/[^"]+\.jpg)"`)
	for _, tmpl := range []string{"video", "blog"} {
		for _, seed := range seeds {
			plain := New(tmpl, seed, testNow)
			if len(plain.Wanted()) == 0 || len(plain.PhotoNames()) < 6 {
				t.Fatalf("%s/%d: 没有照片时应记下要的照片: %d %d", tmpl, seed, len(plain.Wanted()), len(plain.PhotoNames()))
			}
			s := Build(tmpl, seed, testNow, fakePhotos{small})
			if len(s.Wanted()) != 0 || strings.Join(s.PhotoNames(), ",") != strings.Join(plain.PhotoNames(), ",") {
				t.Fatalf("%s/%d: 照片都有时不该缺,且挑的应是同一批", tmpl, seed)
			}
			home := string(s.Home())
			if imgRe.ReplaceAllString(home, "") != imgRe.ReplaceAllString(string(plain.Home()), "") {
				t.Fatalf("%s/%d: 有没有照片,首页上的文字应一样", tmpl, seed)
			}
			m := jpgRe.FindStringSubmatch(home)
			if m == nil {
				t.Fatalf("%s/%d: 首页上没有照片", tmpl, seed)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, m[1], nil))
			if w.Code != 200 || w.Header().Get("Content-Type") != jpegType || !bytes.Equal(w.Body.Bytes(), small) {
				t.Fatalf("%s/%d: 照片 %s 打不开: %d %v", tmpl, seed, m[1], w.Code, w.Header())
			}
			p := string(s.Preview())
			if strings.Contains(p, ` src="/`) || !strings.Contains(p, "data:image/jpeg;base64,") || strings.Contains(p, "<script") {
				t.Fatalf("%s/%d: 预览应内联小图、拿掉脚本", tmpl, seed)
			}
			if s.Size() > 3<<20 {
				t.Fatalf("%s/%d: 带照片的站 %d KB,太大", tmpl, seed, s.Size()>>10)
			}
		}
	}
	if s := New("mirror", 7, testNow); len(s.Wanted()) != 0 || len(s.PhotoNames()) != 0 {
		t.Fatal("镜像站不用照片")
	}
}

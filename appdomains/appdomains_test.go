package appdomains

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 假的域名库:写法与 v2fly/domain-list-community 一致(注释、属性、include、正则、关键字、完整域名)
var files = map[string]string{
	"douyin": `# 抖音
include:douyin-extra
amemv.com
domain:douyinvod.com @cn
full:api.amemv.com
full:only.example.net
keyword:douyin
regexp:^v\d+\.douyin\.com$
domain:DOUYIN.com # 大小写不同也算同一个
include:missing-list
`,
	"douyin-extra": "include:douyin\nsnssdk.com\n",
	"google":       "google\ngoogle.com\n1.2.3.4\nbad domain.com\n",
}

func fakeRepo(t *testing.T, primaryStatus int) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits1, hits2 atomic.Int32
	serve := func(hits *atomic.Int32, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.URL.Path == "/flat" {
				w.Write([]byte(`{"files":[{"name":"/data/douyin"},{"name":"/data/netflix"},{"name":"/data/netflix-ads"},{"name":"/README.md"}]}`))
				return
			}
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/data/")
			body, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(body))
		}))
	}
	s1 := serve(&hits1, primaryStatus)
	s2 := serve(&hits2, 0)
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	return &Client{Bases: []string{s1.URL + "/data/", s2.URL + "/data/"}, Indexes: []string{s2.URL + "/flat"}, HTTP: &http.Client{Timeout: 5 * time.Second}, TTL: time.Hour}, &hits1, &hits2
}

func TestLookupExpandsIncludesAndKeepsSupportedForms(t *testing.T) {
	c, _, _ := fakeRepo(t, 0)
	res := c.Lookup(context.Background(), []string{"抖音", " 抖音 "})
	if len(res) != 1 {
		t.Fatalf("同名只查一次,得到 %d 条", len(res))
	}
	r := res[0]
	if r.Error != "" || r.List != "douyin" {
		t.Fatalf("抖音应查到 douyin 列表: %+v", r)
	}
	if want := []string{"snssdk.com", "amemv.com", "douyinvod.com", "douyin.com"}; !reflect.DeepEqual(r.Suffix, want) {
		t.Fatalf("域名后缀 = %v,应为 %v(include 展开、循环引用不死循环、属性与注释去掉、大小写归一、去重)", r.Suffix, want)
	}
	if want := []string{"only.example.net"}; !reflect.DeepEqual(r.Full, want) {
		t.Fatalf("完整域名 = %v,应为 %v(api.amemv.com 已被 amemv.com 盖住)", r.Full, want)
	}
	if !reflect.DeepEqual(r.Keyword, []string{"douyin"}) || r.Skipped != 1 {
		t.Fatalf("关键字 = %v、跳过 = %d,应为 [douyin] 与 1(正则不支持)", r.Keyword, r.Skipped)
	}
}

func TestLookupFiltersInvalidAndKeepsTLD(t *testing.T) {
	c, _, _ := fakeRepo(t, 0)
	r := c.Lookup(context.Background(), []string{"谷歌"})[0]
	if !reflect.DeepEqual(r.Suffix, []string{"google", "google.com"}) {
		t.Fatalf("应保留顶级域 google,去掉 IP 与带空格的:%v", r.Suffix)
	}
}

func TestLookupNotFoundSuggests(t *testing.T) {
	c, _, _ := fakeRepo(t, 0)
	r := c.Lookup(context.Background(), []string{"Netf"})[0]
	if r.Error == "" || !reflect.DeepEqual(r.Suggest, []string{"netflix", "netflix-ads"}) {
		t.Fatalf("没找到应给出相近的列表: %+v", r)
	}
	r = c.Lookup(context.Background(), []string{"某个中文应用"})[0]
	if r.Error == "" || r.List != "" {
		t.Fatalf("认不出的名字应报没找到: %+v", r)
	}
}

func TestLookupFallbackAndCache(t *testing.T) {
	// 第一个地址出错(不是 404):换镜像;查过的缓存起来,第二次不再请求
	c, h1, h2 := fakeRepo(t, http.StatusBadGateway)
	r := c.Lookup(context.Background(), []string{"netflix-nothing", "google"})
	if r[1].Error != "" || len(r[1].Suffix) != 2 {
		t.Fatalf("主地址出错时应换镜像: %+v", r[1])
	}
	before := h1.Load() + h2.Load()
	c.Lookup(context.Background(), []string{"google"})
	if h1.Load()+h2.Load() != before {
		t.Fatal("查过的列表应走缓存,不再请求")
	}
	// 第一个地址明确说没有(404):就是没有,不再去镜像问
	c2, _, m2 := fakeRepo(t, http.StatusNotFound)
	if r := c2.Lookup(context.Background(), []string{"google"})[0]; r.Error == "" {
		t.Fatalf("主地址 404 应报没找到: %+v", r)
	}
	if m2.Load() != 1 { // 只有一次:取索引推荐相近名字
		t.Fatalf("主地址 404 时不该再去镜像取列表,镜像被请求了 %d 次", m2.Load())
	}
}

func TestListName(t *testing.T) {
	for in, want := range map[string]string{
		"抖音": "douyin", "ChatGPT": "openai", "Prime Video": "primevideo", "Netflix": "netflix",
		"category-ai-chat-!cn": "category-ai-chat-!cn", "../etc/passwd": "", "a b/c": "", "": "",
	} {
		if got := ListName(in); got != want {
			t.Errorf("ListName(%q) = %q,应为 %q", in, got, want)
		}
	}
}

func TestLookupPicksPartOfBigList(t *testing.T) {
	files["tencent"] = "include:qcloud\nqq.com\nwechat.com\nservicewechat.com\nwxcloudrun.com\nad.weixin.qq.com @ads\nqpic.cn\n"
	files["qcloud"] = "wechat-in-include.com\n"
	defer func() { delete(files, "tencent"); delete(files, "qcloud") }()
	c, _, _ := fakeRepo(t, 0)
	r := c.Lookup(context.Background(), []string{"微信"})[0]
	want := []string{"wechat.com", "servicewechat.com", "wxcloudrun.com", "qpic.cn", "weixin.qq.com", "wx.qq.com"}
	if r.Error != "" || r.List != "tencent" || !r.Part || !reflect.DeepEqual(r.Suffix, want) {
		t.Fatalf("微信应从 tencent 里挑出相关的(不展开 include、不带 qq.com,ad.weixin.qq.com 被 weixin.qq.com 盖住):%+v", r)
	}
}

func TestLookupSearchesUmbrellasWhenNoList(t *testing.T) {
	files["microsoft"] = "live.com\noutlook.com\nmyoutlook.net\nfull:outlook.office365.com\n"
	defer delete(files, "microsoft")
	c, _, _ := fakeRepo(t, 0)
	r := c.Lookup(context.Background(), []string{"Outlook"})[0]
	if r.Error != "" || r.List != "microsoft" || !r.Part ||
		!reflect.DeepEqual(r.Suffix, []string{"outlook.com"}) || !reflect.DeepEqual(r.Full, []string{"outlook.office365.com"}) {
		t.Fatalf("没有 outlook 列表时应去大厂列表里找以 outlook 开头的段(myoutlook.net 不算):%+v", r)
	}
	if r := c.Lookup(context.Background(), []string{"wechatt"})[0]; r.Error == "" || !reflect.DeepEqual(r.Suggest, []string{"wechat"}) {
		t.Fatalf("拼错的名字也应能推荐到 picks 里的名字:%+v", r)
	}
	if r := c.Lookup(context.Background(), []string{"liv"})[0]; r.Error == "" {
		t.Fatalf("太短的名字不该去大厂列表里找:%+v", r)
	}
}

func TestSuggestReadsGitHubTreeIndex(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	tree := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tree":[{"path":"data/netflix"},{"path":"data"},{"path":"README.md"},{"path":"data/nettv"}]}`))
	}))
	t.Cleanup(bad.Close)
	t.Cleanup(tree.Close)
	c := &Client{Indexes: []string{bad.URL, tree.URL}, HTTP: &http.Client{Timeout: 5 * time.Second}, TTL: time.Hour}
	if got := c.suggest(context.Background(), "net"); !reflect.DeepEqual(got, []string{"netflix", "nettv"}) {
		t.Fatalf("第一份索引取不到应换下一份,并认得 GitHub 目录树的格式:%v", got)
	}
}

// 几个名字同时去大厂列表里找:同一份列表只请求一次,后来的等前一个取回来。
func TestConcurrentLookupsFetchEachListOnce(t *testing.T) {
	var tencent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/data/tencent" {
			http.NotFound(w, r)
			return
		}
		tencent.Add(1)
		time.Sleep(150 * time.Millisecond) // 让几个查询都赶上同一次下载
		w.Write([]byte("aaaa.com\nbbbb.com\ncccc.com\ndddd.com\n"))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Bases: []string{srv.URL + "/data/"}, HTTP: &http.Client{Timeout: 5 * time.Second}, TTL: time.Hour}
	res := c.Lookup(context.Background(), []string{"aaaa", "bbbb", "cccc", "dddd"})
	for _, r := range res {
		if r.Error != "" || r.List != "tencent" || len(r.Suffix) != 1 {
			t.Fatalf("每个名字都应在 tencent 里找到自己的域名: %+v", r)
		}
	}
	if n := tencent.Load(); n != 1 {
		t.Fatalf("tencent 列表被请求了 %d 次,应只请求 1 次", n)
	}
}

// 先来的那个请求被取消了,在等它的另一个请求不能跟着报错,要自己再取。
func TestWaiterRefetchesWhenFirstCallerCancelled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(300 * time.Millisecond):
			w.Write([]byte("netflix.com\n"))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{Bases: []string{srv.URL + "/data/"}, HTTP: &http.Client{Timeout: 5 * time.Second}, TTL: time.Hour}
	ctx1, cancel1 := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.file(ctx1, "netflix"); first <- err }()
	time.Sleep(50 * time.Millisecond) // 让第一个先开始取
	second := make(chan error, 1)
	var lines []string
	go func() { var err error; lines, err = c.file(context.Background(), "netflix"); second <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel1()
	if err := <-first; err == nil {
		t.Fatal("被取消的那个应该报错")
	}
	if err := <-second; err != nil || len(lines) != 1 {
		t.Fatalf("等着的那个应自己再取成功: %v %v", lines, err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("应请求 2 次(第一次被取消、第二次重取),实际 %d", n)
	}
}

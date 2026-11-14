package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

const extYAML = `proxies:
  - {name: '剩余流量:120.53 GB', type: ss, server: 127.0.0.1, port: 1, cipher: aes-128-gcm, password: x}
  - {name: a, type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}
`

func extRunner(t *testing.T, url string) (*Runner, uint) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	e := model.ExtNode{Name: "A", Type: "sub", Value: url, Enabled: true, Exclude: "剩余流量"}
	db.Create(&e)
	return &Runner{db: db}, e.Id
}

func extRow(t *testing.T, r *Runner, id uint) model.ExtNode {
	t.Helper()
	var e model.ExtNode
	if err := r.db.First(&e, id).Error; err != nil {
		t.Fatal(err)
	}
	return e
}

// 内容没变只更新抓取时间,不重写缓存;节点数是输出的个数(扣除被关键词排除的)。
func TestRefreshExtSkipsWriteWhenUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, extYAML) }))
	defer srv.Close()
	r, id := extRunner(t, srv.URL)
	res, err := r.RefreshExt(id)
	if err != nil || res.Unchanged || res.Total != 2 || res.Out != 1 {
		t.Fatalf("第一次刷新: %+v %v", res, err)
	}
	e := extRow(t, r, id)
	if e.Cache != extYAML || e.ContentSig == "" || e.FetchAgent != "clash" || e.NodeCount != 1 || e.NodeTotal != 2 || e.LastFetch == 0 {
		t.Fatalf("库里没记对: %+v", e)
	}
	// 往缓存里加一行注释(解析结果一样):内容没变时不该重写,注释应该还在
	marked := extYAML + "# marker\n"
	r.db.Model(&model.ExtNode{}).Where("id = ?", id).Updates(map[string]interface{}{"cache": marked, "last_fetch": 1})
	res, err = r.RefreshExt(id)
	if err != nil || !res.Unchanged {
		t.Fatalf("内容没变应报 unchanged: %+v %v", res, err)
	}
	e = extRow(t, r, id)
	if e.Cache != marked || e.LastFetch <= 1 {
		t.Fatalf("内容没变时只应更新抓取时间: cache 改写了=%v last_fetch=%d", e.Cache != marked, e.LastFetch)
	}
}

// 审计 I5:抓取失败(429 等)保留旧缓存,只记错误与尝试时间;上次成功的时间不动(面板显示"上次成功 x 前")。
func TestRefreshExtKeepsCacheOnError(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "ClashMeta") {
			fmt.Fprint(w, "c3M6Ly9ZV1Z6TFRFeU9DMW5ZMjA2ZUFAMS4yLjMuNDo0NDMjb25seQ==") // 另一个身份给的是另一份内容
			return
		}
		fmt.Fprint(w, extYAML)
	}))
	defer srv.Close()
	r, id := extRunner(t, srv.URL)
	if _, err := r.RefreshExt(id); err != nil {
		t.Fatal(err)
	}
	ok := extRow(t, r, id)
	fail.Store(true)
	if _, err := r.RefreshExt(id); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("应报 429: %v", err)
	}
	e := extRow(t, r, id)
	if e.Cache != ok.Cache || e.ContentSig != ok.ContentSig || e.LastFetch != ok.LastFetch || e.LastError == "" || e.LastTry == 0 {
		t.Fatalf("失败时应保留旧缓存、只记错误: %+v", e)
	}
}

// 同一条外部节点的手动刷新与定时刷新撞在一起只抓一次:后来的排队,等前一个做完直接用它的结果。
func TestRefreshExtSerializedPerExt(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		fmt.Fprint(w, extYAML)
	}))
	defer srv.Close()
	r, id := extRunner(t, srv.URL)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = r.RefreshExt(id) }()
	for deadline := time.Now().Add(5 * time.Second); hits.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("第一个刷新没发出请求")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := extRow(t, r, id) // 第二个(定时)刷新来时库里的样子:前一个还没写回
	var second ExtRefreshResult
	var secondErr error
	wg.Add(1)
	go func() { defer wg.Done(); second, secondErr = r.refreshExtAfter(id, before) }()
	time.Sleep(50 * time.Millisecond) // 让第二个排上队(早一点晚一点结果都一样:它看到的 before 已固定)
	close(release)
	wg.Wait()
	if n := hits.Load(); n != 1 {
		t.Fatalf("应只抓一次,实际 %d 次", n)
	}
	if secondErr != nil || second.Total != 2 {
		t.Fatalf("排队的那个应拿到前一个的结果: %+v %v", second, secondErr)
	}
	// 排队期间地址改了:不能拿旧地址的结果交差,要按新地址再抓
	before = extRow(t, r, id)
	r.db.Model(&model.ExtNode{}).Where("id = ?", id).Update("value", srv.URL+"/?v=2")
	if _, err := r.refreshExtAfter(id, before); err != nil || hits.Load() != 2 {
		t.Fatalf("地址变了应重新抓: %v hits=%d", err, hits.Load())
	}
}

// 老库升级上来新加的列是 NULL:定时刷新照样挑得到它(NULL 和任何数比较都不成立,不按 0 算的话老订阅再也不会自动刷新);
// 刚失败过的按间隔再试,不每分钟敲一次。
func TestDueExtsHandlesUpgradedRows(t *testing.T) {
	r, id := extRunner(t, "https://example.com/sub")
	r.db.Exec("UPDATE ext_nodes SET last_try = NULL, node_total = NULL, prefs_rev = NULL, last_fetch = 100 WHERE id = ?", id)
	if due := r.dueExts(1000); len(due) != 1 {
		t.Fatalf("升级上来的老订阅应到期: %d", len(due))
	}
	r.db.Model(&model.ExtNode{}).Where("id = ?", id).Update("last_try", 2000) // 刚刚失败过一次
	if due := r.dueExts(1000); len(due) != 0 {
		t.Fatalf("刚试过的不该马上再刷: %d", len(due))
	}
}

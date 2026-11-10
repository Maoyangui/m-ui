package runner

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
)

func TestFirstPing(t *testing.T) {
	const now = int64(1_800_000_000)
	jitter := int64(time.Hour)
	for _, c := range []struct {
		name string
		last int64
		want time.Duration
	}{
		{"从没发过", 0, time.Hour},
		{"一小时前发过,等满 24 小时", now - 3600, 23 * time.Hour},
		{"刚好 24 小时前", now - 24*3600, time.Hour},
		{"早就过了", now - 72*3600, time.Hour},
		{"时间在未来(改过时钟)", now + 3600, time.Hour},
	} {
		if got := firstPing(c.last, now, jitter); got != c.want {
			t.Errorf("%s: 实际 %v,应为 %v", c.name, got, c.want)
		}
	}
}

func TestRetryPing(t *testing.T) {
	for fails := 1; fails < pingTriesPerDay; fails++ {
		for _, j := range []int64{0, int64(pingRetrySpread) - 1} {
			next, n := retryPing(fails, j)
			if next < time.Hour || next >= 2*time.Hour || n != fails {
				t.Fatalf("第 %d 次失败: %v %d,应在 1–2 小时后重试", fails, next, n)
			}
		}
	}
	if next, n := retryPing(pingTriesPerDay, 0); next != 24*time.Hour || n != 0 {
		t.Fatalf("一天试满 %d 次就等明天: %v %d", pingTriesPerDay, next, n)
	}
}

// 只有 GitHub 记上了才写 statsPingAt 并等 24 小时;没记上(5xx、连不通)不写,1–2 小时后再试。
// 全程打本地的假服务器,不碰真实的下载计数。
func TestPingOnceRecordsOnlyWhenCounted(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, dbPath: filepath.Join(dir, "x.db")}

	var status atomic.Int32
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close() // 连不通的地址
	old := pingURL
	defer func() { pingURL = old }()

	recorded := func() int64 {
		v, _ := strconv.ParseInt(r.setting("statsPingAt"), 10, 64)
		return v
	}
	retrying := func(name string, next time.Duration, fails, want int) {
		t.Helper()
		if next < time.Hour || next >= 2*time.Hour || fails != want || recorded() != 0 {
			t.Fatalf("%s: 等 %v、失败 %d 次、statsPingAt=%d;应 1–2 小时后重试、失败 %d 次、不写 statsPingAt", name, next, fails, recorded(), want)
		}
	}

	pingURL = srv.URL
	status.Store(http.StatusInternalServerError)
	next, fails := r.pingOnce(0)
	retrying("GitHub 回 500", next, fails, 1)

	pingURL = goneURL
	next, fails = r.pingOnce(fails)
	retrying("连不通", next, fails, 2)

	status.Store(http.StatusNotFound)
	pingURL = srv.URL
	next, fails = r.pingOnce(fails)
	retrying("文件不在(404)", next, fails, 3)

	next, fails = r.pingOnce(fails)
	if next != 24*time.Hour || fails != 0 || recorded() != 0 {
		t.Fatalf("一天试满 4 次还不行就等明天: %v %d %d", next, fails, recorded())
	}

	// github.com 给了跳转,跳转后的下载地址连不上:GitHub 在发跳转时已经记上了,算成功,不再重试(重试会多算)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, goneURL+"/ping", http.StatusFound)
	}))
	defer redirect.Close()
	pingURL = redirect.URL
	before := time.Now().Unix()
	next, fails = r.pingOnce(2)
	if next != 24*time.Hour || fails != 0 || recorded() < before {
		t.Fatalf("跳转后下载地址连不上也算记上了: %v %d %d", next, fails, recorded())
	}

	// 200:写下时间,24 小时后再发
	db.Exec("DELETE FROM settings WHERE key = 'statsPingAt'")
	pingURL = srv.URL
	status.Store(http.StatusOK)
	next, fails = r.pingOnce(1)
	if next != 24*time.Hour || fails != 0 || recorded() < before {
		t.Fatalf("200 应写下时间并等 24 小时: %v %d %d", next, fails, recorded())
	}

	// 总开关关着:不发请求,也不写
	db.Exec("DELETE FROM settings WHERE key = 'statsPingAt'")
	if err := r.setSetting("supportOnline", "false"); err != nil {
		t.Fatal(err)
	}
	n := hits.Load()
	next, fails = r.pingOnce(3)
	if next != 24*time.Hour || fails != 0 || hits.Load() != n || recorded() != 0 {
		t.Fatalf("关掉后不该发: %v %d,请求 %d 次", next, fails, hits.Load()-n)
	}
}

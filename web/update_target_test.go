package web

import (
	"testing"

	"github.com/Maoyangui/m-ui/selfupdate"
)

// 一键更新只在真有新版本时执行:已是最新、或本机比最新发布还新时拒绝,不能重装或降级一次、重启数据面(审计 MB07)。
// 缓存说没有新版本时强制重查一次,刚发布的新版本不会被缓存挡住。
func TestUpdateOnlyWhenNewer(t *testing.T) {
	rechecked := 0
	recheck := func(i selfupdate.Info) func() selfupdate.Info {
		return func() selfupdate.Info { rechecked++; return i }
	}
	same := selfupdate.Info{Current: "0.7.0", Latest: "v0.7.0"}
	if _, err := updateTarget(same, recheck(same)); err != errUpToDate || rechecked != 1 {
		t.Fatalf("同版本应拒绝且先重查一次: err=%v 重查=%d", err, rechecked)
	}
	older := selfupdate.Info{Current: "0.7.1-dev", Latest: "v0.7.0"}
	if _, err := updateTarget(older, recheck(older)); err != errUpToDate {
		t.Fatalf("本机比最新发布还新应拒绝: %v", err)
	}
	fresh := selfupdate.Info{Current: "0.7.0", Latest: "v0.7.1", HasUpdate: true}
	if got, err := updateTarget(same, recheck(fresh)); err != nil || got.Latest != "v0.7.1" {
		t.Fatalf("缓存过期时重查到新版本应放行: %v %+v", err, got)
	}
	rechecked = 0
	if got, err := updateTarget(fresh, recheck(same)); err != nil || got.Latest != "v0.7.1" || rechecked != 0 {
		t.Fatalf("缓存里有新版本直接用: %v %+v 重查=%d", err, got, rechecked)
	}
	if _, err := updateTarget(selfupdate.Info{}, recheck(selfupdate.Info{})); err != errNoRelease {
		t.Fatalf("查不到发布应报没有版本: %v", err)
	}
}

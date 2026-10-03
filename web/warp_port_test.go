//go:build !race

// 这条测试会重载真实的内嵌数据面,-race 下会撞上 sing-box 自己的竞争(route.(*NetworkManager).Start 写、
// 接口监听 goroutine 在 updateInterface 里读),与 m-ui 无关;同 user_rotate_dataplane_test.go,只在 -race 那一步跳过。

package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 改了 WARP 端口再重新启用,warp 上游要跟着指向新端口(以前已存在就直接返回,走 WARP 的线路全断);
// 管理员自己改过地址的上游不动;非法端口先拒绝,不能落库(审计 MB05)。
func TestWarpUpstreamFollowsPort(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	port := func() float64 {
		var up model.Upstream
		run.DB().Where("name = ?", "warp").First(&up)
		var o map[string]interface{}
		json.Unmarshal(up.Options, &o)
		p, _ := o["server_port"].(float64)
		return p
	}

	if created, err := s.ensureWarpUpstream(); err != nil || !created || port() != 40000 {
		t.Fatalf("首次应建 warp 上游指向 40000: created=%v err=%v port=%v", created, err, port())
	}
	run.SetSetting("warpPort", "40001")
	if _, err := s.ensureWarpUpstream(); err != nil || port() != 40001 {
		t.Fatalf("改端口后重新启用,上游应指向 40001: err=%v port=%v", err, port())
	}

	run.DB().Model(&model.Upstream{}).Where("name = ?", "warp").Update("options", []byte(`{"server":"10.0.0.2","server_port":1080}`))
	run.SetSetting("warpPort", "40002")
	s.ensureWarpUpstream()
	if port() != 1080 {
		t.Fatalf("管理员改成别的地址的上游不该被动: %v", port())
	}

	w := httptest.NewRecorder()
	s.handleOpsSub(w, httptest.NewRequest("POST", "http://x/app/api/ops/run", strings.NewReader(`{"task":"warp-enable","port":70000}`)))
	if w.Code != 400 || s.warpPort() != 40002 {
		t.Fatalf("非法端口应拒绝且不落库: %d warpPort=%d", w.Code, s.warpPort())
	}
}

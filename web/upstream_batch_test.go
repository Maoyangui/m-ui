package web

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 上游批量删除:有线路在用(自己选的出站、分流规则指向它)的不删;在上游组里的,那个组这次也一起删才删;
// 其余一起删,连同跟随外部节点的来源记录。响应里写明删了几条、哪些没删、为什么。
func TestUpstreamBatchDelete(t *testing.T) {
	s, call := groupServer(t)
	for i, n := range []string{"a", "b", "c", "d", "e", "f"} {
		if code, body := call("POST", "upstreams", socksUp(n, 1000+i)); code != 200 {
			t.Fatalf("建上游 %s: %d %s", n, code, body)
		}
	}
	if code, body := call("POST", "upstreams", groupUp("g", "b", "e")); code != 200 {
		t.Fatalf("建上游组: %d %s", code, body)
	}
	id := map[string]uint{}
	var ups []model.Upstream
	s.db.Find(&ups)
	for _, u := range ups {
		id[u.Name] = u.Id
	}
	s.db.Create(&model.Line{Name: "L1", Protocol: "socks", Port: 20001, UpstreamId: id["a"]})
	s.db.Create(&model.Line{Name: "L2", Protocol: "socks", Port: 20002, RouteRules: []byte(fmt.Sprintf(`[{"type":"domain_suffix","values":["example.com"],"to":%d}]`, id["f"]))})
	s.db.Create(&model.UpstreamSource{UpstreamId: id["c"], ExtId: 9, Name: "C-1"})
	batch := func(action string, names ...string) (int, map[string]interface{}) {
		t.Helper()
		ids := []uint{}
		for _, n := range names {
			ids = append(ids, id[n])
		}
		b, _ := json.Marshal(map[string]interface{}{"ids": ids, "action": action})
		code, body := call("POST", "upstreams/batch", string(b))
		var out map[string]interface{}
		json.Unmarshal([]byte(body), &out)
		return code, out
	}
	left := func() []string {
		var names []string
		var rows []model.Upstream
		s.db.Order("name asc").Find(&rows)
		for _, u := range rows {
			names = append(names, u.Name)
		}
		return names
	}

	code, out := batch("delete", "a", "b", "c", "f")
	if code != 200 || out["deleted"] != float64(1) {
		t.Fatalf("应只删掉 c: %d %v", code, out)
	}
	sk, _ := json.Marshal(out["skipped"])
	var skipped []upstreamBatchSkip
	json.Unmarshal(sk, &skipped)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Name < skipped[j].Name })
	want := []upstreamBatchSkip{{Name: "a", Lines: []string{"L1"}}, {Name: "b", Group: "g"}, {Name: "f", Lines: []string{"L2"}}}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("没删的和原因\n得 %+v\n应 %+v", skipped, want)
	}
	if got := left(); !reflect.DeepEqual(got, []string{"a", "b", "d", "e", "f", "g"}) {
		t.Fatalf("剩下的上游: %v", got)
	}
	var n int64
	s.db.Model(&model.UpstreamSource{}).Where("upstream_id = ?", id["c"]).Count(&n)
	if n != 0 {
		t.Fatal("删掉的上游,跟随来源也要一起删")
	}

	// 组和它的成员一起选:组先删,成员跟着删得掉
	if code, out = batch("delete", "b", "g", "e"); code != 200 || out["deleted"] != float64(3) {
		t.Fatalf("组和成员一起删: %d %v", code, out)
	}
	if got := left(); !reflect.DeepEqual(got, []string{"a", "d", "f"}) {
		t.Fatalf("剩下的上游: %v", got)
	}

	// 请求不对的一律拒绝,什么都不动
	if code, _ = batch("delete"); code != 400 {
		t.Fatalf("没选上游应拒绝: %d", code)
	}
	if code, body := call("POST", "upstreams/batch", fmt.Sprintf(`{"ids":[%d,99999],"action":"delete"}`, id["d"])); code != 400 {
		t.Fatalf("有上游不存在应拒绝: %d %s", code, body)
	}
	if code, _ = batch("rename", "d"); code != 400 {
		t.Fatalf("不认识的操作应拒绝: %d", code)
	}
	if got := left(); !reflect.DeepEqual(got, []string{"a", "d", "f"}) {
		t.Fatalf("被拒绝的请求不该删东西: %v", got)
	}
}

// 批量停止 / 恢复跟随:只动从外部节点加来的,已经是这个状态的不算;单条的开关照旧能用。
func TestUpstreamBatchFollow(t *testing.T) {
	s, call := groupServer(t)
	for i, n := range []string{"x", "y", "z"} {
		if code, body := call("POST", "upstreams", socksUp(n, 1100+i)); code != 200 {
			t.Fatalf("建上游 %s: %d %s", n, code, body)
		}
	}
	id := map[string]uint{}
	var ups []model.Upstream
	s.db.Find(&ups)
	for _, u := range ups {
		id[u.Name] = u.Id
	}
	s.db.Create(&model.UpstreamSource{UpstreamId: id["x"], ExtId: 1, Name: "X"})
	s.db.Create(&model.UpstreamSource{UpstreamId: id["y"], ExtId: 1, Name: "Y", Stop: model.FollowStopUser, Note: "unchanged", TriedAt: 123})
	run := func(action string) int {
		t.Helper()
		b, _ := json.Marshal(map[string]interface{}{"ids": []uint{id["x"], id["y"], id["z"]}, "action": action})
		code, body := call("POST", "upstreams/batch", string(b))
		var out map[string]int
		if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("%s: %d %s", action, code, body)
		}
		return out["changed"]
	}
	stop := func(name string) string {
		var src model.UpstreamSource
		s.db.First(&src, "upstream_id = ?", id[name])
		return src.Stop
	}
	if n := run("unfollow"); n != 1 || stop("x") != model.FollowStopUser || stop("y") != model.FollowStopUser {
		t.Fatalf("停止跟随:改了 %d 条,x=%q y=%q", n, stop("x"), stop("y"))
	}
	if n := run("follow"); n != 2 || stop("x") != "" || stop("y") != "" {
		t.Fatalf("恢复跟随:改了 %d 条,x=%q y=%q", n, stop("x"), stop("y"))
	}
	var y model.UpstreamSource
	s.db.First(&y, "upstream_id = ?", id["y"])
	if y.TriedAt != 0 || y.Note != "" {
		t.Fatalf("重新打开跟随要清掉上次的尝试和结论: %+v", y)
	}
	if n := run("follow"); n != 0 {
		t.Fatalf("已经在跟随的不算改动: %d", n)
	}
	// 单条的开关:不是从外部节点加来的照旧报错;是的照旧能关
	if code, _ := call("PUT", fmt.Sprintf("upstreams/%d/source", id["z"]), `{"follow":false}`); code != 400 {
		t.Fatalf("z 没有来源,单条开关应报错: %d", code)
	}
	if code, body := call("PUT", fmt.Sprintf("upstreams/%d/source", id["x"]), `{"follow":false}`); code != 200 || stop("x") != model.FollowStopUser {
		t.Fatalf("单条停止跟随: %d %s x=%q", code, body, stop("x"))
	}
}

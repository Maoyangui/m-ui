package rules

import (
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

func at(loc *time.Location, y, m, d, hh, mm int) time.Time {
	return time.Date(y, time.Month(m), d, hh, mm, 0, 0, loc)
}

func TestInWindow(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	// 2026-09-11 是周五
	r := model.Rule{Kind: KindSchedule, Start: "19:00", End: "23:00"}
	if !InWindow(r, at(sh, 2026, 9, 11, 20, 0), sh) {
		t.Fatal("20:00 应在 19:00-23:00 内")
	}
	if InWindow(r, at(sh, 2026, 9, 11, 23, 0), sh) {
		t.Fatal("结束时刻本身不算在内")
	}
	if InWindow(r, at(sh, 2026, 9, 11, 18, 59), sh) {
		t.Fatal("18:59 不在窗口内")
	}
	// 面板时区:UTC 12:00 = 上海 20:00,在窗口内;按 UTC 算就不在
	if !InWindow(r, time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC), sh) {
		t.Fatal("应按面板时区判定")
	}
	if InWindow(r, time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC), time.UTC) {
		t.Fatal("UTC 12:00 不该在 19:00-23:00 内")
	}
	// 跨午夜 + 星期:只在周五(5)生效,周六 01:00 属于周五那段;周六 23:30 不属于
	x := model.Rule{Kind: KindSchedule, Days: "5", Start: "23:00", End: "02:00"}
	if !InWindow(x, at(sh, 2026, 9, 11, 23, 30), sh) {
		t.Fatal("周五 23:30 应在窗口内")
	}
	if !InWindow(x, at(sh, 2026, 9, 12, 1, 0), sh) {
		t.Fatal("周六 01:00 属于周五那段跨午夜窗口")
	}
	if InWindow(x, at(sh, 2026, 9, 12, 23, 30), sh) {
		t.Fatal("周六 23:30 不在只选周五的窗口内")
	}
	if InWindow(x, at(sh, 2026, 9, 12, 2, 0), sh) {
		t.Fatal("02:00 已经出窗口")
	}
	// 全天:开始 = 结束
	all := model.Rule{Kind: KindSchedule, Days: "6,7", Start: "00:00", End: "00:00"}
	if !InWindow(all, at(sh, 2026, 9, 12, 15, 0), sh) || InWindow(all, at(sh, 2026, 9, 11, 15, 0), sh) {
		t.Fatal("开始 = 结束表示全天,按星期过滤")
	}
	if InWindow(model.Rule{Start: "25:00", End: "01:00"}, at(sh, 2026, 9, 11, 0, 30), sh) {
		t.Fatal("时刻解析不了应视为不在窗口")
	}
}

func TestTargets(t *testing.T) {
	users := []model.User{
		{Id: 1, Name: "a", Enabled: true},
		{Id: 2, Name: "b", Enabled: false},
		{Id: 3, Name: "c", Enabled: true, ResellerId: 9},
		{Id: 4, Name: "d", Enabled: true, ResellerId: 8},
	}
	names := func(us []model.User) string {
		s := ""
		for _, u := range us {
			s += u.Name
		}
		return s
	}
	if got := names(Targets(model.Rule{AllUsers: true}, users)); got != "acd" {
		t.Fatalf("全部用户应排除停用的: %s", got)
	}
	if got := names(Targets(model.Rule{UserIds: []byte(`[1,2,4]`)}, users)); got != "ad" {
		t.Fatalf("指定用户: %s", got)
	}
	if got := names(Targets(model.Rule{ResellerIds: []byte(`[9]`)}, users)); got != "c" {
		t.Fatalf("代理名下: %s", got)
	}
	if got := names(Targets(model.Rule{UserIds: []byte(`[1]`), ResellerIds: []byte(`[8]`)}, users)); got != "ad" {
		t.Fatalf("用户 + 代理并集: %s", got)
	}
}

func TestEffective(t *testing.T) {
	tight := func(up, down int) model.LimitState {
		return model.LimitState{UpMbps: up, DownMbps: down, TightenOnly: true}
	}
	over := func(up, down int) model.LimitState { return model.LimitState{UpMbps: up, DownMbps: down} }
	// 没有状态 = 用户自己的
	if up, down := Effective(50, 60, nil); up != 50 || down != 60 {
		t.Fatalf("无状态: %d/%d", up, down)
	}
	// 只升不降:规则 20 比自己的 50 严 → 20;规则 80 比自己的宽 → 仍是 50
	if up, _ := Effective(50, 0, []model.LimitState{tight(20, 0)}); up != 20 {
		t.Fatalf("只升不降取更严: %d", up)
	}
	if up, _ := Effective(50, 0, []model.LimitState{tight(80, 0)}); up != 50 {
		t.Fatalf("只升不降不能放宽: %d", up)
	}
	// 覆盖:规则 80 可以高于自己的 50
	if up, _ := Effective(50, 0, []model.LimitState{over(80, 0)}); up != 80 {
		t.Fatalf("覆盖模式应放宽到 80: %d", up)
	}
	// 用户不限(0)时规则值直接生效
	if up, _ := Effective(0, 0, []model.LimitState{tight(20, 0)}); up != 20 {
		t.Fatalf("用户不限时规则值生效: %d", up)
	}
	// 多条同时生效取最严;覆盖那条不能越过只升不降那条
	if up, _ := Effective(50, 0, []model.LimitState{tight(20, 0), over(80, 0)}); up != 20 {
		t.Fatalf("多条取最严: %d", up)
	}
	// 方向填 0 = 不改:上行按规则,下行还是自己的
	if up, down := Effective(50, 60, []model.LimitState{tight(20, 0)}); up != 20 || down != 60 {
		t.Fatalf("0 表示该方向不改: %d/%d", up, down)
	}
}

func TestWindowDelta(t *testing.T) {
	w := &window{}
	if d := w.add(0, 100, 600); d != 0 {
		t.Fatalf("单点增量 0: %d", d)
	}
	w.add(300, 400, 600)
	if d := w.add(590, 700, 600); d != 600 {
		t.Fatalf("窗口内增量 700-100: %d", d)
	}
	if d := w.add(700, 800, 600); d != 400 { // 0 秒那点已出窗口,最早是 300 秒的 400
		t.Fatalf("滑出窗口后从 400 起算: %d", d)
	}
	if d := w.add(710, 50, 600); d != 0 { // 用量变小 = 重置,窗口作废
		t.Fatalf("用量倒退应重开窗口: %d", d)
	}
	w.add(720, 250, 600)
	w.rebase()
	if d := w.add(730, 300, 600); d != 50 {
		t.Fatalf("rebase 后只算之后的增量: %d", d)
	}
}

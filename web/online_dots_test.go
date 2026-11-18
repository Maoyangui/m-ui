package web

import "testing"

// 用户 N4:线路的在线灯要「有用户连着」才亮,和线路详情的在线名单同一个来源。
// 本机的、还在列表里的副机的都算;已删除的服务器留下的旧报告不算。
func TestLinesWithUsersFrom(t *testing.T) {
	local := map[string]map[string][]string{"alice": {"1.2.3.4": {"香港 Reality"}, "5.6.7.8": {"香港 Reality", "日本 Hy2"}}}
	remote := map[uint]map[string]map[string][]string{
		2: {"bob": {"9.9.9.9": {"新加坡 AnyTLS"}}},
		9: {"carol": {"8.8.8.8": {"已删服务器上的线路"}}}, // 9 已经不在服务器列表里
	}
	got := linesWithUsersFrom(local, remote, []uint{1, 2})
	for _, want := range []string{"香港 Reality", "日本 Hy2", "新加坡 AnyTLS"} {
		if !got[want] {
			t.Fatalf("%s 有用户连着,应算: %v", want, got)
		}
	}
	if got["已删服务器上的线路"] || len(got) != 3 {
		t.Fatalf("已删服务器的旧报告不该算: %v", got)
	}
	if len(linesWithUsersFrom(nil, nil, nil)) != 0 {
		t.Fatal("没人在线时应为空")
	}
	// 和在线灯的组合:只有入站字节(探测转给伪装站的流量)、没有用户的线路不亮
	if lit := filterNames([]string{"香港 Reality", "只有探测流量的线路"}, got); len(lit) != 1 || lit[0] != "香港 Reality" {
		t.Fatalf("没有用户连着的线路不该亮: %v", lit)
	}
}

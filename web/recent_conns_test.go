package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// 单服务器:每条带服务器 id 与来源地址族,不带服务器名(只有一台,列出来没意义);按最近时间排、到上限截断。
func TestFairMergeSingleServer(t *testing.T) {
	self := model.Node{Id: 1, Name: "香港"}
	local := []hub.RecentConn{{IP: "1.1.1.1", Ts: 10}, {IP: "2001:db8::1", Ts: 30}, {IP: "::ffff:203.0.113.9", Ts: 20}}
	got := fairMerge(local, nil, self, false, 2)
	if len(got) != 2 || got[0].IP != "2001:db8::1" || got[1].IP != "::ffff:203.0.113.9" {
		t.Fatalf("按最近时间排、到上限截断: %+v", got)
	}
	if got[0].NodeId != 1 || got[0].Family != "v6" || got[1].Family != "v4" || got[0].Server != "" {
		t.Fatalf("要带服务器 id 与地址族(IPv4 映射地址按 IPv4),单服务器不带名字: %+v", got)
	}
}

// 多服务器按服务器公平取:一台有扫描噪声的服务器(一大批更新的记录)挤不掉别的服务器最近的那条,总数仍有上限;
// 每台先留最近的一条,其余按时间补满(本机第二条比噪声那台的都旧,让给噪声);本机的标上服务器 id 和名字,副机的保留 hub 标好的。
func TestFairMergeAcrossServers(t *testing.T) {
	self := model.Node{Id: 1, Name: "主机"}
	var noisy []hub.RecentConn
	for i := 0; i < 50; i++ { // 副机 2:扫描刷出 50 条,都比别的服务器新
		noisy = append(noisy, hub.RecentConn{IP: fmt.Sprintf("198.51.100.%d", i), Ts: int64(1000 + i), Server: "噪声", NodeId: 2, Failed: "tls"})
	}
	quiet := []hub.RecentConn{{IP: "2001:db8::7", Ts: 100, Server: "安静", NodeId: 3, Failed: "tls"}} // 副机 3:用户试过的那条,时间较早
	local := []hub.RecentConn{{IP: "203.0.113.1", Ts: 50, Failed: "auth"}, {IP: "203.0.113.2", Ts: 40, Failed: "auth"}}
	got := fairMerge(local, append(noisy, quiet...), self, true, 10)
	if len(got) != 10 {
		t.Fatalf("总数到上限: %d", len(got))
	}
	var seen = map[uint]int{}
	for i, c := range got {
		seen[c.NodeId]++
		if i > 0 && got[i-1].Ts < c.Ts {
			t.Fatalf("结果要按最近时间排: %+v", got)
		}
	}
	if seen[3] != 1 || seen[1] != 1 || seen[2] != 8 {
		t.Fatalf("每台都要留下自己最近的一条(安静的、本机各 1 条),其余按时间给噪声那台: %v", seen)
	}
	for _, c := range got {
		switch c.NodeId {
		case 1:
			if c.Server != "主机" || c.Family != "v4" {
				t.Fatalf("本机的要标上名字与地址族: %+v", c)
			}
		case 3:
			if c.Server != "安静" || c.Family != "v6" {
				t.Fatalf("副机的保留服务器名、标上地址族: %+v", c)
			}
		}
	}
	// 没超上限:原样全给
	if got := fairMerge(local, quiet, self, true, 10); len(got) != 3 || got[0].NodeId != 3 {
		t.Fatalf("没超上限时全给、按时间排: %+v", got)
	}
}

// 一台服务器最近很忙、别的服务器只有几天前的旧记录(跟踪器只按条数淘汰,不按时间过期)时(审查):
// 每台先留最近的一条,其余按时间补满 —— 忙的那台最近的记录不被别的服务器的旧记录挤掉
// (以前严格轮流,三台时忙的那台只剩三分之一,几分钟前的连接看不到,显示出来的却有两天前的)。
func TestFairMergeKeepsBusyServerRecent(t *testing.T) {
	self := model.Node{Id: 1, Name: "主机"}
	const now = int64(1_800_000_000)
	var busy, old []hub.RecentConn
	for i := 0; i < 100; i++ { // 本机:最近 100 分钟里 100 条
		busy = append(busy, hub.RecentConn{IP: fmt.Sprintf("203.0.113.%d", i), Ts: now - int64(i)*60})
	}
	for i := 0; i < 100; i++ { // 副机 B、C:两天前的
		old = append(old, hub.RecentConn{IP: fmt.Sprintf("198.51.100.%d", i), Ts: now - 2*86400 - int64(i), Server: "B", NodeId: 2},
			hub.RecentConn{IP: fmt.Sprintf("192.0.2.%d", i), Ts: now - 2*86400 - int64(i), Server: "C", NodeId: 3})
	}
	got := fairMerge(busy, old, self, true, 100)
	seen := map[uint]int{}
	for i, c := range got {
		seen[c.NodeId]++
		if i > 0 && got[i-1].Ts < c.Ts {
			t.Fatalf("结果要按最近时间排: %+v", got)
		}
	}
	if len(got) != 100 || seen[1] != 98 || seen[2] != 1 || seen[3] != 1 {
		t.Fatalf("B、C 各留最近的一条,其余给最近的记录(本机 98 条): %v", seen)
	}
	for i := 0; i < 98; i++ { // 本机留下的是它最近的 98 条
		if got[i].NodeId != 1 || got[i].Ts != now-int64(i)*60 {
			t.Fatalf("第 %d 条应是本机第 %d 新的: %+v", i+1, i+1, got[i])
		}
	}
	if got[98].Ts != now-2*86400 || got[99].Ts != now-2*86400 {
		t.Fatalf("B、C 留的是各自最近的那条: %+v %+v", got[98], got[99])
	}
}

// 接口:没有记录时给空数组(不是 null),带不带 ?fails=1 都一样(前端按数组处理)。
func TestRecentConnsEndpoint(t *testing.T) {
	s, _ := certServer(t)
	get := func(q string) string {
		w := httptest.NewRecorder()
		s.handleRecentConns(w, httptest.NewRequest(http.MethodGet, "http://x/app/api/conns/recent"+q, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 %d", w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}
	if got := get(""); got != "[]" {
		t.Fatalf("没有记录时要是空数组: %s", got)
	}
	if got := get("?fails=1"); got != "[]" {
		t.Fatalf("带失败那份也是空数组: %s", got)
	}
}

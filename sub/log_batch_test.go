package sub

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 订阅端口被扫描时一个周期里有几千个来源 IP(审计 I4):聚合好的 404 以前一条 INSERT 写完,每行 6 个变量,
// 五千多个 IP 就超过 SQLite 一条语句的变量上限,整批日志丢掉。
func TestFlushMissesManyIPs(t *testing.T) {
	s, db := shareServer(t)
	const n = 6000
	for i := 0; i < n; i++ {
		s.noteMiss(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255), "guess")
	}
	s.flushMisses()
	var rows int64
	db.Model(&model.SubLog{}).Where("format = ?", "miss").Count(&rows)
	if rows != n {
		t.Fatalf("%d 个来源 IP 应各落一行,实际 %d 行", n, rows)
	}
}

// 订阅访问日志先进内存、成批落库(审计 I4):以前每个订阅请求同步写一行,高峰时每个请求都去抢写锁、占一条连接。
// 一批几千条也要写得进去。
func TestSubLogsBuffered(t *testing.T) {
	s, db := shareServer(t)
	const n = 6000
	r := httptest.NewRequest("GET", "http://hk.example:2056/sub/alice", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	for i := 0; i < n; i++ {
		s.log(r, "alice", false, 200)
	}
	var rows int64
	db.Model(&model.SubLog{}).Count(&rows)
	if rows != 0 {
		t.Fatalf("访问日志不该逐条同步落库,已经有 %d 行", rows)
	}
	s.flushLogs()
	db.Model(&model.SubLog{}).Where("user = ?", "alice").Count(&rows)
	if rows != n {
		t.Fatalf("%d 条访问日志应一批落库,实际 %d 行", n, rows)
	}
}

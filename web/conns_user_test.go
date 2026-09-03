package web

import (
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

// 数据面日志里,认证成功的用户名在紧跟其后的那条 "[用户] inbound connection to" 上。
// 面板"最近入站连接"要把它归到同一条记录,并把行首时间按本机时区解析成绝对时间。
func TestRecentConnsUserAndTime(t *testing.T) {
	logger.SetEnabled(true)
	logger.Clear()
	defer logger.Clear()

	logger.Info("inbound/anytls[美国4] inbound connection from 39.144.90.180:58429")
	logger.Info("inbound/anytls[美国4] [LMEC1A389B797543AAB657] inbound connection to www.gstatic.com:80")
	logger.Info("inbound/hysteria2[香港1] inbound connection from 1.2.3.4:1000")
	logger.Info("inbound/hysteria2[香港1] [alice] inbound connection to example.com:443")
	logger.Info("inbound/hysteria2[香港1] inbound connection from 1.2.3.4:1001") // 同 IP 同线路 → 计数累加
	logger.Info("inbound/tuic[台湾2] inbound connection from 5.6.7.8:2000")      // 没有后续用户行 → 用户为空

	s := &Server{}
	got := map[string]recentConn{}
	for _, c := range s.recentConns(50) {
		got[c.IP+"|"+c.Line] = c
	}
	if len(got) != 3 {
		t.Fatalf("应聚合成 3 条,实际 %d: %+v", len(got), got)
	}
	if c := got["39.144.90.180|美国4"]; c.User != "LMEC1A389B797543AAB657" || c.Protocol != "anytls" || c.Count != 1 {
		t.Fatalf("美国4: %+v", c)
	}
	if c := got["1.2.3.4|香港1"]; c.User != "alice" || c.Count != 2 {
		t.Fatalf("香港1 应带用户且计数为 2: %+v", c)
	}
	if c := got["5.6.7.8|台湾2"]; c.User != "" {
		t.Fatalf("没有用户日志时不应乱认: %+v", c)
	}
	// 时间戳:按本机时区解析日志行首时间,应落在最近一分钟内
	c := got["1.2.3.4|香港1"]
	if d := time.Since(time.Unix(c.Ts, 0)); c.Ts == 0 || d < 0 || d > time.Minute {
		t.Fatalf("时间戳不合理: ts=%d 距今 %v", c.Ts, d)
	}
	// 最近的在最前
	list := s.recentConns(50)
	if list[0].IP != "5.6.7.8" {
		t.Fatalf("应按最近在前: %+v", list[0])
	}
}

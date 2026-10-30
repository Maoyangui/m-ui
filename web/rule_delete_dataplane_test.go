//go:build !race

// 起的是真实的内嵌数据面,-race 下跳过的原因见 agent_apply_e2e_test.go 开头。

package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 删除一条正在生效的限速规则,本机要立刻解除。删除接口直接把它造成的限速状态从库里删掉,接着判定的那一轮
// 看不出"有变化",以前就不会重新下发 —— 副机随快照解除了,主机本机却一直按删掉的规则限速。
// 这里起真实的数据面,经线路实测下载速度:规则生效时慢,删掉之后立刻恢复。
func TestDeleteActiveRuleLiftsLocalLimit(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		run.Stop()
		_ = database.Close(run.DB())
	})
	db := run.DB()
	db.Create(&model.Setting{Key: "allowPrivate", Value: "true"}) // 下载服务在本机回环上
	port := freeTCPPort(t)
	line := model.Line{Name: "s5", Protocol: "socks", Port: port, Enabled: true}
	db.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: []byte(`{"socks":{"password":"alice-pw"}}`)}
	db.Create(&alice)
	db.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	if err := run.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64<<10)
		for {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer src.Close()
	dst := src.Listener.Addr().(*net.TCPAddr)
	// 经 socks 线路下载 1.5 秒,返回收到的字节数
	measure := func() int64 {
		t.Helper()
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte{5, 1, 2})
		b := make([]byte, 10)
		if _, err := io.ReadFull(c, b[:2]); err != nil || b[1] != 2 {
			t.Fatalf("socks 握手失败: %v %v", err, b[:2])
		}
		u, p := "alice", "alice-pw"
		c.Write(append(append([]byte{1, byte(len(u))}, u...), append([]byte{byte(len(p))}, p...)...))
		if _, err := io.ReadFull(c, b[:2]); err != nil || b[1] != 0 {
			t.Fatalf("socks 认证失败: %v %v", err, b[:2])
		}
		c.Write([]byte{5, 1, 0, 1, 127, 0, 0, 1, byte(dst.Port >> 8), byte(dst.Port)})
		if _, err := io.ReadFull(c, b[:10]); err != nil || b[1] != 0 {
			t.Fatalf("socks CONNECT 失败: %v %v", err, b[:2])
		}
		fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", dst)
		_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		n, _ := io.Copy(io.Discard, c)
		return n
	}
	s := NewServer(run)
	if free := measure(); free < 8<<20 {
		t.Fatalf("前提:不限速时 1.5 秒应远超 8MB,实际 %d 字节", free)
	}

	body := fmt.Sprintf(`{"name":"全天 1M","enabled":true,"kind":"schedule","userIds":[%d],"start":"00:00","end":"00:00","upMbps":1,"downMbps":1,"tightenOnly":false}`, alice.Id)
	w := httptest.NewRecorder()
	s.handleRules(w, httptest.NewRequest("POST", "http://x/app/api/rules", strings.NewReader(body)))
	var rule struct {
		Id uint `json:"id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rule) != nil || rule.Id == 0 {
		t.Fatalf("建规则失败: %d %s", w.Code, w.Body.String())
	}
	if limited := measure(); limited > 1<<20 {
		t.Fatalf("前提:1 Mbps 的规则生效后 1.5 秒应远低于 1MB,实际 %d 字节", limited)
	}

	w = httptest.NewRecorder()
	s.handleRuleItem(w, httptest.NewRequest("DELETE", fmt.Sprintf("http://x/app/api/rules/%d", rule.Id), nil))
	if w.Code != 200 {
		t.Fatalf("删规则失败: %d %s", w.Code, w.Body.String())
	}
	if after := measure(); after < 8<<20 {
		t.Fatalf("删除生效中的规则后本机应立刻不再限速,1.5 秒只收到 %d 字节", after)
	}
}

//go:build !race

package runner

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// testSocks 最小的 SOCKS5 服务:能转发,记着转发了多少次,能关。
type testSocks struct {
	ln    net.Listener
	count atomic.Int64
}

func startTestSocks(t *testing.T) *testSocks {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSocks{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *testSocks) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *testSocks) serve(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		io.ReadFull(c, buf[:4])
		host = net.IP(buf[:4]).String()
	case 3:
		io.ReadFull(c, buf[:1])
		n := int(buf[0])
		io.ReadFull(c, buf[:n])
		host = string(buf[:n])
	default:
		return
	}
	io.ReadFull(c, buf[:2])
	port := binary.BigEndian.Uint16(buf[:2])
	up, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	s.count.Add(1)
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, c)
	io.Copy(c, up)
}

// 上游组真跑起来:线路走组;换成员不重启数据面且组用上新成员;成员挂了组自动换到另一条;组改成员也生效。
func TestGroupOnRealCore(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer web.Close()
	s1, s2, s3 := startTestSocks(t), startTestSocks(t), startTestSocks(t)

	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	r.db.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	socks := func(port int) []byte {
		b, _ := json.Marshal(map[string]interface{}{"server": "127.0.0.1", "server_port": port})
		return b
	}
	a := model.Upstream{Name: "a", Type: "socks", Options: socks(s1.port())}
	b := model.Upstream{Name: "b", Type: "socks", Options: socks(1)} // 一开始不通,组只能用 a
	r.db.Create(&a)
	r.db.Create(&b)
	groupOpts := func(members ...string) []byte {
		o, _ := json.Marshal(map[string]interface{}{"outbounds": members, "url": web.URL, "interval": "1s", "tolerance": 50, "interrupt_exist_connections": true})
		return o
	}
	g := model.Upstream{Name: "g", Type: "urltest", Options: groupOpts("a", "b")}
	r.db.Create(&g)
	port := freePort(t)
	r.db.Create(&model.Line{Name: "via-group", Protocol: "mixed", Port: port, UpstreamId: g.Id, Enabled: true, Options: []byte(`{}`)})
	alice := model.User{Name: "alice", Enabled: true, Credentials: []byte(`{"socks":{"password":"alice-pw"}}`)}
	r.db.Create(&alice)
	r.db.Create(&model.UserLine{UserId: alice.Id, LineId: 1})
	if err := r.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer r.Stop()
	box0 := r.core.GetInstance()

	proxy, _ := url.Parse(fmt.Sprintf("http://alice:alice-pw@127.0.0.1:%d", port))
	request := func() error {
		c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}}
		resp, err := c.Get(web.URL)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			return fmt.Errorf("状态 %d", resp.StatusCode)
		}
		return nil
	}
	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("等不到: %s", what)
	}

	until("组选中 a,请求能通", func() bool { return core.OutboundNow("g") == "a" && request() == nil })

	// 上游组的通断检查:有一条通就算通,写明现在走哪条
	if ok, _, method, _ := r.CheckUpstream(g); !ok || method != "via:a" {
		t.Fatalf("组的检查: ok=%v method=%q", ok, method)
	}

	// 改成员:数据面不重启,组用上新的 a(组要是还拿着旧实例,流量还会去 s1)
	r.db.Model(&model.Upstream{}).Where("id = ?", a.Id).Update("options", socks(s2.port()))
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() != box0 {
		t.Fatal("改成员不该重启数据面")
	}
	before1, before2 := s1.count.Load(), s2.count.Load()
	until("改过之后流量走新的 a", func() bool { return request() == nil && s2.count.Load() > before2 })
	if n := s1.count.Load(); n != before1 {
		t.Fatalf("流量还在走旧的 a: %d → %d", before1, n)
	}

	// b 修好,a 挂掉:组自动换到 b,线路不断
	r.db.Model(&model.Upstream{}).Where("id = ?", b.Id).Update("options", socks(s3.port()))
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	s2.ln.Close()
	// 组只在有流量时才定时测:热更新后首测可能赶在 a 挂之前、又选中了 a,这时不发请求它永远不会再测
	until("a 挂了组换到 b", func() bool { _ = request(); return core.OutboundNow("g") == "b" && request() == nil })
	if r.core.GetInstance() != box0 {
		t.Fatal("换成员不该重启数据面")
	}

	// 全都挂了:组的检查报故障
	s3.ln.Close()
	until("全挂时组报故障", func() bool {
		ok, _, _, _ := r.CheckUpstream(g)
		return !ok
	})

	// 组改成员:加一条新的上游进组,数据面不重启,流量走新成员
	s4 := startTestSocks(t)
	c := model.Upstream{Name: "c", Type: "socks", Options: socks(s4.port())}
	r.db.Create(&c)
	r.db.Model(&model.Upstream{}).Where("id = ?", g.Id).Update("options", groupOpts("b", "c"))
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() != box0 {
		t.Fatal("组改成员不该重启数据面")
	}
	if _, ok := r.applied["a"]; ok {
		t.Fatal("不在组里、也没线路用的 a 要从数据面撤掉")
	}
	until("组用上新成员", func() bool { return request() == nil && s4.count.Load() > 0 })
}

package runner

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
)

// 启动报错里认出「哪个入站的端口被占了」:tag 里有空格、方括号、中转记号都要取对;别的错误不认。
func TestBusyInboundOf(t *testing.T) {
	for _, c := range []struct{ msg, tag, net string }{
		{"start inbound/direct[US中转 ⇄13024]: listen tcp 0.0.0.0:13024: bind: address already in use", "US中转 ⇄13024", "tcp"},
		{"start inbound/hysteria2[香港[专线] 1]: listen udp [::]:443: bind: address already in use", "香港[专线] 1", "udp"},
		{"start service: start inbound/mixed[in2]: listen tcp 127.0.0.1:1080: bind: address already in use", "in2", "tcp"},
		{"start inbound/vless[a]: listen tcp 192.0.2.1:443: bind: cannot assign requested address", "", ""},
		{"start inbound/trojan[b]: open /etc/m-ui/cert/x.crt: no such file or directory", "", ""},
	} {
		tag, network := busyInboundOf(errors.New(c.msg))
		if tag != c.tag || network != c.net {
			t.Fatalf("%q:应是 (%q, %q),实际 (%q, %q)", c.msg, c.tag, c.net, tag, network)
		}
	}
	if tag, _ := busyInboundOf(nil); tag != "" {
		t.Fatal("没有错误不该认出入站")
	}
}

// 摘掉一个入站:别的入站、路由原样;没有这个 tag 返回 nil。
func TestWithoutInbound(t *testing.T) {
	raw := []byte(`{"inbounds":[{"tag":"a","listen_port":1},{"tag":"b ⇄2","listen_port":2},{"tag":"c","listen_port":3}],"route":{"rules":[{"inbound":["b ⇄2"],"outbound":"direct"}]}}`)
	out, def := withoutInbound(raw, "b ⇄2")
	if out == nil || !strings.Contains(string(def), `"listen_port":2`) {
		t.Fatalf("应摘掉 b: %s / %s", out, def)
	}
	if strings.Contains(string(out), `"listen_port":2`) || !strings.Contains(string(out), `"tag":"a"`) || !strings.Contains(string(out), `"tag":"c"`) {
		t.Fatalf("别的入站要留着: %s", out)
	}
	if !strings.Contains(string(out), `"inbound":["b ⇄2"]`) {
		t.Fatalf("路由规则不动(入站加回来时照旧生效): %s", out)
	}
	if o, d := withoutInbound(raw, "nope"); o != nil || d != nil {
		t.Fatal("没有这个入站应返回 nil")
	}
}

// 只多了入站、别的没变才算「可以热加」;连路由也变了(新建线路)不算。
func TestAddedInbounds(t *testing.T) {
	prev := []byte(`{"inbounds":[{"tag":"a","listen_port":1,"users":[{"name":"x"}]}],"route":{"rules":[{"inbound":["f ⇄9"],"outbound":"direct"}]}}`)
	next := []byte(`{"inbounds":[{"tag":"a","listen_port":1,"users":[{"name":"y"}]},{"tag":"f ⇄9","listen_port":9}],"route":{"rules":[{"inbound":["f ⇄9"],"outbound":"direct"}]}}`)
	if got := addedInbounds(prev, next); len(got) != 1 || !strings.Contains(string(got[0]), `"listen_port":9`) {
		t.Fatalf("应认出多出来的那个入站(用户表不同不影响): %v", got)
	}
	newLine := []byte(`{"inbounds":[{"tag":"a","listen_port":1},{"tag":"n","listen_port":8}],"route":{"rules":[{"inbound":["f ⇄9"],"outbound":"direct"},{"inbound":["n"],"outbound":"up"}]}}`)
	if got := addedInbounds(prev, newLine); got != nil {
		t.Fatalf("路由也变了(新建线路)要走整机重载: %v", got)
	}
	if got := addedInbounds(next, prev); got != nil {
		t.Fatal("少了入站不是「只多了入站」")
	}
	changed := []byte(`{"inbounds":[{"tag":"a","listen_port":7},{"tag":"f ⇄9","listen_port":9}],"route":{"rules":[{"inbound":["f ⇄9"],"outbound":"direct"}]}}`)
	if got := addedInbounds(prev, changed); got != nil {
		t.Fatal("原有入站改了端口不能热加了事")
	}
}

// 端口被本机别的程序占着的入站:只摘掉它,别的照常起来;同一份配置再来不重启数据面;
// 端口空出来后把它热加回去,数据面还是不重启;管理员改了端口,名单里那条自己消失。
func TestBusyInboundSkippedAndRestored(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: filepath.Join(dir, "x.db")}
	defer r.core.Stop()

	goodPort := freePort(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := ln.Addr().(*net.TCPAddr).Port
	cfg := func(fwPort int) []byte {
		return []byte(fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"mixed","tag":"in","listen":"127.0.0.1","listen_port":%d,"users":[{"username":"a","password":"1"}]},`+
			`{"type":"direct","tag":"线路 ⇄%d","listen":"127.0.0.1","listen_port":%d,"network":"tcp","override_address":"127.0.0.1","override_port":9}],`+
			`"outbounds":[{"type":"direct","tag":"direct"}],"route":{"rules":[{"inbound":["线路 ⇄%d"],"action":"route","outbound":"direct"}]}}`, goodPort, fwPort, fwPort, fwPort))
	}
	if err := r.reloadAllLocked(cfg(busy)); err != nil {
		t.Fatalf("端口被占的入站应被摘掉、其余照常启动: %v", err)
	}
	if !r.core.IsRunning() {
		t.Fatal("数据面应在运行")
	}
	bl := r.BlockedInbounds()
	if len(bl) != 1 || bl[0].Port != busy || bl[0].Net != "tcp" || bl[0].Tag != fmt.Sprintf("线路 ⇄%d", busy) {
		t.Fatalf("应记下被占的那个入站: %+v", bl)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", goodPort)); err != nil {
		t.Fatalf("别的入站应在服务: %v", err)
	} else {
		c.Close()
	}
	if r.lastFailedRaw != nil {
		t.Fatal("这不算一次失败的配置(不进冷却)")
	}

	// 同一份再推:不重启
	box := r.core.GetInstance()
	if err := r.reloadAllLocked(cfg(busy)); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() != box {
		t.Fatal("同一份配置再来不该重启数据面")
	}

	// 端口空出来:名单里拿掉,重载时热加,数据面不重启
	ln.Close()
	if !r.releaseFreedPorts() {
		t.Fatal("端口空了应从名单里拿掉")
	}
	if err := r.reloadAllLocked(cfg(busy)); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() != box {
		t.Fatal("补开入站不该重启数据面")
	}
	if len(r.BlockedInbounds()) != 0 {
		t.Fatalf("名单应空了: %+v", r.BlockedInbounds())
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", busy)); err != nil {
		t.Fatalf("补开的入站应在监听: %v", err)
	} else {
		c.Close()
	}

	// 已有的入站改端口改到被占的端口:不摘(摘了这条就停了),照旧整份回滚,原来的端口继续服务
	lnIn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	moved := []byte(strings.Replace(string(cfg(busy)), fmt.Sprintf(`"listen_port":%d,"users"`, goodPort), fmt.Sprintf(`"listen_port":%d,"users"`, lnIn.Addr().(*net.TCPAddr).Port), 1))
	if err := r.reloadAllLocked(moved); err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("已有入站改到被占的端口应回滚: %v", err)
	}
	lnIn.Close()
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", goodPort)); err != nil {
		t.Fatalf("回滚后原来的端口应继续服务: %v", err)
	} else {
		c.Close()
	}
	if len(r.BlockedInbounds()) != 0 {
		t.Fatalf("回滚的不进名单: %+v", r.BlockedInbounds())
	}
	r.clearFailedConfig()
	box = r.core.GetInstance()

	// 再占住另一个端口、配置改成用它:摘掉;管理员又改回空闲端口:名单清空
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	busy2 := ln2.Addr().(*net.TCPAddr).Port
	if err := r.reloadAllLocked(cfg(busy2)); err != nil {
		t.Fatal(err)
	}
	if bl := r.BlockedInbounds(); len(bl) != 1 || bl[0].Port != busy2 {
		t.Fatalf("换到另一个被占的端口应重新记: %+v", bl)
	}
	free := freePort(t)
	if err := r.reloadAllLocked(cfg(free)); err != nil {
		t.Fatal(err)
	}
	if len(r.BlockedInbounds()) != 0 {
		t.Fatalf("改了端口,名单里那条该自己消失: %+v", r.BlockedInbounds())
	}
}

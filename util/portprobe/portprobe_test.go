package portprobe

import (
	"net"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 端口空着算空闲;TCP、UDP 各自被占时,只探那一种的结论各不相干,两种都要的只要有一种被占就不算空闲。
func TestProbe(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if r := Probe(model.PortProbe{Port: port, Net: "tcp"}); r.Free || r.Net != "tcp" {
		t.Fatalf("TCP 被占着应探出来: %+v", r)
	}
	if r := Probe(model.PortProbe{Port: port}); r.Free {
		t.Fatalf("两种都要:TCP 被占就不算空闲: %+v", r)
	}
	ln.Close()
	if !Free(port, "tcp") {
		t.Fatal("放开后应空闲")
	}

	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	uport := pc.LocalAddr().(*net.UDPAddr).Port
	if r := Probe(model.PortProbe{Port: uport, Net: "udp"}); r.Free || r.Net != "udp" {
		t.Fatalf("UDP 被占着应探出来: %+v", r)
	}
	if r := Probe(model.PortProbe{Port: 0}); r.Free {
		t.Fatal("端口号不对不算空闲")
	}
}

// ss 的输出里取占用端口的进程名。
func TestParseHolder(t *testing.T) {
	out := `LISTEN 0      1024         0.0.0.0:13024      0.0.0.0:*    users:(("realm",pid=812,fd=9))`
	if got := parseHolder(out); got != "realm" {
		t.Fatalf("应取出 realm,实际 %q", got)
	}
	if got := parseHolder("LISTEN 0 128 0.0.0.0:22 0.0.0.0:*"); got != "" {
		t.Fatalf("没有进程信息(不是 root)应为空,实际 %q", got)
	}
	if got := parseHolder(`UNCONN 0 0 *:443 *:* users:(("m-ui",pid=5,fd=20),("m-ui",pid=5,fd=21))`); got != SelfName {
		t.Fatalf("应取出 m-ui,实际 %q", got)
	}
}

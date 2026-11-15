package web

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// 面板跟着主机走以后(见 ha/panelid.go),本机原来的面板地址上继续接 agent 请求:原主机回来时、别的服务器库里记的
// 还是这个地址(真机演练:接不到时原主机核实不了新主机还活着,一直当着主机)。只接 agent 这一类,别的路径一律 404。
func TestLegacyAgentListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	old := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	became := `{"how":"takeover","epoch":2,"identity":{"subUrlKept":true,"subHost":"","panelKept":true,"prevPanel":{"Port":` + strconv.Itoa(old) + `,"Path":"/old/"}}}`
	r := newLDRunner(t, "m.db", map[string]string{"haEpoch": "2", "haBecame": became, "webPort": "2053", "webPath": "/app/", "webListen": "127.0.0.1"})
	s := NewServer(r)
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", s.handleAgent)
	s.startLegacyAgent(mux)
	if got := int(s.legacyAgentPort.Load()); got != old {
		t.Fatalf("原来的面板端口上起了接 agent 的监听: %d", got)
	}
	get := func(path string) int {
		var code int
		for i := 0; i < 20; i++ {
			resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(old) + path)
			if err == nil {
				code = resp.StatusCode
				resp.Body.Close()
				return code
			}
			time.Sleep(50 * time.Millisecond)
		}
		return code
	}
	if c := get("/old/api/agent/ping"); c == http.StatusNotFound || c == 0 {
		t.Fatalf("原来路径下的 agent 请求要转到 agent 处理(没带令牌被拒,但不是 404): %d", c)
	}
	if c := get("/old/api/status"); c != http.StatusNotFound {
		t.Fatalf("只接 agent 这一类: %d", c)
	}
	for _, p := range s.localServicePorts() {
		if p.port == old {
			return
		}
	}
	t.Fatal("原来的面板端口还占着:线路不能用它")
}

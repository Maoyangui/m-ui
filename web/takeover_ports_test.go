package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 开着主机容灾、全量接管:订阅端口随主机身份迁到接任的那台,所以副机上的线路不能用它(保存线路时拦),
// 本机的订阅端口也不能改成副机上线路正用着的(设置页拦);只走 UDP 的线路不冲突;没开容灾时照旧。
func TestTakeoverPortsBlockLinesAndSettings(t *testing.T) {
	r := newLDRunner(t, "master.db", map[string]string{"haEpoch": "1", "haMode": "confirm", "haTakeSub": "true", "haDnsToken": "tok",
		"subDomain": "sub.example.com", "subPort": "2096"})
	s := NewServer(r)
	node := model.Node{Name: "TW", ApiUrl: "https://tw.example.com:2053/app/", Token: "tok", Enabled: true, Sort: 2}
	r.DB().Create(&node)
	on, _ := json.Marshal([]uint{node.Id})
	if err := s.checkTakeoverPorts(model.Line{Name: "a", Protocol: "vless", Port: 2096, NodeIds: on}); err == nil || !strings.Contains(err.Error(), "订阅端口") || !strings.Contains(err.Error(), "TW") {
		t.Fatalf("副机上的 TCP 线路不能用迁来的订阅端口: %v", err)
	}
	if err := s.checkTakeoverPorts(model.Line{Name: "b", Protocol: "hysteria2", Port: 2096, NodeIds: on}); err != nil {
		t.Fatalf("只走 UDP 的不冲突: %v", err)
	}
	if err := s.checkTakeoverPorts(model.Line{Name: "c", Protocol: "vless", Port: 2097, NodeIds: on}); err != nil {
		t.Fatalf("别的端口照常: %v", err)
	}
	r.DB().Create(&model.Line{Name: "VM", Protocol: "vmess", Port: 8443, Enabled: true, NodeIds: on})
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/app/api/settings", strings.NewReader(body)))
		return rec
	}
	if rec := post(`{"subPort":"8443"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VM@TW:8443") {
		t.Fatalf("订阅端口不能改成副机上线路正用着的: %d %s", rec.Code, rec.Body.String())
	}
	// 没开容灾:照旧
	r.DB().Model(&model.Setting{}).Where("key = ?", "haMode").Update("value", "off")
	if err := s.checkTakeoverPorts(model.Line{Name: "d", Protocol: "vless", Port: 2096, NodeIds: on}); err != nil {
		t.Fatalf("没开容灾不留端口: %v", err)
	}
}

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 服务器页「清理磁盘」:主机经 agent/disk-clean 问副机的进度(带副机的磁盘占用)、让它开始清理;
// 副机报告里带着磁盘占用;老版本副机(没有这个接口)说清要先升级;停用的服务器不派。
// 测试机上不是 root,真开始清理会被运维任务拒绝(这里看的是请求转到了副机、错误原样带回来)。
func TestNodeDiskCleanRelay(t *testing.T) {
	master := newLDRunner(t, "master.db", nil)
	ms := NewServer(master)
	mdb := master.DB()
	const token = "disk-agent-token-0123456789"
	node := newLDRunner(t, "node.db", map[string]string{"nodeMode": "true", "nodeToken": token})
	ns := NewServer(node)
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", ns.handleAgent)
	agentSrv := httptest.NewServer(mux)
	defer agentSrv.Close()
	oldSrv := httptest.NewServer(http.NotFoundHandler())
	defer oldSrv.Close()
	a := model.Node{Name: "副机A", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	o := model.Node{Name: "旧版的", ApiUrl: oldSrv.URL + "/app/", Token: "x", Enabled: true, Sort: 3}
	off := model.Node{Name: "停用的", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 4}
	for _, n := range []*model.Node{&a, &o, &off} {
		if err := mdb.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	mdb.Model(&model.Node{}).Where("id = ?", off.Id).Update("enabled", false)
	call := func(method string, id uint, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, innerBase+"api/nodes/"+strconv.Itoa(int(id))+"/disk-clean", strings.NewReader(body))
		ms.handleNodeItem(rec, req)
		return rec
	}
	rec := call(http.MethodGet, a.Id, "")
	var st diskCleanState
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.Running {
		t.Fatalf("问副机的进度: %d %s", rec.Code, rec.Body.String())
	}
	if d := node.DiskStat(); d != nil && (st.Disk == nil || st.Disk.Total != d.Total) {
		t.Fatalf("进度里带着副机的磁盘占用: %+v", st.Disk)
	}
	rec = call(http.MethodPost, a.Id, `{"opsAgent":true}`)
	if rec.Code != http.StatusBadRequest || !(strings.Contains(rec.Body.String(), "root") || strings.Contains(rec.Body.String(), "Linux")) {
		t.Fatalf("测试机不是 root:副机的运维任务拒绝,原因带回来: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, o.Id, ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "版本太旧") {
		t.Fatalf("老副机:说清要先升级: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, off.Id, ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "停用") {
		t.Fatalf("停用的不派: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPut, a.Id, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("只认 GET / POST: %d", rec.Code)
	}
	// 本机那一行:就地看
	var local model.Node
	mdb.Where("is_local = ?", true).First(&local)
	if rec := call(http.MethodGet, local.Id, ""); rec.Code != http.StatusOK {
		t.Fatalf("本机就地看进度: %d %s", rec.Code, rec.Body.String())
	}
	// 副机报告带磁盘占用(读得到时)
	rrec := httptest.NewRecorder()
	rreq := httptest.NewRequest(http.MethodGet, innerBase+"api/agent/report", nil)
	rreq.Header.Set("X-Agent-Token", token)
	ns.handleAgent(rrec, rreq)
	var rep struct {
		Disk *model.DiskStat `json:"disk"`
	}
	if rrec.Code != http.StatusOK || json.Unmarshal(rrec.Body.Bytes(), &rep) != nil {
		t.Fatalf("副机报告: %d %s", rrec.Code, rrec.Body.String())
	}
	if node.DiskStat() != nil && (rep.Disk == nil || rep.Disk.Total <= 0) {
		t.Fatalf("报告里带磁盘占用: %+v", rep.Disk)
	}
}

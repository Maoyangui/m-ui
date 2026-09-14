package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

const extNodesYAML = `proxies:
  - {name: a, type: hysteria2, server: 1.2.3.4, port: 443, password: p, sni: x.example.com, skip-cert-verify: true}
  - {name: b, type: tuic, server: 1.2.3.4, port: 444, uuid: 00000000-0000-0000-0000-000000000000, password: p, sni: x.example.com}
`

func extNodesServer(t *testing.T) *Server {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.ExtNode{Name: "机场A", Type: "sub", Value: "https://example.com/sub", Enabled: true, Cache: extNodesYAML, NodeCount: 2, LastFetch: 1000})
	return &Server{db: db}
}

func extCall(t *testing.T, s *Server, method, path string, body string) (int, map[string]interface{}) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, innerBase+"api/exts/"+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.handleExtItem(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// 展开:节点明细带全部字段与分享链接,服务器列是所有启用的服务器。
func TestExtNodesList(t *testing.T) {
	s := extNodesServer(t)
	code, out := extCall(t, s, http.MethodGet, "1/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("应 200,得到 %d %v", code, out)
	}
	nodes, _ := out["nodes"].([]interface{})
	servers, _ := out["servers"].([]interface{})
	if len(nodes) != 2 || len(servers) != 1 {
		t.Fatalf("应 2 个节点 1 台服务器: %v", out)
	}
	n0 := nodes[0].(map[string]interface{})
	if n0["name"] != "a" || n0["type"] != "hysteria2" || n0["upstream"] != true || n0["link"] == "" {
		t.Fatalf("节点明细不对: %v", n0)
	}
	if fields, _ := n0["fields"].([]interface{}); len(fields) < 6 {
		t.Fatalf("字段太少: %v", fields)
	}
	if out["fetchedAt"] != float64(1000) {
		t.Fatalf("应带抓取时间: %v", out["fetchedAt"])
	}
}

// 测速:起任务后每格先是 pending;没有数据面时都以"数据面未就绪"结束,任务能查到、会 done。
func TestExtNodesTestJob(t *testing.T) {
	s := extNodesServer(t)
	code, out := extCall(t, s, http.MethodPost, "1/nodes/test", `{"indexes":[1]}`)
	if code != http.StatusOK || out["job"] == "" {
		t.Fatalf("起任务失败: %d %v", code, out)
	}
	job := out["job"].(string)
	var st map[string]interface{}
	for i := 0; i < 50; i++ {
		_, st = extCall(t, s, http.MethodGet, "1/nodes/test/"+job, "")
		if st["done"] == true {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st["done"] != true || st["total"] != float64(1) {
		t.Fatalf("任务应结束且只测 1 个节点 × 1 台: %v", st)
	}
	res := st["results"].(map[string]interface{})
	row, ok := res["1"].(map[string]interface{})
	if !ok || len(row) != 1 {
		t.Fatalf("应只有下标 1 的结果: %v", res)
	}
	for _, c := range row {
		cell := c.(map[string]interface{})
		if cell["state"] != "fail" || !strings.Contains(cell["error"].(string), "数据面") {
			t.Fatalf("没有数据面时应报未就绪: %v", cell)
		}
	}
	if code, _ := extCall(t, s, http.MethodGet, "1/nodes/test/nope", ""); code != http.StatusNotFound {
		t.Fatalf("不存在的任务应 404,得到 %d", code)
	}
	// 下标越界的节点不算数
	if code, out := extCall(t, s, http.MethodPost, "1/nodes/test", `{"indexes":[9]}`); code != http.StatusBadRequest {
		t.Fatalf("全是无效下标应 400: %d %v", code, out)
	}
}

// 添加为上游:走上游校验,成功入库;重名报错不改名;不支持的协议报错。
func TestExtNodesAddUpstream(t *testing.T) {
	s := extNodesServer(t)
	code, out := extCall(t, s, http.MethodPost, "1/nodes/add-upstream", `[{"index":0,"name":"机场A-a"},{"index":1,"name":"机场A-b"}]`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, out)
	}
	results := out["results"].([]interface{})
	if len(results) != 2 {
		t.Fatalf("应有两条结果: %v", results)
	}
	for _, r := range results {
		m := r.(map[string]interface{})
		if m["ok"] != true {
			t.Fatalf("应都成功: %v", m)
		}
	}
	var ups []model.Upstream
	s.db.Order("id asc").Find(&ups)
	if len(ups) != 2 || ups[0].Name != "机场A-a" || ups[0].Type != "hysteria2" || ups[1].Type != "tuic" {
		t.Fatalf("上游没建对: %+v", ups)
	}
	var opts map[string]interface{}
	_ = json.Unmarshal(ups[0].Options, &opts)
	if opts["server"] != "1.2.3.4" || opts["password"] != "p" {
		t.Fatalf("上游参数不对: %v", opts)
	}
	// 重名
	code, out = extCall(t, s, http.MethodPost, "1/nodes/add-upstream", `[{"index":0,"name":"机场A-a"}]`)
	m := out["results"].([]interface{})[0].(map[string]interface{})
	if code != http.StatusOK || m["ok"] == true || !strings.Contains(m["error"].(string), "已存在") {
		t.Fatalf("重名应逐条报错: %d %v", code, m)
	}
	// 空名 / 越界
	code, out = extCall(t, s, http.MethodPost, "1/nodes/add-upstream", `[{"index":0,"name":" "},{"index":7,"name":"x"}]`)
	rs := out["results"].([]interface{})
	if code != http.StatusOK || rs[0].(map[string]interface{})["ok"] == true || rs[1].(map[string]interface{})["ok"] == true {
		t.Fatalf("空名与越界都应失败: %v", rs)
	}
	if code, _ := extCall(t, s, http.MethodPost, "1/nodes/add-upstream", `[]`); code != http.StatusBadRequest {
		t.Fatalf("空列表应 400,得到 %d", code)
	}
}

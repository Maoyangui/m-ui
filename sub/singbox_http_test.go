package sub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestSingBoxSubHttpLineAndEmpty(t *testing.T) {
	u := model.User{Name: "h", Credentials: []byte(`{"http":{"username":"h","password":"pw"}}`)}
	line := model.Line{Id: 1, Name: "代理口", Protocol: "http", Port: 8080, Tls: []byte(`{"mode":"none"}`)}
	res, err := BuildSingBoxSub(u, []model.Line{line}, Options{Entries: []Entry{{Host: "1.2.3.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Body, `"type": "http"`) || !strings.Contains(res.Body, `"username": "h"`) {
		t.Fatalf("http 线路应出 http 出站: %s", res.Body)
	}
	// 没有任何线路:给一份只有直连的合法配置,而不是 500
	res, err = BuildSingBoxSub(u, nil, Options{})
	if err != nil {
		t.Fatalf("没有节点时不该报错: %v", err)
	}
	var cfg map[string]interface{}
	if json.Unmarshal([]byte(res.Body), &cfg) != nil || cfg["outbounds"] == nil {
		t.Fatalf("应是合法的 sing-box 配置: %s", res.Body)
	}
}

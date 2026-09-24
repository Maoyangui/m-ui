package web

import (
	"encoding/json"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 面板接口收到 "options": null 时,以前校验会放行(把 null 解码进 map 不报错),存进库的就是 null,
// 渲染时一写就崩。现在和空参数一样归一成 {}。
func TestValidateNormalizesNullOptions(t *testing.T) {
	s := &Server{db: openAgentTestDB(t, "null.db")}
	up := model.Upstream{Name: "up", Type: "socks", Options: json.RawMessage("null")}
	if err := s.validateUpstream(&up); err != nil {
		t.Fatalf("上游校验: %v", err)
	}
	if string(up.Options) != "{}" {
		t.Fatalf("上游参数 null 应归一成 {},实际 %s", up.Options)
	}
	up2 := model.Upstream{Name: "up2", Type: "socks", Options: json.RawMessage(" null ")}
	if err := s.validateUpstream(&up2); err != nil || string(up2.Options) != "{}" {
		t.Fatalf("带空白的 null 也要归一: %v %s", err, up2.Options)
	}
}

package render

import (
	"encoding/json"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 线路参数是 JSON 字面量 null(老库、手工改库、主机把 NULL 列推过来)时,以前 inboundBase 会在
// "assignment to entry in nil map" 上 panic:副机处理推送的协程崩掉,主机只看到 EOF、每 5 秒重试永远不成功。
// 这里钉住:当空参数处理,渲染出来的入站照常带 type / tag / listen_port。
func TestInboundBaseToleratesNullOptions(t *testing.T) {
	for _, opts := range []json.RawMessage{json.RawMessage("null"), nil, json.RawMessage("{}")} {
		line := model.Line{Name: "ss", Protocol: "shadowsocks", Port: 8388, Enabled: true, Options: opts}
		in, err := inboundBase(line, NodeCert{})
		if err != nil {
			t.Fatalf("Options=%s: %v", string(opts), err)
		}
		if in["type"] != "shadowsocks" || in["tag"] != "ss" || in["listen_port"] != 8388 {
			t.Fatalf("Options=%s: 渲染结果不对: %v", string(opts), in)
		}
	}
}

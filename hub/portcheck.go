package hub

import (
	"net/http"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// PortCheck 问副机 n:这些端口现在能不能监听(保存线路 / 中转入口前用)。老版本副机没有这个接口,返回的错误 IsNotFound 为真。
func (h *Hub) PortCheck(n model.Node, ports []model.PortProbe, timeout time.Duration) ([]model.PortProbeResult, error) {
	var out struct {
		Results []model.PortProbeResult `json:"results"`
	}
	if err := h.requestTimeout(n, http.MethodPost, "port-check", map[string]interface{}{"ports": ports}, &out, timeout); err != nil {
		return nil, err
	}
	return out.Results, nil
}

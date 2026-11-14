package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// status.v6Bad(概况"需要处理"的 IPv6 入站一行):主机上这个键始终在、没有时是空数组;副机不给。
func TestStatusV6BadKey(t *testing.T) {
	s, run := certServer(t)
	status := func() map[string]json.RawMessage {
		w := httptest.NewRecorder()
		s.handleStatus(w, httptest.NewRequest(http.MethodGet, "http://x/app/api/status", nil))
		var m map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("status: %v %s", err, w.Body.String())
		}
		return m
	}
	if v, ok := status()["v6Bad"]; !ok || string(v) != "[]" {
		t.Fatalf("主机上要始终有 v6Bad(空数组): %s %v", v, ok)
	}
	if err := run.SetSetting("nodeMode", "true"); err != nil {
		t.Fatal(err)
	}
	if _, ok := status()["v6Bad"]; ok {
		t.Fatal("副机不给 v6Bad")
	}
}

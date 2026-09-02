package sub

import (
	"strings"
	"testing"

	"github.com/fangjunsheng555/m-ui/database/model"
)

func TestHysteria2PortHoppingInSubscriptions(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	line := model.Line{Name: "hy2", Protocol: "hysteria2", Port: 30443, Enabled: true, Options: []byte(`{"port_hopping":"20000-30000"}`)}
	entries := []Entry{{Host: "1.2.3.4", SNI: "h.example.com"}}
	links := GenerateLinks(user, []model.Line{line}, entries)
	if len(links) != 1 || !strings.Contains(links[0], "&mport=20000-30000") {
		t.Fatalf("hy2 链接应带 mport: %v", links)
	}
	out, _ := BuildClash(user, []model.Line{line}, entries, "", "")
	if !strings.Contains(out, "ports: 20000-30000") {
		t.Fatalf("clash 应带 ports:\n%s", out)
	}
	plain := model.Line{Name: "hy2b", Protocol: "hysteria2", Port: 30444, Enabled: true}
	links = GenerateLinks(user, []model.Line{plain}, entries)
	if strings.Contains(links[0], "mport") {
		t.Fatal("未开启时不应带 mport")
	}
}

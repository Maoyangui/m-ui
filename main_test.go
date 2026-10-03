package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 健康检查地址按库里的监听地址拼(面板只监听某个地址时回环连不上,升级会被误判回滚,审计 MB06);
// 服务开着库也能只读取到;库不存在时什么也不给,也不能顺手建出一个空库。
func TestLocalPanelURLFollowsListen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.db")
	db, err := database.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Setting{Key: "webListen", Value: "10.8.0.1"})
	db.Create(&model.Setting{Key: "webPort", Value: "3053"})
	db.Create(&model.Setting{Key: "webCertFile", Value: "/c.pem"})
	if u := localPanelURL(p); u != "https://10.8.0.1:3053/app/" {
		t.Fatalf("健康检查地址不对: %q", u)
	}
	missing := filepath.Join(t.TempDir(), "none.db")
	if u := localPanelURL(missing); u != "" {
		t.Fatalf("库不存在应为空: %q", u)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("不该建出空库")
	}
}

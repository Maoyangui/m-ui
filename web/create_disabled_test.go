package web

import (
	"fmt"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 建号时勾了"停用":gorm 的 default:true 会把 false 写成 true,以前建出来的是启用的用户。
func TestCreateUserDisabledStaysDisabled(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	req := httptest.NewRequest("POST", "http://x/app/api/users", strings.NewReader(`{"name":"bob","enabled":false}`))
	w := httptest.NewRecorder()
	s.handleUsers(w, req)
	if w.Code != 200 {
		t.Fatalf("建号失败: %d %s", w.Code, w.Body.String())
	}
	var u model.User
	if err := db.Where("name = ?", "bob").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	if u.Enabled || u.DisabledReason != model.DisabledManual {
		t.Fatalf("建号时停用的用户应保持停用并记为手动: enabled=%v reason=%q", u.Enabled, u.DisabledReason)
	}
	// 清用量不该让他复活(手动停用)
	if err := s.resetUsage(db, u.Id); err != nil {
		t.Fatal(err)
	}
	db.First(&u, u.Id)
	if u.Enabled {
		t.Fatal("手动停用的用户重置用量后不该自动启用")
	}
}

// 新建代理时去掉"启用":gorm 的 default:true 会把 false 写成 true,建出来的代理以前是启用的。
func TestCreateResellerDisabledStaysDisabled(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	r := httptest.NewRequest("POST", "http://x/app/api/resellers", strings.NewReader(`{"name":"dl","enabled":false}`))
	w := httptest.NewRecorder()
	s.handleResellers(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("建代理失败或回显不对: %d %s", w.Code, w.Body.String())
	}
	var got model.Reseller
	run.DB().Where("name = ?", "dl").First(&got)
	if got.Enabled {
		t.Fatal("建代理时停用,库里应保持停用")
	}
}

// 新建线路时取消「启用」:gorm 的 default:true 会把 false 写成 true,以前建出来的线路是启用的,端口照样开出来。
func TestCreateLineDisabledStaysDisabled(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	body := fmt.Sprintf(`{"name":"off","protocol":"socks","port":%d,"enabled":false}`, idleTCPPort(t))
	r := httptest.NewRequest("POST", "http://x/app/api/lines", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleLines(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("建线路失败或回显不对: %d %s", w.Code, w.Body.String())
	}
	var got model.Line
	run.DB().Where("name = ?", "off").First(&got)
	if got.Id == 0 || got.Enabled {
		t.Fatalf("建线路时停用,库里应保持停用: %+v", got)
	}
}

// idleTCPPort 本机一个空闲的 TCP 端口(这个文件在 -race 下也编译,不能用只在非 race 下才有的 freeTCPPort)。
func idleTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

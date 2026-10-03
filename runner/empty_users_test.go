package runner

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// socks5Greet 发 SOCKS5 问候,只提供 methods 里的认证方式,返回服务端选中的方式(0xFF = 一个都不接受)。
func socks5Greet(t *testing.T, port int, methods ...byte) (net.Conn, byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, 0xFF // 直接断开也算不接受
	}
	return c, resp[1]
}

// socks5Auth 用户名密码认证(RFC 1929),返回是否通过。
func socks5Auth(t *testing.T, port int, user, pass string) bool {
	t.Helper()
	c, m := socks5Greet(t, port, 0x02)
	if c == nil {
		return false
	}
	defer c.Close()
	if m != 0x02 {
		return false
	}
	req := append([]byte{0x01, byte(len(user))}, user...)
	req = append(append(req, byte(len(pass))), pass...)
	if _, err := c.Write(req); err != nil {
		return false
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		return false
	}
	return resp[1] == 0x00
}

// socks / http / mixed 线路一个用户都没有时,sing-box 把空用户表当成不鉴权 —— 公网开放代理。用户全停、代理到期、
// 停用 / 删除副机推的空用户表都会走到这里(审计 render.go:283)。真起数据面,用不带认证的 SOCKS5 问候验证。
func TestEmptySocksLineIsNotOpenProxy(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "m.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: dbPath}
	defer r.core.Stop()

	empty, mixed, used := freePort(t), freePort(t), freePort(t)
	db.Create(&model.Line{Name: "s5-empty", Protocol: "socks", Port: empty, Enabled: true})
	db.Create(&model.Line{Name: "mixed-empty", Protocol: "mixed", Port: mixed, Enabled: true})
	line := model.Line{Name: "s5", Protocol: "socks", Port: used, Enabled: true}
	db.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: []byte(`{"socks":{"password":"alice-pw"}}`)}
	db.Create(&alice)
	db.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	if err := r.ReloadAll(); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}

	for name, port := range map[string]int{"零用户的 socks": empty, "零用户的 mixed": mixed} {
		if c, m := socks5Greet(t, port, 0x00); m == 0x00 {
			c.Close()
			t.Fatalf("%s 线路接受了不带认证的连接:开放代理", name)
		} else if c != nil {
			c.Close()
		}
	}
	if !socks5Auth(t, used, "alice", "alice-pw") {
		t.Fatal("有用户的线路上 alice 应能认证")
	}

	// 停用 alice 后热更新:这条线路变成零用户,同样不能退化成不鉴权,alice 的旧凭据也不能再用
	db.Model(&model.User{}).Where("id = ?", alice.Id).Update("enabled", false)
	if err := r.ReloadUsers(); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if c, m := socks5Greet(t, used, 0x00); m == 0x00 {
		c.Close()
		t.Fatal("最后一个用户停用后,线路接受了不带认证的连接:开放代理")
	} else if c != nil {
		c.Close()
	}
	if socks5Auth(t, used, "alice", "alice-pw") {
		t.Fatal("停用的 alice 还能认证")
	}
}

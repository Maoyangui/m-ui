package web

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// saveEnv 一个带数据面对象(不启动)的面板,建好一条线路,返回改这条线路的请求函数。
func saveEnv(t *testing.T) (*Server, *runner.Runner, func(name string) *httptest.ResponseRecorder) {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	s := NewServer(run)
	// 停用的线路:保存照样整份干跑,但保存后的重载不会真把数据面起起来占端口
	body := fmt.Sprintf(`{"name":"香港1","protocol":"socks","port":%d,"enabled":false}`, idleTCPPort(t))
	w := httptest.NewRecorder()
	s.handleLines(w, httptest.NewRequest("POST", "http://x/app/api/lines", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("建线路失败: %d %s", w.Code, w.Body.String())
	}
	var line model.Line
	run.DB().Where("name = ?", "香港1").First(&line)
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleLineItem(w, httptest.NewRequest("PUT", fmt.Sprintf("http://x/app/api/lines/%d", line.Id), strings.NewReader(body)))
		return w
	}
	return s, run, func(name string) *httptest.ResponseRecorder { // 只改名字:端口不变,不做占用探测
		return put(fmt.Sprintf(`{"name":%q,"protocol":"socks","port":%d,"enabled":false}`, name, line.Port))
	}
}

// 保存前的整份干跑以前在写事务里做,干跑要排队(别的保存、数据面重载,0.7.16 里还有测速的临时实例都在用同一把干跑锁),
// 排队期间数据库写锁一直被这笔保存占着(测试集群上 0.7.16 测速时保存线路能卡出约 20 秒;生产上那次 20 秒是否同因未证实)。
// 现在干跑在事务外:干跑在排队时,别的写库(并账、统计、审计)照常进行;放开后保存立刻完成。
func TestLineSaveDryRunWaitsWithoutHoldingWriteLock(t *testing.T) {
	_, run, rename := saveEnv(t)
	release := core.HoldValidation() // 别人正占着干跑
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rename("香港1-新") }()
	time.Sleep(300 * time.Millisecond) // 让保存走到干跑那一步

	start := time.Now()
	if err := run.DB().Create(&model.Setting{Key: "probe", Value: "1"}).Error; err != nil {
		t.Fatalf("保存在排队干跑时,别的写库不该被挡: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("保存在排队干跑时占着写锁:别的写库等了 %v", d)
	}
	select {
	case w := <-done:
		t.Fatalf("干跑锁还被占着,保存不该已经返回: %d %s", w.Code, w.Body.String())
	default:
	}

	release()
	released = true
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatalf("放开后保存应成功: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("放开干跑锁后保存应立刻完成")
	}
	var n int64
	run.DB().Model(&model.Line{}).Where("name = ?", "香港1-新").Count(&n)
	if n != 1 {
		t.Fatal("保存应已落库")
	}
}

// 干跑在事务外做,"校验过的"和"写进去的"可能不是同一份:干跑期间别人改了库,写入时渲染出来的配置就变了。
// 这时必须按新的这份再校验 —— 别人写进去的是一条坏线路(ss2022 缺 PSK,初始化才报错),这笔保存就不能提交。
func TestLineSaveRevalidatesWhenConfigChangesDuringDryRun(t *testing.T) {
	_, run, rename := saveEnv(t)
	db := run.DB()
	release := core.HoldValidation()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rename("香港1-改名") }()
	time.Sleep(300 * time.Millisecond)

	// 干跑排队期间,另一处(绕过校验)写进一条坏线路并分配给用户
	db.Create(&model.User{Name: "u", Enabled: true, Credentials: []byte(`{"shadowsocks":{"password":"` + randomBase64(32) + `"},"shadowsocks16":{"password":"` + randomBase64(16) + `"}}`)})
	bad := model.Line{Name: "ss2022", Protocol: "shadowsocks", Port: idleTCPPort(t), Enabled: true, Options: []byte(`{"method":"2022-blake3-aes-128-gcm"}`)}
	db.Create(&bad)
	var u model.User
	db.Where("name = ?", "u").First(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: bad.Id})
	release()

	select {
	case w := <-done:
		if w.Code == 200 || !strings.Contains(w.Body.String(), "已取消保存") {
			t.Fatalf("写入时整份配置已经过不了校验,保存应被拒: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("保存没有返回")
	}
	var n int64
	db.Model(&model.Line{}).Where("name = ?", "香港1-改名").Count(&n)
	if n != 0 {
		t.Fatal("没通过校验的保存不该落库")
	}

	// 坏线路去掉以后照常能存
	db.Delete(&bad)
	if w := rename("香港1-改名"); w.Code != 200 {
		t.Fatalf("配置恢复正常后保存应成功: %d %s", w.Code, w.Body.String())
	}
}

// 生产上一台副机不响应、一台令牌对不上(401)时,保存线路不能被主副机同步拖住:保存不和副机通信,
// 也不等同步占着的任何锁。这里让同步一直挂在一台只收不答的"副机"上,再改线路。
func TestLineSaveNotBlockedBySlowOrRejectingNodes(t *testing.T) {
	_, run, rename := saveEnv(t)
	db := run.DB()

	hole, err := net.Listen("tcp", "127.0.0.1:0") // 收下连接,一个字节也不回
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := hole.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		hole.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	}()
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"令牌错误"}`, http.StatusUnauthorized)
	}))
	defer reject.Close()
	db.Create(&model.Node{Name: "黑洞", ApiUrl: "http://" + hole.Addr().String() + "/app/", Token: "t", Enabled: true})
	db.Create(&model.Node{Name: "直连", ApiUrl: reject.URL + "/app/", Token: "旧令牌", Enabled: true})

	h := run.Hub()
	syncing := make(chan struct{})
	go func() { defer close(syncing); _ = h.SyncNow() }() // 推给两台:一台挂着等超时,一台 401
	defer func() {
		h.Stop() // 在途请求随之取消
		<-syncing
	}()
	time.Sleep(500 * time.Millisecond)

	for i, name := range []string{"香港1-a", "香港1-b", "香港1-c"} {
		start := time.Now()
		w := rename(name)
		if w.Code != 200 {
			t.Fatalf("第 %d 次保存失败: %d %s", i+1, w.Code, w.Body.String())
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("副机不响应 / 401 时第 %d 次保存用了 %v,应在 3 秒内返回", i+1, d)
		}
	}
}

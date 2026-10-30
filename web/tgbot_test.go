package web

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// fakeTelegram 假的 Bot API:队列里放消息,记下机器人发出来的东西。
type fakeTelegram struct {
	mu       sync.Mutex
	queue    []string // 每条 "chat|文字"
	base     int64
	offsets  []string
	sent     []string
	docs     []string
	commands int
}

func (f *fakeTelegram) push(chat int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, fmt.Sprintf("%d|%s", chat, text))
}

func (f *fakeTelegram) replies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
		w.WriteHeader(404)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/botTOKEN/") {
	case "getUpdates":
		q := r.URL.Query().Get("offset")
		f.offsets = append(f.offsets, q)
		var off int64
		fmt.Sscan(q, &off)
		type msg struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		}
		var out []msg
		for i, item := range f.queue {
			id := f.base + int64(i) + 1
			if off >= 0 && id < off {
				continue
			}
			if off < 0 && i != len(f.queue)-1 { // 负数 offset:只给最后一条,前面的算确认过
				continue
			}
			var m msg
			m.UpdateID = id
			parts := strings.SplitN(item, "|", 2)
			fmt.Sscan(parts[0], &m.Message.Chat.ID)
			m.Message.Text = parts[1]
			out = append(out, m)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": out})
	case "sendMessage":
		var b struct {
			Text string `json:"text"`
			Chat string `json:"chat_id"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		f.sent = append(f.sent, b.Chat+"|"+b.Text)
		w.Write([]byte(`{"ok":true}`))
	case "sendDocument":
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == "document" {
				data, _ := io.ReadAll(p)
				f.docs = append(f.docs, fmt.Sprintf("%s:%d", p.FileName(), len(data)))
			}
		}
		w.Write([]byte(`{"ok":true}`))
	case "setMyCommands":
		f.commands++
		w.Write([]byte(`{"ok":true}`))
	default:
		w.WriteHeader(404)
	}
}

func botServer(t *testing.T) (*Server, *fakeTelegram) {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	f := &fakeTelegram{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s := NewServer(run)
	for k, v := range map[string]string{"tgEnabled": "true", "tgToken": "TOKEN", "tgChatId": "42", "tgBot": "true", "tgApiBase": srv.URL} {
		s.db.Create(&model.Setting{Key: k, Value: v})
	}
	return s, f
}

func TestTgBotCommands(t *testing.T) {
	s, f := botServer(t)
	s.db.Create(&model.User{Name: "alice", Enabled: true, Remark: "VIP <1>", Credentials: generateCredentials("alice"), Volume: 10 << 30, Up: 1 << 30, Down: 1 << 30})
	s.db.Create(&model.User{Name: "bob smith", Enabled: true, Credentials: generateCredentials("bob smith")})
	alice := func() model.User {
		var u model.User
		s.db.Where("name = ?", "alice").First(&u)
		return u
	}

	if got := s.tgCommand("/help"); !strings.Contains(got, "/extend") {
		t.Fatalf("帮助: %s", got)
	}
	if got := s.tgCommand("/status@my_bot"); !strings.Contains(got, "启用 2 / 共 2") || !strings.Contains(got, "在线:0 人") {
		t.Fatalf("概况(群里带 @机器人名也认): %s", got)
	}
	if got := s.tgCommand("/find ali"); !strings.Contains(got, "<code>alice</code>") || !strings.Contains(got, "2.00 GB / 10.00 GB") || strings.Contains(got, "bob") {
		t.Fatalf("查找: %s", got)
	}
	if got := s.tgCommand("/find VIP"); !strings.Contains(got, "alice") {
		t.Fatalf("备注也能查: %s", got)
	}
	if got := s.tgCommand("/find nobody"); got != "没找到" {
		t.Fatalf("查不到: %s", got)
	}
	got := s.tgCommand("/user alice")
	if !strings.Contains(got, "VIP &lt;1&gt;") || !strings.Contains(got, "当前不在线") || !strings.Contains(got, "/sub/") {
		t.Fatalf("详情要带备注(转义)和订阅地址: %s", got)
	}
	if got := s.tgCommand("/user bob smith"); !strings.Contains(got, "bob smith") {
		t.Fatalf("名字带空格: %s", got)
	}
	if got := s.tgCommand("/user"); !strings.Contains(got, "用法") {
		t.Fatalf("少参数: %s", got)
	}
	if got := s.tgCommand("/user <gone>"); !strings.Contains(got, "&lt;gone&gt;") {
		t.Fatalf("回显要转义,否则 Telegram 拒收: %s", got)
	}

	// 延长:从现在算(原来没到期时间就是不限,给了 days 就是 N 天后)
	if got := s.tgCommand("/extend alice 30"); !strings.Contains(got, "已延长 30 天") {
		t.Fatalf("延长: %s", got)
	}
	exp := alice().Expiry
	if d := exp - time.Now().Unix(); d < 29*86400 || d > 31*86400 {
		t.Fatalf("到期时间: %d", d)
	}
	s.tgCommand("/extend alice 10")
	if d := alice().Expiry - exp; d != 10*86400 {
		t.Fatalf("没到期时在原到期上加: %d", d)
	}
	for _, bad := range []string{"/extend alice", "/extend alice 0", "/extend alice -3", "/extend alice x", "/extend alice 4000"} {
		if got := s.tgCommand(bad); !strings.Contains(got, "用法") && !strings.Contains(got, "整数") {
			t.Fatalf("%s: %s", bad, got)
		}
	}
	if got := s.tgCommand("/extend bob smith 5"); !strings.Contains(got, "已延长 5 天") {
		t.Fatalf("带空格的名字加天数: %s", got)
	}

	// 重置流量、停用、启用、踢线
	if got := s.tgCommand("/reset alice"); !strings.Contains(got, "流量已重置") {
		t.Fatalf("重置: %s", got)
	}
	if u := alice(); u.Up != 0 || u.Down != 0 {
		t.Fatalf("流量要清: %d %d", u.Up, u.Down)
	}
	if got := s.tgCommand("/disable alice"); !strings.Contains(got, "已停用") {
		t.Fatalf("停用: %s", got)
	}
	if u := alice(); u.Enabled || u.DisabledReason != model.DisabledManual {
		t.Fatalf("手动停用: %+v", u)
	}
	if got := s.tgCommand("/find alice"); !strings.Contains(got, "[已停用]") {
		t.Fatalf("列表要标出停用: %s", got)
	}
	if got := s.tgCommand("/enable alice"); !strings.Contains(got, "已启用") || !alice().Enabled {
		t.Fatalf("启用: %s", got)
	}
	if got := s.tgCommand("/kick alice"); !strings.Contains(got, "已断开") {
		t.Fatalf("踢线: %s", got)
	}
	if got := s.tgCommand("/nope"); !strings.Contains(got, "不认识") {
		t.Fatalf("未知命令: %s", got)
	}

	// 动作记进审计,操作人是 telegram
	var n int64
	s.db.Model(&model.Change{}).Where("actor = ?", "telegram").Count(&n)
	if n < 4 {
		t.Fatalf("审计: %d", n)
	}

	// 备份:发文件,不回文字
	if got := s.tgCommand("/backup"); got != "" {
		t.Fatalf("备份: %s", got)
	}
	f.mu.Lock()
	docs := append([]string(nil), f.docs...)
	f.mu.Unlock()
	if len(docs) != 1 || !strings.HasSuffix(strings.Split(docs[0], ":")[0], ".zip") {
		t.Fatalf("备份文件: %v", docs)
	}
}

// 整圈:开机前积压的不执行;别的会话不理;打开才取;关掉就不再执行。
func TestTgBotRound(t *testing.T) {
	s, f := botServer(t)
	s.db.Create(&model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice")})
	enabled := func() bool {
		var u model.User
		s.db.Where("name = ?", "alice").First(&u)
		return u.Enabled
	}
	st := &tgBotState{}

	f.push(42, "/disable alice") // 开机前发的
	if wait := s.tgBotRound(st, 0); wait != 0 {
		t.Fatalf("第一轮: %v", wait)
	}
	if !enabled() {
		t.Fatal("积压的命令不能补执行")
	}
	if f.commands != 1 {
		t.Fatalf("要设命令菜单: %d", f.commands)
	}

	f.push(99, "/disable alice") // 别的会话
	f.push(42, "随便说一句")           // 不是命令
	f.push(42, "/status")
	s.tgBotRound(st, 0)
	if !enabled() {
		t.Fatal("别的会话发的命令不能执行")
	}
	got := f.replies()
	if len(got) != 1 || !strings.HasPrefix(got[0], "42|") || !strings.Contains(got[0], "启用") {
		t.Fatalf("只回命令、只回给设置的会话: %v", got)
	}

	// 取过的不重复执行
	s.tgBotRound(st, 0)
	if got := f.replies(); len(got) != 0 {
		t.Fatalf("同一条不能执行两遍: %v", got)
	}

	// 关掉开关:不取消息
	s.db.Model(&model.Setting{}).Where("key = ?", "tgBot").Update("value", "false")
	f.push(42, "/disable alice")
	polls := len(f.offsets)
	if wait := s.tgBotRound(st, 0); wait == 0 {
		t.Fatal("关着要隔一会儿再看")
	}
	if len(f.offsets) != polls {
		t.Fatal("关着不该去取消息")
	}
	// 再打开:关着期间发的也算积压
	s.db.Model(&model.Setting{}).Where("key = ?", "tgBot").Update("value", "true")
	s.tgBotRound(st, 0)
	if !enabled() {
		t.Fatal("关着期间发的不能补执行")
	}

	// 副机不跑机器人
	s.db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	if s.tgBotOn() {
		t.Fatal("副机不跑机器人")
	}
}

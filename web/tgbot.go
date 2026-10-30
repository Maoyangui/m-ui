package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// Telegram 机器人:手机上查用户、续期、重置流量、启停、要一份备份,不用开面板。
//
// 只在主机上跑,默认关(设置 → 通知)。用的是通知那个 Bot,主动去 Telegram 取消息(长轮询),不开端口;
// 只听设置里那个 Chat ID,别的会话发来的一律不理。动作走外部 API 同一套逻辑,审计里记成 telegram。

const tgBotHelp = `可用命令:
/status 概况
/find 关键词 找用户
/user 名字 用户详情和订阅地址
/extend 名字 天数 延长到期
/reset 名字 重置流量
/enable 名字 启用
/disable 名字 停用并断开
/kick 名字 断开连接
/backup 发一份备份`

var tgBotCommands = [][2]string{
	{"status", "概况"}, {"find", "找用户:/find 关键词"}, {"user", "用户详情:/user 名字"}, {"extend", "延长到期:/extend 名字 天数"},
	{"reset", "重置流量:/reset 名字"}, {"enable", "启用:/enable 名字"}, {"disable", "停用并断开:/disable 名字"}, {"kick", "断开连接:/kick 名字"},
	{"backup", "发一份备份"},
}

func (s *Server) tgBotOn() bool {
	return s.role() == "master" && strings.EqualFold(s.setting("tgBot"), "true") && s.run != nil && s.run.Notifier().Enabled()
}

// tgBotLoop 关着时只隔几秒看一眼开关;开着时长轮询。
func (s *Server) tgBotLoop() {
	st := &tgBotState{}
	for {
		if wait := s.tgBotRound(st, 50); wait > 0 {
			time.Sleep(wait)
		}
	}
}

type tgBotState struct {
	offset int64
	ready  bool // 已经丢掉了积压的消息
}

// tgBotRound 跑一轮,返回下一轮之前要等多久。
func (s *Server) tgBotRound(st *tgBotState, waitSeconds int) time.Duration {
	if !s.tgBotOn() {
		st.ready = false
		return 10 * time.Second
	}
	n := s.run.Notifier()
	if !st.ready {
		// 刚开机或刚打开时,队列里可能堆着之前发的命令:丢掉,不补执行
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		ups, err := n.Poll(ctx, -1, 0)
		cancel()
		if err != nil {
			logger.Warning("Telegram 机器人: ", err)
			return 30 * time.Second
		}
		for _, u := range ups {
			st.offset = u.ID + 1
		}
		st.ready = true
		n.SetCommands(tgBotCommands) // 输入框旁边的命令菜单;失败不影响用
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(waitSeconds+20)*time.Second)
	ups, err := n.Poll(ctx, st.offset, waitSeconds)
	cancel()
	if err != nil {
		logger.Warning("Telegram 机器人: ", err)
		return 15 * time.Second
	}
	chat := strings.TrimSpace(s.setting("tgChatId"))
	on := s.tgBotOn() // 长轮询等了好一会儿,这期间可能刚关掉
	for _, u := range ups {
		st.offset = u.ID + 1
		if !on || u.ChatID != chat || !strings.HasPrefix(u.Text, "/") {
			continue
		}
		if reply := s.tgCommand(u.Text); reply != "" {
			if err := n.Send(reply); err != nil {
				logger.Warning("Telegram 机器人回复失败: ", err)
			}
		}
	}
	return 0
}

// tgCommand 执行一条命令,返回要回的话(空 = 已经单独发了)。
func (s *Server) tgCommand(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return ""
	}
	cmd := strings.ToLower(f[0])
	if i := strings.Index(cmd, "@"); i > 0 { // 群里发的命令带 @机器人名
		cmd = cmd[:i]
	}
	args := f[1:]
	name := strings.Join(args, " ") // 用户名里可能有空格
	switch cmd {
	case "/start", "/help":
		return tgBotHelp
	case "/status":
		return s.tgStatus()
	case "/find":
		if name == "" {
			return "用法:/find 关键词"
		}
		return s.tgFind(name)
	case "/user":
		if name == "" {
			return "用法:/user 名字"
		}
		return s.tgUser(name)
	case "/extend":
		if len(args) < 2 {
			return "用法:/extend 名字 天数"
		}
		days, err := strconv.Atoi(args[len(args)-1])
		if err != nil || days <= 0 || days > 3650 {
			return "天数要是 1 到 3650 的整数"
		}
		name = strings.Join(args[:len(args)-1], " ")
		u, ok := s.apiFindUser(name, 0)
		if !ok {
			return "没有这个用户:" + notify.Esc(name)
		}
		body, _ := json.Marshal(map[string]int{"days": days})
		if msg := s.tgDo(u, http.MethodPatch, "", string(body)); msg != "" {
			return "没有延长:" + notify.Esc(msg)
		}
		return "已延长 " + strconv.Itoa(days) + " 天\n" + s.tgUser(u.Name)
	case "/reset", "/enable", "/disable", "/kick":
		if name == "" {
			return "用法:" + cmd + " 名字"
		}
		u, ok := s.apiFindUser(name, 0)
		if !ok {
			return "没有这个用户:" + notify.Esc(name)
		}
		if msg := s.tgDo(u, http.MethodPost, cmd[1:], "{}"); msg != "" {
			return "没做成:" + notify.Esc(msg)
		}
		return map[string]string{"/reset": "流量已重置", "/enable": "已启用", "/disable": "已停用并断开", "/kick": "已断开"}[cmd] + ":" + notify.Esc(u.Name)
	case "/backup":
		return s.tgBackup()
	}
	return "不认识这个命令,发 /help 看看"
}

// tgDo 走外部 API 同一套逻辑(校验、踢线、重载、审计都在里面)。返回错误文案,空 = 成功。
func (s *Server) tgDo(u model.User, method, action, body string) string {
	rec := &tgRecorder{header: http.Header{}, code: http.StatusOK}
	req, err := http.NewRequest(method, "http://tg.invalid/", strings.NewReader(body))
	if err != nil {
		return err.Error()
	}
	sc := apiScope{actor: "telegram"}
	if action == "" {
		s.apiUpdateUser(rec, req, u, sc)
	} else {
		s.apiUserAction(rec, req, u, action, sc)
	}
	if rec.code >= 300 {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.body, &out)
		if out.Error == "" {
			out.Error = http.StatusText(rec.code)
		}
		return out.Error
	}
	return ""
}

type tgRecorder struct {
	header http.Header
	code   int
	body   []byte
}

func (r *tgRecorder) Header() http.Header         { return r.header }
func (r *tgRecorder) WriteHeader(code int)        { r.code = code }
func (r *tgRecorder) Write(b []byte) (int, error) { r.body = append(r.body, b...); return len(b), nil }

func (s *Server) tgStatus() string {
	var total, enabled int64
	s.db.Model(&model.User{}).Count(&total)
	s.db.Model(&model.User{}).Where("enabled = ?", true).Count(&enabled)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>m-ui v%s</b>\n用户:启用 %d / 共 %d\n", notify.Esc(Version), enabled, total)
	snap := s.apiSnapshot()
	online := map[string]bool{}
	for _, m := range []map[string][]string{snap.localIPs, snap.remoteIPs} {
		for name, ips := range m {
			if len(ips) > 0 {
				online[name] = true
			}
		}
	}
	fmt.Fprintf(&b, "在线:%d 人\n", len(online))
	if s.run.Hub() != nil {
		var nodes []model.Node
		s.db.Where("enabled = ? AND is_local = ?", true, false).Order("sort asc, id asc").Find(&nodes)
		statuses := s.run.Hub().Statuses()
		var down []string
		for _, n := range nodes {
			if st, ok := statuses[n.Id]; ok && !st.OK {
				down = append(down, n.Name)
			}
		}
		if len(nodes) > 0 {
			fmt.Fprintf(&b, "副机:%d 台在线 / 共 %d 台\n", len(nodes)-len(down), len(nodes))
		}
		if len(down) > 0 {
			b.WriteString("🔴 失联:" + notify.Esc(strings.Join(down, "、")) + "\n")
		}
	}
	var failing []string
	for _, row := range s.upstreamHealthRows() {
		for _, sv := range row.Servers {
			if sv.State == "fail" {
				failing = append(failing, row.Name)
				break
			}
		}
	}
	if len(failing) > 0 {
		b.WriteString("🔴 上游故障:" + notify.Esc(strings.Join(failing, "、")) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

const tgFindLimit = 20

func (s *Server) tgFind(kw string) string {
	var users []model.User
	like := "%" + kw + "%"
	s.db.Where("name LIKE ? OR remark LIKE ?", like, like).Order("id asc").Limit(tgFindLimit + 1).Find(&users)
	if len(users) == 0 {
		return "没找到"
	}
	var b strings.Builder
	shown := users
	if len(shown) > tgFindLimit {
		shown = shown[:tgFindLimit]
	}
	for _, u := range shown {
		b.WriteString(tgUserLine(u) + "\n")
	}
	if len(users) > tgFindLimit {
		fmt.Fprintf(&b, "……只列前 %d 个,再写详细点", tgFindLimit)
	}
	return strings.TrimRight(b.String(), "\n")
}

func tgUserLine(u model.User) string {
	state := ""
	if !u.Enabled {
		state = " [已停用]"
	}
	return "<code>" + notify.Esc(u.Name) + "</code> " + tgBytes(u.Up+u.Down) + " / " + tgVolume(u.Volume) + " · " + tgExpiry(u) + state
}

func (s *Server) tgUser(name string) string {
	u, ok := s.apiFindUser(name, 0)
	if !ok {
		return "没有这个用户:" + notify.Esc(name)
	}
	v := s.apiUser(u)
	var b strings.Builder
	b.WriteString(tgUserLine(u) + "\n")
	ips := append([]string(nil), v.OnlineIPs...)
	sort.Strings(ips)
	if len(ips) > 0 {
		b.WriteString("在线:" + notify.Esc(strings.Join(ips, "、")) + "\n")
	} else {
		b.WriteString("当前不在线\n")
	}
	if u.Remark != "" {
		b.WriteString("备注:" + notify.Esc(u.Remark) + "\n")
	}
	if v.SubLink != "" {
		b.WriteString("订阅:<code>" + notify.Esc(v.SubLink) + "</code>")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s *Server) tgBackup() string {
	f, err := s.run.CreateBackupFile()
	if err != nil {
		return "备份失败:" + notify.Esc(err.Error())
	}
	path, err := s.run.BackupFilePath(f.Name)
	if err != nil {
		return "备份失败:" + notify.Esc(err.Error())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "备份失败:" + notify.Esc(err.Error())
	}
	if err := s.run.Notifier().SendDocument(f.Name, data, "m-ui 备份"); err != nil {
		return "备份做好了,但发不出来:" + notify.Esc(err.Error())
	}
	s.auditAs("telegram", "backup", "send", f.Name)
	return ""
}

func tgBytes(n int64) string {
	const gb = 1 << 30
	switch {
	case n >= gb:
		return strconv.FormatFloat(float64(n)/gb, 'f', 2, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 0, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func tgVolume(n int64) string {
	if n <= 0 {
		return "不限"
	}
	return tgBytes(n)
}

func tgExpiry(u model.User) string {
	switch {
	case u.ActivateDays > 0 && u.Expiry == 0:
		return "首次连接后 " + strconv.Itoa(u.ActivateDays) + " 天"
	case u.Expiry <= 0:
		return "不过期"
	}
	return "到期 " + time.Unix(u.Expiry, 0).Format("2006-01-02")
}

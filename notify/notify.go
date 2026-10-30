// Package notify 通过 Telegram Bot 推送事件通知,并做同类事件的去重节流。
//
// 设置项:tgEnabled / tgToken / tgChatId / tgProxy(可选,http 或 socks5 URL);
// 各事件开关 tgOn* 缺省视为开启,只有显式 "false" 才关闭。
// 同样的事件也可以推给 Webhook(webhookUrl / webhookSecret),和 Telegram 互不依赖。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

type Notifier struct {
	setting func(string) string
	mu      sync.Mutex
	last    map[string]int64
	clients map[string]*http.Client // 按代理地址缓存
	// durable 是 OnceDurable 的去重记录(key → 到期时间戳),save 把它写回库;没调 Persist 时只在内存里
	durable map[string]int64
	save    func(string)
	saveMu  sync.Mutex

	// Send 可替换,便于测试;默认走 Telegram API
	sendFn func(token, chatID, text string) error
}

func New(setting func(string) string) *Notifier {
	n := &Notifier{setting: setting, last: map[string]int64{}, clients: map[string]*http.Client{}}
	n.sendFn = n.telegram
	return n
}

// Enabled 报告通知是否已配置并开启。
func (n *Notifier) Enabled() bool {
	return strings.EqualFold(n.setting("tgEnabled"), "true") && n.setting("tgToken") != "" && n.setting("tgChatId") != ""
}

// Event 在总开关与事件开关(toggleKey,缺省开)都打开时异步发送。
func (n *Notifier) Event(toggleKey, text string) {
	if toggleKey != "" && strings.EqualFold(n.setting(toggleKey), "false") {
		return
	}
	if u := strings.TrimSpace(n.setting("webhookUrl")); u != "" {
		go func() {
			if err := n.postWebhook(u, eventName(toggleKey), text); err != nil {
				logger.Warning("Webhook 发送失败: ", err)
			}
		}()
	}
	if !n.Enabled() {
		return
	}
	go func() {
		if err := n.Send(text); err != nil {
			logger.Warning("Telegram 通知发送失败: ", err)
		}
	}()
}

// Once 同一 key 在 ttl 内只放行一次(去重节流)。
func (n *Notifier) Once(key string, ttl time.Duration) bool {
	now := time.Now().Unix()
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.last[key]; ok && now-last < int64(ttl.Seconds()) {
		return false
	}
	n.last[key] = now
	// 顺手清理过期项,避免无限增长
	if len(n.last) > 4096 {
		for k, v := range n.last {
			if now-v > 7*86400 {
				delete(n.last, k)
			}
		}
	}
	return true
}

// Persist 让 OnceDurable 的去重记录落库:以前只在内存里,面板重启(含自升级)后当天的日报、本周期的流量告急
// 会立刻再发一遍(审计 MB13)。
func (n *Notifier) Persist(load func() string, save func(string)) {
	m := map[string]int64{}
	if raw := load(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	n.mu.Lock()
	n.durable, n.save = m, save
	n.mu.Unlock()
}

// OnceDurable 同 Once,但去重记录随 Persist 落库,重启后照样算数;过期的记录写回时顺手清掉。
func (n *Notifier) OnceDurable(key string, ttl time.Duration) bool {
	now := time.Now().Unix()
	n.mu.Lock()
	if n.durable == nil {
		n.durable = map[string]int64{}
	}
	if until, ok := n.durable[key]; ok && until > now {
		n.mu.Unlock()
		return false
	}
	n.durable[key] = now + int64(ttl.Seconds())
	n.mu.Unlock()
	n.flush()
	return true
}

// flush 清掉过期记录后把当前去重记录写回库;saveMu 保证并发写回时最后落库的是最新状态。
func (n *Notifier) flush() {
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	now := time.Now().Unix()
	n.mu.Lock()
	for k, until := range n.durable {
		if until <= now {
			delete(n.durable, k)
		}
	}
	b, _ := json.Marshal(n.durable)
	save := n.save
	n.mu.Unlock()
	if save != nil {
		save(string(b))
	}
}

// Forget 清除某个去重 key(例如用户重置流量后允许再次告警);持久记录里的 key 与以 "key:" 开头的一并清掉。
// 清到持久记录时会写库,不要在数据库事务里调用(写锁在事务手里,会等到超时)。
func (n *Notifier) Forget(key string) {
	n.mu.Lock()
	delete(n.last, key)
	hit := false
	for k := range n.durable {
		if k == key || strings.HasPrefix(k, key+":") {
			delete(n.durable, k)
			hit = true
		}
	}
	n.mu.Unlock()
	if hit {
		n.flush()
	}
}

// Send 同步发送一条消息(HTML 格式)。
func (n *Notifier) Send(text string) error {
	token, chat := n.setting("tgToken"), n.setting("tgChatId")
	if token == "" || chat == "" {
		return errors.New("未配置 Telegram Bot Token 或 Chat ID")
	}
	return n.sendFn(token, chat, text)
}

// Test 发送一条测试消息(设置页"发送测试"按钮)。
func (n *Notifier) Test() error {
	text := fmt.Sprintf("✅ <b>m-ui 通知已连通</b>\n%s", time.Now().Format("2006-01-02 15:04:05"))
	u := strings.TrimSpace(n.setting("webhookUrl"))
	if u != "" {
		if err := n.postWebhook(u, "test", text); err != nil {
			return fmt.Errorf("Webhook 发送失败: %w", err)
		}
		if n.setting("tgToken") == "" {
			return nil // 只配了 Webhook
		}
	}
	return n.Send(text)
}

// eventName tgOnLogin → login,tgDaily → daily。
func eventName(toggleKey string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(toggleKey, "tgOn"), "tg"))
}

var htmlTag = regexp.MustCompile("<[^>]+>")

// postWebhook 把事件 POST 给设置的地址:JSON 正文,配了密钥就带 HMAC-SHA256 签名,对方能验是不是这个面板发的。
func (n *Notifier) postWebhook(url, event, text string) error {
	plain := htmlTag.ReplaceAllString(text, "")
	plain = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(plain)
	body, _ := json.Marshal(map[string]interface{}{"event": event, "text": plain, "time": time.Now().Unix()})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "m-ui")
	if secret := n.setting("webhookSecret"); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-M-UI-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("对方返回 %d", resp.StatusCode)
	}
	return nil
}

// api Telegram 接口的地址。官方连不上的地方可以换成自建的 Bot API(设置 tgApiBase)。
func (n *Notifier) api(token, method string) string {
	base := strings.TrimRight(strings.TrimSpace(n.setting("tgApiBase")), "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	return base + "/bot" + token + "/" + method
}

// Update 机器人收到的一条消息。
type Update struct {
	ID     int64
	ChatID string
	Text   string
}

// Poll 取新消息。offset = 上次最大 ID + 1;waitSeconds > 0 是长轮询。
func (n *Notifier) Poll(ctx context.Context, offset int64, waitSeconds int) ([]Update, error) {
	token := n.setting("tgToken")
	if token == "" {
		return nil, errors.New("未配置 Telegram Bot Token 或 Chat ID")
	}
	u := fmt.Sprintf("%s?timeout=%d&offset=%d&allowed_updates=%%5B%%22message%%22%%5D", n.api(token, "getUpdates"), waitSeconds, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 通知用的客户端超时 15 秒,长轮询要等更久:沿用它的代理设置,超时交给 ctx
	c := *n.client()
	c.Timeout = 0
	resp, err := c.Do(req)
	if err != nil {
		return nil, sanitizeToken(err, token)
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram %d: %s", resp.StatusCode, out.Description)
	}
	ups := make([]Update, 0, len(out.Result))
	for _, r := range out.Result {
		ups = append(ups, Update{ID: r.UpdateID, ChatID: strconv.FormatInt(r.Message.Chat.ID, 10), Text: r.Message.Text})
	}
	return ups, nil
}

// SetCommands 设置 Bot 的命令菜单。只是方便输入,失败不用管。
func (n *Notifier) SetCommands(cmds [][2]string) {
	token := n.setting("tgToken")
	if token == "" {
		return
	}
	list := make([]map[string]string, 0, len(cmds))
	for _, c := range cmds {
		list = append(list, map[string]string{"command": c[0], "description": c[1]})
	}
	body, _ := json.Marshal(map[string]interface{}{"commands": list})
	resp, err := n.client().Post(n.api(token, "setMyCommands"), "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// sanitizeToken 网络错误里带着完整地址,地址里有 Bot Token,不能进日志。
func sanitizeToken(err error, token string) error {
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

func (n *Notifier) client() *http.Client {
	proxy := strings.TrimSpace(n.setting("tgProxy"))
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.clients[proxy]; ok {
		return c
	}
	tr := &http.Transport{}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	c := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	n.clients[proxy] = c
	return c
}

func (n *Notifier) telegram(token, chatID, text string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id": chatID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true,
	})
	resp, err := n.client().Post(n.api(token, "sendMessage"), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// SendDocument 以文件形式发送(备份推送)。
func (n *Notifier) SendDocument(filename string, data []byte, caption string) error {
	token, chat := n.setting("tgToken"), n.setting("tgChatId")
	if token == "" || chat == "" {
		return errors.New("未配置 Telegram Bot Token 或 Chat ID")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("chat_id", chat)
	if caption != "" {
		mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	fw.Write(data)
	mw.Close()
	resp, err := n.client().Post(n.api(token, "sendDocument"), mw.FormDataContentType(), &body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// SetSender 替换发送函数(测试用)。
func (n *Notifier) SetSender(fn func(token, chatID, text string) error) { n.sendFn = fn }

// Esc 转义 HTML 特殊字符,用户名等外部文本进消息前必须经过。
func Esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

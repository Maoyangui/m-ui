// Package notify 通过 Telegram Bot 推送事件通知,并做同类事件的去重节流。
//
// 设置项:tgEnabled / tgToken / tgChatId / tgProxy(可选,http 或 socks5 URL);
// 各事件开关 tgOn* 缺省视为开启,只有显式 "false" 才关闭。
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
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
	if !n.Enabled() {
		return
	}
	if toggleKey != "" && strings.EqualFold(n.setting(toggleKey), "false") {
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

// Forget 清除某个去重 key(例如用户重置流量后允许再次告警)。
func (n *Notifier) Forget(key string) {
	n.mu.Lock()
	delete(n.last, key)
	n.mu.Unlock()
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
	return n.Send(fmt.Sprintf("✅ <b>m-ui 通知已连通</b>\n%s", time.Now().Format("2006-01-02 15:04:05")))
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
	resp, err := n.client().Post("https://api.telegram.org/bot"+token+"/sendMessage", "application/json", bytes.NewReader(body))
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
	resp, err := n.client().Post("https://api.telegram.org/bot"+token+"/sendDocument", mw.FormDataContentType(), &body)
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

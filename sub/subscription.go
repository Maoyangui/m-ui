package sub

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/database/model"
)

// Options 控制订阅输出(来自设置)。
type Options struct {
	ProfileTitle string  // Profile-Title 头
	UpdateHours  int     // Profile-Update-Interval
	Encode       bool    // link 订阅是否 base64
	ShowNotice   bool    // 是否注入流量提示节点
	ClashTmpl    string  // 自定义 clash 模板,空=内置
	Entries      []Entry // 对外入口(单入口即一条)
}

// Result 是一份渲染好的订阅响应。
type Result struct {
	Body    string
	Headers map[string]string
}

// BuildLinkSub 生成分享链接订阅(可选 base64)。
func BuildLinkSub(user model.User, lines []model.Line, opt Options) Result {
	var out []string
	if opt.ShowNotice {
		out = append(out, noticeLinkURI(user))
	}
	out = append(out, GenerateLinks(user, lines, opt.Entries)...)
	body := strings.Join(out, "\n")
	if opt.Encode {
		body = base64.StdEncoding.EncodeToString([]byte(body))
	}
	return Result{Body: body, Headers: headers(user, opt, "text/plain; charset=utf-8")}
}

// BuildClashSub 生成 clash 订阅。
func BuildClashSub(user model.User, lines []model.Line, opt Options) (Result, error) {
	notice := ""
	if opt.ShowNotice {
		notice = noticeText(user)
	}
	body, err := BuildClash(user, lines, opt.Entries, opt.ClashTmpl, notice)
	if err != nil {
		return Result{}, err
	}
	return Result{Body: body, Headers: headers(user, opt, "text/yaml; charset=utf-8")}, nil
}

// headers 组装订阅响应头(流量信息 + 更新周期 + 标题)。
func headers(user model.User, opt Options, contentType string) map[string]string {
	title := opt.ProfileTitle
	if title == "" {
		title = user.Remark
	}
	if title == "" {
		title = user.Name
	}
	return map[string]string{
		"Content-Type":            contentType,
		"Subscription-Userinfo":   fmt.Sprintf("upload=%d; download=%d; total=%d; expire=%d", user.Up, user.Down, user.Volume, user.Expiry),
		"Profile-Update-Interval": fmt.Sprintf("%d", opt.UpdateHours),
		"Profile-Title":           title,
	}
}

// noticeText 生成流量提示节点名:00-勿选-流量:<已用>/<总量> 重置:<天>天
func noticeText(user model.User) string {
	used := formatGB(user.Up + user.Down)
	total := "不限GB"
	if user.Volume > 0 {
		total = formatGB(user.Volume) + "GB"
	}
	reset := "--天"
	if user.NextReset > 0 {
		left := user.NextReset - time.Now().Unix()
		days := int64(0)
		if left > 0 {
			days = int64(math.Ceil(float64(left) / 86400))
		}
		reset = fmt.Sprintf("%d天", days)
	}
	return fmt.Sprintf("00-勿选-流量:%s/%s 重置:%s", used, total, reset)
}

func formatGB(n int64) string {
	if n < 0 {
		n = 0
	}
	return fmt.Sprintf("%.1f", float64(n)/(1024*1024*1024))
}

// noticeLinkURI 把提示节点表示为一个 ss 占位链接(备注为提示文本)。
func noticeLinkURI(user model.User) string {
	name := noticeText(user)
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:dummy"))
	return fmt.Sprintf("ss://%s@127.0.0.1:1#%s", userinfo, url.QueryEscape(name))
}

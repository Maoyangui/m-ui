// Package brand 统一放置项目标识与联系方式:logo(轻量 SVG)、作者 Telegram、打赏地址。
// 面板、订阅落地页、"建议 / 支持"页都从这里取,改一处即可全站生效。
package brand

import (
	_ "embed"
	"encoding/base64"
	"net/http"
)

//go:embed logo.svg
var SVG []byte

// DataURI 内联形式,给无法引用静态文件的页面(订阅落地页)做标签图标。
var DataURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(SVG)

const (
	Name     = "m-ui"
	Telegram = "yangweii"                           // 作者 Telegram 用户名(不含 @)
	Tron     = "TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm" // 打赏地址:TRON(TRX / USDT-TRC20)
	RepoPath = "Maoyangui/m-ui"               // GitHub 仓库 owner/name;换仓库改这里(deploy/install.sh 的 REPO 与 go.mod 模块路径另改)
	Repo     = "https://github.com/" + RepoPath
)

// TelegramURL 点击即可打开对话。
const TelegramURL = "https://t.me/" + Telegram

// ServeLogo 输出 logo.svg,允许浏览器缓存一天。
func ServeLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(SVG)
}

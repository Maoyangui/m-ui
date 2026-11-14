package hysteria2

import (
	"net/http"
	"strconv"

	"github.com/Maoyangui/m-ui/core/protocol/arrival"
)

// hysteria2 的认证是 QUIC 握手之后的一个 HTTP/3 请求(POST https://hysteria/auth,密码在 Hysteria-Auth 头里)。
// 密码不对时 sing-quic 把这个请求交给伪装站点(没配就是 404),不关连接、也不经过入站 —— 面板上一点痕迹都没有。
// 这里包在伪装站点外面:认出这种请求,记一笔"到了但认证没过",再原样交给伪装站点,回给对方的内容一个字节都不变。
// 浏览器、扫描器访问伪装站点的普通请求不是认证请求,不记。
const (
	authHost   = "hysteria" // sing-quic hysteria2/internal/protocol.URLHost
	authPath   = "/auth"    // protocol.URLPath
	authRxHead = "Hysteria-CC-RX"
)

type authFailWatch struct {
	next http.Handler
	in   *Inbound
	// rxAmbiguous 开了 ignore_client_bandwidth 又限了下行时,密码对、但客户端没报下行带宽的认证请求也会落到伪装站点
	// (sing-quic 的 "BBR disabled by server"),这种分不出来就不记
	rxAmbiguous bool
}

func (a *authFailWatch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.Host == authHost && r.URL.Path == authPath && !(a.rxAmbiguous && rxUnset(r)) {
		arrival.NoteAddr(a.in.arrivals, r.RemoteAddr, a.in.Tag(), a.in.Type(), arrival.Auth, "")
	}
	a.next.ServeHTTP(w, r)
}

// rxUnset 客户端没报下行带宽(和 sing-quic 一样按 0 处理:没写或写坏)。
func rxUnset(r *http.Request) bool {
	rx, _ := strconv.ParseUint(r.Header.Get(authRxHead), 10, 64)
	return rx == 0
}

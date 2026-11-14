package web

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 面板改了监听(端口、监听地址、证书 / 开关 HTTPS)后,在进程里换监听:不重启 m-ui,数据面、订阅、代理面板都不动,
// 在线用户不掉线。浏览器按回复(设置页、证书页「用于面板」)或证书进度(签发时勾了用于面板)里的 panelSwitch
// 等新地址能连上就跳过去(见 web/assets/js/panelmove.js)。以前这几项都要手动重启 m-ui 才生效。
//
// 换法:地址变了 —— 先在新地址起好再收旧的,新的起不来就留着旧的;地址没变(只换证书、开关 HTTPS)—— 旧的先停止接新连接,
// 原地址空出来马上起新的,起不来就按旧设置原样起回去。旧连接给 10 秒收尾。

// panelListen 面板监听的样子:地址、证书、私钥(没走 HTTPS 时证书为空)。
type panelListen struct{ addr, cert, key string }

// panelSwitch 面板要换到的新地址,给浏览器跳转用。
type panelSwitch struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host,omitempty"` // 空 = 浏览器沿用现在的主机名(没设面板域名,或换回 http)
	Port   int    `json:"port"`
	Path   string `json:"path"`
	At     int64  `json:"at"` // 预计换好的时间(unix 毫秒)
}

// URL 写进日志、给人看的新地址(主机名空着时写面板域名 / 公网 IP)。
func (p *panelSwitch) URL(fallbackHost string) string {
	host := p.Host
	if host == "" {
		host = fallbackHost
	}
	return fmt.Sprintf("%s://%s%s", p.Scheme, model.URLHost(p.Scheme, host, p.Port), p.Path)
}

// wantPanelListen 按现在的设置面板该怎么监听。
func (s *Server) wantPanelListen() panelListen {
	listen := s.setting("webListen")
	if listen == "" {
		listen = "0.0.0.0"
	}
	pl := panelListen{addr: net.JoinHostPort(listen, strconv.Itoa(s.settingInt("webPort", 2053)))}
	if c, k := s.setting("webCertFile"), s.setting("webKeyFile"); c != "" && k != "" {
		pl.cert, pl.key = c, k
	}
	return pl
}

// openPanelListener 按 pl 起监听(证书文件更新后自动换用,续期不用换监听)。
func (s *Server) openPanelListener(pl panelListen) (net.Listener, error) {
	ln, err := net.Listen("tcp", pl.addr)
	if err != nil {
		return nil, err
	}
	if pl.cert != "" {
		return tls.NewListener(ln, s.tlsFor("panel", "面板", pl.cert, pl.key)), nil
	}
	s.watchTLS("panel", nil) // 换回 http:证书提示里不再列面板
	return ln, nil
}

// servePanel 在 ln 上开一份面板 HTTP 服务(处理器同一个)。调用方持有 panelMu。
func (s *Server) servePanel(ln net.Listener, pl panelListen) {
	srv := &http.Server{Handler: s.panelHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	s.httpSrv, s.listener, s.panelCur = srv, ln, pl
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Warning("面板服务退出: ", err)
		}
	}()
}

// relistenPanel 面板按现在的设置重新监听;和现在一样就什么都不做。
func (s *Server) relistenPanel() error {
	s.panelMu.Lock()
	defer s.panelMu.Unlock()
	want, cur := s.wantPanelListen(), s.panelCur
	if want == cur || s.httpSrv == nil {
		return nil
	}
	old := s.httpSrv
	retire := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = old.Shutdown(ctx)
	}
	if want.addr != cur.addr {
		ln, err := s.openPanelListener(want)
		if err != nil {
			if cur.cert == "" {
				s.watchTLS("panel", nil)
			}
			return fmt.Errorf("面板没能在 %s 上监听,还在原地址: %w", want.addr, err)
		}
		s.servePanel(ln, want)
		go retire()
		logger.Info("面板已改到 ", want.addr, map[bool]string{true: "(HTTPS)", false: ""}[want.cert != ""])
		return nil
	}
	// 同一地址:Shutdown 先关监听再等旧连接收尾,原地址一空出来就起新的
	go retire()
	var ln net.Listener
	var err error
	for i := 0; i < 60; i++ {
		if ln, err = s.openPanelListener(want); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		for i := 0; i < 60; i++ { // 按旧设置原样起回去
			if back, e := s.openPanelListener(cur); e == nil {
				s.servePanel(back, cur)
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Errorf("面板没能换用新的证书设置,已按原样恢复: %w", err)
	}
	s.servePanel(ln, want)
	logger.Info("面板已在 ", want.addr, " 改用", map[bool]string{true: " HTTPS", false: " HTTP"}[want.cert != ""])
	return nil
}

// planPanelSwitch 面板的监听要变的话:记下新地址(证书进度据此告诉页面),delay 后在进程里换;不变返回 nil。
// 同一时刻只排一次:还没换的那次直接顶替(它会按换时的设置来)。
//
// 副机不自动换:主机连副机用的就是它面板的地址(服务器页的 API 地址),自己换了主机就连不上了,
// 仍按重启生效,让管理员先在主机上改好 API 地址再重启。
func (s *Server) planPanelSwitch(delay time.Duration) *panelSwitch {
	if s.run != nil && s.run.IsNode() {
		return nil
	}
	want := s.wantPanelListen()
	s.panelMu.Lock()
	same := want == s.panelCur || s.httpSrv == nil
	s.panelMu.Unlock()
	if same {
		return nil
	}
	_, port, _ := net.SplitHostPort(want.addr)
	p, _ := strconv.Atoi(port)
	sw := &panelSwitch{Scheme: "http", Port: p, Path: s.basePath(), At: time.Now().Add(delay).UnixMilli()}
	if want.cert != "" {
		sw.Scheme = "https"
		sw.Host = strings.TrimSpace(s.setting("webDomain")) // 证书按域名签的:有面板域名就跳到域名上,不然浏览器报证书不匹配
	}
	s.panelPending.Store(sw)
	time.AfterFunc(delay, func() {
		if err := s.relistenPanel(); err != nil {
			logger.Warning(err)
		}
		s.panelPending.CompareAndSwap(sw, nil)
	})
	return sw
}

// panelHostHint 日志里写新地址时,主机名的兜底(面板域名 → 公网 IP)。
func (s *Server) panelHostHint() string {
	if d := strings.TrimSpace(s.setting("webDomain")); d != "" {
		return d
	}
	if ip := strings.TrimSpace(s.setting("publicIp")); ip != "" {
		return ip
	}
	return "<本机IP>"
}

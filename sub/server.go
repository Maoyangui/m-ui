package sub

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"

	"gorm.io/gorm"
)

// Server 对外提供订阅:GET <subPath><用户名>[?format=clash]
type Server struct {
	db       *gorm.DB
	httpSrv  *http.Server
	listener net.Listener
}

func NewServer(db *gorm.DB) *Server {
	return &Server{db: db}
}

func (s *Server) setting(key string) string {
	var v string
	s.db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

func (s *Server) settingBool(key string) bool {
	return strings.EqualFold(s.setting(key), "true")
}

func (s *Server) settingInt(key string, def int) int {
	v, err := strconv.Atoi(s.setting(key))
	if err != nil {
		return def
	}
	return v
}

// options 每次请求时读取,便于面板改设置后即时生效。
func (s *Server) options() Options {
	domain := s.setting("webDomain")
	return Options{
		ProfileTitle: s.setting("subProfileTitle"),
		UpdateHours:  s.settingInt("subUpdates", 12),
		Encode:       s.settingBool("subEncode"),
		ShowNotice:   s.settingBool("subShowNotice"),
		ClashTmpl:    s.setting("subClashExt"),
		Entries:      []Entry{{Host: domain, SNI: domain}},
	}
}

func (s *Server) Start() error {
	subPath := s.setting("subPath")
	if subPath == "" {
		subPath = "/sub/"
	}
	if !strings.HasPrefix(subPath, "/") {
		subPath = "/" + subPath
	}
	if !strings.HasSuffix(subPath, "/") {
		subPath += "/"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(subPath, s.handle(subPath))

	listen := s.setting("subListen")
	if listen == "" {
		listen = "0.0.0.0"
	}
	port := s.settingInt("subPort", 2056)
	addr := net.JoinHostPort(listen, strconv.Itoa(port))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	certFile, keyFile := s.setting("subCertFile"), s.setting("subKeyFile")
	scheme := "http"
	if certFile != "" && keyFile != "" {
		rl, err := certutil.NewReloader(certFile, keyFile)
		if err != nil {
			ln.Close()
			return fmt.Errorf("加载订阅证书: %w", err)
		}
		ln = tls.NewListener(ln, rl.TLSConfig()) // 证书文件更新后自动换用,续期无需重启
		scheme = "https"
	}

	s.listener = ln
	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Warning("订阅服务退出: ", err)
		}
	}()
	logger.Info("订阅服务已启动 ", scheme, "://", addr, subPath)
	return nil
}

func (s *Server) Stop() error {
	if s.httpSrv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}

func (s *Server) handle(subPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, subPath)
		wantQR := strings.HasSuffix(name, "/qr")
		name = strings.TrimSuffix(name, "/qr")
		if name == "" || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}

		var user model.User
		if err := s.db.Where("name = ? AND enabled = ?", name, true).First(&user).Error; err != nil {
			s.log(r, name, 404)
			http.NotFound(w, r)
			return
		}
		if wantQR {
			s.serveQR(w, r, subPath, name)
			return
		}

		var lines []model.Line
		s.db.Raw(`SELECT l.* FROM lines l JOIN user_lines ul ON ul.line_id = l.id
			WHERE ul.user_id = ? AND l.enabled = 1 ORDER BY l.sort`, user.Id).Scan(&lines)

		opt := s.options()
		// 浏览器打开订阅地址 → 订阅页(用量/到期/一键导入/二维码);客户端拉取 → 原始订阅
		if !strings.EqualFold(s.setting("subPageEnabled"), "false") && WantsPage(r) {
			s.servePage(w, r, subPath, user, lines, opt)
			s.log(r, name, 200)
			return
		}
		format := r.URL.Query().Get("format")

		var res Result
		if format == "clash" {
			out, err := BuildClashSub(user, lines, opt)
			if err != nil {
				s.log(r, name, 500)
				http.Error(w, "生成失败", http.StatusInternalServerError)
				return
			}
			res = out
		} else {
			res = BuildLinkSub(user, lines, opt)
		}

		for k, v := range res.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(res.Body))
		}
		s.log(r, name, 200)
	}
}

// log 记录订阅访问,供面板按用户汇总(替代 nginx 日志 + 汇总脚本)。
func (s *Server) log(r *http.Request, user string, status int) {
	ip := r.Header.Get("X-Forwarded-For")
	if ip != "" {
		ip = strings.TrimSpace(strings.Split(ip, ",")[0])
	} else {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "link"
		if WantsPage(r) {
			format = "page"
		}
	}
	entry := model.SubLog{
		Ts: time.Now().Unix(), User: user, Ip: ip,
		Ua: r.UserAgent(), Format: format, Status: status,
	}
	if err := s.db.Create(&entry).Error; err != nil {
		logger.Warning("写订阅日志失败: ", err)
	}
}

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

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/logger"

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
	insecure := s.insecure()
	entries := EntriesFromNodes(s.db, s.setting("webDomain"), s.setting("publicIp"), !strings.EqualFold(s.setting("subServerAddr"), "domain"))
	for i := range entries {
		entries[i].Insecure = insecure
	}
	return Options{
		ProfileTitle: s.setting("subProfileTitle"),
		UpdateHours:  s.settingInt("subUpdates", 12),
		Encode:       s.settingBool("subEncode"),
		ShowNotice:   s.settingBool("subShowNotice"),
		ClashTmpl:    s.setting("subClashExt"),
		Entries:      entries,
		Insecure:     insecure,
	}
}

// insecure 决定订阅是否带"允许不安全":设置 subInsecure=true/false 强制;auto(默认)按数据面证书是否自签判断。
func (s *Server) insecure() bool {
	switch strings.ToLower(strings.TrimSpace(s.setting("subInsecure"))) {
	case "true":
		return true
	case "false":
		return false
	}
	return CertIsSelfSigned(s.setting("certFile"), s.setting("webCertFile"))
}

// externalFor 取用户被分配的外部节点:单条链接直接用,外部订阅用主机抓取的缓存解析。
func (s *Server) externalFor(userID uint) []ExtItem {
	var exts []model.ExtNode
	s.db.Raw(`SELECT e.* FROM ext_nodes e JOIN user_exts ue ON ue.ext_id = e.id
		WHERE ue.user_id = ? AND e.enabled = 1 ORDER BY e.sort, e.id`, userID).Scan(&exts)
	out := make([]ExtItem, 0, len(exts))
	for _, e := range exts {
		var it ext.Items
		if e.Type == "link" {
			it = ext.Parse(e.Value)
		} else {
			it = ext.Parse(e.Cache)
		}
		it = ext.WithPrefix(it, e.Prefix)
		if len(it.Links) == 0 && len(it.Clash) == 0 {
			continue
		}
		out = append(out, ExtItem{Name: e.Name, Links: it.Links, Clash: it.Clash})
	}
	return out
}

// sniFor 域名作 SNI;纯 IP 入口不发 SNI(自签 IP 证书场景)。
func sniFor(host string) string {
	if net.ParseIP(host) != nil {
		return ""
	}
	return host
}

// CertIsSelfSigned 读取数据面证书(certFile,回落 webCertFile)判断是否自签。
func CertIsSelfSigned(certFile, fallback string) bool {
	path := certFile
	if path == "" {
		path = fallback
	}
	if path == "" {
		return false
	}
	info := acme.Info(path)
	return info.Exists && info.SelfSigned
}

// EntriesFromNodes 由入口服务器表生成订阅入口。
//
// 连接地址:preferIP 时用 节点 Addr → 该服务器公网 IP → 域名(大陆 DNS 污染下客户端按域名解析会失败,
// 直接给 IP 最稳),SNI 仍用域名保证 TLS 正常;否则用域名。
// 多入口时每条线路按入口各出一个节点并加 "-名称" 后缀;倍率不为 1 时再加 " x2" 之类标记。
func EntriesFromNodes(db *gorm.DB, webDomain, localPublicIP string, preferIP bool) []Entry {
	var nodes []model.Node
	db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&nodes)
	var out []Entry
	for _, n := range nodes {
		domain := strings.TrimSpace(n.Domain)
		if domain == "" && n.IsLocal {
			domain = webDomain
		}
		ip := strings.TrimSpace(n.Addr)
		if ip == "" {
			if n.IsLocal {
				ip = localPublicIP
			} else {
				ip = n.PublicIP
			}
		}
		host := domain
		if preferIP && ip != "" {
			host = ip
		}
		if host == "" {
			continue
		}
		e := Entry{Name: n.Name, Host: host, SNI: sniFor(domain), NodeId: n.Id, Ratio: n.Ratio, Suffix: "-" + n.Name}
		out = append(out, e)
	}
	if len(out) == 0 {
		host := webDomain
		if preferIP && localPublicIP != "" {
			host = localPublicIP
		}
		return []Entry{{Host: host, SNI: sniFor(webDomain)}}
	}
	for i := range out {
		if len(out) == 1 {
			out[i].Suffix = ""
		}
		if out[i].Ratio > 0 && out[i].Ratio != 1 {
			out[i].Suffix += " x" + strconv.FormatFloat(out[i].Ratio, 'f', -1, 64)
		}
	}
	return out
}

// currentPath 当前订阅路径(设置 subPath,规整为 /xxx/);每个请求读取,改路径保存即生效。
func (s *Server) currentPath() string {
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
	return subPath
}

func (s *Server) Start() error {
	subPath := s.currentPath()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle())

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

func (s *Server) handle() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subPath := s.currentPath()
		if !strings.HasPrefix(r.URL.Path, subPath) {
			http.NotFound(w, r)
			return
		}
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
		opt.External = s.externalFor(user.Id)
		// 浏览器打开订阅地址 → 订阅页(用量/到期/一键导入/二维码);客户端拉取 → 原始订阅
		if !strings.EqualFold(s.setting("subPageEnabled"), "false") && WantsPage(r) {
			s.servePage(w, r, subPath, user, lines, opt)
			s.log(r, name, 200)
			return
		}
		format := r.URL.Query().Get("format")

		var res Result
		switch format {
		case "clash":
			out, err := BuildClashSub(user, lines, opt)
			if err != nil {
				s.log(r, name, 500)
				http.Error(w, "生成失败", http.StatusInternalServerError)
				return
			}
			res = out
		case "json", "sing-box", "singbox", "sfa":
			out, err := BuildSingBoxSub(user, lines, opt)
			if err != nil {
				s.log(r, name, 500)
				http.Error(w, "生成失败: "+err.Error(), http.StatusInternalServerError)
				return
			}
			res = out
		default:
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

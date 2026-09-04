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
	// OnShareChange 生成/取消临时共享后调用:热更新数据面用户;
	// kick 为真表示有旧凭据被作废(取消,或在已有共享上重新生成),需要断开该用户现有连接。
	OnShareChange func(user string, kick bool)
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
		ProfileTitle: pick(s.setting("subProfileTitle"), s.setting("subPageTitle")),
		UpdateHours:  s.settingInt("subUpdates", 12),
		Encode:       s.settingBool("subEncode"),
		ShowNotice:   s.settingBool("subShowNotice"),
		ClashTmpl:    s.setting("subClashExt"),
		Entries:      entries,
		Insecure:     insecure,
		TZ:           s.setting("timezone"),
		Share:        s.shareSelfService(),
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
	p := strings.Trim(strings.TrimSpace(s.setting("subPath")), "\"'")
	if strings.TrimSpace(p) == "" {
		p = "/sub/"
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
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

		// 用户名(主面板用户)→ 订阅令牌(代理建的用户)→ 临时共享令牌
		var user model.User
		shared := false
		byName := s.db.Where("name = ? AND COALESCE(reseller_id, 0) = 0 AND enabled = ? AND COALESCE(sub_token, '') = ''", name, true)
		if err := byName.First(&user).Error; err != nil {
			if s.db.Where("sub_token = ? AND enabled = ?", name, true).First(&user).Error != nil {
				if !s.shareEnabled() || s.db.Where("share_token = ? AND enabled = ?", name, true).First(&user).Error != nil {
					s.log(r, name, false, 404)
					s.serveNotFound(w, r, name) // 浏览器打开时给一页说明(多半是用完/到期被停用)
					return
				}
				shared = true // 共享地址:只发原始订阅,不出订阅页/二维码,也不能改共享状态
				if len(user.ShareCreds) == 0 {
					http.NotFound(w, r) // 老版本留下的令牌没有独立凭据,让用户重新生成
					return
				}
				user.Credentials = user.ShareCreds // 共享用单独凭据,取消即失效
			}
		}
		rs := s.resellerOf(user) // 代理用户:落地页文案与开关按代理的来
		if rs != nil && (!rs.Enabled || (rs.Expiry > 0 && rs.Expiry < time.Now().Unix())) {
			s.log(r, user.Name, shared, 404)
			s.serveNotFound(w, r, name) // 代理被停用或到期,名下用户的订阅一并停
			return
		}
		if shared && (wantQR || r.Method == http.MethodPost) {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			s.handleShare(w, r, subPath, name, user, rs)
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
		opt.Share = s.shareSelfService() && (rs == nil || rs.ShareOn)
		if rs != nil { // 代理填了标题就用代理的,客户端里显示的就是他的品牌
			opt.ProfileTitle = pick(rs.ProfileTitle, pick(rs.PageTitle, opt.ProfileTitle))
		}
		// 浏览器打开订阅地址 → 订阅页(用量/到期/一键导入/二维码);客户端拉取 → 原始订阅
		if !shared && s.pageEnabled(rs) && WantsPage(r) {
			if r.URL.Query().Has("clients") { // 订阅页里那个下载箭头
				s.serveClients(w, r, subPath, name, s.pageTitle(rs, opt))
				s.log(r, user.Name, false, 200)
				return
			}
			s.servePage(w, r, subPath, name, user, lines, opt, rs)
			s.log(r, user.Name, false, 200)
			return
		}
		format := r.URL.Query().Get("format")

		var res Result
		switch format {
		case "clash":
			out, err := BuildClashSub(user, lines, opt)
			if err != nil {
				s.log(r, user.Name, shared, 500)
				http.Error(w, "生成失败", http.StatusInternalServerError)
				return
			}
			res = out
		case "json", "sing-box", "singbox", "sfa":
			out, err := BuildSingBoxSub(user, lines, opt)
			if err != nil {
				s.log(r, user.Name, shared, 500)
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
		s.log(r, user.Name, shared, 200)
	}
}

// resellerOf 取用户所属代理;主面板直属用户返回 nil。
func (s *Server) resellerOf(u model.User) *model.Reseller {
	if u.ResellerId == 0 {
		return nil
	}
	var rs model.Reseller
	if s.db.First(&rs, u.ResellerId).Error != nil {
		return nil
	}
	return &rs
}

// pageEnabled 订阅页开关:主面板关掉就都不出;代理可以只关自己的。
func (s *Server) pageEnabled(rs *model.Reseller) bool {
	if strings.EqualFold(s.setting("subPageEnabled"), "false") {
		return false
	}
	return rs == nil || rs.PageEnabled
}

// truncate 按字节截断(只用于落库的诊断字段,不追求字符边界)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// log 记录订阅访问,供面板按用户汇总(替代 nginx 日志 + 汇总脚本)。
func (s *Server) log(r *http.Request, user string, shared bool, status int) {
	ip := r.Header.Get("X-Forwarded-For")
	if ip != "" {
		ip = strings.TrimSpace(strings.Split(ip, ",")[0])
	} else {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "link"
		if !shared && WantsPage(r) { // 共享地址从不出页面
			format = "page"
		}
	}
	if shared {
		format += "-share" // 后台一眼分辨是共享地址拉的
	}
	entry := model.SubLog{
		Ts: time.Now().Unix(), User: user, Ip: ip,
		Ua: truncate(r.UserAgent(), 200), Format: format, Status: status, // UA 是请求头,能塞很大
	}
	if err := s.db.Create(&entry).Error; err != nil {
		logger.Warning("写订阅日志失败: ", err)
	}
}

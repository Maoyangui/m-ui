package certutil

import (
	"crypto/tls"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

// reloadEvery 握手时最多隔这么久看一次证书文件;正用临时证书顶着时也按这个间隔重试原来的文件(测试里调短)。
var reloadEvery = 5 * time.Second

// errNoCert 连临时证书都没生成出来(系统随机数源坏了):握手失败,但进程照样不退。
var errNoCert = errors.New("没有可用的证书")

// Reloader 让 TLS 服务在证书文件更新后自动换用新证书(续期无需重启)。
// 每次握手最多每 5 秒检查一次文件修改时间,变化时重新加载;加载失败保留旧证书。
type Reloader struct {
	certPath, keyPath string
	mu                sync.Mutex
	cert              *tls.Certificate
	modTime           time.Time
	checkedAt         time.Time
	// temp 配置的证书读不出来(文件不在、私钥配不上……),正用一张临时自签证书顶着;tempErr 是最近一次读的错误,
	// since 是从什么时候开始顶的。文件读出来以后换回去,temp 清掉。
	temp    bool
	tempErr error
	since   time.Time
}

func NewReloader(certPath, keyPath string) (*Reloader, error) {
	r := &Reloader{certPath: certPath, keyPath: keyPath}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// NewReloaderOrTemp 同 NewReloader,但证书读不出来时不报错:先用一张只在内存里的临时自签证书顶上,照样走 HTTPS
// (浏览器会提示不受信任),之后按间隔再试原来的文件,读出来了就换回去,不用重启。
// 面板、订阅、代理面板用它:换机还原后证书文件不在,以前整个进程起不来,systemd 一直重启,面板再也打不开。
// hosts 写进临时证书(空的跳过;一个都没有时用 localhost)。Temp 报告现在是不是在顶着、为什么。
func NewReloaderOrTemp(certPath, keyPath string, hosts ...string) *Reloader {
	r := &Reloader{certPath: certPath, keyPath: keyPath}
	err := r.load()
	if err == nil {
		return r
	}
	r.temp, r.tempErr, r.since, r.checkedAt = true, err, time.Now(), time.Now()
	if c, gerr := tempCert(hosts); gerr == nil {
		r.cert = c
	} else {
		logger.Error("生成临时自签证书失败: ", gerr)
	}
	return r
}

// tempCert 临时自签证书:只在内存里,30 天(进程重启会重新生成)。
func tempCert(hosts []string) (*tls.Certificate, error) {
	var list []string
	seen := map[string]bool{}
	for _, h := range hosts {
		for _, v := range strings.Split(h, ",") {
			if v = strings.Trim(strings.TrimSpace(v), "[]"); v != "" && !seen[v] {
				seen[v] = true
				list = append(list, v)
			}
		}
	}
	if len(list) == 0 {
		list = []string{"localhost"}
	}
	certPEM, keyPEM, err := selfSignedPEM(list, 30, "m-ui temporary")
	if err != nil && list[0] != "localhost" { // 设置里的域名写坏了(进不了证书):至少用 localhost 顶上
		certPEM, keyPEM, err = selfSignedPEM([]string{"localhost"}, 30, "m-ui temporary")
	}
	if err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Reloader) load() error {
	c, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return err
	}
	r.cert = &c
	if st, err := os.Stat(r.certPath); err == nil {
		r.modTime = st.ModTime()
	}
	r.checkedAt = time.Now()
	return nil
}

// refresh 隔 reloadEvery 看一次文件:顶着临时证书时直接重读,读出来就换回去;平时看修改时间,变了才重读
// (读失败保留旧证书)。调用方持锁。
func (r *Reloader) refresh() {
	if time.Since(r.checkedAt) <= reloadEvery {
		return
	}
	r.checkedAt = time.Now()
	if r.temp {
		if err := r.load(); err != nil {
			r.tempErr = err
			return
		}
		r.temp, r.tempErr = false, nil
		logger.Info("证书 ", r.certPath, " 读出来了,临时自签证书已换下")
		return
	}
	if st, err := os.Stat(r.certPath); err == nil && !st.ModTime().Equal(r.modTime) {
		_ = r.load() // 失败保留旧证书
	}
}

// GetCertificate 供 tls.Config.GetCertificate 使用。
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	if r.cert == nil {
		return nil, errNoCert
	}
	return r.cert, nil
}

// Temp 正用临时自签证书顶着时返回原因(配置的证书文件为什么读不出来),用的是配置的证书时返回 nil。
// 到了间隔会顺带再试一次文件:没人连进来,页面上的提示也能在文件放回去之后消失。
func (r *Reloader) Temp() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	if !r.temp {
		return nil
	}
	return r.tempErr
}

// TLSConfig 返回使用该 Reloader 的 tls.Config。
func (r *Reloader) TLSConfig() *tls.Config {
	return &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12}
}

// Watch 记着面板、订阅、代理面板各自用的证书加载器:哪个正用临时自签证书顶着,页面顶部据此提示。
// 零值可用;nil 的 Watch 什么都不做(单独起订阅服务的测试不用管它)。
type Watch struct {
	mu sync.Mutex
	m  map[string]*Reloader
	// OnTemp 登记进来的加载器正顶着临时证书时调一次(另起 goroutine):runner 用它发 Telegram。只在启动前设置。
	OnTemp func(TempCert)
}

// TempCert 一个正用临时自签证书顶着的服务。
type TempCert struct {
	Service  string // panel / sub / reseller
	CertFile string
	KeyFile  string
	Err      string // 配置的证书为什么读不出来
	Since    time.Time
}

// Set 登记某个服务(panel / sub / reseller)现在用的加载器;r 为 nil 表示它没走 HTTPS 或已经停了。
func (w *Watch) Set(service string, r *Reloader) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if r == nil {
		delete(w.m, service)
		w.mu.Unlock()
		return
	}
	if w.m == nil {
		w.m = map[string]*Reloader{}
	}
	w.m[service] = r
	on := w.OnTemp
	w.mu.Unlock()
	if on != nil {
		if tc, ok := r.tempInfo(service); ok {
			go on(tc)
		}
	}
}

// serviceRank 提示里服务的顺序:面板、订阅、代理面板。
var serviceRank = map[string]int{"panel": 0, "sub": 1, "reseller": 2}

// Temps 正用临时证书顶着的服务(面板、订阅、代理面板的顺序)。
func (w *Watch) Temps() []TempCert {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	names := make([]string, 0, len(w.m))
	rs := make(map[string]*Reloader, len(w.m))
	for name, r := range w.m {
		names = append(names, name)
		rs[name] = r
	}
	w.mu.Unlock()
	sort.Slice(names, func(i, j int) bool {
		ri, iok := serviceRank[names[i]]
		rj, jok := serviceRank[names[j]]
		if iok != jok {
			return iok
		}
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	var out []TempCert
	for _, name := range names {
		if tc, ok := rs[name].tempInfo(name); ok {
			out = append(out, tc)
		}
	}
	return out
}

// tempInfo 正顶着临时证书时给出它的情况。
func (r *Reloader) tempInfo(service string) (TempCert, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	if !r.temp {
		return TempCert{}, false
	}
	msg := ""
	if r.tempErr != nil {
		msg = r.tempErr.Error()
	}
	return TempCert{Service: service, CertFile: r.certPath, KeyFile: r.keyPath, Err: msg, Since: r.since}, true
}

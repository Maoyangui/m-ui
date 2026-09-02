package certutil

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// Reloader 让 TLS 服务在证书文件更新后自动换用新证书(续期无需重启)。
// 每次握手最多每 5 秒检查一次文件修改时间,变化时重新加载;加载失败保留旧证书。
type Reloader struct {
	certPath, keyPath string
	mu                sync.Mutex
	cert              *tls.Certificate
	modTime           time.Time
	checkedAt         time.Time
}

func NewReloader(certPath, keyPath string) (*Reloader, error) {
	r := &Reloader{certPath: certPath, keyPath: keyPath}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
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

// GetCertificate 供 tls.Config.GetCertificate 使用。
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checkedAt) > 5*time.Second {
		r.checkedAt = time.Now()
		if st, err := os.Stat(r.certPath); err == nil && !st.ModTime().Equal(r.modTime) {
			_ = r.load() // 失败保留旧证书
		}
	}
	return r.cert, nil
}

// TLSConfig 返回使用该 Reloader 的 tls.Config。
func (r *Reloader) TLSConfig() *tls.Config {
	return &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12}
}

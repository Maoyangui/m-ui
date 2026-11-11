package runner

import (
	"fmt"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/notify"
)

// 面板、订阅、代理面板的证书读不出来(换机还原后文件不在、私钥配不上……)时不让整个进程退出:各自先用临时自签证书顶着,
// 照样走 HTTPS(certutil.NewReloaderOrTemp),文件放回原处几秒内自动换上。这里把"谁在顶着"接进页面顶部的证书提示与 Telegram。
// 线路的证书读不出来照旧:数据面起不来、报错(重载失败的提示条),面板与订阅不受影响。

// TLSWatch 面板 / 代理面板登记各自证书加载器的地方(订阅服务由 runner 自己登记)。
func (r *Runner) TLSWatch() *certutil.Watch { return &r.tls }

var tempServiceLabel = map[string]string{"panel": "面板", "sub": "订阅", "reseller": "代理面板"}

// noticeTempCert 某个服务顶上了临时证书:Telegram 说一声(换机还原后最常见,管理员未必马上打开面板)。
// 同一个服务、同一个文件 12 小时内只说一次,重启几次不刷屏。
func (r *Runner) noticeTempCert(tc certutil.TempCert) {
	if r.notifier == nil || !r.notifier.OnceDurable("tlstemp:"+tc.Service+":"+tc.CertFile, 12*time.Hour) {
		return
	}
	label := tempServiceLabel[tc.Service]
	if label == "" {
		label = tc.Service
	}
	extra := ""
	if tc.Service == "sub" {
		extra = ",校验证书的订阅客户端会更新失败"
	}
	r.notifier.Event("tgOnCert", fmt.Sprintf("🔴 <b>%s证书读不出来</b>:%s(%s)。现在先用临时自签证书顶着,仍走 HTTPS,浏览器会提示证书不受信任%s。"+
		"到证书页换一张证书,或把文件放回原处(放回后几秒内自动换上)", label, notify.Esc(tc.CertFile), notify.Esc(tc.Err), extra))
}

// tempCertAlerts 正用临时证书顶着的服务,按证书文件合并成提示(面板和代理面板常用同一张)。Trouble 记 missing:
// 只认这几种的页面照样显示"文件读不出来";Fallback 列出顶着的服务,证书提示据此说清楚是谁在用临时证书。
func (r *Runner) tempCertAlerts() ([]*CertAlert, map[string]*CertAlert) {
	var list []*CertAlert
	byFile := map[string]*CertAlert{}
	for _, tc := range r.tls.Temps() {
		if a := byFile[tc.CertFile]; a != nil {
			a.Fallback += "," + tc.Service
			continue
		}
		a := &CertAlert{Name: tc.CertFile, Trouble: renewMissing, Fallback: tc.Service, Error: tc.Err}
		byFile[tc.CertFile] = a
		list = append(list, a)
	}
	return list, byFile
}

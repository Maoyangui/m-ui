package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// NoticeFile 还原结果里要管理员知道的那部分(见 Notice),写在数据目录里。
const NoticeFile = "restore-notice.json"

// Notice 一次还原里要管理员知道的事,面板顶部提示一次,点「知道了」删掉。面板上传的还原在重启时应用,结果以前只进启动日志;
// m-ui restore 的输出关掉终端也就没了。放进数据目录的证书副本没有谁续,到期前要换掉,得让人进面板第一眼就看到。
type Notice struct {
	At      int64        `json:"at"`
	Copies  []NoticeCopy `json:"copies,omitempty"`  // 数据目录外的证书放进了 cert/restored/(只列证书,不列私钥)
	Missing []string     `json:"missing,omitempty"` // 面板 / 订阅 / 代理面板 / 线路要用、备份里没有本机也没有的文件
	Failed  []string     `json:"failed,omitempty"`  // 该写却没写成的(原因在启动日志、m-ui restore 的输出里)
}

// NoticeCopy 一张放进数据目录的证书副本。
type NoticeCopy struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Names    []string `json:"names,omitempty"`
	NotAfter int64    `json:"notAfter,omitempty"`
}

// notice 报告里要管理员知道的:放进数据目录的证书副本、还缺的、没写成的;都没有时为 nil。
func (r Report) notice(now time.Time) *Notice {
	n := &Notice{At: now.Unix()}
	for _, m := range r.Moved {
		if len(m.Names) > 0 { // 证书才列,私钥不用管到期
			n.Copies = append(n.Copies, NoticeCopy{From: m.From, To: m.To, Names: m.Names, NotAfter: m.NotAfter.Unix()})
		}
	}
	for _, m := range r.Missing {
		n.Missing = append(n.Missing, m.Path)
	}
	for _, f := range r.Failed {
		n.Failed = append(n.Failed, f.Path)
	}
	if len(n.Copies)+len(n.Missing)+len(n.Failed) == 0 {
		return nil
	}
	return n
}

func noticePath(dataDir string) string { return filepath.Join(dataDir, NoticeFile) }

// saveNotice 记下这次还原要说的;n 为 nil 时删掉上一次留下的(这次没什么要说的,别再提示上一次的)。
func saveNotice(dataDir string, n *Notice) error {
	if n == nil {
		return ClearNotice(dataDir)
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return os.WriteFile(noticePath(dataDir), b, 0o600)
}

// ReadNotice 上次还原留下、还没点「知道了」的提示;没有(或读不出来)时为 nil。
func ReadNotice(dataDir string) *Notice {
	b, err := os.ReadFile(noticePath(dataDir))
	if err != nil {
		return nil
	}
	var n Notice
	if json.Unmarshal(b, &n) != nil || len(n.Copies)+len(n.Missing)+len(n.Failed) == 0 {
		return nil
	}
	return &n
}

// ClearNotice 管理员看过了:删掉提示文件(本来就没有不算错)。
func ClearNotice(dataDir string) error {
	if err := os.Remove(noticePath(dataDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

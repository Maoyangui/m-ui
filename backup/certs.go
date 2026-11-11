package backup

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// 证书怎么进备份、还原时怎么回来。
//
// 备份:设置里引用的(线路默认、面板、订阅、代理面板,见 CertSettings)和证书库里的每一张,按原机绝对路径打进 zip
// (符号链接读的是它指向的内容,certbot 的 live/ 全是链接)。
//
// 还原(meta.json 里的路径是不可信输入,数据目录外一个文件都不写):
//   - 数据目录里的普通文件:写回原路径,还原就是回到备份那一刻;
//   - 数据目录外的(「已有证书」的 certbot / 宝塔 / nginx 文件、旧面板导入的路径),以及数据目录里的符号链接:
//     本机那个文件能接着用(是同一张、不比备份旧、和私钥配得上)就不动它 —— 同一台机器上还原时 certbot 续期照常生效;
//     不能用才写备份里那份:外部的放进 <数据目录>/cert/restored/,设置与证书库改指过去;链接换成文件,不顺着链接写到外面。
//   - 证书与私钥成对处理:一个要换,另一个跟着换,免得拼出一对配不上的。
// 老版本做的备份格式相同,照样这样还原;老版本还原新备份:数据目录外的照旧跳过(只打一行 Warning),不会失败。

// CertSettings 设置里引用证书 / 私钥文件的键,成对(证书, 私钥):线路默认、面板、订阅、代理面板。
var CertSettings = [][2]string{
	{"certFile", "keyFile"},
	{"webCertFile", "webKeyFile"},
	{"subCertFile", "subKeyFile"},
	{"resellerCertFile", "resellerKeyFile"},
}

// certRoles 还原报告里怎么称呼这些用途(按证书的键)。
var certRoles = map[string]string{"certFile": "线路默认证书", "webCertFile": "面板 HTTPS", "subCertFile": "订阅 HTTPS", "resellerCertFile": "代理面板 HTTPS"}

// RestoredDir 数据目录外的证书还原到数据目录下的这个子目录(cert/ 下面板自己的证书按 *.crt 收进证书库,这里不掺和)。
const RestoredDir = "restored"

func certSettingKeys() []string {
	keys := make([]string, 0, 2*len(CertSettings))
	for _, p := range CertSettings {
		keys = append(keys, p[0], p[1])
	}
	return keys
}

// certSettingsOf 读出 CertSettings 里各键的值(去掉首尾空白)。
func certSettingsOf(db *gorm.DB) (map[string]string, error) {
	var rows []struct{ Key, Value string }
	if err := db.Table("settings").Select("key, value").Where("key IN ?", certSettingKeys()).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Key] = strings.TrimSpace(r.Value)
	}
	return out, nil
}

func hasTable(db *gorm.DB, name string) bool {
	var n int64
	db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	return n > 0
}

// storeCerts 证书库里每一张的证书与私钥路径(老库没有证书库时为空)。
func storeCerts(db *gorm.DB) ([][2]string, error) {
	if !hasTable(db, "certs") {
		return nil, nil
	}
	var rows []struct{ CertFile, KeyFile string }
	if err := db.Table("certs").Select("cert_file, key_file").Order("id asc").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{strings.TrimSpace(r.CertFile), strings.TrimSpace(r.KeyFile)})
	}
	return out, nil
}

// CertPaths 库里引用的证书与私钥文件:设置里的(CertSettings)和证书库里的每一张(没在用的也算),去重。备份打包的就是这些。
func CertPaths(db *gorm.DB) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	set, _ := certSettingsOf(db)
	for _, k := range certSettingKeys() {
		add(set[k])
	}
	rows, _ := storeCerts(db)
	for _, r := range rows {
		add(r[0])
		add(r[1])
	}
	return out
}

// Report 一次还原做了什么。m-ui restore 照实打印;面板上传的还原在重启时应用,写进启动日志。
type Report struct {
	DB       bool       // 数据库已经换成备份里的了
	Bak      string     // 原来的库改名成了这个(之前没有库时为空)
	Restored []string   // 写回原路径的证书(数据目录里的)
	Moved    []CertMove // 原在数据目录外、本机没有能用的同一张:放进了 <数据目录>/cert/restored/,设置与证书库已改指
	Skipped  []CertNote // 没从备份里写出来的,和原因(多半是本机已有能用的同一张,继续用它)
	Failed   []CertNote // 该写却没写成的
	Missing  []CertNote // 还原后面板 / 订阅 / 代理面板 / 线路正用着、本机却没有的证书文件
}

// CertMove 一个数据目录外的证书文件还原到了哪。
type CertMove struct{ From, To string }

// CertNote 一个证书文件和一句说明。
type CertNote struct{ Path, Reason string }

// ReportLine 报告里的一行;Warn 为真的是要管理员处理的。
type ReportLine struct {
	Text string
	Warn bool
}

// Lines 报告的文字,一行一句。
func (r Report) Lines() []ReportLine {
	var out []ReportLine
	info := func(s string) { out = append(out, ReportLine{Text: s}) }
	warn := func(s string) { out = append(out, ReportLine{Text: s, Warn: true}) }
	n := len(r.Restored) + len(r.Moved)
	switch {
	case n+len(r.Skipped)+len(r.Failed) == 0:
		info("证书:备份里没有证书文件")
	default:
		head := fmt.Sprintf("证书:已还原 %d 个,跳过 %d 个", n, len(r.Skipped))
		if len(r.Moved) > 0 {
			head += fmt.Sprintf("(已还原的里有 %d 个原在数据目录外,放进了数据目录)", len(r.Moved))
		}
		if len(r.Failed) > 0 {
			head += fmt.Sprintf(",没写成 %d 个", len(r.Failed))
		}
		info(head)
	}
	for _, p := range r.Restored {
		info("  写回原路径:" + p)
	}
	for _, m := range r.Moved {
		info("  " + m.From + " 在数据目录外、本机没有能用的同一张:已放到 " + m.To + ",设置已改指这里")
	}
	for _, s := range r.Skipped {
		info("  跳过 " + s.Path + ":" + s.Reason)
	}
	if len(r.Moved) > 0 {
		info("  放进数据目录的是备份那一刻的副本:原来由 certbot 等工具续期的,以后不会再跟着更新,到期前在证书页重新签发,或者把服务器上的证书重新添加进来")
	}
	for _, f := range r.Failed {
		warn("  没写成 " + f.Path + ":" + f.Reason)
	}
	for _, m := range r.Missing {
		warn("注意:" + m.Path + " " + m.Reason)
	}
	return out
}

// certRefs 还原后的库里引用了哪些证书文件。
type certRefs struct {
	pairs [][2]string     // (证书, 私钥):设置里成对的、证书库的每一行
	keys  map[string]bool // 当私钥用的
	used  map[string]bool // 引用到的全部
}

func readCertRefs(db *gorm.DB) (certRefs, error) {
	refs := certRefs{keys: map[string]bool{}, used: map[string]bool{}}
	set, err := certSettingsOf(db)
	if err != nil {
		return refs, err
	}
	for _, p := range CertSettings {
		refs.add(set[p[0]], set[p[1]])
	}
	rows, err := storeCerts(db)
	if err != nil {
		return refs, err
	}
	for _, r := range rows {
		refs.add(r[0], r[1])
	}
	return refs, nil
}

func (r *certRefs) add(cert, key string) {
	if cert != "" {
		r.used[cert] = true
	}
	if key != "" {
		r.used[key] = true
		r.keys[key] = true
	}
	if cert != "" && key != "" {
		r.pairs = append(r.pairs, [2]string{cert, key})
	}
}

// isPrivateKey 私钥按 0600 写:库里当私钥用的、.key 结尾的、内容是 PEM 私钥的(certbot 的 privkey.pem)。
func isPrivateKey(path string, data []byte, refs certRefs) bool {
	return refs.keys[path] || strings.HasSuffix(path, ".key") || bytes.Contains(data, []byte("PRIVATE KEY-----"))
}

// leafOf PEM 里的第一张证书。
func leafOf(b []byte) *x509.Certificate {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil
		}
		if blk.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil
			}
			return c
		}
	}
}

// certNames 证书写着的域名与 IP(都没有时用 CN),排好序。
func certNames(c *x509.Certificate) []string {
	var out []string
	for _, d := range c.DNSNames {
		out = append(out, strings.ToLower(d))
	}
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	if len(out) == 0 && c.Subject.CommonName != "" {
		out = append(out, strings.ToLower(c.Subject.CommonName))
	}
	sort.Strings(out)
	return out
}

// usableLocal 本机 path 上的文件能不能接着用、不换成备份里那份:是个读得出的文件(符号链接跟过去看),
// 证书要和备份里那张写着同样的域名 / IP,而且不比它旧(同一台机器上 certbot 续过的就比备份新);
// 同一路径上放着别的站点的证书、或者更旧的一张,都不算。私钥看不出是不是"同一把",由成对校验兜着。
func usableLocal(path string, backup []byte) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if bytes.Contains(backup, []byte("PRIVATE KEY-----")) {
		return true
	}
	want := leafOf(backup)
	if want == nil {
		return true // 备份里那份本来就不是证书,没法比:用本机的
	}
	local, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if bytes.Equal(local, backup) {
		return true
	}
	have := leafOf(local)
	if have == nil {
		return false
	}
	a, b := certNames(have), certNames(want)
	return strings.Join(a, ",") == strings.Join(b, ",") && !have.NotAfter.Before(want.NotAfter)
}

// pairOK 本机这对证书与私钥配得上。
func pairOK(cert, key string) bool {
	_, err := tls.LoadX509KeyPair(cert, key)
	return err == nil
}

// restoredName 数据目录外的证书在 cert/restored/ 里叫什么:上一级目录名-文件名(certbot 的 live/<域名>/fullchain.pem
// → <域名>-fullchain.pem,一眼看出是哪张),只留字母、数字和 . _ -,不以点开头。
func restoredName(orig string) string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		out := strings.TrimLeft(b.String(), ".")
		if len(out) > 100 {
			out = out[len(out)-100:]
		}
		return out
	}
	parts := strings.Split(strings.Trim(strings.ReplaceAll(orig, `\`, "/"), "/"), "/")
	name := clean(parts[len(parts)-1])
	if name == "" {
		name = "cert"
	}
	if len(parts) > 1 {
		if dir := clean(parts[len(parts)-2]); dir != "" {
			name = dir + "-" + name
		}
	}
	return name
}

// namer 在 cert/restored/ 里给文件挑名字:同一次还原里撞名(两张都叫 fullchain.pem 又在同名目录下)、
// 或者那里已经有内容不同的文件(以前还原留下的,旧库可能还指着它)时,依次加 -2、-3,不覆盖;内容相同的直接复用。
type namer struct {
	dir   string
	taken map[string]bool
}

func (n *namer) pick(orig string, data []byte) (string, error) {
	base := restoredName(orig)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		p := filepath.Join(n.dir, name)
		if n.taken[p] {
			continue
		}
		if st, err := os.Lstat(p); err == nil {
			if !st.Mode().IsRegular() {
				continue
			}
			if old, err := os.ReadFile(p); err != nil || !bytes.Equal(old, data) {
				continue
			}
		}
		n.taken[p] = true
		return p, nil
	}
	return "", errors.New("同名文件太多")
}

// writeCertFile 原子地写一个证书文件:先写同目录的临时文件再改名。目标是符号链接时换掉的是链接本身,
// 不会顺着链接写到别处(数据目录里的链接可能指向数据目录外)。私钥 0600,证书 0644。
func writeCertFile(path string, data []byte, key bool) error {
	mode := os.FileMode(0o644)
	if key {
		mode = 0o600
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".restore-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Chmod(mode); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// certEntry 备份里的一个证书文件。
type certEntry struct {
	path   string
	data   []byte
	inRoot bool // 原路径在数据目录里
	link   bool // 原路径在数据目录里,本机那里现在是个符号链接
}

// restoreCerts 把备份里的证书放回去(规则见文件开头),改写库里指向数据目录外、已经换了位置的路径。
// db 是刚写好的、还原后的库(打不开时为 nil,dbErr 是原因)。结果记在 rep 里。
func restoreCerts(db *gorm.DB, dbErr error, root string, entries []certEntry, rep *Report) {
	fail := func(p, why string) { rep.Failed = append(rep.Failed, CertNote{Path: p, Reason: why}) }
	var refs certRefs
	known := false
	if dbErr == nil {
		var err error
		if refs, err = readCertRefs(db); err == nil {
			known = true
		} else {
			dbErr = err
		}
	}
	if !known {
		refs = certRefs{keys: map[string]bool{}, used: map[string]bool{}}
	}

	// 数据目录里的普通文件:写回原路径。其余的(数据目录外的、数据目录里的链接)先看本机的能不能接着用。
	cand := map[string]*certEntry{}
	for i := range entries {
		e := &entries[i]
		if e.inRoot && !e.link {
			if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
				fail(e.path, "创建目录: "+err.Error())
			} else if err := writeCertFile(e.path, e.data, isPrivateKey(e.path, e.data, refs)); err != nil {
				fail(e.path, err.Error())
			} else {
				rep.Restored = append(rep.Restored, e.path)
			}
			continue
		}
		if !e.inRoot && known && !refs.used[e.path] {
			rep.Skipped = append(rep.Skipped, CertNote{Path: e.path, Reason: "库里没有用到这个文件"})
			continue
		}
		cand[e.path] = e
	}
	write := map[string]bool{}
	for p, e := range cand {
		if !usableLocal(p, e.data) {
			write[p] = true
		}
	}
	// 成对:一个要换,另一个跟着换;都不换的,本机这对也得配得上
	for changed := true; changed; {
		changed = false
		for _, pr := range refs.pairs {
			if cand[pr[0]] == nil && cand[pr[1]] == nil {
				continue
			}
			if !write[pr[0]] && !write[pr[1]] && pairOK(pr[0], pr[1]) {
				continue
			}
			for _, p := range pr {
				if cand[p] != nil && !write[p] {
					write[p], changed = true, true
				}
			}
		}
	}

	dir := filepath.Join(root, "cert", RestoredDir)
	names := &namer{dir: dir, taken: map[string]bool{}}
	moved := map[string]string{}
	for i := range entries {
		e := &entries[i]
		if cand[e.path] == nil {
			continue
		}
		key := isPrivateKey(e.path, e.data, refs)
		switch {
		case !write[e.path] && e.link:
			rep.Skipped = append(rep.Skipped, CertNote{Path: e.path, Reason: "本机这里是符号链接,指向的文件能用(同一张、不比备份旧),没有换掉"})
		case !write[e.path]:
			rep.Skipped = append(rep.Skipped, CertNote{Path: e.path, Reason: "本机已有这个文件,能用(同一张、不比备份旧),继续用它,没有覆盖"})
		case e.inRoot: // 数据目录里的符号链接,指向的文件用不了:换成备份里的文件
			if err := writeCertFile(e.path, e.data, key); err != nil {
				fail(e.path, err.Error())
			} else {
				rep.Restored = append(rep.Restored, e.path)
			}
		default:
			if err := os.MkdirAll(dir, 0o700); err != nil {
				fail(e.path, "创建 "+dir+": "+err.Error())
				continue
			}
			dst, err := names.pick(e.path, e.data)
			if err == nil {
				err = writeCertFile(dst, e.data, key)
			}
			if err != nil {
				fail(e.path, "放进 "+dir+": "+err.Error())
				continue
			}
			moved[e.path] = dst
			rep.Moved = append(rep.Moved, CertMove{From: e.path, To: dst})
		}
	}
	if len(moved) == 0 {
		return
	}
	if dbErr != nil {
		fail(dir, "证书已放进来,但读不出还原后的库,设置没能改指过去: "+dbErr.Error())
		return
	}
	if err := rewriteCertRefs(db, moved); err != nil {
		fail(dir, "证书已放进来,但改写库里的证书路径失败: "+err.Error())
	}
}

// rewriteCertRefs 设置(CertSettings 里的键)与证书库里指向 from 的路径一律改成 to。
func rewriteCertRefs(db *gorm.DB, moved map[string]string) error {
	certs := hasTable(db, "certs")
	keys := certSettingKeys()
	return db.Transaction(func(tx *gorm.DB) error {
		for from, to := range moved {
			if err := tx.Exec("UPDATE settings SET value = ? WHERE key IN ? AND TRIM(value) = ?", to, keys, from).Error; err != nil {
				return err
			}
			if !certs {
				continue
			}
			if err := tx.Exec("UPDATE certs SET cert_file = ? WHERE TRIM(cert_file) = ?", to, from).Error; err != nil {
				return err
			}
			if err := tx.Exec("UPDATE certs SET key_file = ? WHERE TRIM(key_file) = ?", to, from).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// missingCerts 还原后面板、订阅、代理面板、线路正用着、本机却没有的证书文件:备份里没有(备份时就读不到,
// 或者是没带这张的老备份),本机也没有。m-ui 照样起得来,但要管理员换一张。
func missingCerts(db *gorm.DB, unreadable []string) []CertNote {
	set, err := certSettingsOf(db)
	if err != nil {
		return nil
	}
	unread := map[string]bool{}
	for _, p := range unreadable {
		unread[p] = true
	}
	roles := map[string][]string{}
	var order []string
	for _, pr := range CertSettings {
		c, k := set[pr[0]], set[pr[1]]
		if c == "" || k == "" {
			continue
		}
		for _, p := range []string{c, k} {
			if _, err := os.Stat(p); err == nil {
				continue
			}
			if roles[p] == nil {
				order = append(order, p)
			}
			roles[p] = append(roles[p], pr[0])
		}
	}
	out := make([]CertNote, 0, len(order))
	for _, p := range order {
		var names []string
		lines, https := false, false
		for _, k := range roles[p] {
			names = append(names, certRoles[k])
			if k == "certFile" {
				lines = true
			} else {
				https = true
			}
		}
		why := "备份里没有这个文件"
		if unread[p] {
			why = "备份时就读不到这个文件"
		}
		var then []string
		if https {
			then = append(then, "面板 / 订阅 / 代理面板会先用临时自签证书顶着(仍走 HTTPS)")
		}
		if lines {
			then = append(then, "用这张证书的线路起不来")
		}
		out = append(out, CertNote{Path: p, Reason: "(" + strings.Join(names, "、") + "要用):" + why + ",本机也没有。m-ui 照样能启动," +
			strings.Join(then, ";") + ";到证书页换一张证书,或者把文件放回原处"})
	}
	return out
}

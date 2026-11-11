package backup

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// certbotPair 按 certbot 的样子放一张证书:archive/<域名>/fullchain1.pem、privkey1.pem 是文件,
// live/<域名>/fullchain.pem、privkey.pem 是指向它们的符号链接。返回 live/ 下的两个路径。
func certbotPair(t *testing.T, le, domain string, days int) (string, string) {
	t.Helper()
	arch, live := filepath.Join(le, "archive", domain), filepath.Join(le, "live", domain)
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	n := 1
	for {
		if _, err := os.Stat(filepath.Join(arch, "fullchain"+strconv.Itoa(n)+".pem")); err != nil {
			break
		}
		n++
	}
	ac, ak := filepath.Join(arch, "fullchain"+strconv.Itoa(n)+".pem"), filepath.Join(arch, "privkey"+strconv.Itoa(n)+".pem")
	if err := certutil.GenerateSelfSigned([]string{domain}, ac, ak, days); err != nil {
		t.Fatal(err)
	}
	c, k := filepath.Join(live, "fullchain.pem"), filepath.Join(live, "privkey.pem")
	for link, target := range map[string]string{c: ac, k: ak} {
		os.Remove(link)
		rel, _ := filepath.Rel(live, target)
		if err := os.Symlink(rel, link); err != nil {
			t.Fatal(err)
		}
	}
	return c, k
}

func openDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func setAll(t *testing.T, db *gorm.DB, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		if err := db.Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// backupTo 用 db 打一个备份 zip 放到 dir 下,返回路径。
func backupTo(t *testing.T, db *gorm.DB, dir string, certs []string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Create(db, certs, "1.2.3", &buf); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "backup.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// restoredState 还原后的库里,设置与证书库记着的证书路径。
func restoredState(t *testing.T, dbPath string) (map[string]string, []model.Cert) {
	t.Helper()
	db := openDB(t, dbPath)
	defer database.Close(db)
	var rows []model.Setting
	db.Find(&rows)
	set := map[string]string{}
	for _, r := range rows {
		set[r.Key] = r.Value
	}
	var certs []model.Cert
	db.Order("name asc").Find(&certs)
	return set, certs
}

var certKeys = []string{"certFile", "keyFile", "webCertFile", "webKeyFile", "subCertFile", "subKeyFile", "resellerCertFile", "resellerKeyFile"}

// 换机还原:「已有证书」(certbot 的 live/ 下是符号链接)和旧面板导入的证书路径都在数据目录外。新机上没有这些文件时,
// 以前只打一行 Warning 就跳过,设置仍指着不存在的文件:订阅、面板起不来,整个进程退出。
// 现在备份里的副本放进数据目录,设置与证书库改指过去;两张证书文件名一样(都叫 fullchain.pem)不能互相覆盖;
// 私钥 0600;数据目录外一个文件都不写。
func TestRestoreBringsBackExternalCerts(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90)
	bCert, bKey := certbotPair(t, le, "b.example.com", 90)
	aPEM, _ := os.ReadFile(aCert)
	bPEM, _ := os.ReadFile(bCert)
	db := openDB(t, filepath.Join(base, "old", DBName))
	setAll(t, db, map[string]string{
		"webCertFile": aCert, "webKeyFile": aKey, "subCertFile": aCert, "subKeyFile": aKey,
		"resellerCertFile": bCert, "resellerKeyFile": bKey, "certFile": bCert, "keyFile": bKey,
	})
	db.Create(&model.Cert{Name: "a.example.com", Source: "external", CertFile: aCert, KeyFile: aKey})
	db.Create(&model.Cert{Name: "b.example.com", Source: "external", CertFile: bCert, KeyFile: bKey})
	zipPath := backupTo(t, db, base, []string{bCert, bKey, aCert, aKey, bCert, bKey})
	database.Close(db)
	if err := os.RemoveAll(le); err != nil { // 新机上没有 certbot 的这些文件
		t.Fatal(err)
	}

	newRoot := filepath.Join(base, "new")
	dst := filepath.Join(newRoot, DBName)
	if _, err := Restore(dst, zipPath); err != nil {
		t.Fatal(err)
	}
	set, rows := restoredState(t, dst)
	for _, k := range certKeys {
		p := set[k]
		if !underDir(newRoot, p) {
			t.Fatalf("%s 还指着数据目录外的 %q(新机上没有这个文件)", k, p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s 指的 %s 没有写出来: %v", k, p, err)
		}
	}
	if b, _ := os.ReadFile(set["webCertFile"]); !bytes.Equal(b, aPEM) {
		t.Fatal("面板证书的内容不是备份里的那张")
	}
	if b, _ := os.ReadFile(set["resellerCertFile"]); !bytes.Equal(b, bPEM) {
		t.Fatal("两张证书都叫 fullchain.pem,互相覆盖了")
	}
	if set["webCertFile"] != set["subCertFile"] || set["webKeyFile"] != set["subKeyFile"] {
		t.Fatalf("同一个文件只该放一份: %v", set)
	}
	for _, k := range []string{"webKeyFile", "keyFile"} {
		if st, err := os.Stat(set[k]); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("私钥 %s 的权限应是 0600: %v %v", set[k], st.Mode(), err)
		}
	}
	for _, p := range [][2]string{{"webCertFile", "webKeyFile"}, {"certFile", "keyFile"}} {
		if _, err := tls.LoadX509KeyPair(set[p[0]], set[p[1]]); err != nil {
			t.Fatalf("%s 与 %s 配不上: %v", set[p[0]], set[p[1]], err)
		}
	}
	if len(rows) != 2 || rows[0].CertFile != set["webCertFile"] || rows[0].KeyFile != set["webKeyFile"] ||
		rows[1].CertFile != set["certFile"] || rows[1].KeyFile != set["keyFile"] {
		t.Fatalf("证书库没有一并改指: %+v / %v", rows, set)
	}
	if _, err := os.Lstat(le); !os.IsNotExist(err) {
		t.Fatal("还原往数据目录外写了文件")
	}
}

// 新机同一路径上已有文件,但那是另一张证书(别的站点的):不能拿它冒充,用备份里的那张,本机那个文件不动。
func TestRestoreDoesNotTakeAnotherCertAtSamePath(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90)
	aPEM, _ := os.ReadFile(aCert)
	db := openDB(t, filepath.Join(base, "old", DBName))
	setAll(t, db, map[string]string{"webCertFile": aCert, "webKeyFile": aKey, "subCertFile": aCert, "subKeyFile": aKey})
	zipPath := backupTo(t, db, base, []string{aCert, aKey})
	database.Close(db)
	os.RemoveAll(le)
	if err := os.MkdirAll(filepath.Dir(aCert), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := certutil.GenerateSelfSigned([]string{"other.example.net"}, aCert, aKey, 90); err != nil {
		t.Fatal(err)
	}
	other, _ := os.ReadFile(aCert)

	newRoot := filepath.Join(base, "new")
	if _, err := Restore(filepath.Join(newRoot, DBName), zipPath); err != nil {
		t.Fatal(err)
	}
	set, _ := restoredState(t, filepath.Join(newRoot, DBName))
	if b, _ := os.ReadFile(set["webCertFile"]); !bytes.Equal(b, aPEM) {
		t.Fatalf("面板用的应是备份里 a.example.com 那张,现在指着 %s", set["webCertFile"])
	}
	if b, _ := os.ReadFile(aCert); !bytes.Equal(b, other) {
		t.Fatal("本机原来那个文件被改了")
	}
}

// 同一台机器上还原(或新机已经把 /etc/letsencrypt 拷过来了):本机的文件能用、是同一张而且不比备份旧(certbot 续过),
// 就接着用本机的 —— 换成备份里的冻结副本,以后 certbot 续期就再也换不上了。
func TestRestoreKeepsUsableLocalCerts(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90)
	db := openDB(t, filepath.Join(base, "old", DBName))
	setAll(t, db, map[string]string{"webCertFile": aCert, "webKeyFile": aKey, "subCertFile": aCert, "subKeyFile": aKey})
	zipPath := backupTo(t, db, base, []string{aCert, aKey})
	database.Close(db)
	certbotPair(t, le, "a.example.com", 120) // 备份之后 certbot 续了一次:live/ 的链接改指 fullchain2.pem
	renewed, _ := os.ReadFile(aCert)

	newRoot := filepath.Join(base, "new")
	if _, err := Restore(filepath.Join(newRoot, DBName), zipPath); err != nil {
		t.Fatal(err)
	}
	set, _ := restoredState(t, filepath.Join(newRoot, DBName))
	if set["webCertFile"] != aCert || set["webKeyFile"] != aKey || set["subCertFile"] != aCert {
		t.Fatalf("本机的证书能用,设置不该改: %v", set)
	}
	if b, _ := os.ReadFile(aCert); !bytes.Equal(b, renewed) {
		t.Fatal("本机续过的证书被备份里的旧证书覆盖了")
	}
	if _, err := os.Stat(filepath.Join(newRoot, "cert", "restored")); !os.IsNotExist(err) {
		t.Fatal("本机的证书能用,不该再放一份副本")
	}
}

// 数据目录里的证书是指向外面的符号链接,而外面那个文件已经不是备份里的那张(和私钥配不上):
// 写回时换掉链接本身,不能顺着链接改写数据目录外的文件。
func TestRestoreDoesNotWriteThroughSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	real, realKey := filepath.Join(outside, "a.crt"), filepath.Join(outside, "a.key")
	if err := certutil.GenerateSelfSigned([]string{"a.example.com"}, real, realKey, 90); err != nil {
		t.Fatal(err)
	}
	link, key := filepath.Join(root, "cert", "a.example.com.crt"), filepath.Join(root, "cert", "a.example.com.key")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	kb, _ := os.ReadFile(realKey)
	if err := os.WriteFile(key, kb, 0o600); err != nil {
		t.Fatal(err)
	}
	aPEM, _ := os.ReadFile(real)
	dbPath := filepath.Join(root, DBName)
	db := openDB(t, dbPath)
	setAll(t, db, map[string]string{"certFile": link, "keyFile": key})
	zipPath := backupTo(t, db, base, []string{link, key})
	database.Close(db)
	// 之后外面那个文件换成了别的证书
	if err := certutil.GenerateSelfSigned([]string{"other.example.net"}, real, filepath.Join(outside, "other.key"), 90); err != nil {
		t.Fatal(err)
	}
	other, _ := os.ReadFile(real)

	if _, err := Restore(dbPath, zipPath); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(real); !bytes.Equal(b, other) {
		t.Fatal("顺着数据目录里的符号链接改写了数据目录外的文件")
	}
	if b, _ := os.ReadFile(link); !bytes.Equal(b, aPEM) {
		t.Fatal("数据目录里的证书没有还原成备份里的那张")
	}
	if _, err := tls.LoadX509KeyPair(link, key); err != nil {
		t.Fatalf("还原后证书与私钥配不上: %v", err)
	}
}

// 报告如实区分:写回原路径的、放进数据目录的、跳过的(和原因)、还缺的。证书库里没在用的外部证书也一并放回来;
// 库里没用到的外部文件不放;代理面板的证书也进备份。
func TestRestoreReport(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	root := filepath.Join(base, "data")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90) // 面板、订阅:新机上没有
	keep, keepKey := certbotPair(t, le, "keep.example.com", 90)
	inCert, inKey := filepath.Join(root, "cert", "in.example.com.crt"), filepath.Join(root, "cert", "in.example.com.key")
	if err := certutil.GenerateSelfSigned([]string{"in.example.com"}, inCert, inKey, 90); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(base, "stray.pem")
	os.WriteFile(stray, []byte("not referenced"), 0o644)
	gone := filepath.Join(base, "gone", "reseller.pem")
	dbPath := filepath.Join(root, DBName)
	db := openDB(t, dbPath)
	setAll(t, db, map[string]string{
		"webCertFile": aCert, "webKeyFile": aKey, "subCertFile": aCert, "subKeyFile": aKey,
		"certFile": inCert, "keyFile": inKey, "resellerCertFile": gone, "resellerKeyFile": gone + ".key",
	})
	db.Create(&model.Cert{Name: "keep.example.com", Source: "external", CertFile: keep, KeyFile: keepKey}) // 证书库里没在用的
	paths := CertPaths(db)
	for _, p := range []string{inCert, inKey, aCert, aKey, gone, gone + ".key", keep, keepKey} {
		found := false
		for _, q := range paths {
			found = found || q == p
		}
		if !found {
			t.Fatalf("备份要带上 %s: %v", p, paths)
		}
	}
	zipPath := backupTo(t, db, base, append(paths, stray))
	database.Close(db)
	os.RemoveAll(filepath.Join(le, "live", "a.example.com"))
	os.RemoveAll(filepath.Join(le, "archive", "a.example.com"))

	rep, err := Restore(dbPath, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DB || rep.Bak == "" {
		t.Fatalf("库应已还原、旧库留着: %+v", rep)
	}
	if len(rep.Restored) != 2 || len(rep.Moved) != 2 || len(rep.Failed) != 0 {
		t.Fatalf("写回 2 个(数据目录里的)、放进数据目录 2 个(面板 / 订阅那对): %+v", rep)
	}
	reasons := map[string]string{}
	for _, s := range rep.Skipped {
		reasons[s.Path] = s.Reason
	}
	if len(rep.Skipped) != 3 || !strings.Contains(reasons[keep], "本机已有这个文件") || !strings.Contains(reasons[keepKey], "本机已有这个文件") ||
		reasons[stray] != "库里没有用到这个文件" {
		t.Fatalf("跳过的和原因不对: %+v", rep.Skipped)
	}
	if len(rep.Missing) != 2 || rep.Missing[0].Path != gone || !strings.Contains(rep.Missing[0].Reason, "代理面板 HTTPS要用") ||
		!strings.Contains(rep.Missing[0].Reason, "备份时就读不到这个文件") {
		t.Fatalf("还缺的代理面板证书要说清楚: %+v", rep.Missing)
	}
	var text []string
	for _, l := range rep.Lines() {
		text = append(text, l.Text)
	}
	all := strings.Join(text, "\n")
	for _, s := range []string{"证书:已还原 4 个,跳过 3 个(已还原的里有 2 个原在数据目录外,放进了数据目录)", "写回原路径:" + inCert,
		"跳过 " + stray + ":库里没有用到这个文件", "注意:" + gone} {
		if !strings.Contains(all, s) {
			t.Fatalf("报告里没有 %q:\n%s", s, all)
		}
	}
	if st, err := os.Stat(inKey); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("数据目录里的私钥写回后也是 0600: %v %v", st.Mode(), err)
	}
}

// 同一个备份还原两次:放进数据目录的文件沿用同一个名字(内容相同),不越积越多;那个名字上已经有内容不同的文件
// (以前还原留下、旧库可能还指着它)时另起名字,不覆盖。
func TestRestoreNamesAreStable(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90)
	db := openDB(t, filepath.Join(base, "old", DBName))
	setAll(t, db, map[string]string{"webCertFile": aCert, "webKeyFile": aKey})
	zipPath := backupTo(t, db, base, []string{aCert, aKey})
	database.Close(db)
	os.RemoveAll(le)
	root := filepath.Join(base, "new")
	dst := filepath.Join(root, DBName)
	r1, err := Restore(dst, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Restore(dst, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Moved) != 2 || len(r2.Moved) != 2 || r1.Moved[0].To != r2.Moved[0].To || r1.Moved[1].To != r2.Moved[1].To {
		t.Fatalf("同一个备份两次还原应放在同一个地方: %+v / %+v", r1.Moved, r2.Moved)
	}
	if want := filepath.Join(root, "cert", "restored", "a.example.com-fullchain.pem"); r1.Moved[0].To != want {
		t.Fatalf("放进数据目录的名字应是 <域名>-<文件名>: %s", r1.Moved[0].To)
	}
	os.WriteFile(r1.Moved[0].To, []byte("别的内容"), 0o644)
	r3, err := Restore(dst, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "cert", "restored", "a.example.com-fullchain-2.pem"); len(r3.Moved) != 2 || r3.Moved[0].To != want {
		t.Fatalf("同名文件内容不同时另起名字: %+v", r3.Moved)
	}
	if b, _ := os.ReadFile(r1.Moved[0].To); string(b) != "别的内容" {
		t.Fatal("不该覆盖已有的文件")
	}
	set, _ := restoredState(t, dst)
	if set["webCertFile"] != r3.Moved[0].To || set["webKeyFile"] != r1.Moved[1].To {
		t.Fatalf("设置应改指新放的文件: %v", set)
	}
}

// 老版本做的备份(格式一样,没有代理面板的证书,meta 里也没有 certsUnreadable):照样还原,外部证书照样放回来,
// 没带的代理面板证书在报告里说清楚。
func TestRestoreOldBackup(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90)
	rCert, rKey := certbotPair(t, le, "r.example.com", 90)
	db := openDB(t, filepath.Join(base, "old", DBName))
	setAll(t, db, map[string]string{"webCertFile": aCert, "webKeyFile": aKey, "resellerCertFile": rCert, "resellerKeyFile": rKey})
	snap := filepath.Join(base, "snap.db")
	if err := db.Exec("VACUUM INTO '" + strings.ReplaceAll(filepath.ToSlash(snap), "'", "''") + "'").Error; err != nil {
		t.Fatal(err)
	}
	database.Close(db)
	dbBytes, _ := os.ReadFile(snap)
	aPEM, _ := os.ReadFile(aCert)
	aKeyPEM, _ := os.ReadFile(aKey)
	metaJSON, _ := json.Marshal(map[string]interface{}{"version": "0.7.16", "time": 1, "host": "old", "certs": []map[string]string{
		{"path": aCert, "zip": "cert/2-fullchain.pem"}, {"path": aKey, "zip": "cert/3-privkey.pem"}}}) // 0.7.16 的 CertPaths 不含代理面板的证书
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{{DBName, dbBytes}, {"cert/2-fullchain.pem", aPEM}, {"cert/3-privkey.pem", aKeyPEM}, {"meta.json", metaJSON}} {
		w, _ := zw.Create(f.name)
		w.Write(f.data)
	}
	zw.Close()
	zipPath := filepath.Join(base, "old.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0o600)
	os.RemoveAll(le)

	root := filepath.Join(base, "new")
	rep, err := Restore(filepath.Join(root, DBName), zipPath)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := restoredState(t, filepath.Join(root, DBName))
	if b, _ := os.ReadFile(set["webCertFile"]); !bytes.Equal(b, aPEM) || !underDir(root, set["webKeyFile"]) {
		t.Fatalf("老备份里的外部证书照样放回来: %v", set)
	}
	if len(rep.Missing) != 2 || rep.Missing[0].Path != rCert || !strings.Contains(rep.Missing[0].Reason, "备份里没有这个文件") {
		t.Fatalf("老备份没带的代理面板证书要说清楚: %+v", rep.Missing)
	}
}

// 新版本做的备份,老版本也要能还原:包里还是只有 m-ui.db、meta.json、cert/*,meta 里每一条证书都有对应的文件,
// 新加的字段(certsUnreadable)老版本不认、忽略。老版本照它自己的规则处理(数据目录外的跳过),不会因为新内容失败。
func TestNewBackupStaysReadableForOldVersions(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	aCert, aKey := certbotPair(t, le, "a.example.com", 90)
	db := openDB(t, filepath.Join(base, "old", DBName))
	setAll(t, db, map[string]string{"webCertFile": aCert, "webKeyFile": aKey, "resellerCertFile": filepath.Join(base, "gone.pem"), "resellerKeyFile": filepath.Join(base, "gone.key")})
	var buf bytes.Buffer
	meta, err := CreateInMeta(db, CertPaths(db), "9.9.9", "", &buf)
	database.Close(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.CertsUnreadable) != 2 {
		t.Fatalf("读不出来的证书要记下来: %+v", meta)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	var metaJSON []byte
	for _, f := range zr.File {
		names[f.Name] = true
		if f.Name != DBName && f.Name != "meta.json" && !strings.HasPrefix(f.Name, "cert/") {
			t.Fatalf("包里多了老版本不认识的条目: %s", f.Name)
		}
		if f.Name == "meta.json" {
			metaJSON, _ = readZipFile(f)
		}
	}
	type oldCert struct {
		Path string `json:"path"`
		Zip  string `json:"zip"`
	}
	var old struct { // 0.7.16 的 Meta
		Version string    `json:"version"`
		Time    int64     `json:"time"`
		Host    string    `json:"host"`
		Certs   []oldCert `json:"certs"`
	}
	if err := json.Unmarshal(metaJSON, &old); err != nil || old.Version != "9.9.9" || len(old.Certs) != 2 {
		t.Fatalf("老版本读不懂 meta.json: %v %s", err, metaJSON)
	}
	for _, c := range old.Certs {
		if !names[c.Zip] || c.Path == "" {
			t.Fatalf("meta 里的证书在包里找不到,老版本还原会报错: %+v", c)
		}
	}
}

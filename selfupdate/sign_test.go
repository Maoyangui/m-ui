package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/brand"
)

// 内置的发布者公钥验得过真实发布过的签名:夹具是佛跳墙 v0.8.0 的 SHA256SUMS 和 SHA256SUMS.sig(同一把钥匙、
// 同一种签法 —— 发版流程给 m-ui 签的也是这个格式)。公钥抄错一个字节、或者签名格式的假设不对,这条就挂。
func TestEmbeddedKeyVerifiesAPublishedSignature(t *testing.T) {
	sums, err := os.ReadFile("testdata/published-SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile("testdata/published-SHA256SUMS.sig")
	if err != nil {
		t.Fatal(err)
	}
	if len(releaseKeys) == 0 {
		t.Fatal("没有内置的发布者公钥")
	}
	for i, k := range releaseKeys {
		if len(k) != ed25519.PublicKeySize {
			t.Fatalf("第 %d 把公钥长度不对: %d", i+1, len(k))
		}
	}
	if err := VerifySums(sums, sig); err != nil {
		t.Fatalf("真实发布过的签名应验得过: %v", err)
	}
	// 清单改一个字节、多一个换行,都验不过
	bad := append([]byte{}, sums...)
	bad[0] ^= 1
	if VerifySums(bad, sig) == nil {
		t.Fatal("清单被改过,不该验得过")
	}
	if VerifySums(append(append([]byte{}, sums...), '\n'), sig) == nil {
		t.Fatal("清单多了一个字节,不该验得过")
	}
}

// testKey 把内置公钥换成一把现造的测试钥匙,返回签名用的函数;测试结束换回去。
func testKey(t *testing.T) func(msg []byte) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := releaseKeys
	releaseKeys = []ed25519.PublicKey{pub}
	t.Cleanup(func() { releaseKeys = old })
	return func(msg []byte) []byte {
		return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg)) + "\n")
	}
}

func TestVerifySums(t *testing.T) {
	sign := testKey(t)
	sums := []byte("abc  m-ui-linux-amd64.tar.gz\n")
	good := sign(sums)
	if err := VerifySums(sums, good); err != nil {
		t.Fatalf("自己签的应验得过: %v", err)
	}
	if err := VerifySums(sums, bytes.TrimSpace(good)); err != nil {
		t.Fatalf("签名文件末尾有没有换行都应认: %v", err)
	}
	for name, sig := range map[string][]byte{
		"空的":        nil,
		"不是 base64": []byte("not base64 !!"),
		"长度不对":      []byte(base64.StdEncoding.EncodeToString([]byte("short"))),
		"给别的内容签的":   sign([]byte("other")),
		"全零的 64 字节": []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))),
	} {
		if VerifySums(sums, sig) == nil {
			t.Fatalf("%s:不该验得过", name)
		}
	}
	// 别的钥匙签的不认
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if VerifySums(sums, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(other, sums)))) == nil {
		t.Fatal("别的钥匙签的不该验得过")
	}
	// 列表里有两把时,任何一把验得过都算(换钥匙的过渡期)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	releaseKeys = append(releaseKeys, pub2)
	if err := VerifySums(sums, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv2, sums)))); err != nil {
		t.Fatalf("第二把钥匙签的应验得过: %v", err)
	}
	if err := VerifySums(sums, good); err != nil {
		t.Fatalf("第一把钥匙签的仍应验得过: %v", err)
	}
}

// fakeRelease 起一个假的 Release 下载站;files 是「文件名 → 内容」,没有的回 404。返回被请求过的文件名(按顺序)。
func fakeRelease(t *testing.T, tag string, files map[string][]byte) *[]string {
	t.Helper()
	var asked []string
	prefix := "/" + brand.RepoPath + "/releases/download/" + tag + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, prefix)
		asked = append(asked, name)
		body, ok := files[name]
		if !ok || name == r.URL.Path {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	old := githubBase
	githubBase = srv.URL + "/"
	t.Cleanup(func() { githubBase = old; srv.Close() })
	return &asked
}

// tarGz 一个只装着可执行文件 m-ui 的压缩包(内容是一段 shell 脚本:Stage 会跑一下「m-ui version」确认它能执行)。
func tarGz(t *testing.T, script string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "m-ui", Mode: 0o755, Size: int64(len(script))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// 面板内更新的下载步骤:清单得是发布者签的、程序得和清单对得上,才写出 <bin>.new;
// 缺签名、换了清单、换了程序、别的钥匙签的,一律中止,而且不留下 .new、现在的程序不动。
func TestStageRequiresAValidSignature(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Stage 要试着运行新程序,只在 Linux 上测")
	}
	sign := testKey(t)
	const tag = "v9.9.9"
	asset := "m-ui-linux-" + runtime.GOARCH + ".tar.gz"
	good := tarGz(t, "#!/bin/sh\necho m-ui 9.9.9\n")
	evil := tarGz(t, "#!/bin/sh\necho pwned\n")
	sumsFor := func(b []byte) []byte {
		h := sha256.Sum256(b)
		return []byte(hex.EncodeToString(h[:]) + "  " + asset + "\n")
	}
	goodSums := sumsFor(good)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	cases := []struct {
		name    string
		files   map[string][]byte
		wantErr string // 空 = 应成功
		noAsset bool   // 验签没过时不该再去下那个大文件
	}{
		{"签名和清单都对", map[string][]byte{asset: good, "SHA256SUMS": goodSums, "SHA256SUMS.sig": sign(goodSums)}, "", false},
		{"没有签名文件", map[string][]byte{asset: good, "SHA256SUMS": goodSums}, "取不到这个版本的签名", true},
		{"程序和清单一起被换(签名还是原来那份)", map[string][]byte{asset: evil, "SHA256SUMS": sumsFor(evil), "SHA256SUMS.sig": sign(goodSums)}, "签名没有通过", true},
		{"别的钥匙签的", map[string][]byte{asset: evil, "SHA256SUMS": sumsFor(evil), "SHA256SUMS.sig": []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(otherPriv, sumsFor(evil))))}, "签名没有通过", true},
		{"签名文件是乱的", map[string][]byte{asset: good, "SHA256SUMS": goodSums, "SHA256SUMS.sig": []byte("<html>404</html>")}, "签名没有通过", true},
		{"清单是真的、程序被换", map[string][]byte{asset: evil, "SHA256SUMS": goodSums, "SHA256SUMS.sig": sign(goodSums)}, "SHA256 不一致", false},
		{"没有清单", map[string][]byte{asset: good, "SHA256SUMS.sig": sign(goodSums)}, "取不到这个版本的 SHA256SUMS", true},
		{"清单里没有这个架构的包", map[string][]byte{asset: good, "SHA256SUMS": []byte("00  other.tar.gz\n"), "SHA256SUMS.sig": sign([]byte("00  other.tar.gz\n"))}, "SHA256SUMS 里没有", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asked := fakeRelease(t, tag, c.files)
			bin := filepath.Join(t.TempDir(), "m-ui")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			path, err := Stage(context.Background(), tag, bin, nil)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应成功: %v", err)
				}
				got, _ := os.ReadFile(path)
				if path != bin+".new" || !strings.Contains(string(got), "9.9.9") {
					t.Fatalf("应写出新程序 %s,实际 %s %q", bin+".new", path, got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("应中止并说明「%s」,实际 %v", c.wantErr, err)
			}
			if _, statErr := os.Stat(bin + ".new"); statErr == nil {
				t.Fatal("中止了就不该留下 .new")
			}
			if old, _ := os.ReadFile(bin); !strings.Contains(string(old), "old") {
				t.Fatal("现在的程序不该被动")
			}
			if c.noAsset {
				for _, name := range *asked {
					if name == asset {
						t.Fatalf("清单没验过就不该去下程序,实际请求了 %v", *asked)
					}
				}
			}
		})
	}
}

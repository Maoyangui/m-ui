//go:build !windows

package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这些测试真的用 bash 跑运维脚本里的 apt / DNS 兜底公共函数,系统命令换成 PATH 里的替身
// (apt-get、getent、systemctl、curl、fuser),/etc 下的文件换成临时目录里的同名文件。
// 替身的行为由 state 目录里的开关文件决定:
//   dns_ok              本机 DNS 正常
//   public_blocked      公共 DNS(1.1.1.1 / 8.8.8.8)也不通:临时 resolved 配置和管理员备用 DNS 都没用
//   doh_fail            DoH 查不到
//   apt_fail_<子命令>   apt-get 该子命令以 100 退出,输出为文件内容
//   apt_sleep_<子命令>  apt-get 该子命令先睡这么多秒
//   lock_pid            fuser 报告这个进程占着 dpkg 锁;apt-get 也会打印等锁提示
//   resolved_down       systemd-resolved 不在运行
// 每次调用都记进 state/calls,apt-get 还会记下当时临时 DNS 文件和 /etc/hosts 临时行是否存在。

type aptSim struct {
	t                                *testing.T
	root, bin, state                 string
	hosts, resolv, resolvedD, aptDir string
	origHosts                        string
}

const simFakeGetent = `#!/bin/bash
echo "getent $*" >> "$SIM_STATE/calls"
h="$2"
ok() { echo "203.0.113.5     STREAM $h"; exit 0; }
grep -qE "[[:space:]]$h([[:space:]]|\$)" "$SIM_HOSTS" 2>/dev/null && ok
[ -f "$SIM_STATE/dns_ok" ] && ok
if [ ! -f "$SIM_STATE/public_blocked" ]; then
  [ -f "$SIM_RESOLVED_D/m-ui-tmp-dns.conf" ] && ok
  [ -f "$SIM_RESOLVED_D/m-ui-fallback-dns.conf" ] && ok
fi
exit 2
`

const simFakeApt = `#!/bin/bash
echo "apt-get $* conf=$([ -f "$SIM_RESOLVED_D/m-ui-tmp-dns.conf" ] && echo 1 || echo 0) hosts=$(grep -c 'm-ui-tmp' "$SIM_HOSTS")" >> "$SIM_STATE/calls"
sub=""; skip=""
for a in "$@"; do
  if [ -n "$skip" ]; then skip=""; continue; fi
  case "$a" in -o) skip=1;; -*) ;; *) sub="$a"; break;; esac
done
if [ -f "$SIM_STATE/lock_pid" ]; then
  echo "Waiting for cache lock: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process $(cat "$SIM_STATE/lock_pid") (unattended-upgr)... 1s"
  sleep 2
fi
[ -f "$SIM_STATE/apt_sleep_$sub" ] && sleep "$(cat "$SIM_STATE/apt_sleep_$sub")"
if [ -f "$SIM_STATE/apt_fail_$sub" ]; then cat "$SIM_STATE/apt_fail_$sub"; exit 100; fi
dns() { getent ahosts "$1" >/dev/null 2>&1; }
case "$sub" in
  update)
    if dns archive.ubuntu.com; then echo "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease"
    else
      echo "Err:1 http://archive.ubuntu.com/ubuntu noble InRelease"
      echo "  Temporary failure resolving 'archive.ubuntu.com'"
      echo "W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/noble/InRelease  Temporary failure resolving 'archive.ubuntu.com'"
      echo "W: Some index files failed to download. They have been ignored, or old ones used instead."
    fi
    echo "Reading package lists..."; exit 0;;
  install)
    if ! dns archive.ubuntu.com; then
      echo "E: Failed to fetch http://archive.ubuntu.com/ubuntu/pool/main/x.deb  Temporary failure resolving 'archive.ubuntu.com'"; exit 100
    fi
    for a in "$@"; do case "$a" in -*|install|DPkg::*|Dpkg::*) ;; *) echo "Setting up $a (1.0) ...";; esac; done
    exit 0;;
  *) echo "apt-get $sub ok"; exit 0;;
esac
`

const simFakeSystemctl = `#!/bin/bash
echo "systemctl $*" >> "$SIM_STATE/calls"
case "$1" in
  is-active) [ -f "$SIM_STATE/resolved_down" ] && exit 3; exit 0;;
esac
exit 0
`

const simFakeCurl = `#!/bin/bash
echo "curl $*" >> "$SIM_STATE/calls"
url=""; for a in "$@"; do case "$a" in http*://*) url="$a";; esac; done
case "$url" in
  *dns-query*|*/resolve*)
    [ -f "$SIM_STATE/doh_fail" ] && exit 7
    name="$(sed -E 's/.*[?&]name=([^&]+).*/\1/' <<<"$url")"
    printf '{"Status":0,"Answer":[{"name":"%s","type":5,"data":"cdn.example.net."},{"name":"cdn.example.net","type":1,"TTL":60,"data":"203.0.113.10"}]}\n' "$name"
    exit 0;;
esac
echo "fake curl: $url" >&2
exit 22
`

const simFakeFuser = `#!/bin/bash
echo "fuser $*" >> "$SIM_STATE/calls"
if [ -f "$SIM_STATE/lock_pid" ]; then echo "$1:" >&2; echo " $(cat "$SIM_STATE/lock_pid")"; exit 0; fi
exit 1
`

func newAptSim(t *testing.T, resolved bool) *aptSim {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("没有 bash")
	}
	d := t.TempDir()
	s := &aptSim{t: t, root: d, bin: filepath.Join(d, "bin"), state: filepath.Join(d, "state")}
	s.hosts = filepath.Join(d, "etc", "hosts")
	s.resolv = filepath.Join(d, "etc", "resolv.conf")
	s.resolvedD = filepath.Join(d, "etc", "systemd", "resolved.conf.d")
	s.aptDir = filepath.Join(d, "etc", "apt")
	for _, p := range []string{s.bin, s.state, filepath.Join(s.aptDir, "sources.list.d"), filepath.Join(d, "run", "systemd", "resolve")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.origHosts = "127.0.0.1 localhost\n::1 ip6-localhost\n"
	s.write(s.hosts, s.origHosts)
	s.write(filepath.Join(s.aptDir, "sources.list.d", "ubuntu.sources"),
		"Types: deb\nURIs: http://archive.ubuntu.com/ubuntu/\nSuites: noble\nComponents: main\n")
	stub := filepath.Join(d, "run", "systemd", "resolve", "stub-resolv.conf")
	s.write(stub, "nameserver 127.0.0.53\n")
	if resolved {
		if err := os.Symlink(stub, s.resolv); err != nil {
			t.Fatal(err)
		}
	} else {
		s.write(s.resolv, "nameserver 10.0.0.2\n")
	}
	for name, body := range map[string]string{"getent": simFakeGetent, "apt-get": simFakeApt, "systemctl": simFakeSystemctl, "curl": simFakeCurl, "fuser": simFakeFuser} {
		p := filepath.Join(s.bin, name)
		s.write(p, body)
		os.Chmod(p, 0o755)
	}
	return s
}

func (s *aptSim) write(p, body string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *aptSim) on(flag, content string) { s.write(filepath.Join(s.state, flag), content) }

func (s *aptSim) calls() string { return readFile(filepath.Join(s.state, "calls")) }

// localize 把脚本里的系统路径换成临时目录里的,并把进度间隔改成 1 秒。
func (s *aptSim) localize(script string) string {
	rep := []struct{ from, to string }{
		{"MUI_HOSTS=/etc/hosts\n", "MUI_HOSTS=" + s.hosts + "\n"},
		{"MUI_RESOLV=/etc/resolv.conf\n", "MUI_RESOLV=" + s.resolv + "\n"},
		{"MUI_RESOLVED_D=/etc/systemd/resolved.conf.d\n", "MUI_RESOLVED_D=" + s.resolvedD + "\n"},
		{"MUI_APT_DIR=/etc/apt\n", "MUI_APT_DIR=" + s.aptDir + "\n"},
		{"MUI_TICK=15\n", "MUI_TICK=1\n"},
	}
	for _, r := range rep {
		if !strings.Contains(script, r.from) {
			s.t.Fatalf("脚本里没有 %q,公共函数的路径变量不见了", strings.TrimSpace(r.from))
		}
		script = strings.Replace(script, r.from, r.to, 1)
	}
	return script
}

func (s *aptSim) cmd(ctx context.Context, body string) *exec.Cmd {
	script := s.localize("set -uo pipefail\nexport DEBIAN_FRONTEND=noninteractive\n" + scriptLib + "\n" + body)
	c := newTaskCmd(ctx, script)
	c.Env = append(c.Env, "PATH="+s.bin+":/usr/bin:/bin:/usr/sbin:/sbin",
		"SIM_STATE="+s.state, "SIM_HOSTS="+s.hosts, "SIM_RESOLVED_D="+s.resolvedD)
	return c
}

func (s *aptSim) run(body string) (string, error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.cmd(ctx, body).CombinedOutput()
	s.t.Logf("脚本输出:\n%s\n调用记录:\n%s", out, s.calls())
	return string(out), err
}

func (s *aptSim) assertRestored() {
	s.t.Helper()
	if got := readFile(s.hosts); got != s.origHosts {
		s.t.Errorf("/etc/hosts 没有恢复原样:\n%q", got)
	}
	if _, err := os.Stat(filepath.Join(s.resolvedD, "m-ui-tmp-dns.conf")); err == nil {
		s.t.Error("临时 DNS 文件 m-ui-tmp-dns.conf 没有删掉")
	}
}

const simBody = `mui_apt "更新软件源" "updating package lists" update -y
mui_apt "安装 foo" "installing foo" install -y foo
echo BODY-END
`

func TestAptLibDNSOKAddsNothing(t *testing.T) {
	s := newAptSim(t, true)
	s.on("dns_ok", "")
	out, err := s.run(simBody)
	if err != nil || !strings.Contains(out, "BODY-END") {
		t.Fatalf("DNS 正常时应顺利跑完: %v", err)
	}
	if _, err := os.Stat(s.resolvedD); err == nil {
		t.Error("DNS 正常时不应建 resolved.conf.d")
	}
	c := s.calls()
	if strings.Contains(c, "restart systemd-resolved") || strings.Contains(c, "curl ") {
		t.Error("DNS 正常时不应重启 resolved 或查 DoH")
	}
	if !strings.Contains(c, "DPkg::Lock::Timeout=300") {
		t.Error("apt-get 应带 -o DPkg::Lock::Timeout=300")
	}
	if strings.Contains(out, "1.1.1.1") || strings.Contains(out, "/etc/hosts") {
		t.Error("DNS 正常时输出里不应提临时 DNS")
	}
	s.assertRestored()
}

func TestAptLibResolvedFallback(t *testing.T) {
	s := newAptSim(t, true)
	out, err := s.run(simBody)
	if err != nil || !strings.Contains(out, "BODY-END") {
		t.Fatalf("临时 DNS 后应装成功: %v", err)
	}
	if !strings.Contains(out, "archive.ubuntu.com") || !strings.Contains(out, "1.1.1.1 / 8.8.8.8") || !strings.Contains(out, "任务结束后恢复") {
		t.Error("输出应说明解析不了哪个域名、临时改用了哪些 DNS")
	}
	c := s.calls()
	if !strings.Contains(c, "install -y foo conf=1") {
		t.Error("apt-get install 时临时 DNS 文件应在")
	}
	if n := strings.Count(c, "systemctl restart systemd-resolved"); n != 2 {
		t.Errorf("应重启 resolved 两次(加上、恢复),实际 %d 次", n)
	}
	if strings.Contains(c, "curl ") {
		t.Error("resolved 兜底已生效时不应再查 DoH")
	}
	if _, err := os.Stat(s.resolvedD); err == nil {
		t.Error("resolved.conf.d 是任务建的,结束后应删掉空目录")
	}
	s.assertRestored()
}

// 安装前的预检没发现问题(或者源列表里没有出错的那个主机),apt-get update 输出里出现解析失败时也要兜底重试。
func TestAptLibRetryOnAptOutput(t *testing.T) {
	s := newAptSim(t, true)
	os.Remove(filepath.Join(s.aptDir, "sources.list.d", "ubuntu.sources"))
	out, err := s.run(simBody)
	if err != nil || !strings.Contains(out, "BODY-END") {
		t.Fatalf("应兜底后重试成功: %v", err)
	}
	if strings.Count(s.calls(), " update -y ") != 2 {
		t.Error("apt-get update 应在兜底后重试一次")
	}
	s.assertRestored()
}

func TestAptLibResolvedFallbackFailStillCleans(t *testing.T) {
	s := newAptSim(t, true)
	s.on("apt_fail_install", "E: Unable to locate package foo\n")
	out, err := s.run(simBody)
	if err == nil || strings.Contains(out, "BODY-END") {
		t.Fatal("apt 失败后应立即停下")
	}
	if !strings.Contains(out, "错误:") || !strings.Contains(out, "找不到") {
		t.Errorf("应给出明确原因:\n%s", out)
	}
	s.assertRestored()
	if !strings.Contains(out, "已删除临时 DNS") {
		t.Error("失败时也应说明已恢复 DNS")
	}
}

func TestAptLibCancelCleans(t *testing.T) {
	for _, resolved := range []bool{true, false} {
		s := newAptSim(t, resolved)
		s.on("apt_sleep_install", "40")
		ctx, cancel := context.WithCancel(context.Background())
		c := s.cmd(ctx, simBody)
		var buf strings.Builder
		c.Stdout, c.Stderr = &buf, &buf
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(s.calls(), " install -y foo ") && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if resolved && !strings.Contains(s.calls(), "install -y foo conf=1") || !resolved && !strings.Contains(s.calls(), "install -y foo conf=0 hosts=1") {
			cancel()
			c.Wait()
			t.Fatalf("取消前临时 DNS 应已生效(resolved=%v):\n%s\n%s", resolved, buf.String(), s.calls())
		}
		start := time.Now()
		cancel()
		err := c.Wait()
		t.Logf("resolved=%v 输出:\n%s", resolved, buf.String())
		if err == nil {
			t.Error("取消后应以失败结束")
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("取消后 %v 才结束,应在几秒内撤销并退出", d)
		}
		if !strings.Contains(buf.String(), "取消") {
			t.Error("输出应说明任务被取消")
		}
		s.assertRestored()
		cancel()
	}
}

func TestAptLibHostsFallback(t *testing.T) {
	s := newAptSim(t, false)
	out, err := s.run(simBody)
	if err != nil || !strings.Contains(out, "BODY-END") {
		t.Fatalf("写临时 hosts 后应装成功: %v", err)
	}
	c := s.calls()
	if !strings.Contains(c, "1.1.1.1/dns-query?name=archive.ubuntu.com&type=A") {
		t.Error("应经 1.1.1.1 DoH 查 archive.ubuntu.com")
	}
	if !strings.Contains(c, "install -y foo conf=0 hosts=1") {
		t.Error("apt-get install 时 /etc/hosts 里应有一行临时解析")
	}
	if strings.Contains(c, "restart systemd-resolved") {
		t.Error("不是 systemd-resolved 时不应动 resolved")
	}
	if !strings.Contains(out, "203.0.113.10") || !strings.Contains(out, "/etc/hosts") {
		t.Error("输出应说明临时写了哪条 hosts")
	}
	s.assertRestored()
}

func TestAptLibHostsFallbackKeepsNoTrailingNewline(t *testing.T) {
	s := newAptSim(t, false)
	s.origHosts = "127.0.0.1 localhost"
	s.write(s.hosts, s.origHosts)
	if _, err := s.run(simBody); err != nil {
		t.Fatal(err)
	}
	s.assertRestored()
}

func TestAptLibKeepsAdminFallback(t *testing.T) {
	s := newAptSim(t, true)
	admin := filepath.Join(s.resolvedD, "m-ui-fallback-dns.conf")
	body := "[Resolve]\nDNS=1.1.1.1 8.8.8.8\n"
	s.write(admin, body)
	s.on("public_blocked", "") // 公共 DNS 的 53 端口也不通:管理员的备用 DNS 没用,只能走 DoH
	out, err := s.run(simBody)
	if err != nil || !strings.Contains(out, "BODY-END") {
		t.Fatalf("应改走 DoH 写 hosts 后成功: %v", err)
	}
	if got := readFile(admin); got != body {
		t.Errorf("管理员的备用 DNS 文件被改动了: %q", got)
	}
	c := s.calls()
	if strings.Contains(c, "conf=1") || strings.Contains(c, "restart systemd-resolved") {
		t.Error("已有管理员备用 DNS 时不应再加临时文件、重启 resolved")
	}
	if !strings.Contains(out, "m-ui-fallback-dns.conf") {
		t.Error("输出应说明已有备用 DNS")
	}
	s.assertRestored()
	if _, err := os.Stat(admin); err != nil {
		t.Error("管理员的备用 DNS 文件被删了")
	}
}

func TestAptLibAdminFallbackWorks(t *testing.T) {
	s := newAptSim(t, true)
	admin := filepath.Join(s.resolvedD, "m-ui-fallback-dns.conf")
	s.write(admin, "[Resolve]\nDNS=1.1.1.1\n")
	out, err := s.run(simBody)
	if err != nil || !strings.Contains(out, "BODY-END") {
		t.Fatal(err)
	}
	if c := s.calls(); strings.Contains(c, "restart") || strings.Contains(c, "curl ") || strings.Contains(c, "conf=1") {
		t.Error("管理员的备用 DNS 已经能解析时什么都不应加")
	}
	if _, err := os.Stat(admin); err != nil {
		t.Error("管理员的备用 DNS 文件被删了")
	}
}

func TestAptLibStopsOnAptFailure(t *testing.T) {
	s := newAptSim(t, true)
	s.on("dns_ok", "")
	s.on("apt_fail_update", "E: The repository 'http://archive.ubuntu.com/ubuntu noble Release' does not have a Release file.\n")
	out, err := s.run(simBody)
	if err == nil || strings.Contains(out, "BODY-END") || strings.Contains(s.calls(), "install -y foo") {
		t.Fatal("apt-get update 失败后应立即停下,不再往下装")
	}
	if !strings.Contains(out, "错误:更新软件源失败") || !strings.Contains(out, "Release file") {
		t.Errorf("应给出失败的步骤和 apt 的原话:\n%s", out)
	}
}

func TestAptLibStopsWhenDNSUnfixable(t *testing.T) {
	s := newAptSim(t, true)
	s.on("public_blocked", "")
	s.on("doh_fail", "")
	out, err := s.run(simBody)
	if err == nil || strings.Contains(out, "BODY-END") || strings.Contains(s.calls(), "apt-get") {
		t.Fatal("兜底后仍解析不了时应在跑 apt 之前停下")
	}
	if !strings.Contains(out, "解析不了 archive.ubuntu.com,请检查这台机器的 DNS 或网络") {
		t.Errorf("应说明解析不了哪个域名:\n%s", out)
	}
	s.assertRestored()
}

func TestAptLibLockHolderAndProgress(t *testing.T) {
	s := newAptSim(t, true)
	s.on("dns_ok", "")
	// 起一个名叫 unattended-upgr 的进程,假装它占着 dpkg 锁
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("没有 sleep")
	}
	fake := filepath.Join(s.root, "unattended-upgr")
	b, _ := os.ReadFile(sleepBin)
	os.WriteFile(fake, b, 0o755)
	holder := exec.Command(fake, "30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Process.Kill()
	s.on("lock_pid", strconv.Itoa(holder.Process.Pid))
	s.on("apt_sleep_update", "3")
	out, err := s.run(simBody)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unattended-upgr") || !strings.Contains(out, "PID "+strconv.Itoa(holder.Process.Pid)) {
		t.Errorf("等锁时应说明是谁占着:\n%s", out)
	}
	if !strings.Contains(out, "仍在") || !strings.Contains(out, "秒") {
		t.Errorf("apt 运行期间应定时打进度:\n%s", out)
	}
}

// 整个 WARP 安装脚本:apt-get update 失败就停在第一步,不再去下载 Cloudflare 公钥、加源。
func TestWarpInstallStopsWhenAptFails(t *testing.T) {
	if _, err := exec.LookPath("warp-cli"); err == nil {
		t.Skip("本机装了 warp-cli,脚本会跳过安装步骤")
	}
	s := newAptSim(t, true)
	s.on("dns_ok", "")
	s.on("apt_fail_update", "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 812 (unattended-upgr)\n")
	full, err := Script("warp-install", Params{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, ". /etc/os-release\n") {
		t.Fatal("脚本结构变了")
	}
	full = strings.Replace(full, ". /etc/os-release\n", "ID=ubuntu VERSION_CODENAME=noble\n", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := newTaskCmd(ctx, s.localize(full))
	c.Env = append(c.Env, "PATH="+s.bin+":/usr/bin:/bin:/usr/sbin:/sbin", "SIM_STATE="+s.state, "SIM_HOSTS="+s.hosts, "SIM_RESOLVED_D="+s.resolvedD)
	b, err := c.CombinedOutput()
	out := string(b)
	t.Logf("输出:\n%s\n调用:\n%s", out, s.calls())
	if err == nil {
		t.Fatal("apt 失败时 WARP 安装应以失败结束")
	}
	if strings.Contains(out, "[2/4]") || strings.Contains(s.calls(), "pkg.cloudflareclient.com") {
		t.Error("apt-get update 失败后不应继续添加 Cloudflare 源")
	}
	if !strings.Contains(out, "apt/dpkg 仍被其它程序占用") {
		t.Errorf("应说明 apt 锁被占用:\n%s", out)
	}
}

func TestAllScriptsSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("没有 bash")
	}
	for _, task := range Tasks {
		s, err := Script(task.Name, Params{})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("bash", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s 脚本语法错误: %v\n%s", task.Name, err, out)
		}
		if aptDirect.MatchString(s) {
			t.Errorf("%s 里还有直接调用的 apt-get,应走 mui_apt", task.Name)
		}
	}
}

var aptDirect = regexp.MustCompile(`(?m)^[ \t]*apt-get `)

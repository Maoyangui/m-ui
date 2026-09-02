// Package ops 提供运维能力:系统信息、Cloudflare WARP 一键安装/启停、系统优化(swap/sysctl/limits/NTP)。
// 所有任务都是固定脚本(源自 vps.txt),只接受经校验的数字参数,串行执行并保留输出日志。
package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 系统信息 ----

type WarpInfo struct {
	Installed  bool   `json:"installed"`
	Service    string `json:"service"`  // warp-svc 状态
	Socks      string `json:"socks"`    // warp-socks5 单元状态
	Port       int    `json:"port"`
	Listening  bool   `json:"listening"`
	Status     string `json:"status"`   // warp-cli status 首行
	Exit       string `json:"exit"`     // on | plus | off | 错误
	ExitIP     string `json:"exitIp"`
	ExitLoc    string `json:"exitLoc"`
	ExitColo   string `json:"exitColo"`
}

type Info struct {
	Linux    bool     `json:"linux"`
	OS       string   `json:"os"`
	Kernel   string   `json:"kernel"`
	Arch     string   `json:"arch"`
	Hostname string   `json:"hostname"`
	Uptime   int64    `json:"uptime"`
	Load     string   `json:"load"`
	MemTotal int64    `json:"memTotal"`
	MemAvail int64    `json:"memAvail"`
	SwapTotal int64   `json:"swapTotal"`
	SwapFree int64    `json:"swapFree"`
	DiskTotal int64   `json:"diskTotal"`
	DiskFree int64    `json:"diskFree"`
	CC       string   `json:"cc"`     // tcp_congestion_control
	Qdisc    string   `json:"qdisc"`
	NoFile   uint64   `json:"nofile"` // 本进程打开文件数上限
	NTP      string   `json:"ntp"`    // yes | no | unknown
	Root     bool     `json:"root"`
	Warp     WarpInfo `json:"warp"`
	Tuned    bool     `json:"tuned"`  // 存在 /etc/sysctl.d/99-m-ui-tune.conf
	Limits   bool     `json:"limits"` // 存在 m-ui.service override
}

// Collect 收集系统信息;非 Linux 只填基础字段。
func Collect(ctx context.Context, warpPort int, dataDir string) Info {
	in := Info{Linux: runtime.GOOS == "linux", Arch: runtime.GOARCH, OS: runtime.GOOS}
	in.Hostname, _ = os.Hostname()
	in.Warp.Port = warpPort
	if !in.Linux {
		in.Warp.Listening = portOpen(warpPort)
		return in
	}
	in.Root = os.Geteuid() == 0
	in.OS = osRelease(readFile("/etc/os-release"))
	in.Kernel = strings.TrimSpace(readFile("/proc/sys/kernel/osrelease"))
	if f := strings.Fields(readFile("/proc/uptime")); len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		in.Uptime = int64(v)
	}
	if f := strings.Fields(readFile("/proc/loadavg")); len(f) >= 3 {
		in.Load = strings.Join(f[:3], " ")
	}
	mem := meminfo(readFile("/proc/meminfo"))
	in.MemTotal, in.MemAvail = mem["MemTotal"], mem["MemAvailable"]
	in.SwapTotal, in.SwapFree = mem["SwapTotal"], mem["SwapFree"]
	in.DiskTotal, in.DiskFree = diskUsage(dataDir)
	in.CC = strings.TrimSpace(readFile("/proc/sys/net/ipv4/tcp_congestion_control"))
	in.Qdisc = strings.TrimSpace(readFile("/proc/sys/net/core/default_qdisc"))
	in.NoFile = nofileLimit()
	in.NTP = "unknown"
	if out, err := run(ctx, 3*time.Second, "timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil {
		in.NTP = strings.TrimSpace(out)
	}
	_, err := os.Stat("/etc/sysctl.d/99-m-ui-tune.conf")
	in.Tuned = err == nil
	_, err = os.Stat("/etc/systemd/system/m-ui.service.d/override.conf")
	in.Limits = err == nil

	// WARP
	if _, err := exec.LookPath("warp-cli"); err == nil {
		in.Warp.Installed = true
		if out, err := run(ctx, 3*time.Second, "systemctl", "is-active", "warp-svc.service"); err == nil || out != "" {
			in.Warp.Service = strings.TrimSpace(out)
		}
		if out, err := run(ctx, 3*time.Second, "systemctl", "is-active", "warp-socks5.service"); err == nil || out != "" {
			in.Warp.Socks = strings.TrimSpace(out)
		}
		if out, err := run(ctx, 5*time.Second, "warp-cli", "--accept-tos", "status"); err == nil {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) > 0 {
				in.Warp.Status = strings.TrimSpace(lines[0])
			}
		} else if out != "" {
			in.Warp.Status = strings.TrimSpace(strings.Split(out, "\n")[0])
		}
	}
	in.Warp.Listening = portOpen(warpPort)
	if in.Warp.Listening {
		in.Warp.Exit, in.Warp.ExitIP, in.Warp.ExitLoc, in.Warp.ExitColo = CheckExit(ctx, warpPort)
	}
	return in
}

// CheckExit 经 SOCKS5 访问 Cloudflare trace,返回 warp 状态与出口信息。
func CheckExit(ctx context.Context, port int) (state, ip, loc, colo string) {
	proxyURL, _ := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", port))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.cloudflare.com/cdn-cgi/trace", nil)
	resp, err := c.Do(req)
	if err != nil {
		return "error: " + err.Error(), "", "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return ParseTrace(string(b))
}

// ParseTrace 解析 cdn-cgi/trace 文本。
func ParseTrace(s string) (state, ip, loc, colo string) {
	state = "off"
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "warp":
			state = v
		case "ip":
			ip = v
		case "loc":
			loc = v
		case "colo":
			colo = v
		}
	}
	return
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 800*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func osRelease(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "linux"
}

func meminfo(s string) map[string]int64 {
	out := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(f[0], 10, 64)
		out[k] = n * 1024
	}
	return out
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(c, name, args...).CombinedOutput()
	return string(out), err
}

// ---- 任务执行 ----

type Task struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Desc   string `json:"desc"`
	Danger bool   `json:"danger"`
}

// Tasks 是面板可执行的固定任务列表。
var Tasks = []Task{
	{Name: "warp-install", Title: "安装 WARP", Desc: "添加 Cloudflare 源,安装 cloudflare-warp 并完成注册(Ubuntu/Debian)"},
	{Name: "warp-enable", Title: "启用 WARP SOCKS5", Desc: "MASQUE 代理模式,监听 127.0.0.1:<端口>,写 systemd 单元开机自启,验证出口"},
	{Name: "warp-disable", Title: "停用 WARP", Desc: "断开并禁用 warp-socks5 单元"},
	{Name: "warp-uninstall", Title: "卸载 WARP", Desc: "移除 cloudflare-warp、单元与源", Danger: true},
	{Name: "swap", Title: "创建 swap", Desc: "无 swap 时创建 <N>G /swapfile 并写入 fstab"},
	{Name: "sysctl", Title: "内核参数 + BBR", Desc: "写入 /etc/sysctl.d/99-m-ui-tune.conf(fq+bbr、连接数、端口范围等)并应用"},
	{Name: "limits", Title: "文件句柄上限", Desc: "为 m-ui.service 写 LimitNOFILE=1048576 等 override(重启 m-ui 生效)"},
	{Name: "ntp", Title: "时间同步", Desc: "开启 systemd-timesyncd NTP 同步"},
	{Name: "tune-all", Title: "一键优化", Desc: "swap + 内核参数 + 文件句柄 + NTP"},
}

type Params struct {
	Port   int
	SwapGB int
}

// Script 返回任务脚本;参数经校验后替换占位符。
func Script(name string, p Params) (string, error) {
	if p.Port <= 0 || p.Port > 65535 {
		p.Port = 40000
	}
	if p.SwapGB <= 0 || p.SwapGB > 64 {
		p.SwapGB = 2
	}
	var s string
	switch name {
	case "warp-install":
		s = scriptWarpInstall
	case "warp-enable":
		s = scriptWarpEnable
	case "warp-disable":
		s = scriptWarpDisable
	case "warp-uninstall":
		s = scriptWarpUninstall
	case "swap":
		s = scriptSwap
	case "sysctl":
		s = scriptSysctl
	case "limits":
		s = scriptLimits
	case "ntp":
		s = scriptNTP
	case "tune-all":
		s = scriptSwap + "\n" + scriptSysctl + "\n" + scriptLimits + "\n" + scriptNTP
	default:
		return "", fmt.Errorf("未知任务 %q", name)
	}
	s = strings.ReplaceAll(s, "{{PORT}}", strconv.Itoa(p.Port))
	s = strings.ReplaceAll(s, "{{SWAP_GB}}", strconv.Itoa(p.SwapGB))
	return "set -uo pipefail\nexport DEBIAN_FRONTEND=noninteractive\n" + s, nil
}

// Runner 串行执行任务并保留输出。
type Runner struct {
	mu      sync.Mutex
	running bool
	current string
	log     []string
	last    struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Time int64  `json:"time"`
		Err  string `json:"error,omitempty"`
	}
	cancel context.CancelFunc
}

func NewRunner() *Runner { return &Runner{} }

type Status struct {
	Running bool        `json:"running"`
	Current string      `json:"current"`
	Log     []string    `json:"log"`
	Last    interface{} `json:"last"`
	Linux   bool        `json:"linux"`
}

func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{Running: r.running, Current: r.current, Log: append([]string(nil), r.log...), Last: r.last, Linux: runtime.GOOS == "linux"}
}

func (r *Runner) appendLog(line string) {
	r.mu.Lock()
	r.log = append(r.log, line)
	if len(r.log) > 500 {
		r.log = r.log[len(r.log)-500:]
	}
	r.mu.Unlock()
}

// Start 异步执行任务;done 在结束时回调(ok, err)。
func (r *Runner) Start(name string, p Params, done func(ok bool, err error)) error {
	if runtime.GOOS != "linux" {
		return errors.New("运维任务仅支持 Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("需要以 root 运行 m-ui")
	}
	script, err := Script(name, p)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("已有任务在执行:" + r.current)
	}
	r.running, r.current, r.log = true, name, nil
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	r.cancel = cancel
	r.mu.Unlock()

	go func() {
		defer cancel()
		r.appendLog(fmt.Sprintf("[%s] 开始 %s", time.Now().Format("15:04:05"), name))
		cmd := exec.CommandContext(ctx, "bash", "-c", script)
		cmd.Env = append(os.Environ(), "LANG=C.UTF-8")
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		go func() {
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				r.appendLog(sc.Text())
			}
		}()
		err := cmd.Run()
		pw.Close()
		time.Sleep(50 * time.Millisecond)
		r.mu.Lock()
		r.running, r.current = false, ""
		r.last.Name, r.last.OK, r.last.Time = name, err == nil, time.Now().Unix()
		r.last.Err = ""
		if err != nil {
			r.last.Err = err.Error()
		}
		r.mu.Unlock()
		if err != nil {
			r.appendLog(fmt.Sprintf("[%s] 失败: %v", time.Now().Format("15:04:05"), err))
		} else {
			r.appendLog(fmt.Sprintf("[%s] 完成", time.Now().Format("15:04:05")))
		}
		if done != nil {
			done(err == nil, err)
		}
	}()
	return nil
}

// Cancel 终止当前任务。
func (r *Runner) Cancel() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
}

// ---- 脚本(源自 vps.txt,改为幂等) ----

const scriptWarpInstall = `
. /etc/os-release
case "${ID:-}" in ubuntu|debian) ;; *) echo "错误:WARP 安装只支持 Ubuntu/Debian(当前 ${ID:-unknown})"; exit 1;; esac
if command -v warp-cli >/dev/null 2>&1; then
  echo "warp-cli 已安装,跳过安装步骤"
else
  echo "[1/4] 安装依赖"
  apt-get update -y >/dev/null
  apt-get install -y ca-certificates curl gnupg >/dev/null
  echo "[2/4] 添加 Cloudflare 软件源"
  curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg | gpg --batch --yes --dearmor -o /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
  chmod 0644 /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ ${VERSION_CODENAME:-noble} main" > /etc/apt/sources.list.d/cloudflare-client.list
  chmod 0644 /etc/apt/sources.list.d/cloudflare-client.list
  echo "[3/4] 安装 cloudflare-warp"
  apt-get update -y >/dev/null
  apt-get install -y cloudflare-warp
fi
echo "[4/4] 启动 warp-svc 并注册"
systemctl enable --now warp-svc.service
registration_state=""
for _ in $(seq 1 30); do
  if out="$(warp-cli --accept-tos registration show 2>&1)"; then registration_state="existing"; break
  elif grep -Eqi 'missing registration|no existing registration|not registered' <<<"$out"; then registration_state="missing"; break
  elif grep -Eqi 'unable to connect|failed to connect|ipc|daemon|socket' <<<"$out"; then sleep 1
  else registration_state="error"; break; fi
done
case "$registration_state" in
  existing) echo "WARP 已注册,保留现有注册";;
  missing) warp-cli --accept-tos registration new && echo "注册完成";;
  *) echo "$out"; echo "错误:无法确认 WARP 注册状态"; exit 1;;
esac
echo "WARP 安装完成,下一步:启用 SOCKS5"
`

const scriptWarpEnable = `
command -v warp-cli >/dev/null 2>&1 || { echo "错误:未安装 warp-cli,请先安装 WARP"; exit 1; }
PORT={{PORT}}
if ss -H -ltnp "sport = :${PORT}" 2>/dev/null | grep -q . && ! ss -H -ltnp "sport = :${PORT}" 2>/dev/null | grep -q 'warp-svc'; then
  ss -H -ltnp "sport = :${PORT}"; echo "错误:端口 ${PORT} 已被其他进程占用"; exit 1
fi
cat >/usr/local/sbin/warp-socks5-start <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
PORT="${WARP_PROXY_PORT:-40000}"
TRACE_URL="${WARP_TRACE_URL:-https://www.cloudflare.com/cdn-cgi/trace}"
cli_ready=0
for _ in $(seq 1 30); do warp-cli --accept-tos status >/dev/null 2>&1 && { cli_ready=1; break; }; sleep 1; done
[ "$cli_ready" -eq 1 ] || exit 1
warp-cli --accept-tos tunnel protocol set MASQUE >/dev/null
warp-cli --accept-tos mode proxy >/dev/null
warp-cli --accept-tos proxy port "$PORT" >/dev/null
warp-cli --accept-tos connect >/dev/null
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  status="$(warp-cli --accept-tos status 2>&1 || true)"
  listeners="$(ss -H -ltn "sport = :${PORT}" | awk '{print $4}')"
  if grep -q 'Connected' <<<"$status" && grep -Fxq "127.0.0.1:${PORT}" <<<"$listeners"; then
    trace="$(curl -fsS --max-time 10 --proxy "socks5h://127.0.0.1:${PORT}" "$TRACE_URL" 2>/dev/null || true)"
    grep -Eq '^warp=(on|plus)$' <<<"$trace" && exit 0
  fi
  sleep 1
done
exit 1
EOF
chmod 0755 /usr/local/sbin/warp-socks5-start
cat >/etc/systemd/system/warp-socks5.service <<EOF
[Unit]
Description=Cloudflare WARP local SOCKS5 on 127.0.0.1:${PORT}
Wants=network-online.target warp-svc.service
After=network-online.target warp-svc.service

[Service]
Type=oneshot
Environment=WARP_PROXY_PORT=${PORT}
ExecStart=/usr/local/sbin/warp-socks5-start
RemainAfterExit=yes
TimeoutStartSec=150

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable warp-socks5.service >/dev/null
echo "启动 warp-socks5(最多等 90 秒)…"
if ! systemctl restart warp-socks5.service; then
  warp-cli --accept-tos status || true
  journalctl -u warp-svc.service -u warp-socks5.service -n 40 --no-pager || true
  echo "错误:WARP SOCKS5 启动失败"; exit 1
fi
listeners="$(ss -H -ltn "sport = :${PORT}" | awk '{print $4}')"
grep -Fxq "127.0.0.1:${PORT}" <<<"$listeners" || { echo "错误:WARP 没有监听 127.0.0.1:${PORT}"; exit 1; }
if grep -Ev "^(127\.0\.0\.1|\[::1\]):${PORT}$" <<<"$listeners" | grep -q .; then echo "$listeners"; echo "错误:检测到 WARP 监听在公网地址"; exit 1; fi
trace="$(curl -fsS --max-time 20 --proxy "socks5h://127.0.0.1:${PORT}" https://www.cloudflare.com/cdn-cgi/trace)"
grep -Eq '^warp=(on|plus)$' <<<"$trace" || { grep -E '^(ip|loc|colo|warp)=' <<<"$trace" || true; echo "错误:端口已监听但 WARP 出口验证失败"; exit 1; }
echo "WARP SOCKS5 已就绪:127.0.0.1:${PORT}"
grep -E '^(ip|loc|colo|warp)=' <<<"$trace"
`

const scriptWarpDisable = `
systemctl disable --now warp-socks5.service 2>/dev/null || true
command -v warp-cli >/dev/null 2>&1 && warp-cli --accept-tos disconnect || true
echo "WARP SOCKS5 已停用(warp-svc 仍保留,可随时再启用)"
`

const scriptWarpUninstall = `
systemctl disable --now warp-socks5.service 2>/dev/null || true
command -v warp-cli >/dev/null 2>&1 && warp-cli --accept-tos disconnect || true
systemctl disable --now warp-svc.service 2>/dev/null || true
apt-get remove -y cloudflare-warp || true
rm -f /etc/systemd/system/warp-socks5.service /usr/local/sbin/warp-socks5-start /etc/apt/sources.list.d/cloudflare-client.list /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
systemctl daemon-reload
echo "WARP 已卸载"
`

const scriptSwap = `
echo "== swap =="
if swapon --show 2>/dev/null | grep -q .; then
  echo "已有 swap,跳过"; swapon --show
else
  fallocate -l {{SWAP_GB}}G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=$(( {{SWAP_GB}} * 1024 ))
  chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo "已创建 {{SWAP_GB}}G swap"
fi
`

const scriptSysctl = `
echo "== sysctl / BBR =="
cat >/etc/sysctl.d/99-m-ui-tune.conf <<'EOF'
fs.file-max=1048576
net.core.somaxconn=65535
net.core.netdev_max_backlog=65535
net.ipv4.tcp_max_syn_backlog=65535
net.netfilter.nf_conntrack_max=524288
net.ipv4.ip_local_port_range=10240 65535
net.ipv4.tcp_fin_timeout=15
net.ipv4.tcp_tw_reuse=1
net.ipv4.tcp_fastopen=3
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
vm.swappiness=10
EOF
sysctl --system >/dev/null 2>&1 || true
echo "拥塞控制: $(cat /proc/sys/net/ipv4/tcp_congestion_control)  队列: $(cat /proc/sys/net/core/default_qdisc)"
`

const scriptLimits = `
echo "== limits =="
mkdir -p /etc/systemd/system/m-ui.service.d
cat >/etc/systemd/system/m-ui.service.d/override.conf <<'EOF'
[Service]
LimitNOFILE=1048576
LimitNPROC=1048576
TasksMax=infinity
EOF
systemctl daemon-reload
echo "已写入 m-ui.service override(重启 m-ui 后生效)"
`

const scriptNTP = `
echo "== NTP =="
if command -v timedatectl >/dev/null 2>&1; then
  timedatectl set-ntp true && echo "NTP 同步: $(timedatectl show -p NTPSynchronized --value)  时区: $(timedatectl show -p Timezone --value)"
else
  apt-get install -y chrony >/dev/null && systemctl enable --now chrony && echo "已安装 chrony"
fi
`

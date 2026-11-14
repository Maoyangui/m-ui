package ops

import (
	"os"
	"path/filepath"
	"strings"
)

// 「清理磁盘」动到的目录(测试里换成临时目录)。
var cleanDirs = struct {
	Log, Sbin, Units string
	Rotate           []string // 系统的 syslog 轮转规则(哪个在用哪个)
}{"/var/log", "/usr/local/sbin", "/etc/systemd/system", []string{"/etc/logrotate.d/rsyslog", "/etc/logrotate.d/syslog"}}

// 谷歌云 Ops Agent 的三个服务:它的指标采集器上报失败时把整段堆栈刷进 syslog,小盘的谷歌云机器一周就能被撑满。
var opsAgentUnits = []string{"google-cloud-ops-agent", "google-cloud-ops-agent-opentelemetry-collector", "google-cloud-ops-agent-fluent-bit"}

// OpsAgentActive 本机装了谷歌云 Ops Agent 且没被屏蔽(清理磁盘时才问要不要一起停用)。
func OpsAgentActive() bool {
	unit := opsAgentUnits[0] + ".service"
	installed := false
	for _, d := range []string{"/lib/systemd/system", "/usr/lib/systemd/system"} {
		if _, err := os.Stat(filepath.Join(d, unit)); err == nil {
			installed = true
		}
	}
	if !installed {
		return false
	}
	if dst, err := os.Readlink(filepath.Join(cleanDirs.Units, unit)); err == nil && dst == "/dev/null" {
		return false // 已经屏蔽
	}
	return true
}

func diskCleanScript(p Params) string {
	agent := "0"
	if p.OpsAgent {
		agent = "1"
	}
	return strings.NewReplacer("{{LOG_DIR}}", cleanDirs.Log, "{{SBIN_DIR}}", cleanDirs.Sbin, "{{UNIT_DIR}}", cleanDirs.Units,
		"{{OPS_AGENT}}", agent, "{{OPS_UNITS}}", strings.Join(opsAgentUnits, " "), "{{ROTATE_CONFS}}", strings.Join(cleanDirs.Rotate, " ")).Replace(scriptDiskClean)
}

// scriptDiskClean 清理磁盘:软件包缓存、journald 回收到上限、超过 100MB 的文本日志只留最后 5MB(原地改写,写它的服务不用重启)、
// 一天前轮转下来的旧日志;装一个每小时看一次的小定时器,syslog 超过 200MB 就按系统的 logrotate 规则立刻轮转;
// 可选停用谷歌云 Ops Agent。不碰 m-ui 的数据目录和备份。
const scriptDiskClean = `
echo "== 清理磁盘 =="
LOGDIR="{{LOG_DIR}}"; SBIN="{{SBIN_DIR}}"; UNITS="{{UNIT_DIR}}"
mui_df() { df -Pm / 2>/dev/null | awk 'NR==2{printf "已用 %s MB / 共 %s MB,可用 %s MB(%s)", $3, $2, $4, $5}'; }
mui_size() { stat -c %s "$1" 2>/dev/null || echo 0; }
echo "清理前:$(mui_df)"

# 1 软件包缓存(下载过的安装包,清掉不影响已装的软件)
if command -v apt-get >/dev/null 2>&1; then apt-get clean >/dev/null 2>&1 && echo "已清空 apt 软件包缓存"; fi
if command -v dnf >/dev/null 2>&1; then dnf clean packages >/dev/null 2>&1 && echo "已清空 dnf 软件包缓存"
elif command -v yum >/dev/null 2>&1; then yum clean packages >/dev/null 2>&1 && echo "已清空 yum 软件包缓存"; fi

# 2 journald 回收到上限(上限设置不动;想长期限住用「限制系统日志占用」)
if command -v journalctl >/dev/null 2>&1; then
  journalctl --vacuum-size={{JOURNAL_MB}}M >/dev/null 2>&1 || true
  echo "journald 回收到 {{JOURNAL_MB}}MB 以内,现在:$(journalctl --disk-usage 2>/dev/null | grep -o '[0-9.]*[KMGT]' | head -1)"
fi

# 3 超过 100MB 的文本日志只留最后 5MB
for f in "$LOGDIR/syslog" "$LOGDIR/messages" "$LOGDIR/daemon.log" "$LOGDIR/kern.log" "$LOGDIR/user.log" "$LOGDIR/cloud-init.log"; do
  [ -f "$f" ] || continue
  sz=$(mui_size "$f")
  if [ "$sz" -gt 104857600 ]; then
    if tail -c 5242880 "$f" > "$f.m-ui-tail" && cat "$f.m-ui-tail" > "$f"; then
      echo "截断 $f:原 $((sz / 1048576))MB,只留最后 5MB"
    fi
    rm -f "$f.m-ui-tail"
  fi
done

# 4 一天前轮转下来的旧日志(*.gz、*.1、*.old 这类;journal 目录不碰)
n=0; freed=0
while IFS= read -r -d '' f; do
  sz=$(mui_size "$f")
  rm -f "$f" && { n=$((n + 1)); freed=$((freed + sz)); }
done < <(find "$LOGDIR" -xdev -path "$LOGDIR/journal" -prune -o -type f \( -name '*.gz' -o -name '*.[0-9]' -o -name '*.[0-9].log' -o -name '*.old' \) -mtime +0 -print0 2>/dev/null)
echo "删掉轮转下来的旧日志 $n 个,共 $((freed / 1048576))MB"

# 5 syslog 按大小轮转:每小时看一次,超过 200MB 立刻按系统的 logrotate 规则轮转(报错刷屏时不至于一天就撑满)
conf=""
for c in {{ROTATE_CONFS}}; do [ -f "$c" ] && { conf="$c"; break; }; done
if command -v logrotate >/dev/null 2>&1 && command -v systemctl >/dev/null 2>&1 && [ -n "$conf" ] && [ -d "$UNITS" ]; then
  mkdir -p "$SBIN"
  cat > "$SBIN/m-ui-syslog-cap" <<EOF
#!/bin/sh
# 由 m-ui「清理磁盘」写入:syslog 超过 200MB 就按系统的 logrotate 规则立刻轮转。
# 撤销:systemctl disable --now m-ui-syslog-cap.timer
for f in $LOGDIR/syslog $LOGDIR/messages; do
  if [ -f "\$f" ] && [ "\$(stat -c %s "\$f")" -gt 209715200 ]; then exec logrotate -f $conf; fi
done
EOF
  chmod 755 "$SBIN/m-ui-syslog-cap"
  cat > "$UNITS/m-ui-syslog-cap.service" <<EOF
[Unit]
Description=m-ui: rotate syslog once it grows past 200MB

[Service]
Type=oneshot
ExecStart=$SBIN/m-ui-syslog-cap
EOF
  cat > "$UNITS/m-ui-syslog-cap.timer" <<EOF
[Unit]
Description=m-ui: check the syslog size every hour

[Timer]
OnBootSec=10min
OnUnitActiveSec=1h

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload >/dev/null 2>&1
  if systemctl enable --now m-ui-syslog-cap.timer >/dev/null 2>&1; then echo "已装 m-ui-syslog-cap.timer:每小时看一次,syslog 超过 200MB 就立刻轮转"; fi
fi

# 6 谷歌云 Ops Agent(选了才停):它上报失败时把整段堆栈刷进 syslog
if [ "{{OPS_AGENT}}" = "1" ]; then
  for u in {{OPS_UNITS}}; do
    systemctl disable --now "$u" >/dev/null 2>&1
    systemctl mask "$u" >/dev/null 2>&1
  done
  rm -rf "$LOGDIR/google-cloud-ops-agent/"* 2>/dev/null
  echo "已停用并屏蔽谷歌云 Ops Agent(要恢复:对这三个服务 systemctl unmask,再 systemctl enable --now google-cloud-ops-agent)"
fi

echo "清理后:$(mui_df)"
echo "m-ui 的数据目录和备份没有动"
`

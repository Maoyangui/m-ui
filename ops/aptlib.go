package ops

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// newTaskCmd 构造执行任务脚本的 bash 进程。取消(或超时)时先发 SIGTERM,让脚本的 trap 撤销临时改动
// (临时 DNS、/etc/hosts 临时行)并停掉 apt;30 秒内还没退出再强杀。
func newTaskCmd(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

// scriptLib 是每个任务脚本开头的公共函数:
//   - mui_apt:apt-get 一律带 DPkg::Lock::Timeout=300;等锁时说明是谁占着;运行期间每 15 秒报一次进度;
//     失败立即停下并给出中文原因,不再继续后面的步骤。
//   - DNS 兜底:apt 源里的主机(或 apt 输出里)解析不了时,systemd-resolved 的机器临时加
//     /etc/systemd/resolved.conf.d/m-ui-tmp-dns.conf(DNS=1.1.1.1 8.8.8.8)并重启 resolved;
//     管理员已有自己的备用 DNS 时不重复加、也不动它;不是 resolved(或这样还不行)时经 DoH 查出地址,
//     临时写进 /etc/hosts(行尾 # m-ui-tmp)。任务结束(成功、失败、取消)由 trap 全部撤销。
//     DNS 正常的机器上什么都不加。
//
// 开头几行路径变量是测试替换用的,改名要同步改 apt_script_test.go。
const scriptLib = `
# ---- 公共函数:apt 等锁说明 / 定时进度 / 失败即停;本机 DNS 解析不了软件源时临时改用公共 DNS,任务结束(成功、失败、取消)恢复 ----
MUI_HOSTS=/etc/hosts
MUI_RESOLV=/etc/resolv.conf
MUI_RESOLVED_D=/etc/systemd/resolved.conf.d
MUI_APT_DIR=/etc/apt
MUI_TICK=15
MUI_LOCKS="/var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock /var/cache/apt/archives/lock"
MUI_TAG="# m-ui-tmp"
MUI_TMP_CONF="$MUI_RESOLVED_D/m-ui-tmp-dns.conf"
MUI_CONF_ADDED=""; MUI_DIR_CREATED=""; MUI_HOSTS_ADDED=""; MUI_HOSTS_NONL=""; MUI_RESOLVED_TRIED=""
MUI_APT_PID=""; MUI_APT_OUT=""; MUI_DNS_OK=" "; MUI_DNS_BAD=""; MUI_SOFT=""

mui_restart_resolved() { systemctl restart systemd-resolved >/dev/null 2>&1 || true; }

# 删掉 /etc/hosts 里本任务加的临时行(原地改写,不换文件)
mui_hosts_strip() {
  local tmp
  tmp="$(mktemp)" || return 0
  grep -v -- " $MUI_TAG\$" "$MUI_HOSTS" > "$tmp"
  cat "$tmp" > "$MUI_HOSTS"
  rm -f "$tmp"
  if [ -n "$MUI_HOSTS_NONL" ] && [ -s "$MUI_HOSTS" ] && [ -z "$(tail -c1 "$MUI_HOSTS")" ]; then truncate -s -1 "$MUI_HOSTS"; fi
  return 0
}

mui_cleanup() {
  local rc=$?
  trap - EXIT
  if [ -n "$MUI_APT_PID" ]; then pkill -TERM -P "$MUI_APT_PID" 2>/dev/null; kill -TERM "$MUI_APT_PID" 2>/dev/null; MUI_APT_PID=""; fi
  if [ -n "$MUI_CONF_ADDED" ]; then
    rm -f "$MUI_TMP_CONF"
    if [ -n "$MUI_DIR_CREATED" ]; then rmdir "$MUI_RESOLVED_D" 2>/dev/null; fi
    mui_restart_resolved
    echo "已删除临时 DNS 设置,systemd-resolved 恢复原来的 DNS(temporary DNS removed, systemd-resolved restored)"
  fi
  if [ -n "$MUI_HOSTS_ADDED" ]; then
    mui_hosts_strip
    echo "已删除 /etc/hosts 里的临时解析(temporary /etc/hosts entries removed)"
  fi
  if [ -n "$MUI_APT_OUT" ]; then rm -f "$MUI_APT_OUT"; fi
  exit "$rc"
}
mui_on_signal() { echo "任务被取消,正在撤销临时改动…(task cancelled, reverting temporary changes)"; exit 143; }
trap mui_cleanup EXIT
trap mui_on_signal TERM INT HUP

# 上次任务被强行终止时留下的临时改动,先清掉
if [ -f "$MUI_TMP_CONF" ]; then
  rm -f "$MUI_TMP_CONF"; mui_restart_resolved
  echo "已清理上次任务残留的临时 DNS 设置(removed leftover temporary DNS from an earlier task)"
fi
if grep -q -- " $MUI_TAG\$" "$MUI_HOSTS" 2>/dev/null; then
  mui_hosts_strip
  echo "已清理上次任务残留的 /etc/hosts 临时解析(removed leftover temporary /etc/hosts entries)"
fi

mui_resolves() { timeout 15 getent ahosts "$1" >/dev/null 2>&1; }

mui_is_resolved() {
  case "$(readlink -f "$MUI_RESOLV" 2>/dev/null)" in
    */systemd/resolve/*) systemctl is-active --quiet systemd-resolved 2>/dev/null;;
    *) return 1;;
  esac
}

# 管理员自己加的 resolved 上游 DNS(drop-in 里的 DNS=),有就不重复加、也不动它
mui_admin_dns() {
  local f out=""
  for f in "$MUI_RESOLVED_D"/*.conf; do
    [ -f "$f" ] && [ "$f" != "$MUI_TMP_CONF" ] || continue
    if grep -qE '^[[:space:]]*DNS[[:space:]]*=[[:space:]]*[0-9A-Fa-f]' "$f"; then out="$out${out:+ }$f"; fi
  done
  printf '%s' "$out"
}

# 经 DoH(HTTPS 直连 IP,不依赖本机 DNS)查主机地址,最多两个
mui_doh() {
  local h="$1" t="$2" u j ips re='^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'
  command -v curl >/dev/null 2>&1 || return 0
  [ "$t" = AAAA ] && re='^[0-9A-Fa-f:]*:[0-9A-Fa-f:]*$'
  for u in "https://1.1.1.1/dns-query?name=$h&type=$t" "https://dns.google/resolve?name=$h&type=$t"; do
    j="$(curl -fsS --max-time 8 --resolve dns.google:443:8.8.8.8 -H 'accept: application/dns-json' "$u" 2>/dev/null)" || continue
    ips="$(printf '%s' "$j" | grep -oE '"data" *: *"[^"]*"' | sed -E 's/.*"([^"]*)"$/\1/' | grep -E "$re" | head -n 2)"
    if [ -n "$ips" ]; then echo $ips; return 0; fi
  done
  return 0
}

# 本机 DNS 解析不了这些主机时的兜底:先给 systemd-resolved 临时加公共 DNS,还不行再用 DoH 查到的地址临时写 /etc/hosts。
# 全部能解析返回 0;MUI_DNS_BAD 是仍解析不了的主机。
mui_dns_fix() {
  local h a ips fam bad="" have
  if [ -z "$MUI_RESOLVED_TRIED" ] && mui_is_resolved; then
    MUI_RESOLVED_TRIED=1
    have="$(mui_admin_dns)"
    if [ -n "$have" ]; then
      echo "本机 DNS 解析不了 $*;已有备用 DNS($have),不重复添加(local DNS cannot resolve $*; fallback DNS already configured in $have, left as is)"
    else
      if [ ! -d "$MUI_RESOLVED_D" ]; then mkdir -p "$MUI_RESOLVED_D" && MUI_DIR_CREATED=1; fi
      if printf '# m-ui 运维任务临时写入,任务结束自动删除\n[Resolve]\nDNS=1.1.1.1 8.8.8.8\n' > "$MUI_TMP_CONF"; then
        MUI_CONF_ADDED=1
        mui_restart_resolved
        echo "本机 DNS 解析不了 $*,已临时改用 1.1.1.1 / 8.8.8.8,任务结束后恢复(local DNS cannot resolve $*; using 1.1.1.1 / 8.8.8.8 until the task ends)"
        sleep 1
      fi
    fi
    for h in "$@"; do
      if mui_resolves "$h"; then MUI_DNS_OK="$MUI_DNS_OK$h "; else bad="$bad $h"; fi
    done
    MUI_DNS_BAD="${bad# }"
    [ -z "$bad" ] && return 0
    set -- $bad
    bad=""
  fi
  fam=A
  if command -v ip >/dev/null 2>&1 && [ -z "$(ip -4 route show default 2>/dev/null)" ] && [ -n "$(ip -6 route show default 2>/dev/null)" ]; then fam=AAAA; fi
  for h in "$@"; do
    ips="$(mui_doh "$h" "$fam")"
    if [ -z "$ips" ]; then bad="$bad $h"; continue; fi
    if [ -z "$MUI_HOSTS_ADDED" ] && [ -s "$MUI_HOSTS" ] && [ -n "$(tail -c1 "$MUI_HOSTS")" ]; then echo >> "$MUI_HOSTS"; MUI_HOSTS_NONL=1; fi
    MUI_HOSTS_ADDED=1
    for a in $ips; do printf '%s %s %s\n' "$a" "$h" "$MUI_TAG" >> "$MUI_HOSTS"; done
    echo "本机 DNS 解析不了 $h,已经 DoH 查到 $ips 并临时写进 /etc/hosts,任务结束后删除(local DNS cannot resolve $h; added $ips to /etc/hosts until the task ends)"
    if mui_resolves "$h"; then MUI_DNS_OK="$MUI_DNS_OK$h "; else bad="$bad $h"; fi
  done
  MUI_DNS_BAD="${bad# }"
  [ -z "$bad" ]
}

# 确认这些主机能解析,解析不了就兜底;兜底后仍不行返回 1
mui_dns_check() {
  local h bad=""
  for h in "$@"; do
    case "$MUI_DNS_OK" in *" $h "*) continue;; esac
    if mui_resolves "$h"; then MUI_DNS_OK="$MUI_DNS_OK$h "; else bad="$bad $h"; fi
  done
  [ -z "$bad" ] && return 0
  mui_dns_fix $bad
}

mui_die_dns() {
  echo "错误:解析不了 $*,请检查这台机器的 DNS 或网络(cannot resolve $*; check this server's DNS or network)"
  exit 1
}

# apt 源里的主机名
mui_apt_hosts() {
  {
    cat "$MUI_APT_DIR"/sources.list "$MUI_APT_DIR"/sources.list.d/*.list 2>/dev/null | grep -E '^[[:space:]]*deb(-src)?[[:space:]]'
    cat "$MUI_APT_DIR"/sources.list.d/*.sources 2>/dev/null | grep -E '^[[:space:]]*URIs:'
  } | grep -oE '[a-z+]*https?://[^/[:space:]]+' | sed -E 's#^[^/]*//##; s#^.*@##; s#:[0-9]+$##' | grep -E '^[A-Za-z0-9.-]+$' | sort -u
}

# 谁占着 apt/dpkg 锁:"进程名(PID n)"
mui_lock_holder() {
  local p n out="" pids=""
  command -v fuser >/dev/null 2>&1 && pids="$(fuser $MUI_LOCKS 2>/dev/null | grep -oE '[0-9]+' | sort -un)"
  for p in $pids; do
    n="$(cat "/proc/$p/comm" 2>/dev/null || ps -o comm= -p "$p" 2>/dev/null)"
    out="$out${out:+, }${n:-?}(PID $p)"
  done
  printf '%s' "$out"
}

# 后台跑 apt-get,每 MUI_TICK 秒报一次进度;返回 apt-get 的退出码
mui_apt_run() {
  local zh="$1" en="$2" start=$SECONDS next=$MUI_TICK el said="" holder rc
  shift 2
  [ -n "$MUI_APT_OUT" ] || MUI_APT_OUT="$(mktemp)"
  : > "$MUI_APT_OUT"
  holder="$(mui_lock_holder)"
  if [ -n "$holder" ]; then
    said=1
    echo "apt/dpkg 正被 $holder 占用(新机开机后的自动更新常见),最多等 300 秒…(apt/dpkg is locked by $holder; waiting up to 300s)"
  fi
  command apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$@" > "$MUI_APT_OUT" 2>&1 < /dev/null &
  MUI_APT_PID=$!
  while kill -0 "$MUI_APT_PID" 2>/dev/null; do
    sleep 1
    el=$((SECONDS - start))
    if [ -z "$said" ] && grep -qE 'Waiting for cache lock|Could not get lock' "$MUI_APT_OUT"; then
      said=1
      holder="$(tr '\r' '\n' < "$MUI_APT_OUT" | grep -oE 'held by process [0-9]+( \([^)]*\))?' | tail -n 1 | sed -E 's/held by process ([0-9]+) ?\(?([^)]*)\)?/\2(PID \1)/')"
      echo "apt/dpkg 正被 ${holder:-其它程序} 占用,最多等 300 秒…(apt/dpkg is locked by ${holder:-another process}; waiting up to 300s)"
    fi
    if [ "$el" -ge "$next" ]; then
      next=$((next + MUI_TICK))
      if tr '\r' '\n' < "$MUI_APT_OUT" | grep -v '^[[:space:]]*$' | tail -n 1 | grep -qE 'Waiting for cache lock|Could not get lock'; then
        echo "仍在等待 apt 锁…已 $el 秒(still waiting for the apt lock, ${el}s)"
      else
        echo "仍在$zh…已 $el 秒(still $en, ${el}s)"
      fi
    fi
  done
  wait "$MUI_APT_PID"
  rc=$?
  MUI_APT_PID=""
  return $rc
}

mui_apt_unresolved() {
  tr '\r' '\n' < "$MUI_APT_OUT" | grep -oE "(Temporary failure resolving|Could not resolve( host:)?) '?[A-Za-z0-9.-]+" | awk '{print $NF}' | tr -d "'" | sort -u | tr '\n' ' '
}

mui_apt_tail() { tr '\r' '\n' < "$MUI_APT_OUT" | grep -v '^[[:space:]]*$' | tail -n 15 | sed 's/^/  | /'; }

mui_apt_reason() {
  local o="$MUI_APT_OUT"
  if grep -qE 'Could not get lock|Unable to acquire the dpkg' "$o"; then echo "等了 300 秒 apt/dpkg 仍被其它程序占用(多半是系统自动更新),请过几分钟再试(apt/dpkg still locked after 300s, try again in a few minutes)"
  elif grep -q 'No space left' "$o"; then echo "磁盘空间不足(no space left on disk)"
  elif grep -q 'dpkg was interrupted' "$o"; then echo "上次安装被中断,请先执行 sudo dpkg --configure -a(an earlier install was interrupted; run sudo dpkg --configure -a first)"
  elif grep -qE 'Unable to locate package|has no installation candidate' "$o"; then echo "软件源里找不到要装的包,可能是软件源没更新成功或系统版本不受支持(package not found in the repositories)"
  elif grep -qE 'unmet dependencies|Depends:' "$o"; then echo "依赖装不上,多半是软件源没更新成功(unmet dependencies)"
  elif grep -qE 'NO_PUBKEY|is not signed|EXPKEYSIG|BADSIG' "$o"; then echo "软件源签名校验失败(repository signature check failed)"
  elif grep -qE 'Failed to fetch|Unable to fetch|Could not connect|Connection failed|Connection timed out' "$o"; then echo "下载失败,请检查这台机器的网络或软件源(download failed, check network or repositories)"
  else echo "见上面 apt 的输出(see the apt output above)"; fi
}

# 用法:mui_apt <中文说明> <英文说明> <apt-get 参数…>;失败立即停下(exit 1)并说明原因
mui_apt() {
  local zh="$1" en="$2" sub="" a rc bad w try=0
  shift 2
  for a in "$@"; do case "$a" in -*) ;; *) sub="$a"; break;; esac; done
  case "$sub" in
    update|install|upgrade|dist-upgrade|full-upgrade|reinstall)
      mui_dns_check $(mui_apt_hosts) || mui_die_dns "$MUI_DNS_BAD";;
  esac
  echo "$zh…($en)"
  while :; do
    try=$((try + 1))
    mui_apt_run "$zh" "$en" "$@"
    rc=$?
    bad="$(mui_apt_unresolved)"
    [ -z "$bad" ] && break
    if [ "$try" -ge 2 ] || ! mui_dns_fix $bad; then
      mui_apt_tail
      mui_die_dns ${MUI_DNS_BAD:-$bad}
    fi
    echo "重新$zh…(retrying: $en)"
  done
  if [ "$rc" -ne 0 ]; then
    mui_apt_tail
    echo "错误:${zh}失败(apt-get 退出码 $rc):$(mui_apt_reason)"
    [ -n "$MUI_SOFT" ] && return 1
    exit 1
  fi
  w="$(grep -E '^(W|E): ' "$MUI_APT_OUT" | head -n 6)"
  [ -n "$w" ] && echo "$w" | sed 's/^/  /'
  a="$(grep -cE '^Setting up ' "$MUI_APT_OUT")"
  w="$(grep -oE '^Setting up [^ ]+' "$MUI_APT_OUT" | awk '{print $3}' | sed 's/:[a-z0-9]*$//' | head -n 10)"
  if [ "$a" -gt 10 ]; then echo "  已安装/配置 $a 个包(set up $a packages):" $w "…"
  elif [ -n "$w" ]; then echo "  已安装/配置(set up):" $w; fi
  return 0
}

# 同 mui_apt,但失败只报告、不中止任务(卸载等收尾步骤用)
mui_apt_soft() {
  MUI_SOFT=1
  mui_apt "$@"
  local r=$?
  MUI_SOFT=""
  return $r
}
`

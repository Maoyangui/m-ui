#!/usr/bin/env bash
# m-ui 安装/升级脚本(Linux amd64/arm64,systemd)
#
#   bash install.sh [latest | vX.Y.Z | 二进制/压缩包路径 | 下载 URL] [--db /etc/m-ui/m-ui.db] [--restore backup.zip] [--import 旧面板.db]
#                   [--open-firewall | --keep-firewall]
#   bash install.sh --dry-run              只打印会做什么(装到哪、写哪些文件、默认端口),不改任何东西
#   bash install.sh --uninstall [--purge]  停止并删除服务与程序;不带 --purge 时保留 /etc/m-ui(数据库、证书、备份)
#
#   不带来源参数 = 从 GitHub Releases 安装最新版(自动识别 amd64 / arm64)。
#
# 幂等:重复执行即升级二进制并重启;数据库与证书不动。
#
# 系统防火墙(只在首次安装时):这台机器上要是有系统自带的防火墙在拦入站(ufw、firewalld、Oracle 等镜像带的 iptables 规则),
# 云厂商控制台放行了端口、外面照样连不上。装完会问一句要不要放开它 —— 直接回车或 y 放开,n 保留;
# 没有终端可问(cloud-init、CI)按放开算。--open-firewall / MUI_OPEN_FIREWALL=1 不问直接放开,
# --keep-firewall / MUI_KEEP_FIREWALL=1 不问、保留(m-ui 用到的端口会自动在它那里放行)。
# 放开是可逆的:改之前的规则备份在数据目录的 fw-backup-<时间>/,m-ui firewall undo 或面板「防火墙」页随时恢复。升级不问、不动。
# 升级带回滚(与面板里的一键更新同一套语义):旧程序留作 m-ui.prev,先用旧程序做一份升级前备份;
# 新版本 90 秒内没把面板服务起来就换回旧程序,旧程序也起不来再还原那份备份。回滚结果写进
# <数据目录>/upgrade-status.json,面板会在页面顶部提示。
set -euo pipefail

# ---- 更新包签名 ----
# v0.8.1 起的 Release 里,校验清单 SHA256SUMS 带发布者的签名(同一把 Ed25519 钥匙签两种写法):
#   SHA256SUMS.sshsig  SSH 签名格式,用 ssh-keygen -Y verify 验(OpenSSH 8.1 起有)
#   SHA256SUMS.sig     64 字节签名的 base64,面板内更新用它;这里在没有新 ssh-keygen 时用 OpenSSL 3 验
# 只比 SHA256SUMS 防不了被换:能换掉程序的人同样能换掉那份清单。公钥和面板内更新内置的是同一把(selfupdate/sign.go)。
# 两种工具都没有的老系统验不了签名:提示一句,只做 SHA256 校验(面板内更新不受影响,它自己验)。
RELEASE_KEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIApvcuQzwhE4aH+lWEisKYQnKMJRWsi1Z4idd3DFsCru"
RELEASE_KEY_PEM="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEACm9y5DPCEThof6VYSKwphCcowlFayLVniJ13cMWwKu4=
-----END PUBLIC KEY-----"

# signed_release <标签>:这个版本发布时带不带签名(v0.8.1 起都带)
signed_release() {
  local v="${1#v}"
  [ "$(printf '%s\n' "0.8.1" "$v" | sort -V | head -n1)" = "0.8.1" ]
}

# verify_sums <SHA256SUMS 文件> [<.sshsig 文件>] [<.sig 文件>]
#   返回 0 = 签名验过;1 = 验不过(不是发布者签的,或文件被动过);2 = 验不了(没给签名文件,或这台系统没有能验的工具)
verify_sums() {
  local sums="$1" sshsig="${2:-}" rawsig="${3:-}" tmp help=""
  # ssh-keygen 有没有 -Y verify:看它的用法说明(先收进变量再比:它打印用法时退出码不是 0,开着 pipefail 直接接管道会被当成失败)
  if [ -n "$sshsig" ] && [ -s "$sshsig" ] && command -v ssh-keygen >/dev/null 2>&1; then help="$(ssh-keygen -? 2>&1 || true)"; fi
  if [[ "$help" == *"-Y verify"* ]]; then
    tmp="$(mktemp)"
    printf 'm-ui-release %s\n' "$RELEASE_KEY" > "$tmp"
    if ssh-keygen -Y verify -f "$tmp" -I m-ui-release -n m-ui-release -s "$sshsig" < "$sums" >/dev/null 2>&1; then rm -f "$tmp"; return 0; fi
    rm -f "$tmp"; return 1
  fi
  if [ -n "$rawsig" ] && [ -s "$rawsig" ] && command -v openssl >/dev/null 2>&1 && openssl version 2>/dev/null | grep -Eq '^OpenSSL ([3-9]|[1-9][0-9])\.'; then
    tmp="$(mktemp -d)"
    printf '%s\n' "$RELEASE_KEY_PEM" > "$tmp/pub.pem"
    if base64 -d "$rawsig" > "$tmp/sig.bin" 2>/dev/null && openssl pkeyutl -verify -pubin -inkey "$tmp/pub.pem" -rawin -in "$sums" -sigfile "$tmp/sig.bin" >/dev/null 2>&1; then rm -rf "$tmp"; return 0; fi
    rm -rf "$tmp"; return 1
  fi
  return 2
}

# bash install.sh --verify-sums <SHA256SUMS> [<SHA256SUMS.sshsig>] [<SHA256SUMS.sig>]
#   只用上面这套办法验一下本地的文件(发版流程发布前自检、排查时用),不安装任何东西。退出码同 verify_sums。
if [ "${1:-}" = "--verify-sums" ]; then
  [ -n "${2:-}" ] && [ -f "$2" ] || { echo "用法: bash install.sh --verify-sums <SHA256SUMS> [<SHA256SUMS.sshsig>] [<SHA256SUMS.sig>]"; exit 2; }
  rc=0; verify_sums "$2" "${3:-}" "${4:-}" || rc=$?
  case "$rc" in
    0) echo "签名校验通过";;
    1) echo "签名校验失败:不是发布者签的,或文件被动过";;
    *) echo "验不了:没给签名文件,或这台系统没有能验的工具(需要 OpenSSH 8.1+ 的 ssh-keygen,或 OpenSSL 3+)";;
  esac
  exit "$rc"
fi

REPO="${M_UI_REPO:-Maoyangui/m-ui}" # GitHub 仓库 owner/name;换仓库改这里或设环境变量 M_UI_REPO
SRC=""
if [ $# -gt 0 ] && [[ "$1" != --* ]]; then SRC="$1"; shift; fi
DB="/etc/m-ui/m-ui.db"
RESTORE=""
IMPORT=""
DRY=0
UNINSTALL=0
PURGE=0
FW="ask" # 系统防火墙:ask 问一句 / open 不问直接放开 / keep 不问、保留
[ "${MUI_OPEN_FIREWALL:-}" = "1" ] && FW="open"
[ "${MUI_KEEP_FIREWALL:-}" = "1" ] && FW="keep"
while [ $# -gt 0 ]; do
  case "$1" in
    --open-firewall) FW="open"; shift;;
    --keep-firewall) FW="keep"; shift;;
    --db) DB="$2"; shift 2;;
    --restore) RESTORE="$2"; shift 2;;
    --import) IMPORT="$2"; shift 2;;
    --dry-run) DRY=1; shift;;
    --uninstall) UNINSTALL=1; shift;;
    --purge) PURGE=1; shift;;
    *) echo "未知参数 $1"; exit 2;;
  esac
done
BIN=/usr/local/bin/m-ui
DATA_DIR="$(dirname "$DB")"

# 卸载:与面板菜单里的"卸载"一致 —— 停服务、删单元与程序;数据目录默认保留,--purge 才删
if [ "$UNINSTALL" = 1 ]; then
  [ "$DRY" = 1 ] || [ "$(id -u)" -eq 0 ] || { echo "请以 root 运行"; exit 1; }
  if [ "$DRY" = 1 ]; then
    echo "[dry-run] 将执行:systemctl disable --now m-ui;删除 /etc/systemd/system/m-ui.service 与 $BIN(以及 .prev / .failed),以及运维页写过的 journald / sysctl / service override 文件"
    [ "$PURGE" = 1 ] && echo "[dry-run] --purge:同时删除 $DATA_DIR(数据库、证书、备份)" || echo "[dry-run] 保留 $DATA_DIR(数据库、证书、备份)"
    exit 0
  fi
  systemctl disable --now m-ui 2>/dev/null || true
  rm -f /etc/systemd/system/m-ui.service
  systemctl daemon-reload 2>/dev/null || true
  rm -f "$BIN" "$BIN.prev" "$BIN.failed"
  # 运维页写过的系统文件:journald 上限、内核参数、服务的句柄上限 override
  rm -f /etc/systemd/journald.conf.d/99-m-ui.conf /etc/sysctl.d/99-m-ui-tune.conf
  rm -rf /etc/systemd/system/m-ui.service.d
  systemctl daemon-reload 2>/dev/null || true
  if [ "$PURGE" = 1 ]; then rm -rf "$DATA_DIR"; echo "m-ui 已卸载,数据目录 $DATA_DIR 已删除"; else echo "m-ui 已卸载;数据目录 $DATA_DIR 已保留(数据库、证书、备份),重新安装即可恢复"; fi
  exit 0
fi

if [ "$DRY" = 1 ]; then
  echo "[dry-run] 不会修改任何文件。检查环境:"
  [ "$(id -u)" -eq 0 ] && echo "  root:是" || echo "  root:否(真正安装需要 root)"
  command -v systemctl >/dev/null && echo "  systemd:有" || echo "  systemd:无(真正安装需要 systemd)"
  command -v curl >/dev/null && echo "  curl:有" || echo "  curl:无(真正安装需要 curl)"
else
  [ "$(id -u)" -eq 0 ] || { echo "请以 root 运行"; exit 1; }
  command -v systemctl >/dev/null || { echo "需要 systemd"; exit 1; }
  command -v curl >/dev/null || { echo "需要 curl"; exit 1; }
fi

# ---- 升级事务用的三个小函数(和 selfupdate 包里的 upgrade-watch 同一套判断)----

# 面板在本机的地址:端口、路径、是否 HTTPS 都按数据库里的设置来;取不到就为空,健康检查退化为"服务持续 active"
panel_url() {
  # 纯 IPv6 机器的地址带方括号(http://[2001:db8::1]:2053/app/),主机部分整段换成 127.0.0.1
  "$1" info -db "$DB" 2>/dev/null | grep -oE 'https?://[][A-Za-z0-9.:/_-]+' | head -1 | sed -E 's#^(https?://)(\[[^]/]*\]|[^]/:[]+)(:[0-9]+)?#\1127.0.0.1\3#' || true
}

# healthy <秒数>:服务 active 且面板首页回 2xx/3xx;没有地址时要求连续 8 秒 active 且期间没被 systemd 自动拉起过
healthy() {
  local i code nr0
  nr0="$(systemctl show -p NRestarts --value m-ui 2>/dev/null || echo 0)"
  for i in $(seq 1 "$1"); do
    if systemctl is-active --quiet m-ui; then
      if [ -n "${HEALTH_URL:-}" ]; then
        code="$(curl -ks -o /dev/null -w '%{http_code}' --max-time 3 "$HEALTH_URL" 2>/dev/null || echo 000)"
        if [ "$code" -ge 200 ] 2>/dev/null && [ "$code" -lt 400 ]; then
          # 首页会应答只说明进程活着;api/health(只对本机开放)还看数据面把线路都起来了没有,503 = 没起来
          hc="$(curl -ks -o /dev/null -w '%{http_code}' --max-time 3 "${HEALTH_URL%/}/api/health" 2>/dev/null || echo 000)"
          [ "$hc" = 503 ] || return 0
        fi
      elif [ "$i" -ge 8 ] && [ "$(systemctl show -p NRestarts --value m-ui 2>/dev/null || echo 0)" = "$nr0" ]; then
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

# write_status <rolledBack> <dbRestored> <healthy> <message>:结果文件,面板读到"回滚过"会在页面顶部提示
write_status() {
  printf '{"at":%s,"from":"v%s","to":"v%s","ok":false,"rolledBack":%s,"dbRestored":%s,"healthy":%s,"backup":"%s","failed":"%s","message":"%s"}\n' \
    "$(date +%s)" "${OLDVER:-?}" "${NEWVER:-?}" "$1" "$2" "$3" "${BK:-}" "$BIN.failed" "$4" > "$DATA_DIR/upgrade-status.json" 2>/dev/null || true
  printf '%s v%s → v%s rolledBack=%s dbRestored=%s healthy=%s %s\n' "$(date -Is)" "${OLDVER:-?}" "${NEWVER:-?}" "$1" "$2" "$3" "$4" >> "$DATA_DIR/upgrade.log" 2>/dev/null || true
}

# 不带来源 / latest / vX.Y.Z:从 GitHub Releases 取对应架构的压缩包
if [ -z "$SRC" ] || [ "$SRC" = "latest" ] || [[ "$SRC" == v[0-9]* ]]; then
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64) ARCH=amd64;;
    aarch64|arm64) ARCH=arm64;;
    *) echo "不支持的架构: $ARCH(仅 amd64 / arm64)"; exit 1;;
  esac
  if [ -z "$SRC" ] || [ "$SRC" = "latest" ]; then
    # 先走 releases/latest 的重定向拿标签(不经 API,无限流);拿不到再查 API
    TAG="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null | sed -n 's#.*/tag/##p')"
    if [ -z "$TAG" ]; then
      TAG="$(curl -fsSL -H "User-Agent: m-ui-install" ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)"
    fi
    [ -n "$TAG" ] || { echo "无法从 GitHub 获取最新版本(网络不通、还没有 Release,或仓库为私有;私有仓库请设置 GITHUB_TOKEN,或直接传压缩包路径 / 下载地址)"; exit 1; }
  else
    TAG="$SRC"
  fi
  SRC="https://github.com/$REPO/releases/download/$TAG/m-ui-linux-$ARCH.tar.gz"
  SUMS="https://github.com/$REPO/releases/download/$TAG/SHA256SUMS"
  echo "安装 m-ui $TAG ($ARCH)"
fi

if [ "$DRY" = 1 ]; then
  echo "[dry-run] 计划:"
  echo "  来源:$SRC"
  [ -n "${SUMS:-}" ] && echo "  校验:$SUMS(SHA256 不一致则中止)"
  echo "  程序:$BIN(已存在则覆盖,先停服务)"
  echo "  数据:$DB;目录 $DATA_DIR、$DATA_DIR/cert、$DATA_DIR/backups(已存在的数据库、证书不动)"
  echo "  服务:/etc/systemd/system/m-ui.service(root 运行 m-ui run -db $DB,Restart=always),然后 systemctl enable --now m-ui"
  echo "  端口:面板 2053/tcp 路径 /app/,订阅 2056/tcp,代理面板 2054/tcp(首次安装的默认值,面板里可改)"
  echo "  首次安装账号:admin / admin(登录后请改)"
  if [ -f "$DB" ]; then
    echo "  这是升级:旧程序留作 $BIN.prev,先做升级前备份到 $DATA_DIR/backups/pre-upgrade-*.zip(只留最近两份);"
    echo "          新版本 90 秒内起不来就自动换回旧程序,仍起不来再还原那份备份,结果写 $DATA_DIR/upgrade-status.json"
  fi
  if [ ! -f "$DB" ]; then
    case "$FW" in
      open) echo "  系统防火墙:要是有 ufw / firewalld / 镜像自带的 iptables 规则在拦入站,直接放开(可逆,先备份;m-ui firewall undo 恢复)";;
      keep) echo "  系统防火墙:保留不动;m-ui 用到的端口会自动在它那里放行";;
      *) echo "  系统防火墙:要是有 ufw / firewalld / 镜像自带的 iptables 规则在拦入站,装完会问一句要不要放开(回车 / y = 放开,n = 保留;没有终端可问时按放开算)";;
    esac
  else
    echo "  系统防火墙:升级不问、不动"
  fi
  echo "  不会碰:系统里已有的 sing-box / xray 等程序与它们的配置、其它服务"
  [ -n "$RESTORE" ] && echo "  还原备份:$RESTORE → $DB"
  [ -n "$IMPORT" ] && echo "  导入旧面板库:$IMPORT → $DB"
  exit 0
fi

TMP="$(mktemp)"
case "$SRC" in
  http://*|https://*) echo "下载 $SRC"; curl -fL --progress-bar "$SRC" -o "$TMP";;
  *) cp "$SRC" "$TMP";;
esac
# 官方 Release:先验校验清单的签名(v0.8.1 起的版本),再用清单比 SHA256,通不过就不装
# (能拦住 SHA256SUMS 这个请求的人也能换掉二进制,所以取不到摘要、取不到签名同样中止;
#  确实需要绕过时显式 MUI_SKIP_VERIFY=1)
if [ -n "${SUMS:-}" ] && [ "${MUI_SKIP_VERIFY:-}" != "1" ]; then
  if command -v sha256sum >/dev/null; then SUM="sha256sum"
  elif command -v shasum >/dev/null; then SUM="shasum -a 256"
  else echo "没有 sha256sum,无法校验下载内容,已中止(装 coreutils 后重试)"; rm -f "$TMP"; exit 1; fi
  VDIR="$(mktemp -d)"
  curl -fsSL "$SUMS" -o "$VDIR/SHA256SUMS" 2>/dev/null || { echo "取不到这个版本的 SHA256SUMS,无法校验,已中止"; rm -rf "$TMP" "$VDIR"; exit 1; }
  if signed_release "${TAG:-}"; then
    curl -fsSL "$SUMS.sshsig" -o "$VDIR/sshsig" 2>/dev/null || rm -f "$VDIR/sshsig"
    curl -fsSL "$SUMS.sig" -o "$VDIR/sig" 2>/dev/null || rm -f "$VDIR/sig"
    # 两种签名文件每个版本都发:缺哪一个都中止 —— 不然把这台系统验得了的那一种拿掉,就退成「验不了、只比 SHA256」了
    if [ ! -s "$VDIR/sshsig" ] || [ ! -s "$VDIR/sig" ]; then
      echo "取不到这个版本的签名(SHA256SUMS.sig / SHA256SUMS.sshsig),无法确认它是发布者发的,已中止"; rm -rf "$TMP" "$VDIR"; exit 1
    fi
    VRC=0; verify_sums "$VDIR/SHA256SUMS" "$VDIR/sshsig" "$VDIR/sig" || VRC=$?
    case "$VRC" in
      0) echo "签名校验通过";;
      1) echo "签名校验失败:这个版本的校验清单不是发布者签的(或下载被动过),已中止"; rm -rf "$TMP" "$VDIR"; exit 1;;
      *) echo "提示:这台系统没有能验签名的工具(需要 OpenSSH 8.1+ 的 ssh-keygen,或 OpenSSL 3+),这次只做 SHA256 校验";;
    esac
  fi
  # 清单一行是「摘要  文件名」;文件名前带 * 的写法(sha256sum 的二进制标记)也认,和面板内更新一样
  WANT="$(awk -v f="m-ui-linux-$ARCH.tar.gz" '{ n = $2; sub(/^\*/, "", n); if (NF == 2 && n == f) { print tolower($1); exit } }' "$VDIR/SHA256SUMS")"
  rm -rf "$VDIR"
  [ -n "$WANT" ] || { echo "这个版本的 SHA256SUMS 里没有 m-ui-linux-$ARCH.tar.gz,无法校验,已中止"; rm -f "$TMP"; exit 1; }
  GOT="$($SUM "$TMP" | awk '{print $1}')"
  [ "$WANT" = "$GOT" ] || { echo "校验失败:下载的文件与 Release 的 SHA256 不一致,已中止"; rm -f "$TMP"; exit 1; }
  echo "SHA256 校验通过"
fi
# Releases 里是 tar.gz(内含单文件 m-ui),也接受裸二进制
if file "$TMP" 2>/dev/null | grep -qi gzip || [[ "$SRC" == *.tar.gz ]]; then
  EXTRACT="$(mktemp -d)"
  # 只解出 m-ui 这一个成员:压缩包里就算有 ../ 之类的路径也不会落到别处
  tar -xzf "$TMP" -C "$EXTRACT" m-ui || { echo "压缩包里没有 m-ui"; rm -rf "$EXTRACT"; rm -f "$TMP"; exit 1; }
  mv "$EXTRACT/m-ui" "$TMP"
  rm -rf "$EXTRACT"
fi
chmod 0700 "$TMP" # 临时文件只给 root 执行
"$TMP" version >/dev/null || { echo "二进制无法执行(架构不符?)"; exit 1; }
NEWVER="$("$TMP" version 2>/dev/null | awk '{print $2}' || true)"

mkdir -p "$DATA_DIR" "$DATA_DIR/cert" "$DATA_DIR/backups"
FRESH=0; [ -f "$DB" ] || FRESH=1

# ---- 升级前:留旧程序、做备份、记下面板本机地址 ----
PREV=""; OLDVER=""; BK=""; HEALTH_URL=""
if [ "$FRESH" = 0 ] && [ -x "$BIN" ]; then
  OLDVER="$("$BIN" version 2>/dev/null | awk '{print $2}' || true)"
  HEALTH_URL="$(panel_url "$BIN")"
  cp -p "$BIN" "$BIN.prev" && PREV="$BIN.prev"
  BK="$DATA_DIR/backups/pre-upgrade-${OLDVER:-unknown}-$(date +%Y%m%d-%H%M%S).zip"
  if "$BIN" backup -db "$DB" -out "$BK" >/dev/null 2>&1; then
    echo "升级前备份:$BK"
    ls -1t "$DATA_DIR"/backups/pre-upgrade-*.zip 2>/dev/null | tail -n +3 | xargs -r rm -f
  else
    BK=""; echo "升级前备份失败(继续;回滚时只换回旧程序)"
  fi
fi

if systemctl is-active --quiet m-ui; then
  # 主机开着主机容灾时先暂停接任 15 分钟:升级重启那一两分钟备用别误判主机失联(老版本没有这个命令,忽略)
  "$BIN" pause-takeover -db "$DB" 15 2>/dev/null || true
  systemctl stop m-ui
fi
install -m 0755 "$TMP" "$BIN"
rm -f "$TMP"

if [ -n "$RESTORE" ]; then
  echo "还原备份 $RESTORE → $DB"
  "$BIN" restore -db "$DB" -from "$RESTORE"
elif [ -n "$IMPORT" ]; then
  echo "从旧面板数据库导入 $IMPORT → $DB"
  "$BIN" import -from "$IMPORT" -to "$DB"
fi
# 新程序按库里的设置(含监听地址)给健康检查地址:老程序只会写 127.0.0.1,面板只监听某个地址时好好的新版本会被误判回滚
U="$("$BIN" health-url -db "$DB" 2>/dev/null | grep -E '^https?://' | head -1 || true)"
[ -z "$U" ] || HEALTH_URL="$U"

cat >/etc/systemd/system/m-ui.service <<EOF
[Unit]
Description=m-ui panel and data plane (sing-box)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$DATA_DIR
ExecStart=$BIN run -db $DB
Restart=always
RestartSec=2
LimitNOFILE=1048576
LimitNPROC=1048576
TasksMax=infinity

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now m-ui
[ -n "$HEALTH_URL" ] || HEALTH_URL="$(panel_url "$BIN")"

# ---- 系统防火墙(只在首次安装时问;升级不动)----
# 判断和动手都交给程序自己(m-ui firewall …):和面板「防火墙」页的「放开系统防火墙」是同一个动作,记在同一个地方,哪边都能撤销。
FW_NOTE=""
firewall_step() {
  local kind ans="" tries=0 wait=120
  kind="$("$BIN" firewall detect 2>/dev/null | head -1 || true)"
  [ -n "$kind" ] || return 0 # 没有在拦入站的系统防火墙
  if [ "$FW" = "ask" ]; then
    if { : </dev/tty; } 2>/dev/null; then
      echo
      echo "这台机器的系统防火墙($kind)开着:没在它那里放行的端口,从外面连不上 —— 云厂商控制台放行了也一样。"
      echo "放开它吗?放开是可逆的(先备份,m-ui firewall undo 或面板「防火墙」页随时恢复);真要挡端口,请在云厂商的安全组里设。"
      echo "  回车 / y = 放开        n = 保留(m-ui 用到的端口会自动在它那里放行)"
      # 经 sudo 跑、而且脚本或输出走的是管道(curl … | sudo bash、sudo bash install.sh | tee):新版 sudo 默认给命令另开一个终端,
      # 这种情况下键盘输入传不进来,等也白等 —— 实测要干等满两分钟。只等 15 秒,并且把话说在前面
      if [ -n "${SUDO_USER:-}" ] && { [ ! -t 0 ] || [ ! -t 1 ]; }; then
        wait=15
        echo "  (这次是经 sudo 的管道运行的,键盘输入可能传不进来:$wait 秒内没有回答就按放开算;想保留,装完执行 m-ui firewall undo 即可恢复)"
      fi
      while :; do
        printf "[Y/n]: "
        ans=""
        read -r -t "$wait" ans </dev/tty || { echo; echo "(没有回答,按默认:放开)"; ans="y"; }
        case "$ans" in
          ""|y|Y|yes|YES|Yes) FW="open"; break;;
          n|N|no|NO|No) FW="keep"; break;;
        esac
        tries=$((tries + 1))
        [ "$tries" -lt 3 ] || { echo "(没看懂,按默认:放开)"; FW="open"; break; }
        echo "请输入 y 或 n(直接回车 = y)"
      done
    else
      FW="open" # 没有终端可问(cloud-init、CI、管道里跑):按默认
    fi
  fi
  if [ "$FW" = "keep" ]; then
    FW_NOTE="  系统防火墙($kind)保留着:m-ui 用到的端口会自动在它那里放行;以后想放开:m-ui firewall open,或面板「防火墙」页"
    return 0
  fi
  if "$BIN" firewall open -db "$DB"; then
    if [ -z "$("$BIN" firewall detect 2>/dev/null | head -1 || true)" ]; then
      FW_NOTE="  系统防火墙($kind)已放开;恢复:m-ui firewall undo,或面板「防火墙」页"
    else
      FW_NOTE="  系统防火墙($kind)还在拦入站(见上面的说明):请自己在它那里放行 m-ui 用到的端口"
    fi
  else
    FW_NOTE="  系统防火墙($kind)没能放开(见上面的报错);装好后可以再试:m-ui firewall open"
  fi
}

if healthy 90; then
  rm -f "$PREV"
  if [ "$FRESH" = 1 ]; then firewall_step || true; fi
  echo
  echo "=================== m-ui 安装完成 ==================="
  "$BIN" info -db "$DB"
  [ -z "$FW_NOTE" ] || echo "$FW_NOTE"
  if [ "$FRESH" = 1 ] && [ -z "$RESTORE" ] && [ -z "$IMPORT" ]; then
    echo "  首次安装默认账号: admin / admin  ——  请登录后立即在"管理员"页修改密码"
  elif [ -n "$IMPORT" ]; then
    echo "  账号沿用旧面板库;忘记密码可执行: m-ui passwd -db $DB"
  elif [ -n "$RESTORE" ]; then
    echo "  账号沿用备份;忘记密码可执行: m-ui passwd -db $DB"
  fi
  echo "  下一步:登录面板,按概览页“快速开始”完成 证书(有域名就签发,没有就一键自签)→ 线路 → 用户"
  echo "====================================================="
  exit 0
fi

# ---- 起不来:回滚 ----
echo "新版本没有正常启动:"; journalctl -u m-ui -n 20 --no-pager || true
if [ -z "$PREV" ]; then
  echo "启动失败(首次安装,没有旧版本可回滚)"; exit 1
fi
echo "回滚到 v$OLDVER …"
systemctl stop m-ui || true
mv -f "$BIN" "$BIN.failed"
mv -f "$PREV" "$BIN"
systemctl start m-ui || true
if healthy 60; then
  write_status true false true "已回滚到 v$OLDVER,面板正常;失败的新程序保留在 $BIN.failed"
  echo "已回滚到 v$OLDVER,面板正常。失败的新程序保留在 $BIN.failed;升级前备份:${BK:-无}"
  exit 1
fi
if [ -n "$BK" ]; then
  echo "旧程序仍未起来,还原升级前备份 $BK …"
  systemctl stop m-ui || true
  "$BIN" restore -db "$DB" -from "$BK" || true
  systemctl start m-ui || true
  if healthy 60; then
    write_status true true true "已回滚到 v$OLDVER 并还原升级前数据库备份,面板正常;失败的新程序保留在 $BIN.failed"
    echo "已回滚到 v$OLDVER 并还原升级前数据库,面板正常。失败的新程序保留在 $BIN.failed"
    exit 1
  fi
fi
write_status true "$([ -n "$BK" ] && echo true || echo false)" false "已换回 v$OLDVER 但面板仍未起来,请查看 journalctl -u m-ui"
echo "回滚后仍未起来,请查看:journalctl -u m-ui -n 50 --no-pager"; journalctl -u m-ui -n 30 --no-pager || true
exit 1

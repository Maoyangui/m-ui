#!/usr/bin/env bash
# m-ui 安装/升级脚本(Linux amd64/arm64,systemd)
#
#   bash install.sh [latest | vX.Y.Z | 二进制/压缩包路径 | 下载 URL] [--db /etc/m-ui/m-ui.db] [--restore backup.zip] [--import 旧面板.db]
#   bash install.sh --dry-run              只打印会做什么(装到哪、写哪些文件、默认端口),不改任何东西
#   bash install.sh --uninstall [--purge]  停止并删除服务与程序;不带 --purge 时保留 /etc/m-ui(数据库、证书、备份)
#
#   不带来源参数 = 从 GitHub Releases 安装最新版(自动识别 amd64 / arm64)。
#
# 幂等:重复执行即升级二进制并重启;数据库与证书不动。
set -euo pipefail

REPO="${M_UI_REPO:-Maoyangui/m-ui}" # GitHub 仓库 owner/name;换仓库改这里或设环境变量 M_UI_REPO
SRC=""
if [ $# -gt 0 ] && [[ "$1" != --* ]]; then SRC="$1"; shift; fi
DB="/etc/m-ui/m-ui.db"
RESTORE=""
IMPORT=""
DRY=0
UNINSTALL=0
PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --db) DB="$2"; shift 2;;
    --restore) RESTORE="$2"; shift 2;;
    --import) IMPORT="$2"; shift 2;;
    --dry-run) DRY=1; shift;;
    --uninstall) UNINSTALL=1; shift;;
    --purge) PURGE=1; shift;;
    *) echo "未知参数 $1"; exit 2;;
  esac
done

# 卸载:与面板菜单里的"卸载"一致 —— 停服务、删单元与程序;数据目录默认保留,--purge 才删
if [ "$UNINSTALL" = 1 ]; then
  [ "$DRY" = 1 ] || [ "$(id -u)" -eq 0 ] || { echo "请以 root 运行"; exit 1; }
  DATA_DIR="$(dirname "$DB")"
  if [ "$DRY" = 1 ]; then
    echo "[dry-run] 将执行:systemctl disable --now m-ui;删除 /etc/systemd/system/m-ui.service 与 /usr/local/bin/m-ui"
    [ "$PURGE" = 1 ] && echo "[dry-run] --purge:同时删除 $DATA_DIR(数据库、证书、备份)" || echo "[dry-run] 保留 $DATA_DIR(数据库、证书、备份)"
    exit 0
  fi
  systemctl disable --now m-ui 2>/dev/null || true
  rm -f /etc/systemd/system/m-ui.service
  systemctl daemon-reload 2>/dev/null || true
  rm -f /usr/local/bin/m-ui
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
  echo "  程序:/usr/local/bin/m-ui(已存在则覆盖,先停服务)"
  echo "  数据:$DB;目录 $(dirname "$DB")、$(dirname "$DB")/cert、$(dirname "$DB")/backups(已存在的数据库、证书不动)"
  echo "  服务:/etc/systemd/system/m-ui.service(root 运行 m-ui run -db $DB,Restart=always),然后 systemctl enable --now m-ui"
  echo "  端口:面板 2053/tcp 路径 /app/,订阅 2056/tcp,代理面板 2054/tcp(首次安装的默认值,面板里可改)"
  echo "  首次安装账号:admin / admin(登录后请改)"
  echo "  不会碰:系统里已有的 sing-box / xray 等程序与它们的配置、防火墙规则、其它服务"
  [ -n "$RESTORE" ] && echo "  还原备份:$RESTORE → $DB"
  [ -n "$IMPORT" ] && echo "  导入旧面板库:$IMPORT → $DB"
  exit 0
fi

TMP="$(mktemp)"
case "$SRC" in
  http://*|https://*) echo "下载 $SRC"; curl -fL --progress-bar "$SRC" -o "$TMP";;
  *) cp "$SRC" "$TMP";;
esac
# 官方 Release:用同一个 Release 里的 SHA256SUMS 校验,通不过就不装
# (能拦住 SHA256SUMS 这个请求的人也能换掉二进制,所以取不到摘要同样中止;
#  确实需要绕过时显式 MUI_SKIP_VERIFY=1)
if [ -n "${SUMS:-}" ] && [ "${MUI_SKIP_VERIFY:-}" != "1" ]; then
  if command -v sha256sum >/dev/null; then SUM="sha256sum"
  elif command -v shasum >/dev/null; then SUM="shasum -a 256"
  else echo "没有 sha256sum,无法校验下载内容,已中止(装 coreutils 后重试)"; rm -f "$TMP"; exit 1; fi
  WANT="$(curl -fsSL "$SUMS" 2>/dev/null | grep " m-ui-linux-$ARCH.tar.gz$" | awk '{print $1}')"
  [ -n "$WANT" ] || { echo "取不到这个版本的 SHA256SUMS,无法校验,已中止"; rm -f "$TMP"; exit 1; }
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

mkdir -p "$(dirname "$DB")" /etc/m-ui/cert /etc/m-ui/backups
FRESH=0; [ -f "$DB" ] || FRESH=1
if systemctl is-active --quiet m-ui; then systemctl stop m-ui; fi
install -m 0755 "$TMP" /usr/local/bin/m-ui
rm -f "$TMP"

if [ -n "$RESTORE" ]; then
  echo "还原备份 $RESTORE → $DB"
  /usr/local/bin/m-ui restore -db "$DB" -from "$RESTORE"
elif [ -n "$IMPORT" ]; then
  echo "从旧面板数据库导入 $IMPORT → $DB"
  /usr/local/bin/m-ui import -from "$IMPORT" -to "$DB"
fi

cat >/etc/systemd/system/m-ui.service <<EOF
[Unit]
Description=m-ui panel and data plane (sing-box)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$(dirname "$DB")
ExecStart=/usr/local/bin/m-ui run -db $DB
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
sleep 3
if systemctl is-active --quiet m-ui; then
  echo
  echo "=================== m-ui 安装完成 ==================="
  /usr/local/bin/m-ui info -db "$DB"
  if [ "$FRESH" = 1 ] && [ -z "$RESTORE" ] && [ -z "$IMPORT" ]; then
    echo "  首次安装默认账号: admin / admin  ——  请登录后立即在"管理员"页修改密码"
  elif [ -n "$IMPORT" ]; then
    echo "  账号沿用旧面板库;忘记密码可执行: m-ui passwd -db $DB"
  elif [ -n "$RESTORE" ]; then
    echo "  账号沿用备份;忘记密码可执行: m-ui passwd -db $DB"
  fi
  echo "  下一步:登录面板,按概览页“快速开始”完成 证书(有域名就签发,没有就一键自签)→ 线路 → 用户"
  echo "====================================================="
else
  echo "启动失败:"; journalctl -u m-ui -n 40 --no-pager; exit 1
fi

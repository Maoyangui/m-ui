#!/usr/bin/env bash
# m-ui 安装/升级脚本(Linux amd64/arm64,systemd)
#
#   bash install.sh [latest | vX.Y.Z | 二进制/压缩包路径 | 下载 URL] [--db /etc/m-ui/m-ui.db] [--restore backup.zip] [--import 旧面板.db]
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
while [ $# -gt 0 ]; do
  case "$1" in
    --db) DB="$2"; shift 2;;
    --restore) RESTORE="$2"; shift 2;;
    --import) IMPORT="$2"; shift 2;;
    *) echo "未知参数 $1"; exit 2;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "请以 root 运行"; exit 1; }
command -v systemctl >/dev/null || { echo "需要 systemd"; exit 1; }
command -v curl >/dev/null || { echo "需要 curl"; exit 1; }

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
  echo "安装 m-ui $TAG ($ARCH)"
fi

TMP="$(mktemp)"
case "$SRC" in
  http://*|https://*) echo "下载 $SRC"; curl -fL --progress-bar "$SRC" -o "$TMP";;
  *) cp "$SRC" "$TMP";;
esac
# Releases 里是 tar.gz(内含单文件 m-ui),也接受裸二进制
if file "$TMP" 2>/dev/null | grep -qi gzip || [[ "$SRC" == *.tar.gz ]]; then
  EXTRACT="$(mktemp -d)"
  tar -xzf "$TMP" -C "$EXTRACT"
  mv "$EXTRACT/m-ui" "$TMP"
  rm -rf "$EXTRACT"
fi
chmod 0755 "$TMP"
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
  echo "  下一步:登录面板,按概览页“快速开始”依次完成 域名 → 证书 → 线路 → 用户"
  echo "====================================================="
else
  echo "启动失败:"; journalctl -u m-ui -n 40 --no-pager; exit 1
fi

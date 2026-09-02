#!/usr/bin/env bash
# m-ui 安装/升级脚本(Linux amd64/arm64,systemd)
#
#   bash install.sh <m-ui 二进制路径或下载 URL> [--db /etc/m-ui/m-ui.db] [--restore backup.zip] [--import s-ui.db]
#
# 幂等:重复执行即升级二进制并重启;数据库与证书不动。
set -euo pipefail

SRC="${1:-}"
DB="/etc/m-ui/m-ui.db"
RESTORE=""
IMPORT=""
shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --db) DB="$2"; shift 2;;
    --restore) RESTORE="$2"; shift 2;;
    --import) IMPORT="$2"; shift 2;;
    *) echo "未知参数 $1"; exit 2;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "请以 root 运行"; exit 1; }
[ -n "$SRC" ] || { echo "用法: bash install.sh <m-ui 二进制路径或 URL> [--db 路径] [--restore 备份.zip] [--import s-ui.db]"; exit 2; }
command -v systemctl >/dev/null || { echo "需要 systemd"; exit 1; }

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
if systemctl is-active --quiet m-ui; then systemctl stop m-ui; fi
install -m 0755 "$TMP" /usr/local/bin/m-ui
rm -f "$TMP"

if [ -n "$RESTORE" ]; then
  echo "还原备份 $RESTORE → $DB"
  /usr/local/bin/m-ui restore -db "$DB" -from "$RESTORE"
elif [ -n "$IMPORT" ]; then
  echo "从 s-ui 数据库导入 $IMPORT → $DB"
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
sleep 2
if systemctl is-active --quiet m-ui; then
  echo "m-ui 已启动。面板地址与路径见: journalctl -u m-ui -n 20 --no-pager | grep 面板"
  journalctl -u m-ui -n 20 --no-pager | grep -E "面板|订阅|数据面" || true
else
  echo "启动失败:"; journalctl -u m-ui -n 40 --no-pager; exit 1
fi

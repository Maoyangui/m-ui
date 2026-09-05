#!/usr/bin/env bash
# 一键重拍演示截图:临时库 → 一主两副三个本地实例 → 演示数据 → 无头 Chrome 截图 → docs/screenshots/。
# 全程只用 example.com 域名与 RFC 5737 文档 IP,截图前再把本机名、探测到的公网 IP 抹掉。
# 开发工具,不参与发布二进制;本地(Git Bash / Linux / macOS)与 CI 通用。
set -euo pipefail
cd "$(dirname "$0")/../.."

EXE=""; case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
TMP="$(mktemp -d)"
# Git Bash 下给原生程序传路径要用 Windows 形式:set 命令关闭了 MSYS 的路径转换(避免把 /app/ 转成 C:/.../app/),
# 所以库文件路径本身得先转好
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) TMP="$(cygpath -m "$TMP")";; esac
BIN="$TMP/m-ui$EXE"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; sleep 2; rm -rf "$TMP" 2>/dev/null || true; }
trap cleanup EXIT

echo "构建 m-ui"
go build -tags with_utls -ldflags "-X main.version=demo" -o "$BIN" .

M="http://127.0.0.1:19053/app"
SUBBASE="http://127.0.0.1:19056/sub/"
TOKEN="demo-node-token-0123456789abcdef0123456789abcdef"

set_kv() { MSYS_NO_PATHCONV=1 "$BIN" set -db "$1" "${@:2}" >/dev/null; }
start() { "$BIN" run -db "$1" >"$1.log" 2>&1 & PIDS+=($!); } # 不能套子 shell:EXIT trap 会在子 shell 退出时把临时目录连库一起删掉
wait_http() { for i in $(seq 1 60); do curl -s -m 3 -o /dev/null "$1" && return 0; sleep 0.5; done; echo "等不到 $1,实例日志:"; tail -n 25 "$TMP"/*.log 2>/dev/null | grep -vE "inbound|outbound"; exit 1; }

echo "主机"
set_kv "$TMP/master.db" webPort=19053 webPath=/app/ subPort=19056 resellerPort=19054 webDomain=panel.example.com \
  subPageTitle="m-ui Demo" subProfileTitle="m-ui Demo" subPageSupport="Telegram: @example" \
  subPageNotice="演示环境 · Demo environment. Traffic resets on the 1st of every month." timezone=Asia/Tokyo
PASS="$("$BIN" passwd -db "$TMP/master.db" admin | sed -E 's/.*: *([A-Za-z0-9]+).*/\1/')"
start "$TMP/master.db"

echo "两台副机"
for n in 1 2; do
  set_kv "$TMP/node$n.db" nodeMode=true nodeToken="$TOKEN" webPort=$((19053+100*n)) webPath=/app/ subPort=$((19056+100*n)) resellerPort=$((19054+100*n)) resellerEnabled=false
  start "$TMP/node$n.db"
done
wait_http "$M/"
wait_http "http://127.0.0.1:19153/app/"
wait_http "http://127.0.0.1:19253/app/"

C="$TMP/cookie"
api() { local out; out="$(curl -s -b "$C" -c "$C" -m 30 -H 'Content-Type: application/json' "$@")"; case "$out" in *'"error"'*) echo "API 错误($*): $out" >&2;; esac; printf '%s' "$out"; }
api -X POST "$M/api/login" -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" >/dev/null

echo "证书、上游、服务器"
api -X POST "$M/api/cert/selfsign" -d '{"hosts":["panel.example.com","192.0.2.10"],"applyPanel":false,"applySub":false}' >/dev/null
api -X POST "$M/api/upstreams" -d '{"name":"Frankfurt-exit","type":"shadowsocks","options":{"server":"203.0.113.20","server_port":8388,"method":"aes-256-gcm","password":"demo-exit-password"}}' >/dev/null
LOCAL_ID="$(api "$M/api/nodes" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>{const n=JSON.parse(d).nodes.find(x=>x.isLocal);console.log(n?n.id:"")})')"
[ -n "$LOCAL_ID" ] && api -X PUT "$M/api/nodes/$LOCAL_ID" -d '{"name":"Tokyo","domain":"panel.example.com","addr":"192.0.2.10","ratio":1,"sort":0}' >/dev/null
api -X POST "$M/api/nodes" -d "{\"name\":\"Singapore\",\"domain\":\"sg.example.com\",\"addr\":\"198.51.100.10\",\"apiUrl\":\"http://127.0.0.1:19153/app/\",\"token\":\"$TOKEN\",\"insecure\":true,\"enabled\":true,\"ratio\":1,\"sort\":1}" >/dev/null
api -X POST "$M/api/nodes" -d "{\"name\":\"Frankfurt\",\"domain\":\"de.example.com\",\"addr\":\"203.0.113.30\",\"apiUrl\":\"http://127.0.0.1:19253/app/\",\"token\":\"$TOKEN\",\"insecure\":true,\"enabled\":true,\"ratio\":2,\"sort\":2}" >/dev/null

SG_ID="$(api "$M/api/nodes" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>{const n=JSON.parse(d).nodes.find(x=>x.name==="Singapore");console.log(n?n.id:"")})')"
DE_ID="$(api "$M/api/nodes" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>{const n=JSON.parse(d).nodes.find(x=>x.name==="Frankfurt");console.log(n?n.id:"")})')"

echo "线路"
RK="$(api "$M/api/keygen?type=reality")"
PRIV="$(echo "$RK" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>console.log(JSON.parse(d).privateKey))')"
PUB="$(echo "$RK" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>console.log(JSON.parse(d).publicKey))')"
SID="$(api "$M/api/keygen?type=shortid" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>console.log(JSON.parse(d).shortId))')"
SSPW="$(node -e 'console.log(require("crypto").randomBytes(16).toString("base64"))')" # 2022 系列要 16 字节 base64 密钥
line() { api -X POST "$M/api/lines" -d "$1" >/dev/null; }
line "{\"name\":\"Tokyo-HY2\",\"protocol\":\"hysteria2\",\"port\":24443,\"upstreamId\":0,\"enabled\":true,\"nodeIds\":[$LOCAL_ID],\"tls\":{\"mode\":\"cert\"},\"options\":{\"up_mbps\":0,\"down_mbps\":0,\"port_hopping\":\"20000-30000\"},\"assignAll\":true}"
line "{\"name\":\"Singapore-AnyTLS\",\"protocol\":\"anytls\",\"port\":28443,\"upstreamId\":0,\"enabled\":true,\"nodeIds\":[$SG_ID],\"tls\":{\"mode\":\"cert\"},\"options\":{},\"assignAll\":true}"
line "{\"name\":\"Frankfurt-Reality\",\"protocol\":\"vless\",\"port\":24433,\"upstreamId\":0,\"enabled\":true,\"nodeIds\":[$DE_ID],\"tls\":{\"mode\":\"reality\",\"reality\":{\"handshake_server\":\"www.microsoft.com\",\"handshake_port\":443,\"private_key\":\"$PRIV\",\"public_key\":\"$PUB\",\"short_ids\":[\"$SID\"]}},\"options\":{\"vision\":true},\"assignAll\":true}"
line "{\"name\":\"Global-SS2022\",\"protocol\":\"shadowsocks\",\"port\":28388,\"upstreamId\":1,\"enabled\":true,\"nodeIds\":[],\"options\":{\"method\":\"2022-blake3-aes-128-gcm\",\"password\":\"$SSPW\"},\"assignAll\":true}"

line '{"name":"Global-TUIC","protocol":"tuic","port":28444,"upstreamId":0,"enabled":true,"nodeIds":[],"tls":{"mode":"cert"},"options":{"congestion_control":"bbr"},"assignAll":true}'

echo "套餐与代理"
api -X POST "$M/api/plans" -d '{"name":"Basic","desc":"100 GB / 30 days","volumeGb":100,"days":30,"deviceLimit":2,"speedUp":0,"speedDown":0,"autoReset":true,"resetDays":30,"lineIds":[1,2,4]}' >/dev/null
api -X POST "$M/api/plans" -d '{"name":"Pro","desc":"500 GB / 90 days, all lines","volumeGb":500,"days":90,"deviceLimit":5,"speedUp":0,"speedDown":0,"autoReset":true,"resetDays":30,"lineIds":[1,2,3,4,5]}' >/dev/null
api -X POST "$M/api/resellers" -d "{\"name\":\"demo-reseller\",\"remark\":\"Example reseller\",\"enabled\":true,\"volume\":$((2000*1024*1024*1024)),\"deviceLimit\":0,\"speedUp\":0,\"speedDown\":0,\"expiry\":0,\"lineIds\":[1,2,4]}" >/dev/null

echo "用户"
NOW="$(date +%s)"
user() { api -X POST "$M/api/users" -d "$1" >/dev/null; }
user "{\"name\":\"demo-alice\",\"enabled\":true,\"remark\":\"Pro - monthly\",\"volume\":$((200*1024*1024*1024)),\"expiry\":$((NOW+30*86400)),\"deviceLimit\":3,\"autoReset\":true,\"resetDays\":30,\"lineIds\":[1,2,3,4,5]}"
user "{\"name\":\"demo-bob\",\"enabled\":true,\"remark\":\"Basic\",\"volume\":$((100*1024*1024*1024)),\"expiry\":$((NOW+12*86400)),\"deviceLimit\":2,\"speedUp\":50,\"speedDown\":200,\"lineIds\":[1,2,4]}"
user "{\"name\":\"demo-charlie\",\"enabled\":true,\"remark\":\"Team\",\"volume\":$((500*1024*1024*1024)),\"expiry\":$((NOW+90*86400)),\"deviceLimit\":5,\"lineIds\":[1,2,3,4,5]}"
user "{\"name\":\"demo-dave\",\"enabled\":true,\"remark\":\"Trial\",\"volume\":$((20*1024*1024*1024)),\"expiry\":$((NOW+3*86400)),\"deviceLimit\":1,\"lineIds\":[1,4]}"
user "{\"name\":\"demo-eve\",\"enabled\":true,\"remark\":\"Unlimited\",\"volume\":0,\"expiry\":0,\"deviceLimit\":0,\"lineIds\":[1,2,3,4,5]}"

echo "演示用量与时序"
go run ./docs/screenshots/seed -db "$TMP/master.db"
sleep 12 # 等主副机同步两轮,服务器页显示在线 · 已同步

echo "截图"
SCRUB="$(hostname)"
PUBIP="$(api "$M/api/nodes" | node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>console.log(JSON.parse(d).nodes.map(n=>n.publicIp).filter(Boolean).join(",")))')"
[ -n "$PUBIP" ] && SCRUB="$SCRUB,$PUBIP"
MUI_URL="$M/" MUI_SUB="$SUBBASE" MUI_PASS="$PASS" OUT="docs/screenshots" SCRUB="$SCRUB" node docs/screenshots/shoot.mjs
echo "导出 Live Demo fixture"
FIX="site/demo/fixtures.json"; mkdir -p site/demo
MUI_URL="$M" MUI_COOKIE="$C" MUI_TOKEN="$TOKEN" SCRUB="$SCRUB" FIX="$FIX" node docs/screenshots/fixtures.mjs
echo "完成:docs/screenshots/*.png + $FIX"

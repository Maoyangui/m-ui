#!/usr/bin/env bash
# 安装脚本验签名那一段(install.sh 的 verify_sums / --verify-sums)的测试,CI 里跑(要 OpenSSH 8.1+ 和 OpenSSL 3+)。
#   1. 内置的发布者公钥:拿真实发布过的校验清单和签名(selfupdate/testdata,佛跳墙 v0.8.0 的,同一把钥匙)走 OpenSSL 那条路,
#      验得过;清单改一个字节验不过。
#   2. 两条路的完整流程:现造一把临时钥匙,用发版流程同样的命令签(tools/signsums、ssh-keygen -Y sign),
#      把脚本副本里的公钥换成它 —— ssh-keygen 那条路、OpenSSL 那条路各自验得过;改过的清单、别的命名空间、别的钥匙签的都验不过;
#      没给签名文件返回 2(验不了),不是 0。
# 用法:在仓库根目录 bash deploy/install-verify-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
tr -d '\r' < deploy/install.sh > "$T/install.sh"
fails=0
# expect <期望的退出码> <说明> <命令...>
expect() {
  local want="$1" what="$2" rc=0; shift 2
  "$@" >/dev/null 2>&1 || rc=$?
  if [ "$rc" = "$want" ]; then echo "  ✔ $what"; else echo "  ✘ $what(退出码 $rc,应为 $want)"; fails=$((fails + 1)); fi
}

echo "一、内置公钥 × 真实发布过的签名(OpenSSL)"
S=selfupdate/testdata/published-SHA256SUMS
expect 0 "真实的清单和签名验得过" bash "$T/install.sh" --verify-sums "$S" "" "$S.sig"
{ cat "$S"; printf 'x'; } > "$T/bad"
expect 1 "清单改过一个字节验不过" bash "$T/install.sh" --verify-sums "$T/bad" "" "$S.sig"
expect 2 "没给签名文件:验不了(不是通过)" bash "$T/install.sh" --verify-sums "$S"

echo "二、临时钥匙走完整流程"
ssh-keygen -q -t ed25519 -N "" -C install-verify-test -f "$T/k"
printf 'aaa  m-ui-linux-amd64.tar.gz\nbbb  m-ui-linux-arm64.tar.gz\n' > "$T/SHA256SUMS"
go run ./tools/signsums "$T/k" "$T/SHA256SUMS" > "$T/SHA256SUMS.sig"
cp "$T/SHA256SUMS" "$T/sums" && ssh-keygen -q -Y sign -f "$T/k" -n m-ui-release "$T/sums" 2>/dev/null && mv "$T/sums.sig" "$T/SHA256SUMS.sshsig"
cp "$T/SHA256SUMS" "$T/ns" && ssh-keygen -q -Y sign -f "$T/k" -n something-else "$T/ns" 2>/dev/null
PUB="$(cut -d' ' -f1,2 "$T/k.pub")"
# 公钥的 PEM 写法:固定的 12 字节 DER 头 + 32 字节公钥(SSH 写法的最后 32 字节)
PEM="$( { printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00'; printf '%s' "${PUB#* }" | base64 -d | tail -c 32; } | base64 -w0)"
sed -e "s#^RELEASE_KEY=.*#RELEASE_KEY=\"$PUB\"#" -e "s#^MCowBQYDK2VwAyEA.*#$PEM#" "$T/install.sh" > "$T/alt.sh"
grep -q "^RELEASE_KEY=\"$PUB\"" "$T/alt.sh" && grep -q "^$PEM\$" "$T/alt.sh" || { echo "  ✘ 没能把脚本副本里的公钥换成临时钥匙"; exit 1; }
{ cat "$T/SHA256SUMS"; printf 'x'; } > "$T/bad2"
expect 0 "ssh-keygen:验得过" bash "$T/alt.sh" --verify-sums "$T/SHA256SUMS" "$T/SHA256SUMS.sshsig"
expect 1 "ssh-keygen:清单改过验不过" bash "$T/alt.sh" --verify-sums "$T/bad2" "$T/SHA256SUMS.sshsig"
expect 1 "ssh-keygen:别的命名空间签的验不过" bash "$T/alt.sh" --verify-sums "$T/SHA256SUMS" "$T/ns.sig"
expect 0 "OpenSSL:验得过(tools/signsums 签的)" bash "$T/alt.sh" --verify-sums "$T/SHA256SUMS" "" "$T/SHA256SUMS.sig"
expect 1 "OpenSSL:清单改过验不过" bash "$T/alt.sh" --verify-sums "$T/bad2" "" "$T/SHA256SUMS.sig"
expect 0 "两种签名都给:验得过" bash "$T/alt.sh" --verify-sums "$T/SHA256SUMS" "$T/SHA256SUMS.sshsig" "$T/SHA256SUMS.sig"
expect 1 "内置公钥不认临时钥匙签的(ssh-keygen)" bash "$T/install.sh" --verify-sums "$T/SHA256SUMS" "$T/SHA256SUMS.sshsig"
expect 1 "内置公钥不认临时钥匙签的(OpenSSL)" bash "$T/install.sh" --verify-sums "$T/SHA256SUMS" "" "$T/SHA256SUMS.sig"

echo "三、哪些版本带签名"
bash -c "source <(sed -n '/^signed_release()/,/^}/p' '$T/install.sh'); for v in v0.8.1 v0.8.2 v0.9.0 v0.10.0 v1.0.0 0.8.1; do signed_release \$v || { echo \"  ✘ \$v 应该带签名\"; exit 1; }; done; for v in v0.8.0 v0.7.17 v0.6.9 ''; do if signed_release \"\$v\"; then echo \"  ✘ \$v 不该要求签名\"; exit 1; fi; done" && echo "  ✔ v0.8.1 起要签名,之前的不要" || fails=$((fails + 1))

if [ "$fails" = 0 ]; then echo "安装脚本验签测试全部通过"; else echo "$fails 项不对"; exit 1; fi

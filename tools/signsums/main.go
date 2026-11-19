// signsums 给发版用:拿发布者的 Ed25519 私钥(OpenSSH 格式的私钥文件)给一个文件签名,把 64 字节签名的 base64 打到标准输出。
// 发版流程(.github/workflows/release.yml)用它生成 SHA256SUMS.sig;面板内更新(selfupdate.VerifySums)验的就是这个。
// 和佛跳墙发版流程里那段签名代码是同一种签法。
//
//	go run ./tools/signsums <私钥文件> <要签的文件>  >  SHA256SUMS.sig
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法: signsums <私钥文件> <要签的文件>")
		os.Exit(2)
	}
	pem, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "读私钥:", err)
		os.Exit(1)
	}
	raw, err := ssh.ParseRawPrivateKey(pem)
	if err != nil {
		fmt.Fprintln(os.Stderr, "私钥解析不了(要 OpenSSH 格式、不带口令的 Ed25519 私钥):", err)
		os.Exit(1)
	}
	var key ed25519.PrivateKey
	switch k := raw.(type) {
	case *ed25519.PrivateKey:
		key = *k
	case ed25519.PrivateKey:
		key = k
	default:
		fmt.Fprintf(os.Stderr, "不是 Ed25519 私钥: %T\n", raw)
		os.Exit(1)
	}
	msg, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "读文件:", err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(ed25519.Sign(key, msg)))
}

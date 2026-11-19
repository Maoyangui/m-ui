package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
)

// 更新包签名(任务单 S1)。
//
// 以前面板内更新只拿同一个 Release 里的 SHA256SUMS 去比下载的程序:能发现下坏了,防不了被换 ——
// 能换掉程序的人同样能换掉那份清单。现在发版时用发布者的 Ed25519 私钥给 SHA256SUMS 签名(SHA256SUMS.sig,
// 内容是 64 字节签名的 base64),这里先验签、再比 SHA256。没有签名文件、格式不对、验不过,一律不装,
// 现在的程序照常运行 ——「没验成」和「验不过」必须同样对待,否则把签名文件删掉就绕过去了。
//
// v0.8.1 之前的 Release 没有签名:带验签的程序装不了它们(面板和 m-ui update 只装最新版,用不到;
// 真要回到那些版本用安装脚本)。从 v0.8.0 升上来的那一次用的是旧程序里的更新代码,不经过这里。

// releaseKeys 发布者的 Ed25519 公钥(信任根),列表里任何一把验得过就算数。
// 对应的私钥只在发版流程的机密(RELEASE_SIGNING_KEY)和发布者手里,绝不能进仓库或安装包。
// 现在这一把和佛跳墙客户端内置的是同一把(用户 11-20 定)。
// 换钥匙:先把新公钥加进来发一版(还用旧钥匙签),等大家都升上来以后再改用新钥匙签,最后才去掉旧的。
var releaseKeys = []ed25519.PublicKey{
	{
		0x0a, 0x6f, 0x72, 0xe4, 0x33, 0xc2, 0x11, 0x38,
		0x68, 0x7f, 0xa5, 0x58, 0x48, 0xac, 0x29, 0x84,
		0x27, 0x28, 0xc2, 0x51, 0x5a, 0xc8, 0xb5, 0x67,
		0x88, 0x9d, 0x77, 0x70, 0xc5, 0xb0, 0x2a, 0xee,
	},
}

// VerifySums 核对 SHA256SUMS 的签名。sums 是清单的原始字节(一个字节都不能动),sig 是 SHA256SUMS.sig 的内容。
func VerifySums(sums, sig []byte) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("签名文件的格式不对")
	}
	for _, k := range releaseKeys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, sums, raw) {
			return nil
		}
	}
	return errors.New("签名和发布者的公钥对不上")
}

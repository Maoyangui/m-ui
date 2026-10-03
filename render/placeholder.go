package render

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"

	"github.com/Maoyangui/m-ui/database/model"
)

// PlaceholderName 占位用户在数据面里的名字(见 placeholderUser)。它的凭据谁都不知道,不会有连接用这个名字认证进来。
const PlaceholderName = "m-ui#nobody"

// placeholderSecret 派生占位凭据的密钥:进程启动时随机生成。同一进程里同一条线路的占位凭据不变 —— 热更新时
// 入站定义没变就不重建;重启后换一份,反正整机重载。
var placeholderSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("生成占位凭据密钥失败: " + err.Error())
	}
	return b
}()

// placeholderUser 线路在这台机器上一个用户都没有时渲染的占位用户。这几种协议不能留空用户表:
//   - socks / http / mixed:sing-box 把空用户表当成不鉴权,开成公网开放代理;
//   - SS2022:空用户表退化成只认服务端 PSK 的单用户入站,而老用户的链接里都带着这个 PSK;
//   - 没有线路级密码的传统 SS:空用户表直接建不起来,热更新失败时会把带着刚停用用户的旧定义补回去。
//
// 用户全停、代理到期 / 用尽、停用或删除副机时主机推的空用户表都会走到这里。其余协议空用户表就是谁都认证不过,返回 nil。
func placeholderUser(line model.Line, credKey string) map[string]interface{} {
	mac := hmac.New(sha256.New, placeholderSecret)
	mac.Write([]byte(line.Protocol + "|" + line.Name + "|" + credKey))
	sum := mac.Sum(nil)
	switch line.Protocol {
	case "socks", "http", "mixed":
		return map[string]interface{}{"username": PlaceholderName, "password": hex.EncodeToString(sum)}
	case "shadowsocks":
		n := 32 // SS2022 的用户密钥是方法要求长度的 base64(aes-128 16 字节,其余 32 字节);传统方法当普通口令用
		if credKey == "shadowsocks16" {
			n = 16
		}
		return map[string]interface{}{"name": PlaceholderName, "password": base64.StdEncoding.EncodeToString(sum[:n])}
	}
	return nil
}

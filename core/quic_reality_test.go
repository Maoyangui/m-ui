//go:build !race && with_utls

// REALITY 要 with_utls 构建(CI、发布都带);起真实的数据面(只在本机回环地址),不在 -race 下跑。

package core

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// 走 QUIC 的协议(hysteria2 / tuic)配 Reality:保存前的干跑(只构造、不启动)能过,真正启动才失败。
// 所以面板不能指望干跑拦住它 —— render/combo.go 在保存时另拦(CheckCombo)、渲染时跳过(Unstartable)。
// 这里钉住内核的这个行为:哪天内核支持了 QUIC 上的 Reality,这个测试会挂,提醒把那两处放开。
// 同一份配置换成证书就起得来(对照:失败的原因是 Reality,不是别的字段写错了)。
func TestQUICRealityFailsOnlyAtStart(t *testing.T) {
	certPath, keyPath := e2eSelfSigned(t)
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reality := map[string]interface{}{"enabled": true, "server_name": "www.microsoft.com", "reality": map[string]interface{}{
		"enabled": true, "handshake": map[string]interface{}{"server": "www.microsoft.com", "server_port": 443},
		"private_key": base64.RawURLEncoding.EncodeToString(priv.Bytes()), "short_id": []string{"0123456789abcdef"}}}
	cert := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath}
	users := map[string][]map[string]interface{}{
		"hysteria2": {{"name": "alice", "password": "pw"}},
		"tuic":      {{"name": "alice", "uuid": "5b0e2a9c-3f1d-4c6b-8a7e-2d4c6e8f0a1b", "password": "pw"}},
	}
	config := func(proto string, tls map[string]interface{}) []byte {
		if proto == "tuic" { // 和面板渲染的一样带上 h3
			withALPN := map[string]interface{}{"alpn": []string{"h3"}}
			for k, v := range tls {
				withALPN[k] = v
			}
			tls = withALPN
		}
		raw, _ := json.Marshal(map[string]interface{}{
			"log": map[string]interface{}{"level": "error"},
			"inbounds": []map[string]interface{}{
				{"type": proto, "tag": "q", "listen": "127.0.0.1", "listen_port": e2eFreeUDP(t), "users": users[proto], "tls": tls},
			},
			"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
		})
		return raw
	}
	for _, proto := range []string{"hysteria2", "tuic"} {
		t.Run(proto, func(t *testing.T) {
			raw := config(proto, reality)
			if err := ValidateConfig(raw); err != nil {
				t.Fatalf("%s 配 Reality 的干跑以前是能过的(所以才要在保存时另拦),现在干跑就报错了,看看是不是内核变了: %v", proto, err)
			}
			c := NewCore()
			err := c.Start(raw)
			if err == nil {
				c.Stop()
				t.Fatalf("%s 配 Reality 居然起来了:内核支持 QUIC 上的 Reality 了?那 render/combo.go 的拦截可以放开", proto)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "reality") {
				t.Fatalf("%s 配 Reality 起不来,但原因不是 Reality: %v", proto, err)
			}
			// 对照:换成证书就起得来
			ok := NewCore()
			if err := ok.Start(config(proto, cert)); err != nil {
				t.Fatalf("%s 配证书应该起得来: %v", proto, err)
			}
			ok.Stop()
		})
	}
}

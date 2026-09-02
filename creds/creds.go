// Package creds 生成与补全用户的各协议凭据,并提供 Reality/UUID/短 ID 等密钥生成。
//
// 凭据按协议分键存于 users.credentials:同一用户在所有口令型协议上共用一个口令、
// 在所有 UUID 型协议上共用一个 UUID(与 s-ui 一致,便于用户记忆与迁移)。
package creds

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/gofrs/uuid/v5"
	"golang.org/x/crypto/curve25519"
	"gorm.io/gorm"
)

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Password 生成 n 位字母数字口令。
func Password(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// Base64Key 生成 n 字节随机数据的 base64(shadowsocks 密码要求)。
func Base64Key(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// UUID 生成 v4 UUID。
func UUID() string { return uuid.Must(uuid.NewV4()).String() }

// ShortID 生成 Reality 短 ID(8 位十六进制)。
func ShortID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RealityKeypair 生成 Reality 用的 X25519 密钥对(base64 RawURL 编码,与 xray/sing-box 一致)。
func RealityKeypair() (privateKey, publicKey string, err error) {
	priv := make([]byte, curve25519.ScalarSize)
	if _, err = rand.Read(priv); err != nil {
		return
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return
	}
	return base64.RawURLEncoding.EncodeToString(priv), base64.RawURLEncoding.EncodeToString(pub), nil
}

// Generate 为新用户生成全部协议的凭据。
func Generate(name string) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	fill(out, name, Password(10), UUID(), Base64Key(32), Base64Key(16))
	return out
}

// fill 用给定的共享口令/UUID 补全 m 中缺失的协议键。
func fill(m map[string]map[string]interface{}, name, pass, id, ssPass, ss16 string) bool {
	changed := false
	set := func(key string, v map[string]interface{}) {
		if _, ok := m[key]; !ok {
			m[key] = v
			changed = true
		}
	}
	set("hysteria2", map[string]interface{}{"name": name, "password": pass})
	set("anytls", map[string]interface{}{"name": name, "password": pass})
	set("trojan", map[string]interface{}{"name": name, "password": pass})
	set("tuic", map[string]interface{}{"name": name, "uuid": id, "password": pass})
	set("vless", map[string]interface{}{"name": name, "uuid": id})
	set("vmess", map[string]interface{}{"name": name, "uuid": id, "alterId": 0})
	set("shadowsocks", map[string]interface{}{"name": name, "password": ssPass})
	set("shadowsocks16", map[string]interface{}{"name": name, "password": ss16})
	set("socks", map[string]interface{}{"username": name, "password": pass})
	set("http", map[string]interface{}{"username": name, "password": pass})
	return changed
}

// Ensure 补全既有凭据里缺失的协议键(尽量复用已有的口令与 UUID),返回是否有改动。
func Ensure(raw json.RawMessage, name string) (json.RawMessage, bool) {
	m := map[string]map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	pass, id := "", ""
	for _, k := range []string{"hysteria2", "anytls", "trojan", "tuic", "socks"} {
		if p, ok := m[k]["password"].(string); ok && p != "" {
			pass = p
			break
		}
	}
	for _, k := range []string{"vless", "vmess", "tuic"} {
		if u, ok := m[k]["uuid"].(string); ok && u != "" {
			id = u
			break
		}
	}
	if pass == "" {
		pass = Password(10)
	}
	if id == "" {
		id = UUID()
	}
	changed := fill(m, name, pass, id, Base64Key(32), Base64Key(16))
	if !changed {
		return raw, false
	}
	b, _ := json.Marshal(m)
	return b, true
}

// EnsureAll 为库中所有用户补全凭据(升级到新协议后启动时调用),返回补全的用户数。
func EnsureAll(db *gorm.DB) (int, error) {
	type row struct {
		Id          uint
		Name        string
		Credentials json.RawMessage
	}
	var users []row
	if err := db.Table("users").Select("id, name, credentials").Find(&users).Error; err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if updated, changed := Ensure(u.Credentials, u.Name); changed {
			if err := db.Table("users").Where("id = ?", u.Id).Update("credentials", updated).Error; err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

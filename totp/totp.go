// Package totp 实现 RFC 6238 基于时间的一次性密码(6 位、30 秒、HMAC-SHA1),
// 与 Google Authenticator / Microsoft Authenticator / 1Password 等认证器 App 兼容,用于面板登录两步验证。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	Period = 30 // 秒
	Digits = 6
)

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret 生成 160 位随机密钥(base32,32 字符)。
func GenerateSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return enc.EncodeToString(b)
}

// Normalize 清理手工输入的密钥:去空格/连字符/填充、转大写,并校验能被 base32 解码。
func Normalize(secret string) (string, error) {
	s := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(strings.TrimSpace(secret)))
	if len(s) < 16 {
		return "", errors.New("密钥太短(至少 16 个 base32 字符)")
	}
	if _, err := enc.DecodeString(s); err != nil {
		return "", errors.New("密钥不是合法的 base32(只能包含 A-Z 与 2-7)")
	}
	return s, nil
}

func hotp(key []byte, counter int64) string {
	var msg [8]byte
	for i := 7; i >= 0; i-- {
		msg[i] = byte(counter & 0xff)
		counter >>= 8
	}
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", code%1000000)
}

// Code 返回密钥在时刻 t 的验证码。
func Code(secret string, t time.Time) (string, error) {
	key, err := enc.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	return hotp(key, t.Unix()/Period), nil
}

// Verify 校验验证码,容忍前后各一个时间步(±30 秒)的时钟偏差;返回命中的时间步,调用方据此拒绝重放。
func Verify(secret, code string, t time.Time) (bool, int64) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return false, 0
	}
	key, err := enc.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return false, 0
	}
	step := t.Unix() / Period
	for _, d := range []int64{0, -1, 1} {
		if subtle.ConstantTimeCompare([]byte(hotp(key, step+d)), []byte(code)) == 1 {
			return true, step + d
		}
	}
	return false, 0
}

// URL 生成认证器 App 扫码用的 otpauth 地址。
func URL(issuer, account, secret string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

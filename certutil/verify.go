package certutil

import (
	"crypto/tls"
	"fmt"
	"os"
)

// Verify 校验一对证书 / 私钥文件:存在、是普通文件、能被解析且互相匹配。
// 用于"使用服务器上已有的证书"这类外部路径,避免把一份用不了的证书写进设置。
func Verify(certPath, keyPath string) error {
	for _, p := range []struct{ what, path string }{{"证书", certPath}, {"私钥", keyPath}} {
		st, err := os.Stat(p.path)
		if os.IsNotExist(err) {
			return fmt.Errorf("%s文件不存在: %s", p.what, p.path)
		}
		if err != nil {
			return fmt.Errorf("读取%s文件 %s: %w", p.what, p.path, err)
		}
		if st.IsDir() {
			return fmt.Errorf("%s路径是目录,请填到文件: %s", p.what, p.path)
		}
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("证书与私钥无法加载(格式错误或两者不匹配): %w", err)
	}
	return nil
}

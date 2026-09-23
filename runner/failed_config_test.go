package runner

import (
	"errors"
	"testing"
	"time"
)

// 同一份"校验能过、真正启动失败"的配置,冷却期内不再拆掉正在服务的数据面去重试。
// 副机上新线路的端口被占时,主机每 5 秒重推同一份;0.6.10 每一轮都是"停旧数据面 → 起失败 → 回滚",
// 这台机上所有用户每 5 秒断一次线,直到管理员修好 —— 0.4.16 修过的那个致命问题换了形式回来。
// "只有用户表不一样"的也算同一份:管理员没改线路、只是停用了个用户,推下来的新修订照样起不来。
func TestSameFailedConfigHonoursCooldown(t *testing.T) {
	bad := []byte(`{"inbounds":[{"tag":"a","type":"shadowsocks","listen_port":1,"users":[{"name":"x","password":"1"}]}],"outbounds":[]}`)
	sameLinesNewUsers := []byte(`{"inbounds":[{"tag":"a","type":"shadowsocks","listen_port":1,"users":[{"name":"y","password":"2"}]}],"outbounds":[]}`)
	fixedLines := []byte(`{"inbounds":[{"tag":"a","type":"shadowsocks","listen_port":2,"users":[{"name":"x","password":"1"}]}],"outbounds":[]}`)

	r := &Runner{}
	if r.sameFailedConfig(bad) {
		t.Fatal("从没失败过,不该判成同一份失败配置")
	}
	r.noteFailedConfig(bad, errors.New("listen tcp :1: address already in use"))
	if !r.sameFailedConfig(bad) {
		t.Fatal("一模一样的配置在冷却期内必须判成同一份")
	}
	if !r.sameFailedConfig(sameLinesNewUsers) {
		t.Fatal("只有用户表不一样、线路没改:照样起不来,冷却期内不该再拆数据面")
	}
	if r.sameFailedConfig(fixedLines) {
		t.Fatal("线路改过了(端口换了)就该立刻重试")
	}
	r.lastFailedAt = time.Now().Add(-failedConfigRetryEvery - time.Second)
	if r.sameFailedConfig(bad) {
		t.Fatal("冷却期过了就允许再试一次")
	}
	r.noteFailedConfig(bad, errors.New("x"))
	r.clearFailedConfig()
	if r.sameFailedConfig(bad) {
		t.Fatal("成功启动之后要把失败记录清掉")
	}
}

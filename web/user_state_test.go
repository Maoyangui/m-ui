package web

import (
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestAutoEnableOnlyForAutomaticReasons(t *testing.T) {
	now := time.Now().Unix()
	manual := model.User{Enabled: false, DisabledReason: model.DisabledManual}
	if autoEnable(&manual, now) || manual.Enabled {
		t.Fatal("手动停用的用户不该自动恢复")
	}
	quota := model.User{Enabled: false, DisabledReason: model.DisabledQuota, Volume: 100, Up: 100}
	if autoEnable(&quota, now) {
		t.Fatal("仍然超量就不该恢复")
	}
	quota.Up = 0
	if !autoEnable(&quota, now) || !quota.Enabled || quota.DisabledReason != "" {
		t.Fatal("补量后超量停用的用户应自动恢复并清掉原因")
	}
	expired := model.User{Enabled: false, DisabledReason: model.DisabledExpired, Expiry: now - 10}
	if autoEnable(&expired, now) {
		t.Fatal("仍然到期就不该恢复")
	}
	expired.Expiry = now + 86400
	if !autoEnable(&expired, now) {
		t.Fatal("延期后到期停用的用户应自动恢复")
	}
	legacy := model.User{Enabled: false} // 老库里没有原因的停用,按自动处理
	if !autoEnable(&legacy, now) {
		t.Fatal("没有原因的停用视同自动停用")
	}
}

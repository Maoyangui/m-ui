package tz

import (
	"testing"
	"time"
)

func TestLocation(t *testing.T) {
	// 内嵌 tzdata:即使系统没装时区库也要能解析
	if got := Location("Asia/Shanghai").String(); got != "Asia/Shanghai" {
		t.Fatalf("Asia/Shanghai => %s", got)
	}
	if got := Location("").String(); got != Default {
		t.Fatalf("空值应回落到 %s,实际 %s", Default, got)
	}
	if got := Location("  Europe/London  ").String(); got != "Europe/London" {
		t.Fatalf("应去掉空格: %s", got)
	}
	if got := Location("Not/AZone").String(); got != Default {
		t.Fatalf("无效时区应回落到 %s,实际 %s", Default, got)
	}
	// 同一时刻在不同时区的墙上时间不同
	at := time.Unix(1788400000, 0)
	sh := at.In(Location("Asia/Shanghai")).Format("15:04")
	utc := at.In(Location("UTC")).Format("15:04")
	if sh == utc {
		t.Fatalf("上海与 UTC 的显示不应相同: %s / %s", sh, utc)
	}
	if !Valid("") || !Valid("UTC") || Valid("Nope/Nope") {
		t.Fatal("Valid 判断有误")
	}
}

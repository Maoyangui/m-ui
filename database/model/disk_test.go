package model

import "testing"

// 磁盘告警的口径:按 df 的 Use%(已用 / (已用 + 可用))和剩余字节一起看;小盘看剩余、大盘看比例;读不到不告警。
func TestDiskLevel(t *testing.T) {
	const G, M = int64(1) << 30, int64(1) << 20
	for _, c := range []struct {
		name       string
		used, free int64
		want       string
	}{
		{"读不到", 0, 0, ""},
		{"负数(坏数据)", -1, 5 * G, ""},
		{"宽裕", 40 * G, 60 * G, ""},
		{"用了 89%", 89 * G, 11 * G, ""},
		{"用了 90%", 90 * G, 10 * G, DiskWarn},
		{"大盘 97%", 970 * G, 30 * G, DiskBad},
		{"小盘只剩 900MB(85%)", 5 * G, 900 * M, DiskWarn},
		{"小盘只剩 250MB", 2 * G, 250 * M, DiskBad},
		{"tw 那台 100%(剩 22MB)", 8*G + 600*M, 22 * M, DiskBad},
		{"tw 清理后(92%,剩 781MB)", 8 * G, 781 * M, DiskWarn},
	} {
		if got := DiskLevel(c.used, c.free); got != c.want {
			t.Errorf("%s: DiskLevel(%d, %d) = %q,应为 %q", c.name, c.used, c.free, got, c.want)
		}
	}
	var nilStat *DiskStat
	if nilStat.Level() != "" || nilStat.UsedPct() != 0 {
		t.Fatal("老副机没报:不告警")
	}
	if p := (&DiskStat{Used: 91, Free: 9}).UsedPct(); p != 91 {
		t.Fatalf("百分比和 df 一样: %d", p)
	}
	if p := (&DiskStat{Used: 1, Free: 199}).UsedPct(); p != 1 {
		t.Fatalf("向上取整(df 也是): %d", p)
	}
}

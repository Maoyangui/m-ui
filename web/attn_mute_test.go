package web

import "testing"

// 用户 N14:概览里关掉的提醒类别(设置 attnMute)只留小写字母和数字写的、去重,别的写法丢掉;类别名由前端定(有 v6)。
func TestNormAttnMute(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 "",
		"cert,rejected,v6":                 "cert,rejected,v6",
		" Cert , cert ,REJECTED,,":         "cert,rejected",
		"cert,<script>,a b,nodes":          "cert,nodes",
		"averyveryverylongcategoryname,v6": "v6",
	} {
		if got := normAttnMute(in); got != want {
			t.Fatalf("normAttnMute(%q) = %q,应为 %q", in, got, want)
		}
	}
}

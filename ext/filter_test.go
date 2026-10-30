package ext

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestFilterHidesAndRenames(t *testing.T) {
	vmess := `vmess://eyJ2IjoiMiIsInBzIjoi6aaZ5rivIiwiYWRkIjoiMS4xLjEuMSIsInBvcnQiOiI0NDMiLCJpZCI6ImEifQ==`
	it := Items{
		Links: []string{"trojan://p@1.1.1.1:1#%E5%AE%98%E7%BD%91", "trojan://p@1.1.1.1:2#%E6%97%A5%E6%9C%AC", "trojan://p@1.1.1.1:3#US", vmess},
		Clash: []map[string]interface{}{{"name": "官网", "port": 1}, {"name": "日本", "port": 2}, {"name": "US", "port": 3}, {"name": "香港", "port": 4}},
	}
	hidden, _ := json.Marshal([]string{"US"})
	renames, _ := json.Marshal(map[string]string{"日本": "JP 高速", "香港": "HK"})
	f := ParseFilter(hidden, renames, "官网, 到期")
	got := f.Apply(it)

	names := func(list []string) []string {
		out := []string{}
		for _, l := range list {
			out = append(out, LinkName(l))
		}
		return out
	}
	if fmt.Sprint(names(got.Links)) != "[JP 高速 HK]" {
		t.Fatalf("链接筛选不对: %v", names(got.Links))
	}
	var clash []string
	for _, p := range got.Clash {
		clash = append(clash, p["name"].(string))
	}
	if fmt.Sprint(clash) != "[JP 高速 HK]" {
		t.Fatalf("Clash 筛选不对: %v", clash)
	}
	// 原数据不能被改:抓取缓存解析一次会用很多回
	if it.Clash[1]["name"] != "日本" {
		t.Fatal("改名不能改到原数据")
	}
	// 改过名的 vmess 还是合法链接
	if LinkName(got.Links[1]) != "HK" {
		t.Fatalf("vmess 改名失败: %s", got.Links[1])
	}
}

func TestFilterEmptyKeepsEverything(t *testing.T) {
	it := Items{Links: []string{"trojan://p@1.1.1.1:1#a"}, Clash: []map[string]interface{}{{"name": "a"}}}
	got := ParseFilter(nil, nil, " ").Apply(it)
	if len(got.Links) != 1 || len(got.Clash) != 1 {
		t.Fatalf("没有筛选条件不该动: %+v", got)
	}
	// 关键词不分大小写、中英文分隔符都认
	f := ParseFilter(nil, nil, "EXPIRE，官网|到期、流量")
	for _, n := range []string{"expire 2026", "官网 xx", "到期:明天", "剩余流量"} {
		if hide, kw := f.Hides(n); !hide || !kw {
			t.Errorf("%s 应该被关键词排除", n)
		}
	}
	if hide, _ := f.Hides("日本 01"); hide {
		t.Error("不含关键词的不该被排除")
	}
}

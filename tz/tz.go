// Package tz 统一面板的时区。
//
// VPS 多数跑在 UTC 上,若按服务器时区显示,页面上的时间会和用户预期差好几个小时;
// 若按浏览器时区显示,又会和数据面日志里的时间对不上。所以面板、订阅落地页、流量图分桶
// 一律使用设置里的时区(默认 Asia/Shanghai),与服务器和浏览器所在时区无关。
package tz

import (
	"strings"
	"time"
	_ "time/tzdata" // 内嵌时区库:精简镜像/系统没装 tzdata 时也能解析 Asia/Shanghai 等名字
)

// Default 未设置时使用的时区。
const Default = "Asia/Shanghai"

// Location 解析时区名;为空或无效时回落到 Default,再不行才用进程本地时区。
func Location(name string) *time.Location {
	if name = strings.TrimSpace(name); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation(Default); err == nil {
		return loc
	}
	return time.Local
}

// Valid 判断时区名是否可用(设置保存前校验)。
func Valid(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true // 留空 = 用默认
	}
	_, err := time.LoadLocation(strings.TrimSpace(name))
	return err == nil
}

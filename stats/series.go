// Package stats 把落库的流量样本(model.Stats)聚合成时段柱:面板的流量图和订阅落地页的"用量情况"共用这一份。
package stats

import (
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// Point 一个时段:起点时间戳与上/下行字节数。
type Point struct {
	T    int64 `json:"t"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// Result 一次查询的结果。
type Result struct {
	Points    []Point `json:"points"`
	Span      int64   `json:"span"`
	Start     int64   `json:"start"`
	End       int64   `json:"end"`
	TotalUp   int64   `json:"totalUp"`
	TotalDown int64   `json:"totalDown"`
}

// BucketFor 按时间范围选桶宽(秒):≤1h→5min,≤6h→30min,≤24h→1h,≤7d→6h,更长→1d。与前端 chart.js 一致。
func BucketFor(hours int) int64 {
	switch {
	case hours <= 1:
		return 300
	case hours <= 6:
		return 1800
	case hours <= 24:
		return 3600
	case hours <= 168:
		return 21600
	}
	return 86400
}

// Series 查最近 hours 小时里 resource(user/inbound/outbound)下 tag 的流量,按 bucket 秒聚合。
//
// bucket ≥ floor(落库桶宽)时起点对齐到桶边界:日桶对齐到 loc 时区的零点,小时级桶对齐到当天内的整数倍
// (6h 桶 → 0/6/12/18 点),更细的桶按 UTC 取整;bucket 不合法时自动取 ≤240 个点。tag 为空表示全部。
func Series(db *gorm.DB, resource, tag string, hours int, bucket int64, floor int, loc *time.Location) Result {
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	if floor < 1 {
		floor = 60
	}
	if loc == nil {
		loc = time.Local
	}
	end := time.Now().Unix()
	start := end - int64(hours)*3600
	var span int64
	var n int
	if bucket >= int64(floor) && bucket <= 7*86400 {
		span = bucket
		st := time.Unix(start, 0).In(loc)
		var aligned time.Time
		switch {
		case span >= 86400:
			aligned = time.Date(st.Year(), st.Month(), st.Day(), 0, 0, 0, 0, loc)
		case span >= 3600:
			secOfDay := int64(st.Hour())*3600 + int64(st.Minute())*60 + int64(st.Second())
			midnight := time.Date(st.Year(), st.Month(), st.Day(), 0, 0, 0, 0, loc)
			aligned = midnight.Add(time.Duration(secOfDay-secOfDay%span) * time.Second)
		default:
			aligned = time.Unix(start-start%span, 0)
		}
		start = aligned.Unix()
		n = int((end-start)/span) + 1
	} else {
		n = 240
		if maxB := int((end - start) / int64(floor)); maxB < n {
			n = maxB
		}
		if n < 1 {
			n = 1
		}
		span = (end - start) / int64(n)
		if span == 0 {
			span = 1
		}
	}
	q := db.Model(&model.Stats{}).Where("resource = ? AND date_time > ? AND date_time <= ?", resource, start, end)
	if tag != "" {
		q = q.Where("tag = ?", tag)
	}
	var rows []model.Stats
	q.Order("date_time asc").Find(&rows)

	res := Result{Points: make([]Point, n), Span: span, Start: start, End: end}
	for i := range res.Points {
		res.Points[i].T = start + int64(i)*span
	}
	for _, row := range rows {
		i := int((row.DateTime - start) / span)
		if i < 0 {
			i = 0
		}
		if i >= n {
			i = n - 1
		}
		if row.Direction {
			res.Points[i].Up += row.Traffic
			res.TotalUp += row.Traffic
		} else {
			res.Points[i].Down += row.Traffic
			res.TotalDown += row.Traffic
		}
	}
	return res
}

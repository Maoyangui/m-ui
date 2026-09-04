// 截图用的演示数据:往一个已经建好用户与线路的 m-ui 库里写用量、最近在线时间和
// 最近 24 小时的流量时序,让概览图表和用户列表看起来像在跑,而不是一片 0。
//
// 只写演示数值,不碰凭据、不建用户;用户与线路从库里读。用法:
//
//	go run ./docs/screenshots/seed -db /tmp/demo.db
//
// 这是开发工具,不参与发布二进制的构建。
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func main() {
	dbPath := flag.String("db", "m-ui.db", "m-ui 数据库路径")
	flag.Parse()

	db, err := database.Open(*dbPath)
	if err != nil {
		panic(err)
	}
	defer database.Close(db)

	var users []model.User
	db.Order("id asc").Find(&users)
	var lines []model.Line
	db.Order("id asc").Find(&lines)
	if len(users) == 0 || len(lines) == 0 {
		panic("库里还没有用户或线路,先通过面板 / API 建好再灌数据")
	}

	r := rand.New(rand.NewSource(7)) // 固定种子:每次生成的图一致
	now := time.Now().Unix()
	const gb = 1 << 30

	// 用量:每个用户不同,最近在线时间在一小时内
	for i, u := range users {
		up := int64(float64(gb) * (0.6 + float64(i)*0.9 + r.Float64()))
		down := int64(float64(gb) * (4 + float64(i)*6 + r.Float64()*3))
		db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
			"up": up, "down": down, "online_at": now - int64(r.Intn(3600)),
		})
	}

	// 时序:24 个小时桶,用户维度按用量比例,线路维度均分;晚间略高,像真实曲线
	db.Where("resource IN ('user','line')").Delete(&model.Stats{})
	for h := 23; h >= 0; h-- {
		ts := now - int64(h)*3600
		ts -= ts % 3600
		hour := time.Unix(ts, 0).Hour()
		shape := 0.5 + 0.5*float64((hour+6)%24)/24 // 简单的昼夜起伏
		for i, u := range users {
			base := float64(gb) * 0.08 * (1 + float64(i)*0.7) * shape
			for _, dir := range []bool{false, true} {
				v := base
				if dir {
					v *= 0.18
				}
				db.Create(&model.Stats{DateTime: ts, Resource: "user", Tag: u.Name, Direction: dir, Traffic: int64(v * (0.7 + r.Float64()*0.6))})
			}
		}
		for i, l := range lines {
			base := float64(gb) * 0.12 * (1 + float64(i)*0.4) * shape
			for _, dir := range []bool{false, true} {
				v := base
				if dir {
					v *= 0.18
				}
				db.Create(&model.Stats{DateTime: ts, Resource: "line", Tag: l.Name, Direction: dir, Traffic: int64(v * (0.7 + r.Float64()*0.6))})
			}
		}
	}
	fmt.Printf("已写入演示数据:%d 个用户、%d 条线路、24 小时时序\n", len(users), len(lines))
}

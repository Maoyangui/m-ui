package sub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/upstream"

	"gorm.io/gorm"
)

// 线路作上游(链式代理):某台服务器上的一条线路,被别的线路当作出口。
// 上游的出站参数由线路算出来(和订阅同一套,客户端怎么连它,上游就怎么连),线路或服务器地址一变就重算;
// 连接用上游自己的一份专用凭据(Upstream.RelayCreds),渲染时作为一个内部用户挂到目标线路的入站上(见 render.loadLineUsers),
// 不占任何真实用户的配额,流量只算一次:用户的用量在入口那台记。

// RelayOutbound 算出上游连到这条线路要用的出站(类型与参数)。
func RelayOutbound(db *gorm.DB, up model.Upstream, line model.Line) (*upstream.Parsed, error) {
	if !render.LineOnNode(line, up.FromNode) {
		return nil, errors.New("这条线路没有部署在所选的服务器上")
	}
	e, ok := relayEntry(db, up.FromNode)
	if !ok {
		return nil, errors.New("所选的服务器不存在、已停用,或还不知道它的地址")
	}
	addrs := resolveAddrs(line, []Entry{e}, nil)
	if len(addrs) == 0 {
		return nil, errors.New("这条线路在所选的服务器上没有可连的地址")
	}
	user := model.User{Name: model.RelayUserName(up.Id), Credentials: up.RelayCreds}
	uris := lineToURIs(line, user, addrs[0], up.Name)
	if len(uris) == 0 {
		return nil, fmt.Errorf("不支持把 %s 线路用作上游", line.Protocol)
	}
	p, err := upstream.ParseLink(uris[0])
	if err != nil {
		if p = httpLink(uris[0]); p == nil {
			return nil, err
		}
	}
	return p, nil
}

// relayEntry 这台服务器在订阅里的入口(地址、SNI、是否允许不安全),和用户拿到的一样。
func relayEntry(db *gorm.DB, nodeID uint) (Entry, bool) {
	for _, e := range NewServer(db).options().Entries {
		if e.NodeId == nodeID {
			return e, true
		}
	}
	return Entry{}, false
}

// FillRelay 按线路算好上游的类型与参数;校验失败返回错误,上游不动。
func FillRelay(db *gorm.DB, up *model.Upstream) error {
	var line model.Line
	if err := db.First(&line, up.FromLine).Error; err != nil {
		return errors.New("所选的线路不存在")
	}
	p, err := RelayOutbound(db, *up, line)
	if err != nil {
		return err
	}
	up.Type, up.Options = p.Type, p.OptionsJSON()
	return nil
}

// RelayProblem 这条线路上游现在连不上的原因(给面板看);没问题返回空。
func RelayProblem(db *gorm.DB, up model.Upstream) string {
	var line model.Line
	if err := db.First(&line, up.FromLine).Error; err != nil {
		return errors.New("所选的线路不存在").Error()
	}
	if !line.Enabled {
		return errors.New("这条线路已停用").Error()
	}
	if _, err := RelayOutbound(db, up, line); err != nil {
		return err.Error()
	}
	return ""
}

// SyncRelayUpstreams 线路的端口、凭据参数、服务器地址变了,上游跟着改。返回有没有改动;算不出来的(线路停用、服务器下线)
// 保持上一份,不拖垮别的配置。副机不跑这个:上游随快照下发。
func SyncRelayUpstreams(db *gorm.DB) (bool, error) {
	var ups []model.Upstream
	if err := db.Where("from_line > 0").Find(&ups).Error; err != nil {
		return false, err
	}
	changed := false
	var firstErr error
	for _, up := range ups {
		var line model.Line
		if db.First(&line, up.FromLine).Error != nil {
			continue
		}
		p, err := RelayOutbound(db, up, line)
		if err != nil {
			continue
		}
		opts := p.OptionsJSON()
		if up.Type == p.Type && sameOptions(up.Options, opts) {
			continue
		}
		if err := db.Model(&model.Upstream{}).Where("id = ?", up.Id).
			Updates(map[string]interface{}{"type": p.Type, "options": opts}).Error; err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		changed = true
	}
	return changed, firstErr
}

func sameOptions(a, b []byte) bool {
	var x, y interface{}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	p, _ := json.Marshal(x)
	q, _ := json.Marshal(y)
	return bytes.Equal(p, q)
}

// RelayLoopOf 把这条线路上游指到目标线路后,用它的线路会不会绕回去。
func RelayLoopOf(db *gorm.DB, up model.Upstream) string {
	var target model.Line
	if db.First(&target, up.FromLine).Error != nil {
		return ""
	}
	var lines []model.Line
	db.Select("id, name, upstream_id, route_rules").Find(&lines)
	var all []model.Upstream
	db.Select("id, name, type, options").Find(&all)
	for _, l := range lines {
		for _, id := range render.ExpandIDs(render.LineUpstreams(l), all) { // 线路经上游组用到它也算
			if id == up.Id { // l 用这条上游:从目标线路走下去如果能回到 l,就是环
				if l.Id == target.Id {
					return l.Name + " → " + target.Name
				}
				if p := relayPath(db, target, l.Id, up.Id); p != "" {
					return l.Name + " → " + p
				}
			}
		}
	}
	return ""
}

// relayPath 从 from 这条线路顺着线路上游走,能不能走到 to。skip 是正在改的那条上游(它的目标还是旧的,不算)。
func relayPath(db *gorm.DB, from model.Line, to, skip uint) string {
	var ups []model.Upstream
	db.Where("from_line > 0").Find(&ups)
	var all []model.Upstream
	db.Select("id, name, type, options").Find(&all)
	byID := make(map[uint]model.Upstream, len(ups))
	for _, u := range ups {
		if u.Id != skip {
			byID[u.Id] = u
		}
	}
	seen := map[uint]bool{from.Id: true}
	var walk func(l model.Line, path []string) string
	walk = func(l model.Line, path []string) string {
		path = append(path, l.Name)
		if l.Id == to {
			return strings.Join(path, " → ")
		}
		for _, id := range render.ExpandIDs(render.LineUpstreams(l), all) {
			up, ok := byID[id]
			if !ok || seen[up.FromLine] {
				continue
			}
			seen[up.FromLine] = true
			var next model.Line
			if db.Select("id, name, upstream_id, route_rules").First(&next, up.FromLine).Error == nil {
				if p := walk(next, path); p != "" {
					return p
				}
			}
		}
		return ""
	}
	return walk(from, nil)
}

// RelayLoop 这条线路用这些上游会不会绕回自己(A 的上游是线路 B,B 的上游又是线路 A)。
// 有环的话流量在几台机器之间打转直到耗尽,所以保存时就拦下。返回绕回去的路径,没有环返回空。
func RelayLoop(db *gorm.DB, start model.Line) string {
	var ups []model.Upstream
	db.Where("from_line > 0").Find(&ups)
	if len(ups) == 0 {
		return ""
	}
	byID := make(map[uint]model.Upstream, len(ups))
	for _, u := range ups {
		byID[u.Id] = u
	}
	var all []model.Upstream
	db.Select("id, name, type, options").Find(&all)
	var lines []model.Line
	db.Select("id, name, upstream_id, route_rules").Find(&lines)
	lineByID := make(map[uint]model.Line, len(lines)+1)
	for _, l := range lines {
		lineByID[l.Id] = l
	}
	if start.Id != 0 {
		lineByID[start.Id] = start
	}
	seen := map[uint]bool{}
	var walk func(l model.Line, path []string) string
	walk = func(l model.Line, path []string) string {
		path = append(path, l.Name)
		for _, id := range render.ExpandIDs(render.LineUpstreams(l), all) {
			up, ok := byID[id]
			if !ok {
				continue
			}
			if up.FromLine == start.Id && start.Id != 0 {
				return strings.Join(append(path, start.Name), " → ")
			}
			if seen[up.FromLine] {
				continue
			}
			seen[up.FromLine] = true
			if next, ok := lineByID[up.FromLine]; ok {
				if p := walk(next, path); p != "" {
					return p
				}
			}
		}
		return ""
	}
	return walk(start, nil)
}

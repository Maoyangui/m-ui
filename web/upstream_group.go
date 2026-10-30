package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/sub"
)

// 上游组保存前:成员要存在、至少两条、不能套组;测试地址和间隔由这里定,表单只选成员。
func (s *Server) validateGroup(up *model.Upstream) error {
	var in struct {
		Outbounds []string `json:"outbounds"`
	}
	if err := json.Unmarshal(up.Options, &in); err != nil {
		return fmt.Errorf("上游参数不是合法 JSON: %w", err)
	}
	seen := map[string]bool{}
	members := make([]string, 0, len(in.Outbounds))
	for _, name := range in.Outbounds {
		if name = strings.TrimSpace(name); name != "" && !seen[name] {
			seen[name] = true
			members = append(members, name)
		}
	}
	if len(members) < 2 {
		return errors.New("上游组至少要选两条上游")
	}
	if seen[up.Name] {
		return errors.New("上游组不能包含它自己")
	}
	var found []model.Upstream
	s.db.Where("name IN ?", members).Find(&found)
	byName := make(map[string]model.Upstream, len(found))
	for _, u := range found {
		byName[u.Name] = u
	}
	for _, name := range members {
		u, ok := byName[name]
		if !ok {
			return fmt.Errorf("上游组的成员 %q 不存在", name)
		}
		if render.IsGroup(u) {
			return fmt.Errorf("上游组不能再套上游组:%q", name)
		}
	}
	if up.Id != 0 {
		var cur model.Upstream
		if s.db.First(&cur, up.Id).Error == nil && !render.IsGroup(cur) {
			if g := groupsWith(s.db, cur.Name); len(g) > 0 {
				return fmt.Errorf("这条上游在上游组「%s」里,不能改成组", g[0])
			}
		}
	}
	// 容差给大一点:现用的这条只要还通、又没比最快的慢太多就不换,免得连接跟着延迟抖动来回切。
	// 换了就断开旧连接:换说明旧的多半挂了,挂着的连接等超时要等好几分钟,断开了应用马上重连
	up.Options, _ = json.Marshal(map[string]interface{}{"outbounds": members, "url": s.groupTestURL(), "interval": "30s", "tolerance": 300, "interrupt_exist_connections": true})
	up.FromLine, up.FromNode, up.RelayCreds = 0, 0, nil
	return nil
}

func (s *Server) groupTestURL() string {
	if u := s.setting("upstreamTestUrl"); u != "" {
		return u
	}
	return "http://www.gstatic.com/generate_204"
}

// syncGroupTestURL 设置里的测试地址改了:上游组是内核自己定时测的,地址在它自己的参数里,要跟着改。返回有没有改动。
func (s *Server) syncGroupTestURL() bool {
	var groups []model.Upstream
	s.db.Where("type = ?", render.GroupType).Find(&groups)
	want := s.groupTestURL()
	changed := false
	for _, g := range groups {
		var o map[string]interface{}
		if json.Unmarshal(g.Options, &o) != nil || o["url"] == want {
			continue
		}
		o["url"] = want
		b, _ := json.Marshal(o)
		if s.db.Model(&model.Upstream{}).Where("id = ?", g.Id).Update("options", json.RawMessage(b)).Error == nil {
			changed = true
		}
	}
	return changed
}

// groupLoopCheck 上游组的成员变了:用这个组的线路会不会绕回自己。上游里没写着线路,得把线路都走一遍。
func groupLoopCheck(tx *gorm.DB) error {
	var lines []model.Line
	if err := tx.Select("id, name, upstream_id, route_rules").Find(&lines).Error; err != nil {
		return err
	}
	for _, l := range lines {
		if p := sub.RelayLoop(tx, l); p != "" {
			return fmt.Errorf("这样会绕成环,流量出不去:%s", p)
		}
	}
	return nil
}

// groupsWith 哪些上游组包含这条上游。
func groupsWith(db *gorm.DB, name string) []string {
	var groups []model.Upstream
	db.Where("type = ?", render.GroupType).Find(&groups)
	var out []string
	for _, g := range groups {
		for _, m := range render.GroupMembers(g) {
			if m == name {
				out = append(out, g.Name)
				break
			}
		}
	}
	return out
}

// renameGroupMember 上游改名:组里写的是名字,一起改。
func renameGroupMember(tx *gorm.DB, oldName, newName string) error {
	var groups []model.Upstream
	if err := tx.Where("type = ?", render.GroupType).Find(&groups).Error; err != nil {
		return err
	}
	for _, g := range groups {
		var o map[string]interface{}
		if json.Unmarshal(g.Options, &o) != nil {
			continue
		}
		list, _ := o["outbounds"].([]interface{})
		changed := false
		for i, m := range list {
			if m == oldName {
				list[i], changed = newName, true
			}
		}
		if !changed {
			continue
		}
		b, _ := json.Marshal(o)
		if err := tx.Model(&model.Upstream{}).Where("id = ?", g.Id).Update("options", json.RawMessage(b)).Error; err != nil {
			return err
		}
	}
	return nil
}

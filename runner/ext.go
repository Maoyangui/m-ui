package runner

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/logger"
)

// ExtRefreshResult 是一次外部订阅抓取的结果。
type ExtRefreshResult struct {
	Links int    `json:"links"`
	Clash int    `json:"clash"`
	Error string `json:"error,omitempty"`
}

// RefreshExt 立即抓取一个外部订阅并更新缓存;单条链接类型只重新解析。
func (r *Runner) RefreshExt(id uint) (ExtRefreshResult, error) {
	var e model.ExtNode
	if err := r.db.First(&e, id).Error; err != nil {
		return ExtRefreshResult{}, errors.New("外部节点不存在")
	}
	now := time.Now().Unix()
	if e.Type == "link" {
		it := ext.Parse(e.Value)
		r.db.Model(&model.ExtNode{}).Where("id = ?", id).Updates(map[string]interface{}{"node_count": len(it.Clash), "last_fetch": now, "last_error": ""})
		return ExtRefreshResult{Links: len(it.Links), Clash: len(it.Clash)}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := ext.Fetch(ctx, e.Value)
	if err != nil {
		r.db.Model(&model.ExtNode{}).Where("id = ?", id).Updates(map[string]interface{}{"last_fetch": now, "last_error": err.Error()})
		return ExtRefreshResult{Error: err.Error()}, err
	}
	it := ext.Parse(string(body))
	if len(it.Links) == 0 && len(it.Clash) == 0 {
		msg := "内容里没有可识别的节点"
		r.db.Model(&model.ExtNode{}).Where("id = ?", id).Updates(map[string]interface{}{"last_fetch": now, "last_error": msg})
		return ExtRefreshResult{Error: msg}, errors.New(msg)
	}
	r.db.Model(&model.ExtNode{}).Where("id = ?", id).Updates(map[string]interface{}{
		"cache": string(body), "node_count": len(it.Clash), "last_fetch": now, "last_error": "",
	})
	logger.Info("外部订阅 ", e.Name, " 已更新:", len(it.Clash), " 个节点")
	return ExtRefreshResult{Links: len(it.Links), Clash: len(it.Clash)}, nil
}

// extLoop 主机按 extRefreshMinutes(默认 30)定期刷新外部订阅;副机的缓存随快照下发。
func (r *Runner) extLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if r.IsNode() {
				continue
			}
			minutes, err := strconv.Atoi(strings.TrimSpace(r.setting("extRefreshMinutes")))
			if err != nil || minutes <= 0 {
				minutes = 30
			}
			cutoff := time.Now().Unix() - int64(minutes)*60
			var due []model.ExtNode
			r.db.Where("type = ? AND enabled = ? AND last_fetch < ?", "sub", true, cutoff).Find(&due)
			for _, e := range due {
				if _, err := r.RefreshExt(e.Id); err != nil {
					logger.Warning("外部订阅 ", e.Name, " 刷新失败: ", err)
				}
			}
		case <-stop:
			return
		}
	}
}

package runner

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/logger"
)

// ExtRefreshResult 是一次外部订阅抓取的结果。
type ExtRefreshResult struct {
	Links     int    `json:"links"`
	Clash     int    `json:"clash"`
	Total     int    `json:"total"`               // 解析出的节点数
	Out       int    `json:"out"`                 // 输出到用户订阅的节点数(扣除隐藏、排除的)
	Unchanged bool   `json:"unchanged,omitempty"` // 内容和上次一样,没有改写缓存
	Error     string `json:"error,omitempty"`
}

// refreshTimeout 一次刷新的总时限:每个抓取身份各自 25 秒(见 ext.Fetch),两个身份都试也够用。
const refreshTimeout = 60 * time.Second

func (r *Runner) extLock(id uint) *sync.Mutex {
	v, _ := r.extLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// RefreshExt 立即抓取一个外部订阅并更新缓存;单条链接类型只重新解析。
//
// 同一条的手动刷新与定时刷新排队执行:排队期间前一个刚抓完(地址没变),直接用它的结果,不再抓第二次 —— 以前两边同时抓,
// 后写的覆盖先写的。抓取失败、或解析不出节点时保留旧缓存,只记错误与尝试时间;内容和上次一样只更新抓取时间,不改写缓存。
func (r *Runner) RefreshExt(id uint) (ExtRefreshResult, error) {
	var before model.ExtNode
	if err := r.db.First(&before, id).Error; err != nil {
		return ExtRefreshResult{}, errors.New("外部节点不存在")
	}
	return r.refreshExtAfter(id, before)
}

// refreshExtAfter before 是调用方来刷新时库里的样子(排队期间别人刷新过没有,和它比)。
func (r *Runner) refreshExtAfter(id uint, before model.ExtNode) (ExtRefreshResult, error) {
	mu := r.extLock(id)
	mu.Lock()
	defer mu.Unlock()
	var e model.ExtNode
	if err := r.db.First(&e, id).Error; err != nil {
		return ExtRefreshResult{}, errors.New("外部节点不存在")
	}
	if e.Type == before.Type && e.Value == before.Value && attemptAt(e) > attemptAt(before) {
		// 排队等锁的工夫,另一个刷新刚做完同一个地址
		res := resultOf(e)
		if e.LastError != "" {
			return res, errors.New(e.LastError)
		}
		return res, nil
	}
	now := time.Now().Unix()
	// 只在地址没被改掉时写回:抓取期间管理员改了地址,这份结果作废(改地址那一下已经另起一次刷新)
	write := func(updates map[string]interface{}) {
		r.db.Model(&model.ExtNode{}).Where("id = ? AND type = ? AND value = ?", id, e.Type, e.Value).Updates(updates)
	}
	if e.Type == "link" {
		it := ext.Parse(e.Value)
		out, total := ext.Counts(e)
		write(map[string]interface{}{"node_count": out, "node_total": total, "content_sig": ext.ContentSig(it),
			"last_fetch": now, "last_try": now, "last_error": ""})
		return ExtRefreshResult{Links: len(it.Links()), Clash: len(it.Clash()), Total: total, Out: out}, nil
	}
	prev := e.FetchAgent
	if prev == "" && e.Cache != "" {
		prev = "?" // 老数据:有缓存但没记身份,ext.Fetch 按当时的默认身份算
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	body, agent, err := ext.Fetch(ctx, e.Value, prev)
	if err != nil {
		write(map[string]interface{}{"last_try": now, "last_error": err.Error()})
		return ExtRefreshResult{Error: err.Error()}, err
	}
	it := ext.Parse(string(body))
	if len(it.Nodes) == 0 {
		err := errors.New("内容里没有可识别的节点") // 写成 errors.New:英文界面的错误翻译表靠扫这种写法收录
		write(map[string]interface{}{"last_try": now, "last_error": err.Error()})
		return ExtRefreshResult{Error: err.Error()}, err
	}
	sig := ext.ContentSig(it)
	updates := map[string]interface{}{"last_fetch": now, "last_try": now, "last_error": "", "fetch_agent": agent}
	unchanged := sig == e.ContentSig && e.Cache != ""
	if !unchanged {
		e.Cache = string(body)
		updates["cache"], updates["content_sig"] = e.Cache, sig
	}
	out, total := ext.Counts(e)
	updates["node_count"], updates["node_total"] = out, total
	write(updates)
	if !unchanged {
		logger.Info("外部订阅 ", e.Name, " 已更新:", total, " 个节点,输出 ", out, " 个")
	}
	return ExtRefreshResult{Links: len(it.Links()), Clash: len(it.Clash()), Total: total, Out: out, Unchanged: unchanged}, nil
}

// attemptAt 最近一次抓取(成功或失败)的时间。
func attemptAt(e model.ExtNode) int64 {
	if e.LastTry > e.LastFetch {
		return e.LastTry
	}
	return e.LastFetch
}

func resultOf(e model.ExtNode) ExtRefreshResult {
	it := ext.ItemsOf(e)
	return ExtRefreshResult{Links: len(it.Links()), Clash: len(it.Clash()), Total: e.NodeTotal, Out: e.NodeCount, Error: e.LastError}
}

// dueExts 到期该刷新的外部订阅:最近一次成功和最近一次尝试都早于 cutoff。
// 老库升级上来 last_try 是 NULL(和任何数比较都不成立,以前那样写的话老订阅永远不再定时刷新):按 0 算。
func (r *Runner) dueExts(cutoff int64) []model.ExtNode {
	var due []model.ExtNode
	r.db.Where("type = ? AND enabled = ? AND COALESCE(last_fetch, 0) < ? AND COALESCE(last_try, 0) < ?", "sub", true, cutoff, cutoff).Find(&due)
	return due
}

// extLoop 主机按 extRefreshMinutes(默认 30)定期刷新外部订阅;副机的缓存随快照下发。
// 失败的也按间隔重试(看最近一次尝试的时间),不会每分钟都去敲一个挂了的订阅地址。
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
			for _, e := range r.dueExts(time.Now().Unix() - int64(minutes)*60) {
				if _, err := r.RefreshExt(e.Id); err != nil {
					logger.Warning("外部订阅 ", e.Name, " 刷新失败: ", err)
				}
			}
		case <-stop:
			return
		}
	}
}

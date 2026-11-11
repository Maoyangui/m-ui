package ha

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 停用 / 删除副机时让它删掉手里的副本(删除时再让它离开集群:清纪元、删备用密钥)。那台当时多半连不上,
// 所以请求连同地址、令牌、证书指纹记进本机设置 haDropPending,副本循环每轮看一眼,到点就重试,直到对方回 200
// (或回 404 = 老版本 / 没有这个接口,没有可删的)。停用的那台后来又被启用了,就不再催它。

// dropTask 一台待删副本的副机。
type dropTask struct {
	Id       uint   `json:"id"`
	Name     string `json:"name"`
	ApiUrl   string `json:"apiUrl"`
	Token    string `json:"token"`
	CertFP   string `json:"certFp,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
	Leave    bool   `json:"leave,omitempty"`   // 删除:删副本之后再让它离开集群
	Dropped  bool   `json:"dropped,omitempty"` // 副本已删(只差离开)
	At       int64  `json:"at"`
	Tries    int    `json:"tries,omitempty"`
	Next     int64  `json:"next,omitempty"`
	Err      string `json:"err,omitempty"`
}

func (s *Service) dropTasks() map[uint]dropTask {
	m := map[uint]dropTask{}
	_ = s.getJSON(KeyDropPending, &m)
	return m
}

func (s *Service) saveDropTasks(m map[uint]dropTask) {
	v := ""
	if len(m) > 0 {
		v = mustJSON(m)
	}
	_ = s.set(map[string]string{KeyDropPending: v})
}

// QueueDrop 停用(leave=false)/ 删除(leave=true)一台副机后:记下要它删副本,马上试一次,之后由副本循环重试。
// 没进过纪元、本机是副机、没有地址或令牌时什么都不做。
func (s *Service) QueueDrop(n model.Node, leave bool) {
	if s.Epoch() == 0 || s.IsNode() || n.IsLocal || strings.TrimSpace(n.ApiUrl) == "" || n.Token == "" {
		return
	}
	s.rep.dropMu.Lock()
	m := s.dropTasks()
	t := m[n.Id]
	t.Id, t.Name, t.ApiUrl, t.Token, t.CertFP, t.Insecure = n.Id, n.Name, n.ApiUrl, n.Token, n.CertFP, n.Insecure
	t.Leave = t.Leave || leave
	t.At, t.Next = s.now().Unix(), 0
	m[n.Id] = t
	s.saveDropTasks(m)
	s.rep.dropMu.Unlock()
	go s.rep.runDrops()
}

// retryDrops 副本循环里:有到点的就在后台重试(同一时间只有一轮)。
func (r *replicator) retryDrops() {
	if r.s.setting(KeyDropPending) == "" {
		return
	}
	go r.runDrops()
}

func (r *replicator) runDrops() {
	s := r.s
	r.dropMu.Lock()
	if r.dropRunning {
		r.dropMu.Unlock()
		return
	}
	r.dropRunning = true
	due := []dropTask{}
	now := s.now().Unix()
	for _, t := range s.dropTasks() {
		if t.Next <= now {
			due = append(due, t)
		}
	}
	r.dropMu.Unlock()
	defer func() {
		r.dropMu.Lock()
		r.dropRunning = false
		r.dropMu.Unlock()
	}()
	for _, t := range due {
		done, dropped, err := r.dropOne(t)
		r.dropMu.Lock()
		m := s.dropTasks()
		cur, ok := m[t.Id]
		if !ok || cur.At != t.At { // 期间被取消或重新排过
			r.dropMu.Unlock()
			continue
		}
		switch {
		case done:
			delete(m, t.Id)
		default:
			cur.Dropped = cur.Dropped || dropped
			cur.Tries++
			wait := time.Minute << min(cur.Tries-1, 6)
			if wait > dropRetryMax {
				wait = dropRetryMax
			}
			cur.Next = s.now().Add(wait).Unix()
			if err != nil {
				cur.Err = err.Error()
			}
			m[t.Id] = cur
		}
		s.saveDropTasks(m)
		r.dropMu.Unlock()
	}
}

// dropOne 对一台试一次:返回做完没有、副本删了没有。
func (r *replicator) dropOne(t dropTask) (done, dropped bool, err error) {
	s := r.s
	if s.d.Hub == nil || s.Epoch() == 0 || s.IsNode() {
		return false, false, nil
	}
	var row model.Node
	exists := s.d.DB.Where("id = ?", t.Id).Limit(1).Find(&row).Error == nil && row.Id != 0
	if exists && row.Enabled {
		return true, false, nil // 管理员又把它启用了(或重新加了回来):照常当副机,不再催它删
	}
	n := model.Node{Id: t.Id, Name: t.Name, ApiUrl: t.ApiUrl, Token: t.Token, CertFP: t.CertFP, Insecure: t.Insecure}
	if !t.Dropped {
		if err := s.d.Hub.Send(n, http.MethodPost, "ha/drop", nil, 0, nil, 15*time.Second, nil); err != nil && !notFound(err) {
			return false, false, err
		}
		logger.Info("容灾:副机 ", t.Name, " 已删掉手里的副本")
		dropped = true
	}
	if t.Leave {
		if err := s.d.Hub.Send(n, http.MethodPost, "ha/leave", nil, 0, nil, 15*time.Second, nil); err != nil && !notFound(err) {
			return false, dropped, err
		}
		logger.Info("容灾:已删除的副机 ", t.Name, " 已离开集群(清了纪元与备用密钥)")
	}
	return true, true, nil
}

// notFound 对方回 404:老版本没有这个接口,手里也不会有副本。
func notFound(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "HTTP "+strconv.Itoa(http.StatusNotFound)+":")
}

// DropPending 还没确认删掉副本的副机(面板显示)。
func (s *Service) DropPending() []DropPendingView {
	var out []DropPendingView
	for _, t := range s.dropTasks() {
		out = append(out, DropPendingView{Id: t.Id, Name: t.Name, Leave: t.Leave, Since: t.At, Tries: t.Tries, Err: t.Err})
	}
	return out
}

// DropPendingView 面板上显示的一项(不带地址与令牌)。
type DropPendingView struct {
	Id    uint   `json:"id"`
	Name  string `json:"name"`
	Leave bool   `json:"leave,omitempty"`
	Since int64  `json:"since"`
	Tries int    `json:"tries,omitempty"`
	Err   string `json:"err,omitempty"`
}

// RetryDrops 马上重试一轮(同步;测试用):正有一轮在跑就等它跑完再来一轮。
func (s *Service) RetryDrops() {
	for i := 0; i < 600; i++ {
		s.rep.dropMu.Lock()
		running := s.rep.dropRunning
		s.rep.dropMu.Unlock()
		if !running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.rep.runDrops()
}

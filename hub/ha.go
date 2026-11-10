package hub

// 主机容灾挂进主副机同步的几处。hub 不引用 ha 包(ha 要用 hub 的并账与请求),由 runner 把 ha.Service 接进来。
// HA 为 nil、或本机纪元为 0(没开过容灾)时,请求、推送、修订号都和以前一样。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// HAHooks 容灾在同步里的钩子。
type HAHooks interface {
	// Header 主机发往副机的请求带的纪元与主机 id;epoch 为 0 时不加请求头
	Header() (epoch uint64, master uint)
	// StateFor 这一轮给副机 n 的容灾状态;rev 为空 = 不推
	StateFor(n model.Node) (body []byte, rev string)
	// NoteReply 每次请求副机的纪元结果(降级判定用)
	NoteReply(n model.Node, r EpochReply)
}

// HAReport 副机随报告上报的容灾信息(开过容灾的副机才带;老主机不认这个字段,忽略)。
type HAReport struct {
	Epoch    uint64     `json:"epoch"`
	Role     string     `json:"role,omitempty"`
	Version  string     `json:"version,omitempty"`
	Pub      string     `json:"pub,omitempty"`     // 备用公钥(X25519):主机首次见到时记住,之后变了就不再发副本
	Replica  *HAReplica `json:"replica,omitempty"` // 手里那份副本
	LeaseAgo int64      `json:"leaseAgo"`          // 距上次收到当前主机的请求多少秒
	DiskFree int64      `json:"diskFree"`          // 数据目录所在盘的剩余字节(-1 = 读不到)
	Peers    *HAPeers   `json:"peers,omitempty"`   // 和别的副机互通的情况(互相确认与投票要用)
}

// HAReplica 备用手里那份副本。
type HAReplica struct {
	Epoch      uint64 `json:"epoch"`
	Seq        uint64 `json:"seq"`
	At         int64  `json:"at"`   // 生成时间
	Size       int64  `json:"size"` // 明文字节数
	AsOf       int64  `json:"asOf"` // 主机确认它到这时为止仍是最新的
	VerifiedAt int64  `json:"verifiedAt,omitempty"`
	Err        string `json:"err,omitempty"` // 自检失败的原因
}

// HAPeers 副机之间面板端口互通的情况。
type HAPeers struct {
	OK    int   `json:"ok"`
	Total int   `json:"total"`
	At    int64 `json:"at"`
}

// EpochReply 一次请求里副机回的纪元信息。
type EpochReply struct {
	Aware       bool   // 副机认得纪元(响应带 X-HA-Node,或回了纪元判定)
	Down        bool   // 没连上 / 超时
	NodeEpoch   uint64 // 副机自己的纪元
	Code        string // 409 的纪元判定:stale_epoch / still_following / epoch_not_adopted;空 = 放行
	Epoch       uint64 // 判定里副机认的纪元
	MasterId    uint   // 判定里副机认的主机
	SinceChange uint64
	LeaseAgo    int64 // 副机距上次收到它认的那台主机的请求多少秒
}

// EpochError 副机按纪元规则拒绝了请求(409 + code)。不算"推送被拒":不退避,也不当作配置有问题。
type EpochError struct {
	Code        string
	Epoch       uint64
	MasterId    uint
	SinceChange uint64
	Msg         string
}

// Error 给面板看的说明(用 fmt.Errorf 写:英文界面的译文表按它核对)。
func (e *EpochError) Error() string {
	switch e.Code {
	case "stale_epoch":
		return fmt.Errorf("这台副机已跟随纪元更高的主机(纪元 %d,主机 #%d):本机可能已经不是主机了", e.Epoch, e.MasterId).Error()
	case "still_following":
		return fmt.Errorf("这台副机还跟随原来的主机(纪元 %d):等它的租约到期,或设为主机的交接完成", e.Epoch).Error()
	}
	return fmt.Errorf("这台副机还没切到本机的纪元(它在纪元 %d):下一轮推送容灾状态后会切过来", e.Epoch).Error()
}

// IsEpochError 是不是纪元判定。
func IsEpochError(err error) bool {
	var ee *EpochError
	return errors.As(err, &ee)
}

// setHAHeader 给发往副机的请求带上纪元。
func (h *Hub) setHAHeader(req *http.Request) {
	if h.d.HA == nil {
		return
	}
	if e, m := h.d.HA.Header(); e > 0 {
		req.Header.Set("X-HA-Epoch", strconv.FormatUint(e, 10))
		req.Header.Set("X-HA-Master", strconv.FormatUint(uint64(m), 10))
	}
}

// noteReply 把这次请求的纪元结果交给容灾(没开容灾时什么都不做)。
func (h *Hub) noteReply(n model.Node, resp *http.Response, body []byte, err error) error {
	if h.d.HA == nil {
		return err
	}
	if e, _ := h.d.HA.Header(); e == 0 {
		return err
	}
	var r EpochReply
	switch {
	case resp == nil:
		r.Down = err != nil && !errors.Is(err, context.Canceled)
	default:
		if v := resp.Header.Get("X-HA-Node"); v != "" {
			r.Aware = true
			r.NodeEpoch, _ = strconv.ParseUint(v, 10, 64)
		}
		if resp.StatusCode == http.StatusConflict {
			var rej struct {
				Error       string `json:"error"`
				Code        string `json:"code"`
				Epoch       uint64 `json:"epoch"`
				MasterId    uint   `json:"masterId"`
				SinceChange uint64 `json:"sinceChange"`
				LeaseAgo    int64  `json:"leaseAgo"`
			}
			if json.Unmarshal(body, &rej) == nil && rej.Code != "" {
				r.Aware, r.Code, r.Epoch, r.MasterId, r.SinceChange, r.LeaseAgo = true, rej.Code, rej.Epoch, rej.MasterId, rej.SinceChange, rej.LeaseAgo
				err = &EpochError{Code: rej.Code, Epoch: rej.Epoch, MasterId: rej.MasterId, SinceChange: rej.SinceChange, Msg: rej.Error}
			}
		}
	}
	if !r.Down && !r.Aware && resp == nil {
		return err
	}
	h.d.HA.NoteReply(n, r)
	return err
}

// pushHAState 这一轮先给副机推容灾状态(变了才推,另每 10 分钟重推一次)。返回纪元判定的错误时,这一轮别的请求都不用发了。
func (h *Hub) pushHAState(n model.Node) error {
	if h.d.HA == nil {
		return nil
	}
	body, rev := h.d.HA.StateFor(n)
	if rev == "" {
		return nil
	}
	h.mu.Lock()
	if h.haPushed == nil {
		h.haPushed = map[uint]haPushed{}
	}
	last := h.haPushed[n.Id]
	h.mu.Unlock()
	if last.rev == rev && time.Since(last.at) < 10*time.Minute {
		return nil
	}
	if last.legacy && time.Since(last.at) < 10*time.Minute {
		return nil // 老副机没有这个接口:隔一会儿再试(升级后能接上)
	}
	var out struct {
		OK string `json:"ok"`
	}
	err := h.Send(n, http.MethodPost, "ha/state", bytes.NewReader(body), int64(len(body)), nil, 15*time.Second, &out)
	var hse *httpStatusError
	switch {
	case err == nil:
		h.mu.Lock()
		h.haPushed[n.Id] = haPushed{rev: rev, at: time.Now()}
		h.mu.Unlock()
	case errors.As(err, &hse) && hse.Status == http.StatusNotFound:
		h.mu.Lock()
		h.haPushed[n.Id] = haPushed{legacy: true, at: time.Now()}
		h.mu.Unlock()
		return nil
	case IsEpochError(err):
		return err
	case errors.As(err, &hse):
		logger.Warning("向副机 ", n.Name, " 推送容灾状态被拒: ", err) // 连不上不在这里记:这一轮拉报告也会失败,状态里有
	}
	return nil
}

type haPushed struct {
	rev    string
	at     time.Time
	legacy bool
}

// ForgetHAState 下一轮重推容灾状态(纪元、备用名单变了时容灾服务调用)。
func (h *Hub) ForgetHAState() {
	h.mu.Lock()
	h.haPushed = nil
	h.mu.Unlock()
}

// Send 给副机发一个请求(带令牌、纪元请求头、证书指纹校验),给容灾推副本、推状态、设为主机用。
// body 可为 nil;size 为请求体字节数(未知填 -1);out 非 nil 时把 200 的 JSON 响应解进去。
func (h *Hub) Send(n model.Node, method, path string, body io.Reader, size int64, hdr http.Header, timeout time.Duration, out interface{}) error {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(h.baseCtx(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, agentURL(n, path), body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	ct := req.Header.Get("Content-Type")
	err = h.do(n, req, out)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	return err
}

// StatusOf 一台副机的运行状态(没有就返回 false)。
func (h *Hub) StatusOf(id uint) (NodeStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.status[id]
	if !ok {
		return NodeStatus{}, false
	}
	return *st, true
}

// RunOnce 同步跑一轮定时同步并等它全部收尾(命令行与测试用)。
func (h *Hub) RunOnce() {
	h.tick()
	h.rounds.Wait()
	h.waitMerged()
}

// haErrText 纪元判定给面板看的说明。
func haErrText(err error) string {
	var ee *EpochError
	if errors.As(err, &ee) {
		return ee.Error()
	}
	return strings.TrimSpace(err.Error())
}

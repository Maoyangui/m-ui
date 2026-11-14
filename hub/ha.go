package hub

// 主机容灾挂进主副机同步的几处。hub 不引用 ha 包(ha 要用 hub 的并账与请求),由 runner 把 ha.Service 接进来。
// HA 为 nil、或本机纪元为 0(没开过容灾)时,请求、推送、修订号都和以前一样;每次请求的结果照样交给容灾记下
// (纪元为 0 的原主机也要认出副机已经跟了别的主机)。

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
	// NoteReply 每次请求副机的纪元结果(判定本机是不是该变为副机;本机纪元为 0 时也调)
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
	// ResellerPort 这台的代理面板端口(副机上不监听;被设为主机后才起,主机据此提前查端口冲突)
	ResellerPort int      `json:"resellerPort,omitempty"`
	Peers        *HAPeers `json:"peers,omitempty"` // 和别的副机互通的情况(互相确认与投票要用)
	// 以下为容灾修复契约(设计 3.2)新增
	Instance     string `json:"inst,omitempty"`         // 本机实例号(换机还原围栏用)
	WebPort      int    `json:"webPort,omitempty"`      // 面板端口
	SubPort      int    `json:"subPort,omitempty"`      // 订阅端口
	WarpPort     int    `json:"warpPort,omitempty"`     // WARP 端口(在用时)
	ResellerURL  string `json:"resellerUrl,omitempty"`  // 当主机后的代理面板地址
	ReplicaBytes int64  `json:"replicaBytes,omitempty"` // 手里两代副本文件占的磁盘
	SignFP       string `json:"signFp,omitempty"`       // 本机签名公钥指纹
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
	LeaseAgo    int64  // 副机距上次收到它认的那台主机的请求多少秒(回话的是主机时为 0)
	Role        string // 回话这台自己的角色:master / node / demoted / promoting
	MasterInst  string // 它认的主机的实例号
	BecameAt    int64  // 回话的是主机时:它在这个纪元当上主机的时间
}

// EpochError 副机按纪元规则拒绝了请求(409 + code)。不算"推送被拒":不退避,也不当作配置有问题。
type EpochError struct {
	Code        string
	Epoch       uint64
	MasterId    uint
	SinceChange uint64
	Role        string // 回话这台自己的角色(master = 它自己就是主机)
	Msg         string
}

// Error 给面板看的说明(用 fmt.Errorf 写:英文界面的译文表按它核对)。
func (e *EpochError) Error() string {
	switch {
	case e.Role == "master" && e.Code == "stale_epoch":
		return fmt.Errorf("这台服务器已经以纪元 %d 当主机:本机可能已经不是主机了", e.Epoch).Error()
	case e.Role == "master":
		return fmt.Errorf("这台服务器自己也在当主机(纪元 %d),不接受本机的管理", e.Epoch).Error()
	case e.Code == "stale_epoch":
		return fmt.Errorf("这台副机已跟随纪元更高的主机(纪元 %d,主机 #%d):本机可能已经不是主机了", e.Epoch, e.MasterId).Error()
	case e.Code == "still_following":
		return fmt.Errorf("这台副机还跟随原来的主机(纪元 %d):等它的租约到期,或设为主机的交接完成", e.Epoch).Error()
	}
	return fmt.Errorf("这台副机还没切到本机的纪元(它在纪元 %d):下一轮推送容灾状态后会切过来", e.Epoch).Error()
}

// epochCodes 409 里算纪元判定的代码(和 ha 包的一致;别的 409 —— 版本门槛之类 —— 不是纪元判定)。
var epochCodes = map[string]bool{"stale_epoch": true, "still_following": true, "epoch_not_adopted": true}

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

// noteReply 把这次请求的纪元结果交给容灾(没接容灾时什么都不做)。409 的纪元判定不管本机纪元是多少都解析成 EpochError:
// 纪元为 0 的主机(没开过容灾就设为主机、从开启前的备份还原回来)也要认得出"副机已经跟了别的主机"。
func (h *Hub) noteReply(n model.Node, resp *http.Response, body []byte, err error) error {
	if h.d.HA == nil {
		return err
	}
	var r EpochReply
	switch {
	case resp == nil:
		r.Down = err != nil && !errors.Is(err, context.Canceled)
		if !r.Down {
			return err
		}
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
				Role        string `json:"role"`
				MasterInst  string `json:"masterInst"`
				BecameAt    int64  `json:"becameAt"`
			}
			if json.Unmarshal(body, &rej) == nil && epochCodes[rej.Code] {
				r.Aware, r.Code, r.Epoch, r.MasterId, r.SinceChange, r.LeaseAgo = true, rej.Code, rej.Epoch, rej.MasterId, rej.SinceChange, rej.LeaseAgo
				r.Role, r.MasterInst, r.BecameAt = rej.Role, rej.MasterInst, rej.BecameAt
				err = &EpochError{Code: rej.Code, Epoch: rej.Epoch, MasterId: rej.MasterId, SinceChange: rej.SinceChange, Role: rej.Role, Msg: rej.Error}
			}
		}
		if r.Aware {
			h.forgetLegacy(n.Id, r.Code)
		}
	}
	h.d.HA.NoteReply(n, r)
	return err
}

// forgetLegacy 这台认得纪元了(升级上来了),或者它说还没切到本机的纪元:下一轮立刻推容灾状态,不等"老副机"的 10 分钟。
func (h *Hub) forgetLegacy(id uint, code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.haPushed[id]; ok && (p.legacy || code == "epoch_not_adopted") {
		delete(h.haPushed, id)
	}
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
		h.notePushed(n.Id, haPushed{rev: rev, at: time.Now()})
	case errors.As(err, &hse) && hse.Status == http.StatusNotFound:
		h.notePushed(n.Id, haPushed{legacy: true, at: time.Now()})
		return nil
	case IsEpochError(err):
		return err
	case errors.As(err, &hse):
		logger.Warning("向副机 ", n.Name, " 推送容灾状态被拒: ", err) // 连不上不在这里记:这一轮拉报告也会失败,状态里有
	}
	return nil
}

// notePushed 记下推成功的修订(推的途中容灾服务可能刚让 hub 全部重推:表被清空了就重建)。
func (h *Hub) notePushed(id uint, p haPushed) {
	h.mu.Lock()
	if h.haPushed == nil {
		h.haPushed = map[uint]haPushed{}
	}
	h.haPushed[id] = p
	h.mu.Unlock()
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

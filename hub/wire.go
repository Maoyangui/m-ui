package hub

// 推送快照的线上格式与传输。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// ApplyBodyLimit 副机收一份快照的上限:请求体与解压后的 JSON 都不超过它。以前是 64 MiB 的请求体、不压缩,
// 20 条线路时约 4 万用户就超限,副机回 400,主机进入退避,所有副机永远停在旧配置(停用、超额、到期都不再下发,审计 I4)。
var ApplyBodyLimit int64 = 512 << 20

// DecodeSnapshot 副机读主机推来的快照。新主机对报过 applyGzip 的副机发 gzip 压缩的请求体(Content-Encoding: gzip),
// 老主机照旧发不压缩的 JSON;两种都收。
func DecodeSnapshot(w http.ResponseWriter, r *http.Request) (Snapshot, error) {
	var snap Snapshot
	var rd io.Reader = http.MaxBytesReader(w, r.Body, ApplyBodyLimit)
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return snap, fmt.Errorf("快照解压失败: %w", err)
		}
		defer zr.Close()
		rd = http.MaxBytesReader(w, zr, ApplyBodyLimit) // 解压后的大小同样封顶:几 MB 的压缩包能解出几个 G
	}
	if err := json.NewDecoder(rd).Decode(&snap); err != nil {
		return snap, err
	}
	// 读到底:gzip 的校验和在末尾,读完才知道包是不是完整的;请求体读完,主机断开时 r.Context() 才会跟着取消
	_, err := io.Copy(io.Discard, rd)
	return snap, err
}

// wireSnapshot 一份快照编码好的推送字节。JSON 只编码一次:在 selfNodeId 的值那里切开,各台副机拼上自己的 id,
// 所有副机共用同一份字节,内存不随副机台数增长(以前每台各自 json.Marshal 一整份,并发推 10 台时主机堆峰值 866 MB)。
// 报过 applyGzip 的副机发压缩版(实测约 1/7):前后两段各压一次,中间的 id 单独压成一小段 ——
// gzip 允许多段首尾相接,解出来就是那台副机自己的那份 JSON,和不压缩时逐字节相同。
type wireSnapshot struct {
	rev, version, minNode string
	seq                   uint64
	head, tail            []byte // head 到 "selfNodeId": 为止,tail 是 id 之后的部分
	fixed                 bool   // 整份已经是某一台的(下线推送),不再拼 id;此时只用 head

	gzOnce         sync.Once
	gzHead, gzTail []byte
	gzErr          error
}

// wireEncodes 编码过多少份快照(测试用来确认每个修订只编码一次)。
var wireEncodes atomic.Int64

func encodeWire(s Snapshot) (*wireSnapshot, error) {
	wireEncodes.Add(1)
	s.SelfNodeId = 0
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	// selfNodeId 前面只有修订号、序号、版本这几个简单字段,字符串里的引号都转义过,第一次出现的就是这个字段
	const key = `"selfNodeId":0`
	i := bytes.Index(b, []byte(key))
	if i < 0 {
		return nil, errors.New("快照编码里找不到 selfNodeId")
	}
	cut := i + len(key) - 1
	return &wireSnapshot{rev: s.Revision, version: s.Version, minNode: s.MinNode, seq: s.Sequence, head: b[:cut], tail: b[cut+1:]}, nil
}

// fixedWire 已经编码好、selfNodeId 已经是那一台的整份快照(下线推送落库的那份)。
func fixedWire(rev string, body []byte) *wireSnapshot {
	return &wireSnapshot{rev: rev, head: body, fixed: true}
}

// size 不压缩时的字节数:副机解码、整表替换、渲染的工作量看它。
func (w *wireSnapshot) size() int64 { return int64(len(w.head)+len(w.tail)) + 20 }

func (w *wireSnapshot) compress() error {
	w.gzOnce.Do(func() {
		if w.gzHead, w.gzErr = gzipBytes(w.head); w.gzErr == nil && len(w.tail) > 0 {
			w.gzTail, w.gzErr = gzipBytes(w.tail)
		}
	})
	return w.gzErr
}

// parts 发给 nodeId 那台的请求体(分段,不拼成一整块);gz 为真时是 gzip 多段流。
func (w *wireSnapshot) parts(nodeId uint, gz bool) ([][]byte, error) {
	if gz {
		if err := w.compress(); err != nil {
			return nil, err
		}
	}
	if w.fixed {
		if gz {
			return [][]byte{w.gzHead}, nil
		}
		return [][]byte{w.head}, nil
	}
	id := []byte(strconv.FormatUint(uint64(nodeId), 10))
	if !gz {
		return [][]byte{w.head, id, w.tail}, nil
	}
	gzID, err := gzipBytes(id)
	if err != nil {
		return nil, err
	}
	return [][]byte{w.gzHead, gzID, w.gzTail}, nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed) // 实测两万用户 34 MB → 4.7 MB,百来毫秒
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func joinedReader(parts [][]byte) (io.Reader, int64) {
	rs := make([]io.Reader, len(parts))
	var n int64
	for i, p := range parts {
		rs[i] = bytes.NewReader(p)
		n += int64(len(p))
	}
	return io.MultiReader(rs...), n
}

// 等副机确认应用的时限按快照大小放宽:快照越大,副机解码、整表替换、渲染、热更新越久。以前一律 25 秒,
// 两万用户 × 20 条线路的副机光处理就要 20 多秒,一超时主机就整份重推,副机永远在重做同一份(审计 I4)。
var (
	applyTimeoutBase  = 25 * time.Second
	applyTimeoutPerMB = 2 * time.Second
	applyTimeoutMax   = 5 * time.Minute
)

func applyTimeout(size int64) time.Duration {
	d := applyTimeoutBase + time.Duration(size>>20)*applyTimeoutPerMB
	if d > applyTimeoutMax {
		d = applyTimeoutMax
	}
	return d
}

// pushConcurrency 同时向几台副机推整份快照:大快照几十 MB,十几台一起推既占内存又挤占主机出口带宽。
// 下线推送(没有用户的小快照)不排队。
const pushConcurrency = 3

// applyTimeoutError 副机已经连上、请求发出去了,但在时限内没回确认:大快照多半是还在应用,不一定失败了。
// 主机看同一轮拉到的报告判断它到底好没好(见 confirmByReport);没好就退避,不立刻整份重推。
type applyTimeoutError struct {
	after time.Duration
	size  int64
	err   error
}

func (e *applyTimeoutError) Error() string {
	return fmt.Sprintf("等了 %s 副机还没确认应用(快照 %.1f MB,可能还在处理): %v", e.after, float64(e.size)/(1<<20), e.err)
}

func (e *applyTimeoutError) Unwrap() error { return e.err }

func isApplyTimeout(err error) bool {
	var te *applyTimeoutError
	return errors.As(err, &te)
}

// pushWire 把编码好的快照推给一台副机,等它确认应用(调用方持有这台的闸)。确认了就记为已推送。
func (h *Hub) pushWire(n model.Node, w *wireSnapshot) error {
	if !w.fixed && h.pushSem != nil {
		select {
		case h.pushSem <- struct{}{}:
			defer func() { <-h.pushSem }()
		case <-h.baseCtx().Done():
			return h.baseCtx().Err()
		}
	}
	gz := h.acceptsGzip(n.Id)
	err := h.postApply(n, w, gz)
	var hse *httpStatusError
	if gz && errors.As(err, &hse) && (hse.Status == http.StatusBadRequest || hse.Status == http.StatusUnsupportedMediaType) {
		// 压缩包被拒(副机降过级、中间的反代动了请求体):当场改发不压缩的再试一次;成了就说明是压缩的问题,这台以后都发不压缩的
		if err = h.postApply(n, w, false); err == nil {
			logger.Warning("副机 ", n.Name, " 收不了压缩的快照,以后给它发不压缩的")
			h.mu.Lock()
			if h.noGzip == nil {
				h.noGzip = map[uint]bool{}
			}
			h.noGzip[n.Id] = true
			h.mu.Unlock()
		}
	}
	if err != nil {
		return err
	}
	h.markPushed(n, w.rev)
	return nil
}

func (h *Hub) postApply(n model.Node, w *wireSnapshot, gz bool) error {
	parts, err := w.parts(n.Id, gz)
	if err != nil {
		return err
	}
	timeout := applyTimeout(w.size())
	ctx, cancel := context.WithTimeout(h.baseCtx(), timeout)
	defer cancel()
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }})
	body, size := joinedReader(parts)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL(n, "apply"), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.GetBody = func() (io.ReadCloser, error) { // 连接池里的旧连接断了时传输层要能重发
		r, _ := joinedReader(parts)
		return io.NopCloser(r), nil
	}
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	var out struct {
		OK       json.RawMessage `json:"ok"`
		Revision string          `json:"revision"`
	}
	if err := h.do(n, req, &out); err != nil {
		// 连上了才算"它在处理";根本没连上(失联)照旧不算被拒、不退避,副机一回来下一轮就拿到配置
		if connected.Load() && errors.Is(ctx.Err(), context.DeadlineExceeded) && h.baseCtx().Err() == nil {
			return &applyTimeoutError{after: timeout, size: w.size(), err: err}
		}
		return err
	}
	ok := strings.Trim(strings.TrimSpace(string(out.OK)), `"`)
	if ok != "1" && !strings.EqualFold(ok, "true") {
		return fmt.Errorf("%w:修订 %s (ok=%q)", errApplyUnconfirmed, w.rev, ok)
	}
	if out.Revision != w.rev {
		return fmt.Errorf("%w:确认的修订号不匹配,期望 %s,收到 %s", errApplyUnconfirmed, w.rev, out.Revision)
	}
	return nil
}

// markPushed 记下这台副机已经在这一修订上。
func (h *Hub) markPushed(n model.Node, rev string) {
	h.mu.Lock()
	h.pushed[n.Id] = rev
	if st := h.status[n.Id]; st != nil {
		st.LastPush = time.Now().Unix()
	}
	h.mu.Unlock()
	logger.Info("已向副机 ", n.Name, " 推送配置 ", rev)
}

// wireFor 这份快照编码好的推送字节:同一修订、同一序号只编码一次,所有副机共用。
func (h *Hub) wireFor(snap Snapshot) (*wireSnapshot, error) {
	h.wireMu.Lock()
	defer h.wireMu.Unlock()
	if w := h.wire; w != nil && w.rev == snap.Revision && w.seq == snap.Sequence && w.version == snap.Version && w.minNode == snap.MinNode {
		return w, nil
	}
	w, err := encodeWire(snap)
	if err != nil {
		return nil, err
	}
	h.wire = w
	return w, nil
}

// acceptsGzip 这台副机最近一份报告说它收得了 gzip 压缩的快照,而且没出过压缩包被拒、不压缩的却成了的事。
// 还没拉到过报告(刚启动的第一次推送)按老副机算。
func (h *Hub) acceptsGzip(id uint) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.status[id]
	return st != nil && st.applyGzip && !h.noGzip[id]
}

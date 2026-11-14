package ha

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 主机让副机删副本(ha/drop)、离开集群(ha/leave)的请求要主机签名。
//
// 只凭副机令牌不够:令牌在备用手里的副本里、明文链路上也能嗅到,而纪元请求头没有签名,随便发一个请求副机就会在 409 里
// 报出自己的纪元与主机。离开集群会清掉纪元与钉住的主机公钥,之后谁先推状态就信谁 —— 所以这两个动作的正文是
// 主机用签名密钥签过的 {动作, 副机 id, 纪元, 主机, 时间},副机用本纪元钉住的主机公钥验;没钉住公钥的一律拒绝。

// ctlSigContext 控制请求签名的上下文前缀(与容灾状态的签名区分开)。
const ctlSigContext = "m-ui/ha/ctl/v1\n"

// ctlV 控制请求的版本。
const ctlV = 1

// ctlSkew 控制请求的时间与副机时钟最多差多少(交接标记的时效同样依赖两边时钟大致一致)。
var ctlSkew = peerSkew

// ctlBody 签名的正文。
type ctlBody struct {
	Action string `json:"action"` // ha/drop | ha/leave
	Node   uint   `json:"node"`   // 目标副机在主机服务器表里的 id
	Epoch  uint64 `json:"epoch"`  // 主机当时的纪元
	Master uint   `json:"master"` // 主机的服务器 id
	Ts     int64  `json:"ts"`
}

// signedCtl 线上的控制请求。
type signedCtl struct {
	V    int             `json:"v"`
	Body json.RawMessage `json:"body"`
	Sig  string          `json:"sig"`
}

var (
	errCtlUnsigned = errors.New("这个请求要主机签名(主机和本机的版本不一致时先把两边升级到同一版本)")
	errCtlNoPin    = errors.New("本机还没记下当前主机的签名公钥,无法确认这个请求来自主机:等主机推来容灾状态后它会自动重试")
	errCtlSig      = errors.New("签名不对:不是本机认的主机发的")
	errCtlStale    = errors.New("请求的纪元、主机或目标与本机不一致,或已过期")
)

// ctlRequest 主机侧:给第 n 台副机签一份 action 请求的正文。
func (s *Service) ctlRequest(action string, n model.Node) ([]byte, error) {
	k, err := s.signPriv()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(ctlBody{Action: action, Node: n.Id, Epoch: s.Epoch(), Master: s.SelfId(), Ts: s.now().Unix()})
	sig := ed25519.Sign(k, append([]byte(ctlSigContext), body...))
	return json.Marshal(signedCtl{V: ctlV, Body: body, Sig: base64.StdEncoding.EncodeToString(sig)})
}

// sendCtl 主机侧:签好名发给副机。
func (s *Service) sendCtl(n model.Node, action string, timeout time.Duration) error {
	if s.d.Hub == nil {
		return errors.New("本机容灾服务未初始化")
	}
	b, err := s.ctlRequest(action, n)
	if err != nil {
		return err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return s.d.Hub.Send(n, http.MethodPost, action, bytes.NewReader(b), int64(len(b)), h, timeout, nil)
}

// verifyCtl 副机侧:用本纪元钉住的主机公钥验一份 action 请求;纪元、主机、目标(本机)都要对得上,时间在允许误差内。
func (s *Service) verifyCtl(action string, raw []byte) error {
	var sc signedCtl
	if err := json.Unmarshal(raw, &sc); err != nil || sc.V != ctlV || len(sc.Body) == 0 || sc.Sig == "" {
		return errCtlUnsigned
	}
	pinned := strings.TrimSpace(s.setting(KeyMasterPub))
	if pinned == "" {
		return errCtlNoPin
	}
	pub, err := ParseSignPub(pinned)
	if err != nil {
		return errCtlNoPin
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sc.Sig))
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, append([]byte(ctlSigContext), sc.Body...), sig) {
		return errCtlSig
	}
	var c ctlBody
	if err := json.Unmarshal(sc.Body, &c); err != nil {
		return errCtlUnsigned
	}
	mid, _ := s.masterId()
	self := s.SelfId()
	now := s.now()
	ts := time.Unix(c.Ts, 0)
	if c.Action != action || c.Epoch != s.Epoch() || c.Master != mid || (self != 0 && c.Node != self) ||
		ts.Before(now.Add(-ctlSkew)) || ts.After(now.Add(ctlSkew)) {
		return errCtlStale
	}
	return nil
}

// DropSigned 副机:主机说本机不再是备用(POST agent/ha/drop,正文要主机签名),删掉手里的副本。
func (s *Service) DropSigned(raw []byte) error {
	if err := s.verifyCtl("ha/drop", raw); err != nil {
		return fmt.Errorf("不删副本: %w", err)
	}
	return s.DropReplica()
}

// LeaveSigned 副机:主机删除了本机(POST agent/ha/leave,正文要主机签名),退出容灾集群。
func (s *Service) LeaveSigned(raw []byte) error {
	if err := s.verifyCtl("ha/leave", raw); err != nil {
		return fmt.Errorf("不退出集群: %w", err)
	}
	return s.LeaveCluster()
}

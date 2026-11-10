package ha

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 副机之间互相确认主机是否失联、投票(设计 4.5)。
//
// 集群密钥由主机生成,经容灾状态发给所有副机;停用 / 删除副机时轮换。副机之间直接连对方的面板接口
// (POST <API 地址>/api/ha/peer/status|vote),请求与响应都用集群密钥做 HMAC:
//
//	X-Peer-Node 发起方的服务器 id   X-Peer-Ts 时间戳(±10 分钟)   X-Peer-Sig HMAC-SHA256(key, 方法|动作|时间戳|sha256(正文))
//
// status 只回最少的信息:纪元、主机、距上次联系主机多久、是不是备用、副本到什么时候、投过的票。
// vote(纪元 L+1, 发起方):本机也 60 秒以上没联系上主机、L+1 这个纪元还没投给别人时,先把票写进库再回 granted。

const (
	peerHeaderNode = "X-Peer-Node"
	peerHeaderTs   = "X-Peer-Ts"
	peerHeaderSig  = "X-Peer-Sig"
	peerSkew       = 10 * time.Minute
)

// PeerStatus 一台副机对同伴说的自己的情况。
type PeerStatus struct {
	Id         uint   `json:"id"`
	Version    string `json:"version"`
	Epoch      uint64 `json:"epoch"`
	MasterId   uint   `json:"masterId"`
	LeaseAgo   int64  `json:"leaseAgo"` // 秒
	Role       string `json:"role"`
	Rank       int    `json:"rank"`
	ReplicaAt  int64  `json:"replicaAt,omitempty"` // 手里副本的数据截至
	ReplicaOK  bool   `json:"replicaOk,omitempty"` // 自检通过
	PauseUntil int64  `json:"pauseUntil,omitempty"`
	Vote       *Vote  `json:"vote,omitempty"`
}

// VoteRequest 请同伴投票:纪元 Epoch 由发起方接任。
type VoteRequest struct {
	Epoch     uint64 `json:"epoch"`
	Candidate uint   `json:"candidate"`
}

// VoteReply 投票结果。
type VoteReply struct {
	Granted bool   `json:"granted"`
	Epoch   uint64 `json:"epoch"`
	Reason  string `json:"reason,omitempty"`
}

func peerKey(raw string) []byte {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(b) < 16 {
		return nil
	}
	return b
}

func peerMAC(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{'\n'})
	}
	return hex.EncodeToString(m.Sum(nil))
}

func bodySum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// PeerReply 同伴接口的一个响应(web 层照着写回去)。
type PeerReply struct {
	Status int
	Body   []byte
	Header http.Header
}

// errPeerAuth 鉴权失败:不说是哪一项不对。
var errPeerAuth = errors.New("鉴权失败")

// ServePeer 处理同伴发来的 status / vote(web 层已经限过流)。鉴权失败时调用方要延时再回。
func (s *Service) ServePeer(action string, h http.Header, body []byte) (PeerReply, error) {
	key := peerKey(s.setting(KeyPeerKey))
	if !s.IsNode() || key == nil {
		return PeerReply{}, errPeerAuth
	}
	ts, err := strconv.ParseInt(h.Get(peerHeaderTs), 10, 64)
	if err != nil || absDur(s.now().Sub(time.Unix(ts, 0))) > peerSkew {
		return PeerReply{}, errPeerAuth
	}
	want := peerMAC(key, http.MethodPost, action, h.Get(peerHeaderTs), bodySum(body))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(h.Get(peerHeaderSig)))) {
		return PeerReply{}, errPeerAuth
	}
	from, _ := strconv.ParseUint(h.Get(peerHeaderNode), 10, 32)
	var out interface{}
	switch action {
	case "status":
		out = s.peerStatus()
	case "vote":
		var req VoteRequest
		if json.Unmarshal(body, &req) != nil || uint(from) != req.Candidate || req.Candidate == 0 {
			return PeerReply{}, errPeerAuth
		}
		out = s.grantVote(req)
	default:
		return PeerReply{}, errPeerAuth
	}
	b, _ := json.Marshal(out)
	rts := strconv.FormatInt(s.now().Unix(), 10)
	hdr := http.Header{}
	hdr.Set(peerHeaderTs, rts)
	hdr.Set(peerHeaderSig, peerMAC(key, "resp", action, rts, bodySum(b)))
	hdr.Set("Content-Type", "application/json")
	return PeerReply{Status: http.StatusOK, Body: b, Header: hdr}, nil
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (s *Service) peerStatus() PeerStatus {
	ps := PeerStatus{Id: s.SelfId(), Version: s.d.Version, Epoch: s.Epoch(), LeaseAgo: int64(s.LeaseAgo() / time.Second), Role: s.Role()}
	ps.MasterId, _ = s.masterId()
	if st, ok := s.NodeState(); ok {
		ps.Rank, ps.PauseUntil = st.rank(), st.PauseUntil
	}
	if ri, ok := s.replicaInfo(); ok {
		ps.ReplicaAt, ps.ReplicaOK = ri.AsOf, ri.VerifyErr == ""
	}
	var v Vote
	if s.getJSON(KeyVote, &v) {
		ps.Vote = &v
	}
	return ps
}

// grantVote 给发起方投票:本机也联系不上主机、这个纪元还没投给别人;先落库再回复。
func (s *Service) grantVote(req VoteRequest) VoteReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	L := s.Epoch()
	r := VoteReply{Epoch: req.Epoch}
	if req.Epoch != L+1 {
		r.Reason = "epoch"
		return r
	}
	if s.now().Sub(s.lastContact) < SuspectAfter {
		r.Reason = "masterOnline" // 本机还联系得上主机:不帮着换
		return r
	}
	if st, ok := s.NodeState(); ok && st.PauseUntil > s.now().Unix() {
		r.Reason = "paused"
		return r
	}
	if st, ok := s.NodeState(); !ok || !containsId(st.Standbys, req.Candidate) {
		r.Reason = "notStandby"
		return r
	}
	var v Vote
	if s.getJSON(KeyVote, &v) && v.Epoch == req.Epoch && v.Candidate != req.Candidate {
		r.Reason = "voted"
		return r
	}
	v = Vote{Epoch: req.Epoch, Candidate: req.Candidate, At: s.now().Unix()}
	if err := s.set(map[string]string{KeyVote: mustJSON(v)}); err != nil {
		r.Reason = "store"
		return r
	}
	r.Granted = true
	return r
}

// ---- 发起方 ----

// peer 一台同伴:从本机库里的服务器表来(快照里有地址与主机钉住的证书指纹,没有令牌)。
type peer struct {
	node model.Node
}

// peers 同伴:启用的、不是本机、不是主机、有 API 地址的。
func (s *Service) peers() []peer {
	st, _ := s.NodeState()
	self := s.SelfId()
	var nodes []model.Node
	s.d.DB.Where("enabled = ? AND is_local = ? AND COALESCE(api_url, '') <> ''", true, false).Order("sort asc, id asc").Find(&nodes)
	var out []peer
	for _, n := range nodes {
		if n.Id == self || n.Id == st.MasterId {
			continue
		}
		out = append(out, peer{node: n})
	}
	return out
}

var peerClients sync.Map // 证书指纹 → *http.Client

func peerClient(n model.Node) *http.Client {
	key := fmt.Sprintf("%v|%s", n.Insecure, n.CertFP)
	if c, ok := peerClients.Load(key); ok {
		return c.(*http.Client)
	}
	tr := &http.Transport{TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: time.Minute}
	if n.Insecure {
		tr.TLSClientConfig = pinnedTLS(n.CertFP) // 主机钉住的指纹;没钉过就只做 HMAC(请求与响应都签了)
	} else {
		tr.TLSClientConfig = &tls.Config{}
	}
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	peerClients.Store(key, c)
	return c
}

// callPeer 向一台同伴发 status / vote,验响应签名。
func (s *Service) callPeer(ctx context.Context, p peer, action string, req interface{}, out interface{}) error {
	key := peerKey(s.setting(KeyPeerKey))
	if key == nil {
		return errors.New("还没收到集群密钥")
	}
	var body []byte
	if req != nil {
		body, _ = json.Marshal(req)
	}
	u := strings.TrimRight(strings.TrimSpace(p.node.ApiUrl), "/")
	if !strings.HasSuffix(u, "/api") {
		u += "/api"
	}
	u += "/ha/peer/" + action
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set(peerHeaderNode, strconv.FormatUint(uint64(s.SelfId()), 10))
	hreq.Header.Set(peerHeaderTs, ts)
	hreq.Header.Set(peerHeaderSig, peerMAC(key, http.MethodPost, action, ts, bodySum(body)))
	resp, err := peerClient(p.node).Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("同伴 %s 回了 %d", p.node.Name, resp.StatusCode)
	}
	rts := resp.Header.Get(peerHeaderTs)
	if !hmac.Equal([]byte(peerMAC(key, "resp", action, rts, bodySum(b))), []byte(strings.ToLower(resp.Header.Get(peerHeaderSig)))) {
		return fmt.Errorf("同伴 %s 的回复签名不对", p.node.Name)
	}
	return json.Unmarshal(b, out)
}

// peerObs 最近一次问同伴的结果。
type peerObs struct {
	at  time.Time
	ok  bool
	st  PeerStatus
	err string
}

// collectPeers 并发问一圈同伴(每台 5 秒)。
func (s *Service) collectPeers(ctx context.Context) map[uint]peerObs {
	ps := s.peers()
	out := map[uint]peerObs{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func(p peer) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var st PeerStatus
			err := s.callPeer(c, p, "status", nil, &st)
			o := peerObs{at: s.now(), ok: err == nil, st: st}
			if err != nil {
				o.err = err.Error()
			}
			mu.Lock()
			out[p.node.Id] = o
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

// requestVotes 请同伴为本机在纪元 epoch 接任投票,返回同意的台数(不含本机)。本机自己的票先落库。
func (s *Service) requestVotes(ctx context.Context, epoch uint64) (int, error) {
	self := s.SelfId()
	if err := s.set(map[string]string{KeyVote: mustJSON(Vote{Epoch: epoch, Candidate: self, At: s.now().Unix()})}); err != nil {
		return 0, err
	}
	var granted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range s.peers() {
		wg.Add(1)
		go func(p peer) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var r VoteReply
			if err := s.callPeer(c, p, "vote", VoteRequest{Epoch: epoch, Candidate: self}, &r); err != nil {
				logger.Info("容灾:向 ", p.node.Name, " 要票没成功: ", err)
				return
			}
			if r.Granted {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return granted, nil
}

// quorum 法定多数:启用的服务器总数(含主机)的一半加一。主机不投票,所以一主两副要两台副机都同意。
func quorum(clusterSize int) int { return clusterSize/2 + 1 }

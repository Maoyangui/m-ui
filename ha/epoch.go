package ha

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 纪元(epoch)防脑裂。
//
// 每台机器在本机设置里存 haEpoch:0 表示没开过容灾、没换过主机,行为和以前完全一样;主机开启容灾时设为 1,
// 每次接任、设为主机、收回加 1(一律经 NextEpoch:严格高于本机与全部采信的纪元,有上限,不会回绕)。
// 主机发往副机的每个请求都带 X-HA-Epoch(主机的纪元)与 X-HA-Master(主机在服务器表里的 id)。副机按下面的规则
// 决定接不接受(统一在 agent 鉴权里判,顺序是 令牌 → 纪元 → 副机模式):
//
//	e < L,或 e == L 但主机不是本机认的那台            → 409 stale_epoch(你已经不是主机了)
//	e == L                                             → 放行;来自当前主机就刷新租约
//	e > L:只有 apply、ha/state 能把纪元升上去,其余     → 409 epoch_not_adopted
//	       e 超出上限(MaxEpoch,或比本机高出 MaxEpochJump 以上)一律不升 → 409 epoch_not_adopted
//	       满足任一条件才升:本机 30 分钟内为 (e, m) 投过票;收到过当前主机签发、还没过期的交接标记(给 m 的);
//	       对当前主机的租约已过期(120 秒没联系);本机是回来后变成副机的原主机;本机还没进过纪元(L == 0)
//	       → 否则 409 still_following(本机还跟着原主机)
//
// "还能联系上原主机的副机不换主"可以防止一台备用误判后把整个集群带走。只拿到副机令牌不够让副机换主:
// 交接标记只认当前主机签过名的容灾状态里带的(见 state.go),投票要本机亲自投过。

// 请求头与响应头。
const (
	HeaderEpoch  = "X-HA-Epoch"  // 主机 → 副机:主机的纪元
	HeaderMaster = "X-HA-Master" // 主机 → 副机:主机的服务器 id
	HeaderNode   = "X-HA-Node"   // 副机 → 主机:本机的纪元(认得纪元的副机才带,主机据此区分老副机)
)

// 409 正文里的代码。
const (
	CodeStale       = "stale_epoch"       // 请求方的纪元比本机认的旧(或同纪元但主机不是同一台):它已经不是主机了
	CodeFollowing   = "still_following"   // 请求方纪元更高,但本机还联系得上当前主机,不换主
	CodeNotAdopted  = "epoch_not_adopted" // 请求方纪元更高,这个动作不能升纪元:先推 ha/state 或配置
	LeaseExpire     = 120 * time.Second   // 副机多久没收到当前主机的请求,才接受更高纪元的新主机
	SuspectAfter    = 60 * time.Second    // 多久没联系算"疑似失联"
	freshReplyLimit = 45 * time.Second    // 主机侧:多久以内的副机回复算数(变为副机的判定)
	voteTTL         = 30 * time.Minute    // 票的有效期:过期的票不算投过,也不能拿来升纪元
	grantTTL        = 10 * time.Minute    // 收回、围栏时给自己签的交接标记的有效期
)

// 纪元的取值范围:别人报来的纪元只是"声称",超出窗口不采信(防一台被攻破的副机把集群纪元推到回绕)。
const (
	MaxEpoch     uint64 = 1 << 40 // 纪元上限:远离 uint64 回绕,也在 JS 的安全整数以内
	MaxEpochJump uint64 = 1000    // 别人报的纪元最多比本机高这么多才采信(每次接任、设为主机、收回只加 1)
)

// EpochClaimOK 别人报来的纪元 claim 能不能采信(本机纪元 local)。
func EpochClaimOK(local, claim uint64) bool {
	return claim < MaxEpoch && claim <= local+MaxEpochJump
}

// errEpochLimit 纪元到了上限:这次升纪元的操作放弃(正常使用不可能到,到了就是被人动过)。
var errEpochLimit = errors.New("纪元已到上限,不能再升:集群的纪元可能被人动过,先核实各台的情况")

// NextEpoch 下一个纪元:严格大于本机纪元与全部采信的 seen;不采信的 seen 忽略。结果到了上限返回错误,调用方放弃这次操作。
// 代码里所有"纪元加 1"一律经它。
func NextEpoch(local uint64, seen ...uint64) (uint64, error) {
	top := local
	for _, e := range seen {
		if EpochClaimOK(local, e) && e > top {
			top = e
		}
	}
	if top >= MaxEpoch-1 {
		return 0, errEpochLimit
	}
	return top + 1, nil
}

// Vote 副机投过的票:纪元 Epoch 投给了服务器 Candidate(30 分钟内有效;重新联系上当前主机就作废)。
type Vote struct {
	Epoch     uint64 `json:"epoch"`
	Candidate uint   `json:"candidate"`
	CandPub   string `json:"candPub,omitempty"` // 候选人以后的签名公钥:凭这张票换主后,新主机的状态必须用它验过
	At        int64  `json:"at"`
}

// valid 票还在有效期内。
func (v *Vote) valid(now time.Time) bool {
	return v != nil && v.Epoch > 0 && now.Unix()-v.At < int64(voteTTL/time.Second)
}

// Grant 交接标记:纪元 Epoch 的主机是服务器 To。由当前主机签发、只随它签过名的容灾状态下发:
// 设为主机时给目标(Expires = 冻结截止),收回 / 围栏时给自己(Expires = 签发后 10 分钟)。过期作废,可撤销(State.Revoke)。
type Grant struct {
	Epoch     uint64 `json:"epoch"`
	To        uint   `json:"to"`
	ToPub     string `json:"toPub,omitempty"` // 新主机的签名公钥:凭这张标记换主后,新主机的状态必须用它验过
	From      uint   `json:"from"`            // 签发的主机
	FromEpoch uint64 `json:"fromEpoch"`       // 签发时的纪元
	Expires   int64  `json:"expires"`         // 过期作废(unix 秒)
	At        int64  `json:"at"`              // 副机收下的时间
}

// valid 交接标记还没过期。没写时效的(只可能是本机自己写下的:设为主机完成、原主机变为副机时)按写下后 10 分钟算。
func (g *Grant) valid(now time.Time) bool {
	switch {
	case g == nil || g.Epoch == 0:
		return false
	case g.Expires > 0:
		return now.Unix() < g.Expires
	}
	return g.At > 0 && now.Unix()-g.At < int64(grantTTL/time.Second)
}

// NodeView 副机判定时用到的本机状态。
type NodeView struct {
	Epoch       uint64
	MasterId    uint
	LastContact time.Time // 最近一次收到当前纪元主机的请求
	Demoted     bool      // 回来后变成副机的原主机:谁的纪元更高就跟谁
	Vote        *Vote
	Grant       *Grant
}

// Verdict 副机对一个请求的判定。
type Verdict struct {
	Code    string // 空 = 放行
	Adopt   bool   // 放行,并把本机纪元升到请求方的
	Contact bool   // 放行,并刷新租约(请求来自当前纪元的主机)
	Via     string // 凭什么升的纪元:vote / grant / ""(租约过期、本机还没进过纪元、回来的原主机):决定钉哪把签名公钥
}

// OK 放行。
func (v Verdict) OK() bool { return v.Code == "" }

// Decide 副机按纪元规则判定一个已经通过令牌鉴权的请求。e、m 是请求头里的纪元与主机 id;canAdopt 表示这个动作能升纪元。
func Decide(v NodeView, e uint64, m uint, canAdopt bool, now time.Time) Verdict {
	switch {
	case e == 0 && v.Epoch == 0:
		return Verdict{} // 两边都没开过容灾:和以前一样
	case e < v.Epoch:
		return Verdict{Code: CodeStale}
	case e == v.Epoch:
		if v.MasterId != 0 && m != v.MasterId {
			return Verdict{Code: CodeStale} // 同一纪元出了两台主机:先被本机接受的那台算数
		}
		return Verdict{Contact: true, Adopt: v.MasterId == 0 && m != 0}
	}
	// e > L
	if !canAdopt || e >= MaxEpoch || (v.Epoch > 0 && e > v.Epoch+MaxEpochJump) {
		return Verdict{Code: CodeNotAdopted} // 超出窗口的纪元不升:防被推飞
	}
	via := ""
	switch {
	case v.Vote.valid(now) && v.Vote.Epoch == e && v.Vote.Candidate == m:
		via = "vote"
	case v.Grant.valid(now) && v.Grant.Epoch == e && v.Grant.To == m:
		via = "grant"
	case v.Demoted, v.Epoch == 0:
	case v.LastContact.IsZero() || now.Sub(v.LastContact) >= LeaseExpire:
	default:
		return Verdict{Code: CodeFollowing}
	}
	return Verdict{Adopt: true, Contact: true, Via: via}
}

// Header 读请求头里的纪元与主机 id(没有就是 0)。
func Header(h http.Header) (uint64, uint) {
	e, _ := strconv.ParseUint(strings.TrimSpace(h.Get(HeaderEpoch)), 10, 64)
	m, _ := strconv.ParseUint(strings.TrimSpace(h.Get(HeaderMaster)), 10, 32)
	return e, uint(m)
}

// SetHeader 给发往副机的请求带上纪元(纪元为 0 时什么都不加:没开过容灾的集群请求和以前一字不差)。
func SetHeader(h http.Header, epoch uint64, master uint) {
	if epoch == 0 {
		return
	}
	h.Set(HeaderEpoch, strconv.FormatUint(epoch, 10))
	h.Set(HeaderMaster, strconv.FormatUint(uint64(master), 10))
}

// Reject 409 正文。
type Reject struct {
	Error       string `json:"error"`
	Code        string `json:"code"`
	Epoch       uint64 `json:"epoch"`
	MasterId    uint   `json:"masterId"`
	SinceChange uint64 `json:"sinceChange,omitempty"` // 请求方是本机记下的"失联的原主机"时:接任所用数据里最大的审计 id
	LeaseAgo    int64  `json:"leaseAgo,omitempty"`    // 距本机上次收到自己那台主机的请求多少秒(回话的是主机时不填)
	Role        string `json:"role,omitempty"`        // 回话这台自己的角色:master / node / demoted / promoting
	MasterInst  string `json:"masterInst,omitempty"`  // 它认的主机的实例号(是主机时就是它自己的)
	BecameAt    int64  `json:"becameAt,omitempty"`    // role=master 时:它在这个纪元当上主机的时间(同纪元两台主机比先后)
}

var (
	errStaleText     = errors.New("本机已经跟随纪元更高的主机:发起请求的这台已经不是主机了")
	errFollowingText = errors.New("本机还联系得上当前主机,不接受新主机")
	errNotAdopted    = errors.New("本机还没切到这个纪元:等主机先推送容灾状态或配置")
)

// RejectText 409 的说明文字。
func RejectText(code string) string {
	switch code {
	case CodeStale:
		return errStaleText.Error()
	case CodeFollowing:
		return errFollowingText.Error()
	}
	return errNotAdopted.Error()
}

// ParseReject 从副机的 409 正文里读出纪元判定(不是纪元判定返回 nil)。
func ParseReject(status int, body []byte) *Reject {
	if status != http.StatusConflict {
		return nil
	}
	var r Reject
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	switch r.Code {
	case CodeStale, CodeFollowing, CodeNotAdopted:
		return &r
	}
	return nil
}

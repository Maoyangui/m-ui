package ha

// View 容灾页 / 横幅要的全部状态(GET /api/ha)。按本机角色只填相关的部分。
type View struct {
	Role     string `json:"role"` // master / temp / node / fenced / promoting
	Epoch    uint64 `json:"epoch"`
	SelfId   uint   `json:"selfId"`
	MasterId uint   `json:"masterId,omitempty"`
	Version  string `json:"version"`
	Frozen   int64  `json:"frozenUntil,omitempty"`

	// 主机 / 临时主机
	Config   *Config              `json:"config,omitempty"`
	Replies  map[string]replyView `json:"replies,omitempty"`
	Promoted *PromotedFrom        `json:"promotedFrom,omitempty"`

	// 副机
	Node *NodeView2 `json:"node,omitempty"`

	// 已降级
	FencedBy *FencedBy `json:"fencedBy,omitempty"`
}

// NodeView2 副机看到的容灾状态。
type NodeView2 struct {
	LeaseAgo   int64  `json:"leaseAgo"` // 秒
	MasterName string `json:"masterName,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Rank       int    `json:"rank"` // 本机候选名次,0 = 不是候选
	PauseUntil int64  `json:"pauseUntil,omitempty"`
	Granted    *Grant `json:"grant,omitempty"`
}

// View 当前状态。
func (s *Service) View() View {
	v := View{Role: s.Role(), Epoch: s.Epoch(), SelfId: s.SelfId(), Version: s.d.Version}
	v.MasterId, _ = s.masterId()
	if f := s.freezeUntil(); f > s.now().Unix() && !s.IsNode() {
		v.Frozen = f
	}
	var fb FencedBy
	if s.getJSON(KeyFencedBy, &fb) {
		v.FencedBy = &fb
	}
	if !s.IsNode() {
		c := s.Config()
		v.Config = &c
		v.Replies = s.replyViews()
		v.MasterId = v.SelfId
		var pf PromotedFrom
		if s.getJSON(KeyPromotedFrom, &pf) {
			v.Promoted = &pf
		}
		return v
	}
	nv := &NodeView2{LeaseAgo: int64(s.LeaseAgo().Seconds())}
	if st, ok := s.NodeState(); ok {
		nv.MasterName, nv.Mode, nv.Rank, nv.PauseUntil = st.MasterName, st.Mode, st.rank(), st.PauseUntil
	}
	var g Grant
	if s.getJSON(KeyGrant, &g) {
		nv.Granted = &g
	}
	v.Node = nv
	return v
}

package ha

// 第一步(纪元与防脑裂)先放占位:副本、候选名单、失联判定、迁回提交在后面几步补上。

type replicator struct{ s *Service }

func newReplicator(s *Service) *replicator      { return &replicator{s: s} }
func (r *replicator) tick()                     {}
func (r *replicator) kick()                     {}
func (r *replicator) candidateIds() []uint      { return nil }
func (r *replicator) rankOf(uint) int           { return 0 }
func (r *replicator) markFor(uint) *ReplicaMark { return nil }
func (s *Service) noteReplicaMark(ReplicaMark)  {}
func (s *Service) DropReplica() error           { return nil }
func (s *Service) commitTick()                  {}
func (s *Service) selfCheckTick()               {}

type engine struct{ s *Service }

func newEngine(s *Service) *engine     { return &engine{s: s} }
func (e *engine) tick()                {}
func (e *engine) contact()             {}
func (e *engine) adopted(uint64, uint) {}

// WitnessResult 外部见证的结论:up(主机对外正常,只是网络分区)/ down / unknown。
type WitnessResult struct {
	Verdict string `json:"verdict"`
	OK      int    `json:"ok"`
	Total   int    `json:"total"`
	At      int64  `json:"at"`
	Error   string `json:"error,omitempty"`
}

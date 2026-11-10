package ha

import "github.com/Maoyangui/m-ui/hub"

// 失联判定(副机互相确认与投票、境外见证、Telegram 确认)、命令行请求、自动改 DNS 在后面几步补上,这里先放占位。

type engine struct{ s *Service }

func newEngine(s *Service) *engine          { return &engine{s: s} }
func (e *engine) tick()                     {}
func (e *engine) contact()                  {}
func (e *engine) adopted(uint64, uint)      {}
func (e *engine) peerSummary() *hub.HAPeers { return nil }
func (e *engine) promptView() *PromptView   { return nil }
func (e *engine) evidenceView() *EvidenceView {
	return nil
}

// PromptView 确认后接任:正在等确认。
type PromptView struct{}

// EvidenceView 失联判定的证据。
type EvidenceView struct{}

// WitnessResult 境外见证的结论:up(主机对外正常,只是网络分区)/ down / unknown。
type WitnessResult struct {
	Verdict string `json:"verdict"`
	OK      int    `json:"ok"`
	Total   int    `json:"total"`
	At      int64  `json:"at"`
	Error   string `json:"error,omitempty"`
}

// DnsChange 自动改的一条 DNS 记录。
type DnsChange struct {
	Name string `json:"name"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Service) updateDNS(formerIP, formerIP6 string) ([]DnsChange, error) { return nil, nil }
func (s *Service) cmdTick()                                                  {}

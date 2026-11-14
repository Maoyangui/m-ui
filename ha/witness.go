package ha

// WitnessResult 境外见证的结论:up(主机对外正常,只是网络分区)/ down / unknown。
type WitnessResult struct {
	Verdict string `json:"verdict"`
	OK      int    `json:"ok"`
	Total   int    `json:"total"`
	At      int64  `json:"at"`
	Error   string `json:"error,omitempty"`
}

// WitnessVerdict 见证结论:至少 2/3 的测点连得上 = up;最多 1/3 = down;其余(含一个都没测成)= unknown。
func WitnessVerdict(ok, total int) string {
	switch {
	case total == 0:
		return "unknown"
	case 3*ok >= 2*total:
		return "up"
	case 3*ok <= total:
		return "down"
	}
	return "unknown"
}

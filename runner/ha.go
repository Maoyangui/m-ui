package runner

import (
	"context"

	"github.com/Maoyangui/m-ui/ha"
)

// haWitness 主机容灾的境外见证:从境外 Globalping 测点 TCP 连主机的公网 IP。
// 至少 2/3 的测点连得上 = up(主机还活着,是网络分区,不接任);最多 1/3 = down;其余或接口出错 = unknown。
func (r *Runner) haWitness(ctx context.Context, host string, ports []int) (ha.WitnessResult, error) {
	ok, total, err := r.reach.Witness(ctx, host, ports)
	if err != nil {
		return ha.WitnessResult{Verdict: "unknown", OK: ok, Total: total, Error: err.Error()}, nil
	}
	return ha.WitnessResult{Verdict: ha.WitnessVerdict(ok, total), OK: ok, Total: total}, nil
}

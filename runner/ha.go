package runner

import (
	"context"

	"github.com/Maoyangui/m-ui/ha"
)

// haWitness 容灾的外部见证:第五步接上 Globalping。
func (r *Runner) haWitness(ctx context.Context, host string, ports []int) (ha.WitnessResult, error) {
	return ha.WitnessResult{Verdict: "unknown"}, nil
}

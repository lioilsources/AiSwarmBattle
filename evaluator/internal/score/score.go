// Package score turns evaluation stages into a composite 0-100 score.
//
// Any change to weights or scoring rules must bump Version and be recorded in
// docs/scoring_rubric.md in the same change.
package score

import "github.com/lioilsources/shaderbattle/pkg/types"

// Version is the cost model / scoring version reported as cost_model_version.
const Version = "2026.10-compile"

// Compute scores an evaluation. Compile failure is a hard gate (composite 0).
func Compute(r *types.EvalResult) types.ScoreResult {
	s := types.ScoreResult{Breakdown: map[string]float64{"compile": 0}}
	if !r.Compile.OK {
		return s
	}
	s.Breakdown["compile"] = 100
	s.Composite = 100
	return s
}

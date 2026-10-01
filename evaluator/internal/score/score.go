// Package score turns evaluation stages into a composite 0-100 score.
//
// Any change to weights, thresholds or scoring rules must bump Version and be
// recorded in docs/scoring_rubric.md in the same change.
package score

import (
	"math"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

// Version is the scoring / cost model version reported in cost_model_version.
const Version = "2026.10.1"

// Weights are the maximum points per component. Components that were not
// evaluated (runtime skipped, fuzz not implemented) are left out of both the
// earned and the possible total, so composite stays on a 0-100 scale.
type Weights struct {
	Compile       float64
	NoRuntimeNaN  float64
	NoArtifacts   float64
	FuzzStability float64
	PerfTarget    float64
	CodeQuality   float64
}

// DefaultWeights follow EVALUATOR.md.
var DefaultWeights = Weights{
	Compile:       40,
	NoRuntimeNaN:  15,
	NoArtifacts:   10,
	FuzzStability: 15,
	PerfTarget:    10,
	CodeQuality:   10,
}

// Thresholds for piecewise-linear sub-scores: value <= Full scores 1,
// value >= Zero scores 0, linear in between.
type ramp struct{ Full, Zero float64 }

var (
	branchRamp   = ramp{Full: 16, Zero: 64}
	texReadRamp  = ramp{Full: 4, Zero: 32}
	loopIterRamp = ramp{Full: 64, Zero: 512}
	aluRamp      = ramp{Full: 4000, Zero: 16000}
	artifactRamp = ramp{Full: 0.05, Zero: 0.5}
	frameMSRamp  = ramp{Full: 8, Zero: 16}
)

// unknownLoopScore is the loop sub-score when a loop bound is not static.
const unknownLoopScore = 0.5

// Compute scores an evaluation. Compile failure is a hard gate (composite 0).
func Compute(r *types.EvalResult, w Weights) types.ScoreResult {
	s := types.ScoreResult{Breakdown: map[string]float64{"compile": 0}}
	if !r.Compile.OK {
		return s
	}

	var earned, possible float64
	add := func(name string, weight, fraction float64) {
		pts := round2(weight * fraction)
		s.Breakdown[name] = pts
		earned += pts
		possible += weight
	}

	add("compile", w.Compile, 1)
	add("code_quality", w.CodeQuality, codeQuality(r.Static))

	if rs := r.Runtime.PerResolution; len(rs) > 0 {
		nan := false
		worstArtifacts := 0.0
		worstMS := 0.0
		hardware := false
		for _, res := range rs {
			nan = nan || res.NaNInfDetected
			if res.FramesRendered > 0 {
				ratio := float64(res.BlackFrameCount+res.ConstantFrameCount) / float64(res.FramesRendered)
				worstArtifacts = math.Max(worstArtifacts, ratio)
			}
			if !res.SoftwareRenderer {
				hardware = true
				worstMS = math.Max(worstMS, res.MedianFrameMS)
			}
		}
		add("no_runtime_nan", w.NoRuntimeNaN, boolFrac(!nan))
		add("no_artifacts", w.NoArtifacts, artifactRamp.at(worstArtifacts))
		if hardware {
			add("perf_target", w.PerfTarget, frameMSRamp.at(worstMS))
		}
	}

	if possible > 0 {
		s.Composite = round2(100 * earned / possible)
	}
	return s
}

// codeQuality is the mean of the static sub-scores, in [0,1].
func codeQuality(m types.StaticResult) float64 {
	loop := unknownLoopScore
	if m.LoopMaxIter >= 0 {
		loop = loopIterRamp.at(float64(m.LoopMaxIter))
	}
	var prec float64
	switch m.PrecisionDefault {
	case "highp", "mediump":
		prec = 1
	case "lowp":
		prec = 0.5
	}
	subs := []float64{
		branchRamp.at(float64(m.BranchCount)),
		texReadRamp.at(float64(m.TexReads)),
		loop,
		prec,
		aluRamp.at(float64(m.EstimatedALU)),
	}
	var sum float64
	for _, v := range subs {
		sum += v
	}
	return sum / float64(len(subs))
}

func (r ramp) at(v float64) float64 {
	switch {
	case v <= r.Full:
		return 1
	case v >= r.Zero:
		return 0
	default:
		return (r.Zero - v) / (r.Zero - r.Full)
	}
}

func boolFrac(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

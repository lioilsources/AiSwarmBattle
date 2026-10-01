package score

import (
	"testing"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

func TestCompute(t *testing.T) {
	cheap := types.StaticResult{BranchCount: 1, TexReads: 1, PrecisionDefault: "highp", EstimatedALU: 150}
	tests := []struct {
		name          string
		in            types.EvalResult
		wantComposite float64
		wantBreakdown map[string]float64
	}{
		{
			name:          "compile failure is hard zero",
			in:            types.EvalResult{Compile: types.CompileResult{OK: false}, Static: cheap},
			wantComposite: 0,
			wantBreakdown: map[string]float64{"compile": 0},
		},
		{
			name:          "static only, cheap shader",
			in:            types.EvalResult{Compile: types.CompileResult{OK: true}, Static: cheap},
			wantComposite: 100,
			wantBreakdown: map[string]float64{"compile": 40, "code_quality": 10},
		},
		{
			name: "static only, unknown loop bound and no precision",
			in: types.EvalResult{Compile: types.CompileResult{OK: true}, Static: types.StaticResult{
				LoopMaxIter: -1, PrecisionDefault: "none",
			}},
			// code_quality = (1 + 1 + 0.5 + 0 + 1) / 5 = 0.7 -> 7 pts; 47/50
			wantComposite: 94,
			wantBreakdown: map[string]float64{"compile": 40, "code_quality": 7},
		},
		{
			name: "hardware runtime, slow frames",
			in: types.EvalResult{Compile: types.CompileResult{OK: true}, Static: cheap,
				Runtime: types.RuntimeResult{PerResolution: []types.ResolutionResult{
					{FramesRendered: 60, MedianFrameMS: 12},
				}}},
			// perf = (16-12)/8 = 0.5 -> 5 pts; 80/85
			wantComposite: 94.12,
			wantBreakdown: map[string]float64{"compile": 40, "code_quality": 10, "no_runtime_nan": 15, "no_artifacts": 10, "perf_target": 5},
		},
		{
			name: "software runtime skips perf, nan and black frames",
			in: types.EvalResult{Compile: types.CompileResult{OK: true}, Static: cheap,
				Runtime: types.RuntimeResult{PerResolution: []types.ResolutionResult{
					{FramesRendered: 60, MedianFrameMS: 90, NaNInfDetected: true, BlackFrameCount: 60, SoftwareRenderer: true},
				}}},
			// 50/75
			wantComposite: 66.67,
			wantBreakdown: map[string]float64{"compile": 40, "code_quality": 10, "no_runtime_nan": 0, "no_artifacts": 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compute(&tc.in, DefaultWeights)
			if got.Composite != tc.wantComposite {
				t.Errorf("composite: got %v want %v (%v)", got.Composite, tc.wantComposite, got.Breakdown)
			}
			if len(got.Breakdown) != len(tc.wantBreakdown) {
				t.Fatalf("breakdown: got %v want %v", got.Breakdown, tc.wantBreakdown)
			}
			for k, v := range tc.wantBreakdown {
				if got.Breakdown[k] != v {
					t.Errorf("breakdown[%s]: got %v want %v", k, got.Breakdown[k], v)
				}
			}
		})
	}
}

func TestRamp(t *testing.T) {
	r := ramp{Full: 10, Zero: 20}
	for _, tc := range []struct{ v, want float64 }{{0, 1}, {10, 1}, {15, 0.5}, {20, 0}, {99, 0}} {
		if got := r.at(tc.v); got != tc.want {
			t.Errorf("at(%v) = %v want %v", tc.v, got, tc.want)
		}
	}
}

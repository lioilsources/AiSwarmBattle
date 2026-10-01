package static

import (
	"os"
	"testing"

	"github.com/lioilsources/shaderbattle/evaluator/internal/preprocess"
)

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, got analyzeResult)
	}{
		{
			name: "literal for bound",
			src:  "void main() { for (int i = 0; i < 8; i++) { x += 1.0; } }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopCount", g.LoopCount, 1)
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, 8)
				g.eq(t, "EstimatedALU", g.EstimatedALU, 8) // one += per iteration
			},
		},
		{
			name: "inclusive negative range",
			src:  "void main() { for (int i = -2; i <= 2; i++) { c += texture(t, uv).r; } }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, 5)
				g.eq(t, "TexReads", g.TexReads, 1)
				g.eq(t, "EstimatedALU", g.EstimatedALU, 5*(100+1))
			},
		},
		{
			name: "nested loops multiply cost",
			src:  "void main() { for (int i = 0; i < 4; i++) { for (int j = 0; j < 4; j++) { a = a * b; } } }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopCount", g.LoopCount, 2)
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, 4)
				g.eq(t, "EstimatedALU", g.EstimatedALU, 16)
			},
		},
		{
			name: "single statement body",
			src:  "void main() { for (int i = 0; i < 10; i++) a = a + 1.0; b = b + 1.0; }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "EstimatedALU", g.EstimatedALU, 11)
			},
		},
		{
			name: "define and const bounds",
			src:  "#define STEPS 32\nconst int N = 3;\nvoid main() { for (int i = 0; i < STEPS; i++) {} for (int k = N; k > 0; k--) {} }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopCount", g.LoopCount, 2)
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, 32)
			},
		},
		{
			name: "float step loop",
			src:  "void main() { for (float s = 0.0; s < 1.0; s += 0.25) {} }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, 4)
			},
		},
		{
			name: "dynamic bound is unknown",
			src:  "uniform float n;\nvoid main() { for (int i = 0; i < int(n); i++) { a = a + 1.0; } }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, -1)
				g.eq(t, "EstimatedALU", g.EstimatedALU, int(DefaultWeights.UnknownLoopIter))
			},
		},
		{
			name: "while and do-while are unknown, counted once each",
			src:  "void main() { while (a < 1.0) { a = a * 2.0; } do { b = b * 2.0; } while (b < 1.0); }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "LoopCount", g.LoopCount, 2)
				g.eq(t, "LoopMaxIter", g.LoopMaxIter, -1)
			},
		},
		{
			name: "branches, discard, derivatives, functions, uniforms",
			src: "uniform vec2 a, b;\nuniform float c;\nfloat f(float x) { return x > 0.0 ? x : -x; }\n" +
				"void main() { if (f(1.0) > 0.5) discard; float d = fwidth(c); }",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "FunctionCount", g.FunctionCount, 2)
				g.eq(t, "BranchCount", g.BranchCount, 2)
				g.eq(t, "UniformCount", g.UniformCount, 3)
				g.eqb(t, "UsesDiscard", g.UsesDiscard, true)
				g.eqb(t, "UsesDerivatives", g.UsesDerivatives, true)
			},
		},
		{
			name: "comments are ignored",
			src:  "// texture(t, uv) if for\n/* sin(x) * 2.0\n discard */\nvoid main() {}",
			check: func(t *testing.T, g analyzeResult) {
				g.eq(t, "TexReads", g.TexReads, 0)
				g.eq(t, "BranchCount", g.BranchCount, 0)
				g.eq(t, "EstimatedALU", g.EstimatedALU, 0)
				g.eqb(t, "UsesDiscard", g.UsesDiscard, false)
				g.eq(t, "LOC", g.LOC, 4)
				g.eq(t, "LOCLogical", g.LOCLogical, 1)
			},
		},
		{
			name: "precision from preprocessed source",
			src:  preprocess.Flutter("#include <flutter/runtime_effect.glsl>\nvoid main() {}").Source,
			check: func(t *testing.T, g analyzeResult) {
				if g.PrecisionDefault != "highp" {
					t.Errorf("PrecisionDefault = %q", g.PrecisionDefault)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, analyzeResult{Analyze(tc.src, DefaultWeights)})
		})
	}
}

func TestAnalyzeReferenceShaders(t *testing.T) {
	tests := []struct {
		file        string
		texReads    int
		loopMaxIter int
	}{
		{file: "filter_glitch.frag", texReads: 6, loopMaxIter: 0},
		{file: "filter_crt.frag", texReads: 3, loopMaxIter: 0},
		{file: "filter_thermal.frag", texReads: 1, loopMaxIter: 0},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			src, err := os.ReadFile("../../testdata/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			pp := preprocess.Flutter(string(src)).Source
			a, b := Analyze(pp, DefaultWeights), Analyze(pp, DefaultWeights)
			if a != b {
				t.Fatal("analysis is not deterministic")
			}
			if a.TexReads != tc.texReads || a.LoopMaxIter != tc.loopMaxIter {
				t.Errorf("got tex_reads=%d loop_max_iter=%d", a.TexReads, a.LoopMaxIter)
			}
		})
	}
}

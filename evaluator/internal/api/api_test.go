package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lioilsources/shaderbattle/evaluator/internal/egl"
	"github.com/lioilsources/shaderbattle/evaluator/internal/glslang"
	"github.com/lioilsources/shaderbattle/pkg/types"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	bin := os.Getenv("GLSLANG_VALIDATOR")
	if bin == "" {
		bin = "glslangValidator"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("glslangValidator not available: %v", err)
	}
	s := New(&glslang.Compiler{Bin: bin}, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Renderer: testRenderer(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// testRenderer returns an EGL renderer when EVALUATOR_TEST_EGL is set
// (auto|software), nil otherwise.
func testRenderer(t *testing.T) *egl.Renderer {
	t.Helper()
	mode := os.Getenv("EVALUATOR_TEST_EGL")
	if mode == "" {
		return nil
	}
	r, err := egl.New(egl.Options{PreferSoftware: mode == "software"})
	if err != nil {
		t.Fatalf("egl: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

// mirrorBoothUniforms mirrors _EVAL_UNIFORMS in
// pipelines/mirrorbooth_retrieval/nodes/evaluator.py.
var mirrorBoothUniforms = []types.EvalUniform{
	{Name: "uTexture", Type: "sampler2D", Semantic: "camera"},
	{Name: "uResolution", Type: "vec2", Default: []any{1080.0, 1920.0}, Semantic: "resolution"},
	{Name: "uTime", Type: "float", Default: 0.0, Semantic: "time"},
}

func post(t *testing.T, url string, body any) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/evaluate", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func TestEvaluate(t *testing.T) {
	ts := newTestServer(t)
	glitch, err := os.ReadFile("../../testdata/filter_glitch.frag")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		glsl       string
		wantStatus int
		wantOK     bool
	}{
		{name: "reference shader compiles", glsl: string(glitch), wantStatus: 200, wantOK: true},
		{name: "broken shader", glsl: "#include <flutter/runtime_effect.glsl>\nvoid main() { x = 1; }", wantStatus: 200, wantOK: false},
		{name: "empty source rejected", glsl: "", wantStatus: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := post(t, ts.URL, types.EvalRequest{RequestID: "t1", GLSLES: tc.glsl, Uniforms: mirrorBoothUniforms})
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if tc.wantStatus != 200 {
				return
			}
			var res types.EvalResult
			if err := json.Unmarshal(body, &res); err != nil {
				t.Fatal(err)
			}
			if res.Compile.OK != tc.wantOK {
				t.Fatalf("compile.ok=%v, errors=%+v", res.Compile.OK, res.Compile.Errors)
			}
			if tc.wantOK && res.Compile.SpirvB64 == "" {
				t.Error("spirv_b64 missing")
			}
			if !tc.wantOK && res.Score.Composite != 0 {
				t.Errorf("failed compile must score 0, got %v", res.Score.Composite)
			}
			if res.PassesGate() != tc.wantOK {
				t.Errorf("PassesGate()=%v", res.PassesGate())
			}
			if res.RequestID != "t1" || res.EvaluatorVersion == "" || res.CostModelVersion == "" {
				t.Errorf("missing metadata: %+v", res)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestCorpusCalibration evaluates every shader in $EVALUATOR_CORPUS_DIR (e.g.
// MirrorBooth/mirrorbooth/shaders or the bootstrap ingest corpus). All of them
// are known-good production filters, so each must compile, pass the gate and
// score identically across two runs.
func TestCorpusCalibration(t *testing.T) {
	dir := os.Getenv("EVALUATOR_CORPUS_DIR")
	if dir == "" {
		t.Skip("EVALUATOR_CORPUS_DIR not set")
	}
	ts := newTestServer(t)
	files, _ := filepath.Glob(filepath.Join(dir, "*.frag"))
	if len(files) == 0 {
		t.Fatalf("no .frag files in %s", dir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var runs [2]types.EvalResult
			for i := range runs {
				resp, body := post(t, ts.URL, types.EvalRequest{RequestID: "calib", GLSLES: string(src), Uniforms: mirrorBoothUniforms})
				if resp.StatusCode != 200 {
					t.Fatalf("status %d: %s", resp.StatusCode, body)
				}
				if err := json.Unmarshal(body, &runs[i]); err != nil {
					t.Fatal(err)
				}
			}
			a, b := runs[0], runs[1]
			if !a.Compile.OK {
				t.Fatalf("compile failed: %+v", a.Compile.Errors)
			}
			if !a.PassesGate() {
				t.Errorf("gate failed: score=%v", a.Score)
			}
			if a.Score.Composite != b.Score.Composite || a.Static != b.Static || a.Compile.SpirvB64 != b.Compile.SpirvB64 {
				t.Errorf("non-deterministic result")
			}
			for _, r := range a.Runtime.PerResolution {
				t.Logf("runtime %dx%d frames=%d median=%.3fms p95=%.3fms first=%.3fms nan=%v black=%d constant=%d sw=%v",
					r.Width, r.Height, r.FramesRendered, r.MedianFrameMS, r.P95FrameMS, r.FirstFrameMS,
					r.NaNInfDetected, r.BlackFrameCount, r.ConstantFrameCount, r.SoftwareRenderer)
			}
			if a.Runtime.SkippedReason != "" {
				t.Logf("runtime skipped: %s", a.Runtime.SkippedReason)
			}
			t.Logf("composite=%6.2f alu=%5d tex=%2d loops=%d/%d branches=%d breakdown=%v",
				a.Score.Composite, a.Static.EstimatedALU, a.Static.TexReads,
				a.Static.LoopCount, a.Static.LoopMaxIter, a.Static.BranchCount, a.Score.Breakdown)
		})
	}
}

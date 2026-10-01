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
	"testing"

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
	s := New(&glslang.Compiler{Bin: bin}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
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
			resp, body := post(t, ts.URL, types.EvalRequest{RequestID: "t1", GLSLES: tc.glsl})
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

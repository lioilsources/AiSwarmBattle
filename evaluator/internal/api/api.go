// Package api exposes the evaluator over HTTP.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/lioilsources/shaderbattle/evaluator/internal/glslang"
	"github.com/lioilsources/shaderbattle/evaluator/internal/preprocess"
	"github.com/lioilsources/shaderbattle/evaluator/internal/score"
	"github.com/lioilsources/shaderbattle/evaluator/internal/static"
	"github.com/lioilsources/shaderbattle/pkg/types"
)

// EvaluatorVersion is reported as evaluator_version in every result.
const EvaluatorVersion = "0.1.0"

const (
	maxBodyBytes      = 1 << 20
	maxFrames         = 2000
	maxResolutions    = 4
	maxDimension      = 4096
	defaultFrames     = 60
	defaultMaxCompile = 10
)

var defaultResolutions = [][2]int{{1080, 1920}}

// Server handles evaluation requests. Compiles run concurrently up to
// MaxCompiles.
type Server struct {
	Compiler *glslang.Compiler
	Log      *slog.Logger

	compileSem chan struct{}
}

// New builds a Server. maxCompiles <= 0 uses the default of 10.
func New(compiler *glslang.Compiler, log *slog.Logger, maxCompiles int) *Server {
	if maxCompiles <= 0 {
		maxCompiles = defaultMaxCompile
	}
	return &Server{
		Compiler:   compiler,
		Log:        log,
		compileSem: make(chan struct{}, maxCompiles),
	}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /evaluate", s.handleEvaluate)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	var req types.EvalRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := validate(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	res, err := s.Evaluate(r.Context(), &req)
	if err != nil {
		s.Log.Error("evaluate", "request_id", req.RequestID, "err", err)
		writeError(w, http.StatusInternalServerError, "evaluator_error", err.Error())
		return
	}
	s.Log.Info("evaluated",
		"request_id", req.RequestID,
		"compile_ok", res.Compile.OK,
		"composite", res.Score.Composite,
		"wall_time_ms", res.WallTimeMS,
	)
	writeJSON(w, http.StatusOK, res)
}

// Evaluate runs all stages for one request.
func (s *Server) Evaluate(ctx context.Context, req *types.EvalRequest) (*types.EvalResult, error) {
	start := time.Now()
	res := &types.EvalResult{
		RequestID:        req.RequestID,
		EvaluatorVersion: EvaluatorVersion,
		CostModelVersion: costModelVersion(),
		Runtime:          types.RuntimeResult{PerResolution: []types.ResolutionResult{}},
	}

	src := preprocess.Flutter(req.GLSLES).Source

	select {
	case s.compileSem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for compile slot: %w", ctx.Err())
	}
	comp, err := s.Compiler.Compile(ctx, src)
	<-s.compileSem
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	res.Compile = types.CompileResult{
		OK:             comp.OK,
		Errors:         comp.Errors,
		Warnings:       comp.Warnings,
		SpirvB64:       base64.StdEncoding.EncodeToString(comp.SPIRV),
		SpirvSizeBytes: len(comp.SPIRV),
	}

	res.Static = static.Analyze(src, static.DefaultWeights)

	if !comp.OK {
		res.Runtime.SkippedReason = "compile_failed"
	} else {
		// TODO: runtime profiler (EGL) is not wired yet.
		res.Runtime.SkippedReason = "runtime_not_implemented"
	}

	res.Score = score.Compute(res, score.DefaultWeights)
	res.WallTimeMS = time.Since(start).Milliseconds()
	return res, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ver, err := s.Compiler.Version(r.Context())
	body := map[string]any{
		"ok":                 err == nil,
		"version":            EvaluatorVersion,
		"cost_model_version": costModelVersion(),
		"glslang_version":    ver,
	}
	status := http.StatusOK
	if err != nil {
		body["error"] = err.Error()
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, body)
}

func costModelVersion() string {
	return score.Version + "+" + preprocess.Version
}

func validate(req *types.EvalRequest) error {
	if req.GLSLES == "" {
		return errors.New("glsl_es is required")
	}
	c := &req.Config
	if c.TestDurationFrames == 0 {
		c.TestDurationFrames = defaultFrames
	}
	if c.TestDurationFrames < 0 || c.TestDurationFrames > maxFrames {
		return fmt.Errorf("test_duration_frames must be in 1..%d", maxFrames)
	}
	if len(c.TargetResolutions) == 0 {
		c.TargetResolutions = defaultResolutions
	}
	if len(c.TargetResolutions) > maxResolutions {
		return fmt.Errorf("at most %d target_resolutions", maxResolutions)
	}
	for _, wh := range c.TargetResolutions {
		if wh[0] <= 0 || wh[1] <= 0 || wh[0] > maxDimension || wh[1] > maxDimension {
			return fmt.Errorf("resolution %dx%d out of range 1..%d", wh[0], wh[1], maxDimension)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

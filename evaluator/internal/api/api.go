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

	"github.com/lioilsources/shaderbattle/evaluator/internal/egl"
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
	defaultBudget     = 8 * time.Second
)

var defaultResolutions = [][2]int{{1080, 1920}}

// Options configure a Server. Zero values use defaults.
type Options struct {
	// MaxCompiles bounds concurrent glslang runs (default 10).
	MaxCompiles int
	// Renderer profiles shaders at runtime; nil disables the runtime stage.
	Renderer *egl.Renderer
	// RuntimeBudget caps total rendering time per request across all
	// resolutions (default 8 s; the pipeline client times out at 12 s).
	RuntimeBudget time.Duration
}

// Server handles evaluation requests. Compiles run concurrently; rendering is
// serialized by the Renderer.
type Server struct {
	Compiler *glslang.Compiler
	Log      *slog.Logger

	renderer   *egl.Renderer
	budget     time.Duration
	compileSem chan struct{}
}

// New builds a Server.
func New(compiler *glslang.Compiler, log *slog.Logger, opts Options) *Server {
	if opts.MaxCompiles <= 0 {
		opts.MaxCompiles = defaultMaxCompile
	}
	if opts.RuntimeBudget <= 0 {
		opts.RuntimeBudget = defaultBudget
	}
	return &Server{
		Compiler:   compiler,
		Log:        log,
		renderer:   opts.Renderer,
		budget:     opts.RuntimeBudget,
		compileSem: make(chan struct{}, opts.MaxCompiles),
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

	if err := s.runtimeStage(ctx, req, src, start, res); err != nil {
		return nil, err
	}

	res.Score = score.Compute(res, score.DefaultWeights)
	res.WallTimeMS = time.Since(start).Milliseconds()
	return res, nil
}

// runtimeStage fills res.Runtime. It only returns an error when the request
// context ended; profiling problems are reported via skipped_reason.
func (s *Server) runtimeStage(ctx context.Context, req *types.EvalRequest, src string, start time.Time, res *types.EvalResult) error {
	switch {
	case !res.Compile.OK:
		res.Runtime.SkippedReason = "compile_failed"
		return nil
	case req.Config.SkipRuntime:
		res.Runtime.SkippedReason = "skipped_by_request"
		return nil
	case s.renderer == nil:
		res.Runtime.SkippedReason = "egl_unavailable"
		return nil
	case res.Static.LoopMaxIter < 0:
		// A non-constant loop bound may never terminate; a hung draw call
		// cannot be interrupted and would stall the single render context.
		res.Runtime.SkippedReason = "unbounded_loop"
		return nil
	}

	info := s.renderer.Info()
	res.Runtime.Renderer = info.Renderer
	if info.Software {
		res.CostModelVersion += "+sw"
	}
	uniforms := toUniforms(req.Uniforms)
	deadline := start.Add(s.budget)

	for _, wh := range req.Config.TargetResolutions {
		r, err := s.renderer.Render(ctx, egl.Job{
			Source:   src,
			Uniforms: uniforms,
			Width:    wh[0],
			Height:   wh[1],
			Frames:   req.Config.TestDurationFrames,
			Deadline: deadline,
		})
		var ce *egl.CompileError
		switch {
		case errors.As(err, &ce):
			res.Compile.OK = false
			res.Compile.Errors = append(res.Compile.Errors, types.Diagnostic{
				Severity: "error", Message: "gles driver: " + ce.Log,
			})
			res.Compile.SpirvB64, res.Compile.SpirvSizeBytes = "", 0
			res.Runtime.PerResolution = []types.ResolutionResult{}
			res.Runtime.SkippedReason = "driver_compile_failed"
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("runtime: %w", ctx.Err())
		case err != nil:
			res.Runtime.SkippedReason = "runtime_error: " + err.Error()
			return nil
		}
		res.Runtime.PerResolution = append(res.Runtime.PerResolution, types.ResolutionResult{
			Width:              wh[0],
			Height:             wh[1],
			FramesRendered:     r.FramesRendered,
			MedianFrameMS:      r.MedianMS,
			P95FrameMS:         r.P95MS,
			FirstFrameMS:       r.FirstFrameMS,
			NaNInfDetected:     r.NaNInf,
			BlackFrameCount:    r.BlackFrames,
			ConstantFrameCount: r.ConstantFrames,
			SoftwareRenderer:   info.Software,
		})
		if r.Truncated {
			res.Runtime.SkippedReason = "budget_exceeded"
			break
		}
	}
	return nil
}

// toUniforms converts request uniforms. Missing semantics fall back to the
// MirrorBooth / ShaderToy naming conventions.
func toUniforms(in []types.EvalUniform) []egl.Uniform {
	out := make([]egl.Uniform, 0, len(in))
	for _, u := range in {
		sem := u.Semantic
		if sem == "" {
			switch {
			case u.Type == "sampler2D":
				sem = "camera"
			case u.Name == "uTime" || u.Name == "iTime":
				sem = "time"
			case u.Name == "uResolution" || u.Name == "iResolution":
				sem = "resolution"
			}
		}
		out = append(out, egl.Uniform{Name: u.Name, Type: u.Type, Value: toFloats(u.Default), Semantic: sem})
	}
	return out
}

func toFloats(v any) []float64 {
	switch x := v.(type) {
	case float64:
		return []float64{x}
	case bool:
		if x {
			return []float64{1}
		}
		return []float64{0}
	case []any:
		out := make([]float64, 0, len(x))
		for _, e := range x {
			if f, ok := e.(float64); ok {
				out = append(out, f)
			}
		}
		return out
	default:
		return nil
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ver, err := s.Compiler.Version(r.Context())
	body := map[string]any{
		"ok":                 err == nil,
		"version":            EvaluatorVersion,
		"cost_model_version": costModelVersion(),
		"glslang_version":    ver,
		"egl_initialized":    s.renderer != nil,
	}
	if s.renderer != nil {
		info := s.renderer.Info()
		body["renderer"] = info.Renderer
		body["software_renderer"] = info.Software
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

package types

// ShaderRequest is the inbound request from the pipeline CLI or management API.
type ShaderRequest struct {
	RequestID   string      `json:"request_id"`
	Prompt      string      `json:"prompt"`
	Constraints Constraints `json:"constraints"`
	Seed        int64       `json:"seed"`
}

type Constraints struct {
	MaxMobileCost   int      `json:"max_mobile_cost"`
	RequireCamera   bool     `json:"require_camera"`
	RequireAnimated bool     `json:"require_animated"`
	AspectRatio     string   `json:"aspect_ratio"` // portrait|landscape
	AllowSource     []string `json:"allow_source"` // retrieved|adapted|generated
}

func (c *Constraints) ApplyDefaults() {
	if c.MaxMobileCost == 0 {
		c.MaxMobileCost = 200
	}
	if c.AspectRatio == "" {
		c.AspectRatio = "portrait"
	}
	if len(c.AllowSource) == 0 {
		c.AllowSource = []string{"retrieved", "adapted", "generated"}
	}
}

// FragFile is the output of the retrieval pipeline — a ready-to-install Flutter shader.
type FragFile struct {
	Name      string `json:"name"`       // e.g. "filter_neon_bokeh" (no extension)
	Content   []byte `json:"content"`    // full .frag file content
	NeedsTime bool   `json:"needs_time"` // true if shader uses uTime
	Label     string `json:"label"`      // short display label, e.g. "Neon"
	Icon      string `json:"icon"`       // single unicode char, e.g. "✨"
	Source    string `json:"source"`     // retrieved|adapted|generated
	ShaderID  string `json:"shader_id"`  // original ShaderToy ID
	EvalScore float64 `json:"eval_score,omitempty"`
}

// UniformInfo describes a single uniform in the Flutter shader.
// Floats are set positionally via shader.setFloat(index, value).
// Samplers are set via shader.setImageSampler(index, image).
type UniformInfo struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Semantic string `json:"semantic"`
}

// MirrorBoothUniforms is the fixed uniform layout for static (non-animated) shaders.
// Flutter sets image samplers separately from floats.
var MirrorBoothUniforms = []UniformInfo{
	{Index: 0, Name: "uTexture",    Type: "sampler2D", Semantic: "camera"},  // setImageSampler(0, ...)
	{Index: 0, Name: "uResolution", Type: "vec2",      Semantic: "resolution"}, // setFloat(0, w); setFloat(1, h)
}

// MirrorBoothUniformsAnimated is the layout for animated shaders (needsTime=true).
var MirrorBoothUniformsAnimated = []UniformInfo{
	{Index: 0, Name: "uTexture",    Type: "sampler2D", Semantic: "camera"},
	{Index: 0, Name: "uResolution", Type: "vec2",      Semantic: "resolution"}, // setFloat(0, w); setFloat(1, h)
	{Index: 2, Name: "uTime",       Type: "float",     Semantic: "time"},        // setFloat(2, t)
}

// MirrorBoothPreamble is the Flutter/Impeller preamble for static shaders.
// No #version — Flutter injects it. Uses FlutterFragCoord() not gl_FragCoord.
const MirrorBoothPreamble = `#include <flutter/runtime_effect.glsl>

uniform sampler2D uTexture;
uniform vec2      uResolution;

out vec4 fragColor;
`

// MirrorBoothPreambleAnimated is the preamble for animated shaders (uses uTime).
const MirrorBoothPreambleAnimated = `#include <flutter/runtime_effect.glsl>

uniform sampler2D uTexture;
uniform vec2      uResolution;
uniform float     uTime;

out vec4 fragColor;
`

type RetrievalInfo struct {
	CandidatesFound int     `json:"candidates_found"`
	SelectedRank    int     `json:"selected_rank"`
	SimilarityScore float64 `json:"similarity_score"`
}

type Provenance struct {
	OriginalID   string `json:"original_id"`
	ShaderToyURL string `json:"shadertoy_url"`
	License      string `json:"license"`
	Author       string `json:"author"`
}

// PipelineResult is the full output of one pipeline run, used by the management API.
type PipelineResult struct {
	RequestID  string      `json:"request_id"`
	FragFile   FragFile    `json:"frag_file"`
	Retrieval  RetrievalInfo `json:"retrieval"`
	Provenance Provenance  `json:"provenance"`
	Installed  bool        `json:"installed"`
	LatencyMS  int64       `json:"latency_ms"`
}

// EvalRequest is sent to the evaluator HTTP service (EVALUATOR.md).
type EvalRequest struct {
	RequestID string        `json:"request_id"`
	GLSLES    string        `json:"glsl_es"`
	Uniforms  []EvalUniform `json:"uniforms"`
	Config    EvalConfig    `json:"config"`
}

type EvalUniform struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default any    `json:"default"`
}

type EvalConfig struct {
	TestDurationFrames int      `json:"test_duration_frames"`
	TargetResolutions  [][2]int `json:"target_resolutions"`
	FuzzRuns           int      `json:"fuzz_runs"`
	SkipRuntime        bool     `json:"skip_runtime"`
	SkipFuzz           bool     `json:"skip_fuzz"`
}

// EvalResult is returned by the evaluator.
type EvalResult struct {
	RequestID        string        `json:"request_id"`
	EvaluatorVersion string        `json:"evaluator_version"`
	CostModelVersion string        `json:"cost_model_version"`
	WallTimeMS       int64         `json:"wall_time_ms"`
	Compile          CompileResult `json:"compile"`
	Static           StaticResult  `json:"static"`
	Runtime          RuntimeResult `json:"runtime"`
	Score            ScoreResult   `json:"score"`
}

type CompileResult struct {
	OK       bool         `json:"ok"`
	Errors   []Diagnostic `json:"errors"`
	Warnings []Diagnostic `json:"warnings"`
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Message  string `json:"message"`
}

type StaticResult struct {
	LOC          int `json:"loc"`
	LOCLogical   int `json:"loc_logical"`
	TexReads     int `json:"tex_reads"`
	EstimatedALU int `json:"estimated_alu"`
}

type RuntimeResult struct {
	PerResolution []ResolutionResult `json:"per_resolution"`
}

type ResolutionResult struct {
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	FramesRendered  int     `json:"frames_rendered"`
	MedianFrameMS   float64 `json:"median_frame_ms"`
	P95FrameMS      float64 `json:"p95_frame_ms"`
	NaNInfDetected  bool    `json:"nan_inf_detected"`
	BlackFrameCount int     `json:"black_frame_count"`
}

type ScoreResult struct {
	Composite float64            `json:"composite"`
	Breakdown map[string]float64 `json:"breakdown"`
}

func (r *EvalResult) PassesGate() bool {
	if !r.Compile.OK {
		return false
	}
	if r.Score.Composite < 40 {
		return false
	}
	for _, res := range r.Runtime.PerResolution {
		if res.NaNInfDetected {
			return false
		}
		if res.MedianFrameMS > 8 {
			return false
		}
	}
	return true
}

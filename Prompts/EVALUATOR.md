# EVALUATOR.md — Fáze 2 detail

Evaluator je golden source of truth pro battle. Kompiluje, profiluje, fuzzuje, skóruje. **Deterministický.** Žádný LLM v hot pathu. Žádné heuristiky bez kalibrace. Když řekne „skóre 72.3", tak je to 72.3 reprodukovatelně.

## Veřejné API

HTTP service, běží na DGX (`:8081`), volaný battle orchestratorem.

### `POST /evaluate`

```json
{
  "request_id": "uuid-v7",
  "glsl_es": "#version 300 es\nprecision highp float;\n...",
  "uniforms": [
    {"name": "iResolution", "type": "vec3", "default": [1080, 1920, 1]},
    {"name": "iTime", "type": "float", "default": 0.0}
  ],
  "config": {
    "test_duration_frames": 300,
    "target_resolutions": [[720, 1280], [1080, 1920]],
    "fuzz_runs": 50,
    "skip_runtime": false,
    "skip_fuzz": false
  }
}
```

### Response

```json
{
  "request_id": "uuid-v7",
  "evaluator_version": "0.3.1",
  "cost_model_version": "2024.11",
  "wall_time_ms": 4821,

  "compile": {
    "ok": true,
    "errors": [],
    "warnings": [
      {"line": 42, "severity": "warn", "message": "implicit precision for varying"}
    ],
    "spirv_size_bytes": 8420
  },

  "static": {
    "loc": 142,
    "loc_logical": 96,
    "function_count": 8,
    "branch_count": 12,
    "loop_count": 2,
    "loop_max_iter": 64,
    "tex_reads": 4,
    "uniform_count": 6,
    "uses_discard": false,
    "uses_derivatives": true,
    "estimated_alu": 380,
    "estimated_register_pressure": 14,
    "precision_default": "highp"
  },

  "runtime": {
    "per_resolution": [
      {
        "width": 720, "height": 1280,
        "frames_rendered": 300,
        "median_frame_ms": 3.1,
        "p95_frame_ms": 4.0,
        "p99_frame_ms": 5.8,
        "max_frame_ms": 12.2,
        "nan_inf_detected": false,
        "black_frame_count": 0,
        "constant_frame_count": 0,
        "first_frame_ms": 8.4
      },
      {
        "width": 1080, "height": 1920,
        "..."
      }
    ]
  },

  "robustness": {
    "fuzz_runs": 50,
    "crashes": 0,
    "compile_failures_under_perturbation": 0,
    "nan_outputs": 0,
    "extreme_value_stable": true,
    "property_violations": []
  },

  "score": {
    "composite": 87.5,
    "breakdown": {
      "compile":        40.0,
      "no_runtime_nan": 15.0,
      "no_artifacts":   10.0,
      "fuzz_stability": 13.5,
      "perf_target":     9.0,
      "code_quality":    7.0
    }
  }
}
```

### `GET /healthz`

```json
{
  "ok": true,
  "version": "0.3.1",
  "egl_initialized": true,
  "glslang_version": "11.13.0",
  "renderer": "NVIDIA GB10 Blackwell"
}
```

### `GET /metrics` — Prometheus

```
shaderbattle_eval_total{result="pass"} 1247
shaderbattle_eval_total{result="compile_fail"} 312
shaderbattle_eval_duration_seconds_bucket{le="5"} 1101
shaderbattle_eval_score_composite_bucket{le="50"} 423
shaderbattle_eval_score_composite_bucket{le="80"} 1102
```

## Architektura

```
┌─────────────────────────────────────────────────────────────┐
│ HTTP Server (chi router, :8081)                             │
│  - parallel request acceptance                              │
│  - rate limiting (10 concurrent compiles, 1 render at time) │
└───────────┬─────────────────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────────────────────────┐
│ Pipeline (per request)                                      │
│  1. compile (parallel-safe, glslang)                        │
│  2. static (parallel-safe, AST + SPIR-V)                    │
│  3. runtime (serialized: single EGL context channel)        │
│  4. fuzz (uses runtime channel)                             │
│  5. score (parallel-safe)                                   │
└─────────────────────────────────────────────────────────────┘

EGL goroutine (single, owns GL context):
  - receives RenderJob over channel
  - executes, sends RenderResult back
  - serialized but pipelined: while job N renders, job N+1 prepares
```

Důvod single-threaded EGL: GL context je per-thread, multi-context tah peněz nestojí za to a chyby v driver na ARM64 hlásí 50% lidí. Single thread, channel-based job queue, hotová věc.

## Tech detail: EGL na DGX Spark (ARM64 + Blackwell)

### Headless context init

```go
package egl

/*
#cgo CFLAGS: -I/usr/include
#cgo LDFLAGS: -lEGL -lGLESv2

#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES3/gl3.h>
#include <GLES3/gl3ext.h>
*/
import "C"

type Context struct {
    display C.EGLDisplay
    context C.EGLContext
    surface C.EGLSurface
}

func NewHeadlessContext(width, height int) (*Context, error) {
    // 1. eglQueryDevicesEXT → list NVIDIA devices
    // 2. eglGetPlatformDisplayEXT(EGL_PLATFORM_DEVICE_EXT, device, ...)
    // 3. eglInitialize
    // 4. eglChooseConfig pro GLES 3.0, RGBA8888, no depth
    // 5. eglCreatePbufferSurface(width × height)
    // 6. eglBindAPI(EGL_OPENGL_ES_API)
    // 7. eglCreateContext s GLES 3.0 attributes
    // 8. eglMakeCurrent
}

func (c *Context) MakeCurrent() error
func (c *Context) Destroy() error
```

Pro `eglQueryDevicesEXT` potřebuješ EGL_EXT_device_query + EGL_EXT_platform_device. NVIDIA proprietary driver to umí, ale headers musí být nainstalované (`libegl1-mesa-dev` nebo `libegl-dev` na ARM64 + NVIDIA headers).

### Build podmínky

```dockerfile
# Dockerfile.evaluator (ARM64)
FROM nvcr.io/nvidia/l4t-cuda:12.6.11-runtime-arm64
RUN apt-get update && apt-get install -y \
    build-essential cmake git pkg-config \
    libegl-dev libgles2-mesa-dev \
    libglvnd-dev \
    && rm -rf /var/lib/apt/lists/*

# glslang from source
RUN git clone --depth 1 -b 14.3.0 https://github.com/KhronosGroup/glslang.git /tmp/glslang \
    && cd /tmp/glslang && python3 update_glslang_sources.py \
    && cmake -B build -DENABLE_OPT=0 -DCMAKE_BUILD_TYPE=Release \
    && cmake --build build -j$(nproc) \
    && cmake --install build

# Go binary
COPY . /app
WORKDIR /app
RUN go build -o /usr/local/bin/evaluator ./evaluator/cmd/evaluator

ENV __EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
ENV LIBGL_ALWAYS_INDIRECT=0
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/evaluator"]
```

Spuštění s `--gpus all` + `--device /dev/dri` + `-v /usr/share/glvnd:/usr/share/glvnd:ro`.

### Fallback: llvmpipe SW renderer

Pro CI a unit testy bez GPU:

```go
func NewContext(opts ContextOptions) (*Context, error) {
    if opts.SoftwareFallback {
        return newLLVMPipeContext(opts)
    }
    return newNVIDIAContext(opts)
}
```

Mesa `llvmpipe` umí EGL pbuffer + GLES 3.0. Pomalé (10-100×), ale deterministic a CI-friendly. Pro production běh DGX vždy NVIDIA cesta.

## Compile stage (glslang)

```go
package glslang

/*
#cgo LDFLAGS: -lglslang -lSPIRV -lglslang-default-resource-limits
#include "glslang_c_interface.h"
#include "resource_limits_c.h"
*/
import "C"

type Stage int
const (
    StageFragment Stage = iota
    StageVertex
)

type Target int
const (
    TargetGLES300 Target = iota
    TargetGLES310
    TargetGL450
)

type CompileResult struct {
    OK       bool
    SPIRV    []byte
    Errors   []Diagnostic
    Warnings []Diagnostic
}

type Diagnostic struct {
    Severity string  // error | warn | info
    Line     int
    Column   int
    Message  string
}

func Compile(src []byte, stage Stage, target Target) (*CompileResult, error)
```

**Kritické**: pro mobile target nastavit:
- `client = GLSLANG_CLIENT_OPENGL` (ne Vulkan, jinak je sémantika jiná)
- `client_version = GLSLANG_TARGET_OPENGL_450` ale `messages |= GLSLANG_MSG_RELAXED_ERRORS_BIT` NE
- Resource limits z `DefaultTBuiltInResource` ale upravit pro ES (MaxDrawBuffers atd.)

Diagnostiky parsovat regex z výstupu (glslang má strukturované zprávy `ERROR: 0:42: 'foo' : message`).

## Static analyzer

Pracuje na AST (tree-sitter, sdílený s ingest) + (volitelně) na SPIR-V z glslang.

```go
package static

func Analyze(ast *glsl.AST, spirv []byte) (*Metrics, error)
```

**Cost model** — verzovaný, kalibrovaný:

```go
package cost

type Model struct {
    Version string
    Weights Weights
}

type Weights struct {
    AddSubMul       float64  // 1.0
    Div             float64  // 4.0
    SqrtRsqrt       float64  // 4.0
    ExpLog          float64  // 4.0
    SinCosTan       float64  // 8.0 (drahé na mobilu)
    Pow             float64  // 8.0
    TextureRead     float64  // 100.0 (latence)
    DiscardPenalty  float64  // 50.0
    BranchPenalty   float64  // 2.0 (divergence)
    LoopMultiplier  float64  // applies max_iter
}

func Estimate(ast *glsl.AST, model Model) float64
```

Kalibrace: 20-30 hand-picked shaderů, ručně změřit median frame time na Snapdragon 8 Gen 2 (nebo whatever máš k dispozici), naplň pairs `(metrics, real_ms)`. Linear regression na koeficientech → updated Weights. Snapshot do `cost_model_versions.json`.

```bash
make calibrate-cost-model DEVICE=snapdragon-8-gen-2
```

## Runtime stage

```go
package runtime

type RenderJob struct {
    ShaderProgram   uint32  // GL program ID
    Uniforms        map[string]Uniform
    Width, Height   int
    FrameCount      int
    OutputPath      string  // optional, pro debug snapshots
}

type RenderResult struct {
    FramesRendered     int
    FrameTimesMs       []float64
    NaNFrames          []int  // indexy framů s NaN/Inf
    BlackFrames        []int
    ConstantFrames     []int  // identical s předchozím
    HistogramBuckets   [256]uint32  // per channel
}
```

### Render loop

```go
func renderJobLoop(ctx context.Context, jobs <-chan RenderJob, results chan<- RenderResult) {
    // EGL context owned tímto goroutine
    eglCtx, _ := egl.NewHeadlessContext(1920, 1920)
    defer eglCtx.Destroy()
    
    // Pre-create resources reusable across jobs
    var fbo, tex, query uint32
    gl.GenFramebuffers(1, &fbo)
    gl.GenTextures(1, &tex)
    gl.GenQueries(1, &query)

    for {
        select {
        case <-ctx.Done():
            return
        case job := <-jobs:
            res := executeRenderJob(eglCtx, job, fbo, tex, query)
            results <- res
        }
    }
}
```

### GPU timing

`glBeginQuery(GL_TIME_ELAPSED, query)` ... `glEndQuery(GL_TIME_ELAPSED)` ... `glGetQueryObjectui64v(query, GL_QUERY_RESULT, &nanos)`.

**Důležité**: První frame je vždycky pomalý (shader JIT, cache warmup). Loguj separátně jako `first_frame_ms`. Median počítej z framů 10+.

### Artifact detection

Po každém framu `glReadPixels(0,0,w,h, GL_RGBA, GL_UNSIGNED_BYTE, buf)` (rychlé pro malé pixel buffer 256×256 sample) NEBO `glReadPixels(0,0,16,16, ...)` jako sampling check.

- **NaN/Inf**: float channel readback (GL_RGBA32F FBO) → kontrola `math.IsNaN`, `math.IsInf`
- **Black frame**: všechny pixely < epsilon
- **Constant frame**: hash framu N == hash framu N-1 přes M framů (kde M se mění iTime → očekává se změna)

Toto všechno se počítá při readbacku, ne separátním passem.

## Fuzz harness

**Property-based testing**. Definuj vlastnosti, které musí shader splňovat napříč všemi uniform configuracemi.

```go
package fuzz

type Property struct {
    Name      string
    Predicate func(input UniformSet, output FrameOutput) bool
    Message   string
}

var DefaultProperties = []Property{
    {
        Name: "output_in_unit_range",
        Predicate: func(_ UniformSet, o FrameOutput) bool {
            return o.MaxChannel <= 1.0+EPS && o.MinChannel >= 0.0-EPS
        },
        Message: "shader produces output outside [0,1]",
    },
    {
        Name: "no_nan_for_zero_time",
        Predicate: func(in UniformSet, o FrameOutput) bool {
            if in["iTime"].(float32) != 0 { return true }
            return !o.HasNaN
        },
        Message: "NaN produced at iTime=0 (initial frame issue)",
    },
    {
        Name: "stable_under_large_time",
        Predicate: func(in UniformSet, o FrameOutput) bool {
            if in["iTime"].(float32) < 1e5 { return true }
            return !o.HasNaN && o.MaxChannel < 1e6
        },
        Message: "shader unstable for large iTime (precision loss)",
    },
    {
        Name: "no_division_explosion_for_zero_mouse",
        Predicate: func(in UniformSet, o FrameOutput) bool {
            mouse := in["iMouse"].(mgl32.Vec4)
            if mouse != (mgl32.Vec4{0, 0, 0, 0}) { return true }
            return o.MaxChannel < 100
        },
        Message: "shader produces huge values when mouse is zero",
    },
    {
        Name: "deterministic_for_fixed_inputs",
        Predicate: deterministicProperty,
        Message:   "same inputs produce different outputs across runs",
    },
    // ... další: aspect ratio resilience, frame extreme, etc.
}

type FuzzResult struct {
    Runs        int
    Crashes     int
    Violations  []PropertyViolation
}

func Run(prog uint32, ctx *egl.Context, props []Property, seed int64) FuzzResult
```

### Generování uniform setů

```go
func generateUniforms(seed int64, run int) UniformSet {
    rng := rand.New(rand.NewSource(seed + int64(run)))
    return UniformSet{
        "iTime":       sampleLogUniform(rng, 0.001, 100000),
        "iResolution": sampleResolution(rng),  // mix běžných + extrémních aspect ratios
        "iMouse":      sampleMouse(rng),       // koruny obrazu + střed + (0,0)
        "iFrame":      rng.Int31(),
    }
}
```

Seed je deterministický → reproducible fuzz failures.

## Scoring engine

```go
package score

type Weights struct {
    CompileOK       float64  // 40
    NoRuntimeNaN    float64  // 15
    NoArtifacts     float64  // 10
    FuzzStability   float64  // 15
    PerfTarget      float64  // 10
    CodeQuality     float64  // 10
}

func Compute(result *EvalResult, weights Weights, targets PerfTargets) Score {
    s := Score{Breakdown: map[string]float64{}}

    if !result.Compile.OK {
        s.Composite = 0
        return s  // hard gate
    }
    s.Breakdown["compile"] = weights.CompileOK

    // ... další body
    
    return s
}
```

**Pravidla skórování**:

- `compile`: binární, 0 nebo plné body. Bez kompilace zbytek 0.
- `no_runtime_nan`: binární přes všechny resolutions a fuzz runs.
- `no_artifacts`: black_frames + constant_frames přes všechna runtime běhy < threshold.
- `fuzz_stability`: `(fuzz_runs - violations) / fuzz_runs * weight`.
- `perf_target`: piecewise lineární. Pod target = full. 2× target = 0. Mezi tím interpolace.
- `code_quality`: kompozit z `branch_count` (penalizace nad 16), `tex_reads` (penalizace nad 4), `loop_max_iter` (penalizace nad 64), `precision_default` (penalizace `none`/`lowp`).

**Verzování**: každá změna vah = bump `cost_model_version` + záznam v `docs/scoring_rubric.md`.

## Determinismus

Co může nedeterministicky kolísat:

1. **GPU timing** — frame time má variabilitu ±5%. Mitigace: median z 300 framů, throw away first 10, použij `p95` raději než `max`.
2. **Floating point** — bit-exact reprodukce přes GL drivery není garantována. Mitigace: porovnávej s epsilon (1e-5 pro mobilní precision, 1e-7 pro highp).
3. **Fuzz** — deterministický seed, ale různé seedy v různých runs. Mitigace: seed = hash(shader_source).

Pro každý EvalResult zapiš `seed`, `evaluator_version`, `cost_model_version`. Reproducibility checkpoint: každý měsíc re-run 10 reference shaderů, ověř shodu.

## Performance evaluatoru samotného

Cíl: < 30s per shader (median).

| Stage      | Median ms |
|------------|-----------|
| Compile    | 50        |
| Static     | 20        |
| Runtime 2 resolutions × 300 frames | 4000 |
| Fuzz 50 runs × ~50 frames          | 8000 |
| Score      | 1         |
| **Total**  | **~12s**  |

Při 50 promptech × 3 runs × 2 pipelines = 300 evaluací = ~60 min. Akceptovatelné pro nightly battle.

## Edge cases a known issues

1. **Infinite loops v shaderu** → driver crash, ne timeout. Mitigace: detekce `loop_max_iter < 0` ve static stage → odmítni runtime běh.
2. **Excessive memory allocation** (huge arrays) → GPU OOM, contaminuje další jobs. Mitigace: hard limit `MAX_LOCAL_ARRAY_SIZE`, kontrola v static stage.
3. **NVIDIA driver bug**: occasional EGL crash při rapid context create/destroy. Mitigace: single long-lived context per evaluator instance.
4. **iTime precision loss**: po ~1e5 sekundách `float32` ztrácí přesnost. To je vlastnost mobile, ne bug. Property `stable_under_large_time` to expliciti zachytí jako informaci.
5. **`gl_FragDepth` writes** v ES 3.0 vyžaduje extension. Detekuj a flaguj.

## Test plán

### Unit tests
- `glslang.Compile` na 20 known-good shaderů → OK
- `glslang.Compile` na 20 broken shaderů → errors s expected line numbers
- `cost.Estimate` na sample shadery → výsledky v dokumentovaných range
- `score.Compute` na fixtures → expected breakdown

### Integration tests
- 10 reference shaderů z ingest fáze → full eval → snapshot výsledku → diff
- Property fuzzer na 5 záměrně bug shaderů → každý property violation detected

### Load test
- 100 sériových requestů → bez memory leak (RSS stable po warmup)
- 10 concurrent requestů → throughput stable, no deadlock

### Calibration test
- Reference shadery na fyzickém telefonu (manuální, čtvrtletně) → kalibrace cost model.

## Open items pro budoucnost

- Vertex shader support (zatím jen fragment)
- Compute shader support (ES 3.1+)
- SPIRV-Cross k cross-compile do MSL/HLSL pro multiplatform validation
- Energy estimation (frame_ms × estimated_W) — relevantní pro mobile
- Visual fidelity skóre (CLIP) jako separátní non-blocking metric

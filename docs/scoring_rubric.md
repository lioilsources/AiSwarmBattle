# Scoring rubric — evaluator

Source of truth: `evaluator/internal/score/score.go` and `evaluator/internal/static/static.go`.
Every change to weights, thresholds or rules bumps `score.Version` and adds an entry below.

`cost_model_version` in responses = `<score.Version>+<preprocess.Version>`.

## Current: 2026.10.1 (+ flutter-es300.1)

### Components

| Component        | Weight | Evaluated when                    | Rule |
|------------------|--------|-----------------------------------|------|
| `compile`        | 40     | always                            | binary; failure = composite 0 (hard gate) |
| `code_quality`   | 10     | compile ok                        | mean of 5 static sub-scores (below) |
| `no_runtime_nan` | 15     | runtime ran                       | binary over all resolutions |
| `no_artifacts`   | 10     | runtime ran                       | ramp on worst (black + constant) / frames: <=0.05 full, >=0.5 zero |
| `perf_target`    | 10     | runtime ran on a hardware GPU     | ramp on worst median frame ms: <=8 full, >=16 zero |
| `fuzz_stability` | 15     | not implemented (step 4)          | — |

`composite = 100 * earned / sum(weights of evaluated components)`, rounded to 2 decimals.
Components that did not run are absent from `breakdown` and from the denominator, so the
composite is always on a 0-100 scale. Consequence: a compile-only/static-only result is
not comparable to a full runtime result — compare scores only within the same set of
breakdown keys.

Software renderer (llvmpipe) timings never feed `perf_target` and never fail the gate
(`software_renderer: true` in `per_resolution`).

### code_quality sub-scores (each 0..1, ramp = full at/below, zero at/above)

| Sub-score       | Metric          | Full  | Zero   | Notes |
|-----------------|-----------------|-------|--------|-------|
| branches        | `branch_count`  | 16    | 64     | `if` + `?:` |
| texture reads   | `tex_reads`     | 4     | 32     | static call sites |
| loop bound      | `loop_max_iter` | 64    | 512    | `-1` (non-constant bound) scores 0.5 |
| precision       | `precision_default` | highp/mediump = 1, lowp = 0.5, none = 0 |
| ALU estimate    | `estimated_alu` | 4000  | 16000  | cost table below |

### Cost table (`static.DefaultWeights`, EVALUATOR.md)

| Op | Cost |
|----|------|
| `+ - *` (and compound) | 1 |
| `/` | 4 |
| sqrt, inversesqrt, length, normalize, distance | 4 |
| exp, exp2, log, log2 | 4 |
| sin, cos, tan, asin, acos, atan, sinh, cosh, tanh | 8 |
| pow | 8 |
| texture* call | 100 |
| discard | 50 |
| branch (`if`, `?`) | 2 |

Costs inside loops are multiplied by the product of enclosing loop trip counts.
Trip counts are resolved from literals, `#define NAME N` and `const int|float NAME = N;`.
Non-constant bounds, `while` and `do` loops use multiplier 16 and set `loop_max_iter = -1`.

Known limitations (token-level analysis, no AST): user function bodies are costed once
(calls are not inlined), vector width is ignored, unary minus counts as subtraction.

### Calibration

No on-device timings exist yet, so 2026.10.1 is calibrated only against the known-good
MirrorBooth production filters (24 shaders in `MirrorBooth/mirrorbooth/shaders`):
all must compile, pass the gate and be deterministic across runs.

```bash
cd evaluator
EVALUATOR_CORPUS_DIR=/path/to/MirrorBooth/mirrorbooth/shaders \
  go test ./internal/api -run CorpusCalibration -v
```

Result for 2026.10.1: all 24 pass; composite 98.28–100 (static only); estimated_alu 134–3457;
heaviest are `filter_oil` (3457) and `filter_glow` (3116), both with 5x5 texture kernels.
Thresholds were set so that every production filter keeps code_quality >= 9.

Runtime (EGL, Mesa llvmpipe in the `test` Docker stage, fast config 60 frames @ 1080x1920,
MirrorBooth uniforms with semantics camera/resolution/time): all 24 pass, no NaN/Inf, no black
or constant frames; llvmpipe median 4.9–73 ms/frame (informational only, `software_renderer`).
Heaviest request (`filter_glow`) takes 4.6 s wall on llvmpipe, under the 8 s budget.
Hardware (NVIDIA GB10) timings and the `perf_target` thresholds are not yet validated.

### Runtime measurement rules

- Frame time = CPU wall time around draw + `glFinish` (no GPU timer queries; core GLES 3.0
  has none). Warm-up discarded: first 10 frames when > 20 rendered, otherwise the first.
  `first_frame_ms` is frame 0.
- Render target RGBA32F (`GL_EXT_color_buffer_float` required) so NaN/Inf survive readback.
- Per frame: 128x128 nearest-neighbour sample for NaN/Inf; full last frame checked too.
- Black frame: every RGB channel <= 1/255 on the full frame (sample only pre-filters).
- Constant frame: only for shaders with an active `time` uniform; full-frame hash equals the
  frame 30 frames earlier (0.5 s at 60 fps).
- Camera samplers get a deterministic 512x512 RGBA8 gradient + checkerboard image.
- Shaders with a non-constant loop bound skip runtime (`skipped_reason: unbounded_loop`):
  a hung draw call cannot be interrupted and would stall the single GL context.

Next step: device timings (Snapdragon class phone) for a subset → regression on cost weights.

## History

| Version   | Change |
|-----------|--------|
| 2026.10.1 | Static cost model + code_quality; runtime components; normalization over evaluated components |
| 2026.10-compile | Compile-only MVP: ok = 100, fail = 0 |

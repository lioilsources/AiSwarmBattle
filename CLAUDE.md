# AiSwarmBattle — CLAUDE.md

## Overview

AI swarm orchestration system. Go workspace with three modules (`evaluator`, `orchestrator`, `pkg`) plus Python pipeline servers. Each pipeline is a LangGraph-based FastAPI service that an orchestrator routes tasks to via LiteLLM.

Currently active pipeline: `mirrorbooth_retrieval` — shader/style retrieval for MirrorBooth.

## Structure

```
go.work                             # Go workspace (evaluator + orchestrator + pkg)

evaluator/                          # Go HTTP service :8081 — deterministic shader scoring (no LLM)
  cmd/evaluator/                    # thin main: config, EGL init, HTTP server
  internal/preprocess/              # Flutter/Impeller GLSL -> GLSL ES 3.0 (line-preserving)
  internal/glslang/                 # glslangValidator exec: compile gate + OpenGL SPIR-V
  internal/static/                  # token-level metrics + ALU cost model
  internal/egl/                     # headless EGL/GLES 3.0 profiler (cgo, linux only)
  internal/score/                   # versioned composite scoring (docs/scoring_rubric.md)
  internal/api/                     # POST /evaluate, GET /healthz
  Dockerfile                        # arm64 image, pinned glslang, mesa llvmpipe fallback

orchestrator/
  mirrorbooth/                      # Go HTTP server — MirrorBooth orchestration entry point
  internal/

pkg/
  go.mod                            # Shared Go types
  types/

pipelines/
  mirrorbooth_retrieval/            # Python: LangGraph retrieval pipeline (FastAPI)
    server.py                       # FastAPI app, /retrieve endpoint
    graph.py                        # LangGraph pipeline definition
    state.py                        # State types (RetrievalState, Constraints, ShaderRequest)
    nodes/                          # Individual LangGraph nodes
    prompts/                        # Prompt templates
    pyproject.toml
```

## Running

### Python pipeline (mirrorbooth_retrieval)

```bash
cd pipelines/mirrorbooth_retrieval

# Install
pip install -e .

# Required env vars
export LITELLM_URL=http://localhost:4000
export EMBED_URL=http://localhost:8005/v1
export LITELLM_API_KEY=dummy   # optional

# Run
python -m uvicorn server:app --host 0.0.0.0 --port 8095
```

### Go orchestrator (mirrorbooth)

```bash
cd orchestrator
go build ./mirrorbooth/...
./mirrorbooth
```

### Evaluator

```bash
cd evaluator
go test ./...                       # needs glslangValidator on PATH (brew install glslang)
go run ./cmd/evaluator              # :8081; EGL only on linux+cgo, elsewhere runtime is skipped

# Full test incl. EGL runtime (llvmpipe) in Linux container, from repo root:
docker build -f evaluator/Dockerfile --target test .

# Calibration against known-good shaders (compile, gate, determinism):
EVALUATOR_CORPUS_DIR=/path/to/MirrorBooth/mirrorbooth/shaders go test ./internal/api -run CorpusCalibration -v
```

Deploy: `swarm-evaluator` service (profile `evaluator`) in AiStack `deploy/docker-compose.swarm.yaml`.

| Var | Default | Description |
|-----|---------|-------------|
| `EVALUATOR_ADDR` | `:8081` | listen address |
| `GLSLANG_VALIDATOR` | `glslangValidator` | glslang binary |
| `EVALUATOR_EGL` | `auto` | `auto` (GPU first) / `software` (llvmpipe first) / `off` |
| `EVALUATOR_RUNTIME_BUDGET_MS` | `8000` | render time cap per request (client timeout is 12 s) |
| `EVALUATOR_MAX_COMPILES` | `10` | concurrent glslang runs |
| `EVALUATOR_LOG_FORMAT` | `json` | `json` / `text` |

## Env Vars (Python pipeline)

| Var | Description |
|-----|-------------|
| `LITELLM_URL` | LiteLLM gateway (e.g. `http://192.168.88.66:8080`) |
| `EMBED_URL` | Embedding model endpoint |
| `LITELLM_API_KEY` | Optional, defaults to `dummy` |
| `EVALUATOR_URL` | Evaluator service, default `http://dgx-spark:8081` |

## Adding a New Pipeline

1. Create `pipelines/<name>/` with `server.py` (FastAPI), `graph.py` (LangGraph), `state.py`
2. Register in AiStack `docker-compose.swarm.yaml` if running in Docker
3. Add orchestrator routing in `orchestrator/`

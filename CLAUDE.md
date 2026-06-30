# AiSwarmBattle — CLAUDE.md

## Overview

AI swarm orchestration system. Go workspace with two modules (`orchestrator`, `pkg`) plus Python pipeline servers. Each pipeline is a LangGraph-based FastAPI service that an orchestrator routes tasks to via LiteLLM.

Currently active pipeline: `mirrorbooth_retrieval` — shader/style retrieval for MirrorBooth.

## Structure

```
go.work                             # Go workspace (orchestrator + pkg)

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

## Env Vars (Python pipeline)

| Var | Description |
|-----|-------------|
| `LITELLM_URL` | LiteLLM gateway (e.g. `http://192.168.88.66:8080`) |
| `EMBED_URL` | Embedding model endpoint |
| `LITELLM_API_KEY` | Optional, defaults to `dummy` |

## Adding a New Pipeline

1. Create `pipelines/<name>/` with `server.py` (FastAPI), `graph.py` (LangGraph), `state.py`
2. Register in AiStack `docker-compose.swarm.yaml` if running in Docker
3. Add orchestrator routing in `orchestrator/`

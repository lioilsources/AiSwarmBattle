# ShaderBattle — Master Plán

Battle pipeline pro generování foto-realistic mobile shaderů (GLSL ES 3.0) z knowledgebase ShaderToys, srovnávající dva přístupy: Kimi Agent Swarm vs. LangChain hybrid (Qwen + Nemotron + Phi).

**Primární metrika:** Code quality — kompilace, žádné artefakty, robustnost.
**Target:** GLSL ES 3.0 pro mobile (Flutter/SMPL-16 ekosystém).
**Hardware:** NVIDIA DGX Spark (GB10 Blackwell, 128 GB UMA, ARM64).
**Auxiliary:** NAS (Ubuntu, ~18 TB RAID 10) pro storage a orchestraci.

---

## Fázování

### Fáze 1 — Ingest + Knowledgebase (týden 1-2)

- ShaderToy scraper (Go, NAS)
- Desktop GLSL → GLSL ES 3.0 translator (tree-sitter-glsl AST)
- Static validator (glslang headless)
- Metric extractor (LOC, branches, tex_reads, ALU ops)
- Semantic enrichment přes Qwen3-32B (description + tags)
- Embeddings přes e5-mistral-7b-instruct
- ChromaDB + SQLite metadata
- Function-level decomposition (knowledge graph)

**Detail:** viz `INGEST_PIPELINE.md`

### Fáze 2 — Evaluator (týden 2-3)

- EGL headless renderer (Go + cgo)
- glslang validator wrapper
- Static analyzer (AST cost model)
- Runtime profiler (GPU timer queries)
- Fuzz harness (property-based)
- Composite scoring engine
- HTTP API pro battle orchestrator

**Detail:** viz `EVALUATOR.md`

### Fáze 3 — Pipelines + Battle (týden 3-5)

- Pipeline A: Kimi K2 (přes Moonshot API) agent swarm
- Pipeline B: LangChain LangGraph (Qwen + Nemotron + Phi lokál)
- Battle orchestrator (Go, NAS)
- ELO + bootstrap statistics
- Prompt suite versioning
- Grafana telemetry

---

## Architektura

```
┌──────────────────────────────────────────────────────────────┐
│ NAS (Ubuntu, Celeron)                                        │
│  - shadertoy-ingest (Go)                                     │
│  - battle-orchestrator (Go)                                  │
│  - SQLite (metadata, results)                                │
│  - Grafana                                                   │
└────────────┬─────────────────────────────────────────────────┘
             │ HTTP/2 (Cloudflare Tunnel internal)
             │
┌────────────▼─────────────────────────────────────────────────┐
│ DGX Spark (GB10, 128 GB UMA, ARM64)                          │
│                                                              │
│  Resident services:                                          │
│   - LiteLLM gateway              :4000                       │
│   - Qwen3-32B (vLLM)             :8002  Pipeline B router    │
│   - Phi-4-14B (vLLM)             :8004  Pipeline B coder     │
│   - e5-mistral embedder          :8005                       │
│   - ChromaDB                     :8006                       │
│   - Evaluator (Go + EGL)         :8081                       │
│                                                              │
│  On-demand swap (jeden v čase, max ~28 GB):                  │
│   - Nemotron-Nano-49B (Q4)       :8003  Pipeline B reasoning │
│                                                              │
│  External (API):                                             │
│   - Kimi K2 (Moonshot)                  Pipeline A swarm     │
└──────────────────────────────────────────────────────────────┘
```

## Memory budget na DGX Spark

| Komponenta              | VRAM (GB) | Status      |
|-------------------------|-----------|-------------|
| Qwen3-32B Q5_K_M        | ~22       | resident    |
| Phi-4-14B Q5_K_M        | ~10       | resident    |
| e5-mistral-7b-instruct  | ~5        | resident    |
| ChromaDB index          | ~3        | resident    |
| Evaluator EGL ctx       | ~2        | resident    |
| KV cache + headroom     | ~30       | resident    |
| **Resident total**      | **~72**   |             |
| Nemotron-Nano-49B Q4    | ~28       | on-demand   |
| **Peak**                | **~100**  | OK na 128GB |

## Repo struktura

```
shaderbattle/
├── AGENTS.md                       # Claude Code instrukce
├── OPUS_PLAN.md                    # tento dokument
├── INGEST_PIPELINE.md              # detail fáze 1
├── EVALUATOR.md                    # detail fáze 2
├── README.md
├── go.work                         # multi-module workspace
│
├── ingest/                         # Go module
│   ├── cmd/
│   │   ├── scraper/                # ShaderToy API scraper
│   │   ├── translator/             # GLSL → ES 3.0
│   │   ├── enricher/               # LLM popisy + tagy
│   │   └── indexer/                # ChromaDB + SQLite
│   ├── internal/
│   │   ├── shadertoy/              # API klient
│   │   ├── glsl/                   # AST + transformace
│   │   ├── translate/              # ES 3.0 transformační pravidla
│   │   ├── metrics/                # statická analýza
│   │   └── store/                  # SQLite + ChromaDB
│   └── go.mod
│
├── evaluator/                      # Go module (cgo)
│   ├── cmd/evaluator/
│   ├── internal/
│   │   ├── egl/                    # EGL context + render
│   │   ├── glslang/                # cgo wrapper
│   │   ├── static/                 # cost model
│   │   ├── fuzz/                   # property-based testing
│   │   └── score/                  # composite scoring
│   ├── csrc/                       # C bridge pro EGL/glslang
│   └── go.mod
│
├── orchestrator/                   # Go module
│   ├── cmd/battle/
│   ├── internal/
│   │   ├── pipelines/              # A/B dispatch
│   │   ├── suite/                  # prompt suite + versioning
│   │   ├── stats/                  # ELO + bootstrap
│   │   └── store/
│   └── go.mod
│
├── pipelines/
│   ├── kimi_swarm/                 # Python, Pipeline A
│   │   ├── agents/
│   │   │   ├── curator.py
│   │   │   ├── architect.py
│   │   │   ├── coder.py
│   │   │   ├── critic.py
│   │   │   └── optimizer.py
│   │   ├── blackboard.py           # Redis pub/sub
│   │   ├── server.py               # FastAPI
│   │   └── pyproject.toml
│   │
│   └── langchain_hybrid/           # Python, Pipeline B
│       ├── graph.py                # LangGraph state machine
│       ├── nodes/
│       │   ├── qwen_router.py
│       │   ├── nemotron_reasoner.py
│       │   ├── phi_coder.py
│       │   └── qwen_verifier.py
│       ├── server.py
│       └── pyproject.toml
│
├── prompts/                        # versioned prompt suite
│   ├── v1/
│   │   ├── geometric_sdf.jsonl
│   │   ├── procedural_texture.jsonl
│   │   └── post_processing.jsonl
│   └── manifest.json               # hash + git tag
│
├── deploy/
│   ├── docker-compose.dgx.yml
│   ├── docker-compose.nas.yml
│   ├── caddy/
│   ├── litellm/
│   └── grafana/
│
└── docs/
    ├── glsl_es_translation.md
    ├── scoring_rubric.md
    └── runbook.md
```

## Klíčová rozhodnutí

1. **Go pro infra, Python pro LLM orchestraci.** Go pro ingest, evaluator, orchestrator (typesafe, fast, deploy-friendly). Python pro pipelines protože LangChain/LangGraph je tam.

2. **Kimi přes API.** Plné Kimi K2 (~1T) nesedne na Spark ani Q4. Použij Moonshot API. Pipeline A = "frontier swarm", Pipeline B = "local hybrid". Battle pak ukazuje hodnotu lokálního stacku.

3. **Tree-sitter-glsl pro translator.** Robustní AST parsing, ne regex. Translator je největší engineering effort fáze 1.

4. **Function-level decomposition v KB.** Ne ukládat jen celé shadery, ale extrahovat reusable funkce. Vyhne se plagiátu a umožní composition.

5. **Evaluator je golden source of truth.** Code quality je primárka → static + runtime + fuzz analýza musí být deterministická a verzovaná.

6. **Multi-module Go workspace.** `go.work` pro ingest / evaluator / orchestrator. Sdílené packages přes `pkg/`.

7. **Statistická signifikance povinná.** Bootstrap CI na ELO, ne pouhé „A vyhrál 28:22".

## Externí závislosti

- **ShaderToy API key** — registrace na shadertoy.com
- **Moonshot AI API key** — pro Pipeline A
- **NGC / NIM** — Qwen3, Phi-4, Nemotron-Nano kontejnery
- **glslang** — Khronos GLSL validator (system package nebo build)
- **tree-sitter-glsl** — parser grammar
- **e5-mistral-7b-instruct** — HuggingFace

## Risk register

| Risk                                          | Mitigation                                          |
|-----------------------------------------------|-----------------------------------------------------|
| Translator success rate <50%                  | Iterace, manual rules pro top 20 failure modes      |
| Kimi API rate limity / cost                   | Cache responses per prompt hash, batch off-peak     |
| EGL na ARM64 + Blackwell driver issues        | Fallback na llvmpipe SW renderer pro CI             |
| Nemotron-Nano swap latency >30s               | Pre-warm při zahájení batche, sequential schedule   |
| ShaderToy ToS / license issues                | Filter retrieval podle license field, edit-distance |
| 50 promptů × 3 runs = málo dat                | Bootstrap, designed eval suite s vyváženými kategoriemi |

## Definition of Done — Fáze 1

- [ ] ≥80 % top-1000 ShaderToy shaderů ingested
- [ ] ≥50 % single-pass shaderů úspěšně přeloženo do GLSL ES 3.0
- [ ] ChromaDB obsahuje shader-level + function-level embeddings
- [ ] Translator failure modes dokumentované v `docs/translator_known_issues.md`
- [ ] Ingest je resumable (kill -9 friendly)

## Definition of Done — Fáze 2

- [ ] Evaluator vrátí kompletní `EvalResult` pod 30s pro typický shader
- [ ] Property-based fuzz harness s ≥10 vlastnostmi
- [ ] Statický cost model kalibrovaný na ≥20 manuálních benchmarcích
- [ ] HTTP API stabilní pod load testem (100 req sériově)
- [ ] Determinismus: stejný shader → stejné skóre (±1 % na perf)

## Definition of Done — Fáze 3

- [ ] Pipeline A a B mají identický I/O kontrakt
- [ ] Prompt suite v1: ≥50 promptů ve 3 kategoriích
- [ ] Battle orchestrator: paralelní dispatch, sériový evaluator
- [ ] Grafana dashboard: win rate, compile rate, ELO, $/shader
- [ ] Bootstrap CI na hlavních metrikách (95 % CI)
- [ ] Reproducibilní run: `make battle SUITE=v1 RUNS=3`

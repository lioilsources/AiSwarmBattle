# Architektura: AiSwarmBattle ↔ MirrorBooth

## Jak spolu spolupracují

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ NAS (Ubuntu)                                                                │
│                                                                             │
│  Go Management API  :8090          MirrorBooth Git repo                    │
│  (orchestrator/cmd/mirrorbooth)    /data/MirrorBooth/mirrorbooth/          │
│         │                                    ▲                             │
│         │ spouští CLI subprocess             │ integrator.py               │
│         ▼                                    │ zapíše .frag + dart         │
│  Python CLI                                  │                             │
│  mirrorbooth_retrieval/run.py ───────────────┘                             │
│         │                                                                   │
│         │ HTTP volání                                                       │
└─────────┼───────────────────────────────────────────────────────────────────┘
          │
          ▼ (přes Cloudflare Tunnel / interní síť)
┌─────────────────────────────────────────────────────────────────────────────┐
│ DGX Spark GB10 (128 GB UMA, ARM64)                                          │
│                                                                             │
│  LiteLLM :4001  →  swarm-nano     :8010  Nemotron-Nano-30B  (Router)      │
│                 →  swarm-coder    :8011  Phi-4-14B           (Adapter)     │
│                 →  swarm-rag      :8013  Nemotron-8B-UltraLong             │
│                 →  swarm-director :8012  Super-120B  (on-demand, fallback) │
│                                                                             │
│  TEI embedder   :8005  e5-mistral-7b-instruct                               │
│  ChromaDB       :8006  collection "shaders"  ◄── AiSwarmBattle KB         │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│ Dev machine (nebo NAS)                                                      │
│                                                                             │
│  MirrorBooth/pipeline/run.py  (generace přes LLM)                          │
│  MirrorBooth/pipeline/rag/ingest.py  (lokální ChromaDB)                    │
│  flutter build  →  MirrorBooth Flutter app                                 │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 1. Ingest — dva oddělené systémy

| | MirrorBooth `pipeline/rag/ingest.py` | AiSwarmBattle `bootstrap_ingest.py` |
|---|---|---|
| **Účel** | Napájí MirrorBooth generační pipeline | Napájí AiSwarmBattle retrieval pipeline |
| **Zdroj dat** | `mirrorbooth/shaders/*.frag` + `seed_shaders/*.glsl` | Stejné soubory (čte z MirrorBooth) |
| **Embedding model** | `all-MiniLM-L6-v2` (CPU, lokální) | `e5-mistral-7b-instruct` (TEI na SPARK) |
| **ChromaDB** | Lokální soubor `pipeline/rag/db/` | Remote HTTP SPARK :8006 |
| **Kolekce** | `glsl_shaders` | `shaders` |
| **Chunking** | Po funkcích | Celé shadery |
| **Kde běží** | Dev machine / NAS (bez GPU) | NAS CLI → TEI na SPARK |

**Budoucí třetí zdroj (Fáze 1, zatím neimplementováno):** `ingest/` Go pipeline pro ShaderToy API (~80k shaderů). Do té doby `bootstrap_ingest.py` postačuje pro ~46 shaderů.

---

## 2. AiSwarm — retrieval, ne generace

**AiSwarm je RETRIEVAL pipeline.** Nevytváří nové GLSL — hledá nejlepší existující shader v ChromaDB a adaptuje ho na Flutter/Impeller kontrakt.

**Flow shader → Flutter app:**

```
1. CLI / Go API:
   python -m mirrorbooth_retrieval.run \
     --prompt "dreamy neon bokeh" \
     --name "neon_bokeh" \
     --mirrorbooth-root /data/MirrorBooth/mirrorbooth

2. Pipeline (SPARK):
   Router (Nano) → RAG (ChromaDB) → Selector (Nano) → Adapter (static + Phi-4) → Verify

3. integrator.py zapíše na NAS / dev:
   mirrorbooth/shaders/filter_neon_bokeh.frag
   mirrorbooth/lib/core/mirror_filter.dart   (sentinel anchor insert)
   mirrorbooth/pubspec.yaml                   (shader asset entry)

4. Developer:
   cd MirrorBooth/mirrorbooth && flutter build apk --debug
```

Shader je **build-time asset** — Flutter ho zkompiluje přes Impeller. Žádný runtime HTTP download.

**MirrorBooth vlastní pipeline** (`pipeline/run.py`) naproti tomu **generuje** nové shadery přes LLM (style_architect → glsl_coder). Obě pipeline zapisují do `mirrorbooth/shaders/`, různými jmény.

---

## 3. Modely a SPARK

### AiSwarmBattle (vyžaduje SPARK)

| Model | HF ID | VRAM | Port | Kdy |
|---|---|---|---|---|
| L3 Router/Verifier | `nvidia/Nemotron-Nano-30B-A3B-NVFP4` | ~15 GB | 8010 | resident |
| L2 Coder/Adapter | `microsoft/Phi-4` | ~14 GB | 8011 | resident |
| L4 RAG | `nvidia/Llama-3.1-Nemotron-8B-UltraLong-4M-Instruct` | ~16 GB | 8013 | resident |
| Embed | `intfloat/e5-mistral-7b-instruct` (TEI) | ~5 GB | 8005 | resident |
| L1 Director | `nvidia/Nemotron-Super-120B-A12B-NVFP4` | ~60 GB | 8012 | on-demand |
| LiteLLM gateway | — | — | 4001 | resident |
| ChromaDB | — | ~3 GB | 8006 | resident |

**Resident VRAM (bez Directora): ~53 GB / 128 GB** ✓

### MirrorBooth pipeline (může běžet bez SPARKu)

| Komponenta | Model | Kde |
|---|---|---|
| LLM (style_architect, glsl_coder, ranker) | konfigurovatelný — teď `llama3` @ `192.168.88.66:8000` | SPARK nebo libovolný OpenAI-API server |
| Embedding | `all-MiniLM-L6-v2` | CPU, lokálně |
| ChromaDB | — | lokální soubor |
| glslangValidator | Khronos binary | dev machine |

---

## 4. Závislosti a kde poběží

### SPARK (DGX, docker-compose.swarm.yaml v `../AiStack/deploy/`)

```
swarm-nano      vllm/vllm-openai  Nemotron-Nano-30B
swarm-coder     vllm/vllm-openai  Phi-4
swarm-rag       vllm/vllm-openai  Nemotron-8B-UltraLong
swarm-director  vllm/vllm-openai  Super-120B        (profile: director)
swarm-embed     ghcr.io/huggingface/text-embeddings-inference
chromadb        chromadb/chroma
swarm-litellm   ghcr.io/berriai/litellm
```

### NAS (Ubuntu, bez GPU)

```
Go binary:        orchestrator/cmd/mirrorbooth  →  systemd service :8090
Python:           pipelines/mirrorbooth_retrieval  (CLI subprocess z Go)
MirrorBooth repo: /data/MirrorBooth  (cíl integratoru)
SQLite:           /data/orchestrator.db  (job tracking)
```

### Dev machine

```
Flutter SDK    →  flutter build apk/ipa
glslangValidator  →  MirrorBooth pipeline validator.py
Python venv    →  MirrorBooth pipeline/run.py (generace)
```

### Co ještě chybí / není nasazeno

| Závislost | Stav |
|---|---|
| `docker-compose.swarm.yaml` | popsáno v AGENTS.md, soubor v `../AiStack/` |
| Nemotron modely | `make download-nemotron` v AiStack |
| glslang ARM64 build | nutný pro Evaluator (Fáze 2) |
| EGL headless (Evaluator) | Fáze 2, neimplementováno |
| ShaderToy API key | pro Fáze 1 ingest |
| Moonshot API key | pro Pipeline A (Kimi K2 battle) |

---

## Kompletní flow end-to-end (aktuální stav)

```
INICIALIZACE (jednou):
  NAS: bootstrap_ingest.py
       → čte MirrorBooth/shaders/*.frag + pipeline/seed_shaders/*.glsl
       → TEI SPARK :8005 pro embedding (e5-mistral-7b)
       → ChromaDB SPARK :8006, kolekce "shaders"

PŘIDÁNÍ SHADERU (per-request):
  NAS: POST /api/v1/pipeline/run {"prompt": "neon bokeh", "name": "neon_bokeh"}
       → Go spustí subprocess: python -m mirrorbooth_retrieval.run
            │
            ├─ Router:      LiteLLM → swarm-nano → intent JSON
            ├─ RAG:         TEI embed → ChromaDB query → top-K shaders
            ├─ Selector:    swarm-nano → ranked list
            ├─ Adapter:     static regex + swarm-coder → Flutter GLSL
            ├─ Verify:      statické checky (bez LLM, <50 ms)
            └─ Integrator:  zapíše .frag + mirror_filter.dart + pubspec.yaml

  DEV: cd MirrorBooth/mirrorbooth && flutter build apk --debug
       → nový filter dostupný v app
```

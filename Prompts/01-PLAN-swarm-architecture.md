# SwarmBattle — Nemotron LLM Stack Architecture

> Plán pro deploy orchestrátor LLM vrstvy Pipeline B (LangGraph hybrid) na DGX Spark.
> Implementace žije v `../AiStack/deploy/docker-compose.swarm.yaml`.
>
> **Zastaralé (2026-11):** Plán počítá s rezidentním stackem na SPARKu („vždy running“) a ručním `make up-swarm`. Projekt je teď na SPARKu „cold“ — žádná okna ani kontejnery; GPU práce jen přes `spark-broker` (`sparkctl`). Odblokování: AiStack `PLAN-spark-scheduler.md` §6.

---

## Přehled modelu

Pipeline B používá 4tiered Nemotron stack. Všechny modely jsou explicitně testovány na **DGX Spark GB10 Blackwell (128 GB UMA, ARM64)**.

| Tier | Model HF ID | Velikost | Aktivní params | VRAM | Kontext | Role v LangGraph |
|------|-------------|----------|----------------|------|---------|------------------|
| **L1 Director** | `nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4` | 120B MoE | 12B | ~60 GB | 1M tok | Hluboký reasoning; architekt shaderu |
| **L2 Coder** | `microsoft/Phi-4` (default) | 14B dense | 14B | ~14 GB | 32K tok | Generuje GLSL ES 3.0 kód |
| **L3 Router** | `nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B-NVFP4` | 30B MoE | 3.5B | ~15 GB | 256K tok | Rychlý router + verifier |
| **L4 RAG** | `nvidia/Llama-3.1-Nemotron-8B-UltraLong-4M-Instruct` | 8B dense | 8B | ~16 GB | 4M tok | Retrieval z ShaderToy KB |
| **Embed** | `intfloat/e5-mistral-7b-instruct` (TEI) | 7B | — | ~5 GB | — | Embeddings → ChromaDB |

### Upgrade path pro Coder

Výchozí Phi-4 lze swapnout na `nvidia/Llama-3.3-Nemotron-Super-49B-v1` Q4 (~28 GB)
přes env var `HF_MODEL_SWARM_CODER`. MBPP 91.3%, SWE-Bench lepší než Phi-4 při větší paměťové náročnosti.

---

## Memory budget (DGX Spark 128 GB UMA)

```
Resident services (vždy running):
  L3 swarm-nano      Nano-30B-A3B-NVFP4           ~15 GB
  L2 swarm-coder     Phi-4-14B                    ~14 GB
  L4 swarm-rag       Nemotron-8B-UltraLong         ~16 GB
  Embed swarm-embed  e5-mistral-7b (TEI)           ~5 GB
  ChromaDB                                         ~3 GB
  Evaluator + OS overhead                          ~10 GB
  KV cache headroom                                ~15 GB
  ────────────────────────────────────────────────────────
  Resident total                                   ~78 GB

On-demand (docker-compose profile: director):
  L1 swarm-director  Super-120B-A12B-NVFP4        +60 GB
  ────────────────────────────────────────────────────────
  Peak total                                       ~98 GB  ✓ (pod 100 GB z OPUS_PLAN)

Upgrade (Nemotron-49B místo Phi-4):
  Peak s directorem                               ~112 GB  ✓
```

---

## LangGraph node → model mapping

```
[Prompt] ──→ swarm-nano (L3)          # Router: kategorizace promptu, retrieval query
               │
               ├──→ swarm-rag (L4)    # RAG Curator: top-k shaderů + funkce z ChromaDB
               │         │
               │    (context injected)
               │
               ├──→ swarm-director (L1, on-demand)  # Architect: dekompozice + plán
               │
               ├──→ swarm-coder (L2)  # Coder: generuje GLSL ES 3.0
               │
               └──→ swarm-nano (L3)   # Verifier: statická kontrola, feedback loop
                         │
                    [ShaderResponse]
```

**Reasoning mode:**
- `swarm-nano` a `swarm-director` podporují `enable_thinking` flag (on/off per request)
- Router/verifier: `enable_thinking=false` (latence < 500 ms)
- Director: `enable_thinking=true` (hluboká analýza, čas není kritický)

---

## Technické detaily

### vLLM verze

Všechny swarm services: `vllm/vllm-openai:v0.20.0` (Docker Hub).
Existující AiStack lab/dev zůstávají na `nvcr.io/nvidia/vllm:${VLLM_VERSION}`.

### Nano-30B custom parser

Model potřebuje `nano_v3_reasoning_parser.py` který **není** součástí vLLM obrazu.
Download skript ho stáhne z HuggingFace → `AiStack/deploy/parsers/nano_v3_reasoning_parser.py`.
Container ho dostane přes volume mount: `./parsers:/parsers:ro`.

### Super-120B DGX Spark specifika

NVFP4 na Blackwell vyžaduje specifické backendy:
```
VLLM_NVFP4_GEMM_BACKEND=marlin
VLLM_FLASHINFER_ALLREDUCE_BACKEND=trtllm
VLLM_USE_FLASHINFER_MOE_FP4=0
```

### Porty

| Service | Port | Popis |
|---------|------|-------|
| `swarm-nano` | 8010 | L3 Router/Verifier (vLLM OpenAI API) |
| `swarm-coder` | 8011 | L2 Coder (vLLM OpenAI API) |
| `swarm-director` | 8012 | L1 Director (vLLM OpenAI API, profile: director) |
| `swarm-rag` | 8013 | L4 RAG (vLLM OpenAI API) |
| `swarm-embed` | 8005 | Embeddings (TEI HTTP API) |
| `chromadb` | 8006 | Vector store (ChromaDB HTTP API) |
| `swarm-litellm` | 4001 | LiteLLM gateway (modely: swarm-nano/coder/director/rag) |

---

## Implementace v AiStack

```
AiStack/
├── deploy/
│   ├── docker-compose.swarm.yaml     # hlavní compose modul
│   ├── litellm_config_swarm.yaml     # LiteLLM aliasy
│   └── parsers/                      # nano_v3_reasoning_parser.py (gitignored)
├── scripts/
│   ├── download_nemotron.sh          # L1 + L3 + L4 + parser (~91 GB)
│   └── download_nemotron_coder.sh    # L2 Phi-4 nebo Nemotron-49B
└── .env.example                      # SWARM_* proměnné
```

### Makefile targety

```bash
make download-nemotron           # stáhne L1 + L3 + L4 + custom parser
make download-nemotron-coder     # stáhne Phi-4 (default) nebo Nemotron-49B Q4
make up-swarm                    # spustí resident stack (bez directora)
make up-swarm-director           # přidá L1 director service
make down-swarm-director         # odstaví director (uvolní ~60 GB)
```

---

## Pipeline B — LiteLLM přístup z Python kódu

```python
from openai import AsyncOpenAI

litellm = AsyncOpenAI(base_url="http://dgx-spark:4001/v1", api_key="dummy")

# Rychlý routing (reasoning off)
router_resp = await litellm.chat.completions.create(
    model="swarm-nano",
    messages=[...],
    extra_body={"chat_template_kwargs": {"enable_thinking": False}},
)

# Hluboká analýza (reasoning on)
director_resp = await litellm.chat.completions.create(
    model="swarm-director",
    messages=[...],
    extra_body={"chat_template_kwargs": {"enable_thinking": True}},
)

# Embeddings přes TEI (přímé volání, ne LiteLLM)
import httpx
embed_resp = httpx.post("http://dgx-spark:8005/embed", json={"inputs": [text]})
```

---

## Vztah k ostatním fázím

- **Fáze 1 (Ingest):** Enricher používá `swarm-nano` nebo existující `llm-dev` (Gemma-4-31B) pro description + tagy.
- **Fáze 2 (Evaluator):** Nezávislý Go service, žádné LLM v hot path.
- **Fáze 3 (Battle):** Pipeline B volá swarm přes LiteLLM `:4001`; Pipeline A volá Moonshot API (Kimi K2) přímo.

Viz `OPUS_PLAN.md` pro kompletní architektonický přehled.

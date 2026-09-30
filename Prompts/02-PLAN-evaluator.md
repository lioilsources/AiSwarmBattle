# 02-PLAN — Evaluator (Fáze 2) — zadání pro Opus

Cílový repozitář: **AiSwarmBattle** (tento repo). Nový Go modul `evaluator/` přidaný do `go.work`.
Deploy část (compose service) patří do `../AiStack/deploy/docker-compose.swarm.yaml`.
MirrorBooth repo se nemění.

Řiď se `Prompts/AGENTS.md` (jazyk, Go styl, git workflow). Plná API specifikace je
v `Prompts/EVALUATOR.md` — tento dokument ji upřesňuje podle skutečného stavu kódu.

---

## Kontext — co už existuje (Fáze 0, hotovo)

- `pipelines/mirrorbooth_retrieval/` — LangGraph retrieval pipeline
  (router → rag → selector → adapter → **evaluator gate** → fallback).
- `orchestrator/cmd/mirrorbooth/` — Go management API `:8090`, spouští pipeline jako CLI subprocess.
- `pipelines/mirrorbooth_retrieval/integrator.py` — instalace `.frag` do MirrorBooth Flutter app.
- `pipelines/mirrorbooth_retrieval/bootstrap_ingest.py` — naplnění ChromaDB (~46 shaderů).
- `pkg/types/contract.go` — sdílené typy včetně `EvalRequest` / `EvalResult` / `PassesGate()`.

**Evaluator služba neexistuje.** Graf ji ale volá v hot pathu
(`nodes/evaluator.py` → `POST {EVALUATOR_URL}/evaluate`), takže bez ní žádný kandidát
neprojde gate a každý běh končí ve fallbacku. Evaluator je blokující další krok.

Pozor: `Prompts/OPUS_PLAN.md` je původní master plán a je částečně zastaralý — viz sekce
„Co z OPUS_PLAN.md už neplatí" na konci. Aktuální architektura: `Prompts/01-ARCHITECTURE.md`.

## Cíl

Go HTTP služba `evaluator` běžící na DGX Spark `:8081`:
deterministicky zkompiluje GLSL, spočítá statické metriky, změří runtime (EGL headless)
a vrátí `EvalResult` s composite skóre. Žádné LLM, žádné heuristiky bez kalibrace
(viz „Pravidla pro evaluator" v AGENTS.md).

## Závazný kontrakt

Zdroj pravdy je `pkg/types/contract.go` + skutečný klient `nodes/evaluator.py`. Klient posílá
`_FAST_CONFIG` (60 frames, 1080×1920, bez fuzz) s **httpx timeout 12 s** — fast config musí
odpovědět dřív. Gate: `compile.ok && score.composite >= 40 && !nan_inf && median_frame_ms <= 8`
(implementováno shodně v `PassesGate()` i v Pythonu).

### Známé nekonzistence — vyřešit v rámci této fáze, v jednom PR s oběma stranami

1. **`spirv_b64` chybí v kontraktu.** `nodes/evaluator.py` a `server.py` čtou
   `compile.spirv_b64`, ale `CompileResult` v contract.go to pole nemá. Přidat
   `SpirvB64 string \`json:"spirv_b64"\`` do `CompileResult` a evaluator ho musí vracet.
2. **Uniforms v `_EVAL_UNIFORMS` neodpovídají realitě.** Klient posílá
   `uCameraTexture`/`uMouse`, ale adapter generuje preamble s `uTexture`/`uResolution`/`uTime`
   (viz `MirrorBoothPreamble*` v contract.go). Opravit `_EVAL_UNIFORMS` na skutečný kontrakt;
   evaluator má uniforms brát z requestu, ne hardcodovat.
3. **Vstup je Flutter/Impeller GLSL, ne čisté ES 3.0.** Shadery začínají
   `#include <flutter/runtime_effect.glsl>`, bez `#version`, používají `FlutterFragCoord()`.
   glslang tohle nezkompiluje přímo — evaluator musí před kompilací aplikovat kompatibilní
   preprocessing: odstranit include, injektovat `#version 300 es` + `precision highp float;`,
   nahradit `FlutterFragCoord()` za `gl_FragCoord` (vec4 → `.xy` sémantika zůstává).
   Preprocessing je deterministický a verzovaný spolu s cost modelem.

### Chybějící wiring — opravit hned v prvním kroku

- `orchestrator/cmd/mirrorbooth/main.go`: `config` nemá `EvaluatorURL`; subprocess env
  předává jen `LITELLM_URL`, `EMBED_URL`, `CHROMA_URL`. Přidat
  `EVALUATOR_URL` (default `http://dgx-spark:8081`) do configu i do `cmd.Env`.
- `run.py` má `os.environ.get("EVALUATOR_URL", "")` — prázdný default znamená rozbité URL.
  Buď fail-fast s jasnou chybou, nebo stejný default jako v Go.

## Struktura modulu

```
evaluator/
├── cmd/evaluator/main.go        # tenký main: config, HTTP server
├── internal/
│   ├── api/                     # HTTP handlery, request validace
│   ├── preprocess/              # Flutter GLSL → ES 3.0 (bod 3 výše)
│   ├── glslang/                 # kompilace + SPIR-V (cgo, nebo exec glslangValidator)
│   ├── static/                  # LOC, branches, tex_reads, ALU cost model
│   ├── egl/                     # headless EGL/GLES 3.0 context, render loop
│   └── score/                   # composite scoring, verzované váhy
├── csrc/                        # C bridge, jen pokud cgo
└── go.mod
```

Přidat `./evaluator` do `go.work`. Sdílené typy pouze z `pkg/types`.

## Postup — inkrementálně, každý krok samostatně mergovatelný

### Krok 1 — skeleton + compile gate (MVP, odblokuje pipeline)

- HTTP server `:8081`, `POST /evaluate`, `GET /healthz`.
- Preprocessing (bod 3) + kompilace přes `glslangValidator` (exec je pro MVP OK, cgo později).
- Response: plný `EvalResult` tvar; `runtime` prázdné, `score.composite` zatím jen
  z compile výsledku (ok = 100, fail = 0) — gate tím začne fungovat na compile úrovni.
- Wiring fixy (EVALUATOR_URL) + oprava nekonzistencí 1–2.
- DoD: end-to-end běh `POST :8090/api/v1/pipeline/run` projde gate a nainstaluje shader.

### Krok 2 — statická analýza + cost model

- Parsování bez LLM: LOC, function/branch/loop count, tex_reads, odhad ALU.
  Pro MVP stačí token-level analýza; tree-sitter-glsl až bude potřeba přesnost.
- `score.composite` = vážená kombinace compile + static (váhy v `internal/score`,
  verzované; změna vah = bump `cost_model_version` + update `docs/scoring_rubric.md`).
- DoD: skóre deterministické, kalibrované aspoň na shaderech z bootstrap ingestu (~46 ks).

### Krok 3 — EGL runtime profiler

- Headless EGL + GLES 3.0 context, single render goroutine (serializace přes channel).
- Render `test_duration_frames` snímků, měřit median/p95 frame ms, detekce NaN/Inf
  (readback sample), black/constant frame detekce.
- Fast config (60 frames, 1 rozlišení) musí skončit **< 10 s** kvůli klientskému timeoutu 12 s.
- Risk: EGL na ARM64 + Blackwell driver — fallback `llvmpipe` (SW render) pro CI a dev;
  perf čísla z llvmpipe označit v response (`cost_model_version` suffix), negatovat gate jen na NaN.
- DoD: determinismus ±1 % na perf metrikách, stejný shader → stejné skóre.

### Krok 4 — fuzz harness (odložitelné)

- Property-based: náhodné uniform hodnoty (seeded), žádný crash/NaN.
- Klient teď posílá `skip_fuzz: true`, takže krok 4 neblokuje nic — implementovat
  až po nasazení kroků 1–3.

## Deploy (AiStack)

- Nový service `swarm-evaluator` v `docker-compose.swarm.yaml`, port `8081`,
  ARM64 image s glslang + EGL/mesa. Build stage v tomto repu (`evaluator/Dockerfile`).
- LiteLLM se netýká — evaluator je přímé HTTP, bez gateway.

## Mimo scope této fáze

- Battle orchestrator, ELO, Grafana (Fáze 3 — `ORCHESTRATOR.md`).
- ShaderToy ingest Go pipeline (Fáze 1 — `INGEST_PIPELINE.md`); bootstrap_ingest zatím stačí.
- Director fallback (generace přes swarm-director) — `fallback_node` zůstává stub.
- LLM judge vrstva — nikdy nesmí být součástí primárního skóre.

## Co z OPUS_PLAN.md už neplatí

| OPUS_PLAN.md (červen) | Skutečnost |
|---|---|
| Repo `shaderbattle/` | Repo je **AiSwarmBattle**, jiný layout (viz CLAUDE.md) |
| Modely Qwen3-32B / Phi-4 / Nemotron-49B swap | Nemotron stack: Nano-30B, Phi-4, UltraLong-8B, Super-120B (viz `01-PLAN-swarm-architecture.md`) |
| LiteLLM `:4000` | `:4001`; TEI `:8005`, ChromaDB `:8006` na `dgx-spark` |
| Pipeline B = `langchain_hybrid` generační | Realizována jako `mirrorbooth_retrieval` — **retrieval, ne generace** |
| Fáze 1 ingest jako předpoklad | Přeskočeno; bootstrap_ingest.py stačí, Fáze 1 až po evaluatoru |
| Kimi swarm (Pipeline A) aktivní | Odloženo do Fáze 3 |

Sekce OPUS_PLAN.md, které platí dál: klíčová rozhodnutí 1, 3–7, risk register, DoD Fáze 2
(zde zpřesněno per krok).

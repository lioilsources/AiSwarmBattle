# ORCHESTRATOR.md — Fáze 3 detail

Cíl: na základě textového popisu efektu vrátit MirrorBooth Flutter aplikaci validovaný GLSL ES 3.0 shader
z ShaderToy knowledgebase — s MirrorBooth uniform kontraktem, SPIR-V blobem, a performance zárukou.

**Primární mód: retrieval, ne generování.** Swarm LLM slouží k pochopení intentu, formulaci RAG dotazů,
výběru nejlepšího kandidáta a adaptaci uniforms — ne k psaní nového GLSL od nuly. Generování nastupuje
pouze jako poslední záchrana a výsledek se vrací s flagem `source: "generated"`.

---

## Kontext a omezení

- **Volající**: MirrorBooth Flutter app (nebo její backend, ne přímo device)
- **Latence**: retrieval + adaptace musí proběhnout pod **15 s** (uživatel čeká na efekt)
- **Flutter shader kontrakt**: GLSL ES 3.0 kompilovaný do SPIR-V, uniformy setované pozičně
- **Mobile target**: shader musí projít evaluatorem s `perf_target` score > 0 (median frame < 8 ms na 1080×1920)
- **Realtime v shaderu**: iTime animace, kamera přes sampler2D — ne statické efekty
- **Licence**: jen shadery kde `license IN ('CC-BY', 'CC-BY-NC', 'public')` (viz SQLite schema)

---

## I/O kontrakt (MirrorBooth ↔ Orchestrátor)

Definovaný v `pkg/types/contract.go`. Změna vyžaduje synchronní update obou stran v jednom PR.

### `POST /api/v1/shader/request`

```json
{
  "request_id": "uuid-v7",
  "prompt":     "dreamy neon bokeh with slow color shift, camera passthrough",
  "constraints": {
    "max_mobile_cost":   150,
    "require_camera":    true,
    "require_animated":  true,
    "aspect_ratio":      "portrait",
    "allow_source":      ["retrieved", "adapted", "generated"]
  },
  "seed": 42
}
```

| Pole | Popis |
|------|-------|
| `prompt` | Přirozený text, max 500 znaků |
| `max_mobile_cost` | Horní mez `est_mobile_cost` z SQLite metrics. Default 200. |
| `require_camera` | Pokud true, shader musí používat `uCameraTexture` |
| `require_animated` | Pokud true, `motion` tag musí být `animated` nebo `reactive` |
| `aspect_ratio` | `portrait` (default) nebo `landscape` — ovlivňuje evaluátor resolution |
| `allow_source` | Whitelist zdrojů výsledku. Default vše. Pro produkci: `["retrieved", "adapted"]`. |
| `seed` | Propaguje se do RAG top-k ranking i LLM calls |

### Response (200 OK)

```json
{
  "request_id": "uuid-v7",
  "shader_id":  "XlsSzX",
  "title":      "Neon Bokeh Dreams",
  "source":     "adapted",

  "glsl_es":    "#version 300 es\n...",
  "spirv_b64":  "<base64 SPIR-V blob>",

  "uniforms": [
    {"index": 0, "name": "uResolution",    "type": "vec2",      "semantic": "resolution"},
    {"index": 1, "name": "uTime",          "type": "float",     "semantic": "time"},
    {"index": 2, "name": "uCameraTexture", "type": "sampler2D", "semantic": "camera"}
  ],

  "eval_score":   83.2,
  "eval_version": "0.3.1",
  "mobile_cost":  112,

  "retrieval": {
    "candidates_found": 7,
    "selected_rank":    1,
    "similarity_score": 0.91
  },

  "provenance": {
    "original_id":   "XlsSzX",
    "shadertoy_url": "https://www.shadertoy.com/view/XlsSzX",
    "license":       "CC-BY-NC",
    "author":        "iq"
  },

  "latency_ms": 4821
}
```

### Error responses

```json
{ "error": "no_candidates", "message": "RAG found 0 shaders matching constraints", "request_id": "..." }
{ "error": "eval_fail",     "message": "best candidate scored 0 (compile fail)", "request_id": "..." }
{ "error": "timeout",       "message": "pipeline exceeded 15s budget", "request_id": "..." }
```

---

## MirrorBooth uniform kontrakt

Každý shader opouštějící orchestrátor musí mít **přesně tyto uniformy v přesně tomto pořadí**:

| Index | GLSL název       | Typ        | Sémantika                              | ShaderToy ekvivalent |
|-------|------------------|------------|----------------------------------------|----------------------|
| 0     | `uResolution`    | `vec2`     | Výstupní rozlišení v pixelech          | `iResolution.xy`     |
| 1     | `uTime`          | `float`    | Čas od spuštění v sekundách            | `iTime`              |
| 2     | `uCameraTexture` | `sampler2D`| RGBA kamera frame (YUV → RGBA Flutter) | `iChannel0`          |
| 3     | `uMouse`         | `vec2`     | Touch pozice [0,1] normalized (optional) | `iMouse.xy / iRes` |

Pokud původní shader `require_camera = false`, `uCameraTexture` je přesto deklarovaný (nezabírá binding slot) a shader ho ignoruje. Uniformní počet a pořadí musí být konstantní — Flutter nastavuje pozičně.

**GLSL preamble** (inject adapter vždy):

```glsl
#version 300 es
precision highp float;
precision highp int;

uniform vec2      uResolution;
uniform float     uTime;
uniform sampler2D uCameraTexture;
uniform vec2      uMouse;

out vec4 fragColor;
```

---

## Pipeline architektura

```
[MirrorBooth HTTP request]
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│ Go Orchestrator (NAS :8090)                                 │
│  - request validation + cache lookup                        │
│  - timeout budget tracking (15s)                            │
│  - result persistence (SQLite)                              │
└───────────────┬─────────────────────────────────────────────┘
                │ POST /pipeline/retrieve
                ▼
┌─────────────────────────────────────────────────────────────┐
│ Python Pipeline (DGX :8020) — LangGraph state machine       │
│                                                             │
│  [Router] → [RAG Curator] → [Selector] → [Adapter]         │
│                                    │                        │
│                              [Verifier] ←──────────────┐   │
│                                    │                    │   │
│                              [Evaluator call]           │   │
│                                    │                    │   │
│                         ok? → [Response]    fail? → [next candidate / fallback]
└─────────────────────────────────────────────────────────────┘
```

### LangGraph nodes

```python
# pipelines/mirrorbooth_retrieval/graph.py

builder = StateGraph(ShaderRetrievalState)
builder.add_node("router",    router_node)      # L3 Nano — intent + query
builder.add_node("rag",       rag_node)         # L4 UltraLong — ChromaDB retrieval
builder.add_node("selector",  selector_node)    # L3 Nano — rank candidates
builder.add_node("adapter",   adapter_node)     # L2 Phi-4 — uniform remapping
builder.add_node("verifier",  verifier_node)    # L3 Nano — final check
builder.add_node("evaluator", evaluator_node)   # Go evaluator HTTP call
builder.add_node("fallback",  fallback_node)    # L1 Director (on-demand) — generování

builder.set_entry_point("router")
builder.add_edge("router",   "rag")
builder.add_edge("rag",      "selector")
builder.add_edge("selector", "adapter")
builder.add_edge("adapter",  "verifier")
builder.add_edge("verifier", "evaluator")
builder.add_conditional_edges(
    "evaluator",
    route_after_eval,  # ok → END | next_candidate → selector | exhausted → fallback
)
builder.add_edge("fallback", "verifier")
```

---

## Stage 1 — Router (L3 Nano, thinking=False)

**Model**: `swarm-nano` (:8010), `enable_thinking=false`  
**Latence budget**: < 800 ms

Vstup: surový `prompt` + `constraints`  
Výstup: strukturovaný intent objekt

```python
class RouterOutput(BaseModel):
    chroma_query:    str        # dotaz pro e5-mistral embedding
    technique_tags:  list[str]  # z controlled vocabulary (raymarch, 2d, post_process, ...)
    visual_tags:     list[str]  # plasma, bokeh, abstract, ...
    motion_required: bool
    camera_required: bool
    complexity:      Literal["cheap", "medium", "expensive"]
    chroma_filter:   dict       # where clause pro ChromaDB
```

Systémový prompt (`pipelines/mirrorbooth_retrieval/prompts/router.txt`):
```
You analyze shader effect requests for a mobile camera app.
Output STRICT JSON matching RouterOutput schema.

chroma_query: natural language description optimized for semantic search
technique_tags: pick from [raymarch, 2d, post_process, particle, sdf, fractal, kaleidoscope]
visual_tags: pick from [plasma, fire, water, smoke, stars, geometry, abstract, organic, bokeh, neon]
motion_required: true if prompt implies animation
camera_required: true if prompt mentions camera, passthrough, overlay, reflection
complexity: map from max_mobile_cost (≤80→cheap, ≤150→medium, >150→expensive)

DO NOT explain. Output JSON only.
```

---

## Stage 2 — RAG Curator (L4 UltraLong, thinking=False)

**Model**: `swarm-rag` (:8013), `enable_thinking=false`  
**Latence budget**: < 2 s (ChromaDB query)

Vstup: `RouterOutput`  
Výstup: `top_k` shaderů (default k=10)

```python
async def rag_node(state: ShaderRetrievalState) -> ShaderRetrievalState:
    intent = state.router_output

    # 1. embed query přes TEI
    query_vec = await embed(intent.chroma_query)

    # 2. shader-level retrieval
    shader_results = await chroma_client.query(
        collection_name="shaders",
        query_embeddings=[query_vec],
        n_results=15,
        where={
            "$and": [
                {"compile_ok":        {"$eq": 1}},
                {"license":           {"$in": ["CC-BY", "CC-BY-NC", "public"]}},
                {"est_mobile_cost":   {"$lte": state.request.constraints.max_mobile_cost}},
                *([{"motion": {"$in": ["animated", "reactive"]}}] if intent.motion_required else []),
            ]
        }
    )

    # 3. function-level retrieval (pro fallback kompozici)
    fn_results = await chroma_client.query(
        collection_name="functions",
        query_embeddings=[query_vec],
        n_results=20,
        where={"compile_ok": {"$eq": 1}},
    )

    return state | {"shader_candidates": shader_results, "function_candidates": fn_results}
```

Pokud `shader_results` má méně než 3 výsledky → rozvolni filter (odstraň technique_tags constraint, ne mobile_cost ani license).

---

## Stage 3 — Selector (L3 Nano, thinking=False)

**Model**: `swarm-nano` (:8010), `enable_thinking=false`  
**Latence budget**: < 1 s

Vstup: `top_k` kandidátů ze stage 2 + původní prompt  
Výstup: seřazený seznam shader IDs + odůvodnění pro top 3

Nano dostane sumarizaci každého kandidáta (title, description, tags, score, mobile_cost) — ne celý GLSL kód. Vybírá na základě:
1. Semantická blízkost (similarity_score z ChromaDB)
2. Shoda technique/visual tagů
3. `eval_score` z ingest (vyšší = lepší jako základ)
4. `est_mobile_cost` (nižší = bezpečnější)

```python
class SelectorOutput(BaseModel):
    ranked_ids: list[str]     # shader IDs, best first, max 5
    reasoning:  list[str]     # per-item 1-sentence rationale
```

Selector nevidí GLSL — jen metadata. Cílem je rychlý re-rank, ne deep analysis.

---

## Stage 4 — Adapter (L2 Phi-4, thinking=False)

**Model**: `swarm-coder` (:8011), `enable_thinking=false`  
**Latence budget**: < 3 s

Vstup: vybraný shader GLSL ES 3.0 + MirrorBooth uniform kontrakt  
Výstup: adaptovaný GLSL + diff notes

Adapter provádí **mechanické přejmenování** — ne kreativní generování:

```python
class AdapterOutput(BaseModel):
    glsl_es:      str          # upravený shader
    diff_notes:   list[str]    # co se přejmenovalo/přidalo
    source:       Literal["retrieved", "adapted"]
    camera_wired: bool         # je uCameraTexture skutečně použit?
```

Transformace (v pořadí aplikace):

| Krok | Operace |
|------|---------|
| A01 | Nahradit preamble standardní MirrorBooth preamble |
| A02 | `iResolution.xy` → `uResolution`, `iResolution.z` → `1.0` |
| A03 | `iTime` → `uTime`, `iTimeDelta` → `0.016` (fixní fallback) |
| A04 | `iMouse.xy` → `uMouse * uResolution` |
| A05 | `iChannel0` → `uCameraTexture` (pokud `require_camera=true` a původní shader čte iChannel0) |
| A06 | Ostatní `iChannel[1-3]` → odstraň nebo nahraď konstantní barvou `vec4(0.5)` |
| A07 | `iFrame` → `int(uTime * 60.0)` (aproximace) |
| A08 | Odstraň nepoužité `iDate`, `iSampleRate`, `iChannelResolution` |

Pokud transformace A05 není proveditelná (shader nepoužívá iChannel0 smysluplně) a `require_camera=true` → `camera_wired=false` → selector přejde na dalšího kandidáta.

Adapter **nesmí** přidávat novou logiku, jen přejmenovat/odstraňovat. Pokud identifikuje, že transformace by zničila shader (iChannel0 je noise texture, ne camera input), vrátí `source="retrieved"` bez A05.

Systémový prompt (`pipelines/mirrorbooth_retrieval/prompts/adapter.txt`):
```
You adapt a GLSL ES 3.0 shader to MirrorBooth uniform contract.
Apply ONLY the mechanical renames listed in the task. Do NOT change shader logic.
Do NOT add new effects. Do NOT change visual output.
Output: full adapted GLSL + diff_notes list.
If a rename would break the shader semantics, skip it and note why.
```

---

## Stage 5 — Verifier (L3 Nano, thinking=False)

**Model**: `swarm-nano` (:8010), `enable_thinking=false`  
**Latence budget**: < 500 ms

Vstup: adaptovaný GLSL  
Výstup: `ok: bool`, `issues: list[str]`

Verifier kontroluje:
- Přítomnost `#version 300 es` na řádku 1
- Přítomnost všech 4 povinných uniforms
- Přítomnost `out vec4 fragColor`
- Absence `gl_FragColor` (ES 2.0 legacy)
- Absence `texture2D`, `textureCube` (ES 2.0 legacy)
- `void main()` přítomen

Toto jsou **statické text checks**, ne GLSL parsing. Rychlé a bez LLM (LLM jen failuje-li statická kontrola a shader je neopravitelný).

```python
def static_verify(glsl: str) -> VerifyResult:
    checks = [
        glsl.startswith("#version 300 es"),
        "uniform vec2 uResolution" in glsl,
        "uniform float uTime" in glsl,
        "uniform sampler2D uCameraTexture" in glsl,
        "out vec4 fragColor" in glsl,
        "void main()" in glsl,
        "gl_FragColor" not in glsl,
        "texture2D(" not in glsl,
    ]
    ...
```

Pokud static_verify selže → pošli LLM prompt na opravu (jeden retry, jinak next candidate).

---

## Stage 6 — Evaluator (Go HTTP, bez LLM)

**Service**: evaluator (:8081)  
**Latence budget**: < 8 s (viz EVALUATOR.md, median ~12s celkově — pro realtime path skip fuzz)

Pro MirrorBooth pipeline volej evaluator s redukovanou konfigurací:

```json
{
  "config": {
    "test_duration_frames": 60,
    "target_resolutions": [[1080, 1920]],
    "fuzz_runs": 0,
    "skip_fuzz": true,
    "skip_runtime": false
  }
}
```

`fuzz_runs=0` ušetří ~8s. Pro nightly battle běží full evaluace; pro realtime request stačí compile + 60 framů runtime.

**Gate podmínky** (alespoň jedno splněno pro úspěch):

| Podmínka | Hodnota |
|----------|---------|
| `compile.ok` | `true` |
| `runtime.median_frame_ms` @ 1080×1920 | `< 8 ms` |
| `runtime.nan_inf_detected` | `false` |
| `score.composite` | `> 40` |

Pokud gate nesplněn → selector přejde na dalšího kandidáta. Po vyčerpání kandidátů → fallback.

---

## Fallback — Director + generování (L1 Super-120B, thinking=True)

**Nastupuje jen pokud** všichni kandidáti selhali evaluatorem nebo RAG vrátil < 2 výsledky.  
**Model**: `swarm-director` (:8012, on-demand profile), `enable_thinking=true`  
**Latence budget**: bez limitu (uživatel dostane `202 Accepted`, výsledek doručen webhookem nebo polling)

Director dostane:
- Původní prompt + constraints
- Top-K function candidates z ChromaDB (composable building blocks)
- MirrorBooth uniform kontrakt
- Příklady 3 nejbližších shaderů (metadata + GLSL)

Director generuje nový shader komponováním existujících funkcí z KB. Výsledek jde přes standardní
Adapter → Verifier → Evaluator pipeline. `source: "generated"`.

Response pro fallback cestu:
```json
{ "status": "pending", "request_id": "...", "estimated_wait_s": 60, "poll_url": "/api/v1/shader/result/{id}" }
```

---

## Caching

**Prompt cache**: `sha256(prompt + json(constraints))` → uložen výsledek do SQLite.

```sql
CREATE TABLE shader_cache (
  cache_key     TEXT PRIMARY KEY,
  request_json  TEXT NOT NULL,
  response_json TEXT NOT NULL,
  shader_id     TEXT,
  created_at    INTEGER NOT NULL,
  hit_count     INTEGER DEFAULT 0
);
CREATE INDEX idx_cache_created ON shader_cache(created_at);
```

TTL: 7 dní. MirrorBooth posílá `seed` — pokud seed se liší, považuj za cache miss (testovací variace).

**GLSL cache** (per shader_id + adapter transform hash): pokud stejný shader byl already adaptován pro MirrorBooth, přeskoč Adapter + Evaluator stage a vrať cached GLSL.

---

## HTTP API (Go Orchestrator, NAS :8090)

```
POST   /api/v1/shader/request          Spustí pipeline, vrátí synchronně nebo 202
GET    /api/v1/shader/result/{id}      Poll pro async výsledky (fallback)
GET    /api/v1/shader/{shader_id}      Vrátí cached výsledek pro konkrétní ShaderToy ID
GET    /api/v1/healthz                 Status orchestrátoru + pipeline service
GET    /metrics                        Prometheus
```

**Timeout handling:**

```go
func (h *Handler) HandleRequest(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
    defer cancel()

    result, err := h.pipeline.Run(ctx, req)
    if errors.Is(err, context.DeadlineExceeded) {
        // pokud Director fallback ještě běží, vrátit 202
        // jinak vrátit best partial result s flagem
    }
}
```

---

## Flutter integrace (MirrorBooth strana)

### Shader načtení

```dart
// MirrorBooth: načíst shader přes orchestrátor
Future<FragmentShader> loadOrchestrated(String prompt) async {
  final resp = await http.post(
    Uri.parse('$orchestratorBase/api/v1/shader/request'),
    body: jsonEncode({'prompt': prompt, 'constraints': {'require_camera': true}, 'seed': 0}),
  );
  final data = jsonDecode(resp.body);
  
  // SPIR-V blob přijít jako base64
  final spirvBytes = base64Decode(data['spirv_b64']);
  
  // Flutter FragmentProgram z SPIR-V bytes
  final program = await FragmentProgram.fromBytes(spirvBytes);
  return program.fragmentShader();
}
```

### Uniform nastavení (per frame)

```dart
void applyUniforms(FragmentShader shader, Size size, double time, ui.Image cameraFrame) {
  shader.setFloat(0, size.width);   // uResolution.x
  shader.setFloat(1, size.height);  // uResolution.y
  shader.setFloat(2, time);         // uTime
  shader.setImageSampler(0, cameraFrame); // uCameraTexture (index 0)
  shader.setFloat(3, touchX);       // uMouse.x
  shader.setFloat(4, touchY);       // uMouse.y
}
```

Pořadí odpovídá uniform indexům z response `uniforms[]` array.

### SPIR-V kompilace

Evaluátor již produkuje SPIR-V ze stage compile (viz EVALUATOR.md). Orchestrátor ho vezme z evaluator response a zabalí do base64. Pro Flutter je nutný SPIR-V compiled s `--target-env opengl` (ne Vulkan) — tento flag musí být nastaven v evaluátoru pro MirrorBooth path.

```go
// orchestrator/internal/pipelines/mirrorbooth.go
func compileForFlutter(glsl []byte) ([]byte, error) {
    return glslang.Compile(glsl, glslang.StageFragment, glslang.TargetGLES300Flutter)
}
```

Přidat `TargetGLES300Flutter` target do evaluator glslang wrapperu (viz EVALUATOR.md `Target` enum).

---

## Go module struktura

```
orchestrator/
├── cmd/
│   ├── battle/          # existující battle runner
│   └── mirrorbooth/     # nový: HTTP server pro MirrorBooth API
│       └── main.go
├── internal/
│   ├── pipelines/
│   │   ├── dispatch.go         # A/B dispatch (battle)
│   │   └── mirrorbooth.go      # MirrorBooth pipeline client
│   ├── cache/
│   │   └── shader_cache.go     # SQLite prompt + GLSL cache
│   ├── suite/           # existující
│   ├── stats/           # existující
│   └── store/
│       └── results.go          # výsledky battle + mirrorbooth requestů
└── go.mod
```

Python pipeline:

```
pipelines/
├── kimi_swarm/                 # Pipeline A (nezměněno)
├── langchain_hybrid/           # Pipeline B — battle
└── mirrorbooth_retrieval/      # nový
    ├── graph.py                # LangGraph state machine
    ├── state.py                # ShaderRetrievalState
    ├── nodes/
    │   ├── router.py
    │   ├── rag.py
    │   └── adapter.py
    ├── verify.py               # statická verifikace (bez LLM)
    ├── prompts/
    │   ├── router.txt
    │   ├── selector.txt
    │   └── adapter.txt
    ├── server.py               # FastAPI :8020
    └── pyproject.toml
```

---

## Prometheus metriky

```
mirrorbooth_request_total{source="retrieved|adapted|generated", status="ok|fail"}
mirrorbooth_latency_seconds_bucket{stage="router|rag|selector|adapter|eval"}
mirrorbooth_cache_hits_total
mirrorbooth_candidates_tried_histogram
mirrorbooth_eval_score_histogram
```

---

## Performance budget (worst case, bez cache)

| Stage | Model | Latence |
|-------|-------|---------|
| Router | Nano (thinking=off) | ~600 ms |
| RAG embed + ChromaDB | e5-mistral + HTTP | ~800 ms |
| Selector | Nano (thinking=off) | ~600 ms |
| Adapter | Phi-4 | ~2 000 ms |
| Static verify | Python, bez LLM | ~50 ms |
| Evaluator (60 frames, no fuzz) | Go EGL | ~3 000 ms |
| Orchestrator overhead | Go | ~200 ms |
| **Total** | | **~7 250 ms** |

Pod 15s headroomem i pro 2 pokusy (druhý kandidát při prvním eval failu).

---

## Test plán

### Unit testy

- `static_verify` na 10 validních a 10 nevalidních GLSL snippetů
- `adapter_node` na 5 golden shaderů: input ShaderToy ES 3.0 → expected MirrorBooth GLSL
- `cache.Get/Set` roundtrip
- `selector_node` mock: 5 kandidátů s různými scores → správné pořadí

### Integration testy

- 10 promptů → full pipeline bez fallbacku → response < 15 s, `compile.ok = true`
- 3 "impossible" prompty → fallback Director path → `source: "generated"`, HTTP 202
- Cache hit test: stejný prompt 2× → druhý request pod 500 ms

### Kontrakt test (MirrorBooth side)

- Každý response musí projít `uniforms` validation (4 záznamy, správné indexy)
- `spirv_b64` musí být validní SPIR-V (magic number `0x07230203`)
- Flutter `FragmentProgram.fromBytes` nesmí házet exception

### Load test

- 20 concurrent requestů (různé prompty) → bez deadlocku, NAS orchestrátor stabilní
- Měř cache hit ratio po 100 requestech (očekávej > 30% — MirrorBooth má opakované efekty)

---

## Definition of Done

- [ ] `POST /api/v1/shader/request` vrátí validní response pod 15s pro ≥ 90% testovacích promptů
- [ ] Všechny vrácené shadery mají `compile.ok = true` a `score.composite > 40`
- [ ] MirrorBooth uniform kontrakt dodržen (4 uniformy, správné indexy)
- [ ] SPIR-V blob načitatelný Flutter `FragmentProgram.fromBytes`
- [ ] Cache hit pro opakované prompty
- [ ] `source: "generated"` výskyty < 10% při normálním provozu (RAG má dostatečnou pokrytí)
- [ ] Prometheus metriky exportovány, Grafana dashboard aktualizován

---

## Otevřené otázky

1. **Flutter SPIR-V target**: ověřit, zda Flutter web/mobile akceptuje SPIR-V z `--target-env opengl` nebo potřebuje jiný target. Případně fallback na GLSL source (ne SPIR-V blob) a Flutter nechá zkompilovat na device.
2. **Camera texture format**: Flutter předá `ui.Image` — ověřit RGBA vs YUV422 a nutnost color space conversion v shaderu.
3. **Latence na DGX → NAS**: interní Cloudflare Tunnel přidá overhead. Změřit a případně přesunout Python pipeline na NAS.
4. **License enforcement**: CC-BY-NC zakazuje komerční použití — ověřit, zda MirrorBooth je komerční. Pokud ano, filtrovat pouze CC-BY a public domain.
5. **Cache invalidace**: po re-ingestion s aktualizovanými shadery (nové překlady, opravené metriky) → invalidovat cache záznamy se starými `eval_score`.

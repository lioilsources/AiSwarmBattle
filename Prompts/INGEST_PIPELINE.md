# INGEST_PIPELINE.md — Fáze 1 detail

Cíl: postavit knowledgebase ShaderToy shaderů v podobě, která je užitečná pro mobile GLSL ES 3.0 generování. Ne archiv. Ne searchable raw dump. **Strukturovaná, validovaná, decomposovaná, embedded.**

## Tok dat

```
┌─────────────────┐
│ ShaderToy API   │
└────────┬────────┘
         │ JSON (raw shader + metadata)
         ▼
┌─────────────────┐  raw/{id}.json
│ scraper         │  rate-limited, resumable
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ classifier      │  single-pass? multi-pass? sound? 
└────────┬────────┘  flag multi-pass → skip MVP, save for v2
         │ single-pass only
         ▼
┌─────────────────┐
│ desktop parser  │  tree-sitter-glsl → AST
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ ES translator   │  AST transformace, viz pravidla níže
└────────┬────────┘
         │ translated GLSL
         ▼
┌─────────────────┐
│ validator       │  glslang → kompilace pro ES 3.0
└────┬───────┬────┘
     │ OK    │ FAIL → flag, log důvod, ulož do failures.db
     ▼       
┌─────────────────┐
│ static metrics  │  LOC, branches, tex_reads, ALU estimate
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ decomposer      │  extrakce funkcí → knowledge graph
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ enricher        │  Qwen3-32B: description + tagy
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ embedder        │  e5-mistral → ChromaDB
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ store           │  SQLite metadata + ChromaDB vektory
└─────────────────┘
```

Každý krok je samostatný binary v `ingest/cmd/`, koordinovaný buďto přes makefile (sériově) nebo přes filesystem watching (kontinuální). Pro MVP sériově.

## Storage layout

```
/data/shadertoy/
├── raw/
│   ├── {shader_id}.json          # přímo z API
│   └── _manifest.jsonl           # append-only: id, fetched_at, http_status
├── translated/
│   ├── {shader_id}.es.glsl       # úspěšně přeložené
│   └── _failures.jsonl           # id, stage, error_msg
├── decomposed/
│   └── {shader_id}.functions.json
└── shaders.db                    # SQLite, viz schema níže
```

ChromaDB běží jako separátní HTTP service, persistence v `/data/chromadb/`.

## SQLite schema

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE shaders (
  id              TEXT PRIMARY KEY,        -- ShaderToy ID
  title           TEXT NOT NULL,
  author          TEXT,
  shadertoy_url   TEXT,
  likes           INTEGER DEFAULT 0,
  views           INTEGER DEFAULT 0,
  published_at    INTEGER,                  -- unix ts
  license         TEXT,                     -- CC-BY-NC, CC-BY, custom
  
  is_multipass    INTEGER NOT NULL,         -- 0/1
  has_sound       INTEGER NOT NULL,
  has_keyboard    INTEGER NOT NULL,
  num_buffers     INTEGER DEFAULT 0,
  
  raw_glsl        TEXT NOT NULL,            -- desktop, jak je z API
  es_glsl         TEXT,                     -- translated, NULL on failure
  
  translate_status TEXT NOT NULL,           -- ok|partial|failed
  translate_notes  TEXT,                    -- důvody patchů
  
  compile_ok      INTEGER,                  -- 0/1, NULL pokud netranslated
  compile_log     TEXT,                     -- warnings + errors raw
  
  llm_description TEXT,                     -- 1-věta
  llm_tags        TEXT,                     -- JSON array
  llm_model       TEXT,                     -- který model enrichoval
  llm_enriched_at INTEGER,
  
  ingested_at     INTEGER NOT NULL,
  schema_version  INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX idx_shaders_compile ON shaders(compile_ok);
CREATE INDEX idx_shaders_multipass ON shaders(is_multipass);
CREATE INDEX idx_shaders_license ON shaders(license);

CREATE TABLE shader_metrics (
  shader_id           TEXT PRIMARY KEY REFERENCES shaders(id) ON DELETE CASCADE,
  loc                 INTEGER,
  loc_logical         INTEGER,              -- bez prázdných a komentářů
  function_count      INTEGER,
  branch_count        INTEGER,              -- if/else/?:
  loop_count          INTEGER,
  loop_max_iter       INTEGER,              -- static estimate, -1 = dynamic
  texture_read_count  INTEGER,
  uniform_count       INTEGER,
  varying_count       INTEGER,
  uses_derivatives    INTEGER,              -- dFdx/dFdy
  uses_discard        INTEGER,
  uses_noise          INTEGER,              -- detected noise patterns
  estimated_alu_ops   INTEGER,
  precision_default   TEXT,                 -- highp|mediump|lowp|none
  est_mobile_cost     REAL,                 -- composite, viz cost_model.md
  computed_at         INTEGER
);

CREATE TABLE shader_functions (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  shader_id       TEXT NOT NULL REFERENCES shaders(id) ON DELETE CASCADE,
  function_name   TEXT NOT NULL,
  signature       TEXT NOT NULL,            -- vec4 foo(vec2 a, float b)
  return_type     TEXT NOT NULL,
  body            TEXT NOT NULL,
  category        TEXT,                     -- sdf|noise|color|transform|lighting|util
  is_recursive    INTEGER DEFAULT 0,
  is_pure         INTEGER,                  -- žádné side-effecty (sample, discard)
  body_hash       TEXT NOT NULL,            -- sha256, pro dedup
  chroma_id       TEXT                      -- ID v ChromaDB function collection
);

CREATE INDEX idx_functions_category ON shader_functions(category);
CREATE INDEX idx_functions_hash ON shader_functions(body_hash);
CREATE INDEX idx_functions_name ON shader_functions(function_name);

CREATE TABLE ingest_runs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at      INTEGER NOT NULL,
  finished_at     INTEGER,
  stage           TEXT NOT NULL,            -- scrape|translate|enrich|embed
  shaders_processed INTEGER DEFAULT 0,
  shaders_ok      INTEGER DEFAULT 0,
  shaders_failed  INTEGER DEFAULT 0,
  notes           TEXT
);
```

## Krok po kroku

### 1. Scraper (`cmd/scraper`)

ShaderToy API:
- `GET /api/v1/shaders?key=$KEY` → paginated list of IDs
- `GET /api/v1/shaders/{id}?key=$KEY` → full shader

Implementace:

```go
package shadertoy

type Client struct {
    apiKey   string
    http     *http.Client
    limiter  *rate.Limiter  // golang.org/x/time/rate
    baseURL  string
}

func NewClient(apiKey string) *Client {
    return &Client{
        apiKey:  apiKey,
        http:    &http.Client{Timeout: 30 * time.Second},
        limiter: rate.NewLimiter(rate.Every(time.Second), 1), // 60/min
        baseURL: "https://www.shadertoy.com/api/v1",
    }
}

func (c *Client) ListIDs(ctx context.Context, from, size int) ([]string, error)
func (c *Client) GetShader(ctx context.Context, id string) (*RawShader, error)
```

Klíčové vlastnosti:
- **Resumable**: čte `_manifest.jsonl`, přeskakuje již fetched IDs (pokud nejsou starší než N dní pro refresh)
- **Backoff**: exponenciální na 429/503, max 5 retries
- **Disk write atomické**: write to `.tmp` + rename
- **Žádný in-memory akumulátor**: streaming na disk

### 2. Classifier

Triviální. Z raw JSONu zjisti:
- `Shader.renderpass` count → multipass detekce
- typy passů: `image|buffer|sound|cubemap|common`
- existence channels (texture inputs)

Pro MVP: **drž jen `image` only single-pass shadery bez audio**. Multipass označ a uložil pro v2 (~30 % korpusu).

### 3. Tree-sitter parser

Nainstalovat `tree-sitter-glsl` ([github.com/theHamsta/tree-sitter-glsl](https://github.com/theHamsta/tree-sitter-glsl)) nebo `tree-sitter-glsl` od `cybertk`. Build C grammar, link přes cgo.

```go
package glsl

type AST struct {
    Root      *Node
    Source    []byte
    Functions []FunctionDecl
    Uniforms  []UniformDecl
    Varyings  []VaryingDecl
    Errors    []ParseError
}

func Parse(src []byte) (*AST, error)
func (a *AST) Print() string
func (a *AST) FindCalls(name string) []*Node
```

Drž AST mutable. Translator níže pracuje přímo s ním.

### 4. Translator — největší engineering effort

Cíl: **desktop GLSL (ShaderToy variant) → GLSL ES 3.0**.

Vstup je vždy ShaderToy `mainImage`:
```glsl
void mainImage(out vec4 fragColor, in vec2 fragCoord) { ... }
```

Výstup je samostatný fragment shader:
```glsl
#version 300 es
precision highp float;
precision highp int;

uniform vec3      iResolution;
uniform float     iTime;
uniform float     iTimeDelta;
uniform int       iFrame;
uniform float     iChannelTime[4];
uniform vec4      iMouse;
uniform vec4      iDate;
uniform float     iSampleRate;
uniform vec3      iChannelResolution[4];
uniform sampler2D iChannel0;
uniform sampler2D iChannel1;
uniform sampler2D iChannel2;
uniform sampler2D iChannel3;

out vec4 fragColor;

// <USER CODE: vše kromě mainImage, deduplikovaně>

// <USER CODE: mainImage tělo, přepsané>

void main() {
    vec4 outColor;
    mainImageImpl(outColor, gl_FragCoord.xy);
    fragColor = outColor;
}
```

#### Transformační pravidla

Pojmenuj soubor s pravidly `ingest/internal/translate/rules.go`. Každé pravidlo:

```go
type Rule struct {
    Name        string
    Description string
    AppliesTo   func(*glsl.AST) bool
    Transform   func(*glsl.AST) (TransformResult, error)
}

type TransformResult struct {
    Modified bool
    Notes    []string  // co se změnilo (do translate_notes)
}
```

Pravidla v pořadí aplikace:

| #   | Pravidlo                          | Popis                                                    |
|-----|-----------------------------------|----------------------------------------------------------|
| R01 | EnsureVersionHeader               | Inject `#version 300 es` pokud chybí                     |
| R02 | EnsurePrecisionQualifiers         | `precision highp float; precision highp int;`            |
| R03 | DeclareShaderToyUniforms          | Přidá iTime, iResolution, etc.                           |
| R04 | DeclareFragColorOut               | `out vec4 fragColor;` (renamed na vyhnutí kolize)        |
| R05 | RenameMainImage                   | `mainImage` → `mainImageImpl`                            |
| R06 | AddMainEntrypoint                 | Generuje `void main() { ... fragColor = ...; }`          |
| R07 | ReplaceTexture2D                  | `texture2D(s, uv)` → `texture(s, uv)`                    |
| R08 | ReplaceTextureCube                | `textureCube(s, dir)` → `texture(s, dir)`                |
| R09 | ReplaceVaryingAttribute           | `varying` → `in`/`out` podle stage                       |
| R10 | ReplaceGlFragColor                | `gl_FragColor = X` → `fragColor = X`                     |
| R11 | EnsureNonConstLoopBounds          | Detect `for(int i=0; i<N; i++)` kde N je uniform → flag  |
| R12 | InsertPowGuards                   | `pow(x,y)` → `pow(max(x, 0.0), y)` kdy x může být negative |
| R13 | InsertDivisionGuards              | `1./x` v hot path → option to insert `max(x, EPS)`       |
| R14 | DowngradePrecisionWhereSafe       | Colors / UV → mediump (kvalitativní, ale large win)      |
| R15 | StripDesktopOnlyExtensions        | `#extension GL_ARB_*` (mimo whitelist)                   |
| R16 | NormalizeWhitespace               | gofmt-style pro GLSL, prep pro diffing                   |

**Failure modes** (loguj do translate_notes):
- Multipass simulation v image passu (drawcall in shader code) → fail
- Inline assembly nebo intrinsics specifické pro desktop
- Bitcast manipulace (`floatBitsToInt` ok v ES 3.0; `uintBitsToFloat` ok)
- Custom #define makra, co overloadují builtin funkce
- Shadery, co dynamicky indexují uniform array s non-const indexem (ES 3.0 to umí, ale starší drivery zlobí)

**Strategie pro odhalení edge cases**:
1. Začni s 100 nejlikedovanějšími shadery, ručně projdi failures
2. Patterns přidávej jako pravidla
3. Cíl pro MVP: 60 % first-pass success rate, 80 % po manuálních patch rules

#### Golden testy

```
ingest/internal/translate/testdata/
├── 01_simple_color/
│   ├── input.glsl
│   ├── expected.es.glsl
│   └── notes.txt
├── 02_texture2d_rename/
├── 03_raymarch_no_change/
└── 04_pow_negative_guard/
```

Každý test: input → run translator → diff proti expected. CI gate.

### 5. Validator (`cmd/validator` nebo součást translatoru)

Použij **glslang** binary nebo C++ library přes cgo. Pro ARM64 builduj from source:

```bash
git clone https://github.com/KhronosGroup/glslang
cd glslang && python3 update_glslang_sources.py
cmake -B build -DENABLE_OPT=0 && cmake --build build -j
```

Wrapper:

```go
package glslang

type ValidateRequest struct {
    Source []byte
    Stage  Stage  // VertexShader|FragmentShader
    Target Target // GLSLES300
}

type ValidateResult struct {
    OK       bool
    Errors   []Diagnostic
    Warnings []Diagnostic
    SPIRV    []byte  // optional, useful for cost model
}

func Validate(req ValidateRequest) (*ValidateResult, error)
```

Nebuduj wrapper přes shell-out na binary v hot pathu — cgo je rychlejší. Shell-out OK pro batch ingest.

### 6. Static metrics

Z AST + (volitelně) SPIRV. Klíčové metriky:

```go
type Metrics struct {
    LOC              int
    LOCLogical       int  // bez whitespace a komentářů
    FunctionCount    int
    BranchCount      int  // if + else + ternary + switch case
    LoopCount        int
    LoopMaxIter      int  // -1 pro dynamic
    TextureReads     int  // texture(), textureLod(), atd.
    UniformCount     int
    VaryingCount     int
    UsesDerivatives  bool
    UsesDiscard      bool
    UsesNoise        bool  // pattern match: hash21, perlin, simplex, etc.
    EstimatedALU     int
    DefaultPrecision string
}
```

**ALU estimate** (rough):
- arithmetic op (`+`, `-`, `*`, `/`): 1
- `sqrt`, `rsqrt`, `exp`, `log`: 4
- `sin`, `cos`, `tan`: 8 (na mobilu drahé)
- `pow`: 8
- `texture()`: 0 ALU, +1 tex_read
- loop body × max iterations (capped at 100)
- branches: penalty +2 pro divergence

Toto je deliberately rough — pro fine-tuning bude calibration scriptem proti reálnému mobile profileru. Cost model verzuj.

### 7. Decomposer

Z AST extrahuj všechny user-defined funkce (mimo `mainImage`, `mainImageImpl`, `main`).

```go
func Decompose(ast *glsl.AST) ([]FunctionDecl, error)

type FunctionDecl struct {
    Name       string
    Signature  string
    ReturnType string
    Body       string  // raw text
    IsPure     bool    // no texture(), no discard
    Category   string  // classify(decl)
    BodyHash   string  // sha256(normalize(Body))
}
```

**Klasifikace funkcí** — heuristika podle názvu + AST patterns:

| Category   | Patterns                                                         |
|------------|------------------------------------------------------------------|
| sdf        | name matches `sd[A-Z]`, returns float, takes vec3                |
| noise      | name contains noise/hash/rand/perlin/simplex/worley               |
| color      | name contains palette/hsv/rgb/oklab/tonemap                       |
| transform  | returns mat2/mat3/mat4, name contains rot/scale                   |
| lighting   | name contains lambert/phong/ggx/brdf/specular                     |
| util       | fallback                                                          |

**Dedup**: pokud `body_hash` už existuje, ulož reference (foreign key), ne duplikát. To je důležité — `sdSphere` je v korpusu 10000x identicky.

### 8. Enricher

Pro každý úspěšně přeložený shader zavolat Qwen3-32B přes LiteLLM:

```python
# pipelines/.../enricher_prompt.txt
You analyze GLSL shaders for a mobile shader knowledgebase.
Given the shader code and metadata, output STRICT JSON:

{
  "description": "single sentence, <120 chars, what the shader visually produces",
  "tags": ["tag1", "tag2", ...]  // 5-15 tags from CONTROLLED vocabulary below
}

Controlled vocabulary:
  technique: raymarch, raytrace, 2d, post_process, particle, sdf, fractal, kaleidoscope
  visual:    plasma, fire, water, smoke, stars, geometry, abstract, organic
  motion:    static, animated, reactive, loop
  palette:   monochrome, rainbow, warm, cool, gradient
  cost:      cheap, medium, expensive

DO NOT add tags outside vocabulary. DO NOT explain.

Shader:
{glsl_code}

Metadata:
title: {title}
tags_from_author: {author_tags}
```

Reply parsed jako JSON, validovaný proti Pydantic schématu, retry na invalid.

### 9. Embedder

Embeddings strategy:

**Shader-level (collection `shaders`)**:
```python
input_text = f"""Title: {title}.
Description: {description}.
Tags: {", ".join(tags)}.
Techniques: {extract_technique_names(decomposed_functions)}."""

embedding = e5_mistral.embed(input_text)
```

**Function-level (collection `functions`)**:
```python
input_text = f"""Function {name}: {category}.
Signature: {signature}.
Description: {llm_describe_function(name, body)}."""

embedding = e5_mistral.embed(input_text)
```

ChromaDB collections:
- `shaders` — top-K retrieval pro whole-shader inspiration
- `functions` — top-K retrieval pro composability (primary use case pro pipelines)

Metadata na ChromaDB záznamech: `shader_id`, `category`, `compile_ok`, `est_mobile_cost`, `license`. To umožňuje filtered retrieval (`where: {compile_ok: 1, est_mobile_cost: {"$lt": 100}}`).

### 10. Orchestrace

```bash
# ingest/Makefile
.PHONY: scrape translate enrich embed all

scrape:
	./bin/scraper --output=/data/shadertoy/raw --rate=60/min

translate:
	./bin/translator --input=/data/shadertoy/raw --output=/data/shadertoy/translated --db=/data/shaders.db

enrich:
	./bin/enricher --db=/data/shaders.db --llm=http://dgx:4000 --batch=50

embed:
	./bin/indexer --db=/data/shaders.db --chroma=http://dgx:8006

all: scrape translate enrich embed
```

Každý binary je resumable a idempotentní. Re-run jen processuje nedokončené.

## Resumability & Idempotence

Klíčové. Ingest je multi-hour proces, padne to. Pravidla:

1. **Scraper**: kontroluje `_manifest.jsonl` před fetchem.
2. **Translator**: kontroluje `shaders.es_glsl IS NULL OR translate_status='retry'` před processingem.
3. **Enricher**: kontroluje `llm_enriched_at IS NULL`.
4. **Embedder**: kontroluje existenci v ChromaDB přes `chroma_id` v SQLite.

Každý běh loguj do `ingest_runs` tabulky se stage, počty, časem.

## CLI flags (společné)

```
--config=path/to/config.yaml
--db=/data/shaders.db
--log-level=info|debug
--dry-run                        # nezapisuj, jen logni
--limit=N                        # cap pro testing
--filter='compile_ok=1'          # SQL WHERE pro selective re-run
```

## Testovací strategie

1. **Unit testy** na translator pravidla — golden tests.
2. **Integration test** — 10 hand-picked shaderů, projít full pipeline, assertovat finální stav.
3. **Smoke test pro DB schema** — migrace + insert + query.
4. **Translator coverage** — měř, kolik % top-100 likedovaných shaderů projde.

## Otevřené otázky pro budoucí iterace

- Multipass support (BufferA-D simulace)
- Common code tab (ShaderToy umožňuje sdílený soubor)
- Sound shadery (pro SMPL-16 možná zajímavé)
- Cubemap inputs
- VR / stereo shadery
- Re-fetch logika když ShaderToy mění shader po publishi

## Performance odhad

Pro ~80k shaderů, sériově:

| Stage      | Per-shader | Total       |
|------------|------------|-------------|
| Scrape     | ~1s        | ~22h        |
| Translate  | ~50ms      | ~1.1h       |
| Validate   | ~100ms     | ~2.2h       |
| Metrics    | ~20ms      | ~30min      |
| Enrich     | ~3s (LLM)  | ~67h        |
| Embed      | ~200ms     | ~4.4h       |

Bottleneck: enrich. Možnosti:
- Paralelně přes batch endpoint (vLLM continuous batching)
- Spustit enrich nightly jako background job
- Začít s top-1000 shaderů (~3 hodiny full enrich) a iterovat

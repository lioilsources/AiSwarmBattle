# AGENTS.md — Pokyny pro Claude Code

Tento repozitář se jmenuje **ShaderBattle**. Cílem je benchmark dvou LLM pipelines pro generování mobile GLSL ES 3.0 shaderů z ShaderToy knowledgebase.

## Komunikace

- **Jazyk: čeština.** Veškerá komunikace s uživatelem, commity, PR description, plánovací odpovědi.
- **Kód: angličtina.** Identifikátory, komentáře v kódu, dokumentace API, log messages.
- **Stručnost.** Žádné dlouhé úvody. Žádné "skvělá otázka". Žádné shrnutí, co Claude právě udělal, pokud o to uživatel neřekl.
- **Bez emoji** v kódu, commitech, ani v markdownu pokud nejsou výslovně požadovány.

## Tech stack — neměnit bez diskuze

| Vrstva           | Technologie                                          |
|------------------|------------------------------------------------------|
| Infra (ingest, evaluator, orchestrator) | Go 1.23+, cgo kde nutno         |
| Pipelines        | Python 3.11+, FastAPI, LangChain/LangGraph           |
| LLM serving      | vLLM přes LiteLLM gateway (`llm.ol1n.com` pattern)   |
| GLSL parsing     | tree-sitter-glsl                                     |
| GLSL validation  | glslang (Khronos) přes cgo                           |
| Rendering        | EGL headless + GLES 3.0 context                      |
| Vector store     | ChromaDB (HTTP server mode)                          |
| Metadata         | SQLite (modernc.org/sqlite — pure Go, ARM64 friendly)|
| Queue/blackboard | Redis (Pipeline A swarm)                             |
| Orchestrace      | Docker Compose + Caddy + Cloudflare Tunnel           |
| Telemetrie       | Prometheus + Grafana                                 |

## Kódový styl — Go

- `gofmt -s` + `goimports` při každém uložení.
- Lint: `golangci-lint run` zelený před commitem.
- Struktura: `cmd/<binary>/main.go` velmi tenké, logika v `internal/`.
- Sdílený kód mezi moduly přes `pkg/` na úrovni workspace.
- Errors: `fmt.Errorf("context: %w", err)`. Nikdy `errors.New` se stringem co konkatenuje proměnné. Žádné panic v library kódu.
- Logging: `log/slog`, structured, JSON v produkci, text v dev.
- Context: každá public funkce, co dělá I/O, bere `ctx context.Context` jako první argument.
- Tabular tests: `t.Run(tc.name, ...)`, sub-test per case.
- Pojmenování: package názvy `lowercase` jednoslovné, identifikátory `CamelCase`. Žádné `utils`, `common`, `helpers` packagy.

## Kódový styl — Python

- `ruff` (lint + format) místo black/isort/flake8.
- Type hints všude, `mypy --strict` na nových modulech.
- Pydantic v2 pro datové modely a I/O kontrakty.
- Async I/O kdekoliv se mluví s LLM (`httpx.AsyncClient`).
- Žádné `print()`. `logging.getLogger(__name__)`.
- Imports: stdlib → third-party → local, sorted, `from x import y, z`.

## Git workflow

- Branch per fáze: `phase1/ingest-*`, `phase2/evaluator-*`, `phase3/pipeline-*`.
- Commit message: imperativní mood, anglicky, prefix scope:
  ```
  ingest: add shadertoy api client with rate limiting
  evaluator: implement egl context init for arm64
  orchestrator: wire battle dispatch with bootstrap stats
  docs: update translator known issues
  ```
- Žádné "wip" commity v hlavních branchích. Squash při merge.
- Žádné generated soubory v gitu (binaries, `.so`, `.db`, `data/`).

## Workspace setup

```
go.work obsahuje:
  ./ingest
  ./evaluator
  ./orchestrator
  ./pkg
```

Sdílené typy (např. `EvalRequest`, `EvalResult`, `ShaderRecord`) žijí v `pkg/types/`. Žádný modul nesmí importovat z jiného `cmd/` nebo `internal/`.

## Závazná pravidla pro LLM kód (oba pipeliny)

1. **Identický I/O kontrakt.** Definovaný v `pkg/types/contract.go` a zrcadlený v `pipelines/*/server.py`. Při změně kontraktu se musí změnit oba zároveň, v jednom PR.

2. **Žádný shader nesmí opustit pipeline nezvalidovaný.** Každá pipeline musí lokálně zavolat `glslang` před vrácením výsledku. Pokud nekompiluje, vrátit s flagem, ne tiše zatajit.

3. **Žádné API klíče v kódu ani v commitech.** Vše přes `.env` + `os.Getenv` / `os.environ.get`. Sample v `.env.example`.

4. **Prompty jsou kód.** Žijí v `pipelines/*/prompts/*.txt` nebo `*.yaml`, verzované, nikdy hardcoded v Pythonu.

5. **Reproducibilita.** Pipeline akceptuje `seed` parametr a propaguje ho do retrieval i LLM calls (kde to model umí). Battle běh loguje seedy.

## Pravidla pro evaluator (kritické)

1. **Determinismus.** Stejný shader + stejné podmínky → identické skóre (±1 % tolerance jen pro perf metriky).

2. **Žádné LLM v evaluatoru.** Evaluator je deterministický kompiler/profilier/fuzzer. LLM judge je *separátní* volitelná vrstva, NE součást primárního skóre.

3. **EGL kontext je single-threaded.** Evaluator HTTP server přijímá paralelní requesty, ale interně serializuje na single render goroutine přes channel.

4. **Cost model je verzovaný.** Změna kalibrace = bump verze v response. Battle orchestrator ukládá `evaluator_version` k výsledku.

## Co Claude Code NEMÁ dělat

- **Negenerovat** shadery, prompty nebo testovací data sám pokud o to není explicitně požádán. Vždy preferovat: vytáhnout z ShaderToy KB nebo z `prompts/` suite.
- **Negenerovat** mock implementace, kde má být skutečná. Pokud chybí dependency, napiš `TODO` s odkazem na issue, ne fake.
- **Nepřepisovat** translator pravidla bez testovacího casu. Každé nové pravidlo musí mít golden test v `ingest/internal/translate/testdata/`.
- **Nepřidávat** novou závislost bez kontroly velikosti/údržby. Zvlášť opatrně s C deps na ARM64.
- **Neměnit** scoring váhy v `evaluator/internal/score/` bez aktualizace `docs/scoring_rubric.md` ve stejném PR.
- **Nespouštět** dlouhé batch joby (ingest, battle) z vlastní iniciativy. Návrh nech na uživateli.

## Co Claude Code MÁ dělat

- Při nejasnosti se ptát, ne hádat. Preferovat 2-3 cílené otázky před divokým generováním.
- Před implementací rozsáhlé komponenty napsat krátký design notes (5-10 řádků) a počkat na potvrzení.
- Při refaktoringu zachovat veřejné API, dokud uživatel neschválí breaking change.
- Při psaní testů cílit na chování, ne implementaci. Tabular tests preferované.
- Při debug session navrhnout reprodukční minimální case, ne plošně logovat všechno.

## Lokální dev běh

```bash
# Setup
make setup                    # nainstaluje go deps, python venv, stáhne tree-sitter grammar
make models-pull              # přes ngc / hf-cli stáhne modely na DGX

# Ingest (lokálně proti malému testovacímu setu)
make ingest-test              # 50 shaderů, smoke test
make ingest-full              # plný běh, JEN na NAS

# Evaluator
make eval-build               # cgo build, vyžaduje glslang + EGL headers
make eval-test                # unit tests
make eval-serve               # spustí HTTP server na :8081

# Battle
make battle SUITE=v1 RUNS=1   # quick smoke
make battle SUITE=v1 RUNS=3   # plný běh
```

## Důležité soubory pro orientaci

- `OPUS_PLAN.md` — master plán, fáze, architektura
- `INGEST_PIPELINE.md` — detail fáze 1
- `EVALUATOR.md` — detail fáze 2
- `pkg/types/contract.go` — I/O kontrakty (single source of truth)
- `docs/glsl_es_translation.md` — translator pravidla a known issues
- `docs/scoring_rubric.md` — scoring váhy a jejich verze

## Když se Claude Code dostane mimo scope

Pokud uživatelův dotaz směřuje mimo aktuální fázi (např. fáze 1 běží, ptá se na pipeline prompty), odpověz krátce s odkazem na příslušnou sekci `OPUS_PLAN.md` a nabídni vrácení k aktuální práci.

## Kontext o uživateli

- **Jazyk:** Čeština (Mluv Česky).
- **Background:** Go + Flutter dev, self-hosted infra na DGX Spark + NAS, ekosystém kolem `ol1n.com`.
- **Styl:** Direct, technické detaily, žádné nesmysly. Nezdržuj. Žádné voláním "vynikající otázko" ani disclaimery.
- **Známé projekty:** SMPL-16 (Flutter audio sampler), Ol1nSuperApp/Senát, Vault feature, OpenCode setup na DGX. Tento projekt sdílí infrastrukturní vzory s nimi (Caddy, Cloudflare Tunnel, LiteLLM gateway, custom Modelfiles).

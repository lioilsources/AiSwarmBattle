"""Bootstrap ingest: populate the ChromaDB 'shaders' collection from:

  1. Existing MirrorBooth .frag shaders  (Flutter/Impeller format, ready to use)
  2. ToyShaders seed corpus              (.glsl, Shadertoy convention → ported to Flutter)

The collection schema matches what nodes/rag.py expects:
  document  = full GLSL code (returned as glsl_es in ShaderCandidate)
  embedding = semantic description text  (e5-mistral-7b via TEI)
  metadata  = shader_id, title, author, license, description, tags,
              est_mobile_cost, compile_ok, motion

Usage:
    python -m mirrorbooth_retrieval.bootstrap_ingest \\
        --mirrorbooth-root /path/to/MirrorBooth/mirrorbooth \\
        --seed-dir        /path/to/MirrorBooth/pipeline/seed_shaders \\
        --embed-url       http://dgx-spark:8005 \\
        --chroma-url      http://dgx-spark:8006 \\
        [--reset]         # drop and recreate collection
        [--dry-run]       # print what would be ingested, no writes
"""

from __future__ import annotations

import argparse
import json
import logging
import re
import sys
import time
from pathlib import Path
from typing import NamedTuple

import httpx

log = logging.getLogger(__name__)

COLLECTION = "shaders"
BATCH_SIZE = 16  # embed this many shaders per TEI call

# ── Metadata inference ────────────────────────────────────────────────────────

_TECHNIQUE_KEYWORDS: dict[str, str] = {
    "sobel": "edge_detection",   "edge": "edge_detection",
    "blur": "blur",              "gaussian": "blur",
    "cel": "cel_shading",        "toon": "cel_shading",
    "noise": "noise",            "fbm": "fbm",
    "voronoi": "voronoi",        "worley": "voronoi",
    "plasma": "plasma",          "sin(": "animation",
    "cos(": "animation",         "utime": "animation",
    "itime": "animation",
    "hue": "color",              "hsv": "color",       "oklab": "color",
    "chromat": "chromatic_aberration",
    "halftone": "halftone",      "pixel": "pixelation",
    "glitch": "glitch",          "watercolor": "watercolor",
    "oil": "oil_painting",       "sketch": "sketch",   "pencil": "sketch",
    "thermal": "thermal",        "neon": "neon",       "glow": "neon",
    "crt": "crt",                "scanline": "crt",
    "kaleido": "kaleidoscope",   "tunnel": "tunnel",
    "march": "raymarch",         "raymarch": "raymarch", "sdf": "raymarch",
    "domain": "domain_warp",     "warp": "domain_warp",
    "palette": "palette_cycle",  "truchet": "truchet",
    "metaball": "metaballs",     "curl": "flow_field",
    "hex": "hex_grid",           "polar": "polar_warp",
    "starfield": "stars",        "bokeh": "bokeh",
    "cartoon": "cel_shading",    "anime": "cel_shading",
    "mirror": "mirror",          "reflect": "mirror",
    "comic": "comic",            "manga": "comic",
}

_VISUAL_KEYWORDS: dict[str, str] = {
    "fire": "fire",       "water": "water",    "smoke": "smoke",
    "star": "stars",      "neon": "neon",      "bokeh": "bokeh",
    "abstract": "abstract",
}


def _infer_tags(code: str, stem: str) -> list[str]:
    combined = (code + " " + stem).lower()
    found: set[str] = set()
    for kw, tag in _TECHNIQUE_KEYWORDS.items():
        if kw in combined:
            found.add(tag)
    for kw, tag in _VISUAL_KEYWORDS.items():
        if kw in combined:
            found.add(tag)
    return sorted(found)


def _infer_motion(code: str) -> str:
    low = code.lower()
    if "utime" in low or "itime" in low:
        return "animated"
    return "static"


def _estimate_mobile_cost(code: str) -> int:
    """Rough ALU + texture cost estimate (calibration placeholder)."""
    cost = 30
    cost += code.lower().count("texture(") * 80
    cost += len(re.findall(r"\bsin\s*\(|\bcos\s*\(|\btan\s*\(", code)) * 8
    cost += len(re.findall(r"\bpow\s*\(", code)) * 8
    cost += len(re.findall(r"\bsqrt\s*\(|\bexp\s*\(|\blog\s*\(", code)) * 4
    cost += len(re.findall(r"\bfor\s*\(", code)) * 25
    return min(cost, 500)


def _stem_to_title(stem: str) -> str:
    """'filter_neon_bokeh' → 'Neon Bokeh'."""
    return re.sub(r"^(filter_|seed_)", "", stem).replace("_", " ").title()


def _stem_to_description(stem: str, tags: list[str]) -> str:
    title = _stem_to_title(stem)
    if tags:
        return f"{title} shader effect. Techniques: {', '.join(tags[:5])}."
    return f"{title} GLSL fragment shader effect."


def _embed_text(stem: str, description: str, tags: list[str]) -> str:
    """Build the text that gets embedded (semantic, not raw GLSL)."""
    return (
        f"Title: {_stem_to_title(stem)}. "
        f"Description: {description} "
        f"Tags: {', '.join(tags)}."
    )


# ── Shader record ─────────────────────────────────────────────────────────────

class ShaderRecord(NamedTuple):
    shader_id: str
    title: str
    author: str
    license: str
    description: str
    tags: list[str]
    est_mobile_cost: int
    motion: str
    glsl: str            # full GLSL code → stored as ChromaDB document
    embed_text: str      # what to embed


# ── Readers ───────────────────────────────────────────────────────────────────

def _read_frag_shaders(shaders_dir: Path) -> list[ShaderRecord]:
    records: list[ShaderRecord] = []
    for f in sorted(shaders_dir.glob("*.frag")):
        if f.stem == "mirror":  # core mirror shader, not a filter
            continue
        code = f.read_text(encoding="utf-8")
        tags = _infer_tags(code, f.stem)
        desc = _stem_to_description(f.stem, tags)
        records.append(ShaderRecord(
            shader_id=f.stem,
            title=_stem_to_title(f.stem),
            author="MirrorBooth",
            license="CC-BY-NC",
            description=desc,
            tags=tags,
            est_mobile_cost=_estimate_mobile_cost(code),
            motion=_infer_motion(code),
            glsl=code,
            embed_text=_embed_text(f.stem, desc, tags),
        ))
    return records


def _read_seed_shaders(seed_dir: Path) -> list[ShaderRecord]:
    """Port Shadertoy-convention *.glsl seeds to Flutter format before ingest."""
    # Import static adapter functions (no LLM, no network)
    from .nodes.adapter import apply_static_renames, _convert_main_image, _strip_preamble
    from .nodes.adapter import _PREAMBLE_STATIC, _PREAMBLE_ANIMATED
    from .nodes.adapter import _CHANNEL0_RE, _CHANNEL_OTHER_RE

    records: list[ShaderRecord] = []
    for f in sorted(seed_dir.glob("*.glsl")):
        code = f.read_text(encoding="utf-8")

        # Port to Flutter
        ported, renames = apply_static_renames(code)
        ported, _ = _convert_main_image(ported)
        if _CHANNEL0_RE.search(ported):
            ported = _CHANNEL0_RE.sub("uTexture", ported)
        if _CHANNEL_OTHER_RE.search(ported):
            ported = _CHANNEL_OTHER_RE.sub("uTexture", ported)
        uses_time = "uTime" in ported
        body = _strip_preamble(ported)
        preamble = _PREAMBLE_ANIMATED if uses_time else _PREAMBLE_STATIC
        glsl = preamble + "\n" + body

        tags = _infer_tags(code, f.stem)
        desc = _stem_to_description(f.stem, tags)

        records.append(ShaderRecord(
            shader_id=f.stem,
            title=_stem_to_title(f.stem),
            author="CC0 seed",
            license="public",
            description=desc,
            tags=tags,
            est_mobile_cost=_estimate_mobile_cost(glsl),
            motion=_infer_motion(glsl),
            glsl=glsl,
            embed_text=_embed_text(f.stem, desc, tags),
        ))
    return records


# ── TEI embedding ─────────────────────────────────────────────────────────────

def _embed_batch(embed_url: str, texts: list[str]) -> list[list[float]]:
    with httpx.Client(timeout=30) as client:
        resp = client.post(f"{embed_url}/embed", json={"inputs": texts})
        resp.raise_for_status()
        return resp.json()


def _embed_all(embed_url: str, records: list[ShaderRecord]) -> list[list[float]]:
    embeddings: list[list[float]] = []
    for i in range(0, len(records), BATCH_SIZE):
        batch = records[i : i + BATCH_SIZE]
        texts = [r.embed_text for r in batch]
        log.info("embedding batch %d–%d / %d", i + 1, i + len(batch), len(records))
        vecs = _embed_batch(embed_url, texts)
        embeddings.extend(vecs)
        time.sleep(0.1)  # brief pause between batches
    return embeddings


# ── ChromaDB upsert ───────────────────────────────────────────────────────────

def _get_or_create_collection(chroma_url: str, reset: bool) -> str:
    """Return collection UUID. Creates if missing, deletes+creates if reset."""
    with httpx.Client(timeout=10) as client:
        if reset:
            # List collections and delete matching one
            r = client.get(f"{chroma_url}/api/v1/collections")
            r.raise_for_status()
            for col in r.json():
                if col["name"] == COLLECTION:
                    client.delete(f"{chroma_url}/api/v1/collections/{col['id']}").raise_for_status()
                    log.info("reset: dropped existing '%s' collection", COLLECTION)
                    break

        r = client.post(f"{chroma_url}/api/v1/collections", json={
            "name": COLLECTION,
            "get_or_create": True,
            "metadata": {"hnsw:space": "cosine"},
        })
        r.raise_for_status()
        return r.json()["id"]


def _upsert_batch(
    chroma_url: str,
    collection_id: str,
    records: list[ShaderRecord],
    embeddings: list[list[float]],
) -> None:
    payload = {
        "ids":        [r.shader_id for r in records],
        "documents":  [r.glsl for r in records],
        "embeddings": embeddings,
        "metadatas":  [
            {
                "shader_id":      r.shader_id,
                "title":          r.title,
                "author":         r.author,
                "license":        r.license,
                "description":    r.description,
                "tags":           json.dumps(r.tags),
                "est_mobile_cost": r.est_mobile_cost,
                "compile_ok":     1,
                "motion":         r.motion,
            }
            for r in records
        ],
    }
    with httpx.Client(timeout=30) as client:
        resp = client.post(
            f"{chroma_url}/api/v1/collections/{collection_id}/upsert",
            json=payload,
        )
        resp.raise_for_status()


# ── Main ──────────────────────────────────────────────────────────────────────

def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Bootstrap ChromaDB from MirrorBooth shaders")
    p.add_argument("--mirrorbooth-root", type=Path, default=None,
                   help="Path to mirrorbooth/ Flutter project (contains shaders/)")
    p.add_argument("--seed-dir",         type=Path, default=None,
                   help="Path to pipeline/seed_shaders/ with Shadertoy *.glsl files")
    p.add_argument("--embed-url",        default="http://dgx-spark:8005",
                   help="TEI embedding service URL")
    p.add_argument("--chroma-url",       default="http://dgx-spark:8006",
                   help="ChromaDB HTTP server URL")
    p.add_argument("--reset",            action="store_true",
                   help="Drop and recreate the collection")
    p.add_argument("--dry-run",          action="store_true",
                   help="Print records, skip embedding and ChromaDB writes")
    return p.parse_args()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    args = _parse_args()

    records: list[ShaderRecord] = []

    if args.mirrorbooth_root:
        shaders_dir = args.mirrorbooth_root / "shaders"
        if not shaders_dir.exists():
            log.error("shaders/ not found in %s", args.mirrorbooth_root)
            sys.exit(1)
        frag_records = _read_frag_shaders(shaders_dir)
        log.info("MirrorBooth shaders: %d .frag files", len(frag_records))
        records.extend(frag_records)

    if args.seed_dir:
        if not args.seed_dir.exists():
            log.error("seed-dir not found: %s", args.seed_dir)
            sys.exit(1)
        seed_records = _read_seed_shaders(args.seed_dir)
        log.info("ToyShaders seed corpus: %d .glsl files (ported)", len(seed_records))
        records.extend(seed_records)

    if not records:
        log.error("no sources specified — use --mirrorbooth-root and/or --seed-dir")
        sys.exit(1)

    log.info("total: %d shaders to ingest", len(records))

    if args.dry_run:
        for r in records:
            print(f"  [{r.license:12}] {r.shader_id:40} motion={r.motion} cost={r.est_mobile_cost}")
            print(f"             tags: {', '.join(r.tags[:6])}")
        print(f"\n{len(records)} shaders — dry run complete, no writes")
        return

    log.info("embedding via %s ...", args.embed_url)
    embeddings = _embed_all(args.embed_url, records)

    log.info("upserting to ChromaDB %s ...", args.chroma_url)
    collection_id = _get_or_create_collection(args.chroma_url, reset=args.reset)

    for i in range(0, len(records), BATCH_SIZE):
        batch_r = records[i : i + BATCH_SIZE]
        batch_e = embeddings[i : i + BATCH_SIZE]
        _upsert_batch(args.chroma_url, collection_id, batch_r, batch_e)
        log.info("upserted %d / %d", min(i + BATCH_SIZE, len(records)), len(records))

    log.info("done — %d shaders in ChromaDB collection '%s'", len(records), COLLECTION)
    log.info("next: python -m mirrorbooth_retrieval.run --prompt '...' --name '...'")


if __name__ == "__main__":
    main()

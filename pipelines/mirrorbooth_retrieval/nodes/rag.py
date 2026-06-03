from __future__ import annotations

import logging
from typing import Any

import httpx

from ..state import RetrievalState, ShaderCandidate

log = logging.getLogger(__name__)

_ALLOWED_LICENSES = ["CC-BY", "CC-BY-NC", "public"]


async def rag_node(
    state: RetrievalState,
    *,
    embed_url: str,
    chroma_url: str,
    chroma_collection: str = "shaders",
) -> dict:
    intent = state.router_output
    assert intent is not None

    query_vec = await _embed(embed_url, intent.chroma_query)

    where = _build_where(state)
    candidates = await _query_chroma(chroma_url, chroma_collection, query_vec, where, n=15)

    if len(candidates) < 3:
        log.info("rag: only %d results — relaxing technique filter", len(candidates))
        where_relaxed = _build_where(state, relax_tags=True)
        candidates = await _query_chroma(chroma_url, chroma_collection, query_vec, where_relaxed, n=15)

    log.info("rag: found %d candidates", len(candidates))
    return {"candidates": candidates}


async def _embed(embed_url: str, text: str) -> list[float]:
    async with httpx.AsyncClient(timeout=5) as client:
        resp = await client.post(f"{embed_url}/embed", json={"inputs": [text]})
        resp.raise_for_status()
        return resp.json()[0]


async def _query_chroma(
    chroma_url: str,
    collection: str,
    query_vec: list[float],
    where: dict,
    n: int,
) -> list[ShaderCandidate]:
    async with httpx.AsyncClient(timeout=10) as client:
        resp = await client.post(
            f"{chroma_url}/api/v1/collections/{collection}/query",
            json={
                "query_embeddings": [query_vec],
                "n_results": n,
                "where": where,
                "include": ["metadatas", "distances", "documents"],
            },
        )
        resp.raise_for_status()
        data = resp.json()

    candidates: list[ShaderCandidate] = []
    for meta, dist, doc in zip(
        data["metadatas"][0],
        data["distances"][0],
        data["documents"][0],
    ):
        candidates.append(
            ShaderCandidate(
                shader_id=meta.get("shader_id", ""),
                title=meta.get("title", ""),
                author=meta.get("author", ""),
                license=meta.get("license", ""),
                description=meta.get("description", ""),
                tags=_parse_tags(meta.get("tags", "")),
                mobile_cost=int(meta.get("est_mobile_cost", 999)),
                similarity_score=float(1 - dist),
                glsl_es=doc or "",
                chroma_distance=float(dist),
            )
        )
    return candidates


def _build_where(state: RetrievalState, relax_tags: bool = False) -> dict[str, Any]:
    intent = state.router_output
    assert intent is not None
    c = state.request.constraints

    conditions: list[dict] = [
        {"compile_ok": {"$eq": 1}},
        {"license": {"$in": _ALLOWED_LICENSES}},
        {"est_mobile_cost": {"$lte": c.max_mobile_cost}},
    ]
    if intent.motion_required and not relax_tags:
        conditions.append({"motion": {"$in": ["animated", "reactive"]}})

    return {"$and": conditions} if len(conditions) > 1 else conditions[0]


def _parse_tags(raw: str) -> list[str]:
    try:
        import json
        return json.loads(raw)
    except Exception:
        return [t.strip() for t in raw.split(",") if t.strip()]

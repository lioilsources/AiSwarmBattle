from __future__ import annotations

import logging
import os
from contextlib import asynccontextmanager
from typing import Any

from fastapi import FastAPI, HTTPException
from openai import AsyncOpenAI

from .graph import build_graph
from .state import Constraints, RetrievalState, ShaderRequest

log = logging.getLogger(__name__)

_graph: Any = None
_openai_client: AsyncOpenAI | None = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    global _graph, _openai_client

    _openai_client = AsyncOpenAI(
        base_url=os.environ["LITELLM_URL"] + "/v1",
        api_key=os.environ.get("LITELLM_API_KEY", "dummy"),
    )
    _graph = build_graph(
        openai_client=_openai_client,
        embed_url=os.environ["EMBED_URL"],
        chroma_url=os.environ["CHROMA_URL"],
        evaluator_url=os.environ["EVALUATOR_URL"],
    )
    log.info("pipeline: graph compiled and ready")
    yield
    await _openai_client.close()


app = FastAPI(title="mirrorbooth-retrieval", lifespan=lifespan)


@app.post("/pipeline/retrieve")
async def retrieve(req: ShaderRequest):
    assert _graph is not None

    initial = RetrievalState(request=req)
    try:
        final: RetrievalState = await _graph.ainvoke(initial)
    except Exception as exc:
        log.exception("graph execution failed: %s", exc)
        raise HTTPException(status_code=500, detail=str(exc))

    if final.error == "no_candidates":
        raise HTTPException(status_code=422, detail={"error": "no_candidates", "message": "no shaders matched"})

    if final.fallback_initiated:
        raise HTTPException(status_code=202, detail={"status": "pending", "request_id": req.request_id})

    gate = final.eval_gate
    adapter = final.adapter_output
    if gate is None or not gate.passed or adapter is None:
        raise HTTPException(status_code=422, detail={"error": "eval_fail", "message": "no candidate passed eval gate"})

    idx = final.current_candidate_index
    candidates_by_id = {c.shader_id: c for c in final.candidates}
    ranked = [candidates_by_id[sid] for sid in final.ranked_ids if sid in candidates_by_id]
    winner = ranked[idx] if idx < len(ranked) else ranked[0]

    return {
        "request_id": req.request_id,
        "shader_id": winner.shader_id,
        "title": winner.title,
        "source": adapter.source,
        "glsl_es": adapter.glsl_es,
        "spirv_b64": gate.spirv_b64,
        "eval_score": gate.eval_score,
        "eval_version": gate.eval_version,
        "mobile_cost": winner.mobile_cost,
        "retrieval": {
            "candidates_found": len(final.candidates),
            "selected_rank": idx,
            "similarity_score": winner.similarity_score,
        },
        "provenance": {
            "original_id": winner.shader_id,
            "shadertoy_url": f"https://www.shadertoy.com/view/{winner.shader_id}",
            "license": winner.license,
            "author": winner.author,
        },
    }


@app.get("/healthz")
async def healthz():
    return {"ok": True, "graph_ready": _graph is not None}

from __future__ import annotations

import logging
import uuid

import httpx

from ..state import EvalGate, RetrievalState
from ..verify import verify

log = logging.getLogger(__name__)

_EVAL_UNIFORMS = [
    {"name": "uResolution",    "type": "vec2",      "default": [1080, 1920]},
    {"name": "uTime",          "type": "float",     "default": 0.0},
    {"name": "uCameraTexture", "type": "sampler2D", "default": None},
    {"name": "uMouse",         "type": "vec2",      "default": [0, 0]},
]

_FAST_CONFIG = {
    "test_duration_frames": 60,
    "target_resolutions": [[1080, 1920]],
    "fuzz_runs": 0,
    "skip_runtime": False,
    "skip_fuzz": True,
}


async def evaluator_node(state: RetrievalState, *, evaluator_url: str) -> dict:
    adapter = state.adapter_output
    if adapter is None:
        return {"eval_gate": EvalGate(passed=False, eval_score=0, eval_version="", spirv_b64="", median_frame_ms=0)}

    # Static pre-check (free, no network)
    v = verify(adapter.glsl_es)
    if not v.ok:
        log.info("evaluator: static verify failed: %s", v.issues)
        idx = state.current_candidate_index
        return {
            "eval_gate": EvalGate(passed=False, eval_score=0, eval_version="static", spirv_b64="", median_frame_ms=0),
            "current_candidate_index": idx + 1,
        }

    try:
        result = await _call_evaluator(evaluator_url, adapter.glsl_es, state.request.request_id)
    except Exception as exc:
        log.warning("evaluator: http error: %s", exc)
        idx = state.current_candidate_index
        return {
            "eval_gate": EvalGate(passed=False, eval_score=0, eval_version="", spirv_b64="", median_frame_ms=0),
            "current_candidate_index": idx + 1,
        }

    passed = _passes_gate(result)
    median_ms = _median_frame_ms(result)
    gate = EvalGate(
        passed=passed,
        eval_score=result.get("score", {}).get("composite", 0.0),
        eval_version=result.get("evaluator_version", ""),
        spirv_b64=result.get("compile", {}).get("spirv_b64", ""),
        median_frame_ms=median_ms,
    )

    if not passed:
        log.info(
            "evaluator: candidate %d failed gate (score=%.1f, median_ms=%.1f)",
            state.current_candidate_index, gate.eval_score, median_ms,
        )
        return {
            "eval_gate": gate,
            "current_candidate_index": state.current_candidate_index + 1,
        }

    log.info("evaluator: candidate %d passed (score=%.1f)", state.current_candidate_index, gate.eval_score)
    return {"eval_gate": gate}


async def _call_evaluator(url: str, glsl: str, request_id: str) -> dict:
    payload = {
        "request_id": request_id or str(uuid.uuid4()),
        "glsl_es": glsl,
        "uniforms": _EVAL_UNIFORMS,
        "config": _FAST_CONFIG,
    }
    async with httpx.AsyncClient(timeout=12) as client:
        resp = await client.post(f"{url}/evaluate", json=payload)
        resp.raise_for_status()
        return resp.json()


def _passes_gate(result: dict) -> bool:
    if not result.get("compile", {}).get("ok"):
        return False
    if result.get("score", {}).get("composite", 0) < 40:
        return False
    for res in result.get("runtime", {}).get("per_resolution", []):
        if res.get("nan_inf_detected"):
            return False
        if res.get("median_frame_ms", 999) > 8:
            return False
    return True


def _median_frame_ms(result: dict) -> float:
    resolutions = result.get("runtime", {}).get("per_resolution", [])
    if not resolutions:
        return 0.0
    return resolutions[0].get("median_frame_ms", 0.0)

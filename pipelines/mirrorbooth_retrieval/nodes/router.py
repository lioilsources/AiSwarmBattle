from __future__ import annotations

import json
import logging
from pathlib import Path

from openai import AsyncOpenAI
from pydantic import ValidationError

from ..state import RetrievalState, RouterOutput

log = logging.getLogger(__name__)

_PROMPT = (Path(__file__).parent.parent / "prompts" / "router.txt").read_text()


async def router_node(state: RetrievalState, *, client: AsyncOpenAI) -> dict:
    req = state.request
    user_msg = f"Prompt: {req.prompt}\nmax_mobile_cost: {req.constraints.max_mobile_cost}"

    resp = await client.chat.completions.create(
        model="swarm-nano",
        messages=[
            {"role": "system", "content": _PROMPT},
            {"role": "user", "content": user_msg},
        ],
        response_format={"type": "json_object"},
        temperature=0.0,
        seed=req.seed,
        extra_body={"chat_template_kwargs": {"enable_thinking": False}},
    )

    raw = resp.choices[0].message.content or "{}"
    try:
        router_out = RouterOutput.model_validate(json.loads(raw))
    except (json.JSONDecodeError, ValidationError) as exc:
        log.warning("router parse failed: %s — raw: %.200s", exc, raw)
        router_out = _fallback_router(req.prompt, req.constraints.require_camera)

    log.info("router: query=%r techniques=%s", router_out.chroma_query, router_out.technique_tags)
    return {"router_output": router_out}


def _fallback_router(prompt: str, camera: bool) -> RouterOutput:
    return RouterOutput(
        chroma_query=prompt,
        technique_tags=["post_process"],
        visual_tags=["abstract"],
        motion_required=True,
        camera_required=camera,
        complexity="medium",
    )

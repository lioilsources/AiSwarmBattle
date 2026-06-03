from __future__ import annotations

import json
import logging
from pathlib import Path

from openai import AsyncOpenAI
from pydantic import BaseModel, ValidationError

from ..state import RetrievalState

log = logging.getLogger(__name__)

_PROMPT = (Path(__file__).parent.parent / "prompts" / "selector.txt").read_text()

_MAX_CANDIDATES_TO_RANK = 5


class SelectorOutput(BaseModel):
    ranked_ids: list[str]
    reasoning: list[str]


async def selector_node(state: RetrievalState, *, client: AsyncOpenAI) -> dict:
    candidates = state.candidates[:_MAX_CANDIDATES_TO_RANK]
    if not candidates:
        return {"ranked_ids": [], "error": "no_candidates"}

    summaries = [
        f"{i+1}. [{c.shader_id}] \"{c.title}\" by {c.author}\n"
        f"   desc: {c.description[:120]}\n"
        f"   tags: {', '.join(c.tags[:8])}\n"
        f"   mobile_cost: {c.mobile_cost}, similarity: {c.similarity_score:.3f}, license: {c.license}"
        for i, c in enumerate(candidates)
    ]

    user_msg = (
        f"User wants: {state.request.prompt}\n"
        f"require_camera: {state.request.constraints.require_camera}\n\n"
        + "\n\n".join(summaries)
    )

    resp = await client.chat.completions.create(
        model="swarm-nano",
        messages=[
            {"role": "system", "content": _PROMPT},
            {"role": "user", "content": user_msg},
        ],
        response_format={"type": "json_object"},
        temperature=0.0,
        seed=state.request.seed,
        extra_body={"chat_template_kwargs": {"enable_thinking": False}},
    )

    raw = resp.choices[0].message.content or "{}"
    try:
        out = SelectorOutput.model_validate(json.loads(raw))
        ranked = out.ranked_ids
    except (json.JSONDecodeError, ValidationError):
        log.warning("selector parse failed — using similarity order")
        ranked = [c.shader_id for c in candidates]

    log.info("selector: ranked %d candidates, top=%s", len(ranked), ranked[:1])
    return {"ranked_ids": ranked}

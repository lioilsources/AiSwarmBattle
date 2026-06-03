from __future__ import annotations

import logging

from openai import AsyncOpenAI

from ..state import RetrievalState

log = logging.getLogger(__name__)


async def fallback_node(state: RetrievalState, *, client: AsyncOpenAI) -> dict:
    """Mark state for async Director fallback. Actual generation runs out-of-band."""
    log.warning(
        "fallback: all RAG candidates exhausted for request %s — Director fallback required",
        state.request.request_id,
    )
    # The server layer catches fallback_initiated=True and returns HTTP 202.
    # Director generation is enqueued separately (not in this graph).
    return {"fallback_initiated": True}

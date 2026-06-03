from __future__ import annotations

import logging
from functools import partial
from typing import Any

from langgraph.graph import END, StateGraph
from openai import AsyncOpenAI

from .nodes.adapter import adapter_node
from .nodes.rag import rag_node
from .nodes.router import router_node
from .nodes.selector import selector_node
from .state import RetrievalState

log = logging.getLogger(__name__)


def _route_after_eval(state: RetrievalState) -> str:
    gate = state.eval_gate
    if gate is not None and gate.passed:
        return END

    if state.fallback_initiated:
        return "fallback"

    next_idx = state.current_candidate_index + 1
    if next_idx >= len(state.ranked_ids):
        log.warning("eval: all candidates exhausted — triggering fallback")
        return "fallback"

    return "adapter"


def _route_after_adapter(state: RetrievalState) -> str:
    if state.fallback_initiated or state.adapter_output is None:
        return "fallback"
    return "evaluator"


def build_graph(
    *,
    openai_client: AsyncOpenAI,
    embed_url: str,
    chroma_url: str,
    evaluator_url: str,
) -> Any:
    """Build the LangGraph retrieval state machine."""

    from .nodes.evaluator import evaluator_node  # local import — avoids httpx at module level
    from .nodes.fallback import fallback_node

    builder = StateGraph(RetrievalState)

    builder.add_node("router",    partial(router_node,    client=openai_client))
    builder.add_node("rag",       partial(rag_node,       embed_url=embed_url, chroma_url=chroma_url))
    builder.add_node("selector",  partial(selector_node,  client=openai_client))
    builder.add_node("adapter",   partial(adapter_node,   client=openai_client))
    builder.add_node("evaluator", partial(evaluator_node, evaluator_url=evaluator_url))
    builder.add_node("fallback",  partial(fallback_node,  client=openai_client))

    builder.set_entry_point("router")
    builder.add_edge("router",   "rag")
    builder.add_edge("rag",      "selector")
    builder.add_edge("selector", "adapter")

    builder.add_conditional_edges("adapter",   _route_after_adapter)
    builder.add_conditional_edges("evaluator", _route_after_eval)

    # Fallback always goes to END (async path — server handles 202)
    builder.add_edge("fallback", END)

    return builder.compile()

"""CLI entry point for the MirrorBooth retrieval pipeline.

Usage:
    python -m mirrorbooth_retrieval.run \\
        --prompt "dreamy neon bokeh camera overlay" \\
        --name "neon_bokeh" \\
        --mirrorbooth-root /path/to/MirrorBooth/mirrorbooth \\
        --require-camera \\
        --label "Neon" \\
        --icon "✨"
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys
from pathlib import Path

from openai import AsyncOpenAI

from .graph import build_graph
from .integrator import MirrorBoothIntegrator
from .state import Constraints, RetrievalState, ShaderRequest

logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
log = logging.getLogger(__name__)


def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Retrieve and install a MirrorBooth shader from ShaderToy KB")
    p.add_argument("--prompt",           required=True,                  help="Natural language effect description")
    p.add_argument("--name",             required=True,                  help="Output shader name, e.g. 'neon_bokeh' (no extension)")
    p.add_argument("--mirrorbooth-root", required=True, type=Path,       help="Path to mirrorbooth/ Flutter project root")
    p.add_argument("--label",            default="",                     help="Short display label for the filter (default: title-case of name)")
    p.add_argument("--icon",             default="🎨",                   help="Single unicode icon character")
    p.add_argument("--require-camera",   action="store_true",            help="Shader must use camera passthrough")
    p.add_argument("--require-animated", action="store_true",            help="Shader must be animated (uses uTime)")
    p.add_argument("--max-mobile-cost",  type=int, default=200,          help="Max estimated mobile GPU cost")
    p.add_argument("--seed",             type=int, default=0)
    p.add_argument("--no-install",       action="store_true",            help="Run pipeline but skip Flutter integration")
    p.add_argument("--dry-run",          action="store_true",            help="Print output GLSL to stdout, no file writes")
    return p.parse_args()


async def _run(args: argparse.Namespace) -> int:
    client = AsyncOpenAI(
        base_url=os.environ["LITELLM_URL"] + "/v1",
        api_key=os.environ.get("LITELLM_API_KEY", "dummy"),
    )

    graph = build_graph(
        openai_client=client,
        embed_url=os.environ["EMBED_URL"],
        chroma_url=os.environ["CHROMA_URL"],
        evaluator_url=os.environ.get("EVALUATOR_URL", ""),
    )

    constraints = Constraints(
        max_mobile_cost=args.max_mobile_cost,
        require_camera=args.require_camera,
        require_animated=args.require_animated,
    )
    request = ShaderRequest(
        request_id=f"cli-{args.name}",
        prompt=args.prompt,
        constraints=constraints,
        seed=args.seed,
    )

    log.info("running pipeline for prompt: %r", args.prompt)
    initial = RetrievalState(request=request)
    final: RetrievalState = await graph.ainvoke(initial)

    if final.error == "no_candidates":
        log.error("no candidates found in ChromaDB — is the database populated?")
        return 1

    if final.fallback_initiated:
        log.error("all candidates failed eval — Director fallback not implemented in CLI yet")
        return 1

    adapter = final.adapter_output
    if adapter is None or final.eval_gate is None or not final.eval_gate.passed:
        log.error("pipeline did not produce a valid shader")
        return 1

    glsl = adapter.glsl_es
    needs_time = "uTime" in glsl

    if args.dry_run:
        print(glsl)
        return 0

    label = args.label or args.name.replace("_", " ").title()
    shader_name = f"filter_{args.name}" if not args.name.startswith("filter_") else args.name

    if not args.no_install:
        integrator = MirrorBoothIntegrator(args.mirrorbooth_root)
        installed = integrator.install(
            name=shader_name,
            frag_content=glsl.encode(),
            needs_time=needs_time,
            label=label,
            icon=args.icon,
        )
        if installed:
            log.info("installed: %s/shaders/%s.frag", args.mirrorbooth_root, shader_name)
            log.info("next step: cd %s && flutter build apk --debug", args.mirrorbooth_root.parent)
        else:
            log.info("already installed — no changes")
    else:
        out = Path(f"{shader_name}.frag")
        out.write_text(glsl)
        log.info("wrote: %s", out)

    return 0


def main() -> None:
    args = _parse_args()
    sys.exit(asyncio.run(_run(args)))


if __name__ == "__main__":
    main()

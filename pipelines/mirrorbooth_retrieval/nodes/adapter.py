from __future__ import annotations

import json
import logging
import re
from pathlib import Path

from openai import AsyncOpenAI
from pydantic import ValidationError

from ..state import AdapterOutput, RetrievalState

log = logging.getLogger(__name__)

_PROMPT = (Path(__file__).parent.parent / "prompts" / "adapter.txt").read_text()

# Flutter/Impeller preamble — no #version (Flutter injects it), uses FlutterFragCoord().
_PREAMBLE_STATIC = """\
#include <flutter/runtime_effect.glsl>

uniform sampler2D uTexture;
uniform vec2      uResolution;

out vec4 fragColor;
"""

_PREAMBLE_ANIMATED = """\
#include <flutter/runtime_effect.glsl>

uniform sampler2D uTexture;
uniform vec2      uResolution;
uniform float     uTime;

out vec4 fragColor;
"""

# Mechanical substitutions applied deterministically before any LLM pass.
_STATIC_RENAMES: list[tuple[str, str]] = [
    # ShaderToy uniforms → MirrorBooth uniforms
    (r"\biResolution\.xy\b",  "uResolution"),
    (r"\biResolution\.z\b",   "1.0"),
    (r"\biResolution\b",      "vec3(uResolution, 1.0)"),
    (r"\biTime\b",            "uTime"),
    (r"\biGlobalTime\b",      "uTime"),
    (r"\biTimeDelta\b",       "0.016"),
    (r"\biFrame\b",           "int(uTime * 60.0)"),
    (r"\biDate\b",            "vec4(0.0)"),
    (r"\biSampleRate\b",      "44100.0"),
    # Legacy GLSL → ES 3.0
    (r"\bgl_FragColor\b",     "fragColor"),
    (r"\btexture2D\s*\(",     "texture("),
    (r"\btextureCube\s*\(",   "texture("),
    # Flutter: gl_FragCoord → FlutterFragCoord()
    (r"\bgl_FragCoord\b",     "FlutterFragCoord()"),
    # iMouse is unsupported in MirrorBooth — replace with zero to avoid undefined
    (r"\biMouse\.xy\b",       "vec2(0.0)"),
    (r"\biMouse\b",           "vec4(0.0)"),
]

_CHANNEL0_RE = re.compile(r"\biChannel0\b")
_CHANNEL_OTHER_RE = re.compile(r"\biChannel[1-3]\b")
# Detects Shadertoy mainImage signature (brace-balanced conversion follows)
_MAIN_IMAGE_RE = re.compile(
    r"void\s+mainImage\s*\(\s*out\s+vec4\s+\w+\s*,\s*in\s+vec2\s+\w+\s*\)"
)


def apply_static_renames(glsl: str) -> tuple[str, list[str]]:
    notes: list[str] = []
    for pattern, replacement in _STATIC_RENAMES:
        new = re.sub(pattern, replacement, glsl)
        if new != glsl:
            notes.append(f"{pattern} → {replacement}")
            glsl = new
    return glsl, notes


def _convert_main_image(glsl: str) -> tuple[str, bool]:
    """Replace mainImage(out vec4 fragColor, in vec2 fragCoord) with void main().

    Inserts `vec2 fragCoord = FlutterFragCoord().xy;` as first statement.
    Returns (converted_glsl, was_converted).
    """
    m = _MAIN_IMAGE_RE.search(glsl)
    if not m:
        return glsl, False

    # Find the opening brace of mainImage body
    brace_start = glsl.find("{", m.end())
    if brace_start == -1:
        return glsl, False

    replacement_sig = "void main()"
    injection = "\n    vec2 fragCoord = FlutterFragCoord().xy;"
    converted = (
        glsl[: m.start()]
        + replacement_sig
        + glsl[m.end() : brace_start + 1]
        + injection
        + glsl[brace_start + 1 :]
    )
    return converted, True


async def adapter_node(state: RetrievalState, *, client: AsyncOpenAI) -> dict:
    idx = state.current_candidate_index
    candidates_by_id = {c.shader_id: c for c in state.candidates}
    ranked = [candidates_by_id[sid] for sid in state.ranked_ids if sid in candidates_by_id]

    if idx >= len(ranked):
        log.warning("adapter: exhausted all %d candidates", len(ranked))
        return {"adapter_output": None, "fallback_initiated": True}

    candidate = ranked[idx]
    log.info("adapter: candidate %d/%d shader_id=%s", idx + 1, len(ranked), candidate.shader_id)

    glsl, static_notes = apply_static_renames(candidate.glsl_es)

    # mainImage → void main() with FlutterFragCoord injection
    glsl, converted = _convert_main_image(glsl)
    if converted:
        static_notes.append("mainImage → void main() + FlutterFragCoord()")

    # Wire iChannel0 → uTexture
    camera_wired = False
    if _CHANNEL0_RE.search(glsl):
        glsl = _CHANNEL0_RE.sub("uTexture", glsl)
        static_notes.append("iChannel0 → uTexture")
        camera_wired = True
    elif state.request.constraints.require_camera:
        log.info("adapter: shader %s has no iChannel0 — camera not wired", candidate.shader_id)

    # Replace remaining iChannel[1-3] with uTexture (fallback — best effort)
    if _CHANNEL_OTHER_RE.search(glsl):
        glsl = _CHANNEL_OTHER_RE.sub("uTexture", glsl)
        static_notes.append("iChannel[1-3] → uTexture (fallback)")

    # Choose preamble based on whether shader uses time
    uses_time = "uTime" in glsl
    preamble = _PREAMBLE_ANIMATED if uses_time else _PREAMBLE_STATIC
    glsl_body = _strip_preamble(glsl)
    glsl_final = preamble + "\n" + glsl_body

    source: str = "adapted" if static_notes else "retrieved"

    if _needs_llm_pass(glsl_final):
        glsl_final, llm_notes = await _llm_adapt(client, glsl_final, state.request.prompt)
        static_notes.extend(llm_notes)
        source = "adapted"

    return {
        "adapter_output": AdapterOutput(
            glsl_es=glsl_final,
            diff_notes=static_notes,
            source=source,
            camera_wired=camera_wired,
        )
    }


def _strip_preamble(glsl: str) -> str:
    skip_prefixes = (
        "#version",
        "#include <flutter/runtime_effect.glsl>",
        "precision ",
        "uniform vec2 uResolution",
        "uniform float uTime",
        "uniform sampler2D uTexture",
        "uniform sampler2D uCameraTexture",  # handle old name too
        "uniform vec2 uMouse",
        "out vec4 fragColor",
    )
    body_lines: list[str] = []
    for line in glsl.splitlines():
        if any(line.strip().startswith(p) for p in skip_prefixes):
            continue
        body_lines.append(line)
    return "\n".join(body_lines)


def _needs_llm_pass(glsl: str) -> bool:
    return bool(
        re.search(r"\biChannel[0-3]\b", glsl)
        or re.search(r"\biResolution\b", glsl)
        or re.search(r"\biTime\b", glsl)
        or re.search(r"\biGlobalTime\b", glsl)
        or _MAIN_IMAGE_RE.search(glsl)  # unconverted mainImage
    )


async def _llm_adapt(client: AsyncOpenAI, glsl: str, original_prompt: str) -> tuple[str, list[str]]:
    user_msg = (
        f"Original effect request: {original_prompt}\n\n"
        f"Shader to fix:\n```glsl\n{glsl}\n```\n\n"
        'Apply ONLY the mechanical renames. Output JSON: {"glsl_es": "...", "diff_notes": [...]}'
    )
    resp = await client.chat.completions.create(
        model="swarm-coder",
        messages=[
            {"role": "system", "content": _PROMPT},
            {"role": "user", "content": user_msg},
        ],
        response_format={"type": "json_object"},
        temperature=0.0,
        extra_body={"chat_template_kwargs": {"enable_thinking": False}},
    )
    raw = resp.choices[0].message.content or "{}"
    try:
        data = json.loads(raw)
        return data.get("glsl_es", glsl), data.get("diff_notes", [])
    except (json.JSONDecodeError, ValidationError):
        return glsl, []

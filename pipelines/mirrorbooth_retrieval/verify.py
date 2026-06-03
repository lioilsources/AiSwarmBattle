from __future__ import annotations

import re
from dataclasses import dataclass, field


@dataclass
class VerifyResult:
    ok: bool
    issues: list[str] = field(default_factory=list)


# Required: either the static or animated preamble must be present.
_REQUIRED_ELEMENTS = [
    ("#include <flutter/runtime_effect.glsl>", "missing Flutter runtime include"),
    ("uniform sampler2D uTexture",            "missing required uniform: uTexture"),
    ("uniform vec2 uResolution",              "missing required uniform: uResolution"),
    ("out vec4 fragColor",                    "missing 'out vec4 fragColor'"),
    ("void main()",                           "missing 'void main()'"),
    ("FlutterFragCoord()",                    "missing FlutterFragCoord() — use it instead of gl_FragCoord"),
]

_FORBIDDEN: list[tuple[str, str]] = [
    ("#version",        "forbidden '#version' — Flutter/Impeller injects it"),
    ("gl_FragColor",    "use out vec4 fragColor instead"),
    ("gl_FragCoord",    "use FlutterFragCoord() instead"),
    ("texture2D(",      "use texture() for GLSL ES 3.0"),
    ("textureCube(",    "use texture() for GLSL ES 3.0"),
    ("varying ",        "use in/out instead of varying"),
    ("attribute ",      "use in instead of attribute"),
]

_LEGACY_ITIME = re.compile(r"\biTime\b")
_LEGACY_IRES  = re.compile(r"\biResolution\b")
_LEGACY_ICHAN = re.compile(r"\biChannel[0-3]\b")
_MAIN_IMAGE   = re.compile(r"\bmainImage\s*\(")


def verify(glsl: str) -> VerifyResult:
    issues: list[str] = []

    for token, msg in _REQUIRED_ELEMENTS:
        if token not in glsl:
            issues.append(msg)

    for token, msg in _FORBIDDEN:
        if token in glsl:
            issues.append(f"forbidden '{token}': {msg}")

    for pattern, label in [
        (_LEGACY_ITIME, "iTime"),
        (_LEGACY_IRES,  "iResolution"),
        (_LEGACY_ICHAN, "iChannel[0-3]"),
    ]:
        if pattern.search(glsl):
            issues.append(f"unreplaced ShaderToy uniform: {label}")

    if _MAIN_IMAGE.search(glsl):
        issues.append("unconverted mainImage — must be replaced with void main()")

    return VerifyResult(ok=len(issues) == 0, issues=issues)

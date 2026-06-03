from __future__ import annotations

from typing import Annotated, Literal
from pydantic import BaseModel
import operator


class Constraints(BaseModel):
    max_mobile_cost: int = 200
    require_camera: bool = False
    require_animated: bool = False
    aspect_ratio: Literal["portrait", "landscape"] = "portrait"
    allow_source: list[str] = ["retrieved", "adapted", "generated"]


class ShaderRequest(BaseModel):
    request_id: str
    prompt: str
    constraints: Constraints
    seed: int = 0


class RouterOutput(BaseModel):
    chroma_query: str
    technique_tags: list[str]
    visual_tags: list[str]
    motion_required: bool
    camera_required: bool
    complexity: Literal["cheap", "medium", "expensive"]


class ShaderCandidate(BaseModel):
    shader_id: str
    title: str
    author: str
    license: str
    description: str
    tags: list[str]
    mobile_cost: int
    similarity_score: float
    glsl_es: str
    chroma_distance: float


class AdapterOutput(BaseModel):
    glsl_es: str
    diff_notes: list[str]
    source: Literal["retrieved", "adapted"]
    camera_wired: bool


class EvalGate(BaseModel):
    passed: bool
    eval_score: float
    eval_version: str
    spirv_b64: str
    median_frame_ms: float


class RetrievalState(BaseModel):
    request: ShaderRequest

    router_output: RouterOutput | None = None
    candidates: list[ShaderCandidate] = []
    ranked_ids: list[str] = []

    current_candidate_index: int = 0
    adapter_output: AdapterOutput | None = None
    eval_gate: EvalGate | None = None

    # Set when Director fallback is triggered
    fallback_initiated: bool = False
    error: str | None = None

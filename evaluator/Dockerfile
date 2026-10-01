# Evaluator image (linux/arm64 for DGX Spark; builds on amd64 too).
# Build from the repository root:
#   docker build -f evaluator/Dockerfile -t swarm-evaluator .
# Run unit + EGL (llvmpipe) tests:
#   docker build -f evaluator/Dockerfile --target test .
#
# GPU: run with the NVIDIA container runtime and
# NVIDIA_DRIVER_CAPABILITIES=graphics,utility so libEGL_nvidia is injected;
# glvnd then exposes the GPU as an EGL device. Without it, Mesa llvmpipe is
# used and timings are flagged software_renderer=true.

# glslang pinned: SPIR-V output and diagnostics must be reproducible.
# Same version as local dev (Homebrew) so test expectations match.
ARG GLSLANG_VERSION=16.6.0

FROM debian:bookworm-slim AS glslang
ARG GLSLANG_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git cmake g++ make python3 \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 -b ${GLSLANG_VERSION} https://github.com/KhronosGroup/glslang.git /tmp/glslang \
    && cmake -S /tmp/glslang -B /tmp/glslang/build \
        -DCMAKE_BUILD_TYPE=Release -DENABLE_OPT=OFF -DGLSLANG_TESTS=OFF \
        -DBUILD_SHARED_LIBS=OFF -DCMAKE_INSTALL_PREFIX=/opt/glslang \
    && cmake --build /tmp/glslang/build -j"$(nproc)" --target glslang-standalone \
    && install -D /tmp/glslang/build/StandAlone/glslang /opt/glslang/bin/glslangValidator \
    && /opt/glslang/bin/glslangValidator --version

FROM golang:1.23-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
        libegl-dev libgles-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY pkg/ pkg/
COPY evaluator/ evaluator/
WORKDIR /src/evaluator
ENV GOWORK=off CGO_ENABLED=1
RUN go build -trimpath -o /out/evaluator ./cmd/evaluator

FROM build AS test
RUN apt-get update && apt-get install -y --no-install-recommends \
        libegl-mesa0 libgl1-mesa-dri \
    && rm -rf /var/lib/apt/lists/*
COPY --from=glslang /opt/glslang/bin/glslangValidator /usr/local/bin/
ENV EVALUATOR_TEST_EGL=software EGL_PLATFORM=surfaceless
RUN go vet ./... && go test -v ./... 2>&1 | grep -v "^=== RUN" && go test ./...

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        libegl1 libgles2 libegl-mesa0 libgl1-mesa-dri curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=glslang /opt/glslang/bin/glslangValidator /usr/local/bin/
COPY --from=build /out/evaluator /usr/local/bin/evaluator
ENV EVALUATOR_ADDR=:8081 \
    EVALUATOR_EGL=auto \
    NVIDIA_DRIVER_CAPABILITIES=graphics,utility
EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=5s CMD curl -sf http://localhost:8081/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/evaluator"]

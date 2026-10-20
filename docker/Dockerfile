# syntax=docker/dockerfile:1
# The agent's container: Go to build and test, git, Codex, and the workbench itself (-inside,
# -inside-probe, egress). Build from the repo root, with Codex's Linux binary as a context of
# its own (see README):
#   docker build --build-context codex=<folder with codex-x86_64-unknown-linux-musl> \
#     -t verstak-agent:latest -f docker/Dockerfile .

FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/verstak ./cmd/verstak \
 && CGO_ENABLED=0 go build -trimpath -o /out/egress ./cmd/egress

FROM golang:1.27.1-trixie
COPY --from=codex --chmod=0755 codex-x86_64-unknown-linux-musl /usr/local/bin/codex
COPY --from=build /out/ /usr/local/bin/
# The agent's user owns /work: a fresh volume mounted there takes this owner.
RUN useradd --uid 10001 --create-home --shell /bin/bash agent \
 && mkdir -p /work && chown agent:agent /work \
 && git config --system safe.directory '*'
USER 10001
ENV HOME=/home/agent GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=readonly \
    GOCACHE=/work/cache/go-build CODEX_HOME=/work/codex
WORKDIR /work

# A multi-stage build producing a distroless container image
# containing only the statically linked Sortie binary.
#
# Usage:
#
#   docker build -t sortie .
#   docker build --build-arg VERSION=1.5.0 -t sortie .
#
# Consume in your own Dockerfile:
#
#   COPY --from=ghcr.io/sortie-ai/sortie:latest /usr/bin/sortie /usr/bin/sortie

# ── Builder stage ─────────────────────────────────────────────────────────────

# Run the builder on the host architecture; Go cross-compiles natively
# via GOOS/GOARCH, so there is no need for QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

# Cache module downloads separately from the build so source changes
# do not re-download dependencies.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Build flags match .goreleaser.yaml: static binary, stripped, version injected.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -trimpath \
    -tags osusergo,netgo \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /sortie \
    ./cmd/sortie

# ── Runtime stage ─────────────────────────────────────────────────────────────

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /sortie /usr/bin/sortie

ENTRYPOINT ["/usr/bin/sortie"]
CMD ["--version"]

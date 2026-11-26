FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git wget && \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    debian_arch="$(dpkg --print-architecture)"; \
    case "${debian_arch}" in \
    amd64) codex_arch="x86_64-unknown-linux-musl" ;; \
    arm64) codex_arch="aarch64-unknown-linux-musl" ;; \
    *) echo "unsupported Codex architecture: ${debian_arch}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/openai/codex/releases/latest/download/codex-${codex_arch}.tar.gz" \
    | tar -xzO "codex-${codex_arch}" > /usr/local/bin/codex; \
    chmod +x /usr/local/bin/codex

RUN useradd --create-home --shell /bin/bash --uid 1000 sortie

COPY --from=sortie /usr/bin/sortie /usr/bin/sortie

USER sortie
WORKDIR /home/sortie

# Wins over workspace.root, so workspaces land on the mounted volume.
ENV SORTIE_WORKSPACE_ROOT=/home/sortie/workspaces

EXPOSE 7678

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO /dev/null http://localhost:7678/readyz || exit 1

# --port wins over server.port, so the server listens where the health check probes.
ENTRYPOINT ["/usr/bin/sortie", "--host", "0.0.0.0", "--port", "7678", "--log-format", "json"]

FROM ghcr.io/sortie-ai/sortie:latest AS sortie

# Kiro CLI links against glibc, so a musl base cannot run it.
FROM debian:bookworm-slim

# The Kiro installer unpacks a zip.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git unzip wget && \
    rm -rf /var/lib/apt/lists/*

# The installer writes to root's ~/.local/bin, which the sortie user cannot reach.
RUN set -eux; \
    curl -fsSL https://cli.kiro.dev/install | bash; \
    mv /root/.local/bin/kiro-cli /usr/local/bin/kiro-cli; \
    rm -rf /root/.local

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

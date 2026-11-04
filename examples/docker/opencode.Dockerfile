FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM node:24-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git wget && \
    rm -rf /var/lib/apt/lists/*

# Sortie refuses an OpenCode major after 2 at session start.
RUN npm install -g @opencode/cli@2 && npm cache clean --force

# node:24-slim ships a "node" user at UID 1000.
RUN userdel -r node 2>/dev/null; \
    useradd --create-home --shell /bin/bash --uid 1000 sortie

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

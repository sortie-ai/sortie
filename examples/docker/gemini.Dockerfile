# examples/docker/gemini.Dockerfile
#
# Complete working example: Sortie + Gemini CLI agent.
#
# Gemini CLI requires Node.js (>= 20). Sortie drives it through the
# agent-client-protocol kind, so agent.command in WORKFLOW.md names
# `gemini --acp` (see examples/WORKFLOW.agent-client-protocol.md). The
# container runs as a non-root user for security best practices.
#
# Build:
#   docker build -f examples/docker/gemini.Dockerfile -t sortie-gemini .
#
# Run:
#   docker run --rm --init \
#     -e GEMINI_API_KEY \
#     -v "$(pwd)/workspaces:/home/sortie/workspaces" \
#     -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
#     -p 127.0.0.1:7678:7678 \
#     sortie-gemini /home/sortie/WORKFLOW.md

FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM node:24-slim

# Install git for repository-backed runs and wget for the health check.
RUN apt-get update && apt-get install -y --no-install-recommends \
    git wget && \
    rm -rf /var/lib/apt/lists/*

# Install Gemini CLI globally.
RUN npm install -g @google/gemini-cli@latest && npm cache clean --force

# Create a non-root user. The node base image ships a "node" user at UID 1000;
# remove it so we can claim that UID for the sortie user.
RUN userdel -r node 2>/dev/null; \
    useradd --create-home --shell /bin/bash --uid 1000 sortie

# Copy the Sortie binary from the distroless image.
COPY --from=sortie /usr/bin/sortie /usr/bin/sortie

# Switch to the non-root user for all subsequent operations.
USER sortie
WORKDIR /home/sortie

# Keep per-issue workspaces on the mounted volume, whatever workspace.root
# the workflow sets.
ENV SORTIE_WORKSPACE_ROOT=/home/sortie/workspaces

# The HTTP observability server listens on all interfaces, on the port the
# health check probes, whatever server.port the workflow sets, so the host
# can reach it through the published port.
EXPOSE 7678

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO /dev/null http://localhost:7678/readyz || exit 1

ENTRYPOINT ["/usr/bin/sortie", "--host", "0.0.0.0", "--port", "7678", "--log-format", "json"]

# examples/docker/codex.Dockerfile
#
# Complete working example: Sortie + OpenAI Codex CLI agent.
#
# Codex CLI is a statically linked Rust binary — no Node.js runtime required.
# The container runs as a non-root user for security best practices.
#
# Build:
#   docker build -f examples/docker/codex.Dockerfile -t sortie-codex .
#
# Run:
#   docker run --rm --init \
#     -e CODEX_API_KEY \
#     -v "$(pwd)/workspaces:/home/sortie/workspaces" \
#     -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
#     -p 7678:7678 \
#     sortie-codex /home/sortie/WORKFLOW.md

FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM debian:bookworm-slim

# Install git (Codex requires a git repo) and download utilities.
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates curl wget && \
    rm -rf /var/lib/apt/lists/*

# Install Codex CLI. The Rust binary is statically linked and does not
# require Node.js.
RUN curl -fsSL https://github.com/openai/codex/releases/latest/download/codex-linux-$(dpkg --print-architecture).tar.gz \
    | tar -xz -C /usr/local/bin codex && \
    chmod +x /usr/local/bin/codex

# Create a non-root user.
RUN useradd --create-home --shell /bin/bash --uid 1000 sortie

# Copy the Sortie binary from the distroless image.
COPY --from=sortie /usr/bin/sortie /usr/bin/sortie

# Switch to the non-root user for all subsequent operations.
USER sortie
WORKDIR /home/sortie

# The HTTP observability server listens on all interfaces so the host
# can reach it through the published port.
EXPOSE 7678

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO /dev/null http://localhost:7678/readyz || exit 1

ENTRYPOINT ["/usr/bin/sortie", "--host", "0.0.0.0", "--log-format", "json"]

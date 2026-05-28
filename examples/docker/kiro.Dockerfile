# examples/docker/kiro.Dockerfile
#
# Complete working example: Sortie + Kiro CLI agent.
#
# Kiro CLI is a standalone binary dynamically linked against glibc, installed
# from its official channel. A glibc base image (debian:bookworm-slim) is
# required; musl-based images cannot run the binary. The container runs as a
# non-root user for security best practices.
#
# Build:
#   docker build -f examples/docker/kiro.Dockerfile -t sortie-kiro .
#
# Run:
#   docker run --rm --init \
#     -e KIRO_API_KEY \
#     -v "$(pwd)/workspaces:/home/sortie/workspaces" \
#     -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
#     -p 7678:7678 \
#     sortie-kiro /home/sortie/WORKFLOW.md

FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM debian:bookworm-slim

# Install git (for repository-backed runs), download utilities, and unzip
# (required by the Kiro CLI installer to extract the Linux package).
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates curl wget unzip && \
    rm -rf /var/lib/apt/lists/*

# Install Kiro CLI via the official install channel. The installer drops the
# binary into $HOME/.local/bin; relocate it to /usr/local/bin so it is on PATH
# for the non-root runtime user.
RUN set -eux; \
    curl -fsSL https://cli.kiro.dev/install | bash; \
    mv /root/.local/bin/kiro-cli /usr/local/bin/kiro-cli; \
    mv /root/.local/bin/kiro-cli-chat /usr/local/bin/kiro-cli-chat; \
    chmod +x /usr/local/bin/kiro-cli /usr/local/bin/kiro-cli-chat; \
    rm -rf /root/.local

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

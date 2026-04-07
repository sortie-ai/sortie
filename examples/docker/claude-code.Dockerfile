# examples/docker/claude-code.Dockerfile
#
# Complete working example: Sortie + Claude Code agent.
#
# Claude Code requires Node.js (>= 18) and npm. The --dangerously-skip-permissions
# flag refuses to run under root, so the container runs as a non-root user.
#
# Build:
#   docker build -f examples/docker/claude-code.Dockerfile -t sortie-claude .
#
# Run:
#   docker run --rm --init \
#     -e ANTHROPIC_API_KEY \
#     -v "$(pwd)/workspaces:/home/sortie/workspaces" \
#     -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
#     -p 7678:7678 \
#     sortie-claude /home/sortie/WORKFLOW.md

FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM node:24-slim

# Install Claude Code globally.
RUN npm install -g @anthropic-ai/claude-code@latest && npm cache clean --force

# Create a non-root user. Claude Code's --dangerously-skip-permissions mode
# refuses to run as root. The node base image ships a "node" user at UID 1000;
# remove it so we can claim that UID for the sortie user.
RUN userdel -r node 2>/dev/null; \
    useradd --create-home --shell /bin/bash --uid 1000 sortie

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

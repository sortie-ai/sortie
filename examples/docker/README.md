# Docker Examples

Complete working Dockerfiles that pair Sortie with a coding agent. For the
full guide - volumes, health checks, persistence, process reaping, custom
agents - see
[How to use Sortie in Docker](https://docs.sortie-ai.com/guides/use-sortie-in-docker/).

## Quick start

Build an agent-specific image from the repository root:

```sh
docker build -f examples/docker/claude-code.Dockerfile -t sortie-claude .
docker build -f examples/docker/codex.Dockerfile -t sortie-codex .
docker build -f examples/docker/copilot.Dockerfile -t sortie-copilot .
docker build -f examples/docker/gemini.Dockerfile -t sortie-gemini .
docker build -f examples/docker/kiro.Dockerfile -t sortie-kiro .
docker build -f examples/docker/opencode.Dockerfile -t sortie-opencode .
```

Run it:

```sh
docker run --rm --init \
    -e ANTHROPIC_API_KEY \
    -v "$(pwd)/workspaces:/home/sortie/workspaces" \
    -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
    -p 7678:7678 \
    sortie-claude /home/sortie/WORKFLOW.md
```

## Available examples

The agent kind and `agent.command` come from the mounted `WORKFLOW.md`, so
one image serves every kind its agent speaks. Each command links to the
sample workflow that uses it.

| File | Agent | Agent kinds | `agent.command` on `agent-client-protocol` | Base image |
|---|---|---|---|---|
| `claude-code.Dockerfile` | Claude Code | `claude-code` | none | `node:24-slim` |
| `codex.Dockerfile` | OpenAI Codex CLI | `codex` | none | `debian:bookworm-slim` |
| `copilot.Dockerfile` | GitHub Copilot | `agent-client-protocol`, `copilot-cli` (deprecated) | [`copilot --acp --allow-all-tools`](../WORKFLOW.agent-client-protocol.copilot.md) | `node:24-slim` |
| `gemini.Dockerfile` | Gemini CLI | `agent-client-protocol` | [`gemini --acp --skip-trust --approval-mode yolo`](../WORKFLOW.agent-client-protocol.md) | `node:24-slim` |
| `kiro.Dockerfile` | Kiro CLI | `agent-client-protocol` | [`kiro-cli acp -a`](../WORKFLOW.agent-client-protocol.kiro.md) | `debian:bookworm-slim` |
| `opencode.Dockerfile` | OpenCode | `agent-client-protocol`, `opencode` (deprecated) | [`opencode acp`](../WORKFLOW.agent-client-protocol.opencode.md) | `node:24-slim` |

Each Dockerfile follows the same pattern: copy the Sortie binary from the
distroless image (`ghcr.io/sortie-ai/sortie`), install the agent, create a
non-root user, and set the entrypoint to Sortie. See the Dockerfiles
themselves for the full details.

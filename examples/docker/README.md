# Docker Examples

Complete working Dockerfiles that pair Sortie with a coding agent. For the full guide - volumes, health checks, persistence, process reaping, custom agents - see [How to use Sortie in Docker](https://docs.sortie-ai.com/guides/use-sortie-in-docker/).

## Quick start

Pick your agent in the table below, then build its image from the repository root. Replace `<agent>` with the file name without `.Dockerfile`, for example `kiro`:

```sh
docker build -f examples/docker/<agent>.Dockerfile -t sortie-<agent> .
```

Run it with the credential the table lists for that agent in place of `<CREDENTIAL>`, for example `KIRO_API_KEY`:

```sh
docker run --rm --init \
    -e <CREDENTIAL> \
    -v "$(pwd)/workspaces:/home/sortie/workspaces" \
    -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
    -p 127.0.0.1:7678:7678 \
    sortie-<agent> /home/sortie/WORKFLOW.md
```

## Available examples

The agent kind and `agent.command` come from the mounted `WORKFLOW.md`, so one image serves every kind its agent speaks. Each command links to the sample workflow that uses it.

| File | Agent | Credential | Agent kinds | `agent.command` on `agent-client-protocol` | Base image |
|---|---|---|---|---|---|
| `claude-code.Dockerfile` | Claude Code | `ANTHROPIC_API_KEY` | `claude-code` | none | `node:24-slim` |
| `codex.Dockerfile` | OpenAI Codex CLI | `CODEX_API_KEY` | `codex` | none | `debian:bookworm-slim` |
| `copilot.Dockerfile` | GitHub Copilot | `GITHUB_TOKEN` | `agent-client-protocol`, `copilot-cli` (deprecated) | [`copilot --acp --allow-all-tools`](../WORKFLOW.agent-client-protocol.copilot.md) | `node:24-slim` |
| `gemini.Dockerfile` | Gemini CLI | `GEMINI_API_KEY` | `agent-client-protocol` | [`gemini --acp --skip-trust --approval-mode yolo`](../WORKFLOW.agent-client-protocol.md) | `node:24-slim` |
| `kiro.Dockerfile` | Kiro CLI | `KIRO_API_KEY` starts sessions without Sortie's tools; a stored device login delivers them ([details](https://docs.sortie-ai.com/reference/agent-client-protocol-kiro/#which-login-delivers-sorties-tools)) | `agent-client-protocol` | [`kiro-cli acp -a`](../WORKFLOW.agent-client-protocol.kiro.md) | `debian:bookworm-slim` |
| `opencode.Dockerfile` | OpenCode | The key of the provider your model uses, such as `ANTHROPIC_API_KEY` | `agent-client-protocol`, `opencode` (deprecated) | [`opencode acp`](../WORKFLOW.agent-client-protocol.opencode.md) | `node:24-slim` |

Each Dockerfile follows the same pattern: copy the Sortie binary from the distroless image (`ghcr.io/sortie-ai/sortie`), install the agent, create a non-root user, and set the entrypoint to Sortie. See the Dockerfiles themselves for the full details.

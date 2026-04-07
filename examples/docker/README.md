# Docker Examples

Sortie ships as a distroless container image containing only the statically
linked binary. You compose your own image by copying the binary into whatever
base your agent stack requires.

## The COPY Pattern

The published image at `ghcr.io/sortie-ai/sortie` is a
[distroless](https://github.com/GoogleContainerTools/distroless) container with
a single file: `/usr/bin/sortie`. Consume it in a multi-stage build:

```dockerfile
FROM ghcr.io/sortie-ai/sortie:latest AS sortie

FROM node:22-slim
COPY --from=sortie /usr/bin/sortie /usr/bin/sortie
# … install your agent, create a user, etc.
```

This keeps Sortie agent-agnostic: it does not dictate your OS, package manager,
or runtime environment.

## Non-Root Requirement

Claude Code's `--dangerously-skip-permissions` mode refuses to run as root.
Every example Dockerfile creates a non-root `sortie` user (UID 1000):

```dockerfile
RUN useradd --create-home --shell /bin/bash --uid 1000 sortie
USER sortie
```

Even for agents that do not enforce this restriction, running as non-root is a
security best practice.

## Process Reaping with `--init`

Sortie handles `SIGTERM` for graceful shutdown, but orphaned grandchild
processes (e.g., agent subprocesses that outlive their parent) need an init
process for zombie reaping. Use Docker's built-in `tini` injection:

```sh
docker run --rm --init sortie-claude /home/sortie/WORKFLOW.md
```

On Kubernetes, enable `shareProcessNamespace: true` in the pod spec for the
same effect.

The distroless image intentionally omits `tini` — it contains only the Sortie
binary. If you need `tini` baked into the image, install it in your own
Dockerfile:

```dockerfile
RUN apt-get update && apt-get install -y --no-install-recommends tini \
    && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["tini", "--", "/usr/bin/sortie", "--host", "0.0.0.0"]
```

## Health Checks

The example Dockerfiles include a `HEALTHCHECK` directive that probes the
`/healthz` endpoint on the HTTP observability server:

```dockerfile
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO /dev/null http://localhost:7678/healthz || exit 1
```

The specific tool (`wget`, `curl`, etc.) depends on your base image. The
distroless image has no shell, so health checks must be defined in downstream
images.

## Volume Mounts

Sortie needs two paths at runtime:

| Path | Purpose | Mount type |
|---|---|---|
| Workspace root | Agent working directories for each issue | Read-write volume |
| `WORKFLOW.md` | Workflow configuration file | Read-only bind mount |

Example:

```sh
docker run --rm --init \
    -v "$(pwd)/workspaces:/home/sortie/workspaces" \
    -v "$(pwd)/WORKFLOW.md:/home/sortie/WORKFLOW.md:ro" \
    sortie-claude /home/sortie/WORKFLOW.md
```

The SQLite database (`.sortie.db`) is created in the working directory. If you
need persistence across container restarts, mount a volume for the working
directory or set `--db` to a path on a persistent volume.

## Building Locally

Build the distroless image:

```sh
docker build -t sortie .
```

Inject a version string:

```sh
docker build --build-arg VERSION=v1.3.0 -t sortie .
```

Build an agent-specific image (from the repository root):

```sh
docker build -f examples/docker/claude-code.Dockerfile -t sortie-claude .
docker build -f examples/docker/copilot.Dockerfile -t sortie-copilot .
```

## Available Examples

| File | Agent | Base Image |
|---|---|---|
| `claude-code.Dockerfile` | Claude Code | `node:24-slim` |
| `copilot.Dockerfile` | GitHub Copilot | `node:24-slim` |

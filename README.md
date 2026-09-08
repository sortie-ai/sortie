<p align="center">
  <img src="docs/assets/banner.jpg" alt="Sortie - Turn tracker tickets into agent sessions" width="100%">
</p>

<div align="center">


**Your next sprint isn't capped by headcount.**

The open-source coding agent orchestrator.<br/>
Run the coding agents you already use on tasks from your issue tracker - in parallel, on your own machine or server.

[![CI](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml/badge.svg)](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/sortie-ai/sortie/graph/badge.svg?token=K2TPXBCbvb)](https://codecov.io/gh/sortie-ai/sortie)
[![Go Reference](https://pkg.go.dev/badge/github.com/sortie-ai/sortie.svg)](https://pkg.go.dev/github.com/sortie-ai/sortie)

[Website](https://sortie-ai.com) · [Documentation](https://docs.sortie-ai.com) · [Contributing](CONTRIBUTING.md)

**English | [简体中文](README.zh-CN.md)**

</div>

## The Problem

Coding agents can fix bugs, update dependencies, add tests, and build features. But running agents you trust across a backlog takes more than launching sessions: isolated workspaces, retries, tracker integration, and cost tracking all need to work together.

Building and maintaining that infrastructure becomes another engineering project.

Sortie is that infrastructure.

## Works With

**Issue trackers:** GitHub Issues, GitLab Issues, Gitea Issues, Linear and Jira.

**Coding agents:** Claude Code, Copilot, OpenCode, Codex, Kiro and Gemini.

## Install

```bash
curl -sSL https://get.sortie-ai.com/install.sh | sh
```

Or via Homebrew: `brew install --cask sortie-ai/tap/sortie`

Try the [local demo](https://docs.sortie-ai.com/getting-started/quick-start/) with a simulated agent and no external services, or [connect your tracker and coding agent](https://docs.sortie-ai.com/getting-started/).

## How It Works

1. Choose which issues to work on, which agent to run, and what instructions to give it in one `WORKFLOW.md` file.
2. Sortie picks up matching issues and runs agents in parallel, each in its own workspace.
3. Failed runs are retried automatically. Enable CI and review feedback to send failures and comments back to the agent.

Run history survives restarts. You can track progress and costs, and update the workflow without restarting Sortie.

Sortie is a single binary, with no separate database or job queue to deploy. Sortie itself sends no telemetry.

## Documentation

Full configuration reference, CLI usage, and getting started guide: [docs.sortie-ai.com](https://docs.sortie-ai.com)

- [Guides](https://docs.sortie-ai.com/guides/)
- [Architecture](https://docs.sortie-ai.com/concepts/architecture/)
- [Roadmap](https://github.com/orgs/sortie-ai/projects/1)

## Prior Art

Sortie's architecture is informed by [OpenAI Symphony](https://github.com/openai/symphony).

## License

[Apache License 2.0](LICENSE)

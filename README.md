<p align="center">
  <img src="docs/assets/banner.jpg" alt="Sortie - Turn tracker tickets into agent sessions" width="600">
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

Your coding agent can handle a bug fix or a feature. Running several tasks at once still means preparing workspaces, restarting failed runs, and following up on CI and review comments.

Sortie handles that coordination, so you can focus on the results. It assumes your agent already produces useful work when run manually; code quality still depends on the agent and your instructions.

## Works With

**Issue trackers:** GitHub Issues, GitLab Issues, Gitea Issues, Linear and Jira.

**Coding agents:** Claude Code, Copilot, OpenCode, Codex, Kiro and Gemini CLI.

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

## Why "Sortie"

French for "exit" or "departure", a _sortie_ also means a single aircraft mission. Sortie sends coding agents on missions of their own: each issue has an isolated workspace, a clear objective, and a result to bring back.

## License

[Apache License 2.0](LICENSE)

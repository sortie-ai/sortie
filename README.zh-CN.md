<p align="center">
  <img src="docs/assets/banner.jpg" alt="Sortie —— 将工单系统中的工单转化为智能体会话" width="100%">
</p>

<div align="center">


**下一个迭代，不再受限于人力。**

开源编程智能体编排器。<br/>
让你常用的编程智能体在自己的电脑或服务器上，并行处理工单系统中的任务。

[![CI](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml/badge.svg)](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/sortie-ai/sortie/graph/badge.svg?token=K2TPXBCbvb)](https://codecov.io/gh/sortie-ai/sortie)
[![Go Reference](https://pkg.go.dev/badge/github.com/sortie-ai/sortie.svg)](https://pkg.go.dev/github.com/sortie-ai/sortie)

[官网](https://sortie-ai.com) · [文档](https://docs.sortie-ai.com) · [Discord](https://discord.gg/MCNrc8Wf9M) · [参与贡献](CONTRIBUTING.md)

**[English](README.md) | 简体中文**

</div>

## 要解决的问题

编程智能体能够修复缺陷、更新依赖、补充测试和实现功能。但要让你信赖的智能体处理待办任务，仅仅启动会话还不够：隔离工作区、重试机制、工单系统集成和成本追踪需要协同运作。

构建并维护这套基础设施，本身就成了另一个工程项目。

Sortie 就是这套基础设施。

## 支持的工具

**工单系统：** GitHub Issues、GitLab Issues、Gitea Issues、Linear 和 Jira。

**编程智能体：** Claude Code、Copilot、OpenCode、Codex、Kiro、Gemini 和 Pi。

## 安装

```bash
curl -sSL https://get.sortie-ai.com/install.sh | sh
```

或通过 Homebrew 安装：`brew install --cask sortie-ai/tap/sortie`

先用模拟智能体[在本地体验](https://docs.sortie-ai.com/getting-started/quick-start/)，无需连接外部服务；也可以[连接你的工单系统和编程智能体](https://docs.sortie-ai.com/getting-started/)。

## 运行原理

要做的事情写在一个 `WORKFLOW.md` 文件里就行：处理哪些工单，用哪个智能体，给它什么指令。

```markdown
---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  project: acme/billing-api
  query_filter: "label:agent-ready"
  active_states: [todo, in-progress]
  handoff_state: review
  terminal_states: [done]

agent:
  kind: claude-code
  max_concurrent_agents: 4
---

You are a senior engineer.

## {{ .issue.identifier }}: {{ .issue.title }}

{{ .issue.description }}
```

Sortie 会定期查询工单系统，找到符合条件的工单后，为每个工单分配独立的工作区并启动智能体，多个工单同时处理。运行失败会自动重试。如果开启了 CI 和评审反馈，没通过的检查和评审意见会交回给智能体继续修改。

Sortie 运行时可以直接修改 `WORKFLOW.md`，改动会自动生效，不用重启。运行历史在重启后也会保留，进度和成本记录都还在。

进度可以在内置的仪表盘里查看，也可以接入你自己的工具。JSON API（含实时变更推送）、健康探针和 Prometheus 指标都通过同一个端口提供。

不管是笔记本、虚拟机还是 Kubernetes 集群，Sortie 的用法都一样。它提供容器镜像 `ghcr.io/sortie-ai/sortie`，健康探针可以直接用于存活和就绪检查，Pod 停止时也会正常退出。完整的部署步骤见 [Kubernetes 部署指南](https://docs.sortie-ai.com/guides/deploy-sortie-to-kubernetes/)。

Sortie 只有一个可执行文件，不需要另外部署数据库或任务队列，它本身也不发送任何遥测数据。

## 文档

完整的配置参考、CLI 用法和入门指南：[docs.sortie-ai.com](https://docs.sortie-ai.com)

- [使用指南](https://docs.sortie-ai.com/guides/)
- [架构](https://docs.sortie-ai.com/concepts/architecture/)
- [路线图](https://github.com/orgs/sortie-ai/projects/1)
- [代码签名策略](https://docs.sortie-ai.com/code-signing-policy/)

## 先前工作

Sortie 的架构借鉴了 [OpenAI Symphony](https://github.com/openai/symphony)——一个规范优先的编排框架，附带 Elixir 参考实现。Sortie 在以下方面有所不同：语言选择（Go，简化部署）、持久化方案（SQLite 取代内存状态）、可扩展性（可插拔适配器，支持任意工单系统和智能体，而非硬编码为 Linear 和 Codex）、以及完成信号机制（由编排器管理交接状态转换，而非完全依赖智能体主动写入工单系统）。

## 许可证

[Apache License 2.0](LICENSE)

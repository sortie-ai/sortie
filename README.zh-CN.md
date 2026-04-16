<p align="center">
  <img src="docs/assets/banner.jpg" alt="Sortie —— 将工单系统中的工单转化为智能体会话" width="100%">
</p>

<div align="center">


**下一个迭代，不再受限于人力。**

开源编程智能体编排器。<br/>
将工单系统中的工单转化为自主编程智能体会话——自由选择智能体和工单系统，无需站会即可交付。

[![CI](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml/badge.svg)](https://github.com/sortie-ai/sortie/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/sortie-ai/sortie/graph/badge.svg?token=K2TPXBCbvb)](https://codecov.io/gh/sortie-ai/sortie)

[文档](https://docs.sortie-ai.com) · [参与贡献](CONTRIBUTING.md)

**[English](README.md) | 简体中文**

</div>

Sortie 将工单系统中的工单转化为自主编程智能体会话。工程师在工单层面管理工作，智能体负责具体实现。单一二进制文件，零依赖，SQLite 持久化。

Sortie 假定你的编程智能体在手动运行时已经能够产出有价值的成果。它负责处理调度、重试、隔离和持久化等围绕智能体的基础设施——而非提升智能体本身的输出质量。

## 安装

```bash
curl -sSL https://get.sortie-ai.com/install.sh | sh
```

或通过 Homebrew 安装：`brew install sortie-ai/tap/sortie`

## 要解决的问题

编程智能体已经能够胜任日常工程任务——修复缺陷、更新依赖、补充测试覆盖、实现功能——前提是拥有良好的系统提示词、恰当的工具权限，并在代表性工单上经过验证。然而，要在规模化场景下运行这些经过验证的智能体，所需的基础设施尚不存在：隔离的工作区、重试逻辑、状态协调、工单系统集成、成本追踪。各团队只能临时拼凑，质量参差不齐，且每次实现方式各不相同。

Sortie 就是这套基础设施。

## 运行原理

在目标代码仓库旁定义一个 `WORKFLOW.md` 文件：

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

将 `GITHUB_TOKEN` 设置为一个细粒度 PAT，需要对目标代码仓库具有 **Issues: Read and write** 权限。状态通过 GitHub 标签映射——在启动 Sortie 之前，需要创建与 `active_states` 和 `terminal_states` 对应的标签。`query_filter` 将轮询范围限定在带有特定标签的工单上，确保 Sortie 只处理你明确标记为就绪的工作。完整配置详情参见 [GitHub 适配器参考文档](https://docs.sortie-ai.com/reference/adapter-github/)。

Sortie 监视该文件，轮询匹配的工单，为每个工单创建隔离的工作区，并使用渲染后的提示词启动 Claude Code。剩余工作由 Sortie 处理：停滞检测、超时强制执行、带退避的重试、与工单系统的状态协调，以及工单到达终态后的工作区清理。工作流变更无需重启即可生效。

完整示例参见 [`examples/WORKFLOW.md`](examples/WORKFLOW.md?plain=1)，其中包含所有钩子、续接引导和阻塞处理。

### Copilot CLI

如需使用 GitHub Copilot CLI 替代 Claude Code，替换 agent 配置块：

```yaml
agent:
  kind: copilot-cli
  max_turns: 5
  max_concurrent_agents: 4

copilot-cli:
  model: gpt-5.3
```

该适配器需要 GitHub Copilot 订阅和有效的 GitHub token。完整配置详情和参考文档参见 [Copilot CLI 适配器参考文档](https://docs.sortie-ai.com/reference/adapter-copilot/)，完整工作流示例参见 [`examples/WORKFLOW.copilot.md`](examples/WORKFLOW.copilot.md?plain=1)。

## 架构

Sortie 是一个单一 Go 二进制文件。它使用 SQLite 存储持久化状态（重试队列、会话元数据、运行历史），并通过 stdio 与编程智能体通信。编排器是所有调度决策的唯一权威；不依赖外部任务队列或分布式协调。完整架构详情参见 [`docs/architecture.md`](docs/architecture.md)。

工单系统和编程智能体通过适配器接口集成。为新的工单系统或智能体添加支持是一种增量变更：在新包中实现对应接口即可。

已支持的工单系统：GitHub Issues 和 Jira。已支持的智能体：Claude Code 和 Copilot CLI。技术选型的详细依据参见 [`docs/decisions/`](docs/decisions/)。

## 文档

完整的配置参考、CLI 用法和入门指南：[docs.sortie-ai.com](https://docs.sortie-ai.com)

## 先前工作

Sortie 的架构借鉴了 [OpenAI Symphony](https://github.com/openai/symphony)——一个规范优先的编排框架，附带 Elixir 参考实现。Sortie 在以下方面有所不同：语言选择（Go，简化部署）、持久化方案（SQLite 取代内存状态）、可扩展性（可插拔适配器，支持任意工单系统和智能体，而非硬编码为 Linear 和 Codex）、以及完成信号机制（由编排器管理交接状态转换，而非完全依赖智能体主动写入工单系统）。

## 为何取名 "Sortie"

_Sortie_ 是一个军事和航空术语，指一次自主执行的单独任务。这个隐喻恰如其分：编排器将智能体派遣执行任务（工单），每个任务拥有独立的工作区、明确的目标和预期的回报。名称简短，两个音节，跨语言均可发音，且不与该领域中已有项目冲突。

## 路线图

当前状态和优先级参见[项目看板](https://github.com/orgs/sortie-ai/projects/1)。

## 许可证

[Apache License 2.0](LICENSE)

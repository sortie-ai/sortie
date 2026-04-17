# Security Policy

## Supported Versions

Sortie follows [Semantic Versioning](https://semver.org/). Security fixes are
applied to the **current minor release** of the latest major version, with a
**3-month grace period** during which the previous minor continues to receive
critical and high severity security patches.

| Version   | Supported                        |
|-----------|----------------------------------|
| 1.8.x     | Yes                              |
| 1.7.x     | Security patches until July 2026 |
| < 1.7     | No                               |

When a new minor version is released (e.g., 1.5.0), the previous minor (e.g., 1.4.x) enters this 3-month grace period. After the grace period ends, only the current minor release of the latest major version is supported; all older versions are unsupported.

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities privately through
[GitHub Security Advisories](https://github.com/sortie-ai/sortie/security/advisories/new).

If you are unable to use GitHub Security Advisories, email **security@sortie-ai.com**
with the subject line `[VULNERABILITY] <short description>`.

### What to Include

A good report helps us triage quickly. Please include:

- **Affected component** &mdash; e.g., workspace manager, config loader, agent adapter, persistence layer.
- **Sortie version** &mdash; output of `sortie version` or the Git commit SHA.
- **Environment** &mdash; OS, architecture, Go version (if building from source).
- **Steps to reproduce** &mdash; minimal WORKFLOW.md, commands, and configuration that trigger the issue.
- **Impact assessment** &mdash; what an attacker can achieve (path traversal, secret leak, arbitrary execution, etc.).
- **Proof of concept** &mdash; code, logs, or a patch demonstrating the issue. If you have a fix, include it.

You do not need to encrypt your report. If you require encrypted communication,
note this in your initial report and we will establish a secure channel.

## Response Process

| Stage | Timeline |
|-------|----------|
| Acknowledgment | Within **3 business days** of receipt |
| Initial triage and severity assessment | Within **7 business days** |
| Fix development and review | Depends on severity and complexity |
| Coordinated release | Negotiated with reporter |

We will keep you informed at each stage. If you have not received an acknowledgment
within the stated window, follow up on the same channel.

### Severity Classification

We use a four-level severity model to prioritize response:

| Severity | Description | Target Fix |
|----------|-------------|------------|
| Critical | Remote code execution, workspace escape, secret exfiltration | Patch release within days |
| High | Privilege escalation, path traversal, sandbox bypass | Patch release within 1&ndash;2 weeks |
| Medium | Information disclosure, denial of service, configuration injection | Next scheduled release |
| Low | Hardening improvements, defense-in-depth gaps | Tracked and prioritized |

## Disclosure Policy

We follow **coordinated disclosure**:

1. The reporter and maintainers agree on a disclosure date, defaulting to **90 days**
   from the initial report or sooner once a fix is available.
2. We publish a [GitHub Security Advisory](https://github.com/sortie-ai/sortie/security/advisories)
   with full details, affected versions, and remediation steps.
3. A new release containing the fix is published simultaneously with the advisory.
4. The reporter is credited in the advisory unless they request anonymity.

We may shorten the disclosure window if:
- The vulnerability is already being actively exploited.
- The vulnerability has been independently disclosed elsewhere.

We may request an extension beyond 90 days for issues requiring coordinated fixes
across multiple projects. Extensions require reporter agreement.

## Scope

### In Scope

- Workspace path traversal or escape from the configured workspace root.
- Bypass of workspace key sanitization (`[A-Za-z0-9._-]` enforcement).
- Symlink-based attacks against path containment validation.
- Secret leakage through logs, error messages, API responses, or metrics.
- Unauthorized command execution outside the designated workspace.
- Prompt injection vectors in the orchestrator that bypass workflow-defined boundaries.
- SQLite persistence layer vulnerabilities (injection, corruption, unauthorized access).
- Configuration injection via environment variable handling or YAML parsing.
- Denial of service against the orchestrator (crash, resource exhaustion, deadlock).
- Vulnerabilities in direct dependencies (see [go.mod](go.mod)).

### Out of Scope

The following are **not** security vulnerabilities in Sortie. Please report
them as regular GitHub issues instead:

- Bugs in third-party coding agents (Claude Code, etc.) launched by Sortie.
- Vulnerabilities in the underlying OS, container runtime, or hosting environment.
- Issues that require the attacker to already have write access to WORKFLOW.md
  (workflow configuration is a trusted input &mdash; see architecture Section 15.4).
- Misconfiguration of deployment-specific hardening (sandbox settings, network
  policies, credential scoping).
- Automated scanner output without analysis demonstrating actual exploitability.
- Issues in pre-release branches or unreleased code not present in any tagged version.

If you are unsure whether an issue is in scope, report it. We would rather
triage a borderline report than miss a real vulnerability.

## Security Architecture

Sortie's security model is documented in the
[architecture specification](docs/architecture.md) (Section 15). Key invariants:

- **Path containment**: all workspace paths are validated to remain under the
  configured workspace root using symlink resolution and relative-path checks.
- **Input sanitization**: workspace directory names are restricted to
  `[A-Za-z0-9._-]`; all other characters are replaced. Filesystem special
  names (`.`, `..`) are explicitly rejected.
- **Agent execution boundary**: the coding agent subprocess `cwd` is validated
  to equal the workspace path before every launch.
- **Secret handling**: API tokens and secret values support `$VAR` indirection
  and are never logged or expanded beyond a single level.
- **Minimal dependency surface**: the binary is statically linked with zero
  runtime dependencies. No CGo. Uses a small set of direct dependencies, with
  Prometheus metrics support remaining optional.

## Security Announcements

Security advisories are published through
[GitHub Security Advisories](https://github.com/sortie-ai/sortie/security/advisories)
and noted in the [CHANGELOG](CHANGELOG.md).

Watch this repository's **Security** tab or subscribe to release notifications
to receive security updates.

## Acknowledgments

We value the work of security researchers. Contributors who report valid
vulnerabilities will be:

- Credited by name (or handle) in the security advisory, unless anonymity
  is requested.
- Listed in a `SECURITY_ACKNOWLEDGMENTS.md` file in the repository after
  disclosure.

We do not currently operate a paid bug bounty program. If this changes, it
will be announced here.

# Kiro adapter notes

Working notes for anyone changing Sortie's Kiro adapter in `internal/agent/kiro`: the one architectural fact everything else follows from, the two ways a credential problem presents, and what an exit code does not tell you here.

Last updated: 2026-08-23

## Where to get the volatile facts

Read the flag surface off `kiro-cli chat --help` on the version you are targeting, and the account's own model set off the CLI's model-listing command in its JSON format, which is the only authoritative answer because the list is served by the backend and depends on the subscription. The rendered docs at `kiro.dev` cover the headless path and the exit codes; treat them as the vendor's claim and the binary as the fact, because the two are known to disagree.

Lineage is worth knowing and does not expire: this CLI is a distribution of Amazon Q Developer CLI, so the upstream `aws/amazon-q-developer-cli` repository is the source of record for CLI behavior, while the public Kiro tracker is where product-level feature requests live. When a symptom looks like a CLI bug, search upstream, not the IDE tracker.

## The fact everything else follows from

Headless Kiro has no structured output. There is no JSON event stream, no machine-readable result envelope, and no token reporting on the path Sortie drives. Stdout is a human transcript with ANSI styling; the closing cost trailer is on stderr.

Every awkward thing about this adapter is downstream of that. It emits no tool-result events, because tool activity cannot be reconstructed from transcript text. It leaves the model field empty on its events, because the resolved model is never printed in machine-readable form. It reports no token usage at all: the runtime exposes an abstract credits figure rather than counts, so the adapter emits no usage events and every run is reported unmeasured. Token-based budget enforcement is therefore inert for Kiro, and only the turn timeout and cancellation bound a turn. If you are wondering why a budget rule silently does nothing on a Kiro run, this is why.

Stall detection does still reach these turns. Each non-empty stripped stdout line becomes a notification carrying a fresh timestamp, which is what the orchestrator's stall clock reads, so a turn that keeps printing keeps resetting the timer and a turn that goes quiet gets cancelled.

## Exit zero does not mean the turn succeeded

The cost trailer on stderr is the only positive proof that a turn actually ran. A turn that never ran because the credential was rejected also exits zero, with empty stdout and an authentication line on stderr and no trailer.

So the adapter never maps a bare zero exit to success. It reports success only when the trailer is present, reports a specific authentication failure when a zero exit arrives with the auth marker and empty stdout, and otherwise lets the shared decision treat a zero exit with no trailer as a turn that produced nothing. Both markers are matched by substring containment and never by the numbers that follow them, which is what keeps the classification stable while the values vary.

Non-zero exits carry no category worth reading. The vendor documents one meaning for a code that the binary also uses for something else, so the adapter reads nothing into the value: any non-zero exit fails the turn through the shared non-zero row.

## The credential trap, in two shapes

This is the biggest operational hazard and it has two shapes that need different defenses.

With **no credential at all**, a headless chat does not fail. It enters an interactive device-authorization flow, prints a code and a login URL, and blocks forever waiting for a person. The non-interactive flag does not suppress it. With an **invalid credential** it fails fast instead, exiting zero with empty stdout and an authentication line on stderr.

The adapter defends against both before a turn runs: it rejects an empty credential variable outright and then runs a bounded identity canary, rejecting anything that does not clearly report an authenticated key. A canary that times out or exits non-zero is rejected too, and classified as a retryable credential problem rather than a missing agent, because the binary was already resolved by then. The preflight is local-mode only; in SSH mode the key is injected into the remote command instead.

One more reason not to lean on the CLI for input validation: once a device registration is cached from an earlier login attempt, an unauthenticated invocation resumes that pending authorization and blocks on it rather than reporting a bad flag, so the same malformed invocation that fails cleanly on a pristine machine hangs on a used one. Kiro is no guard against a bad argument. The preflight and the external cancellation bound carry that weight.

## Trust posture

The CLI can pre-approve every tool or a named subset. What it does under headless when it meets a tool the subset does not cover is unestablished, and establishing it needs a paid credential nobody has yet driven a turn with.

The adapter takes the conservative branch rather than guessing: when the workflow sets neither trust key, the resolved posture is full trust, and config validation refuses any posture that falls short of full trust. The reasoning is that an approval wait in an unattended run looks exactly like the credential hang, and the credential hang prints nothing distinguishing while it waits, so there would be no line for the per-line hook to recognize and no way to end the turn except the orchestrator's own timeout. Deliberately, no mid-turn recognition path was added for a request the CLI might print, because none has been observed and a hang that ends by timeout resolves as a cancelled turn regardless of what the parser recorded.

Revisit both the default and the refusal once someone can drive an authenticated turn against a subset that excludes a tool the prompt will attempt. Until then, treat a least-privilege tool list as raw CLI behavior rather than a usable Sortie configuration.

## Session continuity is by directory, not by identity

Conversations are persisted per working directory, and the conversation identifier is not obtainable at run time: the CLI's session listing comes back empty for headless conversations, and the turn output prints no identifier. The adapter therefore continues by position rather than by identity. It arms the resume flag once a turn has actually succeeded and passes it on every later turn in the same workspace, which is safe only because Sortie runs one session per workspace per issue.

The identity the session carries is Sortie's own resume value. It is reported back on the turn result and used for logging, and it is never passed to the CLI. The adapter reads no local store.

## Sortie's own tools do not reach this adapter

MCP on this CLI is gated by a server-side profile check, and that check fails on the unattended credential path: the runtime logs that MCP is disabled and prints a warning on every invocation, and with MCP off, workspace-level server configuration is not loaded at all. The adapter reflects that rather than fighting it: it passes no MCP flag and ignores the generated configuration path entirely, which means a Kiro session gets none of Sortie's own tools.

Whether Sortie's tools reach an agent is a per-adapter property, decided by what the CLI accepts and how the adapter wires it. Here the answer is no, and the cause is upstream of us.

## How this differs from its siblings

Kiro is fork-per-turn, like the Claude Code, Copilot, and OpenCode adapters, and unlike the Codex adapter, which holds a persistent process. What sets it apart is not process shape but information.

Kiro gives the adapter a plain-text transcript; the Claude Code and Copilot adapters parse structured event streams, and the OpenCode adapter parses a CLI-side JSON projection. Even OpenCode, the thinnest of those three, tells the adapter more than Kiro does.

Kiro reports no token counts anywhere on the headless path. The Claude Code and Codex adapters both recover authoritative token usage from the runtime, the Copilot adapter recovers it from an on-disk journal, and the OpenCode adapter recovers it from a post-turn export. Kiro is the only adapter in the fleet with no usage source at all.

Kiro resumes by directory. The Claude Code adapter resumes by an identifier it minted itself, the Copilot adapter by one it captured from the runtime, the OpenCode adapter by a server-generated identifier scoped to its directory, and the Codex adapter by a thread identifier. Kiro is the only one with no identifier to resume by.

## Verifying a change

Unit tests cover argument construction, the trust posture, stderr classification, and disposition. The tests that drive the real binary are gated twice: on `SORTIE_KIRO_TEST=1` and again on the credential variable being set. The second guard is not redundant, it is the defense against the device-login hang, so a credential-less machine skips rather than blocking a test run forever. Keep both guards, and keep them skipping cleanly rather than failing. `SORTIE_KIRO_COMMAND` points at a specific binary.

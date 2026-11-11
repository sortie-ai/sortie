# Copilot CLI adapter notes

Working notes for anyone changing Sortie's GitHub Copilot CLI adapter in `internal/agent/copilot`.

Eligibility: qualified

Product conformance: qualified

This file is checked against the tracked measurement under `tools/qualify/probe/testdata/copilot-cli/`. Changing a grade row below fails the staleness gate until a fresh run replaces that measurement.

## Where to get the volatile facts

Nothing here pins a flag list, an event vocabulary, or a payload shape. Read the flags off `copilot --help` on the version you target, and the output format, permission flags, hooks, and telemetry from GitHub's Copilot CLI reference. Check the upstream issue tracker when the docs and the binary disagree. `buildArgs` and `parse.go` define what Sortie actually sends and parses.

## Shape of the integration

The adapter registers the `copilot-cli` kind and drives the CLI in headless prompt mode with JSON lines on stdout. It holds no process open: the CLI session is disk-persisted state identified by a server-assigned ID, and the OS process is created once per Sortie turn. Process groups, graceful shutdown, the stdout line ceiling, and the exit-code decision table all live in the shared `agentcore.ForkPerTurnSession` skeleton, not here.

Two flags are load-bearing and unconditional. Without the autonomous-continuation flag the CLI answers once and exits, so a non-trivial headless task is impossible; the adapter passes it on every invocation along with a step cap, defaulting to a bounded value when the workflow does not set one. And the flag that disables the CLI's ask-the-user tool is passed on every invocation as well, which closes the human-question path completely: no turn of this adapter can end needing human input, because the runtime has no way to ask.

## Session identity, and why turn one is different

`StartSession` mints a v4 UUID for every session that does not continue an earlier one, before any turn runs, and returns it as `domain.Session.ID`. The first turn of such a session passes `--session-id <uuid>`, and every later turn passes `--resume <uuid>`. A resumed session whose journal is present continues under the requested ID and passes `--resume <uuid>` from its first turn.

No launch ever passes `--continue`. That flag resumes the most recent session in the home directory, whichever directory created it, so it could continue another issue's conversation or a credential-verification session.

A resume of a session the runtime cannot find fails the turn and never starts a new conversation in its place. That holds for an ID that never existed, a deleted session directory, and a session whose `events.jsonl` was deleted. The process exits 1, stdout is empty with no `result` line, and stderr reads `Error: No session, task, or name matched '<uuid>'`. To reproduce, seed a session under a fresh UUID and resume it, resume a UUID that never existed, then delete the seeded session's directory and resume it again:

```sh
copilot -p "Remember the word PELICAN. Reply only OK." --output-format json -s --no-ask-user --allow-all --session-id <uuid>
copilot -p "What word did I ask you to remember? Reply NONE if you do not know." --output-format json -s --no-ask-user --allow-all --resume <uuid>
copilot -p "Reply OK." --output-format json -s --no-ask-user --allow-all --resume <unused-uuid>; echo "exit $?"
rm -r ~/.copilot/session-state/<uuid>
copilot -p "Reply OK." --output-format json -s --no-ask-user --allow-all --resume <uuid>; echo "exit $?"
```

Because the runtime fails a resume it cannot find, the adapter checks that `session-state/<uuid>/events.jsonl` is present before a resumed session's first turn, and starts a new session when it is absent: the first turn passes `--session-id` with a new UUID, never `--resume` and never the requested ID, and the shared warning logs `resume_verdict=replaced`. A lost SSH connection during the check fails the start instead, and the retry resumes the same session. The check finds the runtime's home the way the journal read does, so `COPILOT_HOME` belongs in the environment Sortie runs in, or on the remote host for an SSH launch. A value set only inside the agent command, such as a `COPILOT_HOME=...` prefix of an SSH remote command, is not seen, and every resume then starts a new session.

The adapter opens the runtime's session-state tree only by a known ID, and the shared read validates that ID against a path-segment character set before any file opens or process starts. Both properties are security boundaries, not stylistic choices. The journal's location, the ID rule, and the read size and line caps live in `internal/agent/clientprotocol/usagesource`, which this adapter imports from the protocol kind.

## Preflight, and the fake-binary trap

`StartSession` runs a version canary before it will hand back a session, local-mode only: the shared runtime version query (`internal/agent/agentcore`), which fails when the binary does not answer `--version` with a zero exit, as under a Node.js older than 22. It checks nothing else. The credential itself is proven separately: before the working session's first turn, unless a recent proof of the same setup stands, a shared step (`internal/agent/agentcore`) starts a verification session and sends one fixed request. This CLI reports a refused or absent credential only on stderr with a failing exit, so the step reports it as the shared early-exit report carrying that message. A stored `gh` login passes the step.

This is the thing that ruins an afternoon when writing tests. A stand-in binary that does not answer `--version` with a zero exit never reaches a turn, because the version canary rejects it first, and the failure surfaces as the shared early-exit report at session start, which looks nothing like a parse problem. In SSH mode the canary is skipped, so a broken remote binary instead fails later, at the credential-verification step or the first turn, with a different error kind. Know which mode your test is in.

## Token accounting: checkpoints on the stream, records in the journal

The kind declares two sources and the shared usage component does the arithmetic. The adapter parses lines and states what each source means; it subtracts no baseline, hands no figure over to the next run, and decides no completeness.

The live source is the `session.usage_checkpoint` line on stdout. The members read are `inputTokens`, `outputTokens`, `cacheReadTokens`, and `cacheWriteTokens` under `data.accountingSnapshot.modelMetrics.<model>.usage`. They are cumulative over the session, across every process that resumes it, so a turn's spend is the difference between its last checkpoint and the one before. One checkpoint follows each prompt or autopilot continuation, not each model request, so a turn with three requests can emit two. The last checkpoint before `result` is the turn's figure. Cache reads and writes sit inside the input count, and reasoning sits inside the output count (`reasoningTokens` is not read). Each `modelMetrics` key names a model that served requests: a model switch on resume adds a key, and a sub-agent's requests count under the sub-agent's own served model. The figure reaches the orchestrator once, at the end of the turn, with `turn_end` arrival and `per_model` attribution; the model comes from those keys and never from the operator-configured `copilot-cli.model` value, which is the model requested rather than the model that served.

The same `modelMetrics.<model>` object carries the model's premium requests (`requests.cost`) and AI credits (`totalNanoAiu`, in nano units), cumulative over the session like the counts. The live source declares both as session-cumulative amounts in their own units, `premium requests` and `AI credits`, the second the reported number divided by 10^9, and the usage component counts each as the increase over the session's history. Neither is added to a token figure or converted to the other. The checkpoint's top-level `totalPremiumRequests` and `totalNanoAiu` are the sums of the per-model members and `aiCreditsStatus` is not read. The `result` line's `usage.premiumRequests` is not read either, because the checkpoint attributes the same premium requests to the model that served them.

The records source is the journal's `session.usage_record` lines: one per model request, with the counts, the serving model, and an identity under `data.usage`. The runtime does not write the journal at once, so a kill can leave a completed request's record unwritten, even the whole file absent. The adapter reads them through the same parser and scan the `agent-client-protocol` kind uses (`internal/agent/clientprotocol/usagesource`), in two places: at the start of a resumed session, where their total is the session's starting point, and at the end of a turn the checkpoints left unsettled. A turn is unsettled when it ends without a `result` with exit code 0, when the runtime wrote `abort`, when it has no checkpoint, or when the session's starting point is unknown. The read runs locally or through the shared remote read on the session's SSH launch, so an SSH-worker run is measured like a local one. A read that fails (missing file, unsafe session ID, file above the size cap, line above the line cap, SSH failure, timeout) is logged at debug level and leaves the starting point or the turn's records unknown, never zero; a later turn reads again. Each record also carries the request's `cost`, its `initiator`, and `copilotUsage.totalNanoAiu`, and the records source declares both units as per-request amounts. `cost` is the multiplier applied to every call, so a record's premium requests are `cost` when `initiator` is `user`, 0 when it is `agent` or `sub-agent`, and unknown for any other or absent initiator, which leaves the amount a lower bound. AI credits are `totalNanoAiu` divided by 10^9 whatever the initiator. In every captured journal the user-initiated `cost` values sum to the last checkpoint's `requests.cost`, summing every record's `cost` overshoots wherever an agent-initiated record exists, and the records' `totalNanoAiu` values sum to the checkpoint's. Under a custom model provider (`isByok: true`) the runtime writes no `session.usage_checkpoint`, so the records settle every turn.

A resumed process's counter starts from the total of every usage record the journal holds, not from the last checkpoint, so the records a killed process did write count even though no checkpoint reported them. That total is what the usage component takes as the starting point of a resumed session. When the read at the start fails, the first checkpoint cannot be separated from history: that turn settles no figure, later turns settle their own deltas, and none of them claims to cover the whole session.

Counts decode only as JSON integers. An absent, null, or non-integer count makes the reading unknown instead of zero. The journal writes `cost`, `requests.cost`, and `totalNanoAiu` as floats such as `1.0`, so they decode as JSON numbers, never as integers, and `1.0` reads 1. An amount that is absent, null, not a number, not finite, or negative reads unknown instead of zero.

Sortie cancels a turn with SIGTERM to the process group, then SIGKILL after the grace period. The runtime does not always use the grace period to write its journal: under a custom model provider (`isByok: true`) it writes the journal in one piece when the turn ends and exits within milliseconds of SIGTERM, so a turn cancelled while a request is in flight leaves no journal at all. When the runtime does write `abort`, a final checkpoint that omits the request in flight, and a `result` with exit code 0, the adapter reads `abort` as a cancelled turn, so the records settle it in place of the final checkpoint. After SIGKILL the journal can lack the records of completed requests too. A turn that ends either way without its records can end with no figure, its amount unknown and its spend unaccounted. A request in flight at a kill or a cancel leaves no record either. The `agent-client-protocol` route cancels in the protocol and keeps the process alive, so its cancelled turn is measured.

The practical rule: a usage figure from this adapter never decreases, a failed read never lowers a figure already reported, and the "measured" flag distinguishes an unknown spend from a genuine zero. Assertions that ignore that distinction pass for the wrong reason.

## Quota windows are pulled, not streamed

Neither the JSON stream nor the session-state journal carries a quota figure. The runtime's quota snapshot event is ephemeral and never reaches the journal, so the adapter asks for the quotas after a turn that reached the runtime has ended. A cancelled turn and a credential-verification session do not ask. A pull the runtime cannot answer ends the session's pulls; any other failure leaves the next turn to try again.

There are two routes, tried in one pull. Both yield one reading per quota that is not unlimited. Test the unlimited flag, not the entitlement: the SDK documents `-1` for an unlimited entitlement, while a live answer for an unlimited quota carries `0`.

The endpoint route is `GET https://api.github.com/copilot_internal/user` with `Authorization: token <T>`. `T` is the first non-empty of `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`, read from the turn's environment. The fields read, trimmed:

```json
{
  "quota_reset_date_utc": "2026-11-01T00:00:00.000Z",
  "quota_snapshots": {
    "premium_interactions": {
      "unlimited": false,
      "percent_remaining": 0.0,
      "entitlement": 20000,
      "timestamp_utc": "2026-10-28T00:34:20.549Z"
    },
    "chat": { "unlimited": true, "percent_remaining": 100.0 }
  }
}
```

`percent_remaining` is the share left, so the used share is its complement. `timestamp_utc` is when the snapshot was taken, and `quota_reset_date_utc` is the instant the quotas reset. The answer is fresh on every call. The sibling `quota_reset_date` is a bare date and is not read. A 401, 403, or 404 falls through to the headless route. Any other HTTP error is reported as its status code alone. The token goes in that one header and nowhere else, and the response body is never logged.

The headless route runs when no token variable is set, or when the endpoint refuses the token. It starts `copilot --headless --no-auto-update --stdio` on the runtime's host, locally or over the session's SSH launch, and speaks JSON-RPC with `Content-Length` framing on the standard streams. The exchange is `connect` first, with the parameters `@github/copilot-sdk` sends, then `account.getQuota` with `{}`, then stdin closes. The answer, trimmed:

```json
{
  "quotaSnapshots": {
    "premium_interactions": {
      "isUnlimitedEntitlement": false,
      "entitlementRequests": 20000,
      "usedRequests": 20000,
      "remainingPercentage": 0,
      "resetDate": "2026-10-28T00:15:43.579Z"
    }
  }
}
```

Two traps live in this answer. First, the runtime serves it from a cached copy of the endpoint's answer, kept in its user cache file outside `COPILOT_HOME`, with no network round trip. Authenticated headless runs did not refresh that cache, so the figure can trail the endpoint. Second, `resetDate` is the time the cached snapshot was taken, not a reset: it equals the cache's `timestamp_utc` and differs from the endpoint's `quota_reset_date_utc`. The adapter reads it as the observation time and reports no reset on this route. An error answer, such as `Not authenticated`, means the runtime holds no GitHub sign-in of its own, and it ends the session's pulls.

The `agent-client-protocol` declaration for Copilot CLI runs the same pull, with the same token variables read from the session's launch environment. Its headless command is the configured command cut after the `copilot` word: `--acp` and every later word are dropped, a launcher, environment assignments, or a path before the word stay, and the headless arguments follow. The protocol's live channel carries no quota: `initialize` acknowledges no quota opt-in, a `usage_update` carries only `used` and `size`, and the ACP connection answers `account.getQuota` with method-not-found.

A custom provider reports no quota. With `COPILOT_PROVIDER_BASE_URL` set in the runtime's environment the turn spends no Copilot quota, so the pull returns unsupported before any request on either route.

The endpoint request is made from Sortie's host, also for a remote session. A token that only the remote host sets is invisible there, so such a session reaches the quotas through the headless route on the remote host.

## Approvals

By default the adapter grants everything, because an unattended run has nobody to prompt. Only an approval allow-list (`allowed_tools`) displaces that blanket grant, and only because the grant is a superset of it. A deny rule outranks the grant, and the two visibility filters decide what the model can see rather than what it may do, so both keep their effect beside the grant instead of displacing it. The grant is a bundle covering tool approval, path verification, and URL access, so replacing it narrows all three at once and restores the runtime's default rule confining file access to the working directory. Config validation reports the allow-list case as a warning rather than an error, because the configuration is honored exactly as written, at the cost that a turn whose calls were all denied can still report success.

A denial is recognized on the tool-completion event by its error code, distinct from an ordinary tool failure. The adapter routes it through `agentcore.DecideHumanRequest` like every other adapter and emits the shared operator-facing notice. It never constructs a posture itself, and it never treats a denial as terminal, because the run continues.

## Deciding how a turn ended

The runtime's task-completion report decides the turn, not the exit code alone. A non-zero exit is a failure. On a zero exit with a terminal event but no task-completion report, the turn hit the autopilot continuation ceiling; it did not succeed. Without a terminal event, the decision falls to the process exit and work evidence.

The CLI publishes no stop reason, and its journal writes the same shutdown word for a finished session and for one cut off at the ceiling. The task-completion report is the only signal that separates them. `agentcore.FinalizeTurn` maps the evidence to a disposition. On a non-zero exit the failure reason is the turn's last `session.error` message, or `non-zero exit in result event` when none arrived. A `session.error` that is a usage-limit refusal takes precedence over this mapping (see failure modes below).

Work evidence comes from the shared per-turn observer, never a token count: it records assistant-authored content (`assistant.message`'s non-empty `data.content`, any `assistant.message_delta`) and tool-call activity (`assistant.message`'s non-empty `data.toolRequests`, a parsed `tool.execution_start` or `tool.execution_complete`). Get that wrong and the safety row that turns "exited cleanly, produced nothing" into a failure stops firing for the rest of the run. This CLI has more than one way to exit zero having done nothing.

A zero exit that wrote no decodable event line, for example after a configuration file it could not parse, gets the early-exit report. The zero-work row applies only when at least one event line arrived and no terminal event followed.

The adapter enforces no read deadline and no turn deadline of its own; both are orchestrator-side. Combined with the disabled ask-the-user tool, that makes the response-timeout and input-required outcomes unreachable here by construction.

## Sortie's own tools

The worker generates one MCP configuration file per session and the adapter passes it by file reference. On a local launch the file is derived from the generated one: it names an operator header value by the environment variable that carries it instead of holding the value, and it keeps the env block described below. The flag on this CLI is additive rather than replacing, but the adapter still routes an operator-supplied config through the same merge into one file, and a collision on our reserved server key fails the attempt rather than silently overwriting. When an operator value is passed through on its own, the adapter distinguishes inline JSON from a file path by prefix, because the flag itself does: a bare path is read as inline JSON and fails to parse. The runtime starts the tool server with the environment it inherited plus the configuration file's `env` block, and a block entry wins over an inherited variable of the same name. The block carries every `SORTIE_`-prefixed variable, credentials and the dispatch identity included, so the tool server sees them regardless of what the runtime itself inherited. On an SSH launch no file is passed: the runtime receives an inline declaration of Sortie's tool server, by the address of the launch's reverse forward with the bearer carried by environment variable name, beside the operator's HTTP servers. Operator stdio servers are left out, and the tool server's env block never leaves the orchestrator host.

Whether Sortie's tools reach an agent at all is a per-adapter property. The mechanism depends on what the CLI accepts, so what works here does not carry to the rest of the fleet.

## Failure modes worth recognizing

A process that ends having written nothing on standard output, whether from a configuration file the CLI cannot parse, a rejected switch, or a refused credential, fails with the early-exit report, whose standard-error tail holds the diagnosis, a wrong one included: a rejected switch surfaces as an unquoted-prompt complaint rather than naming the switch. The adapter also logs the same stderr lines at warning level on any failing turn. Look there first when a turn fails with no events at all.

Stderr can also be missing outright. When it carries a single marker line saying it was not collected, a descendant process held the handle open past the direct child, the diagnostics were never read, and the wait for them was cut short so the turn could still be reaped and reported. That marker is the finding, not a truncated log.

Two failures worth recognizing come from outside our code entirely, and both are worth checking before you go looking for a bug in the adapter. A resume that comes back with no conversation history, on a session whose journal is present on disk, points at the journal itself rather than at our resume logic: the journal is line-delimited JSON and a raw character sequence that breaks a line-oriented parse takes the whole history with it. And a subprocess that starts, prints nothing, and never exits has been seen under environment managers that wrap the shell and rearrange its file descriptors; there is nothing on our side to fix, and stall reconciliation is what ends such a turn. If either reproduces, confirm it against the current release before treating it as known.

Session state accumulates under the user's home directory, outside the workspace, so removing a workspace does not reclaim it. The credential-verification step's own conversation does not linger there: `StopSession` deletes it by sending a `Content-Length`-framed JSON-RPC `session.delete` request, carrying the session's UUID, to a one-shot `copilot --server --stdio` process, keeping standard input open until the response arrives; anything but a prompt `success: true` is logged as a warning rather than failing the run.

A `session.error` event whose `data.errorType` is `quota` or `rate_limit` is a provider usage-limit refusal: every HTTP 402 quota answer ends `quota`, and every HTTP 429 answer ends `rate_limit` after the runtime's own retries of a short throttle. The turn ends `usage_limit` and holds the configured model, because a premium-request quota leaves the included models serving and some rate limits name one model. On the `copilot-cli` kind this holds with or without the `result` line that follows the event and whatever exit code the line or the process reports; the captured refusal ends with `exitCode` 1, process exit 1, and empty standard error. On the `agent-client-protocol` kind the runtime sends the event only to a client that subscribed in `initialize`, then ends the turn with `end_turn`, which would otherwise read as a successful turn. A `quota` refusal waits for the latest reset among the exhausted quota windows of the turn-end pull; an exhausted window with no future reset leaves the refusal with no reset, retried on the exponential backoff. A `rate_limit` refusal has no window and retries on the exponential backoff. Both routes read the event through one shared parser. Any other `errorType`, such as `query`, keeps the turn's old outcome. A refusal followed by an automatic switch to another model (`eligibleForAutoSwitch`) has not been observed, so a turn that switches and finishes would read as refused. `docs/agent-client-protocol-adapter-notes.md` describes the subscription condition and the reset sources on the protocol route.

## Verifying a change

The live tests in `integration_test.go` are gated on `SORTIE_COPILOT_TEST=1` and skip cleanly without it. The other `SORTIE_COPILOT_*` variables they read are described in that file. Traps:

- The working case needs a real credential: one of the token variables the CLI accepts, or an authenticated `gh`.
- A stand-in binary is rejected by the version canary before any turn, as described above.
- The scripted-model case uses the CLI's own-provider settings, with `COPILOT_OFFLINE` turning off GitHub authentication, telemetry, and update traffic. Its script must end with a `task_complete` call: in autopilot the runtime keeps prompting until the model calls it, and a plain text finish draws a request the script does not hold.
- The scripted-model case isolates `HOME` and the XDG directories and puts `COPILOT_HOME` inside them, so a version-manager shim that reads `HOME` fails to start. Name the binary by an absolute path.

## Entry points

Two routes drive this runtime. The `copilot-cli` kind, described in the sections above, runs the CLI in headless prompt mode once per turn. The generic `agent-client-protocol` kind drives `copilot --acp` and keeps one process open for the session. This page records how the protocol route meets the generic adapter and how it compares with the native one. The sample workflow for the protocol route is `examples/WORKFLOW.agent-client-protocol.copilot.md`.

`--acp` is the only protocol entry point. The route delivers session continuation, spend through the shared usage component, and Sortie's own tool server over HTTP.

`--allow-all-tools` sets the posture. It approves every tool permission request, so removing it brings back asking for every tool, Sortie's and the runtime's own. No other switch is needed for a declared tool server to arrive; the trust section below explains why.

`--model <id>` pins the model on both routes. Without it the account default applies.

A stored login authenticates a run. Every grade on this page was measured under a stored login with no token variable passed to the runtime.

## Route parity

| Axis | `copilot-cli` kind | Protocol route (`agent-client-protocol` running `copilot --acp`) | Verdict |
| --- | --- | --- | --- |
| Spend and the token and money ceilings, local launch | The stream's usage checkpoint per served model, and the journal's usage records for a turn the checkpoints leave unsettled. A cancelled or killed turn can end with no figure, its amount unknown | The prompt result's usage, split by served model from the journal's usage records at every turn end. A cancelled turn is measured; a killed turn can end with no figure, its amount unknown. A live run stopped at `agent.max_tokens` | The protocol route delivers at least what the kind delivers: it also measures a cancelled turn |
| Spend and the token and money ceilings, launch over SSH | The journal is read on the worker host through the session's SSH launch, so the run is measured as a local run is | The records are read on the worker host. The scripted SSH runs report the figures the local runs report, through a kill, a cancellation after output and a continuation | Parity |
| The served model a run is priced at | Each checkpoint names the models that served the session | Each usage record names the model that served its request | Parity; a records read that fails is an accepted difference |
| Provenance and amounts in vendor units | Premium requests and AI credits, each in its own unit, with the settling source and strategy, through the usage component | The same units from the same records, through the same usage component | Parity |
| Provider usage windows | Pulled after a turn that reached the runtime: the quota endpoint with the first token variable set, else the runtime's headless server | The same pull, reading the token variables from the launch environment and starting the headless server from the configured command up to its `copilot` word | Parity |
| Usage-limit refusal | A `session.error` of type `quota` or `rate_limit` ends the turn `usage_limit` | The same event, through the same parser. The runtime sends it only to a client that subscribed, and the subscription goes only to an `agent.command` that names the `copilot` executable or the `@github/copilot` package | Parity at the cost of a configuration edit: `agent.command` names the runtime |
| Tool-server delivery | Sortie's server through the merged configuration file on a local launch, and inline on a launch over SSH | Sortie's server over HTTP on a local launch (graded usable), and through a reverse forward over SSH | Parity |
| Session continuation | Resumes the session by identifier once its journal is found (graded usable) | Loads the session by identifier from a fresh process, its memory included (graded usable) | Parity |
| Turn outcome, retry classification, permission handling | A gap on the native stream: no outcome for an endpoint failure, a cancellation, or a question for a person | Graded usable; eligibility qualified, product conformance qualified | The protocol route meets the richest native reference |
| Credential verification before the first session | Shared verification step. A refused or absent credential exits failing, with the runtime's message on standard error, before the first turn | Shared verification step. A stored login passes it. No graded run has used a token credential, a refused one, or a host with no sign-in | Unmeasured: a refused or absent credential on this route |
| Configured credentials on a remote launch | `COPILOT_GITHUB_TOKEN`, `GH_TOKEN` and `GITHUB_TOKEN` are carried automatically | Carries the names `worker.ssh_pass_env` lists | Parity at the cost of one configuration line |
| What `sortie validate` refuses offline | Wrong-typed `copilot-cli` keys. It warns, without refusing, when `allowed_tools` replaces the blanket grant | Its block keys' types and the `permissions` value. A switch the runtime rejects fails before the first turn with the runtime's exit status and error text | Parity: no refusal guards a failure the protocol route can reach |
| `model` | `--model` | `--model <id>` in `agent.command`. An unknown id runs the account default silently | Parity: a switch on the protocol entry point, measured to take effect |
| `effort` | `--reasoning-effort` on every turn | `--reasoning-effort <level>` in `agent.command` | Parity: a switch on the protocol entry point, measured to take effect |
| `max_autopilot_continues` | `--max-autopilot-continues`, `50` when unset | Nothing carries it | Withdrawn: the ceiling on autopilot continuations within a turn |
| `agent` | `--agent` | `agent-client-protocol.config_options` with `agent: <name>`. `--agent <name>` in `agent.command` is accepted and selects no agent | Parity at the cost of a configuration edit: a hand probe and the nightly gated case measured the entry to select the agent and apply its instructions. The agent's tools and profile model were measured on neither route. A name the session does not offer runs the current agent with a warning, where the `copilot-cli` kind exits 1 with `No such agent` |
| `allowed_tools` | `--allow-tool` in place of the blanket grant. A call the rule does not match is denied and the turn goes on | `--allow-tool <rule>` without `--allow-all*`. A call the rule does not match asks, Sortie refuses, and the turn ends with no follow-up model request | Withdrawn: denying a call outside the rule while the turn goes on |
| `denied_tools` | `--deny-tool` | `--deny-tool <rule>` in `agent.command` | Parity: a switch on the protocol entry point, measured to take effect |
| `available_tools` | `--available-tools` | `--available-tools <list>` in `agent.command` | Parity: a switch on the protocol entry point, measured to take effect. Its effect on the verification request is an accepted difference |
| `excluded_tools` | `--excluded-tools` | `--excluded-tools <list>` in `agent.command` | Parity: a switch on the protocol entry point, measured to take effect |
| `mcp_config` | The operator's file, merged with Sortie's server, passed to `--additional-mcp-config` | HTTP and SSE servers in `agent-client-protocol.mcp_config`. Stdio servers in a file passed with `--additional-mcp-config @<file>` in `agent.command`, measured with a stdio server. A server member the protocol kind does not read, such as `timeout`, fails the session start through the key | Unmeasured: whether a server member the key refuses, such as `timeout`, reaches the runtime through `--additional-mcp-config @<file>`. Every other server has parity at the cost of a configuration edit |
| `disable_builtin_mcps` | `--disable-builtin-mcps` | `--disable-builtin-mcps` in `agent.command` | Parity: a switch on the protocol entry point, measured to take effect |
| `no_custom_instructions` | `--no-custom-instructions` withholds the instruction files | `--no-custom-instructions` is accepted and withholds no instruction file | Withdrawn: withholding the instruction files |
| `experimental` | `--experimental` | `--experimental` in `agent.command`, or `experimental` in the runtime's own settings | Parity: the switch is accepted; its effect was measured on neither route |
| Autopilot | `--autopilot` on every launch. The runtime's task-completion report decides the turn, and a turn without one ends `turn_incomplete` | Nothing carries it. A `config_options` entry can set the `mode` option, whose values include autopilot; whether that value carries autopilot is unmeasured. A turn ends on its stop reason | Withdrawn: autopilot, and the `turn_incomplete` outcome when no task-completion report arrives |
| Asking the user | `--no-ask-user` on every launch | No asking path appeared, with or without the switch | Parity |
| Blanket grant | `--allow-all` on every launch while `allowed_tools` is unset or blank, granting tool approval, path verification and URL access | `--allow-all` in `agent.command` turns the runtime's `allow_all` option on. `--allow-all-tools`, the posture of the sample workflow, approves every tool request and leaves that option off | Parity: a switch on the protocol entry point, measured to take effect |
| Accepted differences | None | The credential-verification conversation stays in the runtime's store, because the runtime offers no session delete. A session load within the UTC minute this process created the session waits up to a minute. Runs after the move are recorded under the kind `agent-client-protocol`, which every protocol runtime's runs carry, so the `copilot-cli` series in run history and `sortie stats` ends at the move, and the by-model table continues only where both kinds record the same model name. The credential-verification request keeps the instruction files, and keeps the tools the launch configures when `agent.command` already sets `--available-tools` or `--no-custom-instructions`, where the kind's first request keeps the `view` tool alone and withholds the instruction files. A records read that fails leaves a settled turn's amount without its split by served model, so that share is unpriced, where the kind's checkpoint still names the served model | Accepted differences, not withdrawn capabilities |

The `copilot-cli` kind is deprecated in favor of this route. The withdrawn rows are what an operator gives up by moving, and the operator reference states them. The kind's removal waits until no row withdraws a capability an operator holds and no row is unmeasured.

The key rows rest on the launches in the next section. The eligibility row rests on the tracked capture and the verdict lines above. The spend, refusal and delivery rows rest on the gated suite's scripted runs, locally and over SSH, which run every night and gate every release. The window row rests on the pull code both routes share.

The credential row lacks a measurement for three cases: no graded run on this route has used a token credential, a refused one, or a host with no sign-in. The gated suite's credential-verification case, run live in the nightly workflow with a token on a host with no stored sign-in, measures the first two. No run measures the third.

## Where the `copilot-cli` settings go on the protocol route

The launches below ran the installed Copilot CLI locally on Linux, under the host's stored login and with no token variable set, in fresh untrusted workspaces. Each launch added `--log-level all --log-dir <dir>` after the switches under test, and a launch that sent a prompt added `--model mai-code-1.1-flash`. The client answered as the `agent-client-protocol` kind does: it refused a permission request where the request offered a refusing option, cancelled it otherwise, and answered every other client request with method-not-found. An `agent.command` switch rides every launch of a session, credential verification included, so a switch that withdraws a tool or an instruction file also changes the verification request. A value that holds a space needs the list form of `agent.command`.

| Setting | `copilot-cli` kind | Protocol route | Launch and observation | What an operator loses |
| --- | --- | --- | --- | --- |
| `model` | Forwarded to `--model`. | The switch `--model <id>` in `agent.command`. | `--model mai-code-1.1-flash` started a session whose `model` option and current model were that id, and each prompted launch's journal names it as the serving model. `--model no-such-model-xyz` exited 0 with nothing on stderr and ran the account default. No native comparison was run. | Nothing for a known id. An unknown id runs the account default silently on this route. `agent-client-protocol.model` scopes a usage-limit hold and selects no model, so it does not replace the switch. |
| `effort` | Forwarded to `--reasoning-effort` on every turn. | The switch `--reasoning-effort <level>` in `agent.command`. | `--reasoning-effort low` and `--reasoning-effort high` on the default model were accepted, and the `reasoning_effort` option reported the level. `--reasoning-effort no-such-level` exited 1 before the handshake, with the accepted levels on stderr. `--model claude-haiku-4.5 --reasoning-effort high` completed the handshake and then failed `session/new` with error -32603, because that model does not support reasoning effort configuration. | Nothing. A launch that fails (an exit, or an error from `initialize` or `session/new`) fails the run before its first turn with the runtime's error. A session that starts runs the level its `reasoning_effort` option reports, silently. Copilot lists `reasoning_effort` under the reasoning-level category (`thought_level`) with values low, medium, high, xhigh, and max, so a block `effort` key targets it, and it draws a warning in Sortie's log when the session lists no such option; the switch stays the measured route. |
| `max_autopilot_continues` | Forwarded to `--max-autopilot-continues`, always passed, default `50`. | Nothing. | Same measurement as the autopilot row: `--allow-all-tools --autopilot --max-autopilot-continues 3`, with the prompt "Reply with exactly SORTIE_BASELINE_OK and do not call any tool.". The switches were accepted. The turn showed one `usage_update`, no continuation, no `session.task_complete` in the journal, and stop reason `end_turn`. The prompt was answered in a single model request, so no continuation had occasion to appear, and the measurement does not show what the ceiling does on a longer task. | The ceiling is a capability difference: no carrier was measured. The native kind reports a turn that reaches its ceiling as `turn_incomplete`. What this route reports at a ceiling is unmeasured: Sortie grades the turn by its stop reason, `end_turn` as success and `max_turn_requests` as `turn_request_limit`, and no launch reached a ceiling to show which one the runtime gives. |
| `agent` | Forwarded to `--agent`. | The key `agent-client-protocol.config_options` with `agent: <name>`. | `--agent <name>`, with a name taken from the handshake's `agent` option, exited 0. The `agent` option in `session/new` stayed empty. The `agent` option, category `_agent`, lists the profiles the workspace and the user define plus `""`, the default Copilot agent, with current value `""`; without any profile the option is absent. `session/set_config_option` with a profile answered with that current value, and the profile's instructions applied to the turn. `session/load` of that session in a fresh process listed `agent` at the value set earlier. The gated case's document defines a workspace profile that ends every reply with a marker word. `copilot -p --agent <a name no profile defines>` exited 1 with `No such agent: <name>, available: ...`. | The agent's tools and profile model were measured on neither route. A name the session does not offer runs the current agent with a warning in Sortie's log, where the native kind exits 1. Removing the entry leaves the agent in force on a continued session; the entry `agent: ""` selects the default Copilot agent. |
| `allowed_tools` | Forwarded to `--allow-tool`; a non-whitespace value replaces `--allow-all`. | The switch `--allow-tool <rule>` in `agent.command`, without `--allow-all*`. | `--allow-all` set the `allow_all` option to `on`, and `--allow-all-tools` left it `off`, so the native kind's unset state corresponds to `--allow-all`. With `--model mai-code-1.1-flash --allow-tool shell` and the prompt "Run the shell command `pwd`, then reply with exactly SORTIE_PROBE_DONE.", the model ran one shell call and replied, and no permission request arrived. | Nothing for a call the rule matches. An unmatched call asks, Sortie refuses, and the turn ends with no follow-up model request (see `## Protocol-specific observations`); the native kind denies it and goes on. |
| `denied_tools` | Forwarded to `--deny-tool`; composes with `--allow-all`. | The switch `--deny-tool <rule>` in `agent.command`. | `--allow-all-tools --model mai-code-1.1-flash --deny-tool shell` with the same prompt: the shell call failed with the error code `denied`, no permission request arrived, the turn went on, and the model reported the tool as blocked by policy. | Nothing. |
| `available_tools` | Forwarded to `--available-tools`; the first verification request sends `--available-tools view` instead. | The switch `--available-tools <list>` in `agent.command`. | `--allow-all-tools --model mai-code-1.1-flash --available-tools view --no-custom-instructions` with the credential verification prompt: the reply opened with `Info: Disabled tools:` naming every tool but `view`. | Nothing for tool scoping. In `agent.command` the switch leaves verification unclaimed (`usagesource.ClaimVerification`, `setsSwitch`): verification then runs with the configured tools and without `--no-custom-instructions`. |
| `excluded_tools` | Forwarded to `--excluded-tools`; the first verification request sends nothing for it. | The switch `--excluded-tools <list>` in `agent.command`. | `--allow-all-tools --model mai-code-1.1-flash --excluded-tools view` with the prompt "List the names of the tools you can call, one per line, and call none.": the reply opened with `Info: Disabled tools: view`, and the list the model gave held no `view`. | Nothing. |
| `mcp_config` | Reads the operator's file, merges Sortie's tool server, and passes the result to `--additional-mcp-config`. | HTTP and SSE servers: the key `agent-client-protocol.mcp_config`. Stdio servers: the switch `--additional-mcp-config @<declaration>` in `agent.command`, a file in the `{"mcpServers": ...}` form. | `session/new` with an HTTP entry for `localhost:9` and a stdio entry, no switches: the runtime attempted the HTTP connection, and rejected the stdio entry with a warning in its log only, with no protocol error. `--allow-all-tools --model mai-code-1.1-flash --additional-mcp-config @<declaration>` with a prompt to call the tool `markersrv-marker`: the call ran and returned `SORTIE_STDIO_MARKER`. | A stdio server declared through the key never reaches the model, silently; the switch carries it. A server member the protocol kind does not read, such as `timeout`, fails the session start here and passes on the native kind. |
| `disable_builtin_mcps` | Adds `--disable-builtin-mcps`. | The switch `--disable-builtin-mcps` in `agent.command`. | `--disable-builtin-mcps` against a launch with no switch: the runtime log's MCP signature listed the built-in servers (`WorkIQ`, `browser-use`, `github-copilot-browser`, `github-mcp-server`, `githubiq`) as disabled, and listed none as disabled without the switch. | Nothing. |
| `no_custom_instructions` | Adds `--no-custom-instructions`; the first verification request adds it whatever the key says. | Nothing. | A marker workspace with one marker word in `AGENTS.md`, a second in `.github/copilot-instructions.md`, and a third in a directory outside it, passed through `COPILOT_CUSTOM_INSTRUCTIONS_DIRS`. `--allow-all-tools --model mai-code-1.1-flash` with the prompt "Reply with every marker word your instructions give you, separated by spaces, or NONE." returned all three words, so the runtime did not skip the untrusted workspace's instruction files. The same launch plus `--no-custom-instructions` returned all three words again. | The switch withdraws no instruction file on this route: repository files and the added directories keep loading. In `agent.command` the switch also leaves verification unclaimed, so verification runs with every tool and not `view` alone. |
| `experimental` | Adds `--experimental`. | The switch `--experimental` in `agent.command`, or `experimental` in the runtime's `settings.json`. | `--experimental` exited 0 and the handshake and close completed. The host's `settings.json` already enables experimental, so no effect separates from a launch with no switch. | Nothing found; the effect of the switch itself is unmeasured. |
| Autopilot and `turn_incomplete` | Passes `--autopilot` and reads the runtime's task-completion report as the outcome. | Nothing. | `--output-format json -s --autopilot --no-ask-user` exited 0 and the session mode stayed `agent`. `--allow-all-tools --model mai-code-1.1-flash --autopilot --max-autopilot-continues 3` with the prompt "Reply with exactly SORTIE_BASELINE_OK and do not call any tool." gave one `usage_update`, no continuation, no `session.task_complete` in the journal, and stop reason `end_turn`. The prompt was answered in a single model request, so no continuation had occasion to appear. | Nothing carries autopilot or its ceiling. Turns end on their stop reason (`stopReasonEvidence`): `end_turn` reads as success and `max_turn_requests` as `turn_request_limit`, and which one the runtime gives where the native kind reports `turn_incomplete` is unmeasured. And a `config_options` entry can set the `mode` option, which offers agent, plan, and autopilot as URLs; whether its autopilot value carries autopilot is unmeasured. |
| Asking the user | Passes `--no-ask-user` on every launch. | None needed on the launches measured. | The prompt "Ask me which color I prefer with your tool for asking the user, and wait for my answer before you reply." with `--allow-all-tools --model mai-code-1.1-flash`, and again with `--no-ask-user` added: neither launch sent `elicitation/create` or made an asking tool call, and both turns ended with `end_turn` without a stall. Without the switch the model said it had no interactive ask tool and listed the colors in plain text. | Nothing found: no asking path appeared with or without the switch. |
| Verification conversation | Deletes the verification session at the end of the step with `session.delete`. | `session/close` at teardown; no delete exists on this route. | The handshake with no switch advertised `close` and `list` in `sessionCapabilities` and no `delete`. After the credential verification launch exited, `session-state/<id>/events.jsonl` remained in the runtime's store. | Each verification leaves its conversation in the runtime's store, where the native kind removes it. |
| Credentials over SSH | Carries `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, and `GITHUB_TOKEN` on a remote launch by declaration. | `worker.ssh_pass_env`, a Sortie key listing the variable names to carry. | Graded from Sortie's code, which no runtime launch exercises. The `agent-client-protocol` registration calls `registry.DeclareCredentialEnv()` with no name, and `carriedEnvNames` joins the declared names with the names `worker.ssh_pass_env` lists. | A name left out of `worker.ssh_pass_env` does not travel, and the remote host's own login or environment applies. A workflow the load converts from a removed kind carries that kind's names through `convertedCredentialEnv`, so it loses nothing. |
| Run history and `sortie stats` | Records the kind `copilot-cli` in `run_history.agent_adapter`. | Nothing. | Graded from Sortie's code, which no runtime launch exercises. `ByAdapter` keys on the stored kind. The by-model table (`sharesOf`) groups a run under the model names of its token-spending parts, and a run with no such part under `modelOf`: the model the runtime reported, else the block's `model` key. Both kinds take the reported name from the runtime's own model member, the event stream on the native kind and the journal record on the protocol kind. | `by_adapter` keys on the kind, so the `copilot-cli` series ends at the move and the `agent-client-protocol` series mixes every runtime that kind drives. The by-model table continues only if both kinds record the same model names; reading does not show them to be byte-identical, and a protocol run with no token-spending part and no reported model falls under the block's `model` key, or under no model. |

## Load-bearing capability observations

No row blocks transport parity. Turn disposition and retry classification are usable on the protocol surface and a gap on the native stream, which reports no outcome for an endpoint failure, a cancellation, or a question for a person. Token accounting is a gap on the protocol surface only, and the native stream carries the figure, so the routes differ on that row.

No prompt response in the capture carried a token figure, although Sortie reads one from the wire when it is present. Sortie took the figure from the runtime's own records, the session-state journal that the native kind reads too. The effective adapter returned a measured figure from that source, and the ceiling stop worked: a live run through Sortie's orchestrator stopped at the figure ("stopped at ... token(s), the issue entered the token budget hold") and dispatched nothing in the quiet window after it. The grade stays a gap because it describes the wire, the way the Gemini notes record theirs.

The native stream's `session.usage_checkpoint` line carries the token figure the recognizer reads, per served model. Its terminal event reports no token counts, and the checkpoint also carries premium requests and AI credits per served model.

Other gaps on the protocol route, each with an owner:

- The documentation site has no page for the protocol route of this runtime. The sample workflow points at the generic reference page.

The refusal cases, a turn ending in `runtime_refusal` and a retry classified as `non_retryable_refusal`, are declared, not measured, on both surfaces as never produced. On the protocol surface a request the model declines ends `end_turn` with the refusal as the model's prose, and so does a refusal or a content filter answered by a local stand-in upstream, after only the stand-in's own text; the runtime writes no text of its own. On the native stream a request the model declines ends the launch with `exitCode` 0, and the terminal event has no member a refusal could set. `limit_reached` is declared on the protocol surface as folded into a completed turn: an over-budget prompt, and a context-length error from a local stand-in endpoint, each arrive as `Error:` agent text followed by `end_turn`, and the runtime's journal records the over-budget rejection as a `session.error` whose `errorType` is `query`. On the native surface it was not induced, because the prompt channel cannot carry a request big enough.

On the protocol surface, retry classification excludes the human-input case as not applicable. The consent request the asking-posture launch raised offered a refusing option, the client refused it inside the protocol, and the turn went on. That exclusion does not cover a question addressed to a person. The native stream has no terminal vocabulary for the human-input outcome, so the case is not inducible there.

An endpoint failure is a declared outcome on the protocol surface and not inducible on the native one; the next section gives the frames. Cancellation passed on the protocol surface, where the turn ended in cancellation after its own context was cancelled. The native stream writes its terminal only at exit, so cancellation cannot be induced there. The retryable failure case passed on both surfaces: after one SIGKILL to its group, the launch reported transport loss. The `unknown_outcome` case has no deterministic inducer on either surface, so it carries no obligation.

- protocol turn_disposition: Observed: usable
- protocol retry_classification: Observed: usable
- protocol token_ceiling: Observed: gap
- protocol tool_server_delivery: Observed: usable
- protocol session_continuation: Observed: usable
- protocol permission_handling: Observed: usable
- native_stream_json turn_disposition: Observed: gap
- native_stream_json retry_classification: Observed: gap
- native_stream_json token_ceiling: Observed: usable
- native_stream_json session_continuation: Observed: usable

A case that was induced and that the surface reported no outcome for keeps its obligation. It counts against that surface; it is not forgiven.

## Protocol-specific observations

The handshake reports the agent's own name, `Copilot`, and its version, which the gated suite's identity rule reads. It advertises `mcpCapabilities.http`, and `sessionCapabilities` carries `close` and `list` but not `resume`.

Session continuation restores the session and its memory: the recall turn runs under the seed's own session identifier and returns what the seed asked it to remember. A successful load alone does not prove that; only the recall answer does.

Copilot loads a tool server declared in `session/new` only over HTTP. A stdio declaration is ignored: the model is not offered the tool, the turn completes, and nothing marks the failure. Sortie's own tool server arrives over HTTP, so only an operator's stdio server is affected. A tool call is titled `<server>-<tool>`, on this route and on the native one.

Without `--allow-all-tools`, a tool call raises `session/request_permission`. When Sortie answers `reject_once`, Copilot fails the call with "The user rejected this tool call.", prints "Info: Operation cancelled by user", and ends the turn without a follow-up model request. Nothing is left pending. So under a posture that asks, a declared tool is delivered and never called.

An invalid `GITHUB_TOKEN` beside a stored login did not stop a run. The stored login authenticated it, and the sample workflow says the same.

An endpoint failure is folded into a completed turn. When the scripted model endpoint answers HTTP 400 after its script is exhausted, Copilot renders the error as agent text and ends the turn normally. The frames of the script-exhaustion run:

- frame 6: `agent_message_chunk`, "Warning: No token count multiplier ..."
- frame 7: `usage_update`
- frames 8 and 9: `tool_call` and `tool_call_update`
- frame 10: `usage_update`
- frame 11: `agent_message_chunk`, "Error: 400 scripted model: request 2 ... arrived after the script was exhausted"
- frame 12: `{"stopReason":"end_turn",...}`

Nothing on the wire marks the failure, so the profile declares it for the protocol surface only, as an outcome folded into another case. The native route differs: `copilot -p` exits with status 1, prints the error on stderr, and writes no result event. That is why the native surface carries the `output_channel_silent_on_failure` entry for the same case.

An over-budget prompt ends the same way, and the runtime rejects the prompt itself, before any upstream request. The last frames of an oversize turn:

- a `usage_update` whose `used` exceeds `size`
- `agent_message_chunk`, "Error: Execution failed: Request exceeds the model's prompt budget after reserving output (... tokens; limit ...). Reduce context or compact before continuing."
- `{"stopReason":"end_turn",...}`

Every model request sends a `usage_update` before its model text. The Copilot usage declaration rests on that order, because the runtime's own text chunks carry no message identifier and no metadata, so frame order is the only signal on the wire. It held on the bring-your-own-key route, in the frames above, where a `usage_update` (frame 7) comes ahead of the first tool call and another (frame 10) ahead of the next request. It held on the hosted provider too: a text-only request runs `usage_update` at frame 6 and the first model text at frame 7, and a tool-forcing request runs `usage_update` (6), `tool_call` (7), `tool_call_update` (9), a second `usage_update` (10), then the model's text (11).

Runtime text of Copilot's own can precede the first `usage_update`, so the rule reads model text, not any chunk. Two such lines were seen. The notice "Warning: No token count multiplier is known ..." appears for a model id the multiplier table does not know, such as `scripted-model`. On the hosted provider, the credential verification session starts with `Info: Disabled tools: ...` at frame 6, then `usage_update` at frame 7, and the model's text at frame 8 (`"SORTIE_CRE"`). Neither line counts as the model being reached.

The notice depends on the model id. `claude-sonnet-4.5` is known to the installed Copilot CLI and prints no notice; `scripted-model` does. The default-declaration row of the gated suite names the known id, so that its interrupt scenarios measure the default path and not a property only the Copilot declaration describes.

## Native headless observations

`copilot -p <prompt> --output-format json` emits JSON Lines on stdout. The terminal is a `result` event that carries `exitCode` and `sessionId`. A later launch resumes the session with `--resume <session id>`, and the recall turn runs in the same session as the seed and returns the phrase the seed asked it to remember. A recall prompt that asks for "the phrase from earlier" lets a small model answer with its own earlier reply, `STORED`, which reads as lost history; the profile asks which phrase it was told to remember.

The CLI offers no single-document JSON output, so the profile declares the plain-JSON surface absent. The run corroborated the declaration.

A refused model or an endpoint failure ends a `-p` launch with exit status 1 and an error on stderr, and no result event. The stream is silent on failure, so that case is not inducible on this surface. A request the model declines ends the launch with a `result` event whose `exitCode` is 0 and with empty standard error, unlike the exit status 1 of a refused model or an endpoint failure. The result event is written at exit, so cancellation is not inducible either.

Token accounting is covered above: the stream's `session.usage_checkpoint` line carries the token figure, and the same checkpoint carries premium requests and AI credits.

## Workspace trust and process boundary

The containment boundary held under measurement: every launch ran inside the run-scoped root, no project settings applied, and every process-group member observed was the launched command or its descendant.

A tool server declared in `session/new` needs no trust switch. It loaded over HTTP in a workspace the runtime did not trust, and the capture's tool-server row passed without one. There is no trust switch to add on this route: `--trust-folder` is a prompt-mode switch, and the CLI refuses it beside `--acp`.

A workspace `.mcp.json` is a separate path. In prompt mode it loaded only with `--trust-folder`; without the switch the file was ignored.

## Excluded capability cases

- retry_classification human_input: induced, and the surface reported no outcome (terminal_vocabulary_closed), so the case keeps its obligation
- retry_classification human_input: not applicable on protocol: the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose
- retry_classification non_retryable_refusal: declared outcome_never_produced
- retry_classification unknown_outcome: no deterministic inducer on any runtime, so the case carries no obligation
- turn_disposition cancellation: induced, and the surface reported no outcome (terminal_written_at_exit_only), so the case keeps its obligation
- turn_disposition limit_reached: declared outcome_folded_into_another
- turn_disposition limit_reached: not induced (prompt_channel_too_small), so the case stays unmeasured on this surface
- turn_disposition runtime_failure: declared outcome_folded_into_another
- turn_disposition runtime_failure: induced, and the surface reported no outcome (output_channel_silent_on_failure), so the case keeps its obligation
- turn_disposition runtime_refusal: declared outcome_never_produced

## Unobserved surfaces

Windows live qualification is unobserved.

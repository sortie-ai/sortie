## 4. Core Domain Model

### 4.1 Entities

#### 4.1.1 Issue

Normalized issue record used by orchestration, prompt rendering, and observability output.

Fields:

- `id` (string)
  - Stable tracker-internal ID.
- `identifier` (string)
  - Human-readable ticket key (example: `ABC-123`).
- `title` (string)
- `description` (string or null)
- `priority` (integer or null)
  - Lower numbers are higher priority in dispatch sorting.
- `state` (string)
  - Current tracker state name. An empty value means the issue is in no configured state: on a tracker that derives state from labels, an open issue that carries no state label reads empty. An empty state is never active, terminal, or the handoff state, so dispatch refuses it.
- `branch_name` (string or null)
  - Tracker-provided branch metadata if available.
- `url` (string or null)
- `labels` (list of strings)
  - Normalized to lowercase.
- `assignee` (string or null)
  - Assignee identity as provided by the tracker. Used for prompt rendering and observability.
- `issue_type` (string or null)
  - Tracker-defined type (for example Bug, Story, Task, Epic). Used for prompt rendering and per-type concurrency limits.
- `parent` (object or null)
  - Parent issue reference for sub-tasks. Relevant for blocking/dependency logic and prompt context.
- `comments` (list or null)
  - Comment records containing human feedback, review notes, and prior agent workpad entries, oldest first. Needed for continuation runs where the agent must understand prior communication, and for the first run after a person releases a park. Each record has `id`, `author`, `body`, and `created_at`; `created_at` is the tracker's timestamp text passed through unchanged, in RFC 3339 (fractional seconds optional, `Z` or `±hh:mm` offset) or the same form with a `±hhmm` offset, and one parser reads exactly those forms.
- `blocked_by` (list of blocker refs, or null)
  - Authoritative unless the issue is marked `blockers_unresolved`, in which case the list is whatever its producer last held and is not to be trusted; the dispatch gate treats an unresolved list as blocking, the same conservative rule an unknown blocker state already carries one level down.
  - Each blocker ref contains four string fields, and an unavailable value is the empty string rather than null. Null is reserved for the `blocked_by` list itself, which is how an unresolved list is distinguished from one that is known to be empty:
    - `id` (string)
    - `identifier` (string)
    - `state` (string)
    - `display_id` (string)
      - Qualified form of `identifier`, by the same rule the issue's own `display_id` follows. Empty means `identifier` is already display-ready. A blocker ref's identifier fields follow the same rule the issue's own do on every adapter: an adapter that qualifies its issues qualifies its blockers the same way.
  - If `blocker.state` is null or unknown, treat it as non-terminal (conservative).
- `created_at` (timestamp or null)
- `updated_at` (timestamp or null)

#### 4.1.2 Workflow Definition

Parsed `WORKFLOW.md` payload:

- `config` (map)
  - YAML front matter root object.
- `prompt_template` (string)
  - Markdown body after front matter, trimmed.

#### 4.1.3 Service Config (Typed View)

Typed runtime values derived from `WorkflowDefinition.config` plus environment resolution.

Examples:

- poll interval
- workspace root
- active and terminal issue states
- concurrency limits
- coding-agent executable/args/timeouts
- workspace hooks

#### 4.1.4 Workspace

Filesystem workspace assigned to one issue identifier.

Fields (logical):

- `path` (workspace path; current runtime typically uses absolute paths, but relative roots are possible if configured without path separators)
- `workspace_key` (sanitized issue identifier)
- `created_now` (boolean, used to gate `after_create` hook)

#### 4.1.5 Run Attempt

One execution attempt for one issue.

Fields (logical):

- `issue_id`
- `issue_identifier`
- `attempt` (integer or null, `null` for first run, `>=1` for retries/continuation)
- `workspace_path`
- `started_at`
- `completed_at` (timestamp or null)
  - Populated when the attempt finishes. Used for run duration calculation and persistent history.
- `status`
- `agent_adapter` (string)
  - Which agent adapter was used for this attempt. Relevant when multiple agent types are configured.
- `rule_name` (string)
  - The dispatch rule the attempt was routed by. Empty when the workflow-wide fallback fired; `default` when the `dispatch.default` selection fired.
- `template_id` (string)
  - The prompt template the attempt used. Empty selects the `WORKFLOW.md` body.
- `configured_model` (string)
  - The `model` the attempt's resolved settings block carried. Empty when none was set, which leaves the runtime's own default in charge. It records what the operator asked for.
- `configured_effort` (string)
  - The `effort` the attempt's resolved settings block carried, empty by the same rule.
- `reported_model` (string)
  - The model the runtime reported running, taken from the last token-usage event that named one. Empty when the runtime reported none. A reported model never overwrites a configured one, and nothing compares the two: routing aliases and runtime fallbacks make them differ legitimately.
- `stage_previous` (string)
  - The name of the rule whose stage hop led to the attempt's dispatch, frozen with the dispatch selection and rendered as `stage.previous` (Section 12.1). Empty when no hop led to the dispatch.
- `stage_previous_outcome` (string)
  - `succeeded` or `no_change`: the outcome of the run of `stage_previous` that ended in the hop, rendered as `stage.previous_outcome`. Empty whenever `stage_previous` is empty.
- `first_after_hop` (boolean)
  - True on the first attempt dispatched after the hop `stage_previous` names. False on a later dispatch into that stage while the hop record stands (after a restart, a released hold, or a lost retry), on a retry or reaction continuation, and when `stage_previous` is empty. A row recorded before the flag was kept takes it from the upgrade.
- `agent_blocked` (boolean)
  - True when the run's agent ended it with a `blocked` soft stop (Section 21.1). False otherwise, and on a row recorded before the flag was kept.
- `declared_outcome` (string)
  - The recognized `.sortie/status` token that ended the run (`blocked`, `needs-human-review`, or `no-change-needed`), as the agent wrote it (Section 21.1). Empty when the run ended on no recognized token, and on a row recorded before the field was kept.
- `agent_text` (string)
  - The statement that came with the token in `declared_outcome`, normalized and masked as Section 21.1 states. Empty when `declared_outcome` is empty, when the agent wrote no statement, and when normalization empties it.
- `chain_id` (string)
  - The stage chain identifier frozen with the dispatch selection. A dispatch no hop reached starts a chain with a fresh random value; a dispatch a hop reached inherits the chain of the hop; a retry or reaction continuation carries the chain of the run it continues. Empty only on a row written before the chain was recorded.
- `dispatched_at` (timestamp)
  - The start of the first run of the pass, the runs that share the attempt's `chain_id`. The first run of a pass records its own start, frozen with the dispatch selection, and a later stage, retry, reaction continuation, or restart carries the value of the run it continues. Empty on a row the upgrade could not date: one without a `chain_id`, or one whose pass holds no agent run.
- `stage_target` (string)
  - The rule the attempt's hop decision targeted. Empty when the attempt reached no hop decision.
- `stage_result` (string)
  - The decision's result: `advanced`, `partial` (the target's label was added but an old stage label was not removed), `failed`, or `ceiling`. Empty exactly when `stage_target` is empty.
- `issue_title`, `issue_url` (strings)
  - The tracker's title and web link of the issue as read when the attempt was dispatched. Empty on a row recorded before they were kept.
- `trigger` (string or unknown)
  - What started the retry whose fire dispatched the attempt: the `sortie_retries_total` label of the schedule that created that retry (`error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`), kept through every put-back. Empty for a dispatch from the poll tick and for a retry a reaction other than the CI fix started; the CI fix records `ci_fix`. Unknown where the row does not record it: a row recorded before the origin was kept, and an attempt dispatched by a retry that never stored what started it.
- `reaction_kind` (string or unknown)
  - The reaction kind of the retry that dispatched the attempt. Empty when none. Unknown where the row does not record it.
- `session_id` (string or unknown)
  - The agent session the attempt used. Empty when the attempt started none. Unknown where the row does not record it.
- `ssh_host` (string or unknown)
  - The SSH worker the attempt executed on. Empty for a local attempt. Unknown where the row does not record it.
- `dispatch_id` (string or unknown)
  - The dispatch ID of the attempt (Section 4.2), the key of its retained agent events. Empty on the recorded CI result, which no dispatch produced. Unknown where the row does not record it.
- `usage_provenance` (string or unknown)
  - Every source whose readings settled the attempt's token figure, as `source:strategy` pairs from the usage component's vendor-neutral vocabulary, each pair once, sorted, and comma-joined. Empty when no source settled a figure: an unmeasured attempt, or one with no agent turn. Unknown where the row does not record it, such as a row recorded before the field was kept.
- `usage_completeness` (string or unknown)
  - `complete` when every turn's token figure is accounted, `lower_bound` when the attempt ran turns whose spend no source accounted, and `unmeasured` when no figure exists. The recorded CI result is `complete`. Unknown where the row does not record it.
- `reported_amounts` (list)
  - Every amount the agent runtime reported for the attempt's spend, each in the unit the runtime reported it in and never converted. An entry holds the `unit`, the `model` the source attributes it to (empty when none), the `amount`, and a `lower_bound` flag set when the attempt may have spent more in that unit than the amount states. The list is empty when the runtime reported none, and a row recorded before the field was kept has no entries, so its amounts are not recorded rather than absent. An amount in a unit other than US dollars never enters a token figure, a money figure, or a ceiling.
- `error` (optional)

#### 4.1.6 Live Session (Agent Session Metadata)

State tracked while a coding-agent subprocess is running.

Fields:

- `session_id` (string)
  - Opaque string assigned by the agent adapter. For adapters that expose thread/turn concepts, the composition rule (e.g., `<thread_id>-<turn_id>`) is adapter-specific, not universal.
- `thread_id` (string)
- `turn_id` (string)
- `agent_pid` (string or null)
- `last_agent_event` (string/enum or null)
- `last_agent_timestamp` (timestamp or null)
- `last_agent_message` (summarized payload)
- `agent_input_tokens` (integer)
- `agent_output_tokens` (integer)
- `agent_total_tokens` (integer)
- `last_reported_input_tokens` (integer)
- `last_reported_output_tokens` (integer)
- `last_reported_total_tokens` (integer)
- `reported_amounts` (list)
  - The session's run-cumulative reported amounts as of the latest agent event, in the shape the Run Attempt field of the same name uses. The in-flight cost estimate and the money ceilings read the US-dollar entries; an entry in any other unit is recorded and read by no ceiling.
- `turn_count` (integer)
  - Number of coding-agent turns started within the current worker lifetime.
  - The worker counts each turn as it starts, self-review turns included, whichever agent events the session emits.
- `retained_events` (bounded list of events)
  - The session's retained agent events, numbered from 1 within the run and bounded by count and by bytes, written with the run's history row at worker exit (Section 13.9).

#### 4.1.7 Retry Entry

Scheduled retry state for an issue.

Fields:

- `issue_id`
- `identifier` (best-effort human ID for status surfaces/logs)
- `attempt` (integer, 1-based for retry queue)
- `due_at_ms` (monotonic clock timestamp)
- `timer_handle` (runtime-specific timer reference; runtime-only, not persisted to SQLite)
- `error` (string or null)
- `trigger` (string; the `sortie_retries_total` label of the schedule that created the entry, such as `error`, `continuation`, or `usage_limit`, empty when that schedule records none; persisted with the entry)
- `origin_trigger` (string; what started the retry: the `trigger` of the entry, except that an entry the retry timer reschedules keeps the `origin_trigger` of the entry it replaces, so a put-back never replaces what started the retry; empty when no schedule label started it; persisted with the entry, and the attempt the retry dispatches records it as its `trigger`)
- `issue_title`, `issue_url`, `issue_type`, `issue_labels`, `issue_state` (strings and a list of strings; the issue's title, web link, type, labels, and tracker state as read when the entry was scheduled; persisted with the entry and restored at startup; a read that records no labels leaves `issue_labels` absent, which differs from an empty list, and an entry holding a title or link and no labels takes the type, labels, and state of its next read without replacing the title or link)
- `configured_model` (string; the `model` the dispatch's resolved settings block carried, frozen with the selection and empty when none was set; persisted with the entry)
- `dispatched_at` (timestamp; the start of the first run of the pass the entry continues, as on the run attempt (Section 4.1.5); persisted with the entry and empty on an entry the upgrade could not date)
- `usage_limit_notified` (boolean; true when the run the entry follows ended in a provider usage-limit refusal, so the notification of that refusal is not repeated: the run the entry dispatches publishes no `session.started` event and, when refused again, no `session.failed` event; false when the schedule records no refusal; persisted with the entry, and false on a restored entry that predates the field)

Note: `timer_handle` is a runtime-only field and is not persisted. On restart, pending retries are reconstructed from the persisted `due_at` timestamps stored in SQLite.

#### 4.1.8 Issue Outcome

How an issue's latest work ended, as the orchestrator observed it. The record is current state, not a history: one record per issue, backed by SQLite, replaced or kept as observations arrive and deleted when the poll tick picks the issue up again. An issue with no observed ending has no record, and every consumer reads that as "no outcome".

Fields:

- `issue_id`
- `kind` (`handed_off`, `merged`, or `terminal`)
- `observed_at` (timestamp, RFC 3339 in UTC at second precision; when the orchestrator made or observed the ending, never a time the tracker or forge reports)
- `state` (string; for `handed_off`, the state the handoff wrote; for `terminal`, the terminal state the read found or the handoff wrote, as the tracker reported it or the configuration names it; empty for `merged`)
- `merge_reaction_kind` (string; for `merged`, the reaction kind that observed the merge, `merge` for auto-merge or `merge-completion`; empty otherwise)
- `pr_number`, `pr_repository` (integer and `owner/repo` string; for `merged`, the pull request as the reaction's data holds it; `0` and empty otherwise)

Observation sites. The orchestrator records an outcome on its event loop, synchronously, in the code path of the observation, and records it nowhere else:

- A successful handoff transition write records `handed_off` with the state written. When the target is a configured terminal state, as a terminal `tracker.no_change_state` can be, it records `terminal` instead, because no person takes that issue over.
- An auto-merge reaction records `merged` with kind `merge` when its merge call succeeds, when the merge call reports the pull request already merged, and when its check reads the pull request already merged before any merge call (Section 8.5, Part G).
- Merge completion records `merged` with kind `merge-completion` when it reads the pull request merged, with or without a merge commit identifier (Section 8.5, Part J).
- Reconciliation records the outcome of the release read when the tracker reports a held issue terminal, whether or not the issue is running.
- Every other place that lets go of an issue Sortie holds because a read showed a terminal state records `terminal` with that state: a worker exit that finds the issue terminal, the retry timer, the release of a park by a move to a terminal state, the stage hop hold's direct read, and a merge-completion entry dropped because its issue is already terminal. The last one records the outcome of the release read.
- Startup pending-reaction recovery, startup terminal workspace cleanup, and the periodic sweep record nothing. Sortie no longer holds those issues, so recording there would date an old ending at the restart.

Release read. When a terminal read releases an issue that holds a pending merge-completion or auto-merge entry, the orchestrator reads that entry's pull request once before it deletes the entry, and takes the outcome from the read. A forge that closes the issue on merge, or a tracker automation that moves the issue on merge, makes the tracker report terminal before merge completion reads the merge; without the read the issue would end `terminal` and lose its pull request. A read that reports the pull request merged yields `merged` with the entry's reaction kind and pull request, the merge-completion entry when the issue holds both, because merge completion observes a merge whoever performs it. A read that reports it open, closed unmerged, or fails (one warning is logged) yields `terminal`. With no entry or no configured SCM adapter, the outcome is `terminal` and nothing is read. The read changes only the recorded outcome; the release and its dispositions stay as they are.

Record rule. One store statement applies it, with no read-then-write. "Write" replaces every field with the incoming values; "keep" leaves the record unchanged.

| Stored | Incoming `handed_off` | Incoming `merged` | Incoming `terminal` |
|---|---|---|---|
| none | write | write | write |
| `handed_off` | write | write | write |
| `merged` | write | keep | keep |
| `terminal` | write | write | keep |

A handoff always writes, because it ends work newer than anything stored. A stored `merged` keeps against a second merge, so the first observer names the path and the time, and against a terminal read, so a merged issue that then moves to its terminal state still names its pull request. A stored `terminal` keeps against a terminal read, so the first read keeps its time.

Pick-up. The poll tick's dispatch deletes the issue's outcome once it creates the running entry. Retries and reaction continuations fire through the retry timer and never delete it, so a CI or review fix on a handed-off issue keeps its handoff. An issue that has reached no observation site since its pick-up has no outcome, including a running issue and an issue picked up again after an earlier ending.

A failed write is logged and changes nothing else: the site's own work continues, and a later observation the record rule admits writes. A failed pick-up delete is logged, and the earlier outcome stays until the issue's next handoff, which always writes. The outcome is served as recorded, whatever the issue's live state (Section 13.7.2); the table is described in Section 19.2.

#### 4.1.9 Orchestrator Runtime State

Single authoritative state owned by the orchestrator. The running map and active timers are in-memory for performance; retry_attempts, completed set, agent_totals, parked, stage_hops, usage-limit holds, and attention acknowledgements are backed by SQLite and survive restarts.

Fields:

- `poll_interval_ms` (current effective poll interval)
- `max_concurrent_agents` (current effective global concurrency limit)
- `running` (map `issue_id -> running entry`)
- `claimed` (set of issue IDs reserved/running/retrying)
- `retry_attempts` (map `issue_id -> RetryEntry`)
- `completed` (set of issue IDs; bookkeeping only, not dispatch gating)
- `agent_totals` (aggregate tokens + runtime seconds)
- `usage_windows` (agent kind to the newest report of each usage window since start; runtime-only)
- `reaction_attempts` (map `issue_id:kind -> integer`; the tries a reaction kind has used for an issue, which each kind counts in its own unit: `ci`, `review`, and `bot-review` count fix continuations dispatched (`ci` since a passing verdict, or a head change attributed to someone else, last reset it), `merge-conflict` counts rebase continuations dispatched since the conflict episode began, `merge` counts merge calls made, `merge-completion` counts tracker transitions that failed, and `label-review` and `label-fix` count nothing; an escalation keeps its kind's counter while it stands, and a `ci` budget escalation holds it at `max_retries`, which it exceeds only after a configuration reload lowers `max_retries` below the fixes already dispatched; deleted by the owning kind's own paths (a passing CI status, a closed episode, the end of a `review` escalation, a watch-window drop of an entry with no standing escalation) and, for every kind at once, when the tracker reports the issue terminal and when the tracker reads the issue outside `tracker.handoff_state` while Sortie does not drive it; a `bot-review` escalation, also after it ends, and a `review` or `bot-review` watch-window drop at a spent counter, keep it; runtime-only, not persisted, except that the counter of a standing escalation is stored with it in `reaction_escalations` and the startup restore brings it back)
- `reaction_handoffs` (map `issue_id:kind -> set of review comment IDs`, kinds `review` and `bot-review` only; a per-key cache over the persisted `reaction_handoffs` table, whose rows hold the IDs of the comments the issue's runs were given under the kind's prompt variable, recorded when a run exits normally; a present key is authoritative for the process and an absent key means the rows were not loaded, never that the set is empty; a comment is new when it is outside the set, the reported set, and the IDs the issue's running turn carries; dropped by a watch window at any counter value and by the terminal release, and never rebuilt by an escalation; no path deletes a row)
- `reaction_reported_comments` (map `issue_id:kind -> set of review comment IDs`, kind `bot-review` only; the IDs a spent-budget decision observed and a triage escalation reported to a person; deleted wherever the matching `reaction_attempts` counter is deleted and kept wherever it is kept; runtime-only, never persisted)
- `parked` (map `issue_id -> ParkedEntry`; issues held out of primary dispatch until a person acts on them; each entry records the identifier, the park reason, the issue's title, web link, type, and labels as read when the park was recorded (or, for a park recorded or restored without one, at the first read the orchestrator takes of the issue), the tracker state observed when the park was recorded, the run the park follows (its dispatch rule, stage lineage with the dispatch time of its pass, agent kind, and configured model; empty when the park records no run), the parking label the orchestrator applied and whether it is confirmed to have reached the tracker, and the park time, plus the session, prompt template, and stop statement of the run the park follows, which the park hands to the first run after a release; backed by the persisted park records, which are reloaded at startup)
- `park_releases` (map `issue_id -> ParkedEntry`; the park a person released by removing its label or changing the issue's state, kept with its release time and gesture until the issue's next primary dispatch from the poll tick consumes it or a new park of the issue replaces it; an issue ID is never in both `parked` and `park_releases`, and no dispatch gate, retry gate, snapshot, or attention reader of `parked` sees a release; a release by a worker exit that observed work is never recorded here; backed by the same persisted record with its release columns set, which startup loads into this map)
- `stage_hops` (map `issue_id -> hop record`; one record per issue since the issue's consecutive hop count last reset; each record holds the identifier, the count of consecutive hops, the latest hop's source rule, target rule, and target label, the outcome of the source run (`succeeded` or `no_change`), the source run's dispatch ID, the stage chain identifier of the source run, whether a read has shown the target label, the hop time, the issue's qualified identifier, title, web link, and type as the source run's dispatch read them, its state from the run's latest read, the issue's labels as the hop leaves them (the dispatch read's labels without the stage labels the hop removes and with the stage label it adds), the dispatch time of the source run's pass, and whether a run of the issue was dispatched since the hop; the observed flag is the durable stage hop hold (Section 8.1); the dispatched flag ends the issue's listing between stages, the interval from the hop until a run of the issue is dispatched; a reset deletes the record; backed by the persisted stage hop records, which are reloaded at startup)
- `usage_limit_holds` (map `(agent_kind, model_only, model) -> hold`; the new sessions a provider usage limit stops until it resets. A kind-wide hold has `model_only` false and an empty `model`; a hold on one model has `model_only` true and the configured model the refused run used, empty for the runtime's default model. Each hold records the reset instant, already capped, and the instant the hold began. Never nil; persisted, reloaded at startup before the first tick, and deleted when a run that started after the hold began exits normally. A hold whose reset has passed has no effect)
- `attention_acknowledgements` (map attention item `id` -> acknowledgement time; written by an acknowledge request, deleted by the tick step of Section 8.1, reloaded at startup; the item `id` is the one §13.7.2 defines)
- `pending_reactions` (map `issue_id:kind -> PendingReaction`; populated by worker exit on normal exits with SCM metadata when a CI status provider or SCM adapter is configured (a `review` entry only while `reactions.review_comments` is configured), and reconstructed at startup by `RecoverPendingReactions` for eligible handoff-stage runs, including `merge`-kind entries; consumed by the per-kind reconcile functions during the reconcile tick: `reconcile_ci_status` for kind `ci`, `reconcile_review_comments` for kind `review`, `reconcile_bot_review_comments` for kind `bot-review`, `reconcile_auto_merge` for kind `merge`, `reconcile_merge_conflicts` for kind `merge-conflict`, `reconcile_label_review_commands` for kind `label-review`, `reconcile_label_fix_commands` for kind `label-fix`, `reconcile_merge_completion` for kind `merge-completion`; an entry of a kind that escalates stays pending while its escalation stands, and carries the escalation's time, trigger, and subject; runtime-only, not persisted, except that startup rebuilds a standing escalation from `reaction_escalations`, also for an issue that `RecoverPendingReactions` does not reach)
- `held` (map `issue_id -> hold record`; the candidates the latest dispatch pass held back, each with the identifier, the issue's title, link, type, labels, and state as that pass read them, the reason, the details the reason carries (the blockers that hold the issue, the agent slot limit with its usage, the time a spend window resets, or, for a usage-limit hold, the reset time with the agent kind and the model scope the hold covers: `agent_kind`, `model_only`, and `model`), and the time the issue was first seen held; replaced by every pass that runs the dispatch loop and left as it is by a tick that returns before it; runtime-only, so a restart empties it; Section 13.3 describes what the runtime snapshot serves from it)
- `auto_merge_preflight_failed` (boolean): sticky after a startup auth-class preflight failure; cleared only by a successful one-shot transport-class retry or by an orchestrator restart.
- `auto_merge_preflight_retry_due_at` (timestamp): non-zero only when the startup preflight failed with a transport-class error and scheduled a single bounded retry; cleared by the reconcile tick that consumes the retry.

### 4.2 Stable Identifiers and Normalization Rules

- `Issue ID`
  - Use for tracker lookups and internal map keys.
- `Issue Identifier`
  - Use for human-readable logs and workspace naming.
- `Workspace Key`
  - Derive from `issue.identifier` by replacing any character not in `[A-Za-z0-9._-]` with `_`.
  - Use the sanitized value for the workspace directory name.
- `Normalized Issue State`
  - Compare states after `lowercase`.
- `Session ID`
  - Opaque string assigned by the agent adapter. Composition is adapter-specific.
- `Dispatch ID`
  - Opaque string the orchestrator mints each time it dispatches an issue, never reused. Identifies that dispatch's running session to the tool server and in `session_metadata`. Differs from `Session ID` because a resumed session keeps its session ID across dispatches, while each dispatch mints its own dispatch ID. Keys the run's history row and its retained agent events.


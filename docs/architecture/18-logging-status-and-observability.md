## 13. Logging, Status, and Observability

### 13.1 Logging Conventions

Required context fields for issue-related logs:

- `issue_id`
- `issue_identifier`

Required context for coding-agent session lifecycle logs:

- `session_id`

Message formatting requirements:

- Use stable `key=value` phrasing.
- Include action outcome (`completed`, `failed`, `retrying`, etc.).
- Include concise failure reason when present.
- Avoid logging large raw payloads unless necessary.

A configuration advisory is recorded once, at construction or at workflow load, and logged from nowhere else: not at the point it is recorded, and not from configuration construction run for any other purpose. The running orchestrator reports a recorded advisory through one `Warn` record per entry, at the tick that first draws it and again only after a tick whose effective configuration stopped drawing it; a dry run logs the same records once, before it fetches candidate issues. A reachable deprecated agent kind draws one such record, message `"agent kind is deprecated and will be removed in a later release"`, carrying `agent_kind` and `replacement_kind`. A configuration that names a retired agent kind draws one such record per converted kind, message `"agent kind was removed and its configuration was converted to the replacement kind; the conversion will be removed in a later release"`, carrying `agent_kind` (the retired kind) and `replacement_kind`; the same advisory is a `sortie validate` warning under the check `agent.kind.retired`, and its text names the converted references, the launch the replacement kind now performs, the settings not carried, and the remedy, without printing `agent.command` as written. An invalid or incomplete `token_rates` entry draws one such record per warning, message `"skipped invalid token rate entry"`, carrying `detail` (the warning text). Each deprecated notification form the configuration relies on draws one such record, message `"deprecated configuration in use"`, carrying `setting` (the key path of the deprecated form, or the implicit event `auto_merge.merged` or `budget.held`) and `replacement` (the `events` list to write, preceded by `escalation: none` for a reaction's `escalation: comment`); the same advisory is a `sortie validate` warning whose check names the form: `tracker.comments.on_dispatch.deprecated`, `tracker.comments.on_completion.deprecated`, `tracker.comments.on_failure.deprecated`, `reactions.<key>.escalation.deprecated`, `notifications.tracker_comment.implicit_auto_merge`, `notifications.tracker_comment.implicit_budget_hold`, and `notifications[<i>].events.missing`. Nothing is logged per event.

A retry whose selection the configuration in force changes emits one `Info` record, message `"retry dispatching on the selection the configuration in force gives it"`, carrying `agent_kind`, `template_id`, and `rule_name` of the frozen selection and `dispatch_agent_kind`, `dispatch_template_id`, and `dispatch_rule_name` of the selection it dispatches on. A retry that finds the adapter of its selected kind unavailable emits one `Error` record, message `"retry agent kind unavailable"`, carrying `rule_name`, `agent_kind`, `attempt`, `delay_ms`, and `error`, and is rescheduled (Section 8.4).

A request that makes a retry due (`POST /api/v1/retries/{identifier}/run`, §13.7.2) emits one `Info` record, message `"retry made due on request"`, through the issue-scoped logger, carrying `attempt` and `replaced_due_at_ms`, the due time the request replaced.

A request that records an acknowledgement of an attention item (`POST /api/v1/attention/{id}/acknowledge`, §13.7.2) emits one `Info` record, message `"attention item acknowledged"`, carrying `item_id` and `kind`, through the issue-scoped logger when the item concerns an issue. A repeat of an acknowledged item emits none. A failed save emits one `Error` record, message `"failed to persist attention acknowledgement"`, carrying `item_id`, `kind`, and `error`, and the item stays unacknowledged. A failed deletion of the acknowledgements of items that no longer exist emits one `Warn` record, message `"failed to delete attention acknowledgements"`, carrying `error`; the next poll tick that reads the candidate issues retries it.

A request that makes a pending reaction's check due (`POST /api/v1/reactions/{identifier}/{kind}/check`, §13.7.2) emits one `Info` record, message `"reaction check made due on request"`, through the issue-scoped logger, carrying `reaction_kind` and `replaced_next_poll_at`, the due time the request replaced. A request for a reaction that is already due emits none.

An issue that carries the stage labels of two or more dispatch rules emits one `Warn` record, message `"several stage labels found"`, through the issue-scoped logger, carrying `stage_labels` (every stage label the issue carries, as written in the workflow, in rule list order) and `rule_name` (the rule the evaluation selected). The record is emitted once per evaluation of the rules, on the poll tick and on a retry that is routed afresh, and never for a retry whose frozen selection stands.

A stage hop (§7.3) records its decision result on the run's `run_history` row and emits `stage.advanced` or `stage.not_advanced` (§10.4.7), and it emits its log records through the issue-scoped logger. The attributes `source_rule`, `target_rule`, `target_label`, `hop_count`, `hop_ceiling`, and `chain_path` recur: `hop_ceiling` is the per-issue ceiling in force, and `chain_path` is the rule chain through the source rule, rule names joined with ` -> `.

- A made hop emits one `Info` record, message `"stage hop made"`, carrying `source_rule`, `target_rule`, `target_label`, `hop_count` (after the hop), `hop_ceiling`, and `chain_path`.
- A made hop whose removal of a source stage label failed emits one `Warn` record instead, message `"stage hop made, stage labels left on the issue"`, carrying the attributes above plus `stage_labels_left` and `error` (the first removal error). The hop stands.
- A hop that is not made emits one `Warn` record, message `"stage hop not made"`, carrying `reason` (`ceiling` or `add_failed`), `source_rule`, `target_rule`, `target_label`, `hop_count` (before the hop), `hop_ceiling`, and `chain_path`. For `add_failed` it also carries `error` and `missing_stage_labels` (every configured stage label the dispatch read showed that a fresh read no longer shows), or `stage_label_read_error` when that read failed. The exit then takes the handoff write.
- A reset of the hop count emits one `Info` record, message `"stage hop count reset"`, carrying `trigger` (`handoff`, `terminal`, `not_active`, `parked`, or `unparked`) and `hop_count`.
- A failed direct read in the hold emits one `Warn` record, message `"stage hop target read failed, holding issue"`, carrying `target_label` and `error`; the issue is held for that tick.
- A hold released by a read that does not show the target label emits one `Info` record, message `"stage hop target label absent, hold released"`, carrying `target_rule` and `target_label`; the issue is then routed by the labels that read returned.

A failed write of a hop record, of its observation flag, of its dispatch flag, or of its deletion emits one `Error` record, message `"failed to persist stage hop"`, `"failed to persist stage hop observation"`, `"failed to persist stage hop dispatch"`, `"failed to delete stage hop record"`, or `"failed to persist stage hop result"` (the write of the decision result onto the run's row), carrying `error`; the runtime value stands in each case, and a failed result write leaves the row's decision fields empty while the hop and its event stand. A failed load of the records at startup emits one `Warn` record, message `"failed to load stage hop records, starting with none"`, carrying `error`. A failed `tracker_comment` send of a stage event emits one `Warn` record, message `"stage event comment failed"`, carrying `event_type` and `error`; every other destination's failure keeps its existing record. A hop adds no metric; the session events keep their bodies.

A failed write of a live-pass record (Section 19.2) emits one `Error` record, message `"failed to persist live pass"` or `"failed to delete live pass"`, carrying `kind` and `error`; the dispatch, exit, or hold proceeds. A failed clear of the records at startup emits one `Error` record, message `"failed to clear live passes"`, carrying `error`; startup continues.

A failed write of reaction activity (Section 19.2) emits one `Error` record, message `"failed to record reaction activity"`, through the issue-scoped logger, carrying `activity` and `error`; the act the record describes proceeds, and the station of that reaction later reads as having nothing to act on. A failed read of reaction activity for `GET /api/v1/state` emits one `Warn` record, message `"failed to read reaction activity"`, carrying `method`, `path`, and `error`; the route answers `200` with a `null` `pr_loop` on every active entry, and the next request reads again. The other routes answer a failed read with `503` (Section 13.7.2).

Four records carry the settings an attempt dispatches with (Section 8.4). The first dispatch of a claim emits one `Info` record, message `"issue dispatched"`, through the issue-scoped logger, carrying `agent_kind`, `rule_name`, `model`, and `effort`, the last two being the configured values and empty when unset. A retry or continuation dispatch emits the same four attributes on its `"retried issue dispatched"` record, beside `attempt`. An attempt whose resolved settings block fails an error-severity check emits one `Error` record and starts no session: `"agent settings refused"` on the first dispatch, and `"retry agent settings refused"` on the retry lane, carrying `attempt` and `delay_ms` as well. Both carry `error` (the preflight result naming every failed check), `rule_name`, `agent_kind`, and the first failed check's `check` and `diagnostic`. A retry whose claim held a rule's settings block on its previous attempt, whose selection still names that rule, and whose rule no longer carries a block for the kind, emits one `Info` record, message `"rule settings no longer present, attempt runs on the kind's top-level settings"`, carrying `rule_name` and `agent_kind`, once, on the attempt that runs without the block.

Handoff-evidence records are part of the required operator surface:

- A withheld verdict whose verification read (§11.5, §14.2) does not find the issue terminal emits a `Warn` record naming the verdict and carrying `turns_completed` plus the resulting `consecutive_absences` count. The standard issue context fields identify the affected issue.
- A withheld verdict whose verification read does find the issue terminal emits an `Info` record at the read site naming the discarded verdict, its reason, `state_source="verified"`, and `turns_completed`, instead of the `Warn` record above. The terminal disposition's own `Info` record then follows, carrying the verified state and the same `state_source="verified"` value.
- An `evidence not determinable` verdict emits an `Info` record under both `observed` and `strict`, carrying the policy and `turns_completed`. Under `strict`, the separate withheld warning is also emitted because that policy converts the verdict into the absence disposition, unless the verification read above finds the issue terminal.
- A run frozen to `tracker.handoff_evidence: off` emits none of these evidence records.

Parking an issue, whichever trigger produced it, emits exactly one `Warn` record, message `"issue parked"`, carrying `reason` (`agent_blocked` or `handoff_absence`), `parked_state` (the tracker state recorded, empty when unobserved), and `label` (the parking label applied). The consecutive-absence ceiling's park additionally carries `consecutive_absences`, `absence_ceiling`, and `ceiling_setting` (the dotted configuration path that produced the ceiling); a `blocked` park carries none of the three. A failed label write is recorded separately, message `"park label write failed"`, at `Warn`, and does not suppress the parking record. A poll tick that parks issues for consecutive absences reads the newest agent run of those issues in one call to the store, only on a tick that parks one. A failed read emits one `Warn` record, message `"park dispatch context read failed"`, carrying `error`, after which the tick parks every one of those issues without a run: the read never delays or prevents a park, which is what stops a loop of agent runs that produce no work. A park recorded without a run keeps none until it is released. Lifting a park emits one `Info` record, message `"issue unparked"`, carrying `trigger` (`state_changed`, `label_removed`, or `evidence_observed`) and `reason`.

An issue entering the per-issue budget-exhausted set on the poll tick's rebuild emits exactly one `Warn` record, message `"candidate held by budget ceiling"`, carrying the standard issue context fields plus `reason` (`money_budget`, `token_budget`, or `session_budget`), `used_sessions`, `budget_sessions`, when the token ceiling was evaluated for that issue, `used_tokens` and `budget_tokens`, and, when the money ceiling was evaluated for it, `used_usd` and `budget_usd`. It also carries `ceiling_setting`, the dotted configuration path of the setting that governs the fired ceiling, when `reason` maps to a known setting; a reason with no known governing setting emits no `ceiling_setting` attribute rather than an empty or invented one. The record fires once per hold: a tick that re-observes an already-announced hold under the same reason emits nothing further. The retry lane does not emit this record; a retry it blocks emits its own gate record, message `"token budget exhausted, blocking re-dispatch"` on the token axis, carrying `reason`, `used_tokens`, `budget_tokens`, `used_sessions`, and `budget_sessions`, or message `"money budget exhausted, blocking re-dispatch"` on the money axis, carrying `reason`, `used_usd`, `budget_usd`, `used_sessions`, and `budget_sessions`. A read of run history that fails while the poll tick describes the dispatch a hold blocks (Section 13.3) emits one `Warn` record, message `"budget hold dispatch context read failed"`, carrying `error`; the affected records stay without a dispatch context until a later tick reads again.

The in-flight token ceiling adds four records of its own, beyond the hold record above, so budget observability covers a run already in flight and not only a re-dispatch that never starts. A dispatch emits at most one of the two freeze records, never both. A dispatch of a session that can carry no token figure, because no source its kind attaches counts tokens, emits one `Warn` record, message `"token ceiling cannot bound this run"`, carrying `agent_kind`, `usage_arrival`, and `budget_tokens`, and also `error` when the baseline read failed on the same dispatch. A dispatch of a session that can carry a token figure but whose baseline read fails emits one `Warn` record instead, message `"prior token spend unknown, token ceiling bounds this session only"`, carrying `error` and `budget_tokens`. A run that can carry no token figure never gets the second record, because the ceiling does not bound that session either and saying so would contradict the first. A confirming read that fails while a running session is over the pre-filter, and whose session has not reached the ceiling on its own spend, emits one `Warn` record, message `"in-flight token ceiling check failed, run continues"`, carrying `error` and `budget_tokens`, at most once per run regardless of how many failing reads follow. A run recorded `budget_stopped` emits one `Warn` record, message `"run stopped by token ceiling"`, at that exit, carrying `reason`, `used_tokens`, `budget_tokens`, `issue_tokens_completed`, `session_tokens`, `sum_source`, and `ceiling_setting` from the moment the ceiling decided the stop. `sum_source` is `confirmed_read` when a read established the completed sum and `session_spend_alone` when the session's own spend reached the ceiling and no read was needed; `unmeasured_sessions` and `unaccounted_turns` join the record only in the first case, because only a read supplies those counts.

The money ceiling reads one shared query of the issue's run history with the token ceiling, so a failed read warns once per configured ceiling, with the message of that ceiling, and the prior holds of both ceilings stay in force. The in-flight money ceiling adds its own records, so a run already in flight is bounded by money as it is by tokens. A dispatch whose baseline read fails with a money ceiling set emits one `Warn` record, message `"prior spend unknown, money ceiling bounds this session only"`, carrying `error` and `budget_usd`. A confirming read that fails while a running session's estimate has carried the issue to the ceiling, and whose session has not reached the ceiling on its own estimate, emits one `Warn` record, message `"in-flight money ceiling check failed, run continues"`, carrying `error` and `budget_usd`, at most once per run. A run recorded `budget_stopped` by the money ceiling emits one `Warn` record, message `"run stopped by money ceiling"`, at that exit, carrying `reason`, `used_usd`, `budget_usd`, `issue_cost_completed`, `session_cost_usd`, `cost_complete`, `sum_source`, and `ceiling_setting`, and, when a read established the completed figure, `unmeasured_sessions` and `unaccounted_turns`. A figure below the ceiling that is a lower bound, because a run is unpriced, partial, or unmeasured, emits one `Warn` record, message `"money budget cannot be fully evaluated, allowing dispatch"`, carrying `used_usd`, `budget_usd`, `unmeasured_sessions`, `unaccounted_turns`, `partial_sessions`, and `unpriced_sessions`: once per issue while the poll tick's rebuild keeps finding it, and at every evaluation on the retry lane. Every `used_usd`, `issue_cost_completed`, and `session_cost_usd` on these records is estimated cost in USD (Section 13.5).

The instance spend ceilings (Section 8.4) emit the records below, through a logger that carries no issue fields, so each message stays stable for alerting. A window that begins to hold emits one `Warn` record, message `"spend ceiling reached, holding dispatch"`, once per hold, carrying `window` (`daily`, `weekly`, or `monthly`), `used_usd`, `used_usd_complete`, `budget_usd`, `ceiling_setting` (`agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, or `agent.max_monthly_cost_usd`), and `resets_at` in UTC. An evaluation that ends a hold emits one `Info` record, message `"spend ceiling hold ended"`, once per hold, because the window reset, the setting was raised or set to `0`, or a rate change lowered the estimate of a running session. It carries `window`, `budget_usd` (`0` when the setting is `0`), `used_usd` (absent when the setting is `0`), `ceiling_setting`, and `dispatch_held`, which is true while another window still holds. A window that does not hold and whose spend is a lower bound emits one `Warn` record, message `"spend ceiling cannot be fully evaluated, allowing dispatch"`, once per window start, carrying `window`, `used_usd`, `budget_usd`, `unmeasured_sessions`, `unaccounted_turns`, `partial_sessions`, and `unpriced_sessions`; an evaluation that finds the spend complete, or the window holding, re-arms it. An evaluation whose read of a window fails emits one `Warn` record, message `"spend ceiling check failed, keeping the last result"`, carrying `window` and `error`. A held candidate logs nothing, and a retry held by a spend window logs only at `Debug`, message `"retry held by spend ceiling, rechecking"`, carrying `reason`, `attempt`, and `delay_ms`.

The token warning threshold adds one record of its own, at most once per run: the live per-issue figure reaching the configured threshold emits one `Warn` record, message `"token warning threshold reached"`, carrying `used_tokens`, `warning_tokens`, `budget_tokens`, `issue_tokens_completed`, `session_tokens`, `ceiling_setting`, and `warning_setting` (the dotted configuration path `agent.token_warning_percent`) from the moment the threshold was reached. A run that never reaches the threshold emits no such record, and a run whose figure reaches or passes it more than once still emits exactly one.

Every `used_tokens`, `issue_tokens_completed`, and `session_tokens` on the budget and ceiling records above is uncached tokens (Section 13.5), so prompt-cache reads are not in it.

A run that ends under a non-zero ceiling, whose resolved usage arrival reports figures and which recorded none, emits one `Warn` record at its exit, message `"run reported no token usage, token ceiling could not bound it"`, carrying `agent_kind`, `usage_arrival`, and `budget_tokens`, once per run. The arrival is derived from the sources the kind attaches while the figures come from whatever that kind runs, so a protocol runtime that reports no usage fields keeps its kind's arrival, and this record is the only one that reaches that case: the dispatch records above all key on the attached sources and stay silent. A run that can carry no token figure is already announced by the first of them and never reaches this record.

A running session whose resolved usage arrival is `none` and whose runtime reports a usage figure, on an event or a turn result, emits one `Warn` record per run, message `"token usage discarded: agent kind attaches no source that counts tokens"`, carrying `agent_kind`, and the figure is discarded as Section 13.5 states.

A protocol session whose handshake names a runtime other than the one its configured agent command names emits one `Warn` record on the session logger, once per session, message `"agent runtime differs from its configured command"`, carrying `configured_runtime`, `handshake_runtime`, and `usage_declaration`, the declaration the handshake selected.

The notice this same hold delivers is recorded separately, at the delivery site rather than alongside the log record above. When the `tracker_comment` destination received the event, a successful write emits one `Info` record, message `"budget hold notice posted"`, carrying the standard issue context fields. A failed write emits one `Warn` record, message `"budget hold notice failed"`, carrying the standard issue context fields and `error`, and does not suppress the log record above. A hold whose notice has no destination emits neither record.

Every orchestrator event is routed to its destinations before it is delivered, and each outcome other than the `tracker_comment` outcome is logged at the delivery site. A destination whose send failed emits one `Warn` record, message `"notification delivery failed"`, carrying `event_type`, `destination` (`notifications[<i>]`, the position of the entry in the list), `notifier_kind`, and `error`. A destination whose send succeeded emits one `Debug` record, message `"notification delivered"`, carrying the same attributes except `error`. The `tracker_comment` outcome is logged by its producer under that producer's own message, so a tracker write failure is as visible as before the destination existed. A routing table that cannot be installed from a loaded configuration emits one `Error` record, message `"notification routing update failed"`, carrying `error`, and the table in force stays installed. Workflow validation normally rejects such a configuration first, so this record is a defensive path.

The periodic workspace sweep emits exactly one summary record per pass, at `Info` level, message `"sweep: pass complete"`, on every pass that produced a candidate set, including a pass over zero keys, a pass whose tracker read failed, and a pass that removed nothing. This is deliberate: a sweep that finds nothing to remove and says nothing is indistinguishable from a sweep that is not running at all, which is the failure mode this record exists to close. The record carries thirteen attributes:

- `candidates`, `excluded_running`, `excluded_retry`, `excluded_reaction`, `removed_terminal`, `removed_age`, `retained_in_window`, `retained_no_activity`, `retained_not_evaluated`, and `failed` are integer counts. The nine counters after `candidates` partition the candidate set, so `candidates` equals their sum on every pass; `retained_not_evaluated` is what makes that identity hold on a pass where the age bound did not evaluate anything.
- `retention_days` is the configured `workspace.retention_days` value for that pass; `0` means the bound is off.
- `age_pass` is `"on"` when the age bound evaluated its candidates that pass, `"off"` when `retention_days` is `0` or below the floor (`WorkspaceRetentionMinDays`), and `"unavailable"` when the persistence read is not configured or its query failed.
- `tracker_read` is `"ok"` or `"failed"`.

Each workspace removed by the age bound also emits its own `Info` record, message `"sweep: removed expired workspace"`, carrying `workspace_key`, `last_activity` (RFC3339), and `age_days`.

A tick that fetched its candidate issues ends with one `"tick completed"` record: `Info` on the first such tick after startup and on a tick whose attributes differ from those of the previous such tick, and `Debug` when every attribute is equal, so an idle orchestrator logs it at `Info` once after startup. A tick that ends on a failed dispatch preflight or a failed candidate fetch emits none and takes no part in the comparison. The record carries integer attributes for the blocker gate in addition to its existing ones: `held_by_blockers`, `blockers_unresolved`, `blockers_not_read`, and `blockers_incomplete`, each counting the candidates the tick held for that reason. It also carries `budget_exhausted`, the number of this tick's candidates in the per-issue budget-exhausted set after the rebuild.

A candidate the dispatch gate holds for a blocker reason produces exactly one of five per-issue records, and a pass whose reads were refused by the forge produces exactly one pass-level record in addition:

- Held by a non-terminal blocker: one `Debug` record, message `"candidate held by blocker"`, carrying the issue context fields plus `blocker_identifier` and `blocker_state` for the first non-terminal blocker.
- Held because its own blocker read was attempted and failed: one `Warn` record, message `"candidate blockers unresolved, holding issue"`, carrying the issue context fields and `error`.
- Held because the pass had already halted on another candidate's failure: one `Debug` record, message `"candidate blockers not read this tick, pass halted"`, carrying `error_kind` and no `error`, because nothing failed on this candidate.
- Held because its producer declared the list incomplete, with no read attempted: one `Debug` record, message `"candidate blocker list incomplete, holding issue"`, carrying the issue context fields and no `error`.
- Held because the pass had already spent its read budget: one `Debug` record, message `"candidate blockers not read this tick, holding issue"`, carrying `reads_spent` and no `error`.

A pass whose reads halted emits exactly one `Error` record, message `"blocker reads halted for this tick"`, carrying `error_kind`, `http_status` (`0` when the failure carried none), `operation` (`"fetch_blockers"`), and `held_unread`. A tick that dispatches nothing because every blocker read it attempted failed transiently, and that did not already emit the pass-level `Error` above, emits one additional `Warn` record, message `"tick dispatched nothing: every attempted candidate blocker read failed"`, carrying `reads_failed`, the count of reads the pass attempted and lost.

### 13.2 Logging Outputs and Sinks

Sortie does not prescribe where logs must go (stderr, file, remote sink, etc.).

Requirements:

- Operators must be able to see startup/validation/dispatch failures without attaching a debugger.
- Sortie may write to one or more sinks.
- If a configured log sink fails, Sortie continues running when possible and emits an operator-visible warning through any remaining sink.
- Every sink receives records already masked, as §15.3 states.

### 13.3 Runtime Snapshot / Monitoring Interface

If the implementation exposes a synchronous runtime snapshot (for dashboards or monitoring), it should return:

- `running` (list of running session rows)
- each running row should include `turn_count`
- each running row should include `tokens_measured`, meaning at least one usage measurement the orchestrator records has been reported so far in that session; it stays false throughout for a session whose resolved `usage_arrival` is `none`, whatever its runtime reports
- each running row should include `usage_arrival` and `usage_attribution`, the usage-reporting disposition derived from the sources that session's kind attaches, frozen at dispatch
- each running row should include `tokens_pending`, true only when the frozen arrival settles at most one figure per turn, the session is measured, and the turn that figure would settle for is still in flight
- each running row should include `tokens_awaited`, true only when the session has recorded no figure and the moment its frozen arrival names for one has not passed: no turn has begun for an arrival that reports during a turn, and no turn has ended for one that settles at most one figure per turn. A row whose arrival reports figures and whose `tokens_awaited` is false recorded nothing and will record nothing, so a consumer must not present it as a figure still to come
- each running row should include `api_requests_measured`, true when the row's `api_request_count` is a count of model API requests the session measured
- each running row should include the issue's spend and the limits it runs against: the usage of the issue's completed sessions, read from run history at every dispatch whether or not a ceiling is set and refreshed by each confirming read of an in-flight ceiling; whether the issue's token sum is its whole spend, which counts the running session's unaccounted turns as its worker reports them while the session runs; the money ceiling, the token ceiling, and the session budget in force as of the latest poll tick; and the estimated cost of the running session and of the whole issue, each with a state that names why a figure is partial or absent. Section 13.7.2 gives the keys and how to read them
- each running row should include the uncached figure of the session and of the whole issue, the totals less cache reads as Section 13.5 defines it. Section 13.7.2 gives the keys
- each running row should include `rule_name`, `configured_model`, and `configured_effort`, the dispatch rule the session runs under and the model and reasoning level its resolved settings block carries, empty (and omitted from the JSON form) when unset; `model_name` keeps the model the runtime reported, which can differ from the configured one under routing aliases and runtime fallbacks
- each running row should include `dispatched_at`, when Sortie dispatched the first run of the issue's current pass, in UTC. A pass is the runs that share a stage chain identifier: a retry, a reaction continuation, the next stage of a stage hop, and a restart continue the pass of the run they follow, so `dispatched_at` stays fixed across them while `started_at` is the start of the session in flight. It is never empty on a running row
- each running row should include `dispatch_id`, the dispatch ID Sortie minted for the run when it dispatched it. A dispatch ID names one run, is never reused, across restarts included, and is the key under which the run's agent events are retained and streamed (Section 13.9). It is never empty on a running row
- each running row should include `last_event_at`, `quiet_at`, and `stall_at`, so every client shows the same stall indication from the same rule. `last_event_at` is the time of the run's latest agent event of any type, liveness pings and events that are not retained included, and `null` before the first event. The reference point is `last_event_at`, else `started_at`, the same reference the stall check of Section 8.5 measures from. `quiet_at` is the reference point plus half of `agent.stall_timeout_ms`, and `stall_at` is the reference point plus `agent.stall_timeout_ms`, the value the stall check compares with as of the latest poll tick; the stall check stops the run on the first poll tick after `stall_at`. Both are `null` when stall detection is off (`agent.stall_timeout_ms` of zero or less). Each running row also serves its stall sentence as `stall_text` and `stall_text_full`, once `generated_at` reaches `quiet_at`; both are `null` before that and with stall detection off. The silence in the sentence is rounded down, so a served sentence is true when served and stays true until the next event. With stall detection off the row serves `stall_off_text`, the sentence that says Sortie will not stop the session as stalled; it is `null` while stall detection is on. The rule lives in the server: a client computes no threshold and only compares its clock with these times
- each running row should include `stage_lineage` and `stage_position`, where the row's dispatch rule stands on the stage chains. `stage_lineage` names the chain and the stage hop that reached the run, as on a retry row, and is `null` unless a hop reached the run. `stage_position` places the rule on its chain with the hop count against the hop ceiling, and is `null` when the rule is on no chain, so a workflow without stage chains serves `null` on every row. Section 13.7.2 gives the keys
- each running row that is a reaction continuation should include `reaction_kind` and `reaction_config_key`, the kind of the continuation and the `reactions` key of WORKFLOW.md it is configured under, with `pull_request` (the number and repository of the pull request it works on) and, for a kind that counts tries, `reaction_attempt`, which try of its reaction the session is. A session that is no continuation carries none of the four
- each running row should include `issue_title`, `issue_url`, and `issue_labels`, the issue's title, web link, and labels. The title and link are as the session was dispatched with them. The labels are those of the read last recorded for the session, the same read as `state`: the read Sortie takes right after its own in-progress transition, else the latest reconciliation read, and an older read never replaces a newer one, so a label a person or a hook changes during the run appears within a reconcile tick. The agent's prompt keeps the labels of the dispatch read. `issue_title` and `issue_url` are empty (and omitted from the JSON form) when the tracker returned none; `issue_labels` is always present, an empty list when the issue carries none, lowercase in the tracker's order as Section 11 normalizes them
- each running row should include `issue_type` and `state`, the issue's tracker type as the session was dispatched with it and the tracker state the orchestrator last read for the session. `issue_type` is omitted from the JSON form when the tracker returned none
- every such string, `issue_type` and `state` included, is masked as Section 15.3 states, in the snapshot rather than in the state it is copied from, because stage advancement reads the labels as the tracker returned them; the repository of a continuation's pull request is masked the same way
- `retrying` (list of retry queue rows). Each row carries `issue_title`, `issue_url`, `issue_type`, `issue_labels`, and `state` beside the display identifier, the read of the issue the retry was scheduled with. The read is persisted with the retry and restored at startup, so a restart serves what was served before it, and a reaction retry scheduled from a pending reaction rebuilt at startup carries the read its run recorded. A retry a pending reaction schedules carries the reaction's issue facts, so its `state` is the handoff target after that reaction's handoff, and so does a budget-exhausted record the retry lane makes from that retry. A retry that holds no read, because it was restored from a row persisted before the identity was recorded, takes the first read the orchestrator takes of its issue: the next poll tick whose candidates list it, or its next fire. A retry that holds a title or link and no labels holds a partial read, as one restored from a row persisted before the type, labels, and state were recorded does; it takes the type, labels, and state of the next read in the same way. A title or link already held is never replaced. Each row also says why the retry waits. `trigger` is the `sortie_retries_total` label of the schedule that created the retry (`error`, `continuation`, `timer`, `stall`, or `ci_fix`); it is omitted when that schedule records none, as for a review, bot review, merge conflict, or label command continuation, which `reaction_kind` names instead. The trigger is persisted with the retry and restored at startup. `reaction_kind` names the reaction behind the retry, and `reaction_config_key` the `reactions` key of WORKFLOW.md that kind is configured under. `rule_name` is the dispatch rule frozen with the retry. `stage_lineage` names the stage chain and the stage hop that reached the run the retry follows, and `stage_position` places the retry's rule on that chain. `agent_kind` and `configured_model` are the agent kind of the run the retry follows and the model that run's resolved settings block carries. `dispatched_at` is when the first run of the pass the retry continues was dispatched. The model and the dispatch time are persisted with the retry and restored at startup, so a restart serves what was served before it. `paused` is true while the issue's state does not permit dispatching the retry. When the retry timer puts a retry back instead of dispatching it, the row's error is the reason it was put back, never the error of the run before it
- `agent_totals`
  - `input_tokens`
  - `output_tokens`
  - `total_tokens`
  - `cache_read_tokens`
  - `cache_write_tokens`
  - `seconds_running` (aggregate runtime seconds as of snapshot time, including active sessions)
  - `unmeasured_sessions` (cumulative count of ended sessions whose usage was never recorded; persisted alongside the counters above and restored on startup, so it names the sessions they excluded across every restart, not just the current process)
  - `running_unreported` (count of the current running set whose kind reports usage, whose figure has not arrived, and whose `tokens_awaited` is true, so it may still arrive)
  - `running_non_reporting` (count of the current running set reporting no usage at all: either the resolved `usage_arrival` is `none`, in which case any figure the session's runtime sends is discarded, or the arrival reports figures and the moment it names for one passed with nothing recorded)
- `rate_limits` (the provider usage windows each agent kind reported, keyed by agent kind; `{}` when no kind reported). Each member carries `reported_at`, the newest report time among every window the kind holds, and `windows`, the kind's current windows in a stable order, each with `label`, `used_percent`, `status`, `resets_at`, `reported_at`, and `amount`. A member holds the newest report of each usage window since the process started, in memory only. A window past its reset time is not served, so `windows` is `[]` for a kind whose windows have all reset. `amount` is the window's use in its vendor's own unit, with `used`, `limit`, and `unit` (a plural display name such as `credits`), never converted to another unit, and `null` when the window reports no amount. `used_percent`, `status`, `resets_at`, and `amount` are `null` when the report did not carry them, never `0` or an empty string, and `status` is `allowed`, `warning`, or `exhausted`
- `budget_exhausted_count` (number of issues currently blocked by a re-dispatch budget; always present)
- `budget_exhausted` (list of blocked-issue records, sorted by identifier; always present, empty when the set is empty). Each record carries the issue's ID and identifier, the reason (`money_budget`, `token_budget`, or `session_budget`; `money_budget` takes precedence over `token_budget`, and `token_budget` over `session_budget`, when one issue reaches more than one gate), the used and budgeted session, token, and money figures, the count of the issue's sessions a budget ceiling stopped in flight (present only when a ceiling read the issue's usage), the time the hold began, the issue's title, tracker link, type, labels, and state, and the dispatch the hold blocks: its dispatch rule, stage lineage and position, agent kind, configured model, and the dispatch time of its pass. The issue facts come from the read of the poll tick that last rebuilt the set, or from the retry the hold was found on until the next tick. The dispatch context is the retry's when the retry lane recorded the hold, and a later rebuild keeps it. A hold the poll tick finds with no prior record, as after a restart, a stage hop, a park release, or a person's return of the issue to an active state, takes the context of the dispatch it blocks: the target rule of a stage hop still waiting for its next dispatch, resolved from the configuration in force, and otherwise the issue's latest agent run, read from run history through one batched store call and never from the tracker. A failed read is logged and leaves those records without a context until the next tick reads again, and an issue with no agent run has none. The set is rebuilt per tick from those gates (Section 8.4) and also updated by the retry lane when it discovers a hold between ticks.
- `parked_count` (number of issues currently held out of primary dispatch; always present)
- `parked` (list of park records, sorted by identifier and then issue ID; always present, empty when no issue is parked). Each record carries the issue's ID and identifier, the display identifier when the tracker needs a qualified form, the issue's title and tracker link, the reason (`agent_blocked` or `handoff_absence`), the tracker state observed when the park was recorded, the parking label the orchestrator applied, whether a read has shown that label on the issue, and the time the park was recorded. A record also carries the run the park follows (its dispatch rule, stage lineage with the dispatch time of its pass, agent kind, and configured model) and the issue's type and labels as read for the park, and its `state` is the tracker state observed at the park, the value `parked_state` holds. The run is the one that stopped for a park made at a worker exit or at the retry timer, and the newest agent run of the issue in run history for a park the poll tick makes. The title, link, type, labels, and state are masked as Section 15.3 states, in the snapshot rather than in the state they are copied from. The park persists the run, the title, the link, the type, and the labels and restores them at startup, so a restart serves what was served before it. A park recorded before the upgrade that introduced the run takes it from the newest agent run of its issue that started at or before the park, filled when the store migrates, and holds no type or labels until a read supplies them. A park recorded without a run, because the poll tick's read of run history failed, serves no rule, position, agent, model, or dispatch time until it is released. A park that holds no read of its issue, because it was recorded or restored without one, takes the first read the orchestrator takes of the issue: the next poll tick's candidate, or one single-issue read when the candidate fetch omits the issue and the batched state-and-label read of the release step finds it (Section 8.1). A park that holds a title or link and no labels, as one persisted before the type and labels were recorded does, takes the type and labels of the next poll tick's candidate only, with no tracker call of its own, and keeps the title and link it holds. The labels a park stores leave out its own parking label, because a new park's labels are read before the label is written and a later read carries the label once it is applied. A title or link already held is never replaced, and a park that holds a whole read is never read again
- `pending_reactions` (list of pending pull request reactions, one row per issue and kind, sorted by identifier, then issue ID, then kind; always present, empty when none). Each row carries the issue's ID, identifier, display identifier, title, and tracker link; the reaction's kind and the `reactions` key of WORKFLOW.md it is configured under; the pull request it watches (number, repository, web link, head branch, and head commit); when the orchestrator created the reaction, when the pass of the run it follows was dispatched, when its next check is due, and when it stops watching unless its watch restarts first; why it waits for that check; the tries it used of its ceiling; when its standing escalation began and why; and what its latest check observed. Each row also carries the dispatch rule of the run the reaction follows, the position of that rule on its stage chain, and that run's agent kind and configured model, with the issue's type, labels, and state as that run read them. When that run's exit moved the issue to `tracker.handoff_state`, the row's `state` is the handoff target, because the run's read is the active state the transition replaced; every other exit records the run's read unchanged. The identity and every other runtime-derived string, the issue type, labels, and state included, are masked as Section 15.3 states, in the snapshot rather than in the state they are copied from. Pending reactions are runtime-only except for an escalation: the orchestrator stores a standing escalation with its time, its trigger, and the tries it used, and the startup restore brings it back, also for an issue that the recovery of pending reactions does not reach. A restart therefore rebuilds the rows without a link, wait reason, or check result, but with their escalation; the dispatch time, rule, agent kind, and model come back from the run history row the reaction is rebuilt from, and a rebuilt row carries the issue's title and link only, with no type, labels, or state, because run history records neither. Section 13.7.2 gives the keys and how to read them
- `between_stages` (list of issues between stages, sorted by identifier and then issue ID; always present, empty when none). An issue is between stages when a stage hop was made and no run of the issue was dispatched since. Each row carries the issue's ID, identifier, and display identifier, the title, tracker link, type, and state the hopping run read, the issue's labels as the hop leaves them (the run's read without the stage labels the hop removes and with the stage label it adds), the stage the issue left, the stage it reached, the stage label the hop added, the time of the hop, and the dispatch time of the pass the hopping run belongs to. The row also names the stage reached as its dispatch rule, places that rule on its stage chain, and carries the agent kind and configured model a dispatch of that rule resolves to under the configuration as of the latest poll tick, which the orchestrator refreshes every tick and sets at startup so that a hop made between ticks already finds them; each is omitted when that configuration names no rule with a stage label under that name. An issue joins the list when its hop is made and leaves it when a run of the issue is dispatched, whichever lane dispatches it, or when a reset of the hop count drops the hop record. The snapshot leaves out an issue that has a running session or a queued retry, so no issue is in `between_stages` and in `running` or `retrying` at once. The hop record persists the display identifier, title, link, type, labels, state, pass dispatch time, and whether a run was dispatched since the hop, and a restart restores them, so it serves the rows it served before. A hop record written before the upgrade that introduced the type, labels, and state serves none of them for as long as it lasts, which is until the next stage dispatches. The title, link, type, labels, and state are masked as Section 15.3 states, in the snapshot rather than in the state they are copied from. A row also carries the hold the latest dispatch pass gave the issue, as `held` describes it, and none while the pass recorded no hold for it. Section 13.7.2 gives the keys and how to read them
- `active` (list of the issues the row lists hold, one entry per issue; always present, empty when they hold none). An issue appears once, however many of `running`, `retrying`, `budget_exhausted`, `parked`, `pending_reactions`, and `between_stages` hold it. The entry's `status` is the per-issue status the per-issue route gives the issue, so it names the list the entry's facts come from: the issue's row in that list, and in `pending_reactions` the issue's first served row whose escalation stands, else the review row awaiting approval when every other row reports nothing outstanding (Section 13.7.2), else its first served row. The entry carries that row's identity (ID, identifier, display identifier, title, and tracker link), its dispatch rule, stage position, agent kind, configured model, and dispatch time, and its status sentences, each as the row serves it and so already masked. The entry also carries what each configured reaction recorded in the pass of that row (`pr_loop`, Section 13.7.2). An entry on `pending_reactions` also carries the kind of the reaction row that decided it. The orchestrator orders the list by the place it keeps for each issue in memory: the earliest dispatch time the issue's rows carried when it joined the lists, a missing time placing it before every timed issue, then the identifier, then the issue ID. An issue keeps its place through a move between lists, through a new pass begun while it is listed, and when its dispatch time turns from missing into a time; it takes a new place when it returns after no list held it. The orchestrator stores no place, so a restart places each issue by its restored rows. A held issue (`held`) is never an active entry. Section 13.7.2 gives the keys and how to read them
- `held` (list of the eligible candidates the latest dispatch pass held back, sorted by identifier and then issue ID; always present, empty when the pass held none). A dispatch pass is one run of the dispatch loop of a poll tick. The pass records each candidate it holds back with one reason and its details: the blockers that hold it (every blocker whose state is empty or not terminal, in the order the tracker returned them, with its identifier and state), blockers the pass could not read or did not reach, a blocker list the tracker declared incomplete, a full agent slot limit (the scope, global or per state, the sessions running, and the limit), no SSH host below its per-host cap, a spend window that holds (with the time it resets), a usage-limit hold on the candidate's agent kind (with the time it resets and, when the hold covers one model, that model), or a stage hop whose confirmation read has not shown the hop's label. When a capacity limit stops the loop, the pass classifies the remaining candidates by the eligibility gates alone and reads no blocker for them. Each row carries the issue's ID, identifier, display identifier, title, tracker link, type, labels, and state as the pass's candidate read them, masked as Section 15.3 states, in the snapshot rather than in the state they are copied from, and the hold with the time the issue was first seen held. A held issue is a candidate, not an active issue. The snapshot lists it only when none of `running`, `retrying`, `budget_exhausted`, `parked`, and `pending_reactions` holds it and no hop record puts it in `between_stages`, so an issue appears in one place; an issue between stages serves its hold on its `between_stages` row. The record is runtime-only. A pass that runs the dispatch loop replaces it. An issue that the pass holds keeps the time it was first seen held, whatever its reason, and an issue the pass does not hold drops out. A tick that returns before the loop, because the dispatch preflight failed, the candidate fetch failed, or the one-shot run needs no poll, leaves the record as it is. A restart empties it. Section 13.7.2 gives the keys and how to read them
- `config` (the effective workflow shape: the tracker kind and project, the tracker states, the dispatch rules in order with how each is selected, the stage chains their `next` links form, the hop ceiling, the reaction kinds the process applies with their limits, escalation, and timing, the token rates, the session, token, and money budgets, the poll interval, the concurrency caps, the SSH hosts, and whether each agent selection reports token usage). It reports the configuration in force after the last successful load, except that a reaction appears exactly when the process applies it, with the settings it applies, the tracker kind and project are the ones the process started with, and the token rates are the ones in force, which reload with the workflow. It carries no credential. Section 13.7.2 gives the keys and how to read them
- `health` (the health of Sortie's own checks: `database`, `preflight`, `workflow`, `poll`, and `scm`, the last `null` when no reaction uses a source-control provider). Every check reports whether its latest run failed, when it last ran, when it last passed, how many runs in a row have failed, when the first of those failed, and the latest failure's error text. `database` is the ping the poll tick makes of the store after it applies the configuration and before it reconciles. `preflight` is the result of the tick's dispatch preflight, with the error text of the failures it found. `workflow` is the loads of the workflow file, the watcher's and the reload each tick's preflight forces, with the name of the file in effect, so a reload that fails is reported while the previous configuration stays in effect. `poll` is the poll loop: one run per poll tick, whether the timer or a refresh ran it, with how the tick ended, the tracker's latest read error, and when the next poll is due. `scm` is the reads the orchestrator makes of the source-control provider the reactions share; it reflects the most recent provider read across every pull request, so its last run dates that read, and a provider that no reaction reads is not checked. A check's error text is masked as Section 15.3 states, in the snapshot rather than in the record it is copied from. Section 13.7.2 gives the keys and how to read them; the poll check also carries a sentence that words it
- `attention` (list of what needs a person; always present, empty when nothing does). The orchestrator derives the list on each request from the snapshot above and the `health` checks and stores it nowhere, so an item exists exactly while its cause holds. A person can acknowledge an item, and Sortie stores that acknowledgement, so it survives a restart and lasts while its item does. Its causes are a parked issue, an issue held by a per-issue budget ceiling, a pending reaction whose escalation stands, a workflow file that failed to load, and a dispatch preflight that fails for a reason other than that load. Each item carries the issue it concerns (none for a cause that concerns the process), when its cause began, when a person acknowledged it (none while unacknowledged), a severity, the one action a client should offer, the facts behind it, and wording for why it needs a person and what clears it, taken from the status sentence table. The attention count leaves acknowledged items out. Section 13.7.2 gives the keys and how to read them
- `version` and `started_at` (the Sortie version, and the time the process started, taken before the startup work so it is the instant the readiness probe counts its uptime from). Section 13.7.2 gives the keys

Recommended snapshot error modes:

- `timeout`
- `unavailable`

### 13.4 Optional Human-Readable Status Surface

A human-readable status surface is optional and implementation-defined. When the HTTP server is enabled (Section 13.7), the dashboard application under `/ui/` (Sections 13.7.1 and 13.7.4) is the concrete realization of this surface.

If present, it should draw from orchestrator state/metrics only and must not be required for correctness.

Live status answers what is happening now. A second, offline reporting surface answers what happened over a past window: it reads persisted run history, the persisted retry queue, and the orchestrator's record of the passes it holds now, still without contacting the tracker, the coding agent, or a running orchestrator, and summarizes outcomes, durations, turn counts, and token spend, optionally grouped by the recorded run attributes and bounded by a time range. It groups runs by stage chain, listing only the chains in which a hop was due or made and leaving out runs without a chain; its text form bounds that listing to the chains with the most runs and states how many chains it left out, while its machine-readable form lists them all. It also draws one funnel per stage chain definition, chains that share a stage forming one definition: one row per stage in chain order, counting the stage's finished runs, the passes that reached the stage by how each arrived and by how each left it, and the median time a pass spent there. A pass that a running session or a queued retry holds is in the stage, and a pass that a budget holds stops as budget held. It reports the cost recorded with each run (Section 13.5) and prices nothing itself, so its figures do not move when rates change; a run recorded without a price counts as unpriced rather than as a guessed cost. Because it reads only what earlier runs and the orchestrator already recorded, a database written by an older schema yields a reduced set of figures, and the surface reports which figures it could not derive instead of presenting a zero. This surface performs no state mutation and is never required for orchestrator correctness.

A figure can be missing for two distinct reasons: the database predates the schema that records it, or nothing measured it because the coding agent behind the run reported no token usage. The schema tier alone no longer separates the two: a full-tier report can still carry a null `tokens` summary or breakdown row when every run in range is unmeasured. `tokens_unmeasured_runs` is the field that distinguishes a figure missing for want of a schema from one missing for want of a measurement. This supersedes the clause in ADR-0019 (`docs/decisions/0019-keep-usage-data-on-the-host.md`) stating that `schema_tier` alone tells a consumer whether token and cost figures were available at all.

### 13.5 Session Metrics and Token Accounting

A run is measured when the runtime reported at least one usage figure for the session and the adapter carried it into the recorded counters, or when the worker never entered an agent turn, because a run that launched no agent spent exactly zero. A run is unmeasured when an agent turn began and no usage figure ever arrived; its recorded token figures are zero and that zero carries no information. A measurement of zero is a measured run whose reported figures are zero, which is a legitimate statement recorded as measured. A figure reported for a session whose resolved usage arrival is `none` is not a measurement: it enters no token counter, persisted row, or reporting surface, and the run stays unmeasured unless it never entered an agent turn.

The run record carries this distinction alongside the token counters. An unmeasured run contributes nothing to any token counter and is excluded from cost pricing. It advances no Prometheus token counter, and the series of its model keep what other runs added (`0` when none), the same as a run that never emitted a usage event.

The run's usage includes the credential-verification step (§10.9) when the run performed it: the step's own request is a model request like any other, counted in the run's token figures, its API request count, and its cost estimate, and the working session's figures continue its series rather than starting over from zero. The worker captures the step's componentwise watermark once the step ends and adds it to every later working-session figure, event and turn result alike, before that figure is folded into the run's totals and before it is relayed; an all-zero working figure, or a step that reported nothing, changes nothing. A verification `SpendUnaccounted` result raises the run's unaccounted count exactly as a working turn's does. A run that skips the step because a fresh proof of its agent setup stands carries no verification spend, relays no verification event, and applies a zero offset, so its figures and cost estimate exclude the step; because no verification request marks it measured, a run whose resolved usage arrival is `none` is reported unmeasured. The relayed verification events carry the message `verifying the agent credential` and are `token_usage` or `notification` only, with no `session_id`, `agent_pid`, tool field, or usage window.

Token accounting rules:

- Agent adapters normalize token counts before emitting events. The orchestrator receives `{input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens}` directly, and, from an adapter whose runtime tells models apart, the same counts per model (Section 10.3).
- For absolute totals, track deltas relative to last reported totals to avoid double-counting. The `cache_read_tokens` and `cache_write_tokens` fields follow the same cumulative-delta accounting as `input_tokens` / `output_tokens`. Deltas are accumulated from any event carrying a non-zero usage payload, not only `token_usage` events, so an adapter can attach the authoritative run-cumulative snapshot to a turn-finalization event without losing it.
- The uncached figure is `total_tokens` less `cache_read_tokens`, floored at zero, and it keeps cache writes. An issue's figure sums its runs' figures, each floored on its own, so one run's excess cache reads cannot cancel another run's spend. It is not fresh input (`input_tokens` less both cache counters): an agent kind that reports no cache writes counts its newly cached input as uncached, so the figure compares across kinds. Every dashboard token figure is the uncached figure, and cache reads appear as a figure of their own (Section 13.7.1). The report and the JSON API serve the uncached figure beside the counters it derives from, so `uncached` plus `cache_read` equals `total` on every object that serves both, unless one of its runs reported more cache reads than total tokens, in which case the sum exceeds `total` (Section 13.7.2). The figures compared with `agent.max_tokens` are the uncached figure.
- `api_request_count` is incremented monotonically, and only, per `token_usage` event; a usage-bearing terminal event does not count as an additional request. The count is a measurement of API requests only when the session's resolved `usage_arrival` is `incremental` and either a figure has arrived or no turn has begun. A kind resolving `turn_end` settles the count at most once per turn and never measures requests, and a session whose runtime stopped delivering per-request figures reports the count as unmeasured rather than as zero, so a consumer never reads a fabricated zero where the declaration alone promised a request count.
- `tokens_pending` distinguishes a settled figure from one still in flight: it is true only when the resolved `usage_arrival` is `turn_end`, the session is measured, and the turn that figure would settle for has not yet reached a terminal event. A consumer presenting the current token total alongside this flag can tell an operator the figure excludes the turn in progress, rather than presenting a stale total as final.
- Accumulate aggregate totals in orchestrator state (`agent_totals`).
- At session exit, the session's token totals, its per-model parts, and its recorded cost are written to the `run_history` row and its `run_model_usage` rows alongside the aggregate update. The worker derives the run's final usage, its model name, and its request count from every agent event it relays during the run, self-review turns included. The final usage is reconciled from the worker result before that row is written, and the model name and the request count before the `session_metadata` row is written, so an event dropped on its way to the orchestrator cannot lower the recorded total, leave the `session_metadata` row without the model its figure came from, or leave a measured request count below the number of `token_usage` events the worker relayed. The per-issue token budget (`agent.max_tokens`) sums the uncached figure of the issue's `run_history` rows; the in-flight lane adds the running session's live in-memory uncached figure, which is not the same reading the `cost_budget` tool serves. The tool adds the uncached figure of the running session's recorded `session_metadata` row instead, written at most once per throttled write interval, so its advisory reading trails the in-flight lane's enforced figure by up to that interval; the exceptions, made immediately rather than waiting for that interval, are the one write per run that carries the figure for which "token warning threshold reached" (§13.1) was logged, and the write that follows a change in the running session's unaccounted-turn count, which updates the session's existing row only, so the tool reads `used_tokens_complete` as false from its next call.

Pricing rules:

- One pricing component holds every pricing rule: rate parsing, the price-table layers, model-name matching, and the estimate. The orchestrator and the `cost_budget` tool server share it to price runs; the HTTP server's prices route (Section 13.7.2) reads it to state the rates and the state of every price source and prices no run; the rest of the HTTP server and `sortie stats` price nothing and read the recorded cost. No other component reads `token_rates` rate values. Rates are parsed at every startup and every workflow load, whether or not the HTTP server is enabled.
- A run is priced per model. Its token counts are split into one part per model: the run's per-model token totals, attributed by the agent adapter contract's per-model rule, the attribution the `model` label of `sortie_tokens_total` uses. The parts sum to the run's totals class by class.
- Each part takes its rates from the first layer that has an entry for it, highest precedence first. First, the operator's `token_rates`: an entry keyed by the run's agent kind prices every model of that kind alike, and otherwise an entry keyed by a model name prices that model. Second, the shared table fetched at runtime, when `price_table.fetch` is enabled and a fetched table has passed validation. Third, the table embedded in the binary, unless `price_table.embedded` is `false`. An operator table named by `price_table.replace_file` replaces the second and third layers entirely; one named by `price_table.replace_url` replaces them once a download of it has passed validation, and they price until then. `token_rates` entries still lie over either. A per-model `token_rates` entry for a model that some table in force lists may set any subset of the rates and takes the rest from that table; one for a model no table in force lists should set every rate, and one that does not draws a `sortie validate` warning and prices the classes it sets.
- The embedded table is a snapshot of the published LiteLLM `model_prices_and_context_window.json` at a pinned upstream revision, and it ships with the upstream copyright and license notice. The snapshot is refreshed to the latest upstream commit that changed the file, by a scheduled pull request and by a maintainer command, and a refresh is accepted only when it passes the shared validation against the snapshot it replaces; a release embeds the snapshot its source holds. The snapshot keeps every model entry with its provider and its input, output, cache-read, and cache-write rates, in the upstream format, so it maps and prices every name as the full file of its revision does. It records the time that revision was committed as the date its rates were taken. An operator table and a fetched table use the same format and pass the same validation.
- A model name, reported by a runtime or configured for a run, is mapped onto a table key by one matching rule that serves every agent kind alike and the per-model keys of `token_rates` too. A name maps to the key it equals; failing that, to the one key it equals once letter case and the choice of a dot or a hyphen between two digits are set aside (`claude-opus-4.6` maps to `claude-opus-4-6`), and to none when two keys are equal in that sense; failing that, a name of the form `provider/model` maps to the one entry the table lists for `model` under that provider, and the rule knows the table's own name for a provider that runtimes spell differently (`anthropic/claude-sonnet-4-5` maps to the `anthropic` entry `claude-sonnet-4-5`, and `google/gemini-2.5-pro` to the `gemini` entry `gemini/gemini-2.5-pro`); when it finds two entries, the name maps to none. A name that maps to no key has no rate: matching never guesses, so one model name being a prefix of another never makes them match, and a name a runtime stamps on messages it writes itself, written between angle brackets, is not a model. A per-model `token_rates` key matches a name that equals it; failing that, a name that maps to the table key the entry's own key maps to; failing that, for a model no table lists, a name equal to the key once letter case and the choice of a dot or a hyphen between two digits are set aside. When two entries match a name only by the second or third step, neither prices it.
- A part prices each class once at its own rate: fresh input (`input_tokens` minus the two cache counters, floored at zero) at the input rate, cache reads at the cache-read rate, cache writes at the cache-write rate, and output at the output rate. A per-kind `token_rates` entry prices nothing unless it carries both an input and an output rate, and it prices an unset cache-read or cache-write rate at its input rate, so a deployment that priced with per-kind entries sees the same figures. No other layer ever substitutes the input rate: a class the part spent with no rate in its layer contributes nothing and makes the part `partial`. A per-kind entry prices the parts it covers together, on their summed tokens, and records each part's own share beside them. Outside a per-kind entry a part is priced on its own tokens, and a run's cost is the sum of its parts' costs.
- A run's cost status is `estimated` when every part is priced in full; `partial` when the cost is a lower bound, because a part or one of its classes had no rate beside priced spend, or because the run recorded an unaccounted turn; `unpriced` when the run spent tokens and no part priced any of them, which includes spend reported as a total without classes; and `unmeasured` when the run reported no usage. A measured run that spent nothing is `0`, and `estimated` unless it recorded an unaccounted turn.
- When a run's `run_history` row is written, the orchestrator prices its parts under the rates in force and records the cost, the cost status, and for each part its price source (`operator` for `token_rates` and an operator table, `fetched`, or `embedded`) and the version of the rates that priced it: the pinned upstream revision of the embedded table, or a content digest of the fetched table, the operator table, or `token_rates`; a part priced by a per-model entry that takes some of its rates from a table records the `token_rates` digest, a `+`, and that table's version. History is never priced at read: every surface that shows a finished run's cost reads the recorded figure (Section 19.2). A running session's figure is the orchestrator's live estimate under the rates in force, priced as each usage figure arrives and written to `session_metadata` on the throttled cadence of its token figures, and the row written at the session's end fixes it.
- Rows written before costs were recorded are priced once, by the first start that applies the schema change, under the rates in force at that start, and recorded with their source and version. A measured row with no per-model rows is priced on the run's reported model, else its configured model; a row that already has them is priced from them. They never change after that. A start that fails to price them logs the error and starts anyway; they read as `unpriced` until a later start prices them, under the rates in force at that start.
- `token_rates` and `price_table` reload with the rest of WORKFLOW.md (Section 6.2). New rates apply to the live estimate of running sessions and to every run written after the reload, a recorded cost never changes, and a reload that fails keeps the previous rates in force. On a reload, a `token_rates` that is not a map keeps every entry in force, and an entry that is not a map or holds a rate that is not a finite, non-negative number keeps the rates it had before the reload; an entry the reload adds is read as at startup. Every other entry applies.
- The download of the shared table is on by default and governed by `price_table.fetch` (default `true`), which `SORTIE_PRICE_TABLE_FETCH` overrides like any other environment override; the setting governs only the download from `price_table.fetch_url`. The shared table is fetched at startup and refreshed once every 24 hours, and a failed download is tried again after a backoff that starts at 1 minute and doubles to a cap of 1 hour. An operator table named by `price_table.replace_url` is the operator's explicit choice, is not gated by `price_table.fetch` or its override, and is fetched and refreshed the same way. Neither the first fetch nor a refresh blocks startup, dispatch, or a workflow reload, and a failed fetch never fails them; until a fetched table is in force, the layers beneath it price runs. A fetched table is accepted only when it parses against the expected schema, every rate is a finite non-negative number, and it carries a rate in at least one entry and in at least half as many entries as the last table accepted from the same source, so a truncated, empty, or shrunken response never replaces good data; an entry whose rate fell to zero against that table is discarded and that table's entry kept. For the shared table, that table is the last fetched one that passed validation, else the embedded table; a table from `price_table.replace_url` has none until its first download is accepted, and one read from `price_table.replace_file` is measured against none, so an operator table is never measured against the shared table. The last table that passed validation is stored on disk beside the database and loaded at startup, so a restart without network access keeps it, and a failed fetch keeps it in force and is logged. A newly fetched table affects only the live estimate of running sessions and runs written after it lands. Sortie keeps, per configured download, when its next try is due and, while tries fail, their count with the time and error of the latest, in memory only, so a restart keeps the kept copy and its download time and drops the failures.
- A rate source is available when at least one layer can price: a `token_rates` entry, an operator table, a fetched table, or the embedded table. With the embedded table disabled, no fetched table, and no operator rates, no run is priced: every measured run that spent tokens is `unpriced`, and a measured run that spent nothing is still `0` and `estimated`.
- An issue's money figure is one fold that the pricing component computes for every surface that states it: the poll tick's hold, the retry lane's gate, the in-flight lane, the exit of a stopped run, the `cost_budget` tool, the state API, and the dashboard. It sums the recorded cost of the issue's finished runs that recorded `estimated` or `partial`, plus the running session's live estimate while that is `estimated` or `partial`. The figure is complete exactly when it covers the issue's whole spend: no run is `unpriced` or `partial`, none is unmeasured, and none recorded an unaccounted turn. Otherwise it is a lower bound, and every surface says so: the hold comment and the attention text read "at least", the API serves a completeness flag set false, and the service logs that the figure is incomplete. A run that spent tokens and could not be priced adds nothing to the sum and is never counted as a price of zero without that mark, so the money ceiling cannot hold an issue whose spend stays unpriced or unmeasured. Every comparison with the money ceiling (`agent.max_cost_usd`) rounds both sides to whole micro-dollars, so a sum that equals the ceiling reaches it however its floating-point sum falls.
- The money ceiling is enforced at the points the token ceiling uses: the poll tick's rebuild and the retry lane compare the recorded cost of the issue's finished runs, and the in-flight lane adds the running session's live estimate under the rates in force, with no I/O until that sum reaches the ceiling and one confirming read after. A stop lands at the next usage figure, so a run can pass the ceiling by what the agent spends between two reports: about one usage event for a kind that reports `incremental`, a whole turn for one that reports `turn_end`. An operator leaves headroom. A `token_rates` edit moves only the live estimate and the runs written after it. The `cost_budget` tool reads no rates; it adds the estimate recorded in the running session's `session_metadata` row, so its figure trails the in-flight lane's by up to the throttled write interval.
- An instance spend window's figure folds the way an issue's does, over a window instead of an issue. Its recorded part is the aggregate of the `estimated` and `partial` cost of every run whose completion time lies in the window, so a run counts in the window it completed in, whichever window it started in, and a run is never counted twice, because the exit that removes the running entry also appends the run's row within one event-loop step. The recorded part reads no rates, so a restart, a second process on the same database, a `token_rates` edit, a reload, or a downloaded table leaves it unchanged. Each running session then folds in as it folds into an issue's figure: a session whose agent reports no usage figure makes the figure a lower bound, a session that has reported nothing yet adds nothing and keeps the figure complete, and a reporting session adds its live estimate under the rates in force. The figure is complete exactly when no run in the window is `unpriced`, `partial`, or unmeasured, none recorded an unaccounted turn, and no running session reports no figure. Otherwise every surface says it is a lower bound, and a lower bound never holds dispatch. The `ci_failure` run the CI reconcile records carries no tokens, so it adds no cost to any window. Every comparison with an instance ceiling rounds both sides to whole micro-dollars, as the per-issue comparison does.
- Every cost is Sortie's estimate from the input, output, cache-read, and cache-write rates per model, not the provider's bill. The rates do not express long-context price tiers, which apply per request above a token threshold while Sortie records totals per run, the separate price of one-hour cache writes, batch, flex, and priority service tiers, data-residency surcharges, negotiated discounts, subscription plans under which spend is not tokens times a list price, or fees for tools such as web search. Sortie computes every figure by these rules and never reads a cost figure an agent runtime reports on its own stream.

Timing accounting rules:

- `api_time_ms` is the cumulative LLM API wait time in milliseconds for the session. Accumulated from `api_duration_ms` fields on any agent event that carries timing data.
- `tool_time_ms` is the cumulative tool execution time in milliseconds. Accumulated from `tool_result` events that carry `duration_ms`.
- `tool_time_percent` and `api_time_percent` are computed at render time as `(cumulative_ms / session_elapsed_ms) * 100`. Displayed as null/"N/A" when no timing data has been received.

Runtime accounting:

- Runtime should be reported as a live aggregate at snapshot/render time.
- Sortie maintains a cumulative counter for ended sessions and adds active-session elapsed time derived from `running` entries (for example `started_at`) when producing a snapshot/status view.
- Add run duration seconds to the cumulative ended-session runtime when a session ends (normal exit or cancellation/termination).
- Continuous background ticking of runtime totals is not required.

Usage windows:

- Keep the newest report of each provider usage window per agent kind, normalized by the usage component (Section 10.3).
- A window comes from a live report of the runtime and from a pull the usage component runs when each turn ends (Section 10.3). A pull that fails or finds no window changes no held window.
- A usage window is not spend and enters no token or money figure.

### 13.6 Humanized Agent Event Summaries (Optional)

Humanized summaries of raw agent protocol events are optional.

If implemented:

- Treat them as observability-only output.
- Do not make orchestrator logic depend on humanized strings.

The `text` member of a retained agent event (Section 13.9) is such a summary: one sentence the server words from the event's type and fields each time it serves the event. It is never stored, and a client renders it rather than wording the event itself, except that the session view draws a tool result from its `tool_line` and reads a completed turn with a known number as "Turn {n} completed". The `tool_line` of a `tool_result` is the call's line text, which the server words once and every consumer uses: the command of a shell call that has one; otherwise the tool's name followed by the call's main argument when it has one; otherwise the name alone. When the runtime named no tool, the name is the word of the call's kind ("Command", "Read", "Edit", "Skill") or "Tool" when the call has no kind. The sentence of a `tool_result` names the call by its tool line, as in "Ran git status in 1.2 s.". The `text` of a `notification` and an `other_message` is the message itself, never quoted as the agent's words, because adapters also put their own descriptions in these types.

### 13.7 HTTP Server

Sortie includes an embedded HTTP server for observability and operational control. The server starts unconditionally on port **7678** unless explicitly disabled. It is not required for orchestrator correctness, but its absence silently removes health probes, Prometheus metrics, and the dashboard.

Enablement and configuration:

- The HTTP server starts by default on `127.0.0.1:7678` with no flags required.
- `--port N` overrides the listening port. Port `0` disables the server entirely.
- `--host ADDR` overrides the bind address. `ADDR` must be a parseable IP address. Default: `127.0.0.1`. Container deployments use `0.0.0.0`.

The server refuses a state-changing request that a browser sends from a page on another origin. A method other than GET, HEAD, or OPTIONS answers `403` with the error code `cross_origin_request_rejected` on every path, before routing and with nothing changed, so the refusal comes before every `404` or `405` this document states for a route. A request that carries neither an `Origin` nor a `Sec-Fetch-Site` header (curl, scripts, probes, Prometheus), one with `Sec-Fetch-Site: same-origin` or `none`, and one with no `Sec-Fetch-Site` header whose `Origin` host equals its `Host` header, port included, are served as before. Any other `Sec-Fetch-Site` value is refused whatever `Origin` says, and `Origin` is compared with `Host` only when `Sec-Fetch-Site` is absent. A reverse proxy in front of Sortie must therefore pass on the `Host` header the browser sent unchanged, including its port; a proxy that rewrites it makes Sortie refuse the dashboard actions of a browser that sends no `Sec-Fetch-Site` header.
- `server.port` and `server.host` extension keys provide the same overrides via workflow front matter. CLI flags take precedence over extension keys.
- `server.port` must be an integer in the range 1–65535, or `0` to disable.
- `server.host` must be a parseable IP address string. DNS hostnames are not accepted.
- When the default port (7678) is occupied and the operator did not explicitly request a port (`--port` absent, `server.port` extension absent), Sortie logs a warning and starts without the HTTP server; the orchestrator continues normally. When the operator explicitly requested a port (via `--port` or `server.port`) and it is already in use, Sortie exits with code 1 and a descriptive error. No automatic port selection occurs.
- The `--dry-run` flag suppresses server startup regardless of port or host settings.
- Changes to HTTP listener settings require restart (hot-rebind is not supported).

#### 13.7.1 Dashboard

- `GET /` redirects to `/ui/`, where the dashboard application runs (Section 13.7.4).
- The dashboard is a client-side application that reads only the JSON API below and refetches when the change stream signals a change. Prices is the exception: it reads its route each time it opens and never on a change signal, so a change signal never reruns the history read behind it. Every run-history read, which is History, the older runs of an issue page or a session view, and the issue lookup of the command palette, refetches at most once per 5 seconds, and Analytics at most once per 30 seconds, because a run-history refetch rereads every loaded page through the orchestrator's store connection and an Analytics refetch rescans every run in its range. A fetch in flight is never canceled: a signal that arrives during it refetches once, its floor after the fetch ends. A hidden tab does not refetch either screen on a signal.
- The brand block names the instance on every screen at every width: while the state route serves `config.tracker.project`, the project sits beneath the product name, on one line, cut with an ellipsis when it is too long and whole in its hover text, and the browser tab title carries it between the condition word and the product name, as in `(2) Needs attention - sortie-ai/sortie - Sortie`. While the member is absent, which includes the time before the first response, the brand block shows no second line and no placeholder, and the title has no project segment. The first-run text and the poll line of Health do not repeat the project, because the brand block already names it.
- Overview is the first screen. It opens with a headline that says whether anything needs a person, then lists what needs a person (Needs attention), every active issue in the order the state route serves (Active), the eligible issues Sortie holds back from dispatch (Not dispatched), and every issue whose run ended today, newest ending first (Ended today). An item of Needs attention or Ended today that the reader has not opened shows its title in bold, never a separate dot, so a row carries one mark. Active, Ended today, the Pipeline, History, and the command palette name where an issue stands with the same one word and mark it with the same shape its lane draws there, centered in its cell: an active issue by its state (Running, Waiting, Retry, Attention, Held), and an ending by the tracker state it reached with a capital first letter, Merged, or Ended, with a dashed circle while a handed-off issue waits on a person and the ended mark once it is merged or closed. The full status sentence stays on the peek and the issue page. Overview then shows a Usage section between Needs attention and Active, and only while it has something to show: a budget, a current plan window, or both. The budget shows while any of `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, and `agent.max_monthly_cost_usd` is set: one meter per configured window, shortest window first, that shows the window's spend against its ceiling, reads "at least" while the spend is a lower bound, and carries the window's reset time in UTC, with a note that dispatch is held while the window holds. It says the figures are estimated at API prices only while an agent kind shows plan windows below it, because then the agent is on a subscription and its dollars are not a bill. The windows start at midnight UTC, while the dashboard's "today" starts at midnight in the browser's time zone, so for a viewer off UTC its today total and the daily window cover different spans. After the budget comes a block for each agent kind the Workflow screen lists, in that order, that has at least one current window; a kind that reported none, as an agent billed per request does, has no block, and nothing shows until the state route serves `config`. A block names the kind and, only when its last report is an hour old or older, the UTC time stamp of that report. Under it, each current window shows its label and, when it carries an amount, a meter of the amount used against its limit, with the text `{used} of {limit} {unit}` in the window's own unit (digits grouped, at most two fraction digits, no trailing zeros, as in `7.08 of 1,000 credits`), which replaces the share meter of that window; else, when the report carried a used share, a meter with the share in percent (a share above 100 reads as served); without either it shows no meter and words the window's status as Under the limit, Near the limit, Usage limit reached, or Status not reported. An exhausted window reads Usage limit reached in the row without a bar and in the reset line. A line under the window gives its reset time in UTC, or says the reset time is not reported; it opens with the status words for a window whose meter is at warning or exhausted, and closes with the window's own report time when that differs from the kind's and is an hour old or older. The dashboard hides a window once its reset time passes on its own clock, and a kind whose windows have all reset loses its block. The section carries no hover, link, or separator character and no note about dispatch; Not dispatched carries a usage-limit hold. Not dispatched renders only while `held` is non-empty, headed with the count `counts.held` serves, and lists the rows in the order served: each item shows the issue's title and key, the hold's `reason_text` and `release_text`, how long ago the issue was first seen held, and a link to the issue in the tracker. It shows no state mark, peek, or Acknowledge, and the dashboard words only the heading, the count, and the link label, so a `hold.reason` it does not know needs no dashboard change. An Active row of an issue between stages that carries a hold shows the hold's `reason_text` as a further line. Held issues are not active: the Active count and tally, the headline, `counts.attention`, the rail and status badges, and the dashboard condition read nothing from `held`, and Ended today leaves out an issue the `held` list holds.
- Pipeline draws the issues Active lists, in its order, and the issues whose work ended today, as one lane each across one row of stations, so an operator sees where along the configured path every issue stands. Lanes keep a stable order: active lanes keep the order of Active, and ended lanes follow them, oldest ending first. The stations and their order are those of the issue page lane and are the same for every issue. A header above the lanes draws every station as the same gray circle and writes under its name the number of lanes standing at it, in the attention color while one of them needs a person. A lane carries one marker for the state of its issue, the time the issue has stood at its station, one word in a column of fixed width, left aligned, that names where the issue is (Running, Waiting, Retry, Attention, Held, Stopped, or Failed while it is active, and the tracker state it reached with a capital first letter, Merged, or Ended once its work ended; the full status sentence stays on the peek and the issue page), the pull request number under the handoff station once the issue has reached it, and a mark on each reaction station. A station whose kind the issue has a pending reaction of keeps the mark of a waiting reaction, or of an escalated one once its escalation stands, except that a pending reaction whose `last_check.handled` is `true` and that has no standing escalation does not hold its station, which then reads the pass like every other reaction station. An issue whose deciding reaction is such a row stands at the handoff station, or at the agent station when the workflow has none. The Review station of a pending review reaction whose `last_check.awaiting_approval` is `true` and that has no standing escalation keeps the dashed circle of a waiting reaction and reads waiting on approval in its title, and the issue stands at Review while that row decides. Every other reaction station reads what the kind recorded in the issue's pass (`pr_loop`, Section 13.7.2), so an ended issue reads its last pass: a filled dot when Sortie watched the kind in the pass, whether it acted, had nothing to act on, or began an escalation, with the number of fixes or runs scheduled beneath it when Sortie acted, and an empty ring when Sortie has no record. A station whose escalation stands takes the needs-attention fill. The track runs from the tracker to the furthest of the station the issue stands at and the reaction stations Sortie watched in the pass, since Sortie keeps working the issue after its handoff. Station marks are drawn shapes, never glyphs, and the lane names the handoff and end stations after the configured tracker states with the first letter capitalized. In a workflow that lists two or more terminal states, the lane of an issue that ended names the end station after the tracker state the issue reached, with the first letter capitalized, or Merged, while the header keeps End. Go decides each reading and the client draws the mark and the fixed words of the served value. The `ci_failure` station is named CI fix. An issue on a stage chain shows its place as a meter under the agent station, and an ended issue draws the meter from the `stage_chain` of its last stage run. The screen opens with the attention line, which names the issues that need a person and links to Overview; while nothing needs a person, the line shows nothing and keeps its place, so the lanes do not shift. Lifecycle chips, which include Ended today, and the Rule menu narrow the lanes as on Overview, and every count reads the lanes that pass the filters. Above 24 lanes the view compresses: each station with lanes becomes one collapsed line that gives its lane count and how many need a person, and opens to its lanes in dense form. Once a lifecycle chip or a rule is chosen, the view shows the matching lanes in dense form instead of the station lines. Wherever an issue or a reaction waits on someone, on a signal, a reaction, or a person reviewing a handed-off issue, the lane draws a dashed circle the size of a station dot, and a station Sortie has moved past is a filled dot; an issue the tracker closed stands at the end station with the ended mark. A legend names the markers and marks the workflow can draw: one waiting entry covers every dashed circle, and with a reaction station it adds no record. The screen takes the full width of the working area and never scrolls sideways, so station names shrink to a short form and then to an ellipsis. List and Pipeline are two views of the one Active list: a switch on the Active head moves between them, and `L` does the same from either screen, carrying the lifecycle chip and the rule. The peek, the selection keys, and the holding behavior of Overview apply on the Pipeline, and the Pipeline subscribes to no run on the change stream.
- History lists finished runs grouped by issue, newest first, one group per issue however often it retried, a page of issues at a time. A group shows the issue's title and key, how the issue ended or what it is doing now in the sentences the server words, its run count, its tokens, its cache reads, and its estimated cost, and lists its newest runs with a way to read the rest. A run row shows the status, the dispatch rule and the hop it reached, the agent and model, the skill calls, the duration, the turns, the tokens, the estimated cost, and the finish time. The skill calls are the run row's `skills`, every call in the order served with repeats kept, on one line cut at the cell's edge with the whole list on hover; a failed call is marked as failed, and a run that called no skill shows an empty cell. History reads them from the run row and requests no events for them. A run recorded as succeeded whose agent declared `blocked`, `needs-human-review`, or `no-change-needed` shows that declared outcome in place of the status, and the agent's statement shows on hover over it. Tokens are the uncached figure of Section 13.5 and the column header says cache reads are not included; a run row gives its cache reads on hover over its tokens. A cache-read count of zero shows nowhere, because it also means an agent that reports no cache data. Filters narrow the groups by issue key, dispatch rule, run status, and the rule a stage hop reached, and a key matches the form the dashboard shows, the qualified form of a GitHub, GitLab, or Gitea issue included. A workflow without dispatch rules or stage chains shows no rule filter, no hop filter, and no rule column.
- Analytics shows the run statistics report for a chosen range: the last 24 hours, the last 7, 30, or 90 days, or all time, which is every run on record. The range defaults to 7 days, is chosen from a picker in the header (in the page heading on a narrow screen), and is kept in the address. It opens with the success rate, then the run and issue counts, the duration of succeeded runs, the turns per succeeded run, the tokens, the cache reads, the estimated cost, and the cost per succeeded run, and breaks the runs down by dispatch rule, model (a run counts under every model it spent tokens on, with that model's tokens and recorded cost, so the rows' runs can add up to more than the runs; a run with no per-model spend counts under the model the agent reported running, else the configured one), agent, run status, retry trigger, and what started each run. A bar shows how the runs ended (succeeded, stopped, cancelled, failed) from the summary's counts, with a legend of counts, and its segments carry no hover; an outcome without runs draws nothing. While a rate source is available, a Spend chart follows the bar for every range but the last 24 hours. It draws the report's `spend`: one slot per UTC day for the 7, 30, and 90 day ranges and one per UTC month for all time, in the order served, of equal width across the chart with no sideways scroll, and the partial day that holds the range's start counts as a day slot. A bar's height is the bucket's `cost_usd` against a scale top of the smallest 1, 2, or 5 times a power of ten that holds the largest bucket cost. A bar takes the estimated fill while its bucket's cost is complete and the lower-bound fill while the bucket counts a run that is unpriced, has a partial cost, or reported no usage, and the lower-bound fill is a bar with a diagonal hatch, which the slot's hover explains. A bucket with runs and no cost, whose runs are unpriced or reported no usage, draws a thin bar in the lower-bound fill so it never reads as a period without spend; a bucket without runs, and a bucket whose runs spent nothing, draws no bar. Each slot opens a hover with its period, the time the range starts from on a first bucket the range's start cuts, and one sentence saying what the bar means, with no amount and no run count. Every date and time on the chart is written in UTC, whatever the viewer's time zone. Each breakdown that shows a cost column shows the cost per succeeded run in a column beside it. A breakdown or funnel table draws no bar, mark, or chart in its cells: each shows its runs, its success rate (its share in the run status breakdown), and its duration as the p50 to p95 range as text. After the summary it shows one funnel table per stage chain definition, one row per stage; a workflow without stage chains shows none. A breakdown the workflow cannot have is hidden, and so is the rule breakdown while its only group is the dispatch default or the runs that recorded no rule name. The last block, By skill, lists each skill the runs called with the runs that called it (a run counts once however often it called the skill), its calls, and its failed calls, in the order served until the reader sorts, and it is hidden while `skill_calls` is `null` or its `by_skill` is empty. While `skill_calls.unrecorded_runs` is above 0, every count of the table is a lower bound, drawn with a dotted underline whose hover says the count leaves out runs recorded before Sortie kept skill calls. Every figure but an outcome's share of the runs is a field of the report passed through a formatter and matches `sortie stats --format json` for the same range; the screen sums nothing and divides only to size the outcome bar and the Spend chart and to give that share. The tokens are `tokens.uncached` and the cache reads are `tokens.cache_read`, as served; the cache reads stay empty while the tokens read "not measured" or the count is zero. A breakdown row's tokens column and its sort use the uncached figure, and the header saying cache reads are not included. A group's tokens read "not measured" when none of its runs reported usage, and its cost reads "unpriced" when none of its runs was priced. Otherwise both state the sum of what was recorded, and the count of runs that reported no usage or that were recorded unpriced stands beside the summary's figures. The cost tile and the cost column show the amount cut down, never rounded up, with a dotted underline while the report leaves runs out of it, which it does for a run that is unpriced, has a partial cost, or reported no usage, and the hover on the figure opens with "At least this much" and says in plain words that it leaves those runs out. An Analytics hover explains in plain language what a figure means and never repeats an amount, a count, or a share, and a figure that is complete carries none. The cost per succeeded run is the served `cost_per_succeeded_run_usd` worded the same way, and a tile or cell appears only while a run succeeded.
- Health lists Sortie's own checks in a fixed order, each with one line on its state: a passing check says so, and a failing one gives the runs in a row that failed, when the first of those failed, its last success, and its error. Beside the checks, the poll loop gives the interval, the last poll, the last good poll, the next poll, and the polls in a row that failed, and the process gives the version, the start time, the agents running, and when the configuration in effect loaded. A check that has not run reads as not run, never as passing, and a mark follows the served status and whether the check has run, never the outcome or the error text. An edit of the workflow file that failed to load reads as failing: poll ticks dispatch nothing until the file loads, and the configuration from the last successful load stays in effect. The tracker poll shows the sentence the server words (Section 13.7.2) and the last tracker error.
- Workflow shows the configuration in effect: the members the state route's `config` serves and nothing else. Its standfirst says when the configuration in effect loaded and, while the latest load failed, that the newer edit did not load and poll ticks dispatch nothing until it does. Rules outside any stage chain form one table, and each chain reads as numbered steps from its entry rule to the handoff state, with tracker labels shown as chips; reactions form one table of kind, limit and escalation. A workflow without dispatch rules, stage chains, reactions, SSH hosts, or token rates shows the empty form of that part, and an unset limit reads as unset, never as zero. Its Agent panel shows the money budget after the token budget, as an amount per issue, and as unset while `config.agent.max_cost_usd` is `null`.
- Prices shows the rates in force when it is read, per agent kind and model, the layer that supplies each rate, and the state of every price source. It computes no cost and changes nothing, and it MUST take every layer, rate, and source state from the pricing component through `GET /api/v1/prices` (Section 13.7.2), never derive one. It opens from the rail beside Health and Workflow, from the command palette, and with the `G` then `R` chord. Each opening requests the route again and shows the rates in force at that moment, which a later reload or download can change before the next run; a return to the window refetches it as it refetches every dashboard query. It shows, in order, a Prices in force panel, one panel per rate layer in precedence order (Token rates, Custom prices while an operator table is configured, Downloaded prices, Built-in prices), and the Models table. The Prices in force panel names, as its Source, the highest-precedence table layer in force, or reads "None" while none is, with the date its prices are as of and their age in whole days. The screen states the age and gates nothing: it has no age threshold and no warning. A layer panel gives the status of its source, whether the table is in force, when it was loaded or last downloaded, when the next download is due, its version, and the number of models it lists; the Built-in prices panel gives its version and that version's date instead. Every panel labels the version a run records as Version: the content digest of `token_rates` and of a loaded or downloaded table, the upstream revision of the embedded table. A download turned off reads "Off by" and the setting or environment variable that turned it off, and a disabled embedded table reads "Off", so a source that is off never reads as failing. A table that has not loaded reads "Not loaded yet", and the layers beneath it are in force. A failing source adds the failures in a row while they are above 0, the time of the latest failure, and its error. A version shows its first 12 characters, after `sha256:` for a digest, whole on hover. The Models table lists one row per agent kind and model, in the order the route serves until the reader sorts a column; every column sorts, as the Analytics breakdowns do, and the choice is kept in the address. A search field above the table narrows the rows, as it is typed, to the models whose name contains the text, ignoring case; `Esc` or the clear control empties it, and while no row matches the table reads "No model matches this search." A row gives the model ("Model not reported" for an empty name), the agent, the layer that prices it, the input, output, cache-read, and cache-write rates as amounts under column heads that carry the per-1M unit, the last use, and the number of runs. Columns take the width of their content and no cell is cut short; a table wider than the screen scrolls sideways. A rate that is not set reads "not set", never as zero, and a rate of zero reads as an amount; a cache rate a kind entry leaves unset reads as priced at its input rate, as the Workflow screen reads it. While the classes of a row come from more than one layer, a rate's hover names the layer of its class. A value the route serves as `null` leaves its place empty.
- The issue page at `/ui/issues/{identifier}` and the peek, the drawer that opens over Overview and Pipeline, show one issue's whole story. The breadcrumb names the key, and the page opens with the tracker state as a chip colored by where the state sits in `tracker` (done for a terminal state, in progress for the in-progress state, the handoff state, and an active state after the first, otherwise idle), and the issue type over the title, then the status sentence, the links to the issue and its pull request, and the lane. Below them the story sits beside a margin. The margin holds the attention items that concern the issue, each in the attention item form Overview uses with its action and Acknowledge until a person acknowledges it and the time of the acknowledgement after; a scheduled retry's due time, trigger, and error with Retry now; what happens next; one panel per pull request with its repository, branch, and head and one line per pending reaction, naming the reaction and where its watch stands with the fixes used beneath, with Check now; the budget and cost; the route; and the issue's tracker state, type, and labels. The lane draws the stations the workflow in effect defines: the tracker, the agent, the handoff state, one per configured reaction, and, once the workflow configures a reaction, the end. The end station carries the name of the tracker's terminal state when the workflow lists exactly one terminal state and every merge Sortie records moves the issue into it, which holds without `auto_merge` or with `merge_completion`; otherwise it reads End, because an issue that reaches a terminal state without an observed merge also ends there. In a workflow that lists two or more terminal states, the page and peek lane of an issue that ended names the end station after the tracker state the issue reached, with the first letter capitalized, or Merged. It marks where the issue stands, so every issue of one workflow gets the same stations and order whatever its stage chain, and a stage never becomes a station. Its reaction stations read as the Pipeline's do, from the `pr_loop` of the per-issue response, so the page and the peek show the history the Pipeline shows. An issue on a stage chain shows its place in the chain as a meter under the agent station, captioned with the stage and its place while the issue stands at the agent and with the count of stages done elsewhere. Above the story, a one-line summary counts the stages done, names the current stage or the hop that was not made, and counts the stages ahead, with a control that lists every stage and its time span; for a finished issue the chain comes from `history.last_stage` (Section 13.7.2). The story is one chapter per stage, or per stretch of work on the pull request, each a timeline of run cards: the run's number, status, what started it, its time span and duration, its error, and its turns, tokens, and estimated cost. A run recorded as succeeded whose agent declared `blocked`, `needs-human-review`, or `no-change-needed` shows that declared outcome in place of the status, and the card shows the agent's statement under the error. Every chapter but the current one folds to one line, and the current one ends on the issue's status sentence. A run's number is its position among the issue's runs ordered by start time, so the oldest is Run 1, and the run in progress reads Live run; the number is independent of `attempt`, which counts retries and which reaction runs copy or advance. A workflow without stages tells its runs as one chapter. Older runs load on request, a page at a time. The budget shows sessions and tokens used against their limits as meters, and the estimated cost split into stage runs and pull request fixes. While a money ceiling is set and the issue's cost is known, a cost meter after the tokens shows the issue's estimated cost against the ceiling, marked "at least" while the figure is a lower bound, and takes the place of the standalone estimated cost line; otherwise the cost reads as before, and an unknown cost never reads as zero. A running issue takes its figures from its running row, and any other issue takes its usage from `history.totals` and its limits from `agent` in `config`. The run cards and the peek show the uncached figure: a running issue's from `used_tokens` of its running row, any other issue's from `uncached_tokens` of `history.totals`. The peek shows the same head and lane over the issue's facts, what happens next, and its last four runs, and its Spent fact states the cost against the money ceiling with the same amounts and the same lower-bound mark as the page's cost meter. A run in the peek reads the same outcome word as on the issue page, without the statement. Retry now and Check now each state their effect before they run and report the answer, and neither asks for confirmation. The page opens from Overview, from Pipeline, from History, and from the command palette, which lists the issues matching its query, active and finished alike, by any part of the key or the title, and lists the sessions of those issues in a group of its own, where a finished session reads the same outcome word as on the issue page, without the statement. Each run card links to the run's session view, and the action row of the page and the peek holds a button that opens the issue's live session, else its latest one; `O` does the same on the page, with a peek open, and on the selected issue of Overview and Pipeline. The breadcrumb and `Esc` return to the screen the page was opened from, and to Overview after a deep link. The page and the peek follow every change signal while open, and the times on them tick each second.
- The session view at `/ui/issues/{identifier}/sessions/{dispatch_id}` shows one agent run. The address names the run by its dispatch ID, never by `attempt`, so it names exactly one run; the view's number is the run number the issue page gives the run, and a live session keeps its number when it finishes. The header gives the issue key and title, the agent and model (the one the agent reported running, else the configured one), the stage and its place on a chain or else the dispatch rule, what started a continuation, the SSH host, the time span and elapsed time, and the status with the turn count. Once the issue has more than one session, a sessions panel beside the events lists every session of the issue, newest first, the running one included: each row gives the run number the issue page gives the session, linked to its session view except for the one on screen, then its status and its duration, which ticks while the session runs; older runs load on request. The events panel lists the events Sortie kept for the run and then each new event as it arrives, each as the sentence the server words, with its time, except that a tool result draws as a tool line, the same for every adapter: an icon for the call's kind (a terminal for a command, a file for a read, a file with a pen for an edit, an open book for a skill, a wrench for any other tool), the served `tool_line`, then the call's duration, with no glyph before or between the parts. The line is cut at its container's width with an ellipsis and shows whole in a hover tooltip, the icon and the duration never cut. A failed call draws the same line inside the failed mark, in the event line and in the expanded group. The session view masks nothing and words no tool line itself. Consecutive tool results that are not skill calls fold into one line when there are two or more: its summary has one phrase per kind in the group, in the order the kinds first appear, joined by commas, each counting the group's calls of that kind ("3 commands, 2 file reads"); the phrases are command, file read, file edit, and tool call for a call with no kind. It sums their durations, marks failed calls as "{n} failed", and when expanded lists each call as its tool line. A skill call never folds: it closes the pending group and stands as its own line, so every skill name shows without opening a fold. A lone tool result stays a plain line, and agent messages and requests for input never fold. A turn-ending event carries its turn number, the turn's own tokens, and the turn's duration, and a completed turn whose number is known reads "Turn {n} completed" in place of the served sentence, with the message the agent ended the turn on beneath it: the number is counted from the run's first event and the tokens are the difference between the uncached figures of consecutive turn-ending events, each the event's total less its cache reads (a retained event serves no uncached member), so each shows only when no event it counts is missing. A gap line counts the events Sortie no longer keeps, and a run with none kept says so. A live session opens at the bottom and follows new events while the reader stays there; scrolling up stops following and offers a control that jumps to the latest event, and pausing the dashboard freezes the timeline while events keep merging. A finished session opens at the top, without the live parts, and the event that closed its last turn carries the run's outcome word and mark. A session recorded as succeeded whose agent declared `blocked`, `needs-human-review`, or `no-change-needed` reads that declared outcome in place of the status in the header and in the sessions panel. A live session whose quiet instant has passed shows `stall_text_full` as a notice above the panels, and with stall detection off shows `stall_off_text` in the same place without the mark. A finished session that recorded an error, or whose agent left a statement, shows a notice in that place. With an error, the notice gives the worded error, as an alert when the run failed, then the agent's statement, and, on the issue's newest session while nothing of the issue runs, what happens to the issue next. Without an error, the notice gives the statement alone. Tokens per turn draw as a bar chart once the session has two completed turns with a known figure, one bar for each such turn and one for the turn in progress, under a legend of the fills and with the axes named; before that, the view shows no chart. Beside the events, a usage panel gives the tokens, which are the uncached figure and equal its fresh input, output, and cache write rows summed, its cache reads, and the estimated cost, which reads "unpriced" when the session's cost status is `unpriced` and "at least" before the amount when it is `partial`, with the exact amount on hover. A finished session whose run serves `model_usage` (Section 13.7.2) shows a Cost per model panel directly after the usage panel: one group per part in the order served, headed by the model name ("Model not reported" for an empty name), giving the part's tokens, its estimated cost worded and hovered as the usage panel words it, and, only when the part names classes without a rate, a "No rate for" line that lists them as input, output, cache read, and cache write. A live session shows no such panel, and a finished run whose `model_usage` is `[]` shows none. The panel appears when the session finishes and the run-history read that brings its run returns. A session whose row serves `skills` (Section 13.7.2), the running row while the session is live and the run's row once it has ended, shows a Skills panel directly after the Cost per model panel, or after the usage panel when there is none: one line per entry, in order and none left out, each the entry's position, right-aligned in a column as wide as the widest position, then the skill name cut and shown whole on hover as a tool line is, inside the failed mark for a failed call. A live panel updates with its running row, and a finished session's panel appears when the run-history read that brings its run returns, as the Cost per model panel does. A session whose row serves no `skills` shows no panel. A session panel gives the models, the workspace's last path segment, the head of the session ID, the workflow, and the stall timeout, each long value whole on hover. The view opens from a run card, from the action row of the issue page and the peek, with `O`, from the command palette, and from the previous and next links; `Esc` returns to the issue page, and the breadcrumb returns to the screen the reader came from.
- Nothing in the dashboard assumes a stage, a dispatch rule, or a reaction. A workflow without them renders without those parts.
- A figure Sortie does not know reads as unknown, never as zero. Every cost is labeled an estimate, and its wording follows its cost status. `estimated` reads as the amount, `$0.00` for zero and "under $0.01" for an amount above zero and below one cent, so a cheap run never reads as a free one. `partial` reads "at least" before the amount, which the dashboard cuts and never rounds up, to two fraction digits below $100, to whole dollars from $100, and after its first non-zero digit below one cent ("at least $0.004"), so a lower bound never reads above the spend it bounds. `unpriced` reads "unpriced" and `unmeasured` reads "not measured". The marker is the words "at least" in the text of the figure, never a glyph, except on Analytics, where the figure is the amount cut the same way with a dotted underline and its hover opens with "At least this much" ("At least this many" on a skill count). Every cost with an amount shows its exact amount on hover, and a lower bound adds that the figure leaves out spend Sortie could not price or measure, on every screen but Analytics, whose hovers carry no amount. A figure of the run statistics report is a lower bound when it has a cost and any of its runs is unpriced, has a partial cost, or reported no usage, and Analytics notes that it leaves those runs out. A skill count of Analytics is a lower bound while `skill_calls.unrecorded_runs` is above 0, and its hover opens with "At least this many".
- Dashboard tokens are the uncached figure of Section 13.5, so no figure counts cache reads as tokens, and a run or issue that shows cache reads names them apart from its tokens.
- The dashboard holds still while a detail panel is open, and an operator can pause it. Read state stays in the browser.
- A run in an issue's detail shows its rule and, when its exit reached a hop decision, the target rule and the result.
- A run's agent events are live in the view that shows the run: the dashboard subscribes to that run on the change stream it already holds and opens no second stream (Section 13.7.2). The browser's one stream subscribes the runs that visible tabs show, each once and at most 16, drops a run while no visible tab shows it, and reopens only when that set changes. A tab merges what it receives, a replay included, by `seq`, so an event never shows twice. A gap the server reports, earlier events evicted or a run not retained, reads as a gap, never as continuity.
- A running run shows a neutral quiet marker, the row's `stall_text`, on the issue's Active row, on its Pipeline lane, on the issue page, and in the session view. The browser refetches the state when its clock, corrected by the offset between it and the state response's `generated_at`, reaches the row's `quiet_at`, because silence produces no signal and the server words the marker. While `stall_text` is `null` there is no marker, only the time of the last event. The text is as of the state response's `generated_at`; every poll tick ends with a change signal, and the refetch it triggers updates the silence. With stall detection off, list rows show no marker and the session view shows `stall_off_text`.

#### 13.7.2 JSON REST API (`/api/v1/*`)

Provide a JSON REST API under `/api/v1/*` for current runtime state and operational debugging.

Minimum endpoints:

- `GET /api/v1/state`
  - Returns a summary view of the current system state (running sessions, retry queue/delays, pending pull request reactions, issues between stages, the eligible candidates the latest dispatch pass held back, one entry per active issue in a stable order, what needs a person and which of it a person has acknowledged, aggregate token/runtime totals, the usage windows each agent kind reported, the effective workflow shape, the health of Sortie's own checks, the Sortie version, the process start time, and any additional tracked summary fields).
  - Suggested response shape:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "version": "0.4.0",
      "started_at": "2026-02-24T19:11:28Z",
      "counts": {
        "running": 2,
        "retrying": 1,
        "budget_exhausted": 1,
        "parked": 1,
        "pending_reactions": 2,
        "between_stages": 1,
        "held": 1,
        "attention": 3
      },
      "running": [
        {
          "issue_id": "abc123",
          "issue_identifier": "MT-649",
          "issue_title": "Fix the flaky login test",
          "issue_url": "https://tracker.example.com/browse/MT-649",
          "issue_labels": ["bug", "backend"],
          "issue_type": "Bug",
          "state": "In Progress",
          "session_id": "thread-1-turn-1",
          "turn_count": 7,
          "self_review_active": false,
          "self_review_iteration": 0,
          "last_event": "turn_completed",
          "last_message": "",
          "started_at": "2026-02-24T20:10:12Z",
          "dispatched_at": "2026-02-24T19:48:20Z",
          "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
          "last_event_at": "2026-02-24T20:14:59Z",
          "quiet_at": "2026-02-24T20:17:29Z",
          "stall_at": "2026-02-24T20:19:59Z",
          "stall_text": null,
          "stall_text_full": null,
          "stall_off_text": null,
          "tokens": {
            "input_tokens": 1200,
            "output_tokens": 800,
            "total_tokens": 2000,
            "cache_read_tokens": 400,
            "cache_write_tokens": 100
          },
          "uncached_tokens": 1600,
          "model_name": "claude-sonnet-4-20250514",
          "rule_name": "implement",
          "stage_lineage": {
            "chain_id": "3c8v1n6w0z4y7t2s5r9q8p1mkd",
            "previous": "specify",
            "previous_outcome": "succeeded"
          },
          "stage_position": {
            "chain": ["specify", "implement"],
            "current": "implement",
            "hop_count": 1,
            "hop_ceiling": 10
          },
          "configured_model": "claude-sonnet-4-20250514",
          "configured_effort": "high",
          "workflow_file": "WORKFLOW.md",
          "agent_kind": "claude-code",
          "reaction_kind": "ci",
          "reaction_config_key": "ci_failure",
          "reaction_attempt": 1,
          "pull_request": {"number": 936, "repository": "acme/web"},
          "api_request_count": 3,
          "requests_by_model": {"claude-sonnet-4-20250514": 3},
          "skills": [{"skill": "ce-plan"}, {"skill": "ce-work", "tool_error": true}],
          "tool_time_percent": 12.3,
          "api_time_percent": 45.6,
          "tokens_measured": true,
          "usage_arrival": "incremental",
          "usage_attribution": "per_model",
          "tokens_pending": false,
          "tokens_awaited": false,
          "api_requests_measured": true,
          "used_sessions": 2,
          "budget_sessions": 5,
          "issue_tokens_completed": 53400,
          "unmeasured_sessions": 0,
          "used_tokens": 55000,
          "used_tokens_complete": true,
          "budget_tokens": 500000,
          "budget_usd": null,
          "estimated_cost_usd": 0.014595,
          "cost_status": "estimated",
          "issue_estimated_cost_usd": 0.235095,
          "issue_cost_status": "estimated",
          "status_text": "Fixing CI on #936, fix 1 of 2",
          "status_text_full": "Fixing CI on PR #936, fix 1 of 2."
        }
      ],
      "retrying": [
        {
          "issue_id": "def456",
          "issue_identifier": "MT-650",
          "issue_title": "Retry the export job",
          "issue_url": "https://tracker.example.com/browse/MT-650",
          "issue_type": "Task",
          "issue_labels": ["export"],
          "state": "In Progress",
          "dispatched_at": "2026-02-24T20:02:41Z",
          "attempt": 3,
          "due_at": "2026-02-24T20:16:00Z",
          "error": "no available orchestrator slots",
          "trigger": "timer",
          "rule_name": "default",
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "stage_lineage": {
            "chain_id": "7q2m4x9b1k5d8f3h6j0n2p4rst",
            "previous": "review",
            "previous_outcome": "succeeded"
          },
          "stage_position": null,
          "paused": false,
          "status_text": "Retry 3 in 30s: no agent slot was free",
          "status_text_full": "The retry came due and was not dispatched: no agent slot was free. Retry 3 in 30s."
        }
      ],
      "budget_exhausted": [
        {
          "issue_id": "ghi789",
          "issue_identifier": "MT-651",
          "issue_title": "Migrate the billing tables",
          "issue_url": "https://tracker.example.com/browse/MT-651",
          "issue_type": "Task",
          "issue_labels": [],
          "state": "In Progress",
          "dispatched_at": "2026-02-24T19:31:07Z",
          "reason": "session_budget",
          "used_sessions": 3,
          "budget_sessions": 3,
          "used_tokens": null,
          "budget_tokens": null,
          "used_usd": null,
          "used_usd_complete": null,
          "budget_usd": null,
          "unmeasured_sessions": null,
          "stopped_in_flight": null,
          "exhausted_at": "2026-02-24T20:12:00Z",
          "rule_name": "default",
          "stage_lineage": null,
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "status_text": "Held: session budget reached",
          "status_text_full": "Held at the session budget of 3. Sessions used: 3."
        }
      ],
      "parked": [
        {
          "issue_id": "jkl012",
          "issue_identifier": "MT-652",
          "issue_title": "Rotate the signing keys",
          "issue_url": "https://tracker.example.com/browse/MT-652",
          "issue_type": "Task",
          "issue_labels": ["security"],
          "state": "In Progress",
          "dispatched_at": "2026-02-24T19:40:12Z",
          "reason": "agent_blocked",
          "parked_state": "In Progress",
          "label": "needs-human",
          "label_applied": true,
          "parked_at": "2026-02-24T20:05:00Z",
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "status_text": "Parked: the agent needs a person",
          "status_text_full": "Parked: the agent stopped because it cannot go on without a person. Parking label: `needs-human`."
        }
      ],
      "pending_reactions": [
        {
          "issue_id": "mno345",
          "issue_identifier": "MT-653",
          "issue_title": "Add the export button",
          "issue_url": "https://tracker.example.com/browse/MT-653",
          "issue_type": "Story",
          "issue_labels": ["export"],
          "state": "In Review",
          "kind": "ci",
          "config_key": "ci_failure",
          "pull_request": {
            "number": 931,
            "repository": "acme/web",
            "url": "https://github.com/acme/web/pull/931",
            "branch": "sortie/MT-653",
            "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
          },
          "created_at": "2026-02-24T20:09:30Z",
          "dispatched_at": "2026-02-24T19:58:03Z",
          "next_poll_at": "2026-02-24T20:17:40Z",
          "wait": "poll_interval",
          "used_attempts": 2,
          "max_attempts": 2,
          "escalated_at": "2026-02-24T20:12:40Z",
          "escalation_trigger": "budget_exhausted",
          "watch_ends_at": null,
          "last_check": {
            "observed_at": "2026-02-24T20:12:40Z",
            "head_sha": "9b7e3d1c0a2f4e6b8d0c1a3e5f7b9d2c4e6a8f0b",
            "status": "failing",
            "failing_checks": ["Test"],
            "failing": 1,
            "pending": 0,
            "passing": 4
          },
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "status_text": "CI escalated on #931",
          "status_text_full": "Sortie escalated failing CI on PR #931; it keeps watching the pull request. CI fixes used: 2 of 2."
        },
        {
          "issue_id": "mno345",
          "issue_identifier": "MT-653",
          "issue_title": "Add the export button",
          "issue_url": "https://tracker.example.com/browse/MT-653",
          "issue_type": "Story",
          "issue_labels": ["export"],
          "state": "In Review",
          "kind": "review",
          "config_key": "review_comments",
          "pull_request": {
            "number": 931,
            "repository": "acme/web",
            "url": "https://github.com/acme/web/pull/931",
            "branch": "sortie/MT-653",
            "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
          },
          "created_at": "2026-02-24T20:09:30Z",
          "dispatched_at": "2026-02-24T19:58:03Z",
          "next_poll_at": "2026-02-24T20:16:05Z",
          "wait": "debounce",
          "used_attempts": 1,
          "max_attempts": 3,
          "escalated_at": null,
          "watch_ends_at": "2026-02-24T20:39:30Z",
          "last_check": {
            "observed_at": "2026-02-24T20:15:10Z",
            "actionable_comments": 3,
            "new_comments": 1
          },
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "status_text": "1 new review comment on #931; next check in 35s",
          "status_text_full": "Waiting for 1 new review comment on PR #931 (3 actionable) to settle; next check in 35s. Review fix runs used: 1 of 3."
        }
      ],
      "between_stages": [
        {
          "issue_id": "pqr678",
          "issue_identifier": "MT-654",
          "issue_title": "Plan the export schema",
          "issue_url": "https://tracker.example.com/browse/MT-654",
          "issue_type": "Task",
          "issue_labels": ["export", "stage-implement"],
          "state": "In Progress",
          "source_rule": "specify",
          "target_rule": "implement",
          "target_label": "stage-implement",
          "hopped_at": "2026-02-24T20:15:02Z",
          "dispatched_at": "2026-02-24T20:01:17Z",
          "rule_name": "implement",
          "stage_position": {
            "chain": ["specify", "implement"],
            "current": "implement",
            "hop_count": 1,
            "hop_ceiling": 10
          },
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "hold": null,
          "status_text": "Between stages: specify to implement",
          "status_text_full": "Moved from stage specify to stage implement, and Sortie added `stage-implement`; implement starts when a poll tick reads that label on the issue."
        }
      ],
      "active": [
        {
          "issue_id": "ghi789",
          "issue_identifier": "MT-651",
          "issue_title": "Migrate the billing tables",
          "issue_url": "https://tracker.example.com/browse/MT-651",
          "status": "budget_exhausted",
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "dispatched_at": "2026-02-24T19:31:07Z",
          "status_text": "Held: session budget reached",
          "status_text_full": "Held at the session budget of 3. Sessions used: 3."
        },
        {
          "issue_id": "jkl012",
          "issue_identifier": "MT-652",
          "issue_title": "Rotate the signing keys",
          "issue_url": "https://tracker.example.com/browse/MT-652",
          "status": "parked",
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "dispatched_at": "2026-02-24T19:40:12Z",
          "status_text": "Parked: the agent needs a person",
          "status_text_full": "Parked: the agent stopped because it cannot go on without a person. Parking label: `needs-human`."
        },
        {
          "issue_id": "abc123",
          "issue_identifier": "MT-649",
          "issue_title": "Fix the flaky login test",
          "issue_url": "https://tracker.example.com/browse/MT-649",
          "status": "running",
          "rule_name": "implement",
          "stage_position": {
            "chain": ["specify", "implement"],
            "current": "implement",
            "hop_count": 1,
            "hop_ceiling": 10
          },
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "dispatched_at": "2026-02-24T19:48:20Z",
          "status_text": "Fixing CI on #936, fix 1 of 2",
          "status_text_full": "Fixing CI on PR #936, fix 1 of 2."
        },
        {
          "issue_id": "mno345",
          "issue_identifier": "MT-653",
          "issue_title": "Add the export button",
          "issue_url": "https://tracker.example.com/browse/MT-653",
          "status": "pending_reactions",
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "reaction_kind": "ci",
          "dispatched_at": "2026-02-24T19:58:03Z",
          "status_text": "CI escalated on #931",
          "status_text_full": "Sortie escalated failing CI on PR #931; it keeps watching the pull request. CI fixes used: 2 of 2."
        },
        {
          "issue_id": "pqr678",
          "issue_identifier": "MT-654",
          "issue_title": "Plan the export schema",
          "issue_url": "https://tracker.example.com/browse/MT-654",
          "status": "between_stages",
          "rule_name": "implement",
          "stage_position": {
            "chain": ["specify", "implement"],
            "current": "implement",
            "hop_count": 1,
            "hop_ceiling": 10
          },
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "dispatched_at": "2026-02-24T20:01:17Z",
          "status_text": "Between stages: specify to implement",
          "status_text_full": "Moved from stage specify to stage implement, and Sortie added `stage-implement`; implement starts when a poll tick reads that label on the issue."
        },
        {
          "issue_id": "def456",
          "issue_identifier": "MT-650",
          "issue_title": "Retry the export job",
          "issue_url": "https://tracker.example.com/browse/MT-650",
          "status": "retrying",
          "rule_name": "default",
          "stage_position": null,
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "dispatched_at": "2026-02-24T20:02:41Z",
          "status_text": "Retry 3 in 30s: no agent slot was free",
          "status_text_full": "The retry came due and was not dispatched: no agent slot was free. Retry 3 in 30s."
        }
      ],
      "held": [
        {
          "issue_id": "stu901",
          "issue_identifier": "MT-655",
          "issue_title": "Add the export button",
          "issue_url": "https://tracker.example.com/browse/MT-655",
          "issue_type": "Story",
          "issue_labels": ["export"],
          "state": "To Do",
          "hold": {
            "reason": "blocked_by",
            "held_since": "2026-02-24T20:09:44Z",
            "blockers": [
              {"issue_id": "abc124", "issue_identifier": "MT-648", "state": "In Review"}
            ],
            "slot": null,
            "resets_at": null,
            "reason_text": "Blocked by MT-648 (In Review)",
            "release_text": "Released when the blocker reaches a terminal state."
          },
          "status_text": "Blocked by MT-648 (In Review)",
          "status_text_full": "Blocked by MT-648 (In Review). Sortie dispatches the issue once the blocker reaches a terminal state."
        }
      ],
      "attention": [
        {
          "id": "d028c459215208a709b5ecc3ff4535b1",
          "kind": "workflow_reload_failed",
          "severity": "critical",
          "since": "2026-02-24T20:14:51Z",
          "acknowledged_at": null,
          "reason_text": "WORKFLOW.md failed to load",
          "why_text": "WORKFLOW.md failed to load (config: agent.turn_timeout_ms: must be greater than 0), so poll ticks dispatch nothing until it loads, and the configuration that last loaded stays in effect.",
          "clears_text": "Clears when WORKFLOW.md loads again, which Sortie tries each time the file is saved and on every poll tick.",
          "action": {"kind": "acknowledge"},
          "evidence": {
            "file": "WORKFLOW.md",
            "error": "config: agent.turn_timeout_ms: must be greater than 0"
          }
        },
        {
          "id": "8a6c3b55f9b8dd4b16eb33baf67df78d",
          "kind": "parked",
          "severity": "warning",
          "since": "2026-02-24T20:05:00Z",
          "acknowledged_at": null,
          "issue_id": "jkl012",
          "issue_identifier": "MT-652",
          "issue_title": "Rotate the signing keys",
          "issue_url": "https://tracker.example.com/browse/MT-652",
          "reason_text": "Parked: the agent needs a person",
          "why_text": "The agent stopped because it cannot go on without a person, so Sortie parked the issue under the label `needs-human`.",
          "clears_text": "Clears when a poll tick finds `needs-human` removed from the issue, or the issue moved out of In Progress.",
          "action": {"kind": "open_issue", "url": "https://tracker.example.com/browse/MT-652"},
          "evidence": {
            "reason": "agent_blocked",
            "label": "needs-human"
          }
        },
        {
          "id": "464f9ff8ea54c4c6cfb43af394234d57",
          "kind": "budget_held",
          "severity": "warning",
          "since": "2026-02-24T20:12:00Z",
          "acknowledged_at": "2026-02-24T20:13:05Z",
          "issue_id": "ghi789",
          "issue_identifier": "MT-651",
          "issue_title": "Migrate the billing tables",
          "issue_url": "https://tracker.example.com/browse/MT-651",
          "reason_text": "Session budget reached",
          "why_text": "The issue has used 3 sessions of its budget of 3.",
          "clears_text": "Clears on the next poll tick after `agent.max_sessions` is raised above 3 or is removed, or after the issue leaves the active tracker states.",
          "action": {"kind": "acknowledge"},
          "evidence": {
            "reason": "session_budget",
            "used_sessions": 3,
            "budget_sessions": 3
          }
        },
        {
          "id": "cd2e4d3c2c309f9ae99caa324f6e5d9d",
          "kind": "reaction_escalated",
          "severity": "warning",
          "since": "2026-02-24T20:12:40Z",
          "acknowledged_at": null,
          "issue_id": "mno345",
          "issue_identifier": "MT-653",
          "issue_title": "Add the export button",
          "issue_url": "https://tracker.example.com/browse/MT-653",
          "reason_text": "CI escalated on #931",
          "why_text": "Sortie escalated failing CI on PR #931 after 2 of 2 CI fixes; the latest check found Test failing.",
          "clears_text": "Clears when a check of PR #931 passes, a new commit lands on it, it merges or closes, `reactions.ci_failure.max_retries` rises above 2, or the issue reaches a terminal tracker state or leaves In Review.",
          "action": {"kind": "open_pull_request", "url": "https://github.com/acme/web/pull/931"},
          "evidence": {
            "reaction_kind": "ci",
            "config_key": "ci_failure",
            "pull_request": {
              "number": 931,
              "repository": "acme/web",
              "url": "https://github.com/acme/web/pull/931",
              "branch": "sortie/MT-653",
              "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
            },
            "escalation_trigger": "budget_exhausted",
            "used_attempts": 2,
            "max_attempts": 2,
            "last_check": {
              "observed_at": "2026-02-24T20:12:40Z",
              "head_sha": "9b7e3d1c0a2f4e6b8d0c1a3e5f7b9d2c4e6a8f0b",
              "status": "failing",
              "failing_checks": ["Test"],
              "failing": 1,
              "pending": 0,
              "passing": 4
            }
          }
        }
      ],
      "agent_totals": {
        "input_tokens": 5000,
        "output_tokens": 2400,
        "total_tokens": 7400,
        "cache_read_tokens": 1500,
        "cache_write_tokens": 300,
        "seconds_running": 1834.2,
        "unmeasured_sessions": 3,
        "running_unreported": 1,
        "running_non_reporting": 1
      },
      "rate_limits": {
        "claude-code": {
          "reported_at": "2026-10-28T13:57:00Z",
          "windows": [
            {"label": "5-hour limit", "used_percent": null, "status": "allowed", "resets_at": "2026-10-28T17:00:00Z", "reported_at": "2026-10-28T13:57:00Z", "amount": null},
            {"label": "Weekly limit", "used_percent": 82, "status": "warning", "resets_at": "2026-10-30T09:00:00Z", "reported_at": "2026-10-28T11:20:00Z", "amount": null}
          ]
        },
        "agent-client-protocol": {
          "reported_at": "2026-10-28T13:50:00Z",
          "windows": [
            {"label": "Monthly limit", "used_percent": 0.708, "status": null, "resets_at": "2026-11-01T00:00:00Z", "reported_at": "2026-10-28T13:50:00Z", "amount": {"used": 7.08, "limit": 1000, "unit": "credits"}}
          ]
        }
      },
      "config": {
        "tracker": {"kind": "jira", "project": "PROJ", "active_states": ["To Do", "In Progress"], "in_progress_state": "In Progress", "handoff_state": "In Review", "terminal_states": ["Done"]},
        "polling": {"interval_ms": 30000},
        "agent": {
          "kind": "claude-code", "usage_arrival": "incremental", "max_turns": 20, "stall_timeout_ms": 300000,
          "max_retry_backoff_ms": 300000, "max_sessions": 6, "max_tokens": null, "max_cost_usd": null,
          "max_concurrent_agents": 6, "max_concurrent_agents_by_state": {}
        },
        "worker": {"ssh_hosts": [], "max_concurrent_agents_per_host": null},
        "dispatch": {
          "rules": [
            {"name": "specify", "selected_by": "match", "match": {"issue_type": ["Story"]}, "next": "implement", "agent_kind": "claude-code", "usage_arrival": "incremental"},
            {"name": "implement", "selected_by": "stage", "stage": "stage-implement", "agent_kind": "claude-code", "usage_arrival": "incremental"},
            {"name": "deps", "selected_by": "match", "match": {"labels": ["dependencies"], "priority": {"op": "lte", "value": 2}}, "agent_kind": "codex", "usage_arrival": "incremental"},
            {"name": "everything", "selected_by": "catch_all", "agent_kind": "claude-code", "usage_arrival": "incremental"}
          ],
          "default": null,
          "chains": [["specify", "implement"]],
          "max_consecutive_hops": null,
          "hop_ceiling": 10
        },
        "reactions": {
          "ci_failure": {"max_retries": 2, "triage": false, "escalation": "label", "escalation_label": "needs-human"},
          "review_comments": {"max_continuation_turns": 3, "debounce_ms": 60000, "poll_interval_ms": 120000, "triage": false, "escalation": "label", "escalation_label": "needs-human"},
          "bot_review": {"max_continuation_turns": 5, "poll_interval_ms": 60000, "bot_usernames": ["review-bot"], "triage": false, "escalation": "none"},
          "auto_merge": {"strategy": "squash", "require_ci": true, "delete_branch": true, "poll_interval_ms": 60000, "max_retries": null, "escalation": "label", "escalation_label": "needs-human"}
        },
        "token_rates": {
          "claude-code": {"input_per_mtok": 3.0, "output_per_mtok": 15.0, "cache_read_per_mtok": 0.3, "cache_write_per_mtok": null, "complete": true, "per_kind": true},
          "codex": {"input_per_mtok": 2.5, "output_per_mtok": null, "cache_read_per_mtok": null, "cache_write_per_mtok": null, "complete": false, "per_kind": true}
        }
      },
      "health": {
        "database": {"status": "pass", "checked_at": "2026-02-24T20:15:03Z", "last_success_at": "2026-02-24T20:15:03Z", "consecutive_failures": 0, "failing_since": null},
        "preflight": {"status": "fail", "checked_at": "2026-02-24T20:15:03Z", "last_success_at": "2026-02-24T20:14:32Z", "consecutive_failures": 1, "failing_since": "2026-02-24T20:15:03Z", "error": "dispatch preflight failed: workflow file cannot be loaded: config: agent.turn_timeout_ms: must be greater than 0"},
        "workflow": {"file": "WORKFLOW.md", "status": "fail", "checked_at": "2026-02-24T20:15:03Z", "last_success_at": "2026-02-24T20:14:32Z", "consecutive_failures": 2, "failing_since": "2026-02-24T20:14:51Z", "error": "config: agent.turn_timeout_ms: must be greater than 0"},
        "poll": {"status": "fail", "checked_at": "2026-02-24T20:15:04Z", "last_success_at": "2026-02-24T20:14:33Z", "consecutive_failures": 1, "failing_since": "2026-02-24T20:15:04Z", "outcome": "preflight_failed", "next_poll_at": "2026-02-24T20:15:34Z", "summary_text": "The latest poll stopped at the dispatch preflight before reading the tracker; the last successful poll was 57s ago."},
        "scm": {"provider": "github", "status": "pass", "checked_at": "2026-02-24T20:15:04Z", "last_success_at": "2026-02-24T20:15:04Z", "consecutive_failures": 0, "failing_since": null}
      }
    }
    ```

- `GET /api/v1/runs`
  - Returns the finished runs of run history, newest first, one page at a time. Filters narrow the result on the server, before the page is cut.
  - Query parameters. Each is optional, an empty value is the same as an absent one, and filters combine with AND and match exactly, case included.
    - `issue`: the issue identifier; selects the runs whose `issue_identifier` equals it.
    - `issue_id`: the tracker issue ID; selects the runs whose `issue_id` equals it. The per-issue response's `history.next_cursor` continues on this filter.
    - `rule`: a dispatch rule name, `default` for `dispatch.default`, or `<none>` for the runs that carry no `rule_name`, which ran on the workflow-wide fallback. `<none>` is the sentinel Section 5.3.9 gives an empty rule name, which no rule name can take.
    - `status`: the run status.
    - `stage`: a dispatch rule name; selects the runs whose exit reached a hop decision targeting that rule, whatever its result.
    - `since` and `until`: RFC 3339 timestamps in any offset, fractions allowed. `since` selects the runs completed at or after it and `until` the runs completed before it, each compared as an instant. When both are given, `since` MUST be before `until`.
    - `ended_since` and `ended_until`: RFC 3339 timestamps under the rules of `since` and `until`. `ended_since` selects every run of an issue whose outcome (Section 4.1.8) was observed at or after it and `ended_until` every run of an issue whose outcome was observed before it, each compared as an instant. A run that completed before `ended_since` is selected when its issue's outcome was observed after it, and a run of an issue with no outcome matches neither bound. When both are given, `ended_since` MUST be before `ended_until`. A selected row carries the issue's outcome as the read sees it, so an issue picked up again after the selection serves `null`, which a client reads as not ended.
    - `cursor`: a `next_cursor` value from the previous page.
    - `limit`: the page size, an integer from 1 to 200, default 50.
  - Order is descending `run_id`, the order Sortie recorded the runs. Each run is appended at its completion, so this is completion order; across a wall-clock step back the route promises no order of `completed_at`.
  - `next_cursor` is non-null exactly when a run older than the page's last run matches the filters. It is the last served run ID in decimal, opaque to a client, and valid with any filters. The next page holds smaller run IDs, so a run recorded between two requests never lands on a later page and never displaces a served run; a client sees it by reading the first page again.
  - A parameter this route does not define, a parameter given more than once, and a value that breaks the rules above answer `400` with the error code `invalid_parameter`, whose message names the parameter and the rule. The route reads nothing in that case, so a client never takes unfiltered rows for filtered ones. A run-history read that fails, the read of the served issues' outcomes included, answers `503` with `history_unavailable`, as does a server without run history. Any method other than `GET` answers `405`.
  - Suggested response shape:

    ```json
    {
      "runs": [
        {
          "run_id": 42,
          "issue_id": "abc123",
          "issue_identifier": "MT-649",
          "issue_title": "Fix the flaky login test",
          "issue_url": "https://tracker.example.com/browse/MT-649",
          "attempt": 2,
          "status": "succeeded",
          "error": null,
          "declared_outcome": null,
          "agent_text": null,
          "rule_name": "implement",
          "agent_kind": "claude-code",
          "configured_model": "claude-sonnet-4-20250514",
          "configured_effort": "high",
          "model_name": "claude-sonnet-4-20250514",
          "workflow_file": "WORKFLOW.md",
          "started_at": "2026-02-24T20:10:12Z",
          "completed_at": "2026-02-24T20:31:47Z",
          "dispatched_at": "2026-02-24T19:48:20Z",
          "turns_completed": 9,
          "tokens": {
            "input_tokens": 52000,
            "output_tokens": 9100,
            "total_tokens": 61100,
            "cache_read_tokens": 30000,
            "cache_write_tokens": 4000
          },
          "uncached_tokens": 31100,
          "tokens_measured": true,
          "unaccounted_turns": 0,
          "estimated_cost_usd": 0.1923,
          "cost_status": "estimated",
          "model_usage": [
            {
              "model": "claude-sonnet-4-20250514",
              "tokens": {
                "input_tokens": 52000,
                "output_tokens": 9100,
                "total_tokens": 61100,
                "cache_read_tokens": 30000,
                "cache_write_tokens": 4000
              },
              "uncached_tokens": 31100,
              "estimated_cost_usd": 0.1923,
              "cost_status": "estimated",
              "unrated_classes": []
            }
          ],
          "chain_id": "3c8v1n6w0z4y7t2s5r9q8p1mkd",
          "stage_previous": "specify",
          "stage_previous_outcome": "succeeded",
          "stage_target": "review",
          "stage_result": "advanced",
          "self_review": {
            "final_verdict": "pass",
            "total_iterations": 2,
            "cap_reached": false
          },
          "origin_recorded": true,
          "trigger": "ci_fix",
          "reaction_kind": "ci",
          "reaction_config_key": "ci_failure",
          "session_id": "thread-1-turn-1",
          "dispatch_id": "ZYFOD7WNYR3TGMMZBTOKQ6ZS5A",
          "ssh_host": "",
          "stage_chain": ["specify", "implement", "review"],
          "issue_outcome": {
            "kind": "merged",
            "observed_at": "2026-02-24T21:02:10Z",
            "merge_path": "auto_merge",
            "pull_request": {"number": 936, "repository": "acme/web"},
            "status_text": "Merged #936",
            "status_text_full": "Ended merged: auto-merge merged PR #936."
          }
        }
      ],
      "next_cursor": "42"
    }
    ```

- `GET /api/v1/runs/issues`
  - Returns the finished runs of run history grouped by issue, one page of issues at a time, newest group first. A group holds one issue however many runs it has, so an issue that retried many times occupies one group and never fills the page, which a flat list cannot promise.
  - Query parameters. Each is optional, an empty value is the same as an absent one, and filters combine with AND and match exactly, case included, except `q`.
    - `issue`: an issue key; selects the issues with a run whose `issue_identifier` or `display_identifier` equals it, so the qualified form a GitHub, GitLab, or Gitea tracker shows (`owner/repo#9`) finds its issue. A selected issue's group lists its runs whatever key each run recorded. This differs from `GET /api/v1/runs`, where `issue` matches `issue_identifier` alone.
    - `q`: search text; selects the issues with a run whose `issue_identifier`, `display_identifier`, or `issue_title` contains it, ignoring ASCII case. With `issue` also given, both must hold.
    - `rule`, `status`, and `stage`: as on `GET /api/v1/runs`, where `rule` is a dispatch rule name, `default`, or `<none>`. They select runs, so a group exists only while one of its issue's runs matches all of them given, and the group lists only matching runs.
    - `cursor`: a `next_cursor` value from the previous page.
    - `limit`: the page size in groups, an integer from 1 to 50, default 20.
  - A group is one issue, by `issue_id`, with at least one matching run. Groups are ordered by the issue's newest matching run, descending `run_id`.
  - The page's `next_cursor` is non-null exactly when a group exists below the page's last group. It is the `run_id` of that last group's newest matching run in decimal, opaque to a client. The next page holds the groups whose newest matching run is older and that have no matching run at or above the cursor, so a traversal serves each group once. A run recorded between two requests moves its issue's group to the first page and never repeats a group already served on a later page.
  - A group carries these members:
    - `issue_id`, `issue_identifier`, `display_identifier`, `issue_title`, and `issue_url` are those of the issue's newest run, under the run row rules: omitted when empty, and the title and link masked as Section 15.3 states, when the response is built.
    - `status`, `status_text`, and `status_text_full` are what `GET /api/v1/<issue_identifier>` serves for the issue at the same instant. While a live entry of the snapshot holds the issue ID, they are that entry's status and sentence pair. Otherwise `status` is `history` and the pair is worded from the issue's outcome and its newest run's hop decision.
    - `issue_outcome` is the issue's outcome in the run row shape, `null` when it has none.
    - `runs` holds the issue's newest matching runs, at most 5, newest first, in the run row shape, none newer than the group's newest matching run.
    - `next_cursor` is non-null exactly when the issue has an older matching run. It continues on `GET /api/v1/runs` with `issue_id` set to the group's `issue_id` and the request's `rule`, `status`, and `stage`.
    - `totals` is the `history.totals` of the per-issue route, over every run of the issue, whatever the filters select.
  - The orchestrator state is read before run history, as the per-issue route reads it, so a run that ends between the reads appears as live and in history, never in neither.
  - A parameter this route does not define, a parameter given more than once, a `limit` that is not an integer from 1 to 50, and a `cursor` that is not an integer of 1 or more answer `400` with the error code `invalid_parameter`, whose message names the parameter and the rule, for example `parameter "limit" must be an integer from 1 to 50`. The route reads nothing in that case. A server without run history, and a run-history read that fails, the read of outcomes included, answer `503` with `history_unavailable`. A snapshot that fails answers `503` with `snapshot_unavailable`. Any method other than `GET` answers `405`. A client that disconnects gets no response, and the reads stop.
  - Suggested response shape, abridged to one group with one run:

    ```json
    {
      "groups": [
        {
          "issue_id": "abc123",
          "issue_identifier": "9",
          "display_identifier": "acme/web#9",
          "issue_title": "Fix the flaky login test",
          "issue_url": "https://github.com/acme/web/issues/9",
          "status": "history",
          "status_text": "Merged #936",
          "status_text_full": "Ended merged: auto-merge merged PR #936.",
          "issue_outcome": {
            "kind": "merged",
            "observed_at": "2026-02-24T21:02:10Z",
            "merge_path": "auto_merge",
            "pull_request": {"number": 936, "repository": "acme/web"},
            "status_text": "Merged #936",
            "status_text_full": "Ended merged: auto-merge merged PR #936."
          },
          "runs": [
            {
              "run_id": 42,
              "issue_id": "abc123",
              "issue_identifier": "9",
              "display_identifier": "acme/web#9",
              "attempt": 2,
              "status": "succeeded",
              "started_at": "2026-02-24T20:10:12Z",
              "completed_at": "2026-02-24T20:31:47Z"
            }
          ],
          "next_cursor": "37",
          "totals": {
            "run_count": 6,
            "unmeasured_runs": 0,
            "tokens": {
              "input_tokens": 310000,
              "output_tokens": 52000,
              "total_tokens": 362000,
              "cache_read_tokens": 150000,
              "cache_write_tokens": 20000
            },
            "uncached_tokens": 212000,
            "tokens_complete": true,
            "estimated_cost_usd": 1.1425,
            "cost_status": "estimated"
          }
        }
      ],
      "next_cursor": "31"
    }
    ```

- `GET /api/v1/runs/{dispatch_id}/events`
  - Returns every retained agent event of one run, oldest first, in the event shape of Section 13.9. `{dispatch_id}` is the run's dispatch ID, one path segment matched exactly, as a running row and a run row serve it. The route answers a running run from its memory buffer and a finished run from the stored copy, and ignores the query string.
  - Response (`200 OK`) shape:

    ```json
    {
      "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
      "first_seq": 1,
      "events": [
        {
          "seq": 1,
          "at": "2026-02-24T20:10:14Z",
          "event": "session_started",
          "message": "",
          "text": "Session started.",
          "session_id": "thread-1-turn-1"
        }
      ]
    }
    ```

  - `first_seq` is the `seq` of the oldest event served. A value above `1` says the run's earlier events were evicted, and a client says so instead of showing the list as the whole run. It is `null` when `events` is `[]`, which only a running run that has retained no event yet serves.
  - `404 Not Found` with code `run_events_not_retained` answers a dispatch ID whose events are not retained: an unknown ID, a run whose stored events were pruned, a run whose events were not written, a run lost in a crash, a run recorded before retention existed, and a finished run that retained no event. A run-history read that fails answers `503` with `history_unavailable`, as does a server without run history for a run that is not running. Any method other than `GET` answers `405`.

- `GET /api/v1/stats`
  - Returns the run statistics report that `sortie stats --format json` prints, over the finished runs of run history in a range. The route serves that report unchanged and computes it through the same aggregation as the command, so no figure is computed twice and no second shape exists. `chain_funnels` also counts the passes the orchestrator holds now; the route and the command read them from the database, never from the running orchestrator.
  - Query parameters. Each is optional, an empty value is the same as an absent one, and an open bound is unlimited, so a request without parameters covers every run on record.
    - `since`: a range bound; selects the runs completed at or after it.
    - `until`: a range bound; selects the runs completed before it. When both are given, `since` MUST be before `until`.
    - A range bound is an RFC 3339 timestamp in any offset (fractions allowed), a date such as `2026-07-01` (00:00:00 UTC that day), or a positive Go duration such as `24h`, which counts back from the instant of the request. A timestamp without an offset, a zero or negative duration, and any other text are malformed.
  - `generated_at` is the instant of the request, and relative bounds count from it. `since` and `until` carry the resolved bounds in UTC, `null` when open.
  - Every cost is the cost recorded with each run (Section 13.5). The command and the route parse no rates and price nothing, so they report the same cost for the same range whatever rates are in force. A database whose rows carry no recorded cost, because no service start has applied the schema change that records it, yields no cost figures, and its capability warning names cost among what it leaves out. `warnings` holds at most one warning for an older database: it names every column the database lacks and every part of the report left out because of it.
  - `summary.issues` is the number of distinct issues among the runs `summary.runs` counts, `0` when no run is in range. `by_trigger` groups those runs by the retry trigger that started them, one group per stored value, named by the value. `<none>` names the empty trigger: runs dispatched from the poll tick, runs a reaction other than the CI fix started, and recorded CI results. `<unknown>` names a run whose row records no trigger: runs recorded before the trigger was kept, and runs dispatched by a retry that never stored what started it. The groups partition the counted runs, carry the figures `by_rule` computes, and sort like every other breakdown. `by_trigger` is `[]` when the database predates the trigger column, and `warnings` says so.
  - `by_origin` groups the same runs by what started each one, and the first rule that matches decides a run's group. `<unknown>` names a run whose row records no trigger. A non-empty trigger names the group (`error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`), so a group named by a retry trigger holds the same runs, with the same figures, in `by_origin` and in `by_trigger`. A run with an empty trigger and a reaction kind takes the name of the kind (`review`, `bot-review`, `merge-conflict`, `label-review`, or `label-fix`). `<none>` names recorded CI results, which no dispatch started. `stage_hop` names the first run dispatched after a stage hop and no later run. `poll_tick` names every other run the poll tick dispatched, a later dispatch into a stage a hop led to included while the hop record stands, as after a restart, a released hold, or a lost retry. The groups partition the counted runs, carry the figures `by_rule` computes, and sort like every other breakdown. `by_origin` is `[]` when the database predates the first-run column, and `warnings` says so; `by_trigger` is `[]` beside it when the database also predates the trigger columns.
  - `skill_calls` counts the skill calls the runs recorded in their rows' `skills`. It is `null` on a database that lacks the skills column or a column an earlier tier needs, and `warnings` says so. Otherwise it holds `unrecorded_runs` and `by_skill`. `unrecorded_runs` counts the counted runs whose row records no skill calls: a row written before Sortie kept them, a value that does not decode as a list of skill calls, and a run whose calls could not be stored. A run that called no skill stores an empty list, and so does a CI result recorded since Sortie keeps skill calls, so neither is counted there. `by_skill` holds one entry per distinct skill name among the other counted runs, sorted by `runs` descending and then by `skill` ascending in byte order, and is `[]`, never `null`, when none of them called a skill or no run is in range. An entry carries `skill` as stored (no trimming, no case folding, no placeholder name), `runs` (the runs that called it at least once), `calls` (every call, repeats included), and `failed_calls` (the calls that failed). `failed_calls` never exceeds `calls`, and `runs` never exceeds `calls` or `summary.runs`. The entries are not a partition: a run that called two skills counts in both, so their `runs` can add up to more than `summary.runs`. A client reads the counts as lower bounds while `unrecorded_runs` is above `0`.
  - Every entry of every breakdown, `by_chain` entries included, carries `cost_unpriced_runs`, counted as `summary.cost_unpriced_runs` counts it, over the group's runs: the measured runs, a recorded CI result excepted, whose recorded cost status is `unpriced`, which the group's `cost_usd` leaves out. A `by_model` group counts model shares instead of whole runs, as its bullet below states. It is `0` on the `base` tier. A client reads a group's `cost_usd` as complete only while `cost_unpriced_runs`, `cost_partial_runs`, and `tokens_unmeasured_runs` are all `0`. The text form of `sortie stats` does not change.
  - The `summary` and every entry of every breakdown, `by_chain` entries included, carry `cost_partial_runs`: the runs whose recorded cost status is `partial`, a recorded CI result excepted. A run counts whether or not it spent tokens, because a run that spent nothing and recorded an unaccounted turn is `partial`. The group's `cost_usd` includes their recorded cost, which is a lower bound. A run's recorded status is one value, so no run counts in both `cost_unpriced_runs` and `cost_partial_runs`. It is `0` on the `base` tier and on a database whose rows carry no recorded cost.
  - `cost_per_succeeded_run_usd`, in the summary and in every group, is `cost_usd` divided by the succeeded runs that reported usage. It is rounded as `cost_usd` is and not to cents, so a figure under one cent never reads as zero.
  - `spend` is the recorded cost of the runs the report counts, summed per UTC calendar period for a chart. It is `null` on the `base` tier and on a database whose rows carry no recorded cost; otherwise it holds `period` and `buckets`. `period` is `day` when the range has a `since` and `since` lies no more than 2160 hours before the end of the range, which is `until` or else `generated_at`, and `month` otherwise, so the last 24 hours and the last 7, 30, and 90 days get days and an open `since` gets months. One rule maps a range to a period, and no client derives it. `buckets` ascend by `start`, the first instant of the period as an RFC 3339 timestamp in UTC. They list every period from the one holding `since` (the one holding the earliest run when `since` is open) through the later of the one holding the end of the range and the one holding the latest run, so a period without runs appears with `runs` `0` and `cost_usd` `null`; `buckets` is `[]`, never `null`, when no run falls in a period. A bucket carries `runs`, `cost_usd`, `tokens_unmeasured_runs`, `cost_unpriced_runs`, and `cost_partial_runs`, counted as the summary counts them over the bucket's runs, with `cost_usd` the sum of the recorded costs of the priced runs and `null` when none is priced. A run counts in the bucket whose period holds its `completed_at`, the span the daily and monthly spend ceilings count, so the buckets partition the counted runs: their `runs`, `tokens_unmeasured_runs`, `cost_unpriced_runs`, and `cost_partial_runs` sum to the summary's and their `cost_usd` values to `summary.cost_usd` within $0.000001. The first bucket of a range whose `since` lies inside a period holds only the runs at or after `since`. A bucket reads recorded costs and prices nothing. A run whose `completed_at` does not parse as an RFC 3339 timestamp counts in the summary and the breakdowns and in no bucket; Sortie writes none.
  - `by_model` splits each run across the per-model parts recorded with it. A run counts once in the group of every model it spent tokens on, with that part's tokens and recorded cost and with the run's outcome, turns, and duration, so a group's success rate, mean turns, duration, and cost per succeeded run cover the runs that used the model. A run with no part that spent tokens counts once, under the model its runtime reported, else its configured model, whatever its age. The groups' `runs` can add up to more than `summary.runs` and their `share` values to more than `1`, and no run-level token or cost total reaches a group. For a run priced per model the groups' tokens and costs add up to the run's; under a per-kind `token_rates` entry the parts' recorded costs can differ in sum from the run's recorded cost, and the groups carry the parts as recorded and never reconcile them. A `by_model` group counts `tokens_unmeasured_runs`, `cost_unpriced_runs`, and `cost_partial_runs` per model share: a share from a part counts as partial when the part is `partial`, or when it is `estimated` and the run recorded unaccounted turns, because the spend those turns leave out belongs to no recorded model, so such a run counts as partial in every group whose part carries a recorded cost; a share from an `unpriced` part counts in `cost_unpriced_runs` only. On a database whose rows carry no recorded cost, every run counts under the one model it reported, else its configured model.
  - Every non-null `tokens` object of the report, the summary's and each breakdown entry's, carries `uncached` beside the stored sums: `total` less `cache_read`, floored at zero, with cache writes kept (Section 13.5). It is derived when the report is built, so `uncached` plus `cache_read` equals `total`.
  - `chain_funnels` is not a breakdown: it holds one funnel per stage chain definition and its entries are stages, without the figures a breakdown group carries. A definition is the chains that share a stage, merged, and the funnels follow the order of the first chain of each definition in the workflow. A funnel lists its `stages` in chain order, one entry per stage however many passes ran: the stages of the definition's first chain, then, before the stage where a later chain joins, the stages only that chain holds. A stage entry carries `rule`, `entry` (true on a stage a chain starts at), `last` (true on the final stage of the definition), `runs`, `entered`, `arrivals`, `outcomes`, `no_hop_reasons`, and `median_seconds`.
    - A pass is the runs that share a stage chain identifier, and it enters a stage once however many runs it makes there. A run decides its pass on a stage when it is a stage run or a reaction run that recorded a hop result; the latest such run decides. `runs` counts the finished runs of the stage's rule in range and equals the `runs` of that rule's `by_rule` group, reaction runs and runs without a chain identifier included. `entered` counts the passes with a deciding run on the stage in range or held there now.
    - `arrivals` splits `entered` by how the pass reached the stage, and the first match decides: `hop` when a stage hop from a previous stage reached it, `label` when the rule is one a stage label selects, `match` otherwise. `outcomes` splits `entered` by how the pass left it, and the first matching row decides:
      1. A running session or a queued retry holds the pass: `in_stage`.
      2. The latest hop result is `advanced` or `partial`: `advanced`, or `handed_off` on the last stage.
      3. The latest hop result is `ceiling`: `ceiling`.
      4. The latest hop result is `failed`: `hop_failed`.
      5. The latest run succeeded and its agent declared `blocked`: `no_hop`, reason `needs_person`.
      6. The latest run succeeded on the last stage: `handed_off`.
      7. A budget holds the pass: `no_hop`, reason `budget_held`.
      8. The latest run succeeded on another stage: `no_hop`, reason `operator_stopped`.
      9. The latest run failed because the evidence verdict withheld the handoff: `no_hop`, reason `verdict_withheld`.
      10. The latest run's status is `needs_person`: `no_hop`, reason `needs_person`.
      11. The latest run's status is `budget_stopped`: `no_hop`, reason `budget_held`.
      12. The latest run's status is `usage_limited`: `no_hop`, reason `usage_limited`.
      13. The latest run's status is `cancelled`: `no_hop`, reason `operator_stopped`.
      14. Any other status: `no_hop`, reason `run_failed`.
    - A budget hold that follows a hop decision or a handoff changes nothing, because rows 2 to 6 come first. `no_hop_reasons` lists each reason with passes, as `{"reason", "passes"}`, by passes descending and, on a tie, in the order `run_failed`, `needs_person`, `budget_held`, `usage_limited`, `operator_stopped`, `verdict_withheld`. On every stage `entered` equals the sum of the `arrivals` members and the sum of the `outcomes` members, `outcomes.no_hop` equals the sum of the reasons' passes, and `advanced` is `0` on the last stage and `handed_off` is `0` elsewhere.
    - `median_seconds` is the median over the passes of the seconds their finished stage runs on the stage took, summed per pass over the runs whose duration the report uses, and `null` when no pass has such a run. A pass that only a live pass supplies adds no sample.
    - Live passes count only when `until` is open or after `generated_at`; for a range that ends earlier, each pass is decided by its runs in range. A key that a live pass and finished runs share counts once.
    - `chain_funnels` is `[]` when the workflow has no stage chains, and when the database predates the column that records which runs their agent declared `blocked`; `warnings` says so in the second case while chains exist. A range with no run lists every definition with zero counts, apart from the passes held now.
  - `summary.succeeded`, `failed`, `stopped`, and `cancelled` count the runs by how they ended and add up to `summary.runs`. `failed` counts `failed` and `ci_failed` runs and any status the report does not know, `stopped` counts `needs_person`, `budget_stopped`, and `usage_limited` runs, and `cancelled` counts `cancelled` runs.
  - With no run in range the route answers `200` with `summary.runs` and `summary.issues` at `0`, every breakdown `[]`, `chain_funnels` still listing every definition, and `null` for every figure no run supplies.
  - The route refuses a request it cannot read exactly as asked, as `GET /api/v1/runs` does. A parameter this route does not define, a parameter given more than once, a value that is not a range bound, and a `since` that is not before `until` answer `400` with the error code `invalid_parameter`, whose message names the parameter and the rule, and the route reads nothing. A failed read, or a server without run history, answers `503` with `history_unavailable`. Any method other than `GET` answers `405`. A client that disconnects during the read gets no response, and the read stops.
  - The report is not masked, because it holds no issue titles or error text and no agent output beyond the skill names of `skill_calls`, which are the names stored with each run, masked when they were recorded, so the report reads no source the run-history routes do not already serve. Its strings are names and identifiers, including the operator's rule, template, and model names, and host paths: `workflow_path`, `db_path`, and the `by_template` names, served as the command prints them.
  - Each request reads on its own read-only connection to the database, never through the orchestrator's store, so a long range never delays a write of the orchestrator.
  - Suggested response shape, abridged to one group per breakdown:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "workflow_path": "/srv/sortie/WORKFLOW.md",
      "db_path": "/srv/sortie/.sortie.db",
      "since": "2026-02-23T20:15:30Z",
      "until": null,
      "schema_tier": "full",
      "warnings": [],
      "summary": {
        "runs": 6,
        "issues": 5,
        "succeeded": 3,
        "failed": 3,
        "stopped": 0,
        "cancelled": 0,
        "success_rate": 0.5,
        "duration_seconds": {"p50": 1200, "p95": 1800, "mean": 1200, "samples": 3},
        "mean_turns_succeeded": 4,
        "tokens": {"input": 3900000, "output": 760000, "total": 4660000, "cache_read": 50000, "cache_write": 10000, "uncached": 4610000},
        "cost_usd": 19.52,
        "cost_per_succeeded_run_usd": 6.506666667,
        "zero_duration_runs": 1,
        "duration_excluded_runs": 1,
        "cost_unpriced_runs": 1,
        "cost_partial_runs": 1,
        "tokens_unmeasured_runs": 1
      },
      "spend": {
        "period": "day",
        "buckets": [
          {"start": "2026-02-23T00:00:00Z", "runs": 2, "cost_usd": 7.27, "tokens_unmeasured_runs": 1, "cost_unpriced_runs": 0, "cost_partial_runs": 0},
          {"start": "2026-02-24T00:00:00Z", "runs": 4, "cost_usd": 12.25, "tokens_unmeasured_runs": 0, "cost_unpriced_runs": 1, "cost_partial_runs": 1}
        ]
      },
      "by_status": [
        {
          "name": "succeeded",
          "runs": 3,
          "succeeded": 3,
          "success_rate": 1,
          "share": 0.5,
          "duration_seconds": {"p50": 1200, "p95": 1800, "mean": 1200, "samples": 3},
          "mean_turns": 4,
          "tokens": {"input": 3500000, "output": 700000, "total": 4200000, "cache_read": 50000, "cache_write": 10000, "uncached": 4150000},
          "cost_usd": 17.87,
          "cost_per_succeeded_run_usd": 5.956666667,
          "tokens_unmeasured_runs": 0,
          "cost_unpriced_runs": 0,
          "cost_partial_runs": 0
        }
      ],
      "by_adapter": [],
      "by_rule": [],
      "by_template": [],
      "by_model": [],
      "by_chain": [],
      "chain_funnels": [],
      "by_trigger": [
        {
          "name": "<unknown>",
          "runs": 4,
          "succeeded": 2,
          "success_rate": 0.5,
          "share": 0.6667,
          "duration_seconds": {"p50": 600, "p95": 1800, "mean": 900, "samples": 4},
          "mean_turns": 3,
          "tokens": {"input": 3000000, "output": 600000, "total": 3600000, "cache_read": 0, "cache_write": 0, "uncached": 3600000},
          "cost_usd": 12.25,
          "cost_per_succeeded_run_usd": 6.125,
          "tokens_unmeasured_runs": 1,
          "cost_unpriced_runs": 1,
          "cost_partial_runs": 1
        }
      ],
      "by_origin": [
        {
          "name": "<unknown>",
          "runs": 4,
          "succeeded": 2,
          "success_rate": 0.5,
          "share": 0.6667,
          "duration_seconds": {"p50": 600, "p95": 1800, "mean": 900, "samples": 4},
          "mean_turns": 3,
          "tokens": {"input": 3000000, "output": 600000, "total": 3600000, "cache_read": 0, "cache_write": 0, "uncached": 3600000},
          "cost_usd": 12.25,
          "cost_per_succeeded_run_usd": 6.125,
          "tokens_unmeasured_runs": 1,
          "cost_unpriced_runs": 1,
          "cost_partial_runs": 1
        }
      ],
      "skill_calls": {
        "unrecorded_runs": 1,
        "by_skill": [
          {"skill": "plan", "runs": 2, "calls": 2, "failed_calls": 0},
          {"skill": "test", "runs": 1, "calls": 2, "failed_calls": 1}
        ]
      },
      "self_review": {
        "runs_with_metadata": 2,
        "by_final_verdict": [{"verdict": "iterate", "runs": 1}, {"verdict": "pass", "runs": 1}],
        "cap_reached_runs": 1,
        "mean_iterations": 2.5,
        "unparsed_runs": 1
      }
    }
    ```

- `GET /api/v1/prices`
  - Returns the rates in force when the request is served, per agent kind and model, the layer each rate comes from, and the state of every price source. Every layer, rate, and source state comes from the pricing component (Section 13.5); the route maps them onto the response and MUST NOT derive one itself. It states rates and prices no run, and a recorded run keeps the price it was written with. The route takes no query parameters, writes nothing, and is idempotent.
  - Times are RFC 3339 in UTC. A time that is unknown, a string that is empty, and the model count of a source with no table serve `null`. `generated_at` is the instant of the request.
  - `table_in_force` is `null` while no table layer is in force. Otherwise it holds `layer`, the highest-precedence table layer (`operator_table`, `fetched`, or `embedded`), `as_of`, that table's date, and `age_days`, the whole days from `as_of` to `generated_at`, never below `0`. Each model takes its rates from the first table layer that lists it, so the embedded table can price a name the layer named here lacks.
  - `token_rates` holds `kind_entries` and `model_entries`, the entries in force, and `version`, the version of those entries, `null` when there are none.
  - `operator` is `null` unless `price_table.replace_file` or `price_table.replace_url` is set. `fetched` and `embedded` are always present. A source holds `status` (`off`, `not_loaded`, `loaded`, or `failing`), `in_force` (the table is one of the table layers that price runs), `off_by`, `file`, `address`, `version`, `as_of`, `models`, `next_try_at`, `failures`, `failed_at`, and `error`.
    - `status` is decided from the settings in force before any download state is read. `off` is the shared download turned off, or the embedded table disabled. Otherwise it is `failing` while the source has failed tries or its latest workflow load failed, else `loaded` once it holds a table, else `not_loaded`.
    - `off_by` is set only on `fetched` while it is `off`: `SORTIE_PRICE_TABLE_FETCH` when the environment variable turned the download off, else `price_table.fetch`. `fetched` carries no table facts while it is `off`, because a kept copy never prices while the download is off. `embedded` carries the binary's revision, that revision's date, and its model count whether it is on or off.
    - `file` is the `price_table.replace_file` setting value, after `$VAR` resolution and before `~` expansion and path joining. `address` is the download address as its scheme, host, and path only, `null` when it does not parse, in which case the download reads `failing` with `invalid address`.
    - `version`, `as_of`, and `models` describe the table the source holds: its version (the pinned upstream revision of the embedded table, a content digest of any other), its date, and the number of entries it lists. For a table read from `price_table.replace_file`, `as_of` is when this process first loaded the file's current content, and a reload that reads unchanged content keeps it. For a downloaded table it is the time of the download, and a kept copy returns after a restart with its original download time.
    - `next_try_at` is the end of the armed wait before the next download try, `null` while a try is due or running or the download is not wanted. `failures` is the consecutive failed tries since the last accepted one, `0` otherwise. `failed_at` and `error` are the end and the error text of the latest failed try, or for `price_table.replace_file` the end and error of the latest workflow load, and are `null` unless the source is `failing`. A `price_table.replace_file` failure shows only while the configuration in effect names it; a failed load for another reason shows on Health and Workflow only. These fields live in memory, so a restart keeps a kept copy and its download time and drops the failures (Section 13.5).
  - `models` holds one row per agent kind and model, matched by exact string equality: every pair run history records, and every pair the configuration in effect names with a non-empty model, among them the top-level agent kind and the settings block of each dispatch rule. A pair with no recorded run has `last_used_at` `null` and `runs` `0`. A row holds `agent_kind`, `model`, `table_key`, `layer`, `entry_key`, `input`, `output`, `cache_read`, `cache_write`, `cost_status`, `unrated_classes`, `last_used_at`, and `runs`.
    - `table_key` is the key the model name maps to in the first table layer that lists it, `null` when none. `layer` is the layer that prices the model (`kind_entry`, `model_entry`, `operator_table`, `fetched`, or `embedded`), `null` when none, and `entry_key` is the `token_rates` key of a kind entry or model entry, `null` otherwise. A `layer` is for display only: runs keep the price source recorded with them (Section 13.5), which does not tell a `token_rates` entry from an operator table.
    - A class object holds `per_mtok`, the rate in USD per 1M tokens, `layer`, the layer that supplies it, and `from_input`. `per_mtok` is `null` and `layer` is `null` when no layer rates the class, so an unset rate never reads as zero, and `0` when a layer sets a rate of zero. A table rate is served to 10 significant digits. `from_input` is `true` for the cache rate of a kind entry that leaves it unset and prices it at its input rate.
    - `cost_status` is the status of the run the next dispatch would record for the model: `estimated` when every class is priced, `partial` when a class has no rate beside priced ones, `unpriced` when no class is priced. `unrated_classes` lists the classes with no rate (`input`, `output`, `cache_read`, `cache_write`), `[]` when none. The status and the classes are those the pricing of a run records, so the row cannot disagree with the price a run recorded under the same rates gets.
    - Rows with a last use come first, latest first, then rows without, and ties break by `agent_kind`, then `model`, in byte order. `models` is `[]` when there are none.
  - The route reads the rates the workflow manager holds, never the state snapshot's, because the snapshot takes new rates only on its next poll tick or worker exit. A successful workflow load, a `token_rates` edit, a landed download, and a failed try therefore show on the next request. The route caches nothing.
  - Every `error` passes the masking that the Health workflow check's error passes, and is `null` where the server has no masking. A request with no run history to read, or one whose read fails, answers `503` with `history_unavailable`, and any method other than `GET` answers `405`. A server without a rates source answers `200` with every row `unpriced`.
  - Suggested response shape, abridged to one row of a priced model and one of an unpriced one:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "table_in_force": {"layer": "fetched", "as_of": "2026-02-24T04:00:12Z", "age_days": 0},
      "token_rates": {"kind_entries": 0, "model_entries": 1, "version": "sha256:3b1f0c8d5a7e2f94b6c1d8e0a3f5b7c9d2e4f6a8b0c1d3e5f7a9b1c3d5e7f9a1"},
      "operator": null,
      "fetched": {
        "status": "loaded",
        "in_force": true,
        "off_by": null,
        "file": null,
        "address": "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json",
        "version": "sha256:8d2a6c4e0b1f3a5c7e9d1b3f5a7c9e0b2d4f6a8c0e1b3d5f7a9c1e3b5d7f9a0c",
        "as_of": "2026-02-24T04:00:12Z",
        "models": 2315,
        "next_try_at": "2026-02-25T04:00:12Z",
        "failures": 0,
        "failed_at": null,
        "error": null
      },
      "embedded": {
        "status": "loaded",
        "in_force": true,
        "off_by": null,
        "file": null,
        "address": null,
        "version": "9f3c2e1a7b4d5c6e8f0a1b2c3d4e5f6a7b8c9d0e",
        "as_of": "2026-02-20T11:42:07Z",
        "models": 2290,
        "next_try_at": null,
        "failures": 0,
        "failed_at": null,
        "error": null
      },
      "models": [
        {
          "agent_kind": "claude-code",
          "model": "claude-sonnet-4-5",
          "table_key": "claude-sonnet-4-5",
          "layer": "model_entry",
          "entry_key": "claude-sonnet-4-5",
          "input": {"per_mtok": 2.7, "layer": "model_entry", "from_input": false},
          "output": {"per_mtok": 15, "layer": "fetched", "from_input": false},
          "cache_read": {"per_mtok": 0.3, "layer": "fetched", "from_input": false},
          "cache_write": {"per_mtok": 3.75, "layer": "fetched", "from_input": false},
          "cost_status": "estimated",
          "unrated_classes": [],
          "last_used_at": "2026-02-24T19:58:41Z",
          "runs": 12
        },
        {
          "agent_kind": "codex",
          "model": "acme-1",
          "table_key": null,
          "layer": null,
          "entry_key": null,
          "input": {"per_mtok": null, "layer": null, "from_input": false},
          "output": {"per_mtok": null, "layer": null, "from_input": false},
          "cache_read": {"per_mtok": null, "layer": null, "from_input": false},
          "cache_write": {"per_mtok": null, "layer": null, "from_input": false},
          "cost_status": "unpriced",
          "unrated_classes": ["input", "output", "cache_read", "cache_write"],
          "last_used_at": null,
          "runs": 0
        }
      ]
    }
    ```

- `GET /api/v1/<issue_identifier>`
  - Returns issue-specific runtime/debug details for the identified issue, including any information tracked that is useful for debugging.
  - Suggested response shape:

    ```json
    {
      "issue_identifier": "MT-649",
      "issue_id": "abc123",
      "status": "running",
      "status_text": "Fixing CI on #936, fix 1 of 2",
      "status_text_full": "Fixing CI on PR #936, fix 1 of 2.",
      "next_outcomes": [],
      "issue": {
        "title": "Fix the flaky login test",
        "url": "https://tracker.example.com/browse/MT-649",
        "issue_type": "Bug",
        "labels": ["bug", "backend"],
        "state": "In Progress"
      },
      "workspace": {
        "path": "/tmp/sortie_workspaces/MT-649"
      },
      "attempts": {
        "restart_count": 1,
        "current_retry_attempt": 2
      },
      "running": {
        "issue_title": "Fix the flaky login test",
        "issue_url": "https://tracker.example.com/browse/MT-649",
        "issue_labels": ["bug", "backend"],
        "issue_type": "Bug",
        "session_id": "thread-1-turn-1",
        "turn_count": 7,
        "state": "In Progress",
        "started_at": "2026-02-24T20:10:12Z",
        "dispatched_at": "2026-02-24T19:48:20Z",
        "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
        "last_event": "notification",
        "last_message": "Working on tests",
        "last_event_at": "2026-02-24T20:14:59Z",
        "quiet_at": "2026-02-24T20:17:29Z",
        "stall_at": "2026-02-24T20:19:59Z",
        "stall_text": null,
        "stall_text_full": null,
        "stall_off_text": null,
        "tokens": {
          "input_tokens": 1200,
          "output_tokens": 800,
          "total_tokens": 2000,
          "cache_read_tokens": 400,
          "cache_write_tokens": 100
        },
        "uncached_tokens": 1600,
        "rule_name": "implement",
        "stage_lineage": {
          "chain_id": "3c8v1n6w0z4y7t2s5r9q8p1mkd",
          "previous": "specify",
          "previous_outcome": "succeeded"
        },
        "stage_position": {
          "chain": ["specify", "implement"],
          "current": "implement",
          "hop_count": 1,
          "hop_ceiling": 10
        },
        "reaction_kind": "ci",
        "reaction_config_key": "ci_failure",
        "reaction_attempt": 1,
        "pull_request": {"number": 936, "repository": "acme/web"},
        "api_request_count": 3,
        "tokens_measured": true,
        "usage_arrival": "incremental",
        "usage_attribution": "per_model",
        "tokens_pending": false,
        "api_requests_measured": true,
        "used_sessions": 2,
        "budget_sessions": 5,
        "issue_tokens_completed": 53400,
        "unmeasured_sessions": 0,
        "used_tokens": 55000,
        "used_tokens_complete": true,
        "budget_tokens": 500000,
        "budget_usd": null,
        "estimated_cost_usd": 0.014595,
        "cost_status": "estimated",
        "issue_estimated_cost_usd": 0.235095,
        "issue_cost_status": "estimated",
        "status_text": "Fixing CI on #936, fix 1 of 2",
        "status_text_full": "Fixing CI on PR #936, fix 1 of 2."
      },
      "retry": null,
      "budget_exhausted": null,
      "parked": null,
      "pending_reactions": [],
      "between_stages": null,
      "history": {
        "runs": [
          {
            "run_id": 41,
            "issue_id": "abc123",
            "issue_identifier": "MT-649",
            "attempt": 1,
            "status": "succeeded",
            "error": null,
            "declared_outcome": null,
            "agent_text": null,
            "rule_name": "specify",
            "agent_kind": "claude-code",
            "started_at": "2026-02-24T19:48:20Z",
            "completed_at": "2026-02-24T20:05:31Z",
            "dispatched_at": "2026-02-24T19:48:20Z",
            "turns_completed": 6,
            "tokens": {
              "input_tokens": null,
              "output_tokens": null,
              "total_tokens": null,
              "cache_read_tokens": null,
              "cache_write_tokens": null
            },
            "uncached_tokens": null,
            "tokens_measured": false,
            "unaccounted_turns": 0,
            "estimated_cost_usd": null,
            "cost_status": "unmeasured",
            "model_usage": [],
            "chain_id": "3c8v1n6w0z4y7t2s5r9q8p1mkd",
            "stage_target": "implement",
            "stage_result": "advanced",
            "self_review": null,
            "origin_recorded": true,
            "trigger": "",
            "reaction_kind": "",
            "reaction_config_key": "",
            "session_id": "thread-1-turn-1",
            "dispatch_id": "NTGPSQBPYLYSE2ULN22IDBX7HX",
            "ssh_host": "",
            "stage_chain": ["specify", "implement"],
            "issue_outcome": null
          }
        ],
        "next_cursor": null,
        "totals": {
          "run_count": 1,
          "unmeasured_runs": 1,
          "tokens": {
            "input_tokens": null,
            "output_tokens": null,
            "total_tokens": null,
            "cache_read_tokens": null,
            "cache_write_tokens": null
          },
          "uncached_tokens": null,
          "tokens_complete": false,
          "estimated_cost_usd": null,
          "cost_status": "unmeasured"
        },
        "hops": [
          {
            "run_id": 41,
            "chain_id": "3c8v1n6w0z4y7t2s5r9q8p1mkd",
            "source_rule": "specify",
            "target_rule": "implement",
            "result": "advanced",
            "completed_at": "2026-02-24T20:05:31Z"
          }
        ],
        "last_stage": {
          "chain": ["specify", "implement"],
          "rule": "specify"
        },
        "issue_outcome": null
      },
      "recent_events": [
        {
          "seq": 41,
          "at": "2026-02-24T20:14:31Z",
          "event": "tool_result",
          "message": "",
          "text": "Ran go test ./... in 2.4s.",
          "tool_line": "go test ./...",
          "tool_name": "Bash",
          "tool_kind": "shell",
          "tool_argument": "go test ./...",
          "duration_ms": 2400,
          "tool_error": false
        },
        {
          "seq": 42,
          "at": "2026-02-24T20:14:52Z",
          "event": "turn_completed",
          "message": "",
          "text": "Turn completed.",
          "usage": {
            "input_tokens": 1200,
            "output_tokens": 800,
            "total_tokens": 2000,
            "cache_read_tokens": 400,
            "cache_write_tokens": 100
          }
        },
        {
          "seq": 43,
          "at": "2026-02-24T20:14:59Z",
          "event": "notification",
          "message": "Working on tests",
          "text": "Working on tests"
        }
      ],
      "last_error": null,
      "tracked": {}
    }
    ```

  - `status` is `"running"` when the issue has a running session, otherwise `"retrying"` when it has a pending retry, otherwise `"budget_exhausted"` when it is held out of dispatch by a per-issue budget ceiling, otherwise `"parked"` when it is parked, otherwise `"pending_reactions"` when it is known only through a pending reaction, otherwise `"between_stages"` when it is known only through a stage hop whose next stage is not yet dispatched, otherwise `"history"` when it has run history and no live entry; `budget_exhausted` in the response body carries the same record shape as the `budget_exhausted` array on `GET /api/v1/state` and is `null` when the issue is not in that set, and `parked` carries the record shape of the `parked` array and is `null` when the issue is not parked. `between_stages` carries the row shape of the `between_stages` array and is `null` when the issue is not between stages, whatever the status. `pending_reactions` is always present and holds the issue's rows in the record shape and order of the `pending_reactions` array, `[]` when the issue has none, whatever the status. An issue that is only parked, known only through a pending reaction, only between stages, or only through run history answers with a `null` workspace, zero attempts, and a `null` `last_error`, as a budget-held one does. A `history` response carries `[]` for `pending_reactions` and `null` for every other live member. `status_text` and `status_text_full` on the response are the pair of the row that decides `status`; for `pending_reactions` they are the pair of the first served row whose `escalated_at` is non-null, else of the review row awaiting approval when every other row reports nothing outstanding (Section 13.7.2), else of the first served row, and for `history` they word how the issue ended from its outcome and the newest run's hop decision, and say so when Sortie recorded no ending. `last_error` is the retry row's error, so after the retry timer reschedules a retry it is the reschedule reason.
  - `history` is present on every `200` response: `null` when the issue has no run, and otherwise the issue's newest runs in the row shape of `GET /api/v1/runs`, newest first, beside live detail when the issue is also live. A running issue with earlier runs serves both. It holds `runs`, `next_cursor`, `totals`, `hops`, `last_stage`, and `issue_outcome`. `runs` is bounded to a page, because an issue looping on retries has no bound on its runs. `next_cursor` is `null` on the last page and otherwise continues on `GET /api/v1/runs?issue_id=<issue_id>`, which selects exactly the rows the page was cut from. `totals` covers all of the issue's runs, not the page. `hops` holds the hop decisions of the runs in `runs`, newest first; an older run's decision arrives with its row on the pages `next_cursor` continues. `last_stage` is the stage chain the issue's last stage run stood on, or `null`. `issue_outcome` is how the issue's latest work ended, the object every run in `runs` carries, or `null`. The API design notes give the keys and how to read them.
  - `recent_events` is present on every `200` response: the newest 50 retained events of one run, oldest first, in the event shape of Section 13.9. The run is the issue's running run, the one `running.dispatch_id` names; else the issue's newest run in run history that carries a dispatch ID, so the recorded CI result, which has none, is passed over. It is `[]` when no such run exists or the run's events are not retained. The route is refetched up to once per second, so it carries 50 events and not the whole buffer; `GET /api/v1/runs/{dispatch_id}/events` serves every retained event of the run, and the change stream serves them live.
  - Resolution runs in this order. The orchestrator state is read first, then run history, so a run that ends between the two reads can appear both as the running session and as the newest run, where the opposite order could hide it from both. An identifier that no live entry holds resolves through run history: the issue ID of the newest run recorded under that identifier is looked up in live state, so an identifier the issue no longer carries (a tracker move keeps the issue ID) answers with the live detail of the issue under the identifier it carries now. `issue_identifier` is the identifier of the entry that decides `status`, and on a `history` response that of the newest run. History is scoped by issue ID, so runs recorded under an older identifier stay on the issue's page.
  - `issue` is present on every `200` response and holds the issue's tracker facts under the names the prompt template's `issue` variable uses: `title`, `url`, `issue_type`, `labels`, and `state`. It is built from the entry that decides `status`, and each member is omitted when that entry holds none. For `running`, `retrying`, and `budget_exhausted` it carries the row's `issue_title`, `issue_url`, `issue_type`, `issue_labels`, and `state`; `labels` is `[]` when the row holds labels and the issue carries none, and absent while the row holds no read of them. For `parked` it carries the record's title, link, type, labels, and `parked_state` as `state`. For `pending_reactions` it carries the same members of the issue's first row, and for `between_stages` those of the issue's row. For `history` it carries the title and link of the issue's newest run, each omitted when empty.
  - `next_outcomes` is present on every `200` response: a list of condition and sentence pairs that say what Sortie does next for the issue, `[]` when no pair applies, a `history` response included. The API design notes give the rules.
  - If neither the current in-memory state nor run history knows the identifier, return `404` with an error response (for example `{"error":{"code":"issue_not_found","message":"..."}}`); an issue held by a budget ceiling, parked, between stages, known through a pending reaction, or known through run history is not unknown, so this case is distinct from those.
  - `pr_loop` is present on every `200` response and says what each configured reaction recorded in one pass: for a live issue the pass of the row that decides `status`, for a `history` issue the pass of its newest run. It is `null` when the server holds no run history or no workflow shape. The shape, the results, and the recorded-pass rule are those of the `pr_loop` bullet of this section.
  - A run-history read that fails, the read of the issue's outcome and of reaction activity included, answers `503` with `history_unavailable`, for a live and for a non-live identifier alike, never `history` `null` or `404`. A server without run history serves live detail with `history` `null` and answers `404` for an identifier no live entry holds.

- `POST /api/v1/refresh`
  - Queues an immediate tracker poll + reconciliation cycle (best-effort trigger; implementations may coalesce repeated requests).
  - Suggested request body: empty body or `{}`.
  - Suggested response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "coalesced": false,
      "requested_at": "2026-02-24T20:15:30Z",
      "operations": ["poll", "reconcile"],
      "outcome_text": "Tracker poll and reconciliation queued."
    }
    ```

  - Every response, a refusal during shutdown included, carries `outcome_text`: one sentence that states what the response confirms, a poll and reconciliation that was queued, one that was already queued and that this request joined, or that nothing was queued because Sortie is shutting down. It never states an effect still to come.

- `POST /api/v1/retries/{identifier}/run`
  - Makes the scheduled retry of an issue due now, so the retry runs without waiting out its delay. `{identifier}` is the retry row's `issue_identifier`, one path segment that a client percent-encodes, matched exactly. The route ignores the request body and the query string.
  - The retry takes the path of a timer that fired. The event loop re-arms the retry's entry in place with a zero delay and changes nothing outside its schedule, and the fire passes every check a timer's fire passes: the budget gates, the park and absence gates, the tracker re-read, the paused-retry rules, and the capacity checks. A retry the issue's state pauses stays paused. The one gate the fire skips is a usage-limit hold on the retry's agent kind, so a request starts a refused issue's retry before the reset, as the early probe of the provider's allowance; the instance spend gate still holds it. When the fire cannot start the retry, the put-back keeps the retry's attempt number and the due time it had before the request, or makes it due at once when that time has passed, under the put-back's own reason (§8.4). A request never leaves a retry due later, or at a higher attempt number, than before it.
  - A retry whose row the store keeps has the row saved with the new due time, so a restart before the fire restores the retry past due and fires it at startup. A reaction retry keeps no row. A failed save is logged and does not change the answer.
  - The response waits for the event loop to take the request, with no fixed bound: it arrives at once while the loop is idle, and at the end of the iteration in progress otherwise. The wait ends when the loop takes the request, when the client disconnects, or when shutdown begins. A request withdrawn by a disconnect or by shutdown changes nothing.
  - Response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "requested_at": "2026-02-24T20:15:30Z",
      "issue_id": "def456",
      "issue_identifier": "MT-650",
      "attempt": 3,
      "outcome_text": "Retry 3 of MT-650 is due now."
    }
    ```

  - The `202` also carries `display_identifier` when the issue has one. `issue_id`, `issue_identifier`, `display_identifier`, and `attempt` are the facts `outcome_text` is built from.
  - `404 Not Found` with code `retry_not_found` answers an identifier that no scheduled retry carries: an unknown issue, an issue with no queued retry, and a retry that ran or was released before the loop took the request.
  - `409 Conflict` answers a request made while Sortie shuts down, or one the loop had not yet taken when shutdown began. Its body holds `queued` false, `requested_at`, and `outcome_text`, and nothing changed.
  - Every `202` and `409` response carries `outcome_text`: one sentence that states what the response confirms, a retry due now, or that nothing was queued because Sortie is shutting down. It never states an effect still to come, because whether the retry starts is decided by the fire after the response.
  - Sortie's HTTP server has no authentication, so anyone who can reach it can run a retry early. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

- `POST /api/v1/attention/{id}/acknowledge`
  - Records that a person has seen an attention item. The item leaves `counts.attention` and stays in the `attention` array of `GET /api/v1/state` with `acknowledged_at` set, so a client moves it out of the main list and still shows when it was acknowledged. `{id}` is the item's `id`, one path segment, matched exactly against the items the state derives when Sortie takes the request. The route ignores the request body and the query string. Every item can be acknowledged, whatever its `action.kind`.
  - The event loop checks that the item is current and records the acknowledgement in one iteration, so no poll tick clears the item between the check and the write. Sortie saves the acknowledgement to the store before it changes the state it serves, so every `200` survives a restart. The request makes no call to the tracker, the forge, or an agent. It changes nothing in the tracker or the pull request, and nothing in Sortie but the acknowledgement: it releases no park, lifts no hold, ends no escalation, and leaves every retry as it was.
  - The response waits for the event loop to take the request, with no fixed bound: it arrives at once while the loop is idle, and at the end of the iteration in progress otherwise. The wait ends when the loop takes the request, when the client disconnects, or when shutdown begins. A request withdrawn by a disconnect or by shutdown changes nothing.
  - Response (`200 OK`) shape:

    ```json
    {
      "acknowledged": true,
      "already_acknowledged": false,
      "requested_at": "2026-02-24T20:13:05Z",
      "id": "464f9ff8ea54c4c6cfb43af394234d57",
      "kind": "budget_held",
      "issue_id": "ghi789",
      "issue_identifier": "MT-651",
      "acknowledged_at": "2026-02-24T20:13:05Z",
      "outcome_text": "Acknowledged MT-651 (Session budget reached). It comes back only if a new cause appears."
    }
    ```

  - The `200` also carries `display_identifier` when the issue has one. An item of a cause that concerns the process has no issue, so its response omits every issue key. `id`, `kind`, and the issue keys are the facts `outcome_text` is built from.
  - A repeat for an item that is already acknowledged answers `200` with `already_acknowledged` true and `acknowledged_at` set to the first acknowledgement. It changes nothing, keeps the first time, and emits no change signal on `GET /api/v1/events`. Two requests for one item reach the event loop one after the other: the first records, and the second is the repeat.
  - The route answers `200` and not `202`, because the acknowledgement is recorded and saved when the response is written.
  - `404 Not Found` with code `attention_item_not_found` answers an `id` that no current item carries: an unknown `id`, and an item that cleared before the loop took the request. The client refetches the state, and the item is gone.
  - `409 Conflict` answers a request made while Sortie shuts down, or one the loop had not yet taken when shutdown began. Its body holds `acknowledged` false, `already_acknowledged` false, `requested_at`, and `outcome_text`, and nothing changed.
  - `500 Internal Server Error` with code `acknowledgement_not_saved` answers a request whose acknowledgement the store did not save. Nothing changed, the item stays unacknowledged, and the person acknowledges again.
  - Every `200` and `409` response carries `outcome_text`: one sentence that states what the response confirms, an item acknowledged, an item that was already acknowledged, or that nothing was acknowledged because Sortie is shutting down. It names which item of the issue it concerns by the item's `reason_text`, and states the scope of what the response confirms, that the item comes back only if a new cause appears.
  - Sortie's HTTP server has no authentication, so anyone who can reach it can acknowledge an attention item and take it out of the attention count. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

- `POST /api/v1/reactions/{identifier}/{kind}/check`
  - Makes the next check of a pending pull request reaction due now, so the reaction does not wait out its schedule: the poll interval of its kind, a backoff, the end of a review debounce, or the growing delay of a CI check. `{identifier}` is the pending reaction row's `issue_identifier` and `{kind}` is its `kind`, not its `config_key`, because `label-review` and `label-fix` share one key. Each is one path segment that a client percent-encodes, matched exactly. The route ignores the request body and the query string.
  - The event loop moves the reaction's `next_poll_at` to the time it takes the request when that time is later, and changes nothing else about the reaction: its `wait`, its backoff count, the fixes counted against its ceiling, its escalation, and its last check stay as they were. A request for a reaction that is already due changes nothing, emits no change signal, and still answers `202`.
  - The request makes no call to the tracker, the forge, a CI provider, or an agent, starts no session, and schedules no retry. It changes nothing in the tracker or the pull request, and it stores nothing, because pending reactions are kept in memory and a reaction rebuilt at startup is due at once.
  - The check runs on the next poll tick, or on a refresh before it, because each runs the reconcile passes. The route runs no tick and dispatches no new candidate. The pass checks the reaction under every rule its kind has: the review debounce, which applies again after the pass reads the comments, the retry slot, the triage gate, the attempt ceilings and the escalation at the ceiling, the watch window, and the budget gates that apply to a fix the check schedules. What the check finds is handled as it would be at the reaction's due time: a fix dispatch, a merge, a label removal, an escalation, or a tracker transition.
  - The check the request brings forward is one more check. When it keeps the reaction pending and starts before the due time the request replaced, the reaction gets that due time back, and the lower of its backoff count and the count from before the request, in place of the instant and the count the check sets. The count is zero when the check found a new pull request head. The `wait` and `last_check` the check recorded stay. A check that starts at or after the replaced due time is one the reaction had without the request, so its schedule stands. A log record of a pass that states a delay states the one that pass computed, and `next_poll_at` serves the put-back. A request never leaves a reaction's next check later, or its backoff count higher, than without the request.
  - The response waits for the event loop to take the request, with no fixed bound: it arrives at once while the loop is idle, and at the end of the iteration in progress otherwise. The wait ends when the loop takes the request, when the client disconnects, or when shutdown begins. A request withdrawn by a disconnect or by shutdown changes nothing. A request that arrives while a tick checks the reaction applies when that iteration ends and moves the check that tick scheduled, so the reaction gets one more check on the next tick, on the schedule the earlier tick set. Further requests before that tick find the reaction due and change nothing, so the reaction is checked once per tick, however many requests arrive.
  - The route answers `202` and not `200`, because the check runs after the response is written.
  - Response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "requested_at": "2026-02-24T20:15:30Z",
      "issue_id": "mno345",
      "issue_identifier": "MT-653",
      "kind": "ci",
      "pull_request": {"number": 931, "repository": "acme/web"},
      "outcome_text": "MT-653: a check of PR #931 for CI is queued for the next poll tick."
    }
    ```

  - The `202` also carries `display_identifier` when the issue has one. `issue_id`, `issue_identifier`, `display_identifier`, `kind`, and `pull_request.number` are the facts `outcome_text` is built from. `pull_request.repository` is `owner/repo`, masked as the pending reaction row masks it, because a number alone does not name a pull request across repositories.
  - `404 Not Found` with code `reaction_not_found` answers an identifier and kind that no pending reaction carries: an unknown issue, a kind outside the list of kinds, an issue with no pending reaction of that kind, and a reaction that left the pending set before the loop took the request because its watch ended or its check dispatched a fix. The client refetches the state, which shows what became of the reaction. For a kind that issue has no pending reaction of, the body is `{"error": {"code": "reaction_not_found", "message": "no pending \"bot-review\" reaction for issue \"MT-653\""}}`.
  - `409 Conflict` answers a request made while Sortie shuts down, or one the loop had not yet taken when shutdown began. Its body holds `queued` false, `requested_at`, and `outcome_text`, and nothing changed.
  - Every `202` and `409` response carries `outcome_text`: one sentence that states what the response confirms, a check queued for the next poll tick, or that nothing was queued because Sortie is shutting down. It never states what the check finds or does, because the pass decides that after the response.
  - Sortie's HTTP server has no authentication, so anyone who can reach it can make a pull request reaction check early. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

- `GET /api/v1/events`
  - Opens a server-sent event stream that signals each time anything the JSON API serves changes, so a client refetches what it shows instead of polling, and that carries the agent events of the runs the client subscribes to. A change signal is never a payload: it names no issue, agent message, or figure, and change signals expose nothing that `GET /api/v1/state` does not. An agent-event frame carries an event as the run-events route serves it, so the stream as a whole exposes nothing that the JSON API does not.
  - Answers `200` with `Content-Type: text/event-stream`, `Cache-Control: no-cache`, and `X-Accel-Buffering: no` so a default nginx does not buffer. The route reads the `run` query parameter and ignores every other one. A method other than `GET` answers `405` with `Allow: GET` and the JSON error envelope.
  - The change signal is one frame:

    ```
    id: <frame id>
    event: change
    data: {}

    ```

    The revision is an unsigned decimal counter that rises with every change. It starts from the wall-clock time at server start in nanoseconds, so a restarted process does not repeat the revisions of the previous one. `data` is the empty JSON object, because a browser dispatches no event whose data is empty; a client ignores members it does not know.
  - Every frame the stream writes, a change signal, a heartbeat, and each run frame below, carries the frame id: the revision, followed by `;<dispatch_id>:<seq>` for each subscribed run in the order of its `run` parameter, where `<seq>` is the run's cursor as the stream holds it: `first_seq` minus one when the run's last frame was a `cursor_expired` reset, else the `seq` of the run's last event the stream wrote, else the subscription's cursor (`0` without one) lowered to the run's newest `seq` when above it. The revision part is that of the last change signal the stream wrote or, when it wrote none, the revision part of the frame id the stream opened with, so a run frame never advances the revision a client has seen. A stream with no run subscribed carries the revision alone. A dispatch ID contains neither `;` nor `:`.
  - A signal follows each change after the change is applied, so a refetch made after a signal sees every change the signal counted. Signals come from every event-loop iteration that ends a tick, a refresh, a worker exit, a retry, a request that makes a retry due, a request that makes a pending reaction's check due, or a request that records an acknowledgement; from every message of a running agent session (an agent event, a turn start, a self-review progress report, or an unaccounted-turn count); from the start of shutdown; and from every load of the workflow file by the watcher, whether it succeeds or fails. Values that move with the clock alone change without a signal: `agent_totals.seconds_running`, the elapsed times a client computes from `started_at`, and the window of a relative `since` on `GET /api/v1/stats`.
  - A stream writes at most one change signal per second. The first change after a quiet second is written at once; the changes made during a second are written together as one signal when it ends, and none is dropped. Two consecutive signals on one stream never carry the same `id`.
  - Reconnect is decided by exact string equality of the revision part of the `Last-Event-ID` request header, the text before its first `;`, with the current revision, never by ordering. An equal value opens a stream with no signal until the next change. An absent header, an earlier revision, a revision of an earlier process, or any other string gets one signal at once.
  - A client subscribes to the agent events of a run with `run=<dispatch_id>` or `run=<dispatch_id>:<seq>`, repeatable, at most 16 per connection. Without `<seq>` the stream replays every retained event of the run; with it, replay starts after that `seq`. The cursor is a query parameter because `EventSource` cannot set a header on its first connection. A dispatch ID has 1 to 64 characters, each from `A` to `Z` or `2` to `7`, and a `seq` has 1 to 19 decimal digits and fits a signed 64-bit integer; an empty `run` value is invalid. A `run` value that breaks that form, a dispatch ID given twice, and more than 16 `run` parameters answer `400` with the error code `invalid_parameter` and open no stream. The message names the broken rule and echoes no input.
  - On reconnect the cursors of `Last-Event-ID` take precedence over those of the query: for each run the `run` parameters name, a cursor the header carries for that dispatch ID replaces the query's cursor. The query alone decides which runs are subscribed, and a header cursor for a run the query does not name is ignored. A header part after the first `;` that is not `<dispatch_id>:<seq>` in the form above is ignored, the first part for a run counts, and the header never causes a `400`. The cursors so resolved are those of the frame id the stream opens with.
  - A subscribed run gets these frames, beside the change signals:

    ```
    id: <frame id>
    event: agent_event
    data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","seq":43,"at":"2026-02-24T20:14:59Z","event":"notification","message":"Working on tests","text":"Working on tests"}

    id: <frame id>
    event: run_ended
    data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","last_seq":43}

    id: <frame id>
    event: reset
    data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","reason":"cursor_expired","first_seq":12}

    ```

    - `agent_event` carries one retained event: the run's `dispatch_id` and the members of the event shape of Section 13.9.
    - `run_ended` follows the run's last event, once the run has ended and the stream has written every retained event after the cursor. `last_seq` is the `seq` of the run's newest retained event. The stream drops the subscription and keeps the run's final cursor in the frame id, so a reconnect with the same query replays nothing and gets `run_ended` again.
    - `reset` with `reason` `cursor_expired` says the cursor fell behind retention: the events between the cursor and `first_seq` were evicted. The same stream then writes every retained event of the run from `first_seq`, and the reset's frame id carries `first_seq` minus one as the run's cursor. A client keeps what it holds and marks the gap; it fetches nothing. A cursor above the run's newest `seq` reads as that `seq`, and a subscription without a cursor to a run whose oldest retained `seq` is above 1 starts with this reset, so the gap is said in the stream. Every read of a run, the first and each later one, checks the cursor against retention, so eviction that overtakes a slow stream also yields this reset.
    - `reset` with `reason` `not_retained` says the run's events are not retained, by the rule of the run-events route's `404`. It carries no `first_seq`, the stream drops the subscription, the run leaves the frame id, and no other frame of the run follows.
    - A stored copy that cannot be read writes nothing for its run and never a reset. The stream keeps the run's cursor in the frame id, reads the copy again at its next heartbeat, and continues with its other runs and its change signals meanwhile; the run-events route answers that run `503` with `history_unavailable`.
  - Run frames are never coalesced and are written only for subscribed runs, as soon as the stream is woken, outside the one-per-second bound of change signals, in `seq` order within a run. Subscriptions leave change signals as they are: payload-free, coalesced, at most one per second, and produced by every agent event.
  - A heartbeat is written every 15 seconds, counted from the stream's start.
  - The server's per-request write timeout does not end a stream. Each frame has its own write deadline of 2 seconds, set before its write. Run frames written together are flushed once, the flush under its own deadline, and the deadline is cleared once the stream has nothing more to write, so an idle stream holds none and the deadline measures a client that stopped reading, never the length of a catch-up. A client that stops reading is dropped once a write cannot complete within 2 seconds; until then it holds one pending change at most and no queue of agent events, and slows no other client and not the event loop. A client that keeps reading is never dropped for the length of a catch-up (Section 13.9).
  - Shutdown ends every open stream as soon as it begins, also one in the middle of writing run frames, which ends before its next frame, and the connection stops counting as active, so graceful shutdown does not wait for a stream. A stream opened during shutdown answers `200` with headers only and ends. The route never answers `503`, because a browser does not reconnect after a non-`200` answer. Browser reconnects to the restarting process reach a closed listener and are refused until the restarted process listens; behind a reverse proxy they get `502`, which closes the stream for good.
  - Sortie's HTTP server has no authentication, so anyone who can reach it can read the agent events of every retained run. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.
  - Reconnect with backoff: a client whose stream is closed (`EventSource.readyState` is `CLOSED` after an `error` event, as a non-`200` answer or a wrong `Content-Type` leaves it) opens a new stream after a delay that doubles from 1 second up to 30 seconds while new streams keep failing, and returns to 1 second once one opens. A new stream carries no `Last-Event-ID`, so its opening change signal makes the client refetch, and the client writes the last `seq` it holds of each run into that run's `run` parameter, so no event is replayed twice or missed.
  - The heartbeat is an event frame:

    ```
    id: <last>
    event: heartbeat
    data: {}

    ```

    `<last>` is the frame id as of the heartbeat: the revision part of the last change signal the stream wrote, or of the frame id it opened with, and the current cursor of every subscribed run. A heartbeat without an `id` line would clear the cursors the reconnect rule reads. `data` is the empty JSON object. A browser dispatches the heartbeat only to `heartbeat` listeners, never to `message` or `change` listeners. On a healthy stream at most 16 seconds pass between two frames.
  - Idle timeout: a client whose open stream delivers no event, change signal or heartbeat, for 45 seconds closes it and opens a new one with the same backoff. This recovers a stream that died without the browser receiving a FIN or RST, for example a laptop that wakes holding a connection the server dropped while it slept.

API design notes:

- The JSON shapes above are the recommended baseline for interoperability and debugging ergonomics.
- Implementations may add fields, but should avoid breaking existing fields within a version.
- Running, retry, budget-exhausted, parked, pending reaction, and between-stages rows name their issue in flat keys beside `issue_id`, `issue_identifier`, and `display_identifier`, never in a nested object, and the same rows inside `GET /api/v1/<issue_identifier>` carry them. `issue_title` and `issue_url` are the tracker's title and web link, each omitted when empty; a running row always carries a title, and a retry row lacks it only while the retry holds no read of its issue, a parked row only while the park holds none, and a between-stages row only when the hopping run read none. `issue_type` is the tracker's issue type, omitted when empty or unread, on every kind of row. `state` is the tracker state as read, omitted when empty or unread, on every kind of row; a parked row's `state` equals its `parked_state`. `issue_labels` is on every kind of row. It is always present on a running row, lowercase and in the tracker's order. On every other row it is `[]` when the issue was read and carries none, and absent while the row holds no read of the labels, so a client never reads an unread issue as one without labels. Every such string is masked as Section 15.3 states.
- On a running row, `api_request_count` is `null` exactly when `api_requests_measured` is false, the members of `tokens` and `uncached_tokens` are `null` exactly when `tokens_measured` is false, and `requests_by_model` is absent on the first condition and when the attribution names no model. `skills` is absent on a running row until the run has completed a skill call.
- Sortie deviates from the field-stability note above for those figures, and for the `budget_tokens`, `budget_sessions`, and `used_tokens` keys of a budget-exhausted record, narrowing each from an integer to a nullable one, because a consumer reading a number cannot tell a measured zero from an unmeasured one. A typed consumer is forced to handle the null; an untyped one, in a language where `null` coerces to `0` in arithmetic, is no worse off than it was.
- Sortie also deviates from the field-stability note above for the issue's token spend. The key `issue_uncached_tokens` is removed, because `used_tokens` now carries its figure. The keys `issue_tokens_completed` and `used_tokens` of a running row, and `used_tokens` of a budget-exhausted record and of a `budget_held` attention entry, no longer count prompt-cache reads, so a consumer comparing them with a total taken from the agent's runtime sees a lower number than before.
- A running row, and the `running` object of the per-issue response, which serves the same keys, carries the issue's spend and limits in flat keys.
  - `used_sessions` counts the issue's completed sessions: the run-history rows that `agent.max_sessions` is compared with. The running session is not counted, and a recorded CI result counts as a session. `unmeasured_sessions` counts the completed sessions whose usage was never recorded, which the token figures leave out.
  - `issue_tokens_completed` is the summed uncached tokens of the completed sessions, the baseline the in-flight ceiling adds the running session's spend to. `used_tokens` is `issue_tokens_completed` plus the row's `uncached_tokens`, each counted as `0` when `null`: the figure the ceiling compares with `agent.max_tokens`.
  - `uncached_tokens` is the row's `tokens.total_tokens` less its `tokens.cache_read_tokens`, floored at zero.
  - `used_tokens_complete` is true exactly when `used_tokens` is the issue's whole spend: the completed sessions were read, none is unmeasured, none recorded an unaccounted turn, the running session is measured, and it has recorded no unaccounted turn. When it is false, `used_tokens` is a lower bound. It ignores `tokens_pending`, which keeps telling a consumer that the figure excludes a turn in flight.
  - `budget_tokens`, `budget_sessions`, and `budget_usd` are `agent.max_tokens`, `agent.max_sessions`, and `agent.max_cost_usd` as of the latest poll tick. A limit key whose limit is not configured serves `null`, present and never `0` or omitted, because `0` computes as a ceiling of zero. The budget-exhausted record encodes all three keys the same way. `budget_usd` is compared with the issue's money figure, `issue_estimated_cost_usd`, which is complete exactly when `issue_cost_status` is `estimated`.
  - `null` on a limit key means no ceiling; `null` on a usage key (`used_sessions`, `unmeasured_sessions`, `issue_tokens_completed`, `used_tokens`) means unknown, and it is `null` for the first three when no read of the issue's run history has succeeded for the run. Headroom is a limit key minus its usage key (`budget_tokens` minus `used_tokens`, `budget_sessions` minus `used_sessions`) and has no value when either is `null`. Token headroom turns negative when a run overshoots, because the ceiling stops a run at the first usage figure at or past it; the API serves no clamped headroom key.
  - A key never serves a sum of `0` that leaves spend out. `issue_tokens_completed` and `used_tokens` serve `null` instead, so a completed session that never reported usage, or a measured zero beside unknown spend, never reads as a spend of zero. A completed session is measured when it reported a usage figure or never entered an agent turn; a measured zero is exact and `issue_tokens_completed` serves it.
  - `estimated_cost_usd` is the running session's live estimate under the rates in force (Section 13.5), and `cost_status` says why it is absent or a lower bound: `estimated`, `partial` when a model or a token class the session spent has no rate beside priced spend, `unpriced` when the session is measured and no rate prices any of its spend, or `unmeasured` when the session has reported no usage. `issue_estimated_cost_usd` sums the estimates of the issue's priced parts, completed sessions included, and `issue_cost_status` is `estimated` when the sum covers the whole spend, `partial` when it is a lower bound because a part that spent tokens is left out, `unpriced` when no priced part spent tokens and a part that did has no rate, and `unmeasured` when `used_tokens` is `null` or no part is priced or unpriced. A running session that has reported zero tokens leaves nothing out, so a missing rate for its model does not make a complete, otherwise priced issue `partial`.
  - Estimates are unrounded USD. A completed session contributes the cost recorded with its run, and the running session its live estimate under the rates in force. `used_tokens` is `null` only when `issue_cost_status` is `unmeasured`, and `issue_cost_status` is `estimated` only when `used_tokens_complete` is true.
  - A budget-exhausted record also carries `stopped_in_flight`, the count of the issue's sessions a budget ceiling stopped mid-run, money stops included. `stopped_in_flight` and `unmeasured_sessions` are `null` exactly when no ceiling read the issue's usage. `used_tokens` is `null` then too, and also when its sum is `0` while `unmeasured_sessions` or an unaccounted turn leaves spend out.
  - A budget-exhausted record carries the money figures `used_usd`, `used_usd_complete`, and `budget_usd`. `used_usd` is the issue's estimated cost in unrounded USD (Section 13.5), `used_usd_complete` is true exactly when it is the issue's whole spend and false when it is a lower bound, and `budget_usd` is `agent.max_cost_usd`. `used_usd` and `used_usd_complete` are `null` when the money ceiling did not read the issue, and `budget_usd` is `null` when no money ceiling is set. All three keys are always present. A hold under another reason serves them too whenever a money ceiling is set and read the issue, so an enriched token or session hold states the money line.
- A retry row, on `GET /api/v1/state` and in the `retry` object of the per-issue response, says why the retry waits in these keys. They are additive; no existing key changes name or type.
  - `trigger` is the `sortie_retries_total` label of the schedule that created the row: `error`, `usage_limit`, `continuation`, `timer`, `stall`, or `ci_fix`. It is omitted when that schedule records none: a review, bot review, merge conflict, or label command continuation, which `reaction_kind` names.
  - `reaction_kind` is the retry's reaction kind, omitted when it carries none. An `error` or `stall` retry of a run a reaction dispatched carries that run's kind, and a `timer` row keeps the kind of the retry it replaced. `reaction_config_key` is present exactly when `reaction_kind` is, and names the `reactions` key of WORKFLOW.md the kind is configured under, so a client holds no mapping of its own: `ci` is `ci_failure`, `review` is `review_comments`, `bot-review` is `bot_review`, `merge` is `auto_merge`, `merge-conflict` is `merge_conflicts`, `label-review` and `label-fix` are `label_commands`, and `merge-completion` is `merge_completion`.
  - `rule_name` is the dispatch rule frozen with the retry, `default` for `dispatch.default`, omitted when the workflow-wide fallback routed the run. When the configuration in force no longer reaches that selection, the retry dispatches on another rule (Section 8.4, "Retry selection").
  - `agent_kind` and `configured_model` are the agent kind of the run the retry follows and the model that run's resolved settings block carries, each omitted when empty. The orchestrator persists the model with the retry and restores it at startup.
  - `stage_lineage` is always present and `null` unless a stage hop reached the run the retry follows. The object holds `chain_id`, `previous`, and `previous_outcome`, each omitted when empty: the run's stage chain, the rule whose hop reached the run, and that hop's outcome, `succeeded` or `no_change`. A chain's first stage, which no hop reached, reports `null`. Running and budget-exhausted rows serve the key by the same rule; on a budget-exhausted record it describes the dispatch the hold blocks.
  - `paused` is always present, and true exactly when the retry timer last rescheduled the retry because its issue's state does not permit dispatching it, which is when `error` is `reaction retry paused by an open blocking issue` or `reaction retry paused by issue state`.
  - When the retry timer reschedules a retry, `error` carries the reason, one of `worker still running`, `retry issue fetch failed`, `issue without a state label`, `reaction retry paused by an open blocking issue`, `reaction retry paused by issue state`, `no available orchestrator slots`, `no available SSH hosts`, `retry agent kind unavailable`, or `retry agent settings refused`, and the trigger is `timer`. The error of the run before it stays in the logs and in that run's run history. The per-issue `last_error` copies `error`, so it carries the reason too.
- `dispatched_at` orders the rows that follow a dispatch. It is on every kind of row, and always present. The key is additive; no existing key changes name, type, or meaning.
  - It is when Sortie dispatched the first run of the issue's current pass, in UTC. A pass is the runs that share a stage chain identifier: a dispatch that no stage hop reached starts one, and the next stage of a hop, a retry, a reaction continuation, and a restart continue it. The value therefore does not change across those events, where a running row's `started_at` is the start of the session in flight. On a parked record it is the pass of the run the park follows, never the time of the park. On a pending reaction row it is the pass of the run the reaction follows, and on a between-stages row the pass of the run that made the hop, which the next stage's run continues.
  - It is never `null` on a running row. It is `null` on a row whose pass the orchestrator cannot date: a row the upgrade that introduced the value could not date from run history, a between-stages record that holds no pass time, a budget-exhausted record that holds no dispatch context, and a parked record that records no run.
  - `active` is the stable order the state route serves, so a client that shows one line per issue keeps that order. A client that orders the rows of one list sorts by `dispatched_at`, a `null` before every timestamp, and breaks ties by `issue_identifier`, then `issue_id`, because rows dispatched in one tick can share a value.
- `stage_position` places a row's dispatch rule on its stage chain. It is on every kind of row and always present. The key is additive; no existing key changes name, type, or meaning.
  - It is `null` exactly when the row's rule is on no stage chain, so a workflow without stage chains serves `null` on every row. Otherwise it holds `chain` (the chain's rule names, equal to a member of `config.dispatch.chains`), `current` (the row's rule, a chain's first rule included), `hop_count` (the issue's consecutive automatic hops, `0` when the issue has no hop record), and `hop_ceiling` (the limit a worker exit compares, as of the latest poll tick).
  - `chain` is the first chain that holds the rule and, when the row's `stage_lineage` names a previous rule, holds that rule immediately before it; when no chain does, it is the first chain that holds the rule. An issue on a chain's first stage therefore serves one position while it runs and while it retries.
  - A parked record and a pending reaction row place the rule of the run they follow, choosing the chain with that run's previous rule, and a parked record that records no run serves `null`. A between-stages row places `target_rule`, choosing the chain with `source_rule` as the previous rule, so it serves the position the next stage's running row serves.
  - `chain` and `hop_ceiling` follow the configuration as of the latest poll tick, as a running row's `budget_tokens` does, while `rule_name` and `stage_lineage` stay frozen with the selection. After a reload removes the rule from every chain, the row serves `null` and keeps its `rule_name`.
  - `stage_position` and `stage_lineage` keep separate rules. `stage_lineage` is `null` unless a stage hop reached the run, whichever kind of row serves it, so an issue on a chain's first stage serves `stage_lineage` `null` beside a `stage_position` object.
- A running row that is a reaction continuation names the reaction and its pull request in these keys. They are additive; no existing key changes name, type, or meaning, and a session that is no continuation carries none of them.
  - `reaction_kind` is `ci`, `review`, `bot-review`, `merge-conflict`, `label-review`, or `label-fix`. `reaction_config_key` is present exactly when `reaction_kind` is, and names the `reactions` key of WORKFLOW.md by the mapping a retry row's `reaction_config_key` uses.
  - `reaction_attempt` is which try of its reaction the session is: the reaction's count once its pass counted this continuation, which is the value a pending reaction row serves as `used_attempts`. It is present for `ci`, `review`, `bot-review`, and `merge-conflict`, and omitted for `label-review` and `label-fix`, which count nothing.
  - `pull_request` is present exactly when `reaction_kind` is. It holds `number` and `repository` (`owner/repo`, masked), named as on a pending reaction row.
- The `issue` object of the per-issue response gives a client the issue's facts without choosing a row to read them from. It is additive; no existing key changes name, type, or meaning.
  - It is present on every `200` response, whatever `status` says, and an empty object when the deciding entry holds none of its members.
  - `title`, `url`, `issue_type`, `labels`, and `state` are the members, each omitted when its source holds none and masked. The members take the names of the prompt template's `issue` variable, so a client that renders the template's view of an issue reads the same keys.
  - A running, retry, or budget-exhausted row supplies all five, and so do a parked record (its `state` is its `parked_state`), the first pending reaction row of the issue, and the issue's between-stages row. The response is built from the snapshot and makes no tracker call.
- A retry or budget-exhausted row serves the issue's type, labels, and state beside its title and link, as read when the retry was scheduled or the poll tick last rebuilt the set. The orchestrator persists them with a retry and restores them at startup, so a restart serves what was served before it. A retry whose read holds a title or link and no labels takes the type, labels, and state of its next read and keeps the title and link it holds; a title or link already held is never replaced. A retry a pending reaction schedules carries the reaction's issue facts, so its `state` is the handoff target after that reaction's handoff, and so does a budget-exhausted record the retry lane makes from that retry.
- A budget-exhausted record describes the dispatch the hold blocks in `rule_name`, `stage_lineage`, `stage_position`, `agent_kind`, `configured_model`, and `dispatched_at`, with the meanings a retry row gives them. The keys are additive; no existing key changes name, type, or meaning. `rule_name` is `default` for `dispatch.default` and omitted for the workflow-wide fallback. A record holds a dispatch context exactly when `agent_kind` is present; while it holds none, `rule_name`, `agent_kind`, and `configured_model` are omitted and `stage_lineage`, `stage_position`, and `dispatched_at` are `null`. Section 13.3 gives where the context comes from.
- A parked record, on the `parked` array and in the `parked` object of the per-issue response, carries these keys.
  - `issue_id` and `issue_identifier` are always present. `display_identifier` is omitted when the tracker needs no qualified form.
  - `issue_title` and `issue_url` are the tracker's title and web link as read when the park was recorded or, for a park recorded or restored without a read, at the first poll tick on which the tracker answers for the issue. Each is omitted when empty and masked.
  - `issue_type`, `issue_labels`, and `state` are the issue's facts as read for the park, each omitted when empty or unread and masked, with the types and presence rules they have on a retry row. `state` equals `parked_state`. `issue_labels` leave out the parking label: a new park reads the issue before it writes the label, a park completed from a later read stores the labels less the label, and `label_applied` reports whether a read showed the label on the issue.
  - `rule_name`, `stage_position`, `agent_kind`, `configured_model`, and `dispatched_at` describe the run the park follows, with the meanings a retry row gives them. The run is the one that stopped for a park made at a worker exit or at the retry timer, and the issue's newest agent run in run history for a park the poll tick makes. The park persists the run and restores it at startup, so a restart serves what was served before it, and the store migration fills it for a park recorded before the upgrade from the newest agent run of the issue that started at or before the park. `rule_name` is `default` for `dispatch.default` and omitted for the workflow-wide fallback. A park holds a run exactly when `agent_kind` is present; while it holds none, because the poll tick's read of run history failed, `rule_name`, `agent_kind`, and `configured_model` are omitted and `stage_position` and `dispatched_at` are `null`. A park made before the upgrade for an issue with no agent run holds none either.
  - `reason` is `agent_blocked` or `handoff_absence`. `parked_state` is the tracker state observed when the park was recorded, or at the first observation after a park recorded before any read, omitted while unobserved and masked. `label` is the parking label the orchestrator applied, served unmasked because it is configuration, and `label_applied` is true once a read showed the label on the issue. `parked_at` is the time the park was recorded, in UTC.
  - The list is sorted by identifier and then issue ID, and `counts.parked` equals its length. An issue appears in every list whose set holds it: a parked issue with a review continuation queued is in `parked` and `retrying` both.
- A pending reaction row, on the `pending_reactions` array and in the `pending_reactions` array of the per-issue response, describes one pull request reaction the orchestrator keeps watching. The keys are additive; no existing key changes name, type, or meaning.
  - `issue_id` and `issue_identifier` are always present. `display_identifier` is omitted when the tracker needs no qualified form. `issue_title` and `issue_url` are the issue's title and link as the run that created the reaction read them, or as startup recovery restored them from run history; each is omitted when empty and masked.
  - `issue_type`, `issue_labels`, and `state` are the issue as the run that created the reaction read them, each omitted when empty or unread and masked, with the types and presence rules they have on a retry row. When that run's exit moved the issue to `tracker.handoff_state`, `state` is the handoff target, because the run's read is the active state the transition replaced; an exit whose handoff transition failed, or that made none, serves the run's read. A retry the reaction schedules carries the same facts. A reaction restored at startup serves none of them, because run history records the title and link only.
  - `rule_name`, `stage_position`, `agent_kind`, and `configured_model` describe the run the reaction follows, with the meanings a retry row gives them; a restored reaction takes them from the run history row it is rebuilt from. `stage_position` chooses its chain with the previous rule of the run's stage lineage.
  - `kind` is `ci`, `review`, `bot-review`, `merge`, `merge-conflict`, `label-review`, `label-fix`, or `merge-completion`. `config_key` is the `reactions` key of WORKFLOW.md the kind is configured under, as a retry row's `reaction_config_key` names it, so a client finds the reaction's settings at `config.reactions[config_key]` and holds no mapping of its own.
  - `pull_request` is always present. `number` is always present. `repository` is `owner/repo` as the pull request record the run left in its workspace holds them, masked. `url` is the pull request's web page as the provider reported it on the latest read of the pull request, omitted until a read reports one, and masked. A `review` reaction awaiting approval reads the pull request on every due pass, so the link follows the pull request for as long as the hold lasts. `branch` is the head branch that record holds for the run that created the reaction, omitted for `label-review` and `merge-completion`, which keep none, and masked. `head_sha` is the head commit that record holds for that run, omitted when none was recorded and for `label-review`, `label-fix`, and `merge-completion`, and masked; a later push does not change it, and the head a check read is `last_check.head_sha` on the kinds whose check reads one.
  - The link comes from the pull request read each source-control adapter already performs, and no adapter builds one from its configured endpoint, because a forge can serve its web pages from another host. A pass that does not otherwise read the pull request (`review`, `bot-review`, `label-review`, and `label-fix`) reads it once for an entry that holds no link and repeats the read on later due passes only while the link is missing. A failed read is logged at `Debug` and changes nothing else: no backoff, no wait reason, no counted try.
  - `dispatched_at` is the dispatch time of the pass of the run the reaction follows, as a running row's `dispatched_at` defines it. A reaction restored at startup takes it from the run history row it is rebuilt from, and it is `null` for a run the upgrade could not date.
  - `created_at` is when this process created the reaction: at the worker exit that seeded it, or at the startup that restored it. A retry-slot deferral leaves it unchanged; a watch window that a deferral restarts keeps its own mark.
  - `next_poll_at` is when the next check is due. Every arm of every pass schedules one instant, and the check runs on the first reconcile pass (poll tick or refresh) at or after it, so a value at or before `generated_at` means the next pass checks the reaction. A request to check the reaction now (`POST /api/v1/reactions/{identifier}/{kind}/check`) moves a later value to the time Sortie takes the request. The check that request brings forward, when it keeps the reaction pending and started before the moved value, gives that value back in place of the instant its arm schedules. The value is a due time, as a retry row's `due_at` is, and `health.poll.next_poll_at` gives the next poll tick. An entry no pass has taken yet is due at its creation time.
  - `wait` says why the reaction waits for that check, by the cause the pass that last returned it to the pending set chose, not by the delay formula: the CI pass grows its delay alike for a pending verdict and a fetch error, and only the cause tells them apart. It is omitted until a pass first returns the reaction, because a reaction no pass has taken has had no check and is due at once.
    - `poll_interval`: the pass completed its check and found nothing to act on yet, or acted and keeps watching; the kind checks again at its poll cadence.
    - `debounce`: for `review`, the check found new actionable comments within `debounce_ms` of the newest; `next_poll_at` ends the window, and the reaction dispatches then unless a newer comment arrives. A request to check the reaction now moves `next_poll_at`. The check that request brings forward applies the window again and gives `next_poll_at` back the window end from before the request, and a window that a newer comment extended is applied once more at that next check.
    - `backoff`: the pass could not complete its check, because a provider, tracker, or store call failed or the provider answered without an identifier the check needs.
    - `retry_slot`: the pass found work to dispatch while another retry holds the issue's retry slot; for `merge-completion`, while a run or retry claims the issue; for `label-review` and `label-fix`, a confirmed command that waits for a run of another kind to end.
    - `triage`: the reaction waits on its triage command, because a run for the current subject is in flight, was just started, or waits for a free run slot.
  - `used_attempts` is an integer counting what the kind counts, and `null` for `label-review` and `label-fix`, which count nothing. `max_attempts` is the ceiling the kind's pass applies as of the latest poll tick, and `null` when it applies none. A kind counts as follows.
    - `ci` counts CI-fix continuations dispatched since a passing verdict, or a head change attributed to someone else, last reset the count. The ceiling is `reactions.ci_failure.max_retries` and the next failing verdict at the ceiling escalates, so the count exceeds the ceiling only after a reload lowers `max_retries` below the fixes already dispatched.
    - `review` and `bot-review` count review-fix and bot-review-fix continuations dispatched, against `max_continuation_turns`. At the ceiling the next set holding a new comment (for `bot-review`, a new comment on the code) escalates.
    - `merge-conflict` counts rebase continuations dispatched since the conflict episode began, against `max_retries`. At the ceiling the next new conflicting head escalates.
    - `merge` counts merge calls made, against `max_retries`, and `max_attempts` is `null` at `0`. At the ceiling the next check escalates.
    - `merge-completion` counts tracker transitions that failed, against `max_retries`. At the ceiling the next failed transition escalates.
  - `escalated_at` is always present: when the standing escalation of the reaction began, and `null` while none stands. The label a client shows is `config.reactions[config_key].escalation_label`. Every kind whose pass escalates keeps its row while the escalation stands, because the escalation is a soft stop that keeps watching the pull request, and the value is the time the escalation began: a recheck does not move it. The escalation survives a restart with its time, trigger, and the tries it used. What ends it depends on the kind (Sections 11A.7 to 11G.6), and a person moving the issue out of `tracker.handoff_state`, or releasing its park so Sortie runs the issue again, ends it for every kind. A later escalation of the same reaction records a new time.
  - `escalation_trigger` is present exactly while `escalated_at` is non-null, and omitted otherwise. It says why the standing escalation began. `budget_exhausted`: the kind used up its tries. `triage`: the kind's triage command asked for a person. `refused`: a write the forge or the tracker answered with an authentication or payload error, on `merge` and `merge-completion`. `missing_merge_commit`: the forge reports the pull request merged but gives no merge commit by the end of the grace period, on `merge-completion`.
  - `watch_ends_at` is always present: when the reaction stops watching unless its watch restarts first, in UTC, and `null` for a kind without a watch window (`merge-completion`, `label-review`, and `label-fix`), for a window of `0`, while an escalation stands, because no window ages out a standing escalation, and for a `review` reaction whose latest check is awaiting approval, because the watch is held past its window. The pass that owns the kind drops an entry with no standing escalation, and for `review` no check awaiting approval, once its age exceeds the window, so the value is the last instant the watch holds, and it measures the age from that pass's own start of the watch. `ci` measures from the head commit its latest check recorded, or from the creation of the reaction while it has recorded none, so a new head moves the value. `review`, `bot-review`, and `merge-conflict` measure from the creation of the reaction or from its last retry-slot deferral, which restarts the watch, and `merge` from the creation. The window of each kind is the one its pass applies as of the latest poll tick.
  - `last_check` is always present: what the latest completed check observed, `null` before the first check completes and always `null` for `merge`, `merge-completion`, `label-review`, and `label-fix`. A result replaces the previous one and leaves with its entry. A head change clears a CI result, because a result for an earlier head describes nothing the reaction now evaluates. It is a flat object whose members follow the kind.
    - `observed_at` is when the check completed, in UTC, on every kind that records a result.
    - `head_sha` is the head commit the check read, on `ci` and `merge-conflict`, omitted when the provider reported none, and masked.
    - On `ci`, `status` is the verdict the reaction acted on, `passing`, `pending`, or `failing`, as the provider reported it; an empty check set is `pending`, so a client never reads it as passing. `failing_checks` holds the names of the check runs the verdict counts as failing, in the provider's order and uncapped, `[]` when none, and masked. `failing`, `pending`, and `passing` count the check runs by outcome, each run once in the class the verdict reads it as, so the counts and the verdict cannot disagree. The log excerpt and each check's details link are never served.
    - On `review` and `bot-review`, `actionable_comments` counts the comments the check found actionable: current, and for `review` outside `bot_usernames`. A comment a run was already given counts until the forge reports it outdated. `new_comments` counts those the reaction acts on next, meaning no run was given them, the reaction has not already reported them, and the running turn does not carry them. It is `0` whenever `wait` is `poll_interval`, since the check then found nothing to act on. Without it a client would read every healthy fix lap as comments still to answer, because a forge keeps a comment a run answered in the set until it reports it outdated, and `new_comments` follows the pass's outcome rather than set membership.
    - On `bot-review`, `handled` is `true` while the newest triage answer Sortie recorded for the comment set the check found is `handled`, so no run is given that set. It is omitted otherwise and is never `false`, so a payload without a handled answer is unchanged. Like `new_comments`, it follows the pass's outcome and each completed check replaces it. Sortie records every triage answer it applies per reaction kind and comment set, so the member returns after a restart with the entry's first completed check. A set that differs from the one answered, such as a new review, a new comment, or a set with one comment fewer, carries no `handled` until its own answer is applied.
    - On `review`, `awaiting_approval` is `true` when the check found no actionable comment and the review decision was `REVIEW_REQUIRED`, meaning an approval rule is unmet. It is omitted otherwise and is never `false`, so a payload without an approval wait is unchanged, and `bot-review` never carries it. A changes request never sets it. A failed decision read on a later check keeps the previous value, and each completed check replaces it. While it is `true` the watch outlives its window and `watch_ends_at` is `null`.
    - On `merge-conflict`, `mergeable` is `false` when the provider reports merge conflicts, `true` when it reports none, and `null` while it has not computed mergeability; a branch-protection block is not a conflict.
  - The list holds one row per issue and kind, sorted by `issue_identifier`, then `issue_id`, then `kind`, and `counts.pending_reactions` counts rows, not issues, so it equals the list's length. An issue appears in every list whose set holds it: an issue with a CI fix queued and review comments pending is in `retrying` and `pending_reactions` both. The per-issue response lists the issue's rows in the same order.
- A between-stages row, on the `between_stages` array and in the `between_stages` object of the per-issue response, describes one issue whose stage hop was made and whose next stage is not yet dispatched. The keys are additive; no existing key changes name, type, or meaning.
  - `issue_id` and `issue_identifier` are always present. `display_identifier` is omitted when the tracker needs no qualified form. `issue_title` and `issue_url` are the title and link as the hopping run's dispatch read them, each omitted when empty and masked.
  - `issue_type` and `state` are as the hopping run read them, each omitted when empty or unread and masked. `issue_labels` are the labels the hop leaves on the issue: the run's dispatch read without the stage labels the hop removes, followed by the stage label it adds. The dispatch read carries the label of the stage the issue left and lacks the label of the stage it reached, so it is not served as is. The record is written before the removals run, so a removal that fails leaves a stage label on the issue that the row lacks until a run is dispatched. The type and state stay true after the hop, which writes no tracker state. A hop record written before the upgrade serves none of them until the next stage dispatches.
  - `rule_name` is the stage reached, equal to `target_rule`. `stage_position` places `target_rule`, choosing the chain with `source_rule` as the previous rule. `agent_kind` and `configured_model` are what a dispatch of `target_rule` resolves to under the configuration as of the latest poll tick, set at startup and refreshed every tick so that a hop made between ticks already finds them, and omitted when that configuration names no rule with that name and a stage label.
  - `source_rule` is the stage the issue left: the rule whose run made the hop. `target_rule` is the stage it reached: the rule the hop advanced it to. `target_label` is the stage label the hop added, served unmasked because it is configuration.
  - `hopped_at` is when the hop was made, in UTC, and `null` when the record holds no time. `dispatched_at` is the pass of the run that made the hop, which the next stage's run continues, in UTC, and `null` when the record holds no pass time.
  - `hold` is the issue's hold as a held-row API note below describes it, or `null` while the latest dispatch pass recorded no hold for the issue. A hop record whose target label no read has shown holds the issue with `stage_hop_unconfirmed` until a read shows it, so `hold` is how a client tells an issue waiting for that read from one waiting for the next poll tick. The other keys of the row, `status_text` and `status_text_full` included, do not depend on `hold`.
  - An issue is listed from its hop until a run of it is dispatched, or until a reset of its hop count drops the record (Section 13.1). It is never also in `running` or `retrying`, and it can also be in `budget_exhausted`.
  - The list is sorted by `issue_identifier`, then `issue_id`, and `counts.between_stages` equals its length. The per-issue response answers for an issue known only through such a row.
- A held row, on the `held` array of `GET /api/v1/state`, describes one eligible candidate the latest dispatch pass held back and that no other row list holds. The keys are additive; no existing key changes name, type, or meaning.
  - `issue_id` and `issue_identifier` are always present. `display_identifier`, `issue_title`, `issue_url`, `issue_type`, and `state` are as the pass's candidate read them, each omitted when empty and masked. `issue_labels` is always present, `[]` when the issue carries none, because the candidate read always holds the labels.
  - `hold` carries `reason`, `held_since`, `blockers`, `slot`, `resets_at`, `reason_text`, and `release_text`, and, for the reason `usage_limit` only, `agent_kind`, `model_only`, and `model`. The same object is the `hold` of a between-stages row.
  - `hold.reason` is one of the values of the precedence below, and the API schema enumerates them. A client MUST tolerate a `reason` its copy of the schema does not list, one a newer version serves, and show the served texts.
  - `hold.held_since` is when a dispatch pass first held the issue, in UTC, and is never `null`. It is unchanged while later passes keep the issue held, whatever their reasons and across a candidate fetch that fails, and it is new after a pass that runs the loop does not hold the issue, then holds it again. A restart resets it.
  - `hold.blockers` is non-null exactly for `blocked_by`: every blocker whose state is empty or not terminal, in the order the tracker returned them, each with `issue_id`, `issue_identifier`, `display_identifier` (omitted when the tracker needs no qualified form), and `state` (omitted when the tracker returned none, masked).
  - `hold.slot` is non-null exactly for `agent_slots_full`: `scope` is `global` or `state`, `state` is present exactly for `state` and is the candidate's state as the pass read it, `used` is the sessions running against the limit, and `limit` is the limit.
  - `hold.resets_at` is non-null exactly for the spend reasons and `usage_limit`, in UTC: the reset instant of the window or the usage limit that holds.
  - `hold.agent_kind`, `hold.model_only`, and `hold.model` are served for the reason `usage_limit` and omitted otherwise. `agent_kind` is the agent kind the hold stops. `model_only` is true when the hold stops that kind on one configured model alone, and omitted when it stops every model of the kind. `model` is that configured model, omitted for a kind-wide hold and for the runtime's default model.
  - `hold.reason_text` is a short operator text without a final period, and `hold.release_text` is one sentence ending with a period that says what releases the hold. On a held row `status_text` equals `reason_text`.
  - A candidate's reason is the first row of this table that matches it on the latest pass. A spend window or a usage-limit hold that holds ranks above the capacity rows, because a freed slot would not dispatch the candidate while the hold stands. A spend hold ranks above a usage-limit hold.

    | Order | Check | Condition | `reason` | Details |
    | --- | --- | --- | --- | --- |
    | 0 | capacity checks under a spend or usage-limit hold | A spend window, or a usage-limit hold on the candidate's agent kind, holds and a capacity check stops the candidate | The window's spend reason, else `usage_limit` | `resets_at`; for `usage_limit` also `agent_kind`, `model_only`, `model` |
    | 1 | global slots | No global slot is free | `agent_slots_full` | Scope `global`; this and every later eligible candidate |
    | 2 | SSH capacity | SSH is on and no host is below its cap | `ssh_hosts_full` | None; this and every later eligible candidate |
    | 3 | per-state slots | The candidate's state has no free slot | `agent_slots_full` | Scope `state` |
    | 4 | eligibility | The candidate is not eligible | None | |
    | 5 | blocker read | The read budget is spent, or the candidate is before this pass's read window | `blockers_not_read` | None |
    | 6 | blocker read | The read failed, or an earlier failure halted reads | `blockers_unresolved` | None |
    | 7 | blocker list | The tracker declared the list incomplete | `blockers_incomplete` | None |
    | 8 | blockers | A blocker is empty or non-terminal | `blocked_by` | `blockers` |
    | 9 | spend | A spend window holds | `daily_spend_ceiling`, `weekly_spend_ceiling`, `monthly_spend_ceiling` | `resets_at` |
    | 10 | stage hop | The hop confirmation read did not confirm and the hop record stands | `stage_hop_unconfirmed` | None |
    | 11 | usage limit | A usage-limit hold covers the candidate's agent kind, or that kind on the candidate's configured model | `usage_limit` | `resets_at`, `agent_kind`, `model_only`, `model` |
    | 12 | dispatch | The agent kind, prompt template, or attempt settings are refused | None | |
    | 13 | host acquisition | No SSH host accepts the issue | `ssh_hosts_full` | None; this and every later eligible candidate |
    | 14 | park release | The issue's park was released by a person and the read of its comments failed | `park_comments_unread` | None |

  - A capacity check that stops the loop classifies the candidates it does not evaluate by the eligibility gates alone and reads no blocker for them, so while every slot or host is taken a candidate that also has an open blocker shows the capacity reason until a pass reaches it with capacity free. Under a spend hold it shows the spend reason instead, and under a usage-limit hold on its agent kind it shows `usage_limit`. A candidate refused at dispatch for its agent kind, prompt template, or attempt settings is held for nothing and is on no row list. A hop confirmation read that shows a terminal or non-active state resets the hop record and records no hold.
  - The list is sorted by `issue_identifier`, then `issue_id`, and `counts.held` equals its length. A held issue is never an `active` entry and adds nothing to `counts.attention` or to `attention`. The spend ceiling keeps its instance-level attention items, one per holding window. The per-issue response does not read `held`, so an issue only that list holds is answered from the other row lists and run history alone, and a between-stages issue's `between_stages` object carries `hold`.
- An active-issue entry, on the `active` array of `GET /api/v1/state`, describes one issue the row lists hold, so a client that shows one line per issue merges, chooses, and orders nothing itself. The key is additive; no existing key changes name, type, or meaning, and the row lists, `counts`, and the per-issue response stay as they are.
  - `active` is always present, never `null`, and `[]` when the row lists hold no issue. It holds one entry per distinct `issue_id` across `running`, `retrying`, `budget_exhausted`, `parked`, `pending_reactions`, and `between_stages`, so its length is the number of active issues and `counts` gains no member. A held issue is not in it.
  - `status` is always present and is the per-issue status by the precedence of the per-issue route: `running`, else `retrying`, else `budget_exhausted`, else `parked`, else `pending_reactions`, else `between_stages`. It is never `history`. It is also the name of the list the entry comes from, so no second key repeats it.
  - The deciding row is the issue's row in the list `status` names. In `pending_reactions` it is the issue's first served row whose `escalated_at` is non-null; else the `review` row awaiting approval (its `escalated_at` is `null` and its `last_check.awaiting_approval` is `true`), when every other row reports nothing outstanding; else its first served row. That row decides the per-issue sentences. Without this second rule the first row in kind order decides, and `bot-review` sorts before `review`, so the sentence would never name the approval while a quiet watch of another kind lives. A `ci` row reports nothing outstanding when its `last_check.status` is `passing`. A `bot-review` row does when its `last_check` holds comments and `actionable_comments` is `0` or `handled` is `true`. A `merge-conflict` row does when its `last_check.mergeable` is `true`. A `merge`, `merge-completion`, `label-review`, or `label-fix` row always does, because it records no check and waits on the merge or a label rather than on a merge condition. A row of any other kind never does.
  - `issue_id` and `issue_identifier` are always present. `display_identifier`, `issue_title`, `issue_url`, `rule_name`, `agent_kind`, and `configured_model` are omitted when the deciding row holds none. `stage_position` and `dispatched_at` are always present and `null` when the deciding row serves `null`. Each key is the deciding row's value, so the masked strings are the ones that row serves.
  - `reaction_kind` is on every entry whose `status` is `pending_reactions` and is omitted on every other entry. It is the `kind` of the deciding row. Only `pending_reactions` holds more than one row for an issue, so a client finds the deciding row by `issue_id` in the list `status` names, and in `pending_reactions` by `issue_id` and `kind` equal to `reaction_kind`. The `reactions` key the kind is configured under follows from the kind, so the entry serves none, and a running or retry entry carries no reaction key because its row already serves the continuation's pair.
  - `status_text` and `status_text_full` are the deciding row's pair, equal to the top-level pair the per-issue response serves for the issue.
  - `pr_loop` is always present. It describes the pass of the deciding row and is `null` when the server holds no run history or no workflow shape, and when the read of reaction activity fails.
  - The order is the place the orchestrator keeps for each issue, then `issue_identifier`, then `issue_id`, each in byte order, and a place that is absent counts as the zero time, which sorts before every timed issue. An issue's place is the earliest non-null `dispatched_at` its rows carried when it joined the lists, or the zero time when every row is undated. The place survives a move between lists, a new pass begun while the issue is listed (a park released and dispatched on one tick, a budget hold cleared by a raised ceiling, an issue a person returns to an active state while a reaction waits), and a `dispatched_at` that turns from `null` into a time. An issue that leaves every list and returns takes a new place from its new rows. The place is held in memory and served nowhere, so a client that filters the list keeps the served order, and a restart places each issue by its restored rows.
- `pr_loop`, on each entry of `active`, on the per-issue response, and on each run row of `GET /api/v1/runs`, says what each configured pull request reaction recorded in one pass, so a client draws a reaction station that holds no pending reaction. The key is additive; no existing key changes name, type, or meaning.
  - A pass is the runs that share a stage chain identifier (Section 13.3). The member describes the pass of the deciding row on an `active` entry; on the per-issue response, the pass of the row that decides `status` for a live issue and the pass of the newest run for a `history` issue; on a run row of `GET /api/v1/runs`, the run's own `chain_id` and `dispatched_at`. The group runs of `GET /api/v1/runs/issues` and the `history.runs` array keep the run row shape and serve `pr_loop` as `null` always, with no read of reaction activity, because no lane reads them.
  - A served `pr_loop` is an object with one member per key of `config.reactions` in the workflow shape the request reads, keyed as in WORKFLOW.md, and `{}` when that shape holds none. The kinds `label-review` and `label-fix` fold into `label_commands`. Each member holds `result` and `continuations`.
  - `result` is `escalated` when a kind of the key began an escalation in the pass, an escalation that ended within the pass included. Otherwise it is `acted` when a kind of the key applied a triage verdict, made a merge call, made a tracker transition for a merge completion, or scheduled a continuation; otherwise `quiet` when a kind of the key was watched in the pass and did none of these; otherwise `unrecorded`. A pass that recorded nothing for a key, because Sortie started watching the kind after a restart and the kind has not acted, reads `unrecorded` at that key.
  - `continuations` counts the continuations scheduled for the key in the pass, summed over its kinds. It is the quantity the pull request panel calls the fixes used, so a fix that a budget hold kept from running still counts. A retry of a failed reaction run is not a continuation.
  - A pass is recorded when it has a chain identifier and a dispatch time and began no earlier than the second Sortie started recording reaction activity (Section 19.2). Every key of any other pass reads `unrecorded` whatever rows exist, so a pass that began before Sortie recorded reaction activity never claims that a reaction had nothing to act on.
  - `pr_loop` is `null` when the server holds no run history or no workflow shape. On `GET /api/v1/state` it is also `null` on every entry when the read of reaction activity fails; the route still answers `200` (Section 13.1). On the per-issue route and `GET /api/v1/runs`, a failed read answers `503` with `history_unavailable`, as a failed read of the issue's outcome does.
  - A request makes at most one read of reaction activity, covering every pass it serves.
- An attention item, on the `attention` array of `GET /api/v1/state`, describes one thing that needs a person. The orchestrator derives the list on each request from the snapshot and the health checks, so an item exists exactly while its cause holds and disappears on the first request after the cause clears. The list is stored nowhere, while the acknowledgements of its items are stored (`acknowledged_at`). The keys are additive; no existing key changes name, type, or meaning. A client that shows the list counts, orders, and words nothing itself.
  - `attention` is always present and is `[]` when nothing needs a person. `counts.attention` counts the items whose `acknowledged_at` is `null`, so an acknowledged item leaves the count and stays in the list.
  - `kind` is one of the following. Each cause yields one item, with the `since`, `severity`, and `action` the row gives.

    | `kind` | Cause | `since` | `severity` | `action.kind` |
    |---|---|---|---|---|
    | `parked` | A row of `parked` | `parked_at` truncated to whole seconds, `null` when the stored time does not parse | `warning` | `open_issue`; `acknowledge` when the row holds no `issue_url` |
    | `budget_held` | A row of `budget_exhausted` | `exhausted_at` | `warning` | `acknowledge` |
    | `reaction_escalated` | A row of `pending_reactions` whose `escalated_at` is non-null, whatever its kind | `escalated_at` | `warning` | `merge-completion`: `open_issue`, else `acknowledge`. Every other kind: `open_pull_request`; else `open_issue`; else `acknowledge` |
    | `workflow_reload_failed` | `health.workflow.consecutive_failures` above `0` | `health.workflow.failing_since` | `critical` | `acknowledge` |
    | `preflight_failed` | `health.preflight.consecutive_failures` above `0`, unless the latest failed run failed on the workflow load | `health.preflight.failing_since` | `critical` | `acknowledge` |
    | `spend_ceiling_reached` | A window of `spend_ceiling` whose `holds_dispatch` is true; each window that holds yields its own item | The evaluation that began the hold | `critical` | `acknowledge` |

  - A preflight that fails on the workflow load yields no `preflight_failed` item, because the reload item already names the cause. Without that rule one broken file shows an item for the load and another for the preflight, and after the watcher loads the fixed file the preflight check keeps the load error until the next tick and raises an item for a cause that is gone. A `preflight_failed` item's `since` is when the preflight began failing, which predates the item when the failing streak began on the workflow load.
  - `severity` is `critical` for a kind under which poll ticks dispatch nothing for any issue and `warning` for a kind about one issue. It is fixed per kind and never grows with age.
  - `since` is `null` only for a park whose stored time does not parse. A hold's `since` is the row's `exhausted_at`, and after a restart it is the first poll tick, because the onset of a hold lives only in memory. A spend hold's `since` is the evaluation that began it, and after a restart it is the first evaluation, for the same reason.
  - `acknowledged_at` is present on every item. It is when a person acknowledged the item, in UTC, and `null` while the item is unacknowledged. An acknowledgement moves no item in the order and changes none of its facts.
    - An acknowledgement belongs to one episode of its item's cause: from the item's derivation until the first poll tick that reads the candidate issues and no longer derives the item. That tick deletes the acknowledgement. A different reason on the issue, or the same reason after its cause cleared, shows as a new item with `acknowledged_at` `null`.
    - A poll tick that cannot read the candidate issues, because its preflight or its candidate fetch failed, deletes no acknowledgement. The set of budget holds changes only on a tick that reads the candidates, and before the first such tick after a restart it is empty, so a deletion then would drop the acknowledgement of every hold during a tracker outage.
    - A hold whose issue left the active states and came back returns unacknowledged, with its earlier `since`, because leaving the candidates clears a hold and the tick that found it gone deleted its acknowledgement.
    - Acknowledgements survive a restart. Sortie reloads them before the event loop starts. A park and an escalation return with their `id` and keep their acknowledgement, and a hold keeps it once the first poll tick that reads the candidate issues rebuilds the hold, or earlier when the fire of a recovered retry holds the issue. That tick deletes the acknowledgement of every item it no longer derives. A reload failure or a preflight failure cannot span a restart, because startup stops on a workflow file that fails to load and on a preflight that fails, so its acknowledgement goes at that tick. A spend hold spans a restart: its `id` is built from its window, its window start, and its ceiling, which a restart changes none of, and a restart leaves the window's recorded spend unchanged, so the item returns with the same `id` and keeps its acknowledgement, and its `since` is the first evaluation after the restart.
  - `action` holds `kind`, which is `open_issue`, `open_pull_request`, or `acknowledge`, and `url`, present exactly for the first two. `acknowledge` is the action of an item with nothing to open. Every item can be acknowledged through `POST /api/v1/attention/{id}/acknowledge`, whatever its `action.kind`.
  - The issue keys (`issue_id`, `issue_identifier`, `display_identifier`, `issue_title`, `issue_url`) are copied from the source row as the row serves them, masked, with `display_identifier`, `issue_title`, and `issue_url` omitted when empty. An item of a cause that concerns the process omits every issue key.
  - `id` is the lowercase hex of the first 16 bytes of SHA-256 over the JSON encoding of the string array `[kind, parts...]`, and the encoding never changes, because Sortie keys stored acknowledgements by `id` and a client may key its own state by it. A time part is in UTC. The parts are the following; no masked string enters an `id`, so a secret registered later changes none.

    | `kind` | Parts |
    |---|---|
    | `parked` | `issue_id`; the park time truncated to whole seconds in RFC 3339, `""` when the stored time does not parse |
    | `budget_held` | `issue_id`; `reason`; the ceiling `reason` names in decimal, `budget_tokens` for `token_budget`, `budget_sessions` for `session_budget`, and `budget_usd` for `money_budget`, which is written in the shortest decimal form with no exponent and no trailing zeros, so `5`, `5.0`, and `5.00` give `5` |
    | `reaction_escalated` | `issue_id`; the row's `kind`; `escalated_at` in RFC 3339 with nanoseconds |
    | `workflow_reload_failed` | `failing_since` in RFC 3339 with nanoseconds |
    | `preflight_failed` | `failing_since` in RFC 3339 with nanoseconds |
    | `spend_ceiling_reached` | the window, `daily`, `weekly`, or `monthly`; the window start in RFC 3339 in UTC; the ceiling in the shortest decimal form with no exponent and no trailing zeros, as for `budget_held` |

  - No part changes while its cause holds, so an `id` stays the same across requests and across a restart for a park and for an escalation, which keep their time. A recurrence changes a part: a new park has a new time, a hold reached again after a raise or under an edited ceiling has a new ceiling, a different budget reason is a new reason, an escalation that ended and began again has a new time, a failure after a pass has a new `failing_since`, and a spend hold under a new window start or an edited ceiling has a new `id`, so a new day's, week's, or month's hold, or a hold that continues under a raised ceiling, returns unacknowledged. The one recurrence that changes no part is a hold that clears and returns under the same reason and ceiling, because its issue left the active states and came back or its ceiling was raised and restored: it gets its `id` back, and no acknowledgement carries over to it, because Sortie deletes an acknowledgement once a poll tick that reads the candidate issues no longer finds its item. So a stored acknowledgement never hides a new cause. The park time enters at whole seconds because the store keeps a park at that precision, so the precision survives a restart.
  - The list is ordered by `severity` (`critical` before `warning`), then `since` (`null` first, then ascending), then `id` ascending. The order is total, so the same inputs serve the same list.
  - `reason_text`, `why_text`, and `clears_text` are English sentences taken from the status sentence table, one template per park reason, per budget reason, per spend window, per usage-limit hold, per check, and per escalating reaction kind (the template of an escalated reaction takes its `why_text` from the row's `escalation_trigger`). `reason_text` is a short phrase without a final period; `why_text` and `clears_text` are one sentence each, ending with a period. They follow the rules of the status sentences below: only a fact Sortie records, masked as Section 15.3 states, no clock time, date, or stage, no cost except the figures of a `money_budget` hold, whose `why_text` states the issue's estimated cost and its budget and reads "at least" while the cost is a lower bound, and the figures of a spend ceiling hold, whose `why_text` states the window's estimated cost and its ceiling and reads "at least" while the cost is a lower bound, and `Status not available` for a part that fails to render, with the failure logged. The item stays when a part fails, so neither the list nor its count drops a cause.
  - `evidence` holds the facts behind the item. A member is present exactly on the kinds named and absent on every other.

    | Member | Kind | Value |
    |---|---|---|
    | `reason` | `parked`, `budget_held`, `spend_ceiling_reached` | The row's `reason`: `agent_blocked` or `handoff_absence`; `money_budget`, `token_budget`, or `session_budget`; for a spend hold, `daily_spend_ceiling`, `weekly_spend_ceiling`, or `monthly_spend_ceiling` |
    | `label` | `parked` | The row's `label` |
    | `used_sessions`, `budget_sessions` | `budget_held` with `session_budget` | The row's values |
    | `used_tokens`, `used_tokens_complete`, `budget_tokens` | `budget_held` with `token_budget` | The row's values; `used_tokens_complete` is false exactly when the spend is a lower bound |
    | `used_usd`, `used_usd_complete`, `budget_usd` | `budget_held` with `money_budget` | The row's values; `used_usd_complete` is false exactly when the cost is a lower bound |
    | `config_key` | `spend_ceiling_reached` | `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, or `agent.max_monthly_cost_usd`, the setting that governs the window |
    | `used_usd`, `used_usd_complete`, `budget_usd` | `spend_ceiling_reached` | The window's values in `spend_ceiling`; `used_usd_complete` is false exactly when the cost is a lower bound |
    | `resets_at` | `spend_ceiling_reached` | The window's `resets_at` |
    | `reaction_kind`, `config_key`, `pull_request`, `escalation_trigger` | `reaction_escalated` | The row's `kind`, `config_key`, `pull_request`, and `escalation_trigger`, in the shapes the row serves |
    | `used_attempts`, `max_attempts` | `reaction_escalated` | The row's values, each omitted when `null` |
    | `last_check` | `reaction_escalated` | The row's `last_check`, omitted when `null` |
    | `file` | `workflow_reload_failed` | `health.workflow.file` |
    | `error` | `workflow_reload_failed`, `preflight_failed` | The check's `error` exactly as `health` serves it |

  - What clears each cause is the source's own rule. A park is released when a person removes its label after Sortie has seen it, or moves the issue to another tracker state. A hold leaves the set when the poll tick's rebuild no longer finds the issue at a ceiling or among its candidates. A reaction escalation ends on the facts of its kind (Sections 11A.7 to 11G.6). A reload failure ends with the next load that succeeds, and a preflight failure with the next preflight that passes. A spend hold ends at the first evaluation that finds its window reset, its ceiling raised above the window's spend or set to `0`, or its window's spend lowered below the ceiling by a rate change in a running session's estimate.
- A run row, on `GET /api/v1/runs`, in the `runs` of each group of `GET /api/v1/runs/issues`, and in the `history.runs` array of the per-issue response, describes one finished run read from run history. The keys are additive; no existing key changes name, type, or meaning.
  - `run_id` is the run's key in run history and the paging key. `issue_id`, `issue_identifier`, `attempt`, `status`, `started_at`, and `completed_at` are always present. `display_identifier`, `issue_title`, `issue_url`, `rule_name`, `agent_kind`, `configured_model`, `configured_effort`, `model_name`, `workflow_file`, and the stage keys are omitted when the row holds none. Every value is served as recorded: the timestamps are RFC 3339 in UTC as the writer stored them, `dispatched_at` is the pass dispatch time as a running row defines it and `null` when the row holds none, `model_name` is the model the runtime reported, as on a running row, and `rule_name` is `default` for `dispatch.default` and omitted for the workflow-wide fallback.
  - `issue_title`, `issue_url`, `error`, `agent_text`, and `self_review.final_verdict` are masked as Section 15.3 states, when the response is built rather than when the row is written, so a value registered as a secret after the run is masked too. Rule names, models, the workflow file, the session ID, and the SSH host are served unmasked, as running rows serve them. `error` is `null` when the run recorded none. A stored error that begins with `handoff withheld: `, or with `worker exited: ` followed by it as a retry row stores it, is served as a plain sentence worded by the sentence table (`The agent finished without changing anything, so Sortie did not hand the issue off.` for an observed absence, `Sortie could not tell whether the agent changed anything, so it did not hand the issue off.` for an undeterminable verdict, and `Sortie did not hand the issue off` followed by the text after the prefix for any other value); a retry row's `error`, the per-issue `last_error`, and the retry's status sentence are worded the same way, the error chains the worker builds are worded the same way, with the optional `worker exited: ` wrapper removed and any closing `context canceled` or `context deadline exceeded` dropped: `agent turn N: agent: <kind>: <message>` and `agent session start: agent: <kind>: <message>` read as one sentence per adapter error kind, keeping the adapter's message after a colon except for `turn_cancelled` (`Sortie stopped the session before the agent finished turn N.`) and `turn_timeout` (`The agent did not finish turn N within the turn time limit, so Sortie stopped it.`); `agent turn N ended: <reason>` reads as the cancelled or failed turn sentence, or `The agent ended turn N early (<reason>).`; `workspace preparation`, `mcp config generation`, `prompt render (turn N)`, and `issue state refresh (turn N)` errors read `Sortie hit an error while <doing>` followed by the cause, `Sortie stopped the run while <doing>.` for a cancelled cause, or `Sortie gave up on <doing> because it took too long.` for a deadline; `worker panic: <value>` reads `Sortie hit an internal error while running the agent: <value>.`; and a bare `context canceled` or `context deadline exceeded` reads `Sortie stopped the run before it finished.` or `The run ran out of time.`. The errors the orchestrator records itself read as one sentence as well: `CI checks failed on ref <sha>` reads `The CI checks failed on commit <sha>.`; `money ceiling reached: used <amount> of <ceiling>` reads `Sortie stopped the run because the issue's estimated cost, <amount>, reached its limit of <ceiling>.`; `token ceiling reached: used <n> of <limit> tokens` reads `Sortie stopped the run because the issue used <n> tokens, reaching its limit of <limit>.`, with both figures in the abbreviated token form the dashboard uses; and the reason a cancelled run records reads `The agent sent no event within the stall timeout, so Sortie stopped the run.` for `stall timeout exceeded`, `Sortie stopped the run because the issue moved to <state>, a final state.` for `issue reached terminal state: <state>`, `Sortie stopped the run because the issue is no longer in an active state (now <state>).` for `issue left the active states: <state>` (without the parenthesis when no state was recorded), and `Sortie stopped the run because the service was shutting down.` for `service shutting down`. The adapter and worker stems (`agent turn N: `, `agent session start: `, `agent: <kind>: `) never lead a served detail, and a detail whose words all appear in its sentence or are filler words, such as `You've hit your limit` after the usage-limit sentence, is left out, while one that adds a word, such as a reset time, is kept. An error matching none of these is served as stored, and the stored value and its prefix are unchanged.
  - `declared_outcome` and `agent_text` are always present and additive, each a string or `null`, never `""`. `declared_outcome` is the `.sortie/status` token the run's agent ended it with (`blocked`, `needs-human-review`, or `no-change-needed`), served as written and unmasked because it is one of a fixed set, and `agent_text` is the statement that came with it. Each is `null` when the stored value is empty: a run that ended on no recognized token, a retracted `no-change-needed`, an error, cancelled, `budget_stopped`, or `needs_person` exit, the recorded CI result, and a run recorded before the members were kept, a `blocked` one included, which is not backfilled. `declared_outcome` states what the agent declared and `status` the disposition Sortie recorded, and neither derives from the other: a run that ended on `blocked`, `needs-human-review`, or `no-change-needed` keeps status `succeeded`, and a `needs-human-review` run whose handoff the evidence verdict withheld keeps status `failed` and its `handoff withheld` error while it still carries its declaration and statement. When the run's exit emits `session.stopped`, `agent_text` equals the agent text that notification carries (Section 21.1).
  - The members of `tokens` and `uncached_tokens` are `null` exactly when `tokens_measured` is false, and a measured zero is `0` (Section 13.5). `uncached_tokens` is `tokens.total_tokens` less `tokens.cache_read_tokens`, floored at zero. `unaccounted_turns` counts the turns that issued a model request no token figure was proven to account for.
  - `estimated_cost_usd` is the cost recorded when the run's row was written (Section 13.5), never re-priced, and `cost_status` is the status recorded with it, saying why it is absent or partial. `unmeasured` marks a run that reported no usage. `estimated` marks a priced run, and a measured run that spent nothing, classes and total alike, is `0` and `estimated` whether or not its kind has a rate, unless it recorded an unaccounted turn, because a finished run's figure is final: this is how the recorded CI result, which has no agent kind, reads. `unpriced` marks a run that spent tokens no rate priced, including spend reported as a total without classes, which is never a free estimate. `partial` marks an estimate that is a lower bound because a model or a token class the run spent had no rate beside priced spend, or because the run recorded an unaccounted turn. A running row prices a measured session that has spent nothing as `unpriced` when no rate prices its model, because that session can still spend.
  - `model_usage` is the run's spend split by model: one element per model the run spent tokens on, ordered by `model`. It is `[]` when the run recorded no per-model spend, which holds for an unmeasured run, a run that spent nothing, and a run recorded before per-model spend was kept that no start has priced yet. The key is always present and additive, and it is never `null`. An element holds `model`, `tokens`, `uncached_tokens`, `estimated_cost_usd`, `cost_status`, and `unrated_classes`. `model` is the name the runtime reported, `""` when it reported none, served unmasked as `model_name` is. `tokens` and `uncached_tokens` follow the run row's shapes, no member of `tokens` is `null`, and `tokens.total_tokens` is `input_tokens` plus `output_tokens`. `estimated_cost_usd` and `cost_status` are the cost recorded with the part and follow the run row's status rule, except that a part recorded without a price serves `unpriced` and a `null` cost until a start prices it. `unrated_classes` lists the token classes whose missing rate left the part `partial` or `unpriced`, from `input`, `output`, `cache_read`, and `cache_write`, in that order. It is `[]` when the part names none, because every class it spent had a rate or its spend was reported as a total without classes, and `null` when the part records none, because it has no recorded price or was priced before the classes were kept. `null` never reads as `[]`. An element serves no price source and no price table version.
  - `skills` lists every skill call of the run in completion order, repeats included and without a limit: each element holds `skill`, the skill name, and `tool_error` as `true` when the call failed, omitted otherwise. A failed call loaded no skill, and once its event is no longer retained the element is the only record of the try. The list is built from every `tool_result` event on arrival, so it is complete when the run's retained events are not. The member is omitted when the run recorded no skill call, for a runtime whose adapter reports none, and on a run recorded before the list was kept, so its absence never claims a measured zero. A running row serves the calls so far under the same name.
  - `self_review` is `null` when the run carries no review metadata or the metadata cannot be decoded. Otherwise it holds `final_verdict`, which is `none` when the run recorded an empty verdict, `total_iterations`, and `cap_reached`.
  - `chain_id`, `stage_previous`, and `stage_previous_outcome` are the stage lineage the run was dispatched with. `stage_target` and `stage_result` are the hop decision the run's exit reached, recorded after the row is appended, so a read between the two serves the run without its decision and the next read serves it.
  - `stage_chain` is the stage chain the run's rule stands on, as the rule names of the chain in `next` order, so a client places a finished run on its chain without a live row. It is the chain `stage_position.chain` serves for the run's `rule_name` and `stage_previous`, so a rule that more than one chain holds resolves as it does on a live row. It follows the configuration in effect, so a reload that edits the chains changes it for runs recorded before the reload. The key is always present and additive, and it is `null` when the run has no `rule_name`, when its rule is on no chain, and when the server holds no workflow shape. Rule names are served unmasked, as configuration.
  - `dispatch_id` is the dispatch ID of the run, the key of its retained events (Section 13.9). It is `""` on the recorded CI result, which no dispatch produced, and `null` on a run recorded before run history held dispatch IDs. Neither has retained events, and a client offers no event view for them.
  - `origin_recorded` is false for a run recorded before the origin columns existed, and `trigger`, `reaction_kind`, `reaction_config_key`, `session_id`, and `ssh_host` are all `null` on such a run. Where `origin_recorded` is true, each key is `""` when the run has none: no retry trigger, no reaction, no session, or a local run. `trigger` alone is `null` on a run dispatched by a retry whose row never stored what started it, as a retry queued before the origin was recorded can be. A client never reads an unrecorded origin as a local poll-tick dispatch.
  - `trigger` is what started the retry whose fire dispatched the run: the `sortie_retries_total` label of the schedule that created that retry (`error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`), kept through every put-back by the retry timer. The `timer` label stays on the retry row and the metric. It is `""` for a dispatch from the poll tick and for a retry a reaction other than the CI fix started, which `reaction_kind` names. `reaction_config_key` is present with the meaning a retry row gives it, and is `""` or `null` exactly when `reaction_kind` is. `session_id` is the agent session the run used, `""` when it started none, and `ssh_host` is the SSH worker the run executed on. The recorded CI result carries `""` in each origin key, because it records a verdict and nothing started it.
  - `pr_loop` is always present. On a row of `GET /api/v1/runs` it describes the run's own pass, named by its `chain_id` and `dispatched_at`, and is `null` when the server holds no workflow shape. It is `null` on every row of a group of `GET /api/v1/runs/issues` and of `history.runs`. The `pr_loop` bullet gives the shape.
  - `issue_outcome` is how the issue's latest work ended, as Section 4.1.8 defines it, or `null` when Sortie observed no ending since it last picked the issue up. The key is always present. The value is the issue's, so every run of one issue on a page carries the same object, and it is never the outcome of one run. Sortie serves it as recorded, whatever the issue's live state: a handed-off issue that waits on a reaction or runs a reaction continuation serves its handoff beside its live members, and the live entry, not the outcome, says Sortie still holds the issue. A running issue picked up again after an earlier ending, and an issue that ended before outcomes were recorded, serve `null`.
  - An outcome holds `kind`, `observed_at`, `status_text`, and `status_text_full` always. `kind` is `handed_off`, `merged`, or `terminal`, and `observed_at` is the time Sortie made or observed the ending, RFC 3339 in UTC. `state` is present on `handed_off` and `terminal`: the state the handoff wrote, or the terminal state read. `merge_path` and `pull_request` are present on `merged`. `merge_path` is `auto_merge` or `merge_completion`, the `reactions` key of WORKFLOW.md of the reaction that observed the merge, and `pull_request` holds `number` and `repository` in the shape a pending reaction row serves. `state` and `repository` are masked as Section 15.3 states, when the response is built. `status_text` and `status_text_full` are the sentence of how the issue ended, worded from the outcome alone, so a stage hop that was not made appears only in the top-level pair of a `history` response. A failed read of outcomes answers `503` with `history_unavailable` on both routes and is never served as `null`, which reads as no outcome observed.
- `history.totals`, which `totals` on each group of `GET /api/v1/runs/issues` repeats, sums all of the issue's runs, whatever the page holds, so it describes the issue where `history.runs` describes the page. The keys are additive; no existing key changes name, type, or meaning.
  - `run_count` counts every run, a recorded CI result included, which is the count `agent.max_sessions` is compared with. `unmeasured_runs` counts the runs whose usage was never recorded.
  - `tokens` holds the same members as a running row's `tokens`. `total_tokens` sums every run, an unmeasured run holding `0`, and the other members sum the measured runs. `uncached_tokens` is that `total_tokens` less the sum of `cache_read_tokens`, floored at zero, and it follows the null rule below. A member, `uncached_tokens` included, is `null` when its sum is `0` and `tokens_complete` is false, so unknown spend never reads as zero. `tokens_complete` is true exactly when no run is unmeasured and none recorded an unaccounted turn; when it is false, the sums are lower bounds.
  - `estimated_cost_usd` and `cost_status` follow the issue cost rule that `issue_estimated_cost_usd` and `issue_cost_status` apply, over the issue's runs alone with no running-session part. `partial` marks an unmeasured run beside priced spend.
- `history.hops` lists the hop decisions of the runs on the page, newest first. A hop row holds `run_id`, `chain_id` (omitted when empty), `source_rule`, `target_rule`, `result`, and `completed_at`. `source_rule` is the deciding run's rule name, named as on a between-stages row, and `target_rule` is the rule the exit reached. `advanced` and `partial` made the hop, and `failed` and `ceiling` did not. The list is bounded to the runs on the page, so a client reads an older run's decision from the row's own `stage_target` and `stage_result` on the pages `next_cursor` continues.
- `history.last_stage` is the stage chain the issue's last stage run stood on and the rule it ran, so a client places a finished issue on its chain, which no live row serves for it. It holds `chain`, the rule names of the chain in `next` order, and `rule`, the rule name of the deciding run. The key is always present and additive; no existing key changes name, type, or meaning.
  - The deciding run is the newest run on the page whose `reaction_kind` is empty and whose `rule_name` is not. A reaction run follows a pull request rather than a stage, and the recorded CI result carries no `rule_name`, so neither decides.
  - `chain` is the chain `stage_position.chain` serves for that rule and the run's `stage_previous`, so a rule that more than one chain holds resolves as it does on a live row. It equals the `stage_chain` of the deciding run on the same response. It follows the configuration in effect, as `stage_position.chain` does, so a reload that edits the chains changes it for runs recorded before the reload.
  - The value is `null` when no run on the page decides, when the deciding rule is on no chain, and when the server holds no workflow shape. Only the page is read, never a further page, so an issue whose newest runs are all reaction runs and recorded CI results serves `null`.
- History reads run on the request goroutine through the store's single connection, so a read delays the orchestrator's next store call by its own duration. `GET /api/v1/state` makes one such read, of the reaction activity of the passes its `active` entries describe, after it takes the snapshot, and the per-issue route and `GET /api/v1/runs` make one for the passes they serve. The activity read holds one open statement at a time: it reads the start of the recorded passes before it opens its row query, because a second statement opened beside the first waits for the one connection forever. The routes that serve run rows, `GET /api/v1/runs`, the per-issue route, and each group of `GET /api/v1/runs/issues`, then read the per-model parts of the runs they serve in one more statement, keyed by run ID and bounded by the page. That read opens only after the run read has returned, so one statement is open at a time, and it goes through the primary key of the parts. An `issue` or `issue_id` filter and every per-issue read go through an index. No index serves the time range, so a request whose filters match fewer runs than the page holds below the cursor reads run history down to its start. A page of unfiltered or recent runs stops after the page. `GET /api/v1/runs/issues` reads one streamed selection that stops once it holds the page's groups plus one, and issues no other store read while that selection is open, because the store has one connection and a second read would wait for it forever. Then it reads each served issue's matching runs, newest run, and totals through the issue ID index. An `issue` key resolves first through the identifier index and the display identifier index, and the selection reads only the keyed issues' runs.
- `active_estimated_cost_usd`, an implementation extension beyond the baseline shape above, sums the live estimates of running, measured sessions that price, in full or in part; it is omitted when no running, measured session prices. `cost_unpriced_running` sits beside it and counts the running, measured sessions the sum leaves out for want of a rate. It is present, zero included, whenever a rate source is available (Section 13.5), and omitted otherwise, so its presence alone tells a consumer whether Sortie can price at all. While a rate source is available, `active_estimated_cost_usd` equals the sum of `estimated_cost_usd` over the running rows whose `cost_status` is `estimated` or `partial` (and is absent when no row is), and `cost_unpriced_running` equals the count of rows whose `cost_status` is `unpriced`.
- `spend_ceiling`, an implementation extension beyond the baseline shape above, serves the latest evaluation of the instance spend ceilings (Section 8.4). It is omitted while no instance spend window's setting is above `0`, as `active_estimated_cost_usd` is omitted when it does not apply, so a deployment without any of these settings sees no change in the response. Otherwise it holds `daily`, `weekly`, and `monthly`, each an object for a configured window and `null` for the others. An object holds `budget_usd`, the setting the evaluation read; `used_usd`, the window's estimated cost in unrounded USD (Section 13.5); `used_usd_complete`, false exactly when `used_usd` is a lower bound; `holds_dispatch`, true while the window holds; and `resets_at`, the exclusive end of the window as RFC 3339 in UTC. The route computes nothing and reads no store: it serves the evaluation the dispatch lanes last applied, so `holds_dispatch` equals their decision and the figures are at most one `polling.interval_ms` old. The windows start at midnight UTC, while the dashboard's "today" starts at midnight in the browser's time zone, so for a viewer off UTC its today total and the daily window cover different spans.
- `config` on `GET /api/v1/state` is the workflow shape in effect, so a client draws the rules, stages, and reactions the workflow has and assumes none it lacks. The key is additive; no existing key changes name, type, or meaning.
  - `config` reports the configuration promoted at the last successful load, kept when a reload fails. A setting whose change needs a restart reports what the process applies: every reaction except `ci_failure` is the one built at startup, and `tracker.kind` and `tracker.project` are the ones the tracker adapter was built with. So `config` names no reaction a reload added and keeps naming one a reload removed, and it keeps the tracker kind and project the process started with, until the restart that applies the edit.
  - The poll loop copies budgets, concurrency caps, and SSH hosts into its own state at its next tick, so for at most one poll interval after a reload `agent.max_tokens`, `agent.max_sessions`, and `agent.max_cost_usd` in `config` can lead a running row's `budget_tokens`, `budget_sessions`, and `budget_usd`. A client shows a row's own budget keys on that row, and the `config` values where it describes the workflow.
  - A member whose setting imposes no ceiling serves `null`, present and never `0`: `agent.stall_timeout_ms`, `agent.max_sessions`, `agent.max_tokens`, `agent.max_cost_usd`, `worker.max_concurrent_agents_per_host`, and the auto-merge `max_retries`, where `0` turns count-based escalation off. The other `max_retries` members serve the value held, `0` included: a merge-conflict `0` escalates on the first detection. `max_turns` is the limit a session applies, so a configured value below 1 serves as 1.
  - `tracker.project`, `tracker.in_progress_state`, `tracker.handoff_state`, and `tracker.no_change_state` are omitted when unset. A list member is `[]` when empty, and `max_concurrent_agents_by_state` is `{}`. Its keys are lowercase states and its caps are positive.
  - `dispatch.rules` lists the rules in configuration order, which is the order match rules are tried; a rule selected by its stage label is chosen ahead of them. `selected_by` is `stage` for a rule that carries a stage label, `catch_all` for a rule with no `match` block that takes every remaining issue, and `match` otherwise. `stage` appears only on a `stage` rule. `match` appears only on a `match` rule and holds each non-empty predicate as written: `labels`, `issue_type`, `title`, `identifier`, and `assignee` as lists, and `priority` as `op` with `value`, or `op` `in` with `values`. `name` and `next` are omitted when the rule has none. Each rule carries the `agent_kind` dispatch resolves for it.
  - `dispatch.default` holds `agent_kind` and `usage_arrival` when the `dispatch.default` block names an agent or a template, and is `null` otherwise. Running, retry, and run-history rows name this selection `default`; a row with no `rule_name` ran on the workflow-wide fallback, which `agent` describes. A catch-all rule takes every issue that no stage rule and no earlier match rule takes, so while one is configured neither `dispatch.default` nor the workflow-wide fallback selects an issue. `config` still serves both, and a client draws no dispatch path through them.
  - `dispatch.chains` lists each stage chain the `next` links form, as rule names in `next` order. A chain starts at a rule that carries `next` and that no `next` names, the chains follow the order of the rules that start them, and a walk stops at a rule without `next`, at a `next` that names no rule, or before a repeated rule. Chains that share a tail list it in each chain. A rule that carries no `next` and that no `next` names belongs to no chain. The list is `[]` when no rule carries `next`.
  - `dispatch.max_consecutive_hops` is the value WORKFLOW.md writes and `null` when the key is absent. `dispatch.hop_ceiling` is the limit a worker exit compares: the written value, or the larger of 10 and the hop count of the longest chain. With no chain no hop occurs, and `hop_ceiling` reports the ceiling a reload that adds `next` would apply.
  - `usage_arrival` appears on `agent` for the workflow-wide fallback, on `dispatch.default`, and on each rule. It is derived the way a session derives it at its start, from the sources the selection's kind attaches, so it follows the kind and never the selection's settings block. When `worker.ssh_hosts` is set, a kind whose session can carry no token figure on a remote launch reports `none`. Its values are those of a running row: `incremental`, `turn_end`, `none`, or `""` for a kind the registry does not hold. A selection reports token usage exactly when the value is `incremental` or `turn_end`. `worker.ssh_hosts` lists each host once, in the order written, and is `[]` in local mode.
  - `reactions` holds a member per reaction kind the process applies, keyed as in WORKFLOW.md, and is `{}` when it applies none; its key order carries no meaning. A retry row's `reaction_config_key` and a pending reaction row's `config_key` name a member of it. `ci_failure` is present exactly while the process runs the CI check, which needs both a CI provider and a source-control adapter. Every other kind is present exactly when it was configured at startup, and `label_commands` when the review command or the fix command was. Each member carries the limits and timing its kind applies: `max_retries` for `ci_failure`, `merge_conflicts`, `auto_merge`, and `merge_completion`; `max_continuation_turns` for `review_comments` and `bot_review`; `debounce_ms` for `review_comments`; `bot_usernames` for `bot_review`; `strategy`, `require_ci`, and `delete_branch` for `auto_merge`; `target_state` for `merge_completion`; and `poll_interval_ms` for every kind except `ci_failure`; and `triage` for `ci_failure`, `review_comments`, `bot_review`, and `merge_conflicts`, true exactly when the kind runs a triage command, as configured at startup.
  - `escalation` is the mode a reaction applies: `label`, `comment`, or `none`. `escalation_label` is present exactly when `escalation` is `label`, and holds the label the reaction applies: the configured value, or `needs-human` where the block writes none. `ci_failure` reads its block on every tick, so when the configuration in force carries no `ci_failure` block while the CI check still runs from startup, it serves `escalation` `none` and `max_retries` `0`, which is what the check applies. `label_commands` has no escalation members; it carries `review_label` while the review command is configured and `fix_label` while the fix command is, and both commands share its one `poll_interval_ms`.
  - `config.token_rates` serves the rates in force, which reload with the workflow. It is `null` when the configuration holds no entry, which is how the shape says `token_rates` is not configured; the price tables can still price runs then. Otherwise it holds a member per entry keyed by the agent kind or the model name as written, with each `*_per_mtok` rate (`null` when not set), `complete`, true exactly when the entry prices on its own (a kind entry when `input_per_mtok` and `output_per_mtok` are both set, a model entry when all four rates are set), and `per_kind`, true exactly when the key names a registered agent kind. An unset cache rate of a kind entry prices that class at the input rate, an unset rate of a model entry comes from the table that lists the model, and `config` serves the configured `null`, never the substituted rate.
  - `config` is an allowlist of typed settings and holds no credential: it carries no tracker endpoint, key, or query filter, no agent command, hook, workspace or database path, or notification destination, no reaction extra key or triage script, and no settings block. Its configuration strings are served unmasked, as the parked `label` is, because they are configuration.
  - A row's `rule_name`, `stage_lineage.previous`, and `reaction_config_key` are those in force when its run was dispatched or the row was stored, while `config` follows the rule above, so each can name a member `config` does not hold. A reload that renames or removes a rule leaves running and retry rows naming it, as does a restart on the edited file for the retry rows it restores, and a row whose `rule_name` is `default` keeps it while `dispatch.default` serves `null`. A client then draws the row from the row's own keys.
- The key is additive; no existing key changes name, type, or meaning: `health` on `GET /api/v1/state` reports Sortie's own checks, so a client draws a failed reload, a failed preflight, or a failing tracker without reading the log. It holds `database`, `preflight`, `workflow`, `poll`, and `scm`.
  - Every check serves one object. `status` is `fail` exactly when `consecutive_failures` is above `0`, and `pass` otherwise. `checked_at` is when the latest run ended. `last_success_at` is when a run last passed. `consecutive_failures` counts the runs that failed since then, the run that reports it included. `failing_since` is when the first of those runs ended, and `null` while the check passes. `error` is the latest failure's text, present exactly when `status` is `fail`, except on `poll`, which has its own rule below. A time Sortie does not have is `null`, never a zero time.
  - A check that has not run reads `pass` with `null` for `checked_at`, `last_success_at`, and `failing_since`, and `0` failures: `status` says whether the latest run failed, not that health was proven, and a `null` `checked_at` tells a client the check has not run. The records are runtime-only, so a clean start reports every check passing, and a restart starts them unrun, except `workflow`, whose initial load is recorded.
  - `database` is the ping each poll tick makes of the store after it applies the configuration and before it reconciles; its `error` is the ping's error text. A tick whose context shutdown canceled records nothing. The readiness probe keeps its own ping per probe.
  - `preflight` is the dispatch preflight each poll tick runs, a pass when validation succeeds. Otherwise its `error` is the text the `dispatch preflight failed` log record carries: every failure's message, joined by `; `. Warnings neither fail the check nor appear in it. The startup preflight is not recorded; the first tick runs at once.
  - `workflow` is every load of the workflow file: the watcher's and the reload each tick's preflight forces. `file` is the base name of the file in effect. `checked_at` is the latest load, successful or not. `last_success_at` is the latest load that promoted its configuration, which is the configuration `config` serves, so while the file loads it advances every tick, and once a load fails it stays at the load the configuration in effect came from. `failing_since` is the first failed load since then, the one an operator's edit caused, and `checked_at` is the latest failed load, which each tick repeats. `error` is the latest load's error text. A failed load changes nothing else: the previous configuration stays in effect and `config` keeps serving it.
  - A failed load also fails that tick's `preflight`, whose `error` then carries the load error after `workflow file cannot be loaded:`, and that tick's `poll` ends `preflight_failed`, so the tick dispatches nothing, though it still applies the configuration, reconciles, and sweeps. `/readyz` keeps reporting `workflow: pass` beside a failing `health.workflow`, because a configuration stays in effect.
  - `poll` has one run per poll tick, whether the timer or `POST /api/v1/refresh` ran it, and its `checked_at` is the last poll time. `outcome` is `succeeded` when the candidate read returned without an error, an empty list included, and a dispatch failure afterwards is not a poll failure. It is `tracker_failed` when the candidate read returned an error, and `preflight_failed` when the tick returned before the read because preflight failed, so the poll never reads healthy while no read reaches the tracker. It is omitted before the first poll. `succeeded` passes and the other two fail.
  - `poll.error` is the text of the latest failed candidate read since the last `succeeded` poll: `tracker_failed` sets it, `succeeded` clears it, and `preflight_failed` leaves it, so it is absent while no read has failed since the last success, and it can be present beside a `preflight_failed` outcome.
  - `poll.next_poll_at` is when the poll timer fires next. It changes only where the loop arms or stops the timer: the start of the loop for the first tick, the re-arm time plus the poll interval after each timer tick, and `null` once shutdown stops the timer, so a draining process shows no poll that will not come. A refresh does not move it, and a value at or before `generated_at` means a poll is due or running.
  - `poll.summary_text` words the poll check in one sentence from the status sentence table: Sortie has not polled the tracker yet; the latest poll read the tracker without an error; or how many polls in a row failed, whether the latest stopped at the dispatch preflight or could not read the tracker, and how long ago a poll last succeeded, or that none has since Sortie started. It is English, ends with a period, states no clock time or date, measures a relative time from `generated_at`, and is masked as Section 15.3 states. It reads `Status not available` when it fails to render, is present whenever `health` is, and is never empty. It carries no error text; `poll.error` serves that.
  - `scm` is `null` when no reaction uses a source-control provider, and otherwise holds `provider`, the provider kind every reaction shares, beside the check object. It records the reads the orchestrator makes of the provider: the pull request reviews and bot review comments, the review decision, the CI status of a pull request and of a head commit, the mergeability, the label events, and the auto-merge token-scope verification. A read that returns without an error is a pass. A read that returns an error is a failure with the error's text, a not-found answer included, because it can end the reaction and a provider answers not found to a token that cannot see the repository. A read that shutdown canceled records nothing.
  - `scm` records no write: merging, deleting a branch, and removing a label leave it untouched, whatever they return, because providers word a refused write in their own statuses and the reaction handles it. It runs only when a reaction reads the provider, and it reflects the most recent read across every pull request, so one pull request's read that fails can be reset by another's that passes, and `checked_at` dates that read. The reads made by startup recovery and by the startup token-scope checks are not recorded.
  - Every `error` is masked as Section 15.3 states, in the snapshot rather than in the record, so a value registered as a secret after the failure is masked too, and the unmasked text is served nowhere. Before masking, the snapshot shortens the workflow file's absolute path in the `workflow` and `preflight` error text to its base name, as `file` names it. The snapshot reads the records and the load outcome in memory and performs no I/O.
  - The readiness probe's checks, status codes, and draining behavior are unchanged. Its `workflow` check passes while a configuration is in effect, its `preflight` reads the result the poll tick stores where `health.preflight` records it, and its `database` pings per probe.
- The key is additive; no existing key changes name, type, or meaning: `version` is the Sortie version the readiness probe serves, and `started_at` is the time the process started, in UTC. The process start is taken before the startup work, so `started_at` and the readiness probe's `uptime_seconds` name one instant, and both count the startup duration.
- Every row of `running`, `retrying`, `budget_exhausted`, `parked`, `pending_reactions`, `between_stages`, and `held`, on `GET /api/v1/state` and in the per-issue response, every entry of `active`, the per-issue response itself, and every `issue_outcome` carry `status_text` and `status_text_full`: a plain English sentence that says what the issue is doing, what it waits on, or how it ended. `status_text` is short, for a row or a lane; `status_text_full` is long, for the issue page. The keys are additive; no existing key changes name, type, or meaning, and every structured field a sentence is built from stays served, so a client that needs data does not parse text.
  - Both keys are always present and never empty. A short sentence ends without a period and a long one with a period. Sentences are English and are not translated.
  - A sentence does not name what the response carrying it already shows. No long sentence, next outcome, or attention item's `why_text` or `clears_text` names the issue key, so the issue reads as "the issue" or is left implied. Only an operator action's `outcome_text`, which a client shows on its own, names the issue.
  - Sortie renders each sentence on the request, from the snapshot, the `config` the state route serves, and run history, and stores none. A relative time in a sentence, such as "in 40s", is measured from `generated_at`.
  - A sentence states only a fact Sortie records for the row, in `config`, or in run history. A figure Sortie does not have reads as not measured, never as zero, and a figure that is only a lower bound reads as at least. A sentence states no cost (one that did would word it as an estimate) and no clock time or date. It uses stage wording only when the workflow has stage chains.
  - Every sentence is masked as Section 15.3 states, so a value registered as a secret after it was stored is masked too.
  - A sentence that fails to render is served as `Status not available` and the failure is logged. The route answers as it would otherwise.
  - A hold's `reason_text` and `release_text` come from the same table: the short and the clears form of one entry, the same entry whose short and long forms are a held row's `status_text` and `status_text_full`. Neither the long form nor `release_text` names the held issue's key; a blocker's key appears because it names another issue.
  - Attention items, agent session events, operator action outcomes, next outcomes, the poll check's sentence, and the stall sentences take their wording from the same table of templates. Notification bodies keep the text the renderer builds.
- Every row of `running`, on `GET /api/v1/state` and in the per-issue response, carries `stall_text`, `stall_text_full`, and `stall_off_text`, so every client states a stall in the same words. `stall_text` is short and `stall_text_full` long, and both are `null` until `generated_at` reaches `quiet_at` and while stall detection is off. They read the silence from the row's reference point (`last_event_at`, else `started_at`) to `generated_at`, rounded down, and the timeout as `stall_at` less the reference point. The short form reads "No events for <silence>; Sortie stops it after <timeout>". `stall_off_text` is `null` whenever `quiet_at` is not, and otherwise says that stall detection is off and Sortie will not stop the session as stalled. The text is true as of `generated_at` and stays true until the next event; a client refetches at `quiet_at` to get it. A list row shows `stall_text` and so keeps no marker with stall detection off.
- `next_outcomes` on `GET /api/v1/<issue_identifier>` lists what Sortie does next for the issue, so a client shows the branches ahead and interprets no Sortie state. The key is additive; no existing key changes name, type, or meaning. Each member holds `condition`, a capitalized phrase without a final period, and `text`, one or two sentences that each end with a period.
  - Sortie renders the list on the request, in English, from the snapshot, the `config` the state route serves, and run history, and stores none. Every member is masked as Section 15.3 states, and the same rules as for a status sentence apply: only a fact recorded for the row, in `config`, or in run history, no cost, no clock time or date, and stage wording only when the workflow has stage chains.
  - The list is derived from the work session's exit, the issue's retry, the wait for a stage label, and each pending reaction of the issue, in that order. A running reaction continuation or label command run contributes no exit pair; the pending rows of the issue say what follows it. A held or finished issue lists only the pairs of its pending reactions, and `[]` when it has none.
  - A pair never names a reaction whose `config` member is absent, nor a stage the workflow's rules do not hold.
  - A pair names the pull request it watches as "the pull request", and by number only when the issue's pending reactions watch more than one pull request.
  - A condition on a watched reaction is worded as what a check finds, and carries the time left in which a check can still lead to the outcome, measured from `generated_at`: the time to `watch_ends_at`, less what a found event still waits for. A reaction with no time left yields no pair.
  - An outcome that starts a session names the budget hold when a budget gate would stop it: the money budget, the token budget, or the session budget, in that precedence, which is the order the dispatch gates apply. The money budget is named by its setting and never by an amount, because a next outcome states no cost.
  - A running row whose agent kind reports usage lists, before the pair for the token budget, a pair for the money budget when the workflow sets a money ceiling: the session stops and the issue is held at the money budget. An outcome that a configured triage command replaces says that the triage command runs, and an outcome that a spent count replaces says that the reaction escalates, in the order the kind's pass decides them.
  - The list is not exhaustive. It leaves out what the configuration does not decide: a stop the agent requests, a person's tracker or pull request edit other than a label command, a failed tracker, forge, or store call, a withheld handoff and the park it leads to, a spend ceiling hold, and a wait for a free agent slot. A client does not read a missing branch as one that cannot happen.
  - A pair whose condition or text fails to render is left out of the list and the failure is logged. The route answers as it would otherwise.
- Endpoints should be read-only except for operational triggers like `/refresh`, `/retries/{identifier}/run`, `/attention/{id}/acknowledge`, and `/reactions/{identifier}/{kind}/check`.
- Unsupported methods on defined routes should return `405 Method Not Allowed`, except that a request refused as cross-origin answers `403`.
- API errors should use a JSON envelope such as `{"error":{"code":"...","message":"..."}}`.
- The dashboard consumes this API and holds no state logic of its own.

#### 13.7.3 Prometheus Metrics Endpoint (`/metrics`)

When the HTTP server is enabled, Sortie exposes a Prometheus exposition-format scrape endpoint at `/metrics`, backed by a dedicated registry so the process exports only its own series and none of the runtime's defaults. See ADR-0008. The endpoint is co-located with the JSON API and dashboard on the same address and port; no separate configuration is required.

Implementation requirements:

- Use a dedicated `prometheus.Registry` (not the global default) to prevent pollution from unrelated collectors and to enable isolated test assertions.
- Register the handler via `promhttp.InstrumentMetricHandler(registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))`. `InstrumentMetricHandler` wraps `HandlerFor` and registers `promhttp_metric_handler_*` counters on the same dedicated registry automatically, ensuring scrape self-instrumentation appears in scrape output rather than landing silently on the global default.
- Register standard Go runtime and process collectors (`collectors.NewGoCollector`, `collectors.NewProcessCollector`) on the dedicated registry so that `go_*` and `process_*` metrics appear in scrape output alongside Sortie's own metrics.

Defined metrics (label sets and buckets are specified here; see ADR-0008 for historical rationale):

| Name | Type | Description |
|------|------|-------------|
| `sortie_sessions_running` | Gauge | Number of agent sessions currently executing. |
| `sortie_sessions_retrying` | Gauge | Number of issues in the retry queue. |
| `sortie_slots_available` | Gauge | Remaining dispatch capacity under current concurrency limits. |
| `sortie_active_sessions_elapsed_seconds` | Gauge | Cumulative wall-clock elapsed time across all currently running sessions. |
| `sortie_ssh_host_usage{host}` | Gauge | Current session count per remote SSH host, partitioned by host. Recomputed from the host-usage map after each tick, worker exit, and retry-timer event; hosts removed by config reload decrement to zero rather than freezing at their last value. |
| `sortie_tokens_total{type,model}` | Counter | Tokens consumed, partitioned by type (`input`, `output`, `cache_read`, `cache_write`) and by model. `cache_read` and `cache_write` are subsets of `input`, so summing across every `type` double-counts them. A run that spends tokens on several models counts each model's share under that model. |
| `sortie_agent_runtime_seconds_total{model}` | Counter | Cumulative agent-session wall-clock time for completed sessions, partitioned by model. Every model a settings block configures (`<none>` when a block configures none) and every model an earlier run reported or spent tokens on exports a series at `0` from start-up; a model none of these name gets its series when a run first reports it, so a query filtered to such a model returns no data before then and an alert on it uses `or vector(0)`. |
| `sortie_dispatches_total{outcome}` | Counter | Dispatch attempts, partitioned by outcome (`success`, `error`). |
| `sortie_worker_exits_total{exit_type,model}` | Counter | Worker exits, partitioned by exit type (`normal`, `error`, `cancelled`, `soft_stop`) and by model. A soft-stop exit reports `soft_stop` rather than the exit kind it would otherwise map to. |
| `sortie_retries_total{trigger}` | Counter | Retry schedule events, partitioned by trigger (`error`, `usage_limit`, `continuation`, `timer`, `stall`, `ci_fix`). A review, bot review, merge conflict, or label command continuation records no trigger. |
| `sortie_reconciliation_actions_total{action}` | Counter | Reconciliation outcomes per issue, partitioned by action (`stop`, `cleanup`, `keep`, `sweep_cleanup`, `sweep_expired`). |
| `sortie_poll_cycles_total{result}` | Counter | Poll tick completions, partitioned by result (`success`, `error`, `skipped`). |
| `sortie_tracker_requests_total{operation,result}` | Counter | Tracker adapter API calls, partitioned by operation (`fetch_candidates`, `fetch_issue`, `fetch_by_states`, `fetch_states_by_ids`, `fetch_states_by_identifiers`, `fetch_comments`, `fetch_blockers`, `transition`, `comment`, `add_label`, `remove_label`) and result (`success`, `error`). |
| `sortie_tracker_comments_total{lifecycle,result}` | Counter | Tracker comment attempts, partitioned by lifecycle point (`dispatch`, `completion`, `failure`, `budget_hold`, among others the project may add) and result (`success`, `error`). Recorded from the `tracker_comment` destination's outcome, and only for an event that destination received; an event routed elsewhere records nothing. |
| `sortie_handoff_transitions_total{result}` | Counter | Handoff-state dispositions, partitioned by result (`success`, `error`, `skipped`, `withheld`). `withheld` means the handoff-evidence policy selected the absence failure path before any transition attempt, distinguishing it from an ordinary worker failure; a withholding verdict whose own verification read then finds the issue terminal does not increment this label, it increments `skipped` instead. `skipped` names three causes and does not distinguish them: the issue was no longer in an active state at worker exit, the issue was already reported terminal and the handoff write was suppressed (Section 11.5), or a withheld verdict's own verification read reported the issue terminal and suppressed the absence-failure recording (Section 14.2). All four values are counted only when `tracker.handoff_state` is configured. |
| `sortie_dispatch_transitions_total{result}` | Counter | Dispatch-time in-progress transition attempts, partitioned by result (`success`, `error`, `skipped`). `skipped` indicates the issue was already in the target state. |
| `sortie_issue_parks_total{reason}` | Counter | Issue park events, partitioned by reason (`agent_blocked`, `handoff_absence`). Incremented once per park, whichever trigger produced it. |
| `sortie_dispatch_rule_match_total{layer,rule}` | Counter | Dispatch rule match outcomes, partitioned by resolution layer (`rule`, `default`, `fallback`) and matched rule name. Empty rule names report as `<none>` to bound label cardinality. |
| `sortie_candidate_holds_total{reason}` | Counter | `IncCandidateHolds`. Candidates the dispatch loop held, partitioned by reason (`blocked_by`, `blockers_unresolved`, `blockers_not_read`, `blockers_incomplete`, `daily_spend_ceiling`, `weekly_spend_ceiling`, `monthly_spend_ceiling`). Incremented once per held candidate, a spend reason once per candidate that passes every other gate and that the instance spend hold alone keeps from starting, on every tick the hold stands; never incremented for a candidate rejected by an eligibility or capacity gate, and never incremented a second time for the pass-level `blocker reads halted for this tick` ERROR that accompanies a run of `blockers_unresolved` holds. |
| `sortie_budget_exhaustions_total{reason}` | Counter | `IncBudgetExhaustions`. Issue entries into the per-issue budget-exhausted set, partitioned by reason (`money_budget`, `token_budget`, `session_budget`, open to a later value). Incremented once per hold, from whichever lane discovered it; never incremented on a tick that merely re-observes an already-announced hold. |
| `sortie_budget_exhausted_issues{reason}` | Gauge | `SetBudgetExhaustedIssues`. Issues currently held out of dispatch, partitioned by reason. Recomputed from the full budget-exhausted set on every gauge update, including every declared reason at zero when nothing is held, so a reason that clears reports zero rather than freezing at its last value. |
| `sortie_runs_stopped_by_budget_total{reason}` | Counter | `IncRunsStoppedByBudget`. Runs recorded `budget_stopped`, partitioned by reason. Only `token_budget` and `money_budget` are reachable: a session is a discrete unit, so the session ceiling cannot be crossed part-way through one. Incremented once per run whose exit is recorded `budget_stopped`, at that exit. |
| `sortie_tool_calls_total{tool,result}` | Counter | Agent tool call completions, partitioned by tool and result (`success`, `error`). `tool` is the name the runtime gives the tool, `{server}.{tool}` for an MCP or dynamic tool; the call's kind when the runtime named none, and `other` when the call has neither or the name holds a character outside ASCII letters, digits, `_`, `-`, `.`, and `:`. A command line or other argument text never becomes a label. |
| `sortie_ci_status_checks_total{result}` | Counter | CI status check outcomes, partitioned by result (`passing`, `pending`, `failing`, `error`). |
| `sortie_ci_escalations_total{action}` | Counter | CI escalation actions when fix retries are exhausted, partitioned by action (`label`, `comment`, `none`, `error`). Under `escalation: comment` or `none`, `comment` records an accepted `tracker_comment` write, `error` a failed one, and `none` an escalation event that `tracker_comment` did not receive. |
| `sortie_reactions_auto_merge_total{result}` | Counter | Auto-merge reaction outcomes, partitioned by result (`merged`, `error`, `escalated`). |
| `sortie_review_checks_total{result}` | Counter | Review comment check outcomes, partitioned by result (`dispatched`, `error`, `skipped`). |
| `sortie_review_escalations_total{action}` | Counter | Review escalation actions when continuation turns are exhausted, partitioned by action (`label`, `comment`, `none`, `error`), with the same meaning of `comment`, `none`, and `error` as `sortie_ci_escalations_total`. |
| `sortie_self_review_iterations_total{verdict}` | Counter | Self-review iterations, partitioned by per-iteration verdict (`pass`, `iterate`, `none`). `none` indicates a missing or unparseable verdict for that iteration. |
| `sortie_self_review_sessions_total{final_verdict}` | Counter | Self-review sessions, partitioned by final verdict (`pass`, `iterate`, `none`). |
| `sortie_self_review_cap_reached_total` | Counter | Self-review sessions that reached the iteration cap without a passing verdict. |
| `sortie_poll_duration_seconds` | Histogram | Wall-clock time per poll cycle; buckets via `ExponentialBuckets(0.1, 2, 10)` (0.1 s–51.2 s). |
| `sortie_worker_duration_seconds{exit_type}` | Histogram | Worker session wall-clock time; buckets via `ExponentialBuckets(10, 2, 12)` (10 s–5.7 h). `exit_type` carries the same values as `sortie_worker_exits_total`. |
| `sortie_self_review_verification_duration_seconds{command}` | Histogram | Per-command verification duration during self-review; buckets via `ExponentialBuckets(10, 2, 12)` (10 s–5.7 h). The `command` label is truncated to the first 64 characters. |
| `sortie_build_info{version,go_version}` | Gauge | Always `1`; carries build metadata as labels. |

The `model` label on the token, agent-runtime, and worker-exit metrics names the run's model by one rule: the model carried by the event that reports the usage, otherwise the model the run last reported, otherwise the model configured for the run, otherwise `<none>`, the placeholder `sortie stats` uses for the same case. Token samples count as usage arrives. When the agent reports how many tokens each model spent, each model's share of a sample counts under that model, never more than the sample, and only the rest follows this rule. Usage recorded before a run's first model report counts under the configured model or `<none>` and is not moved when the model arrives. A run whose runtime switches model splits its tokens between the models. Usage an agent reports without a per-model count counts under the run's model at that moment. Runtime and exit samples are recorded when the session ends, after the run's reported model is final, so they carry the run's final model: reported, else configured, else `<none>`. Label values come only from model names in the workflow configuration and from agent reports, passed verbatim, so the label is bounded by the models in use and never grows with issues or runs.

`sortie_worker_duration_seconds` carries no `model` label: as a histogram it would multiply its bucket series per model, and the mean session duration per model is the runtime counter over the exit counter, each summed `by (model)`. A configured model name that the agent reports under a different, resolved name keeps its series at its true total, usually a flat `0`, beside the series of the reported name; the same holds for a model only earlier runs used. On a default Prometheus, `rate()` and `increase()` miss the first sample of a model the deployment has never run before. Enabling the Prometheus feature flag `--enable-feature=created-timestamp-zero-ingestion`, still experimental in Prometheus, closes that gap with no Sortie setting: the endpoint sends each series' creation time in the protobuf format the flag makes Prometheus request, so a scrape job that sets `scrape_protocols` keeps `PrometheusProto` in the list.

#### 13.7.4 Dashboard application (`/ui/`)

The server serves a client-side application that is compiled into the binary, under `/ui/` with path routing. `GET /` answers `302 Found` with `Location: /ui/`. The application document and its files are read only from the compiled-in output, never from the filesystem.

The route answers `GET` and `HEAD`. Any other method on a path under `/ui/` answers `405 Method Not Allowed` with `Allow: GET, HEAD`, and `GET /ui` redirects to `/ui/`. No path outside `/ui/` answers the application document, and the route does not overlap `/api/v1/*`, `/metrics`, `/livez`, or `/readyz`.

| Request path | Status | Body | Headers |
|---|---|---|---|
| `/ui/assets/{name}` naming a regular file whose extension is in the fixed content-type table | 200 | the file | `Content-Type` from the table; `Cache-Control: public, max-age=31536000, immutable`; `X-Content-Type-Options: nosniff`; `Content-Security-Policy` |
| `/ui/assets/{name}` naming nothing, a directory (`/ui/assets/` included), or a file whose extension is outside the table | 404 | `404 page not found` | those of a plain-text 404 |
| any other path under `/ui/`, such as `/ui/`, `/ui/index.html`, `/ui/assets`, `/ui/issues/BP-1` | 200 | the application document | `Content-Type: text/html; charset=utf-8`; `Cache-Control: no-cache`; `ETag`; `X-Content-Type-Options: nosniff`; `Content-Security-Policy` |
| the same, with `If-None-Match` matching the `ETag` | 304 | empty | `Cache-Control`, `ETag`, `X-Content-Type-Options`, `Content-Security-Policy` |

The document answers every path under `/ui/` that is not an asset, so a deep link renders the application, which resolves the path itself.

- Every response that carries application bytes carries `X-Content-Type-Options: nosniff` and a `Content-Security-Policy` header with this policy: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`.
- The `ETag` is the quoted lowercase hexadecimal SHA-256 digest of the application document, a strong validator computed once at startup. Assets carry no validator, and no response carries `Last-Modified`. Conditional requests, ranges, and `HEAD` follow standard content-serving semantics.
- The content type of an asset comes from a fixed table, never from the host's media type registry: `.js` is `text/javascript; charset=utf-8` and `.css` is `text/css; charset=utf-8`. A host registry can override its built-in types, and a browser refuses a stylesheet or module script served under a wrong type. An extension outside the table answers `404`, and a later output that emits another type adds a row.
- The application document loads only files under `/ui/assets/` and carries no inline script, `style` element, or `style` attribute, so the policy applies without `'unsafe-inline'`.

### 13.8 Self-Review Observability on an Admitting Status Signal

A run that ends on the `needs-human-review` status signal, or on a `no-change-needed` declaration, and passes through the self-review phase records review metadata on `run_history` exactly as a run that reached the phase by exhausting the turn budget does: the JSON document under `review_metadata`, described in Section 19, carries a value rather than null for this population.

The `after_run` hook on that path receives the run's real `SORTIE_SELF_REVIEW_STATUS` value (`passed`, `cap_reached`, or `error`) instead of the `disabled` value it receives when self-review is not configured or the run did not reach the phase. `SORTIE_SELF_REVIEW_SUMMARY_PATH` is populated on the same terms as on any other path that ran the phase. A declared run supplies both values on these same terms whether the phase confirms the declaration or retracts it: the retraction is a separate outcome recorded in the run's own log, not a change to what this section populates.

### 13.9 Agent event retention

Sortie retains the agent events of each run, so an operator sees what an agent does while it runs and what a run did before they looked. The unit is one run, one dispatch, keyed by its dispatch ID. Dispatch IDs are minted per dispatch and never reused, across restarts included, so a cursor can never match another run. Each run numbers its retained events with `seq`, from 1, rising by one per retained event; eviction never renumbers.

Retention has two tiers, bounded by count and by bytes:

- **Memory.** Each running run keeps its newest 1,000 retained events, and at most 256 KiB measured as the sum of the encoded events, whichever bound is reached first. The oldest events are evicted, and `seq` keeps rising.
- **Store.** At worker exit the run's buffer is written to the `run_events` table once, in the transaction that appends the run's `run_history` row (Section 19.2), and the memory buffer is then released. When the events cannot be written, the history row is still stored, and Sortie holds the events in memory and retries writing them up to five times, off the event loop, waiting 4, 8, 16, 32, and 64 seconds before the successive retries. Held events total at most 4 MiB of encoded events; holding a run that would exceed that drops the oldest held run first. Events the last retry fails to write, events dropped for room, and events still held when Sortie stops are lost and logged at `Warn`; such a run, like one whose events are still held, is not retained. No event is written to the store while the run is running.
- **Limits.** A finished run's events stay readable for at least 365 days after the run's `completed_at`; pruning runs only when a later run is written, so an instance that writes none keeps serving them. The stored events of every run together hold at most 2 GiB of encoded events. Each write first deletes the events of at most 4 runs that ended more than 365 days ago, so expired events can outlast 365 days until later writes reach them, then deletes whole runs, oldest `completed_at` first, until the total is within 2 GiB, never the run being written.
- **Durability** is that of the run's `run_history` row: a graceful shutdown ends every worker and keeps both, and a crash loses both for every run in flight.

A run's events are not retained when its stored events were pruned, when they were not written, when it was lost in a crash, when it was recorded before retention existed, when it retained no event, and when no run carries the dispatch ID. Every surface says so explicitly: the run-events route answers `404`, the change stream answers a subscription with a `not_retained` reset, and `recent_events` is `[]`. A run that kept only the tail of its events serves `first_seq` above 1, so a client says that earlier events are gone.

Every normalized event type of Section 10.3 is retained, except:

- a `notification` or `other_message` with a blank message, which is a liveness ping;
- `token_usage`, whose run totals attach to the turn-ending events instead;
- a `notification`, `other_message`, or `malformed` event whose type and message equal those of the run's newest retained event.

A session that fails to start gets a `startup_failed` event from Sortie when its adapter emitted none, its message the start error. A session stopped by Sortie while starting retains none, and a run never retains two.

An event that is not retained still advances `last_event_at` and still produces a change signal.

A retained event, as every surface serves it:

| Key | Present on | Value |
|---|---|---|
| `seq` | every event | The event's number within the run |
| `at` | every event | The event's timestamp, RFC 3339 in UTC |
| `event` | every event | The normalized event type |
| `message` | every event | The event's message, masked as Section 15.3 states and cut to 999 runes followed by `…` when it is retained, so it never exceeds 1,000 runes, and masked again when it is served; `""` when the event carries none |
| `text` | every event | One operator-facing sentence the server words from the event when it serves it (Section 13.6); never stored |
| `session_id` | `session_started` | The session the run started |
| `tool_name`, `duration_ms`, `tool_error` | `tool_result` | The tool, its duration in milliseconds, and whether it returned an error; `tool_name` and `duration_ms` are omitted when the adapter reported none. `tool_name` follows Section 10.3: the name the runtime gives the tool, `{server}.{tool}` for an MCP or dynamic tool |
| `tool_kind` | `tool_result` | `shell`, `read`, `edit`, or `skill`; omitted for any other tool and for an event recorded before the member existed |
| `tool_argument` | `tool_result` | The call's one main argument (Section 10.3), cleaned, masked as Section 15.3 states, and cut to 999 runes followed by `…` when it is retained, and masked again when it is served; omitted when the call has none and for an event recorded before the member existed |
| `tool_line` | `tool_result` | The call's line text (Section 13.6), worded from the masked name and argument when the event is served; never stored |
| `usage` | `turn_completed`, `turn_failed`, `turn_cancelled`, `turn_ended_with_error`, `turn_input_required` | The run's token totals after the event, with the members of a running row's `tokens` and no uncached figure, which a client derives as `total_tokens` less `cache_read_tokens`; `null` when the run has measured no usage, as a run whose resolved `usage_arrival` is `none` never has, so an unmeasured figure never reads as zero |

Masking precedes the cut, so a secret straddling the bound is never half revealed. The agent process ID, usage windows, the model name, and the API duration are not retained: the process ID and the model say nothing about the run's progress, the newest report of each usage window per agent kind is already in the snapshot, and the API duration is part of a total the run already reports.

No surface exposes per-session log files or their host paths. Sortie writes no per-session log file, a host path on an unauthenticated API discloses the host's layout, and a raw runtime transcript bypasses the normalized vocabulary.

A client that stops reading never delays the event loop:

- The event loop appends an event to the run's buffer under the buffer's own lock, which is never held across I/O, and the change signal for the event wakes every open stream through its flag of capacity 1 with a non-blocking send, the mechanism change signals use. A stream reads only the runs it subscribes to.
- Each stream registers its flag before its first read. It then copies the events after its cursor, from memory while the run runs and from the stored copy once it has ended, writes them as one batch with each frame under its own 2-second write deadline, waits on its flag, and repeats. Registration before the first read and reads by cursor leave no window in which an event is lost or written twice, and at worker exit the stored copy is written before memory is released, so a stream moving from one to the other finds every event in one place or the other.
- Every read checks the cursor against retention, and a stream whose cursor fell behind gets a `cursor_expired` reset. A stream that misses its write deadline is closed, and its client reconnects with its cursor. No stream holds a queue of its own.
- A stream that never reads, while 10,000 events are appended to its run, leaves every append wait-free, and is closed within 2 seconds of its blocked write.

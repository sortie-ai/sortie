## 16. Reference Algorithms

### 16.1 Service Startup

```text
function start_service():
  configure_logging()
  start_observability_outputs()
  start_workflow_watch(on_change=reload_and_reapply_workflow)

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    fail_startup(validation)

  open_or_create_sqlite_db()
  run_schema_migrations()

  persisted_retries = sqlite.load_retry_entries()

  state = {
    poll_interval_ms: get_config_poll_interval_ms(),
    max_concurrent_agents: get_config_max_concurrent_agents(),
    running: {},
    claimed: set(),
    retry_attempts: {},
    completed: set(),
    agent_totals: {input_tokens: 0, output_tokens: 0, total_tokens: 0, seconds_running: 0},
    agent_rate_limits: null
  }

  for entry in persisted_retries:
    state = reconstruct_retry_timer(state, entry)

  startup_terminal_workspace_cleanup()
  schedule_tick(delay_ms=0)

  event_loop(state)
```

### 16.2 Poll-and-Dispatch Tick

```text
on_tick(state):
  # Preflight forces a defensive reload, so it runs before every step
  # that reads config, not only before dispatch.
  validation = validate_dispatch_config()
  state = apply_config_to_state(state, current_config())
  report_new_advisories(current_config().advisories() + worker_config().advisories())

  state = reconcile_running_issues(state)

  state.sweep_tick_counter += 1
  if state.sweep_tick_counter >= sweep_every_n_ticks:
    state.sweep_tick_counter = 0
    sweep_workspaces(state)

  if validation is not ok:
    log_validation_error(validation)
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  issues = tracker.fetch_candidate_issues()
  if issues failed:
    log_tracker_error()
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  sorted = sort_for_dispatch(issues)

  state = rebuild_budget_exhausted(state, sorted)
  state = refresh_parked_issues(state, sorted)
  state = park_exhausted_absences(state, sorted)

  for issue in sorted:
    if no_available_slots(state):
      break

    if should_dispatch(issue, state):
      issue, dispatchable = hold_for_stage_hop(issue, state)   // Section 16.4
      if not dispatchable:
        continue
      # The seed holds the pull request's review comments no run of the
      # issue was given, per configured kind without a triage block; nil
      # dispatches as before.
      seed = fresh_run_seed(issue, state)
      state = dispatch_issue(issue, state, attempt=null, continuation_context=seed)

  notify_observers()
  schedule_tick(state.poll_interval_ms)
  return state
```

### 16.3 Reconcile Active Runs

```text
function reconcile_running_issues(state):
  state = reconcile_stalled_runs(state)

  running_ids = keys(state.running)
  if running_ids is empty:
    state = reconcile_ci_status(state)
    state = reconcile_review_comments(state)
    return state

  refreshed = tracker.fetch_issue_states_by_ids(running_ids)
  if refreshed failed:
    log_debug("keep workers running")
    state = reconcile_ci_status(state)
    state = reconcile_review_comments(state)
    return state

  for issue in refreshed:
    if issue.state in terminal_states:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=true)
    else if issue.state in active_states:
      state.running[issue.id].issue = issue
    else:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=false)

  state = reconcile_ci_status(state)
  state = reconcile_review_comments(state)
  return state
```

```text
function reconcile_ci_status(state):
  if ci_provider is nil:
    return state

  for key, pending in state.pending_reactions where pending.kind == "ci":
    delete(state.pending_reactions, key)

    ref = pending.sha or pending.branch
    result, err = ci_provider.fetch_ci_status(ref)

    if err:
      log_warn("CI status fetch failed, will retry next tick")
      state.pending_reactions[key] = pending
      continue

    switch result.status:
      case "passing":
        delete(state.reaction_attempts, key)
      case "pending":
        state.pending_reactions[key] = pending
      case "failing":
        handle_ci_failure(state, pending, result)

  return state
```

```text
function reconcile_review_comments(state):
  if scm_adapter is nil:
    return state

  now = utc_now()

  for key, pending in state.pending_reactions where pending.kind == "review":
    delete(state.pending_reactions, key)
    data = pending.kind_data  # ReviewReactionData
    rkey = reaction_key(pending.issue_id, "review")

    # Watch window; a spent counter survives the drop. The cached
    # handed-off set is dropped at any counter value, its rows are kept
    if review_config.watch_window_ms > 0 and now - pending.created_at > review_config.watch_window_ms:
      cancel_triage(pending)
      if state.reaction_attempts[rkey] < review_config.max_continuation_turns:
        delete(state.reaction_attempts, rkey)
      delete(state.handed_off_comments, rkey)
      log_warn("review watch window elapsed, dropping")
      continue

    # Poll throttle
    if now < pending.pending_retry_at:
      state.pending_reactions[key] = pending
      continue

    # An unfinished triage run makes this pass's fetch redundant
    if pending.triage is set and not triage_finished(pending.triage):
      pending.pending_retry_at = now
      state.pending_reactions[key] = pending
      continue

    # Fetch reviews from SCM adapter
    comments, err = scm_adapter.fetch_pending_reviews(data.pr_number, data.owner, data.repo)
    if err:
      pending.pending_attempts++
      pending.pending_retry_at = now + backoff(pending.pending_attempts)
      state.pending_reactions[key] = pending
      log_warn("review fetch failed, retrying with backoff")
      continue

    # Filter outdated and allowlisted authors, compute debounce timestamp
    actionable = filter(comments, c -> not c.outdated and not is_allowlisted_bot(c.reviewer))
    max_time = max(c.submitted_at for c in actionable)

    if len(actionable) == 0:
      cancel_triage(pending)
      pending.pending_retry_at = now + poll_interval
      state.pending_reactions[key] = pending
      continue

    # Fingerprint from sorted non-outdated comment IDs
    fingerprint = sha256(sorted(c.id for c in actionable))

    # Dedup via reaction_fingerprints table
    store.upsert_reaction_fingerprint(pending.issue_id, "review", fingerprint)
    stored_fp, dispatched = store.get_reaction_fingerprint(pending.issue_id, "review")
    if stored_fp == fingerprint and dispatched:
      pending.pending_retry_at = now + poll_interval
      state.pending_reactions[key] = pending
      continue

    # Handed-off check, also when the fingerprint read failed. A comment is
    # new when it is outside the handed-off set and the running turn's IDs;
    # created_at stays untouched
    if not loaded(state.handed_off_comments[rkey]):
      ids, err = store.list_reaction_handoffs(pending.issue_id, "review")
      if err:
        log_warn("failed to load handed-off comments, deferring")
        pending.pending_retry_at = now + poll_interval
        state.pending_reactions[key] = pending
        continue
      state.handed_off_comments[rkey] = set(ids)
    if not any(is_new_comment(state, pending.issue_id, "review", c) for c in actionable):
      if all(c.id in state.handed_off_comments[rkey] for c in actionable):
        store.mark_reaction_dispatched(pending.issue_id, "review")
      pending.pending_retry_at = now + poll_interval
      state.pending_reactions[key] = pending
      continue

    # Debounce
    if max_time is set and now - max_time < debounce_ms:
      pending.pending_retry_at = max_time + debounce_ms
      state.pending_reactions[key] = pending
      continue

    # Retry slot arbitration
    if retry_slot_incumbent(state, pending.issue_id) is not nil:
      pending.created_at = now
      state.pending_reactions[key] = pending
      continue

    # Continuation budget; a set reaching this point holds a new comment
    turn_count = state.reaction_attempts[rkey]
    if turn_count >= review_config.max_continuation_turns:
      escalate_review_failure(state, pending, turn_count)
      continue

    review_context = build_review_template_map(actionable)

    # Triage gate, above the dispatch counter
    verdict = triage_gate(state, pending, review_config.triage, review_context)
    if verdict in ("wait", "handled"):
      pending.pending_retry_at = now + poll_interval
      state.pending_reactions[key] = pending
      continue
    if verdict == "escalate":
      escalate_review_failure(state, pending, turn_count)
      continue

    # The fingerprint is marked dispatched by the retry handler, after the
    # scheduled retry fires and dispatch succeeds
    schedule_retry(state, pending.issue_id, pending.attempt, {
      identifier: pending.identifier,
      delay_type: continuation,
      continuation_context: {"review_comments": review_context},
      reaction_kind: "review"
    })
    state.reaction_attempts[rkey]++
    # The comments join the handed-off set when the run exits normally,
    # not here (Section 16.6)

  return state
```

### 16.4 Dispatch One Issue

```text
function dispatch_issue(issue, state, attempt):
  (agent_kind, template_id, rule_name) = resolve_rule(
    issue, state.dispatch_cfg, hop_route_of(state, issue.id),
    state.default_agent_kind, state.default_template_id
  )
  stage_previous = fresh_previous(state, issue.id, rule_name)

  // The selection is frozen per claim; the settings are resolved for every attempt.
  attempt_settings = resolve_attempt_settings(state.cfg, (agent_kind, rule_name), ssh_host)
  if attempt_settings.refusals is not empty:
    release_host(issue.id)
    log_error("agent settings refused")
    return state

  worker = spawn_worker(
    fn -> run_agent_attempt(issue, attempt, attempt_settings.settings, parent_orchestrator_pid) end
  )

  if worker spawn failed:
    return schedule_retry(state, issue.id, next_attempt(attempt), {
      identifier: issue.identifier,
      error: "failed to spawn agent"
    })

  state.running[issue.id] = {
    worker_handle,
    monitor_handle,
    identifier: issue.identifier,
    issue,
    agent_kind,
    template_id,
    rule_name,
    stage_previous,
    rule_settings_applied: attempt_settings.settings.rule_name != "",
    configured_model: attempt_settings.settings.model,
    configured_effort: attempt_settings.settings.effort,
    session_id: null,
    agent_pid: null,
    last_agent_message: null,
    last_agent_event: null,
    last_agent_timestamp: null,
    agent_input_tokens: 0,
    agent_output_tokens: 0,
    agent_total_tokens: 0,
    last_reported_input_tokens: 0,
    last_reported_output_tokens: 0,
    last_reported_total_tokens: 0,
    retry_attempt: normalize_attempt(attempt),
    started_at: now_utc()
  }

  state.claimed.add(issue.id)
  state.retry_attempts.remove(issue.id)
  return state
```

The `resolve_rule` call takes the hop route first, then the stage labels, and then evaluates the rules without a stage label in order, returning the first match:

```text
function resolve_rule(issue, dispatch_cfg, hop, default_agent_kind, default_template_id):
  // The hop route names the rule the issue's latest hop advanced to. It
  // wins while the issue carries the target's stage label, so a leftover
  // label of a downstream stage cannot pull the issue past its target.
  if hop.target_rule is set and issue.labels contains hop.target_label (case-insensitive):
    rule = dispatch_cfg.rule_named(hop.target_rule)
    if rule exists:
      return selection_of(rule)

  staged = [rule for rule in dispatch_cfg.rules
            if rule.stage is set and issue.labels contains rule.stage (case-insensitive)]
  if staged is not empty:
    // The most downstream candidate: one that no other candidate reaches by
    // following next links forward from it. The lowest list index breaks ties.
    return selection_of(most_downstream(staged))

  for rule in dispatch_cfg.rules:
    if rule.stage is set:
      continue                                      // a staged rule never matches here
    if rule is catch_all or rule.match succeeds for issue:
      return selection_of(rule)

  return dispatch_cfg.default, then the workflow-wide defaults

function hop_route_of(state, issue_id):
  record = state.stage_hops[issue_id]
  return {record.target_rule, record.target_label} if record exists else {}
```

See §5.3.9 for match semantics and stage labels. The resolved triple is recorded on `RunningEntry` and rides through retries and reaction-driven continuations. Each retry timer selects again from the configuration in force and keeps the recorded triple while that configuration still launches it (`on_retry_timer` in §16.6; §5.3.9).

`resolve_attempt_settings` lays the frozen rule's settings block over the top-level block of the frozen kind (§5.3.9) and returns the resolved block, the usage-reporting disposition it produces, and the error-severity settings checks the block fails. It runs on the event loop once per attempt, from the configuration snapshot the dispatching lane already holds, and the worker receives the result by value. The first dispatch, every retry, and every reaction continuation resolve it the same way (§8.4).

A poll tick holds an issue that carries a stage hop record until a read shows the hop's target label, so a candidate listing that lags the label write cannot send the issue back to the stage it just left. The hold runs after the issue is admitted as a candidate and before `resolve_rule`:

```text
function hold_for_stage_hop(issue, state):
  hop = state.stage_hops[issue.id]
  if hop is absent or hop.target_observed:
    return issue, dispatchable
  if issue.labels contains hop.target_label:
    mark_observed(state, issue.id)
    return issue, dispatchable
  read, err = tracker.fetch_issue_by_id(issue.id)
  if err:
    log_warn("stage hop target read failed, holding issue")
    return issue, held                                   // retried next tick
  if is_terminal_state(read.state):
    reset_stage_hop(state, issue.id, "terminal")
    return issue, held
  if not is_active_state(read.state):
    reset_stage_hop(state, issue.id, "not_active")
    return issue, held
  mark_observed(state, issue.id)
  if read.labels does not contain hop.target_label:
    log_info("stage hop target label absent, hold released")
  issue.labels = read.labels                             // routing uses the fresher read
  return issue, dispatchable

function mark_observed(state, issue_id):
  state.stage_hops[issue_id].target_observed = true
  persist_stage_hop_observed(issue_id)                   // a failure is logged; the runtime value stands
```

The hold reads at most once per hop and only when the listing lacks the target label. A held issue the listing omits is never read here; reconciliation reads it every tick (§16.3). The retry lane needs no hold, because a made hop leaves the issue with no queued retry. The dispatch records the frozen stage pair the render reads: `fresh_previous(state, issue_id, rule_name)` returns the hop's source rule and outcome when the issue holds a hop record whose target is `rule_name`, and an empty pair otherwise. A retry that keeps its frozen rule reuses the pair it froze; a retry routed afresh computes it as a poll tick does. Every worker prompt render passes the pair with `stage.current`, which is the rule name when the rule carries a stage label and empty otherwise (§12.1).

### 16.5 Worker Attempt (Workspace + Prompt + Agent)

```text
function run_agent_attempt(issue, attempt, settings, orchestrator_channel):
  cfg = current_config()
  turn_timeout_ms = cfg.agent.turn_timeout_ms  // snapshot at attempt start; bounds every turn below, including the self-review phase's turns (not re-read per turn)

  // Dispatch-time in-progress transition (non-fatal).
  if cfg.tracker.in_progress_state is configured:
    if issue.state == cfg.tracker.in_progress_state (case-insensitive):
      log_debug("issue already in in-progress state, skipping transition")
      metrics.inc_dispatch_transitions("skipped")
    else:
      result = tracker.transition_issue(issue.id, cfg.tracker.in_progress_state)
      if result failed:
        log_warn("dispatch in-progress transition failed", issue.id, error)
        metrics.inc_dispatch_transitions("error")
      else:
        log_info("dispatch in-progress transition succeeded", issue.id)
        metrics.inc_dispatch_transitions("success")

  workspace = workspace_manager.create_for_issue(issue.identifier)
  if workspace failed:
    fail_worker("workspace error")

  if run_hook("before_run", workspace.path) failed:
    fail_worker("before_run hook error")

  // Credential-verification step (§10.9): a separate session, one
  // fixed request, stopped, before the working session starts.
  notify("verifying the agent credential")
  verify_result, verify_err = agent_adapter.verify_credential(workspace=workspace.path, settings=settings.passthrough)
  fold verify_result into the run's usage mirror
  offset = the mirror's componentwise watermark
  if verify_err failed:
    run_hook_best_effort("after_run", workspace.path)
    fail_worker("agent session start error", verify_err)
  log_info("agent credential verified", duration_ms)

  session = agent_adapter.start_session(workspace=workspace.path, settings=settings.passthrough)
  if session failed:
    run_hook_best_effort("after_run", workspace.path)
    fail_worker("agent session startup error")

  max_turns = config.agent.max_turns
  turn_number = 1
  pending_reason = ""  // set once a post-turn read admits a recognized value
  pending_statement = empty  // the statement of the read that set pending_reason; empty whenever pending_reason is

  while true:
    if worker_ctx is done:
      // Applies to every cancellation source reaching this point, not only
      // the ceiling's own stop: without it, a persistent-session kind would
      // start a runtime turn on an already-cancelled worker context.
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      exit_cancelled()

    if turn_number == 1:
      # presented holds, per review kind, the comment IDs this render showed
      # the agent; a fresh run keeps a seeded render only when it contains
      # every line of the unseeded one, in order
      prompt, presented = render_first_turn_prompt(workflow_template, issue, attempt, max_turns, continuation_context, fresh_run)
    else:
      prompt = build_turn_prompt(workflow_template, issue, attempt, turn_number, max_turns)
    cancelled_at_ending = worker_ctx is done  // read immediately after the call, before any teardown below
    if prompt failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker(exit_kind_at_ending(worker_ctx, cancelled_at_ending), "prompt error")

    // The derived turn context bounds only this call; worker_ctx itself is
    // never replaced and keeps flowing to every consumer below.
    turn_result, turn_err = agent_adapter.run_turn(
      ctx=with_timeout(worker_ctx, turn_timeout_ms),
      session=session,
      prompt=prompt,
      issue=issue,
      on_message=(msg) -> send(orchestrator_channel, {agent_update, issue.id, msg})
    )
    cancelled_at_ending = worker_ctx is done  // one read serves both exits below
    // An outcome the adapter reports cancelled, after the derived context
    // expired and with worker_ctx still live, is reclassified as a
    // turn_timeout error; any other report stands.
    // A worker_ctx that is already done (stall detection, terminal-state
    // reconciliation, shutdown) keeps its own cancellation disposition
    // instead, whatever the derived context is doing.

    if turn_result failed:
      agent_adapter.stop_session(session)  // on worker_ctx, never the derived turn context
      run_hook_best_effort("after_run", workspace.path)
      fail_worker(exit_kind_at_ending(worker_ctx, cancelled_at_ending), turn_err)

    status = read_sortie_status(workspace.path)  // token, plus the statement when the token is recognized
    if status.token in ["blocked", "needs-human-review", "no-change-needed"]:
      pending_reason = status.token
      pending_statement = status.statement
      break  // leaves the loop; the phase and teardown below run regardless of which value this is

    // run_ctx, not worker_ctx: an in-flight ceiling stop does not interrupt
    // this refresh. Stall detection, a tracker state change, and shutdown
    // still cancel run_ctx itself, which does interrupt it.
    refreshed_issue = tracker.fetch_issue_states_by_ids(ctx=run_ctx, [issue.id])
    cancelled_at_ending = worker_ctx is done
    if refreshed_issue failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker(exit_kind_at_ending(worker_ctx, cancelled_at_ending), "issue state refresh error")

    issue = refreshed_issue[0] or issue
    observed_issue_state = refreshed_issue[0].state or observed_issue_state  # carried into the exit report

    if issue.state is not active:
      break

    if turn_number >= max_turns:
      break

    turn_number = turn_number + 1

  // Self-review phase (between turn loop exit and session teardown). The gate
  // that already admits an exhausted turn budget also admits pending_reason
  // when it is empty or names the completion signal or the no-change
  // declaration; a pending "blocked" reason skips the phase. A "blocked"
  // signal the phase itself reports becomes the run's soft-stop reason
  // whichever admission the gate granted. A stop request that cuts the
  // phase short or keeps it from starting once it was otherwise admitted
  // ends the run as a ceiling stop rather than falling through to the
  // normal exit below; any other cancellation of the phase keeps the
  // normal exit.
  review_metadata = null
  phase_err = null
  phase_cut = false
  cfg = current_config()  // re-read for dynamic reload; NOT the source of turn_timeout_ms (R-9)
  signal_admits = pending_reason == "" OR pending_reason == "needs-human-review" OR pending_reason == "no-change-needed"
  admitted = cfg.self_review.enabled AND issue.state is active AND deps.Posture.DrivesIssueState() AND signal_admits
  if admitted AND worker_ctx is not done:
    if pending_reason != "":
      log_info("agent signaled a status admitting self-review, entering the phase", issue.id, pending_reason)
      remove_sortie_status(workspace.path)  // consume on entry, before the phase's first read
    review_metadata, phase_signal, phase_statement, cancelled_at_ending, phase_err = run_self_review_loop(
      session, workspace, issue, cfg.self_review, agent_adapter, orchestrator_channel,
      turn_timeout_ms=turn_timeout_ms  // the attempt-start snapshot, bounding both phase turns
    )
    if phase_signal == "blocked":
      pending_reason = "blocked"
      pending_statement = phase_statement  // replaces the pending one; the other in-phase values discard theirs
    // The phase's own verification commands and review turn are what can
    // falsify a no-change declaration. It stands only where the phase
    // recorded exactly one iteration ending on a "pass" verdict with no
    // failing verification result; any other outcome retracts it.
    if pending_reason == "no-change-needed" AND review_metadata != null:
      if any result in review_metadata.iterations[*].verification_results has exit_code != 0 OR timed_out:
        log_info("no-change declaration retracted", cause="verification", command, exit_code, timed_out)
        pending_reason = ""
        pending_statement = empty
      else if review_metadata.total_iterations != 1 OR review_metadata.final_verdict != "pass":
        log_info("no-change declaration retracted", cause="phase_unconfirmed", iterations=review_metadata.total_iterations, final_verdict=review_metadata.final_verdict)
        pending_reason = ""
        pending_statement = empty
    phase_cut = phase_err == null AND cancelled_at_ending
  else if admitted:
    phase_cut = true  // worker_ctx was already done at the gate
  else if pending_reason != "":
    log_info("agent signaled status, exiting worker", issue.id, pending_reason)

  self_review_status = "disabled"
  if review_metadata != null:
    if review_metadata.final_verdict == "pass":
      self_review_status = "passed"
    else if review_metadata.cap_reached:
      self_review_status = "cap_reached"
    else:
      self_review_status = "error"

  // A phase turn's deadline expiry is the only self-review failure that
  // ends the attempt: a diff-generation failure, a verification-command
  // failure, a verdict parse failure, and a "blocked" status all keep the
  // degrade-not-fail disposition above and fall through to the normal exit
  // below. The derivation of self_review_status above runs unconditionally,
  // on both the failure exit and the normal exit, so either teardown call
  // carries the phase's real status rather than the "disabled" value a
  // phase that never ran would leave behind.
  if phase_err != null:
    agent_adapter.stop_session(session)  // on worker_ctx, never the derived turn context
    run_hook_best_effort("after_run", workspace.path, {
      SORTIE_SELF_REVIEW_STATUS: self_review_status,
      SORTIE_SELF_REVIEW_SUMMARY_PATH: workspace.path + "/.sortie/review_summary.md"
    })
    fail_worker(exit_kind_at_ending(worker_ctx, cancelled_at_ending), phase_err, review_metadata=review_metadata)

  if phase_cut AND cancellation_cause(worker_ctx) == token_ceiling_stop:
    agent_adapter.stop_session(session)
    run_hook_best_effort("after_run", workspace.path, {
      SORTIE_SELF_REVIEW_STATUS: self_review_status,
      SORTIE_SELF_REVIEW_SUMMARY_PATH: workspace.path + "/.sortie/review_summary.md"
    })
    exit_cancelled(review_metadata=review_metadata)

  agent_adapter.stop_session(session)
  run_hook_best_effort("after_run", workspace.path, {
    SORTIE_SELF_REVIEW_STATUS: self_review_status,
    SORTIE_SELF_REVIEW_SUMMARY_PATH: workspace.path + "/.sortie/review_summary.md"
  })

  exit_normal(soft_stop=pending_reason != "", soft_stop_reason=pending_reason, soft_stop_statement=pending_statement, handed_off_comments=presented)
```

### 16.6 Worker Exit and Retry Handling

```text
on_worker_exit(issue_id, reason, worker_result, state):
  running_entry = state.running.remove(issue_id)
  if running_entry.cancel_func is not null:
    running_entry.cancel_func()  // also cancels the worker context nested under it
  state = add_runtime_seconds_to_totals(state, running_entry)

  status = status_for_exit(reason)
  run_error = worker_result.error

  // A stop request the run's own cancellation confirms gets its own
  // status, distinct from a stall, terminal-state, or shutdown cancel.
  if reason == cancelled AND worker_result.stopped_by_token_ceiling AND running_entry.token_ceiling_stop_request is not null:
    status = "budget_stopped"
    run_error = token_ceiling_stop_error(running_entry.issue_tokens_completed + running_entry.agent_total_tokens, running_entry.token_ceiling_stop_request.budget_tokens)
    report_token_ceiling_stop(log, metrics, running_entry.token_ceiling_stop_request)

  handoff_path = false
  evidence_withheld = false
  evidence_work_observed = false
  absence_parked = false

  # A normal exit resolves the conditions of its handoff disposition
  # here, ahead of the persist, because a withheld handoff is what
  # decides the status this run records. The persist still precedes any
  # retry the dispositions schedule, and those dispositions are still
  # taken in order below; this pass only reads what they will test.
  if reason == normal:
    # The resolved observation feeds the terminal test and nothing else.
    # The active test reads the dispatch-time snapshot, because the most
    # common non-active state at a normal exit is the handoff state
    # itself, applied by the agent through its own tracker calls.
    observation = resolve_terminal_observation(running_entry, worker_result)
    terminal = is_terminal_state(observation, cfg.tracker.terminal_states)
    is_active = is_active_state(running_entry.issue.state, cfg.tracker.active_states)
    drives_state = dispatch_drives_issue_state(running_entry)
    blocked_soft_stop = is_blocked_soft_stop(running_entry, worker_result)
    handoff_path = cfg.tracker.handoff_state is not empty and is_active
        and not blocked_soft_stop and drives_state

    # The evidence verdict is a fifth condition on the handoff path, read
    # only where those four already select it and no terminal observation
    # suppresses the exit. It can withhold a handoff write and can never
    # cause one (Section 11.5). Under `off` no verdict is computed at all
    # and the four-condition decision stands.
    policy = worker_result.handoff_evidence_policy  # frozen at dispatch
    if handoff_path and not terminal and policy != "off":
      # evaluate_handoff_evidence tests a stood no-change declaration first,
      # ahead of any workspace inspection: a declared run always yields
      # "work observed" here, under every policy value this branch reaches,
      # and runs no Git subprocess to get there.
      evidence = evaluate_handoff_evidence(worker_result)
      absence = evidence.verdict == "absence of work observed"
      undeterminable = evidence.verdict == "evidence not determinable"
      evidence_withheld = absence or (undeterminable and policy == "strict")

      # Before recording an absence failure, re-read the issue's state
      # once, gated the same way the permit path's own pre-write read is
      # gated below. A terminal result routes this exit to disposition 2
      # instead of the withheld path: no failure record, no failure
      # comment, no retry, no absence count.
      if evidence_withheld and cfg.tracker.terminal_states and tracker_adapter:
        verified, verify_err = tracker_adapter.fetch_issue_states_by_ids([issue_id])
        if verify_err:
          log_warn("withheld handoff verification read failed, recording withheld handoff",
                    verify_err, observation_source)
        elif issue_id in verified and is_terminal_state(verified[issue_id], cfg.tracker.terminal_states):
          log_info("withheld handoff suppressed for terminal issue",
                    verified[issue_id], "verified", cfg.tracker.handoff_state,
                    policy, evidence.verdict, evidence.reason, worker_result.turns_completed)
          observation = verified[issue_id]
          observation_source = "verified"
          terminal = true
          evidence_withheld = false

      if evidence_withheld:
        # A withheld handoff is an unsuccessful run even though the agent
        # process exited normally, so the persisted status is failed with
        # the verdict named as its cause (Section 19.2).
        metrics.inc_handoff_transitions("withheld")
        status = "failed"
        run_error = handoff_evidence_failure(policy, evidence.verdict)
      elif evidence.verdict == "work observed":
        evidence_work_observed = true

  sqlite.persist_run_attempt(running_entry, status, run_error)

  # The absence sequence is reconstructed from run_history rather than
  # from a verdict column (Section 19.2), so both steps below follow the
  # row written above: the reset marks that row as the end of the previous
  # sequence, and the count reads only what follows the last reset. Work
  # observed clears the sequence at once, ahead of the tracker write the
  # handoff disposition may still attempt, so a failed write cannot
  # restore the old count.
  if evidence_work_observed:
    sqlite.reset_handoff_absence_sequence(issue_id)
  elif evidence_withheld:
    absences = sqlite.consecutive_handoff_absences(issue_id)
    ceiling = cfg.agent.max_consecutive_absences
    log_warn("handoff withheld by evidence policy", evidence.verdict, absences)
    if absences >= ceiling:
      # The sequence stops at the ceiling: parking cancels the retry,
      # releases the claim, and holds the issue out of dispatch until a
      # later tick observes a release (Section 14.2).
      park_issue(state, issue_id, reason="handoff_absence")
      absence_parked = true

  if reason == normal:
    state.completed.add(issue_id)  # bookkeeping only
    # The comments the first prompt presented join the handed-off set, in
    # the cache and as rows, ahead of the reaction seeding below
    for kind, ids in worker_result.handed_off_comments:
      record_handed_off_comments(state, issue_id, kind, ids)
    was_claimed = issue_id in state.claimed
    claim_protected_for_incumbent = false
    stage_hop_made = false

    # Exactly one of six dispositions applies, evaluated in this order;
    # the first match wins and overrides every later one.

    # Disposition 1: the agent reported itself blocked through a soft
    # stop. Blocked work has nowhere to continue to. Where the dispatch
    # drives issue state, park the issue instead of merely releasing the
    # claim, so it stays out of dispatch until a later tick observes a
    # release (Section 14.2). Parking ends the issue's hop count.
    if blocked_soft_stop:
      if drives_state:
        park_issue(state, issue_id, reason="agent_blocked")
      else:
        cancel_retry(state, issue_id)
        state.claimed.remove(issue_id)
      notify_observers()
      return state

    # Disposition 2: the freshest tracker observation (reconciliation's
    # observation, else the worker's own per-turn observation, else the
    # dispatch-time snapshot) reports a terminal state. Overwriting a
    # terminal decision with the handoff state would undo it, so no
    # handoff, retry, or reaction follows.
    if terminal:
      cancel_retry(state, issue_id)
      state.claimed.remove(issue_id)
      reset_stage_hop(state, issue_id, "terminal")
      log_info("handoff suppressed for terminal issue", observation)
      notify_observers()
      return state

    # Disposition 3: a handoff state is configured, the issue is still
    # active, and the dispatch drives issue state. The evidence verdict
    # resolved above decides which arm runs: a verdict that permits the
    # write performs the handoff transition (Section 11.5), and one that
    # withholds it makes no tracker write at all.
    if handoff_path and evidence_withheld:
      # A withheld verdict makes no tracker transition and leaves the
      # issue in its active state. The exit is a failure even though the
      # agent process exited normally, so it takes the ordinary
      # exponential-backoff failure lane under the same retry-slot
      # arbitration, not the short continuation lane. Parking at the
      # ceiling above already cancelled the sequence and released the
      # claim, so nothing further is scheduled for it here.
      if not absence_parked and retry_slot_incumbent(state, issue_id) is nil:
        state = schedule_retry(state, issue_id, next_attempt_from(running_entry), {
          identifier: running_entry.identifier,
          error: format("worker exited: %run_error")
        })

    elif handoff_path:
      # The target is resolved once, ahead of the write and every log record
      # this arm emits: a stood declaration selects cfg.tracker.no_change_state
      # where that field is configured, and cfg.tracker.handoff_state otherwise.
      declared = worker_result.soft_stop and worker_result.soft_stop_reason == "no-change-needed"
      target = cfg.tracker.no_change_state if (declared and cfg.tracker.no_change_state) else cfg.tracker.handoff_state

      # The terminal test and verification read of Section 11.5 run once and
      # serve both the hop and the handoff write. A terminal result keeps the
      # existing suppression and ends the issue's hop count.
      verification = verify_exit_state(issue_id, observation)
      if verification.terminal:
        reset_stage_hop(state, issue_id, "terminal")
      else:
        stage_hop_made = advance_stage(state, running_entry, worker_result, declared)

      if stage_hop_made:
        # The hop released the retry, the reactions, and the claim, and
        # wrote no tracker state. The issue stays active on its new stage
        # label, so the next poll tick dispatches the next rule.
        pass
      else:
        result = perform_handoff_transition(issue_id, target, verification)
        if result.ok:
          reset_stage_hop(state, issue_id, "handoff")
          if retry_slot_incumbent(state, issue_id) is nil:
            state.claimed.remove(issue_id)
          else:
            claim_protected_for_incumbent = true  # incumbent kept, claim stays
        elif is_soft_stop(running_entry, worker_result):
          state.claimed.remove(issue_id)
        elif retry_slot_incumbent(state, issue_id) is nil:
          state = schedule_retry(state, issue_id, 1, {
            identifier: running_entry.identifier,
            delay_type: continuation,
            session_id: worker_result.session_id or running_entry.session_id
          })
        else:
          claim_protected_for_incumbent = true  # deferred to incumbent, claim stays

    # Disposition 4: any other soft stop. An unrecognized soft-stop
    # reason is logged before taking this path.
    elif is_soft_stop(running_entry, worker_result):
      if is_unrecognized_soft_stop(running_entry, worker_result):
        log_warn("unrecognized soft-stop reason", issue_id, running_entry.soft_stop_reason)
      cancel_retry(state, issue_id)
      state.claimed.remove(issue_id)

    # Disposition 5: the issue is still active and the dispatch drives
    # issue state. Schedule the continuation retry (attempt 1) so the
    # next tick can re-check whether the issue needs another session.
    elif is_active and drives_state:
      if retry_slot_incumbent(state, issue_id) is nil:
        state = schedule_retry(state, issue_id, 1, {
          identifier: running_entry.identifier,
          delay_type: continuation,
          session_id: worker_result.session_id or running_entry.session_id
        })
      # else: the retry slot (Section 7.5) is occupied, so this exit
      # defers to the incumbent instead of scheduling a continuation.

    # Disposition 6: otherwise the issue is no longer active. The slot is
    # consulted before the cancellation, so an incumbent is never
    # destroyed by the very step that is meant to leave it alone.
    else:
      # An active label-command dispatch also lands here and keeps its
      # issue's hop count.
      if not is_active:
        reset_stage_hop(state, issue_id, "not_active")
      if retry_slot_incumbent(state, issue_id) is nil:
        cancel_retry(state, issue_id)
        state.claimed.remove(issue_id)
      else:
        # An incumbent occupies the retry slot, so the claim stays to
        # protect it -- exactly the population mid-session-queued
        # retries need protected, since the issue may have left the
        # active states while a sibling reaction was still queued for it.
        claim_protected_for_incumbent = true

    # A reaction entry is enqueued only when the issue was claimed at the
    # moment of exit, the exit either took the handoff disposition or
    # left the issue still claimed, and the exit was not suppressed by a
    # terminal observation (already returned above, at disposition 2). A
    # claim retained solely to protect a foreign retry-slot incumbent
    # counts as released for this predicate, so protecting an incumbent
    # never widens which reaction kinds this exit seeds. A label-command
    # dispatch never satisfies this predicate: it always takes
    # disposition 6, and its retained claim counts as released here
    # exactly as an ordinary released claim would. An exit that made a stage
    # hop seeds no reaction of any kind: the hop released the previous
    # stage's reactions and the next stage owns the issue.
    still_claimed = (issue_id in state.claimed) and not claim_protected_for_incumbent
    if was_claimed and not stage_hop_made and (handoff_path or still_claimed):
      scm = read_scm_metadata(workspace_path) if workspace_path is not empty else nil

      # CI is rewritten on every exit so it always carries the ref the
      # latest run pushed.
      if ci_provider is not nil and scm is not nil and scm.branch is not empty:
        rkey = reaction_key(issue_id, "ci")
        state.pending_reactions[rkey] = {
          issue_id, identifier, display_id, attempt,
          kind: "ci", branch: scm.branch, sha: scm.sha
        }

      # Review is created only when not already present, preserving
      # in-progress debounce state.
      if scm_adapter is not nil and scm is not nil and scm.pr_number > 0
          and scm.owner is not empty and scm.repo is not empty:
        rkey = reaction_key(issue_id, "review")
        if rkey not in state.pending_reactions:
          state.pending_reactions[rkey] = {
            issue_id, identifier, display_id, attempt,
            kind: "review",
            pr_number: scm.pr_number, owner: scm.owner, repo: scm.repo,
            branch: scm.branch, sha: scm.sha
          }

      # Every other configured reaction kind (bot-review, merge,
      # merge-conflict, label-review, label-fix, merge-completion) records
      # its own pending entry, created only when one is not already
      # present, when the workspace SCM metadata satisfies that kind's
      # field requirements.
      for kind in configured_reaction_kinds(cfg) - {"ci", "review"}:
        rkey = reaction_key(issue_id, kind)
        if rkey not in state.pending_reactions and scm is not nil
            and satisfies_field_requirements(scm, kind):
          state.pending_reactions[rkey] = {
            issue_id, identifier, display_id, attempt, kind
          }
  else:
    if retry_slot_incumbent(state, issue_id) is nil:
      state = schedule_retry(state, issue_id, next_attempt_from(running_entry), {
        identifier: running_entry.identifier,
        error: format("worker exited: %reason")
      })
    # else: the retry slot (Section 7.5) is occupied, so this exit defers
    # to the incumbent instead of scheduling a backoff retry.

  notify_observers()
  return state
```

A made hop replaces the handoff write. The hop runs on the event loop, synchronously, like the write it replaces:

```text
function advance_stage(state, entry, result, declared) -> made:
  source = cfg.dispatch.rule_named(entry.rule_name)
  if source is absent or source.next is empty:
    return false                                       // no next, or a reload removed it
  target = cfg.dispatch.rule_named(source.next)
  if target is absent or target.stage is empty:
    return false

  count = state.stage_hops[issue_id].count, or 0
  ceiling = cfg.dispatch.max_consecutive_hops
  if count + 1 > ceiling:
    log_warn("stage hop not made", reason="ceiling")
    return false                                       // the handoff write follows

  err = tracker.add_label(issue_id, target.stage)
  if err:
    missing = stage_labels_the_dispatch_read_showed_and_a_fresh_read_lacks(entry)
    log_warn("stage hop not made", reason="add_failed", missing)
    return false                                       // the handoff write follows

  state.stage_hops[issue_id] = {
    count: count + 1, source_rule: source.name, target_rule: target.name,
    target_label: target.stage,
    previous_outcome: "no_change" if declared else "succeeded",
    source_dispatch_id: entry.dispatch_id, target_observed: false
  }
  persist_stage_hop(state.stage_hops[issue_id])        // a failure is logged; the runtime record stands

  // Cancel the previous stage's follow-ups: the queued retry and its row,
  // the pending reactions, their attempt counters and comment caches, and
  // the claim. The next dispatch is the poll tick's.
  release_issue_runtime_state(state, issue_id)

  left = []
  for label in configured_stage_labels(cfg.dispatch):  // list order, deduplicated case-insensitively
    if label equals target.stage or entry.issue.labels does not contain label:
      continue                                         // only labels the dispatch read showed
    err = tracker.remove_label(issue_id, label)
    if err: left.append(label)
  if left is empty:
    log_info("stage hop made")
  else:
    log_warn("stage hop made, stage labels left on the issue", left)
  return true
```

The hop leaves the issue in its active tracker state with the target's stage label added and the source's label removed, does not increment `sortie_handoff_transitions_total`, and writes no state; the next dispatch performs the in-progress transition like any dispatch. A run that never reaches the handoff arm (a blocked soft stop, a terminal exit, a withheld verdict, an abnormal exit, a label-command dispatch, or an unset handoff state) never reaches `advance_stage`.

```text
on_retry_timer(issue_id, state):
  retry_entry = state.retry_attempts.pop(issue_id)
  if missing:
    return state

  candidates = tracker.fetch_candidate_issues()
  if fetch failed:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: retry_entry.identifier,
      error: "retry poll failed",
      session_id: retry_entry.session_id
    })

  issue = find_by_id(candidates, issue_id)
  if issue is null:
    state.claimed.remove(issue_id)
    return state

  if available_slots(state) == 0:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: issue.identifier,
      error: "no available orchestrator slots",
      session_id: retry_entry.session_id
    })

  frozen = (retry_entry.agent_kind, retry_entry.template_id, retry_entry.rule_name)
  selection = retry_selection(state.cfg, template_held, frozen, issue, hop_route_of(state, issue_id))
  if selection != frozen:
    log_info("retry dispatching on the selection the configuration in force gives it")
  resume_session_id = retry_entry.session_id
  if selection.agent_kind != frozen.agent_kind or selection.template_id != frozen.template_id:
    resume_session_id = null

  if agent_adapter(selection.agent_kind) is unavailable:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: issue.identifier,
      error: "retry agent kind unavailable",
      session_id: retry_entry.session_id
    })

  attempt_settings = resolve_attempt_settings(state.cfg, selection, ssh_host)
  if attempt_settings.refusals is not empty:
    log_error("retry agent settings refused")
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: issue.identifier,
      error: "retry agent settings refused",
      session_id: retry_entry.session_id
    })
  if selection.rule_name == frozen.rule_name and retry_entry.rule_settings_applied and attempt_settings.settings.rule_name == "":
    log_info("rule settings no longer present, attempt runs on the kind's top-level settings")

  return dispatch_issue(issue, state, attempt=retry_entry.attempt,
    resume_session_id=resume_session_id, selection=selection, attempt_settings=attempt_settings)
```

A changed settings result never clears `resume_session_id`; only a changed kind or template does.

`retry_selection` keeps the frozen triple, with a retired kind replaced by its replacement kind, while the kind is still reachable through `agent.kind`, `dispatch.default.agent`, or a rule and the template is still held, and otherwise returns `resolve_rule` over the configuration in force, passing the issue's hop route (§5.3.9, §8.4). A retry that finds its issue terminal or no longer active ends the issue's hop count.


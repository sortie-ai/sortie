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
  state = reconcile_running_issues(state)

  validation = validate_dispatch_config()
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

  for issue in sort_for_dispatch(issues):
    if no_available_slots(state):
      break

    if should_dispatch(issue, state):
      state = dispatch_issue(issue, state, attempt=null)

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

    # Poll throttle
    if now < pending.pending_retry_at:
      state.pending_reactions[key] = pending
      continue

    # Continuation turn cap
    rkey = reaction_key(pending.issue_id, "review")
    turn_count = state.reaction_attempts[rkey]
    if turn_count >= review_config.max_continuation_turns:
      escalate_review_failure(state, pending, turn_count)
      continue

    # Fetch reviews from SCM adapter
    comments, err = scm_adapter.fetch_pending_reviews(data.pr_number, data.owner, data.repo)
    if err:
      pending.pending_attempts++
      pending.pending_retry_at = now + backoff(pending.pending_attempts)
      state.pending_reactions[key] = pending
      log_warn("review fetch failed, retrying with backoff")
      continue

    # Filter outdated, compute debounce timestamp
    actionable = filter(comments, c -> not c.outdated)
    max_time = max(c.submitted_at for c in actionable)

    if len(actionable) == 0:
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

    # Debounce
    if max_time is set and now - max_time < debounce_ms:
      pending.pending_retry_at = max_time + debounce_ms
      state.pending_reactions[key] = pending
      continue

    # Mark dispatched synchronously before scheduling retry
    store.mark_reaction_dispatched(pending.issue_id, "review")

    review_context = build_review_template_map(actionable)
    cancel_retry(state, pending.issue_id)
    schedule_retry(state, pending.issue_id, pending.attempt, {
      identifier: pending.identifier,
      delay_type: continuation,
      continuation_context: {"review_comments": review_context},
      reaction_kind: "review"
    })
    state.reaction_attempts[rkey]++

  return state
```

### 16.4 Dispatch One Issue

```text
function dispatch_issue(issue, state, attempt):
  (agent_kind, template_id, rule_name) = resolve_rule(
    issue, state.dispatch_cfg, state.default_agent_kind, state.default_template_id
  )

  worker = spawn_worker(
    fn -> run_agent_attempt(issue, attempt, parent_orchestrator_pid) end
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

The `resolve_rule` call evaluates `dispatch.rules` in order and returns the first match; see
§5.3.10 for match semantics and the `ResolveRule` function for the full algorithm. The resolved
triple is frozen on `RunningEntry` so retries and reaction-driven continuations reuse the same
selection without re-evaluating rules.

### 16.5 Worker Attempt (Workspace + Prompt + Agent)

```text
function run_agent_attempt(issue, attempt, orchestrator_channel):
  cfg = current_config()

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

  session = agent_adapter.start_session(workspace=workspace.path)
  if session failed:
    run_hook_best_effort("after_run", workspace.path)
    fail_worker("agent session startup error")

  max_turns = config.agent.max_turns
  turn_number = 1

  while true:
    prompt = build_turn_prompt(workflow_template, issue, attempt, turn_number, max_turns)
    if prompt failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("prompt error")

    turn_result = agent_adapter.run_turn(
      session=session,
      prompt=prompt,
      issue=issue,
      on_message=(msg) -> send(orchestrator_channel, {agent_update, issue.id, msg})
    )

    if turn_result failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("agent turn error")

    refreshed_issue = tracker.fetch_issue_states_by_ids([issue.id])
    if refreshed_issue failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("issue state refresh error")

    issue = refreshed_issue[0] or issue
    observed_issue_state = refreshed_issue[0].state or observed_issue_state  # carried into the exit report

    if issue.state is not active:
      break

    if turn_number >= max_turns:
      break

    turn_number = turn_number + 1

  // Self-review phase (between turn loop exit and session teardown).
  review_metadata = null
  cfg = current_config()  // re-read for dynamic reload
  if cfg.self_review.enabled AND issue.state is active AND context not cancelled:
    review_metadata = run_self_review_loop(
      session, workspace, issue, cfg.self_review, agent_adapter, orchestrator_channel
    )

  self_review_status = "disabled"
  if review_metadata != null:
    if review_metadata.final_verdict == "pass":
      self_review_status = "passed"
    else if review_metadata.cap_reached:
      self_review_status = "cap_reached"
    else:
      self_review_status = "error"

  agent_adapter.stop_session(session)
  run_hook_best_effort("after_run", workspace.path, {
    SORTIE_SELF_REVIEW_STATUS: self_review_status,
    SORTIE_SELF_REVIEW_SUMMARY_PATH: workspace.path + "/.sortie/review_summary.md"
  })

  exit_normal()
```

### 16.6 Worker Exit and Retry Handling

```text
on_worker_exit(issue_id, reason, state):
  running_entry = state.running.remove(issue_id)
  state = add_runtime_seconds_to_totals(state, running_entry)
  sqlite.persist_run_attempt(running_entry, reason)  # persist before scheduling retry

  if reason == normal:
    state.completed.add(issue_id)  # bookkeeping only

    # A terminal observation — reconciliation's observation on the running
    # entry, else the worker's own per-turn observation, else the
    # dispatch-time snapshot — suppresses the handoff transition and every
    # reaction enqueue below, directly, regardless of claim state.
    observation = resolve_terminal_observation(running_entry, worker_result)
    if is_terminal_state(observation, cfg.tracker.terminal_states):
      log_info("handoff suppressed for terminal issue", observation)
      notify_observers()
      return state

    state = schedule_retry(state, issue_id, 1, {
      identifier: running_entry.identifier,
      delay_type: continuation,
      session_id: running_entry.session_id
    })

    # Enqueue CI check when provider is configured and workspace has SCM metadata
    if ci_provider is not nil and workspace_path is not empty:
      if issue_id in state.claimed:
        scm = read_scm_metadata(workspace_path)
        if scm.branch is not empty:
          rkey = reaction_key(issue_id, "ci")
          state.pending_reactions[rkey] = {
            issue_id, identifier, display_id, attempt,
            kind: "ci", branch: scm.branch, sha: scm.sha
          }

    # Enqueue review check when SCM adapter is configured and workspace has PR metadata
    if scm_adapter is not nil and workspace_path is not empty:
      if issue_id in state.claimed:
        scm = read_scm_metadata(workspace_path)
        if scm.pr_number > 0 and scm.owner is not empty and scm.repo is not empty:
          rkey = reaction_key(issue_id, "review")
          # Only create if not already present (preserves in-progress debounce)
          if rkey not in state.pending_reactions:
            state.pending_reactions[rkey] = {
              issue_id, identifier, display_id, attempt,
              kind: "review",
              pr_number: scm.pr_number, owner: scm.owner, repo: scm.repo,
              branch: scm.branch, sha: scm.sha
            }
  else:
    state = schedule_retry(state, issue_id, next_attempt_from(running_entry), {
      identifier: running_entry.identifier,
      error: format("worker exited: %reason")
    })

  notify_observers()
  return state
```

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

  return dispatch_issue(issue, state, attempt=retry_entry.attempt,
    resume_session_id=retry_entry.session_id)
```


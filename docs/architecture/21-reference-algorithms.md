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
  pending_reason = ""  // set once a post-turn read admits a recognized value

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

    status = read_sortie_status(workspace.path)
    if status in ["blocked", "needs-human-review"]:
      pending_reason = status
      break  // leaves the loop; the phase and teardown below run regardless of which value this is

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

  // Self-review phase (between turn loop exit and session teardown). The gate
  // that already admits an exhausted turn budget also admits pending_reason
  // when it is empty or names the completion signal; a pending "blocked"
  // reason skips the phase.
  review_metadata = null
  cfg = current_config()  // re-read for dynamic reload
  signal_admits = pending_reason == "" OR pending_reason == "needs-human-review"
  if cfg.self_review.enabled AND issue.state is active AND context not cancelled AND signal_admits:
    if pending_reason != "":
      log_info("agent signaled completion, entering self-review", issue.id, pending_reason)
      remove_sortie_status(workspace.path)  // consume on entry, before the phase's first read
    review_metadata, phase_signal = run_self_review_loop(
      session, workspace, issue, cfg.self_review, agent_adapter, orchestrator_channel
    )
    if pending_reason != "" AND phase_signal == "blocked":
      pending_reason = "blocked"
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

  agent_adapter.stop_session(session)
  run_hook_best_effort("after_run", workspace.path, {
    SORTIE_SELF_REVIEW_STATUS: self_review_status,
    SORTIE_SELF_REVIEW_SUMMARY_PATH: workspace.path + "/.sortie/review_summary.md"
  })

  exit_normal(soft_stop=pending_reason != "", soft_stop_reason=pending_reason)
```

### 16.6 Worker Exit and Retry Handling

```text
on_worker_exit(issue_id, reason, state):
  running_entry = state.running.remove(issue_id)
  state = add_runtime_seconds_to_totals(state, running_entry)
  sqlite.persist_run_attempt(running_entry, reason)  # persist before scheduling retry

  if reason == normal:
    state.completed.add(issue_id)  # bookkeeping only
    was_claimed = issue_id in state.claimed
    claim_protected_for_incumbent = false

    # Exactly one of six dispositions applies, evaluated in this order;
    # the first match wins and overrides every later one.

    # Disposition 1: the agent reported itself blocked through a soft
    # stop. Blocked work has nowhere to continue to.
    if is_blocked_soft_stop(running_entry, worker_result):
      cancel_retry(state, issue_id)
      state.claimed.remove(issue_id)
      notify_observers()
      return state

    # Disposition 2: the freshest tracker observation (reconciliation's
    # observation, else the worker's own per-turn observation, else the
    # dispatch-time snapshot) reports a terminal state. Overwriting a
    # terminal decision with the handoff state would undo it, so no
    # handoff, retry, or reaction follows.
    observation = resolve_terminal_observation(running_entry, worker_result)
    if is_terminal_state(observation, cfg.tracker.terminal_states):
      cancel_retry(state, issue_id)
      state.claimed.remove(issue_id)
      log_info("handoff suppressed for terminal issue", observation)
      notify_observers()
      return state

    # The resolved observation feeds the terminal test above and nothing
    # else. The active test reads the dispatch-time snapshot, because the
    # most common non-active state at a normal exit is the handoff state
    # itself, applied by the agent through its own tracker calls.
    is_active = is_active_state(running_entry.issue.state, cfg.tracker.active_states)
    drives_state = dispatch_drives_issue_state(running_entry)
    handoff_taken = false

    # Disposition 3: a handoff state is configured, the issue is still
    # active, and the dispatch drives issue state. Perform the handoff
    # transition (Section 11.5).
    if cfg.tracker.handoff_state is not empty and is_active and drives_state:
      handoff_taken = true
      result = perform_handoff_transition(issue_id, cfg.tracker.handoff_state)
      if result.ok:
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
          session_id: running_entry.session_id
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
          session_id: running_entry.session_id
        })
      # else: the retry slot (Section 7.5) is occupied, so this exit
      # defers to the incumbent instead of scheduling a continuation.

    # Disposition 6: otherwise the issue is no longer active. The slot is
    # consulted before the cancellation, so an incumbent is never
    # destroyed by the very step that is meant to leave it alone.
    else:
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
    # exactly as an ordinary released claim would.
    still_claimed = (issue_id in state.claimed) and not claim_protected_for_incumbent
    if was_claimed and (handoff_taken or still_claimed):
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


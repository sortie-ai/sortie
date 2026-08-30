package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// ReactionTriageDisposition is a triage command's answer.
type ReactionTriageDisposition string

const (
	// TriageHandled means the command resolved the subject itself, so
	// no agent continuation is warranted for it.
	TriageHandled ReactionTriageDisposition = "handled"

	// TriageDispatchAgent means the command wants the agent
	// continuation the reaction would have scheduled anyway. It is
	// also the fallback applied to every failure mode.
	TriageDispatchAgent ReactionTriageDisposition = "dispatch-agent"

	// TriageEscalate means the command wants a person, so the kind's
	// configured escalation is applied instead of a continuation.
	TriageEscalate ReactionTriageDisposition = "escalate"
)

// triageInputSchemaVersion is the version of the input document the
// runner writes. A script reads it to tell one document shape from a
// later one.
const triageInputSchemaVersion = 1

// triageMaxResultBytes is the largest result file the runner reads. A
// larger file is rejected rather than parsed, because a triage answer
// is a single small object and an oversized file is a script writing
// something else to that path.
const triageMaxResultBytes = 64 * 1024

// Fallback reasons carried on a [ReactionTriageOutcome] whose
// disposition was defaulted rather than read from the command.
const (
	triageFallbackWorkspaceMissing     = "workspace_missing"
	triageFallbackWorkspacePathInvalid = "workspace_path_invalid"
	triageFallbackStartFailed          = "start_failed"
	triageFallbackTimeout              = "timeout"
	triageFallbackExitStatus           = "exit_status"
	triageFallbackHookRejected         = "hook_rejected"
	triageFallbackNoResult             = "no_result"
	triageFallbackResultTooLarge       = "result_too_large"
	triageFallbackMalformedResult      = "malformed_result"
	triageFallbackUnknownDisposition   = "unknown_disposition"
)

// ReactionTriageOutcome is one completed triage run. It holds no
// captured output: the tail belongs to the completion log record that
// [RunReactionTriage] writes, and an outcome is retained on a pending
// entry for as long as its fingerprint stands, which can be the whole
// watch window.
type ReactionTriageOutcome struct {
	// Disposition is the applied answer. It is [TriageDispatchAgent]
	// whenever Fallback is non-empty.
	Disposition ReactionTriageDisposition

	// Fallback names why the default was applied. Empty when the
	// command produced a recognized disposition.
	Fallback string

	// Elapsed is the command's wall-clock duration.
	Elapsed time.Duration
}

// ReactionTriageRun is an in-flight or finished triage run. It is
// runtime-only and is never persisted.
//
// Outcome is written by the runner goroutine before Done is closed and
// MUST NOT be read until a receive on Done has succeeded. That receive
// is the only synchronization between the goroutine and the reconcile
// pass. Applied and Marked are written afterwards by the event loop
// alone, so they need none. No field is written by both.
type ReactionTriageRun struct {
	// Fingerprint binds the run to the subject it was started for.
	Fingerprint string

	// StartedAt is the UTC time the run began.
	StartedAt time.Time

	// Cancel terminates the subprocess and its process group.
	Cancel context.CancelFunc

	// Done is closed once Outcome is set.
	Done <-chan struct{}

	// Outcome is valid only after a successful receive on Done.
	Outcome *ReactionTriageOutcome

	// Applied reports whether a reconcile pass has already acted on
	// Outcome. Written only by the event loop, and only after a
	// successful receive on Done. A consumed outcome is re-applied from
	// memory rather than re-running the command, which is what bounds a
	// subject to one run.
	Applied bool

	// Marked reports whether the fingerprint write that a handled or
	// escalate outcome requires has succeeded. False after a failed
	// write, which is the one case a later pass retries.
	Marked bool
}

// ReactionTriageRequest carries everything the input document needs.
type ReactionTriageRequest struct {
	// Kind is the runtime reaction discriminator, for example
	// [ReactionKindCI].
	Kind string

	// WorkspaceRoot is the configured workspace root the per-issue
	// directory is derived from.
	WorkspaceRoot string

	// IssueID is the tracker-assigned issue ID.
	IssueID string

	// Identifier is the human-readable ticket key the workspace key is
	// derived from.
	Identifier string

	// DisplayID is the display identifier carried into the document.
	DisplayID string

	// Attempt is the overall run attempt number of the entry.
	Attempt int

	// SSHHost is the host preference carried through to the command's
	// environment, exactly as a workspace hook receives it.
	SSHHost string

	// Fingerprint is the value the kind stores for the current subject.
	Fingerprint string

	// AttemptsUsed is the kind's continuation counter for this
	// reaction key at the moment the run starts.
	AttemptsUsed int

	// MaxAttempts is the kind's own budget field: the continuation-turn
	// cap for review and bot-review, the retry cap for the other two.
	MaxAttempts int

	// Subject is the same map the continuation template receives for
	// this kind, so the command sees what the agent would have seen.
	Subject any
}

// triageInputIssue is the issue identity block of the input document.
type triageInputIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	DisplayID  string `json:"display_id"`
}

// triageInputDocument is the JSON document written to the path named by
// SORTIE_REACTION_INPUT. Externally authored text reaches the command
// only through this document, never through an environment variable or
// a shell word.
type triageInputDocument struct {
	SchemaVersion int              `json:"schema_version"`
	ReactionKind  string           `json:"reaction_kind"`
	Issue         triageInputIssue `json:"issue"`
	Attempt       int              `json:"attempt"`
	Workspace     string           `json:"workspace"`
	Fingerprint   string           `json:"fingerprint"`
	AttemptsUsed  int              `json:"attempts_used"`
	MaxAttempts   int              `json:"max_attempts"`
	Subject       any              `json:"subject"`
}

// triageResultDocument is the JSON document the command writes to the
// path named by SORTIE_REACTION_RESULT. Unknown keys are ignored so a
// later extension can add fields without breaking existing scripts.
type triageResultDocument struct {
	Disposition string `json:"disposition"`
}

// RunReactionTriage executes one triage command and returns its
// outcome. It never returns an error: every failure mode maps to a
// [ReactionTriageOutcome] whose Disposition is [TriageDispatchAgent]
// and whose Fallback names the reason.
//
// It writes exactly one completion log record before it returns, on
// every path including the ones that never start a subprocess. That
// record is the only place the command's captured output tail is
// reported, which is why it is written here rather than by the caller:
// the tail lives on the [workspace.HookResult] or [workspace.HookError]
// and is not carried on the returned outcome.
//
// It blocks for at most cfg.TimeoutMS milliseconds plus the process
// group teardown grace, and MUST be called from a goroutine, never from
// the orchestrator event loop.
func RunReactionTriage(
	ctx context.Context,
	cfg config.ReactionTriageConfig,
	req ReactionTriageRequest,
	log *slog.Logger,
) ReactionTriageOutcome {
	if log == nil {
		log = slog.Default()
	}

	// The record is written through a context stripped of cancellation
	// so a run cancelled at shutdown still reports its outcome.
	logCtx := context.WithoutCancel(ctx)
	finish := func(outcome ReactionTriageOutcome, hookOutput string) ReactionTriageOutcome {
		logTriageCompletion(logCtx, log, req, outcome, hookOutput)
		return outcome
	}

	pathResult, err := workspace.ComputePath(req.WorkspaceRoot, req.Identifier)
	if err != nil {
		return finish(triageFallbackOutcome(triageFallbackWorkspacePathInvalid, 0), "")
	}

	// The directory is never created here: creating it would make the
	// next dispatch see an already-existing workspace, skip the
	// after-create hook, and run the before-run hook in an empty tree.
	info, statErr := os.Stat(pathResult.Path)
	if statErr != nil || !info.IsDir() {
		return finish(triageFallbackOutcome(triageFallbackWorkspaceMissing, 0), "")
	}

	// The temporary directory sits outside the workspace so a stale
	// result file cannot be read as this run's answer and so externally
	// authored text stays out of the tree the agent reads.
	tmpDir, tmpErr := os.MkdirTemp("", "sortie-triage-")
	if tmpErr != nil {
		return finish(triageFallbackOutcome(triageFallbackStartFailed, 0), "")
	}
	defer func() {
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			log.Warn("failed to remove reaction triage temporary directory",
				slog.String("reaction_kind", req.Kind),
				slog.Any("error", rmErr),
			)
		}
	}()

	inputPath := filepath.Join(tmpDir, "input.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	if writeErr := writeTriageInput(inputPath, req, pathResult.Path); writeErr != nil {
		return finish(triageFallbackOutcome(triageFallbackStartFailed, 0), "")
	}

	env := workspace.HookEnv(req.IssueID, req.Identifier, pathResult.Path, req.Attempt, req.SSHHost)
	env["SORTIE_REACTION_KIND"] = req.Kind
	env["SORTIE_REACTION_INPUT"] = inputPath
	env["SORTIE_REACTION_RESULT"] = resultPath

	started := time.Now()
	_, hookErr := workspace.RunHook(ctx, workspace.HookParams{
		Script:    cfg.Script,
		Dir:       pathResult.Path,
		Env:       env,
		TimeoutMS: cfg.TimeoutMS,
	})
	elapsed := time.Since(started)

	if hookErr != nil {
		output := ""
		op := ""
		if he, ok := errors.AsType[*workspace.HookError](hookErr); ok {
			output = he.Output
			op = he.Op
		}
		return finish(triageFallbackOutcome(fallbackForHookOp(op), elapsed), output)
	}

	return finish(decodeResult(resultPath, elapsed), "")
}

// triageFallbackOutcome builds the defaulted outcome for one fallback
// reason.
func triageFallbackOutcome(reason string, elapsed time.Duration) ReactionTriageOutcome {
	return ReactionTriageOutcome{
		Disposition: TriageDispatchAgent,
		Fallback:    reason,
		Elapsed:     elapsed,
	}
}

// fallbackForHookOp maps a [workspace.HookError] Op to the fallback
// reason reported for it. The mapping is total: an Op this function
// does not name still yields a named reason rather than an unlabelled
// fallback.
func fallbackForHookOp(op string) string {
	switch op {
	case "start":
		return triageFallbackStartFailed
	case "timeout":
		return triageFallbackTimeout
	case "run":
		return triageFallbackExitStatus
	default:
		return triageFallbackHookRejected
	}
}

// writeTriageInput marshals the input document and writes it at mode
// 0600.
func writeTriageInput(path string, req ReactionTriageRequest, workspacePath string) error {
	doc := triageInputDocument{
		SchemaVersion: triageInputSchemaVersion,
		ReactionKind:  req.Kind,
		Issue: triageInputIssue{
			ID:         req.IssueID,
			Identifier: req.Identifier,
			DisplayID:  req.DisplayID,
		},
		Attempt:      req.Attempt,
		Workspace:    workspacePath,
		Fingerprint:  req.Fingerprint,
		AttemptsUsed: req.AttemptsUsed,
		MaxAttempts:  req.MaxAttempts,
		Subject:      req.Subject,
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal triage input: %w", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write triage input: %w", err)
	}
	return nil
}

// decodeResult reads the result file a successful command left behind
// and returns the outcome it implies. Every rejection yields
// [TriageDispatchAgent] with a named fallback reason.
func decodeResult(path string, elapsed time.Duration) ReactionTriageOutcome {
	info, err := os.Stat(path)
	if err != nil {
		return triageFallbackOutcome(triageFallbackNoResult, elapsed)
	}
	if info.Size() > triageMaxResultBytes {
		return triageFallbackOutcome(triageFallbackResultTooLarge, elapsed)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // the path is orchestrator-generated inside a per-run temporary directory
	if err != nil {
		return triageFallbackOutcome(triageFallbackMalformedResult, elapsed)
	}

	var doc triageResultDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return triageFallbackOutcome(triageFallbackMalformedResult, elapsed)
	}

	switch ReactionTriageDisposition(strings.TrimSpace(doc.Disposition)) {
	case TriageHandled:
		return ReactionTriageOutcome{Disposition: TriageHandled, Elapsed: elapsed}
	case TriageDispatchAgent:
		return ReactionTriageOutcome{Disposition: TriageDispatchAgent, Elapsed: elapsed}
	case TriageEscalate:
		return ReactionTriageOutcome{Disposition: TriageEscalate, Elapsed: elapsed}
	default:
		return triageFallbackOutcome(triageFallbackUnknownDisposition, elapsed)
	}
}

// logTriageCompletion writes the single completion record every run
// emits, whether or not a reconcile pass ever consumes the outcome. It
// is info when the command produced a recognized disposition and warn
// when a fallback applied.
func logTriageCompletion(ctx context.Context, log *slog.Logger, req ReactionTriageRequest, outcome ReactionTriageOutcome, hookOutput string) {
	attrs := []slog.Attr{
		slog.String("reaction_kind", req.Kind),
		slog.String("fingerprint", req.Fingerprint),
		slog.String("disposition", string(outcome.Disposition)),
		slog.Int64("elapsed_ms", outcome.Elapsed.Milliseconds()),
	}
	if outcome.Fallback != "" {
		attrs = append(attrs, slog.String("fallback", outcome.Fallback))
	}
	if hookOutput != "" {
		attrs = append(attrs, slog.String("hook_output", hookOutput))
	}

	level := slog.LevelInfo
	if outcome.Fallback != "" {
		level = slog.LevelWarn
	}
	log.LogAttrs(ctx, level, "reaction triage completed", attrs...)
}

// reactionTriageVerdict tells the calling reconcile pass what to do with
// the entry the gate just advanced.
type reactionTriageVerdict int

const (
	// triageProceed means no triage is configured, or the run returned
	// dispatch-agent. The pass continues to its dispatch block.
	triageProceed reactionTriageVerdict = iota

	// triageWait means a run is in flight or was just started. The
	// caller re-enqueues the entry ready for the next tick and continues
	// to the next entry.
	triageWait

	// triageHandled means the run returned handled. The gate has already
	// marked the fingerprint dispatched. The caller re-enqueues the entry
	// with its poll interval and continues to the next entry.
	triageHandled

	// triageEscalate means the run returned escalate. The gate has
	// already marked the fingerprint dispatched. The caller invokes its
	// own escalation function with [EscalationTriggerTriage] and
	// continues to the next entry.
	triageEscalate
)

// startReactionTriage launches a triage run on its own goroutine,
// registers it with state.TriageWg, counts it in state.TriageInFlight,
// and returns the handle to store on the pending entry. It returns nil
// without starting anything when the in-flight cap is already reached;
// the gate reads a nil handle as a wait.
func startReactionTriage(
	state *State,
	ctx context.Context,
	cfg config.ReactionTriageConfig,
	req ReactionTriageRequest,
	log *slog.Logger,
) *ReactionTriageRun {
	if log == nil {
		log = slog.Default()
	}

	// The cap is sized from the concurrency the operator already chose
	// for this host. It adds to the agent processes that number bounds
	// rather than sharing slots with them.
	limit := int64(max(state.MaxConcurrentAgents, 1))
	if state.TriageInFlight.Load() >= limit {
		log.Debug("reaction triage deferred: in-flight cap reached",
			slog.String("reaction_kind", req.Kind),
			slog.String("fingerprint", req.Fingerprint),
			slog.Int64("cap", limit),
		)
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	run := &ReactionTriageRun{
		Fingerprint: req.Fingerprint,
		StartedAt:   time.Now().UTC(),
		Cancel:      cancel,
		Done:        done,
	}

	log.Info("reaction triage started",
		slog.String("reaction_kind", req.Kind),
		slog.String("fingerprint", req.Fingerprint),
		slog.Int("timeout_ms", cfg.TimeoutMS),
	)

	state.TriageInFlight.Add(1)
	state.TriageWg.Go(func() {
		defer state.TriageInFlight.Add(-1)
		outcome := RunReactionTriage(runCtx, cfg, req, log)
		run.Outcome = &outcome
		close(done)
		cancel()
	})

	return run
}

// triageRunFinished reports whether a run's outcome is readable, using a
// non-blocking receive so no reconcile pass ever waits on a subprocess.
func triageRunFinished(run *ReactionTriageRun) bool {
	if run == nil || run.Done == nil {
		return false
	}
	select {
	case <-run.Done:
		return true
	default:
		return false
	}
}

// reactionTriageGate advances a pending entry's triage state for one
// reconcile pass and reports what the pass should do next. It starts,
// cancels, discards, and applies runs, and marks the reaction
// fingerprint dispatched on a handled or escalate answer. It never
// escalates, dispatches, or re-enqueues: those remain the pass's own
// responsibility.
func reactionTriageGate(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	cfg config.ReactionTriageConfig,
	req ReactionTriageRequest,
	log *slog.Logger,
	ctx context.Context,
) reactionTriageVerdict {
	if log == nil {
		log = slog.Default()
	}
	if params.WorkspaceRoot == "" || !cfg.Enabled() {
		return triageProceed
	}

	if pending.Triage == nil {
		if run := startReactionTriage(state, ctx, cfg, req, log); run != nil {
			pending.Triage = run
		}
		return triageWait
	}

	run := pending.Triage
	finished := triageRunFinished(run)

	// A moved fingerprint voids the run bound to the previous subject,
	// whether it has finished or not.
	if run.Fingerprint != req.Fingerprint {
		logTriageDiscarded(ctx, log, run, req, finished)
		if !finished && run.Cancel != nil {
			run.Cancel()
		}
		pending.Triage = nil
		if fresh := startReactionTriage(state, ctx, cfg, req, log); fresh != nil {
			pending.Triage = fresh
		}
		return triageWait
	}

	if !finished {
		return triageWait
	}

	outcome := run.Outcome
	if outcome == nil {
		log.Warn("reaction triage finished without an outcome, dispatching the agent",
			slog.String("reaction_kind", req.Kind),
			slog.String("fingerprint", req.Fingerprint),
		)
		return triageProceed
	}

	if !run.Applied {
		run.Applied = true
		logTriageApplied(ctx, log, req, *outcome)

		switch outcome.Disposition {
		case TriageHandled:
			markTriageDispatched(params, run, req, log, ctx)
			return triageHandled
		case TriageEscalate:
			markTriageDispatched(params, run, req, log, ctx)
			return triageEscalate
		default:
			return triageProceed
		}
	}

	// A consumed outcome is re-applied from memory rather than re-run.
	// A memoized escalate re-applies as handled so a stored answer
	// cannot post a second escalation; the fingerprint write is the one
	// action a later pass repeats, and only while it has failed.
	if outcome.Disposition == TriageDispatchAgent {
		return triageProceed
	}
	if !run.Marked {
		markTriageDispatched(params, run, req, log, ctx)
	}
	return triageHandled
}

// markTriageDispatched marks the reaction fingerprint dispatched and
// records the write's success on the run, so a failed write is retried
// by a later pass and a successful one is never repeated.
func markTriageDispatched(
	params ReconcileParams,
	run *ReactionTriageRun,
	req ReactionTriageRequest,
	log *slog.Logger,
	ctx context.Context,
) {
	if params.Store == nil {
		return
	}
	if err := params.Store.MarkReactionDispatched(ctx, req.IssueID, req.Kind); err != nil {
		run.Marked = false
		log.Warn("failed to mark reaction dispatched after triage",
			slog.String("reaction_kind", req.Kind),
			slog.String("fingerprint", req.Fingerprint),
			slog.Any("error", err),
		)
		return
	}
	run.Marked = true
}

// logTriageApplied writes the record reporting that a pass consumed an
// outcome. It is warn when the outcome carried a fallback reason.
func logTriageApplied(ctx context.Context, log *slog.Logger, req ReactionTriageRequest, outcome ReactionTriageOutcome) {
	attrs := []slog.Attr{
		slog.String("reaction_kind", req.Kind),
		slog.String("fingerprint", req.Fingerprint),
		slog.String("disposition", string(outcome.Disposition)),
	}
	level := slog.LevelInfo
	if outcome.Fallback != "" {
		attrs = append(attrs, slog.String("fallback", outcome.Fallback))
		level = slog.LevelWarn
	}
	log.LogAttrs(ctx, level, "reaction triage applied", attrs...)
}

// logTriageDiscarded writes the record reporting that a handle bound to
// a fingerprint the pass no longer computes is being thrown away.
func logTriageDiscarded(ctx context.Context, log *slog.Logger, run *ReactionTriageRun, req ReactionTriageRequest, finished bool) {
	attrs := []slog.Attr{
		slog.String("reaction_kind", req.Kind),
		slog.String("fingerprint", req.Fingerprint),
		slog.String("previous_fingerprint", run.Fingerprint),
	}

	level := slog.LevelInfo
	if !finished {
		attrs = append(attrs, slog.String("discarded", "cancelled_in_flight"))
	} else {
		attrs = append(attrs, slog.String("discarded", "outcome_superseded"))
		if run.Outcome != nil {
			attrs = append(attrs, slog.String("disposition", string(run.Outcome.Disposition)))
			if run.Outcome.Fallback != "" {
				attrs = append(attrs, slog.String("fallback", run.Outcome.Fallback))
				level = slog.LevelWarn
			}
		}
	}

	log.LogAttrs(ctx, level, "reaction triage discarded: subject changed", attrs...)
}

// cancelReactionTriage terminates an entry's in-flight triage run. It is
// called by every path that removes a pending entry without re-inserting
// it, so no subprocess outlives the entry it was started for. A nil
// entry, a nil handle, and a finished run are all no-ops.
func cancelReactionTriage(pending *PendingReaction) {
	if pending == nil || pending.Triage == nil || pending.Triage.Cancel == nil {
		return
	}
	if triageRunFinished(pending.Triage) {
		return
	}
	pending.Triage.Cancel()
}

// ReactionEscalationTrigger names why an escalation fired. It selects
// the escalation's log message and, for the comment action, its text; it
// changes no action, no metric, and no post-condition.
type ReactionEscalationTrigger string

const (
	// EscalationTriggerBudget is the escalation a kind fires when its
	// own retry or continuation budget is spent.
	EscalationTriggerBudget ReactionEscalationTrigger = "budget_exhausted"

	// EscalationTriggerTriage is the escalation a triage command asked
	// for, with no budget spent on the subject.
	EscalationTriggerTriage ReactionEscalationTrigger = "triage"
)

// buildTriageEscalationComment returns the operator-facing comment for
// an escalation a triage command requested. One builder serves all four
// kinds. subject is the short phrase naming what was triaged: "ref <head
// sha>" for CI, and "PR #<number>" for the three pull-request kinds, the
// same subjects the budget escalations name.
func buildTriageEscalationComment(kind string, subject string) string {
	return fmt.Sprintf(
		"The %s triage command requested human attention for %s.\n\n"+
			"Sortie ran the configured triage command before dispatching a continuation "+
			"and the command answered \"escalate\", so no continuation was dispatched and "+
			"no retry or continuation budget was spent. Manual intervention required.",
		kind, subject)
}

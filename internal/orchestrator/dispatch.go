package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// maxBlockerReadsPerPass bounds how many per-issue blocker reads one
// dispatch pass may issue.
const maxBlockerReadsPerPass = 4

// SortForDispatch returns a new slice of issues sorted in dispatch priority
// order: priority ascending (nil sorts last), created_at oldest first (empty
// sorts last), identifier lexicographic tiebreaker. The input slice is not
// modified. Returns nil when the input is empty or nil.
func SortForDispatch(issues []domain.Issue) []domain.Issue {
	if len(issues) == 0 {
		return nil
	}
	sorted := slices.Clone(issues)
	slices.SortStableFunc(sorted, compareDispatchOrder)
	return sorted
}

// compareDispatchOrder implements the three-key comparator for dispatch
// sorting. Returns negative if a should sort before b, positive if after,
// zero if equal.
func compareDispatchOrder(a, b domain.Issue) int {
	// Priority ascending, nil last.
	if c := comparePriority(a.Priority, b.Priority); c != 0 {
		return c
	}
	// Created-at oldest first, empty last.
	if c := compareCreatedAt(a.CreatedAt, b.CreatedAt); c != 0 {
		return c
	}
	// Identifier lexicographic tiebreaker.
	return cmp.Compare(a.Identifier, b.Identifier)
}

// comparePriority compares two nullable integer priorities. Non-nil values
// sort ascending; nil values sort after all non-nil values.
func comparePriority(a, b *int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return cmp.Compare(*a, *b)
	}
}

// compareCreatedAt compares two ISO-8601 timestamp strings. Non-empty values
// sort lexicographically (oldest first); empty values sort after all
// non-empty values.
func compareCreatedAt(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	default:
		return cmp.Compare(a, b)
	}
}

// ShouldDispatch reports whether an issue is eligible for dispatch given the
// current orchestrator state and configured active/terminal states. It
// evaluates issue-level eligibility rules: required fields, active state,
// not running, not claimed, and blocker rule. Capacity checks (global and
// per-state slot limits) are not included; the dispatch loop checks slot
// availability incrementally between dispatches via [HasAvailableSlots].
//
// ShouldDispatch does not resolve blockers: it reads BlockedBy and
// BlockersUnresolved as the caller's issue value already carries them
// and issues no tracker request of its own. It MUST NOT be used to
// make a dispatch decision on an adapter whose candidates do not
// already carry a resolved blocker list; [EvaluateCandidate] is the
// dispatch decision. ShouldDispatch has no production caller and
// survives as an independent oracle other tests compare
// [EvaluateCandidate] against.
func ShouldDispatch(issue domain.Issue, state *State, activeStates, terminalStates []string) bool {
	// Issues missing required fields are not eligible for dispatch.
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}

	activeSet := stateSet(activeStates)
	terminalSet := stateSet(terminalStates)
	normalizedState := strings.ToLower(issue.State)

	// Issue state must be active and not terminal.
	if _, active := activeSet[normalizedState]; !active {
		return false
	}
	if _, terminal := terminalSet[normalizedState]; terminal {
		return false
	}

	// Already-running issues cannot be dispatched again.
	if _, running := state.Running[issue.ID]; running {
		return false
	}

	// Claimed issues are pending dispatch or retry and cannot be re-dispatched.
	if _, claimed := state.Claimed[issue.ID]; claimed {
		return false
	}

	// Issues that exhausted their effort budget are blocked from dispatch.
	if _, exhausted := state.BudgetExhausted[issue.ID]; exhausted {
		return false
	}

	// Parked issues are held out of dispatch until the release rule lifts
	// the park, whatever the parking reason.
	if _, parked := state.Parked[issue.ID]; parked {
		return false
	}

	// Any non-terminal blocker blocks dispatch.
	if isBlockedByNonTerminalSet(issue, terminalSet) {
		return false
	}

	return true
}

// ShouldDispatchWithSets is the pre-built-set variant of [ShouldDispatch].
// The dispatch loop calls this to avoid rebuilding state sets on each
// candidate. activeSet and terminalSet must contain lowercase state names
// built via [stateSet].
//
// ShouldDispatchWithSets does not resolve blockers, for the same
// reason and with the same restriction as [ShouldDispatch].
// [EvaluateCandidate] is the dispatch decision.
func ShouldDispatchWithSets(issue domain.Issue, state *State, activeSet, terminalSet map[string]struct{}) bool {
	// Issues missing required fields are not eligible for dispatch.
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}

	normalizedState := strings.ToLower(issue.State)

	// Issue state must be active and not terminal.
	if _, active := activeSet[normalizedState]; !active {
		return false
	}
	if _, terminal := terminalSet[normalizedState]; terminal {
		return false
	}

	// Already-running issues cannot be dispatched again.
	if _, running := state.Running[issue.ID]; running {
		return false
	}

	// Claimed issues are pending dispatch or retry and cannot be re-dispatched.
	if _, claimed := state.Claimed[issue.ID]; claimed {
		return false
	}

	// Issues that exhausted their effort budget are blocked from dispatch.
	if _, exhausted := state.BudgetExhausted[issue.ID]; exhausted {
		return false
	}

	// Parked issues are held out of dispatch until the release rule lifts
	// the park, whatever the parking reason.
	if _, parked := state.Parked[issue.ID]; parked {
		return false
	}

	// Any non-terminal blocker blocks dispatch.
	if isBlockedByNonTerminalSet(issue, terminalSet) {
		return false
	}

	return true
}

// IsBlockedByNonTerminal reports whether the issue has any blocker whose
// state is empty or non-terminal. A blocker with an empty state is treated
// as non-terminal (unknown state blocks by default). Used by
// [ShouldDispatch] for the normal dispatch path and by [HandleRetryTimer]
// for the retry eligibility check.
func IsBlockedByNonTerminal(issue domain.Issue, terminalStates []string) bool {
	return isBlockedByNonTerminalSet(issue, stateSet(terminalStates))
}

// isBlockedByNonTerminalSet is the pre-built-set variant of
// [IsBlockedByNonTerminal]. [ShouldDispatch] calls this directly to
// avoid rebuilding the terminal set that it already constructed.
//
// An issue whose BlockersUnresolved is true is treated as blocked,
// because BlockedBy is not authoritative for it. This is the one
// authority for the blocker-hold decision; every caller, including
// [EvaluateCandidate] and the retry lane, inherits it.
func isBlockedByNonTerminalSet(issue domain.Issue, terminalSet map[string]struct{}) bool {
	if issue.BlockersUnresolved {
		return true
	}
	for _, blocker := range issue.BlockedBy {
		if blocker.State == "" {
			return true
		}
		if _, terminal := terminalSet[strings.ToLower(blocker.State)]; !terminal {
			return true
		}
	}
	return false
}

// nextBlockerReadOffset returns the offset the next pass starts from:
// advanced by the reads this pass spent when the budget denied a needy
// candidate its read, and reset to zero otherwise. Only a denial leaves
// a backlog for the window to step to, so a pass that spends its whole
// budget with nobody left to deny resets, as does one that walked the
// whole candidate list, broke on capacity, or halted. Resetting is what
// keeps the window from creeping against a needy sequence that shifts
// as the fleet fills and drains.
func nextBlockerReadOffset(pass *TickResolution) int {
	if pass.budgetExhausted {
		return pass.offset + pass.reads
	}
	return 0
}

// stateSet builds a set of lowercase state names for O(1) membership testing.
func stateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, s := range states {
		set[strings.ToLower(s)] = struct{}{}
	}
	return set
}

// BlockerResolver is the orchestrator's view of the blocker resolver.
// NeedsRead lets the caller price a candidate before spending its
// read budget on one, without the caller learning which blocker
// source the adapter declared.
type BlockerResolver interface {
	NeedsRead(issue domain.Issue) bool
	Resolve(ctx context.Context, issue domain.Issue) (domain.Issue, error)
}

// SkipReason names why a candidate was not dispatched.
type SkipReason string

const (
	SkipNone               SkipReason = ""
	SkipIneligible         SkipReason = "ineligible"
	SkipBlockedBy          SkipReason = "blocked_by"
	SkipBlockersUnresolved SkipReason = "blockers_unresolved"
	SkipBlockersNotRead    SkipReason = "blockers_not_read"
	SkipBlockersIncomplete SkipReason = "blockers_incomplete"
)

// CandidateDecision is the outcome of evaluating one candidate.
type CandidateDecision struct {
	Dispatch bool
	Reason   SkipReason
	Issue    domain.Issue // the issue as resolved; dispatch uses this value
	Err      error        // resolution failure, for the caller's log record only
}

// TickResolution carries the blocker-resolution state one dispatch
// pass owns: whether a failure has already halted reads for this
// pass, the failure that halted them, how many candidates needing a
// read this pass has visited so far, how many reads it has spent,
// where in that needy order it began reading, and how many
// candidates it held without one. Its zero value is ready to use. It
// is not safe for concurrent use and MUST NOT be shared between
// passes; one dispatch loop constructs one and discards it when the
// loop ends.
type TickResolution struct {
	halted     bool
	haltErr    error
	needy      int
	reads      int
	offset     int
	heldUnread int

	// budgetExhausted records that a needy candidate went unread
	// because the budget was already spent, which is what makes a
	// backlog exist for the next pass to step to. Spending the whole
	// budget on the last needy candidates denies nobody, so it does
	// not set this.
	budgetExhausted bool
}

// passesEligibilityGates applies the seven non-blocker checks
// [ShouldDispatchWithSets] applies before its blocker arm, in the
// same order: required fields, active state, not terminal, not
// running, not claimed, not budget-exhausted, not parked. This is a
// fresh, independent re-implementation rather than a call into
// [ShouldDispatchWithSets], so the latter survives as an independent
// oracle a parity test compares [EvaluateCandidate] against.
func passesEligibilityGates(issue domain.Issue, state *State, activeSet, terminalSet map[string]struct{}) bool {
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}

	normalizedState := strings.ToLower(issue.State)

	if _, active := activeSet[normalizedState]; !active {
		return false
	}
	if _, terminal := terminalSet[normalizedState]; terminal {
		return false
	}
	if _, running := state.Running[issue.ID]; running {
		return false
	}
	if _, claimed := state.Claimed[issue.ID]; claimed {
		return false
	}
	if _, exhausted := state.BudgetExhausted[issue.ID]; exhausted {
		return false
	}
	if _, parked := state.Parked[issue.ID]; parked {
		return false
	}

	return true
}

// classifyBlockerFailureClass reports whether a blocker-read failure
// is deployment class, meaning the next tick's read of the same
// candidate will not fix it, or transient class otherwise. The
// classification reads the error alone: an adapter kind or a
// forge-specific value is never inspected.
func classifyBlockerFailureClass(err error) bool {
	if errors.Is(err, domain.ErrNoBlockerReader) {
		return true
	}

	trackerErr, ok := errors.AsType[*domain.TrackerError](err)
	if !ok {
		return false
	}

	if !trackerErr.Kind.RetryClassification().Retryable {
		return true
	}

	switch trackerErr.Status {
	case 403, 405, 410, 423, 429:
		return true
	default:
		return false
	}
}

// blockerErrorKind reports the domain error kind of a blocker-read
// failure for observability records: the [domain.TrackerErrorKind]
// when err wraps a [*domain.TrackerError], a fixed string for
// [domain.ErrNoBlockerReader], and "unknown" otherwise.
func blockerErrorKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, domain.ErrNoBlockerReader) {
		return "no_blocker_reader"
	}

	if trackerErr, ok := errors.AsType[*domain.TrackerError](err); ok {
		return string(trackerErr.Kind)
	}

	return "unknown"
}

// blockerErrorStatus reports the HTTP status a blocker-read failure
// carried, or 0 when it carried none.
func blockerErrorStatus(err error) int {
	if trackerErr, ok := errors.AsType[*domain.TrackerError](err); ok {
		return trackerErr.Status
	}
	return 0
}

// firstNonTerminalBlocker returns the first blocker whose state is
// empty or not in terminalSet, matching the criterion
// [isBlockedByNonTerminalSet] uses to decide the hold. Returns the
// zero [domain.BlockerRef] when blockers has no such entry.
func firstNonTerminalBlocker(blockers []domain.BlockerRef, terminalSet map[string]struct{}) domain.BlockerRef {
	for _, b := range blockers {
		if b.State == "" {
			return b
		}
		if _, terminal := terminalSet[strings.ToLower(b.State)]; !terminal {
			return b
		}
	}
	return domain.BlockerRef{}
}

// EvaluateCandidate applies the issue-level dispatch gates to one
// candidate, resolving its blockers first when, and only when, every
// cheaper gate has already passed and this pass still has both a
// read budget and no halting failure. Capacity is the caller's
// business, as it is today.
func EvaluateCandidate(ctx context.Context, issue domain.Issue, state *State,
	activeSet, terminalSet map[string]struct{}, resolver BlockerResolver,
	pass *TickResolution,
) CandidateDecision {
	if !passesEligibilityGates(issue, state, activeSet, terminalSet) {
		return CandidateDecision{Reason: SkipIneligible, Issue: issue}
	}

	var readErr error
	haltSkipped := false
	budgetSkipped := false

	if resolver != nil && resolver.NeedsRead(issue) {
		pass.needy++
		switch {
		case pass.halted:
			haltSkipped = true
			pass.heldUnread++
		case pass.reads >= maxBlockerReadsPerPass || pass.needy <= pass.offset:
			if pass.reads >= maxBlockerReadsPerPass {
				pass.budgetExhausted = true
			}
			budgetSkipped = true
			pass.heldUnread++
		default:
			issue, readErr = resolver.Resolve(ctx, issue)
			pass.reads++
			if readErr != nil && classifyBlockerFailureClass(readErr) {
				pass.halted = true
				pass.haltErr = readErr
			}
		}
	}

	if readErr == nil && !isBlockedByNonTerminalSet(issue, terminalSet) {
		return CandidateDecision{Dispatch: true, Issue: issue}
	}

	var reason SkipReason
	switch {
	case budgetSkipped:
		reason = SkipBlockersNotRead
	case readErr != nil || haltSkipped:
		reason = SkipBlockersUnresolved
	case issue.BlockersUnresolved:
		reason = SkipBlockersIncomplete
	default:
		reason = SkipBlockedBy
	}

	return CandidateDecision{Reason: reason, Issue: issue, Err: readErr}
}

// NextAttempt returns the next retry attempt number. A nil input (first
// dispatch) returns 1. A non-nil input returns *attempt + 1.
func NextAttempt(current *int) int {
	if current == nil {
		return 1
	}
	return *current + 1
}

// CancelRetry stops the retry timer for the given issue (if any) and
// removes the entry from [State.RetryAttempts]. No-op if no retry exists.
// Does not modify [State.Claimed].
func CancelRetry(state *State, issueID string) {
	entry, exists := state.RetryAttempts[issueID]
	if !exists {
		return
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
	delete(state.RetryAttempts, issueID)
}

// retrySlotIncumbent returns the retry entry occupying the issue's retry
// slot, or nil when the slot is free. The retry slot holds at most one
// entry per issue; a non-nil result means another unit of work already
// owns the issue's next dispatch.
func retrySlotIncumbent(state *State, issueID string) *RetryEntry {
	return state.RetryAttempts[issueID]
}

// retrySlotOwnerLabel returns the reaction kind reported in a retry-slot
// log record, substituting the literal "continuation" for the empty
// reaction kind that marks the orchestrator's own continuation lane.
func retrySlotOwnerLabel(reactionKind string) string {
	if reactionKind == "" {
		return "continuation"
	}
	return reactionKind
}

// logRetrySlotDeferral records that a challenger left the retry slot to
// its incumbent.
func logRetrySlotDeferral(log *slog.Logger, challenger string, incumbent *RetryEntry) {
	if log == nil {
		log = slog.Default()
	}
	log.Debug("retry slot occupied, deferring",
		slog.String("challenger_kind", challenger),
		slog.String("incumbent_kind", retrySlotOwnerLabel(incumbent.ReactionKind)),
		slog.Int("incumbent_attempt", incumbent.Attempt),
		slog.Int64("incumbent_due_at_ms", incumbent.DueAtMS),
	)
}

// ScheduleRetryParams holds the inputs for [ScheduleRetry].
type ScheduleRetryParams struct {
	IssueID     string
	Identifier  string
	DisplayID   string
	Attempt     int   // 1-based retry attempt number.
	DelayMS     int64 // Delay before timer fires, in milliseconds.
	Error       string
	LastSSHHost string // Runtime-only: SSH host from previous attempt for retry affinity.
	SessionID   string // Session identifier from previous attempt for cross-retry resume.

	// Logger receives the displacement warning when ScheduleRetry finds
	// the retry slot occupied. Nil selects slog.Default().
	Logger *slog.Logger

	// ContinuationContext carries reaction continuation data to inject
	// into the prompt template on the first turn of the retry worker.
	// Nil for non-reaction retries.
	ContinuationContext map[string]any

	// ReactionKind is the reaction type that triggered this retry.
	// Propagated to [RetryEntry.ReactionKind]. Empty for non-reaction
	// retries.
	ReactionKind string

	// AgentKind is the dispatch-frozen adapter kind. Propagated
	// verbatim into the new [RetryEntry] so retries reuse the
	// original adapter without re-running rule resolution.
	AgentKind string

	// RuleName is the dispatch-frozen rule name. Propagated verbatim
	// into the new [RetryEntry] so logs and metrics report the
	// original matched rule across every retry attempt.
	RuleName string

	// TemplateID is the dispatch-frozen template registry key.
	// Propagated verbatim into the new [RetryEntry] so retries
	// render the same template as the initial dispatch.
	TemplateID string
}

// ScheduleRetry creates a new timer and stores a [RetryEntry] in the
// state's retry map. The onFire callback is invoked when the timer
// expires; the caller provides the retry-timer handler. The claim on the
// issue is preserved.
//
// Callers MUST consult [retrySlotIncumbent] for the issue and call
// ScheduleRetry only when it returns nil, so the write always lands in a
// free slot. HandleRetryTimer's inner reschedule closure is the one
// exception: it pops and deletes the entry it is rescheduling before
// calling ScheduleRetry, so it always writes back into the slot it just
// freed.
//
// Concurrency note: [time.Timer.Stop] does not guarantee the callback
// will not fire if the timer goroutine has already been scheduled. The
// event loop handler (on_retry_timer) must therefore validate the entry
// still exists and matches the expected attempt before acting.
//
// Panics if onFire is nil (programming error in orchestrator wiring).
func ScheduleRetry(state *State, params ScheduleRetryParams, onFire func(issueID string)) {
	if onFire == nil {
		panic("ScheduleRetry: nil onFire callback")
	}

	if incumbent := retrySlotIncumbent(state, params.IssueID); incumbent != nil {
		log := params.Logger
		if log == nil {
			log = slog.Default()
		}
		log.Warn("retry slot displaced",
			slog.String("challenger_kind", retrySlotOwnerLabel(params.ReactionKind)),
			slog.String("incumbent_kind", retrySlotOwnerLabel(incumbent.ReactionKind)),
			slog.Int("incumbent_attempt", incumbent.Attempt),
		)
	}

	// Callers check the slot first, so this call never displaces an
	// entry in practice. It stays as a backstop that frees the slot on
	// the rare call site that skips the check, and it is what the Warn
	// above reports when that happens.
	CancelRetry(state, params.IssueID)

	delayMS := max(params.DelayMS, 0)

	dueAtMS := time.Now().UnixMilli() + delayMS

	timer := time.AfterFunc(time.Duration(delayMS)*time.Millisecond, func() {
		onFire(params.IssueID)
	})

	state.RetryAttempts[params.IssueID] = &RetryEntry{
		IssueID:             params.IssueID,
		Identifier:          params.Identifier,
		DisplayID:           params.DisplayID,
		SessionID:           params.SessionID,
		Attempt:             params.Attempt,
		DueAtMS:             dueAtMS,
		Error:               params.Error,
		TimerHandle:         timer,
		LastSSHHost:         params.LastSSHHost,
		ContinuationContext: params.ContinuationContext,
		ReactionKind:        params.ReactionKind,
		RuleName:            params.RuleName,
		TemplateID:          params.TemplateID,
		AgentKind:           params.AgentKind,
		scheduledAt:         time.Now(),
		scheduledDelayMS:    delayMS,
	}
}

// WorkerFunc is the function signature for the worker goroutine spawned by
// [DispatchIssue]. The orchestrator provides the actual worker implementation
// at call time; tests inject a controllable stub.
//
// The context carries a per-worker cancellation signal used by reconciliation
// (stall timeout, terminal-state detection) and graceful shutdown. The worker
// must select on ctx.Done() to terminate promptly when cancelled.
type WorkerFunc func(ctx context.Context, issue domain.Issue, attempt *int)

// DispatchIssue claims the issue, populates the running map with initial
// session fields, clears any existing retry entry, and spawns the worker
// goroutine. All state mutations happen synchronously on the caller's
// goroutine before the goroutine starts.
//
// The attempt parameter follows the architecture convention: nil for first
// dispatch, non-nil and >= 1 for retries/continuations.
//
// The sshHost parameter is the SSH destination for remote execution.
// Empty for local execution.
//
// Panics if workerFn is nil (programming error in orchestrator wiring).
func DispatchIssue(ctx context.Context, state *State, issue domain.Issue, attempt *int, sshHost string, workerFn WorkerFunc) {
	if workerFn == nil {
		panic("DispatchIssue: nil WorkerFunc")
	}

	workerCtx, cancelFn := context.WithCancel(ctx) //nolint:gosec // G118: cancelFn is stored in RunningEntry.CancelFunc for later use

	var attemptCopy *int
	if attempt != nil {
		attemptCopy = new(*attempt)
	}

	state.Claimed[issue.ID] = struct{}{}

	state.Running[issue.ID] = &RunningEntry{
		Identifier:   issue.Identifier,
		Issue:        issue,
		RetryAttempt: attemptCopy,
		StartedAt:    time.Now().UTC(),
		CancelFunc:   cancelFn,
		SSHHost:      sshHost,
	}

	CancelRetry(state, issue.ID)

	state.WorkerWg.Go(func() {
		workerFn(workerCtx, issue, attemptCopy)
	})
}

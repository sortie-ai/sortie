package orchestrator

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

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
func isBlockedByNonTerminalSet(issue domain.Issue, terminalSet map[string]struct{}) bool {
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

// stateSet builds a set of lowercase state names for O(1) membership testing.
func stateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, s := range states {
		set[strings.ToLower(s)] = struct{}{}
	}
	return set
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

// ScheduleRetryParams holds the inputs for [ScheduleRetry].
type ScheduleRetryParams struct {
	IssueID     string
	Identifier  string
	DisplayID   string
	Attempt     int   // 1-based retry attempt number.
	DelayMS     int64 // Delay before timer fires, in milliseconds.
	Error       string
	LastSSHHost string // Runtime-only: SSH host from previous attempt for retry affinity.

	// ContinuationContext carries reaction continuation data to inject
	// into the prompt template on the first turn of the retry worker.
	// Nil for non-reaction retries.
	ContinuationContext map[string]any

	// ReactionKind is the reaction type that triggered this retry.
	// Propagated to [RetryEntry.ReactionKind]. Empty for non-reaction
	// retries.
	ReactionKind string
}

// ScheduleRetry cancels any existing retry for the issue, creates a new
// timer, and stores a [RetryEntry] in the state's retry map. The onFire
// callback is invoked when the timer expires; the caller provides the
// retry-timer handler. The claim on the issue is preserved.
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

	CancelRetry(state, params.IssueID)

	delayMS := params.DelayMS
	if delayMS < 0 {
		delayMS = 0
	}

	dueAtMS := time.Now().UnixMilli() + delayMS

	timer := time.AfterFunc(time.Duration(delayMS)*time.Millisecond, func() {
		onFire(params.IssueID)
	})

	state.RetryAttempts[params.IssueID] = &RetryEntry{
		IssueID:             params.IssueID,
		Identifier:          params.Identifier,
		DisplayID:           params.DisplayID,
		Attempt:             params.Attempt,
		DueAtMS:             dueAtMS,
		Error:               params.Error,
		TimerHandle:         timer,
		LastSSHHost:         params.LastSSHHost,
		ContinuationContext: params.ContinuationContext,
		ReactionKind:        params.ReactionKind,
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
		v := *attempt
		attemptCopy = &v
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

	state.WorkerWg.Add(1)
	go func() {
		defer state.WorkerWg.Done()
		workerFn(workerCtx, issue, attemptCopy)
	}()
}

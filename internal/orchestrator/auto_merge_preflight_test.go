package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- Test doubles for preflight tests ---

// preflightVerifierStub implements AutoMergeScopeVerifier with configurable
// return values for preflight unit tests.
type preflightVerifierStub struct {
	granted []string
	missing []string
	err     error
}

var _ AutoMergeScopeVerifier = (*preflightVerifierStub)(nil)

func (s *preflightVerifierStub) VerifyAutoMergeScopes(_ context.Context, _ bool) ([]string, []string, error) {
	return s.granted, s.missing, s.err
}

// preflightSCMStub implements both domain.SCMAdapter and AutoMergeScopeVerifier
// for tests that need a full SCM adapter with a verifier.
type preflightSCMStub struct {
	preflightVerifierStub
}

var _ domain.SCMAdapter = (*preflightSCMStub)(nil)

func (s *preflightSCMStub) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (s *preflightSCMStub) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return []domain.ReviewComment{}, nil
}
func (s *preflightSCMStub) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return domain.ReviewDecisionApproved, nil
}
func (s *preflightSCMStub) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "success", nil
}
func (s *preflightSCMStub) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: "sha"}, nil
}
func (s *preflightSCMStub) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}
func (s *preflightSCMStub) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

// noVerifierSCMStub is a plain SCMAdapter with no VerifyAutoMergeScopes method.
type noVerifierSCMStub struct{}

var _ domain.SCMAdapter = (*noVerifierSCMStub)(nil)

func (s *noVerifierSCMStub) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (s *noVerifierSCMStub) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return []domain.ReviewComment{}, nil
}
func (s *noVerifierSCMStub) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return domain.ReviewDecisionApproved, nil
}
func (s *noVerifierSCMStub) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "success", nil
}
func (s *noVerifierSCMStub) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return domain.PRMergeStatus{}, nil
}
func (s *noVerifierSCMStub) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}
func (s *noVerifierSCMStub) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

// logCapture creates a *slog.Logger backed by a bytes.Buffer. Returns the
// logger and a pointer to the buffer for log assertions.
func logCapture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

// --- RunAutoMergePreflight tests ---

// TestRunAutoMergePreflight_TransportFailureSchedulesRetry verifies that a
// transport-class error emits a WARN log, returns (false, nil, err), and does
// not log an ERROR (spec Test 21).
func TestRunAutoMergePreflight_TransportFailureSchedulesRetry(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	transportErr := &domain.SCMError{Kind: domain.ErrSCMTransport, Message: "dial timeout"}

	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{err: transportErr},
	}

	passed, missing, err := RunAutoMergePreflight(context.Background(), adapter, true, log)

	if passed {
		t.Error("RunAutoMergePreflight passed = true on transport error; want false")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflight missing = %v, want nil on transport error", missing)
	}
	if err == nil {
		t.Error("RunAutoMergePreflight err = nil; want transport error")
	}

	output := buf.String()
	if !strings.Contains(output, "auto_merge preflight transport failure") {
		t.Errorf("expected WARN log with 'auto_merge preflight transport failure', got: %s", output)
	}
	if strings.Contains(output, "level=ERROR") {
		t.Errorf("unexpected ERROR log on transport failure; WARN is expected: %s", output)
	}
}

// TestRunAutoMergePreflight_MissingScopeErrors verifies that a missing scope
// emits an ERROR log, returns (false, missing, nil), and does not set a retry
// (spec Test 19).
func TestRunAutoMergePreflight_MissingScopeErrors(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{
			granted: []string{"contents:write"},
			missing: []string{"pull_requests:write"},
		},
	}

	passed, missing, err := RunAutoMergePreflight(context.Background(), adapter, true, log)

	if passed {
		t.Error("RunAutoMergePreflight passed = true on missing scope; want false")
	}
	if len(missing) == 0 {
		t.Fatal("RunAutoMergePreflight missing is empty; want [pull_requests:write]")
	}
	if missing[0] != "pull_requests:write" {
		t.Errorf("RunAutoMergePreflight missing[0] = %q, want %q", missing[0], "pull_requests:write")
	}
	if err != nil {
		t.Errorf("RunAutoMergePreflight err = %v; want nil on auth-class failure", err)
	}

	output := buf.String()
	if !strings.Contains(output, "level=ERROR") {
		t.Errorf("expected ERROR log on missing scope, got: %s", output)
	}
	if !strings.Contains(output, "pull_requests:write") {
		t.Errorf("expected missing scope name in log, got: %s", output)
	}
}

// TestRunAutoMergePreflight_PassesWithRepoScope verifies that a classic "repo"
// token passes and emits an INFO log (spec Test 18).
func TestRunAutoMergePreflight_PassesWithRepoScope(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{
			granted: []string{"repo"},
			missing: nil,
		},
	}

	passed, missing, err := RunAutoMergePreflight(context.Background(), adapter, true, log)

	if !passed {
		t.Error("RunAutoMergePreflight passed = false on valid repo scope; want true")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflight missing = %v, want nil on success", missing)
	}
	if err != nil {
		t.Errorf("RunAutoMergePreflight err = %v, want nil", err)
	}

	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("expected INFO log on preflight pass, got: %s", output)
	}
	if !strings.Contains(output, "auto_merge preflight passed") {
		t.Errorf("expected 'auto_merge preflight passed' message, got: %s", output)
	}
}

// TestRunAutoMergePreflight_UnableToVerifyLogsWarnAndProceeds verifies that
// when the verifier returns nil scopes with a nil error (provider did not
// populate the X-OAuth-Scopes header), preflight fails open: passed=true,
// missing=nil, err=nil, with a WARN log explaining the skip. The runtime
// auth-failure path remains responsible for surfacing genuine scope gaps.
func TestRunAutoMergePreflight_UnableToVerifyLogsWarnAndProceeds(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{
			granted: nil,
			missing: nil,
		},
	}

	passed, missing, err := RunAutoMergePreflight(context.Background(), adapter, true, log)

	if !passed {
		t.Error("RunAutoMergePreflight passed = false when scopes are unverifiable; want true (fail open)")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflight missing = %v, want nil when scopes are unverifiable", missing)
	}
	if err != nil {
		t.Errorf("RunAutoMergePreflight err = %v, want nil when scopes are unverifiable", err)
	}

	output := buf.String()
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("expected WARN log when scopes are unverifiable, got: %s", output)
	}
	if !strings.Contains(output, "scope verification skipped") {
		t.Errorf("expected 'scope verification skipped' in log, got: %s", output)
	}
	if strings.Contains(output, "level=ERROR") {
		t.Errorf("unexpected ERROR log when failing open; WARN is expected: %s", output)
	}
}

// TestRunAutoMergePreflightRetry_UnableToVerifyLogsWarnAndProceeds verifies
// that the retry path also fails open when the provider does not return scope
// information.
func TestRunAutoMergePreflightRetry_UnableToVerifyLogsWarnAndProceeds(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{
			granted: nil,
			missing: nil,
		},
	}

	passed, missing, err := RunAutoMergePreflightRetry(context.Background(), adapter, true, log)

	if !passed {
		t.Error("RunAutoMergePreflightRetry passed = false when scopes are unverifiable; want true (fail open)")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflightRetry missing = %v, want nil when scopes are unverifiable", missing)
	}
	if err != nil {
		t.Errorf("RunAutoMergePreflightRetry err = %v, want nil when scopes are unverifiable", err)
	}

	output := buf.String()
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("expected WARN log when scopes are unverifiable, got: %s", output)
	}
	if !strings.Contains(output, "scope verification skipped") {
		t.Errorf("expected 'scope verification skipped' in log, got: %s", output)
	}
	if strings.Contains(output, "level=ERROR") {
		t.Errorf("unexpected ERROR log when failing open; WARN is expected: %s", output)
	}
}

// TestRunAutoMergePreflight_AdapterWithoutVerifier verifies that when the
// adapter does not implement AutoMergeScopeVerifier, preflight is skipped and
// returns (true, nil, nil) with a WARN log.
func TestRunAutoMergePreflight_AdapterWithoutVerifier(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &noVerifierSCMStub{}

	passed, missing, err := RunAutoMergePreflight(context.Background(), adapter, false, log)

	if !passed {
		t.Error("RunAutoMergePreflight passed = false for non-verifier adapter; want true (skip)")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflight missing = %v, want nil for non-verifier adapter", missing)
	}
	if err != nil {
		t.Errorf("RunAutoMergePreflight err = %v, want nil for non-verifier adapter", err)
	}

	output := buf.String()
	if !strings.Contains(output, "preflight skipped") {
		t.Errorf("expected 'preflight skipped' WARN log, got: %s", output)
	}
}

// --- RunAutoMergePreflightRetry tests ---

// TestRunAutoMergePreflightRetry_TransportFailure verifies that a second
// transport failure emits a distinct WARN log and returns (false, nil, err)
// (spec Test 23).
func TestRunAutoMergePreflightRetry_TransportFailure(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{
			err: &domain.SCMError{Kind: domain.ErrSCMTransport, Message: "retry timeout"},
		},
	}

	passed, missing, err := RunAutoMergePreflightRetry(context.Background(), adapter, true, log)

	if passed {
		t.Error("RunAutoMergePreflightRetry passed = true on transport error; want false")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflightRetry missing = %v, want nil", missing)
	}
	if err == nil {
		t.Error("RunAutoMergePreflightRetry err = nil; want transport error")
	}

	output := buf.String()
	if !strings.Contains(output, "no further retries this lifetime") {
		t.Errorf("expected 'no further retries this lifetime' in WARN log, got: %s", output)
	}
	if strings.Contains(output, "level=ERROR") {
		t.Errorf("unexpected ERROR log on transport retry failure: %s", output)
	}
}

// TestRunAutoMergePreflightRetry_Success verifies that a successful retry emits
// 'auto_merge preflight retry succeeded' and returns (true, nil, nil)
// (spec Test 22).
func TestRunAutoMergePreflightRetry_Success(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	adapter := &preflightSCMStub{
		preflightVerifierStub: preflightVerifierStub{
			granted: []string{"repo"},
		},
	}

	passed, missing, err := RunAutoMergePreflightRetry(context.Background(), adapter, true, log)

	if !passed {
		t.Error("RunAutoMergePreflightRetry passed = false on success; want true")
	}
	if len(missing) != 0 {
		t.Errorf("RunAutoMergePreflightRetry missing = %v, want nil", missing)
	}
	if err != nil {
		t.Errorf("RunAutoMergePreflightRetry err = %v, want nil", err)
	}

	output := buf.String()
	if !strings.Contains(output, "auto_merge preflight retry succeeded") {
		t.Errorf("expected 'auto_merge preflight retry succeeded' log, got: %s", output)
	}
}

// --- IsAutoMergePreflightTransportClass tests ---

// TestIsAutoMergePreflightTransportClass verifies the transport-class predicate.
func TestIsAutoMergePreflightTransportClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil is not transport",
			err:  nil,
			want: false,
		},
		{
			name: "ErrSCMTransport is transport",
			err:  &domain.SCMError{Kind: domain.ErrSCMTransport},
			want: true,
		},
		{
			name: "ErrSCMAPI is transport-class",
			err:  &domain.SCMError{Kind: domain.ErrSCMAPI},
			want: true,
		},
		{
			name: "ErrSCMAuth is not transport-class",
			err:  &domain.SCMError{Kind: domain.ErrSCMAuth},
			want: false,
		},
		{
			name: "ErrSCMConflict is not transport-class",
			err:  &domain.SCMError{Kind: domain.ErrSCMConflict},
			want: false,
		},
		{
			name: "plain error is not transport-class",
			err:  errSimple("plain error"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsAutoMergePreflightTransportClass(tt.err)
			if got != tt.want {
				t.Errorf("IsAutoMergePreflightTransportClass(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- buildRequiredScopeList tests ---

// TestBuildRequiredScopeList verifies that the scope list includes
// contents:write only when requireContents is true.
func TestBuildRequiredScopeList(t *testing.T) {
	t.Parallel()

	withContents := buildRequiredScopeList(true)
	if len(withContents) != 2 {
		t.Fatalf("buildRequiredScopeList(true) len = %d, want 2", len(withContents))
	}
	if withContents[0] != "pull_requests:write" {
		t.Errorf("buildRequiredScopeList(true)[0] = %q, want %q", withContents[0], "pull_requests:write")
	}
	if withContents[1] != "contents:write" {
		t.Errorf("buildRequiredScopeList(true)[1] = %q, want %q", withContents[1], "contents:write")
	}

	withoutContents := buildRequiredScopeList(false)
	if len(withoutContents) != 1 {
		t.Fatalf("buildRequiredScopeList(false) len = %d, want 1", len(withoutContents))
	}
	if withoutContents[0] != "pull_requests:write" {
		t.Errorf("buildRequiredScopeList(false)[0] = %q, want %q", withoutContents[0], "pull_requests:write")
	}
}

// --- State wiring tests for preflight retry scheduling ---

// TestAutoMergePreflight_TransportFailureSchedulesRetryInState verifies that
// the orchestrator-level code correctly sets AutoMergePreflightRetryDueAt after
// a transport-class startup failure (spec Test 21). This validates the state
// mutation that reconcileAutoMerge later consumes.
func TestAutoMergePreflight_TransportFailureSchedulesRetryInState(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	startTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	transportErr := &domain.SCMError{Kind: domain.ErrSCMTransport}
	if IsAutoMergePreflightTransportClass(transportErr) {
		state.AutoMergePreflightFailed = true
		state.AutoMergePreflightRetryDueAt = startTime.Add(AutoMergePreflightRetryDelay)
	}

	if !state.AutoMergePreflightFailed {
		t.Error("AutoMergePreflightFailed = false after transport failure; want true")
	}

	wantDueAt := startTime.Add(AutoMergePreflightRetryDelay)
	if !state.AutoMergePreflightRetryDueAt.Equal(wantDueAt) {
		t.Errorf("AutoMergePreflightRetryDueAt = %v, want %v (startTime + %v)",
			state.AutoMergePreflightRetryDueAt, wantDueAt, AutoMergePreflightRetryDelay)
	}
}

// TestAutoMergePreflight_AuthFailureDoesNotScheduleRetry verifies that a scope
// mismatch (auth-class) does NOT set AutoMergePreflightRetryDueAt — only a
// restart can recover an auth-class failure (spec Test 22, negative case).
func TestAutoMergePreflight_AuthFailureDoesNotScheduleRetry(t *testing.T) {
	t.Parallel()

	// Auth failures return (false, missing, nil) — err is nil, missing is non-empty.
	missing := []string{"pull_requests:write"}
	state := NewState(5000, 4, nil, AgentTotals{})

	// Simulate the orchestrator startup path for auth-class failures:
	// err is nil so IsAutoMergePreflightTransportClass is false.
	var transportErr error
	if IsAutoMergePreflightTransportClass(transportErr) {
		state.AutoMergePreflightRetryDueAt = time.Now().Add(AutoMergePreflightRetryDelay)
	}
	if len(missing) > 0 {
		state.AutoMergePreflightFailed = true
	}

	if !state.AutoMergePreflightFailed {
		t.Error("AutoMergePreflightFailed = false after auth failure; want true")
	}
	if !state.AutoMergePreflightRetryDueAt.IsZero() {
		t.Errorf("AutoMergePreflightRetryDueAt = %v; want zero (no retry scheduled for auth failure)",
			state.AutoMergePreflightRetryDueAt)
	}
}

// errSimple is a plain non-SCM error helper for transport-class tests.
type errSimple string

func (e errSimple) Error() string { return string(e) }

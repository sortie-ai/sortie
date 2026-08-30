package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test helpers ---

// mustTriageWorkspace creates a workspace root containing a directory for
// identifier and returns the root. RunReactionTriage never creates the
// workspace itself, so every test that expects a run to start must call
// this first.
func mustTriageWorkspace(t *testing.T, identifier string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, identifier), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return root
}

// triageVerdictScript returns a hook script that writes a result
// document naming disposition, built for the shell RunHook actually
// invokes on this platform: POSIX sh on unix, cmd.exe on windows. Test
// code that needs a script driving a specific disposition should go
// through this rather than hand-writing shell syntax, so the whole
// suite runs the real command on every platform CI covers instead of
// falling back to a defaulted disposition through a syntax mismatch.
func triageVerdictScript(disposition string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`echo {"disposition":"%s"} > "%%SORTIE_REACTION_RESULT%%"`, disposition)
	}
	return fmt.Sprintf(`echo '{"disposition":"%s"}' > "$SORTIE_REACTION_RESULT"`, disposition)
}

// triageSleepScript returns a hook script that blocks for roughly
// seconds. cmd.exe has no sleep builtin, so the windows form pings the
// loopback address instead, one echo request per second.
func triageSleepScript(seconds int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("ping -n %d 127.0.0.1 >NUL", seconds+1)
	}
	return fmt.Sprintf("sleep %d", seconds)
}

// triageChain joins hook script steps into a single script for the
// shell RunHook invokes on this platform. cmd.exe does not reliably
// treat an embedded newline within a -C command line as a statement
// separator, so the windows form chains with "&" instead.
func triageChain(steps ...string) string {
	sep := "\n"
	if runtime.GOOS == "windows" {
		sep = " & "
	}
	return strings.Join(steps, sep)
}

// triageCopyInputScript returns the platform command for copying the
// input document RunReactionTriage wrote into name, relative to the
// workspace directory the hook runs in.
func triageCopyInputScript(name string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`copy "%%SORTIE_REACTION_INPUT%%" "%%CD%%\%s"`, name)
	}
	return fmt.Sprintf(`cp "$SORTIE_REACTION_INPUT" "$PWD/%s"`, name)
}

// triageDumpEnvScript returns the platform command for dumping the
// hook's environment into name, relative to the workspace directory the
// hook runs in.
func triageDumpEnvScript(name string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`set > "%%CD%%\%s"`, name)
	}
	return fmt.Sprintf(`env > "$PWD/%s"`, name)
}

// handledScript is a minimal triage command that answers "handled".
var handledScript = triageVerdictScript("handled")

// writeHookScript returns the value a [config.ReactionTriageConfig]'s
// Script field must carry for body to reach the hook's interpreter
// byte-for-byte.
//
// On unix it returns body unchanged: RunHook passes it as a single
// argv element to "sh -c", and os/exec's unix argv path applies no
// escaping, so the interpreter reads body verbatim. On windows,
// RunHook instead passes params.Script to "cmd.exe /C", and Go's
// windows argv escaping backslash-escapes any embedded double quote
// before re-quoting the whole argument; cmd.exe's own escape
// character is "^", not "\", so a script built by triageVerdictScript
// (which redirects into a quoted %SORTIE_REACTION_RESULT%) is
// corrupted before cmd.exe ever sees it. Writing body verbatim to a
// .cmd file under t.TempDir() and returning that path instead sends a
// bare path through argv escaping - no quote, space, or tab, since
// t.Name() maps subtest-name spaces to underscores before they reach
// the temp directory pattern - which Go's escaper passes through
// unmodified; cmd.exe then reads the file's bytes directly off disk.
func writeHookScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return body
	}
	path := filepath.Join(t.TempDir(), "script.cmd")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing hook script: %v", err)
	}
	return path
}

// probeTriageConfig returns a triage configuration whose script always
// answers "handled" quickly, used by tests that only need the gate to
// start and finish a real run without caring about the disposition.
func probeTriageConfig(t *testing.T) config.ReactionTriageConfig {
	t.Helper()
	return config.ReactionTriageConfig{Script: writeHookScript(t, handledScript), TimeoutMS: 5000}
}

// triageTestRunTimeout bounds how long a test waits for a real triage
// subprocess to finish.
const triageTestRunTimeout = 5 * time.Second

// waitTriageRunDone blocks until run's Done channel closes or
// triageTestRunTimeout elapses, failing the test on timeout. Tests use
// this instead of a fixed sleep because the real subprocess's
// completion time is not otherwise observable.
func waitTriageRunDone(t *testing.T, run *ReactionTriageRun) {
	t.Helper()
	if run == nil {
		t.Fatal("waitTriageRunDone: run is nil")
	}
	select {
	case <-run.Done:
	case <-time.After(triageTestRunTimeout):
		t.Fatalf("triage run did not finish within %v", triageTestRunTimeout)
	}
}

// triageCancelSpy records how many times Cancel was invoked, for tests
// that assert an in-flight run is torn down rather than left to leak.
type triageCancelSpy struct {
	mu     sync.Mutex
	called int
}

func (s *triageCancelSpy) cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called++
}

func (s *triageCancelSpy) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

// finishedTriageRun builds a completed run whose outcome is already
// readable, for gate subtests that drive a specific post-completion row
// without waiting on a real subprocess.
func finishedTriageRun(fingerprint string, outcome ReactionTriageOutcome, applied, marked bool) *ReactionTriageRun {
	done := make(chan struct{})
	close(done)
	o := outcome
	return &ReactionTriageRun{
		Fingerprint: fingerprint,
		StartedAt:   time.Now().UTC(),
		Cancel:      func() {},
		Done:        done,
		Outcome:     &o,
		Applied:     applied,
		Marked:      marked,
	}
}

// inFlightTriageRun builds a run whose Done channel never closes, for
// gate subtests that drive a not-done row without a real subprocess.
func inFlightTriageRun(fingerprint string, cancel func()) *ReactionTriageRun {
	if cancel == nil {
		cancel = func() {}
	}
	return &ReactionTriageRun{
		Fingerprint: fingerprint,
		StartedAt:   time.Now().UTC(),
		Cancel:      cancel,
		Done:        make(chan struct{}),
	}
}

// triageGateStore is a minimal ReconcileStore for gate tests: only
// MarkReactionDispatched carries behavior, the rest satisfy the
// interface but are never expected to be called by the gate.
type triageGateStore struct {
	unsupportedReactionObservationStore

	markCalls int
	markErr   error
}

var _ ReconcileStore = (*triageGateStore)(nil)

func (s *triageGateStore) SaveRetryEntry(context.Context, persistence.RetryEntry) error {
	return nil
}

func (s *triageGateStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	s.markCalls++
	return s.markErr
}

func (s *triageGateStore) UpsertReactionFingerprint(context.Context, string, string, string) error {
	return nil
}

func (s *triageGateStore) GetReactionFingerprint(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

func (s *triageGateStore) DeleteReactionFingerprint(context.Context, string, string) error {
	return nil
}

func (s *triageGateStore) DeleteRetryEntry(context.Context, string) error {
	return nil
}

func (s *triageGateStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

// gateParams returns a ReconcileParams and State wired for
// reactionTriageGate tests: a real workspace, a real MaxConcurrentAgents
// cap, and a triageGateStore.
func gateParams(t *testing.T, store *triageGateStore, workspaceRoot string) (*State, ReconcileParams) {
	t.Helper()
	state := NewState(1000, 5, nil, AgentTotals{})
	params := ReconcileParams{
		Store:         store,
		WorkspaceRoot: workspaceRoot,
	}
	return state, params
}

// gateRequest returns a ReactionTriageRequest addressed at the
// workspace mustTriageWorkspace created for identifier. root must be
// the same workspace root the caller's params carries: RunReactionTriage
// resolves the workspace path from the request, not from params, so a
// request pointed at a different root (or none) never reaches the
// configured script and instead reports the workspace_path_invalid
// fallback before starting anything.
func gateRequest(root, identifier, fingerprint string) ReactionTriageRequest {
	return ReactionTriageRequest{
		Kind:          "ci",
		WorkspaceRoot: root,
		Identifier:    identifier,
		IssueID:       "ISS-GATE",
		Fingerprint:   fingerprint,
		Subject:       map[string]any{"head_sha": fingerprint},
	}
}

// --- RunReactionTriage: disposition table ---

func TestRunReactionTriage(t *testing.T) {
	t.Parallel()

	t.Run("disposition table", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name             string
			script           string
			winScript        string // overrides script on windows when non-empty
			timeoutMS        int
			workspaceMissing bool
			emptyRoot        bool
			wantDisposition  ReactionTriageDisposition
			wantFallback     string
		}{
			{
				name:            "handled",
				script:          triageVerdictScript("handled"),
				wantDisposition: TriageHandled,
			},
			{
				name:            "dispatch-agent explicit",
				script:          triageVerdictScript("dispatch-agent"),
				wantDisposition: TriageDispatchAgent,
			},
			{
				name:            "escalate",
				script:          triageVerdictScript("escalate"),
				wantDisposition: TriageEscalate,
			},
			{
				name:            "no result file",
				script:          `true`,
				winScript:       `exit 0`,
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackNoResult,
			},
			{
				name:            "result too large",
				script:          `awk 'BEGIN{for (n=0;n<70000;n++) printf "a"}' > "$SORTIE_REACTION_RESULT"`,
				winScript:       `for /L %i in (1,1,700) do echo 0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789>>"%SORTIE_REACTION_RESULT%"`,
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackResultTooLarge,
			},
			{
				name:            "malformed result",
				script:          `printf 'not json' > "$SORTIE_REACTION_RESULT"`,
				winScript:       `echo not json > "%SORTIE_REACTION_RESULT%"`,
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackMalformedResult,
			},
			{
				name:            "unknown disposition",
				script:          triageVerdictScript("nope"),
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackUnknownDisposition,
			},
			{
				name:            "non-zero exit ignores a valid result",
				script:          triageVerdictScript("handled") + "; exit 3",
				winScript:       triageVerdictScript("handled") + " & exit 3",
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackExitStatus,
			},
			{
				name:            "timeout",
				script:          triageSleepScript(5),
				timeoutMS:       200,
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackTimeout,
			},
			{
				name:             "workspace missing",
				workspaceMissing: true,
				script:           triageVerdictScript("handled"),
				wantDisposition:  TriageDispatchAgent,
				wantFallback:     triageFallbackWorkspaceMissing,
			},
			{
				name:            "workspace path invalid",
				emptyRoot:       true,
				script:          triageVerdictScript("handled"),
				wantDisposition: TriageDispatchAgent,
				wantFallback:    triageFallbackWorkspacePathInvalid,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				identifier := "ISS-RT"
				if !tt.workspaceMissing {
					if err := os.MkdirAll(filepath.Join(root, identifier), 0o755); err != nil {
						t.Fatalf("MkdirAll: %v", err)
					}
				}

				timeoutMS := tt.timeoutMS
				if timeoutMS == 0 {
					timeoutMS = 5000
				}

				req := ReactionTriageRequest{
					Kind:          "ci",
					WorkspaceRoot: root,
					IssueID:       "ISS-1",
					Identifier:    identifier,
					DisplayID:     identifier,
					Fingerprint:   "fp-1",
					Subject:       map[string]any{"head_sha": "abc123"},
				}
				if tt.emptyRoot {
					req.WorkspaceRoot = ""
				}

				script := tt.script
				if runtime.GOOS == "windows" && tt.winScript != "" {
					script = tt.winScript
				}
				cfg := config.ReactionTriageConfig{Script: writeHookScript(t, script), TimeoutMS: timeoutMS}

				outcome := RunReactionTriage(context.Background(), cfg, req, discardLogger())

				if outcome.Disposition != tt.wantDisposition {
					t.Errorf("RunReactionTriage() Disposition = %q, want %q", outcome.Disposition, tt.wantDisposition)
				}
				if outcome.Fallback != tt.wantFallback {
					t.Errorf("RunReactionTriage() Fallback = %q, want %q", outcome.Fallback, tt.wantFallback)
				}
			})
		}
	})

	t.Run("nil logger falls back to the default logger", func(t *testing.T) {
		t.Parallel()

		root := mustTriageWorkspace(t, "ISS-RT-NILLOG")
		req := ReactionTriageRequest{
			Kind:          "ci",
			WorkspaceRoot: root,
			Identifier:    "ISS-RT-NILLOG",
			Fingerprint:   "fp",
		}
		cfg := config.ReactionTriageConfig{Script: writeHookScript(t, handledScript), TimeoutMS: 5000}

		outcome := RunReactionTriage(context.Background(), cfg, req, nil)
		if outcome.Disposition != TriageHandled {
			t.Errorf("RunReactionTriage() Disposition = %q, want %q", outcome.Disposition, TriageHandled)
		}
	})
}

// TestRunReactionTriage_StartFailedWhenTempDirUnavailable drives the
// start_failed fallback through a genuine os.MkdirTemp failure rather
// than a fault-injection seam. It cannot run in parallel with its
// siblings: t.Setenv panics when any ancestor test has called
// t.Parallel.
func TestRunReactionTriage_StartFailedWhenTempDirUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TMPDIR is not honored by os.TempDir on windows")
	}

	root := t.TempDir()
	identifier := "ISS-RT-START"
	if err := os.MkdirAll(filepath.Join(root, identifier), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))

	req := ReactionTriageRequest{
		Kind:          "ci",
		WorkspaceRoot: root,
		Identifier:    identifier,
		Fingerprint:   "fp-start-failed",
	}
	cfg := config.ReactionTriageConfig{Script: handledScript, TimeoutMS: 5000}

	outcome := RunReactionTriage(context.Background(), cfg, req, discardLogger())

	if outcome.Disposition != TriageDispatchAgent || outcome.Fallback != triageFallbackStartFailed {
		t.Errorf("RunReactionTriage() = %+v, want dispatch-agent/start_failed", outcome)
	}
}

// TestFallbackForHookOp asserts the mapping is total: every named
// HookError.Op maps to its documented fallback, and any value the table
// does not name, including "validate" and the empty string, maps to
// hook_rejected rather than an unlabelled fallback.
func TestFallbackForHookOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		op   string
		want string
	}{
		{"start", triageFallbackStartFailed},
		{"timeout", triageFallbackTimeout},
		{"run", triageFallbackExitStatus},
		{"validate", triageFallbackHookRejected},
		{"", triageFallbackHookRejected},
		{"some-future-op", triageFallbackHookRejected},
	}
	for _, tt := range tests {
		t.Run(tt.op, func(t *testing.T) {
			t.Parallel()
			if got := fallbackForHookOp(tt.op); got != tt.want {
				t.Errorf("fallbackForHookOp(%q) = %q, want %q", tt.op, got, tt.want)
			}
		})
	}
}

// TestRunReactionTriage_InputDocumentShape verifies the input document's
// shape the operator script contract documents: schema_version,
// reaction_kind, the issue identity block, attempt, workspace,
// fingerprint, attempts_used, max_attempts, and the subject map
// verbatim.
func TestRunReactionTriage_InputDocumentShape(t *testing.T) {
	t.Parallel()

	identifier := "ISS-SHAPE"
	root := mustTriageWorkspace(t, identifier)
	wsPath := filepath.Join(root, identifier)

	req := ReactionTriageRequest{
		Kind:          "merge-conflict",
		WorkspaceRoot: root,
		IssueID:       "10432",
		Identifier:    identifier,
		DisplayID:     "MT-649",
		Attempt:       3,
		Fingerprint:   "fp-shape",
		AttemptsUsed:  0,
		MaxAttempts:   1,
		Subject: map[string]any{
			"pr_number": 128,
			"branch":    "sortie/MT-649",
			"head_sha":  "abc123",
			"base":      "main",
		},
	}
	cfg := config.ReactionTriageConfig{
		Script:    writeHookScript(t, triageChain(triageCopyInputScript("captured_input.json"), handledScript)),
		TimeoutMS: 5000,
	}

	outcome := RunReactionTriage(context.Background(), cfg, req, discardLogger())
	if outcome.Disposition != TriageHandled {
		t.Fatalf("RunReactionTriage() Disposition = %q, want %q", outcome.Disposition, TriageHandled)
	}

	raw, err := os.ReadFile(filepath.Join(wsPath, "captured_input.json"))
	if err != nil {
		t.Fatalf("reading captured input: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal captured input: %v", err)
	}

	if got := doc["schema_version"]; got != float64(1) {
		t.Errorf("schema_version = %v, want 1", got)
	}
	if got := doc["reaction_kind"]; got != "merge-conflict" {
		t.Errorf("reaction_kind = %v, want %q", got, "merge-conflict")
	}
	issue, ok := doc["issue"].(map[string]any)
	if !ok {
		t.Fatalf("issue field = %T, want map", doc["issue"])
	}
	if issue["id"] != "10432" || issue["identifier"] != identifier || issue["display_id"] != "MT-649" {
		t.Errorf("issue = %+v, want id=10432 identifier=%s display_id=MT-649", issue, identifier)
	}
	if got := doc["attempt"]; got != float64(3) {
		t.Errorf("attempt = %v, want 3", got)
	}
	if got := doc["fingerprint"]; got != "fp-shape" {
		t.Errorf("fingerprint = %v, want %q", got, "fp-shape")
	}
	if got := doc["attempts_used"]; got != float64(0) {
		t.Errorf("attempts_used = %v, want 0", got)
	}
	if got := doc["max_attempts"]; got != float64(1) {
		t.Errorf("max_attempts = %v, want 1", got)
	}
	if got := doc["workspace"]; got != wsPath {
		t.Errorf("workspace = %v, want %q", got, wsPath)
	}
	subject, ok := doc["subject"].(map[string]any)
	if !ok {
		t.Fatalf("subject field = %T, want map", doc["subject"])
	}
	if subject["head_sha"] != "abc123" || subject["base"] != "main" {
		t.Errorf("subject = %+v, want head_sha=abc123 base=main", subject)
	}
}

// TestRunReactionTriage_SubjectTextNeverInEnvironment verifies that a
// review comment body containing shell metacharacters reaches the
// command only through the input file, never through an environment
// variable or the script text.
func TestRunReactionTriage_SubjectTextNeverInEnvironment(t *testing.T) {
	t.Parallel()

	identifier := "ISS-SECRET"
	root := mustTriageWorkspace(t, identifier)
	wsPath := filepath.Join(root, identifier)

	const marker = `$(touch should-not-run) ` + "`echo pwned`"

	req := ReactionTriageRequest{
		Kind:          "review",
		WorkspaceRoot: root,
		Identifier:    identifier,
		Fingerprint:   "fp-secret",
		Subject: []map[string]any{
			{"body": marker},
		},
	}
	cfg := config.ReactionTriageConfig{
		Script: writeHookScript(t, triageChain(
			triageDumpEnvScript("captured_env.txt"),
			triageCopyInputScript("captured_input.json"),
			triageVerdictScript("dispatch-agent"),
		)),
		TimeoutMS: 5000,
	}

	outcome := RunReactionTriage(context.Background(), cfg, req, discardLogger())
	if outcome.Disposition != TriageDispatchAgent || outcome.Fallback != "" {
		t.Fatalf("RunReactionTriage() = %+v, want a clean dispatch-agent", outcome)
	}

	if _, statErr := os.Stat(filepath.Join(wsPath, "should-not-run")); statErr == nil {
		t.Error("marker was executed as a shell command; a file it would create exists")
	}

	envRaw, err := os.ReadFile(filepath.Join(wsPath, "captured_env.txt"))
	if err != nil {
		t.Fatalf("reading captured env: %v", err)
	}
	if strings.Contains(string(envRaw), "touch") || strings.Contains(string(envRaw), "pwned") {
		t.Errorf("captured environment contains subject text:\n%s", envRaw)
	}

	inputRaw, err := os.ReadFile(filepath.Join(wsPath, "captured_input.json"))
	if err != nil {
		t.Fatalf("reading captured input: %v", err)
	}
	if !strings.Contains(string(inputRaw), "touch") {
		t.Error("captured input file does not contain the subject text; the fixture is not exercising the marker")
	}
}

// --- reactionTriageGate: decision table ---

func TestReactionTriageGate(t *testing.T) {
	t.Parallel()

	t.Run("no workspace root proceeds without acting", func(t *testing.T) {
		t.Parallel()

		store := &triageGateStore{}
		state, params := gateParams(t, store, "")
		pending := &PendingReaction{IssueID: "ISS-1"}
		req := gateRequest(params.WorkspaceRoot, "ISS-1", "fp")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageProceed {
			t.Errorf("reactionTriageGate() = %v, want triageProceed", verdict)
		}
		if pending.Triage != nil {
			t.Error("pending.Triage set with no workspace root, want nil")
		}
	})

	t.Run("triage not enabled proceeds without acting", func(t *testing.T) {
		t.Parallel()

		identifier := "ISS-2"
		root := mustTriageWorkspace(t, identifier)
		store := &triageGateStore{}
		state, params := gateParams(t, store, root)
		pending := &PendingReaction{IssueID: identifier}
		req := gateRequest(params.WorkspaceRoot, identifier, "fp")

		verdict := reactionTriageGate(state, params, pending, config.ReactionTriageConfig{}, req, discardLogger(), context.Background())

		if verdict != triageProceed {
			t.Errorf("reactionTriageGate() = %v, want triageProceed", verdict)
		}
		if pending.Triage != nil {
			t.Error("pending.Triage set with triage disabled, want nil")
		}
	})

	t.Run("nil handle starts a run and waits", func(t *testing.T) {
		t.Parallel()

		identifier := "ISS-3"
		root := mustTriageWorkspace(t, identifier)
		store := &triageGateStore{}
		state, params := gateParams(t, store, root)
		pending := &PendingReaction{IssueID: identifier}
		req := gateRequest(params.WorkspaceRoot, identifier, "fp-3")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageWait {
			t.Fatalf("reactionTriageGate() = %v, want triageWait", verdict)
		}
		if pending.Triage == nil {
			t.Fatal("pending.Triage = nil, want a started run")
		}
		if pending.Triage.Fingerprint != "fp-3" {
			t.Errorf("pending.Triage.Fingerprint = %q, want %q", pending.Triage.Fingerprint, "fp-3")
		}

		waitTriageRunDone(t, pending.Triage)
	})

	t.Run("not done same fingerprint waits without acting", func(t *testing.T) {
		t.Parallel()

		spy := &triageCancelSpy{}
		run := inFlightTriageRun("fp-4", spy.cancel)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-4"))
		pending := &PendingReaction{IssueID: "ISS-4", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-4", "fp-4")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageWait {
			t.Errorf("reactionTriageGate() = %v, want triageWait", verdict)
		}
		if pending.Triage != run {
			t.Error("pending.Triage handle replaced, want the same in-flight handle")
		}
		if spy.calls() != 0 {
			t.Errorf("Cancel called %d times, want 0 (same fingerprint, still in flight)", spy.calls())
		}
	})

	t.Run("not done different fingerprint cancels and starts a new run", func(t *testing.T) {
		t.Parallel()

		spy := &triageCancelSpy{}
		oldRun := inFlightTriageRun("fp-old", spy.cancel)
		identifier := "ISS-5"
		root := mustTriageWorkspace(t, identifier)
		store := &triageGateStore{}
		state, params := gateParams(t, store, root)
		pending := &PendingReaction{IssueID: identifier, Triage: oldRun}
		req := gateRequest(params.WorkspaceRoot, identifier, "fp-new")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageWait {
			t.Errorf("reactionTriageGate() = %v, want triageWait", verdict)
		}
		if spy.calls() != 1 {
			t.Errorf("Cancel called %d times, want 1 (fingerprint moved while in flight)", spy.calls())
		}
		if pending.Triage == oldRun {
			t.Error("pending.Triage still the old handle, want a fresh run for the new fingerprint")
		}
		if pending.Triage == nil || pending.Triage.Fingerprint != "fp-new" {
			t.Errorf("pending.Triage = %+v, want a run for fp-new", pending.Triage)
		}

		waitTriageRunDone(t, pending.Triage)
	})

	t.Run("done different fingerprint discards and starts a new run", func(t *testing.T) {
		t.Parallel()

		oldRun := finishedTriageRun("fp-old", ReactionTriageOutcome{Disposition: TriageDispatchAgent}, false, false)
		identifier := "ISS-6"
		root := mustTriageWorkspace(t, identifier)
		store := &triageGateStore{}
		state, params := gateParams(t, store, root)
		pending := &PendingReaction{IssueID: identifier, Triage: oldRun}
		req := gateRequest(params.WorkspaceRoot, identifier, "fp-new-2")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageWait {
			t.Errorf("reactionTriageGate() = %v, want triageWait", verdict)
		}
		if pending.Triage == oldRun {
			t.Error("pending.Triage still the discarded handle, want a fresh run")
		}
		if pending.Triage == nil || pending.Triage.Fingerprint != "fp-new-2" {
			t.Errorf("pending.Triage = %+v, want a run for fp-new-2", pending.Triage)
		}

		waitTriageRunDone(t, pending.Triage)
	})

	t.Run("done same fingerprint unapplied dispatch-agent proceeds", func(t *testing.T) {
		t.Parallel()

		run := finishedTriageRun("fp-7", ReactionTriageOutcome{Disposition: TriageDispatchAgent}, false, false)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-7"))
		pending := &PendingReaction{IssueID: "ISS-7", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-7", "fp-7")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageProceed {
			t.Errorf("reactionTriageGate() = %v, want triageProceed", verdict)
		}
		if !run.Applied {
			t.Error("run.Applied = false, want true after the first application")
		}
		if store.markCalls != 0 {
			t.Errorf("MarkReactionDispatched called %d times, want 0 for dispatch-agent", store.markCalls)
		}
	})

	t.Run("done same fingerprint unapplied handled marks dispatched", func(t *testing.T) {
		t.Parallel()

		run := finishedTriageRun("fp-8", ReactionTriageOutcome{Disposition: TriageHandled}, false, false)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-8"))
		pending := &PendingReaction{IssueID: "ISS-8", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-8", "fp-8")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageHandled {
			t.Errorf("reactionTriageGate() = %v, want triageHandled", verdict)
		}
		if !run.Applied {
			t.Error("run.Applied = false, want true")
		}
		if !run.Marked {
			t.Error("run.Marked = false, want true after a successful MarkReactionDispatched")
		}
		if store.markCalls != 1 {
			t.Errorf("MarkReactionDispatched called %d times, want 1", store.markCalls)
		}
	})

	t.Run("done same fingerprint unapplied escalate marks dispatched", func(t *testing.T) {
		t.Parallel()

		run := finishedTriageRun("fp-9", ReactionTriageOutcome{Disposition: TriageEscalate}, false, false)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-9"))
		pending := &PendingReaction{IssueID: "ISS-9", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-9", "fp-9")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageEscalate {
			t.Errorf("reactionTriageGate() = %v, want triageEscalate", verdict)
		}
		if !run.Marked {
			t.Error("run.Marked = false, want true")
		}
		if store.markCalls != 1 {
			t.Errorf("MarkReactionDispatched called %d times, want 1", store.markCalls)
		}
	})

	t.Run("applied dispatch-agent takes no further action", func(t *testing.T) {
		t.Parallel()

		run := finishedTriageRun("fp-10", ReactionTriageOutcome{Disposition: TriageDispatchAgent}, true, false)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-10"))
		pending := &PendingReaction{IssueID: "ISS-10", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-10", "fp-10")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageProceed {
			t.Errorf("reactionTriageGate() = %v, want triageProceed", verdict)
		}
		if store.markCalls != 0 {
			t.Errorf("MarkReactionDispatched called %d times, want 0", store.markCalls)
		}
	})

	t.Run("applied handled with a failed mark retries only the write", func(t *testing.T) {
		t.Parallel()

		run := finishedTriageRun("fp-11", ReactionTriageOutcome{Disposition: TriageHandled}, true, false)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-11"))
		pending := &PendingReaction{IssueID: "ISS-11", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-11", "fp-11")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageHandled {
			t.Errorf("reactionTriageGate() = %v, want triageHandled", verdict)
		}
		if store.markCalls != 1 {
			t.Errorf("MarkReactionDispatched called %d times, want 1 (retry)", store.markCalls)
		}
		if !run.Marked {
			t.Error("run.Marked = false, want true after the retried write succeeds")
		}
	})

	t.Run("applied and marked handled takes no further action", func(t *testing.T) {
		t.Parallel()

		run := finishedTriageRun("fp-12", ReactionTriageOutcome{Disposition: TriageHandled}, true, true)
		store := &triageGateStore{}
		state, params := gateParams(t, store, mustTriageWorkspace(t, "ISS-12"))
		pending := &PendingReaction{IssueID: "ISS-12", Triage: run}
		req := gateRequest(params.WorkspaceRoot, "ISS-12", "fp-12")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageHandled {
			t.Errorf("reactionTriageGate() = %v, want triageHandled", verdict)
		}
		if store.markCalls != 0 {
			t.Errorf("MarkReactionDispatched called %d times, want 0 (already marked)", store.markCalls)
		}
	})

	t.Run("in-flight cap reached defers without starting a run", func(t *testing.T) {
		t.Parallel()

		identifier := "ISS-CAP"
		root := mustTriageWorkspace(t, identifier)
		store := &triageGateStore{}
		state := NewState(1000, 1, nil, AgentTotals{})
		state.TriageInFlight.Add(1) // saturate the cap of max(MaxConcurrentAgents, 1) == 1
		params := ReconcileParams{Store: store, WorkspaceRoot: root}
		pending := &PendingReaction{IssueID: identifier}
		req := gateRequest(params.WorkspaceRoot, identifier, "fp-cap")

		verdict := reactionTriageGate(state, params, pending, probeTriageConfig(t), req, discardLogger(), context.Background())

		if verdict != triageWait {
			t.Errorf("reactionTriageGate() = %v, want triageWait", verdict)
		}
		if pending.Triage != nil {
			t.Error("pending.Triage set despite the in-flight cap, want nil")
		}
	})

	t.Run("fingerprint sequence F1 F2 F1 starts three runs", func(t *testing.T) {
		t.Parallel()

		// Each run's script sleeps so the first two are still in flight
		// when the next fingerprint arrives, driving the not-done rows
		// rather than the done rows.
		slowCfg := config.ReactionTriageConfig{Script: writeHookScript(t, triageSleepScript(5)), TimeoutMS: 5000}
		identifier := "ISS-SEQ"
		root := mustTriageWorkspace(t, identifier)
		store := &triageGateStore{}
		state, params := gateParams(t, store, root)
		state.MaxConcurrentAgents = 5 // headroom: up to three runs may overlap briefly
		pending := &PendingReaction{IssueID: identifier}

		reactionTriageGate(state, params, pending, slowCfg, gateRequest(params.WorkspaceRoot, identifier, "F1"), discardLogger(), context.Background())
		run1 := pending.Triage
		if run1 == nil {
			t.Fatal("run1 = nil, want a started run for F1")
		}

		reactionTriageGate(state, params, pending, slowCfg, gateRequest(params.WorkspaceRoot, identifier, "F2"), discardLogger(), context.Background())
		run2 := pending.Triage
		if run2 == nil || run2 == run1 {
			t.Fatalf("run2 = %p, want a new run distinct from run1 (%p)", run2, run1)
		}

		reactionTriageGate(state, params, pending, slowCfg, gateRequest(params.WorkspaceRoot, identifier, "F1"), discardLogger(), context.Background())
		run3 := pending.Triage
		if run3 == nil || run3 == run2 {
			t.Fatalf("run3 = %p, want a new run distinct from run2 (%p)", run3, run2)
		}

		if run1.Cancel != nil {
			run1.Cancel()
		}
		if run2.Cancel != nil {
			run2.Cancel()
		}
		if run3.Cancel != nil {
			run3.Cancel()
		}
		waitStateTriageWg(t, state, 5*time.Second)
	})
}

// waitStateTriageWg waits for state.TriageWg to drain, bounded, so a test
// that cancels its own background runs does not leak goroutines into
// later tests.
func waitStateTriageWg(t *testing.T, state *State, bound time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		state.TriageWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bound):
		t.Log("state.TriageWg did not drain within the bound; background goroutines may still be running")
	}
}

// TestStartReactionTriage_UnconsumedRun_StillLogsCompletion verifies
// that the completion record is written by the runner goroutine
// whether or not any reconcile pass ever consumes the outcome.
func TestStartReactionTriage_UnconsumedRun_StillLogsCompletion(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	identifier := "ISS-UNCONSUMED"
	root := mustTriageWorkspace(t, identifier)
	state := NewState(1000, 5, nil, AgentTotals{})
	req := ReactionTriageRequest{
		Kind:        "ci",
		Identifier:  identifier,
		Fingerprint: "fp-unconsumed",
	}
	req.WorkspaceRoot = root

	run := startReactionTriage(state, context.Background(), probeTriageConfig(t), req, logger)
	if run == nil {
		t.Fatal("startReactionTriage() = nil, want a started run")
	}
	waitTriageRunDone(t, run)

	mu.Lock()
	output := buf.String()
	mu.Unlock()

	if !strings.Contains(output, "reaction triage completed") {
		t.Errorf("log output missing the completion record; got:\n%s", output)
	}
	if !strings.Contains(output, "fp-unconsumed") {
		t.Errorf("log output missing the run's fingerprint; got:\n%s", output)
	}
}

// lockedWriter serializes writes from concurrent goroutines onto a
// shared buffer, needed because the completion record is written from
// the runner goroutine while the test goroutine may read the buffer.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (lw lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// --- The supported-kind set and the gated kinds agree ---

// probeCITriageGate drives one reconcileCIStatus pass on a failing CI
// entry with triage configured and reports whether the gate started a
// run.
func probeCITriageGate(t *testing.T) bool {
	t.Helper()
	issueID := "PROBE-CI"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)

	state := stateWithPendingReaction(t, issueID, "feature/probe", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := defaultCISCM()
	params := ciParams(t, store, ci, nil, scm)
	params.WorkspaceRoot = root
	params.CITriage = probeTriageConfig(t)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), newCIMetricsSpy())

	got := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	return got != nil && got.Triage != nil
}

// probeReviewTriageGate drives one reconcileReviewComments pass with an
// actionable comment and triage configured and reports whether the gate
// started a run.
func probeReviewTriageGate(t *testing.T) bool {
	t.Helper()
	issueID := "PROBE-REVIEW"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)

	state := stateWithReviewReaction(t, issueID, 55)
	scm := &mockSCMAdapter{comments: []domain.ReviewComment{
		{ID: "c1", Body: "fix this", SubmittedAt: reviewBaseTime.Add(-time.Hour)},
	}}
	store := &reviewReconcileStore{}
	params := reviewParams(store, scm, nil)
	params.WorkspaceRoot = root
	params.ReviewConfig.Triage = probeTriageConfig(t)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), newReviewMetricsSpy())

	got := state.PendingReactions[ReactionKey(issueID, ReactionKindReview)]
	return got != nil && got.Triage != nil
}

// probeBotReviewTriageGate drives one reconcileBotReviewComments pass
// with an actionable bot comment and triage configured and reports
// whether the gate started a run.
func probeBotReviewTriageGate(t *testing.T) bool {
	t.Helper()
	issueID := "PROBE-BOTREVIEW"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)

	state := stateWithBotReviewReaction(t, issueID, 77)
	scm := &mockSCMAdapter{botComments: []domain.ReviewComment{{ID: "b1", Body: "fix this too"}}}
	store := &reviewReconcileStore{}
	params := botReviewParams(store, scm, nil)
	params.WorkspaceRoot = root
	params.BotReviewConfig.Triage = probeTriageConfig(t)

	reconcileBotReviewComments(state, params, discardLogger(), context.Background(), newBotReviewMetricsSpy())

	got := state.PendingReactions[ReactionKey(issueID, ReactionKindBotReview)]
	return got != nil && got.Triage != nil
}

// probeMergeConflictTriageGate drives one reconcileMergeConflicts pass
// on a dirty PR with triage configured and reports whether the gate
// started a run.
func probeMergeConflictTriageGate(t *testing.T) bool {
	t.Helper()
	issueID := "PROBE-MC"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)

	state := stateWithMergeConflict(t, issueID, 88)
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-mc", "main"), nil
	}}
	store := newStatefulFingerprintStore()
	params := mergeConflictParams(store, scm, nil)
	params.WorkspaceRoot = root
	params.MergeConflictConfig.Triage = probeTriageConfig(t)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), newMergeConflictMetricsSpy())

	got := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeConflict)]
	return got != nil && got.Triage != nil
}

// TestReactionTriageSupportedKinds asserts the coupling structurally:
// the set of reaction config keys probed here, driven once per member of
// config.TriageSupportedReactionKeys, is exactly the set whose reconcile
// pass calls reactionTriageGate. A fifth kind added to one side without
// the other fails this test rather than drifting silently.
func TestReactionTriageSupportedKinds(t *testing.T) {
	t.Parallel()

	gatedReactionKinds := map[string]func(t *testing.T) bool{
		"ci_failure":      probeCITriageGate,
		"review_comments": probeReviewTriageGate,
		"bot_review":      probeBotReviewTriageGate,
		"merge_conflicts": probeMergeConflictTriageGate,
	}

	got := make(map[string]bool, len(gatedReactionKinds))
	for key, probe := range gatedReactionKinds {
		t.Run(key, func(t *testing.T) {
			if !probe(t) {
				t.Fatalf("reconcile pass for %q did not call reactionTriageGate", key)
			}
		})
		got[key] = true
	}

	if !maps.Equal(got, config.TriageSupportedReactionKeys) {
		t.Errorf("gated reaction kinds = %v, want exactly config.TriageSupportedReactionKeys = %v",
			got, config.TriageSupportedReactionKeys)
	}
}

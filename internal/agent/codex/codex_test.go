package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

var _ domain.AgentAdapter = (*CodexAdapter)(nil)

func requireAgentError(t *testing.T, err error, wantKind domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", wantKind)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, wantKind)
	}
}

func TestStartSession_ConnectionLostDuringResumeFailsLikeDuringStart(t *testing.T) {
	t.Parallel()

	failureKind := func(fake recordFramesParams, resumeID string) domain.AgentErrorKind {
		_, err := startRecordingSessionE(t, &CodexAdapter{}, fake, nil, resumeID, false)
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) {
			t.Fatalf("StartSession(resume %q, exit on %q) error = %v, want *domain.AgentError", resumeID, fake.ExitOn, err)
		}
		return agentErr.Kind
	}

	duringStart := failureKind(recordFramesParams{ExitOn: "thread/start"}, "")
	duringResume := failureKind(recordFramesParams{ExitOn: "thread/resume"}, "thread-prior")

	if duringResume != duringStart {
		t.Errorf("StartSession(connection lost during thread/resume) kind = %q, want %q as during thread/start", duringResume, duringStart)
	}
}

func TestSessionContinuity(t *testing.T) {
	t.Parallel()

	agenttest.AssertSessionContinuity(t, func(t *testing.T, held, resumeID string) (domain.Session, error) {
		t.Helper()

		started, err := startRecordingSessionE(t, &CodexAdapter{}, recordFramesParams{HeldThread: held}, nil, resumeID, false)
		if err != nil {
			return domain.Session{}, err
		}
		if resumeID == held && len(started.recorded(t, "thread/start")) != 0 {
			t.Errorf("StartSession(resume %q) sent thread/start for a carried session, want only thread/resume", resumeID)
		}
		return started.session, nil
	})
}

func TestNewCodexAdapter(t *testing.T) {
	t.Parallel()

	adapter, err := NewCodexAdapter()
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}
	if adapter == nil {
		t.Fatal("adapter is nil")
	}
}

func TestStartSession_SettingsArePerSession(t *testing.T) {
	t.Parallel()

	adapter := &CodexAdapter{}
	first := startRecordingSession(t, adapter, map[string]any{
		"model": "model-a", "effort": "high", "approval_policy": "never", "thread_sandbox": "workspaceWrite",
		"personality": "helpful", "turn_sandbox_policy": map[string]any{"type": "readOnly"},
	}, "")
	resumed := startRecordingSession(t, adapter, map[string]any{"model": "model-b"}, "thread-prior")
	runRecordedTurns(t, adapter, first, 1)
	runRecordedTurns(t, adapter, resumed, 2)

	if pt := first.state.passthrough; pt.Model != "model-a" || pt.Effort != "high" || pt.ApprovalPolicy != "never" || pt.ThreadSandbox != "workspaceWrite" || pt.Personality != "helpful" {
		t.Errorf("first session settings = %+v, want every key parsed", pt)
	}
	firstTurn := first.recorded(t, "turn/start")[0]
	if policy, _ := firstTurn.Params["sandboxPolicy"].(map[string]any); firstTurn.Params["model"] != "model-a" || policy["type"] != "readOnly" {
		t.Errorf("first session turn/start model, sandboxPolicy = %v, %v, want model-a and type readOnly", firstTurn.Params["model"], firstTurn.Params["sandboxPolicy"])
	}
	if got := resumed.recorded(t, "thread/resume"); len(got) != 1 {
		t.Fatalf("resumed session thread/resume frames = %d, want 1", len(got))
	}
	turns := resumed.recorded(t, "turn/start")
	if len(turns) != 2 {
		t.Fatalf("resumed session turn/start frames = %d, want 2", len(turns))
	}
	for i, turn := range turns {
		if policy, _ := turn.Params["sandboxPolicy"].(map[string]any); turn.Params["model"] != "model-b" || policy["type"] == "readOnly" {
			t.Errorf("resumed session turn/start %d model, sandboxPolicy = %v, %v, want model-b and not the first session's policy", i+1, turn.Params["model"], turn.Params["sandboxPolicy"])
		}
	}
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	factory, err := registry.Agents.Get("codex")
	if err != nil {
		t.Fatalf(`registry.Agents.Get("codex") error = %v`, err)
	}
	adapter, err := factory()
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := adapter.(*CodexAdapter); !ok {
		t.Errorf("factory() type = %T, want *CodexAdapter", adapter)
	}
}

func TestStartSession_EmptyWorkspace(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter()
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_NonexistentPath(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter()
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: "/nonexistent/path/that/does/not/exist/codex-test",
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_WorkspaceIsFile(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter, _ := NewCodexAdapter()
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpFile,
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_WorkspaceIsSymlink(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	adapter, _ := NewCodexAdapter()
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: link,
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)

	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatalf("ReadDir(target): %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("link target gained entries %v, want none (no subprocess started)", entries)
	}
}

func TestStartSession_BinaryNotFound(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter()
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-codex-binary-99999"},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

func TestRunTurn_UsageMeasured_AbsentWhenNoTokenUsageNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_misc_notifications.jsonl"))
	adapter, _ := NewCodexAdapter()

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertMeasurementAbsent(t, events, result)
}

func TestRunTurn_CompletedTurnReturnsUntypedNilError(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter()

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil (not a typed-nil *domain.AgentError)", err)
	}
}

// atomicErrContext's Done never closes, so a test can make ctx.Err()
// report cancellation without racing RunTurn's own <-ctx.Done() case.
type atomicErrContext struct {
	context.Context
	cancelled *atomic.Bool
}

func (c atomicErrContext) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

// The peer answers turn/start only after it sees the request written: an
// earlier response would be routed as unmatched, ahead of Call's own
// pending-map registration.
func newInterruptedStatusState(t *testing.T, stdin io.Writer) *sessionState {
	t.Helper()

	sig := newSignalingWriter(stdin)
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		inbox:      jsonrpc.NewInbox[jsonrpc.Message](),
		readerDone: make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
		ledger:     adaptNewLedger(t),
	}
	state.conn = jsonrpc.NewConn(sig, inPr, jsonrpc.Deliver(state.inbox, identity))
	t.Cleanup(state.conn.Close)
	go watchTermination(state)

	go func() {
		<-sig.done
		_, _ = fmt.Fprintln(inPw, `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`)
	}()

	return state
}

// deliverTurnCompleted is serialized with RunTurn's Take through the
// inbox mutex, so everything the test did before it, the cancellation
// flag included, is visible when RunTurn reads ctx.Err().
func deliverTurnCompleted(state *sessionState, params string) {
	state.inbox.Put(jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(params)})
}

func onEventSignalingSessionStarted(onEvent func(domain.AgentEvent), started chan<- struct{}) func(domain.AgentEvent) {
	var once sync.Once
	return func(e domain.AgentEvent) {
		if e.Type == domain.EventSessionStarted {
			once.Do(func() { close(started) })
		}
		onEvent(e)
	}
}

func TestRunTurn_InterruptedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		contextIsLive bool
		wantEvidence  agentcore.TurnEvidence
	}{
		{
			name:          "live context maps to failure, preserving the retryable classification",
			contextIsLive: true,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
				TerminalMessage:   "turn interrupted",
			},
		},
		{
			name:          "already-cancelled context maps to cancellation",
			contextIsLive: false,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: "turn cancelled after the runtime reported status interrupted",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newInterruptedStatusState(t, nil)

			var cancelled atomic.Bool
			ctx := atomicErrContext{Context: context.Background(), cancelled: &cancelled}

			type outcome struct {
				result domain.TurnResult
				err    error
			}
			outcomeCh := make(chan outcome, 1)

			adapter, _ := NewCodexAdapter()
			go func() {
				result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
					Prompt:  "go",
					OnEvent: func(domain.AgentEvent) {},
				})
				outcomeCh <- outcome{result, err}
			}()

			if !tt.contextIsLive {
				cancelled.Store(true)
			}

			deliverTurnCompleted(state, `{"turn":{"id":"turn-001","status":"interrupted"}}`)

			got := <-outcomeCh
			result, err := got.result, got.err

			dispositiontest.AssertDispositionContract(t, tt.wantEvidence, result, err)
		})
	}
}

func TestRunTurn_CompletedNotificationUnderCancelledContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		params      string
		wantMessage string
	}{
		{
			name:        "completed status with no error object",
			params:      `{"turn":{"id":"turn-001","status":"completed"}}`,
			wantMessage: "turn cancelled after the runtime reported status completed",
		},
		{
			name:        "interrupted status with no error object",
			params:      `{"turn":{"id":"turn-001","status":"interrupted"}}`,
			wantMessage: "turn cancelled after the runtime reported status interrupted",
		},
		{
			name:        "failed status carrying a turn.error object",
			params:      `{"turn":{"id":"turn-001","status":"failed","error":{"message":"context window exceeded","codexErrorInfo":"ContextWindowExceeded"}}}`,
			wantMessage: "turn cancelled after the runtime reported status failed: context window exceeded",
		},
		{
			name:        "unrecognized status",
			params:      `{"turn":{"id":"turn-001","status":"queued_for_review"}}`,
			wantMessage: "turn cancelled after the runtime reported status queued_for_review",
		},
		{
			name:        "payload with no status member",
			params:      `{"turn":{"id":"turn-001"}}`,
			wantMessage: "turn cancelled after the runtime reported no status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newInterruptedStatusState(t, nil)

			var cancelled atomic.Bool
			ctx := atomicErrContext{Context: context.Background(), cancelled: &cancelled}

			type outcome struct {
				result domain.TurnResult
				err    error
			}
			outcomeCh := make(chan outcome, 1)

			adapter, _ := NewCodexAdapter()
			go func() {
				result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
					Prompt:  "go",
					OnEvent: func(domain.AgentEvent) {},
				})
				outcomeCh <- outcome{result, err}
			}()

			cancelled.Store(true)

			deliverTurnCompleted(state, tt.params)

			got := <-outcomeCh
			result, err := got.result, got.err

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: tt.wantMessage,
			}, result, err)
		})
	}
}

func turnFailedFixture(codexErrorInfo string) string {
	return `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"failed","error":{"message":"token expired","codexErrorInfo":` + codexErrorInfo + `}}}}` + "\n"
}

func TestRunTurn_FailedOrUnrecognizedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantKind    domain.AgentErrorKind
		wantMessage string
	}{
		{
			name:        "failed status with no turn.error object",
			fixture:     `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"failed"}}}` + "\n",
			wantKind:    domain.ErrTurnFailed,
			wantMessage: "turn failed",
		},
		{name: "unauthorized as a string", fixture: turnFailedFixture(`"unauthorized"`), wantKind: domain.ErrCredentialUnverified, wantMessage: agentcore.CredentialRefusedMessage("token expired")},
		{name: "unauthorized as an object", fixture: turnFailedFixture(`{"unauthorized":{"httpStatusCode":401}}`), wantKind: domain.ErrCredentialUnverified, wantMessage: agentcore.CredentialRefusedMessage("token expired")},
		{name: "connection error as an object", fixture: turnFailedFixture(`{"httpConnectionFailed":{"httpStatusCode":502}}`), wantKind: domain.ErrTurnFailed, wantMessage: "token expired"},
		{name: "unrecognized error info shape", fixture: turnFailedFixture(`7`), wantKind: domain.ErrTurnFailed, wantMessage: "token expired"},
		{
			name:        "unrecognized status",
			fixture:     `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"queued_for_review"}}}` + "\n",
			wantKind:    domain.ErrTurnFailed,
			wantMessage: "turn queued_for_review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}` + "\n" + tt.fixture
			state := makeTestState(t, []byte(fixture))
			adapter, _ := NewCodexAdapter()

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != tt.wantKind {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, tt.wantKind)
			}
			if agentErr.Message != tt.wantMessage {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMessage)
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: tt.wantKind,
				TerminalMessage:   tt.wantMessage,
			}, result, err)
		})
	}
}

type replayAdapter struct {
	*CodexAdapter
	state *sessionState
}

func (r replayAdapter) StartSession(context.Context, domain.StartSessionParams) (domain.Session, error) {
	return fakeSession(r.state), nil
}

func (replayAdapter) StopSession(context.Context, domain.Session) error { return nil }

func recordedRefusalTurn(t *testing.T, mutate func(string) string) []byte {
	t.Helper()

	_, turn, ok := strings.Cut(string(loadFixture(t, "usage_limit_exceeded.jsonl")), `{"id":3,`)
	if !ok {
		t.Fatalf("usage_limit_exceeded.jsonl has no turn/start response")
	}
	return []byte(mutate(`{"id":1,` + turn))
}

func windowBelowLimit(t *testing.T) func(string) string {
	t.Helper()
	return func(recorded string) string {
		const exhausted, open = `"usedPercent":100`, `"usedPercent":99`
		if !strings.Contains(recorded, exhausted) {
			t.Fatalf("recorded refusal has no %s window", exhausted)
		}
		return strings.Replace(recorded, exhausted, open, 1)
	}
}

func withRateLimitsBefore(t *testing.T, rateLimits string) func(string) string {
	t.Helper()
	return func(recorded string) string {
		const recordedLine = `{"method":"account/rateLimits/updated"`
		if !strings.Contains(recorded, recordedLine) {
			t.Fatalf("recorded refusal has no %s line", recordedLine)
		}
		inserted := `{"method":"account/rateLimits/updated","params":{"rateLimits":` + rateLimits + `}}` + "\n"
		return strings.Replace(recorded, recordedLine, inserted+recordedLine, 1)
	}
}

func TestRunTurn_UsageLimitRefusal(t *testing.T) {
	t.Parallel()

	replay := func(model string, mutate func(string) string) *replayAdapter {
		state := makeTestState(t, recordedRefusalTurn(t, mutate))
		state.passthrough.Model = model
		return &replayAdapter{CodexAdapter: &CodexAdapter{}, state: state}
	}
	exhaustedBucket := func(limitID, limitName string) string {
		return `{"limitId":"` + limitID + `","limitName":"` + limitName + `","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":4103049600}}`
	}
	const weeklyHeld = `{"limitId":"codex","limitName":null,"primary":null,"secondary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":4103049600}}`
	recordedReset, laterReset := time.Unix(4102444800, 0).UTC(), time.Unix(4103049600, 0).UTC()
	var mu sync.Mutex
	var windows []domain.UsageWindow
	collect := func(e domain.AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		windows = append(windows, e.UsageWindows...)
	}
	cases := []agenttest.UsageLimitRefusalCase{
		{
			Name:         "recorded refusal",
			Adapter:      replay("", func(s string) string { return s }),
			Turn:         domain.RunTurnParams{OnEvent: collect},
			WantResetsAt: recordedReset,
		},
		{Name: "window below the limit", Adapter: replay("", windowBelowLimit(t))},
		{
			Name:         "weekly window held across notifications",
			Adapter:      replay("", withRateLimitsBefore(t, weeklyHeld)),
			WantResetsAt: laterReset,
		},
		{
			Name:    "reset already past",
			Adapter: replay("", func(s string) string { return strings.Replace(s, "4102444800", "946684800", 1) }),
		},
		{
			Name:         "another model's bucket",
			Adapter:      replay("gpt-5.4", withRateLimitsBefore(t, exhaustedBucket("codex_bengalfox", "gpt-5.2-codex-sonic"))),
			WantResetsAt: recordedReset,
		},
		{
			Name:         "the configured model's bucket",
			Adapter:      replay("gpt-5.2-codex-sonic", withRateLimitsBefore(t, exhaustedBucket("codex_bengalfox", "gpt-5.2-codex-sonic"))),
			WantResetsAt: laterReset,
		},
		{
			Name:         "bucket label",
			Adapter:      replay("gpt-5.4", withRateLimitsBefore(t, exhaustedBucket("codex_other", "codex_other"))),
			WantResetsAt: laterReset,
		},
	}

	agenttest.AssertUsageLimitRefusal(t, "codex", cases)

	mu.Lock()
	defer mu.Unlock()
	if len(windows) != 1 {
		t.Fatalf("usage windows emitted by the recorded refusal = %+v, want exactly one for the primary window", windows)
	}
	got := windows[0]
	if got.Key != "codex/primary" || got.Label != "5-hour limit" || got.Status != "" {
		t.Errorf("window = {Key %q, Label %q, Status %q}, want {%q, %q, %q}", got.Key, got.Label, got.Status, "codex/primary", "5-hour limit", "")
	}
	if got.UsedPercent == nil || *got.UsedPercent != 100 {
		t.Errorf("window UsedPercent = %v, want 100", got.UsedPercent)
	}
	if want := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC); !got.ResetsAt.Equal(want) {
		t.Errorf("window ResetsAt = %v, want %v", got.ResetsAt, want)
	}
	if got.ReportedAt.IsZero() {
		t.Error("window ReportedAt is zero, want the instant the report was observed")
	}
}

func TestRunTurn_EmptyTurnStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params string
	}{
		{"params do not unmarshal into an object", `[1,2,3]`},
		{"turn object omits the status member", `{"turn":{"id":"turn-001"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}` + "\n" +
				`{"method":"turn/completed","params":` + tt.params + `}` + "\n"
			state := makeTestState(t, []byte(fixture))
			adapter, _ := NewCodexAdapter()

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != domain.ErrTurnFailed {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
			}
			if agentErr.Message != "turn failed" {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, "turn failed")
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
			}, result, err)
		})
	}
}

func TestRunTurn_StdoutParseFailure(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"not valid json\n"
	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter()

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if !strings.HasPrefix(agentErr.Message, "stdout read error: ") {
		t.Errorf("AgentError.Message = %q, want prefix %q", agentErr.Message, "stdout read error: ")
	}

	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != agentErr.Message {
		t.Errorf("turn_failed Message = %q, want the same text as AgentError.Message %q", turnFailedEvents[0].Message, agentErr.Message)
	}
}

// entrySignalingWriter closes done when Write is entered, not when it
// returns like signalingWriter, so a test sees a write that never returns.
type entrySignalingWriter struct {
	w    io.Writer
	once sync.Once
	done chan struct{}
}

func newEntrySignalingWriter(w io.Writer) *entrySignalingWriter {
	return &entrySignalingWriter{w: w, done: make(chan struct{})}
}

func (s *entrySignalingWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.done) })
	return s.w.Write(p)
}

func TestStopSession_ReturnsWhileWriteParked(t *testing.T) {
	t.Parallel()

	// No process, no wait channel and readerDone already closed: no other
	// bounded wait in StopSession can mask a Close that waits for the write.
	pr, pw := io.Pipe()
	sig := newEntrySignalingWriter(pw)

	readerDone := make(chan struct{})
	close(readerDone)

	state := &sessionState{
		inbox:      jsonrpc.NewInbox[jsonrpc.Message](),
		readerDone: readerDone,
	}
	state.conn = jsonrpc.NewConn(sig, strings.NewReader(""), jsonrpc.Deliver(state.inbox, identity))

	written := make(chan struct{})
	go func() {
		_ = state.conn.Notify("parked/notify", nil)
		close(written)
	}()

	// Registered before the assertions below, so a t.Fatal on the
	// regression path still releases the parked write.
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
		waittest.Receive(t, written, "parked write release")
	})

	waittest.Receive(t, sig.done, "write entering the wrapped writer")

	stopErr := make(chan error, 1)
	go func() {
		stopErr <- (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state})
	}()

	if err := waittest.Receive(t, stopErr, "StopSession return while a write is parked"); err != nil {
		t.Errorf("StopSession() error = %v, want nil", err)
	}
}

type handlerParkedOutcome struct {
	result domain.TurnResult
	err    error
}

const (
	scenarioBurstDuringTurnOpening = "burst-turn-opening"
	scenarioBurstBetweenTurns      = "burst-between-turns"
	scenarioHandshakeBurstLogin    = "handshake-burst-login"
	scenarioHandshakeBurstThread   = "handshake-burst-thread"
	scenarioRecordFrames           = "record-frames"
)

// burstCount notifications, at least 4096 and 1 MiB in one write, park
// any bounded hand-off and fill every platform's pipe buffer.
const burstCount = 27000

// fakeScenarios gains unix-only scenarios from those files' init:
// package variables are initialized before any init runs.
var fakeScenarios = map[string]agenttest.Scenario{
	scenarioBurstDuringTurnOpening: agenttest.Typed(func(_ []string, _ struct{}) int {
		return serveBurstDuringTurnOpening(newFakeAppServerScanner(), os.Stdout)
	}),
	scenarioBurstBetweenTurns: agenttest.Typed(func(_ []string, params burstBetweenTurnsParams) int {
		return serveBurstBetweenTurns(newFakeAppServerScanner(), os.Stdout, params.MarkerPath)
	}),
	scenarioHandshakeBurstLogin: agenttest.Typed(func(_ []string, _ struct{}) int {
		return serveHandshakeBurstLogin(newFakeAppServerScanner(), os.Stdout)
	}),
	scenarioHandshakeBurstThread: agenttest.Typed(func(_ []string, _ struct{}) int {
		return serveHandshakeBurstThread(newFakeAppServerScanner(), os.Stdout)
	}),
	scenarioRecordFrames: agenttest.Typed(func(_ []string, params recordFramesParams) int {
		return serveRecordFrames(newFakeAppServerScanner(), os.Stdout, params)
	}),
}

func TestMain(m *testing.M) {
	agenttest.Main(m, fakeScenarios)
}

type burstBetweenTurnsParams struct {
	MarkerPath string `json:"markerPath,omitempty"`
}

type recordFramesParams struct {
	RecordPath        string   `json:"recordPath"`
	ConfigReadServers []string `json:"configReadServers,omitempty"`
	ConfigReadFails   bool     `json:"configReadFails,omitempty"`
	HeldThread        string   `json:"heldThread,omitempty"`
	ExitOn            string   `json:"exitOn,omitempty"`
}

func answerConfigRead(out io.Writer, id json.RawMessage, params recordFramesParams) bool {
	if params.ConfigReadFails {
		return writeJSON(out, map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}
	servers := map[string]any{}
	for _, name := range params.ConfigReadServers {
		servers[name] = struct{}{}
	}
	return writeJSON(out, rpcResult{ID: id, Result: map[string]any{"config": map[string]any{"mcp_servers": servers}}})
}

func serveRecordFrames(client *bufio.Scanner, out io.Writer, params recordFramesParams) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	record, err := os.OpenFile(params.RecordPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 1
	}
	defer func() { _ = record.Close() }()

	for turn := 1; client.Scan(); {
		var frame fakeFrame
		if json.Unmarshal(client.Bytes(), &frame) != nil {
			return 1
		}
		if params.ExitOn != "" && frame.Method == params.ExitOn {
			return 0
		}
		var result, completed any
		switch frame.Method {
		case "config/read":
			if !answerConfigRead(out, frame.ID, params) {
				return 1
			}
			continue
		case "thread/start", "thread/resume":
			if frame.Method == "thread/resume" && params.HeldThread != "" && frame.Params.ThreadID != params.HeldThread {
				if !writeJSON(out, map[string]any{"id": frame.ID, "error": map[string]any{"code": -32002, "message": "thread not found"}}) {
					return 1
				}
				continue
			}
			result, completed = map[string]any{"thread": map[string]any{"id": "fake-thread-1"}}, rpcNotification{Method: "thread/started"}
		case "turn/start":
			id := fmt.Sprintf("t%d", turn)
			turn++
			result, completed = map[string]any{"turn": map[string]any{"id": id}}, rpcNotification{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": id, "status": "completed"}}}
		default:
			continue
		}
		if _, err := fmt.Fprintf(record, "%s\n", client.Bytes()); err != nil || !writeJSON(out, rpcResult{ID: frame.ID, Result: result}) || !writeJSON(out, completed) {
			return 1
		}
	}
	return 0
}

func fakeAppServer(t *testing.T, scenario string) string {
	t.Helper()
	return agenttest.FakeRuntime(t, t.TempDir(), "codex", scenario, struct{}{})
}

func newFakeAppServerScanner() *bufio.Scanner {
	client := bufio.NewScanner(os.Stdin)
	client.Buffer(make([]byte, 0, 4096), 1024*1024)
	return client
}

type fakeFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ThreadID string `json:"threadId"`
	} `json:"params"`
}

type rpcResult struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
}

type rpcNotification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

func nextFrame(client *bufio.Scanner) (fakeFrame, bool) {
	if !client.Scan() {
		return fakeFrame{}, false
	}
	var frame fakeFrame
	if err := json.Unmarshal(client.Bytes(), &frame); err != nil {
		return fakeFrame{}, false
	}
	return frame, true
}

func writeJSON(out io.Writer, v any) bool {
	line, err := json.Marshal(v)
	if err != nil {
		return false
	}
	_, err = out.Write(append(line, '\n'))
	return err == nil
}

func answerPreThreadHandshake(client *bufio.Scanner, out io.Writer) bool {
	initialize, ok := nextFrame(client)
	if !ok {
		return false
	}
	if !writeJSON(out, rpcResult{ID: initialize.ID, Result: struct{}{}}) {
		return false
	}

	if _, ok := nextFrame(client); !ok {
		return false
	}

	accountRead, ok := nextFrame(client)
	if !ok {
		return false
	}
	return writeJSON(out, rpcResult{ID: accountRead.ID, Result: struct{}{}})
}

func fillerNotifications() string {
	var fill strings.Builder
	for i := range burstCount {
		line, err := json.Marshal(rpcNotification{Method: "filler/notification", Params: map[string]any{"i": i}})
		if err != nil {
			continue
		}
		fill.Write(line)
		fill.WriteByte('\n')
	}
	return fill.String()
}

func serveBurstDuringTurnOpening(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: threadStart.ID, Result: map[string]any{"thread": map[string]any{"id": "fake-thread-1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "thread/started"}) {
		return 1
	}

	turnStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: turnStart.ID, Result: map[string]any{"turn": map[string]any{"id": "t1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "t1", "status": "completed"}}}) {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

func serveBurstBetweenTurns(client *bufio.Scanner, out io.Writer, markerPath string) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: threadStart.ID, Result: map[string]any{"thread": map[string]any{"id": "fake-thread-1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "thread/started"}) {
		return 1
	}

	turnStart1, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: turnStart1.ID, Result: map[string]any{"turn": map[string]any{"id": "t1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "t1", "status": "completed"}}}) {
		return 1
	}

	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if markerPath != "" {
		if err := os.WriteFile(markerPath, []byte("done"), 0o644); err != nil {
			return 1
		}
	}

	turnStart2, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: turnStart2.ID, Result: map[string]any{"turn": map[string]any{"id": "t2"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "t2", "status": "completed"}}}) {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

func serveHandshakeBurstLogin(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	loginStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "account/login/completed", Params: map[string]any{"success": true}}) {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: loginStart.ID, Result: struct{}{}}) {
		return 1
	}

	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: threadStart.ID, Result: map[string]any{"thread": map[string]any{"id": "fake-thread-1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "thread/started"}) {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

func serveHandshakeBurstThread(client *bufio.Scanner, out io.Writer) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if _, err := io.WriteString(out, fillerNotifications()); err != nil {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "thread/started"}) {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: threadStart.ID, Result: map[string]any{"thread": map[string]any{"id": "fake-thread-1"}}}) {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

func waitChOpen(state *sessionState) bool {
	select {
	case <-state.waitCh:
		return false
	default:
		return true
	}
}

func countBlankPings(events []domain.AgentEvent) int {
	var n int
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == "" {
			n++
		}
	}
	return n
}

func TestRunTurn_BurstDuringTurnOpeningDoesNotHang(t *testing.T) {
	t.Parallel()

	command := fakeAppServer(t, scenarioBurstDuringTurnOpening)
	adapter := &CodexAdapter{}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: command},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	var events []domain.AgentEvent
	outcomeCh := make(chan handlerParkedOutcome, 1)
	go func() {
		result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "work",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- handlerParkedOutcome{result: result, err: runErr}
	}()

	got := waittest.Receive(t, outcomeCh, "RunTurn outcome")

	if got.err != nil {
		t.Errorf("RunTurn() error = %v, want nil", got.err)
	}
	if got.result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn() ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCompleted)
	}
	if !waitChOpen(state) {
		t.Error("state.waitCh is closed, want the runtime still alive and unreaped")
	}
	if n := countBlankPings(events); n != burstCount {
		t.Errorf("filler notification events = %d, want %d", n, burstCount)
	}
}

func TestRunTurn_BurstBetweenTurnsDoesNotHang(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker")
	command := agenttest.FakeRuntime(t, t.TempDir(), "codex", scenarioBurstBetweenTurns, burstBetweenTurnsParams{MarkerPath: markerPath})
	adapter := &CodexAdapter{}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: command},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(1) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(1) ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	waittest.File(t, markerPath)

	var events []domain.AgentEvent
	outcomeCh := make(chan handlerParkedOutcome, 1)
	go func() {
		result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "second",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- handlerParkedOutcome{result: result, err: runErr}
	}()

	got := waittest.Receive(t, outcomeCh, "RunTurn(2) outcome")

	if got.err != nil {
		t.Errorf("RunTurn(2) error = %v, want nil", got.err)
	}
	if got.result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(2) ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCompleted)
	}
	if !waitChOpen(state) {
		t.Error("state.waitCh is closed, want the runtime still alive and unreaped")
	}
	if n := countBlankPings(events); n != burstCount {
		t.Errorf("filler notification events = %d, want %d", n, burstCount)
	}
}

func TestStartSession_HandshakeBurstDoesNotLoseAwaitedNotification(t *testing.T) {
	const readTimeout = waittest.HangGuard
	tests := []struct {
		name     string
		scenario string
		apiKey   string
	}{
		{
			name:     "account/login/completed observed despite the burst",
			scenario: scenarioHandshakeBurstLogin,
			apiKey:   "test-api-key",
		},
		{
			name:     "thread/started observed despite the burst",
			scenario: scenarioHandshakeBurstThread,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CODEX_API_KEY", tt.apiKey)

			command := fakeAppServer(t, tt.scenario)
			adapter := &CodexAdapter{}

			start := time.Now()
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig: domain.AgentConfig{
					Command:       command,
					ReadTimeoutMS: int(readTimeout.Milliseconds()),
				},
			})
			elapsed := time.Since(start)

			if err != nil {
				var agentErr *domain.AgentError
				if errors.As(err, &agentErr) && agentErr.Kind == domain.ErrResponseError && strings.Contains(agentErr.Message, "account/login/completed") {
					t.Fatalf("StartSession() lost the awaited notification to the burst and failed authentication: %v", err)
				}
				t.Fatalf("StartSession() error = %v, want nil", err)
			}
			t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

			// A lost thread/started is accepted silently at the read
			// timeout, so waiting it out is the only trace it leaves.
			if elapsed >= readTimeout {
				t.Fatalf("StartSession() took %v, want less than the %v read timeout: it waited the timeout out, so the awaited notification was lost to the burst", elapsed, readTimeout)
			}
		})
	}
}

func TestStartSession_LocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	const varName = "SORTIE_CODEX_STARTSESSION_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	for _, tc := range []struct {
		name        string
		sshEnvNames []string
	}{
		{"SSHEnvNames absent", nil},
		{"SSHEnvNames naming a set variable", []string{varName}},
	} {
		command := fakeAppServer(t, scenarioHandshakeBurstThread)
		adapter := &CodexAdapter{}

		session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: command},
			SSHEnvNames:   tc.sshEnvNames,
		})
		if err != nil {
			t.Fatalf("%s: StartSession() error = %v, want nil", tc.name, err)
		}
		if err := adapter.StopSession(context.Background(), session); err != nil {
			t.Errorf("%s: StopSession() error = %v", tc.name, err)
		}
	}
}

package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/storage"
	"github.com/sortie-ai/sortie/internal/storage/sqlite"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func retainingState() (*State, *RunningEntry) {
	const issueID, dispatchID = "ISS-1", "d-1"
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	entry := &RunningEntry{
		Identifier: issueID + "-ident",
		StartedAt:  baseTime,
		DispatchID: dispatchID,
		events:     state.RetainedEvents.register(dispatchID),
	}
	state.Running[issueID] = entry
	state.Claimed[issueID] = struct{}{}
	return state, entry
}

func notificationAt(i int, message string) domain.AgentEvent {
	return domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: baseTime.Add(time.Duration(i) * time.Second),
		Message:   message,
	}
}

func numberedNotifications(count int, suffix string) []domain.AgentEvent {
	events := make([]domain.AgentEvent, count)
	for i := range events {
		events[i] = notificationAt(i+1, fmt.Sprintf("note %d %s", i+1, suffix))
	}
	return events
}

func TestHandleAgentEvent_RetainsNewestEventsInOrder(t *testing.T) {
	t.Parallel()

	secret := eventTestSecret(t)
	redact.Add("test.run events masking", secret)

	var newest []string
	for i := 201; i <= 1200; i++ {
		newest = append(newest, fmt.Sprintf("note %d %s", i, redact.Marker))
	}

	tests := []struct {
		name         string
		events       []domain.AgentEvent
		wantFirstSeq int64
		wantMessages []string
	}{
		{
			name:         "1200 notifications keep the newest 1000 with every message masked",
			events:       numberedNotifications(1200, secret),
			wantFirstSeq: 201,
			wantMessages: newest,
		},
		{
			name: "liveness ping and token usage are not retained",
			events: []domain.AgentEvent{
				notificationAt(1, "first"),
				notificationAt(2, "  "),
				{Type: domain.EventTokenUsage, Timestamp: baseTime.Add(3 * time.Second), Usage: domain.TokenUsage{InputTokens: 5, TotalTokens: 5}},
				notificationAt(4, "second"),
			},
			wantFirstSeq: 1,
			wantMessages: []string{"first", "second"},
		},
		{
			name: "a run with three events returns those three",
			events: []domain.AgentEvent{
				{Type: domain.EventSessionStarted, Timestamp: baseTime.Add(time.Second), SessionID: "s1", Message: "started"},
				notificationAt(2, "working"),
				{Type: domain.EventTurnCompleted, Timestamp: baseTime.Add(3 * time.Second), Message: "done"},
			},
			wantFirstSeq: 1,
			wantMessages: []string{"started", "working", "done"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, entry := retainingState()

			for _, event := range tt.events {
				HandleAgentEvent(state, "ISS-1", event, discardLogger(), nil)
				if !entry.LastAgentTimestamp.Equal(event.Timestamp) {
					t.Fatalf("LastAgentTimestamp after %s at %v = %v, want it advanced", event.Type, event.Timestamp, entry.LastAgentTimestamp)
				}
			}

			got, ok := state.RetainedEvents.Events("d-1")
			if !ok {
				t.Fatalf("Events(%q) ok = false, want true", "d-1")
			}
			if len(got) != len(tt.wantMessages) {
				t.Fatalf("Events(%q) returned %d events, want %d", "d-1", len(got), len(tt.wantMessages))
			}
			for i, event := range got {
				if want := tt.wantFirstSeq + int64(i); event.Seq != want {
					t.Errorf("Events(%q)[%d].Seq = %d, want %d", "d-1", i, event.Seq, want)
				}
				if event.Message != tt.wantMessages[i] {
					t.Errorf("Events(%q)[%d].Message = %q, want %q", "d-1", i, event.Message, tt.wantMessages[i])
				}
			}
		})
	}
}

func TestHandleAgentEvent_BufferStaysWithinBothBounds(t *testing.T) {
	t.Parallel()

	const phaseLength = 1250
	state, entry := retainingState()
	buffer := entry.events
	live := func() int { return len(buffer.events) - buffer.head }

	var countBound, byteBound bool
	for i := range 10_000 {
		message := "x"
		if i/phaseLength%2 == 1 {
			message = strings.Repeat("y", 999)
		}
		eventType := domain.EventNotification
		if i%2 == 1 {
			eventType = domain.EventOtherMessage
		}
		before := live()

		HandleAgentEvent(state, "ISS-1", domain.AgentEvent{
			Type: eventType, Timestamp: baseTime.Add(time.Duration(i+1) * time.Second), Message: message,
		}, discardLogger(), nil)

		after := live()
		countBound = countBound || before == 1000 && after == 1000
		byteBound = byteBound || before < 1000 && after <= before
		if after > 1000 || buffer.bytes > 256<<10 {
			t.Fatalf("after event %d buffer holds %d events, %d bytes, want at most %d events and %d bytes",
				i+1, after, buffer.bytes, 1000, 256<<10)
		}
	}

	if !countBound || !byteBound {
		t.Errorf("count bound binding = %v, byte bound binding = %v, want both", countBound, byteBound)
	}
}

func TestHandleWorkerExit_PersistedRunEventsReadBackEqual(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := t.TempDir() + "/events.db"
	store := openFileStore(t, dbPath)
	state, _ := retainingState()

	for _, event := range []domain.AgentEvent{
		{Type: domain.EventSessionStarted, Timestamp: baseTime.Add(1 * time.Second), SessionID: "sess-1", Message: "Session started"},
		{Type: domain.EventTurnCompleted, Timestamp: baseTime.Add(2 * time.Second), Message: "turn one"},
		{Type: domain.EventTokenUsage, Timestamp: baseTime.Add(3 * time.Second), Usage: domain.TokenUsage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140}},
		{Type: domain.EventToolResult, Timestamp: baseTime.Add(4 * time.Second), ToolName: "Bash", ToolKind: domain.ToolKindRead, ToolArgument: "internal/foo.go", ToolDurationMS: 2400, ToolError: true, Message: "exit 1"},
		{Type: domain.EventTurnCompleted, Timestamp: baseTime.Add(5 * time.Second), Message: "turn two"},
	} {
		HandleAgentEvent(state, "ISS-1", event, discardLogger(), nil)
	}
	want, ok := state.RetainedEvents.Events("d-1")
	if !ok || len(want) != 4 {
		t.Fatalf("Events(%q) = %d events, ok %v, want 4 events and true", "d-1", len(want), ok)
	}

	exitRun(state, store)

	if _, ok := state.RetainedEvents.Events("d-1"); ok {
		t.Errorf("Events(%q) ok = true after the exit, want false", "d-1")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openFileStore(t, dbPath)
	rows, err := reopened.RunEvents(ctx, "d-1", 0)
	if err != nil {
		t.Fatalf("RunEvents(%q, 0): %v", "d-1", err)
	}
	got, err := RunEventsFromRows(rows)
	if err != nil {
		t.Fatalf("RunEventsFromRows: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("RunEventsFromRows returned %d events, want %d", len(got), len(want))
	}
	for _, event := range got {
		if event.Event == domain.EventToolResult && (event.ToolKind != domain.ToolKindRead || event.ToolArgument != "internal/foo.go") {
			t.Errorf("tool_result kind, argument = %q, %q, want %q, %q", event.ToolKind, event.ToolArgument, domain.ToolKindRead, "internal/foo.go")
		}
	}
	for i := range want {
		if !bytes.Equal(got[i].Payload(), want[i].Payload()) {
			t.Errorf("event %d Payload() = %s, want %s", i, got[i].Payload(), want[i].Payload())
		}
		if got[i].Seq != want[i].Seq || got[i].Event != want[i].Event || !got[i].At.Equal(want[i].At) || got[i].size != want[i].size {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	history, err := reopened.QueryRunHistory(ctx, storage.RunHistoryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("QueryRunHistory: %v", err)
	}
	if len(history) != 1 || history[0].DispatchID == nil || *history[0].DispatchID != "d-1" {
		t.Errorf("run_history rows = %+v, want one row with dispatch ID %q", history, "d-1")
	}
}

func exitRun(state *State, store OrchestratorStore) {
	HandleWorkerExit(state, WorkerResult{
		IssueID: "ISS-1", Identifier: "ISS-1-ident", ExitKind: WorkerExitNormal, AgentAdapter: "mock",
	}, HandleWorkerExitParams{
		Store:             store,
		MaxRetryBackoffMS: 300_000,
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:            discardLogger(),
		Ctx:               context.Background(),
	})
}

func TestHandleAgentEvent_SkillCallsOutliveTheEventBuffer(t *testing.T) {
	t.Parallel()

	const calls = 1001
	state, entry := retainingState()
	store := openFileStore(t, t.TempDir()+"/skills.db")
	want := make([]domain.SkillCall, calls)

	for i := range calls {
		skill := fmt.Sprintf("skill-%d", i+1)
		argument := skill
		if i == 1 {
			argument += "\u202E"
		}
		want[i] = domain.SkillCall{Skill: skill, ToolError: i == 0}
		HandleAgentEvent(state, "ISS-1", domain.AgentEvent{
			Type: domain.EventToolResult, Timestamp: baseTime.Add(time.Duration(2*i+1) * time.Second),
			ToolName: "Skill", ToolKind: domain.ToolKindSkill, ToolArgument: argument, ToolError: i == 0,
		}, discardLogger(), nil)
		if i == calls/2 {
			HandleAgentEvent(state, "ISS-1", domain.AgentEvent{
				Type: domain.EventToolResult, Timestamp: baseTime.Add(time.Duration(2*i+2) * time.Second),
				ToolName: "Bash", ToolKind: domain.ToolKindShell, ToolArgument: "ls",
			}, discardLogger(), nil)
		}
	}

	if live := len(entry.events.events) - entry.events.head; live > 1000 {
		t.Errorf("buffer holds %d events, want at most 1000", live)
	}

	exitRun(state, store)

	history, err := store.QueryRunHistory(context.Background(), storage.RunHistoryFilter{Limit: 10})
	if err != nil || len(history) != 1 || history[0].Skills == nil {
		t.Fatalf("QueryRunHistory = %+v, %v, want one row holding skills", history, err)
	}
	var got []domain.SkillCall
	if err := json.Unmarshal([]byte(*history[0].Skills), &got); err != nil {
		t.Fatalf("Unmarshal(skills): %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("skills = %d calls, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("skills[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	noCallState, _ := retainingState()
	noCallStore := openFileStore(t, t.TempDir()+"/no-skills.db")
	exitRun(noCallState, noCallStore)
	noCall, err := noCallStore.QueryRunHistory(context.Background(), storage.RunHistoryFilter{Limit: 10})
	if err != nil || len(noCall) != 1 || noCall[0].Skills == nil || *noCall[0].Skills != "[]" {
		t.Errorf("QueryRunHistory after a run without skill calls = %+v, %v, want one row storing %q", noCall, err, "[]")
	}
}

func openFileStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store
}

type failingRunEventStore struct {
	storage.RunEventStore

	failures int

	mu    sync.Mutex
	calls map[string]int
	last  map[string][]storage.RunEvent
}

func (s *failingRunEventStore) AppendRunEvents(_ context.Context, dispatchID, _ string, events []storage.RunEvent, _ storage.RunEventLimits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls, s.last = map[string]int{}, map[string][]storage.RunEvent{}
	}
	s.calls[dispatchID]++
	s.last[dispatchID] = events
	if s.calls[dispatchID] <= s.failures {
		return errors.New("disk full")
	}
	return nil
}

func (s *failingRunEventStore) callsFor(dispatchID string) (int, []storage.RunEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[dispatchID], s.last[dispatchID]
}

func heldDispatchIDs(r *RunEventRetry) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, len(r.held))
	for i, h := range r.held {
		ids[i] = h.run.DispatchID
	}
	return ids
}

func heldRunOf(dispatchID string, size int64) HeldRunEvents {
	return HeldRunEvents{
		IssueID: "ISS-" + dispatchID, Identifier: "ISS-" + dispatchID, DispatchID: dispatchID,
		CompletedAt: "2026-03-15T12:01:00Z",
		Events:      []storage.RunEvent{{Seq: 1, At: "2026-03-15T12:00:00Z", Event: "notification", Payload: `{"message":"m"}`, Size: size}},
	}
}

func TestRunEventRetry_StoresOrDropsHeldRuns(t *testing.T) {
	t.Parallel()

	const twoMiB = 2 << 20
	tests := []struct {
		name       string
		failures   int
		runs       []HeldRunEvents
		wantHeld   []string
		wantCalls  int
		wantEvents int
	}{
		{
			name:       "store failing once receives the events on the second retry",
			failures:   1,
			runs:       []HeldRunEvents{heldRunOf("a", 100)},
			wantHeld:   []string{"a"},
			wantCalls:  2,
			wantEvents: 1,
		},
		{
			name:       "store always failing never holds more than 4 MiB and drops the oldest first",
			failures:   1 << 30,
			runs:       []HeldRunEvents{heldRunOf("a", twoMiB), heldRunOf("b", twoMiB), heldRunOf("c", twoMiB)},
			wantHeld:   []string{"b", "c"},
			wantCalls:  5,
			wantEvents: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &failingRunEventStore{failures: tt.failures}
			retry := NewRunEventRetry(store, discardLogger())
			t.Cleanup(retry.Stop)
			retry.delays = []time.Duration{time.Hour, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}

			for _, run := range tt.runs {
				retry.Hold(run)
				retry.mu.Lock()
				heldBytes := retry.heldBytes
				retry.mu.Unlock()
				if heldBytes > 4<<20 {
					t.Fatalf("held bytes after Hold(%q) = %d, want at most %d", run.DispatchID, heldBytes, 4<<20)
				}
			}
			if got := heldDispatchIDs(retry); !slices.Equal(got, tt.wantHeld) {
				t.Fatalf("held runs = %v, want %v", got, tt.wantHeld)
			}

			retry.mu.Lock()
			held := slices.Clone(retry.held)
			retry.mu.Unlock()
			for _, h := range held {
				retry.retry(h)
			}
			waittest.Until(t, func() error {
				if held := heldDispatchIDs(retry); len(held) != 0 {
					return fmt.Errorf("held runs = %v, want none", held)
				}
				return nil
			})

			for _, id := range tt.wantHeld {
				calls, events := store.callsFor(id)
				if calls != tt.wantCalls || len(events) != tt.wantEvents {
					t.Errorf("AppendRunEvents for %q = %d calls with %d events, want %d calls with %d events", id, calls, len(events), tt.wantCalls, tt.wantEvents)
				}
			}
			if calls, _ := store.callsFor("a"); len(tt.runs) > len(tt.wantHeld) && calls != 0 {
				t.Errorf("AppendRunEvents calls for dropped %q = %d, want 0", "a", calls)
			}
		})
	}
}

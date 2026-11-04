package sqlite

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var unboundedRunEvents = storage.RunEventLimits{MaxAge: 100 * 365 * 24 * time.Hour, MaxBytes: math.MaxInt64, MaxExpiredRuns: 4}

func twoEvents() []storage.RunEvent {
	return []storage.RunEvent{
		{Seq: 1, At: "2026-03-19T10:00:00Z", Event: "notification", Payload: `{"message":"a"}`, Size: 10},
		{Seq: 2, At: "2026-03-19T10:00:01Z", Event: "notification", Payload: `{"message":"b"}`, Size: 10},
	}
}

func runWithDispatch(dispatchID, completedAt string) storage.RunHistory {
	run := newTestRun(1)
	run.DispatchID = &dispatchID
	run.CompletedAt = completedAt
	return run
}

func scalar(t *testing.T, s *Store, query string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func assertTotalMatchesSizes(t *testing.T, s *Store) {
	t.Helper()
	got := scalar(t, s, `SELECT bytes FROM run_events_total WHERE id = 1`)
	want := scalar(t, s, `SELECT COALESCE(SUM(size), 0) FROM run_events`)
	if got != want {
		t.Errorf("run_events_total.bytes = %d, want %d (sum of run_events.size)", got, want)
	}
}

func storedEventRuns(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `SELECT DISTINCT dispatch_id FROM run_events ORDER BY dispatch_id`)
	if err != nil {
		t.Fatalf("query stored event runs: %v", err)
	}
	defer rows.Close() //nolint:errcheck // test helper

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan stored event run: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stored event runs: %v", err)
	}
	return ids
}

func TestAppendRunHistoryWithEvents_FailureIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		trigger       string
		wantEventsErr bool
		wantHistory   int64
		wantTurns     int64
	}{
		{
			name:        "run_history insert raises",
			trigger:     `CREATE TRIGGER fail BEFORE INSERT ON run_history BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			wantHistory: 0,
		},
		{
			name:          "run_events insert raises after the first event",
			trigger:       `CREATE TRIGGER fail BEFORE INSERT ON run_events WHEN NEW.seq = 2 BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			wantEventsErr: true,
			wantHistory:   1,
			wantTurns:     1,
		},
		{
			name:        "run_turn_models insert raises",
			trigger:     `CREATE TRIGGER fail BEFORE INSERT ON run_turn_models BEGIN SELECT RAISE(ABORT, 'boom'); END`,
			wantHistory: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := mustOpenStore(t)
			if _, err := s.db.ExecContext(context.Background(), tt.trigger); err != nil {
				t.Fatalf("create trigger: %v", err)
			}

			run := runWithDispatch("d1", "2026-03-19T10:00:30Z")
			run.Turns = []storage.RunTurn{{
				Seq: 1, EndedAt: time.Date(2026, 3, 19, 10, 0, 10, 0, time.UTC), TokensMeasured: true,
				Models: map[string]storage.ModelTokens{"model-a": {InputTokens: 10}},
			}}
			_, err := s.AppendRunHistoryWithEvents(context.Background(), run, twoEvents(), unboundedRunEvents)

			var eventsErr *storage.RunEventsError
			if err == nil {
				t.Fatalf("AppendRunHistoryWithEvents() = nil, want error")
			}
			if got := errors.As(err, &eventsErr); got != tt.wantEventsErr {
				t.Errorf("AppendRunHistoryWithEvents() error %T is *storage.RunEventsError = %v, want %v", err, got, tt.wantEventsErr)
			}
			if got := scalar(t, s, `SELECT COUNT(*) FROM run_history`); got != tt.wantHistory {
				t.Errorf("run_history rows = %d, want %d", got, tt.wantHistory)
			}
			if got := scalar(t, s, `SELECT COUNT(*) FROM run_turns`); got != tt.wantTurns {
				t.Errorf("run_turns rows = %d, want %d", got, tt.wantTurns)
			}
			if got := scalar(t, s, `SELECT COUNT(*) FROM run_turn_models`); got != tt.wantTurns {
				t.Errorf("run_turn_models rows = %d, want %d", got, tt.wantTurns)
			}
			if got := scalar(t, s, `SELECT COUNT(*) FROM run_events`); got != 0 {
				t.Errorf("run_events rows = %d, want 0", got)
			}
			assertTotalMatchesSizes(t, s)
		})
	}
}

func TestAppendRunHistoryWithEvents_Pruning(t *testing.T) {
	t.Parallel()

	const written = "w"
	tests := []struct {
		name     string
		seeds    map[string]string
		maxBytes int64
		want     []string
	}{
		{
			name: "run older than max age is pruned and one within it kept",
			seeds: map[string]string{
				"expired": "2026-03-19T11:00:00Z",
				"fresh":   "2026-03-19T13:00:00Z",
			},
			maxBytes: math.MaxInt64,
			want:     []string{"fresh", written},
		},
		{
			name: "byte cap prunes the oldest runs whole until the sum fits",
			seeds: map[string]string{
				"a": "2026-03-20T09:00:00Z",
				"b": "2026-03-20T10:00:00Z",
				"c": "2026-03-20T11:00:00Z",
			},
			maxBytes: 45,
			want:     []string{"c", written},
		},
		{
			name: "byte cap below the written run keeps the written run",
			seeds: map[string]string{
				"a": "2026-03-20T09:00:00Z",
			},
			maxBytes: 1,
			want:     []string{written},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			s := mustOpenStore(t)
			for id, completedAt := range tt.seeds {
				if _, err := s.AppendRunHistoryWithEvents(ctx, runWithDispatch(id, completedAt), twoEvents(), unboundedRunEvents); err != nil {
					t.Fatalf("seed %q: %v", id, err)
				}
				assertTotalMatchesSizes(t, s)
			}

			limits := storage.RunEventLimits{MaxAge: 24 * time.Hour, MaxBytes: tt.maxBytes, MaxExpiredRuns: 4}
			if _, err := s.AppendRunHistoryWithEvents(ctx, runWithDispatch(written, "2026-03-20T12:00:00Z"), twoEvents(), limits); err != nil {
				t.Fatalf("AppendRunHistoryWithEvents(%q): %v", written, err)
			}

			if got := storedEventRuns(t, s); !slices.Equal(got, tt.want) {
				t.Errorf("runs with stored events = %v, want %v", got, tt.want)
			}
			assertTotalMatchesSizes(t, s)
		})
	}
}

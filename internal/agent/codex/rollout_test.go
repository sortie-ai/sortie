package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

type rolloutTurn struct {
	usage.Settlement
	Counts domain.TokenUsage
}

func rolloutCapture(t *testing.T, name string) [][]byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "rollout", name))
	if err != nil {
		t.Fatalf("reading capture %s: %v", name, err)
	}
	return bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
}

func rolloutSegments(lines [][]byte) [][][]byte {
	segments := [][][]byte{nil}
	for _, line := range lines {
		if bytes.Contains(line, []byte(`"type":"task_started"`)) {
			segments = append(segments, nil)
		}
		segments[len(segments)-1] = append(segments[len(segments)-1], line)
	}
	return segments
}

func rolloutMessages(appserver [][]byte) []jsonrpc.Message {
	var messages []jsonrpc.Message
	for _, line := range appserver {
		msg := jsonrpc.Message{Kind: jsonrpc.KindResponse}
		_ = json.Unmarshal(line, &msg)
		if msg.Method != "" {
			msg.Kind = jsonrpc.KindNotification
		}
		messages = append(messages, msg)
	}
	return messages
}

func rolloutReadFile(state *sessionState) usage.RecordsRead {
	return state.readRollout(context.Background(), slog.New(slog.DiscardHandler), true)
}

func rolloutInstall(t *testing.T, home string, state *sessionState, lines [][]byte) {
	t.Helper()

	file := filepath.Join(append([]string{home}, state.rollout.Segments...)...)
	err := os.MkdirAll(filepath.Dir(file), 0o750)
	if err == nil {
		err = os.WriteFile(file, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600)
	}
	if err != nil {
		t.Fatalf("installing rollout file: %v", err)
	}
}

func rolloutStart(t *testing.T, appserver, boundary, rollout [][]byte) (*sessionState, *usage.Ledger) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	state := &sessionState{target: agentcore.LaunchTarget{WorkspacePath: t.TempDir()}}
	for _, msg := range rolloutMessages(appserver) {
		var resp threadResult
		if json.Unmarshal(msg.Result, &resp) == nil && resp.path() != "" {
			state.rollout = rolloutPath(resp.path())
			break
		}
	}
	ledger, err := usage.NewLedger(usageSources.Live, usageSources.Records)
	if err != nil || state.rollout == nil {
		t.Fatalf("usage.NewLedger() error = %v, rollout path = %v", err, state.rollout)
	}
	if boundary == nil {
		ledger.SessionCreated()
	} else {
		rolloutInstall(t, home, state, boundary)
		read := rolloutReadFile(state)
		if !read.OK {
			t.Fatal("boundary read failed")
		}
		ledger.Boundary(read)
	}
	rolloutInstall(t, home, state, rollout)
	return state, ledger
}

func rolloutSettle(t *testing.T, appserver, boundary, rollout [][]byte) []rolloutTurn {
	t.Helper()

	state, ledger := rolloutStart(t, appserver, boundary, rollout)
	var (
		turnID string
		live   *usage.SpendReading
		prev   domain.TokenUsage
		turns  []rolloutTurn
	)
	for _, msg := range rolloutMessages(appserver) {
		var turn struct{ Turn struct{ ID, Status string } }
		_ = json.Unmarshal(msg.Params, &turn)
		switch msg.Method {
		case "turn/started":
			turnID = turn.Turn.ID
		case "thread/tokenUsage/updated":
			p, err := parseTokenUsageUpdated(msg.Params)
			if err != nil || p.TokenUsage == nil {
				t.Fatalf("parseTokenUsageUpdated() = %v, %v", p, err)
			}
			reading := p.TokenUsage.reading("")
			inTurn := turnID != "" && p.TurnID == turnID
			ledger.ObserveCumulative(reading, inTurn)
			if inTurn {
				live = &reading
			}
		case "turn/completed":
			status := turn.Turn.Status
			end := usage.TurnEnd{Replied: status != "", Cancelled: status == "interrupted", ModelReached: live != nil}
			var records usage.RecordsRead
			if ledger.NeedsRecords(end, live) {
				records = rolloutReadFile(state)
			}
			settled := ledger.Settle(end, live, records)
			turns = append(turns, rolloutTurn{settled, usage.Subtract(settled.Run, prev)})
			prev, turnID, live = settled.Run, "", nil
		}
	}
	return turns
}

func TestRollout_Location(t *testing.T) {
	tests := []struct {
		name, appserver, rollout string
		defaultHome              bool
		records                  int
	}{
		{name: "relocated home", appserver: "relocated_home_two_turns_appserver.jsonl", rollout: "relocated_home_two_turns_rollout.jsonl", records: 4},
		{name: "default home", appserver: "fresh_turn_default_home_appserver.jsonl", rollout: "fresh_turn_default_home_rollout.jsonl", defaultHome: true, records: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, _ := rolloutStart(t, rolloutCapture(t, tt.appserver), nil, rolloutCapture(t, tt.rollout))
			if tt.defaultHome {
				home := t.TempDir()
				t.Setenv("CODEX_HOME", "")
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				rolloutInstall(t, filepath.Join(home, ".codex"), state, rolloutCapture(t, tt.rollout))
			}

			got := rolloutReadFile(state)

			if !got.OK || len(got.Readings) != tt.records {
				t.Errorf("readRollout() = OK %v, %d readings, want OK true, %d readings", got.OK, len(got.Readings), tt.records)
			}
		})
	}

	for _, reported := range []string{"", "<CODEX_HOME>/archive/2026/10/29/rollout-x.jsonl"} {
		state := &sessionState{target: agentcore.LaunchTarget{WorkspacePath: t.TempDir()}, rollout: rolloutPath(reported)}

		if got := rolloutReadFile(state); got.OK {
			t.Errorf("readRollout() for reported path %q = OK true, want OK false", reported)
		}
	}
}

func TestRollout_SettlesEachTurnAtTheRunsRequests(t *testing.T) {
	tests := []struct {
		name                         string
		appserver, boundary, rollout string
		want                         []domain.TokenUsage
		lastFromRecords              bool
	}{
		{
			name:      "fresh turn",
			appserver: "fresh_turn_default_home_appserver.jsonl",
			rollout:   "fresh_turn_default_home_rollout.jsonl",
			want:      []domain.TokenUsage{{InputTokens: 43337, CacheReadTokens: 21376, OutputTokens: 93, TotalTokens: 43430}},
		},
		{
			name:      "two turns",
			appserver: "relocated_home_two_turns_appserver.jsonl",
			rollout:   "relocated_home_two_turns_rollout.jsonl",
			want: []domain.TokenUsage{
				{InputTokens: 26969, CacheReadTokens: 13312, OutputTokens: 63, TotalTokens: 27032},
				{InputTokens: 29140, CacheReadTokens: 27648, OutputTokens: 66, TotalTokens: 29206},
			},
		},
		{
			name:      "killed first run",
			appserver: "killed_turn_first_run_appserver.jsonl",
			rollout:   "killed_turn_rollout.jsonl",
			want:      []domain.TokenUsage{{InputTokens: 26956, CacheReadTokens: 20480, OutputTokens: 40, TotalTokens: 26996}},
		},
		{
			name:      "resumed thread, completed then interrupted turn",
			appserver: "resumed_thread_interrupted_turn_appserver.jsonl",
			boundary:  "relocated_home_two_turns_rollout.jsonl",
			rollout:   "resumed_thread_interrupted_turn_rollout.jsonl",
			want: []domain.TokenUsage{
				{InputTokens: 14629, CacheReadTokens: 14464, OutputTokens: 5, TotalTokens: 14634},
				{InputTokens: 14656, CacheReadTokens: 14464, OutputTokens: 56, TotalTokens: 14712},
			},
			lastFromRecords: true,
		},
		{
			name:      "resumed after a request killed mid-turn",
			appserver: "resumed_after_killed_turn_appserver.jsonl",
			boundary:  "killed_turn_rollout.jsonl",
			rollout:   "resumed_after_killed_turn_rollout.jsonl",
			want:      []domain.TokenUsage{{InputTokens: 14577, CacheReadTokens: 14336, OutputTokens: 5, TotalTokens: 14582}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var boundary [][]byte
			if tt.boundary != "" {
				boundary = rolloutCapture(t, tt.boundary)
			}

			got := rolloutSettle(t, rolloutCapture(t, tt.appserver), boundary, rolloutCapture(t, tt.rollout))

			if len(got) != len(tt.want) {
				t.Fatalf("settled turns = %d, want %d", len(got), len(tt.want))
			}
			var runTotal int64
			for i, want := range tt.want {
				fromRecords := tt.lastFromRecords && i == len(tt.want)-1
				runTotal += want.TotalTokens
				if got[i].Counts != want || got[i].Complete == fromRecords || (got[i].Provenance.Source == usage.SourceJournal) != fromRecords {
					t.Errorf("turn %d settled %+v complete %v source %q, want %+v from records %v",
						i, got[i].Counts, got[i].Complete, got[i].Provenance.Source, want, fromRecords)
				}
				if got[i].Run.TotalTokens != runTotal {
					t.Errorf("turn %d run TotalTokens = %d, want %d", i, got[i].Run.TotalTokens, runTotal)
				}
			}
		})
	}
}

func TestRollout_TurnKilledDuringToolCallSettlesFromRecords(t *testing.T) {
	appserver := rolloutCapture(t, "killed_turn_appserver.jsonl")
	segments := rolloutSegments(rolloutCapture(t, "killed_turn_rollout.jsonl"))
	resumed, ledger := rolloutStart(t, appserver, slices.Concat(segments[0], segments[1]), slices.Concat(segments...))
	messages := rolloutMessages(appserver)
	var response string
	for _, msg := range messages {
		if bytes.Contains(msg.Result, []byte(`"turn"`)) {
			response = fmt.Sprintf(`{"id":1,"result":%s}`, msg.Result)
		}
	}
	state := gatedTurnStartState(t, response)
	state.rollout, state.ledger = resumed.rollout, ledger
	for _, msg := range messages {
		state.inbox.Put(msg)
	}
	state.inbox.Close()

	var events []domain.AgentEvent
	done := make(chan domain.TurnResult, 1)
	go func() {
		adapter, _ := NewCodexAdapter()
		result, _ := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
		})
		done <- result
	}()
	got := waittest.Receive(t, done, "RunTurn outcome")

	wantUsage := domain.TokenUsage{InputTokens: 14503, OutputTokens: 52, CacheReadTokens: 13312, TotalTokens: 14555}
	wantProvenance := domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords}
	if got.Usage != wantUsage || got.UsageProvenance != wantProvenance || !got.SpendUnaccounted {
		t.Errorf("RunTurn() = usage %+v provenance %+v unaccounted %v, want %+v %+v true",
			got.Usage, got.UsageProvenance, got.SpendUnaccounted, wantUsage, wantProvenance)
	}
	if slices.ContainsFunc(events, func(e domain.AgentEvent) bool { return e.Type == domain.EventTokenUsage }) {
		t.Error("RunTurn emitted a token_usage event, want none")
	}
}

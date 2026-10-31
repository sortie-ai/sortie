package usagesource

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const toolTurnSessionID = "df64f30d-3446-4c7c-924e-9b58c79cdbc7"
const toolTurnJournal = "gemini-journal-tool-turn.jsonl"

var (
	liveProvenance    = domain.UsageProvenance{Source: usage.SourceProtocol, Strategy: usage.StrategyLive}
	recordsProvenance = domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords}
)

func journalRuntime(sessionID string, files ...string) geminiRuntime {
	rt := geminiRuntime{env: map[string]string{geminiHomeVar: "testdata"}, sessionID: sessionID, matched: new(agentcore.RuntimePath)}
	for _, name := range files {
		rt.matches = append(rt.matches, agentcore.RuntimePath{HomeVar: geminiHomeVar, Segments: []string{name}})
	}
	return rt
}

func flashLiteSettled(in, out, cacheRead int64, complete bool, source domain.UsageProvenance) usage.Settlement {
	run := domain.TokenUsage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, CacheReadTokens: cacheRead}
	return usage.Settlement{Run: run, ByModel: map[string]domain.TokenUsage{"gemini-3.5-flash-lite": run}, Figure: true, Complete: complete, Provenance: []domain.UsageProvenance{source}}
}

func TestGeminiDeclarationSettlesTheCapturedTurns(t *testing.T) {
	t.Parallel()

	type turn struct {
		result  string
		end     usage.TurnEnd
		visible int
		want    usage.Settlement
	}
	replied := usage.TurnEnd{Replied: true, ModelReached: true}
	tests := []struct {
		name    string
		journal string
		history int
		turns   []turn
	}{
		{"a tool turn, then a turn with no usage", toolTurnJournal, -1, []turn{
			{"gemini-prompt-result-tool-turn.json", replied, 3, flashLiteSettled(23681, 445, 8123, true, liveProvenance)},
			{"", replied, 4, flashLiteSettled(35774, 482, 16241, true, recordsProvenance)},
		}},
		{"a continued session counts only the new turn", "gemini-journal-continued.jsonl", 1, []turn{
			{"gemini-prompt-result-continued.json", replied, 2, flashLiteSettled(11576, 347, 0, true, liveProvenance)},
		}},
		{"a kill after a tool call keeps the finished request", "gemini-journal-killed-after-tool.jsonl", -1, []turn{
			{"", usage.TurnEnd{ModelReached: true}, 2, flashLiteSettled(11603, 447, 0, false, recordsProvenance)},
		}},
		{"a cancel before any request finished has no figure", "gemini-journal-cancelled.jsonl", -1, []turn{
			{"", usage.TurnEnd{Replied: true, Cancelled: true, ModelReached: true}, 0, usage.Settlement{}},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declaration := mustDeclaration(t, geminiAgentName).Effective()
			ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
			if err != nil {
				t.Fatalf("NewLedger(%q) = %v, want a ledger", geminiAgentName, err)
			}
			var readings []usage.SpendReading
			for line := range strings.Lines(readFixture(t, tt.journal)) {
				if reading, ok := geminiJournalReading([]byte(strings.TrimSpace(line))); ok {
					readings = append(readings, reading)
				}
			}
			if tt.history < 0 {
				ledger.SessionCreated()
			} else {
				ledger.Boundary(usage.RecordsRead{Readings: readings[:tt.history], OK: true})
			}

			for i, turn := range tt.turns {
				var live *usage.SpendReading
				if turn.result != "" {
					live = declaration.PromptSpend([]byte(readFixture(t, turn.result)))
				}
				if !ledger.NeedsRecords(turn.end, live) {
					t.Errorf("%s turn %d NeedsRecords() = false, want true: only the journal names the served model", tt.journal, i+1)
				}
				got := ledger.Settle(turn.end, live, usage.RecordsRead{Readings: readings[:turn.visible], OK: true})

				got.Model, got.Spent, got.CoversSession, got.Estimated = "", false, false, false
				if !reflect.DeepEqual(got, turn.want) {
					t.Errorf("%s turn %d Settle() = %+v, want %+v", tt.journal, i+1, got, turn.want)
				}
			}
		})
	}
}

func TestGeminiUnrecordedRequestsMarkAnAutoOrUnnamedModel(t *testing.T) {
	t.Parallel()

	named := readFixture(t, "gemini-session-new-model.json")
	tests := map[string]struct {
		session string
		want    bool
	}{
		"an auto selector":          {readFixture(t, "gemini-session-new-auto.json"), true},
		"an auto-prefixed selector": {strings.Replace(named, `"gemini-3.1-flash-lite"`, `"auto-gemini-3"`, 1), true},
		"a result naming no models": {`{"sessionId":"s"}`, true},
		"a named model":             {named, false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := geminiUnrecordedRequests([]byte(tt.session)); got != tt.want {
				t.Errorf("geminiUnrecordedRequests(%s) = %v, want %v", name, got, tt.want)
			}
		})
	}
}

func TestGeminiJournalReading(t *testing.T) {
	t.Parallel()

	withTool := *counts(15, 2, 3, 4, 0)
	withTool.Identity, withTool.Model = "req-1", "m"
	tests := []struct {
		name   string
		line   string
		want   usage.SpendReading
		wantOK bool
	}{
		{"tool tokens count as input", `{"type":"gemini","id":"req-1","model":"m","tokens":{"input":10,"output":2,"cached":4,"thoughts":3,"tool":5,"total":99}}`, withTool, true},
		{"a torn line is an unknown reading", `{"type":"gemini","id":"req-1","tok`, usage.SpendReading{}, true},
		{"a request still streaming has no tokens", `{"type":"gemini","id":"req-1","model":"m"}`, usage.SpendReading{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := geminiJournalReading([]byte(tt.line))

			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("geminiJournalReading(%s) = %+v, %v, want %+v, %v", tt.line, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestGeminiReadRecords(t *testing.T) {
	t.Parallel()

	errMatch := errors.New("match failed")
	toolTurnIDs := []string{"1e12452c-2909-414c-90eb-bca689001a2b", "1e12452c-2909-414c-90eb-bca689001a2b", "3ac6b4e8-120a-4a9b-bab4-43e827d50b13", "79399540-374c-4946-af3e-500538482b34"}
	tests := []struct {
		name      string
		sessionID string
		files     []string
		matchErr  error
		wantIDs   []string
		wantErr   bool
		wantMatch bool
	}{
		{"the process-start file adds nothing and repeated ids stay", toolTurnSessionID, []string{"gemini-journal-startup.jsonl", toolTurnJournal}, nil, toolTurnIDs, false, true},
		{"a later patch does not remove an earlier turn", "a4d27195-6a52-42bf-ace6-bbdaa513b721", []string{"gemini-journal-compressed.jsonl"}, nil,
			[]string{"1a61daca-1b08-489a-8109-143f623b3115", "cf728d83-c129-4b20-9678-c6eecc70fd1e"}, false, true},
		{"no file matches", toolTurnSessionID, nil, nil, nil, false, true},
		{"a vanished file does not hide the next one", toolTurnSessionID, []string{"gone.jsonl", toolTurnJournal}, nil, toolTurnIDs, false, true},
		{"a header naming a session with the same id prefix", "df64f30d-0000-0000-0000-000000000000", []string{toolTurnJournal}, nil, nil, false, true},
		{"the match fails", toolTurnSessionID, []string{toolTurnJournal}, errMatch, nil, true, true},
		{"the scan fails", toolTurnSessionID, []string{"."}, nil, nil, true, true},
		{"an id holding a wildcard", "ab*defgh-1234", []string{toolTurnJournal}, nil, nil, true, false},
		{"an id holding a separator", "abcdefg/hij", []string{toolTurnJournal}, nil, nil, true, false},
		{"an id shorter than the file name prefix", "abc", []string{toolTurnJournal}, nil, nil, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := journalRuntime(tt.sessionID, tt.files...)
			rt.matchErr = tt.matchErr

			records, err := geminiReadRecords(t.Context(), rt)

			if (err != nil) != tt.wantErr || (tt.matchErr != nil && !errors.Is(err, tt.matchErr)) {
				t.Errorf("geminiReadRecords(%q) error = %v, want error %v", tt.sessionID, err, tt.wantErr)
			}
			var got []string
			for _, reading := range records.Spend {
				got = append(got, reading.Identity)
			}
			if !slices.Equal(got, tt.wantIDs) {
				t.Errorf("geminiReadRecords(%q) identities = %q, want %q", tt.sessionID, got, tt.wantIDs)
			}
			wantPattern := agentcore.RuntimePath{HomeVar: "GEMINI_CLI_HOME", Segments: []string{".gemini", "tmp", "*", "chats", "session-*-" + tt.sessionID[:min(8, len(tt.sessionID))] + ".jsonl"}}
			if matched := rt.matched.Segments != nil; matched != tt.wantMatch || (matched && !reflect.DeepEqual(*rt.matched, wantPattern)) {
				t.Errorf("geminiReadRecords(%q) matched %+v, want a match = %v of %+v", tt.sessionID, *rt.matched, tt.wantMatch, wantPattern)
			}
		})
	}
}

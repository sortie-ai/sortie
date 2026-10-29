package usagesource

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

func spendJSON(t *testing.T, reading *usage.SpendReading) string {
	t.Helper()

	data, err := json.Marshal(reading)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(data)
}

func mustCopilotDeclaration(t *testing.T) Declaration {
	t.Helper()

	declaration, err := Declarations.Get(copilotHandshakeName)
	if err != nil {
		t.Fatalf("Declarations.Get(%q): %v", copilotHandshakeName, err)
	}
	return declaration
}

func TestStandardPromptSpend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result string
		want   *usage.SpendReading
	}{
		{
			name:   "first turn of a process",
			result: `{"stopReason":"end_turn","usage":{"inputTokens":40352,"outputTokens":4,"totalTokens":40356,"thoughtTokens":0,"cachedReadTokens":0,"cachedWriteTokens":40348}}`,
			want:   &usage.SpendReading{Input: new(int64(40352)), Output: new(int64(4)), CacheRead: new(int64(0)), CacheWrite: new(int64(40348))},
		},
		{
			name:   "thought and total tokens are not read",
			result: `{"stopReason":"end_turn","usage":{"inputTokens":58233,"outputTokens":80,"totalTokens":58313,"thoughtTokens":67,"cachedReadTokens":29084,"cachedWriteTokens":29129}}`,
			want:   &usage.SpendReading{Input: new(int64(58233)), Output: new(int64(80)), CacheRead: new(int64(29084)), CacheWrite: new(int64(29129))},
		},
		{name: "usage null", result: `{"stopReason":"end_turn","usage":null}`},
		{name: "usage absent", result: `{"stopReason":"end_turn"}`},
		{name: "result is not an object", result: `[]`},
		{
			name:   "negative member kept for the ledger to refuse",
			result: `{"usage":{"inputTokens":10,"outputTokens":-4,"cachedReadTokens":0,"cachedWriteTokens":0}}`,
			want:   &usage.SpendReading{Input: new(int64(10)), Output: new(int64(-4)), CacheRead: new(int64(0)), CacheWrite: new(int64(0))},
		},
		{
			name:   "absent member is an unknown count, not zero",
			result: `{"usage":{"inputTokens":10,"cachedReadTokens":0,"cachedWriteTokens":0}}`,
			want:   &usage.SpendReading{Input: new(int64(10)), CacheRead: new(int64(0)), CacheWrite: new(int64(0))},
		},
		{
			name:   "null member is an unknown count, not zero",
			result: `{"usage":{"inputTokens":10,"outputTokens":null,"cachedReadTokens":0,"cachedWriteTokens":0}}`,
			want:   &usage.SpendReading{Input: new(int64(10)), CacheRead: new(int64(0)), CacheWrite: new(int64(0))},
		},
		{
			name:   "fractional and string members are unknown counts",
			result: `{"usage":{"inputTokens":10.5,"outputTokens":"4","cachedReadTokens":0,"cachedWriteTokens":0}}`,
			want:   &usage.SpendReading{CacheRead: new(int64(0)), CacheWrite: new(int64(0))},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := standardPromptSpend([]byte(tt.result))

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("standardPromptSpend(%s) = %s, want %s", tt.result, spendJSON(t, got), spendJSON(t, tt.want))
			}
		})
	}
}

func TestCopilotDeclarationAutoRecords(t *testing.T) {
	t.Parallel()

	declaration := mustCopilotDeclaration(t)
	var records []usage.SpendReading
	for line := range strings.Lines(readGeminiFixture(t, "auto-records.jsonl")) {
		reading, ok := sessionstate.UsageRecord([]byte(line))
		if !ok {
			t.Fatalf("UsageRecord(%.60q) = not a record, want a record", line)
		}
		records = append(records, reading)
	}
	if len(records) != 2 {
		t.Fatalf("auto-records.jsonl holds %d records, want 2", len(records))
	}

	live := declaration.PromptSpend([]byte(`{"stopReason":"end_turn","usage":{"inputTokens":48222,"outputTokens":10,"totalTokens":48232,"thoughtTokens":0,"cachedReadTokens":24085,"cachedWriteTokens":24131}}`))
	if live == nil {
		t.Fatal("PromptSpend(auto turn) = nil, want a reading")
	}
	ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	ledger.Boundary(usage.RecordsRead{OK: true})

	got := ledger.Settle(usage.TurnEnd{Replied: true, ModelReached: true}, live, usage.RecordsRead{Readings: records, OK: true})

	want := domain.TokenUsage{InputTokens: 48222, OutputTokens: 10, TotalTokens: 48232, CacheReadTokens: 24085, CacheWriteTokens: 24131}
	if got.Run != want {
		t.Errorf("Settle().Run = %+v, want %+v", got.Run, want)
	}
	if !got.CoversSession {
		t.Errorf("Settle().CoversSession = false, want true: the live figure settles the turn")
	}
	if byModel := got.ByModel["gpt-6.1-terra"]; len(got.ByModel) != 1 || byModel != want {
		t.Errorf("Settle().ByModel = %+v, want only gpt-6.1-terra at %+v", got.ByModel, want)
	}
}

func TestCopilotReadRecordsRefusesUnsafeSessionID(t *testing.T) {
	t.Parallel()

	readRecords := mustCopilotDeclaration(t).ReadRecords
	for _, id := range []string{"../escape", "a/b", `a\b`, "", ".", ".."} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()

			var scans int

			readings, err := readRecords(t.Context(), geminiRuntime{sessionID: id, scans: &scans})

			if err == nil {
				t.Errorf("ReadRecords(session %q) error = nil, want an error", id)
			}
			if len(readings) != 0 || scans != 0 {
				t.Errorf("ReadRecords(session %q) = %d readings after %d scans, want none", id, len(readings), scans)
			}
		})
	}
}

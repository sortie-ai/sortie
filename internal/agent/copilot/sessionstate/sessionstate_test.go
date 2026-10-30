package sessionstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

func fixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

func TestUsageRecord(t *testing.T) {
	t.Parallel()

	journal := fixtureLines(t, "turn-order.jsonl")

	tests := []struct {
		name        string
		fixtureLine int
		line        string
		want        usage.SpendReading
		wantOK      bool
	}{
		{
			name:        "first request of a turn",
			fixtureLine: 4,
			want: usage.SpendReading{
				Input:      new(int64(40352)),
				Output:     new(int64(4)),
				CacheRead:  new(int64(0)),
				CacheWrite: new(int64(40348)),
				Model:      "claude-sonnet-5.5",
				Identity:   "msg_011StandInRequestAAAAA01",
			},
			wantOK: true,
		},
		{
			name:        "request of a later turn",
			fixtureLine: 10,
			want: usage.SpendReading{
				Input:      new(int64(40405)),
				Output:     new(int64(4)),
				CacheRead:  new(int64(40348)),
				CacheWrite: new(int64(53)),
				Model:      "claude-sonnet-5.5",
				Identity:   "msg_011StandInRequestAAAAA02",
			},
			wantOK: true,
		},
		{name: "user message", fixtureLine: 1},
		{name: "assistant message", fixtureLine: 5},
		{name: "usage checkpoint", fixtureLine: 7},
		{name: "shutdown", fixtureLine: 14},
		{
			name: "checkpoint carrying a usage member",
			line: `{"type":"session.usage_checkpoint","data":{"usage":{"model":"m","inputTokens":9,"outputTokens":9,"cacheReadTokens":0,"cacheWriteTokens":0,"accounting":{"usageId":"x"}}}}`,
		},
		{name: "not JSON", line: `session.usage_record`},
		{name: "truncated JSON", line: `{"type":"session.usage_record","data":{"usage":{"inputTok`},
		{name: "empty line", line: ``},
		{
			name: "absent count is nil",
			line: `{"type":"session.usage_record","data":{"usage":{"model":"m","inputTokens":10,"outputTokens":2,"cacheReadTokens":0,"accounting":{"usageId":"x"}}}}`,
			want: usage.SpendReading{
				Input:     new(int64(10)),
				Output:    new(int64(2)),
				CacheRead: new(int64(0)),
				Model:     "m",
				Identity:  "x",
			},
			wantOK: true,
		},
		{
			name: "null, fractional, and string counts are nil",
			line: `{"type":"session.usage_record","data":{"usage":{"model":"m","inputTokens":null,"outputTokens":2.5,"cacheReadTokens":"7","cacheWriteTokens":3,"accounting":{"usageId":"x"}}}}`,
			want: usage.SpendReading{
				CacheWrite: new(int64(3)),
				Model:      "m",
				Identity:   "x",
			},
			wantOK: true,
		},
		{
			name: "model passes through verbatim",
			line: `{"type":"session.usage_record","data":{"usage":{"model":"Claude-Sonnet-5.5 ","inputTokens":1,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0,"accounting":{"usageId":"x"}}}}`,
			want: usage.SpendReading{
				Input:      new(int64(1)),
				Output:     new(int64(1)),
				CacheRead:  new(int64(0)),
				CacheWrite: new(int64(0)),
				Model:      "Claude-Sonnet-5.5 ",
				Identity:   "x",
			},
			wantOK: true,
		},
		{
			name: "record without accounting has no identity",
			line: `{"type":"session.usage_record","data":{"usage":{"model":"m","inputTokens":1,"outputTokens":1,"cacheReadTokens":0,"cacheWriteTokens":0}}}`,
			want: usage.SpendReading{
				Input:      new(int64(1)),
				Output:     new(int64(1)),
				CacheRead:  new(int64(0)),
				CacheWrite: new(int64(0)),
				Model:      "m",
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			line := []byte(tt.line)
			if tt.fixtureLine > 0 {
				line = journal[tt.fixtureLine-1]
			}

			got, ok := UsageRecord(line)

			if ok != tt.wantOK {
				t.Fatalf("UsageRecord(%q) ok = %v, want %v", line, ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UsageRecord(%q) = %+v, want %+v", line, got, tt.want)
			}
		})
	}
}

func checkpointWith(models string) string {
	return `{"type":"session.usage_checkpoint","data":{"totalNanoAiu":1.5,"accountingSnapshot":{"modelMetrics":{` + models + `}}}}`
}

func TestUsageCheckpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		line   string
		want   usage.SpendReading
		wantOK bool
	}{
		{"not a checkpoint", `{"type":"session.usage_record"}`, usage.SpendReading{}, false},
		{"not JSON", `session.usage_checkpoint`, usage.SpendReading{}, false},
		{"two models sorted by key, float members ignored",
			checkpointWith(`"b":{"totalNanoAiu":6.5,"usage":{"inputTokens":7,"cacheWriteTokens":0}},"a":{"requests":{"cost":1.0},"usage":{"inputTokens":4,"outputTokens":3,"cacheReadTokens":2,"cacheWriteTokens":1}}`),
			usage.SpendReading{ByModel: []usage.SpendReading{
				{Input: new(int64(4)), Output: new(int64(3)), CacheRead: new(int64(2)), CacheWrite: new(int64(1)), Model: "a"},
				{Input: new(int64(7)), CacheWrite: new(int64(0)), Model: "b"},
			}}, true},
		{"null and float counts are nil", checkpointWith(`"m":{"usage":{"outputTokens":null,"cacheReadTokens":1.0,"cacheWriteTokens":3}}`),
			usage.SpendReading{ByModel: []usage.SpendReading{{CacheWrite: new(int64(3)), Model: "m"}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := UsageCheckpoint([]byte(tt.line))

			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UsageCheckpoint(%q) = %+v, %v, want %+v, %v", tt.line, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestScanRecords(t *testing.T) {
	t.Parallel()

	errScan := errors.New("scan failed")
	lines := []string{
		`{"type":"session.usage_record","data":{"usage":{"model":"b","inputTokens":2,"accounting":{"usageId":"u2"}}}}`,
		`{"type":"session.usage_checkpoint"}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"a","inputTokens":1,"accounting":{"usageId":"u1"}}}}`,
	}
	var gotPath agentcore.RuntimePath
	var gotLimit, gotLineLimit int64
	scan := func(_ context.Context, path agentcore.RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error {
		gotPath, gotLimit, gotLineLimit = path, limit, lineLimit
		for _, line := range lines {
			if err := fn([]byte(line)); err != nil {
				return err
			}
		}
		return errScan
	}

	got, err := ScanRecords(t.Context(), "sess-1", scan)

	if !reflect.DeepEqual(gotPath, JournalPath("sess-1")) || gotLimit != MaxFileBytes || gotLineLimit != usage.MaxRecordLineBytes {
		t.Errorf("ScanRecords scanned (%+v, %d, %d), want (%+v, %d, %d)", gotPath, gotLimit, gotLineLimit, JournalPath("sess-1"), int64(MaxFileBytes), int64(usage.MaxRecordLineBytes))
	}
	want := []usage.SpendReading{{Input: new(int64(2)), Model: "b", Identity: "u2"}, {Input: new(int64(1)), Model: "a", Identity: "u1"}}
	if !errors.Is(err, errScan) || !reflect.DeepEqual(got, want) {
		t.Errorf("ScanRecords = %+v, %v, want %+v, %v", got, err, want, errScan)
	}
}

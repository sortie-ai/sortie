package sessionstate

import (
	"bytes"
	"os"
	"reflect"
	"testing"

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

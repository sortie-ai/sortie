package sessionexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func exportDoc(messages ...string) []byte {
	return []byte(`{"info":{"id":"ses_t"},"messages":[` + strings.Join(messages, ",") + `]}`)
}

func mustParse(t *testing.T, data []byte, sessionID string) []Record {
	t.Helper()
	records, err := Parse(data, sessionID)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return records
}

func TestParseCaptures(t *testing.T) {
	t.Parallel()

	type counts [4]int64
	tests := []struct {
		fixture   string
		sessionID string
		want      []counts
	}{
		{"two-turns-export.json", "ses_e7ac6691effe73waoQYzNihPM6", []counts{{15681, 2, 0, 1536}, {827, 2, 0, 17152}}},
		{"flash-latest-export.json", "ses_e7ac4b348ffeP2wfjLJwh4m3tB", []counts{{17283, 1, 39, 0}, {2924, 17, 52, 15232}, {2221, 14, 62, 16108}}},
		{"child-cancel-parent-export.json", "ses_e79773d5cffeGZOgs16v5Zm70A", []counts{{102, 106, 79, 17152}}},
		{"child-cancel-child-export.json", "ses_e79773346ffeKVGj032T0Q7h9p", nil},
		{"cancel-export.json", "ses_e7ac4d139ffeivndL710DYWtze", nil},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()

			var got []counts
			for _, r := range mustParse(t, loadFixture(t, tt.fixture), tt.sessionID) {
				got = append(got, counts{*r.Spend.Input, *r.Spend.Output, *r.Spend.Reasoning, *r.Spend.CacheRead})
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("Parse(%s) counts = %v, want %v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestParseSkipsMessagesThatAreNotFinishedAssistantsWithTokens(t *testing.T) {
	t.Parallel()

	const tokens = `"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}`
	tests := []struct {
		name    string
		message string
	}{
		{"unfinished assistant", `{"type":"assistant",` + tokens + `}`},
		{"user message", `{"type":"user","finish":"stop",` + tokens + `}`},
		{"tokens null", `{"type":"assistant","finish":"error","tokens":null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			records := mustParse(t, exportDoc(tt.message), "ses_t")

			if len(records) != 0 {
				t.Errorf("Parse(%s) = %d records, want 0", tt.message, len(records))
			}
		})
	}
}

func TestParseMapsMembers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    Record
	}{
		{
			"every member",
			`{"id":"msg_1","type":"assistant","finish":"stop",` +
				`"model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"time":{"created":1793236506454},` +
				`"tokens":{"input":3000,"output":500,"reasoning":70,"cache":{"read":400,"write":900}}}`,
			Record{
				Spend: usage.SpendReading{
					Input: new(int64(3000)), Output: new(int64(500)), Reasoning: new(int64(70)), CacheRead: new(int64(400)), CacheWrite: new(int64(900)),
					Model: "anthropic/claude-sonnet-4-5", Identity: "msg_1",
				},
				CreatedMS: 1793236506454,
			},
		},
		{
			"unusable members",
			`{"id":"msg_1","type":"assistant","finish":"stop",` +
				`"model":{"providerID":"anthropic"},"time":{"created":1.5},` +
				`"tokens":{"input":null,"output":1.5,"reasoning":1e2}}`,
			Record{Spend: usage.SpendReading{Identity: "msg_1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mustParse(t, exportDoc(tt.message), "ses_t")

			if want := []Record{tt.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("Parse(%s) = %+v, want %+v", tt.message, got, want)
			}
		})
	}
}

func TestParseLeavesTheSessionAggregateOut(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "two-turns-export.json")
	var doc struct {
		Info struct {
			Tokens struct {
				Input int64 `json:"input"`
			} `json:"tokens"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding aggregate: %v", err)
	}

	var sum int64
	for _, r := range mustParse(t, data, "ses_e7ac6691effe73waoQYzNihPM6") {
		sum += *r.Spend.Input
	}

	if sum >= doc.Info.Tokens.Input {
		t.Errorf("Parse(two-turns) input sum = %d, want below the aggregate %d", sum, doc.Info.Tokens.Input)
	}
}

func TestParseRefusesAnotherSession(t *testing.T) {
	t.Parallel()

	records, err := Parse(loadFixture(t, "two-turns-export.json"), "ses_other")

	if !errors.Is(err, ErrOtherSession) || len(records) != 0 {
		t.Errorf("Parse(two-turns, other session) = %d records, %v, want none and ErrOtherSession", len(records), err)
	}
}

func TestParseRefusesUnreadableData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{"not json", []byte("not valid json")},
		{"above usage.MaxRecordsBytes", append(exportDoc(), bytes.Repeat([]byte(" "), usage.MaxRecordsBytes)...)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.data, "ses_t")

			if err == nil || errors.Is(err, ErrOtherSession) {
				t.Errorf("Parse(%s) error = %v, want a refusal that is not ErrOtherSession", tt.name, err)
			}
		})
	}
}

func TestArgs(t *testing.T) {
	t.Parallel()

	got := Args("ses_1")

	if want := []string{"session", "export", "--standalone", "--sanitize", "ses_1"}; !slices.Equal(got, want) {
		t.Errorf("Args(%q) = %v, want %v", "ses_1", got, want)
	}
}

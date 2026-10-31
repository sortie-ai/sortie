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

func mustParse(t *testing.T, data []byte, sessionID string) []usage.SpendReading {
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
		wantCosts []float64
	}{
		{"two-turns-export.json", "ses_e7ac6691effe73waoQYzNihPM6", []counts{{15681, 2, 0, 1536}, {827, 2, 0, 17152}}, []float64{0, 0}},
		{"flash-latest-export.json", "ses_e7ac4b348ffeP2wfjLJwh4m3tB", []counts{{17283, 1, 39, 0}, {2924, 17, 52, 15232}, {2221, 14, 62, 16108}}, []float64{0.01311225, 0.0035941499999999995, 0.00315885}},
		{"child-cancel-parent-export.json", "ses_e79773d5cffeGZOgs16v5Zm70A", []counts{{102, 106, 79, 17152}}, []float64{0}},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()

			var got []counts
			var gotCosts []float64
			for _, r := range mustParse(t, loadFixture(t, tt.fixture), tt.sessionID) {
				got = append(got, counts{*r.Input, *r.Output, *r.Reasoning, *r.CacheRead})
				gotCosts = append(gotCosts, *r.Amounts[0].Amount)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("Parse(%s) counts = %v, want %v", tt.fixture, got, tt.want)
			}
			if !slices.Equal(gotCosts, tt.wantCosts) {
				t.Errorf("Parse(%s) costs = %v, want %v", tt.fixture, gotCosts, tt.wantCosts)
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
		want    usage.SpendReading
	}{
		{
			"every member",
			`{"id":"msg_1","type":"assistant","finish":"stop","cost":1.0,` +
				`"model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},` +
				`"tokens":{"input":3000,"output":500,"reasoning":70,"cache":{"read":400,"write":900}}}`,
			usage.SpendReading{
				Input: new(int64(3000)), Output: new(int64(500)), Reasoning: new(int64(70)), CacheRead: new(int64(400)), CacheWrite: new(int64(900)),
				Model: "anthropic/claude-sonnet-4-5", Identity: "msg_1", Amounts: []usage.AmountReading{{Amount: new(1.0)}},
			},
		},
		{
			"model without an id",
			`{"id":"msg_1","type":"assistant","finish":"stop","model":{"providerID":"anthropic"}}`,
			usage.SpendReading{Identity: "msg_1", Amounts: []usage.AmountReading{{}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mustParse(t, exportDoc(tt.message), "ses_t")

			if want := []usage.SpendReading{tt.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("Parse(%s) = %+v, want %+v", tt.message, got, want)
			}
		})
	}
}

func TestParseReadsAnUnusableCostAsUnknown(t *testing.T) {
	t.Parallel()

	for _, cost := range []string{`null`, `"0.5"`, `-1`} {
		records := mustParse(t, exportDoc(`{"id":"m","type":"assistant","finish":"stop","cost":`+cost+`}`), "ses_t")

		if len(records) != 1 || records[0].Amounts[0].Amount != nil {
			t.Errorf("Parse(cost %s) = %+v, want one reading with an unknown amount", cost, records)
		}
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
		sum += *r.Input
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

func TestParseKeepsAFinishedMessageWithoutTokensUncounted(t *testing.T) {
	t.Parallel()

	records := mustParse(t, exportDoc(
		`{"id":"msg_counted","type":"assistant","finish":"stop","tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		`{"id":"msg_aborted","type":"assistant","finish":"error","tokens":null}`), "ses_t")

	if len(records) != 2 {
		t.Fatalf("Parse() = %d records, want 2", len(records))
	}
	aborted := records[1]
	if aborted.Identity != "msg_aborted" {
		t.Errorf("Parse() aborted reading identity = %q, want msg_aborted", aborted.Identity)
	}
	if _, ok := usage.Normalize(aborted, usage.SpendDeclaration{}); ok {
		t.Errorf("Normalize(aborted record) ok = true, want its spend unknown")
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want usage.SpendReading
	}{
		{
			"every member",
			`{"input":1,"output":2,"reasoning":3,"cache":{"read":4,"write":5}}`,
			usage.SpendReading{Input: new(int64(1)), Output: new(int64(2)), Reasoning: new(int64(3)), CacheRead: new(int64(4)), CacheWrite: new(int64(5))},
		},
		{"absent members", `{"input":1,"cache":{"write":5}}`, usage.SpendReading{Input: new(int64(1)), CacheWrite: new(int64(5))}},
		{"null members", `{"input":null,"output":null,"reasoning":null,"cache":{"read":null,"write":null}}`, usage.SpendReading{}},
		{"fractional members", `{"input":1.5,"output":2.5,"reasoning":3.5,"cache":{"read":4.5,"write":5.5}}`, usage.SpendReading{}},
		{"non-number members", `{"input":"1","output":true,"reasoning":[],"cache":{"read":{},"write":"5"}}`, usage.SpendReading{}},
		{"cache that is not an object", `{"input":1,"cache":5}`, usage.SpendReading{Input: new(int64(1))}},
		{"null", `null`, usage.SpendReading{}},
		{"array", `[1,2]`, usage.SpendReading{}},
		{"number", `5`, usage.SpendReading{}},
		{"string", `"tokens"`, usage.SpendReading{}},
		{"absent", ``, usage.SpendReading{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Tokens(json.RawMessage(tt.raw))

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Tokens(%s) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
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

func withMessages(data []byte, messages ...string) []byte {
	end := bytes.LastIndexByte(data, ']')
	return slices.Concat(data[:end], []byte(","+strings.Join(messages, ",")), data[end:])
}

func TestLastError(t *testing.T) {
	t.Parallel()

	const (
		captures     = "../../../clientprotocol/testdata/usage_limit/"
		freeSession  = "ses_e77a9b75fffeBqgRXMBQsUYvPv"
		quotaSession = "ses_e77a0ddb7ffe1dlxp5Oso7Eq8N"
	)
	free := loadFixture(t, captures+"opencode_free_limit_export.json")
	newTurn := []string{`{"type":"user"}`}
	tests := []struct {
		name      string
		data      []byte
		sessionID string
		want      SessionError
		wantFound bool
		wantErr   error
	}{
		{"the idle message after the assistant message wins", free, freeSession, SessionError{Type: "provider.free-limit", RetryAfterMS: new(int64(3600000)), FailedAtMS: 1793288718816}, true, nil},
		{"a refusal without a delay", loadFixture(t, captures+"opencode_quota_export.json"), quotaSession, SessionError{Type: "provider.quota", FailedAtMS: 1793289298588}, true, nil},
		{"a refusal before the last user message is ignored", withMessages(free, newTurn...), freeSession, SessionError{}, false, nil},
		{"a refusal after the last user message replaces the earlier one", withMessages(free, append(newTurn, `{"type":"assistant","error":{"type":"provider.quota"},"time":{"created":9}}`)...), freeSession, SessionError{Type: "provider.quota", FailedAtMS: 9}, true, nil},
		{"another session", free, "ses_other", SessionError{}, false, ErrOtherSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, found, err := LastError(tt.data, tt.sessionID)

			if !errors.Is(err, tt.wantErr) || found != tt.wantFound || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LastError(%s) = %+v, %v, %v, want %+v, %v, %v", tt.name, got, found, err, tt.want, tt.wantFound, tt.wantErr)
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

package usagesource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

func apiResponse(sessionID, model string, input, output, cached, thoughts, tool, total int64) string {
	record := map[string]any{
		"body": "API response from " + model,
		"attributes": map[string]any{
			"session.id":                 sessionID,
			"event.name":                 "gemini_cli.api_response",
			"model":                      model,
			"input_token_count":          input,
			"output_token_count":         output,
			"cached_content_token_count": cached,
			"thoughts_token_count":       thoughts,
			"tool_token_count":           tool,
			"total_token_count":          total,
			"prompt_id":                  "p1",
			"status_code":                200,
		},
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(encoded) + "\n"
}

// otherRepresentation builds a record of a shape that shares the outfile but is
// not the authoritative api_response.
func otherRepresentation(sessionID string) string {
	record := map[string]any{
		"attributes": map[string]any{
			"session.id":                 sessionID,
			"event.name":                 "gen_ai.client.inference.operation.details",
			"gen_ai.usage.input_tokens":  999999,
			"gen_ai.usage.output_tokens": 999999,
		},
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(encoded) + "\n"
}

func newArmedReader(t *testing.T, sessionID string) *geminiReader {
	t.Helper()

	dir := t.TempDir()

	reader := newGeminiReader()
	reader.dir = dir
	reader.home = filepath.Join(dir, "home")
	reader.outfile = filepath.Join(dir, "telemetry.json")
	reader.Open(sessionID)
	return reader
}

func writeOutfile(t *testing.T, reader *geminiReader, values ...string) {
	t.Helper()

	file, err := os.OpenFile(reader.outfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open outfile: %v", err)
	}
	defer file.Close() //nolint:errcheck // test handle
	if _, err := file.WriteString(strings.Join(values, "")); err != nil {
		t.Fatalf("write outfile: %v", err)
	}
}

func drain(t *testing.T, reader *geminiReader, bound int64) (agentcore.RecoveredUsage, string, bool) {
	t.Helper()
	return reader.Drain(context.Background(), bound)
}

func TestGeminiCounterMappingMatchesTheMeasuredSequence(t *testing.T) {
	t.Parallel()

	reader := newArmedReader(t, "sess-a")
	writeOutfile(t, reader,
		apiResponse("sess-a", "gemini-3.5-flash", 11835, 1, 0, 170, 0, 12006),
		apiResponse("sess-a", "gemini-3.5-flash", 11864, 20, 8118, 241, 0, 12125),
		apiResponse("sess-a", "gemini-3.5-flash", 12143, 4, 8113, 41, 0, 12188),
	)

	// The total 36319 lands only if reasoning goes into output and the cache
	// read goes nowhere but its own counter.
	recovered, source, found := drain(t, reader, 35867)
	if !found {
		t.Fatal("Drain() found = false, want a figure")
	}
	if source != geminiSourceTelemetry {
		t.Errorf("Drain() source = %q, want %q", source, geminiSourceTelemetry)
	}

	want := domain.TokenUsage{
		InputTokens:     35842,
		OutputTokens:    477,
		TotalTokens:     36319,
		CacheReadTokens: 16231,
	}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v", recovered.Run, want)
	}
	if recovered.Run.CacheReadTokens > recovered.Run.InputTokens {
		t.Error("the cache read exceeds input, so it was not folded in as the subset it is")
	}
	if recovered.Model != "gemini-3.5-flash" {
		t.Errorf("Drain() model = %q, want the model of record", recovered.Model)
	}
}

func TestGeminiDiscardsTheCountersWithNoHome(t *testing.T) {
	t.Parallel()

	reader := newArmedReader(t, "sess-a")
	writeOutfile(t, reader,
		apiResponse("sess-a", "model-x", 100, 10, 40, 5, 7777, 999999),
	)

	recovered, _, found := drain(t, reader, 1)
	if !found {
		t.Fatal("Drain() found = false, want a figure")
	}

	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 15, TotalTokens: 115, CacheReadTokens: 40}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v: the tool counter is discarded and the total recomputed",
			recovered.Run, want)
	}
}

func TestGeminiReadsOnlyTheAuthoritativeRepresentation(t *testing.T) {
	t.Parallel()

	reader := newArmedReader(t, "sess-z")
	writeOutfile(t, reader,
		otherRepresentation("sess-z"),
		apiResponse("sess-z", "model-x", 100, 10, 0, 0, 0, 110),
		otherRepresentation("sess-z"),
	)

	recovered, _, found := drain(t, reader, 1)
	if !found {
		t.Fatal("Drain() found = false, want a figure")
	}
	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v", recovered.Run, want)
	}
}

func TestGeminiCountsOnlyThisRunsRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	reader := newGeminiReader()
	reader.dir = dir
	reader.home = filepath.Join(dir, "home")
	reader.outfile = filepath.Join(dir, "telemetry.json")

	// On disk before the session opened: a runtime the transport resumed.
	if err := os.WriteFile(reader.outfile,
		[]byte(apiResponse("sess-a", "model-x", 500000, 500000, 0, 0, 0, 1000000)), 0o600); err != nil {
		t.Fatalf("seed outfile: %v", err)
	}

	reader.Open("sess-a")
	writeOutfile(t, reader,
		apiResponse("sess-b", "model-x", 777, 777, 0, 0, 0, 1554),
		apiResponse("sess-a", "model-x", 100, 10, 0, 0, 0, 110),
	)

	recovered, _, found := drain(t, reader, 1)
	if !found {
		t.Fatal("Drain() found = false, want a figure")
	}
	want := domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v: only this session's records, only after its own position",
			recovered.Run, want)
	}
}

func TestGeminiJournalBacksUpALostTelemetryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	home := filepath.Join(dir, "home")

	reader := newGeminiReader()
	reader.dir = dir
	reader.home = home
	reader.outfile = filepath.Join(dir, "telemetry.json")
	reader.now = steppingClock()
	reader.Open("sess-a")

	writeJournal(t, home, "session-2026-01-01T00-00-sess-a.jsonl", []string{
		`{"sessionId":"sess-a","startTime":"2026-01-01T00:00:00Z"}`,
		`{"id":"m1","type":"gemini","model":"gemini-3.5-flash","timestamp":"2026-01-01T00:00:01Z",` +
			`"tokens":{"input":11835,"output":2,"cached":8123,"thoughts":198,"tool":0,"total":12035}}`,
	})

	recovered, source, found := drain(t, reader, 1)
	if !found {
		t.Fatal("Drain() found = false, want the journal's figure")
	}
	if source != geminiSourceJournal {
		t.Errorf("Drain() source = %q, want %q", source, geminiSourceJournal)
	}
	want := domain.TokenUsage{InputTokens: 11835, OutputTokens: 200, TotalTokens: 12035, CacheReadTokens: 8123}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v", recovered.Run, want)
	}
	if recovered.Model != "gemini-3.5-flash" {
		t.Errorf("Drain() model = %q, want the model of record", recovered.Model)
	}
}

func TestGeminiJournalIgnoresAForeignSessionAndAResumedHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	home := filepath.Join(dir, "home")

	reader := newGeminiReader()
	reader.dir = dir
	reader.home = home
	reader.outfile = filepath.Join(dir, "telemetry.json")
	reader.now = steppingClock()

	writeJournal(t, home, "session-2026-01-01T00-00-sess-aaa.jsonl", []string{
		journalHeader("sess-aaaaaaaa-other-run"),
		journalMessage("x1", "2026-01-01T00:00:01Z", 999, 999, 0, 0),
	})
	writeJournal(t, home, "session-2026-01-01T00-01-sess-aaa.jsonl", []string{
		journalHeader("sess-aaaaaaaa-this-run"),
		journalMessage("old", "2025-12-31T23:00:00Z", 777, 777, 0, 0),
	})

	reader.Open("sess-aaaaaaaa-this-run")
	appendJournal(t, home, "session-2026-01-01T00-01-sess-aaa.jsonl", []string{
		journalMessage("m1", "2026-01-01T00:00:01Z", 100, 10, 0, 0),
		journalMessage("m1", "2026-01-01T00:00:02Z", 120, 12, 0, 0),
	})

	recovered, _, found := drain(t, reader, 1)
	if !found {
		t.Fatal("Drain() found = false, want the journal's figure")
	}
	want := domain.TokenUsage{InputTokens: 120, OutputTokens: 12, TotalTokens: 132}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v: this run's journal, this run's messages, last revision winning",
			recovered.Run, want)
	}
}

func TestGeminiRecognizesOnlyTheMeasuredBuilds(t *testing.T) {
	t.Parallel()

	reader := newGeminiReader()
	tests := []struct {
		name    string
		agent   string
		version string
		want    bool
	}{
		{name: "the measured build", agent: "gemini-cli", version: "0.59.0", want: true},
		{name: "an older build of the same runtime", agent: "gemini-cli", version: "0.58.0"},
		{name: "a newer build of the same runtime", agent: "gemini-cli", version: "0.60.0"},
		{name: "another runtime reporting a measured version", agent: "other-cli", version: "0.59.0"},
		{name: "a handshake that named nothing", agent: "", version: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reader.Recognize(tt.agent, tt.version); got != tt.want {
				t.Errorf("Recognize(%q, %q) = %v, want %v", tt.agent, tt.version, got, tt.want)
			}
		})
	}
}

func TestGeminiClaimRefusesARemoteLaunch(t *testing.T) {
	t.Parallel()

	reader := newGeminiReader()
	t.Cleanup(reader.Close)

	if _, claimed := reader.Claim(agentcore.LaunchTarget{Command: "ssh", RemoteCommand: "gemini", SSHHost: "worker"}); claimed {
		t.Error("Claim(remote) = true, want false")
	}

	env, claimed := reader.Claim(agentcore.LaunchTarget{Command: "gemini", WorkspacePath: "/w"})
	if !claimed {
		t.Fatal("Claim(local) = false, want true")
	}
	if len(env) != 3 {
		t.Errorf("Claim(local) env = %v, want the three assignments the source's own output needs", env)
	}
}

func TestGeminiCloseRemovesWhatClaimArmed(t *testing.T) {
	t.Parallel()

	reader := newGeminiReader()
	if _, claimed := reader.Claim(agentcore.LaunchTarget{Command: "gemini", WorkspacePath: "/w"}); !claimed {
		t.Fatal("Claim(local) = false, want true")
	}
	dir := reader.dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stat armed directory: %v", err)
	}

	reader.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("stat armed directory after Close() = %v, want it removed", err)
	}
}

// steppingClock advances an hour per reading, so a drain's deadline has already
// elapsed by its next read and a figure on disk is answered without a wait.
func steppingClock() func() time.Time {
	at := time.Date(2026, 1, 1, 0, 0, 0, 900_000_000, time.UTC)
	calls := 0
	return func() time.Time {
		calls++
		return at.Add(time.Duration(calls-1) * time.Hour)
	}
}

// slowClock advances one second per reading, so a drain runs its poll loop
// several times and sees a record appended while it polls.
func slowClock() func() time.Time {
	at := time.Date(2026, 1, 1, 0, 0, 0, 900_000_000, time.UTC)
	calls := 0
	return func() time.Time {
		calls++
		return at.Add(time.Duration(calls-1) * time.Second)
	}
}

func writeJournal(t *testing.T, home, name string, lines []string) {
	t.Helper()

	dir := filepath.Join(home, ".gemini", "tmp", "project", "chats")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("make journal directory: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
}

// apiResponseWithoutCounters builds an authoritative-shape record whose
// attribute block carries no usage counter.
func apiResponseWithoutCounters(sessionID, model string) string {
	record := map[string]any{
		"body": "API response from " + model,
		"attributes": map[string]any{
			"session.id":  sessionID,
			"event.name":  "gemini_cli.api_response",
			"model":       model,
			"prompt_id":   "p1",
			"status_code": 200,
		},
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(encoded) + "\n"
}

func journalMessage(id, stamp string, input, output, cached, thoughts int64) string {
	line := map[string]any{
		"id":     id,
		"type":   "gemini",
		"model":  "model-x",
		"tokens": map[string]any{"input": input, "output": output, "cached": cached, "thoughts": thoughts, "tool": 0, "total": input + output + thoughts},
	}
	if stamp != "" {
		line["timestamp"] = stamp
	}
	encoded, err := json.Marshal(line)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func journalHeader(sessionID string) string {
	return `{"sessionId":"` + sessionID + `","startTime":"2026-01-01T00:00:00Z"}`
}

// newSeededReader returns a reader whose home already holds prior as sess-a's
// journal, opened afterwards so those lines are the run's inherited history.
func newSeededReader(t *testing.T, prior []string) *geminiReader {
	t.Helper()

	dir := t.TempDir()
	home := filepath.Join(dir, "home")

	reader := newGeminiReader()
	reader.dir = dir
	reader.home = home
	reader.outfile = filepath.Join(dir, "telemetry.json")
	reader.now = steppingClock()
	if len(prior) > 0 {
		writeJournal(t, home, seededJournalName, append([]string{journalHeader("sess-a")}, prior...))
	}
	reader.Open("sess-a")
	return reader
}

const seededJournalName = "session-2026-01-01T00-00-sess-a.jsonl"

func appendJournal(t *testing.T, home, name string, lines []string) {
	t.Helper()

	path := filepath.Join(home, ".gemini", "tmp", "project", "chats", name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer file.Close() //nolint:errcheck // test handle
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("append journal: %v", err)
	}
}

func TestGeminiReportsTheFullerSourceWhenTelemetryIsShort(t *testing.T) {
	t.Parallel()

	// The second request read from cache and thought about its answer; neither
	// the cache read nor the reasoning moves the basis the bound is compared on.
	twoRequests := []string{
		journalMessage("m1", "2026-01-01T00:00:01Z", 500, 5, 0, 0),
		journalMessage("m2", "2026-01-01T00:00:02Z", 500, 5, 200, 40),
	}

	tests := []struct {
		name             string
		telemetry        []string
		journal          []string
		bound            int64
		wantSource       string
		want             domain.TokenUsage
		wantCompleteness Completeness
	}{
		{
			name:             "one request of a turn that made two",
			telemetry:        []string{apiResponse("sess-a", "model-x", 100, 0, 0, 0, 0, 100)},
			journal:          twoRequests,
			bound:            1000,
			wantSource:       geminiSourceJournal,
			want:             domain.TokenUsage{InputTokens: 1000, OutputTokens: 50, TotalTokens: 1050, CacheReadTokens: 200},
			wantCompleteness: CompletenessAccounted,
		},
		{
			name:             "reasoning on the first request covers the bound the bound does not count",
			telemetry:        []string{apiResponse("sess-a", "model-x", 100, 0, 0, 950, 0, 1050)},
			journal:          twoRequests,
			bound:            1000,
			wantSource:       geminiSourceJournal,
			want:             domain.TokenUsage{InputTokens: 1000, OutputTokens: 50, TotalTokens: 1050, CacheReadTokens: 200},
			wantCompleteness: CompletenessAccounted,
		},
		{
			name: "telemetry accounts for the turn in full",
			telemetry: []string{
				apiResponse("sess-a", "model-x", 500, 5, 0, 7, 0, 512),
				apiResponse("sess-a", "model-x", 500, 5, 0, 8, 0, 513),
			},
			journal:          twoRequests,
			bound:            1000,
			wantSource:       geminiSourceTelemetry,
			want:             domain.TokenUsage{InputTokens: 1000, OutputTokens: 25, TotalTokens: 1025},
			wantCompleteness: CompletenessAccounted,
		},
		{
			name:             "telemetry is short and no journal survives",
			telemetry:        []string{apiResponse("sess-a", "model-x", 100, 0, 0, 0, 0, 100)},
			bound:            1000,
			wantSource:       geminiSourceTelemetry,
			want:             domain.TokenUsage{InputTokens: 100, TotalTokens: 100},
			wantCompleteness: CompletenessPartial,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newSeededReader(t, nil)
			if len(tt.journal) > 0 {
				writeJournal(t, reader.home, seededJournalName,
					append([]string{journalHeader("sess-a")}, tt.journal...))
			}
			writeOutfile(t, reader, tt.telemetry...)

			recovered, source, found := drain(t, reader, tt.bound)
			if !found {
				t.Fatal("Drain() found = false, want a figure")
			}
			if source != tt.wantSource {
				t.Errorf("Drain() source = %q, want %q", source, tt.wantSource)
			}
			if recovered.Run != tt.want {
				t.Errorf("Drain() run-cumulative = %+v, want %+v", recovered.Run, tt.want)
			}
			if got := reader.Completeness(); got != tt.wantCompleteness {
				t.Errorf("Completeness() = %v, want %v: a figure short of the turn's bound is not the turn", got, tt.wantCompleteness)
			}
		})
	}
}

func TestGeminiDoesNotFallBehindAFigureAlreadyReported(t *testing.T) {
	t.Parallel()

	reader := newSeededReader(t, nil)
	writeJournal(t, reader.home, seededJournalName, []string{
		journalHeader("sess-a"),
		journalMessage("m1", "2026-01-01T00:00:01Z", 500, 5, 0, 0),
		journalMessage("m2", "2026-01-01T00:00:02Z", 500, 5, 0, 0),
	})

	first, source, found := drain(t, reader, 1000)
	if !found || source != geminiSourceJournal {
		t.Fatalf("first Drain() source = %q found = %v, want the journal's figure", source, found)
	}
	if first.Run.TotalTokens != 1010 {
		t.Fatalf("first Drain() total = %d, want 1010", first.Run.TotalTokens)
	}

	// The first turn's telemetry batch was lost with the process; only the
	// second turn's own record is ever delivered.
	writeOutfile(t, reader, apiResponse("sess-a", "model-x", 500, 5, 0, 0, 0, 505))
	appendJournal(t, reader.home, seededJournalName, []string{
		journalMessage("m3", "2026-01-01T00:00:03Z", 500, 5, 0, 0),
	})

	second, source, found := drain(t, reader, 500)
	if !found {
		t.Fatal("second Drain() found = false, want a figure")
	}
	want := domain.TokenUsage{InputTokens: 1500, OutputTokens: 15, TotalTokens: 1515}
	if second.Run != want {
		t.Errorf("second Drain() run-cumulative = %+v (source %q), want %+v: the run-cumulative figure never goes backwards",
			second.Run, source, want)
	}
}

func TestGeminiWaitsForTheTailOfAMultiRequestTurn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reader := newGeminiReader()
	reader.dir = dir
	reader.home = filepath.Join(dir, "home")
	reader.outfile = filepath.Join(dir, "telemetry.json")
	reader.now = slowClock()
	reader.Open("sess-a")

	writeOutfile(t, reader, apiResponse("sess-a", "model-x", 400, 5, 0, 0, 0, 405))

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(250 * time.Millisecond)
		file, err := os.OpenFile(reader.outfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer file.Close()                                                       //nolint:errcheck // test handle
		file.WriteString(apiResponse("sess-a", "model-x", 600, 5, 0, 0, 0, 605)) //nolint:errcheck,gosec // best effort
	}()
	t.Cleanup(func() { <-done })

	recovered, source, found := drain(t, reader, 1000)
	if !found {
		t.Fatal("Drain() found = false, want a figure")
	}
	if source != geminiSourceTelemetry {
		t.Errorf("Drain() source = %q, want %q", source, geminiSourceTelemetry)
	}
	want := domain.TokenUsage{InputTokens: 1000, OutputTokens: 10, TotalTokens: 1010}
	if recovered.Run != want {
		t.Errorf("Drain() run-cumulative = %+v, want %+v: both of the turn's requests", recovered.Run, want)
	}
}

// A zero counter is a measurement only where the turn's own bound reports no
// spend to contradict it.
func TestGeminiCountersAbsentOrDefaultedAreNotAMeasurement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		telemetry        []string
		bound            int64
		wantFound        bool
		want             domain.TokenUsage
		wantCompleteness Completeness
	}{
		{
			name:      "no record of this session at all",
			telemetry: []string{otherRepresentation("sess-a")},
			bound:     100,
		},
		{
			name:      "no counter in the attribute block",
			telemetry: []string{apiResponseWithoutCounters("sess-a", "model-x")},
			bound:     100,
		},
		{
			name:      "every counter defaulted to zero while the wire reported spend",
			telemetry: []string{apiResponse("sess-a", "model-x", 0, 0, 0, 0, 0, 0)},
			bound:     100,
		},
		{
			name:             "every counter defaulted to zero and no bound contradicts it",
			telemetry:        []string{apiResponse("sess-a", "model-x", 0, 0, 0, 0, 0, 0)},
			wantFound:        true,
			wantCompleteness: CompletenessUnknown,
		},
		{
			name:             "a counter carries a figure",
			telemetry:        []string{apiResponse("sess-a", "model-x", 100, 10, 0, 0, 0, 110)},
			bound:            100,
			wantFound:        true,
			want:             domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
			wantCompleteness: CompletenessAccounted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newSeededReader(t, nil)
			writeOutfile(t, reader, tt.telemetry...)

			recovered, _, found := drain(t, reader, tt.bound)
			if found != tt.wantFound {
				t.Fatalf("Drain() found = %v, want %v", found, tt.wantFound)
			}
			if recovered.Run != tt.want {
				t.Errorf("Drain() run-cumulative = %+v, want %+v", recovered.Run, tt.want)
			}
			if got := reader.Completeness(); got != tt.wantCompleteness {
				t.Errorf("Completeness() = %v, want %v", got, tt.wantCompleteness)
			}
		})
	}
}

func TestGeminiJournalBaselineIsTheHistoryPresentAtOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		prior []string
		after []string
		want  domain.TokenUsage
	}{
		{
			name:  "history written inside the second the session opened in",
			prior: []string{journalMessage("old", "2026-01-01T00:00:00.100Z", 999, 0, 0, 0)},
			after: []string{journalMessage("m1", "2026-01-01T00:00:01Z", 100, 10, 0, 0)},
			want:  domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
		},
		{
			name:  "history carrying no stamp at all",
			prior: []string{journalMessage("old", "", 999, 0, 0, 0)},
			after: []string{journalMessage("m1", "2026-01-01T00:00:01Z", 100, 10, 0, 0)},
			want:  domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
		},
		{
			name:  "a later revision of a message the run inherited",
			prior: []string{journalMessage("old", "2025-12-31T23:00:00Z", 999, 0, 0, 0)},
			after: []string{
				journalMessage("old", "2026-01-01T00:00:01Z", 999, 77, 0, 0),
				journalMessage("m1", "2026-01-01T00:00:02Z", 100, 10, 0, 0),
			},
			want: domain.TokenUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
		},
		{
			name:  "a message this run appended, whatever it is stamped",
			prior: []string{journalMessage("old", "2025-12-31T23:00:00Z", 999, 0, 0, 0)},
			after: []string{
				journalMessage("m1", "", 100, 10, 0, 0),
				journalMessage("m1", "2026-01-01T00:00:02Z", 120, 12, 0, 0),
			},
			want: domain.TokenUsage{InputTokens: 120, OutputTokens: 12, TotalTokens: 132},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newSeededReader(t, tt.prior)
			appendJournal(t, reader.home, seededJournalName, tt.after)

			recovered, source, found := drain(t, reader, 1)
			if !found {
				t.Fatal("Drain() found = false, want the journal's figure")
			}
			if source != geminiSourceJournal {
				t.Errorf("Drain() source = %q, want %q", source, geminiSourceJournal)
			}
			if recovered.Run != tt.want {
				t.Errorf("Drain() run-cumulative = %+v, want %+v: only what this run appended", recovered.Run, tt.want)
			}
		})
	}
}

// Neither source says which turn a late record belongs to, so a turn that ended
// short is only accounted once a later figure covers the shortfall too.
func TestGeminiGradesATurnAgainstWhatTheRunStillOwes(t *testing.T) {
	t.Parallel()

	const firstBound, secondBound = 1000, 200

	tests := []struct {
		name             string
		firstTelemetry   []string
		firstJournal     []string
		lateTelemetry    []string
		lateJournal      []string
		wantFirstTotal   int64
		wantFirst        Completeness
		wantSecondSource string
		wantSecondTotal  int64
		wantSecond       Completeness
	}{
		{
			name:             "the tail of a partial turn arrives in telemetry with no journal to read",
			firstTelemetry:   []string{apiResponse("sess-a", "model-x", 100, 0, 0, 0, 0, 100)},
			lateTelemetry:    []string{apiResponse("sess-a", "model-x", 900, 0, 0, 0, 0, 900)},
			wantFirstTotal:   100,
			wantFirst:        CompletenessPartial,
			wantSecondSource: geminiSourceTelemetry,
			wantSecondTotal:  1000,
			wantSecond:       CompletenessPartial,
		},
		{
			name:             "the tail of a partial turn arrives in the journal",
			firstTelemetry:   []string{apiResponse("sess-a", "model-x", 100, 0, 0, 0, 0, 100)},
			firstJournal:     []string{journalMessage("m1", "2026-01-01T00:00:01Z", 100, 0, 0, 0)},
			lateJournal:      []string{journalMessage("m2", "2026-01-01T00:00:02Z", 900, 0, 0, 0)},
			wantFirstTotal:   100,
			wantFirst:        CompletenessPartial,
			wantSecondSource: geminiSourceJournal,
			wantSecondTotal:  1000,
			wantSecond:       CompletenessPartial,
		},
		{
			name:           "the tail and the turn that followed it both arrive",
			firstTelemetry: []string{apiResponse("sess-a", "model-x", 100, 0, 0, 0, 0, 100)},
			lateTelemetry: []string{
				apiResponse("sess-a", "model-x", 900, 0, 0, 0, 0, 900),
				apiResponse("sess-a", "model-x", 200, 0, 0, 0, 0, 200),
			},
			wantFirstTotal:   100,
			wantFirst:        CompletenessPartial,
			wantSecondSource: geminiSourceTelemetry,
			wantSecondTotal:  1200,
			wantSecond:       CompletenessAccounted,
		},
		{
			name:             "two turns delivered in full owe nothing between them",
			firstTelemetry:   []string{apiResponse("sess-a", "model-x", 1000, 0, 0, 0, 0, 1000)},
			lateTelemetry:    []string{apiResponse("sess-a", "model-x", 200, 0, 0, 0, 0, 200)},
			wantFirstTotal:   1000,
			wantFirst:        CompletenessAccounted,
			wantSecondSource: geminiSourceTelemetry,
			wantSecondTotal:  1200,
			wantSecond:       CompletenessAccounted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newSeededReader(t, nil)
			if len(tt.firstJournal) > 0 {
				writeJournal(t, reader.home, seededJournalName,
					append([]string{journalHeader("sess-a")}, tt.firstJournal...))
			}
			writeOutfile(t, reader, tt.firstTelemetry...)

			first, _, found := drain(t, reader, firstBound)
			if !found {
				t.Fatal("first Drain() found = false, want a figure")
			}
			if first.Run.TotalTokens != tt.wantFirstTotal {
				t.Fatalf("first Drain() total = %d, want %d", first.Run.TotalTokens, tt.wantFirstTotal)
			}
			if got := reader.Completeness(); got != tt.wantFirst {
				t.Fatalf("first Completeness() = %v, want %v", got, tt.wantFirst)
			}

			writeOutfile(t, reader, tt.lateTelemetry...)
			if len(tt.lateJournal) > 0 {
				appendJournal(t, reader.home, seededJournalName, tt.lateJournal)
			}

			second, source, found := drain(t, reader, secondBound)
			if !found {
				t.Fatal("second Drain() found = false, want a figure")
			}
			if source != tt.wantSecondSource {
				t.Errorf("second Drain() source = %q, want %q", source, tt.wantSecondSource)
			}
			if second.Run.TotalTokens != tt.wantSecondTotal {
				t.Errorf("second Drain() total = %d, want %d", second.Run.TotalTokens, tt.wantSecondTotal)
			}
			if got := reader.Completeness(); got != tt.wantSecond {
				t.Errorf("second Completeness() = %v, want %v: the run owes what earlier turns' bounds reported and no figure covered",
					got, tt.wantSecond)
			}
		})
	}
}

func TestGeminiATurnNothingMeasuredIsStillOwed(t *testing.T) {
	t.Parallel()

	reader := newSeededReader(t, nil)
	if _, _, found := drain(t, reader, 1000); found {
		t.Fatal("first Drain() found = true, want no figure at all")
	}

	writeOutfile(t, reader, apiResponse("sess-a", "model-x", 1000, 0, 0, 0, 0, 1000))

	second, _, found := drain(t, reader, 200)
	if !found {
		t.Fatal("second Drain() found = false, want a figure")
	}
	if second.Run.TotalTokens != 1000 {
		t.Errorf("second Drain() total = %d, want 1000", second.Run.TotalTokens)
	}
	if got := reader.Completeness(); got != CompletenessPartial {
		t.Errorf("second Completeness() = %v, want %v: the first turn's whole bound is still uncovered", got, CompletenessPartial)
	}
}

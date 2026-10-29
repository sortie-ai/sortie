package claude

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	journalSessionID  = "a597a993-8d4d-4b72-b9bc-97b8bf3a14b8"
	journalSubagentID = "a1cb8879fa61c9eed"
)

func showReading(r *usage.SpendReading) string {
	if r == nil {
		return "nil"
	}
	count := func(c *int64) string {
		if c == nil {
			return "-"
		}
		return fmt.Sprint(*c)
	}
	shown := fmt.Sprintf("%s|%s|%s,%s,%s,%s", r.Model, r.Identity, count(r.Input), count(r.Output), count(r.CacheRead), count(r.CacheWrite))
	for _, entry := range r.ByModel {
		shown += " {" + showReading(&entry) + "}"
	}
	return shown
}

func rawMembers(members ...string) []json.RawMessage {
	raw := make([]json.RawMessage, len(members))
	for i, member := range members {
		raw[i] = json.RawMessage(member)
	}
	return raw
}

func newJournalState(t *testing.T) (state *sessionState, projectDir string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	state = &sessionState{
		claudeSessionID: journalSessionID,
		journalDir:      journalDir("/workspace/project"),
		baseLogger:      slog.New(slog.DiscardHandler),
	}
	return state, filepath.Join(home, "projects", state.journalDir)
}

func writeJournalFile(t *testing.T, content []byte, elem ...string) {
	t.Helper()

	path := filepath.Join(elem...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

type journalFiles struct {
	session string
	agent   string
	tail    string
}

func (f journalFiles) install(t *testing.T, projectDir string) {
	t.Helper()

	if f.session != "" {
		content := string(loadFixture(t, "journal/"+f.session)) + f.tail
		writeJournalFile(t, []byte(content), projectDir, journalSessionID+".jsonl")
	}
	if f.agent != "" {
		writeJournalFile(t, loadFixture(t, "journal/"+f.agent), projectDir, journalSessionID, "subagents", "agent-"+journalSubagentID+".jsonl")
	}
}

func TestJournalDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"space", "/with space", "-with-space"},
		{"letters outside ascii", "/zażółć", "-za----"},
		{"cjk", "/中文", "---"},
		{"astral is two units", "/e\U0001F600x", "-e--x"},
		{"colon", "/a:b", "-a-b"},
		{"runs are not collapsed", "/x--__..  ::y", "-x----------y"},
		{"case kept", "/UpperCase", "-UpperCase"},
		{"underscore and dot", "/ws/my_proj.v2", "-ws-my-proj-v2"},
		{"leading dash", "/a/-b", "-a--b"},
		{"200 units has no suffix", "/" + strings.Repeat("a", 197) + "\U0001F600", "-" + strings.Repeat("a", 197) + "--"},
		{"201 units gains hash suffix", "/" + strings.Repeat("a", 198) + "\U0001F600", "-" + strings.Repeat("a", 198) + "--b5wpam"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := journalDir(tt.cwd)

			if got != tt.want {
				t.Errorf("journalDir(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
			if !agentcore.ValidPathSegment(got) || len(got) > 207 {
				t.Errorf("journalDir(%q) = %q, want one valid segment of at most 207 bytes", tt.cwd, got)
			}
		})
	}
}

func TestLiveReading(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		members []json.RawMessage
		want    string
	}{
		{"no members", nil, "nil"},
		{"absent member", rawMembers(``), "nil"},
		{"null member", rawMembers(`null`), "nil"},
		{"non-object member", rawMembers(`[]`), "nil"},
		{"one bad member spoils the turn", rawMembers(`{"a":{"inputTokens":1}}`, `"x"`), "nil"},
		{"only empty members is a measured zero", rawMembers(`{}`, `{}`), "||0,0,0,0"},
		{
			"empty later member adds nothing",
			rawMembers(`{"a":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4}}`, `{}`),
			"||-,-,-,- {a||1,2,3,4}",
		},
		{
			"entries of every member, sorted within a member",
			rawMembers(`{"b":{"outputTokens":2},"a":{"outputTokens":1}}`, `{"c":{"outputTokens":3}}`),
			"||-,-,-,- {a||-,1,-,-} {b||-,2,-,-} {c||-,3,-,-}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := showReading(liveReading(tt.members))

			if got != tt.want {
				t.Errorf("liveReading(%q) = %q, want %q", tt.members, got, tt.want)
			}
		})
	}
}

func TestJournalLineReadings(t *testing.T) {
	journal := strings.Join([]string{
		`not json`,
		`null`,
		`{"type":"assistant","message":{"id":"","model":"m","usage":{"input_tokens":9,"output_tokens":9}}}`,
		`{"type":"assistant","message":{"id":"msg_s","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"user","toolUseResult":"plain text"}`,
		`{"type":"assistant","requestId":"req_1","message":{"id":"msg_2","model":"m","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}}`,
	}, "\n")
	state, projectDir := newJournalState(t)
	writeJournalFile(t, []byte(journal), projectDir, journalSessionID+".jsonl")

	read := state.readJournal(t.Context())

	var got []string
	for _, reading := range read.Readings {
		got = append(got, showReading(&reading))
	}
	want := []string{
		"|" + journalSessionID + ".jsonl:1|-,-,-,-",
		"|" + journalSessionID + ".jsonl:2|-,-,-,-",
		"|" + journalSessionID + ".jsonl:3|-,-,-,-",
		"m|msg_2/req_1|1,2,3,4",
	}
	if !read.OK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("readJournal() = OK %t, readings %q, want OK true, readings %q", read.OK, got, want)
	}
}

func TestJournalReadOutcome(t *testing.T) {
	tests := []struct {
		name         string
		files        journalFiles
		streamAgents []string
		wantOK       bool
		wantReadings int
	}{
		{"missing session journal fails the read", journalFiles{}, nil, false, 0},
		{"sub-agent named by the session journal but missing is skipped", journalFiles{session: "subagent.jsonl"}, nil, true, 3},
		{"sub-agent named by the session journal is read", journalFiles{session: "subagent.jsonl", agent: "subagent_agent.jsonl"}, nil, true, 5},
		{"sub-agent named by the stream is read", journalFiles{session: "killed.jsonl", agent: "subagent_agent.jsonl"}, []string{journalSubagentID}, true, 5},
		{"sub-agent unnamed by either is not found", journalFiles{session: "killed.jsonl", agent: "subagent_agent.jsonl"}, nil, true, 3},
		{"sub-agent named by both is read once", journalFiles{session: "subagent.jsonl", agent: "subagent_agent.jsonl"}, []string{journalSubagentID}, true, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, projectDir := newJournalState(t)
			state.subagents = tt.streamAgents
			tt.files.install(t, projectDir)

			read := state.readJournal(t.Context())

			if read.OK != tt.wantOK || len(read.Readings) != tt.wantReadings {
				t.Errorf("readJournal() = OK %t with %d readings, want OK %t with %d", read.OK, len(read.Readings), tt.wantOK, tt.wantReadings)
			}
		})
	}
}

func TestJournalSettlesThroughLedger(t *testing.T) {
	const (
		cutLine = `{"type":"assistant","message":{"id":"msg_cut`
		newLine = `{"type":"assistant","requestId":"req_new","message":{"id":"msg_new","model":"claude-haiku-5-5","usage":{"input_tokens":1,"output_tokens":7,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}}`
	)
	usageOf := func(input, output, cacheRead, cacheWrite int64) domain.TokenUsage {
		return domain.TokenUsage{
			InputTokens:      input + cacheRead + cacheWrite,
			OutputTokens:     output,
			TotalTokens:      input + cacheRead + cacheWrite + output,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
		}
	}

	tests := []struct {
		name       string
		boundary   *journalFiles
		settle     journalFiles
		want       domain.TokenUsage
		incomplete bool
	}{
		{
			name:       "killed turn counts completed requests once",
			settle:     journalFiles{session: "killed.jsonl"},
			want:       usageOf(4, 405, 58949, 32416),
			incomplete: true,
		},
		{
			name:   "session and sub-agent journals sum to the stream member",
			settle: journalFiles{session: "subagent.jsonl", agent: "subagent_agent.jsonl"},
			want:   usageOf(8, 643, 88584, 72305),
		},
		{
			name:     "resumed process counts only its own request",
			boundary: &journalFiles{session: "killed.jsonl"},
			settle:   journalFiles{session: "resumed_after_kill.jsonl"},
			want:     usageOf(2, 4, 46594, 52),
		},
		{
			name:     "resumed sub-agent keeps its earlier requests as history",
			boundary: &journalFiles{session: "subagent.jsonl", agent: "subagent_agent.jsonl"},
			settle:   journalFiles{session: "subagent_resumed.jsonl", agent: "subagent_resumed_agent.jsonl"},
			want:     usageOf(16, 854, 196423, 78745),
		},
		{
			name:       "boundary line cut short by a kill stays history",
			boundary:   &journalFiles{session: "killed.jsonl", tail: cutLine},
			settle:     journalFiles{session: "killed.jsonl", tail: cutLine + "\n" + newLine},
			want:       usageOf(1, 7, 2, 3),
			incomplete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, projectDir := newJournalState(t)
			ledger, err := usage.NewLedger(usageSources.Live, usageSources.Records)
			if err != nil {
				t.Fatalf("NewLedger: %v", err)
			}
			if tt.boundary == nil {
				ledger.SessionCreated()
			} else {
				tt.boundary.install(t, projectDir)
				read := state.readJournal(t.Context())
				if !read.OK {
					t.Fatalf("readJournal() for the boundary = OK false, want true")
				}
				ledger.Boundary(read)
			}
			tt.settle.install(t, projectDir)

			got := ledger.Settle(usage.TurnEnd{ModelReached: true}, nil, state.readJournal(t.Context()))

			if got.Run != tt.want {
				t.Errorf("Settle().Run = %+v, want %+v", got.Run, tt.want)
			}
			wantProvenance := domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords}
			if got.Provenance != wantProvenance {
				t.Errorf("Settle().Provenance = %+v, want %+v", got.Provenance, wantProvenance)
			}
			if tt.incomplete && got.Complete {
				t.Errorf("Settle().Complete = true, want false")
			}
		})
	}
}

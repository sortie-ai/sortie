package usagesource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	openCodeExportDir = "../../../opencode/sessionexport/testdata/"
	openCodeLaunchVar = "SORTIE_TEST_OPENCODE_VAR"
	openCodeSessionID = "ses_e7ac6691effe73waoQYzNihPM6"
	openCodeModel     = "opencode/deepseek-v4.1-flash-free"
)

type exportRecorder struct {
	WantArgs                 []string
	WantDir, WantVar, Stdout string
}

func recordExport(args []string, want exportRecorder) int {
	dir, _ := os.Getwd()
	dir, _ = filepath.EvalSymlinks(dir)
	wantDir, _ := filepath.EvalSymlinks(want.WantDir)
	if !slices.Equal(args, want.WantArgs) || dir != wantDir || os.Getenv(openCodeLaunchVar) != want.WantVar {
		return 3
	}
	fmt.Print(want.Stdout)
	return 0
}

func TestMain(m *testing.M) {
	agenttest.Main(m, map[string]agenttest.Scenario{"exportRecorder": agenttest.Typed(recordExport)})
}

type openCodeRuntime struct {
	PullRuntime
	session string
	export  []byte
	target  agentcore.LaunchTarget
	env     []string
}

func (r openCodeRuntime) SessionID() string      { return r.session }
func (r openCodeRuntime) CommandWords() []string { return r.target.CommandWords() }

func (r openCodeRuntime) RunHostCommand(ctx context.Context, cmd agentcore.HostCommand) ([]byte, error) {
	if r.export != nil {
		return r.export, nil
	}
	return r.target.RunHostCommand(ctx, r.env, cmd)
}

func readOpenCodeRecords(t *testing.T, rt openCodeRuntime) []usage.SpendReading {
	t.Helper()

	records, err := mustDeclaration(t, opencodeHandshakeName).ReadRecords(t.Context(), rt)
	if err != nil {
		t.Fatalf("ReadRecords(%q) = %v, want records", rt.session, err)
	}
	return records.Spend
}

func counts(input, output, reasoning, cacheRead, cacheWrite int64) *usage.SpendReading {
	return &usage.SpendReading{Input: &input, Output: &output, Reasoning: &reasoning, CacheRead: &cacheRead, CacheWrite: &cacheWrite}
}

func TestOpenCodePromptSpend(t *testing.T) {
	t.Parallel()

	missingInput, nullReasoning := counts(0, 2, 0, 0, 0), counts(1, 2, 0, 0, 0)
	missingInput.Input, nullReasoning.Reasoning = nil, nil
	tests := []struct {
		name, result string
		want         *usage.SpendReading
	}{
		{"cache write tokens", readFixture(t, "opencode-haiku-cache-result-1.json"), counts(4, 4, 0, 1385, 24473)},
		{"cancelled after a finished step", readFixture(t, "opencode-child-cancel-result.json"), counts(102, 106, 79, 17152, 0)},
		{"cancelled before any step", readFixture(t, "opencode-cancel-result.json"), nil},
		{"null usage", `{"usage":null}`, nil},
		{"missing input tokens", `{"usage":{"outputTokens":2}}`, missingInput},
		{"null thought tokens", `{"usage":{"inputTokens":1,"outputTokens":2,"thoughtTokens":null}}`, nullReasoning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := opencodePromptSpend([]byte(tt.result))

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("opencodePromptSpend(%.60s) = %s, want %s", tt.result, spendJSON(t, got), spendJSON(t, tt.want))
			}
		})
	}
}

func TestOpenCodeSettlesEachTurnToItsMessages(t *testing.T) {
	t.Parallel()

	type turn struct {
		result  string
		records int
		run     domain.TokenUsage
	}
	tests := []struct {
		name, session, export, history, model string
		turns                                 []turn
	}{
		{"a turn of two steps with reasoning outside output", "ses_e7ac4b348ffeP2wfjLJwh4m3tB", "flash-latest-export.json", "", "google/gemini-flash-latest", []turn{
			{"opencode-flash-latest-result-1.json", 1, domain.TokenUsage{InputTokens: 17283, OutputTokens: 40, TotalTokens: 17323}},
			{"opencode-flash-latest-result-2.json", 3, domain.TokenUsage{InputTokens: 53768, OutputTokens: 185, TotalTokens: 53953, CacheReadTokens: 31340}},
		}},
		{"a resumed session counts only the new message", openCodeSessionID, "resume-resumed-export.json", "load-resumed-export.json", openCodeModel, []turn{
			{"opencode-resume-resumed-result.json", 4, domain.TokenUsage{InputTokens: 18003, OutputTokens: 13, TotalTokens: 18016, CacheReadTokens: 17920}},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declaration := mustDeclaration(t, opencodeHandshakeName)
			ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
			if err != nil {
				t.Fatalf("NewLedger: %v", err)
			}
			if tt.history == "" {
				ledger.SessionCreated()
			} else {
				ledger.Boundary(usage.RecordsRead{Readings: readOpenCodeRecords(t, openCodeRuntime{session: tt.session, export: []byte(readFixture(t, openCodeExportDir+tt.history))}), OK: true})
			}
			records := readOpenCodeRecords(t, openCodeRuntime{session: tt.session, export: []byte(readFixture(t, openCodeExportDir+tt.export))})

			for _, turn := range tt.turns {
				live := declaration.PromptSpend([]byte(readFixture(t, turn.result)))
				got := ledger.Settle(usage.TurnEnd{Replied: true, ModelReached: true}, live, usage.RecordsRead{Readings: records[:turn.records], OK: true})

				if got.Run != turn.run || got.ByModel[tt.model] != turn.run || len(got.ByModel) != 1 || !got.Complete {
					t.Errorf("Settle(%s) = run %+v by model %+v complete %v, want %+v under %q complete", turn.result, got.Run, got.ByModel, got.Complete, turn.run, tt.model)
				}
			}
		})
	}
}

func TestOpenCodeLiveReadingAddsCacheOutsideInput(t *testing.T) {
	t.Parallel()

	declaration := mustDeclaration(t, opencodeHandshakeName)
	ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	live := declaration.PromptSpend([]byte(readFixture(t, "opencode-haiku-cache-result-1.json")))

	got := ledger.Settle(usage.TurnEnd{Replied: true, ModelReached: true}, live, usage.RecordsRead{}).Run

	if want := (domain.TokenUsage{InputTokens: 25862, OutputTokens: 4, TotalTokens: 25866, CacheReadTokens: 1385, CacheWriteTokens: 24473}); got != want {
		t.Errorf("Settle(live only) run = %+v, want %+v", got, want)
	}
}

func TestExportCommandCutsTheConfiguredCommandAtTheRuntime(t *testing.T) {
	t.Parallel()

	for command, want := range map[string]agentcore.HostCommand{
		"opencode acp":                  {LaunchWords: 1},
		"npx -y @opencode/cli@2 acp":    {LaunchWords: 3},
		"env OPENCODE_X=1 opencode acp": {LaunchWords: 3},
		"run-agent.sh":                  {Name: "opencode"},
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()

			want.Args, want.InSession, want.Limit = []string{"session", "export", "--standalone", "--sanitize", "ses_1"}, true, usage.MaxRecordsBytes

			got := exportCommand(strings.Fields(command), "ses_1")

			if !reflect.DeepEqual(got, want) {
				t.Errorf("exportCommand(%q) = %+v, want %+v", command, got, want)
			}
		})
	}
}

func TestOpenCodeReadRecordsRunsTheConfiguredRuntimeInTheWorkspace(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	wantArgs := []string{"-y", "@opencode/cli@2", "session", "export", "--standalone", "--sanitize", openCodeSessionID}
	recorder := exportRecorder{wantArgs, workspace, "from-launch", readFixture(t, openCodeExportDir+"two-turns-export.json")}
	exe := agenttest.FakeRuntime(t, t.TempDir(), "npx", "exportRecorder", recorder)
	args := []string{"-y", "@opencode/cli@2", "acp"}
	rt := openCodeRuntime{session: openCodeSessionID, target: agentcore.LaunchTarget{Command: exe, Args: args, WorkspacePath: workspace}, env: []string{openCodeLaunchVar + "=from-launch"}}

	if got := readOpenCodeRecords(t, rt); len(got) != 2 {
		t.Errorf("ReadRecords(%v) = %d records, want 2", args, len(got))
	}
}

func TestOpenCodeDisableTitle(t *testing.T) {
	t.Parallel()

	const disabled = `{"agent":{"title":{"disable":true}}}`
	tests := []struct{ name, current, want string }{
		{"unset", "", disabled},
		{"object without agent", `{"model":"a/b","permission":{"*":"deny"}}`, `{"agent":{"title":{"disable":true}},"model":"a/b","permission":{"*":"deny"}}`},
		{"agent with other members", `{"agent":{"build":{"mode":"primary"}}}`, `{"agent":{"build":{"mode":"primary"},"title":{"disable":true}}}`},
		{"disable true", disabled, disabled},
		{"disable false", `{"agent":{"title":{"disable":false}}}`, ""},
		{"not JSON", `not json`, ""},
		{"agent not an object", `{"agent":"x"}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, inEffect := opencodeDisableTitle(tt.current, tt.current != "")

			if inEffect != (tt.want != "") || (inEffect && got != tt.want) || (!inEffect && got != tt.current) {
				t.Errorf("opencodeDisableTitle(%q) = %q, %v, want %q, %v", tt.current, got, inEffect, tt.want, tt.want != "")
			}
		})
	}
}

func TestOpenCodeChildSession(t *testing.T) {
	t.Parallel()

	const parent = "ses_e7a97b044ffeLBcm210kbDgVGn"
	tests := []struct {
		name, update string
		want         ChildSignal
	}{
		{"subagent call starts", readFixture(t, "opencode-child-session-update-15.json"), ChildSignal{Call: "call_00_cxt443ul4ms1i2w2360sslz0", Started: true}},
		{"child chunk forwarded", readFixture(t, "opencode-child-session-update-17.json"), ChildSignal{Session: "ses_e7a97a463ffeLUn2P96bFtM6MV"}},
		{"subagent call completes", readFixture(t, "opencode-child-session-update-18.json"), ChildSignal{Call: "call_00_cxt443ul4ms1i2w2360sslz0", Session: "ses_e7a97a463ffeLUn2P96bFtM6MV", Completed: true}},
		{"cancelled subagent call fails", readFixture(t, "opencode-child-cancel-update-19.json"), ChildSignal{Call: "call_00_k6e245re32391p6jwn5jo9ho", Session: "ses_e79773346ffeKVGj032T0Q7h9p"}},
		{"read call completes", readFixture(t, "opencode-flash-latest-read-15.json"), ChildSignal{}},
		{"null agent names nothing", `{"sessionUpdate":"tool_call_update","toolCallId":"c","status":"in_progress","rawInput":{"agent":null}}`, ChildSignal{}},
		{"the session's own chunk names nothing", `{"sessionUpdate":"agent_message_chunk","_meta":{"opencode/child-session":{"id":"` + parent + `"}}}`, ChildSignal{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := opencodeChildSession(parent, []byte(tt.update))

			if got != tt.want {
				t.Errorf("opencodeChildSession(%q, %.80s) = %+v, want %+v", parent, tt.update, got, tt.want)
			}
		})
	}
}

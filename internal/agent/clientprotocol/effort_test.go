package clientprotocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func selectOption(id, category, current string, values ...string) string {
	entries := make([]string, len(values))
	for i, value := range values {
		entries[i] = fmt.Sprintf(`{"value":%[1]q,"name":%[1]q}`, value)
	}
	return fmt.Sprintf(`{"id":%[1]q,"name":%[1]q,"category":%q,"type":"select","currentValue":%q,"options":[%s]}`,
		id, category, current, strings.Join(entries, ","))
}

func configOptions(options string) string { return `{"configOptions":[` + options + `]}` }

type effortRuntime struct {
	options, setResult string
	setError, hold     bool
	sets, carried      []string
}

func (r *effortRuntime) run(t *testing.T, logger *slog.Logger, effort string, verification, continued bool, turns int) error {
	t.Helper()

	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)
	params := domain.StartSessionParams{CredentialVerification: verification, Settings: map[string]any{registry.EffortKey: effort}}
	if continued {
		params.ResumeSessionID = "held-1"
	}
	caps := &agentCapabilities{SessionCapabilities: &sessionCapabilities{Resume: &sessionResumeCapabilities{}}}
	done := make(chan error, 1)
	go func() {
		session, err := finishSession(t.Context(), state, params, agentcore.LaunchTarget{}, &initializeResponse{AgentCapabilities: caps}, usagesource.Declarations)
		for range turns {
			if err == nil {
				_, err = runTurn(t.Context(), session, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
			}
		}
		done <- err
	}()

	reply := func(id json.RawMessage, result string) { respondLine(t, inPw, id, json.RawMessage(result)) }
	var held json.RawMessage
	last := ""
	for len(r.carried) < turns {
		var msg struct {
			wireHeader
			Params setConfigOptionRequest `json:"params"`
		}
		_ = json.Unmarshal(out.next(t), &msg)
		switch msg.Method {
		case negativeControlMethod:
			respondErrorLine(t, inPw, msg.ID, -32601, "method not found")
		case methodSessionNew, methodSessionResume:
			reply(msg.ID, `{"sessionId":"sess-1","configOptions":[`+r.options+`]}`)
		case methodSessionSetConfigOption:
			r.sets = append(r.sets, fmt.Sprintf("%s=%s", msg.Params.ConfigID, msg.Params.Value))
			last = string(msg.Params.Value)
			switch {
			case r.hold:
				held = msg.ID
			case r.setError:
				respondErrorLine(t, inPw, msg.ID, -32602, "bad value")
			default:
				reply(msg.ID, r.setResult)
			}
		case methodSessionPrompt:
			r.carried = append(r.carried, last)
			if held != nil {
				reply(held, "{}")
			}
			respondLine(t, inPw, msg.ID, promptResponse{StopReason: stopReasonEndTurn})
		}
	}
	return waittest.Receive(t, done, "session")
}

func protocolEffortProbe(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error) {
	t.Helper()

	effort, fault := registry.EffortSetting(settings)
	if fault != nil {
		adapter, _ := NewClientProtocolAdapter()
		_, err := adapter.StartSession(t.Context(), domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: "sortie-never-launched"},
			Settings:      settings,
		})
		return nil, err
	}
	runtime := &effortRuntime{
		options:   selectOption("effort", "thought_level", "default", "default", effort),
		setResult: configOptions(selectOption("effort", "thought_level", effort, "default", effort)),
	}

	err := runtime.run(t, discardLogger(), effort, verification, resumed, turns)
	return runtime.carried, err
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	agenttest.AssertEffortForwarding(t, protocolEffortProbe)
}

func logRecord(level slog.Level, msg string, attrs ...string) agenttest.LogSpyEntry {
	entry := agenttest.LogSpyEntry{Level: level, Msg: msg, Attrs: map[string]string{"agent_kind": "agent-client-protocol"}}
	for i := 0; i < len(attrs); i += 2 {
		entry.Attrs[attrs[i]] = attrs[i+1]
	}
	return entry
}

func TestApplyReasoningLevel(t *testing.T) {
	t.Parallel()

	offers := selectOption("effort", "thought_level", "default", "low", "high")
	warn := func(effort, reason string, attrs ...string) []agenttest.LogSpyEntry {
		return []agenttest.LogSpyEntry{logRecord(slog.LevelWarn, "reasoning level not applied by the agent", append([]string{"effort", effort, "reason", reason}, attrs...)...)}
	}
	grouped := `{"id":"effort","name":"Effort","category":"thought_level","type":"select","currentValue":"low","options":[{"group":"g","name":"G","options":[{"value":"high","name":"high"}]}]}`
	bound := fmt.Sprint(agentcore.ReadTimeout(domain.AgentConfig{}).Milliseconds())
	tests := []struct {
		name, options, effort, setResult string
		continued, setError, hold        bool
		wantSet                          string
		want                             []agenttest.LogSpyEntry
	}{
		{name: "no options", effort: "high", want: warn("high", "no_option")},
		{name: "neither a boolean option nor a select of another category", options: `{"id":"effort","name":"Effort","category":"thought_level","type":"boolean","currentValue":true},` + selectOption("model", "model", "a", "high"), effort: "high", want: warn("high", "no_option")},
		{name: "a level the option does not offer", options: offers, effort: "ultra", want: warn("ultra", "not_offered", "option", "effort", "offered", "low, high")},
		{name: "the runtime answers with an error", options: offers, effort: "high", setError: true, wantSet: "effort=high", want: warn("high", "rejected", "option", "effort", "agent_error", "-32602: bad value")},
		{name: "the result lists another current value", options: offers, effort: "high", wantSet: "effort=high", setResult: configOptions(selectOption("effort", "thought_level", "low", "low", "high")), want: warn("high", "other_value", "option", "effort", "current", "low")},
		{name: "no answer within the read bound, then a late answer", options: offers, effort: "high", hold: true, wantSet: "effort=high", want: warn("high", "no_answer", "option", "effort", "read_timeout_ms", bound)},
		{name: "a grouped option's values match and the result lists the level current", options: grouped, effort: "high", wantSet: "effort=high", setResult: configOptions(strings.Replace(grouped, `"low"`, `"high"`, 1))},
		{name: "the first thought_level select in sent order is the target", options: selectOption("first", "thought_level", "low", "low") + "," + selectOption("second", "thought_level", "low", "low", "high"), effort: "high", want: warn("high", "not_offered", "option", "first", "offered", "low")},
		{name: "a continued session without a level keeps the held one", options: offers, continued: true, want: []agenttest.LogSpyEntry{logRecord(slog.LevelInfo, "continued session keeps the agent's reasoning level", "option", "effort", "current", "default")}},
		{name: "a continued session whose option holds no value", options: selectOption("effort", "thought_level", "", "low"), continued: true},
		{name: "a created session without a level", options: offers},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				spy := &agenttest.LogSpy{}
				runtime := &effortRuntime{options: tt.options, setResult: tt.setResult, setError: tt.setError, hold: tt.hold}

				err := runtime.run(t, slog.New(spy), tt.effort, false, tt.continued, 1)

				if err != nil {
					t.Fatalf("session with effort %q error = %v, want the first turn to run", tt.effort, err)
				}
				if got := strings.Join(runtime.sets, ","); got != tt.wantSet {
					t.Errorf("set_config_option requests for effort %q = %q, want %q", tt.effort, got, tt.wantSet)
				}
				got := slices.DeleteFunc(spy.Entries(), func(e agenttest.LogSpyEntry) bool {
					return !strings.HasPrefix(e.Msg, "reasoning level not applied") && !strings.HasPrefix(e.Msg, "continued session keeps")
				})
				if fmt.Sprint(got) != fmt.Sprint(tt.want) {
					t.Errorf("log records for effort %q = %v, want %v", tt.effort, got, tt.want)
				}
			})
		})
	}
}

func TestValidateVariantNotRead(t *testing.T) {
	t.Parallel()

	for variant, want := range map[any]int{"high": 1, nil: 0} {
		diags := validateConfig(registry.AgentConfigFields{Kind: "agent-client-protocol", Passthrough: map[string]any{variantKey: variant}})

		if len(diags) != want || (want == 1 && (diags[0].Check != "agent-client-protocol.variant.not_read" || diags[0].Severity != string(registry.SeverityWarning))) {
			t.Errorf("validateConfig(variant=%v) = %+v, want %d variant.not_read warning", variant, diags, want)
		}
	}
}

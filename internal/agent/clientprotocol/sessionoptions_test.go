package clientprotocol

import (
	"cmp"
	"encoding/json"
	"errors"
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

func selectOptionJSON(id, category, current string, values ...string) string {
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
	entries, results   map[string]string
	setError, hold     bool
	sets, carried      []string
}

func (r *effortRuntime) run(t *testing.T, logger *slog.Logger, effort string, verification, continued bool, turns int) error {
	t.Helper()

	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, logger)
	state.configOptions = r.entries
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
				result, ok := r.results[string(msg.Params.ConfigID)]
				if !ok {
					result = r.setResult
				}
				reply(msg.ID, cmp.Or(result, "{}"))
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
		options:   selectOptionJSON("effort", "thought_level", "default", "default", effort),
		setResult: configOptions(selectOptionJSON("effort", "thought_level", effort, "default", effort)),
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

func TestApplySessionOptions(t *testing.T) {
	t.Parallel()

	offers := selectOptionJSON("effort", "thought_level", "default", "low", "high")
	warn := func(effort, reason string, attrs ...string) []agenttest.LogSpyEntry {
		return []agenttest.LogSpyEntry{logRecord(slog.LevelWarn, "reasoning level not applied by the agent", append([]string{"effort", effort, "reason", reason}, attrs...)...)}
	}
	grouped := `{"id":"effort","name":"Effort","category":"thought_level","type":"select","currentValue":"low","options":[{"group":"g","name":"G","options":[{"value":"high","name":"high"}]}]}`
	skip := func(option, value, reason string, attrs ...string) []agenttest.LogSpyEntry {
		return []agenttest.LogSpyEntry{logRecord(slog.LevelWarn, "session option not applied by the agent", append([]string{"option", option, "value", value, "reason", reason}, attrs...)...)}
	}
	mode := selectOptionJSON("mode", "mode", "default", "default", "plan")
	modeSet := func(current string) string {
		return configOptions(selectOptionJSON("mode", "mode", current, "default", "plan"))
	}
	held := strings.Join([]string{
		selectOptionJSON("named", "_runtime", "b", "a", "b"),
		selectOptionJSON("agent", "_runtime", "sortie-probe", "default", "sortie-probe"),
		selectOptionJSON("cleared", "_runtime", "", "x"),
		selectOptionJSON("allow_all", "permissions", "off", "off", "on"),
	}, ",")
	namedSet := configOptions(strings.Replace(held, `"currentValue":"b"`, `"currentValue":"a"`, 1))
	heldRecord := []agenttest.LogSpyEntry{logRecord(slog.LevelInfo, "continued session keeps the agent's session option", "option", "agent", "current", "sortie-probe")}
	bound := fmt.Sprint(agentcore.ReadTimeout(domain.AgentConfig{}).Milliseconds())
	tests := []struct {
		name, options, effort, setResult string
		entries, results                 map[string]string
		continued, setError, hold        bool
		wantSet                          string
		want                             []agenttest.LogSpyEntry
	}{
		{name: "no options", effort: "high", want: warn("high", "no_option", "advertised", "")},
		{name: "neither a boolean option nor a select of another category", options: `{"id":"effort","name":"Effort","category":"thought_level","type":"boolean","currentValue":true},` + selectOptionJSON("model", "model", "a", "high") + "," + mode, effort: "high", want: warn("high", "no_option", "advertised", "mode")},
		{name: "a level the option does not offer", options: offers, effort: "ultra", want: warn("ultra", "not_offered", "option", "effort", "offered", "low, high")},
		{name: "the runtime answers with an error", options: offers, effort: "high", setError: true, wantSet: "effort=high", want: warn("high", "rejected", "option", "effort", "agent_error", "-32602: bad value")},
		{name: "the result lists another current value", options: offers, effort: "high", wantSet: "effort=high", setResult: configOptions(selectOptionJSON("effort", "thought_level", "low", "low", "high")), want: warn("high", "other_value", "option", "effort", "current", "low")},
		{name: "no answer within the read bound, then a late answer", options: offers, effort: "high", hold: true, wantSet: "effort=high", want: warn("high", "no_answer", "option", "effort", "read_timeout_ms", bound)},
		{name: "a grouped option's values match and the result lists the level current", options: grouped, effort: "high", wantSet: "effort=high", setResult: configOptions(strings.Replace(grouped, `"low"`, `"high"`, 1))},
		{name: "the first thought_level select in sent order is the target", options: selectOptionJSON("first", "thought_level", "low", "low") + "," + selectOptionJSON("second", "thought_level", "low", "low", "high"), effort: "high", want: warn("high", "not_offered", "option", "first", "offered", "low")},
		{name: "an offered entry is set before the first prompt", options: mode, entries: map[string]string{"mode": "plan"}, results: map[string]string{"mode": modeSet("plan")}, wantSet: "mode=plan"},
		{name: "an entry naming no select option", options: mode, entries: map[string]string{"modee": "plan"}, want: skip("modee", "plan", "no_option", "advertised", "mode")},
		{name: "entries naming a model or thought_level option send nothing", options: mode + "," + selectOptionJSON("model", "model", "a", "a", "b") + "," + selectOptionJSON("level", "thought_level", "low", "low", "high"), entries: map[string]string{"model": "b", "level": "high"}, want: slices.Concat(skip("level", "high", "no_option", "advertised", "mode"), skip("model", "b", "no_option", "advertised", "mode"))},
		{name: "an entry value the option does not offer", options: mode, entries: map[string]string{"mode": "fast"}, want: skip("mode", "fast", "not_offered", "offered", "default, plan")},
		{name: "the runtime answers an entry with an error", options: mode, entries: map[string]string{"mode": "plan"}, setError: true, wantSet: "mode=plan", want: skip("mode", "plan", "rejected", "agent_error", "-32602: bad value")},
		{name: "no answer to an entry within the read bound, then a late answer", options: mode, entries: map[string]string{"mode": "plan"}, hold: true, wantSet: "mode=plan", want: skip("mode", "plan", "no_answer", "read_timeout_ms", bound)},
		{name: "the result lists another current value for an entry", options: mode, entries: map[string]string{"mode": "plan"}, results: map[string]string{"mode": modeSet("default")}, wantSet: "mode=plan", want: skip("mode", "plan", "other_value", "current", "default")},
		{name: "the level is checked against the list the entry's answer returned and sent last", options: selectOptionJSON("effort", "thought_level", "low", "low") + "," + mode, effort: "high", entries: map[string]string{"mode": "plan"}, results: map[string]string{"mode": configOptions(selectOptionJSON("effort", "thought_level", "low", "low", "high") + "," + selectOptionJSON("mode", "mode", "plan", "default", "plan"))}, wantSet: "mode=plan,effort=high"},
		{name: "a continued session names only an unnamed runtime option holding a value", options: held, entries: map[string]string{"named": "a"}, results: map[string]string{"named": namedSet}, continued: true, wantSet: "named=a", want: heldRecord},
		{name: "a created session names no held option", options: held, entries: map[string]string{"named": "a"}, results: map[string]string{"named": namedSet}, wantSet: "named=a"},
		{name: "a continued session without a level keeps the held one", options: offers, continued: true, want: []agenttest.LogSpyEntry{logRecord(slog.LevelInfo, "continued session keeps the agent's reasoning level", "option", "effort", "current", "default")}},
		{name: "a continued session whose option holds no value", options: selectOptionJSON("effort", "thought_level", "", "low"), continued: true},
		{name: "a created session without a level", options: offers},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				spy := &agenttest.LogSpy{}
				runtime := &effortRuntime{options: tt.options, setResult: tt.setResult, entries: tt.entries, results: tt.results, setError: tt.setError, hold: tt.hold}

				err := runtime.run(t, slog.New(spy), tt.effort, false, tt.continued, 1)

				if err != nil {
					t.Fatalf("session with effort %q error = %v, want the first turn to run", tt.effort, err)
				}
				if got := strings.Join(runtime.sets, ","); got != tt.wantSet {
					t.Errorf("set_config_option requests for effort %q = %q, want %q", tt.effort, got, tt.wantSet)
				}
				got := slices.DeleteFunc(spy.Entries(), func(e agenttest.LogSpyEntry) bool {
					return !strings.HasPrefix(e.Msg, "reasoning level not applied") && !strings.HasPrefix(e.Msg, "session option not applied") && !strings.HasPrefix(e.Msg, "continued session keeps")
				})
				if fmt.Sprint(got) != fmt.Sprint(tt.want) {
					t.Errorf("log records for effort %q = %v, want %v", tt.effort, got, tt.want)
				}
			})
		})
	}
}

func assertStartRefusalMatches(t *testing.T, settings map[string]any, diag registry.ValidationDiag) {
	t.Helper()

	adapter, _ := NewClientProtocolAdapter()
	_, err := adapter.StartSession(t.Context(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-never-launched"},
		Settings:      settings,
	})

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrAgentNotFound {
		t.Fatalf("StartSession(%v) error = %v, want an %s settings refusal", settings, err, domain.ErrAgentNotFound)
	}
	if diag.Severity != string(registry.SeverityError) || agentErr.Message != diag.Message {
		t.Errorf("validate diagnostic (%s) %q, start refusal %q, want severity error and equal text", diag.Severity, diag.Message, agentErr.Message)
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		settings  map[string]any
		wantCheck string
	}{
		{"variant is not read", map[string]any{variantKey: "high"}, "agent-client-protocol.variant.not_read"},
		{"null variant is silent", map[string]any{variantKey: nil}, ""},
		{"config_options is not a mapping", map[string]any{"config_options": "agent"}, "agent-client-protocol.config_options.wrong_type"},
		{"a config_options entry is not a string", map[string]any{"config_options": map[string]any{"agent": 7}}, "agent-client-protocol.config_options.wrong_type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags := validateConfig(registry.AgentConfigFields{Kind: "agent-client-protocol", Passthrough: tt.settings})

			if tt.wantCheck == "" {
				if len(diags) != 0 {
					t.Fatalf("validateConfig(%v) = %+v, want no diagnostics", tt.settings, diags)
				}
				return
			}
			if len(diags) != 1 || diags[0].Check != tt.wantCheck {
				t.Fatalf("validateConfig(%v) = %+v, want one %s diagnostic", tt.settings, diags, tt.wantCheck)
			}
			if strings.HasSuffix(tt.wantCheck, "wrong_type") {
				assertStartRefusalMatches(t, tt.settings, diags[0])
			}
		})
	}
}

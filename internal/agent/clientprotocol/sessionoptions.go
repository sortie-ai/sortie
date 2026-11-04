package clientprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// selectOption decodes any select option from the raw payload the generated
// sessionConfigOption union keeps.
type selectOption struct {
	ID       sessionConfigId              `json:"id"`
	Category *sessionConfigOptionCategory `json:"category,omitempty"`
	sessionConfigSelect
}

const configOptionsKey = "config_options"

const configOptionsWrongType = "agent-client-protocol.config_options.wrong_type"

// configOptionsSetting reads the config_options map of the settings block:
// session option ids to the value ids to set. An absent or null key yields no
// entries; the first fault in id order is returned, so the verdict is stable
// across runs. A fault names the key and never a value.
func configOptionsSetting(settings map[string]any) (map[string]string, *settingFault) {
	raw, present := settings[configOptionsKey]
	if !present || raw == nil {
		return nil, nil
	}

	table, ok := raw.(map[string]any)
	if !ok {
		return nil, &settingFault{
			check:   configOptionsWrongType,
			message: "agent-client-protocol.config_options must be a mapping of session option ids to values",
		}
	}

	entries := make(map[string]string, len(table))
	for _, id := range slices.Sorted(maps.Keys(table)) {
		value, ok := table[id].(string)
		if !ok {
			return nil, &settingFault{
				check:   configOptionsWrongType,
				message: "agent-client-protocol.config_options." + id + " must be a string",
			}
		}
		entries[id] = value
	}
	return entries, nil
}

// setConfigOptionRequest is the value-id arm of session/set_config_option. The
// generator cannot emit the request, and the schema uses a type member only
// for boolean options, which this kind never sets.
type setConfigOptionRequest struct {
	SessionID sessionId            `json:"sessionId"`
	ConfigID  sessionConfigId      `json:"configId"`
	Value     sessionConfigValueId `json:"value"`
}

// selectReasoningOption returns the first select option, in sent order, whose
// category is thought_level. It never looks at the option id: ids are the
// runtime's choice and differ between runtimes.
func selectReasoningOption(options []sessionConfigOption) (selectOption, bool) {
	for _, option := range selectOptions(options) {
		if option.Category != nil && *option.Category == sessionConfigOptionCategoryThoughtLevel {
			return option, true
		}
	}
	return selectOption{}, false
}

func selectOptions(options []sessionConfigOption) []selectOption {
	var selects []selectOption
	for _, option := range options {
		if option.Type != sessionConfigOptionSelect {
			continue
		}
		var decoded selectOption
		if json.Unmarshal(option.Remainder, &decoded) != nil {
			continue
		}
		selects = append(selects, decoded)
	}
	return selects
}

// configOptionsOf reads the configOptions member of a session result. An
// absent, null or non-array member means no options, and an element that does
// not decode is skipped.
func configOptionsOf(result json.RawMessage) []sessionConfigOption {
	var envelope struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(result, &envelope) != nil {
		return nil
	}
	var elems []json.RawMessage
	if json.Unmarshal(envelope.ConfigOptions, &elems) != nil {
		return nil
	}
	options := make([]sessionConfigOption, 0, len(elems))
	for _, elem := range elems {
		var option sessionConfigOption
		if json.Unmarshal(elem, &option) == nil {
			options = append(options, option)
		}
	}
	return options
}

func offeredValues(option selectOption) []string {
	var values []string
	for _, entry := range option.Options.Ungrouped {
		values = append(values, string(entry.Value))
	}
	for _, group := range option.Options.Grouped {
		for _, entry := range group.Options {
			values = append(values, string(entry.Value))
		}
	}
	return values
}

// excludedFromEntries reports whether an option belongs to the model or the
// reasoning level, which an operator entry never sets.
func excludedFromEntries(option selectOption) bool {
	if option.Category == nil {
		return false
	}
	return *option.Category == sessionConfigOptionCategoryModel || *option.Category == sessionConfigOptionCategoryThoughtLevel
}

// nextEntryTarget returns the first select option, in sent order, that a
// pending entry names and an entry may set.
func nextEntryTarget(options []sessionConfigOption, pending map[string]string) (selectOption, bool) {
	for _, option := range selectOptions(options) {
		if _, named := pending[string(option.ID)]; named && !excludedFromEntries(option) {
			return option, true
		}
	}
	return selectOption{}, false
}

// advertised lists the ids of the select options an entry may name, in sent
// order, so an operator can correct a misspelled id.
func advertised(options []sessionConfigOption) string {
	var ids []string
	for _, option := range selectOptions(options) {
		if !excludedFromEntries(option) {
			ids = append(ids, string(option.ID))
		}
	}
	return strings.Join(ids, ", ")
}

// setSessionOption sets one select option to value and returns the option
// list in force afterwards. It is the only sender of session/set_config_option.
// Every outcome that leaves the value unapplied goes to report and returns the
// list unchanged; only a lost connection or the end of the start's own context
// returns an error.
func setSessionOption(ctx context.Context, state *sessionState, sessionID sessionId, option selectOption, value string, current []sessionConfigOption, report func(agentcore.NotAppliedReason, ...slog.Attr)) ([]sessionConfigOption, *domain.AgentError) {
	offered := offeredValues(option)
	if !slices.Contains(offered, value) {
		report(agentcore.NotAppliedNotOffered, slog.String("offered", strings.Join(offered, ", ")))
		return current, nil
	}

	bound := agentcore.ReadTimeout(state.agentConfig)
	callCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	req := setConfigOptionRequest{SessionID: sessionID, ConfigID: option.ID, Value: sessionConfigValueId(value)}
	resp, err := state.conn.Call(callCtx, methodSessionSetConfigOption, req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			report(agentcore.NotAppliedNoAnswer, slog.Int64("read_timeout_ms", bound.Milliseconds()))
			return current, nil
		}
		return current, translateCallError(state, err, callCtx, ctx)
	}
	if resp.Error != nil {
		report(agentcore.NotAppliedRejected, slog.String("agent_error", fmt.Sprintf("%d: %s", resp.Error.Code, quoteJSONRPCError(resp.Error))))
		return current, nil
	}

	updated := configOptionsOf(resp.Result)
	if updated == nil {
		return current, nil
	}
	for _, listed := range selectOptions(updated) {
		if listed.ID == option.ID && string(listed.CurrentValue) != value {
			report(agentcore.NotAppliedOtherValue, slog.String("current", string(listed.CurrentValue)))
			break
		}
	}
	return updated, nil
}

// logHeldOptions names the runtime-private options a continued session holds
// at a value no entry set. Only the runtime's own categories qualify: other
// options always hold a value and would log on every continuation.
func logHeldOptions(logger *slog.Logger, current []sessionConfigOption, entries map[string]string) {
	for _, option := range selectOptions(current) {
		if option.Category == nil || !strings.HasPrefix(string(*option.Category), "_") || option.CurrentValue == "" {
			continue
		}
		if _, named := entries[string(option.ID)]; named {
			continue
		}
		logger.Info("continued session keeps the agent's session option",
			slog.String("agent_kind", kindName),
			slog.String("option", string(option.ID)),
			slog.String("current", string(option.CurrentValue)),
		)
	}
}

// applySessionOptions sets every config_options entry and then effort, before
// the session's first prompt. Each answer's option list replaces the one
// before it for every later check, because setting one option can change what
// another lists. Every outcome that leaves a value unapplied is a warning;
// only a lost connection or the end of the start's own context returns an
// error.
func applySessionOptions(ctx context.Context, state *sessionState, effort string, sessionID sessionId, opened json.RawMessage, continued bool) *domain.AgentError {
	logger := logging.WithSession(state.logger, string(sessionID))
	current := configOptionsOf(opened)
	pending := maps.Clone(state.configOptions)

	for {
		option, found := nextEntryTarget(current, pending)
		if !found {
			break
		}
		id, value := string(option.ID), pending[string(option.ID)]
		delete(pending, id)

		var err *domain.AgentError
		current, err = setSessionOption(ctx, state, sessionID, option, value, current, func(reason agentcore.NotAppliedReason, detail ...slog.Attr) {
			agentcore.WarnSessionOptionNotApplied(logger, kindName, id, value, reason, detail...)
		})
		if err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(pending)) {
		agentcore.WarnSessionOptionNotApplied(logger, kindName, id, pending[id], agentcore.NotAppliedNoOption,
			slog.String("advertised", advertised(current)))
	}

	if continued {
		logHeldOptions(logger, current, state.configOptions)
	}

	target, found := selectReasoningOption(current)
	if effort == "" {
		if continued && found && target.CurrentValue != "" {
			logger.Info("continued session keeps the agent's reasoning level",
				slog.String("agent_kind", kindName),
				slog.String("option", string(target.ID)),
				slog.String("current", string(target.CurrentValue)),
			)
		}
		return nil
	}

	reportEffort := func(reason agentcore.NotAppliedReason, detail ...slog.Attr) {
		if reason != agentcore.NotAppliedNoOption {
			detail = append([]slog.Attr{slog.String("option", string(target.ID))}, detail...)
		}
		agentcore.WarnEffortNotApplied(logger, kindName, effort, reason, detail...)
	}
	if !found {
		reportEffort(agentcore.NotAppliedNoOption, slog.String("advertised", advertised(current)))
		return nil
	}
	_, err := setSessionOption(ctx, state, sessionID, target, effort, current, reportEffort)
	return err
}

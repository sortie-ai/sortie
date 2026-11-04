package clientprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// reasoningOption decodes one select option from the raw payload the
// generated sessionConfigOption union keeps.
type reasoningOption struct {
	ID       sessionConfigId              `json:"id"`
	Category *sessionConfigOptionCategory `json:"category,omitempty"`
	sessionConfigSelect
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
func selectReasoningOption(options []sessionConfigOption) (reasoningOption, bool) {
	for _, option := range selectOptions(options) {
		if option.Category != nil && *option.Category == sessionConfigOptionCategoryThoughtLevel {
			return option, true
		}
	}
	return reasoningOption{}, false
}

func selectOptions(options []sessionConfigOption) []reasoningOption {
	var selects []reasoningOption
	for _, option := range options {
		if option.Type != sessionConfigOptionSelect {
			continue
		}
		var decoded reasoningOption
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

func offeredValues(option reasoningOption) []string {
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

// applyReasoningLevel sets state.effort on the session's reasoning option once,
// before the session's first prompt. Every outcome that leaves the level
// unapplied is a warning and returns nil; only a lost connection or the end of
// the start's own context returns an error.
func applyReasoningLevel(ctx context.Context, state *sessionState, sessionID sessionId, opened json.RawMessage, continued bool) *domain.AgentError {
	if state.effort == "" && !continued {
		return nil
	}

	logger := logging.WithSession(state.logger, string(sessionID))
	option, found := selectReasoningOption(configOptionsOf(opened))
	if state.effort == "" {
		if found && option.CurrentValue != "" {
			logger.Info("continued session keeps the agent's reasoning level",
				slog.String("agent_kind", kindName),
				slog.String("option", string(option.ID)),
				slog.String("current", string(option.CurrentValue)),
			)
		}
		return nil
	}

	if !found {
		agentcore.WarnEffortNotApplied(logger, kindName, state.effort, agentcore.EffortNoOption)
		return nil
	}

	offered := offeredValues(option)
	if !slices.Contains(offered, state.effort) {
		agentcore.WarnEffortNotApplied(logger, kindName, state.effort, agentcore.EffortNotOffered,
			slog.String("option", string(option.ID)),
			slog.String("offered", strings.Join(offered, ", ")),
		)
		return nil
	}

	bound := agentcore.ReadTimeout(state.agentConfig)
	callCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	req := setConfigOptionRequest{SessionID: sessionID, ConfigID: option.ID, Value: sessionConfigValueId(state.effort)}
	resp, err := state.conn.Call(callCtx, methodSessionSetConfigOption, req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			agentcore.WarnEffortNotApplied(logger, kindName, state.effort, agentcore.EffortNoAnswer,
				slog.String("option", string(option.ID)),
				slog.Int64("read_timeout_ms", bound.Milliseconds()),
			)
			return nil
		}
		return translateCallError(state, err, callCtx, ctx)
	}
	if resp.Error != nil {
		agentcore.WarnEffortNotApplied(logger, kindName, state.effort, agentcore.EffortRejected,
			slog.String("option", string(option.ID)),
			slog.String("agent_error", fmt.Sprintf("%d: %s", resp.Error.Code, quoteJSONRPCError(resp.Error))),
		)
		return nil
	}

	for _, updated := range selectOptions(configOptionsOf(resp.Result)) {
		if updated.ID == option.ID && updated.CurrentValue != "" && string(updated.CurrentValue) != state.effort {
			agentcore.WarnEffortNotApplied(logger, kindName, state.effort, agentcore.EffortOtherValue,
				slog.String("option", string(option.ID)),
				slog.String("current", string(updated.CurrentValue)),
			)
			break
		}
	}
	return nil
}

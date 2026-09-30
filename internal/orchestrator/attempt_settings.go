package orchestrator

import (
	"path/filepath"
	"strconv"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// AttemptSettings is what one attempt reads from the configuration in
// force at its start.
type AttemptSettings struct {
	Settings         config.AgentSettings
	UsageArrival     registry.UsageArrival
	UsageAttribution registry.UsageAttribution

	// Refusals are the error-severity checks the block fails; an attempt
	// with any starts no session.
	Refusals []PreflightError
}

// resolveAttemptSettings resolves the settings an attempt of selection
// runs with from cfg; sshHost is empty for a local launch. Call it once
// per attempt on the event loop, so a reload reaches a claim already held.
func (o *Orchestrator) resolveAttemptSettings(cfg config.ServiceConfig, selection DispatchResolution, sshHost string) AttemptSettings {
	settings := config.ResolveAgentSettings(cfg,
		config.SettingsSelection{Kind: selection.AgentKind, RuleName: selection.RuleName},
		filepath.Dir(o.workflowManager.WorkflowAbsPath()))
	meta, registered := o.preflightParams.AgentRegistry.Meta(selection.AgentKind)

	arrival, attribution := registry.UsageArrivalUndeclared, registry.UsageAttributionUndeclared
	if registered {
		arrival, attribution = meta.UsageDisposition(settings.Passthrough, sshHost != "")
	}
	refusals, _ := settingsDiagnostics(cfg, meta, registered, settings, sshHost != "")

	return AttemptSettings{
		Settings:         settings,
		UsageArrival:     arrival,
		UsageAttribution: attribution,
		Refusals:         refusals,
	}
}

// settingsDiagnostics runs every check that reads a resolved settings
// block. Preflight and every attempt share it, so they cannot disagree
// about a block. An unregistered kind draws nothing.
func settingsDiagnostics(cfg config.ServiceConfig, meta registry.AgentMeta, registered bool, settings config.AgentSettings, remote bool) ([]PreflightError, []PreflightWarning) {
	if !registered {
		return nil, nil
	}

	var errs []PreflightError
	var warns []PreflightWarning
	kind := settings.Kind

	if meta.MCPInjection == registry.MCPInjectionUnsupported && settings.MCPConfigPath != "" {
		warns = append(warns, PreflightWarning{
			Check:   "agent.mcp_config",
			Message: "mcp_config in the " + strconv.Quote(kind) + " block cannot reach the agent: this agent kind receives no MCP configuration",
		})
	}

	arrival, _ := meta.UsageDisposition(settings.Passthrough, remote)
	if !arrival.ReportsAnyFigure() {
		if cfg.Agent.MaxTokens != 0 {
			warns = append(warns, PreflightWarning{
				Check: "agent.kind.no_usage_reporting",
				Message: "agent.max_tokens is set but agent kind " + strconv.Quote(kind) +
					" reports no token usage for the sessions this configuration produces: the per-issue token ceiling can never be reached for it",
			})
		}
		if tokenRatesPrice(cfg, kind) {
			warns = append(warns, PreflightWarning{
				Check: "agent.kind.no_cost_estimate",
				Message: "token_rates prices agent kind " + strconv.Quote(kind) +
					", which reports no token usage for the sessions this configuration produces: no cost can be estimated for it",
			})
		}
	}

	// Sortie re-dispatches an issue with its earlier session after every
	// retry, continuation, stall, or restart, so a kind that cannot
	// resume fails every such turn.
	if meta.SessionResumeBlockedBy != nil {
		if key := meta.SessionResumeBlockedBy(settings.Passthrough); key != "" {
			errs = append(errs, PreflightError{
				Check: "agent.kind.session_resume",
				Message: kind + "." + key + " stops this agent kind from resuming a session across separate agent launches, " +
					"but Sortie re-dispatches an issue with its earlier session after a retry, a continuation, a stall, or a restart, " +
					"and every such turn fails. Change " + kind + "." + key + ", or use an agent kind that can resume a session.",
			})
		}
	}

	if meta.ValidateAgentConfig == nil {
		return errs, warns
	}
	fields := registry.AgentConfigFields{
		Kind:        kind,
		Passthrough: settings.Passthrough,
	}
	for _, d := range meta.ValidateAgentConfig(fields) {
		switch d.Severity {
		case "warning":
			warns = append(warns, PreflightWarning{Check: d.Check, Message: d.Message})
		default:
			errs = append(errs, PreflightError{Check: d.Check, Message: d.Message})
		}
	}
	return errs, warns
}

// tokenRatesPrice reports whether token_rates prices kind. It reads only
// the key set because the rate values belong to internal/server, which
// this package must not import.
func tokenRatesPrice(cfg config.ServiceConfig, kind string) bool {
	raw, present := cfg.ExtensionValue("token_rates")
	if !present {
		return false
	}
	rates, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	_, priced := rates[kind]
	return priced
}

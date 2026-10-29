package orchestrator

import (
	"cmp"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/maputil"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/registry"
)

// PreflightFailedPrefix leads the text of a failed [PreflightResult].
const PreflightFailedPrefix = "dispatch preflight failed: "

// preflightCheckWorkflowLoad is the [PreflightError.Check] of a workflow file
// that cannot be loaded.
const preflightCheckWorkflowLoad = "workflow_load"

// PreflightError represents a single preflight validation failure.
type PreflightError struct {
	// Check identifies which validation check failed. Known values:
	// "workflow_load", "tracker.kind", "tracker.api_key",
	// "tracker.project", "tracker_adapter", "tracker.handoff_state",
	// "tracker.in_progress_state", "agent.kind", "agent.command",
	// "agent_adapter", "workspace.root_writable",
	// "agent.kind.session_resume", "dispatch.agent.missing_block",
	// "dispatch.stage.collision".
	Check string

	// Message is an operator-friendly description of the failure.
	Message string
}

// PreflightWarning represents a non-fatal advisory diagnostic from
// preflight validation. Warnings do not block dispatch.
type PreflightWarning struct {
	// Check identifies which validation produced the warning.
	Check string

	// Message is an operator-friendly description of the advisory.
	Message string
}

// PreflightResult holds the outcome of dispatch preflight validation.
type PreflightResult struct {
	// Errors contains all validation failures found. Empty slice when
	// validation passes.
	Errors []PreflightError

	// Warnings contains non-fatal advisory diagnostics from preflight
	// validation. Warnings do not affect [PreflightResult.OK].
	Warnings []PreflightWarning
}

// OK reports whether preflight validation passed (no errors).
func (r PreflightResult) OK() bool {
	return len(r.Errors) == 0
}

// Error returns a combined human-readable diagnostic of all preflight
// failures. Returns empty string when OK.
func (r PreflightResult) Error() string {
	if r.OK() {
		return ""
	}
	msgs := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		msgs[i] = e.Message
	}
	return PreflightFailedPrefix + strings.Join(msgs, "; ")
}

// PreflightParams holds the dependencies for preflight validation.
// The orchestrator constructs this once at startup and reuses it on
// each tick.
type PreflightParams struct {
	// ReloadWorkflow triggers a defensive re-read of the workflow
	// file. Returns an error if the file cannot be loaded or parsed.
	ReloadWorkflow func() error

	// ConfigFunc returns the current effective config after any
	// successful reload.
	ConfigFunc func() config.ServiceConfig

	// TrackerRegistry provides adapter lookup and metadata queries
	// for the configured tracker kind.
	TrackerRegistry interface {
		Get(kind string) (registry.TrackerConstructor, error)
		Meta(kind string) (registry.TrackerMeta, bool)
	}

	// AgentRegistry provides adapter lookup and metadata queries
	// for the configured agent kind.
	AgentRegistry interface {
		Get(kind string) (registry.AgentConstructor, error)
		Meta(kind string) (registry.AgentMeta, bool)
	}

	// RatesFunc returns the price rates in force. A nil func reads as no
	// rates.
	RatesFunc func() *pricing.Rates
}

// ValidateDispatchConfig runs all dispatch preflight checks.
// Config-level errors are collected (not short-circuited) so the
// operator sees every problem at once. The one exception is a
// workflow reload failure: if the workflow file cannot be loaded,
// the function returns immediately because the remaining checks
// would evaluate stale or default config.
func ValidateDispatchConfig(params PreflightParams) PreflightResult {
	var errs []PreflightError

	// Workflow file must be loadable and parseable. When the reload
	// fails, remaining checks are skipped because ConfigFunc would
	// return stale (or default) config, making those results
	// misleading. The operator must fix the workflow file first.
	if err := params.ReloadWorkflow(); err != nil {
		errs = append(errs, PreflightError{
			Check:   preflightCheckWorkflowLoad,
			Message: "workflow file cannot be loaded: " + err.Error(),
		})
		return PreflightResult{Errors: errs}
	}

	cfg := params.ConfigFunc()

	// Tracker kind must be set for any subsequent tracker validation.
	if cfg.Tracker.Kind == "" {
		errs = append(errs, PreflightError{
			Check:   "tracker.kind",
			Message: "tracker.kind is required",
		})
	}

	// Tracker-specific validations share a single Meta() lookup.
	var warns []PreflightWarning
	var trackerMeta registry.TrackerMeta
	if cfg.Tracker.Kind != "" {
		trackerMeta, _ = params.TrackerRegistry.Meta(cfg.Tracker.Kind)

		// API key is mandatory for adapters that declare it required.
		if trackerMeta.RequiresAPIKey && cfg.Tracker.APIKey == "" {
			errs = append(errs, PreflightError{
				Check: "tracker.api_key",
				Message: "tracker.api_key is required for tracker kind " + strconv.Quote(cfg.Tracker.Kind) +
					" (value may be empty after environment variable expansion)",
			})
		}

		// Project is mandatory for adapters that declare it required.
		if trackerMeta.RequiresProject && cfg.Tracker.Project == "" {
			errs = append(errs, PreflightError{
				Check:   "tracker.project",
				Message: "tracker.project is required for tracker kind " + strconv.Quote(cfg.Tracker.Kind),
			})
		}

		// Tracker adapter must be registered in the registry.
		if _, err := params.TrackerRegistry.Get(cfg.Tracker.Kind); err != nil {
			errs = append(errs, PreflightError{
				Check:   "tracker_adapter",
				Message: err.Error(),
			})
		}

		// Adapter-specific tracker config validation, if provided.
		if trackerMeta.ValidateTrackerConfig != nil {
			fields := registry.TrackerConfigFields{
				Kind:            cfg.Tracker.Kind,
				Project:         cfg.Tracker.Project,
				Endpoint:        cfg.Tracker.Endpoint,
				APIKey:          cfg.Tracker.APIKey,
				ActiveStates:    cfg.Tracker.ActiveStates,
				TerminalStates:  cfg.Tracker.TerminalStates,
				HandoffState:    cfg.Tracker.HandoffState,
				InProgressState: cfg.Tracker.InProgressState,
				QueryFilter:     cfg.Tracker.QueryFilter,
				APIVersion:      cfg.Tracker.APIVersion,
			}
			for _, d := range trackerMeta.ValidateTrackerConfig(fields) {
				switch d.Severity {
				case "warning":
					warns = append(warns, PreflightWarning{Check: d.Check, Message: d.Message})
				default:
					errs = append(errs, PreflightError{Check: d.Check, Message: d.Message})
				}
			}
		}
	}

	if len(cfg.Tracker.ActiveStates) == 0 {
		warns = append(warns, PreflightWarning{
			Check:   "tracker.active_states.empty",
			Message: "tracker.active_states is empty, so no issue is dispatched",
		})
	}

	// Agent kind must be set for any subsequent agent validation.
	if cfg.Agent.Kind == "" {
		errs = append(errs, PreflightError{
			Check:   "agent.kind",
			Message: "agent.kind is required",
		})
	}

	// Agent adapter must be registered in the registry.
	if cfg.Agent.Kind != "" {
		if _, err := params.AgentRegistry.Get(cfg.Agent.Kind); err != nil {
			errs = append(errs, PreflightError{
				Check:   "agent_adapter",
				Message: err.Error(),
			})
		}
	}

	// worker.ssh_hosts is the one configuration key that can produce a
	// remote session, so its presence decides whether a kind can carry a
	// token figure for every session this configuration can produce.
	remote := len(ParseWorkerConfig(cfg.ExtensionSection("worker"), cfg.ExtensionEnvRefPaths("worker")).SSHHosts) > 0

	// Adapter-specific agent config validation, for every distinct kind
	// this configuration can reach. A registered kind the configuration
	// never references is skipped, because that would report a fault in
	// a block no run reads.
	selectors := agentKindSelectors(cfg)
	for _, ref := range orderedUniqueAgentKinds(cfg) {
		agentMeta, registered := params.AgentRegistry.Meta(ref.Kind)
		settings := config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: ref.Kind}, "")

		if registered && agentMeta.RequiresCommand && !launchesExecutable(cfg, agentMeta, ref.Kind, remote) {
			errs = append(errs, PreflightError{
				Check:   "agent.command",
				Message: missingCommandMessage(ref, cmp.Or(cfg.Dispatch.Default.AgentKind, cfg.Agent.Kind)),
			})
		}

		// A kind the dispatch block introduces, rather than the
		// workflow-wide default, must carry its own settings block.
		// First-occurrence deduplication in orderedUniqueAgentKinds
		// already resolves the "differs from cfg.Agent.Kind"
		// comparison into the origin, so no second kind comparison
		// belongs here.
		//
		// A kind selected only by rules that carry its block needs no
		// top-level block: no attempt reads it alone.
		blockOnly := false
		if registered && ref.Origin != agentKindOriginDefault {
			uncovered, hasUncovered := firstSelectorWithoutBlock(selectors, ref.Kind)
			blockOnly = !hasUncovered
			switch settings.BlockPresence {
			case config.ExtensionBlockAbsent:
				if hasUncovered {
					errs = append(errs, PreflightError{
						Check:   "dispatch.agent.missing_block",
						Message: missingBlockAbsentMessage(uncovered),
					})
				}
			case config.ExtensionBlockNotAMapping:
				errs = append(errs, PreflightError{
					Check:   "dispatch.agent.missing_block",
					Message: missingBlockNotAMappingMessage(ref, settings.BlockDescription),
				})
			}
		}

		// A kind that delivers no tool channel can neither call nor be told
		// about Sortie's tools.
		if registered && !agentMeta.MCPDialect.DeliversTools() {
			warns = append(warns, PreflightWarning{
				Check:   "agent.kind.no_tool_channel",
				Message: "agent kind " + strconv.Quote(ref.Kind) + " has no tool execution channel: Sortie's tools are neither advertised nor callable for it",
			})
		}

		if blockOnly {
			continue
		}
		settingsErrs, settingsWarns := settingsDiagnostics(cfg, agentMeta, registered, settings, remote)
		errs = append(errs, settingsErrs...)
		warns = append(warns, settingsWarns...)
	}

	for i, rule := range cfg.Dispatch.Rules {
		if rule.SettingsKind == "" {
			continue
		}
		agentMeta, registered := params.AgentRegistry.Meta(rule.SettingsKind)
		settings := config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: rule.SettingsKind, RuleName: rule.Name}, "")
		prefix := ruleSettingsPrefix(i, rule)

		ruleErrs, ruleWarns := settingsDiagnostics(cfg, agentMeta, registered, settings, remote)
		for _, e := range ruleErrs {
			errs = append(errs, PreflightError{Check: e.Check, Message: prefix + e.Message})
		}
		for _, w := range ruleWarns {
			warns = append(warns, PreflightWarning{Check: w.Check, Message: prefix + w.Message})
		}
	}

	errs = append(errs, stageCollisions(cfg)...)

	if settings := moneyCeilingSettings(cfg.Agent); len(settings) > 0 && !ratesInForce(params).SourceConfigured() {
		for _, setting := range settings {
			warns = append(warns, PreflightWarning{
				Check: setting.Key + ".no_rate_source",
				Message: setting.Key + " is set but no price source is configured: price_table.embedded and price_table.fetch are off " +
					"and no token_rates entry or operator price table is set, so no run can be priced",
			})
		}
	}

	// Workspace root must exist and be writable.
	if cfg.Workspace.Root != "" {
		if err := checkWorkspaceRootWritable(cfg.Workspace.Root); err != nil {
			errs = append(errs, PreflightError{
				Check:   "workspace.root_writable",
				Message: "workspace.root is not writable: " + cfg.Workspace.Root + ": " + err.Error(),
			})
		}
	}

	return PreflightResult{Errors: errs, Warnings: warns}
}

// moneyCeilingSetting is one money setting above 0 with the consequence of
// its kind reporting no token usage.
type moneyCeilingSetting struct {
	Key     string
	NoUsage string
}

// moneyCeilingSettings returns each money setting of agent above 0, in the
// order per-issue, daily, weekly, monthly.
func moneyCeilingSettings(agent config.AgentConfig) []moneyCeilingSetting {
	var settings []moneyCeilingSetting
	if agent.MaxCostUSD > 0 {
		settings = append(settings, moneyCeilingSetting{
			Key:     "agent.max_cost_usd",
			NoUsage: "the per-issue money ceiling can never be reached for it",
		})
	}
	for _, def := range spendWindowDefs {
		if def.ceiling(agent) > 0 {
			settings = append(settings, moneyCeilingSetting{
				Key:     def.setting,
				NoUsage: "the " + string(def.window) + " spend ceiling counts none of its spend",
			})
		}
	}
	return settings
}

// ratesInForce returns the rates params carries; a nil RatesFunc reads as
// no rates.
func ratesInForce(params PreflightParams) *pricing.Rates {
	if params.RatesFunc == nil {
		return nil
	}
	return params.RatesFunc()
}

// agentKindOrigin identifies the front-matter field that first
// introduced an agentKindRef's kind, under the first-occurrence
// deduplication orderedUniqueAgentKinds applies.
type agentKindOrigin int

const (
	agentKindOriginDefault         agentKindOrigin = iota // cfg.Agent.Kind
	agentKindOriginDispatchDefault                        // cfg.Dispatch.Default.AgentKind
	agentKindOriginRule                                   // cfg.Dispatch.Rules[i].Selection.AgentKind
)

// agentKindRef is one entry of orderedUniqueAgentKinds's enumeration,
// carrying the front-matter field that first introduced Kind.
// RuleIndex and RuleName are meaningful only when Origin is
// agentKindOriginRule.
type agentKindRef struct {
	Kind      string
	Origin    agentKindOrigin
	RuleIndex int
	RuleName  string
}

// orderedUniqueAgentKinds returns the agent kinds cfg can reach, in
// deterministic order: the default cfg.Agent.Kind first, then
// cfg.Dispatch.Default.AgentKind, then each
// cfg.Dispatch.Rules[i].Selection.AgentKind in rule order. Empty
// strings are skipped and duplicates are removed, keeping each kind's
// first occurrence, so a kind that is both the workflow default and
// later named by a rule carries agentKindOriginDefault.
func orderedUniqueAgentKinds(cfg config.ServiceConfig) []agentKindRef {
	seen := make(map[string]bool)
	var refs []agentKindRef

	add := func(kind string, origin agentKindOrigin, ruleIndex int, ruleName string) {
		if kind == "" || seen[kind] {
			return
		}
		seen[kind] = true
		refs = append(refs, agentKindRef{Kind: kind, Origin: origin, RuleIndex: ruleIndex, RuleName: ruleName})
	}

	add(cfg.Agent.Kind, agentKindOriginDefault, 0, "")
	add(cfg.Dispatch.Default.AgentKind, agentKindOriginDispatchDefault, 0, "")
	for i, rule := range cfg.Dispatch.Rules {
		add(rule.Selection.AgentKind, agentKindOriginRule, i, rule.Name)
	}

	return refs
}

// ruleSettingsPrefix opens every message about the settings block of
// rules[index].
func ruleSettingsPrefix(index int, rule config.DispatchRule) string {
	return "dispatch rule " + strconv.Quote(rule.Name) + " (dispatch.rules[" + strconv.Itoa(index) + "]." + rule.SettingsKind + "): "
}

// stageCollisionSource is one value a stage label must not equal, with
// the words that name it in a diagnostic.
type stageCollisionSource struct {
	value     string
	rendering string
}

// stageCollisions returns one error per staged rule and per source value
// that equals its stage label, rules in list order and sources in the
// order stageCollisionSources lists them. It reads configuration and
// registry metadata only, so it runs without a tracker call.
func stageCollisions(cfg config.ServiceConfig) []PreflightError {
	var sources []stageCollisionSource
	var errs []PreflightError
	for i, rule := range cfg.Dispatch.Rules {
		if rule.Stage == "" {
			continue
		}
		if sources == nil {
			sources = stageCollisionSources(cfg)
		}
		for _, source := range sources {
			if !config.StageLabelsEqual(rule.Stage, source.value) {
				continue
			}
			errs = append(errs, PreflightError{
				Check: "dispatch.stage.collision",
				Message: "dispatch rule " + strconv.Quote(rule.Name) + " (dispatch.rules[" + strconv.Itoa(i) + "].stage): stage label " +
					strconv.Quote(rule.Stage) + " collides with " + source.rendering +
					"; a stage label must not equal a tracker state or a label Sortie applies to issues",
			})
		}
	}
	return errs
}

// stageCollisionSources lists every value a stage label must not equal:
// the tracker states as written, then the labels Sortie applies to issues.
func stageCollisionSources(cfg config.ServiceConfig) []stageCollisionSource {
	tc := cfg.Tracker
	var sources []stageCollisionSource
	sources = append(sources, stateListSources("active", tc.ActiveStates)...)
	sources = append(sources, stateListSources("terminal", tc.TerminalStates)...)
	for _, named := range []struct{ key, value string }{
		{"tracker.handoff_state", tc.HandoffState},
		{"tracker.in_progress_state", tc.InProgressState},
		{"tracker.no_change_state", tc.NoChangeState},
	} {
		if named.value != "" {
			sources = append(sources, stageCollisionSource{value: named.value, rendering: named.key + " " + strconv.Quote(named.value)})
		}
	}

	escalationLabels := make(map[string]string, len(cfg.Reactions)+1)
	for key, reaction := range cfg.Reactions {
		escalationLabels[key] = reaction.EscalationLabel
	}
	if cfg.CIFeedback.EscalationLabel != "" {
		escalationLabels["ci_failure"] = cfg.CIFeedback.EscalationLabel
	}
	for _, key := range maputil.SortedKeys(escalationLabels) {
		if label := escalationLabels[key]; label != "" {
			sources = append(sources, stageCollisionSource{value: label, rendering: "reactions." + key + ".escalation_label " + strconv.Quote(label)})
		}
	}

	parking := resolveHandoffParkingLabel(cfg.Reactions)
	return append(sources, stageCollisionSource{value: parking, rendering: "the parking label " + strconv.Quote(parking)})
}

// stateListSources names each state of a written list. noun is "active"
// or "terminal".
func stateListSources(noun string, states []string) []stageCollisionSource {
	sources := make([]stageCollisionSource, len(states))
	for i, state := range states {
		sources[i] = stageCollisionSource{value: state, rendering: noun + " state " + strconv.Quote(state)}
	}
	return sources
}

// agentKindSelector is a dispatch.default.agent or rule agent selection;
// carriesBlock marks a rule holding a settings block for its kind.
type agentKindSelector struct {
	ref          agentKindRef
	carriesBlock bool
}

// agentKindSelectors lists every non-empty selector, default first and
// then rules in order, without [orderedUniqueAgentKinds]'s deduplication.
func agentKindSelectors(cfg config.ServiceConfig) []agentKindSelector {
	var selectors []agentKindSelector
	if kind := cfg.Dispatch.Default.AgentKind; kind != "" {
		selectors = append(selectors, agentKindSelector{ref: agentKindRef{Kind: kind, Origin: agentKindOriginDispatchDefault}})
	}
	for i, rule := range cfg.Dispatch.Rules {
		kind := rule.Selection.AgentKind
		if kind == "" {
			continue
		}
		selectors = append(selectors, agentKindSelector{
			ref:          agentKindRef{Kind: kind, Origin: agentKindOriginRule, RuleIndex: i, RuleName: rule.Name},
			carriesBlock: rule.SettingsKind == kind,
		})
	}
	return selectors
}

// firstSelectorWithoutBlock returns the first selector of kind that is not
// a rule carrying its block.
func firstSelectorWithoutBlock(selectors []agentKindSelector, kind string) (agentKindRef, bool) {
	for _, selector := range selectors {
		if selector.ref.Kind == kind && !selector.carriesBlock {
			return selector.ref, true
		}
	}
	return agentKindRef{}, false
}

// agentKindOriginPrefix names the front-matter field that introduced
// ref.Kind, in the wording the dispatch.agent.missing_block and
// agent.command messages use to point the operator at the exact
// selector.
func agentKindOriginPrefix(ref agentKindRef) string {
	switch ref.Origin {
	case agentKindOriginRule:
		field := "dispatch.rules[" + strconv.Itoa(ref.RuleIndex) + "].agent"
		if ref.RuleName != "" {
			return "dispatch rule " + strconv.Quote(ref.RuleName) + " (" + field + ")"
		}
		return field
	case agentKindOriginDispatchDefault:
		return "dispatch.default.agent"
	default:
		return "agent.kind"
	}
}

// launchesExecutable reports whether a session of kind, in the given
// launch mode, would launch a process: its own command names an
// executable, or the kind has a default command.
func launchesExecutable(cfg config.ServiceConfig, meta registry.AgentMeta, kind string, remote bool) bool {
	return cfg.AgentCommand(kind, remote).NamesExecutable() || domain.AgentCommand{Line: meta.DefaultCommand}.NamesExecutable()
}

// missingCommandMessage renders the agent.command message for a kind
// that would launch no executable. Only the default agent kind reads
// agent.command, so any other kind is pointed at its selector.
func missingCommandMessage(ref agentKindRef, defaultKind string) string {
	kind := strconv.Quote(ref.Kind)
	if ref.Kind == defaultKind {
		return "agent.command is required for agent kind " + kind
	}
	return agentKindOriginPrefix(ref) + " selects agent kind " + kind +
		", which has no default command and launches agent.command only as the default agent kind (dispatch.default.agent, else agent.kind)"
}

// missingBlockAbsentMessage renders the dispatch.agent.missing_block
// message for a covered kind whose settings block is absent from the
// workflow front matter.
func missingBlockAbsentMessage(ref agentKindRef) string {
	kind := strconv.Quote(ref.Kind)
	return agentKindOriginPrefix(ref) + " selects agent kind " + kind +
		", but the workflow front matter carries no " + kind + " settings block; " +
		"add a top-level " + strconv.Quote(ref.Kind+":") + " block for that kind, or write " + strconv.Quote(ref.Kind+": {}")
}

// missingBlockNotAMappingMessage renders the dispatch.agent.missing_block
// message for a covered kind whose settings block is present but is
// not a YAML mapping. found names the found value's shape, in the
// fixed vocabulary [config.AgentSettings.BlockDescription] carries.
func missingBlockNotAMappingMessage(ref agentKindRef, found string) string {
	kind := strconv.Quote(ref.Kind)
	return agentKindOriginPrefix(ref) + " selects agent kind " + kind +
		", but the " + kind + " key in the workflow front matter holds " + found +
		" where a block of settings was expected; write the kind's settings as keys under " + strconv.Quote(ref.Kind+":") +
		", or write " + strconv.Quote(ref.Kind+": {}")
}

// checkWorkspaceRootWritable verifies that root exists (creating it
// if necessary) and is writable by creating and removing a temporary
// file. Returns nil on success.
func checkWorkspaceRootWritable(root string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(absRoot, 0o750); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(absRoot, ".sortie-preflight-*")
	if err != nil {
		return err
	}
	defer tmpFile.Close()           //nolint:errcheck // best-effort cleanup in defer
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // best-effort cleanup in defer

	return nil
}

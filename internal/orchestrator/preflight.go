package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// PreflightError represents a single preflight validation failure.
type PreflightError struct {
	// Check identifies which validation check failed. Known values:
	// "workflow_load", "tracker.kind", "tracker.api_key",
	// "tracker.project", "tracker_adapter", "tracker.handoff_state",
	// "tracker.in_progress_state", "agent.kind", "agent.command",
	// "agent_adapter", "workspace.root_writable".
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
	return "dispatch preflight failed: " + strings.Join(msgs, "; ")
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
			Check:   "workflow_load",
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
	if cfg.Tracker.Kind != "" {
		trackerMeta, _ := params.TrackerRegistry.Meta(cfg.Tracker.Kind)

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

		// The config loader rules on the state lists as written. A list the
		// tracker adapter fills from its own fallback needs the same rules
		// run against the lists the run actually uses.
		errs = append(errs, validateDefaultedTrackerStates(cfg.Tracker, trackerMeta)...)
	}

	// Agent kind must be set for any subsequent agent validation.
	if cfg.Agent.Kind == "" {
		errs = append(errs, PreflightError{
			Check:   "agent.kind",
			Message: "agent.kind is required",
		})
	}

	// Command is mandatory for adapters that declare it required.
	if cfg.Agent.Kind != "" {
		agentMeta, _ := params.AgentRegistry.Meta(cfg.Agent.Kind)
		if agentMeta.RequiresCommand && cfg.Agent.Command == "" {
			errs = append(errs, PreflightError{
				Check:   "agent.command",
				Message: "agent.command is required for agent kind " + strconv.Quote(cfg.Agent.Kind),
			})
		}
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

// validateDefaultedTrackerStates reports the state collisions that
// become visible only after the tracker adapter's default state lists
// fill an empty tracker.active_states or tracker.terminal_states.
//
// Both arguments are read-only. The result is nil when no list was
// defaulted, when the defaulted list produces no collision, or when the
// written lists already violate a rule, in which case the config loader
// reports the fault under its own field key.
func validateDefaultedTrackerStates(tc config.TrackerConfig, meta registry.TrackerMeta) []PreflightError {
	activeDefaulted := len(tc.ActiveStates) == 0 && len(meta.DefaultActiveStates) > 0
	terminalDefaulted := len(tc.TerminalStates) == 0 && len(meta.DefaultTerminalStates) > 0
	if !activeDefaulted && !terminalDefaulted {
		return nil
	}

	// A collision the written lists already carry has its own diagnostic from
	// the config loader. Returning here keeps every diagnostic below
	// attributable to one empty front-matter key.
	if config.ValidateHandoffState(tc.HandoffState, tc.ActiveStates, tc.TerminalStates) != nil {
		return nil
	}
	if config.ValidateInProgressState(tc.InProgressState, tc.ActiveStates, tc.TerminalStates, tc.HandoffState) != nil {
		return nil
	}

	effectiveActive := tc.ActiveStates
	if activeDefaulted {
		effectiveActive = meta.DefaultActiveStates
	}
	effectiveTerminal := tc.TerminalStates
	if terminalDefaulted {
		effectiveTerminal = meta.DefaultTerminalStates
	}

	var errs []PreflightError

	// The handoff rule runs once per list, each call passing the other list as
	// nil, so a violation names the one list whose emptiness the fallback
	// filled instead of leaving the operator to guess.
	if activeDefaulted {
		if err := config.ValidateHandoffState(tc.HandoffState, effectiveActive, nil); err != nil {
			errs = append(errs, defaultedStateError("tracker.handoff_state", err, "tracker.active_states", tc.Kind))
		}
	}
	if terminalDefaulted {
		if err := config.ValidateHandoffState(tc.HandoffState, nil, effectiveTerminal); err != nil {
			errs = append(errs, defaultedStateError("tracker.handoff_state", err, "tracker.terminal_states", tc.Kind))
		}
		// The guards above returned on any membership or handoff-equality
		// violation, so the only rule left for this call is the terminal
		// collision.
		if err := config.ValidateInProgressState(tc.InProgressState, effectiveActive, effectiveTerminal, tc.HandoffState); err != nil {
			errs = append(errs, defaultedStateError("tracker.in_progress_state", err, "tracker.terminal_states", tc.Kind))
		}
	}

	return errs
}

// defaultedStateError builds the diagnostic for a collision that a
// tracker adapter's fallback state list exposed, naming the empty
// front-matter key and the tracker kind whose fallback filled it.
//
// The sentence before the semicolon is the config loader's own, so both
// paths report the same collision in the same words. ConfigError.Error
// is not used: its field prefix belongs to a config-load failure, not to
// a preflight message.
func defaultedStateError(check string, err error, emptyKey, kind string) PreflightError {
	message := err.Error()
	if cfgErr, ok := errors.AsType[*config.ConfigError](err); ok {
		message = cfgErr.Message
	}

	stateNoun := "terminal"
	if emptyKey == "tracker.active_states" {
		stateNoun = "active"
	}

	return PreflightError{
		Check: check,
		Message: message + "; " + emptyKey + " is empty, so the " + strconv.Quote(kind) +
			" adapter falls back to its own " + stateNoun + " states",
	}
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

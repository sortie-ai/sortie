package orchestrator

import (
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
	// "tracker.project", "tracker_adapter", "agent.kind",
	// "agent.command", "agent_adapter", "workspace.root_writable".
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
		Meta(kind string) registry.AdapterMeta
	}

	// AgentRegistry provides adapter lookup and metadata queries
	// for the configured agent kind.
	AgentRegistry interface {
		Get(kind string) (registry.AgentConstructor, error)
		Meta(kind string) registry.AdapterMeta
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
		trackerMeta := params.TrackerRegistry.Meta(cfg.Tracker.Kind)

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

	// Agent kind must be set for any subsequent agent validation.
	if cfg.Agent.Kind == "" {
		errs = append(errs, PreflightError{
			Check:   "agent.kind",
			Message: "agent.kind is required",
		})
	}

	// Command is mandatory for adapters that declare it required.
	if cfg.Agent.Kind != "" && params.AgentRegistry.Meta(cfg.Agent.Kind).RequiresCommand {
		if cfg.Agent.Command == "" {
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

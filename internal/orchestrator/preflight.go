package orchestrator

import (
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
	// "agent.command", "agent_adapter".
	Check string

	// Message is an operator-friendly description of the failure.
	Message string
}

// PreflightResult holds the outcome of dispatch preflight validation.
type PreflightResult struct {
	// Errors contains all validation failures found. Empty slice when
	// validation passes.
	Errors []PreflightError
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

	// Check 1: Workflow file can be loaded and parsed. When the
	// reload fails, remaining checks are skipped because ConfigFunc
	// would return stale (or default) config, making those results
	// misleading. The operator must fix the workflow file first.
	if err := params.ReloadWorkflow(); err != nil {
		errs = append(errs, PreflightError{
			Check:   "workflow_load",
			Message: "workflow file cannot be loaded: " + err.Error(),
		})
		return PreflightResult{Errors: errs}
	}

	cfg := params.ConfigFunc()

	// Check 2: tracker.kind is present.
	if cfg.Tracker.Kind == "" {
		errs = append(errs, PreflightError{
			Check:   "tracker.kind",
			Message: "tracker.kind is required",
		})
	}

	// Checks 3 & 4: tracker.api_key and tracker.project when required
	// by the adapter. Fetch meta once to avoid duplicate lookups.
	if cfg.Tracker.Kind != "" {
		trackerMeta := params.TrackerRegistry.Meta(cfg.Tracker.Kind)

		// Check 3: tracker.api_key when required by the adapter.
		if trackerMeta.RequiresAPIKey && cfg.Tracker.APIKey == "" {
			errs = append(errs, PreflightError{
				Check: "tracker.api_key",
				Message: "tracker.api_key is required for tracker kind " + strconv.Quote(cfg.Tracker.Kind) +
					" (value may be empty after environment variable expansion)",
			})
		}

		// Check 4: tracker.project when required by the adapter.
		if trackerMeta.RequiresProject && cfg.Tracker.Project == "" {
			errs = append(errs, PreflightError{
				Check:   "tracker.project",
				Message: "tracker.project is required for tracker kind " + strconv.Quote(cfg.Tracker.Kind),
			})
		}
	}

	// Check 5: Tracker adapter registered and available.
	if cfg.Tracker.Kind != "" {
		if _, err := params.TrackerRegistry.Get(cfg.Tracker.Kind); err != nil {
			errs = append(errs, PreflightError{
				Check:   "tracker_adapter",
				Message: err.Error(),
			})
		}
	}

	// Check 6: agent.kind is present.
	if cfg.Agent.Kind == "" {
		errs = append(errs, PreflightError{
			Check:   "agent.kind",
			Message: "agent.kind is required",
		})
	}

	// Check 7: agent.command when required by the adapter.
	if cfg.Agent.Kind != "" && params.AgentRegistry.Meta(cfg.Agent.Kind).RequiresCommand {
		if cfg.Agent.Command == "" {
			errs = append(errs, PreflightError{
				Check:   "agent.command",
				Message: "agent.command is required for agent kind " + strconv.Quote(cfg.Agent.Kind),
			})
		}
	}

	// Check 8: Agent adapter registered and available.
	if cfg.Agent.Kind != "" {
		if _, err := params.AgentRegistry.Get(cfg.Agent.Kind); err != nil {
			errs = append(errs, PreflightError{
				Check:   "agent_adapter",
				Message: err.Error(),
			})
		}
	}

	return PreflightResult{Errors: errs}
}

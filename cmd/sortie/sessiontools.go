package main

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/tool/budget"
	"github.com/sortie-ai/sortie/internal/tool/history"
	"github.com/sortie-ai/sortie/internal/tool/notify"
	"github.com/sortie-ai/sortie/internal/tool/status"
	"github.com/sortie-ai/sortie/internal/tool/trackerapi"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// SessionToolParams contains the inputs that determine a session's tools.
type SessionToolParams struct {
	// TrackerAdapter is the configured tracker adapter, or nil when no
	// tracker is configured. With Project, gates tracker_api.
	TrackerAdapter domain.TrackerAdapter

	// Project is the tracker project. Empty disables tracker_api.
	Project string

	// WorkspacePath gates sortie_status. Empty disables it.
	WorkspacePath string

	// DBPath is the SQLite database path. With IssueID, gates
	// workspace_history and cost_budget.
	DBPath string

	// IssueID is the tracker-internal issue ID (domain.Issue.ID, not the
	// human-readable domain.Issue.Identifier). With DBPath, gates
	// workspace_history and cost_budget, and is the key the two
	// database-backed tools query by.
	IssueID string

	// Identifier is the human-readable issue key (domain.Issue.Identifier,
	// e.g. "ABC-123"). It gates no tool's registration; it feeds the
	// notify_operator envelope context only.
	Identifier string

	// DispatchID fences a notification's session ID to this dispatch.
	DispatchID string

	// Attempt is nil on the first run.
	Attempt *int

	// AgentKind is the agent kind frozen at dispatch time.
	AgentKind string

	// MaxTokens is the agent.max_tokens ceiling reported by cost_budget.
	MaxTokens int

	// MaxSessions is the agent.max_sessions ceiling reported by
	// cost_budget.
	MaxSessions int

	// Notifications are the configured notifier backends. At least one
	// backend that resolves to a valid constructed backend gates
	// notify_operator.
	Notifications []config.NotificationBackend
}

// SessionToolRegistry pairs a built tool registry with the read-only
// store opened to back its database tools. The caller owns Store and
// must call its Close method when registry consumption is complete.
// Store is nil when no database-backed tool was registered.
type SessionToolRegistry struct {
	// Registry is the gated tool set. Never nil on success; an empty
	// registry is a valid result when no gate is satisfied.
	Registry *domain.ToolRegistry

	// Store is the read-only connection backing workspace_history and
	// cost_budget, or nil when neither was registered. The caller closes
	// it after consuming Registry.
	Store *persistence.Store
}

// BuildSessionToolRegistry returns tools whose registration inputs are satisfied.
//
// An unavailable read-only database omits its tools, but notifier
// misconfiguration is fatal to avoid a partially configured notification path.
func BuildSessionToolRegistry(ctx context.Context, logger *slog.Logger, params SessionToolParams) (SessionToolRegistry, error) {
	if logger == nil {
		logger = slog.Default()
	}

	reg := domain.NewToolRegistry()
	var store *persistence.Store

	if params.TrackerAdapter != nil && params.Project != "" {
		reg.Register(trackerapi.New(params.TrackerAdapter, params.Project))
	}

	if params.WorkspacePath != "" {
		reg.Register(status.New(params.WorkspacePath))
	}

	if params.DBPath != "" && params.IssueID != "" {
		openedStore, err := persistence.OpenReadOnly(ctx, params.DBPath)
		if err != nil {
			logger.Warn("failed to open read-only db for workspace_history and cost_budget",
				slog.String("db_path", params.DBPath),
				slog.Any("error", err))
		} else {
			store = openedStore
			reg.Register(history.New(buildHistoryQuery(store), params.IssueID))
			reg.Register(budget.New(buildBudgetQuery(store), params.IssueID, params.DispatchID, params.MaxTokens, params.MaxSessions))
		}
	}

	sessionIDFunc := func() string {
		return workspace.ReadDispatchSessionID(params.WorkspacePath, params.DispatchID, logger)
	}
	notifyTool, err := buildNotifyTool(params.Notifications, notify.NotificationEnvelopeContext{
		IssueID:    params.IssueID,
		Identifier: params.Identifier,
		DispatchID: params.DispatchID,
		Attempt:    params.Attempt,
		Agent:      params.AgentKind,
	}, sessionIDFunc)
	if err != nil {
		if store != nil {
			store.Close() //nolint:errcheck,gosec // There is no recovery path during cleanup.
		}
		return SessionToolRegistry{}, err
	}
	if notifyTool != nil {
		reg.Register(notifyTool)
	}

	return SessionToolRegistry{Registry: reg, Store: store}, nil
}

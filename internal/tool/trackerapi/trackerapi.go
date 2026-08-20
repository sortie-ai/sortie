// Package trackerapi implements [domain.AgentTool] for issue tracker
// operations. It delegates to a [domain.TrackerAdapter] and enforces
// project-level scoping so agents can only query or mutate issues
// within the configured project.
package trackerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tool/toolresult"
)

// Compile-time interface check.
var _ domain.AgentTool = (*TrackerAPITool)(nil)

// TrackerAPITool implements [domain.AgentTool] by delegating to a
// [domain.TrackerAdapter]. Scoped to a single tracker project at
// construction time. Supports fetch_issue, fetch_comments,
// search_issues, and transition_issue operations.
type TrackerAPITool struct {
	adapter domain.TrackerAdapter
	project string
}

// New creates a [TrackerAPITool] scoped to the given tracker adapter
// and project. Panics if adapter is nil (programming error). The
// project parameter should be non-empty; callers should skip
// construction when tracker.project is unconfigured.
func New(adapter domain.TrackerAdapter, project string) *TrackerAPITool {
	if adapter == nil {
		panic("trackerapi.New: adapter must not be nil")
	}
	return &TrackerAPITool{
		adapter: adapter,
		project: project,
	}
}

// Name returns "tracker_api".
func (t *TrackerAPITool) Name() string { return "tracker_api" }

// Description returns a summary of the tool's capabilities.
func (t *TrackerAPITool) Description() string {
	return "Query or modify issues in the configured issue tracker. " +
		"Supports fetch_issue, fetch_comments, search_issues (active states only), " +
		"and transition_issue operations."
}

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation": {
      "type": "string",
      "enum": ["fetch_issue", "fetch_comments", "search_issues", "transition_issue"],
      "description": "The tracker operation to perform. search_issues returns active-state issues only."
    },
    "issue_id": {
      "type": "string",
      "description": "The tracker issue ID (required for fetch_issue, fetch_comments, transition_issue)."
    },
    "target_state": {
      "type": "string",
      "description": "The target state for transition_issue."
    }
  },
  "required": ["operation"],
  "additionalProperties": false
}`)

// InputSchema returns the JSON Schema describing the expected input.
// The returned slice is a defensive copy; callers may modify it
// without affecting other consumers.
func (t *TrackerAPITool) InputSchema() json.RawMessage {
	out := make(json.RawMessage, len(inputSchema))
	copy(out, inputSchema)
	return out
}

type toolInput struct {
	Operation   string `json:"operation"`
	IssueID     string `json:"issue_id,omitempty"`
	TargetState string `json:"target_state,omitempty"`
}

// Execute runs the requested tracker operation and returns a
// structured JSON result. Domain-level errors are encoded in the
// JSON response as {"success": false, "error": {...}}. The Go error
// return is reserved for truly unexpected internal failures.
func (t *TrackerAPITool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var in toolInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolresult.Failure("invalid_input", fmt.Sprintf("failed to parse input: %s", err))
	}
	if dec.More() {
		return toolresult.Failure("invalid_input", "unexpected trailing content after JSON object")
	}

	if in.Operation == "" {
		return toolresult.Failure("invalid_input", "operation is required")
	}

	switch in.Operation {
	case "fetch_issue":
		if in.IssueID == "" {
			return toolresult.Failure("invalid_input", "issue_id is required for fetch_issue")
		}
		return t.fetchIssue(ctx, in.IssueID)

	case "fetch_comments":
		if in.IssueID == "" {
			return toolresult.Failure("invalid_input", "issue_id is required for fetch_comments")
		}
		return t.fetchComments(ctx, in.IssueID)

	case "search_issues":
		return t.searchIssues(ctx)

	case "transition_issue":
		if in.IssueID == "" {
			return toolresult.Failure("invalid_input", "issue_id is required for transition_issue")
		}
		if in.TargetState == "" {
			return toolresult.Failure("invalid_input", "target_state is required for transition_issue")
		}
		return t.transitionIssue(ctx, in.IssueID, in.TargetState)

	default:
		return toolresult.Failure("unsupported_operation", fmt.Sprintf("unknown operation: %s", in.Operation))
	}
}

func (t *TrackerAPITool) fetchIssue(ctx context.Context, issueID string) (json.RawMessage, error) {
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	if !t.isInProject(issue.Identifier) {
		return toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
	}

	return toolresult.Success(issue.ToTemplateMap())
}

func (t *TrackerAPITool) fetchComments(ctx context.Context, issueID string) (json.RawMessage, error) {
	// Fetch issue first for project scope validation (defense-in-depth).
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	if !t.isInProject(issue.Identifier) {
		return toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
	}

	comments, err := t.adapter.FetchIssueComments(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	commentMaps := make([]map[string]any, len(comments))
	for i, c := range comments {
		commentMaps[i] = map[string]any{
			"id":         c.ID,
			"author":     c.Author,
			"body":       c.Body,
			"created_at": c.CreatedAt,
		}
	}

	return toolresult.Success(commentMaps)
}

func (t *TrackerAPITool) searchIssues(ctx context.Context) (json.RawMessage, error) {
	issues, err := t.adapter.FetchCandidateIssues(ctx)
	if err != nil {
		return mapTrackerError(err)
	}

	issueMaps := make([]map[string]any, len(issues))
	for i := range issues {
		issueMaps[i] = issues[i].ToTemplateMap()
	}

	return toolresult.Success(issueMaps)
}

func (t *TrackerAPITool) transitionIssue(ctx context.Context, issueID, targetState string) (json.RawMessage, error) {
	// Fetch issue first for project scope validation (defense-in-depth).
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	if !t.isInProject(issue.Identifier) {
		return toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
	}

	if err := t.adapter.TransitionIssue(ctx, issueID, targetState); err != nil {
		return mapTrackerError(err)
	}

	return toolresult.Success(map[string]any{"transitioned": true})
}

// isInProject is a defense-in-depth check using identifier prefix.
// It is NOT the primary access control — the TrackerAdapter's own
// project scoping (JQL filter, API endpoint) is the primary control.
// This check catches edge cases where an agent passes an issue ID
// from another project that the API key happens to have access to.
//
// Behavior by tracker kind:
//   - Jira/Linear (PROJ-123 format): extracts prefix, compares with project
//   - File adapter (arbitrary format): identifiers without "-" pass (trusted)
//   - GitHub (future, numeric IDs): no "-", passes (adapter is already scoped)
//
// When project is empty, all identifiers pass (no scoping configured).
func (t *TrackerAPITool) isInProject(identifier string) bool {
	if t.project == "" {
		return true
	}

	lastDash := strings.LastIndex(identifier, "-")
	if lastDash < 0 {
		return true
	}

	prefix := identifier[:lastDash]
	return strings.EqualFold(prefix, t.project)
}

// mapTrackerError converts a tracker error into the failure envelope.
// Context cancellation and deadline exceeded are mapped to
// tracker_transport_error. TrackerError kinds map directly. Unknown
// errors become internal_error. The Go error return is reserved for an
// internal marshal failure.
func mapTrackerError(err error) (json.RawMessage, error) {
	if errors.Is(err, context.Canceled) {
		return toolresult.Failure("tracker_transport_error", "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return toolresult.Failure("tracker_transport_error", "deadline exceeded")
	}

	if te, ok := errors.AsType[*domain.TrackerError](err); ok {
		return toolresult.Failure(string(te.Kind), te.Message)
	}

	return toolresult.Failure("internal_error", "an unexpected internal error occurred")
}

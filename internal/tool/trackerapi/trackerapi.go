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
// and project. The project parameter should be non-empty; callers
// should skip construction when tracker.project is unconfigured.
func New(adapter domain.TrackerAdapter, project string) *TrackerAPITool {
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
		"Supports fetch_issue, fetch_comments, search_issues, and transition_issue operations."
}

// inputSchema is the static JSON Schema for the tool's input.
var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation": {
      "type": "string",
      "enum": ["fetch_issue", "fetch_comments", "search_issues", "transition_issue"],
      "description": "The tracker operation to perform."
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
func (t *TrackerAPITool) InputSchema() json.RawMessage {
	return inputSchema
}

// toolInput is the parsed input for Execute.
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
		return errorResult("invalid_input", fmt.Sprintf("failed to parse input: %s", err)), nil
	}

	switch in.Operation {
	case "fetch_issue":
		if in.IssueID == "" {
			return errorResult("invalid_input", "issue_id is required for fetch_issue"), nil
		}
		return t.fetchIssue(ctx, in.IssueID)

	case "fetch_comments":
		if in.IssueID == "" {
			return errorResult("invalid_input", "issue_id is required for fetch_comments"), nil
		}
		return t.fetchComments(ctx, in.IssueID)

	case "search_issues":
		return t.searchIssues(ctx)

	case "transition_issue":
		if in.IssueID == "" {
			return errorResult("invalid_input", "issue_id is required for transition_issue"), nil
		}
		if in.TargetState == "" {
			return errorResult("invalid_input", "target_state is required for transition_issue"), nil
		}
		return t.transitionIssue(ctx, in.IssueID, in.TargetState)

	default:
		return errorResult("unsupported_operation", fmt.Sprintf("unknown operation: %s", in.Operation)), nil
	}
}

func (t *TrackerAPITool) fetchIssue(ctx context.Context, issueID string) (json.RawMessage, error) {
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err), nil
	}

	if !t.isInProject(issue.Identifier) {
		return errorResult("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project)), nil
	}

	return successResult(issue.ToTemplateMap())
}

func (t *TrackerAPITool) fetchComments(ctx context.Context, issueID string) (json.RawMessage, error) {
	// Fetch issue first for project scope validation (defense-in-depth).
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err), nil
	}

	if !t.isInProject(issue.Identifier) {
		return errorResult("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project)), nil
	}

	comments, err := t.adapter.FetchIssueComments(ctx, issueID)
	if err != nil {
		return mapTrackerError(err), nil
	}

	data := make([]map[string]any, len(comments))
	for i, c := range comments {
		data[i] = map[string]any{
			"id":         c.ID,
			"author":     c.Author,
			"body":       c.Body,
			"created_at": c.CreatedAt,
		}
	}

	return successResult(data)
}

func (t *TrackerAPITool) searchIssues(ctx context.Context) (json.RawMessage, error) {
	issues, err := t.adapter.FetchCandidateIssues(ctx)
	if err != nil {
		return mapTrackerError(err), nil
	}

	data := make([]map[string]any, len(issues))
	for i := range issues {
		data[i] = issues[i].ToTemplateMap()
	}

	return successResult(data)
}

func (t *TrackerAPITool) transitionIssue(ctx context.Context, issueID, targetState string) (json.RawMessage, error) {
	// Fetch issue first for project scope validation (defense-in-depth).
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err), nil
	}

	if !t.isInProject(issue.Identifier) {
		return errorResult("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project)), nil
	}

	if err := t.adapter.TransitionIssue(ctx, issueID, targetState); err != nil {
		return mapTrackerError(err), nil
	}

	return successResult(map[string]any{"transitioned": true})
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

// mapTrackerError converts a tracker error into a structured JSON
// error response. Context cancellation and deadline exceeded are
// mapped to tracker_transport_error. TrackerError kinds map directly.
// Unknown errors become internal_error.
func mapTrackerError(err error) json.RawMessage {
	if errors.Is(err, context.Canceled) {
		return errorResult("tracker_transport_error", "request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errorResult("tracker_transport_error", "deadline exceeded")
	}

	var te *domain.TrackerError
	if errors.As(err, &te) {
		return errorResult(string(te.Kind), te.Message)
	}

	return errorResult("internal_error", err.Error())
}

// successResult marshals a success response envelope.
func successResult(data any) (json.RawMessage, error) {
	result, err := json.Marshal(map[string]any{
		"success": true,
		"data":    data,
	})
	if err != nil {
		return nil, fmt.Errorf("trackerapi: marshal success result: %w", err)
	}
	return result, nil
}

// errorResult marshals an error response envelope. Panics on marshal
// failure (should never happen with string inputs).
func errorResult(kind, message string) json.RawMessage {
	result, err := json.Marshal(map[string]any{
		"success": false,
		"error": map[string]string{
			"kind":    kind,
			"message": message,
		},
	})
	if err != nil {
		panic(fmt.Sprintf("trackerapi: marshal error result: %v", err))
	}
	return result
}

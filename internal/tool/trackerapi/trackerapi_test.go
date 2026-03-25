package trackerapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- mock tracker adapter ---

// mockTrackerAdapter is a configurable test double for domain.TrackerAdapter.
type mockTrackerAdapter struct {
	fetchCandidatesFn func(ctx context.Context) ([]domain.Issue, error)
	fetchByIDFn       func(ctx context.Context, issueID string) (domain.Issue, error)
	fetchCommentsFn   func(ctx context.Context, issueID string) ([]domain.Comment, error)
	transitionFn      func(ctx context.Context, issueID, targetState string) error
}

var _ domain.TrackerAdapter = (*mockTrackerAdapter)(nil)

func (m *mockTrackerAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	if m.fetchCandidatesFn != nil {
		return m.fetchCandidatesFn(ctx)
	}
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	if m.fetchByIDFn != nil {
		return m.fetchByIDFn(ctx, issueID)
	}
	return domain.Issue{}, nil
}

func (m *mockTrackerAdapter) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	if m.fetchCommentsFn != nil {
		return m.fetchCommentsFn(ctx, issueID)
	}
	return nil, nil
}

func (m *mockTrackerAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	if m.transitionFn != nil {
		return m.transitionFn(ctx, issueID, targetState)
	}
	return nil
}

// --- test helpers ---

// projIssue returns an issue with the given identifier in project PROJ.
func projIssue(id, identifier string) domain.Issue {
	return domain.Issue{
		ID:         id,
		Identifier: identifier,
		Title:      "Test issue",
		State:      "To Do",
		Labels:     []string{},
	}
}

// trackerErr returns a *domain.TrackerError with the given kind and message.
func trackerErr(kind domain.TrackerErrorKind, msg string) *domain.TrackerError {
	return &domain.TrackerError{Kind: kind, Message: msg}
}

// resultFields unmarshals a tool JSON response and returns the top-level
// fields. Fatals on parse failure.
func resultFields(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal result: %v\nraw: %s", err, raw)
	}
	return m
}

// assertSuccess checks the result has "success":true and returns the "data" field.
func assertSuccess(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	fields := resultFields(t, raw)
	var success bool
	if err := json.Unmarshal(fields["success"], &success); err != nil {
		t.Fatalf("unmarshal success field: %v", err)
	}
	if !success {
		t.Fatalf("expected success=true, got result: %s", raw)
	}
	data, ok := fields["data"]
	if !ok {
		t.Fatal("success result missing \"data\" field")
	}
	return data
}

// assertErrorKind checks the result has "success":false and the error kind matches.
func assertErrorKind(t *testing.T, raw json.RawMessage, wantKind string) {
	t.Helper()
	fields := resultFields(t, raw)
	var success bool
	if err := json.Unmarshal(fields["success"], &success); err != nil {
		t.Fatalf("unmarshal success field: %v", err)
	}
	if success {
		t.Fatalf("expected success=false, got result: %s", raw)
	}
	var errObj struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(fields["error"], &errObj); err != nil {
		t.Fatalf("unmarshal error field: %v\nraw: %s", err, raw)
	}
	if errObj.Kind != wantKind {
		t.Errorf("error.kind = %q, want %q\nraw: %s", errObj.Kind, wantKind, raw)
	}
}

// assertErrorContains checks that the error message contains substr.
func assertErrorContains(t *testing.T, raw json.RawMessage, substr string) {
	t.Helper()
	fields := resultFields(t, raw)
	var errObj struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(fields["error"], &errObj); err != nil {
		t.Fatalf("unmarshal error field: %v\nraw: %s", err, raw)
	}
	if len(errObj.Message) == 0 {
		t.Fatalf("error.message is empty, want to contain %q", substr)
	}
	for i := range errObj.Message {
		if len(errObj.Message[i:]) >= len(substr) && errObj.Message[i:i+len(substr)] == substr {
			return
		}
	}
	t.Errorf("error.message = %q, want to contain %q", errObj.Message, substr)
}

// --- tests ---

func TestName(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	if got := tool.Name(); got != "tracker_api" {
		t.Errorf("Name() = %q, want %q", got, "tracker_api")
	}
}

func TestDescription(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	if got := tool.Description(); got == "" {
		t.Error("Description() is empty, want non-empty")
	}
}

func TestInputSchema(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	schema := tool.InputSchema()
	if len(schema) == 0 {
		t.Fatal("InputSchema() is empty")
	}
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		t.Fatalf("InputSchema() is not valid JSON: %v", err)
	}
	if parsed["type"] != "object" {
		t.Errorf("schema type = %v, want \"object\"", parsed["type"])
	}
}

func TestFetchIssue_Success(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "PROJ-123"), nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_issue","issue_id":"12345"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}

	data := assertSuccess(t, result)
	var issueMap map[string]any
	if err := json.Unmarshal(data, &issueMap); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if issueMap["identifier"] != "PROJ-123" {
		t.Errorf("data.identifier = %v, want %q", issueMap["identifier"], "PROJ-123")
	}
	if issueMap["title"] != "Test issue" {
		t.Errorf("data.title = %v, want %q", issueMap["title"], "Test issue")
	}
}

func TestFetchIssue_NotFound(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, _ string) (domain.Issue, error) {
			return domain.Issue{}, trackerErr(domain.ErrTrackerNotFound, "issue not found")
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_issue","issue_id":"99999"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "tracker_not_found")
}

func TestFetchIssue_ProjectScopeViolation(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "OTHERPROJ-456"), nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_issue","issue_id":"456"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "project_scope_violation")
	assertErrorContains(t, result, "OTHERPROJ-456")
}

func TestFetchComments_Success(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "PROJ-10"), nil
		},
		fetchCommentsFn: func(_ context.Context, _ string) ([]domain.Comment, error) {
			return []domain.Comment{
				{ID: "c1", Author: "alice", Body: "looks good", CreatedAt: "2025-01-01T00:00:00Z"},
				{ID: "c2", Author: "bob", Body: "needs work", CreatedAt: "2025-01-02T00:00:00Z"},
			}, nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_comments","issue_id":"10"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}

	data := assertSuccess(t, result)
	var comments []map[string]any
	if err := json.Unmarshal(data, &comments); err != nil {
		t.Fatalf("unmarshal data array: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0]["author"] != "alice" {
		t.Errorf("comments[0].author = %v, want %q", comments[0]["author"], "alice")
	}
}

func TestFetchComments_NotFound(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, _ string) (domain.Issue, error) {
			return domain.Issue{}, trackerErr(domain.ErrTrackerNotFound, "issue not found")
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_comments","issue_id":"99999"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "tracker_not_found")
}

func TestSearchIssues_Success(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return []domain.Issue{
				projIssue("1", "PROJ-1"),
				projIssue("2", "PROJ-2"),
			}, nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"search_issues"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}

	data := assertSuccess(t, result)
	var issues []map[string]any
	if err := json.Unmarshal(data, &issues); err != nil {
		t.Fatalf("unmarshal data array: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
}

func TestSearchIssues_TransportError(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, trackerErr(domain.ErrTrackerTransport, "connection refused")
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"search_issues"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "tracker_transport_error")
}

func TestTransitionIssue_Success(t *testing.T) {
	t.Parallel()

	var transitionedID, transitionedState string
	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "PROJ-42"), nil
		},
		transitionFn: func(_ context.Context, id, state string) error {
			transitionedID = id
			transitionedState = state
			return nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"transition_issue","issue_id":"42","target_state":"In Review"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}

	data := assertSuccess(t, result)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if m["transitioned"] != true {
		t.Errorf("data.transitioned = %v, want true", m["transitioned"])
	}
	if transitionedID != "42" {
		t.Errorf("TransitionIssue called with id=%q, want %q", transitionedID, "42")
	}
	if transitionedState != "In Review" {
		t.Errorf("TransitionIssue called with state=%q, want %q", transitionedState, "In Review")
	}
}

func TestTransitionIssue_Failure(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "PROJ-42"), nil
		},
		transitionFn: func(_ context.Context, _, _ string) error {
			return trackerErr(domain.ErrTrackerPayload, "no valid transition")
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"transition_issue","issue_id":"42","target_state":"Done"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "tracker_payload_error")
}

func TestTransitionIssue_ProjectScope(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "OTHERPROJ-42"), nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"transition_issue","issue_id":"42","target_state":"Done"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "project_scope_violation")
}

func TestUnknownOperation(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"delete_issue"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "unsupported_operation")
}

func TestInvalidJSON(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{not valid json`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "invalid_input")
}

func TestMissingIssueID(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")

	tests := []struct {
		name  string
		input string
	}{
		{"fetch_issue", `{"operation":"fetch_issue"}`},
		{"fetch_comments", `{"operation":"fetch_comments"}`},
		{"transition_issue", `{"operation":"transition_issue","target_state":"Done"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := tool.Execute(context.Background(), json.RawMessage(tt.input))
			if err != nil {
				t.Fatalf("Execute returned Go error: %v", err)
			}
			assertErrorKind(t, result, "invalid_input")
			assertErrorContains(t, result, "issue_id")
		})
	}
}

func TestMissingTargetState(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"transition_issue","issue_id":"42"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "invalid_input")
	assertErrorContains(t, result, "target_state")
}

func TestProjectScopeNoPrefix(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "123"), nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_issue","issue_id":"123"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertSuccess(t, result)
}

func TestContextCanceled(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, _ string) (domain.Issue, error) {
			return domain.Issue{}, context.Canceled
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_issue","issue_id":"1"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "tracker_transport_error")
	assertErrorContains(t, result, "cancel")
}

func TestUnknownFields(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"search_issues","extra_field":"bad"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "invalid_input")
}

func TestDeadlineExceeded(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, context.DeadlineExceeded
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"search_issues"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "tracker_transport_error")
	assertErrorContains(t, result, "deadline")
}

func TestIsInProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		project    string
		identifier string
		want       bool
	}{
		{"matching prefix", "PROJ", "PROJ-123", true},
		{"case-insensitive match", "proj", "PROJ-123", true},
		{"mismatching prefix", "PROJ", "OTHERPROJ-456", false},
		{"no dash in identifier", "PROJ", "123", true},
		{"empty project allows all", "", "OTHERPROJ-456", true},
		{"multi-dash takes last", "PROJ", "PROJ-SUB-123", false},
		{"prefix with last dash", "PROJ-SUB", "PROJ-SUB-123", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tool := &TrackerAPITool{project: tt.project}
			got := tool.isInProject(tt.identifier)
			if got != tt.want {
				t.Errorf("isInProject(%q) with project=%q = %t, want %t", tt.identifier, tt.project, got, tt.want)
			}
		})
	}
}

func TestFetchComments_ProjectScopeViolation(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return projIssue(id, "OTHERPROJ-10"), nil
		},
	}, "PROJ")

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"fetch_comments","issue_id":"10"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "project_scope_violation")
}

func TestExecuteNeverReturnsGoError(t *testing.T) {
	t.Parallel()

	// All tracker operations should produce JSON error results, not Go errors.
	tool := New(&mockTrackerAdapter{
		fetchByIDFn: func(_ context.Context, _ string) (domain.Issue, error) {
			return domain.Issue{}, trackerErr(domain.ErrTrackerAuth, "forbidden")
		},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, trackerErr(domain.ErrTrackerAPI, "rate limited")
		},
	}, "PROJ")

	inputs := []string{
		`{"operation":"fetch_issue","issue_id":"1"}`,
		`{"operation":"fetch_comments","issue_id":"1"}`,
		`{"operation":"search_issues"}`,
		`{"operation":"unknown_op"}`,
		`{invalid`,
	}

	for _, input := range inputs {
		_, err := tool.Execute(context.Background(), json.RawMessage(input))
		if err != nil {
			t.Errorf("Execute(%s) returned Go error: %v, want nil", input, err)
		}
	}
}

func TestNewNilAdapterPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New(nil, ...) did not panic, want panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", r)
		}
		if msg != "trackerapi.New: adapter must not be nil" {
			t.Errorf("panic message = %q, want %q", msg, "trackerapi.New: adapter must not be nil")
		}
	}()

	New(nil, "PROJ")
}

func TestTrailingJSONContent(t *testing.T) {
	t.Parallel()

	tool := New(&mockTrackerAdapter{}, "PROJ")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"search_issues"}{"extra":"object"}`))
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	assertErrorKind(t, result, "invalid_input")
	assertErrorContains(t, result, "trailing")
}

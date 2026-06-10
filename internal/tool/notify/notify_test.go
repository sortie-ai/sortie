package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// mockNotifier is a test double for domain.Notifier that records calls
// and optionally returns a configured error.
type mockNotifier struct {
	received []domain.Notification
	err      error
}

var _ domain.Notifier = (*mockNotifier)(nil)

func (m *mockNotifier) Send(_ context.Context, n domain.Notification) error {
	m.received = append(m.received, n)
	return m.err
}

// testEnv returns a NotificationEnvelopeContext with recognizable test
// values.
func testEnv() NotificationEnvelopeContext {
	attempt := 2
	return NotificationEnvelopeContext{
		IssueID:    "issue-42",
		Identifier: "PROJ-42",
		SessionID:  "sess-001",
		Attempt:    &attempt,
		Agent:      "claude-code",
		Source:     "test-host",
	}
}

// executeJSON runs Execute with the given JSON input and unmarshals the
// result into a map.
func executeJSON(t *testing.T, tool *NotifyTool, input string) map[string]any {
	t.Helper()
	raw, err := tool.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute returned non-nil Go error: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Execute result unmarshal: %v", err)
	}
	return m
}

func assertSuccess(t *testing.T, m map[string]any) {
	t.Helper()
	if m["success"] != true {
		t.Errorf("result[\"success\"] = %v, want true", m["success"])
	}
}

func assertFailureKind(t *testing.T, m map[string]any, wantKind string) {
	t.Helper()
	if m["success"] != false {
		t.Errorf("result[\"success\"] = %v, want false", m["success"])
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("result[\"error\"] = %v (%T), want map", m["error"], m["error"])
	}
	if got := errObj["kind"].(string); got != wantKind {
		t.Errorf("error.kind = %q, want %q", got, wantKind)
	}
}

// --- Tool identity tests ---

func TestNotifyTool_Name(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	if got := tool.Name(); got != "notify_operator" {
		t.Errorf("Name() = %q, want %q", got, "notify_operator")
	}
}

func TestNotifyTool_Description(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	if got := tool.Description(); got == "" {
		t.Error(`Description() = "", want non-empty`)
	}
}

func TestNotifyTool_InputSchema_ValidJSON(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	schema := tool.InputSchema()

	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		t.Fatalf("InputSchema() unmarshal: %v", err)
	}

	if got, _ := m["type"].(string); got != "object" {
		t.Errorf("schema type = %v, want %q", m["type"], "object")
	}

	v, ok := m["additionalProperties"]
	if !ok {
		t.Fatal("additionalProperties key missing from schema")
	}
	if v != false {
		t.Errorf("additionalProperties = %v, want false", v)
	}

	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties missing from schema")
	}
	for _, key := range []string{"severity", "title", "body", "category"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema properties missing key %q", key)
		}
	}
}

func TestNotifyTool_InputSchema_DefensiveCopy(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	schema1 := tool.InputSchema()
	for i := range schema1 {
		schema1[i] = 'X'
	}

	schema2 := tool.InputSchema()
	var m map[string]any
	if err := json.Unmarshal(schema2, &m); err != nil {
		t.Fatalf("InputSchema() after mutation: %v", err)
	}
}

// --- New panic tests ---

func TestNew_PanicsOnEmptyBackends(t *testing.T) {
	t.Parallel()

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("New(empty backends) did not panic, want panic")
		}
		msg := fmt.Sprint(v)
		if !strings.Contains(msg, "backends") {
			t.Errorf("panic message = %q, want to contain %q", msg, "backends")
		}
	}()

	New([]domain.Notifier{}, testEnv(), 10)
}

func TestNew_PanicsOnNilBackends(t *testing.T) {
	t.Parallel()

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("New(nil backends) did not panic, want panic")
		}
	}()

	New(nil, testEnv(), 10)
}

// --- Execute dispatch and delivery tests ---

func TestExecute_ValidCallDispatchesToBackend(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"Hello","body":"World"}`)
	assertSuccess(t, m)

	if len(mock.received) != 1 {
		t.Fatalf("backend Send called %d times, want 1", len(mock.received))
	}
}

func TestExecute_EnvelopeCarriesSessionContext(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	env := testEnv()
	tool := New([]domain.Notifier{mock}, env, 10)

	executeJSON(t, tool, `{"severity":"warning","title":"Check","body":"Details"}`)

	if len(mock.received) != 1 {
		t.Fatalf("Send called %d times, want 1", len(mock.received))
	}
	n := mock.received[0]

	if n.Envelope.IssueID != env.IssueID {
		t.Errorf("Envelope.IssueID = %q, want %q", n.Envelope.IssueID, env.IssueID)
	}
	if n.Envelope.Identifier != env.Identifier {
		t.Errorf("Envelope.Identifier = %q, want %q", n.Envelope.Identifier, env.Identifier)
	}
	if n.Envelope.SessionID != env.SessionID {
		t.Errorf("Envelope.SessionID = %q, want %q", n.Envelope.SessionID, env.SessionID)
	}
	if n.Envelope.Agent != env.Agent {
		t.Errorf("Envelope.Agent = %q, want %q", n.Envelope.Agent, env.Agent)
	}
	if n.Envelope.Attempt == nil {
		t.Fatal("Envelope.Attempt = nil, want non-nil")
	}
	if *n.Envelope.Attempt != *env.Attempt {
		t.Errorf("Envelope.Attempt = %d, want %d", *n.Envelope.Attempt, *env.Attempt)
	}
	if n.Envelope.NotificationID == "" {
		t.Error("Envelope.NotificationID is empty, want generated ID")
	}
	if n.Envelope.Timestamp == "" {
		t.Error("Envelope.Timestamp is empty, want ISO-8601 UTC timestamp")
	}
}

func TestExecute_MessageCarriesAgentFields(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	executeJSON(t, tool, `{"severity":"critical","title":"Urgent","body":"Stop now","category":"blocked"}`)

	if len(mock.received) != 1 {
		t.Fatalf("Send called %d times, want 1", len(mock.received))
	}
	msg := mock.received[0].Message

	if msg.Severity != "critical" {
		t.Errorf("Message.Severity = %q, want %q", msg.Severity, "critical")
	}
	if msg.Title != "Urgent" {
		t.Errorf("Message.Title = %q, want %q", msg.Title, "Urgent")
	}
	if msg.Body != "Stop now" {
		t.Errorf("Message.Body = %q, want %q", msg.Body, "Stop now")
	}
	if msg.Category != "blocked" {
		t.Errorf("Message.Category = %q, want %q", msg.Category, "blocked")
	}
}

func TestExecute_SuccessResultShape(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"Hi","body":"Body"}`)
	assertSuccess(t, m)

	if _, ok := m["delivered"]; !ok {
		t.Error("result missing \"delivered\" field")
	}
	if _, ok := m["notification_id"]; !ok {
		t.Error("result missing \"notification_id\" field")
	}
}

// --- Validation error tests ---

func TestExecute_InvalidInput_UnknownField(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B","unknown_field":"x"}`)
	assertFailureKind(t, m, "invalid_input")
}

func TestExecute_InvalidInput_BadSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		severity string
	}{
		{name: "empty severity", severity: ""},
		{name: "unknown severity", severity: "debug"},
		{name: "uppercase severity", severity: "INFO"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
			input := fmt.Sprintf(`{"severity":%q,"title":"T","body":"B"}`, tt.severity)
			m := executeJSON(t, tool, input)
			assertFailureKind(t, m, "invalid_input")
		})
	}
}

func TestExecute_InvalidInput_BadCategory(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B","category":"not_valid"}`)
	assertFailureKind(t, m, "invalid_input")
}

func TestExecute_InvalidInput_EmptyTitle(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	m := executeJSON(t, tool, `{"severity":"info","title":"","body":"B"}`)
	assertFailureKind(t, m, "invalid_input")
}

func TestExecute_InvalidInput_EmptyBody(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":""}`)
	assertFailureKind(t, m, "invalid_input")
}

func TestExecute_InvalidInput_MalformedJSON(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	m := executeJSON(t, tool, `{bad json}`)
	assertFailureKind(t, m, "invalid_input")
}

func TestExecute_InvalidInput_GoErrorIsNil(t *testing.T) {
	t.Parallel()

	tool := New([]domain.Notifier{&mockNotifier{}}, testEnv(), 10)
	// Unknown field triggers invalid_input; the Go error must be nil.
	_, goErr := tool.Execute(context.Background(), json.RawMessage(`{"severity":"info","title":"T","body":"B","extra":1}`))
	if goErr != nil {
		t.Errorf("Execute(invalid input) Go error = %v, want nil", goErr)
	}
}

// --- Rate limiting tests ---

func TestExecute_RateLimited_PastCap(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	const cap = 3
	tool := New([]domain.Notifier{mock}, testEnv(), cap)

	input := `{"severity":"info","title":"T","body":"B"}`
	for range cap {
		m := executeJSON(t, tool, input)
		assertSuccess(t, m)
	}

	// The (cap+1)-th call must be rate_limited.
	m := executeJSON(t, tool, input)
	assertFailureKind(t, m, "rate_limited")
}

func TestExecute_RateLimited_SendNotCalledWhenCapped(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 1)

	input := `{"severity":"info","title":"T","body":"B"}`
	executeJSON(t, tool, input) // consumes the cap

	before := len(mock.received)
	executeJSON(t, tool, input) // should be rate_limited, Send not called
	after := len(mock.received)

	if after != before {
		t.Errorf("mock.Send call count changed from %d to %d after rate_limited; want no additional call", before, after)
	}
}

func TestExecute_AcceptedCallIncrementsCounterOnce(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	input := `{"severity":"info","title":"T","body":"B"}`
	executeJSON(t, tool, input)
	executeJSON(t, tool, input)
	executeJSON(t, tool, input)

	if mock.received != nil && len(mock.received) != 3 {
		t.Errorf("Send called %d times, want 3 for 3 accepted calls", len(mock.received))
	}
	if tool.count != 3 {
		t.Errorf("internal counter = %d, want 3", tool.count)
	}
}

func TestExecute_RejectedCallDoesNotIncrementCounter(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	// Validation failure — counter must not increment.
	executeJSON(t, tool, `{"severity":"bad","title":"T","body":"B"}`)
	if tool.count != 0 {
		t.Errorf("counter = %d after rejected call, want 0", tool.count)
	}
}

// --- Send failure tests ---

func TestExecute_SendFailed_ReturnsCorrectKind(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{err: errors.New("connection failure")}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)
	assertFailureKind(t, m, "send_failed")
}

func TestExecute_SendFailed_GoErrorIsNil(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{err: errors.New("transport error")}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	_, goErr := tool.Execute(context.Background(), json.RawMessage(`{"severity":"info","title":"T","body":"B"}`))
	if goErr != nil {
		t.Errorf("Execute(send failure) Go error = %v, want nil", goErr)
	}
}

// classifiedSendError simulates a backend-classified error that carries
// only a category, never the URL or secret.
type classifiedSendError struct {
	Category string
}

func (e *classifiedSendError) Error() string { return e.Category }

func TestExecute_SendFailed_MessageRedacted(t *testing.T) {
	t.Parallel()

	// A real backend (webhook/slack) returns a *sendError whose Error()
	// returns only a category string (e.g. "connection failure"), never
	// the URL or secret. The tool should propagate that category.
	const secretURL = "https://secret-endpoint.example.com/tok"
	mock := &mockNotifier{err: &classifiedSendError{Category: "connection failure"}}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)
	assertFailureKind(t, m, "send_failed")

	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatal("error field missing or wrong type")
	}
	msg, _ := errObj["message"].(string)
	if strings.Contains(msg, secretURL) {
		t.Errorf("send_failed message contains URL %q: %q", secretURL, msg)
	}
	// The message should contain the classified category.
	if !strings.Contains(msg, "connection failure") {
		t.Errorf("send_failed message = %q, want to contain %q", msg, "connection failure")
	}
}

// --- Valid severity and category acceptance ---

func TestExecute_AllValidSeverities(t *testing.T) {
	t.Parallel()

	for _, sev := range []string{"info", "warning", "critical"} {
		t.Run(sev, func(t *testing.T) {
			t.Parallel()

			mock := &mockNotifier{}
			tool := New([]domain.Notifier{mock}, testEnv(), 10)
			input := fmt.Sprintf(`{"severity":%q,"title":"T","body":"B"}`, sev)
			m := executeJSON(t, tool, input)
			assertSuccess(t, m)
		})
	}
}

func TestExecute_AllValidCategories(t *testing.T) {
	t.Parallel()

	for _, cat := range []string{"decision_needed", "progress", "blocked", "completed", "other"} {
		t.Run(cat, func(t *testing.T) {
			t.Parallel()

			mock := &mockNotifier{}
			tool := New([]domain.Notifier{mock}, testEnv(), 10)
			input := fmt.Sprintf(`{"severity":"info","title":"T","body":"B","category":%q}`, cat)
			m := executeJSON(t, tool, input)
			assertSuccess(t, m)
		})
	}
}

func TestExecute_OptionalCategoryAbsent(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	tool := New([]domain.Notifier{mock}, testEnv(), 10)
	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)
	assertSuccess(t, m)

	if len(mock.received) != 1 {
		t.Fatalf("Send called %d times, want 1", len(mock.received))
	}
	if mock.received[0].Message.Category != "" {
		t.Errorf("Message.Category = %q, want empty when category absent", mock.received[0].Message.Category)
	}
}

// --- Multi-backend tests ---

func TestExecute_MultipleBackends_AllReceiveNotification(t *testing.T) {
	t.Parallel()

	mock1 := &mockNotifier{}
	mock2 := &mockNotifier{}
	tool := New([]domain.Notifier{mock1, mock2}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)
	assertSuccess(t, m)

	if len(mock1.received) != 1 {
		t.Errorf("backend 1 Send called %d times, want 1", len(mock1.received))
	}
	if len(mock2.received) != 1 {
		t.Errorf("backend 2 Send called %d times, want 1", len(mock2.received))
	}
	if got := m["delivered"]; got != float64(2) {
		t.Errorf("delivered = %v, want 2", got)
	}
}

func TestExecute_MultipleBackends_FirstErrorShortCircuits(t *testing.T) {
	t.Parallel()

	mock1 := &mockNotifier{err: errors.New("first backend error")}
	mock2 := &mockNotifier{}
	tool := New([]domain.Notifier{mock1, mock2}, testEnv(), 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)
	assertFailureKind(t, m, "send_failed")

	if len(mock2.received) != 0 {
		t.Errorf("backend 2 was called %d times after first backend failed, want 0", len(mock2.received))
	}
}

// --- Envelope source field ---

func TestExecute_SourceFromEnvContext(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	env := testEnv()
	env.Source = "custom-source"
	tool := New([]domain.Notifier{mock}, env, 10)

	executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)

	if len(mock.received) != 1 {
		t.Fatalf("Send called %d times, want 1", len(mock.received))
	}
	if got := mock.received[0].Envelope.Source; got != "custom-source" {
		t.Errorf("Envelope.Source = %q, want %q", got, "custom-source")
	}
}

func TestExecute_EmptyAgentFieldIsNotError(t *testing.T) {
	t.Parallel()

	mock := &mockNotifier{}
	env := testEnv()
	env.Agent = ""
	tool := New([]domain.Notifier{mock}, env, 10)

	m := executeJSON(t, tool, `{"severity":"info","title":"T","body":"B"}`)
	assertSuccess(t, m)

	if len(mock.received) != 1 {
		t.Fatalf("Send called %d times, want 1", len(mock.received))
	}
	if got := mock.received[0].Envelope.Agent; got != "" {
		t.Errorf("Envelope.Agent = %q, want empty when Agent is unset", got)
	}
}

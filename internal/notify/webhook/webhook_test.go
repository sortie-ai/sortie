package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// captureServer starts an httptest.Server that records the last request
// body and returns the given status code.
func captureServer(t *testing.T, statusCode int) (server *httptest.Server, body func() []byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("captureServer: read body: %v", err)
		}
		captured = b
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte { return captured }
}

// slogCapture returns a slog.Logger backed by a bytes.Buffer at Debug
// level and a function that retrieves the captured output.
func slogCapture() (*slog.Logger, func() string) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), func() string { return buf.String() }
}

// makeNotification builds a test Notification with recognizable field
// values that can be checked in the posted body.
func makeNotification() domain.Notification {
	return domain.Notification{
		Envelope: domain.NotificationEnvelope{
			NotificationID: "notif-uuid-001",
			Timestamp:      "2026-06-10T14:00:00Z",
			Source:         "test-host",
			IssueID:        "issue-7",
			Identifier:     "PROJ-7",
			SessionID:      "sess-xyz",
			Attempt:        new(2),
			Agent:          "claude-code",
		},
		Message: domain.NotificationMessage{
			Severity: "warning",
			Title:    "Needs review",
			Body:     "Agent paused for human decision.",
			Category: "decision_needed",
		},
	}
}

func TestWebhook_NewNotifier_MissingURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
	}{
		{name: "absent url key", config: map[string]any{}},
		{name: "empty string url", config: map[string]any{"url": ""}},
		{name: "whitespace only url", config: map[string]any{"url": "   "}},
		{name: "nil url value", config: map[string]any{"url": nil}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newNotifier(tt.config)
			if err == nil {
				t.Fatalf("newNotifier(%v) = nil error, want constructor error for missing url", tt.config)
			}
		})
	}
}

func TestWebhook_NewNotifier_ValidURL(t *testing.T) {
	t.Parallel()

	srv, _ := captureServer(t, http.StatusOK)

	n, err := newNotifier(map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("newNotifier(valid url) = %v, want nil", err)
	}
	if n == nil {
		t.Fatal("newNotifier(valid url) returned nil notifier")
	}
}

func TestWebhook_Send_PostsEnvelopeAndMessage(t *testing.T) {
	t.Parallel()

	srv, getBody := captureServer(t, http.StatusOK)

	n, err := newNotifier(map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	notif := makeNotification()
	if err := n.Send(context.Background(), notif); err != nil {
		t.Fatalf("Send: %v", err)
	}

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("Send posted empty body, want non-empty JSON")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Send body unmarshal: %v", err)
	}

	fields := map[string]string{
		"notification_id": "notif-uuid-001",
		"timestamp":       "2026-06-10T14:00:00Z",
		"source":          "test-host",
		"issue_id":        "issue-7",
		"identifier":      "PROJ-7",
		"session_id":      "sess-xyz",
		"agent":           "claude-code",
		"severity":        "warning",
		"title":           "Needs review",
		"body":            "Agent paused for human decision.",
		"category":        "decision_needed",
	}
	for key, want := range fields {
		got, ok := m[key].(string)
		if !ok {
			t.Errorf("body[%q] missing or wrong type: %v", key, m[key])
			continue
		}
		if got != want {
			t.Errorf("body[%q] = %q, want %q", key, got, want)
		}
	}

	if attemptRaw, ok := m["attempt"]; !ok {
		t.Error("body[\"attempt\"] missing")
	} else if attempt, ok := attemptRaw.(float64); !ok || int(attempt) != 2 {
		t.Errorf("body[\"attempt\"] = %v, want 2", attemptRaw)
	}
}

func TestWebhook_Send_Non2xxReturnsClassifiedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "401 unauthorized", statusCode: http.StatusUnauthorized},
		{name: "403 forbidden", statusCode: http.StatusForbidden},
		{name: "429 rate limited", statusCode: http.StatusTooManyRequests},
		{name: "500 server error", statusCode: http.StatusInternalServerError},
		{name: "404 client error", statusCode: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := captureServer(t, tt.statusCode)

			n, err := newNotifier(map[string]any{"url": srv.URL})
			if err != nil {
				t.Fatalf("newNotifier: %v", err)
			}

			err = n.Send(context.Background(), makeNotification())
			if err == nil {
				t.Fatalf("Send(status %d) = nil, want error", tt.statusCode)
			}

			var se *sendError
			if !errors.As(err, &se) {
				t.Fatalf("Send error type = %T, want *sendError", err)
			}
			if se.Category == "" {
				t.Error("sendError.Category is empty, want a non-empty category string")
			}
		})
	}
}

func TestWebhook_Send_TransportFailureReturnsClassifiedError(t *testing.T) {
	t.Parallel()

	// Use a server that is immediately closed so the transport fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	n, err := newNotifier(map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	err = n.Send(context.Background(), makeNotification())
	if err == nil {
		t.Fatal("Send(closed server) = nil, want transport error")
	}

	var se *sendError
	if !errors.As(err, &se) {
		t.Fatalf("Send error type = %T, want *sendError", err)
	}
	if se.Category == "" {
		t.Error("sendError.Category is empty")
	}
}

func TestWebhook_Send_ContextCancellationReturnsError(t *testing.T) {
	t.Parallel()

	// A server that closes the connection immediately after accepting, so
	// the client's Do() returns a transport error fast without waiting for
	// the handler to complete.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close so the client sees a connection reset quickly.
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	n, err := newNotifier(map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	// Cancel the context before sending.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = n.Send(ctx, makeNotification())
	if err == nil {
		t.Fatal("Send(cancelled ctx) = nil, want error")
	}
}

func TestWebhook_ErrorRedaction_URLAbsentFromError(t *testing.T) {
	t.Parallel()

	// Use a real httptest server that returns a non-2xx response. The
	// classified sendError must not echo the server URL or any secret.
	srv, _ := captureServer(t, http.StatusInternalServerError)
	const secretToken = "token123"
	webhookURL := srv.URL + "/" + secretToken

	n, err := newNotifier(map[string]any{"url": webhookURL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	sendErr := n.Send(context.Background(), makeNotification())
	if sendErr == nil {
		t.Fatal("Send(500 response) = nil, want error")
	}

	errText := sendErr.Error()
	if strings.Contains(errText, webhookURL) {
		t.Errorf("error message contains URL %q: %q", webhookURL, errText)
	}
	if strings.Contains(errText, secretToken) {
		t.Errorf("error message contains secret token: %q", errText)
	}
}

func TestWebhook_ErrorRedaction_URLAbsentFromLog(t *testing.T) {
	t.Parallel()

	srv, _ := captureServer(t, http.StatusUnauthorized)
	const secretToken = "token456"
	webhookURL := srv.URL + "/" + secretToken

	logger, getLog := slogCapture()

	n, err := newNotifier(map[string]any{"url": webhookURL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	sendErr := n.Send(context.Background(), makeNotification())
	if sendErr == nil {
		t.Fatal("Send(401 response) = nil, want error")
	}

	// Simulate what the sidecar does: log the classified error. The
	// message field must not embed the URL.
	logger.Error("send failed", slog.Any("error", sendErr))
	logOutput := getLog()
	if strings.Contains(logOutput, webhookURL) {
		t.Errorf("log output contains URL %q: %q", webhookURL, logOutput)
	}
	if strings.Contains(logOutput, secretToken) {
		t.Errorf("log output contains secret token: %q", logOutput)
	}
}

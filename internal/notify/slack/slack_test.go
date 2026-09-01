package slack

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
// values.
func makeNotification() domain.Notification {
	return domain.Notification{
		Envelope: domain.NotificationEnvelope{
			NotificationID: "notif-slack-001",
			Timestamp:      "2026-06-10T15:00:00Z",
			Source:         "slack-test-host",
			IssueID:        "issue-8",
			Identifier:     "PROJ-8",
			SessionID:      "sess-slack",
			Attempt:        new(1),
			Agent:          "claude-code",
		},
		Message: domain.NotificationMessage{
			Severity: "critical",
			Title:    "Blocker found",
			Body:     "Agent cannot proceed without operator input.",
			Category: "blocked",
		},
	}
}

func TestSlack_NewNotifier_MissingWebhookURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
	}{
		{name: "absent webhook_url key", config: map[string]any{}},
		{name: "empty string webhook_url", config: map[string]any{"webhook_url": ""}},
		{name: "whitespace only webhook_url", config: map[string]any{"webhook_url": "   "}},
		{name: "nil webhook_url value", config: map[string]any{"webhook_url": nil}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newNotifier(tt.config)
			if err == nil {
				t.Fatalf("newNotifier(%v) = nil error, want constructor error for missing webhook_url", tt.config)
			}
		})
	}
}

// TestSlack_NewNotifier_WrongTypeVsAbsentWebhookURL covers the distinction
// between a wrong-typed webhook_url and an absent one: the type fault
// carries the typed-fault message, while the absent key keeps its
// existing "webhook_url is required" message.
func TestSlack_NewNotifier_WrongTypeVsAbsentWebhookURL(t *testing.T) {
	t.Parallel()

	t.Run("webhook_url wrong type", func(t *testing.T) {
		t.Parallel()

		_, err := newNotifier(map[string]any{"webhook_url": 4242})

		if err == nil {
			t.Fatal("newNotifier(webhook_url=4242) error = nil, want error")
		}
		if err.Error() != "slack notifier: webhook_url: expected string, got integer" {
			t.Errorf("newNotifier(webhook_url=4242) error = %q, want %q", err.Error(), "slack notifier: webhook_url: expected string, got integer")
		}
	})

	t.Run("webhook_url absent", func(t *testing.T) {
		t.Parallel()

		_, err := newNotifier(map[string]any{})

		if err == nil {
			t.Fatal("newNotifier({}) error = nil, want error")
		}
		if err.Error() != "slack notifier: webhook_url is required" {
			t.Errorf("newNotifier({}) error = %q, want %q", err.Error(), "slack notifier: webhook_url is required")
		}
	})
}

func TestSlack_NewNotifier_ValidWebhookURL(t *testing.T) {
	t.Parallel()

	srv, _ := captureServer(t, http.StatusOK)

	n, err := newNotifier(map[string]any{"webhook_url": srv.URL})
	if err != nil {
		t.Fatalf("newNotifier(valid webhook_url) = %v, want nil", err)
	}
	if n == nil {
		t.Fatal("newNotifier(valid webhook_url) returned nil notifier")
	}
}

func TestSlack_Send_PostsSlackShapedBody(t *testing.T) {
	t.Parallel()

	srv, getBody := captureServer(t, http.StatusOK)

	n, err := newNotifier(map[string]any{"webhook_url": srv.URL})
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

	text, ok := m["text"].(string)
	if !ok || text == "" {
		t.Fatalf("body[\"text\"] = %v, want non-empty string", m["text"])
	}

	if !strings.Contains(text, notif.Message.Title) {
		t.Errorf("text %q does not contain title %q", text, notif.Message.Title)
	}
	if !strings.Contains(text, notif.Message.Body) {
		t.Errorf("text %q does not contain body %q", text, notif.Message.Body)
	}
	// Severity should appear in some form in the text field.
	severityUpper := strings.ToUpper(notif.Message.Severity)
	if !strings.Contains(strings.ToUpper(text), severityUpper) {
		t.Errorf("text %q does not contain severity %q (case-insensitive)", text, notif.Message.Severity)
	}
}

func TestSlack_Send_Non2xxReturnsClassifiedError(t *testing.T) {
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

			n, err := newNotifier(map[string]any{"webhook_url": srv.URL})
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

func TestSlack_Send_TransportFailureReturnsClassifiedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	n, err := newNotifier(map[string]any{"webhook_url": srv.URL})
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

func TestSlack_Send_ContextCancellationReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	n, err := newNotifier(map[string]any{"webhook_url": srv.URL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = n.Send(ctx, makeNotification())
	if err == nil {
		t.Fatal("Send(cancelled ctx) = nil, want error")
	}
}

func TestSlack_ErrorRedaction_URLAbsentFromError(t *testing.T) {
	t.Parallel()

	srv, _ := captureServer(t, http.StatusInternalServerError)
	const secretToken = "XXXX_SECRET"
	webhookURL := srv.URL + "/" + secretToken

	n, err := newNotifier(map[string]any{"webhook_url": webhookURL})
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

func TestSlack_ErrorRedaction_URLAbsentFromLog(t *testing.T) {
	t.Parallel()

	srv, _ := captureServer(t, http.StatusUnauthorized)
	const secretToken = "SECRET_TOKEN"
	webhookURL := srv.URL + "/" + secretToken

	logger, getLog := slogCapture()

	n, err := newNotifier(map[string]any{"webhook_url": webhookURL})
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}

	sendErr := n.Send(context.Background(), makeNotification())
	if sendErr == nil {
		t.Fatal("Send(401 response) = nil, want error")
	}

	logger.Error("send failed", slog.Any("error", sendErr))
	logOutput := getLog()
	if strings.Contains(logOutput, webhookURL) {
		t.Errorf("log output contains URL %q: %q", webhookURL, logOutput)
	}
	if strings.Contains(logOutput, secretToken) {
		t.Errorf("log output contains secret token: %q", logOutput)
	}
}

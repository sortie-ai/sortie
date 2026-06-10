// Package webhook implements [domain.Notifier] for a generic outbound
// HTTP webhook. It posts a normalized notification as a JSON object to
// an operator-configured URL, using the generic notifier vocabulary so
// the body is backend-neutral. This is an outbound POST backend and is
// distinct from inbound tracker webhook ingress.
//
// The package builds on [httpkit.Client] with a mandatory per-call
// timeout, classifies its own transport and non-2xx errors, and never
// logs the endpoint URL, request body, or response body.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
)

var _ domain.Notifier = (*notifier)(nil)

// sendTimeout bounds each outbound POST so a slow endpoint cannot stall
// the agent turn.
const sendTimeout = 10 * time.Second

func init() {
	registry.Notifiers.Register("webhook", newNotifier)
}

// notifier posts notifications to a configured webhook URL.
type notifier struct {
	client *httpkit.Client
}

// newNotifier constructs a webhook [domain.Notifier] from the entry's
// pass-through config. It requires a non-empty url; an absent or empty
// value (including a secret reference that resolved to the empty string)
// is rejected.
func newNotifier(config map[string]any) (domain.Notifier, error) {
	rawURL, _ := config["url"].(string)
	endpoint := strings.TrimSpace(rawURL)
	if endpoint == "" {
		return nil, fmt.Errorf("webhook notifier: url is required")
	}

	client := httpkit.NewClient(httpkit.ClientOptions{
		BaseURL:           endpoint,
		Timeout:           sendTimeout,
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: classifyTransportError,
	})
	return &notifier{client: client}, nil
}

// wirePayload is the JSON object posted to the webhook endpoint. Field
// names use the generic notifier vocabulary so any machine consumer can
// correlate and route without backend-specific knowledge.
type wirePayload struct {
	NotificationID string `json:"notification_id"`
	Timestamp      string `json:"timestamp"`
	Source         string `json:"source"`
	IssueID        string `json:"issue_id"`
	Identifier     string `json:"identifier"`
	SessionID      string `json:"session_id"`
	Attempt        *int   `json:"attempt"`
	Agent          string `json:"agent"`
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Category       string `json:"category,omitempty"`
}

// Send posts the notification as a JSON object and returns nil on any
// 2xx response. A non-2xx response or a transport failure yields a
// classified error that omits the URL, request body, and response body.
func (n *notifier) Send(ctx context.Context, notification domain.Notification) error {
	payload := wirePayload{
		NotificationID: notification.Envelope.NotificationID,
		Timestamp:      notification.Envelope.Timestamp,
		Source:         notification.Envelope.Source,
		IssueID:        notification.Envelope.IssueID,
		Identifier:     notification.Envelope.Identifier,
		SessionID:      notification.Envelope.SessionID,
		Attempt:        notification.Envelope.Attempt,
		Agent:          notification.Envelope.Agent,
		Severity:       notification.Message.Severity,
		Title:          notification.Message.Title,
		Body:           notification.Message.Body,
		Category:       notification.Message.Category,
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook notifier: encode payload: %w", err)
	}

	if _, err := n.client.Send(ctx, http.MethodPost, "", bytes.NewReader(encoded)); err != nil {
		return err
	}
	return nil
}

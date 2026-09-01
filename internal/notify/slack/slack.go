// Package slack implements [domain.Notifier] for a Slack incoming
// webhook. A Slack incoming webhook is itself an HTTP POST of JSON, so
// this backend differs from the generic webhook backend only in how it
// renders the body: it posts a Slack-shaped object with a text field.
//
// The package builds on [httpkit.Client] with a mandatory per-call
// timeout, classifies its own transport and non-2xx errors, and never
// logs the endpoint URL, request body, or response body.
package slack

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
	"github.com/sortie-ai/sortie/internal/typeutil"
)

var _ domain.Notifier = (*notifier)(nil)

// sendTimeout bounds each outbound POST so a slow endpoint cannot stall
// the agent turn.
const sendTimeout = 10 * time.Second

func init() {
	registry.Notifiers.Register("slack", newNotifier)
}

// notifier posts notifications to a configured Slack incoming webhook.
type notifier struct {
	client *httpkit.Client
}

// newNotifier constructs a slack [domain.Notifier] from the entry's
// pass-through config. It requires a non-empty webhook_url; an absent or
// empty value (including a secret reference that resolved to the empty
// string) is rejected.
func newNotifier(config map[string]any) (domain.Notifier, error) {
	rawURL, fault := typeutil.StringField(config, "webhook_url")
	if fault != nil {
		return nil, fmt.Errorf("slack notifier: %w", fault)
	}
	endpoint := strings.TrimSpace(rawURL)
	if endpoint == "" {
		return nil, fmt.Errorf("slack notifier: webhook_url is required")
	}

	client := httpkit.NewClient(httpkit.ClientOptions{
		BaseURL:           endpoint,
		Timeout:           sendTimeout,
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: classifyTransport,
	})
	return &notifier{client: client}, nil
}

// slackPayload is the body posted to a Slack incoming webhook. The text
// field renders the severity, title, and body.
type slackPayload struct {
	Text string `json:"text"`
}

// Send posts a Slack-shaped body and returns nil on any 2xx response.
// A non-2xx response or a transport failure yields a classified error
// that omits the URL, request body, and response body.
func (n *notifier) Send(ctx context.Context, notification domain.Notification) error {
	encoded, err := json.Marshal(slackPayload{Text: renderText(notification)})
	if err != nil {
		return fmt.Errorf("slack notifier: encode payload: %w", err)
	}

	if _, err := n.client.Send(ctx, http.MethodPost, "", bytes.NewReader(encoded)); err != nil {
		return err
	}
	return nil
}

// renderText composes the Slack message text from the message severity,
// title, and body.
func renderText(notification domain.Notification) string {
	severity := strings.ToUpper(notification.Message.Severity)
	return fmt.Sprintf("[%s] %s\n%s", severity, notification.Message.Title, notification.Message.Body)
}

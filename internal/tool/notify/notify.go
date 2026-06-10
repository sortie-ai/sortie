// Package notify implements [domain.AgentTool] for the notify_operator
// tool. The tool fills a system-owned envelope from session context the
// agent cannot forge, validates the agent-supplied message, enforces a
// per-session cap, and delivers a normalized [domain.Notification] to
// the configured backends in configuration order, stopping at the first
// backend that fails. It knows nothing about Slack or HTTP; backends
// arrive as a resolved slice of [domain.Notifier].
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

var _ domain.AgentTool = (*NotifyTool)(nil)

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "severity": { "type": "string", "enum": ["info", "warning", "critical"] },
    "title":    { "type": "string", "minLength": 1 },
    "body":     { "type": "string", "minLength": 1 },
    "category": { "type": "string", "enum": ["decision_needed", "progress", "blocked", "completed", "other"] }
  },
  "required": ["severity", "title", "body"],
  "additionalProperties": false
}`)

// validSeverities is the closed set of accepted severity values.
var validSeverities = map[string]bool{
	"info":     true,
	"warning":  true,
	"critical": true,
}

// validCategories is the closed set of accepted optional category values.
var validCategories = map[string]bool{
	"decision_needed": true,
	"progress":        true,
	"blocked":         true,
	"completed":       true,
	"other":           true,
}

// NotificationEnvelopeContext carries the system-owned envelope inputs
// read from the sidecar environment. The agent supplies none of these.
type NotificationEnvelopeContext struct {
	// IssueID is the tracker-internal issue id.
	IssueID string

	// Identifier is the human-readable issue key.
	Identifier string

	// SessionID is the agent session id; may be empty.
	SessionID string

	// Attempt is the retry or continuation attempt; nil on the first run.
	Attempt *int

	// Agent is the dispatch-frozen agent kind; may be empty.
	Agent string

	// Source identifies the Sortie instance. When empty, the tool
	// defaults it to the hostname at send time.
	Source string
}

// NotifyTool implements [domain.AgentTool] for notify_operator.
// Construct via [New] with the resolved backends and the session
// envelope context.
type NotifyTool struct {
	backends      []domain.Notifier
	env           NotificationEnvelopeContext
	maxPerSession int
	count         int
}

// New returns a [NotifyTool]. backends is the ordered set of resolved
// notifiers; the caller gates registration on a configured backend, so
// New panics when backends is empty (programming error). env carries
// the system-owned envelope context, and maxPerSession is the effective
// per-session cap after default resolution.
func New(backends []domain.Notifier, env NotificationEnvelopeContext, maxPerSession int) *NotifyTool {
	if len(backends) == 0 {
		panic("notify.New: backends must not be empty")
	}
	return &NotifyTool{
		backends:      backends,
		env:           env,
		maxPerSession: maxPerSession,
	}
}

// Name returns "notify_operator".
func (t *NotifyTool) Name() string { return "notify_operator" }

// Description returns a human-readable summary of the tool.
func (t *NotifyTool) Description() string {
	return "Send a real-time notification to the operator's configured channel. " +
		"Use this to escalate a decision you should not make alone, report progress " +
		"on a long task, or flag a blocker, without terminating the session. The " +
		"issue, session, and agent context is attached automatically."
}

// InputSchema returns the JSON Schema for notify_operator input. The
// agent supplies only the message; the envelope is system-owned and
// absent from the schema. The returned slice is a defensive copy.
func (t *NotifyTool) InputSchema() json.RawMessage {
	out := make(json.RawMessage, len(inputSchema))
	copy(out, inputSchema)
	return out
}

// toolInput is the agent-supplied message decoded from the tool call.
type toolInput struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Category string `json:"category,omitempty"`
}

// Execute validates the message, enforces the per-session cap, and
// delivers one [domain.Notification] to the configured backends in
// configuration order. The first backend that fails short-circuits the
// loop and yields a send_failed result; partial delivery across
// backends is not reported in this version. Domain failures are encoded
// in the JSON result with success: false and a nil Go error. The Go
// error return is reserved for a result-marshal failure.
func (t *NotifyTool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var in toolInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return errorResult("invalid_input", fmt.Sprintf("failed to parse input: %s", err))
	}
	if dec.More() {
		return errorResult("invalid_input", "unexpected trailing content after JSON object")
	}

	if !validSeverities[in.Severity] {
		return errorResult("invalid_input", "severity must be info, warning, or critical")
	}
	if in.Category != "" && !validCategories[in.Category] {
		return errorResult("invalid_input", "category is not a recognized value")
	}
	if in.Title == "" || in.Body == "" {
		return errorResult("invalid_input", "title and body must be non-empty")
	}

	if len(t.backends) == 0 {
		return errorResult("backend_unavailable", "no notification backend is configured")
	}

	if t.count >= t.maxPerSession {
		return errorResult("rate_limited", "per-session notification cap reached")
	}

	notification := domain.Notification{
		Envelope: t.buildEnvelope(),
		Message: domain.NotificationMessage{
			Severity: in.Severity,
			Title:    in.Title,
			Body:     in.Body,
			Category: in.Category,
		},
	}

	delivered := 0
	for _, backend := range t.backends {
		if err := backend.Send(ctx, notification); err != nil {
			return errorResult("send_failed", fmt.Sprintf("notification delivery failed: %s", err))
		}
		delivered++
	}

	t.count++
	return successResult(delivered, notification.Envelope.NotificationID)
}

// buildEnvelope generates the notification id and timestamp at call time
// and copies the session context from the stored envelope context. The
// agent cannot set any envelope field.
func (t *NotifyTool) buildEnvelope() domain.NotificationEnvelope {
	source := t.env.Source
	if source == "" {
		if host, err := os.Hostname(); err == nil {
			source = host
		}
	}

	return domain.NotificationEnvelope{
		NotificationID: newUUID(),
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Source:         source,
		IssueID:        t.env.IssueID,
		Identifier:     t.env.Identifier,
		SessionID:      t.env.SessionID,
		Attempt:        t.env.Attempt,
		Agent:          t.env.Agent,
	}
}

// successResult marshals the success envelope.
func successResult(delivered int, notificationID string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"success":         true,
		"delivered":       delivered,
		"notification_id": notificationID,
	})
}

// errorResult marshals the failure envelope with a closed-set kind and a
// redacted message. The kind is one of invalid_input, rate_limited,
// send_failed, or backend_unavailable.
func errorResult(kind, message string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"success": false,
		"error": map[string]string{
			"kind":    kind,
			"message": message,
		},
	})
}

// newUUID generates a random v4 UUID string using crypto/rand. Panics if
// the system random source is unavailable.
func newUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("notify: crypto/rand unavailable: %v", err))
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

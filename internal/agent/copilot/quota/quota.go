// Package quota reads a Copilot CLI account's quota windows: from the
// endpoint the runtime itself calls when a token is at hand, and from a
// one-shot headless runtime server otherwise. Both the hand-written Copilot
// adapter and the agent-client-protocol declaration share it; the caller
// supplies the [Runtime] that reaches the runtime's host. Start from [Pull]
// and [Source].
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// Source declares the quota windows both pull routes report: the share
// is what is left of the window.
var Source = usage.Source{
	Strategy: usage.StrategyPull,
	Windows: usage.WindowDeclaration{
		Share:          usage.SharePercent,
		ShareRemaining: true,
		Spans: map[string]usage.Span{
			"premium_interactions": {Period: "Monthly", Scope: "premium request"},
			"chat":                 {Period: "Monthly", Scope: "chat"},
			"completions":          {Period: "Monthly", Scope: "completion"},
		},
	},
}

// quotaEndpoint is the URL the runtime itself calls for the signed-in
// account's quota. It is a variable so a test can point it at a server.
var quotaEndpoint = "https://api.github.com/copilot_internal/user"

const providerBaseURLEnv = "COPILOT_PROVIDER_BASE_URL"

// MaxAnswerBytes bounds an answer read from the endpoint or from the headless
// server.
const MaxAnswerBytes = 1 << 20

// quotaTokenEnv lists the token variables in the runtime's precedence order.
var quotaTokenEnv = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

type rawQuotaAnswer struct {
	QuotaSnapshots    map[string]rawQuotaSnapshot `json:"quota_snapshots"`
	QuotaResetDateUTC string                      `json:"quota_reset_date_utc"`
}

type rawQuotaSnapshot struct {
	Unlimited        bool     `json:"unlimited"`
	PercentRemaining *float64 `json:"percent_remaining"`
	TimestampUTC     string   `json:"timestamp_utc"`
}

// Runtime is how a quota pull reaches the runtime it asks.
type Runtime interface {
	// Env returns a variable as the runtime receives it from Sortie.
	Env(name string) (string, bool)

	// Exchange runs the configured runtime with args on its host and hands the
	// command's standard streams to exchange; output above limit fails.
	Exchange(ctx context.Context, args []string, limit int64, exchange func(stdin io.Writer, stdout io.Reader) error) error
}

// Pull reads the account's Copilot quota windows. A custom provider spends no
// Copilot quota, so it ends the pulls before any request. The token goes only
// into the request header; an answer the endpoint refuses, or no token at all,
// falls back to asking the runtime itself.
func Pull(ctx context.Context, rt Runtime) ([]usage.WindowReading, error) {
	if base, _ := rt.Env(providerBaseURLEnv); base != "" {
		return nil, usage.ErrPullUnsupported
	}
	token := quotaToken(rt)
	if token == "" {
		return askHeadless(ctx, rt)
	}
	answer, status, err := fetchQuota(ctx, token)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return quotaReadings(answer), nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return askHeadless(ctx, rt)
	default:
		return nil, fmt.Errorf("copilot quota request: status %d", status)
	}
}

func quotaToken(rt Runtime) string {
	for _, name := range quotaTokenEnv {
		if token, _ := rt.Env(name); token != "" {
			return token
		}
	}
	return ""
}

// fetchQuota returns the decoded answer only for a 200 status; any other
// status comes back with a nil answer and no body read.
func fetchQuota(ctx context.Context, token string) (*rawQuotaAnswer, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, quotaEndpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("copilot quota request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("copilot quota request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var answer rawQuotaAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxAnswerBytes)).Decode(&answer); err != nil {
		return nil, 0, fmt.Errorf("decode copilot quota answer: %w", err)
	}
	return &answer, resp.StatusCode, nil
}

// quotaReadings skips an unlimited snapshot, which has no window to fill, and
// orders the rest by key so a pull is reproducible.
func quotaReadings(answer *rawQuotaAnswer) []usage.WindowReading {
	resetsAt := unixSeconds(answer.QuotaResetDateUTC)
	var readings []usage.WindowReading
	for _, key := range slices.Sorted(maps.Keys(answer.QuotaSnapshots)) {
		snapshot := answer.QuotaSnapshots[key]
		if snapshot.Unlimited {
			continue
		}
		readings = append(readings, usage.WindowReading{
			Window:     key,
			Share:      snapshot.PercentRemaining,
			ResetsAt:   resetsAt,
			ObservedAt: unixSeconds(snapshot.TimestampUTC),
		})
	}
	return readings
}

// unixSeconds returns the RFC 3339 instant as Unix seconds, or 0 when it is
// absent or unreadable.
func unixSeconds(instant string) int64 {
	t, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		return 0
	}
	return t.Unix()
}

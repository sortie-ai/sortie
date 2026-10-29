// Package usagelimits reads the console usage limits of an OpenCode
// subscription from the runtime's usage command and names the session errors
// that are a provider usage-limit refusal. It registers no kind and holds no
// adapter, so every route to the runtime shares one declaration, one parser,
// and one refusal vocabulary. Start from [Pull], [Refusal], and [RetryAt].
package usagelimits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	limitedStatus  = "limited"
	minutesPerWeek = 7 * 24 * 60
)

// The runtime's own names for a session error that is a usage-limit refusal.
const (
	FreeLimitError        = "provider.free-limit"
	QuotaError            = "provider.quota"
	BudgetLimitError      = "provider.budget-limit"
	ModelBudgetLimitError = "provider.model-budget-limit"
)

// Refusal reports whether name, matched exactly, is a usage-limit refusal and
// returns the limit it ends a turn with. Every refusal is model-only: one
// configuration reaches models of several providers, and the refusal comes
// from the provider serving the run's model. A throttle such as
// provider.rate-limit is not a refusal.
func Refusal(name string) (domain.UsageLimit, bool) {
	switch name {
	case FreeLimitError, QuotaError, BudgetLimitError, ModelBudgetLimitError:
		return domain.UsageLimit{ModelOnly: true}, true
	}
	return domain.UsageLimit{}, false
}

// ConsoleMetered reports whether the OpenCode console meters the refusal name,
// so an exhausted console window is evidence for it. QuotaError is not
// metered: any provider can send it.
func ConsoleMetered(name string) bool {
	switch name {
	case FreeLimitError, BudgetLimitError, ModelBudgetLimitError:
		return true
	}
	return false
}

// RetryAt returns the instant retryAfterMS after failedAtMS (Unix
// milliseconds), in UTC. It returns the zero time unless the failure time is
// positive and the delay is present and positive.
func RetryAt(failedAtMS int64, retryAfterMS *int64) time.Time {
	if failedAtMS <= 0 || retryAfterMS == nil || *retryAfterMS <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(failedAtMS + *retryAfterMS).UTC()
}

// Source declares the console limits the usage command reports.
var Source = usage.Source{
	Strategy: usage.StrategyPull,
	Windows: usage.WindowDeclaration{
		Share:    usage.SharePercent,
		Statuses: map[string]domain.UsageWindowStatus{limitedStatus: domain.UsageWindowExhausted},
		Spans: map[string]usage.Span{
			"rolling": {Period: "Rolling"},
			"weekly":  {Minutes: minutesPerWeek},
			"monthly": {Period: "Monthly"},
		},
	},
}

type rawMeter struct {
	Percent  *float64 `json:"percent"`
	Limited  bool     `json:"limited"`
	ResetsAt int64    `json:"resetsAt"` // Unix milliseconds
}

type rawGoLimits struct {
	Rolling *rawMeter `json:"rolling"`
	Weekly  *rawMeter `json:"weekly"`
	Monthly *rawMeter `json:"monthly"`
}

type rawLimitsBody struct {
	Go *rawGoLimits `json:"go"`
}

// rawLimitsAnswer is the usage command's answer: the limits sit under data
// when the runtime wraps them, and data is null before a console sign-in.
type rawLimitsAnswer struct {
	rawLimitsBody
	Data *rawLimitsBody `json:"data"`
}

// Pull runs the usage-limits command through run, which returns the
// command's standard output, and reads the answer. The command asks for a
// private server because the default one starts a background service that
// outlives the call and escapes the caller's process group. A run error wrapping
// [sshutil.ErrConnectionFailed] is returned unchanged, because a dropped
// connection says nothing about the runtime. Every other failure to get an
// answer wraps [usage.ErrPullUnsupported]: a build without the command, a
// missing sign-in, or a command outliving ctx does not start answering
// within the session.
func Pull(ctx context.Context, run func(ctx context.Context, args []string) ([]byte, error)) ([]usage.WindowReading, error) {
	out, err := run(ctx, []string{"api", "--standalone", "experimental.usage.limits"})
	if err != nil {
		if errors.Is(err, sshutil.ErrConnectionFailed) {
			return nil, err
		}
		return nil, fmt.Errorf("run opencode usage limits: %w: %w", usage.ErrPullUnsupported, err)
	}

	var answer rawLimitsAnswer
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("opencode usage limits printed no object: %w", usage.ErrPullUnsupported)
	}
	body := answer.rawLimitsBody
	if answer.Data != nil {
		body = *answer.Data
	}
	if body.Go == nil {
		return nil, fmt.Errorf("opencode usage limits reported no subscription: %w", usage.ErrPullUnsupported)
	}

	var readings []usage.WindowReading
	for _, member := range []struct {
		name  string
		meter *rawMeter
	}{
		{"rolling", body.Go.Rolling},
		{"weekly", body.Go.Weekly},
		{"monthly", body.Go.Monthly},
	} {
		if member.meter == nil {
			continue
		}
		reading := usage.WindowReading{
			Window:   member.name,
			Share:    member.meter.Percent,
			ResetsAt: member.meter.ResetsAt / 1000,
		}
		if member.meter.Limited {
			reading.Status = limitedStatus
		}
		readings = append(readings, reading)
	}
	return readings, nil
}

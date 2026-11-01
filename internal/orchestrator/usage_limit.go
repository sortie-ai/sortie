package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

// maxUsageLimitWait caps how far past now a reported reset can hold a kind.
// A garbled or distant instant would otherwise stop dispatch for as long as
// the value survives restarts.
const maxUsageLimitWait = 7 * 24 * time.Hour

// UsageLimitHoldKey names what a usage-limit hold stops: every model of an
// agent kind, or one configured model of it.
type UsageLimitHoldKey struct {
	AgentKind string

	// ModelOnly marks a hold on Model alone; false holds every model.
	ModelOnly bool

	// Model is the configured model a ModelOnly hold stops, empty for the
	// runtime's default model; always empty without ModelOnly.
	Model string
}

// UsageLimitHold stops new sessions of its key until a provider usage limit
// resets.
type UsageLimitHold struct {
	ResetsAt time.Time // UTC, already capped
	Since    time.Time // UTC; when the hold began
}

// isUsageLimitRefusal reports whether err unwraps to an agent error of kind
// [domain.ErrUsageLimit], whatever instant it carries.
func isUsageLimitRefusal(err error) bool {
	agentErr, ok := errors.AsType[*domain.AgentError](err)
	return ok && agentErr.Kind == domain.ErrUsageLimit
}

// usageLimitReset reports waits exactly when err is a usage-limit refusal
// whose reset is after now. reset is that instant capped at maxUsageLimitWait
// past now, and limit is the refusal's payload. It is the only place an
// agent-reported instant is validated.
func usageLimitReset(err error, now time.Time) (reset time.Time, limit *domain.UsageLimit, waits bool) {
	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok || agentErr.Kind != domain.ErrUsageLimit || agentErr.UsageLimit == nil {
		return time.Time{}, nil, false
	}
	if !agentErr.UsageLimit.ResetsAt.After(now) {
		return time.Time{}, nil, false
	}
	reset = agentErr.UsageLimit.ResetsAt.UTC()
	if latest := now.Add(maxUsageLimitWait); reset.After(latest) {
		reset = latest.UTC()
	}
	return reset, agentErr.UsageLimit, true
}

// recordUsageLimitHold stores the hold a refusal with the given reset gives
// key. A reset that does not move an active hold later leaves it as it is.
func recordUsageLimitHold(state *State, key UsageLimitHoldKey, reset, now time.Time) (hold UsageLimitHold, changed bool) {
	prior, found := state.UsageLimitHolds[key]
	switch {
	case !found || !prior.ResetsAt.After(now):
		hold = UsageLimitHold{ResetsAt: reset, Since: now.UTC()}
	case reset.After(prior.ResetsAt):
		hold = UsageLimitHold{ResetsAt: reset, Since: prior.Since}
	default:
		return prior, false
	}
	state.UsageLimitHolds[key] = hold
	return hold, true
}

// usageLimitGate reports whether a dispatch of kind configured with model is
// held, and by which hold. A hold on the kind and a hold on the kind's model
// each release the dispatch at its reset, or a grace after it unless ownRetry
// marks the dispatch as the refused issue's own retry; the dispatch is held
// while now is before the later release, which the gate returns with the hold
// that sets it.
func usageLimitGate(state *State, kind, model string, ownRetry bool, grace time.Duration, now time.Time) (key UsageLimitHoldKey, hold UsageLimitHold, release time.Time, held bool) {
	for _, candidate := range [...]UsageLimitHoldKey{
		{AgentKind: kind},
		{AgentKind: kind, ModelOnly: true, Model: model},
	} {
		found, ok := state.UsageLimitHolds[candidate]
		if !ok {
			continue
		}
		at := found.ResetsAt
		if !ownRetry {
			at = at.Add(grace)
		}
		if at.After(release) {
			key, hold, release = candidate, found, at
		}
	}
	return key, hold, release, now.Before(release)
}

// holdUsageLimit records and persists the hold a refusal gives key.
func holdUsageLimit(ctx context.Context, log *slog.Logger, store storage.UsageLimitHoldStore, state *State, key UsageLimitHoldKey, reset, now time.Time) UsageLimitHold {
	hold, changed := recordUsageLimitHold(state, key, reset, now)
	if !changed {
		return hold
	}
	row := storage.UsageLimitHold{
		AgentKind:   key.AgentKind,
		ModelOnly:   key.ModelOnly,
		Model:       key.Model,
		ResetsAtMs:  hold.ResetsAt.UnixMilli(),
		HeldSinceMs: hold.Since.UnixMilli(),
	}
	if err := store.SaveUsageLimitHold(ctx, row); err != nil {
		log.Error("failed to persist usage limit hold",
			slog.Any("error", err),
		)
	}
	log.Info("agent kind held by usage limit",
		slog.String("agent_kind", key.AgentKind),
		slog.Bool("model_only", key.ModelOnly),
		slog.String("model", key.Model),
		slog.Time("resets_at", hold.ResetsAt),
	)
	return hold
}

// clearUsageLimitHolds lifts the holds a run that ended normally proves open:
// the hold on its kind and the hold on its kind's configured model, each only
// when the run started after the hold began. A run started earlier proves
// nothing about the window after it.
func clearUsageLimitHolds(ctx context.Context, log *slog.Logger, store storage.UsageLimitHoldStore, state *State, entry *RunningEntry) {
	for _, key := range [...]UsageLimitHoldKey{
		{AgentKind: entry.AgentKind},
		{AgentKind: entry.AgentKind, ModelOnly: true, Model: entry.ConfiguredModel},
	} {
		hold, found := state.UsageLimitHolds[key]
		if !found || !entry.StartedAt.After(hold.Since) {
			continue
		}
		delete(state.UsageLimitHolds, key)
		if err := store.DeleteUsageLimitHold(ctx, key.AgentKind, key.ModelOnly, key.Model); err != nil {
			log.Error("failed to delete usage limit hold",
				slog.Any("error", err),
			)
		}
		log.Info("usage limit hold cleared",
			slog.String("agent_kind", key.AgentKind),
			slog.Bool("model_only", key.ModelOnly),
			slog.String("model", key.Model),
		)
	}
}

// usageLimitHoldActive reports whether any hold still holds a dispatch of its
// key at now, once grace is counted.
func usageLimitHoldActive(state *State, grace time.Duration, now time.Time) bool {
	for _, hold := range state.UsageLimitHolds {
		if now.Before(hold.ResetsAt.Add(grace)) {
			return true
		}
	}
	return false
}

// usageLimitHeldEntry returns the hold entry of a poll-tick dispatch of kind
// configured with model, or nil when no usage limit holds it.
func usageLimitHeldEntry(state *State, kind, model string, grace time.Duration, now time.Time) *HeldEntry {
	key, hold, _, held := usageLimitGate(state, kind, model, false, grace, now)
	if !held {
		return nil
	}
	return &HeldEntry{
		Reason:    SkipUsageLimit,
		ResetsAt:  hold.ResetsAt,
		AgentKind: key.AgentKind,
		ModelOnly: key.ModelOnly,
		Model:     key.Model,
	}
}

// usageLimitNotifiedCtxKey is the context key for the usage-limit episode
// flag of a dispatch.
type usageLimitNotifiedCtxKey struct{}

// withUsageLimitNotified marks a dispatch as a retry inside a notified
// usage-limit episode. The flag travels on the context because the worker
// builds its dependencies on its own goroutine, where the running entry is
// not safe to read.
func withUsageLimitNotified(ctx context.Context) context.Context {
	return context.WithValue(ctx, usageLimitNotifiedCtxKey{}, true)
}

// usageLimitNotifiedFrom reports whether ctx carries the mark of
// [withUsageLimitNotified].
func usageLimitNotifiedFrom(ctx context.Context) bool {
	notified, _ := ctx.Value(usageLimitNotifiedCtxKey{}).(bool)
	return notified
}

// usageLimitRetryDue returns the instant a refused issue's retry fires: the
// hold's reset, or the end of the ordinary backoff when that is later, which
// bounds the cadence when a refusal keeps reporting a stale reset.
func usageLimitRetryDue(hold UsageLimitHold, now time.Time, backoffMS int64) time.Time {
	due := hold.ResetsAt
	if floor := now.Add(time.Duration(backoffMS) * time.Millisecond); floor.After(due) {
		return floor
	}
	return due
}

// carryRefusalEpisode gives the retry an exit deferred to the exit's place in
// the refusal episode, and persists it when that changes the flag. A known
// reaction retry has no persisted row.
func carryRefusalEpisode(ctx context.Context, log *slog.Logger, store storage.RetryEntryStore, state *State, issueID string, refused bool) {
	incumbent, ok := state.RetryAttempts[issueID]
	if !ok || incumbent.UsageLimitNotified == refused {
		return
	}
	incumbent.UsageLimitNotified = refused
	if isKnownReactionKind(incumbent.ReactionKind) {
		return
	}
	if err := store.SaveRetryEntry(ctx, retryEntryRow(incumbent)); err != nil {
		log.Error("failed to persist retry entry",
			slog.Any("error", err),
		)
	}
}

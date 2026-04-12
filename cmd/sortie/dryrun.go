package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// runDryRun executes a single poll cycle in read-only mode: fetches
// candidate issues, computes dispatch eligibility, logs results, and
// returns an exit code. No database is opened, no agents are spawned,
// and no state is written. The caller constructs and defers closing
// the tracker adapter.
func runDryRun(ctx context.Context, cfg config.ServiceConfig, logger *slog.Logger, trackerAdapter domain.TrackerAdapter) int {
	issues, err := trackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		logger.Error("dry-run: failed to fetch candidate issues", slog.Any("error", err))
		return 1
	}

	sorted := orchestrator.SortForDispatch(issues)

	state := orchestrator.NewState(
		cfg.Polling.IntervalMS,
		cfg.Agent.MaxConcurrentAgents,
		cfg.Agent.MaxConcurrentByState,
		orchestrator.AgentTotals{},
	)

	wc := orchestrator.ParseWorkerConfig(cfg.Extensions)
	for _, w := range wc.Warnings {
		logger.LogAttrs(ctx, slog.LevelWarn, w.Message, w.Attrs...) //nolint:sloglint // WorkerWarning.Message is one of two fixed string constants from parseSSHStrictHostKeyChecking
	}
	hostPool := orchestrator.NewHostPool(wc.SSHHosts, wc.MaxPerHost)

	activeSet := dryRunStateSet(cfg.Tracker.ActiveStates)
	terminalSet := dryRunStateSet(cfg.Tracker.TerminalStates)

	var eligible, ineligible int
	for i, issue := range sorted {
		globalAvail := orchestrator.GlobalAvailableSlots(
			state.MaxConcurrentAgents, len(state.Running))

		if hostPool.IsSSHEnabled() && !hostPool.HasCapacity() {
			for _, remaining := range sorted[i:] {
				ineligible++
				logger.Info("dry-run: candidate",
					slog.String("issue_id", remaining.ID),
					slog.String("issue_identifier", remaining.Identifier),
					slog.String("state", remaining.State),
					slog.Bool("would_dispatch", false),
					slog.String("skip_reason", "ssh_hosts_at_capacity"),
				)
			}
			break
		}

		stateRunning := orchestrator.RunningCountByState(state.Running, issue.State)
		stateAvail := orchestrator.StateAvailableSlots(
			issue.State, state.MaxConcurrentByState, stateRunning, globalAvail)

		wouldDispatch := orchestrator.ShouldDispatchWithSets(
			issue, state, activeSet, terminalSet) && globalAvail > 0 && stateAvail > 0

		if wouldDispatch && hostPool.IsSSHEnabled() {
			_, ok := hostPool.AcquireHost(issue.ID, "")
			if !ok {
				wouldDispatch = false
			}
		}

		logFields := []any{
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("title", issue.Title),
			slog.String("state", issue.State),
			slog.Bool("would_dispatch", wouldDispatch),
			slog.Int("global_slots_available", globalAvail),
			slog.Int("state_slots_available", stateAvail),
		}
		if issue.Priority != nil {
			logFields = append(logFields, slog.Int("priority", *issue.Priority))
		}
		if hostPool.IsSSHEnabled() {
			logFields = append(logFields, slog.String("ssh_host", hostPool.HostFor(issue.ID)))
		}

		logger.Info("dry-run: candidate", logFields...)

		if wouldDispatch {
			eligible++
			state.Claimed[issue.ID] = struct{}{}
			state.Running[issue.ID] = &orchestrator.RunningEntry{
				Identifier: issue.Identifier,
				Issue:      issue,
			}
		} else {
			ineligible++
		}
	}

	logger.Info("dry-run: complete",
		slog.Int("candidates_fetched", len(issues)),
		slog.Int("would_dispatch", eligible),
		slog.Int("ineligible", ineligible),
		slog.Int("max_concurrent_agents", cfg.Agent.MaxConcurrentAgents),
	)

	return 0
}

// dryRunStateSet builds a set of lowercase state names for O(1) membership
// testing. Mirrors orchestrator.stateSet which is unexported.
func dryRunStateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, s := range states {
		set[strings.ToLower(s)] = struct{}{}
	}
	return set
}

package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/server"
)

// runHistoryReader serves the HTTP server's run history from the store, so
// the server never names a persistence type.
type runHistoryReader struct {
	store *persistence.Store
}

var _ server.RunHistoryReader = runHistoryReader{}

// QueryRuns returns the runs matching q, newest first.
func (r runHistoryReader) QueryRuns(ctx context.Context, q server.RunHistoryQuery) ([]server.RunHistoryEntry, error) {
	runs, err := r.store.QueryRunHistory(ctx, persistence.RunHistoryFilter{
		IssueID:         q.IssueID,
		Identifier:      q.Identifier,
		RuleName:        q.RuleName,
		Status:          q.Status,
		StageTarget:     q.StageTarget,
		CompletedSince:  q.Since,
		CompletedBefore: q.Until,
		EndedSince:      q.EndedSince,
		EndedBefore:     q.EndedUntil,
		HasDispatchID:   q.HasDispatchID,
		BeforeID:        q.BeforeID,
		Limit:           q.Limit,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}
	parts, err := r.store.QueryRunModelUsage(ctx, ids)
	if err != nil {
		return nil, err
	}
	amounts, err := r.store.QueryRunReportedAmounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]server.RunHistoryEntry, len(runs))
	for i, run := range runs {
		out[i] = runHistoryEntry(run, parts[run.ID], amounts[run.ID])
	}
	return out, nil
}

func runModelUsage(parts []persistence.RunModelPart) []server.RunModelUsage {
	out := make([]server.RunModelUsage, len(parts))
	for i, p := range parts {
		out[i] = server.RunModelUsage{
			Model: p.Model,
			Tokens: domain.TokenUsage{
				InputTokens:      p.Tokens.InputTokens,
				OutputTokens:     p.Tokens.OutputTokens,
				TotalTokens:      p.Tokens.InputTokens + p.Tokens.OutputTokens,
				CacheReadTokens:  p.Tokens.CacheReadTokens,
				CacheWriteTokens: p.Tokens.CacheWriteTokens,
			},
			EstimatedCostUSD: p.EstimatedCostUSD,
			CostStatus:       p.CostStatus,
			UnratedClasses:   p.UnratedClasses,
			UnratedRecorded:  p.UnratedRecorded,
			PriceSource:      p.PriceSource,
		}
	}
	return out
}

// QueryIssueGroups returns the issues whose runs match q, newest first.
func (r runHistoryReader) QueryIssueGroups(ctx context.Context, q server.IssueGroupQuery) ([]server.IssueGroupKey, error) {
	groups, err := r.store.QueryIssueGroups(ctx, persistence.IssueGroupFilter{
		IssueKey:    q.IssueKey,
		Search:      q.Search,
		RuleName:    q.RuleName,
		Status:      q.Status,
		StageTarget: q.StageTarget,
		BeforeID:    q.BeforeID,
		Limit:       q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]server.IssueGroupKey, len(groups))
	for i, g := range groups {
		out[i] = server.IssueGroupKey{IssueID: g.IssueID, NewestRunID: g.NewestRunID}
	}
	return out, nil
}

// IssueTotals sums the issue's spend over all of its recorded runs. An issue
// with no run yields zero totals.
func (r runHistoryReader) IssueTotals(ctx context.Context, issueID string) (server.IssueRunTotals, error) {
	usage, err := r.store.TokenUsageByIssue(ctx, issueID)
	if err != nil {
		return server.IssueRunTotals{}, fmt.Errorf("issue run totals: %w", err)
	}
	provenance, err := r.store.IssueSpendProvenance(ctx, issueID)
	if err != nil {
		return server.IssueRunTotals{}, fmt.Errorf("issue spend provenance: %w", err)
	}
	return server.IssueRunTotals{
		Usage: *orchestrator.SnapshotIssueUsageOf(&usage),
		Tokens: domain.TokenUsage{
			InputTokens:      usage.InputTokens,
			OutputTokens:     usage.OutputTokens,
			CacheReadTokens:  usage.CacheReadTokens,
			CacheWriteTokens: usage.CacheWriteTokens,
		},
		Provenance: issueProvenance(provenance),
	}, nil
}

// issueProvenance maps what an issue's recorded runs say about the origin of
// their figures to the server reader type.
func issueProvenance(p persistence.IssueSpendProvenance) server.IssueProvenance {
	amounts := make([]server.UnitAmount, len(p.VendorAmounts))
	for i, a := range p.VendorAmounts {
		amounts[i] = server.UnitAmount{Unit: a.Unit, Amount: a.Amount, LowerBound: a.LowerBound}
	}
	return server.IssueProvenance{
		UnmeasuredCause:   string(p.UnmeasuredCause),
		UnmeasuredUnits:   p.UnmeasuredUnits,
		VendorAmounts:     amounts,
		AgentPricedUSD:    p.AgentPricedUSD,
		AgentPricedModels: p.AgentPricedModels,
	}
}

// RunEvents returns one run's stored events, decoded, ascending by seq: all
// when newest is 0, else the newest up to newest.
func (r runHistoryReader) RunEvents(ctx context.Context, dispatchID string, newest int) ([]orchestrator.RunEvent, error) {
	rows, err := r.store.RunEvents(ctx, dispatchID, newest)
	if err != nil {
		return nil, err
	}
	return orchestrator.RunEventsFromRows(rows)
}

// IssueOutcomes returns the outcome of each listed issue that has one.
func (r runHistoryReader) IssueOutcomes(ctx context.Context, issueIDs []string) (map[string]server.IssueOutcome, error) {
	stored, err := r.store.IssueOutcomes(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]server.IssueOutcome, len(stored))
	for id, o := range stored {
		out[id] = server.IssueOutcome{
			Kind:              o.Kind,
			ObservedAt:        o.ObservedAt,
			State:             o.State,
			MergeReactionKind: o.MergeReactionKind,
			PRNumber:          o.PRNumber,
			PRRepository:      o.PRRepository,
		}
	}
	return out, nil
}

// PassActivities returns what the reactions recorded in each listed pass.
func (r runHistoryReader) PassActivities(ctx context.Context, passes []server.PassRef) (map[server.PassKey]server.PassActivity, error) {
	refs := make([]persistence.PassRef, len(passes))
	for i, p := range passes {
		refs[i] = persistence.PassRef{IssueID: p.IssueID, ChainID: p.ChainID, DispatchedAt: p.DispatchedAt}
	}
	stored, err := r.store.PassActivities(ctx, refs)
	if err != nil {
		return nil, err
	}
	out := make(map[server.PassKey]server.PassActivity, len(stored))
	for key, activity := range stored {
		kinds := make(map[string]server.KindActivity, len(activity.Kinds))
		for kind, k := range activity.Kinds {
			kinds[kind] = server.KindActivity{
				Watched:       k.Watched,
				Continuations: k.Continuations,
				Acted:         k.Acted,
				Escalated:     k.Escalated,
			}
		}
		out[server.PassKey{IssueID: key.IssueID, ChainID: key.ChainID}] = server.PassActivity{Recorded: activity.Recorded, Kinds: kinds}
	}
	return out, nil
}

func runHistoryEntry(r persistence.RunHistory, parts []persistence.RunModelPart, amounts []persistence.ReportedAmount) server.RunHistoryEntry {
	entry := server.RunHistoryEntry{
		Identifier:       r.Identifier,
		DisplayID:        r.DisplayID,
		Attempt:          r.Attempt,
		Status:           r.Status,
		WorkflowFile:     r.WorkflowFile,
		StartedAt:        r.StartedAt,
		CompletedAt:      r.CompletedAt,
		Error:            r.Error,
		TurnsCompleted:   r.TurnsCompleted,
		RuleName:         r.RuleName,
		ChainID:          r.ChainID,
		StagePrevious:    r.StagePrevious,
		StageTarget:      r.StageTarget,
		StageResult:      r.StageResult,
		ID:               r.ID,
		IssueID:          r.IssueID,
		IssueTitle:       r.IssueTitle,
		IssueURL:         r.IssueURL,
		AgentKind:        r.AgentAdapter,
		ConfiguredModel:  r.ConfiguredModel,
		ConfiguredEffort: r.ConfiguredEffort,
		ReportedModel:    r.ReportedModel,
		DispatchedAt:     r.DispatchedAt,
		Tokens: domain.TokenUsage{
			InputTokens:      r.InputTokens,
			OutputTokens:     r.OutputTokens,
			TotalTokens:      r.TotalTokens,
			CacheReadTokens:  r.CacheReadTokens,
			CacheWriteTokens: r.CacheWriteTokens,
		},
		TokensMeasured:       r.TokensMeasured,
		EstimatedCostUSD:     r.EstimatedCostUSD,
		CostStatus:           r.CostStatus,
		StagePreviousOutcome: r.StagePreviousOutcome,
		ReviewMetadata:       r.ReviewMetadata,
		Skills:               r.Skills,
		OriginRecorded:       r.OriginRecorded,
		Trigger:              r.Trigger,
		ReactionKind:         r.ReactionKind,
		SessionID:            r.SessionID,
		SSHHost:              r.SSHHost,
		DispatchID:           r.DispatchID,
		DeclaredOutcome:      r.DeclaredOutcome,
		AgentText:            r.AgentText,
		UsageCompleteness:    r.Completeness(),
		ModelUsage:           runModelUsage(parts),
	}
	// A row recorded before provenance was kept has no sources or amounts to
	// serve; an empty answer there would claim the run had none.
	if r.UsageCompleteness != "" {
		entry.UsageProvenance = usagePairs(r.UsageProvenance)
		entry.VendorAmounts = vendorAmounts(amounts)
	}
	if entry.UsageCompleteness == persistence.UsageUnmeasured {
		units := reportedUnits(amounts)
		cause := persistence.UnmeasuredCauseOf(r.Status, r.UsageCompleteness, units)
		entry.UnmeasuredCause = string(cause)
		entry.UnmeasuredUnits = []string{}
		if cause == persistence.UnmeasuredAmountsOnly {
			entry.UnmeasuredUnits = units
		}
	}
	return entry
}

func usagePairs(text string) []server.UsagePair {
	parsed := persistence.ParseUsageProvenance(text)
	pairs := make([]server.UsagePair, len(parsed))
	for i, p := range parsed {
		pairs[i] = server.UsagePair{Source: p.Source, Strategy: p.Strategy}
	}
	return pairs
}

// vendorAmounts keeps the amounts in a unit other than US dollars, in the
// order the store returned them. US dollars are already priced into the
// run's cost.
func vendorAmounts(amounts []persistence.ReportedAmount) []server.VendorAmount {
	out := make([]server.VendorAmount, 0, len(amounts))
	for _, a := range amounts {
		if a.Unit == domain.UnitUSD {
			continue
		}
		out = append(out, server.VendorAmount{Unit: a.Unit, Model: a.Model, Amount: a.Amount, LowerBound: a.LowerBound})
	}
	return out
}

// reportedUnits lists the distinct units of amounts in ascending order, US
// dollars included.
func reportedUnits(amounts []persistence.ReportedAmount) []string {
	units := make([]string, len(amounts))
	for i, a := range amounts {
		units[i] = a.Unit
	}
	slices.Sort(units)
	return slices.Compact(units)
}

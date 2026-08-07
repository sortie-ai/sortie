package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/server"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// statsNow follows the serverShutdownTimeout precedent for a
// test-overridable package variable, so a test can pin generated_at and
// any relative --since or --until bound.
var statsNow = time.Now

// statsReport is the "sortie stats --format json" envelope. Every slice
// field is never nil; every pointer field is null when the figure it
// carries is unavailable.
type statsReport struct {
	GeneratedAt  string           `json:"generated_at"`
	WorkflowPath string           `json:"workflow_path"`
	DBPath       string           `json:"db_path"`
	Since        *string          `json:"since"`
	Until        *string          `json:"until"`
	SchemaTier   string           `json:"schema_tier"`
	Warnings     []string         `json:"warnings"`
	Summary      statsSummary     `json:"summary"`
	ByStatus     []statsGroup     `json:"by_status"`
	ByAdapter    []statsGroup     `json:"by_adapter"`
	ByRule       []statsGroup     `json:"by_rule"`
	ByTemplate   []statsGroup     `json:"by_template"`
	SelfReview   *statsSelfReview `json:"self_review"`
}

// statsSummary reports the report-wide figures. MeanTurnsSucceeded,
// Tokens, CostUSD, and CostPerSucceededRunUSD are null on the base
// schema tier; CostUSD and CostPerSucceededRunUSD are also null when
// no priced run exists in range.
type statsSummary struct {
	Runs                   int           `json:"runs"`
	Succeeded              int           `json:"succeeded"`
	SuccessRate            float64       `json:"success_rate"`
	Duration               statsDuration `json:"duration_seconds"`
	MeanTurnsSucceeded     *float64      `json:"mean_turns_succeeded"`
	Tokens                 *statsTokens  `json:"tokens"`
	CostUSD                *float64      `json:"cost_usd"`
	CostPerSucceededRunUSD *float64      `json:"cost_per_succeeded_run_usd"`
	ZeroDurationRuns       int           `json:"zero_duration_runs"`
	DurationExcludedRuns   int           `json:"duration_excluded_runs"`
	CostUnpricedRuns       int           `json:"cost_unpriced_runs"`
}

// statsDuration reports p50, p95, and the arithmetic mean over a sample
// population, in seconds. All three are null when Samples is 0.
type statsDuration struct {
	P50     *float64 `json:"p50"`
	P95     *float64 `json:"p95"`
	Mean    *float64 `json:"mean"`
	Samples int      `json:"samples"`
}

// statsTokens reports the four token sums as stored, never recomputed.
type statsTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Total     int64 `json:"total"`
	CacheRead int64 `json:"cache_read"`
}

// statsGroup is one row of a by_status, by_adapter, by_rule, or
// by_template breakdown. MeanTurns, Tokens, CostUSD, and
// CostPerSucceededRunUSD are null on the base schema tier; CostUSD and
// CostPerSucceededRunUSD are also null when the group holds no priced
// run. In by_status, Succeeded and SuccessRate are structural rather
// than informative: the "succeeded" row necessarily reports Succeeded
// equal to Runs.
type statsGroup struct {
	Name                   string        `json:"name"`
	Runs                   int           `json:"runs"`
	Succeeded              int           `json:"succeeded"`
	SuccessRate            float64       `json:"success_rate"`
	Share                  float64       `json:"share"`
	Duration               statsDuration `json:"duration_seconds"`
	MeanTurns              *float64      `json:"mean_turns"`
	Tokens                 *statsTokens  `json:"tokens"`
	CostUSD                *float64      `json:"cost_usd"`
	CostPerSucceededRunUSD *float64      `json:"cost_per_succeeded_run_usd"`
}

// statsSelfReview reports the self-review aggregation. MeanIterations is
// null when RunsWithMetadata is 0. Present only on the full schema tier.
type statsSelfReview struct {
	RunsWithMetadata int                 `json:"runs_with_metadata"`
	ByFinalVerdict   []statsVerdictCount `json:"by_final_verdict"`
	CapReachedRuns   int                 `json:"cap_reached_runs"`
	MeanIterations   *float64            `json:"mean_iterations"`
	UnparsedRuns     int                 `json:"unparsed_runs"`
}

// statsVerdictCount is one entry of statsSelfReview.ByFinalVerdict.
type statsVerdictCount struct {
	Verdict string `json:"verdict"`
	Runs    int    `json:"runs"`
}

// tier reports the report's schema_tier label for caps.
func tier(caps persistence.RunHistoryCapabilities) string {
	if caps.Full() {
		return "full"
	}
	return "base"
}

// label maps an empty dimension value to the sentinel a reader sees for
// a run that carries no adapter, rule, template, or (in principle) status.
func label(v string) string {
	if v == "" {
		return "<none>"
	}
	return v
}

// duration computes row's duration in seconds from its stored RFC3339
// timestamps. ok is false when either timestamp fails to parse or the
// resulting difference is negative.
func duration(row persistence.RunStatsRow) (seconds float64, ok bool) {
	started, err := time.Parse(time.RFC3339, row.StartedAt)
	if err != nil {
		return 0, false
	}
	completed, err := time.Parse(time.RFC3339, row.CompletedAt)
	if err != nil {
		return 0, false
	}
	diff := completed.Sub(started).Seconds()
	if diff < 0 {
		return 0, false
	}
	return diff, true
}

// percentile returns the nearest-rank percentile p over sorted, which
// MUST already be sorted ascending. No interpolation is performed.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	idx := int(math.Ceil(p/100*float64(n))) - 1
	idx = max(0, min(idx, n-1))
	return sorted[idx]
}

// priceOf returns the estimated USD cost for row under rates, or nil
// when row's agent adapter has no rate entry.
func priceOf(row persistence.RunStatsRow, rates server.TokenRates) *float64 {
	rc, ok := rates[row.AgentAdapter]
	if !ok {
		return nil
	}
	return server.EstimateCost(row.InputTokens, row.OutputTokens, row.CacheReadTokens, &rc)
}

// statsSelfReviewAccum accumulates the self-review folding state across
// every row that carries non-nil review metadata.
type statsSelfReviewAccum struct {
	runsWithMetadata int
	byVerdict        map[string]int
	capReachedRuns   int
	iterationSum     int
	unparsedRuns     int
}

// foldSelfReview unmarshals raw into a [domain.ReviewMetadata] and folds
// the result into acc. A value that fails to unmarshal increments acc's
// unparsed count and contributes to nothing else.
func foldSelfReview(raw string, acc *statsSelfReviewAccum) {
	var metadata domain.ReviewMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		acc.unparsedRuns++
		return
	}

	acc.runsWithMetadata++
	verdict := metadata.FinalVerdict
	if verdict == "" {
		verdict = "none"
	}
	acc.byVerdict[verdict]++
	if metadata.CapReached {
		acc.capReachedRuns++
	}
	acc.iterationSum += metadata.TotalIterations
}

// parseRangeBound parses a --since or --until flag value into a UTC
// time bound. An empty string returns nil, nil, meaning the bound is
// absent. raw MUST be an RFC3339 timestamp, a YYYY-MM-DD date (read as
// 00:00:00Z on that date), or a positive Go duration (read as
// statsNow() minus that duration); any other input, including a zero
// or negative duration, is a usage error.
func parseRangeBound(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		bound := t.UTC()
		return &bound, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		bound := t.UTC()
		return &bound, nil
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		bound := statsNow().UTC().Add(-d)
		return &bound, nil
	}
	return nil, fmt.Errorf(
		"invalid range bound %q: accepts an RFC3339 timestamp (2026-07-01T00:00:00Z), "+
			"a date (2026-07-01), or a positive duration (24h)", raw)
}

// statsGroupAccum accumulates the folding state for one statsGroup row
// before the derived figures are computed in statsAggregator.report.
type statsGroupAccum struct {
	name            string
	runs            int
	succeeded       int
	turnSum         int
	tokens          statsTokens
	costUSD         float64
	pricedRuns      int
	durationSamples []float64
}

// statsTotalAccum accumulates the report-wide folding state.
type statsTotalAccum struct {
	runs                 int
	succeeded            int
	succeededTurns       int
	tokens               statsTokens
	costUSD              float64
	pricedRuns           int
	costUnpricedRuns     int
	zeroDurationRuns     int
	durationExcludedRuns int
	succeededSamples     []float64
}

// statsAggregator folds a stream of [persistence.RunStatsRow] values,
// via [statsAggregator.add], into the totals and per-dimension groups
// [statsAggregator.report] renders into a [statsReport]. add touches no
// [persistence.Store] method, as [persistence.Store.ScanRunHistoryRange]'s
// visit callback requires.
type statsAggregator struct {
	caps            persistence.RunHistoryCapabilities
	rates           server.TokenRates
	ratesConfigured bool
	tierLabel       string

	total statsTotalAccum

	byStatus   map[string]*statsGroupAccum
	byAdapter  map[string]*statsGroupAccum
	byRule     map[string]*statsGroupAccum
	byTemplate map[string]*statsGroupAccum

	selfReview statsSelfReviewAccum
}

// newStatsAggregator constructs a statsAggregator for caps and rates.
// It derives the schema tier and whether rates are configured once,
// from caps.Full and from rates holding at least one entry.
func newStatsAggregator(caps persistence.RunHistoryCapabilities, rates server.TokenRates) *statsAggregator {
	return &statsAggregator{
		caps:            caps,
		rates:           rates,
		ratesConfigured: len(rates) > 0,
		tierLabel:       tier(caps),
		byStatus:        make(map[string]*statsGroupAccum),
		byAdapter:       make(map[string]*statsGroupAccum),
		byRule:          make(map[string]*statsGroupAccum),
		byTemplate:      make(map[string]*statsGroupAccum),
		selfReview:      statsSelfReviewAccum{byVerdict: make(map[string]int)},
	}
}

// bumpStatsGroup returns groups' entry for name, creating it on first
// use, and increments its run count and, when isSucceeded, its
// succeeded count.
func bumpStatsGroup(groups map[string]*statsGroupAccum, name string, isSucceeded bool) *statsGroupAccum {
	g, ok := groups[name]
	if !ok {
		g = &statsGroupAccum{name: name}
		groups[name] = g
	}
	g.runs++
	if isSucceeded {
		g.succeeded++
	}
	return g
}

// add folds one row into the aggregator. It is the visit callback
// [persistence.Store.ScanRunHistoryRange] calls once per row; it calls
// no Store method and always returns nil.
func (a *statsAggregator) add(row persistence.RunStatsRow) error {
	isSucceeded := row.Status == "succeeded"
	a.total.runs++
	if isSucceeded {
		a.total.succeeded++
	}

	bumped := []*statsGroupAccum{
		bumpStatsGroup(a.byStatus, label(row.Status), isSucceeded),
		bumpStatsGroup(a.byAdapter, label(row.AgentAdapter), isSucceeded),
	}

	full := a.caps.Full()
	if full {
		bumped = append(bumped,
			bumpStatsGroup(a.byRule, label(row.RuleName), isSucceeded),
			bumpStatsGroup(a.byTemplate, label(row.TemplateID), isSucceeded),
		)

		for _, g := range bumped {
			g.tokens.Input += row.InputTokens
			g.tokens.Output += row.OutputTokens
			g.tokens.Total += row.TotalTokens
			g.tokens.CacheRead += row.CacheReadTokens
			g.turnSum += row.TurnsCompleted
		}
		a.total.tokens.Input += row.InputTokens
		a.total.tokens.Output += row.OutputTokens
		a.total.tokens.Total += row.TotalTokens
		a.total.tokens.CacheRead += row.CacheReadTokens
		if isSucceeded {
			a.total.succeededTurns += row.TurnsCompleted
		}

		if a.ratesConfigured {
			cost := priceOf(row, a.rates)
			if cost == nil {
				a.total.costUnpricedRuns++
			} else {
				a.total.costUSD += *cost
				a.total.pricedRuns++
				for _, g := range bumped {
					g.costUSD += *cost
					g.pricedRuns++
				}
			}
		}

		if row.ReviewMetadata != nil {
			foldSelfReview(*row.ReviewMetadata, &a.selfReview)
		}
	}

	seconds, ok := duration(row)
	if !ok {
		a.total.durationExcludedRuns++
		return nil
	}
	if seconds == 0 {
		a.total.zeroDurationRuns++
	}
	for _, g := range bumped {
		g.durationSamples = append(g.durationSamples, seconds)
	}
	if isSucceeded {
		a.total.succeededSamples = append(a.total.succeededSamples, seconds)
	}

	return nil
}

// round1, round2, and round4 round v to the named number of decimal
// places so the JSON report is byte-stable.
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// durationStats computes p50, p95, and the mean over samples, in
// seconds. All three are null when samples is empty. samples is sorted
// in place by a copy so the caller's slice is unmodified.
func durationStats(samples []float64) statsDuration {
	d := statsDuration{Samples: len(samples)}
	if len(samples) == 0 {
		return d
	}

	sorted := slices.Clone(samples)
	slices.Sort(sorted)

	var sum float64
	for _, s := range sorted {
		sum += s
	}

	p50 := percentile(sorted, 50)
	p95 := percentile(sorted, 95)
	mean := round1(sum / float64(len(sorted)))
	d.P50 = &p50
	d.P95 = &p95
	d.Mean = &mean
	return d
}

// report computes every derived figure exactly once from a's folded
// state and returns the complete statsReport. warnings are folded into
// the envelope's Warnings field verbatim.
func (a *statsAggregator) report(
	generatedAt time.Time,
	workflowPath, dbPath string,
	since, until *time.Time,
	warnings []string,
) statsReport {
	full := a.caps.Full()

	rpt := statsReport{
		GeneratedAt:  generatedAt.Format(time.RFC3339),
		WorkflowPath: workflowPath,
		DBPath:       dbPath,
		SchemaTier:   a.tierLabel,
		Warnings:     append([]string{}, warnings...),
		Summary:      a.summary(full),
		ByStatus:     a.groupSlice(a.byStatus, full),
		ByAdapter:    a.groupSlice(a.byAdapter, full),
		ByRule:       []statsGroup{},
		ByTemplate:   []statsGroup{},
	}
	if since != nil {
		s := since.UTC().Format(time.RFC3339)
		rpt.Since = &s
	}
	if until != nil {
		u := until.UTC().Format(time.RFC3339)
		rpt.Until = &u
	}
	if full {
		rpt.ByRule = a.groupSlice(a.byRule, full)
		rpt.ByTemplate = a.groupSlice(a.byTemplate, full)
		rpt.SelfReview = a.selfReviewReport()
	}

	return rpt
}

// summary computes the report-wide statsSummary.
func (a *statsAggregator) summary(full bool) statsSummary {
	s := statsSummary{
		Runs:                 a.total.runs,
		Succeeded:            a.total.succeeded,
		Duration:             durationStats(a.total.succeededSamples),
		ZeroDurationRuns:     a.total.zeroDurationRuns,
		DurationExcludedRuns: a.total.durationExcludedRuns,
		CostUnpricedRuns:     a.total.costUnpricedRuns,
	}
	if s.Runs > 0 {
		s.SuccessRate = round4(float64(s.Succeeded) / float64(s.Runs))
	}
	if full && s.Succeeded > 0 {
		mean := round2(float64(a.total.succeededTurns) / float64(s.Succeeded))
		s.MeanTurnsSucceeded = &mean
	}
	if full {
		tokens := a.total.tokens
		s.Tokens = &tokens
	}
	if full && a.total.pricedRuns > 0 {
		cost := round2(a.total.costUSD)
		s.CostUSD = &cost
		if s.Succeeded > 0 {
			perRun := round2(cost / float64(s.Succeeded))
			s.CostPerSucceededRunUSD = &perRun
		}
	}
	return s
}

// groupSlice converts groups into a statsGroup slice, sorted by
// descending runs with ties broken by ascending name under byte
// comparison.
func (a *statsAggregator) groupSlice(groups map[string]*statsGroupAccum, full bool) []statsGroup {
	out := make([]statsGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, a.groupStat(g, full))
	}
	slices.SortFunc(out, func(x, y statsGroup) int {
		if x.Runs != y.Runs {
			return y.Runs - x.Runs
		}
		return strings.Compare(x.Name, y.Name)
	})
	return out
}

// groupStat computes the derived statsGroup figures for one accumulated
// group.
func (a *statsAggregator) groupStat(g *statsGroupAccum, full bool) statsGroup {
	stat := statsGroup{
		Name:      g.name,
		Runs:      g.runs,
		Succeeded: g.succeeded,
		Duration:  durationStats(g.durationSamples),
	}
	if g.runs > 0 {
		stat.SuccessRate = round4(float64(g.succeeded) / float64(g.runs))
	}
	if a.total.runs > 0 {
		stat.Share = round4(float64(g.runs) / float64(a.total.runs))
	}
	if full {
		mean := round2(float64(g.turnSum) / float64(g.runs))
		stat.MeanTurns = &mean
		tokens := g.tokens
		stat.Tokens = &tokens
		if g.pricedRuns > 0 {
			cost := round2(g.costUSD)
			stat.CostUSD = &cost
			if g.succeeded > 0 {
				perRun := round2(cost / float64(g.succeeded))
				stat.CostPerSucceededRunUSD = &perRun
			}
		}
	}
	return stat
}

// selfReviewReport computes the derived statsSelfReview from a's folded
// self-review state. by_final_verdict is sorted by ascending verdict.
func (a *statsAggregator) selfReviewReport() *statsSelfReview {
	sr := &statsSelfReview{
		RunsWithMetadata: a.selfReview.runsWithMetadata,
		ByFinalVerdict:   make([]statsVerdictCount, 0, len(a.selfReview.byVerdict)),
		CapReachedRuns:   a.selfReview.capReachedRuns,
		UnparsedRuns:     a.selfReview.unparsedRuns,
	}
	if a.selfReview.runsWithMetadata > 0 {
		mean := round2(float64(a.selfReview.iterationSum) / float64(a.selfReview.runsWithMetadata))
		sr.MeanIterations = &mean
	}
	for verdict, count := range a.selfReview.byVerdict {
		sr.ByFinalVerdict = append(sr.ByFinalVerdict, statsVerdictCount{Verdict: verdict, Runs: count})
	}
	slices.SortFunc(sr.ByFinalVerdict, func(x, y statsVerdictCount) int {
		return strings.Compare(x.Verdict, y.Verdict)
	})
	return sr
}

// degradedSchemaWarning names the figures the report leaves out and states
// the remedy. It MUST NOT be called when caps is full.
//
// A partly migrated database is the common case, and on it the two lists
// differ: the report falls back to the basic figures wholesale, so it drops
// groups this database does carry as well as the ones it never recorded.
// Naming only the unrecorded groups would let a reader conclude the
// remaining figures are present when they are not.
func degradedSchemaWarning(caps persistence.RunHistoryCapabilities) string {
	groups := []struct {
		recorded bool
		name     string
	}{
		{caps.HasTurnsCompleted, "turns"},
		{caps.HasReviewMetadata, "self-review results"},
		{caps.HasRuleRouting, "dispatch-rule routing"},
		{caps.HasTokens, "tokens and cost"},
	}

	var unrecorded, dropped []string
	for _, g := range groups {
		if g.recorded {
			dropped = append(dropped, g.name)
		} else {
			unrecorded = append(unrecorded, g.name)
		}
	}

	if len(dropped) == 0 {
		return fmt.Sprintf(
			"this database was written before sortie recorded %s, so the report leaves them out. "+
				"Run sortie once with this workflow to add them.",
			strings.Join(unrecorded, ", "))
	}
	return fmt.Sprintf(
		"this database was written before sortie recorded %s. The report falls back to run counts and "+
			"durations, so it also leaves out %s, which this database does carry. "+
			"Run sortie once with this workflow to get the full report.",
		strings.Join(unrecorded, ", "), strings.Join(dropped, ", "))
}

// formatShare renders a 0..1 fraction as a one-decimal percentage.
func formatShare(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

// formatFloatDash renders a nullable float64 with format, or "-" when v
// is nil.
func formatFloatDash(v *float64, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}

// formatCostDash renders a nullable USD figure with [server.FormatCost],
// or "-" when v is nil.
func formatCostDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return server.FormatCost(*v)
}

// formatSecondsDash renders a nullable seconds figure with
// [server.FormatDuration], or "-" when v is nil.
func formatSecondsDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return server.FormatDuration(time.Duration(*v * float64(time.Second)))
}

// formatStatsDuration renders a statsDuration's p50, p95, mean, and
// sample count on one line.
func formatStatsDuration(d statsDuration) string {
	return fmt.Sprintf("p50 %s   p95 %s   mean %s   samples %d",
		formatSecondsDash(d.P50), formatSecondsDash(d.P95), formatSecondsDash(d.Mean), d.Samples)
}

// formatStatsTokens renders a statsTokens's four sums on one line, or
// "-" when t is nil.
func formatStatsTokens(t *statsTokens) string {
	if t == nil {
		return "-"
	}
	return fmt.Sprintf("input %s   output %s   total %s   cache read %s",
		server.FormatInt(t.Input), server.FormatInt(t.Output), server.FormatInt(t.Total), server.FormatInt(t.CacheRead))
}

// formatTokensTotalDash renders only t's Total field, or "-" when t is
// nil, for the "total tokens" group table column.
func formatTokensTotalDash(t *statsTokens) string {
	if t == nil {
		return "-"
	}
	return server.FormatInt(t.Total)
}

// renderStatsGroupTable prints one section of the text report: the
// section title, a header row, and one row per group. isOutcomeTable
// selects the "share" column for the outcome breakdown; every other
// breakdown carries "succeeded" and "success rate" instead. full selects
// the turns, total tokens, and cost columns.
//
// Cells are tab-separated and flushed through a tabwriter so every column
// lines up at the width of its widest cell. A row is never written
// directly to w.
func renderStatsGroupTable(w io.Writer, title, dimLabel string, groups []statsGroup, full, isOutcomeTable bool) {
	fmt.Fprintln(w, title) //nolint:errcheck // stdout write failure is unrecoverable

	header := []string{dimLabel, "runs"}
	if isOutcomeTable {
		header = append(header, "share")
	} else {
		header = append(header, "succeeded", "success rate")
	}
	header = append(header, "p50", "p95", "mean")
	if full {
		header = append(header, "turns", "total tokens", "cost")
	}

	table := newStatsTable(w)
	writeStatsRow(table, upperAll(header))

	for _, g := range groups {
		row := []string{g.Name, strconv.Itoa(g.Runs)}
		if isOutcomeTable {
			row = append(row, formatShare(g.Share))
		} else {
			row = append(row, strconv.Itoa(g.Succeeded), formatShare(g.SuccessRate))
		}
		row = append(row,
			formatSecondsDash(g.Duration.P50), formatSecondsDash(g.Duration.P95), formatSecondsDash(g.Duration.Mean))
		if full {
			row = append(row,
				formatFloatDash(g.MeanTurns, "%.1f"),
				formatTokensTotalDash(g.Tokens),
				formatCostDash(g.CostUSD))
		}
		writeStatsRow(table, row)
	}
	table.Flush() //nolint:errcheck,gosec // stdout write failure is unrecoverable
}

// newStatsTable returns a tabwriter that indents every row by two spaces
// and separates columns by two spaces, so a table reads as a block
// belonging to the section title above it.
func newStatsTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// writeStatsRow writes one tab-separated, two-space-indented row to a
// tabwriter.
func writeStatsRow(table *tabwriter.Writer, cells []string) {
	fmt.Fprintf(table, "  %s\n", strings.Join(cells, "\t")) //nolint:errcheck // stdout write failure is unrecoverable
}

// upperAll returns cells with every element upper-cased, for a table
// header row.
func upperAll(cells []string) []string {
	upper := make([]string, len(cells))
	for i, c := range cells {
		upper[i] = strings.ToUpper(c)
	}
	return upper
}

// renderStatsSelfReview prints the "self review" section.
func renderStatsSelfReview(w io.Writer, sr *statsSelfReview) {
	fmt.Fprintln(w, "self review") //nolint:errcheck // stdout write failure is unrecoverable
	if sr == nil {
		return
	}

	parts := []string{fmt.Sprintf("runs reviewed %d", sr.RunsWithMetadata)}
	for _, vc := range sr.ByFinalVerdict {
		parts = append(parts, fmt.Sprintf("%s %d", vc.Verdict, vc.Runs))
	}
	parts = append(parts,
		fmt.Sprintf("hit iteration cap %d", sr.CapReachedRuns),
		fmt.Sprintf("mean iterations %s", formatFloatDash(sr.MeanIterations, "%.2f")))
	fmt.Fprintf(w, "  %s\n", strings.Join(parts, "   ")) //nolint:errcheck // stdout write failure is unrecoverable
}

// statsFootnotes returns one line per non-zero disclosure counter in s,
// naming the counter's value and its cause.
func statsFootnotes(s statsSummary) []string {
	var lines []string
	if s.ZeroDurationRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: %d of these runs took no measurable time. A run that a CI result closed out carries the same start and finish time.",
			s.ZeroDurationRuns))
	}
	if s.DurationExcludedRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: the duration figures skip %d of these runs, whose recorded start and finish times were unusable.",
			s.DurationExcludedRuns))
	}
	if s.CostUnpricedRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: the cost figures skip %d of these runs, because token_rates has no price for the coding agent behind them.",
			s.CostUnpricedRuns))
	}
	return lines
}

// formatStatsRange describes the reported range in prose. An absent bound
// is named as such rather than shown as a sentinel, so a first-time reader
// is not left to interpret one.
func formatStatsRange(since, until *string) string {
	switch {
	case since == nil && until == nil:
		return "every run on record"
	case until == nil:
		return *since + " onward"
	case since == nil:
		return "the start of the record until " + *until
	default:
		return *since + " until " + *until
	}
}

// emptyRangeMessage states that nothing matched, distinguishing an empty
// database from a range that happens to hold no run.
func emptyRangeMessage(since, until *string) string {
	if since == nil && until == nil {
		return "No runs on record yet. sortie adds one each time an agent session finishes."
	}
	return "No runs finished in this range."
}

// renderStatsText writes report to stdout in text form and writes
// report.Warnings, one per line, to stderr.
func renderStatsText(stdout, stderr io.Writer, report statsReport) {
	fmt.Fprintf(stdout, "workflow:  %s\n", report.WorkflowPath)                          //nolint:errcheck // stdout write failure is unrecoverable
	fmt.Fprintf(stdout, "database:  %s\n", report.DBPath)                                //nolint:errcheck // stdout write failure is unrecoverable
	fmt.Fprintf(stdout, "covering:  %s\n", formatStatsRange(report.Since, report.Until)) //nolint:errcheck // stdout write failure is unrecoverable
	fmt.Fprintf(stdout, "generated: %s\n", report.GeneratedAt)                           //nolint:errcheck // stdout write failure is unrecoverable
	fmt.Fprintln(stdout)                                                                 //nolint:errcheck // stdout write failure is unrecoverable

	if report.Summary.Runs == 0 {
		fmt.Fprintln(stdout, emptyRangeMessage(report.Since, report.Until)) //nolint:errcheck // stdout write failure is unrecoverable
		writeStatsWarnings(stderr, report.Warnings)
		return
	}

	full := report.SchemaTier == "full"

	fmt.Fprintf(stdout, "runs %d   succeeded %d (%s)\n", //nolint:errcheck // stdout write failure is unrecoverable
		report.Summary.Runs, report.Summary.Succeeded, formatShare(report.Summary.SuccessRate))
	fmt.Fprintf(stdout, "duration (succeeded)    %s\n", formatStatsDuration(report.Summary.Duration)) //nolint:errcheck // stdout write failure is unrecoverable
	if full {
		fmt.Fprintf(stdout, "turns (succeeded)       %s\n", //nolint:errcheck // stdout write failure is unrecoverable
			formatFloatDash(report.Summary.MeanTurnsSucceeded, "%.1f"))
		fmt.Fprintf(stdout, "tokens (all runs)       %s\n", formatStatsTokens(report.Summary.Tokens)) //nolint:errcheck // stdout write failure is unrecoverable
		switch {
		case report.Summary.CostUSD != nil:
			fmt.Fprintf(stdout, "cost (all runs)         %s   per succeeded run %s\n", //nolint:errcheck // stdout write failure is unrecoverable
				server.FormatCost(*report.Summary.CostUSD), formatCostDash(report.Summary.CostPerSucceededRunUSD))
		case report.Summary.CostUnpricedRuns == 0:
			fmt.Fprintln(stdout, //nolint:errcheck // stdout write failure is unrecoverable
				"cost                    not estimated; set token_rates in the workflow to price these runs")
		default:
			fmt.Fprintln(stdout, "cost (all runs)         -   per succeeded run -") //nolint:errcheck // stdout write failure is unrecoverable
		}
	}
	fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable

	renderStatsGroupTable(stdout, "by outcome", "outcome", report.ByStatus, full, true)
	fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
	renderStatsGroupTable(stdout, "by coding agent", "agent", report.ByAdapter, full, false)
	if full {
		fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
		renderStatsGroupTable(stdout, "by dispatch rule", "rule", report.ByRule, full, false)
		fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
		renderStatsGroupTable(stdout, "by prompt template", "template", report.ByTemplate, full, false)
		fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
		renderStatsSelfReview(stdout, report.SelfReview)
	}

	if footnotes := statsFootnotes(report.Summary); len(footnotes) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
		for _, note := range footnotes {
			fmt.Fprintln(stdout, note) //nolint:errcheck // stdout write failure is unrecoverable
		}
	}

	writeStatsWarnings(stderr, report.Warnings)
}

// writeStatsWarnings writes warnings to stderr, one line each, under the
// same "warning: " prefix emitDiags uses, so a warning is never mistaken
// for part of the report. The prefix belongs to the rendering rather than
// to the message, keeping the JSON envelope's warnings field clean.
func writeStatsWarnings(stderr io.Writer, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w) //nolint:errcheck // stderr write failure is unrecoverable
	}
}

// runStats implements the "sortie stats" subcommand: it opens the
// database read-only, aggregates run_history over the requested range,
// and renders the result to stdout in text or JSON form.
func runStats(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if containsHelpFlag(args) {
		printStatsHelp(stdout)
		return 0
	}

	fs := flag.NewFlagSet("sortie stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "text", `Output format: "text" or "json"`)
	sinceRaw := fs.String("since", "", "Count only runs that finished at or after this point")
	untilRaw := fs.String("until", "", "Count only runs that finished before this point")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printStatsHelp(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "sortie stats: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "sortie stats: invalid --format value %q: must be \"text\" or \"json\"\n", *format) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	since, err := parseRangeBound(*sinceRaw)
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: --since: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	until, err := parseRangeBound(*untilRaw)
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: --until: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	if since != nil && until != nil && !since.Before(*until) {
		fmt.Fprintln(stderr, "sortie stats: --since must be strictly before --until") //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	path, err := resolveWorkflowPath(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	wf, err := workflow.Load(path)
	if err != nil {
		emitDiags(stdout, stderr, "text", mapManagerError(err), nil)
		return 1
	}
	cfg, err := config.NewServiceConfig(wf.Config)
	if err != nil {
		emitDiags(stdout, stderr, "text", mapManagerError(err), nil)
		return 1
	}

	dbPath := resolveDBPath(cfg.DBPath, filepath.Dir(path))
	store, err := persistence.OpenReadOnly(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: open database %s: %s\n", dbPath, err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	defer store.Close() //nolint:errcheck,gosec // read-only handle; close error is non-actionable

	caps, err := store.RunHistoryCapabilities(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	rates, rateWarnings := server.ParseTokenRates(cfg.Extensions)

	agg := newStatsAggregator(caps, rates)
	if err := store.ScanRunHistoryRange(ctx, caps, since, until, agg.add); err != nil {
		fmt.Fprintf(stderr, "sortie stats: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	warnings := rateWarnings
	if !caps.Full() {
		warnings = append([]string{degradedSchemaWarning(caps)}, warnings...)
	}

	report := agg.report(statsNow().UTC(), path, dbPath, since, until, warnings)

	if *format == "json" {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "sortie stats: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return 1
		}
		return 0
	}

	renderStatsText(stdout, stderr, report)
	return 0
}

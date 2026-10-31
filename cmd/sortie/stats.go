package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/runstats"
	"github.com/sortie-ai/sortie/internal/server"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// statsNow follows the serverShutdownTimeout precedent for a
// test-overridable package variable, so a test can pin generated_at and
// any relative --since or --until bound.
var statsNow = time.Now

// statsChainTextRows is the number of by_chain entries the text form prints.
// by_chain grows by one entry per qualifying pass, unlike every other
// breakdown, and a report without --since reads every run on record.
const statsChainTextRows = 20

// statsSource is what a stats report is read from.
type statsSource struct {
	dbPath       string
	workflowPath string

	// stages supplies the stage chains chain_funnels reads; nil serves an
	// empty chain_funnels.
	stages func() runstats.Stages
}

// report opens dbPath read-only and aggregates the runs completed in
// [since, until). Each call opens its own connection, so concurrent calls
// share nothing and a long scan never holds the orchestrator's store.
func (src statsSource) report(ctx context.Context, since, until *time.Time, generatedAt time.Time) (runstats.Report, error) {
	store, err := persistence.OpenReadOnly(ctx, src.dbPath)
	if err != nil {
		return runstats.Report{}, fmt.Errorf("open database %s: %w", src.dbPath, err)
	}
	defer store.Close() //nolint:errcheck,gosec // read-only handle; close error is non-actionable

	caps, err := store.RunHistoryCapabilities(ctx)
	if err != nil {
		return runstats.Report{}, err
	}

	agg := runstats.New(caps)
	if src.stages != nil {
		stages := src.stages()
		if len(stages.Chains) > 0 && caps.FunnelBreakdown() {
			passes, err := store.ListLivePasses(ctx)
			if err != nil {
				return runstats.Report{}, err
			}
			stages.Live = livePasses(passes)
		}
		agg.SetStages(stages)
	}
	if err := store.ScanRunHistoryRange(ctx, caps, since, until, agg.Add); err != nil {
		return runstats.Report{}, err
	}
	return agg.Report(generatedAt, src.workflowPath, src.dbPath, since, until), nil
}

// dispatchStages joins the dispatch section to the stages chain_funnels
// reads: its chains and the rules a stage label selects.
func dispatchStages(d config.DispatchConfig) runstats.Stages {
	stages := runstats.Stages{Chains: d.Chains()}
	for _, rule := range d.Rules {
		if rule.Stage != "" {
			stages.LabelRules = append(stages.LabelRules, rule.Name)
		}
	}
	return stages
}

// livePasses maps the persisted live passes to the stage pass records
// chain_funnels counts.
func livePasses(passes []persistence.LivePass) []runstats.LivePass {
	live := make([]runstats.LivePass, len(passes))
	for i, pass := range passes {
		live[i] = runstats.LivePass{
			ChainID:      pass.ChainID,
			RuleName:     pass.RuleName,
			PreviousRule: pass.StagePrevious,
			BudgetHeld:   pass.Kind == persistence.LivePassBudget,
		}
	}
	return live
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

// formatCostDash renders a nullable USD figure with [server.FormatCost], or
// with [server.FormatCostCut] when lowerBound is set so the figure never
// rounds up past the spend it bounds, or "-" when v is nil.
func formatCostDash(v *float64, lowerBound bool) string {
	if v == nil {
		return "-"
	}
	if lowerBound {
		return server.FormatCostCut(*v)
	}
	return server.FormatCost(*v)
}

// statsLineLabel pads a summary line's label to the report's label width and
// marks the line as a lower bound when its figure counts only part of its
// runs.
func statsLineLabel(name string, lowerBound bool) string {
	if lowerBound {
		name += " (lower bound)"
	}
	return fmt.Sprintf("%-24s", name)
}

// statsLowerBoundCell names the figures of g that count only part of its
// runs, or "-" when none does.
func statsLowerBoundCell(g runstats.Group) string {
	switch {
	case g.TokensLowerBound && g.CostLowerBound:
		return "tokens, cost"
	case g.TokensLowerBound:
		return "tokens"
	case g.CostLowerBound:
		return "cost"
	default:
		return "-"
	}
}

// formatSecondsDash renders a nullable seconds figure with
// [server.FormatDuration], or "-" when v is nil.
func formatSecondsDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return server.FormatDuration(time.Duration(*v * float64(time.Second)))
}

// formatStatsDuration renders a [runstats.Duration]'s p50, p95, mean, and
// sample count on one line.
func formatStatsDuration(d runstats.Duration) string {
	return fmt.Sprintf("p50 %s   p95 %s   mean %s   samples %d",
		formatSecondsDash(d.P50), formatSecondsDash(d.P95), formatSecondsDash(d.Mean), d.Samples)
}

// formatStatsTokens renders a [runstats.Tokens]'s five sums on one line, or
// "-" when t is nil.
func formatStatsTokens(t *runstats.Tokens) string {
	if t == nil {
		return "-"
	}
	return fmt.Sprintf("input %s   output %s   total %s   cache read %s   cache write %s",
		server.FormatInt(t.Input), server.FormatInt(t.Output), server.FormatInt(t.Total),
		server.FormatInt(t.CacheRead), server.FormatInt(t.CacheWrite))
}

// formatTokensTotalDash renders only t's Total field, or "-" when t is
// nil, for the "total tokens" group table column.
func formatTokensTotalDash(t *runstats.Tokens) string {
	if t == nil {
		return "-"
	}
	return server.FormatInt(t.Total)
}

// renderStatsGroupTable prints one section of the text report: the
// section title, a header row, and one row per group. isOutcomeTable
// selects the "share" column for the outcome breakdown; every other
// breakdown carries "succeeded" and "success rate" instead. full selects
// the turns, total tokens, cost, and lower bound columns.
//
// Cells are tab-separated and flushed through a tabwriter so every column
// lines up at the width of its widest cell. A row is never written
// directly to w.
func renderStatsGroupTable(w io.Writer, title, dimLabel string, groups []runstats.Group, full, isOutcomeTable bool) {
	fmt.Fprintln(w, title) //nolint:errcheck // stdout write failure is unrecoverable

	table := newStatsTable(w)
	writeStatsRow(table, upperAll(append([]string{dimLabel}, statsGroupHeader(full, isOutcomeTable)...)))
	for _, g := range groups {
		writeStatsRow(table, append([]string{g.Name}, statsGroupCells(g, full, isOutcomeTable)...))
	}
	table.Flush() //nolint:errcheck,gosec // stdout write failure is unrecoverable
}

// renderStatsChainTable prints the "by stage chain" section: the first
// statsChainTextRows entries, with the chain, issue, stage path, and last
// hop result ahead of the figures of a non-outcome breakdown.
func renderStatsChainTable(w io.Writer, chains []runstats.ChainGroup, full bool) {
	fmt.Fprintln(w, "by stage chain") //nolint:errcheck // stdout write failure is unrecoverable

	table := newStatsTable(w)
	writeStatsRow(table, upperAll(append([]string{"chain", "issue", "stages", "last hop"}, statsGroupHeader(full, false)...)))
	for _, c := range chains[:min(len(chains), statsChainTextRows)] {
		lead := []string{c.Name, c.Issue, strings.Join(c.Stages, " -> "), cmp.Or(c.LastHop, "-")}
		writeStatsRow(table, append(lead, statsGroupCells(c.Group, full, false)...))
	}
	table.Flush() //nolint:errcheck,gosec // stdout write failure is unrecoverable
}

// noHopReasonWords names each no-hop reason the way a reader of the report
// says it.
var noHopReasonWords = map[string]string{
	"run_failed":       "failed run",
	"needs_person":     "needs a person",
	"budget_held":      "budget held",
	"usage_limited":    "usage limit",
	"operator_stopped": "stopped by an operator",
	"verdict_withheld": "verdict withheld",
}

// renderStatsFunnel prints one chain funnel: a title naming its entry rules
// and one row per stage.
func renderStatsFunnel(w io.Writer, funnel runstats.Funnel) {
	var entries []string
	for _, stage := range funnel.Stages {
		if stage.Entry {
			entries = append(entries, stage.Rule)
		}
	}
	fmt.Fprintf(w, "chain from %s\n", strings.Join(entries, " or ")) //nolint:errcheck // stdout write failure is unrecoverable

	table := newStatsTable(w)
	writeStatsRow(table, upperAll([]string{
		"#", "stage", "runs", "entered", "by label", "advanced", "ceiling", "hop failed", "no hop", "in stage", "median",
	}))
	for i, stage := range funnel.Stages {
		name := stage.Rule
		if stage.Entry {
			name += " (entry)"
		}
		advanced := strconv.Itoa(stage.Outcomes.Advanced)
		if stage.Last {
			advanced = fmt.Sprintf("%d handed off", stage.Outcomes.HandedOff)
		}
		noHop := strconv.Itoa(stage.Outcomes.NoHop)
		if stage.Outcomes.NoHop > 0 && len(stage.NoHopReasons) > 0 {
			reason := stage.NoHopReasons[0].Reason
			noHop += " " + cmp.Or(noHopReasonWords[reason], reason)
		}
		writeStatsRow(table, []string{
			strconv.Itoa(i + 1), name,
			strconv.Itoa(stage.Runs), strconv.Itoa(stage.Entered), strconv.Itoa(stage.Arrivals.Label),
			advanced, strconv.Itoa(stage.Outcomes.Ceiling), strconv.Itoa(stage.Outcomes.HopFailed),
			noHop, strconv.Itoa(stage.Outcomes.InStage), formatSecondsDash(stage.MedianSeconds),
		})
	}
	table.Flush() //nolint:errcheck,gosec // stdout write failure is unrecoverable
}

// statsGroupHeader returns the header cells that follow a group's name.
func statsGroupHeader(full, isOutcomeTable bool) []string {
	header := []string{"runs"}
	if isOutcomeTable {
		header = append(header, "share")
	} else {
		header = append(header, "succeeded", "success rate")
	}
	header = append(header, "p50", "p95", "mean")
	if full {
		header = append(header, "turns", "total tokens", "cost", "lower bound")
	}
	return header
}

// statsGroupCells returns the row cells that follow a group's name, in
// the order of [statsGroupHeader].
func statsGroupCells(g runstats.Group, full, isOutcomeTable bool) []string {
	cells := []string{strconv.Itoa(g.Runs)}
	if isOutcomeTable {
		cells = append(cells, formatShare(g.Share))
	} else {
		cells = append(cells, strconv.Itoa(g.Succeeded), formatShare(g.SuccessRate))
	}
	cells = append(cells,
		formatSecondsDash(g.Duration.P50), formatSecondsDash(g.Duration.P95), formatSecondsDash(g.Duration.Mean))
	if full {
		cells = append(cells,
			formatFloatDash(g.MeanTurns, "%.1f"),
			formatTokensTotalDash(g.Tokens),
			formatCostDash(g.CostUSD, g.CostLowerBound),
			statsLowerBoundCell(g))
	}
	return cells
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
func renderStatsSelfReview(w io.Writer, sr *runstats.SelfReview) {
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
func statsFootnotes(s runstats.Summary, chains int) []string {
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
	if s.TokensUnmeasuredRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: the token and cost figures skip %d of these runs, because the coding agent behind them reported no token usage.",
			s.TokensUnmeasuredRuns))
	}
	if s.CostUnpricedRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: the cost figures skip %d of these runs, because Sortie had no price for the models they used when they were recorded.",
			s.CostUnpricedRuns))
	}
	if s.TokensLowerBoundRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: the token figures count only part of %d of these runs, because some of their turns were not fully counted.",
			s.TokensLowerBoundRuns))
	}
	if s.CostPartialRuns > 0 {
		lines = append(lines, fmt.Sprintf(
			"note: the cost figures count only part of %d of these runs, because part of their spend was not counted or had no price.",
			s.CostPartialRuns))
	}
	if s.AgentPricedUSD != nil {
		lines = append(lines, fmt.Sprintf(
			"note: the cost figures include %s that the coding agent reported for models Sortie had no price for when the runs were recorded.",
			server.FormatCost(*s.AgentPricedUSD)))
	}
	if chains > statsChainTextRows {
		lines = append(lines, fmt.Sprintf(
			"note: the breakdown by stage chain shows the %d stage chains with the most runs; --format json lists all %d.",
			statsChainTextRows, chains))
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
func renderStatsText(stdout, stderr io.Writer, report runstats.Report) {
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

	fmt.Fprintf(stdout, "runs %d   issues %d   succeeded %d (%s)\n", //nolint:errcheck // stdout write failure is unrecoverable
		report.Summary.Runs, report.Summary.Issues, report.Summary.Succeeded, formatShare(report.Summary.SuccessRate))
	fmt.Fprintf(stdout, "duration (succeeded)    %s\n", formatStatsDuration(report.Summary.Duration)) //nolint:errcheck // stdout write failure is unrecoverable
	if full {
		fmt.Fprintf(stdout, "turns (succeeded)       %s\n", //nolint:errcheck // stdout write failure is unrecoverable
			formatFloatDash(report.Summary.MeanTurnsSucceeded, "%.1f"))
		fmt.Fprintf(stdout, "%s%s\n", statsLineLabel("tokens", report.Summary.TokensLowerBound), formatStatsTokens(report.Summary.Tokens)) //nolint:errcheck // stdout write failure is unrecoverable
		costLabel := statsLineLabel("cost", report.Summary.CostLowerBound)
		if report.Summary.CostUSD != nil {
			fmt.Fprintf(stdout, "%s%s   per succeeded run %s\n", costLabel, //nolint:errcheck // stdout write failure is unrecoverable
				formatCostDash(report.Summary.CostUSD, report.Summary.CostLowerBound),
				formatCostDash(report.Summary.CostPerSucceededRunUSD, report.Summary.CostLowerBound))
		} else {
			fmt.Fprintf(stdout, "%s-   per succeeded run -\n", costLabel) //nolint:errcheck // stdout write failure is unrecoverable
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
		if len(report.ByModel) > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
			renderStatsGroupTable(stdout, "by model", "model", report.ByModel, full, false)
		}
		if len(report.ByProvenance) > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
			renderStatsGroupTable(stdout, "by usage provenance", "provenance", report.ByProvenance, full, false)
		}
		if len(report.ByChain) > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
			renderStatsChainTable(stdout, report.ByChain, full)
		}
		for _, funnel := range report.ChainFunnels {
			fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
			renderStatsFunnel(stdout, funnel)
		}
		if len(report.ByTrigger) > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
			renderStatsGroupTable(stdout, "by retry trigger", "trigger", report.ByTrigger, full, false)
		}
		if len(report.ByOrigin) > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
			renderStatsGroupTable(stdout, "by run origin", "origin", report.ByOrigin, full, false)
		}
		fmt.Fprintln(stdout) //nolint:errcheck // stdout write failure is unrecoverable
		renderStatsSelfReview(stdout, report.SelfReview)
	}

	if footnotes := statsFootnotes(report.Summary, len(report.ByChain)); len(footnotes) > 0 {
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
//
// A blank line opens the block so the warnings do not run into the report's
// last line when both streams share a terminal. It is written to stderr
// alongside the warnings it separates rather than to stdout, so a report
// with nothing to warn about does not end in a stray blank line.
func writeStatsWarnings(stderr io.Writer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(stderr) //nolint:errcheck // stderr write failure is unrecoverable
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

	now := statsNow().UTC()
	since, err := runstats.ParseRangeBound(*sinceRaw, now)
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: --since: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	until, err := runstats.ParseRangeBound(*untilRaw, now)
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

	dispatch, err := config.BuildDispatchConfig(wf.Config, filepath.Dir(path), func(string) bool { return true }, cfg.Agent.Kind)
	if err != nil {
		emitDiags(stdout, stderr, "text", mapManagerError(err), nil)
		return 1
	}

	dbPath := resolveDBPath(cfg.DBPath, filepath.Dir(path))

	src := statsSource{
		dbPath:       dbPath,
		workflowPath: path,
		stages:       func() runstats.Stages { return dispatchStages(dispatch) },
	}
	report, err := src.report(ctx, since, until, now)
	if err != nil {
		fmt.Fprintf(stderr, "sortie stats: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

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

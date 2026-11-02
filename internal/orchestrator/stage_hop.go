package orchestrator

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
)

// Values of the trigger attribute on the stage hop count reset record.
const (
	stageResetHandoff   = "handoff"
	stageResetTerminal  = "terminal"
	stageResetNotActive = "not_active"
	stageResetParked    = "parked"
	stageResetUnparked  = "unparked"
)

// Outcomes a hop records for the run that made it.
const (
	stageOutcomeSucceeded = "succeeded"
	stageOutcomeNoChange  = "no_change"
)

// Results of a hop decision, recorded on the run that reached it.
const (
	stageResultAdvanced = "advanced"
	stageResultPartial  = "partial"
	stageResultFailed   = "failed"
	stageResultCeiling  = domain.StageReasonCeiling
)

const stageChainSeparator = " -> "

// stageHopDecision is the outcome of one exit's hop decision. The zero
// value means no hop was due.
type stageHopDecision struct {
	Result     string
	SourceRule string
	TargetRule string
	ChainID    string

	// HopCount is the issue's consecutive hop count once the decision
	// applies.
	HopCount int
}

// made reports whether the decision added the target's stage label.
func (d stageHopDecision) made() bool {
	return d.Result == stageResultAdvanced || d.Result == stageResultPartial
}

// hopRouteOf returns the rule and stage label of the issue's latest hop,
// or the zero [HopRoute] when it has no hop record.
func hopRouteOf(state *State, id string) HopRoute {
	hop := state.StageHops[id]
	if hop == nil {
		return HopRoute{}
	}
	return HopRoute{TargetRule: hop.TargetRule, TargetLabel: hop.TargetLabel}
}

// freshLineage returns the lineage a dispatch of ruleName inherits when it
// selects the target of the issue's latest hop, pass dispatch time included,
// and the zero lineage for any other rule or an issue without a hop record.
func freshLineage(state *State, id, ruleName string) StageLineage {
	hop := state.StageHops[id]
	if hop == nil || hop.TargetRule != ruleName {
		return StageLineage{}
	}
	return StageLineage{PreviousRule: hop.SourceRule, PreviousOutcome: hop.PreviousOutcome, ChainID: hop.ChainID, DispatchedAt: hop.DispatchedAt}
}

// withChainID starts a chain for a lineage that carries none.
func withChainID(lineage StageLineage) StageLineage {
	if lineage.ChainID == "" {
		// Independent of the dispatch ID, so a public comment that carries
		// the chain never carries session identity.
		lineage.ChainID = rand.Text()
	}
	return lineage
}

// startPass dates the pass of a freshly dispatched entry by the entry's own
// start when no earlier run of the pass did.
func startPass(entry *RunningEntry) {
	if entry.StageLineage.DispatchedAt.IsZero() {
		entry.StageLineage.DispatchedAt = entry.StartedAt
	}
}

// stageCurrent returns ruleName when the rule carries a stage label, and
// the empty string otherwise.
func stageCurrent(dispatch config.DispatchConfig, ruleName string) string {
	rule, ok := dispatch.RuleByName(ruleName)
	if !ok || rule.Stage == "" {
		return ""
	}
	return ruleName
}

// resetStageHop drops the issue's hop record, in memory and in the store,
// and logs the reset with trigger. It does nothing when the issue has no
// record. log must already carry issue_id and issue_identifier.
func resetStageHop(ctx context.Context, state *State, store storage.StageHopStore, issueID, trigger string, log *slog.Logger) {
	hop, ok := state.StageHops[issueID]
	if !ok {
		return
	}
	delete(state.StageHops, issueID)

	if err := store.DeleteStageHop(ctx, issueID); err != nil {
		log.Error("failed to delete stage hop record", slog.Any("error", err))
	}
	log.Info("stage hop count reset",
		slog.String("trigger", trigger),
		slog.Int("hop_count", hop.Count),
	)
}

// markStageHopDispatched records that a run of the issue was dispatched after
// its hop, in memory and in the store, and does nothing when the issue has no
// record or the record already says so. A failed write leaves the runtime
// value standing. log must already carry issue_id and issue_identifier.
func markStageHopDispatched(ctx context.Context, state *State, store storage.StageHopStore, issueID string, log *slog.Logger) {
	hop := state.StageHops[issueID]
	if hop == nil || hop.RunDispatched {
		return
	}
	hop.RunDispatched = true
	if err := store.MarkStageHopDispatched(ctx, issueID); err != nil {
		log.Error("failed to persist stage hop dispatch", slog.Any("error", err))
	}
}

// advanceStage makes the automatic hop of a successful run: it adds the
// next rule's stage label, records the hop, releases the issue's runtime
// state, and removes the other stage labels the dispatch read showed. The
// returned decision is made exactly when the label add succeeded, in which
// case the caller skips the handoff write. Any other outcome leaves the
// issue for the handoff write to move, and the zero decision means no hop
// was due. log must already carry issue_id and issue_identifier.
func advanceStage(ctx context.Context, state *State, entry *RunningEntry, result WorkerResult, params HandleWorkerExitParams, declared bool, log *slog.Logger) stageHopDecision {
	source, ok := params.Dispatch.RuleByName(entry.RuleName)
	if !ok || source.Next == "" {
		return stageHopDecision{}
	}
	target, ok := params.Dispatch.RuleByName(source.Next)
	if !ok || target.Stage == "" {
		return stageHopDecision{}
	}

	issueID := result.IssueID
	count := 0
	if hop := state.StageHops[issueID]; hop != nil {
		count = hop.Count
	}
	ceiling := params.Dispatch.HopCeiling()
	decide := func(result string, hopCount int) stageHopDecision {
		return stageHopDecision{
			Result:     result,
			SourceRule: source.Name,
			TargetRule: target.Name,
			ChainID:    entry.StageLineage.ChainID,
			HopCount:   hopCount,
		}
	}
	chainPath := strings.Join(params.Dispatch.ChainPath(source.Name), stageChainSeparator)
	notMade := func(reason string, extra ...slog.Attr) {
		attrs := []slog.Attr{
			slog.String("reason", reason),
			slog.String("source_rule", source.Name),
			slog.String("target_rule", target.Name),
			slog.String("target_label", target.Stage),
			slog.Int("hop_count", count),
			slog.Int("hop_ceiling", ceiling),
			slog.String("chain_path", chainPath),
		}
		log.LogAttrs(ctx, slog.LevelWarn, "stage hop not made", append(attrs, extra...)...)
	}

	if count+1 > ceiling {
		notMade("ceiling")
		return decide(stageResultCeiling, count)
	}

	if err := params.TrackerAdapter.AddLabel(ctx, issueID, target.Stage); err != nil {
		missing, readErr := missingStageLabels(ctx, params.TrackerAdapter, params.Dispatch, entry.DispatchLabels, issueID)
		extra := []slog.Attr{slog.Any("error", err)}
		if readErr != nil {
			extra = append(extra, slog.Any("stage_label_read_error", readErr))
		} else {
			extra = append(extra, slog.Any("missing_stage_labels", missing))
		}
		notMade("add_failed", extra...)
		return decide(stageResultFailed, count)
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc()
	}
	previousOutcome := stageOutcomeSucceeded
	if declared {
		previousOutcome = stageOutcomeNoChange
	}
	identity := issueIdentityOf(entry.Issue)
	identity.Labels = labelsAfterHop(entry.DispatchLabels, params.Dispatch.Rules, target.Stage)
	state.StageHops[issueID] = &StageHopEntry{
		Identifier:       result.Identifier,
		Count:            count + 1,
		SourceRule:       source.Name,
		TargetRule:       target.Name,
		TargetLabel:      target.Stage,
		PreviousOutcome:  previousOutcome,
		SourceDispatchID: entry.DispatchID,
		HoppedAt:         now,
		ChainID:          entry.StageLineage.ChainID,
		DisplayID:        entry.Issue.DisplayID,
		IssueIdentity:    identity,
		DispatchedAt:     entry.StageLineage.DispatchedAt,
	}
	row := storage.StageHop{
		IssueID:          issueID,
		Identifier:       result.Identifier,
		HopCount:         count + 1,
		SourceRule:       source.Name,
		TargetRule:       target.Name,
		TargetLabel:      target.Stage,
		PreviousOutcome:  previousOutcome,
		SourceDispatchID: entry.DispatchID,
		HoppedAt:         now.Format(time.RFC3339),
		ChainID:          entry.StageLineage.ChainID,
		DisplayID:        entry.Issue.DisplayID,
		IssueTitle:       entry.Issue.Title,
		IssueURL:         entry.Issue.URL,
		IssueType:        identity.IssueType,
		IssueLabels:      identity.Labels,
		IssueState:       identity.State,
		DispatchedAt:     formatDispatchedAt(entry.StageLineage.DispatchedAt),
	}
	if err := params.Store.RecordStageHop(ctx, row); err != nil {
		log.Error("failed to persist stage hop", slog.Any("error", err))
	}

	releaseIssueRuntimeState(ctx, state, params.Store, issueID, log)

	var left []string
	var firstErr error
	for _, label := range carriedStageLabels(params.Dispatch.Rules, entry.DispatchLabels) {
		if config.StageLabelsEqual(label, target.Stage) {
			continue
		}
		if err := params.TrackerAdapter.RemoveLabel(ctx, issueID, label); err != nil {
			left = append(left, label)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	madeAttrs := []slog.Attr{
		slog.String("source_rule", source.Name),
		slog.String("target_rule", target.Name),
		slog.String("target_label", target.Stage),
		slog.Int("hop_count", count+1),
		slog.Int("hop_ceiling", ceiling),
		slog.String("chain_path", chainPath),
	}
	if len(left) == 0 {
		log.LogAttrs(ctx, slog.LevelInfo, "stage hop made", madeAttrs...)
		return decide(stageResultAdvanced, count+1)
	}
	madeAttrs = append(madeAttrs, slog.Any("stage_labels_left", left), slog.Any("error", firstErr))
	log.LogAttrs(ctx, slog.LevelWarn, "stage hop made, stage labels left on the issue", madeAttrs...)
	return decide(stageResultPartial, count+1)
}

// reportStageHop records the decision on the run's history row and routes
// the stage event it produced. A zero runID means the row was never
// appended, so nothing is recorded and the event is still published. log
// must already carry issue_id and issue_identifier.
func reportStageHop(ctx context.Context, state *State, params HandleWorkerExitParams, runID int64, identity sessionEvent, hop stageHopDecision, log *slog.Logger) {
	if runID > 0 {
		if err := params.Store.RecordRunStageResult(ctx, runID, hop.TargetRule, hop.Result); err != nil {
			log.Error("failed to persist stage hop result", slog.Any("error", err))
		}
	}

	eventType := domain.EventStageNotAdvanced
	reason := hop.Result
	if hop.made() {
		eventType = domain.EventStageAdvanced
		reason = ""
	}
	notification := identity.notification(eventType, "", domain.StageFacts{})
	notification.Envelope.Stage = domain.StageTransition{
		SourceRule: hop.SourceRule,
		TargetRule: hop.TargetRule,
		ChainID:    hop.ChainID,
		HopCount:   hop.HopCount,
		Reason:     reason,
	}

	deliverDetached(ctx, &state.TrackerOpsWg, params.Router.Route(notification), log, func(received bool, err error) {
		if received && err != nil {
			log.Warn("stage event comment failed",
				slog.String("event_type", string(eventType)),
				slog.Any("error", err),
			)
		}
	})
}

// missingStageLabels returns the configured stage labels dispatchLabels
// carried and a fresh read of the issue no longer shows. It reads only when
// dispatchLabels carried one, and returns the read's error when it fails.
func missingStageLabels(ctx context.Context, tracker domain.TrackerAdapter, dispatch config.DispatchConfig, dispatchLabels []string, issueID string) ([]string, error) {
	carried := carriedStageLabels(dispatch.Rules, dispatchLabels)
	if len(carried) == 0 {
		return nil, nil
	}
	read, err := tracker.FetchIssueByID(ctx, issueID)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, label := range carried {
		if !carriesLabel(read.Labels, label) {
			missing = append(missing, label)
		}
	}
	return missing, nil
}

// carriedStageLabels returns the configured stage labels that labels
// carries, in rule list order, each once under [config.StageLabelsEqual].
func carriedStageLabels(rules []config.DispatchRule, labels []string) []string {
	var carried []string
	for _, rule := range rules {
		if rule.Stage == "" || !carriesLabel(labels, rule.Stage) {
			continue
		}
		if carriesLabel(carried, rule.Stage) {
			continue
		}
		carried = append(carried, rule.Stage)
	}
	return carried
}

// labelsAfterHop returns labels as a hop to targetLabel leaves them: a new
// non-nil slice without the stage labels the removal loop of [advanceStage]
// removes, then targetLabel lowercased unless a kept label already matches
// it. It exists so the hop record does not show the label the hop removed in
// place of the one it added.
func labelsAfterHop(labels []string, rules []config.DispatchRule, targetLabel string) []string {
	removed := slices.DeleteFunc(carriedStageLabels(rules, labels), func(stage string) bool {
		return config.StageLabelsEqual(stage, targetLabel)
	})
	after := make([]string, 0, len(labels)+1)
	for _, label := range labels {
		if !carriesLabel(removed, label) {
			after = append(after, label)
		}
	}
	if !carriesLabel(after, targetLabel) {
		after = append(after, strings.ToLower(targetLabel))
	}
	return after
}

// holdForStageHop keeps an issue that was just hopped from dispatching on
// any rule until a read shows the label the hop added. It returns the
// issue to dispatch, carrying the labels of the direct read when one was
// made, the comments of that read (non-nil exactly when the read was made and
// succeeded), and false when the issue must wait or its hop record was reset.
// activeSet and terminalSet hold lowercased state names.
func (o *Orchestrator) holdForStageHop(ctx context.Context, issue domain.Issue, activeSet, terminalSet map[string]struct{}) (domain.Issue, []domain.Comment, bool) {
	hop := o.state.StageHops[issue.ID]
	if hop == nil || hop.TargetObserved {
		return issue, nil, true
	}
	log := logging.WithIssue(o.logger, issue.ID, issue.Identifier)

	if carriesLabel(issue.Labels, hop.TargetLabel) {
		o.markObserved(ctx, issue.ID, hop, log)
		return issue, nil, true
	}

	read, err := o.trackerAdapter.FetchIssueByID(ctx, issue.ID)
	if err != nil {
		log.Warn("stage hop target read failed, holding issue",
			slog.String("target_label", hop.TargetLabel),
			slog.Any("error", err),
		)
		return issue, nil, false
	}

	// Reconcile reads every hop-record issue each tick and resets the
	// record at the second consecutive no-state reading; a single one,
	// possibly a label edit in flight, only holds the issue for this tick.
	if read.State == "" {
		log.Info("stage hop target read showed no state, holding issue",
			slog.String("target_label", hop.TargetLabel),
		)
		return issue, nil, false
	}

	readState := strings.ToLower(read.State)
	if _, terminal := terminalSet[readState]; terminal {
		resetStageHop(ctx, o.state, o.store, issue.ID, stageResetTerminal, log)
		recordIssueOutcome(ctx, o.store, terminalOutcome(issue.ID, read.State, time.Now()), log)
		return issue, nil, false
	}
	if _, active := activeSet[readState]; !active {
		resetStageHop(ctx, o.state, o.store, issue.ID, stageResetNotActive, log)
		return issue, nil, false
	}

	o.markObserved(ctx, issue.ID, hop, log)
	if !carriesLabel(read.Labels, hop.TargetLabel) {
		log.Info("stage hop target label absent, hold released",
			slog.String("target_rule", hop.TargetRule),
			slog.String("target_label", hop.TargetLabel),
		)
	}
	issue.Labels = read.Labels
	return issue, nonNilComments(read.Comments), true
}

// markObserved latches the hold release in memory and persists it.
// A failed write leaves the runtime value standing.
func (o *Orchestrator) markObserved(ctx context.Context, issueID string, hop *StageHopEntry, log *slog.Logger) {
	hop.TargetObserved = true
	if err := o.store.MarkStageHopObserved(ctx, issueID); err != nil {
		log.Error("failed to persist stage hop observation", slog.Any("error", err))
	}
}

// PopulateStageHops loads persisted hop records into state.StageHops.
// Called during startup recovery, before the event loop starts, after
// [PopulateParked].
func PopulateStageHops(state *State, rows []storage.StageHop, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	for _, row := range rows {
		if row.IssueID == "" {
			log.Warn("skipping malformed stage hop record")
			continue
		}

		hoppedAt, err := time.Parse(time.RFC3339, row.HoppedAt)
		if err != nil {
			hoppedAt = time.Time{}
		}

		state.StageHops[row.IssueID] = &StageHopEntry{
			Identifier:       row.Identifier,
			Count:            row.HopCount,
			SourceRule:       row.SourceRule,
			TargetRule:       row.TargetRule,
			TargetLabel:      row.TargetLabel,
			PreviousOutcome:  row.PreviousOutcome,
			SourceDispatchID: row.SourceDispatchID,
			TargetObserved:   row.TargetObserved,
			HoppedAt:         hoppedAt,
			ChainID:          row.ChainID,
			DisplayID:        row.DisplayID,
			IssueIdentity: IssueIdentity{
				Title:     row.IssueTitle,
				URL:       row.IssueURL,
				IssueType: row.IssueType,
				Labels:    row.IssueLabels,
				State:     row.IssueState,
			},
			DispatchedAt:  parseDispatchedAt(row.DispatchedAt),
			RunDispatched: row.RunDispatched,
		}
	}
}

// StageChainAdvisories returns the advisory for a longest stage chain that
// needs more runs than agent.max_sessions allows an issue. It performs no
// I/O and returns nil when the budget is unlimited or no rule carries
// next.
func StageChainAdvisories(cfg config.ServiceConfig) []config.Advisory {
	chain := cfg.Dispatch.LongestChain()
	if cfg.Agent.MaxSessions <= 0 || cfg.Agent.MaxSessions >= len(chain) {
		return nil
	}
	path := strings.Join(chain, stageChainSeparator)
	return []config.Advisory{{
		Check: "dispatch.next.max_sessions",
		Text: fmt.Sprintf("agent.max_sessions %d is smaller than the %d runs the longest stage chain needs (%s); an issue on it exhausts its session budget before the chain ends",
			cfg.Agent.MaxSessions, len(chain), path),
		Message: "agent.max_sessions is smaller than the longest stage chain needs",
		Attrs: []slog.Attr{
			slog.Int("max_sessions", cfg.Agent.MaxSessions),
			slog.Int("chain_runs", len(chain)),
			slog.String("chain_path", path),
		},
	}}
}

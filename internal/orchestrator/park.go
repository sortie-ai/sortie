package orchestrator

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/storage"
)

// Park reason and unpark trigger values, shared by both parking triggers
// and by every release path.
const (
	parkReasonAgentBlocked   = "agent_blocked"
	parkReasonHandoffAbsence = "handoff_absence"

	unparkTriggerStateChanged     = "state_changed"
	unparkTriggerLabelRemoved     = "label_removed"
	unparkTriggerEvidenceObserved = "evidence_observed"
)

// ParkStore is the persistence contract [parkIssue] and [unparkIssue] need.
type ParkStore interface {
	storage.ParkedIssueStore
	storage.RetryEntryStore
	storage.RunHistoryStore
	storage.StageHopStore
}

// parkHandOver is what a park keeps from the run it follows for the first
// run after a person releases it.
type parkHandOver struct {
	SessionID    string
	TemplateID   string // "" for the workflow body template
	Statement    string
	LastPromptAt time.Time
}

// parkHandOverOf derives the hand-over from the record of the run a park
// follows. The statement and the last-prompt moment always carry over. The
// session and template carry over only from a run with no reaction kind that
// ended normally, because a retry policy starts a new session after any other
// run, so resuming one of those sessions would resume a suspect conversation.
func parkHandOverOf(run storage.RunHistory) parkHandOver {
	handOver := parkHandOver{
		Statement:    run.AgentText,
		LastPromptAt: parseDispatchedAt(run.LastPromptAt),
	}
	if run.ReactionKind != "" || !endedNormally(run) {
		return handOver
	}
	handOver.SessionID = run.SessionID
	handOver.TemplateID = run.TemplateID
	return handOver
}

// endedNormally reports whether run succeeded or failed only because its
// handoff was withheld for lack of observed work.
func endedNormally(run storage.RunHistory) bool {
	switch run.Status {
	case "succeeded":
		return true
	case "failed":
		return run.Error != nil && strings.HasPrefix(*run.Error, storage.HandoffAbsenceErrorPrefix)
	default:
		return false
	}
}

// parkedRun is the run a park follows: its dispatch rule, its frozen stage
// lineage (pass dispatch time included), its agent kind, its configured
// model, and what it hands to the first run after a release. The zero value
// records no run.
type parkedRun struct {
	RuleName        string
	StageLineage    StageLineage
	AgentKind       string
	ConfiguredModel string
	HandOver        parkHandOver
}

// parkedRunOf returns the run row describes: AgentKind from AgentAdapter,
// StageLineage through [stageLineageOf], HandOver through [parkHandOverOf].
func parkedRunOf(row storage.RunHistory) parkedRun {
	return parkedRun{
		RuleName:        row.RuleName,
		StageLineage:    stageLineageOf(row),
		AgentKind:       row.AgentAdapter,
		ConfiguredModel: row.ConfiguredModel,
		HandOver:        parkHandOverOf(row),
	}
}

// parkIssueParams carries the inputs of one park.
type parkIssueParams struct {
	IssueID       string
	Identifier    string
	DisplayID     string
	IssueIdentity IssueIdentity // the issue as read; zero when unread
	ObservedState string        // tracker state to record as parked_state
	Reason        string
	Label         string // resolved parking label; empty disables the label write
	Absences      int    // log attribute only; omitted from the record when zero
	Ceiling       int    // log attribute only; omitted from the record when zero
	Run           parkedRun

	Store          ParkStore
	TrackerAdapter domain.TrackerAdapter // nil skips the label write
	Metrics        domain.Metrics
	Logger         *slog.Logger
	Ctx            context.Context
}

// parkIssue records the park, releases the claim, cancels and deletes any
// pending retry, resets the issue's stage hop count, and starts the detached
// parking-label write. A new park replaces a pending release of the same
// issue, since the full-replace upsert clears the row's release columns.
func parkIssue(state *State, params parkIssueParams) {
	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	CancelRetry(state, params.IssueID)
	delete(state.Claimed, params.IssueID)

	// An issue parked again still carries the label of its earlier park,
	// since an unpark never removes it.
	identity := params.IssueIdentity
	identity.Labels = labelsWithout(identity.Labels, params.Label)

	parkedAt := time.Now().UTC()
	entry := &ParkedEntry{
		Identifier:    params.Identifier,
		DisplayID:     params.DisplayID,
		IssueIdentity: identity,
		Reason:        params.Reason,
		ParkedState:   params.ObservedState,
		Label:         params.Label,
		LabelApplied:  false,
		ParkedAt:      parkedAt,

		RuleName:        params.Run.RuleName,
		StageLineage:    params.Run.StageLineage,
		AgentKind:       params.Run.AgentKind,
		ConfiguredModel: params.Run.ConfiguredModel,

		SessionID:    params.Run.HandOver.SessionID,
		TemplateID:   params.Run.HandOver.TemplateID,
		Statement:    params.Run.HandOver.Statement,
		LastPromptAt: params.Run.HandOver.LastPromptAt,
	}
	delete(state.ParkReleases, params.IssueID)
	state.Parked[params.IssueID] = entry

	if err := params.Store.UpsertParkedIssue(ctx, parkedIssueRow(params.IssueID, entry)); err != nil {
		log.Error("failed to persist park record", slog.Any("error", err))
	}
	if err := params.Store.DeleteRetryEntry(ctx, params.IssueID); err != nil {
		log.Error("failed to delete retry entry after park", slog.Any("error", err))
	}
	resetStageHop(ctx, state, params.Store, params.IssueID, stageResetParked, log)

	params.Metrics.IncIssueParks(params.Reason)

	attrs := []slog.Attr{
		slog.String("reason", params.Reason),
		slog.String("parked_state", params.ObservedState),
		slog.String("label", params.Label),
	}
	if params.Absences != 0 {
		attrs = append(attrs, slog.Int("consecutive_absences", params.Absences))
	}
	if params.Ceiling != 0 {
		attrs = append(attrs,
			slog.Int("absence_ceiling", params.Ceiling),
			slog.String("ceiling_setting", "agent.max_consecutive_absences"),
		)
	}
	log.LogAttrs(ctx, slog.LevelWarn, "issue parked", attrs...)

	if params.TrackerAdapter == nil || params.Label == "" {
		return
	}

	issueID := params.IssueID
	label := params.Label
	tracker := params.TrackerAdapter
	state.TrackerOpsWg.Go(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := tracker.AddLabel(dctx, issueID, label); err != nil {
			log.Warn("park label write failed", slog.Any("error", err))
		}
	})
}

// isReleaseGesture reports whether trigger is a person's release, as opposed
// to a worker exit that observed work.
func isReleaseGesture(trigger string) bool {
	return trigger == unparkTriggerLabelRemoved || trigger == unparkTriggerStateChanged
}

// unparkIssue lifts the park, resets the absence sequence and the issue's
// stage hop count, and settles the persisted row. A person's release moves
// the entry to state.ParkReleases and keeps the row, with its release
// columns set, until [consumeParkRelease] runs; any other trigger deletes the
// row. It is a no-op when issueID is absent from state.Parked.
func unparkIssue(ctx context.Context, state *State, issueID, trigger string, store ParkStore, log *slog.Logger) {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}

	entry, ok := state.Parked[issueID]
	if !ok {
		return
	}
	delete(state.Parked, issueID)

	if isReleaseGesture(trigger) {
		entry.ReleasedAt = time.Now().UTC().Truncate(time.Second)
		entry.ReleaseGesture = trigger
		state.ParkReleases[issueID] = entry
		if err := store.UpsertParkedIssue(ctx, parkedIssueRow(issueID, entry)); err != nil {
			log.Error("failed to persist park release", slog.Any("error", err))
		}
	} else if err := store.DeleteParkedIssue(ctx, issueID); err != nil {
		log.Error("failed to delete park record", slog.Any("error", err))
	}
	if err := store.ResetHandoffAbsenceSequence(ctx, issueID); err != nil {
		log.Error("failed to reset handoff absence sequence after unpark", slog.Any("error", err))
	}
	resetStageHop(ctx, state, store, issueID, stageResetUnparked, log)

	log.Info("issue unparked",
		slog.String("trigger", trigger),
		slog.String("reason", entry.Reason),
	)
}

// consumeParkRelease deletes the pending release of issueID from
// state.ParkReleases and its persisted row. It is a no-op when none is
// pending. A failed delete is logged and the runtime deletion stands.
func consumeParkRelease(ctx context.Context, state *State, store ParkStore, issueID string, log *slog.Logger) {
	if _, ok := state.ParkReleases[issueID]; !ok {
		return
	}
	delete(state.ParkReleases, issueID)
	if err := store.DeleteParkedIssue(ctx, issueID); err != nil {
		log.Error("failed to delete park release", slog.Any("error", err))
	}
}

// parkContextOf builds the park render context from a pending release, the
// issue's comments, and the IDs of the comments Sortie posted. The window
// opens at the parked run's last prompt, or at the park when no prompt was
// recorded, and includes that whole second. A comment whose time does not
// parse stays so a possible answer is never dropped, and a zero start keeps
// every comment. Sortie's own comments leave by exact ID alone, because a
// time, author, or text test would also drop a person's comment.
func parkContextOf(release *ParkedEntry, comments []domain.Comment, posted []string) prompt.ParkContext {
	start := release.LastPromptAt
	if start.IsZero() {
		start = release.ParkedAt
	}
	boundary := start.UTC().Truncate(time.Second)
	kept := make([]domain.Comment, 0, len(comments))
	for _, c := range comments {
		if slices.Contains(posted, c.ID) {
			continue
		}
		created, err := c.CreatedTime()
		if start.IsZero() || err != nil || !created.Before(boundary) {
			kept = append(kept, c)
		}
	}
	return prompt.ParkContext{
		Reason:     release.Reason,
		Statement:  release.Statement,
		ParkedAt:   formatWholeSecondUTC(release.ParkedAt),
		ReleasedAt: formatWholeSecondUTC(release.ReleasedAt),
		Gesture:    release.ReleaseGesture,
		Comments:   domain.CommentTemplateMaps(kept),
	}
}

// formatWholeSecondUTC renders t as RFC 3339 in UTC truncated to the whole
// second, or "" for the zero time.
func formatWholeSecondUTC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// PopulateParked loads persisted park records into state.Parked, and rows
// a person already released into state.ParkReleases. Called during startup
// recovery, before the event loop starts, beside [PopulateRetries].
func PopulateParked(state *State, rows []storage.ParkedIssue, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	for _, row := range rows {
		if row.IssueID == "" {
			log.Warn("skipping malformed park record")
			continue
		}

		parkedAt, err := time.Parse(time.RFC3339, row.ParkedAt)
		if err != nil {
			parkedAt = time.Time{}
		}

		entry := &ParkedEntry{
			Identifier: row.Identifier,
			DisplayID:  row.DisplayID,
			IssueIdentity: IssueIdentity{
				Title:     row.IssueTitle,
				URL:       row.IssueURL,
				IssueType: row.IssueType,
				Labels:    row.IssueLabels,
			},
			Reason:       row.Reason,
			ParkedState:  row.ParkedState,
			Label:        row.Label,
			LabelApplied: row.LabelApplied,
			ParkedAt:     parkedAt,

			RuleName: row.RuleName,
			StageLineage: StageLineage{
				PreviousRule:    row.StagePrevious,
				PreviousOutcome: row.StagePreviousOutcome,
				ChainID:         row.ChainID,
				DispatchedAt:    parseDispatchedAt(row.DispatchedAt),
			},
			AgentKind:       row.AgentKind,
			ConfiguredModel: row.ConfiguredModel,

			SessionID:    row.SessionID,
			TemplateID:   row.TemplateID,
			Statement:    row.Statement,
			LastPromptAt: parseDispatchedAt(row.LastPromptAt),
		}
		if row.ReleasedAt == "" {
			state.Parked[row.IssueID] = entry
			continue
		}
		entry.ReleasedAt, _ = time.Parse(time.RFC3339, row.ReleasedAt)
		entry.ReleaseGesture = row.ReleaseGesture
		state.ParkReleases[row.IssueID] = entry
	}
}

// observeParkedState evaluates the state-change release rule for one
// parked issue against an observed tracker state and reports whether the
// issue is still parked afterward. An empty observed state is ignored. An
// empty entry.ParkedState is backfilled from observed without releasing.
// A non-empty entry.ParkedState that differs from observed, compared
// case-insensitively, unparks the issue with trigger "state_changed".
func observeParkedState(ctx context.Context, state *State, store ParkStore, log *slog.Logger, entry *ParkedEntry, issueID, observed string) (stillParked bool) {
	if observed == "" {
		return true
	}
	if entry.ParkedState == "" {
		entry.ParkedState = observed
		if err := store.UpsertParkedIssue(ctx, parkedIssueRow(issueID, entry)); err != nil {
			log.Error("failed to persist park state backfill", slog.Any("error", err))
		}
		return true
	}
	if !strings.EqualFold(observed, entry.ParkedState) {
		unparkIssue(ctx, state, issueID, unparkTriggerStateChanged, store, log)
		return false
	}
	return true
}

// observeParkedLabels evaluates the label-confirmation and label-removal
// release rules for one parked issue against an observed label set. A
// parking label observed while unconfirmed sets and persists
// LabelApplied. A parking label observed absent while confirmed unparks
// the issue with trigger "label_removed". A parking label observed absent
// while unconfirmed leaves the park unchanged.
func observeParkedLabels(ctx context.Context, state *State, store ParkStore, log *slog.Logger, entry *ParkedEntry, issueID string, labels []string) {
	hasLabel := slices.ContainsFunc(labels, func(label string) bool {
		return strings.EqualFold(label, entry.Label)
	})

	if hasLabel && !entry.LabelApplied {
		entry.LabelApplied = true
		if err := store.MarkParkedIssueLabelApplied(ctx, issueID); err != nil {
			log.Error("failed to persist park label confirmation", slog.Any("error", err))
		}
		return
	}
	if !hasLabel && entry.LabelApplied {
		unparkIssue(ctx, state, issueID, unparkTriggerLabelRemoved, store, log)
	}
}

// labelsWithout returns a copy of labels without each label that
// [strings.EqualFold] matches to drop, comparing as [observeParkedLabels]
// does. A nil labels stays nil.
func labelsWithout(labels []string, drop string) []string {
	if labels == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(labels), func(label string) bool {
		return strings.EqualFold(label, drop)
	})
}

// parkedIssueRow builds the persisted row for the given runtime park entry.
func parkedIssueRow(issueID string, entry *ParkedEntry) storage.ParkedIssue {
	return storage.ParkedIssue{
		IssueID:      issueID,
		Identifier:   entry.Identifier,
		DisplayID:    entry.DisplayID,
		Reason:       entry.Reason,
		ParkedState:  entry.ParkedState,
		Label:        entry.Label,
		LabelApplied: entry.LabelApplied,
		ParkedAt:     entry.ParkedAt.Format(time.RFC3339),
		IssueTitle:   entry.IssueIdentity.Title,
		IssueURL:     entry.IssueIdentity.URL,
		IssueType:    entry.IssueIdentity.IssueType,
		IssueLabels:  entry.IssueIdentity.Labels,

		RuleName:             entry.RuleName,
		AgentKind:            entry.AgentKind,
		ConfiguredModel:      entry.ConfiguredModel,
		StagePrevious:        entry.StageLineage.PreviousRule,
		StagePreviousOutcome: entry.StageLineage.PreviousOutcome,
		ChainID:              entry.StageLineage.ChainID,
		DispatchedAt:         formatDispatchedAt(entry.StageLineage.DispatchedAt),

		SessionID:      entry.SessionID,
		TemplateID:     entry.TemplateID,
		Statement:      entry.Statement,
		LastPromptAt:   formatDispatchedAt(entry.LastPromptAt),
		ReleasedAt:     formatWholeSecondUTC(entry.ReleasedAt),
		ReleaseGesture: entry.ReleaseGesture,
	}
}

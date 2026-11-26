package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/sentence"
)

const (
	defaultIssueGroupsLimit = 20
	maxIssueGroupsLimit     = 50

	// issueGroupRunsPageSize is how many runs a group carries before it hands
	// the rest to GET /api/v1/runs.
	issueGroupRunsPageSize = 5
)

// issueGroupsResponse is the JSON wire format for GET /api/v1/runs/issues.
type issueGroupsResponse struct {
	Groups     []issueGroupResponse `json:"groups"`
	NextCursor *string              `json:"next_cursor"`
}

// issueGroupResponse is one issue of run history by issue. Runs holds the
// issue's newest matching runs; NextCursor continues them on GET /api/v1/runs.
type issueGroupResponse struct {
	IssueID           string                `json:"issue_id"`
	IssueIdentifier   string                `json:"issue_identifier"`
	DisplayIdentifier string                `json:"display_identifier,omitempty"`
	IssueTitle        string                `json:"issue_title,omitempty"`
	IssueURL          string                `json:"issue_url,omitempty"`
	Status            string                `json:"status"`
	StatusText        string                `json:"status_text"`
	StatusTextFull    string                `json:"status_text_full"`
	IssueOutcome      *issueOutcomeResponse `json:"issue_outcome"`
	Runs              []runResponse         `json:"runs"`
	NextCursor        *string               `json:"next_cursor"`
	Totals            historyTotalsResponse `json:"totals"`
}

var issueGroupsParams = []string{"issue", "q", "rule", "status", "stage", "cursor", "limit"}

// parseIssueGroupsQuery validates every parameter of GET /api/v1/runs/issues
// before any read. An empty value reads as absent, and Limit is the page size
// in groups.
func parseIssueGroupsQuery(values url.Values) (IssueGroupQuery, error) {
	if err := checkParamNames(values, issueGroupsParams); err != nil {
		return IssueGroupQuery{}, err
	}

	q := IssueGroupQuery{
		IssueKey:    values.Get("issue"),
		Search:      values.Get("q"),
		Status:      values.Get("status"),
		StageTarget: values.Get("stage"),
		Limit:       defaultIssueGroupsLimit,
	}

	if v := values.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil || limit < 1 || limit > maxIssueGroupsLimit {
			return IssueGroupQuery{}, &paramError{"limit", "must be an integer from 1 to 50"}
		}
		q.Limit = limit
	}
	if v := values.Get("cursor"); v != "" {
		cursor, err := strconv.ParseInt(v, 10, 64)
		if err != nil || cursor < 1 {
			return IssueGroupQuery{}, &paramError{"cursor", "must be an integer of at least 1"}
		}
		q.BeforeID = cursor
	}

	if v := values.Get("rule"); v == noRuleFilter {
		q.RuleName = new("")
	} else if v != "" {
		q.RuleName = new(v)
	}
	return q, nil
}

func (s *Server) routeIssueGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleIssueGroups(w, r)
}

func (s *Server) handleIssueGroups(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.runHistory == nil {
		writeHistoryUnavailable(w, s.logger)
		return
	}

	q, err := parseIssueGroupsQuery(r.URL.Query())
	if err != nil {
		s.logger.Debug("run history request rejected",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}

	// The snapshot comes before any history read, so a run that ends between
	// the reads appears in both and never in neither.
	snap := s.snapshotFn()
	words := s.sentenceContext(snap)

	ctx := r.Context()
	limit := q.Limit
	q.Limit = limit + 1
	keys, err := s.runHistory.QueryIssueGroups(ctx, q)
	if err != nil {
		s.groupReadFailed(w, r, err)
		return
	}
	var next *string
	if len(keys) > limit {
		keys = keys[:limit]
		next = new(strconv.FormatInt(keys[limit-1].NewestRunID, 10))
	}

	groups := []issueGroupResponse{}
	if len(keys) > 0 {
		issueIDs := make([]string, len(keys))
		for i, key := range keys {
			issueIDs[i] = key.IssueID
		}
		outcomes, err := s.runHistory.IssueOutcomes(ctx, issueIDs)
		if err != nil {
			s.groupReadFailed(w, r, err)
			return
		}
		for _, key := range keys {
			group, ok, err := s.buildIssueGroup(ctx, key, q, snap, words, outcomes)
			if err != nil {
				s.groupReadFailed(w, r, err)
				return
			}
			if ok {
				groups = append(groups, group)
			}
		}
	}
	writeJSON(w, s.logger, http.StatusOK, issueGroupsResponse{Groups: groups, NextCursor: next})

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

// buildIssueGroup assembles the group of one selected issue. It reports false
// when the issue has no run left to describe it.
func (s *Server) buildIssueGroup(ctx context.Context, key IssueGroupKey, q IssueGroupQuery, snap orchestrator.RuntimeSnapshotResult, words sentence.Context, outcomes map[string]IssueOutcome) (issueGroupResponse, bool, error) {
	page, err := s.runHistory.QueryRuns(ctx, RunHistoryQuery{
		IssueID:     key.IssueID,
		RuleName:    q.RuleName,
		Status:      q.Status,
		StageTarget: q.StageTarget,
		BeforeID:    key.NewestRunID + 1,
		Limit:       issueGroupRunsPageSize + 1,
	})
	if err != nil {
		return issueGroupResponse{}, false, err
	}
	page, runsNext := cutPage(page, issueGroupRunsPageSize)

	newestRows := page
	if q.RuleName != nil || q.Status != "" || q.StageTarget != "" || len(page) == 0 {
		newestRows, err = s.runHistory.QueryRuns(ctx, RunHistoryQuery{IssueID: key.IssueID, Limit: 1})
		if err != nil {
			return issueGroupResponse{}, false, err
		}
	}
	if len(newestRows) == 0 {
		return issueGroupResponse{}, false, nil
	}
	newest := newestRows[0]

	totals, err := s.runHistory.IssueTotals(ctx, key.IssueID)
	if err != nil {
		return issueGroupResponse{}, false, err
	}

	var chains [][]string
	if words.Shape != nil {
		chains = words.Shape.Dispatch.Chains
	}
	outcome, hasOutcome := outcomes[key.IssueID]
	group := issueGroupResponse{
		IssueID:           key.IssueID,
		IssueIdentifier:   newest.Identifier,
		DisplayIdentifier: newest.DisplayID,
		IssueTitle:        redact.Mask(newest.IssueTitle),
		IssueURL:          redact.Mask(newest.IssueURL),
		Runs:              toRunResponses(page, chains),
		NextCursor:        runsNext,
		Totals:            historyTotalsOf(totals),
	}
	attachIssueOutcomes(group.Runs, outcomes)
	if hasOutcome {
		group.IssueOutcome = toIssueOutcomeResponse(outcome, newest.Identifier, newest.DisplayID)
	}
	if live := buildIssueDetail("", key.IssueID, snap, words); live != nil {
		group.Status, group.StatusText, group.StatusTextFull = live.Status, live.StatusText, live.StatusTextFull
	} else {
		group.Status = "history"
		group.StatusText, group.StatusTextFull = sentencesOf(sentence.Entry{Finished: finishedOf(newest, outcome, hasOutcome)}, words)
	}
	return group, true, nil
}

// groupReadFailed ends a request whose history read failed: silently when the
// client has gone, and with 503 otherwise.
func (s *Server) groupReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		s.logger.Debug("run history request canceled",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		return
	}
	s.historyReadFailed(w, r, err)
}

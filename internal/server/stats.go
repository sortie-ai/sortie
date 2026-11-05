package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/runstats"
)

const statsBoundRule = "must be an RFC 3339 timestamp, a date such as 2026-07-01, or a positive duration such as 24h"

var statsParams = []string{"since", "until", "period"}

var spanParams = []string{"since", "until"}

const statsPeriodRule = "must be five_minutes, quarter_hour, hour, day, or month"

// StatsFunc computes the run statistics report over the runs completed within
// r, with the spend series in period p; an empty p lets the server rule pick
// it. generatedAt is the report time, in UTC. It must be safe for concurrent
// use.
type StatsFunc func(ctx context.Context, r runstats.Range, p runstats.Period, generatedAt time.Time) (runstats.Report, error)

// SpanFunc computes the report of the sessions with turns that ended at or
// after since and before until, a nil until being open. generatedAt is the
// report time, in UTC. It must be safe for concurrent use.
type SpanFunc func(ctx context.Context, since time.Time, until *time.Time, generatedAt time.Time) (runstats.SpanReport, error)

// parseBounds reads the since and until bounds of a query. An empty value
// reads as absent, and a relative bound counts from now.
func parseBounds(values url.Values, now time.Time) (since, until *time.Time, err error) {
	for _, bound := range []struct {
		name string
		dest **time.Time
	}{{"since", &since}, {"until", &until}} {
		at, err := runstats.ParseRangeBound(values.Get(bound.name), now)
		if err != nil {
			return nil, nil, &paramError{bound.name, statsBoundRule}
		}
		*bound.dest = at
	}
	if since != nil && until != nil && !since.Before(*until) {
		return nil, nil, &paramError{"since", "must be before until"}
	}
	return since, until, nil
}

// parseStatsQuery validates every parameter of GET /api/v1/stats before any
// read, so a malformed bound never serves an unbounded report as a bounded
// one. An empty value reads as absent.
func parseStatsQuery(values url.Values, now time.Time) (runstats.Range, runstats.Period, error) {
	if err := checkParamNames(values, statsParams); err != nil {
		return runstats.Range{}, "", err
	}
	since, until, err := parseBounds(values, now)
	if err != nil {
		return runstats.Range{}, "", err
	}
	r := runstats.Range{
		Since:           since,
		Until:           until,
		SinceIsDuration: runstats.IsDurationBound(values.Get("since")),
	}
	text := values.Get("period")
	if text == "" {
		return r, "", nil
	}
	period, err := runstats.ParsePeriod(text)
	if err != nil {
		return runstats.Range{}, "", &paramError{"period", statsPeriodRule}
	}
	if err := runstats.CheckSpendPeriod(r, period, now); err != nil {
		return runstats.Range{}, "", &paramError{"period", err.Error()}
	}
	return r, period, nil
}

// parseSpanQuery validates every parameter of GET /api/v1/stats/span before
// any read. since is required, and both bounds are exact.
func parseSpanQuery(values url.Values, now time.Time) (time.Time, *time.Time, error) {
	if err := checkParamNames(values, spanParams); err != nil {
		return time.Time{}, nil, err
	}
	if values.Get("since") == "" {
		return time.Time{}, nil, &paramError{"since", "is required"}
	}
	since, until, err := parseBounds(values, now)
	if err != nil {
		return time.Time{}, nil, err
	}
	return *since, until, nil
}

func (s *Server) routeStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleStats(w, r)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.stats == nil {
		writeHistoryUnavailable(w, s.logger)
		return
	}

	now := start.UTC()
	rng, period, err := parseStatsQuery(r.URL.Query(), now)
	if err != nil {
		s.logger.Debug("stats request rejected",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}

	report, err := s.stats(r.Context(), rng, period, now)
	if r.Context().Err() != nil {
		s.logger.Debug("stats request canceled",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		return
	}
	if err != nil {
		s.historyReadFailed(w, r, err)
		return
	}
	writeJSON(w, s.logger, http.StatusOK, report)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) routeStatsSpan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleStatsSpan(w, r)
}

func (s *Server) handleStatsSpan(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.statsSpan == nil {
		writeHistoryUnavailable(w, s.logger)
		return
	}

	now := start.UTC()
	since, until, err := parseSpanQuery(r.URL.Query(), now)
	if err != nil {
		s.logger.Debug("stats span request rejected",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}

	report, err := s.statsSpan(r.Context(), since, until, now)
	if r.Context().Err() != nil {
		s.logger.Debug("stats span request canceled",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		return
	}
	if err != nil {
		s.historyReadFailed(w, r, err)
		return
	}
	for i := range report.Sessions {
		report.Sessions[i].IssueTitle = redact.Mask(report.Sessions[i].IssueTitle)
	}
	writeJSON(w, s.logger, http.StatusOK, report)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

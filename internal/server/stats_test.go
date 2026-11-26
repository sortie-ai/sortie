package server

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/runstats"
)

type statsCall struct {
	r           runstats.Range
	period      runstats.Period
	generatedAt time.Time
}

func TestStatsQueryBoundary(t *testing.T) {
	t.Parallel()

	july1 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		path       string
		query      string
		wantParam  string
		wantSince  *time.Time
		wantAgo    time.Duration
		wantPeriod runstats.Period
		failWith   error
	}{
		{name: "unknown parameter", query: "format=json", wantParam: "format"},
		{name: "repeated parameter", query: "since=24h&since=48h", wantParam: "since"},
		{name: "day-suffixed duration", query: "since=7d", wantParam: "since"},
		{name: "zero duration", query: "until=0s", wantParam: "until"},
		{name: "timestamp without offset", query: "since=2026-07-01T00:00:00", wantParam: "since"},
		{name: "since after until as dates", query: "since=2026-07-02&until=2026-07-01", wantParam: "since"},
		{name: "since after until as durations", query: "since=24h&until=48h", wantParam: "since"},
		{name: "empty values read as absent", query: "since=&until="},
		{name: "date reads as midnight UTC", query: "since=2026-07-01", wantSince: &july1},
		{name: "duration counts back from the report time", query: "since=168h", wantAgo: 168 * time.Hour},
		{name: "unknown period", query: "since=336h&period=week", wantParam: "period"},
		{name: "period without since", query: "period=hour", wantParam: "period"},
		{name: "period over 336 buckets", query: "since=2026-10-01&until=2026-10-08&period=five_minutes", wantParam: "period"},
		{name: "period within 336 buckets", query: "since=336h&period=hour", wantAgo: 336 * time.Hour, wantPeriod: runstats.PeriodHour},
		{name: "span without since", path: "/api/v1/stats/span", query: "until=2026-10-08", wantParam: "since"},
		{name: "stats failure answers 503", failWith: errors.New("db closed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := make(chan statsCall, 2)
			srv := New(Params{
				SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
				RefreshFn:  acceptingRefresh(),
				Logger:     slog.New(slog.DiscardHandler),
				Stats: func(_ context.Context, r runstats.Range, p runstats.Period, generatedAt time.Time) (runstats.Report, error) {
					calls <- statsCall{r, p, generatedAt}
					return runstats.Report{}, tt.failWith
				},
				StatsSpan: func(_ context.Context, since time.Time, until *time.Time, generatedAt time.Time) (runstats.SpanReport, error) {
					calls <- statsCall{runstats.Range{Since: &since, Until: until}, "", generatedAt}
					return runstats.SpanReport{}, tt.failWith
				},
			})
			ts := httptest.NewServer(srv.Mux())
			t.Cleanup(ts.Close)

			url := cmp.Or(tt.path, "/api/v1/stats") + "?" + tt.query
			status, body := getJSON[errorResponse](t, ts, url)

			wantStatus, wantCode, wantCalls := http.StatusOK, "", 1
			switch {
			case tt.wantParam != "":
				wantStatus, wantCode, wantCalls = http.StatusBadRequest, "invalid_parameter", 0
			case tt.failWith != nil:
				wantStatus, wantCode = http.StatusServiceUnavailable, "history_unavailable"
			}
			if status != wantStatus || body.Error.Code != wantCode {
				t.Fatalf("GET %s = %d %q, want %d %q", url, status, body.Error.Code, wantStatus, wantCode)
			}
			if got := len(calls); got != wantCalls {
				t.Fatalf("GET %s called the stats function %d times, want %d", url, got, wantCalls)
			}
			if tt.wantParam != "" && !strings.Contains(body.Error.Message, `"`+tt.wantParam+`"`) {
				t.Errorf("GET %s error message = %q, want it to name parameter %q", url, body.Error.Message, tt.wantParam)
			}
			if wantStatus != http.StatusOK {
				return
			}

			call := <-calls
			if tt.wantAgo != 0 {
				ago := call.generatedAt.Add(-tt.wantAgo)
				tt.wantSince = &ago
			}
			if since := call.r.Since; (since == nil) != (tt.wantSince == nil) || (tt.wantSince != nil && !since.Equal(*tt.wantSince)) {
				t.Errorf("GET %s since = %v, want %v", url, since, tt.wantSince)
			}
			if call.r.Until != nil {
				t.Errorf("GET %s until = %v, want nil", url, call.r.Until)
			}
			if wantDuration := tt.wantAgo != 0; call.r.SinceIsDuration != wantDuration {
				t.Errorf("GET %s SinceIsDuration = %t, want %t", url, call.r.SinceIsDuration, wantDuration)
			}
			if call.period != tt.wantPeriod {
				t.Errorf("GET %s period = %q, want %q", url, call.period, tt.wantPeriod)
			}
		})
	}
}

func TestStatsSpanMasksRegisteredValues(t *testing.T) {
	t.Parallel()

	const secret = "span-test-credential-4b8e2a"
	redact.Add("test.stats span masking", secret)

	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		StatsSpan: func(context.Context, time.Time, *time.Time, time.Time) (runstats.SpanReport, error) {
			return runstats.SpanReport{Sessions: []runstats.SpanSession{
				{Identifier: "MT-1", IssueTitle: "Fix " + secret, Status: "succeeded"},
				{Identifier: "MT-2", IssueTitle: "Run " + secret, Status: "running"},
			}}, nil
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	status, got := getJSON[runstats.SpanReport](t, ts, "/api/v1/stats/span?since=24h")

	if status != http.StatusOK || len(got.Sessions) != 2 {
		t.Fatalf("GET /api/v1/stats/span = status %d with %d sessions, want 200 with 2", status, len(got.Sessions))
	}
	for _, session := range got.Sessions {
		if strings.Contains(session.IssueTitle, secret) || !strings.Contains(session.IssueTitle, redact.Marker) {
			t.Errorf("GET /api/v1/stats/span %s issue_title = %q, want the registered value masked", session.Identifier, session.IssueTitle)
		}
	}
}

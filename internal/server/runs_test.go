package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/redact"
)

func TestRunsQueryBoundary(t *testing.T) {
	t.Parallel()

	const groupsPath = "/api/v1/runs/issues"
	tests := []struct {
		name      string
		path      string
		query     string
		wantParam string
	}{
		{name: "unknown parameter", query: "bogus=1", wantParam: "bogus"},
		{name: "repeated parameter", query: "rule=a&rule=b", wantParam: "rule"},
		{name: "limit below range", query: "limit=0", wantParam: "limit"},
		{name: "limit above range", query: "limit=201", wantParam: "limit"},
		{name: "limit not a number", query: "limit=x", wantParam: "limit"},
		{name: "cursor below range", query: "cursor=0", wantParam: "cursor"},
		{name: "cursor not a number", query: "cursor=x", wantParam: "cursor"},
		{name: "malformed since", query: "since=yesterday", wantParam: "since"},
		{name: "malformed until", query: "until=tomorrow", wantParam: "until"},
		{name: "since equals until as instants", query: "since=2026-03-24T09:00:00Z&until=2026-03-24T11:00:00%2B02:00", wantParam: "since"},
		{name: "ended_since equals ended_until", query: "ended_since=2026-03-24T09:00:00Z&ended_until=2026-03-24T09:00:00Z", wantParam: "ended_since"},
		{name: "empty values read as absent", query: "limit=&cursor=&rule=&since=2026-03-24T09:00:00Z&until=2026-03-24T12:00:00%2B02:00"},
		{name: "limit at lower bound", query: "limit=1"},
		{name: "limit at upper bound", query: "limit=200"},
		{name: "cursor at lower bound", query: "cursor=1"},
		{name: "issues: limit below range", path: groupsPath, query: "limit=0", wantParam: "limit"},
		{name: "issues: limit above range", path: groupsPath, query: "limit=51", wantParam: "limit"},
		{name: "issues: cursor below range", path: groupsPath, query: "cursor=0", wantParam: "cursor"},
		{name: "issues: parameter of the flat route", path: groupsPath, query: "ended_since=2026-03-24T09:00:00Z", wantParam: "ended_since"},
		{name: "issues: repeated issue", path: groupsPath, query: "issue=a&issue=b", wantParam: "issue"},
		{name: "issues: empty values read as absent", path: groupsPath, query: "issue=&rule=&status=&stage=&cursor=&limit="},
		{name: "issues: limit at upper bound", path: groupsPath, query: "limit=50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := cmp.Or(tt.path, "/api/v1/runs") + "?" + tt.query
			var reads atomic.Int32
			ts := historyServer(t, orchestrator.RuntimeSnapshotResult{}, &fakeRunHistory{
				onQuery:  func(RunHistoryQuery) { reads.Add(1) },
				onGroups: func(IssueGroupQuery) { reads.Add(1) },
			})

			status, body := getJSON[errorResponse](t, ts, path)

			wantStatus, wantReads := http.StatusOK, int32(1)
			if tt.wantParam != "" {
				wantStatus, wantReads = http.StatusBadRequest, 0
			}
			if status != wantStatus {
				t.Fatalf("GET %s status = %d, want %d", path, status, wantStatus)
			}
			if got := reads.Load(); got != wantReads {
				t.Errorf("GET %s read run history %d times, want %d", path, got, wantReads)
			}
			if tt.wantParam == "" {
				return
			}
			if body.Error.Code != "invalid_parameter" {
				t.Errorf("GET %s error code = %q, want %q", path, body.Error.Code, "invalid_parameter")
			}
			if !strings.Contains(body.Error.Message, `"`+tt.wantParam+`"`) {
				t.Errorf("GET %s error message = %q, want it to name parameter %q", path, body.Error.Message, tt.wantParam)
			}
		})
	}
}

func TestRunsFailedOutcomeReadAnswers503(t *testing.T) {
	t.Parallel()

	ts := historyServer(t, orchestrator.RuntimeSnapshotResult{}, &fakeRunHistory{
		rows:        []RunHistoryEntry{{ID: 3, IssueID: "id-1", Identifier: "MT-100", Status: "succeeded"}},
		outcomesErr: errors.New("db closed"),
	})

	status, body := getJSON[errorResponse](t, ts, "/api/v1/runs")

	if status != http.StatusServiceUnavailable || body.Error.Code != "history_unavailable" {
		t.Errorf("GET /api/v1/runs = %d %q, want %d %q", status, body.Error.Code, http.StatusServiceUnavailable, "history_unavailable")
	}
}

func TestRunsMaskRegisteredValues(t *testing.T) {
	t.Parallel()

	const secret = "runs-test-credential-7c1d9e"
	redact.Add("test.runs response masking", secret)

	failure := "agent failed with " + secret
	review := `{"enabled":true,"final_verdict":"blocked by ` + secret + `","total_iterations":1}`
	ts := historyServer(t, orchestrator.RuntimeSnapshotResult{}, &fakeRunHistory{rows: []RunHistoryEntry{{
		ID:             7,
		IssueID:        "id-1",
		Identifier:     "MT-1",
		IssueTitle:     "Fix " + secret,
		IssueURL:       "https://tracker.example/" + secret,
		Status:         "failed",
		Error:          &failure,
		ReviewMetadata: &review,
		AgentText:      "Need " + secret,
	}}})

	status, got := getJSON[runsResponse](t, ts, "/api/v1/runs")

	if status != http.StatusOK || len(got.Runs) != 1 {
		t.Fatalf("GET /api/v1/runs = status %d with %d runs, want 200 with 1", status, len(got.Runs))
	}
	run := got.Runs[0]
	if run.Error == nil || run.SelfReview == nil || run.AgentText == nil {
		t.Fatalf("GET /api/v1/runs run error, self_review, agent_text = %v, %v, %v, want all present", run.Error, run.SelfReview, run.AgentText)
	}
	served := map[string]string{
		"issue_title":               run.IssueTitle,
		"issue_url":                 run.IssueURL,
		"error":                     *run.Error,
		"agent_text":                *run.AgentText,
		"self_review.final_verdict": run.SelfReview.FinalVerdict,
	}
	for field, value := range served {
		if strings.Contains(value, secret) {
			t.Errorf("GET /api/v1/runs %s = %q, want the registered value masked", field, value)
		}
		if !strings.Contains(value, redact.Marker) {
			t.Errorf("GET /api/v1/runs %s = %q, want it to hold %q", field, value, redact.Marker)
		}
	}
}

func withEmptyProvenance(w historyTotalsResponse) historyTotalsResponse {
	w.UnmeasuredUnits = []string{}
	w.VendorAmounts = []unitAmountResponse{}
	w.AgentPricedModels = []string{}
	return w
}

func TestHistoryTotalsOf(t *testing.T) {
	t.Parallel()

	spend := domain.TokenUsage{InputTokens: 1_000_000, OutputTokens: 100_000, TotalTokens: 1_100_000}
	partialTokens := tokenInfo{
		InputTokens: new(int64(1_000_000)), OutputTokens: new(int64(100_000)), TotalTokens: new(int64(1_100_000)),
	}

	tests := []struct {
		name string
		in   IssueRunTotals
		want historyTotalsResponse
	}{
		{
			name: "every run measured and priced",
			in: IssueRunTotals{
				Usage:  orchestrator.SnapshotIssueUsage{Sessions: 2, TotalTokens: 1_100_000, UncachedTokens: 850_000, RecordedCostUSD: 27, PricedSessions: 2},
				Tokens: domain.TokenUsage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 300_000},
			},
			want: withEmptyProvenance(historyTotalsResponse{
				RunCount: 2,
				Tokens: tokenInfo{
					InputTokens: new(int64(1_000_000)), OutputTokens: new(int64(100_000)), TotalTokens: new(int64(1_100_000)),
					CacheReadTokens: new(int64(300_000)), CacheWriteTokens: new(int64(0)),
				},
				UncachedTokens:   new(int64(850_000)),
				TokensComplete:   true,
				EstimatedCostUSD: new(27.0),
				CostStatus:       costStatusEstimated,
			}),
		},
		{
			name: "unmeasured run beside priced spend",
			in: IssueRunTotals{
				Usage:  orchestrator.SnapshotIssueUsage{Sessions: 3, UnmeasuredSessions: 1, TotalTokens: 1_100_000, UncachedTokens: 1_100_000, RecordedCostUSD: 27, PricedSessions: 2},
				Tokens: spend,
			},
			want: withEmptyProvenance(historyTotalsResponse{
				RunCount:         3,
				UnmeasuredRuns:   1,
				Tokens:           partialTokens,
				UncachedTokens:   new(int64(1_100_000)),
				EstimatedCostUSD: new(27.0),
				CostStatus:       costStatusPartial,
			}),
		},
		{
			name: "unmeasured run and nothing else",
			in:   IssueRunTotals{Usage: orchestrator.SnapshotIssueUsage{Sessions: 1, UnmeasuredSessions: 1}},
			want: withEmptyProvenance(historyTotalsResponse{RunCount: 1, UnmeasuredRuns: 1, CostStatus: costStatusUnmeasured}),
		},
		{
			name: "vendor amounts and an agent-priced share leave the figures unchanged",
			in: IssueRunTotals{
				Usage:  orchestrator.SnapshotIssueUsage{Sessions: 2, TotalTokens: 1_100_000, UncachedTokens: 850_000, RecordedCostUSD: 27, PricedSessions: 2},
				Tokens: domain.TokenUsage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 300_000},
				Provenance: IssueProvenance{
					UnmeasuredCause:   "amounts_only",
					UnmeasuredUnits:   []string{"credits"},
					VendorAmounts:     []UnitAmount{{Unit: "credits", Amount: 3.62}, {Unit: "premium_requests", Amount: 2, LowerBound: true}},
					AgentPricedUSD:    new(0.4),
					AgentPricedModels: []string{"", "model-a"},
				},
			},
			want: historyTotalsResponse{
				RunCount: 2,
				Tokens: tokenInfo{
					InputTokens: new(int64(1_000_000)), OutputTokens: new(int64(100_000)), TotalTokens: new(int64(1_100_000)),
					CacheReadTokens: new(int64(300_000)), CacheWriteTokens: new(int64(0)),
				},
				UncachedTokens:    new(int64(850_000)),
				TokensComplete:    true,
				EstimatedCostUSD:  new(27.0),
				CostStatus:        costStatusEstimated,
				UnmeasuredCause:   new("amounts_only"),
				UnmeasuredUnits:   []string{"credits"},
				VendorAmounts:     []unitAmountResponse{{Unit: "credits", Amount: 3.62}, {Unit: "premium_requests", Amount: 2, LowerBound: true}},
				AgentPricedUSD:    new(0.4),
				AgentPricedModels: []string{"", "model-a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := historyTotalsOf(tt.in)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("historyTotalsOf(%+v) = %v, want %v", tt.in, jsonObject(t, got), jsonObject(t, tt.want))
			}
		})
	}
}

func TestToRunResponseModelUsage(t *testing.T) {
	t.Parallel()

	spend := domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CacheReadTokens: 40, CacheWriteTokens: 7}
	const spendJSON = `{"input_tokens":100,"output_tokens":50,"total_tokens":150,"cache_read_tokens":40,"cache_write_tokens":7}`

	tests := []struct {
		name  string
		parts []RunModelUsage
		want  string
	}{
		{"no part", nil, `[]`},
		{
			"part recorded without a price",
			[]RunModelUsage{{Model: "model-a", Tokens: spend}},
			`[{"model":"model-a","tokens":` + spendJSON + `,"uncached_tokens":110,"estimated_cost_usd":null,"cost_status":"unpriced","unrated_classes":null,"price_source":null}]`,
		},
		{
			"priced part with no unrated class recorded",
			[]RunModelUsage{{Model: "model-a", Tokens: spend, EstimatedCostUSD: new(1.5), CostStatus: "estimated", UnratedRecorded: true}},
			`[{"model":"model-a","tokens":` + spendJSON + `,"uncached_tokens":110,"estimated_cost_usd":1.5,"cost_status":"estimated","unrated_classes":[],"price_source":null}]`,
		},
		{
			"partial part with an unrated class",
			[]RunModelUsage{{Model: "model-b", Tokens: spend, EstimatedCostUSD: new(0.2), CostStatus: "partial", UnratedClasses: []string{"cache_write"}, UnratedRecorded: true}},
			`[{"model":"model-b","tokens":` + spendJSON + `,"uncached_tokens":110,"estimated_cost_usd":0.2,"cost_status":"partial","unrated_classes":["cache_write"],"price_source":null}]`,
		},
		{
			"part priced from an agent-reported amount",
			[]RunModelUsage{{Model: "model-a", Tokens: spend, EstimatedCostUSD: new(1.5), CostStatus: "estimated", PriceSource: "agent"}},
			`[{"model":"model-a","tokens":` + spendJSON + `,"uncached_tokens":110,"estimated_cost_usd":1.5,"cost_status":"estimated","unrated_classes":null,"price_source":"agent"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := toRunResponse(RunHistoryEntry{ID: 1, ModelUsage: tt.parts}, nil)

			got, err := json.Marshal(resp.ModelUsage)
			if err != nil {
				t.Fatalf("json.Marshal(model_usage): %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("toRunResponse(%q).model_usage = %s, want %s", tt.name, got, tt.want)
			}
		})
	}
}

func TestToRunResponseUsageMembers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   RunHistoryEntry
		want string
	}{
		{
			"recorded before provenance was kept",
			RunHistoryEntry{UsageCompleteness: "unmeasured", UnmeasuredCause: "unknown"},
			`{"unmeasured_cause":"unknown","unmeasured_units":[],"usage_completeness":"unmeasured","usage_provenance":null,"vendor_amounts":null}`,
		},
		{
			"provenance known and empty",
			RunHistoryEntry{UsageCompleteness: "complete", UsageProvenance: []UsagePair{}, VendorAmounts: []VendorAmount{}},
			`{"unmeasured_cause":null,"unmeasured_units":[],"usage_completeness":"complete","usage_provenance":[],"vendor_amounts":[]}`,
		},
		{
			"run that reported only vendor units",
			RunHistoryEntry{
				UsageCompleteness: "unmeasured",
				UsageProvenance:   []UsagePair{{Source: "protocol", Strategy: "live"}, {Source: "journal", Strategy: "records"}},
				VendorAmounts:     []VendorAmount{{Unit: "credits", Model: "model-a", Amount: 3.62, LowerBound: true}},
				UnmeasuredCause:   "amounts_only",
				UnmeasuredUnits:   []string{"credits"},
			},
			`{"unmeasured_cause":"amounts_only","unmeasured_units":["credits"],"usage_completeness":"unmeasured",` +
				`"usage_provenance":[{"source":"protocol","strategy":"live"},{"source":"journal","strategy":"records"}],` +
				`"vendor_amounts":[{"unit":"credits","model":"model-a","amount":3.62,"lower_bound":true}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var served map[string]json.RawMessage
			data, err := json.Marshal(toRunResponse(tt.in, nil))
			if err == nil {
				err = json.Unmarshal(data, &served)
			}
			if err != nil {
				t.Fatalf("round-tripping toRunResponse(%q): %v", tt.name, err)
			}
			picked := map[string]json.RawMessage{}
			for _, key := range []string{"unmeasured_cause", "unmeasured_units", "usage_completeness", "usage_provenance", "vendor_amounts"} {
				picked[key] = served[key]
			}

			got, _ := json.Marshal(picked)

			if string(got) != tt.want {
				t.Errorf("toRunResponse(%q) usage members = %s, want %s", tt.name, got, tt.want)
			}
		})
	}
}

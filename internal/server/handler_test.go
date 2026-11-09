package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/sentence"
)

func fixedSnapshot(snap orchestrator.RuntimeSnapshotResult) SnapshotFunc {
	return func() orchestrator.RuntimeSnapshotResult {
		return snap
	}
}

func acceptingRefresh() RefreshFunc {
	return func() bool { return true }
}

func coalescingRefresh() RefreshFunc {
	return func() bool { return false }
}

func testServer(t *testing.T, snapFn SnapshotFunc, refreshFn RefreshFunc) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn: snapFn,
		RefreshFn:  refreshFn,
		Logger:     slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	return v
}

func getJSON[T any](t *testing.T, ts *httptest.Server, path string) (int, T) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, decodeJSON[T](t, resp)
}

func jsonObject(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(%T): %v", v, err)
	}
	return decoded
}

func assertJSONKeys(t *testing.T, body any, want map[string]any) {
	t.Helper()
	got := jsonObject(t, body)
	for key, wantValue := range jsonObject(t, want) {
		gotValue, present := got[key]
		gotNum, gotIsNum := gotValue.(float64)
		wantNum, wantIsNum := wantValue.(float64)
		near := gotIsNum && wantIsNum && math.Abs(gotNum-wantNum) <= 1e-9
		if !present || !near && !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("JSON %q = %v (present=%t), want %v", key, gotValue, present, wantValue)
		}
	}
}

const wantPendingReactionsJSON = `[
  {"issue_id": "mno345", "issue_identifier": "MT-653", "issue_title": "Add the export button",
   "issue_url": "https://tracker.example.com/browse/MT-653", "kind": "ci", "config_key": "ci_failure",
   "pull_request": {"number": 931, "repository": "acme/web", "url": "https://github.com/acme/web/pull/931",
                    "branch": "sortie/MT-653", "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"},
   "created_at": "2026-02-24T20:09:30Z", "dispatched_at": "2026-02-24T20:05:00Z", "next_poll_at": "2026-02-24T20:17:40Z", "wait": "poll_interval", "stage_position": null,
   "used_attempts": 2, "max_attempts": 2, "escalated_at": "2026-02-24T20:12:40Z", "escalation_trigger": "budget_exhausted", "watch_ends_at": null,
   "last_check": {"observed_at": "2026-02-24T20:12:40Z", "head_sha": "9b7e3d1c0a2f4e6b8d0c1a3e5f7b9d2c4e6a8f0b",
                  "status": "failing", "failing_checks": ["Test"], "failing": 1, "pending": 0, "passing": 4},
   "status_text": "CI escalated on #931",
   "status_text_full": "Sortie escalated failing CI on PR #931; it keeps watching the pull request. CI fixes used: 2 of 2."},
  {"issue_id": "mno345", "issue_identifier": "MT-653", "issue_title": "Add the export button",
   "issue_url": "https://tracker.example.com/browse/MT-653", "kind": "review", "config_key": "review_comments",
   "pull_request": {"number": 931, "repository": "acme/web", "url": "https://github.com/acme/web/pull/931",
                    "branch": "sortie/MT-653", "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"},
   "created_at": "2026-02-24T20:09:30Z", "dispatched_at": null, "next_poll_at": "2026-02-24T20:16:05Z", "wait": "debounce", "stage_position": null,
   "used_attempts": 1, "max_attempts": 3, "escalated_at": null, "watch_ends_at": null,
   "last_check": {"observed_at": "2026-02-24T20:15:10Z", "actionable_comments": 3, "new_comments": 1},
   "status_text": "1 new review comment on #931; next check in 30s",
   "status_text_full": "Waiting for 1 new review comment on PR #931 (3 actionable) to settle; next check in 30s. Review fix runs used: 1 of 3."}
]`

func pendingReactionRows() []orchestrator.SnapshotPendingReaction {
	base := orchestrator.SnapshotPendingReaction{
		IssueID: "mno345", Identifier: "MT-653",
		IssueTitle: "Add the export button", IssueURL: "https://tracker.example.com/browse/MT-653",
		PullRequest: orchestrator.SnapshotPullRequest{
			Number: 931, Repository: "acme/web", URL: "https://github.com/acme/web/pull/931",
			Branch: "sortie/MT-653", HeadSHA: "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b",
		},
	}

	ci := base
	ci.Kind, ci.ConfigKey, ci.Wait = "ci", "ci_failure", "poll_interval"
	ci.CreatedAt = time.Date(2026, 2, 24, 15, 9, 30, 0, time.FixedZone("EST", -5*60*60))
	ci.DispatchedAt = time.Date(2026, 2, 24, 15, 5, 0, 0, time.FixedZone("EST", -5*60*60))
	ci.NextPollAt = time.Date(2026, 2, 24, 20, 17, 40, 0, time.UTC)
	ci.UsedAttempts, ci.MaxAttempts = new(2), new(2)
	ci.EscalatedAt = new(time.Date(2026, 2, 24, 20, 12, 40, 0, time.UTC))
	ci.EscalationTrigger = "budget_exhausted"
	ci.LastCheck = &orchestrator.ReactionCheck{
		ObservedAt: time.Date(2026, 2, 24, 20, 12, 40, 0, time.UTC),
		HeadSHA:    "9b7e3d1c0a2f4e6b8d0c1a3e5f7b9d2c4e6a8f0b",
		CI:         &orchestrator.CICheck{Status: "failing", FailingChecks: []string{"Test"}, Failing: 1, Passing: 4},
	}

	review := base
	review.Kind, review.ConfigKey, review.Wait = "review", "review_comments", "debounce"
	review.CreatedAt = time.Date(2026, 2, 24, 20, 9, 30, 0, time.UTC)
	review.NextPollAt = time.Date(2026, 2, 24, 20, 16, 5, 0, time.UTC)
	review.UsedAttempts, review.MaxAttempts = new(1), new(3)
	review.LastCheck = &orchestrator.ReactionCheck{
		ObservedAt: time.Date(2026, 2, 24, 20, 15, 10, 0, time.UTC),
		Comments:   &orchestrator.CommentCheck{Actionable: 3, New: 1},
	}

	return []orchestrator.SnapshotPendingReaction{ci, review}
}

func TestToRunningEntryResponse(t *testing.T) {
	t.Parallel()

	est := time.FixedZone("EST", -5*60*60)
	startedAt := time.Date(2026, 3, 24, 12, 0, 0, 0, est)
	lastEventAt := time.Date(2026, 3, 24, 12, 5, 0, 0, est)

	entry := orchestrator.SnapshotRunningEntry{
		LastAgentEvent:     domain.EventTurnCompleted,
		LastAgentTimestamp: lastEventAt,
		LastAgentMessage:   "Implementing feature",
		StartedAt:          startedAt,
		AgentInputTokens:   1000,
		AgentOutputTokens:  500,
		AgentTotalTokens:   1500,
		UsageMeasured:      true,
	}

	got := toRunningEntryResponse(entry)

	if got.LastEvent != string(domain.EventTurnCompleted) {
		t.Errorf("LastEvent = %q, want %q", got.LastEvent, domain.EventTurnCompleted)
	}
	if got.LastMessage != "Implementing feature" {
		t.Errorf("LastMessage = %q, want %q", got.LastMessage, "Implementing feature")
	}

	if got.StartedAt.Location() != time.UTC {
		t.Errorf("StartedAt location = %v, want UTC", got.StartedAt.Location())
	}
	if !got.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, startedAt)
	}
	if got.LastEventAt.Location() != time.UTC {
		t.Errorf("LastEventAt location = %v, want UTC", got.LastEventAt.Location())
	}

	if got.Tokens.InputTokens == nil || got.Tokens.OutputTokens == nil || got.Tokens.TotalTokens == nil {
		t.Fatalf("Tokens = %+v, want three non-nil members on a measured entry", got.Tokens)
	}
	if *got.Tokens.InputTokens != 1000 {
		t.Errorf("Tokens.InputTokens = %d, want %d", *got.Tokens.InputTokens, 1000)
	}
	if *got.Tokens.OutputTokens != 500 {
		t.Errorf("Tokens.OutputTokens = %d, want %d", *got.Tokens.OutputTokens, 500)
	}
	if *got.Tokens.TotalTokens != 1500 {
		t.Errorf("Tokens.TotalTokens = %d, want %d", *got.Tokens.TotalTokens, 1500)
	}
}

func TestToRunningEntryResponse_ConfiguredSettingsBesideTheReportedModel(t *testing.T) {
	t.Parallel()

	bare := orchestrator.SnapshotRunningEntry{IssueID: "id-1", Identifier: "MT-1", State: "In Progress", StartedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)}
	populated := bare
	populated.RuleName, populated.ConfiguredModel, populated.ConfiguredEffort, populated.ModelName = "cheap", "configured-model", "low", "reported-model"
	tests := []struct {
		name  string
		entry orchestrator.SnapshotRunningEntry
		want  map[string]any
	}{
		{"populated", populated, map[string]any{"rule_name": "cheap", "configured_model": "configured-model", "configured_effort": "low", "model_name": "reported-model"}},
		{"bare omits every empty key", bare, map[string]any{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(toRunningEntryResponse(tt.entry))
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			got := map[string]any{}
			for _, key := range []string{"rule_name", "configured_model", "configured_effort", "model_name"} {
				if value, present := decoded[key]; present {
					got[key] = value
				}
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("running object settings keys = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestToRunningEntryResponse_UsageDispositionFields proves the four
// new members are additive: every member the JSON response
// carried before this change keeps its name, type, and value. It
// covers one incremental entry, whose APIRequestsMeasured is true and
// TokensPending is always false, and one turn_end entry, whose
// APIRequestsMeasured is false and TokensPending reflects the frozen
// snapshot's own TokensPending field. Both verdicts are resolved
// upstream and carried on the snapshot; the constructor re-derives
// neither.
func TestToRunningEntryResponse_UsageDispositionFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                    string
		arrival                 registry.UsageArrival
		attribution             registry.UsageAttribution
		tokensPending           bool
		wantAPIRequestsMeasured bool
	}{
		{
			name:                    "incremental arrival reports during the turn",
			arrival:                 registry.UsageArrivalIncremental,
			attribution:             registry.UsageAttributionPerModel,
			tokensPending:           false,
			wantAPIRequestsMeasured: true,
		},
		{
			name:                    "turn_end arrival, tokens pending",
			arrival:                 registry.UsageArrivalTurnEnd,
			attribution:             registry.UsageAttributionSessionTotal,
			tokensPending:           true,
			wantAPIRequestsMeasured: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry := orchestrator.SnapshotRunningEntry{
				IssueID:             "issue-usage",
				Identifier:          "MT-USAGE",
				State:               "In Progress",
				UsageArrival:        tt.arrival,
				UsageAttribution:    tt.attribution,
				TokensPending:       tt.tokensPending,
				APIRequestsMeasured: tt.wantAPIRequestsMeasured,
			}

			got := toRunningEntryResponse(entry)

			if got.UsageArrival != string(tt.arrival) {
				t.Errorf("UsageArrival = %q, want %q", got.UsageArrival, tt.arrival)
			}
			if got.UsageAttribution != string(tt.attribution) {
				t.Errorf("UsageAttribution = %q, want %q", got.UsageAttribution, tt.attribution)
			}
			if got.TokensPending != tt.tokensPending {
				t.Errorf("TokensPending = %v, want %v", got.TokensPending, tt.tokensPending)
			}
			if got.APIRequestsMeasured != tt.wantAPIRequestsMeasured {
				t.Errorf("APIRequestsMeasured = %v, want %v", got.APIRequestsMeasured, tt.wantAPIRequestsMeasured)
			}

			// Additivity: an existing member's name, type, and value
			// survive JSON round-tripping unchanged.
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if decoded["issue_id"] != "issue-usage" {
				t.Errorf("JSON issue_id = %v, want %q (existing member unaffected)", decoded["issue_id"], "issue-usage")
			}
			if decoded["state"] != "In Progress" {
				t.Errorf("JSON state = %v, want %q (existing member unaffected)", decoded["state"], "In Progress")
			}
			if decoded["usage_arrival"] != string(tt.arrival) {
				t.Errorf("JSON usage_arrival = %v, want %q", decoded["usage_arrival"], tt.arrival)
			}
			if decoded["usage_attribution"] != string(tt.attribution) {
				t.Errorf("JSON usage_attribution = %v, want %q", decoded["usage_attribution"], tt.attribution)
			}
			if decoded["tokens_pending"] != tt.tokensPending {
				t.Errorf("JSON tokens_pending = %v, want %v", decoded["tokens_pending"], tt.tokensPending)
			}
			if decoded["api_requests_measured"] != tt.wantAPIRequestsMeasured {
				t.Errorf("JSON api_requests_measured = %v, want %v", decoded["api_requests_measured"], tt.wantAPIRequestsMeasured)
			}
		})
	}
}

// TestToRunningEntryResponse_RequestsByModelGate_FromRuntimeSnapshot is
// exercised from [orchestrator.RuntimeSnapshot] outward over a real
// [orchestrator.State], because the breakdown's gate lives in
// RuntimeSnapshot itself and a hand-built SnapshotRunningEntry would
// bypass it. It also asserts that api_requests_measured equals
// api_request_count != null on the same serialized row.
func TestToRunningEntryResponse_RequestsByModelGate_FromRuntimeSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		arrival          registry.UsageArrival
		attribution      registry.UsageAttribution
		turnCount        int
		apiRequestCount  int
		requestsByModel  map[string]int
		wantMeasured     bool
		wantBreakdownKey bool
	}{
		{
			name:             "verdict false: breakdown absent despite per-model attribution",
			arrival:          registry.UsageArrivalIncremental,
			attribution:      registry.UsageAttributionPerModel,
			turnCount:        2,
			apiRequestCount:  0,
			requestsByModel:  map[string]int{"claude-sonnet-4-20250514": 3},
			wantMeasured:     false,
			wantBreakdownKey: false,
		},
		{
			name:             "verdict true, attribution does not name a model: breakdown absent",
			arrival:          registry.UsageArrivalIncremental,
			attribution:      registry.UsageAttributionSessionTotal,
			turnCount:        1,
			apiRequestCount:  5,
			requestsByModel:  map[string]int{"claude-sonnet-4-20250514": 5},
			wantMeasured:     true,
			wantBreakdownKey: false,
		},
		{
			name:             "verdict true, per-model attribution: breakdown present",
			arrival:          registry.UsageArrivalIncremental,
			attribution:      registry.UsageAttributionPerModel,
			turnCount:        1,
			apiRequestCount:  5,
			requestsByModel:  map[string]int{"claude-sonnet-4-20250514": 5},
			wantMeasured:     true,
			wantBreakdownKey: true,
		},
		{
			name:             "verdict true, zero turns and zero requests: genuine zero, not unmeasured",
			arrival:          registry.UsageArrivalIncremental,
			attribution:      registry.UsageAttributionSessionTotal,
			turnCount:        0,
			apiRequestCount:  0,
			requestsByModel:  nil,
			wantMeasured:     true,
			wantBreakdownKey: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := orchestrator.NewState(5000, 10, 0, nil, orchestrator.AgentTotals{})
			state.Running["issue-1"] = &orchestrator.RunningEntry{
				Identifier:       "MT-RBM",
				Issue:            domain.Issue{ID: "issue-1", State: "In Progress"},
				StartedAt:        time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
				TurnCount:        tt.turnCount,
				APIRequestCount:  tt.apiRequestCount,
				RequestsByModel:  tt.requestsByModel,
				UsageArrival:     tt.arrival,
				UsageAttribution: tt.attribution,
			}

			snap := orchestrator.RuntimeSnapshot(state, time.Date(2026, 3, 24, 12, 1, 0, 0, time.UTC))
			if len(snap.Running) != 1 {
				t.Fatalf("len(snap.Running) = %d, want 1", len(snap.Running))
			}

			got := toRunningEntryResponse(snap.Running[0])
			if got.APIRequestsMeasured != tt.wantMeasured {
				t.Fatalf("APIRequestsMeasured = %v, want %v", got.APIRequestsMeasured, tt.wantMeasured)
			}
			// The boolean and the count's nullity must agree.
			if (got.APIRequestCount != nil) != got.APIRequestsMeasured {
				t.Errorf("APIRequestCount != nil is %v, want it to equal APIRequestsMeasured (%v)",
					got.APIRequestCount != nil, got.APIRequestsMeasured)
			}

			data, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			_, hasKey := decoded["requests_by_model"]
			if hasKey != tt.wantBreakdownKey {
				t.Errorf("JSON has requests_by_model key = %v, want %v", hasKey, tt.wantBreakdownKey)
			}

			// The wire must carry the count itself, not merely agree on
			// its nullity: a measured genuine zero must reach the wire
			// as 0, never as null.
			wantAPIRequestCount := decoded["api_request_count"]
			if tt.wantMeasured {
				if wantAPIRequestCount != float64(tt.apiRequestCount) {
					t.Errorf("JSON api_request_count = %#v, want %v", wantAPIRequestCount, tt.apiRequestCount)
				}
			} else if wantAPIRequestCount != nil {
				t.Errorf("JSON api_request_count = %#v, want null", wantAPIRequestCount)
			}
		})
	}
}

// TestToRunningEntryResponse_TokenFiguresNullGate asserts that the four
// tokens members are nil together exactly when the
// entry's UsageMeasured is false, four numbers otherwise, including a
// genuine zero the entry's own runtime reported, and a tokens_pending
// entry carries four numbers rather than a null.
func TestToRunningEntryResponse_TokenFiguresNullGate(t *testing.T) {
	t.Parallel()

	t.Run("unmeasured entry: all four members nil, present in JSON as null", func(t *testing.T) {
		t.Parallel()

		entry := orchestrator.SnapshotRunningEntry{
			IssueID:           "issue-unmeasured",
			Identifier:        "MT-UNMEASURED",
			AgentInputTokens:  999, // raw figures the presenter must not read
			AgentOutputTokens: 999,
			AgentTotalTokens:  999,
			CacheReadTokens:   999,
			UsageMeasured:     false,
		}

		got := toRunningEntryResponse(entry)
		if got.Tokens != (tokenInfo{}) {
			t.Errorf("Tokens = %+v, want all four members nil", got.Tokens)
		}

		data, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		tokens, ok := decoded["tokens"].(map[string]any)
		if !ok {
			t.Fatalf("JSON tokens is not an object: %v", decoded["tokens"])
		}
		for _, field := range []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens"} {
			if value, exists := tokens[field]; !exists {
				t.Errorf("JSON tokens.%s key missing, want present as null", field)
			} else if value != nil {
				t.Errorf("JSON tokens.%s = %v, want null", field, value)
			}
		}
	})

	t.Run("measured entry with a genuine zero cache figure: four numbers, zero not null", func(t *testing.T) {
		t.Parallel()

		entry := orchestrator.SnapshotRunningEntry{
			IssueID:           "issue-measured-zero-cache",
			Identifier:        "MT-ZEROCACHE",
			AgentInputTokens:  1000,
			AgentOutputTokens: 500,
			AgentTotalTokens:  1500,
			CacheReadTokens:   0, // the runtime reports usage but no cache data
			UsageMeasured:     true,
		}

		got := toRunningEntryResponse(entry)
		if got.Tokens.CacheReadTokens == nil {
			t.Fatal("Tokens.CacheReadTokens = nil, want a non-nil pointer to 0")
		}
		if *got.Tokens.CacheReadTokens != 0 {
			t.Errorf("Tokens.CacheReadTokens = %d, want 0", *got.Tokens.CacheReadTokens)
		}

		data, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		tokens := decoded["tokens"].(map[string]any)
		if value, exists := tokens["cache_read_tokens"]; !exists {
			t.Error("JSON tokens.cache_read_tokens key missing")
		} else if value != float64(0) {
			t.Errorf("JSON tokens.cache_read_tokens = %v, want 0 (not null)", value)
		}
	})

	t.Run("tokens_pending entry carries four numbers, never a null", func(t *testing.T) {
		t.Parallel()

		entry := orchestrator.SnapshotRunningEntry{
			IssueID:           "issue-pending",
			Identifier:        "MT-PENDING",
			AgentInputTokens:  200,
			AgentOutputTokens: 100,
			AgentTotalTokens:  300,
			CacheReadTokens:   50,
			UsageMeasured:     true,
			TokensPending:     true,
		}

		got := toRunningEntryResponse(entry)
		if !got.TokensPending {
			t.Fatal("TokensPending = false, want true")
		}
		if got.Tokens.InputTokens == nil || got.Tokens.OutputTokens == nil ||
			got.Tokens.TotalTokens == nil || got.Tokens.CacheReadTokens == nil {
			t.Fatalf("Tokens = %+v, want four non-nil members on a tokens_pending entry", got.Tokens)
		}
	})
}

// TestToRunningEntryResponse_TimingPercentages verifies that
// toRunningEntryResponse computes correct tool_time_percent and
// api_time_percent when called with a now argument.
func TestToRunningEntryResponse_TimingPercentages(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		toolTimeMs  int64
		apiTimeMs   int64
		now         time.Time
		wantToolNil bool
		wantAPINil  bool
		wantToolPct float64
		wantAPIPct  float64
	}{
		{
			name:        "both timing values present",
			toolTimeMs:  6000,  // 6s tool time
			apiTimeMs:   30000, // 30s API time
			now:         startedAt.Add(60 * time.Second),
			wantToolPct: 10.0, // 6000/60000 * 100
			wantAPIPct:  50.0, // 30000/60000 * 100
		},
		{
			name:        "zero tool time yields nil",
			toolTimeMs:  0,
			apiTimeMs:   15000,
			now:         startedAt.Add(60 * time.Second),
			wantToolNil: true,
			wantAPIPct:  25.0,
		},
		{
			name:        "zero api time yields nil",
			toolTimeMs:  3000,
			apiTimeMs:   0,
			now:         startedAt.Add(60 * time.Second),
			wantToolPct: 5.0,
			wantAPINil:  true,
		},
		{
			name:        "both zero yields nil",
			toolTimeMs:  0,
			apiTimeMs:   0,
			now:         startedAt.Add(60 * time.Second),
			wantToolNil: true,
			wantAPINil:  true,
		},
		{
			name:        "zero elapsed yields nil",
			toolTimeMs:  500,
			apiTimeMs:   500,
			now:         startedAt,
			wantToolNil: true,
			wantAPINil:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry := orchestrator.SnapshotRunningEntry{
				IssueID:    "pct-1",
				Identifier: "MT-PCT",
				StartedAt:  startedAt,
				ToolTimeMs: tt.toolTimeMs,
				APITimeMs:  tt.apiTimeMs,
			}

			got := toRunningEntryResponse(entry, tt.now)

			if tt.wantToolNil {
				if got.ToolTimePercent != nil {
					t.Errorf("ToolTimePercent = %v, want nil", *got.ToolTimePercent)
				}
			} else {
				if got.ToolTimePercent == nil {
					t.Fatal("ToolTimePercent = nil, want non-nil")
				}
				if diff := *got.ToolTimePercent - tt.wantToolPct; diff > 0.01 || diff < -0.01 {
					t.Errorf("ToolTimePercent = %.4f, want %.4f", *got.ToolTimePercent, tt.wantToolPct)
				}
			}

			if tt.wantAPINil {
				if got.APITimePercent != nil {
					t.Errorf("APITimePercent = %v, want nil", *got.APITimePercent)
				}
			} else {
				if got.APITimePercent == nil {
					t.Fatal("APITimePercent = nil, want non-nil")
				}
				if diff := *got.APITimePercent - tt.wantAPIPct; diff > 0.01 || diff < -0.01 {
					t.Errorf("APITimePercent = %.4f, want %.4f", *got.APITimePercent, tt.wantAPIPct)
				}
			}
		})
	}
}

// TestToRunningEntryResponse_NoNowArg_NilPercentages verifies backward
// compatibility: calling without now produces nil percentages.
func TestToRunningEntryResponse_NoNowArg_NilPercentages(t *testing.T) {
	t.Parallel()

	entry := orchestrator.SnapshotRunningEntry{
		IssueID:    "compat-1",
		Identifier: "MT-COMPAT",
		StartedAt:  time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
		ToolTimeMs: 5000,
		APITimeMs:  10000,
	}

	got := toRunningEntryResponse(entry)

	if got.ToolTimePercent != nil {
		t.Errorf("ToolTimePercent = %v, want nil (no now arg)", *got.ToolTimePercent)
	}
	if got.APITimePercent != nil {
		t.Errorf("APITimePercent = %v, want nil (no now arg)", *got.APITimePercent)
	}
}

// TestToRunningEntryResponse_TimingJSON_NullWhenNil verifies that
// the JSON output contains null for timing percentages when they are nil.
func TestToRunningEntryResponse_TimingJSON_NullWhenNil(t *testing.T) {
	t.Parallel()

	entry := orchestrator.SnapshotRunningEntry{
		IssueID:    "json-null",
		Identifier: "MT-NUL",
		StartedAt:  time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
	}

	got := toRunningEntryResponse(entry)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded["tool_time_percent"] != nil {
		t.Errorf("JSON tool_time_percent = %v, want null", decoded["tool_time_percent"])
	}
	if decoded["api_time_percent"] != nil {
		t.Errorf("JSON api_time_percent = %v, want null", decoded["api_time_percent"])
	}
}

// TestToRunningEntryResponse_TimingJSON_NumberWhenPresent verifies that
// the JSON output contains numeric values for timing percentages.
func TestToRunningEntryResponse_TimingJSON_NumberWhenPresent(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	now := startedAt.Add(100 * time.Second) // 100s elapsed = 100000ms

	entry := orchestrator.SnapshotRunningEntry{
		IssueID:    "json-num",
		Identifier: "MT-NUM",
		StartedAt:  startedAt,
		ToolTimeMs: 25000, // 25%
		APITimeMs:  50000, // 50%
	}

	got := toRunningEntryResponse(entry, now)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	toolPct, ok := decoded["tool_time_percent"].(float64)
	if !ok {
		t.Fatalf("JSON tool_time_percent is not a number: %v", decoded["tool_time_percent"])
	}
	if diff := toolPct - 25.0; diff > 0.01 || diff < -0.01 {
		t.Errorf("JSON tool_time_percent = %v, want 25.0", toolPct)
	}

	apiPct, ok := decoded["api_time_percent"].(float64)
	if !ok {
		t.Fatalf("JSON api_time_percent is not a number: %v", decoded["api_time_percent"])
	}
	if diff := apiPct - 50.0; diff > 0.01 || diff < -0.01 {
		t.Errorf("JSON api_time_percent = %v, want 50.0", apiPct)
	}
}

// TestToRunningEntryResponse_ZeroStartedAt_NilPercentages verifies that
// when StartedAt is the zero time, timing percentages are nil even when
// now is provided and timing values are non-zero.
func TestToRunningEntryResponse_ZeroStartedAt_NilPercentages(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	entry := orchestrator.SnapshotRunningEntry{
		IssueID:    "zero-start",
		Identifier: "MT-ZS",
		StartedAt:  time.Time{}, // zero value
		ToolTimeMs: 5000,
		APITimeMs:  10000,
	}

	got := toRunningEntryResponse(entry, now)

	if got.ToolTimePercent != nil {
		t.Errorf("ToolTimePercent = %v, want nil (zero StartedAt)", *got.ToolTimePercent)
	}
	if got.APITimePercent != nil {
		t.Errorf("APITimePercent = %v, want nil (zero StartedAt)", *got.APITimePercent)
	}
}

// TestToRunningEntryResponse_ZeroStartedAt_JSON_Null verifies that the
// JSON output contains null for timing percentages when StartedAt is zero.
func TestToRunningEntryResponse_ZeroStartedAt_JSON_Null(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	entry := orchestrator.SnapshotRunningEntry{
		IssueID:    "zero-start-json",
		Identifier: "MT-ZSJ",
		StartedAt:  time.Time{},
		ToolTimeMs: 5000,
		APITimeMs:  10000,
	}

	got := toRunningEntryResponse(entry, now)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded["tool_time_percent"] != nil {
		t.Errorf("JSON tool_time_percent = %v, want null (zero StartedAt)", decoded["tool_time_percent"])
	}
	if decoded["api_time_percent"] != nil {
		t.Errorf("JSON api_time_percent = %v, want null (zero StartedAt)", decoded["api_time_percent"])
	}
}

func TestEntryResponses_KeysJSON(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	passAt := time.Date(2026, 3, 24, 4, 0, 0, 0, time.FixedZone("EST", -5*60*60))
	lineage := orchestrator.StageLineage{ChainID: "chain-1", PreviousRule: "specify", PreviousOutcome: "succeeded", DispatchedAt: passAt}
	position := &orchestrator.SnapshotStagePosition{Chain: []string{"specify", "implement"}, Current: "implement", HopCount: 1, HopCeiling: 3}
	wantLineage := map[string]any{"chain_id": "chain-1", "previous": "specify", "previous_outcome": "succeeded"}
	wantPosition := map[string]any{"chain": []any{"specify", "implement"}, "current": "implement", "hop_count": float64(1), "hop_ceiling": float64(3)}
	tests := []struct {
		name   string
		body   any
		want   map[string]any
		absent []string
	}{
		{
			name: "running ci continuation",
			body: toRunningEntryResponse(orchestrator.SnapshotRunningEntry{
				IssueID:             "id-1",
				Identifier:          "PROJ-1",
				DisplayID:           "acme/web#1",
				IssueTitle:          "Fix the login redirect",
				IssueURL:            "https://acme.atlassian.net/browse/PROJ-1",
				IssueLabels:         []string{"backend", "urgent"},
				IssueType:           "Bug",
				State:               "In Progress",
				SessionID:           "sess-1",
				TurnCount:           4,
				SelfReviewActive:    true,
				SelfReviewIteration: 2,
				WorkspacePath:       "/work/PROJ-1",
				SSHHost:             "build-1.example.test",
				WorkflowFile:        "backend.WORKFLOW.md",
				AgentKind:           "claude-code",
				TokensAwaited:       true,
				StartedAt:           startedAt,
				StageLineage:        lineage,
				StagePosition:       position,
				ReactionKind:        "ci",
				ReactionConfigKey:   "ci_failure",
				Continuation:        &orchestrator.SnapshotContinuation{Number: 931, Repository: "acme/web", Attempt: 1},
			}),
			want: map[string]any{
				"issue_id":              "id-1",
				"issue_identifier":      "PROJ-1",
				"display_identifier":    "acme/web#1",
				"issue_title":           "Fix the login redirect",
				"issue_url":             "https://acme.atlassian.net/browse/PROJ-1",
				"issue_labels":          []any{"backend", "urgent"},
				"issue_type":            "Bug",
				"state":                 "In Progress",
				"session_id":            "sess-1",
				"turn_count":            float64(4),
				"workspace_path":        "/work/PROJ-1",
				"ssh_host":              "build-1.example.test",
				"workflow_file":         "backend.WORKFLOW.md",
				"agent_kind":            "claude-code",
				"self_review_active":    true,
				"self_review_iteration": float64(2),
				"tokens_awaited":        true,
				"dispatched_at":         "2026-03-24T09:00:00Z",
				"stage_lineage":         wantLineage,
				"stage_position":        wantPosition,
				"reaction_kind":         "ci",
				"reaction_config_key":   "ci_failure",
				"reaction_attempt":      float64(1),
				"pull_request":          map[string]any{"number": float64(931), "repository": "acme/web"},
			},
		},
		{
			name: "running on a chain's first stage has no lineage and no continuation",
			body: toRunningEntryResponse(orchestrator.SnapshotRunningEntry{
				StageLineage:  orchestrator.StageLineage{ChainID: "chain-1", DispatchedAt: passAt},
				StagePosition: &orchestrator.SnapshotStagePosition{Chain: []string{"specify", "implement"}, Current: "specify", HopCeiling: 3},
			}),
			want: map[string]any{
				"dispatched_at":  "2026-03-24T09:00:00Z",
				"stage_lineage":  nil,
				"stage_position": map[string]any{"chain": []any{"specify", "implement"}, "current": "specify", "hop_count": float64(0), "hop_ceiling": float64(3)},
			},
			absent: []string{"reaction_kind", "reaction_config_key", "reaction_attempt", "pull_request"},
		},
		{
			name: "retry",
			body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{
				IssueID:         "id-2",
				Identifier:      "PROJ-2",
				DisplayID:       "acme/web#2",
				IssueTitle:      "Retry me",
				IssueURL:        "https://acme.atlassian.net/browse/PROJ-2",
				IssueType:       "Task",
				IssueLabels:     []string{"backend", "urgent"},
				State:           "To Do",
				Attempt:         3,
				DueAtMS:         startedAt.UnixMilli(),
				Error:           "agent timeout",
				AgentKind:       "claude-code",
				ConfiguredModel: "opus",
				StageLineage:    lineage,
				StagePosition:   position,
			}),
			want: map[string]any{
				"issue_id":           "id-2",
				"issue_identifier":   "PROJ-2",
				"display_identifier": "acme/web#2",
				"issue_title":        "Retry me",
				"issue_url":          "https://acme.atlassian.net/browse/PROJ-2",
				"issue_type":         "Task",
				"issue_labels":       []any{"backend", "urgent"},
				"state":              "To Do",
				"attempt":            float64(3),
				"due_at":             "2026-03-24T12:00:00Z",
				"error":              "agent timeout",
				"dispatched_at":      "2026-03-24T09:00:00Z",
				"agent_kind":         "claude-code",
				"configured_model":   "opus",
				"stage_lineage":      wantLineage,
				"stage_position":     wantPosition,
			},
		},
		{
			name:   "retry without a read or a dated pass omits the facts and serves null",
			body:   toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Attempt: 1}),
			want:   map[string]any{"attempt": float64(1), "dispatched_at": nil, "stage_position": nil},
			absent: []string{"issue_type", "issue_labels", "state", "agent_kind", "configured_model"},
		},
		{
			name: "retry whose issue carries no labels serves an empty list",
			body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{IssueLabels: []string{}}),
			want: map[string]any{"issue_labels": []any{}},
		},
		{
			name: "running measured usage and per-model requests",
			body: toRunningEntryResponse(orchestrator.SnapshotRunningEntry{
				IssueID:             "id-4",
				Identifier:          "PROJ-4",
				StartedAt:           startedAt,
				AgentInputTokens:    1000,
				AgentOutputTokens:   500,
				AgentTotalTokens:    1500,
				CacheReadTokens:     3000,
				ModelName:           "test-model",
				APIRequestCount:     7,
				RequestsByModel:     map[string]int{"test-model": 7},
				UsageMeasured:       true,
				APIRequestsMeasured: true,
			}),
			want: map[string]any{
				"tokens": map[string]any{
					"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500,
					"cache_read_tokens": 3000, "cache_write_tokens": 0,
				},
				"model_name":        "test-model",
				"api_request_count": 7,
				"requests_by_model": map[string]any{"test-model": 7},
			},
		},
		{name: "retry trigger error", body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Trigger: "error"}), want: map[string]any{"trigger": "error"}},
		{name: "retry trigger continuation", body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Trigger: "continuation"}), want: map[string]any{"trigger": "continuation"}},
		{name: "retry trigger stall", body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Trigger: "stall"}), want: map[string]any{"trigger": "stall"}},
		{
			name: "retry trigger timer carries the reschedule reason",
			body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Trigger: "timer", Error: "no available orchestrator slots"}),
			want: map[string]any{"trigger": "timer", "error": "no available orchestrator slots"},
		},
		{
			name: "retry trigger ci_fix names its reaction",
			body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Trigger: "ci_fix", ReactionKind: "ci", ReactionConfigKey: "ci_failure"}),
			want: map[string]any{"trigger": "ci_fix", "reaction_kind": "ci", "reaction_config_key": "ci_failure"},
		},
		{name: "retry paused", body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{Trigger: "timer", Paused: true}), want: map[string]any{"paused": true}},
		{
			name:   "review continuation has no trigger",
			body:   toRetryEntryResponse(orchestrator.SnapshotRetryEntry{ReactionKind: "review", ReactionConfigKey: "review_comments"}),
			want:   map[string]any{"reaction_kind": "review", "reaction_config_key": "review_comments"},
			absent: []string{"trigger"},
		},
		{
			name: "retry stage lineage with a previous rule",
			body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{StageLineage: orchestrator.StageLineage{ChainID: "chain-1", PreviousRule: "implement", PreviousOutcome: "succeeded"}}),
			want: map[string]any{"stage_lineage": map[string]any{"chain_id": "chain-1", "previous": "implement", "previous_outcome": "succeeded"}},
		},
		{
			name: "retry stage lineage with only a chain is null",
			body: toRetryEntryResponse(orchestrator.SnapshotRetryEntry{StageLineage: orchestrator.StageLineage{ChainID: "chain-1"}}),
			want: map[string]any{"stage_lineage": nil},
		},
		{
			name: "budget exhausted",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{
				IssueID:         "id-3",
				Identifier:      "PROJ-3",
				DisplayID:       "acme/web#3",
				IssueTitle:      "Held by budget",
				IssueURL:        "https://acme.atlassian.net/browse/PROJ-3",
				IssueType:       "Story",
				IssueLabels:     []string{"billing"},
				State:           "Backlog",
				Reason:          "session_budget",
				UsedSessions:    3,
				BudgetSessions:  3,
				RuleName:        "implement",
				StageLineage:    lineage,
				StagePosition:   position,
				AgentKind:       "claude-code",
				ConfiguredModel: "opus",
			}),
			want: map[string]any{
				"issue_id":           "id-3",
				"issue_identifier":   "PROJ-3",
				"display_identifier": "acme/web#3",
				"issue_title":        "Held by budget",
				"issue_url":          "https://acme.atlassian.net/browse/PROJ-3",
				"issue_type":         "Story",
				"issue_labels":       []any{"billing"},
				"state":              "Backlog",
				"reason":             "session_budget",
				"used_sessions":      float64(3),
				"budget_sessions":    float64(3),
				"budget_tokens":      nil,
				"dispatched_at":      "2026-03-24T09:00:00Z",
				"rule_name":          "implement",
				"stage_lineage":      wantLineage,
				"stage_position":     wantPosition,
				"agent_kind":         "claude-code",
				"configured_model":   "opus",
			},
		},
		{
			name:   "budget exhausted without a dispatch context serves null and omits the rest",
			body:   toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{}),
			want:   map[string]any{"dispatched_at": nil, "stage_lineage": nil, "stage_position": nil},
			absent: []string{"rule_name", "agent_kind", "configured_model"},
		},
		{
			name: "between stages",
			body: toStageHopResponse(orchestrator.SnapshotStageHop{
				IssueID:      "id-5",
				Identifier:   "PROJ-5",
				DisplayID:    "acme/web#5",
				IssueTitle:   "Hop me",
				IssueURL:     "https://acme.atlassian.net/browse/PROJ-5",
				SourceRule:   "specify",
				TargetRule:   "implement",
				TargetLabel:  "stage:implement",
				HoppedAt:     time.Date(2026, 3, 24, 5, 0, 0, 0, time.FixedZone("EST", -5*60*60)),
				DispatchedAt: passAt,

				IssueType: "Story", IssueLabels: []string{"backend", "stage:implement"}, State: "In Progress",
				RuleName: "implement", StagePosition: position, AgentKind: "claude-code", ConfiguredModel: "opus",
			}, sentence.Context{}),
			want: map[string]any{
				"issue_id":           "id-5",
				"issue_identifier":   "PROJ-5",
				"display_identifier": "acme/web#5",
				"issue_title":        "Hop me",
				"issue_url":          "https://acme.atlassian.net/browse/PROJ-5",
				"source_rule":        "specify",
				"target_rule":        "implement",
				"target_label":       "stage:implement",
				"hopped_at":          "2026-03-24T10:00:00Z",
				"dispatched_at":      "2026-03-24T09:00:00Z",
				"issue_type":         "Story",
				"issue_labels":       []any{"backend", "stage:implement"},
				"state":              "In Progress",
				"rule_name":          "implement",
				"stage_position":     wantPosition,
				"agent_kind":         "claude-code",
				"configured_model":   "opus",
			},
		},
		{
			name:   "between stages without times or identity serves null times",
			body:   toStageHopResponse(orchestrator.SnapshotStageHop{IssueID: "id-6", Identifier: "PROJ-6"}, sentence.Context{}),
			want:   map[string]any{"hopped_at": nil, "dispatched_at": nil, "hold": nil},
			absent: []string{"display_identifier", "issue_title", "issue_url"},
		},
		{
			name: "hold by a blocker serves its blockers and no slot or reset",
			body: toHoldResponse(&orchestrator.SnapshotHold{
				Reason: orchestrator.SkipBlockedBy, Since: startedAt,
				Blockers: []orchestrator.SnapshotBlocker{{IssueID: "b-1", Identifier: "PROJ-9", DisplayID: "acme/web#9"}},
			}, sentence.Context{}),
			want: map[string]any{
				"reason":     "blocked_by",
				"held_since": "2026-03-24T12:00:00Z",
				"blockers":   []any{map[string]any{"issue_id": "b-1", "issue_identifier": "PROJ-9", "display_identifier": "acme/web#9"}},
				"slot":       nil,
				"resets_at":  nil,
			},
		},
		{
			name: "hold by a state slot limit serves its slot and no blockers or reset",
			body: toHoldResponse(&orchestrator.SnapshotHold{
				Reason: orchestrator.SkipAgentSlotsFull, Since: startedAt,
				Slot: &orchestrator.SnapshotSlotHold{Scope: orchestrator.SlotScopeState, State: "In Progress", Used: 2, Limit: 2},
			}, sentence.Context{}),
			want: map[string]any{
				"slot":      map[string]any{"scope": "state", "state": "In Progress", "used": 2, "limit": 2},
				"blockers":  nil,
				"resets_at": nil,
			},
		},
		{
			name: "hold by the global slot limit serves a slot without a state",
			body: toHoldResponse(&orchestrator.SnapshotHold{
				Reason: orchestrator.SkipAgentSlotsFull, Since: startedAt,
				Slot: &orchestrator.SnapshotSlotHold{Scope: orchestrator.SlotScopeGlobal, Used: 1, Limit: 1},
			}, sentence.Context{}),
			want: map[string]any{"slot": map[string]any{"scope": "global", "used": 1, "limit": 1}},
		},
		{
			name: "hold by a spend ceiling serves its reset in UTC and no blockers or slot",
			body: toHoldResponse(&orchestrator.SnapshotHold{
				Reason: orchestrator.SkipDailySpendCeiling, Since: passAt, ResetsAt: passAt,
			}, sentence.Context{}),
			want: map[string]any{"held_since": "2026-03-24T09:00:00Z", "resets_at": "2026-03-24T09:00:00Z", "blockers": nil, "slot": nil},
		},
		{
			name: "hold by a usage limit serves its reset and the model scope it covers",
			body: toHoldResponse(&orchestrator.SnapshotHold{
				Reason: orchestrator.SkipUsageLimit, Since: passAt, ResetsAt: passAt,
				AgentKind: "claude-code", ModelOnly: true, Model: "opus",
			}, sentence.Context{}),
			want: map[string]any{"reason": "usage_limit", "resets_at": "2026-03-24T09:00:00Z", "agent_kind": "claude-code", "model_only": true, "model": "opus"},
		},
		{
			name:   "hold by a kind-wide usage limit omits the model scope",
			body:   toHoldResponse(&orchestrator.SnapshotHold{Reason: orchestrator.SkipUsageLimit, Since: passAt, ResetsAt: passAt, AgentKind: "codex"}, sentence.Context{}),
			want:   map[string]any{"agent_kind": "codex"},
			absent: []string{"model_only", "model"},
		},
		{
			name: "held issue without labels serves an empty list",
			body: toHeldIssueResponse(orchestrator.SnapshotHeldEntry{
				IssueID: "id-8", Identifier: "PROJ-8", IssueLabels: []string{},
				Hold: orchestrator.SnapshotHold{Reason: orchestrator.SkipSSHHostsFull, Since: startedAt},
			}, sentence.Context{}),
			want: map[string]any{"issue_labels": []any{}},
		},
		{
			name: "parked record",
			body: toParkedEntryResponse(orchestrator.SnapshotParkedEntry{
				IssueID:         "id-7",
				IssueType:       "Bug",
				IssueLabels:     []string{"backend"},
				DispatchedAt:    passAt,
				Reason:          "agent_blocked",
				ParkedState:     "In Progress",
				Label:           "needs-human",
				ParkedAt:        startedAt,
				RuleName:        "implement",
				StagePosition:   position,
				AgentKind:       "claude-code",
				ConfiguredModel: "opus",
			}),
			want: map[string]any{
				"issue_id":         "id-7",
				"reason":           "agent_blocked",
				"parked_state":     "In Progress",
				"label":            "needs-human",
				"parked_at":        "2026-03-24T12:00:00Z",
				"issue_type":       "Bug",
				"issue_labels":     []any{"backend"},
				"state":            "In Progress",
				"dispatched_at":    "2026-03-24T09:00:00Z",
				"rule_name":        "implement",
				"stage_position":   wantPosition,
				"agent_kind":       "claude-code",
				"configured_model": "opus",
			},
		},
		{
			name:   "parked record without a run or a read serves null and omits the rest",
			body:   toParkedEntryResponse(orchestrator.SnapshotParkedEntry{IssueID: "id-8", Identifier: "PROJ-8"}),
			want:   map[string]any{"dispatched_at": nil, "stage_position": nil},
			absent: []string{"issue_type", "issue_labels", "state", "rule_name", "agent_kind", "configured_model"},
		},
		{
			name: "pending reaction",
			body: toPendingReactionResponse(orchestrator.SnapshotPendingReaction{
				IssueID:         "id-9",
				Kind:            "ci",
				ConfigKey:       "ci_failure",
				IssueType:       "Task",
				IssueLabels:     []string{"backend"},
				State:           "In Review",
				RuleName:        "implement",
				StagePosition:   position,
				AgentKind:       "claude-code",
				ConfiguredModel: "opus",
			}),
			want: map[string]any{
				"issue_id":         "id-9",
				"kind":             "ci",
				"config_key":       "ci_failure",
				"issue_type":       "Task",
				"issue_labels":     []any{"backend"},
				"state":            "In Review",
				"rule_name":        "implement",
				"stage_position":   wantPosition,
				"agent_kind":       "claude-code",
				"configured_model": "opus",
			},
		},
		{
			name: "budget exhausted with sessions stopped in flight",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{StoppedInFlight: new(1)}),
			want: map[string]any{"stopped_in_flight": 1},
		},
		{
			name: "budget held by tokens serves a null session ceiling",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{BudgetTokens: 500000}),
			want: map[string]any{"budget_sessions": nil, "budget_tokens": 500000},
		},
		{
			name: "session-held token read failed serves null spend and counts",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{BudgetTokens: 500000}),
			want: map[string]any{"used_tokens": nil, "unmeasured_sessions": nil, "stopped_in_flight": nil},
		},
		{
			name: "session-held no usage reported serves null spend",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{
				BudgetTokens: 500000, UsedTokens: new(int64(0)), UnmeasuredSessions: new(3), StoppedInFlight: new(0),
			}),
			want: map[string]any{"used_tokens": nil, "unmeasured_sessions": 3, "stopped_in_flight": 0},
		},
		{
			name: "session-held unaccounted turn serves null spend",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{
				BudgetTokens: 500000, UsedTokens: new(int64(0)), UnmeasuredSessions: new(0), StoppedInFlight: new(0), UnaccountedTurns: 1,
			}),
			want: map[string]any{"used_tokens": nil, "unmeasured_sessions": 0, "stopped_in_flight": 0},
		},
		{
			name: "session-held measured zero serves zero spend",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{
				BudgetTokens: 500000, UsedTokens: new(int64(0)), UnmeasuredSessions: new(0), StoppedInFlight: new(0),
			}),
			want: map[string]any{"used_tokens": 0, "unmeasured_sessions": 0, "stopped_in_flight": 0},
		},
		{
			name: "session-held partial sum serves the lower bound",
			body: toBudgetExhaustedResponse(orchestrator.SnapshotBudgetEntry{
				BudgetTokens: 500000, UsedTokens: new(int64(1200)), UnmeasuredSessions: new(1), StoppedInFlight: new(0),
			}),
			want: map[string]any{"used_tokens": 1200, "unmeasured_sessions": 1, "stopped_in_flight": 0},
		},
		{
			name: "pending reaction merge-conflict with mergeability not computed",
			body: toPendingReactionResponse(orchestrator.SnapshotPendingReaction{
				Kind: "merge-conflict",
				LastCheck: &orchestrator.ReactionCheck{
					ObservedAt:   startedAt,
					HeadSHA:      "9b7e3d1c",
					Mergeability: &orchestrator.MergeabilityCheck{},
				},
			}),
			want: map[string]any{"last_check": map[string]any{"observed_at": "2026-03-24T12:00:00Z", "head_sha": "9b7e3d1c", "mergeable": nil}},
		},
		{
			name: "pending reaction label-review counts no tries and records no check",
			body: toPendingReactionResponse(orchestrator.SnapshotPendingReaction{
				Kind:        "label-review",
				ConfigKey:   "label_review",
				PullRequest: orchestrator.SnapshotPullRequest{Number: 7, Repository: "acme/web"},
			}),
			want: map[string]any{
				"used_attempts": nil,
				"max_attempts":  nil,
				"escalated_at":  nil,
				"last_check":    nil,
				"pull_request":  map[string]any{"number": 7, "repository": "acme/web"},
			},
			absent: []string{"wait", "display_identifier", "issue_title", "issue_url"},
		},
		{
			name: "pending reaction passing CI serves zero counts and an empty failing_checks",
			body: toPendingReactionResponse(orchestrator.SnapshotPendingReaction{
				Kind: "ci",
				LastCheck: &orchestrator.ReactionCheck{
					ObservedAt: startedAt,
					CI:         &orchestrator.CICheck{Status: "passing", Passing: 5},
				},
			}),
			want: map[string]any{"last_check": map[string]any{
				"observed_at": "2026-03-24T12:00:00Z", "status": "passing", "failing_checks": []any{}, "failing": 0, "pending": 0, "passing": 5,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assertJSONKeys(t, tt.body, tt.want)
			got := jsonObject(t, tt.body)
			for _, key := range tt.absent {
				if _, present := got[key]; present {
					t.Errorf("JSON %q present, want absent", key)
				}
			}
		})
	}
}

func TestToEntryResponses_DisplayID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		id             string
		dispID         string
		wantIdentifier string
		wantDisplayID  string
	}{
		{
			name:           "DisplayID set — raw Identifier preserved, qualified form in DisplayIdentifier",
			id:             "42",
			dispID:         "owner/repo#42",
			wantIdentifier: "42",
			wantDisplayID:  "owner/repo#42",
		},
		{
			name:           "DisplayID empty — Identifier unchanged, DisplayIdentifier empty",
			id:             "PROJ-42",
			dispID:         "",
			wantIdentifier: "PROJ-42",
			wantDisplayID:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			running := toRunningEntryResponse(orchestrator.SnapshotRunningEntry{IssueID: "issue-x", Identifier: tt.id, DisplayID: tt.dispID})
			retry := toRetryEntryResponse(orchestrator.SnapshotRetryEntry{IssueID: "issue-y", Identifier: tt.id, DisplayID: tt.dispID})

			if running.IssueIdentifier != tt.wantIdentifier || retry.IssueIdentifier != tt.wantIdentifier {
				t.Errorf("IssueIdentifier running, retry = %q, %q, want %q", running.IssueIdentifier, retry.IssueIdentifier, tt.wantIdentifier)
			}
			if running.DisplayIdentifier != tt.wantDisplayID || retry.DisplayIdentifier != tt.wantDisplayID {
				t.Errorf("DisplayIdentifier running, retry = %q, %q, want %q", running.DisplayIdentifier, retry.DisplayIdentifier, tt.wantDisplayID)
			}
		})
	}
}

func TestToStateResponse(t *testing.T) {
	t.Parallel()

	t.Run("empty snapshot produces non-nil slices", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
			Running:     []orchestrator.SnapshotRunningEntry{},
			Retrying:    []orchestrator.SnapshotRetryEntry{},
			AgentTotals: orchestrator.SnapshotAgentTotals{},
		}

		got := toStateResponse(snap, sentence.Context{}, nil)

		if got.Running == nil {
			t.Fatal("Running = nil, want non-nil empty slice")
		}
		if len(got.Running) != 0 {
			t.Errorf("len(Running) = %d, want 0", len(got.Running))
		}
		if got.Retrying == nil {
			t.Fatal("Retrying = nil, want non-nil empty slice")
		}
		if got.PendingReactions == nil {
			t.Fatal("PendingReactions = nil, want non-nil empty slice")
		}
		if got.BetweenStages == nil {
			t.Fatal("BetweenStages = nil, want non-nil empty slice")
		}
		if len(got.Retrying) != 0 {
			t.Errorf("len(Retrying) = %d, want 0", len(got.Retrying))
		}
		if got.Counts.Running != 0 {
			t.Errorf("Counts.Running = %d, want 0", got.Counts.Running)
		}
		if got.Counts.Retrying != 0 {
			t.Errorf("Counts.Retrying = %d, want 0", got.Counts.Retrying)
		}
		if got.RateLimits == nil {
			t.Fatal("RateLimits = nil, want non-nil empty map")
		}
		if len(got.RateLimits) != 0 {
			t.Errorf("len(RateLimits) = %d, want 0", len(got.RateLimits))
		}
	})

	t.Run("counts match entries", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "a", Identifier: "MT-1"},
				{IssueID: "b", Identifier: "MT-2"},
			},
			Retrying: []orchestrator.SnapshotRetryEntry{
				{IssueID: "c", Identifier: "MT-3"},
			},
		}

		got := toStateResponse(snap, sentence.Context{}, nil)

		if got.Counts.Running != 2 {
			t.Errorf("Counts.Running = %d, want 2", got.Counts.Running)
		}
		if got.Counts.Retrying != 1 {
			t.Errorf("Counts.Retrying = %d, want 1", got.Counts.Retrying)
		}
	})

	t.Run("held rows are counted and leave active and attention as they were", func(t *testing.T) {
		t.Parallel()

		now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
		base := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: now,
			Running:     []orchestrator.SnapshotRunningEntry{{IssueID: "id-1", Identifier: "MT-1", StartedAt: now}},
			Parked:      []orchestrator.SnapshotParkedEntry{{IssueID: "id-2", Identifier: "MT-2", Reason: "agent_blocked", Label: "needs-human"}},
		}
		withHeld := base
		withHeld.Held = []orchestrator.SnapshotHeldEntry{
			{IssueID: "id-3", Identifier: "MT-3", Hold: orchestrator.SnapshotHold{Reason: orchestrator.SkipSSHHostsFull, Since: now}},
			{IssueID: "id-4", Identifier: "MT-4", Hold: orchestrator.SnapshotHold{Reason: orchestrator.SkipSSHHostsFull, Since: now}},
		}

		want := toStateResponse(base, sentence.Context{Now: now}, nil)
		got := toStateResponse(withHeld, sentence.Context{Now: now}, nil)

		if want.Held == nil || len(want.Held) != 0 || want.Counts.Held != 0 {
			t.Errorf("toStateResponse(no held).Held, Counts.Held = %v, %d, want an empty non-nil list and 0", want.Held, want.Counts.Held)
		}
		if got.Counts.Held != len(got.Held) || len(got.Held) != 2 {
			t.Errorf("toStateResponse(2 held).Counts.Held, len(Held) = %d, %d, want 2, 2", got.Counts.Held, len(got.Held))
		}
		if !reflect.DeepEqual(got.Active, want.Active) || !reflect.DeepEqual(got.Attention, want.Attention) || got.Counts.Attention != want.Counts.Attention {
			t.Errorf("toStateResponse(held).Active, Attention, Counts.Attention = %+v, %+v, %d, want %+v, %+v, %d",
				got.Active, got.Attention, got.Counts.Attention, want.Active, want.Attention, want.Counts.Attention)
		}
	})

	t.Run("parked entry serves every key and counts", func(t *testing.T) {
		t.Parallel()

		est := time.FixedZone("EST", -5*60*60)
		snap := orchestrator.RuntimeSnapshotResult{
			Parked: []orchestrator.SnapshotParkedEntry{{
				IssueID: "id-p", Identifier: "MT-9", DisplayID: "acme/web#9",
				IssueTitle: "Needs a human", IssueURL: "https://acme.atlassian.net/browse/MT-9",
				Reason: "handoff_absence", ParkedState: "In Review", Label: "needs-human", LabelApplied: true,
				ParkedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, est),
			}},
		}

		got := toStateResponse(snap, sentence.Context{}, nil)

		if got.Counts.Parked != 1 || len(got.Parked) != 1 {
			t.Fatalf("Counts.Parked, len(Parked) = %d, %d, want 1, 1", got.Counts.Parked, len(got.Parked))
		}
		assertJSONKeys(t, got.Parked[0], map[string]any{
			"issue_id":           "id-p",
			"issue_identifier":   "MT-9",
			"display_identifier": "acme/web#9",
			"issue_title":        "Needs a human",
			"issue_url":          "https://acme.atlassian.net/browse/MT-9",
			"reason":             "handoff_absence",
			"parked_state":       "In Review",
			"label":              "needs-human",
			"label_applied":      true,
			"parked_at":          "2026-03-24T17:00:00Z",
		})
	})

	t.Run("pending reactions serve every key and count", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{PendingReactions: pendingReactionRows()}

		got := toStateResponse(snap, sentence.Context{Now: time.Date(2026, 2, 24, 20, 15, 35, 0, time.UTC)}, nil)

		assertJSONKeys(t, got, map[string]any{
			"counts": map[string]any{
				"running": 0, "retrying": 0, "budget_exhausted": 0, "parked": 0, "pending_reactions": 2, "between_stages": 0, "held": 0, "attention": 1,
			},
			"pending_reactions": json.RawMessage(wantPendingReactionsJSON),
		})
	})

	t.Run("between stages serve the list and count", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{BetweenStages: []orchestrator.SnapshotStageHop{
			{IssueID: "id-1", Identifier: "MT-1", SourceRule: "specify", TargetRule: "implement", TargetLabel: "stage:implement"},
		}}

		got := toStateResponse(snap, sentence.Context{}, nil)

		assertJSONKeys(t, got, map[string]any{
			"counts": map[string]any{
				"running": 0, "retrying": 0, "budget_exhausted": 0, "parked": 0, "pending_reactions": 0, "between_stages": 1, "held": 0, "attention": 0,
			},
			"between_stages": json.RawMessage(`[{"issue_id": "id-1", "issue_identifier": "MT-1", "source_rule": "specify",
				"target_rule": "implement", "target_label": "stage:implement", "hopped_at": null, "dispatched_at": null, "stage_position": null, "hold": null,
				"status_text": "Between stages: specify to implement",
				"status_text_full": "Moved from stage specify to stage implement, and Sortie added \u0060stage:implement\u0060; implement starts when a poll tick reads that label on the issue."}]`),
		})
	})

	for _, window := range orchestrator.SpendWindows() {
		t.Run("spend window "+string(window)+" serves under its own member", func(t *testing.T) {
			t.Parallel()

			snap := orchestrator.RuntimeSnapshotResult{SpendCeilings: []orchestrator.SpendCeilingStatus{{Window: window, BudgetUSD: 7}}}

			got := jsonObject(t, toStateResponse(snap, sentence.Context{}, nil))

			members, _ := got["spend_ceiling"].(map[string]any)
			for _, member := range orchestrator.SpendWindows() {
				if served := members[string(member)] != nil; served != (member == window) {
					t.Errorf("toStateResponse(%s only).spend_ceiling[%q] = %v, want an object only under %q", window, member, members[string(member)], window)
				}
			}
		})
	}

	t.Run("tokens_measured reflects each running entry's own flag", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "measured", Identifier: "MT-MEAS", UsageMeasured: true},
				{IssueID: "unmeasured", Identifier: "MT-UNM", UsageMeasured: false},
			},
		}

		got := toStateResponse(snap, sentence.Context{}, nil)

		if len(got.Running) != 2 {
			t.Fatalf("len(Running) = %d, want 2", len(got.Running))
		}
		byID := make(map[string]bool, len(got.Running))
		for _, e := range got.Running {
			byID[e.IssueID] = e.TokensMeasured
		}
		if !byID["measured"] {
			t.Error(`Running[issue_id="measured"].TokensMeasured = false, want true`)
		}
		if byID["unmeasured"] {
			t.Error(`Running[issue_id="unmeasured"].TokensMeasured = true, want false`)
		}

		data, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if !strings.Contains(string(data), `"tokens_measured":true`) || !strings.Contains(string(data), `"tokens_measured":false`) {
			t.Errorf("JSON = %s, want both tokens_measured:true and tokens_measured:false", data)
		}
	})
}

func TestToStateResponse_AgentTotalsExclusionFieldsJSON(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Now().UTC(),
		AgentTotals: orchestrator.SnapshotAgentTotals{
			UnmeasuredSessions:  3,
			RunningUnreported:   1,
			RunningNonReporting: 2,
		},
	}

	got := toStateResponse(snap, sentence.Context{}, nil)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded struct {
		AgentTotals struct {
			UnmeasuredSessions  int64 `json:"unmeasured_sessions"`
			RunningUnreported   int   `json:"running_unreported"`
			RunningNonReporting int   `json:"running_non_reporting"`
		} `json:"agent_totals"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.AgentTotals.UnmeasuredSessions != 3 {
		t.Errorf("agent_totals.unmeasured_sessions = %d, want 3", decoded.AgentTotals.UnmeasuredSessions)
	}
	if decoded.AgentTotals.RunningUnreported != 1 {
		t.Errorf("agent_totals.running_unreported = %d, want 1", decoded.AgentTotals.RunningUnreported)
	}
	if decoded.AgentTotals.RunningNonReporting != 2 {
		t.Errorf("agent_totals.running_non_reporting = %d, want 2", decoded.AgentTotals.RunningNonReporting)
	}
}

func loadTestRates(t *testing.T, raw map[string]any, kinds ...string) *pricing.Rates {
	t.Helper()

	cfg, err := config.NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("config.NewServiceConfig: %v", err)
	}
	rates, _, err := pricing.LoadRates(cfg, pricing.LoadInput{IsKind: func(key string) bool { return slices.Contains(kinds, key) }})
	if err != nil {
		t.Fatalf("pricing.LoadRates: %v", err)
	}
	return rates
}

func TestToStateResponse_CostUnpricedRunning(t *testing.T) {
	t.Parallel()

	fptr := func(v float64) *float64 { return &v }

	tests := []struct {
		name       string
		raw        map[string]any
		wantServed bool
	}{
		{name: "zero served with defaults", wantServed: true},
		{name: "omitted when no rate source is available", raw: map[string]any{"price_table": map[string]any{"embedded": false}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snap := orchestrator.RuntimeSnapshotResult{
				GeneratedAt: time.Now().UTC(),
				Rates:       loadTestRates(t, tt.raw),
				Running: []orchestrator.SnapshotRunningEntry{
					{IssueID: "b", Identifier: "MT-B", EstimatedCostUSD: fptr(3.0), CostStatus: "estimated"},
				},
			}

			got := jsonObject(t, toStateResponse(snap, sentence.Context{}, nil))

			unpriced, served := got["cost_unpriced_running"]
			if served != tt.wantServed || served && unpriced != float64(0) {
				t.Errorf("cost_unpriced_running = %v (present=%t), want 0 (present=%t)", unpriced, served, tt.wantServed)
			}
			if _, served := got["active_estimated_cost_usd"]; served != tt.wantServed {
				t.Errorf("active_estimated_cost_usd present = %t, want %t", served, tt.wantServed)
			}
		})
	}
}

func TestToServedRunningEntry(t *testing.T) {
	t.Parallel()

	type entry = orchestrator.SnapshotRunningEntry
	workedExample := func() entry {
		return entry{
			AgentKind: "claude-code", UsageMeasured: true,
			AgentInputTokens: 60000, CacheReadTokens: 40000, AgentOutputTokens: 5000, AgentTotalTokens: 65000,
			EstimatedCostUSD: new(12.0), CostStatus: "estimated",
			IssueUsage: &orchestrator.SnapshotIssueUsage{Sessions: 3, TotalTokens: 420000, UncachedTokens: 120000, RecordedCostUSD: 15, PricedSessions: 2},
		}
	}
	noFigure := func(usage orchestrator.SnapshotIssueUsage) func(*entry) {
		return func(e *entry) {
			e.UsageMeasured, e.IssueUsage, e.EstimatedCostUSD, e.CostStatus = false, &usage, nil, "unmeasured"
		}
	}
	unknownSpend := func(completed any) map[string]any {
		return map[string]any{
			"issue_tokens_completed": completed, "used_tokens": nil, "used_tokens_complete": false,
			"cost_status": "unmeasured", "estimated_cost_usd": nil, "issue_cost_status": "unmeasured", "issue_estimated_cost_usd": nil,
		}
	}
	partialWorked := map[string]any{"used_tokens": 145000, "used_tokens_complete": false, "issue_cost_status": "partial", "issue_estimated_cost_usd": 27}

	tests := []struct {
		name                   string
		maxTokens, maxSessions int
		edit                   func(*entry)
		want                   map[string]any
	}{
		{
			name: "worked example", maxTokens: 1_000_000, maxSessions: 5,
			want: map[string]any{
				"used_sessions": 3, "budget_sessions": 5, "issue_tokens_completed": 120000, "unmeasured_sessions": 0,
				"used_tokens": 145000, "used_tokens_complete": true, "budget_tokens": 1_000_000,
				"estimated_cost_usd": 12, "cost_status": "estimated", "issue_estimated_cost_usd": 27, "issue_cost_status": "estimated",
			},
		},
		{
			name: "larger live total with no ceilings", edit: func(e *entry) { e.AgentTotalTokens = 75000 },
			want: map[string]any{"used_tokens": 155000, "issue_tokens_completed": 120000, "budget_tokens": nil, "budget_sessions": nil},
		},
		{
			name: "session budget served, token ceiling null", maxSessions: 3,
			want: map[string]any{"budget_tokens": nil, "budget_sessions": 3},
		},
		{
			name: "unmeasured completed session and no figure",
			edit: noFigure(orchestrator.SnapshotIssueUsage{Sessions: 2, UnmeasuredSessions: 1}), want: unknownSpend(nil),
		},
		{
			name: "completed run that never entered a turn and no figure",
			edit: noFigure(orchestrator.SnapshotIssueUsage{Sessions: 1}), want: unknownSpend(0),
		},
		{
			name: "unmeasured completed session beside a measured one",
			edit: func(e *entry) { e.IssueUsage = &orchestrator.SnapshotIssueUsage{Sessions: 1, UnmeasuredSessions: 1} },
			want: map[string]any{"issue_tokens_completed": nil, "used_tokens": 25000, "used_tokens_complete": false, "issue_cost_status": "partial", "issue_estimated_cost_usd": 12},
		},
		{
			name: "unaccounted turn of the running session", edit: func(e *entry) { e.UnaccountedTurns = 1 },
			want: partialWorked,
		},
		{
			name: "unaccounted turn of a completed session", edit: func(e *entry) { e.IssueUsage.UnaccountedTurns = 1 },
			want: partialWorked,
		},
		{
			name: "no usage read and a priced session", edit: func(e *entry) { e.IssueUsage = nil },
			want: map[string]any{"used_sessions": nil, "unmeasured_sessions": nil, "issue_tokens_completed": nil, "used_tokens": 25000, "used_tokens_complete": false, "issue_cost_status": "partial", "issue_estimated_cost_usd": 12},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := workedExample()
			if tt.edit != nil {
				tt.edit(&e)
			}
			snap := orchestrator.RuntimeSnapshotResult{MaxTokens: tt.maxTokens, MaxSessions: tt.maxSessions}

			assertJSONKeys(t, toServedRunningEntry(e, snap), tt.want)
		})
	}
}

func TestBuildIssueDetail(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	snap := orchestrator.RuntimeSnapshotResult{
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:       "id-1",
				Identifier:    "MT-100",
				State:         "In Progress",
				StartedAt:     now,
				WorkspacePath: "/tmp/ws/mt-100",
				UsageMeasured: true,
			},
		},
		Retrying: []orchestrator.SnapshotRetryEntry{
			{
				IssueID:    "id-2",
				Identifier: "MT-200",
				Attempt:    2,
				DueAtMS:    now.UnixMilli(),
				Error:      "boom",
			},
		},
	}

	t.Run("running issue", func(t *testing.T) {
		t.Parallel()
		got := buildIssueDetail("MT-100", "", snap, sentence.Context{})
		if got == nil {
			t.Fatal("buildIssueDetail(MT-100) = nil, want non-nil")
			return
		}
		if got.IssueID != "id-1" {
			t.Errorf("IssueID = %q, want %q", got.IssueID, "id-1")
		}
		if got.Running == nil {
			t.Fatal("Running = nil, want non-nil")
		}
		if got.Retry != nil {
			t.Errorf("Retry = %v, want nil for running issue", got.Retry)
		}
		if got.Workspace == nil {
			t.Fatal("Workspace = nil, want non-nil for running issue with path")
		}
		if got.Workspace.Path != "/tmp/ws/mt-100" {
			t.Errorf("Workspace.Path = %q, want %q", got.Workspace.Path, "/tmp/ws/mt-100")
		}
		if !got.Running.TokensMeasured {
			t.Error("Running.TokensMeasured = false, want true")
		}
		if got.Attempts == nil {
			t.Fatal("Attempts = nil, want non-nil")
		}
		if got.RecentEvents == nil {
			t.Fatal("RecentEvents = nil, want non-nil empty slice")
		}
		if got.Tracked == nil {
			t.Fatal("Tracked = nil, want non-nil empty map")
		}
	})

	t.Run("retrying issue", func(t *testing.T) {
		t.Parallel()
		got := buildIssueDetail("MT-200", "", snap, sentence.Context{})
		if got == nil {
			t.Fatal("buildIssueDetail(MT-200) = nil, want non-nil")
			return
		}
		if got.Running != nil {
			t.Errorf("Running = %v, want nil for retrying issue", got.Running)
		}
		if got.Retry == nil {
			t.Fatal("Retry = nil, want non-nil")
		}
		if got.Retry.Attempt != 2 {
			t.Errorf("Retry.Attempt = %d, want %d", got.Retry.Attempt, 2)
		}
		if got.LastError == nil {
			t.Fatal("LastError = nil, want non-nil for retrying issue with error")
		}
		if *got.LastError != "boom" {
			t.Errorf("LastError = %q, want %q", *got.LastError, "boom")
		}
		if got.Workspace != nil {
			t.Errorf("Workspace = %v, want nil for retrying issue", got.Workspace)
		}
		if got.Attempts.RestartCount != 1 {
			t.Errorf("Attempts.RestartCount = %d, want %d", got.Attempts.RestartCount, 1)
		}
	})

	t.Run("issue object carries the deciding row's values", func(t *testing.T) {
		t.Parallel()
		issueSnap := orchestrator.RuntimeSnapshotResult{
			Running:          []orchestrator.SnapshotRunningEntry{{Identifier: "MT-1", IssueTitle: "Run", IssueURL: "https://t.example/MT-1", IssueType: "Bug", IssueLabels: []string{"a"}, State: "In Progress"}},
			Retrying:         []orchestrator.SnapshotRetryEntry{{Identifier: "MT-2", IssueTitle: "Retry", IssueURL: "https://t.example/MT-2", IssueType: "Task", IssueLabels: []string{"b"}, State: "To Do"}},
			BudgetExhausted:  []orchestrator.SnapshotBudgetEntry{{Identifier: "MT-3", IssueTitle: "Held", IssueURL: "https://t.example/MT-3", IssueType: "Story", IssueLabels: []string{"c"}, State: "Backlog"}},
			Parked:           []orchestrator.SnapshotParkedEntry{{Identifier: "MT-5", IssueTitle: "Parked", IssueURL: "https://t.example/MT-5", IssueType: "Epic", IssueLabels: []string{"d"}, ParkedState: "Blocked"}},
			PendingReactions: []orchestrator.SnapshotPendingReaction{{Identifier: "MT-6", IssueTitle: "Waiting", IssueURL: "https://t.example/MT-6", IssueType: "Chore", IssueLabels: []string{"e"}, State: "In Review"}},
			BetweenStages: []orchestrator.SnapshotStageHop{
				{IssueID: "id-3", Identifier: "MT-3", IssueTitle: "Hopped", IssueURL: "https://t.example/hop-3"},
				{IssueID: "id-4", Identifier: "MT-4", IssueTitle: "Between", IssueURL: "https://t.example/MT-4", IssueType: "Spike", IssueLabels: []string{"f"}, State: "Triage", SourceRule: "specify", TargetRule: "implement"},
			},
		}
		tests := []struct {
			identifier string
			wantStatus string
			want       issueResponse
		}{
			{"MT-1", "running", issueResponse{Title: "Run", URL: "https://t.example/MT-1", IssueType: "Bug", Labels: []string{"a"}, State: "In Progress"}},
			{"MT-2", "retrying", issueResponse{Title: "Retry", URL: "https://t.example/MT-2", IssueType: "Task", Labels: []string{"b"}, State: "To Do"}},
			{"MT-3", "budget_exhausted", issueResponse{Title: "Held", URL: "https://t.example/MT-3", IssueType: "Story", Labels: []string{"c"}, State: "Backlog"}},
			{"MT-4", "between_stages", issueResponse{Title: "Between", URL: "https://t.example/MT-4", IssueType: "Spike", Labels: []string{"f"}, State: "Triage"}},
			{"MT-5", "parked", issueResponse{Title: "Parked", URL: "https://t.example/MT-5", IssueType: "Epic", Labels: []string{"d"}, State: "Blocked"}},
			{"MT-6", "pending_reactions", issueResponse{Title: "Waiting", URL: "https://t.example/MT-6", IssueType: "Chore", Labels: []string{"e"}, State: "In Review"}},
		}

		for _, tt := range tests {
			got := buildIssueDetail(tt.identifier, "", issueSnap, sentence.Context{})
			if got == nil {
				t.Fatalf("buildIssueDetail(%q) = nil, want non-nil", tt.identifier)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("buildIssueDetail(%q).Status = %q, want %q", tt.identifier, got.Status, tt.wantStatus)
			}
			if !reflect.DeepEqual(got.Issue, tt.want) {
				t.Errorf("buildIssueDetail(%q).Issue = %+v, want %+v", tt.identifier, got.Issue, tt.want)
			}
		}
	})

	t.Run("issue only between stages answers as one only parked does", func(t *testing.T) {
		t.Parallel()
		hop := orchestrator.SnapshotStageHop{IssueID: "id-4", Identifier: "MT-4", SourceRule: "specify", TargetRule: "implement", TargetLabel: "stage:implement"}

		got := buildIssueDetail("MT-4", "", orchestrator.RuntimeSnapshotResult{BetweenStages: []orchestrator.SnapshotStageHop{hop}}, sentence.Context{})

		if got == nil {
			t.Fatal("buildIssueDetail(MT-4) = nil, want non-nil")
		}
		if got.BetweenStages == nil {
			t.Fatal("buildIssueDetail(MT-4).BetweenStages = nil, want non-nil")
		}
		want := toStageHopResponse(hop, sentence.Context{})
		want.StatusText, want.StatusTextFull = got.BetweenStages.StatusText, got.BetweenStages.StatusTextFull
		if !reflect.DeepEqual(*got.BetweenStages, want) {
			t.Errorf("buildIssueDetail(MT-4).BetweenStages = %+v, want %+v", got.BetweenStages, want)
		}
		if got.IssueID != "id-4" || got.Workspace != nil || got.LastError != nil || *got.Attempts != (attemptsInfo{}) {
			t.Errorf("buildIssueDetail(MT-4) IssueID, Workspace, LastError, Attempts = %q, %v, %v, %+v, want %q, nil, nil, zeroed",
				got.IssueID, got.Workspace, got.LastError, got.Attempts, "id-4")
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		got := buildIssueDetail("NONEXISTENT", "", snap, sentence.Context{})
		if got != nil {
			t.Errorf("buildIssueDetail(NONEXISTENT) = %v, want nil", got)
		}
	})

	t.Run("running issue without workspace path", func(t *testing.T) {
		t.Parallel()
		noWSSnap := orchestrator.RuntimeSnapshotResult{
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "id-3", Identifier: "MT-300", State: "To Do"},
			},
		}
		got := buildIssueDetail("MT-300", "", noWSSnap, sentence.Context{})
		if got == nil {
			t.Fatal("buildIssueDetail(MT-300) = nil, want non-nil")
			return
		}
		if got.Workspace != nil {
			t.Errorf("Workspace = %v, want nil when WorkspacePath is empty", got.Workspace)
		}
	})

	t.Run("retrying issue without error", func(t *testing.T) {
		t.Parallel()
		noErrSnap := orchestrator.RuntimeSnapshotResult{
			Retrying: []orchestrator.SnapshotRetryEntry{
				{IssueID: "id-4", Identifier: "MT-400", Attempt: 1, Error: ""},
			},
		}
		got := buildIssueDetail("MT-400", "", noErrSnap, sentence.Context{})
		if got == nil {
			t.Fatal("buildIssueDetail(MT-400) = nil, want non-nil")
			return
		}
		if got.LastError != nil {
			t.Errorf("LastError = %v, want nil when Error is empty", got.LastError)
		}
	})
}

func acknowledgeHold(t *testing.T, snap *orchestrator.RuntimeSnapshotResult, at time.Time) {
	t.Helper()
	for _, cause := range orchestrator.AttentionCauses(*snap, nil) {
		if cause.Kind == orchestrator.AttentionKindBudgetHeld {
			snap.AttentionAcknowledgements = map[string]time.Time{cause.ID: at}
			return
		}
	}
	t.Fatal("AttentionCauses() holds no budget_held cause, want one to acknowledge")
}

func TestHandleState(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("success with populated snapshot", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "id-1", Identifier: "MT-1", StartedAt: fixedTime},
			},
			Retrying: []orchestrator.SnapshotRetryEntry{
				{IssueID: "id-2", Identifier: "MT-2", DueAtMS: fixedTime.UnixMilli()},
			},
			AgentTotals: orchestrator.SnapshotAgentTotals{
				InputTokens:    100,
				OutputTokens:   50,
				TotalTokens:    150,
				SecondsRunning: 60.0,
			},
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
		}

		var body stateResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		_ = resp.Body.Close()

		if body.Counts.Running != 1 {
			t.Errorf("Counts.Running = %d, want 1", body.Counts.Running)
		}
		if body.Counts.Retrying != 1 {
			t.Errorf("Counts.Retrying = %d, want 1", body.Counts.Retrying)
		}
		if body.AgentTotals.TotalTokens != 150 {
			t.Errorf("AgentTotals.TotalTokens = %d, want 150", body.AgentTotals.TotalTokens)
		}
	})

	t.Run("attention", func(t *testing.T) {
		t.Parallel()

		parkedAt := time.Date(2026, 3, 24, 11, 0, 0, 0, time.UTC)
		heldAndParked := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			Parked: []orchestrator.SnapshotParkedEntry{
				{IssueID: "id-1", Identifier: "MT-652", Reason: "agent_blocked", ParkedAt: parkedAt},
			},
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{IssueID: "id-1", Identifier: "MT-652", Reason: "session_budget", UsedSessions: 3, BudgetSessions: 3, ExhaustedAt: parkedAt.Add(time.Hour)},
			},
		}
		acknowledgeHold(t, &heldAndParked, parkedAt.Add(90*time.Minute))
		tests := []struct {
			name               string
			snap               orchestrator.RuntimeSnapshotResult
			wantLen            int
			wantCount          int
			wantAcknowledgedAt map[string]any
			wantItem           map[string]any
		}{
			{
				name: "clean state serves an empty list",
				snap: orchestrator.RuntimeSnapshotResult{GeneratedAt: fixedTime},
			},
			{
				name: "parked issue serves tagged keys and omits empty issue keys",
				snap: orchestrator.RuntimeSnapshotResult{
					GeneratedAt: fixedTime,
					Parked: []orchestrator.SnapshotParkedEntry{
						{IssueID: "id-1", Identifier: "MT-652", Reason: "agent_blocked", Label: "needs-human", LabelApplied: true, ParkedAt: parkedAt},
					},
				},
				wantLen:            1,
				wantCount:          1,
				wantAcknowledgedAt: map[string]any{"parked": nil},
				wantItem: map[string]any{
					"kind":             "parked",
					"severity":         "warning",
					"since":            "2026-03-24T11:00:00Z",
					"issue_id":         "id-1",
					"issue_identifier": "MT-652",
					"reason_text":      "Parked: the agent needs a person",
					"action":           map[string]any{"kind": "acknowledge"},
					"evidence":         map[string]any{"reason": "agent_blocked", "label": "needs-human"},
				},
			},
			{
				name:               "acknowledged hold stays listed and leaves the count while the park of its issue stays counted",
				snap:               heldAndParked,
				wantLen:            2,
				wantCount:          1,
				wantAcknowledgedAt: map[string]any{"budget_held": "2026-03-24T12:30:00Z", "parked": nil},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				ts := testServer(t, fixedSnapshot(tt.snap), acceptingRefresh())
				resp, err := http.Get(ts.URL + "/api/v1/state")
				if err != nil {
					t.Fatalf("GET /api/v1/state: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				var body struct {
					Attention []map[string]any `json:"attention"`
					Counts    struct {
						Attention int `json:"attention"`
					} `json:"counts"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode response: %v", err)
				}

				if body.Attention == nil || len(body.Attention) != tt.wantLen {
					t.Fatalf("GET /api/v1/state attention = %v, want a non-null list of %d", body.Attention, tt.wantLen)
				}
				if body.Counts.Attention != tt.wantCount {
					t.Errorf("GET /api/v1/state counts.attention = %d, want %d", body.Counts.Attention, tt.wantCount)
				}
				for _, listed := range body.Attention {
					kind, _ := listed["kind"].(string)
					got, ok := listed["acknowledged_at"]
					if !ok {
						t.Errorf("attention %q lacks acknowledged_at, want the key present", kind)
						continue
					}
					if want := tt.wantAcknowledgedAt[kind]; got != want {
						t.Errorf("attention %q acknowledged_at = %v, want %v", kind, got, want)
					}
				}
				if tt.wantItem == nil {
					return
				}
				item := body.Attention[0]
				for key, want := range tt.wantItem {
					if !reflect.DeepEqual(item[key], want) {
						t.Errorf("attention[0].%s = %v, want %v", key, item[key], want)
					}
				}
				for _, key := range []string{"id", "why_text", "clears_text"} {
					if text, _ := item[key].(string); text == "" {
						t.Errorf("attention[0].%s = %q, want non-empty", key, text)
					}
				}
				for _, key := range []string{"display_identifier", "issue_title", "issue_url"} {
					if _, ok := item[key]; ok {
						t.Errorf("attention[0] carries %q, want it omitted when empty", key)
					}
				}
			})
		}
	})

	t.Run("empty state returns non-null JSON arrays", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			Running:     nil,
			Retrying:    nil,
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rawMap); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if string(rawMap["running"]) == "null" {
			t.Error("running = null in JSON, want []")
		}
		if string(rawMap["retrying"]) == "null" {
			t.Error("retrying = null in JSON, want []")
		}
		if got := string(rawMap["rate_limits"]); got != "{}" {
			t.Errorf("rate_limits = %s in JSON, want {}", got)
		}
	})

	t.Run("rate limits serve unknown values as null", func(t *testing.T) {
		t.Parallel()

		reset := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
		share := 82.0
		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			UsageWindows: map[string]orchestrator.SnapshotUsageWindows{
				"claude-code": {
					ReportedAt: fixedTime,
					Windows: []domain.UsageWindow{
						{Key: "seven_day", Label: "Weekly limit", UsedPercent: &share, Status: domain.UsageWindowWarning, ResetsAt: reset, ReportedAt: fixedTime, Amount: &domain.UsageWindowAmount{Used: 410, Limit: 500, Unit: "credits"}},
						{Key: "five_hour", Label: "5-hour limit", ReportedAt: fixedTime},
					},
				},
				"codex": {ReportedAt: fixedTime, Windows: []domain.UsageWindow{}},
			},
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		_, body := getJSON[struct {
			RateLimits map[string]struct {
				Windows []struct {
					Label       string          `json:"label"`
					UsedPercent *float64        `json:"used_percent"`
					Status      *string         `json:"status"`
					ResetsAt    *time.Time      `json:"resets_at"`
					Amount      json.RawMessage `json:"amount"`
				} `json:"windows"`
			} `json:"rate_limits"`
		}](t, ts, "/api/v1/state")

		claude := body.RateLimits["claude-code"].Windows
		if len(claude) != 2 {
			t.Fatalf("len(rate_limits[claude-code].windows) = %d, want 2", len(claude))
		}
		known, unknown := claude[0], claude[1]
		if known.UsedPercent == nil || *known.UsedPercent != 82 {
			t.Errorf("known window used_percent = %v, want 82", known.UsedPercent)
		}
		if known.Status == nil || *known.Status != "warning" {
			t.Errorf("known window status = %v, want %q", known.Status, "warning")
		}
		if known.ResetsAt == nil || !known.ResetsAt.Equal(reset) {
			t.Errorf("known window resets_at = %v, want %v", known.ResetsAt, reset)
		}
		if unknown.UsedPercent != nil {
			t.Errorf("unknown window used_percent = %v, want null", *unknown.UsedPercent)
		}
		if unknown.Status != nil {
			t.Errorf("unknown window status = %q, want null", *unknown.Status)
		}
		if unknown.ResetsAt != nil {
			t.Errorf("unknown window resets_at = %v, want null", *unknown.ResetsAt)
		}
		if got, want := string(known.Amount), `{"used":410,"limit":500,"unit":"credits"}`; got != want {
			t.Errorf("known window amount = %s, want %s", got, want)
		}
		if got := string(unknown.Amount); got != "null" {
			t.Errorf("unknown window amount = %s, want null", got)
		}

		codex, ok := body.RateLimits["codex"]
		if !ok {
			t.Fatal("rate_limits[codex] missing, want a member with no current window")
		}
		if codex.Windows == nil {
			t.Error("rate_limits[codex].windows = null, want []")
		}
	})

	t.Run("failed reaction activity read serves null pr_loop", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			Running: []orchestrator.SnapshotRunningEntry{
				{IssueID: "id-1", Identifier: "MT-1", StageLineage: orchestrator.StageLineage{ChainID: "chain-1", DispatchedAt: fixedTime}},
				{IssueID: "id-2", Identifier: "MT-2", StageLineage: orchestrator.StageLineage{ChainID: "chain-2", DispatchedAt: fixedTime}},
			},
		}
		srv := New(Params{
			SnapshotFn: fixedSnapshot(snap),
			RefreshFn:  acceptingRefresh(),
			Logger:     slog.New(slog.DiscardHandler),
			RunHistory: &fakeRunHistory{passesErr: errors.New("database is locked")},
			WorkflowShapeFn: func() orchestrator.WorkflowShape {
				return orchestrator.WorkflowShape{Reactions: orchestrator.ShapeReactions{CIFailure: &orchestrator.ShapeCIFailure{}}}
			},
		})
		ts := httptest.NewServer(srv.Mux())
		t.Cleanup(ts.Close)

		status, body := getJSON[stateResponse](t, ts, "/api/v1/state")

		if status != http.StatusOK {
			t.Fatalf("GET /api/v1/state status = %d, want %d", status, http.StatusOK)
		}
		if len(body.Active) != 2 {
			t.Fatalf("GET /api/v1/state active = %d entries, want 2", len(body.Active))
		}
		for _, entry := range body.Active {
			if entry.PRLoop != nil {
				t.Errorf("GET /api/v1/state active %s pr_loop = %v, want null", entry.IssueID, entry.PRLoop)
			}
		}
	})
}

func TestHandleState_EveryRowCarriesBothForms(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{IssueID: "id-1", Identifier: "MT-11", StartedAt: now, TurnCount: 2},
			{IssueID: "id-2", Identifier: "MT-22", StartedAt: now, TurnCount: 5},
		},
		Retrying:         []orchestrator.SnapshotRetryEntry{{IssueID: "id-3", Identifier: "MT-33", Attempt: 1, Trigger: "error", DueAtMS: now.Add(time.Minute).UnixMilli()}},
		BudgetExhausted:  []orchestrator.SnapshotBudgetEntry{{IssueID: "id-4", Identifier: "MT-44", Reason: "session_budget"}},
		Parked:           []orchestrator.SnapshotParkedEntry{{IssueID: "id-5", Identifier: "MT-55", Reason: "agent_blocked", Label: "needs-human"}},
		PendingReactions: []orchestrator.SnapshotPendingReaction{{IssueID: "id-6", Identifier: "MT-66", Kind: "ci", NextPollAt: now.Add(time.Minute), PullRequest: orchestrator.SnapshotPullRequest{Number: 7}}},
		BetweenStages:    []orchestrator.SnapshotStageHop{{IssueID: "id-7", Identifier: "MT-77", SourceRule: "specify", TargetRule: "implement", TargetLabel: "stage:implement"}},
		Held: []orchestrator.SnapshotHeldEntry{{
			IssueID: "id-8", Identifier: "MT-88",
			Hold: orchestrator.SnapshotHold{
				Reason: orchestrator.SkipBlockedBy, Since: now,
				Blockers: []orchestrator.SnapshotBlocker{{IssueID: "id-9", Identifier: "MT-99", State: "In Progress"}},
			},
		}},
	}
	ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())

	_, body := getJSON[map[string]json.RawMessage](t, ts, "/api/v1/state")

	lists := []string{"running", "retrying", "budget_exhausted", "parked", "pending_reactions", "between_stages", "held"}
	served := 0
	for _, list := range lists {
		var rows []struct {
			Identifier string `json:"issue_identifier"`
			Short      string `json:"status_text"`
			Long       string `json:"status_text_full"`
			Hold       *struct {
				ReasonText string `json:"reason_text"`
			} `json:"hold"`
		}
		if err := json.Unmarshal(body[list], &rows); err != nil {
			t.Fatalf("GET /api/v1/state %s: %v", list, err)
		}
		for _, row := range rows {
			served++
			if row.Short == "" || row.Long == "" || strings.Contains(row.Long, row.Identifier) {
				t.Errorf("GET /api/v1/state %s %s status_text, status_text_full = %q, %q, want a short form and a long form that does not repeat the issue key", list, row.Identifier, row.Short, row.Long)
			}
			if list == "held" && (row.Hold == nil || row.Hold.ReasonText != row.Short) {
				t.Errorf("GET /api/v1/state held %s status_text = %q, want hold.reason_text %+v", row.Identifier, row.Short, row.Hold)
			}
		}
	}
	if served != 8 {
		t.Errorf("GET /api/v1/state served %d rows with a sentence, want 8", served)
	}
}

func TestHandleState_Budget(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("decodes identifier, reason, and the used and budgeted numbers", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt:          fixedTime,
			BudgetExhaustedCount: 1,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{
					IssueID:        "id-budget-1",
					Identifier:     "MT-649",
					DisplayID:      "org/repo#649",
					Reason:         "session_budget",
					UsedSessions:   3,
					BudgetSessions: 3,
					BudgetTokens:   10000,
					ExhaustedAt:    fixedTime,
				},
			},
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body := decodeJSON[stateResponse](t, resp)
		if body.Counts.BudgetExhausted != 1 {
			t.Errorf("Counts.BudgetExhausted = %d, want 1", body.Counts.BudgetExhausted)
		}
		if len(body.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(body.BudgetExhausted))
		}
		entry := body.BudgetExhausted[0]
		if entry.IssueID != "id-budget-1" {
			t.Errorf("IssueID = %q, want %q", entry.IssueID, "id-budget-1")
		}
		if entry.IssueIdentifier != "MT-649" {
			t.Errorf("IssueIdentifier = %q, want %q", entry.IssueIdentifier, "MT-649")
		}
		if entry.Reason != "session_budget" {
			t.Errorf("Reason = %q, want %q", entry.Reason, "session_budget")
		}
		if entry.UsedSessions != 3 || entry.BudgetSessions == nil || *entry.BudgetSessions != 3 {
			t.Errorf("UsedSessions/BudgetSessions = %d/%v, want 3/3", entry.UsedSessions, entry.BudgetSessions)
		}
		if entry.BudgetTokens == nil || *entry.BudgetTokens != 10000 {
			t.Errorf("BudgetTokens = %v, want 10000", entry.BudgetTokens)
		}
	})

	t.Run("empty set serializes as an empty array, not null", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{GeneratedAt: fixedTime}), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var rawMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rawMap); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if string(rawMap["budget_exhausted"]) == "null" {
			t.Error("budget_exhausted = null in JSON, want []")
		}
	})

	t.Run("no emitted string names a setting to change", func(t *testing.T) {
		t.Parallel()

		snap := orchestrator.RuntimeSnapshotResult{
			GeneratedAt: fixedTime,
			BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
				{IssueID: "id-1", Identifier: "MT-1", Reason: "session_budget", UsedSessions: 3, BudgetSessions: 3, ExhaustedAt: fixedTime},
			},
		}

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/state")
		if err != nil {
			t.Fatalf("GET /api/v1/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var rawMap map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&rawMap); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		delete(rawMap, "attention")
		for key, raw := range rawMap {
			for _, forbidden := range []string{"max_sessions", "max_tokens", "max_consecutive_absences"} {
				if strings.Contains(string(raw), forbidden) {
					t.Errorf("response member %q contains %q, want absent", key, forbidden)
				}
			}
		}
	})
}

func TestHandleIssueDetail(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	snap := orchestrator.RuntimeSnapshotResult{
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:       "id-1",
				Identifier:    "MT-100",
				State:         "In Progress",
				StartedAt:     fixedTime,
				WorkspacePath: "/tmp/ws/mt-100",
			},
		},
	}

	t.Run("found running issue", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())

		status, body := getJSON[issueDetailResponse](t, ts, "/api/v1/MT-100")

		if status != http.StatusOK || body.IssueIdentifier != "MT-100" || body.Status != "running" {
			t.Errorf("GET /api/v1/MT-100 = %d %q %q, want %d %q %q",
				status, body.IssueIdentifier, body.Status, http.StatusOK, "MT-100", "running")
		}
		if body.StatusText == "" || body.StatusTextFull == "" || body.StatusText != body.Running.StatusText {
			t.Errorf("GET /api/v1/MT-100 status_text = %q, want the running row's sentence %q", body.StatusText, body.Running.StatusText)
		}
	})

	t.Run("not found returns 404", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())

		status, body := getJSON[errorResponse](t, ts, "/api/v1/NONEXISTENT")

		if status != http.StatusNotFound || body.Error.Code != "issue_not_found" {
			t.Errorf("GET /api/v1/NONEXISTENT = %d %q, want %d %q", status, body.Error.Code, http.StatusNotFound, "issue_not_found")
		}
	})

	history := []RunHistoryEntry{{ID: 3, IssueID: "id-1", Identifier: "MT-100", Status: "succeeded"}}
	dbClosed := errors.New("db closed")
	historyTests := []struct {
		name       string
		history    *fakeRunHistory
		path       string
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{name: "failed read for a live identifier", history: &fakeRunHistory{err: dbClosed}, path: "MT-100", wantStatus: http.StatusServiceUnavailable, wantCode: "history_unavailable"},
		{name: "failed read for a non-live identifier", history: &fakeRunHistory{err: dbClosed}, path: "MT-OLD", wantStatus: http.StatusServiceUnavailable, wantCode: "history_unavailable"},
		{name: "failed totals read", history: &fakeRunHistory{rows: history, totalsErr: dbClosed}, path: "MT-100", wantStatus: http.StatusServiceUnavailable, wantCode: "history_unavailable"},
		{name: "failed outcome read", history: &fakeRunHistory{rows: history, outcomesErr: dbClosed}, path: "MT-100", wantStatus: http.StatusServiceUnavailable, wantCode: "history_unavailable"},
		{
			name: "no live entry and no run", history: &fakeRunHistory{}, path: "MT-OLD", wantStatus: http.StatusNotFound, wantCode: "issue_not_found",
			wantMsg: `issue identifier "MT-OLD" not found in current state or run history`,
		},
	}

	for _, tt := range historyTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := historyServer(t, snap, tt.history)

			status, body := getJSON[errorResponse](t, ts, "/api/v1/"+tt.path)

			if status != tt.wantStatus || body.Error.Code != tt.wantCode {
				t.Errorf("GET /api/v1/%s = %d %q, want %d %q", tt.path, status, body.Error.Code, tt.wantStatus, tt.wantCode)
			}
			if tt.wantMsg != "" && body.Error.Message != tt.wantMsg {
				t.Errorf("GET /api/v1/%s error message = %q, want %q", tt.path, body.Error.Message, tt.wantMsg)
			}
		})
	}
}

func TestHandleIssueDetail_StatusSentence(t *testing.T) {
	t.Parallel()

	pending := pendingReactionRows()
	handedOff := IssueOutcome{Kind: "handed_off", State: "In Review"}

	tests := []struct {
		name             string
		snap             orchestrator.RuntimeSnapshotResult
		history          *fakeRunHistory
		path             string
		wantShort        string
		wantFull         string
		wantOutcomeShort string
		wantOutcomeFull  string
	}{
		{
			name: "between stages",
			snap: orchestrator.RuntimeSnapshotResult{BetweenStages: []orchestrator.SnapshotStageHop{
				{IssueID: "id-1", Identifier: "MT-1", SourceRule: "specify", TargetRule: "implement", TargetLabel: "stage:implement"},
			}},
			history:   &fakeRunHistory{},
			path:      "MT-1",
			wantShort: "Between stages: specify to implement",
			wantFull:  "Moved from stage specify to stage implement, and Sortie added `stage:implement`; implement starts when a poll tick reads that label on the issue.",
		},
		{
			name:      "escalated pending reaction decides when it is not first",
			snap:      orchestrator.RuntimeSnapshotResult{PendingReactions: []orchestrator.SnapshotPendingReaction{pending[1], pending[0]}},
			history:   &fakeRunHistory{},
			path:      "MT-653",
			wantShort: "CI escalated on #931",
			wantFull:  "Sortie escalated failing CI on PR #931; it keeps watching the pull request. CI fixes used: 2 of 2.",
		},
		{
			name: "history only, merged by auto-merge",
			history: &fakeRunHistory{
				rows:     []RunHistoryEntry{{ID: 5, IssueID: "id-1", Identifier: "MT-1", Status: "succeeded"}},
				outcomes: map[string]IssueOutcome{"id-1": {Kind: "merged", MergeReactionKind: orchestrator.ReactionKindAutoMerge, PRNumber: 88}},
			},
			path:             "MT-1",
			wantShort:        "Merged #88",
			wantFull:         "Ended merged: auto-merge merged PR #88.",
			wantOutcomeShort: "Merged #88",
			wantOutcomeFull:  "Ended merged: auto-merge merged PR #88.",
		},
		{
			name: "history only, hop ceiling then handoff",
			history: &fakeRunHistory{
				rows:     []RunHistoryEntry{{ID: 5, IssueID: "id-2", Identifier: "MT-2", Status: "succeeded", StageTarget: "implement", StageResult: "ceiling"}},
				outcomes: map[string]IssueOutcome{"id-2": handedOff},
			},
			path:             "MT-2",
			wantShort:        "Handed off to In Review; stage implement not reached",
			wantFull:         "Handed off to In Review because the hop to stage implement was not made: the issue reached its limit of consecutive stage moves (`dispatch.max_consecutive_hops`).",
			wantOutcomeShort: "Handed off to In Review",
			wantOutcomeFull:  "Handed off: Sortie moved the issue to In Review.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := historyServer(t, tt.snap, tt.history)

			status, body := getJSON[issueDetailResponse](t, ts, "/api/v1/"+tt.path)

			if status != http.StatusOK {
				t.Fatalf("GET /api/v1/%s status = %d, want %d", tt.path, status, http.StatusOK)
			}
			if body.StatusText != tt.wantShort || body.StatusTextFull != tt.wantFull {
				t.Errorf("GET /api/v1/%s status_text, status_text_full = %q, %q, want %q, %q",
					tt.path, body.StatusText, body.StatusTextFull, tt.wantShort, tt.wantFull)
			}
			if row := body.BetweenStages; row != nil && (row.StatusText != body.StatusText || row.StatusTextFull != body.StatusTextFull) {
				t.Errorf("GET /api/v1/%s between_stages pair = %q, %q, want the top-level pair %q, %q",
					tt.path, row.StatusText, row.StatusTextFull, body.StatusText, body.StatusTextFull)
			}
			if tt.wantOutcomeShort == "" {
				return
			}
			if body.History == nil || len(body.History.Runs) == 0 {
				t.Fatalf("GET /api/v1/%s history = %+v, want a run", tt.path, body.History)
			}
			outcomes := []*issueOutcomeResponse{body.History.IssueOutcome, body.History.Runs[0].IssueOutcome}
			for _, o := range outcomes {
				if o == nil || o.StatusText != tt.wantOutcomeShort || o.StatusTextFull != tt.wantOutcomeFull {
					t.Errorf("GET /api/v1/%s issue_outcome = %+v, want pair %q, %q", tt.path, o, tt.wantOutcomeShort, tt.wantOutcomeFull)
				}
			}
		})
	}
}

type totalsHistory struct {
	*fakeRunHistory
	totals IssueRunTotals
}

func (h totalsHistory) IssueTotals(context.Context, string) (IssueRunTotals, error) {
	return h.totals, nil
}

func nextOutcomesServer(t *testing.T, snap orchestrator.RuntimeSnapshotResult, history RunHistoryReader) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		RunHistory: history,
		WorkflowShapeFn: func() orchestrator.WorkflowShape {
			return orchestrator.WorkflowShape{Agent: orchestrator.ShapeAgent{MaxSessions: new(3)}}
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func TestHandleIssueDetail_NextOutcomes(t *testing.T) {
	t.Parallel()

	watching := orchestrator.SnapshotPendingReaction{
		IssueID: "id-1", Identifier: "MT-1", Kind: orchestrator.ReactionKindCI, ConfigKey: "ci_failure",
		WatchEndsAt: new(time.Date(2026, 3, 24, 7, 30, 0, 0, time.FixedZone("EST", -5*60*60))),
	}
	unwatched := orchestrator.SnapshotPendingReaction{
		IssueID: "id-1", Identifier: "MT-1", Kind: orchestrator.ReactionKindMergeCompletion, ConfigKey: "merge_completion",
	}
	oneRunShort := totalsHistory{
		fakeRunHistory: &fakeRunHistory{rows: []RunHistoryEntry{{ID: 3, IssueID: "id-1", Identifier: "MT-1", Status: "succeeded"}}},
		totals:         IssueRunTotals{Usage: orchestrator.SnapshotIssueUsage{Sessions: 2}},
	}
	held := "Sortie holds the issue at its session budget (3 of 3 sessions used)."

	tests := []struct {
		name          string
		snap          orchestrator.RuntimeSnapshotResult
		history       RunHistoryReader
		wantNext      []nextOutcomeResponse
		wantWatchEnds map[string]string
	}{
		{
			name:    "work session one run short of the session budget holds on both exits",
			snap:    orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{{IssueID: "id-1", Identifier: "MT-1"}}},
			history: oneRunShort,
			wantNext: []nextOutcomeResponse{
				{Condition: "If the run succeeds", Text: held},
				{Condition: "If the run fails", Text: held},
			},
		},
		{
			name:     "history only issue serves an empty list",
			history:  oneRunShort,
			wantNext: []nextOutcomeResponse{},
		},
		{
			name:          "watch end is served in UTC and null when the kind has none",
			snap:          orchestrator.RuntimeSnapshotResult{PendingReactions: []orchestrator.SnapshotPendingReaction{watching, unwatched}},
			wantWatchEnds: map[string]string{"ci": "2026-03-24T12:30:00Z", "merge-completion": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := nextOutcomesServer(t, tt.snap, tt.history)

			status, body := getJSON[struct {
				NextOutcomes     *[]nextOutcomeResponse `json:"next_outcomes"`
				PendingReactions []struct {
					Kind        string  `json:"kind"`
					WatchEndsAt *string `json:"watch_ends_at"`
				} `json:"pending_reactions"`
			}](t, ts, "/api/v1/MT-1")

			if status != http.StatusOK || body.NextOutcomes == nil {
				t.Fatalf("GET /api/v1/MT-1 = %d with next_outcomes %v, want %d with a list", status, body.NextOutcomes, http.StatusOK)
			}
			if tt.wantNext != nil && !reflect.DeepEqual(*body.NextOutcomes, tt.wantNext) {
				t.Errorf("GET /api/v1/MT-1 next_outcomes = %+v, want %+v", *body.NextOutcomes, tt.wantNext)
			}
			if len(body.PendingReactions) != len(tt.wantWatchEnds) {
				t.Fatalf("GET /api/v1/MT-1 pending_reactions = %d rows, want %d", len(body.PendingReactions), len(tt.wantWatchEnds))
			}
			for _, row := range body.PendingReactions {
				got := ""
				if row.WatchEndsAt != nil {
					got = *row.WatchEndsAt
				}
				if got != tt.wantWatchEnds[row.Kind] {
					t.Errorf("GET /api/v1/MT-1 %s watch_ends_at = %q, want %q", row.Kind, got, tt.wantWatchEnds[row.Kind])
				}
			}
		})
	}
}

func TestHandleIssueDetail_Parked(t *testing.T) {
	t.Parallel()

	parked := []orchestrator.SnapshotParkedEntry{
		{IssueID: "id-p", Identifier: "MT-9", Reason: "agent_blocked", Label: "needs-human", ParkedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)},
	}
	otherIssueRow := orchestrator.SnapshotPendingReaction{IssueID: "id-o", Identifier: "MT-700", Kind: "merge", ConfigKey: "merge"}
	rows := append(pendingReactionRows(), otherIssueRow)

	tests := []struct {
		name        string
		snap        orchestrator.RuntimeSnapshotResult
		identifier  string
		wantCode    int
		wantStatus  string
		wantIssueID string
		wantKinds   []string
	}{
		{
			name:        "parked issue serves no pending reactions",
			snap:        orchestrator.RuntimeSnapshotResult{Parked: parked, PendingReactions: rows},
			identifier:  "MT-9",
			wantCode:    http.StatusOK,
			wantStatus:  "parked",
			wantIssueID: "id-p",
			wantKinds:   []string{},
		},
		{
			name:        "issue held only by pending reactions serves its own rows",
			snap:        orchestrator.RuntimeSnapshotResult{PendingReactions: rows},
			identifier:  "MT-653",
			wantCode:    http.StatusOK,
			wantStatus:  "pending_reactions",
			wantIssueID: "mno345",
			wantKinds:   []string{"ci", "review"},
		},
		{
			name: "retrying issue keeps its status and still serves its pending reactions",
			snap: orchestrator.RuntimeSnapshotResult{
				Retrying:         []orchestrator.SnapshotRetryEntry{{IssueID: "mno345", Identifier: "MT-653"}},
				PendingReactions: rows,
			},
			identifier:  "MT-653",
			wantCode:    http.StatusOK,
			wantStatus:  "retrying",
			wantIssueID: "mno345",
			wantKinds:   []string{"ci", "review"},
		},
		{
			name:       "identifier in no list answers 404",
			snap:       orchestrator.RuntimeSnapshotResult{Parked: parked, PendingReactions: rows},
			identifier: "NONEXISTENT",
			wantCode:   http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := testServer(t, fixedSnapshot(tt.snap), acceptingRefresh())

			resp, err := http.Get(ts.URL + "/api/v1/" + tt.identifier)
			if err != nil {
				t.Fatalf("GET /api/v1/%s: %v", tt.identifier, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.wantCode {
				t.Fatalf("GET /api/v1/%s status = %d, want %d", tt.identifier, resp.StatusCode, tt.wantCode)
			}
			if tt.wantCode != http.StatusOK {
				return
			}
			body := decodeJSON[issueDetailResponse](t, resp)

			if body.Status != tt.wantStatus {
				t.Errorf("GET /api/v1/%s status = %q, want %q", tt.identifier, body.Status, tt.wantStatus)
			}
			if body.IssueID != tt.wantIssueID {
				t.Errorf("GET /api/v1/%s issue_id = %q, want %q", tt.identifier, body.IssueID, tt.wantIssueID)
			}
			if body.Workspace != nil || body.LastError != nil || body.Attempts == nil || *body.Attempts != (attemptsInfo{}) {
				t.Errorf("GET /api/v1/%s workspace, attempts, last_error = %+v, %+v, %v, want nil, zero, nil",
					tt.identifier, body.Workspace, body.Attempts, body.LastError)
			}
			if body.PendingReactions == nil {
				t.Fatalf("GET /api/v1/%s pending_reactions = null, want an array", tt.identifier)
			}
			gotKinds := []string{}
			for _, row := range body.PendingReactions {
				gotKinds = append(gotKinds, row.Kind)
			}
			if !slices.Equal(gotKinds, tt.wantKinds) {
				t.Errorf("GET /api/v1/%s pending_reactions kinds = %v, want %v", tt.identifier, gotKinds, tt.wantKinds)
			}
		})
	}
}

func TestHandleIssueDetail_Budget(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	usedTokens := int64(1500)
	unmeasured := 1
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: fixedTime,
		BudgetExhausted: []orchestrator.SnapshotBudgetEntry{
			{
				IssueID:            "id-budget-2",
				Identifier:         "MT-650",
				Reason:             "token_budget",
				UsedSessions:       2,
				UsedTokens:         &usedTokens,
				BudgetTokens:       1000,
				UnmeasuredSessions: &unmeasured,
				ExhaustedAt:        fixedTime,
			},
		},
	}

	t.Run("budget-exhausted issue that is neither running nor retrying returns 200", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/MT-650")
		if err != nil {
			t.Fatalf("GET /api/v1/MT-650: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		body := decodeJSON[issueDetailResponse](t, resp)
		if body.Status != "budget_exhausted" {
			t.Errorf("Status = %q, want %q", body.Status, "budget_exhausted")
		}
		if body.BudgetExhausted == nil {
			t.Fatal("BudgetExhausted = nil, want populated")
		}
		if body.BudgetExhausted.Reason != "token_budget" {
			t.Errorf("BudgetExhausted.Reason = %q, want %q", body.BudgetExhausted.Reason, "token_budget")
		}
		if body.BudgetExhausted.UsedTokens == nil || *body.BudgetExhausted.UsedTokens != 1500 {
			t.Errorf("BudgetExhausted.UsedTokens = %v, want 1500", body.BudgetExhausted.UsedTokens)
		}
		if body.LastError != nil {
			t.Errorf("LastError = %v, want nil (a budget hold is not an error)", body.LastError)
		}
	})

	t.Run("genuine miss still returns 404", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/NONEXISTENT")
		if err != nil {
			t.Fatalf("GET /api/v1/NONEXISTENT: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
	})

	t.Run("no emitted string names a setting to change", func(t *testing.T) {
		t.Parallel()

		ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())
		resp, err := http.Get(ts.URL + "/api/v1/MT-650")
		if err != nil {
			t.Fatalf("GET /api/v1/MT-650: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		for _, forbidden := range []string{"max_sessions", "max_tokens", "max_consecutive_absences"} {
			if strings.Contains(string(raw), forbidden) {
				t.Errorf("response body contains %q, want absent", forbidden)
			}
		}
	})
}

func TestHandleRefresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		refreshFn     RefreshFunc
		draining      bool
		wantStatus    int
		wantQueued    bool
		wantCoalesced bool
		wantOps       int
		wantText      string
	}{
		{
			name: "accepted", refreshFn: acceptingRefresh(),
			wantStatus: http.StatusAccepted, wantQueued: true, wantOps: 2,
			wantText: "Tracker poll and reconciliation queued.",
		},
		{
			name: "coalesced", refreshFn: coalescingRefresh(),
			wantStatus: http.StatusAccepted, wantQueued: true, wantCoalesced: true, wantOps: 2,
			wantText: "A tracker poll and reconciliation was already queued; this request joined it.",
		},
		{
			name: "rejected during drain even when refreshFn accepts", refreshFn: acceptingRefresh(), draining: true,
			wantStatus: http.StatusConflict,
			wantText:   "Nothing queued: Sortie is shutting down.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := New(Params{
				SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
				RefreshFn:  tt.refreshFn,
				Logger:     slog.New(slog.DiscardHandler),
			})
			if tt.draining {
				srv.SetDraining()
			}
			ts := httptest.NewServer(srv.Mux())
			t.Cleanup(ts.Close)

			resp, err := http.Post(ts.URL+"/api/v1/refresh", "", nil)
			if err != nil {
				t.Fatalf("POST /api/v1/refresh: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body := decodeJSON[refreshResponse](t, resp)

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("POST /api/v1/refresh status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if body.Queued != tt.wantQueued || body.Coalesced != tt.wantCoalesced || len(body.Operations) != tt.wantOps {
				t.Errorf("POST /api/v1/refresh queued, coalesced, operations = %t, %t, %v, want %t, %t, %d operations",
					body.Queued, body.Coalesced, body.Operations, tt.wantQueued, tt.wantCoalesced, tt.wantOps)
			}
			if body.OutcomeText != tt.wantText {
				t.Errorf("POST /api/v1/refresh outcome_text = %q, want %q", body.OutcomeText, tt.wantText)
			}
		})
	}
}

func TestHandleIssueTriggers(t *testing.T) {
	t.Parallel()

	retryNow := func(_ context.Context, identifier string) orchestrator.RetryNowResult {
		if identifier != "MT-650" {
			return orchestrator.RetryNowResult{Status: orchestrator.RetryNowNotFound}
		}
		return orchestrator.RetryNowResult{Status: orchestrator.RetryNowDue, IssueID: "def456", Identifier: identifier, Attempt: 3}
	}
	checkNow := func(_ context.Context, identifier, kind string) orchestrator.CheckNowResult {
		if identifier != "MT-653" || kind != "ci" {
			return orchestrator.CheckNowResult{Status: orchestrator.CheckNowNotFound}
		}
		return orchestrator.CheckNowResult{
			Status: orchestrator.CheckNowDue, IssueID: "mno345", Identifier: identifier, DisplayID: identifier,
			Kind: kind, PRNumber: 931, PRRepository: "acme/web",
		}
	}
	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:  acceptingRefresh(),
		RetryNowFn: retryNow,
		CheckNowFn: checkNow,
		Logger:     slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantText   string
		wantCode   string
	}{
		{name: "scheduled retry", path: "/api/v1/retries/MT-650/run", wantStatus: http.StatusAccepted, wantText: "Retry 3 of MT-650 is due now."},
		{name: "unknown issue", path: "/api/v1/retries/MT-999/run", wantStatus: http.StatusNotFound, wantCode: "retry_not_found"},
		{
			name: "pending reaction check", path: "/api/v1/reactions/MT-653/ci/check", wantStatus: http.StatusAccepted,
			wantText: "MT-653: a check of PR #931 for CI is queued for the next poll tick.",
		},
		{name: "reaction kind the issue lacks", path: "/api/v1/reactions/MT-653/bot-review/check", wantStatus: http.StatusNotFound, wantCode: "reaction_not_found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp, err := http.Post(ts.URL+tt.path, "", nil)
			if err != nil {
				t.Fatalf("POST %s: %v", tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			body := decodeJSON[struct {
				OutcomeText string `json:"outcome_text"`
				errorResponse
			}](t, resp)

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("POST %s status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			}
			if body.OutcomeText != tt.wantText {
				t.Errorf("POST %s outcome_text = %q, want %q", tt.path, body.OutcomeText, tt.wantText)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("POST %s error code = %q, want %q", tt.path, body.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestHandleAcknowledge(t *testing.T) {
	t.Parallel()

	const (
		recordedID = "464f9ff8ea54c4c6cfb43af394234d57"
		repeatedID = "0b8e1c52a7d94f3e8a61c9d27f4b5e30"
		failingID  = "9c3d7a10e5b84f26b1a0d8e47c2f6a95"
	)
	firstAt := time.Date(2026, 2, 24, 20, 0, 0, 0, time.UTC)
	hold := func(id string) orchestrator.AttentionCause {
		return orchestrator.AttentionCause{
			ID:   id,
			Kind: orchestrator.AttentionKindBudgetHeld,
			Budget: &orchestrator.SnapshotBudgetEntry{
				IssueID: "ghi789", Identifier: "MT-651", Reason: "session_budget", UsedSessions: 3, BudgetSessions: 3, ExhaustedAt: firstAt,
			},
		}
	}
	acknowledge := func(_ context.Context, id string) orchestrator.AcknowledgeResult {
		switch id {
		case recordedID:
			return orchestrator.AcknowledgeResult{Status: orchestrator.AcknowledgeRecorded, Cause: hold(id), AcknowledgedAt: firstAt.Add(13*time.Minute + 5*time.Second)}
		case repeatedID:
			return orchestrator.AcknowledgeResult{Status: orchestrator.AcknowledgeAlready, Cause: hold(id), AcknowledgedAt: firstAt}
		case failingID:
			return orchestrator.AcknowledgeResult{Status: orchestrator.AcknowledgeFailed}
		}
		return orchestrator.AcknowledgeResult{Status: orchestrator.AcknowledgeNotFound}
	}
	srv := New(Params{
		SnapshotFn:    fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:     acceptingRefresh(),
		AcknowledgeFn: acknowledge,
		Logger:        slog.New(slog.DiscardHandler),
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	tests := []struct {
		name        string
		id          string
		wantStatus  int
		wantAlready bool
		wantAt      string
		wantText    string
		wantCode    string
	}{
		{
			name:       "recorded item",
			id:         recordedID,
			wantStatus: http.StatusOK,
			wantAt:     "2026-02-24T20:13:05Z",
			wantText:   "Acknowledged MT-651 (Session budget reached). It comes back only if a new cause appears.",
		},
		{
			name:        "item acknowledged before keeps its first time",
			id:          repeatedID,
			wantStatus:  http.StatusOK,
			wantAlready: true,
			wantAt:      "2026-02-24T20:00:00Z",
			wantText:    "Already acknowledged MT-651 (Session budget reached). It comes back only if a new cause appears.",
		},
		{name: "unknown item", id: "ffffffffffffffffffffffffffffffff", wantStatus: http.StatusNotFound, wantCode: "attention_item_not_found"},
		{name: "acknowledgement not saved", id: failingID, wantStatus: http.StatusInternalServerError, wantCode: "acknowledgement_not_saved"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := "/api/v1/attention/" + tt.id + "/acknowledge"
			resp, err := http.Post(ts.URL+path, "", nil)
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			body := decodeJSON[struct {
				acknowledgeResponse
				errorResponse
			}](t, resp)

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("POST %s status = %d, want %d", path, resp.StatusCode, tt.wantStatus)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("POST %s error code = %q, want %q", path, body.Error.Code, tt.wantCode)
			}
			if body.OutcomeText != tt.wantText {
				t.Errorf("POST %s outcome_text = %q, want %q", path, body.OutcomeText, tt.wantText)
			}
			if body.AlreadyAcknowledged != tt.wantAlready {
				t.Errorf("POST %s already_acknowledged = %t, want %t", path, body.AlreadyAcknowledged, tt.wantAlready)
			}
			if tt.wantAt == "" {
				return
			}
			if !body.Acknowledged || body.ID != tt.id {
				t.Errorf("POST %s acknowledged = %t, id = %q, want true, %q", path, body.Acknowledged, body.ID, tt.id)
			}
			if body.AcknowledgedAt == nil || body.AcknowledgedAt.Format(time.RFC3339) != tt.wantAt {
				t.Errorf("POST %s acknowledged_at = %v, want %s", path, body.AcknowledgedAt, tt.wantAt)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	ts := testServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), acceptingRefresh())

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "POST /api/v1/state", method: http.MethodPost, path: "/api/v1/state"},
		{name: "GET /api/v1/refresh", method: http.MethodGet, path: "/api/v1/refresh"},
		{name: "POST /api/v1/events", method: http.MethodPost, path: "/api/v1/events"},
		{name: "POST /api/v1/MT-100", method: http.MethodPost, path: "/api/v1/MT-100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(tt.method, ts.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tt.method, tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
			}

			body := decodeJSON[errorResponse](t, resp)

			if body.Error.Code != "method_not_allowed" {
				t.Errorf("error code = %q, want %q", body.Error.Code, "method_not_allowed")
			}
		})
	}
}

// TestWriteJSONMarshalFailure verifies that writeJSON returns a complete
// error envelope (not a partial body) when JSON encoding fails.
func TestWriteJSONMarshalFailure(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	logger := slog.New(slog.DiscardHandler)

	// math.NaN is not representable in JSON; forces an encoding error.
	writeJSON(rec, logger, http.StatusOK, math.NaN())

	res := rec.Result()
	defer res.Body.Close() //nolint:errcheck // test code

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var envelope errorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body: %s)", err, body)
	}
	if envelope.Error.Code != "internal_error" {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, "internal_error")
	}
}

// testHealthServer creates a server with configurable health-check functions.
func testHealthServer(t *testing.T, opts ...func(*Params)) *httptest.Server {
	t.Helper()
	params := Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		Version:    "1.0.0-test",
		StartedAt:  time.Now(),
	}
	for _, o := range opts {
		o(&params)
	}
	srv := New(params)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func TestHandleLivez_Pass(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t)
	resp, err := http.Get(ts.URL + "/livez")
	if err != nil {
		t.Fatalf("GET /livez: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body := decodeJSON[healthResponse](t, resp)
	if body.Status != "pass" {
		t.Errorf("Status = %q, want %q", body.Status, "pass")
	}
}

func TestHandleLivez_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t)

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(method, ts.URL+"/livez", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s /livez: %v", method, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestHandleReadyz_AllPass(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t,
		func(p *Params) { p.DBPingFn = func(_ context.Context) error { return nil } },
		func(p *Params) { p.PreflightFn = func() bool { return true } },
		func(p *Params) { p.WorkflowLoadedFn = func() bool { return true } },
	)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body := decodeJSON[readyResponse](t, resp)
	if body.Status != "pass" {
		t.Errorf("Status = %q, want %q", body.Status, "pass")
	}
	if body.Version != "1.0.0-test" {
		t.Errorf("Version = %q, want %q", body.Version, "1.0.0-test")
	}
	if body.Checks["database"] != "pass" {
		t.Errorf("Checks[database] = %q, want %q", body.Checks["database"], "pass")
	}
	if body.Checks["preflight"] != "pass" {
		t.Errorf("Checks[preflight] = %q, want %q", body.Checks["preflight"], "pass")
	}
	if body.Checks["workflow"] != "pass" {
		t.Errorf("Checks[workflow] = %q, want %q", body.Checks["workflow"], "pass")
	}
}

func TestHandleReadyz_DatabaseFail(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t,
		func(p *Params) { p.DBPingFn = func(_ context.Context) error { return fmt.Errorf("db down") } },
		func(p *Params) { p.PreflightFn = func() bool { return true } },
		func(p *Params) { p.WorkflowLoadedFn = func() bool { return true } },
	)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body := decodeJSON[readyResponse](t, resp)
	if body.Status != "fail" {
		t.Errorf("Status = %q, want %q", body.Status, "fail")
	}
	if body.Checks["database"] != "fail" {
		t.Errorf("Checks[database] = %q, want %q", body.Checks["database"], "fail")
	}
	if body.Checks["preflight"] != "pass" {
		t.Errorf("Checks[preflight] = %q, want %q", body.Checks["preflight"], "pass")
	}
}

func TestHandleReadyz_PreflightFail(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t,
		func(p *Params) { p.DBPingFn = func(_ context.Context) error { return nil } },
		func(p *Params) { p.PreflightFn = func() bool { return false } },
		func(p *Params) { p.WorkflowLoadedFn = func() bool { return true } },
	)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body := decodeJSON[readyResponse](t, resp)
	if body.Status != "fail" {
		t.Errorf("Status = %q, want %q", body.Status, "fail")
	}
	if body.Checks["preflight"] != "fail" {
		t.Errorf("Checks[preflight] = %q, want %q", body.Checks["preflight"], "fail")
	}
	if body.Checks["database"] != "pass" {
		t.Errorf("Checks[database] = %q, want %q", body.Checks["database"], "pass")
	}
}

func TestHandleReadyz_WorkflowFail(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t,
		func(p *Params) { p.DBPingFn = func(_ context.Context) error { return nil } },
		func(p *Params) { p.PreflightFn = func() bool { return true } },
		func(p *Params) { p.WorkflowLoadedFn = func() bool { return false } },
	)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body := decodeJSON[readyResponse](t, resp)
	if body.Status != "fail" {
		t.Errorf("Status = %q, want %q", body.Status, "fail")
	}
	if body.Checks["workflow"] != "fail" {
		t.Errorf("Checks[workflow] = %q, want %q", body.Checks["workflow"], "fail")
	}
	if body.Checks["database"] != "pass" {
		t.Errorf("Checks[database] = %q, want %q", body.Checks["database"], "pass")
	}
}

func TestHandleReadyz_NilFunctions(t *testing.T) {
	t.Parallel()

	// When all check functions are nil, all checks default to "pass".
	ts := testHealthServer(t)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body := decodeJSON[readyResponse](t, resp)
	if body.Status != "pass" {
		t.Errorf("Status = %q, want %q", body.Status, "pass")
	}
	for _, check := range []string{"database", "preflight", "workflow"} {
		if body.Checks[check] != "pass" {
			t.Errorf("Checks[%s] = %q, want %q (nil func should default to pass)", check, body.Checks[check], "pass")
		}
	}
}

func TestHandleReadyz_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	ts := testHealthServer(t)

	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(method, ts.URL+"/readyz", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s /readyz: %v", method, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestLivezDuringDrain(t *testing.T) {
	t.Parallel()

	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
		Version:    "1.0.0-test",
		StartedAt:  time.Now(),
	})
	srv.SetDraining()

	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/livez")
	if err != nil {
		t.Fatalf("GET /livez: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body := decodeJSON[healthResponse](t, resp)
	if body.Status != "fail" {
		t.Errorf("Status = %q, want %q", body.Status, "fail")
	}
}

func TestReadyzDuringDrain(t *testing.T) {
	t.Parallel()

	srv := New(Params{
		SnapshotFn:       fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:        acceptingRefresh(),
		Logger:           slog.New(slog.DiscardHandler),
		Version:          "1.0.0-test",
		StartedAt:        time.Now(),
		DBPingFn:         func(_ context.Context) error { return nil },
		PreflightFn:      func() bool { return true },
		WorkflowLoadedFn: func() bool { return true },
	})
	srv.SetDraining()

	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	body := decodeJSON[readyResponse](t, resp)
	if body.Status != "fail" {
		t.Errorf("Status = %q, want %q", body.Status, "fail")
	}
	// During drain, all checks report "fail" as fail-fast.
	for _, check := range []string{"database", "preflight", "workflow"} {
		if body.Checks[check] != "fail" {
			t.Errorf("Checks[%s] = %q, want %q (draining should fail-fast all checks)", check, body.Checks[check], "fail")
		}
	}
}

func TestLivez200_Readyz503_DBDown(t *testing.T) {
	t.Parallel()

	// Livez should pass (process alive) while readyz fails (DB down).
	ts := testHealthServer(t,
		func(p *Params) { p.DBPingFn = func(_ context.Context) error { return fmt.Errorf("disk full") } },
		func(p *Params) { p.PreflightFn = func() bool { return true } },
		func(p *Params) { p.WorkflowLoadedFn = func() bool { return true } },
	)

	// Livez should be 200.
	liveResp, err := http.Get(ts.URL + "/livez")
	if err != nil {
		t.Fatalf("GET /livez: %v", err)
	}
	defer func() { _ = liveResp.Body.Close() }()

	if liveResp.StatusCode != http.StatusOK {
		t.Errorf("livez status = %d, want %d", liveResp.StatusCode, http.StatusOK)
	}

	// Readyz should be 503.
	readyResp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = readyResp.Body.Close() }()

	if readyResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d, want %d", readyResp.StatusCode, http.StatusServiceUnavailable)
	}

	body := decodeJSON[readyResponse](t, readyResp)
	if body.Checks["database"] != "fail" {
		t.Errorf("readyz Checks[database] = %q, want %q", body.Checks["database"], "fail")
	}
}

// TestStateRouteNullsUnmeasuredFigures locks the wire contract at the
// route rather than at the constructor: a figure no measurement
// produced serializes as JSON null, and a measured one serializes as a
// number - a genuine zero included, beside its true qualifier, never
// suppressed into a null alongside the unmeasured figures. It decodes
// into map[string]any rather than into runningEntryResponse so a wrong
// or missing struct tag cannot round-trip through the same types and
// hide the defect, which is the whole failure this change exists to
// prevent.
func TestStateRouteNullsUnmeasuredFigures(t *testing.T) {
	t.Parallel()

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Now().UTC(),
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID:             "measured",
				Identifier:          "MT-1",
				AgentInputTokens:    1200,
				AgentOutputTokens:   800,
				AgentTotalTokens:    2000,
				CacheReadTokens:     300,
				APIRequestCount:     12,
				RequestsByModel:     map[string]int{"model-a": 7, "model-b": 5},
				APIRequestsMeasured: true,
				UsageMeasured:       true,
				UsageArrival:        registry.UsageArrivalIncremental,
				UsageAttribution:    registry.UsageAttributionPerModel,
			},
			{
				IssueID:          "unmeasured",
				Identifier:       "MT-2",
				UsageArrival:     registry.UsageArrivalNone,
				UsageAttribution: registry.UsageAttributionNone,
			},
			{
				IssueID:             "measured-zero",
				Identifier:          "MT-3",
				AgentInputTokens:    0,
				AgentOutputTokens:   0,
				AgentTotalTokens:    0,
				CacheReadTokens:     0,
				APIRequestCount:     0,
				APIRequestsMeasured: true,
				UsageMeasured:       true,
				UsageArrival:        registry.UsageArrivalIncremental,
				UsageAttribution:    registry.UsageAttributionSessionTotal,
			},
		},
	}

	ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())

	resp, err := http.Get(ts.URL + "/api/v1/state")
	if err != nil {
		t.Fatalf("GET /api/v1/state: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := decodeJSON[map[string]any](t, resp)
	running, ok := body["running"].([]any)
	if !ok || len(running) != 3 {
		t.Fatalf("running = %#v, want three entries", body["running"])
	}

	byID := map[string]map[string]any{}
	for _, raw := range running {
		entry, entryOK := raw.(map[string]any)
		if !entryOK {
			t.Fatalf("running entry = %#v, want an object", raw)
		}
		id, idOK := entry["issue_id"].(string)
		if !idOK {
			t.Fatalf("running entry has no issue_id: %#v", entry)
		}
		byID[id] = entry
	}

	tokenKeys := []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens"}

	unmeasured := byID["unmeasured"]
	if got, present := unmeasured["api_request_count"]; !present || got != nil {
		t.Errorf("unmeasured api_request_count = %#v (present=%v), want present and null", got, present)
	}
	if got := unmeasured["api_requests_measured"]; got != false {
		t.Errorf("unmeasured api_requests_measured = %#v, want false", got)
	}
	unmeasuredTokens, ok := unmeasured["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("unmeasured tokens = %#v, want an object rather than a null", unmeasured["tokens"])
	}
	for _, key := range tokenKeys {
		if got, present := unmeasuredTokens[key]; !present || got != nil {
			t.Errorf("unmeasured tokens.%s = %#v (present=%v), want present and null", key, got, present)
		}
	}
	if got, present := unmeasured["requests_by_model"]; present {
		t.Errorf("unmeasured requests_by_model = %#v, want the key absent", got)
	}

	measured := byID["measured"]
	if got := measured["api_request_count"]; got != float64(12) {
		t.Errorf("measured api_request_count = %#v, want 12", got)
	}
	if got := measured["api_requests_measured"]; got != true {
		t.Errorf("measured api_requests_measured = %#v, want true", got)
	}
	if got := measured["tokens_measured"]; got != true {
		t.Errorf("measured tokens_measured = %#v, want true", got)
	}
	measuredTokens, ok := measured["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("measured tokens = %#v, want an object", measured["tokens"])
	}
	for key, want := range map[string]float64{
		"input_tokens": 1200, "output_tokens": 800, "total_tokens": 2000, "cache_read_tokens": 300,
	} {
		if got := measuredTokens[key]; got != want {
			t.Errorf("measured tokens.%s = %#v, want %v", key, got, want)
		}
	}
	if _, present := measured["requests_by_model"]; !present {
		t.Error("measured requests_by_model absent, want the breakdown present")
	}

	measuredZero := byID["measured-zero"]
	if got, present := measuredZero["api_request_count"]; !present || got != float64(0) {
		t.Errorf("measured-zero api_request_count = %#v (present=%v), want present and 0, not null", got, present)
	}
	if got := measuredZero["api_requests_measured"]; got != true {
		t.Errorf("measured-zero api_requests_measured = %#v, want true", got)
	}
	if got := measuredZero["tokens_measured"]; got != true {
		t.Errorf("measured-zero tokens_measured = %#v, want true", got)
	}
	measuredZeroTokens, ok := measuredZero["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("measured-zero tokens = %#v, want an object", measuredZero["tokens"])
	}
	for _, key := range tokenKeys {
		if got, present := measuredZeroTokens[key]; !present || got != float64(0) {
			t.Errorf("measured-zero tokens.%s = %#v (present=%v), want present and 0, not null", key, got, present)
		}
	}
}

type fakeRunHistory struct {
	rows        []RunHistoryEntry
	err         error
	totalsErr   error
	outcomes    map[string]IssueOutcome
	outcomesErr error
	passes      map[PassKey]PassActivity
	passesErr   error
	onQuery     func(RunHistoryQuery)
	groups      []IssueGroupKey
	onGroups    func(IssueGroupQuery)
	events      map[string][]orchestrator.RunEvent
}

func (f *fakeRunHistory) QueryRuns(_ context.Context, q RunHistoryQuery) ([]RunHistoryEntry, error) {
	if f.onQuery != nil {
		f.onQuery(q)
	}
	return f.rows, f.err
}

func (f *fakeRunHistory) QueryIssueGroups(_ context.Context, q IssueGroupQuery) ([]IssueGroupKey, error) {
	if f.onGroups != nil {
		f.onGroups(q)
	}
	return f.groups, f.err
}

func (f *fakeRunHistory) IssueTotals(context.Context, string) (IssueRunTotals, error) {
	return IssueRunTotals{}, f.totalsErr
}

func (f *fakeRunHistory) IssueOutcomes(context.Context, []string) (map[string]IssueOutcome, error) {
	return f.outcomes, f.outcomesErr
}

func (f *fakeRunHistory) PassActivities(context.Context, []PassRef) (map[PassKey]PassActivity, error) {
	return f.passes, f.passesErr
}

func (f *fakeRunHistory) RunEvents(_ context.Context, dispatchID string, newest int) ([]orchestrator.RunEvent, error) {
	events := f.events[dispatchID]
	if newest > 0 {
		events = events[max(len(events)-newest, 0):]
	}
	return events, f.err
}

func historyServer(t *testing.T, snap orchestrator.RuntimeSnapshotResult, history RunHistoryReader) *httptest.Server {
	t.Helper()
	return eventsServer(t, snap, history, nil)
}

func eventsServer(t *testing.T, snap orchestrator.RuntimeSnapshotResult, history RunHistoryReader, runEvents RunEventsFunc) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn:  fixedSnapshot(snap),
		RefreshFn:   acceptingRefresh(),
		Logger:      slog.New(slog.DiscardHandler),
		StartedAt:   time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC),
		RunHistory:  history,
		RunEventsFn: runEvents,
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func TestLastStageOf(t *testing.T) {
	t.Parallel()

	chains := [][]string{{"plan", "build", "review"}, {"fix", "review"}}

	tests := []struct {
		name   string
		runs   []RunHistoryEntry
		chains [][]string
		want   *lastStageResponse
	}{
		{"newest run decides", []RunHistoryEntry{{RuleName: "build"}, {RuleName: "plan"}}, chains, &lastStageResponse{Chain: chains[0], Rule: "build"}},
		{"reaction run skipped", []RunHistoryEntry{{RuleName: "fix", ReactionKind: "ci"}, {RuleName: "build"}}, chains, &lastStageResponse{Chain: chains[0], Rule: "build"}},
		{"ci result row never decides", []RunHistoryEntry{{Status: "ci_failed"}, {RuleName: "plan"}}, chains, &lastStageResponse{Chain: chains[0], Rule: "plan"}},
		{"previous picks the chain", []RunHistoryEntry{{RuleName: "review", StagePrevious: "fix"}}, chains, &lastStageResponse{Chain: chains[1], Rule: "review"}},
		{"no deciding run", []RunHistoryEntry{{Status: "ci_failed"}, {RuleName: "fix", ReactionKind: "ci"}}, chains, nil},
		{"empty chains", []RunHistoryEntry{{RuleName: "build"}}, nil, nil},
		{"rule on no chain", []RunHistoryEntry{{RuleName: "deploy"}}, chains, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := lastStageOf(tt.runs, tt.chains)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lastStageOf(%+v, %v) = %+v, want %+v", tt.runs, tt.chains, got, tt.want)
			}
		})
	}
}

type servedEvent struct {
	Seq      int64  `json:"seq"`
	Event    string `json:"event"`
	Message  string `json:"message"`
	Text     string `json:"text"`
	ToolLine string `json:"tool_line"`
}

type servedEventsBody struct {
	FirstSeq     *int64         `json:"first_seq"`
	Events       []servedEvent  `json:"events"`
	RecentEvents *[]servedEvent `json:"recent_events"`
	Error        struct {
		Code string `json:"code"`
	} `json:"error"`
}

func liveRunEvents(first, last int64) []orchestrator.RunEvent {
	at := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	var events []orchestrator.RunEvent
	for seq := first; seq <= last; seq++ {
		event := orchestrator.RunEvent{Seq: seq, At: at.Add(time.Duration(seq) * time.Second), Event: domain.EventNotification, Message: fmt.Sprintf("step %d", seq)}
		if seq%3 == 0 {
			event.Event, event.ToolName, event.DurationMS = domain.EventToolResult, "Bash", 2400
		}
		events = append(events, event)
	}
	return events
}

func TestRunEventsServed(t *testing.T) {
	t.Parallel()

	live := orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{
		{IssueID: "id-1", Identifier: "MT-1", DispatchID: "d-live", StartedAt: time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)},
	}}
	memory := func(dispatchID string) ([]orchestrator.RunEvent, bool) {
		return liveRunEvents(81, 200), dispatchID == "d-live"
	}
	noDispatchID := &fakeRunHistory{rows: []RunHistoryEntry{
		{ID: 3, IssueID: "id-2", Identifier: "MT-2", Status: "succeeded", DispatchID: new("")},
		{ID: 2, IssueID: "id-2", Identifier: "MT-2", Status: "succeeded"},
	}}

	tests := []struct {
		name         string
		snap         orchestrator.RuntimeSnapshotResult
		history      *fakeRunHistory
		path         string
		wantStatus   int
		wantCode     string
		wantFirstSeq int64
		wantCount    int
	}{
		{name: "route serves every memory event", snap: live, history: &fakeRunHistory{}, path: "/api/v1/runs/d-live/events", wantStatus: 200, wantFirstSeq: 81, wantCount: 120},
		{name: "recent_events is the newest 50", snap: live, history: &fakeRunHistory{}, path: "/api/v1/MT-1", wantStatus: 200, wantFirstSeq: 151, wantCount: 50},
		{name: "run without rows is not retained", snap: live, history: &fakeRunHistory{}, path: "/api/v1/runs/d-gone/events", wantStatus: 404, wantCode: "run_events_not_retained"},
		{name: "runs without a dispatch ID serve no recent_events", history: noDispatchID, path: "/api/v1/MT-2", wantStatus: 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ts := eventsServer(t, tt.snap, tt.history, memory)

			status, body := getJSON[servedEventsBody](t, ts, tt.path)

			if status != tt.wantStatus || body.Error.Code != tt.wantCode {
				t.Fatalf("GET %s = %d %q, want %d %q", tt.path, status, body.Error.Code, tt.wantStatus, tt.wantCode)
			}
			served := body.Events
			if body.RecentEvents != nil {
				served = *body.RecentEvents
			} else if strings.HasPrefix(tt.path, "/api/v1/MT-") {
				t.Fatalf("GET %s recent_events = null, want an array", tt.path)
			}
			if len(served) != tt.wantCount {
				t.Fatalf("GET %s served %d events, want %d", tt.path, len(served), tt.wantCount)
			}
			if body.FirstSeq != nil && *body.FirstSeq != tt.wantFirstSeq {
				t.Errorf("GET %s first_seq = %d, want %d", tt.path, *body.FirstSeq, tt.wantFirstSeq)
			}
			for i, event := range served {
				if want := tt.wantFirstSeq + int64(i); event.Seq != want {
					t.Errorf("GET %s event %d seq = %d, want %d", tt.path, i, event.Seq, want)
				}
				if event.Text == "" || event.Text == "Status not available" {
					t.Errorf("GET %s event %d (%s) text = %q, want a worded sentence", tt.path, i, event.Event, event.Text)
				}
			}
		})
	}
}

func TestRunEventsServeToolLine(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	retained := func(string) ([]orchestrator.RunEvent, bool) {
		return []orchestrator.RunEvent{
			{Seq: 1, At: at, Event: domain.EventToolResult, ToolName: "Bash", ToolKind: domain.ToolKindShell, ToolArgument: "go test ./..."},
		}, true
	}
	ts := eventsServer(t, orchestrator.RuntimeSnapshotResult{}, &fakeRunHistory{}, retained)

	status, body := getJSON[servedEventsBody](t, ts, "/api/v1/runs/d-1/events")

	if status != http.StatusOK || len(body.Events) != 1 {
		t.Fatalf("GET /api/v1/runs/d-1/events = %d with %d events, want 200 with 1", status, len(body.Events))
	}
	if got, want := body.Events[0].ToolLine, "go test ./..."; got != want {
		t.Errorf("GET /api/v1/runs/d-1/events tool_line = %q, want %q", got, want)
	}
}

func TestStateStallInstants(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	lastEvent := started.Add(10 * time.Second)
	tests := []struct {
		name          string
		timeoutMS     int
		lastEvent     time.Time
		wantLastEvent *time.Time
		wantQuiet     *time.Time
		wantStall     *time.Time
	}{
		{name: "before any event", timeoutMS: 60_000, wantQuiet: new(started.Add(30 * time.Second)), wantStall: new(started.Add(60 * time.Second))},
		{name: "after an event", timeoutMS: 60_000, lastEvent: lastEvent, wantLastEvent: &lastEvent, wantQuiet: new(lastEvent.Add(30 * time.Second)), wantStall: new(lastEvent.Add(60 * time.Second))},
		{name: "stall detection off", lastEvent: lastEvent, wantLastEvent: &lastEvent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := orchestrator.NewState(5000, 4, 0, nil, orchestrator.AgentTotals{})
			state.StallTimeoutMS = tt.timeoutMS
			state.Running["id-1"] = &orchestrator.RunningEntry{
				Issue: domain.Issue{ID: "id-1", Identifier: "MT-1"}, Identifier: "MT-1", StartedAt: started, LastAgentTimestamp: tt.lastEvent,
			}
			ts := historyServer(t, orchestrator.RuntimeSnapshot(state, started.Add(time.Hour)), nil)

			status, body := getJSON[stateResponse](t, ts, "/api/v1/state")

			if status != http.StatusOK || len(body.Running) != 1 {
				t.Fatalf("GET /api/v1/state = %d with %d running rows, want 200 with 1", status, len(body.Running))
			}
			row := body.Running[0]
			for name, pair := range map[string][2]*time.Time{
				"last_event_at": {row.LastEventAt, tt.wantLastEvent},
				"quiet_at":      {row.QuietAt, tt.wantQuiet},
				"stall_at":      {row.StallAt, tt.wantStall},
			} {
				if got, want := pair[0], pair[1]; (got == nil) != (want == nil) || got != nil && !got.Equal(*want) {
					t.Errorf("running row %s = %v, want %v", name, got, want)
				}
			}
		})
	}
}

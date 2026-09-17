package main

import "testing"

// TestStreaksAndDecideAction covers deriveStreaks and decideAction: the
// streak pseudocode, every row of the decision table, the five named
// state transitions, and the read-degradation and annotation properties
// layered on top of the table.
func TestStreaksAndDecideAction(t *testing.T) {
	t.Parallel()

	t.Run("deriveStreaks", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name            string
			current         sampleVerdict
			history         []historySample
			historyRead     bool
			wantFailStreak  int
			wantPassStreak  int
			wantCountedSize int
		}{
			{
				name:            "not_a_sample_resets_both_streaks",
				current:         verdictNotASample,
				history:         []historySample{{Conclusion: "failure"}, {Conclusion: "failure"}},
				historyRead:     true,
				wantFailStreak:  0,
				wantPassStreak:  0,
				wantCountedSize: 0,
			},
			{
				name:            "failing_streak_counts_matching_prior_failures",
				current:         verdictFailing,
				history:         []historySample{{Conclusion: "failure"}, {Conclusion: "timed_out"}, {Conclusion: "success"}},
				historyRead:     true,
				wantFailStreak:  3,
				wantPassStreak:  0,
				wantCountedSize: 2,
			},
			{
				name:            "failing_streak_stops_at_first_non_matching_verdict",
				current:         verdictFailing,
				history:         []historySample{{Conclusion: "success"}},
				historyRead:     true,
				wantFailStreak:  1,
				wantPassStreak:  0,
				wantCountedSize: 0,
			},
			{
				name:            "passing_streak_counts_matching_prior_passes",
				current:         verdictPassing,
				history:         []historySample{{Conclusion: "success"}, {Conclusion: "success"}, {Conclusion: "failure"}},
				historyRead:     true,
				wantFailStreak:  0,
				wantPassStreak:  3,
				wantCountedSize: 2,
			},
			{
				name:            "non_sample_conclusions_are_skipped_without_breaking_the_streak",
				current:         verdictFailing,
				history:         []historySample{{Conclusion: "cancelled"}, {Conclusion: "failure"}, {Conclusion: "skipped"}, {Conclusion: "failure"}},
				historyRead:     true,
				wantFailStreak:  3,
				wantPassStreak:  0,
				wantCountedSize: 2,
			},
			{
				name:            "history_read_false_treats_history_as_empty",
				current:         verdictFailing,
				history:         []historySample{{Conclusion: "failure"}, {Conclusion: "failure"}},
				historyRead:     false,
				wantFailStreak:  1,
				wantPassStreak:  0,
				wantCountedSize: 0,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got := deriveStreaks(tt.current, tt.history, tt.historyRead)

				if got.failStreak != tt.wantFailStreak {
					t.Errorf("deriveStreaks(%v, %v, %v).failStreak = %d, want %d", tt.current, tt.history, tt.historyRead, got.failStreak, tt.wantFailStreak)
				}
				if got.passStreak != tt.wantPassStreak {
					t.Errorf("deriveStreaks(%v, %v, %v).passStreak = %d, want %d", tt.current, tt.history, tt.historyRead, got.passStreak, tt.wantPassStreak)
				}
				if len(got.counted) != tt.wantCountedSize {
					t.Errorf("len(deriveStreaks(%v, %v, %v).counted) = %d, want %d", tt.current, tt.history, tt.historyRead, len(got.counted), tt.wantCountedSize)
				}
			})
		}
	})

	t.Run("verdictForConclusion", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			conclusion string
			want       sampleVerdict
		}{
			{"success", verdictPassing},
			{"failure", verdictFailing},
			{"timed_out", verdictFailing},
			{"cancelled", verdictNotASample},
			{"skipped", verdictNotASample},
			{"neutral", verdictNotASample},
			{"action_required", verdictNotASample},
			{"", verdictNotASample},
		}

		for _, tt := range tests {
			t.Run(tt.conclusion, func(t *testing.T) {
				t.Parallel()

				got := verdictForConclusion(tt.conclusion)
				if got != tt.want {
					t.Errorf("verdictForConclusion(%q) = %v, want %v", tt.conclusion, got, tt.want)
				}
			})
		}
	})

	t.Run("verdictForClassification", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			classification string
			want           sampleVerdict
		}{
			{"not_a_sample", verdictNotASample},
			{"pass", verdictPassing},
			{"contract", verdictFailing},
			{"environment", verdictFailing},
		}

		for _, tt := range tests {
			t.Run(tt.classification, func(t *testing.T) {
				t.Parallel()

				got := verdictForClassification(tt.classification)
				if got != tt.want {
					t.Errorf("verdictForClassification(%q) = %v, want %v", tt.classification, got, tt.want)
				}
			})
		}
	})

	t.Run("decideAction", func(t *testing.T) {
		t.Parallel()

		const failureThreshold = 2
		const passThreshold = 2

		tests := []struct {
			name           string
			incidentState  string
			current        sampleVerdict
			streaks        streakResult
			wantAction     string
			wantAnnotation bool // true means Annotation must be non-empty
		}{
			{
				name:          "any_incident_state_not_a_sample_is_none",
				incidentState: "open",
				current:       verdictNotASample,
				streaks:       streakResult{},
				wantAction:    "none",
			},
			{
				name:           "named_transition_failing_below_threshold_absent_is_none",
				incidentState:  "absent",
				current:        verdictFailing,
				streaks:        streakResult{failStreak: 1},
				wantAction:     "none",
				wantAnnotation: true,
			},
			{
				name:          "named_transition_failing_at_threshold_absent_is_open",
				incidentState: "absent",
				current:       verdictFailing,
				streaks:       streakResult{failStreak: failureThreshold},
				wantAction:    "open",
			},
			{
				name:          "absent_passing_is_none",
				incidentState: "absent",
				current:       verdictPassing,
				streaks:       streakResult{passStreak: 5},
				wantAction:    "none",
			},
			{
				name:          "open_failing_is_comment",
				incidentState: "open",
				current:       verdictFailing,
				streaks:       streakResult{failStreak: 1},
				wantAction:    "comment",
			},
			{
				name:           "named_transition_passing_below_threshold_open_is_comment",
				incidentState:  "open",
				current:        verdictPassing,
				streaks:        streakResult{passStreak: 1},
				wantAction:     "comment",
				wantAnnotation: false,
			},
			{
				name:          "named_transition_passing_at_threshold_open_is_close",
				incidentState: "open",
				current:       verdictPassing,
				streaks:       streakResult{passStreak: passThreshold},
				wantAction:    "close",
			},
			{
				name:           "closed_failing_below_threshold_is_none",
				incidentState:  "closed",
				current:        verdictFailing,
				streaks:        streakResult{failStreak: 1},
				wantAction:     "none",
				wantAnnotation: true,
			},
			{
				name:          "named_transition_failing_at_threshold_against_closed_is_reopen",
				incidentState: "closed",
				current:       verdictFailing,
				streaks:       streakResult{failStreak: failureThreshold},
				wantAction:    "reopen",
			},
			{
				name:          "closed_passing_is_none",
				incidentState: "closed",
				current:       verdictPassing,
				streaks:       streakResult{passStreak: 5},
				wantAction:    "none",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got := decideAction(tt.incidentState, 100, tt.current, tt.streaks, failureThreshold, passThreshold, true, true)

				if got.Action != tt.wantAction {
					t.Errorf("decideAction(%q, ..., %v, %v, ...) = %q, want %q", tt.incidentState, tt.current, tt.streaks, got.Action, tt.wantAction)
				}
				if hasAnnotation := got.Annotation != ""; hasAnnotation != tt.wantAnnotation {
					t.Errorf("decideAction(%q, ..., %v, %v, ...).Annotation = %q, want non-empty = %v", tt.incidentState, tt.current, tt.streaks, got.Annotation, tt.wantAnnotation)
				}
			})
		}
	})

	t.Run("close_unreachable_below_pass_threshold", func(t *testing.T) {
		t.Parallel()

		for threshold := 1; threshold <= 5; threshold++ {
			for passStreak := 0; passStreak < threshold; passStreak++ {
				got := decideAction("open", 1, verdictPassing, streakResult{passStreak: passStreak}, 1, threshold, true, true)
				if got.Action == "close" {
					t.Errorf("decideAction(%q, ..., passStreak=%d, passThreshold=%d, ...) = %q, want never %q below threshold", "open", passStreak, threshold, got.Action, "close")
				}
			}
		}
	})

	t.Run("reopen_carries_the_given_incident_number", func(t *testing.T) {
		t.Parallel()

		for _, number := range []int{1, 7, 999} {
			got := decideAction("closed", number, verdictFailing, streakResult{failStreak: 2}, 2, 2, true, true)
			if got.Action != "reopen" {
				t.Fatalf("decideAction(%q, %d, ...).Action = %q, want %q", "closed", number, got.Action, "reopen")
			}
			if got.IncidentNumber != number {
				t.Errorf("decideAction(%q, %d, ...).IncidentNumber = %d, want %d", "closed", number, got.IncidentNumber, number)
			}
		}
	})

	t.Run("open_unreachable_when_incident_read_false", func(t *testing.T) {
		t.Parallel()

		got := decideAction("absent", 0, verdictFailing, streakResult{failStreak: 2}, 2, 2, true, false)
		if got.Action != "none" {
			t.Errorf("decideAction(..., incidentRead=false) = %q, want %q", got.Action, "none")
		}
		if got.Annotation == "" {
			t.Error("decideAction(..., incidentRead=false).Annotation is empty, want a non-empty degraded-read annotation")
		}
	})

	t.Run("incident_read_false_does_not_affect_reopen_or_close", func(t *testing.T) {
		t.Parallel()

		reopened := decideAction("closed", 5, verdictFailing, streakResult{failStreak: 2}, 2, 2, true, false)
		if reopened.Action != "reopen" {
			t.Errorf("decideAction(%q, ..., incidentRead=false) = %q, want %q", "closed", reopened.Action, "reopen")
		}

		closed := decideAction("open", 5, verdictPassing, streakResult{passStreak: 2}, 2, 2, true, false)
		if closed.Action != "close" {
			t.Errorf("decideAction(%q, ..., incidentRead=false) = %q, want %q", "open", closed.Action, "close")
		}
	})

	t.Run("open_reopen_and_close_unreachable_when_history_read_false", func(t *testing.T) {
		t.Parallel()

		opened := decideAction("absent", 0, verdictFailing, streakResult{failStreak: 1}, 1, 1, false, true)
		if opened.Action != "none" {
			t.Errorf("decideAction(%q, ..., historyRead=false) = %q, want %q", "absent", opened.Action, "none")
		}
		if opened.Annotation == "" {
			t.Error("decideAction(absent, ..., historyRead=false).Annotation is empty, want a non-empty degraded-read annotation")
		}

		reopened := decideAction("closed", 5, verdictFailing, streakResult{failStreak: 1}, 1, 1, false, true)
		if reopened.Action != "none" {
			t.Errorf("decideAction(%q, ..., historyRead=false) = %q, want %q", "closed", reopened.Action, "none")
		}

		closed := decideAction("open", 5, verdictPassing, streakResult{passStreak: 1}, 1, 1, false, true)
		if closed.Action != "comment" {
			t.Errorf("decideAction(%q, ..., historyRead=false) = %q, want %q", "open", closed.Action, "comment")
		}
	})

	t.Run("history_read_false_does_not_affect_comment", func(t *testing.T) {
		t.Parallel()

		got := decideAction("open", 5, verdictFailing, streakResult{failStreak: 1}, 1, 1, false, true)
		if got.Action != "comment" {
			t.Errorf("decideAction(%q, ..., historyRead=false) = %q, want %q", "open", got.Action, "comment")
		}
		if got.Annotation != "" {
			t.Errorf("decideAction(%q, ..., historyRead=false).Annotation = %q, want empty", "open", got.Annotation)
		}
	})
}

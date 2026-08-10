package scmcore_test

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

func TestAggregateCIStatus(t *testing.T) {
	t.Parallel()

	run := func(status domain.CheckRunStatus, conclusion domain.CheckConclusion) domain.CheckRun {
		return domain.CheckRun{Status: status, Conclusion: conclusion}
	}

	tests := []struct {
		name string
		runs []domain.CheckRun
		want domain.CIStatus
	}{
		{
			name: "nil slice returns Pending",
			runs: nil,
			want: domain.CIStatusPending,
		},
		{
			name: "empty slice returns Pending",
			runs: []domain.CheckRun{},
			want: domain.CIStatusPending,
		},
		{
			name: "all success completed returns Passing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSuccess),
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSuccess),
			},
			want: domain.CIStatusPassing,
		},
		{
			name: "one failure returns Failing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionFailure),
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSuccess),
			},
			want: domain.CIStatusFailing,
		},
		{
			name: "any in_progress returns Pending",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSuccess),
				run(domain.CheckRunStatusInProgress, domain.CheckConclusionPending),
			},
			want: domain.CIStatusPending,
		},
		{
			name: "cancelled returns Failing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionCancelled),
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSuccess),
			},
			want: domain.CIStatusFailing,
		},
		{
			name: "timed_out returns Failing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionTimedOut),
			},
			want: domain.CIStatusFailing,
		},
		{
			name: "all neutral and skipped completed returns Passing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionNeutral),
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSkipped),
			},
			want: domain.CIStatusPassing,
		},
		{
			name: "failure and in_progress returns Failing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionFailure),
				run(domain.CheckRunStatusInProgress, domain.CheckConclusionPending),
			},
			want: domain.CIStatusFailing,
		},
		{
			name: "all completed with a neutral conclusion yields passing",
			runs: []domain.CheckRun{
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionNeutral),
				run(domain.CheckRunStatusCompleted, domain.CheckConclusionSuccess),
			},
			want: domain.CIStatusPassing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := scmcore.AggregateCIStatus(tt.runs)
			if got != tt.want {
				t.Errorf("AggregateCIStatus: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFailingCount(t *testing.T) {
	t.Parallel()

	run := func(conclusion domain.CheckConclusion) domain.CheckRun {
		return domain.CheckRun{Status: domain.CheckRunStatusCompleted, Conclusion: conclusion}
	}

	tests := []struct {
		name string
		runs []domain.CheckRun
		want int
	}{
		{"nil returns 0", nil, 0},
		{
			name: "all passing returns 0",
			runs: []domain.CheckRun{
				run(domain.CheckConclusionSuccess),
				run(domain.CheckConclusionNeutral),
				run(domain.CheckConclusionSkipped),
			},
			want: 0,
		},
		{
			name: "one failure",
			runs: []domain.CheckRun{
				run(domain.CheckConclusionFailure),
				run(domain.CheckConclusionSuccess),
			},
			want: 1,
		},
		{
			name: "counts failure timed_out and cancelled",
			runs: []domain.CheckRun{
				run(domain.CheckConclusionFailure),
				run(domain.CheckConclusionTimedOut),
				run(domain.CheckConclusionCancelled),
				run(domain.CheckConclusionSuccess),
			},
			want: 3,
		},
		{
			name: "empty slice returns 0",
			runs: []domain.CheckRun{},
			want: 0,
		},
		{
			name: "all passing yields zero",
			runs: []domain.CheckRun{
				run(domain.CheckConclusionSuccess),
				run(domain.CheckConclusionNeutral),
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := scmcore.FailingCount(tt.runs)
			if got != tt.want {
				t.Errorf("FailingCount: got %d, want %d", got, tt.want)
			}
		})
	}
}

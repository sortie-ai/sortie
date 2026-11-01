package sentence

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/storage"
)

func TestRunError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stored string
		want   string
	}{
		{
			name:   "absence of work",
			stored: storage.HandoffAbsenceErrorPrefix + "absence of work observed under observed policy",
			want:   "The agent finished without changing anything, so Sortie did not hand the issue off.",
		},
		{
			name:   "evidence not determinable",
			stored: storage.HandoffAbsenceErrorPrefix + "evidence not determinable under strict policy",
			want:   "Sortie could not tell whether the agent changed anything, so it did not hand the issue off.",
		},
		{
			name:   "worker exit wrapper",
			stored: orchestrator.WorkerExitedErrorPrefix + storage.HandoffAbsenceErrorPrefix + "absence of work observed under observed policy",
			want:   "The agent finished without changing anything, so Sortie did not hand the issue off.",
		},
		{
			name:   "multiline detail",
			stored: "agent turn 2: agent: port_exit: exit status 1\npanic: boom",
			want:   "The agent process ended unexpectedly: exit status 1\npanic: boom.",
		},
		{
			name:   "cancelled turn chain",
			stored: "agent turn 1: agent: turn_cancelled: turn cancelled: context canceled",
			want:   "Sortie stopped the session before the agent finished turn 1.",
		},
		{
			name:   "wrapped failed turn keeps the agent message",
			stored: orchestrator.WorkerExitedErrorPrefix + "agent turn 2: agent: turn_failed: the request failed: context deadline exceeded",
			want:   "The agent reported that turn 2 failed: the request failed.",
		},
		{
			name:   "session start failure keeps the cause",
			stored: `agent session start: agent: port_exit: failed to start subprocess: exec: "x": not found`,
			want:   `The agent process ended unexpectedly: failed to start subprocess: exec: "x": not found.`,
		},
		{
			name:   "cancelled worker stage",
			stored: "workspace preparation: prepare workspace for issue 7: context canceled",
			want:   "Sortie stopped the run while preparing the workspace.",
		},
		{
			name:   "legacy reason",
			stored: storage.HandoffAbsenceErrorPrefix + "no output",
			want:   "Sortie did not hand the issue off: no output.",
		},
		{name: "failed CI checks", stored: "CI checks failed on ref 3f9a2c1", want: "The CI checks failed on commit 3f9a2c1."},
		{name: "money ceiling", stored: "money ceiling reached: used $12.40 of $10.00", want: "Sortie stopped the run because the issue's estimated cost, $12.40, reached its limit of $10.00."},
		{name: "token ceiling", stored: "token ceiling reached: used 1500000 of 1000000 tokens", want: "Sortie stopped the run because the issue used 1.5M tokens, reaching its limit of 1M."},
		{name: "stall", stored: orchestrator.StallCancelError, want: "The agent sent no event within the stall timeout, so Sortie stopped the run."},
		{name: "shutdown", stored: orchestrator.ShutdownCancelError, want: "Sortie stopped the run because the service was shutting down."},
		{name: "terminal state", stored: orchestrator.TerminalCancelErrorPrefix + "Done", want: "Sortie stopped the run because the issue moved to Done, a final state."},
		{name: "inactive state", stored: orchestrator.InactiveCancelErrorPrefix + "Backlog", want: "Sortie stopped the run because the issue is no longer in an active state (now Backlog)."},
		{name: "inactive without a state name", stored: "issue left the active states", want: "Sortie stopped the run because the issue is no longer in an active state."},
		{name: "stems and a restating detail are dropped", stored: orchestrator.WorkerExitedErrorPrefix + "agent turn 1: agent: usage_limit: you've hit your usage limit", want: "The agent provider refused the run because the account's usage limit is reached."},
		{name: "stems inside a detail are dropped", stored: "worker panic: agent turn 1: agent: usage_limit: you've hit your usage limit", want: "Sortie hit an internal error while running the agent: you've hit your usage limit."},
		{name: "detail in another script is kept", stored: "agent turn 1: agent: usage_limit: 已达到使用上限", want: "The agent provider refused the run because the account's usage limit is reached: 已达到使用上限."},
		{name: "detail with a reset time is kept", stored: orchestrator.WorkerExitedErrorPrefix + "agent turn 1: agent: usage_limit: you've hit your usage limit, resets 3pm", want: "The agent provider refused the run because the account's usage limit is reached: you've hit your usage limit, resets 3pm."},
		{
			name:   "unprefixed error",
			stored: "worker error: agent crashed",
			want:   "worker error: agent crashed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RunError(tt.stored); got != tt.want {
				t.Errorf("RunError(%q) = %q, want %q", tt.stored, got, tt.want)
			}
		})
	}
}

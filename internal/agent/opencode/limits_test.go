//go:build unix

package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func startLimitsSession(t *testing.T, limitsAnswer, runFixture string) (domain.AgentAdapter, domain.Session, string) {
	t.Helper()
	dir := t.TempDir()
	callLog := filepath.Join(dir, "limits-calls")
	runPath, _ := filepath.Abs(filepath.Join("testdata", runFixture))
	answer := "exit 1"
	if limitsAnswer != "" {
		answer = "echo '" + limitsAnswer + "'"
	}
	script := writeOpenCodeScript(t, dir, `case "$1 $2" in
  "api experimental.usage.limits") echo called >> '`+callLog+`'; `+answer+`;;
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
cat '`+runPath+`'`)
	a, _ := NewOpenCodeAdapter()
	return a, mustStartSession(t, a, dir, script), callLog
}

func pulledWindows(events []domain.AgentEvent) []domain.UsageWindow {
	var windows []domain.UsageWindow
	for _, e := range events {
		windows = append(windows, e.UsageWindows...)
	}
	return windows
}

func TestRunTurn_PulledLimitWindows(t *testing.T) {
	t.Parallel()

	pct := func(v float64) *float64 { return &v }
	signedIn := []domain.UsageWindow{
		{Key: "rolling", Label: "Rolling limit", UsedPercent: pct(37.5), ResetsAt: time.UnixMilli(1793199600000).UTC()},
		{Key: "weekly", Label: "Weekly limit", Minutes: 10080, UsedPercent: pct(12), ResetsAt: time.UnixMilli(1793577600000).UTC()},
		{Key: "monthly", Label: "Monthly limit", UsedPercent: pct(100), Status: domain.UsageWindowExhausted, ResetsAt: time.UnixMilli(1794700800000).UTC()},
	}
	tests := []struct {
		name   string
		answer string
		turns  int
		want   []domain.UsageWindow
	}{
		{"signed in", string(loadFixture(t, "../usagelimits/testdata/api-limits-signed.json")), 1, signedIn},
		{"command fails", "", 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, session, callLog := startLimitsSession(t, tt.answer, "simple_turn.jsonl")

			var got []domain.UsageWindow
			for range tt.turns {
				events, _, _ := collectEvents(t, a, session, "work")
				got = append(got, pulledWindows(events)...)
			}

			for i := range got {
				got[i].ReportedAt = time.Time{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pulled usage windows = %+v, want %+v", got, tt.want)
			}
			calls, _ := os.ReadFile(callLog)
			if n := strings.Count(string(calls), "called"); n != 1 {
				t.Errorf("usage limits invocations over %d turns = %d, want 1", tt.turns, n)
			}
		})
	}
}

func TestRunTurn_FreeUsageLimitWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
		want    int
	}{
		{"free usage limit", "run2-free.jsonl", 1},
		{"other provider refusal", "free_tier_refusal.jsonl", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, session, _ := startLimitsSession(t, "", tt.fixture)

			events, result, _ := collectEvents(t, a, session, "work")

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			windows := pulledWindows(events)
			if len(windows) != tt.want {
				t.Fatalf("usage windows = %+v, want %d", windows, tt.want)
			}
			for _, w := range windows {
				if w.Label != "Free usage limit" || w.Status != domain.UsageWindowExhausted {
					t.Errorf("window = {Label %q, Status %q}, want {%q, %q}", w.Label, w.Status, "Free usage limit", domain.UsageWindowExhausted)
				}
				if got := w.ResetsAt.Sub(w.ReportedAt); got != time.Hour {
					t.Errorf("window ResetsAt - ReportedAt = %v, want %v", got, time.Hour)
				}
			}
		})
	}
}

func TestOpencodePull_ConnectionFailureClass(t *testing.T) {
	t.Parallel()

	for _, exitCode := range []int{255, 1} {
		t.Run("exit "+strconv.Itoa(exitCode), func(t *testing.T) {
			t.Parallel()

			command := agenttest.FakeRuntime(t, t.TempDir(), "ssh", agenttest.OutputScenario, agenttest.Output{ExitCode: exitCode})
			state := &sessionState{target: agentcore.LaunchTarget{Command: command, WorkspacePath: t.TempDir(), SSHHost: "user@stand-in-host", RemoteCommand: "opencode"}}

			_, err := opencodePull(state)(waittest.Context(t))

			if got, want := errors.Is(err, sshutil.ErrConnectionFailed), exitCode == 255; got != want {
				t.Errorf("errors.Is(opencodePull() error, ErrConnectionFailed) = %v, want %v for ssh exit %d (error: %v)", got, want, exitCode, err)
			}
		})
	}
}

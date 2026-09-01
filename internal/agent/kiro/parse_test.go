//go:build unix

package kiro

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestStripANSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "observed colorized prompt marker",
			input: "\x1b[38;5;141m> \x1b[0mPONG",
			want:  "> PONG",
		},
		{
			name:  "plain text passes through unchanged",
			input: "just an answer",
			want:  "just an answer",
		},
		{
			name:  "empty string stays empty",
			input: "",
			want:  "",
		},
		{
			name:  "reset-only sequence stripped",
			input: "\x1b[0mhello",
			want:  "hello",
		},
		{
			name:  "multiple escapes in one line",
			input: "\x1b[1m\x1b[31mERROR\x1b[0m: failed",
			want:  "ERROR: failed",
		},
		{
			name:  "text with no escapes but bracket chars",
			input: "result [ok] (done)",
			want:  "result [ok] (done)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestClassifyStderr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		lines          []string
		wantCredits    bool
		wantAuthFailed bool
	}{
		{
			name:           "credits trailer present",
			lines:          []string{"some warning", "▸ Credits: 0.01 • Time: 1s"},
			wantCredits:    true,
			wantAuthFailed: false,
		},
		{
			name:           "different credit and time values still match",
			lines:          []string{"▸ Credits: 0.05 • Time: 6s"},
			wantCredits:    true,
			wantAuthFailed: false,
		},
		{
			name:           "authentication failure present",
			lines:          []string{"Authentication failed. Your API key may be invalid or expired."},
			wantCredits:    false,
			wantAuthFailed: true,
		},
		{
			name:           "neither marker present",
			lines:          []string{"Failed to retrieve MCP settings; MCP functionality disabled"},
			wantCredits:    false,
			wantAuthFailed: false,
		},
		{
			name:           "empty lines",
			lines:          nil,
			wantCredits:    false,
			wantAuthFailed: false,
		},
		{
			name:           "credits trailer embedded mid-line",
			lines:          []string{"trailing noise ▸ Credits: 1.20 • Time: 12s done"},
			wantCredits:    true,
			wantAuthFailed: false,
		},
		{
			name:           "abandonment marker is not evidence",
			lines:          []string{procutil.AbandonedMarker},
			wantCredits:    false,
			wantAuthFailed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotCredits, gotAuthFailed := classifyStderr(tt.lines)
			if gotCredits != tt.wantCredits {
				t.Errorf("classifyStderr(%v) creditsSeen = %v, want %v", tt.lines, gotCredits, tt.wantCredits)
			}
			if gotAuthFailed != tt.wantAuthFailed {
				t.Errorf("classifyStderr(%v) authFailed = %v, want %v", tt.lines, gotAuthFailed, tt.wantAuthFailed)
			}
		})
	}
}

func TestParseLine_EmitsNotification(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	// stdout is the observed colorized marker; stderr carries the credits
	// trailer so the turn completes cleanly. Splitting requires a newline so
	// the skeleton's line scanner reads exactly one stdout line.
	bin := fakeChatScript(t, t.TempDir(), "\x1b[38;5;141m> \x1b[0mPONG\n", creditsLine, 0)
	adapter, session, state := mustStartSession(t, bin)

	events, _, err := runChatTurn(t, adapter, session, "ping")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var notifications []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventNotification {
			notifications = append(notifications, e)
		}
		if e.Type == domain.EventToolResult {
			t.Errorf("ParseLine emitted EventToolResult %+v, want only EventNotification for transcript lines", e)
		}
	}

	if len(notifications) != 1 {
		t.Fatalf("EventNotification count = %d, want 1; events = %+v", len(notifications), events)
	}
	n := notifications[0]
	if n.Message != "> PONG" {
		t.Errorf("notification.Message = %q, want stripped text %q", n.Message, "> PONG")
	}
	if n.AgentPID == "" {
		t.Error("notification.AgentPID is empty, want the subprocess PID")
	}

	if got := state.turnStdout.String(); !strings.Contains(got, "> PONG") {
		t.Errorf("state.turnStdout = %q, want it to contain the stripped line %q", got, "> PONG")
	}
}

// TestResumePath verifies the resume contract: --resume is absent on turn 1,
// becomes present on turn 2 only after a turn-1 OnFinalize that saw the credits
// trailer, and stays absent when turn 1 fails.
func TestResumePath(t *testing.T) {
	t.Run("resume enabled after a successful turn 1", func(t *testing.T) {
		// t.Setenv is incompatible with t.Parallel.
		setValidAPIKey(t)

		bin := fakeChatScript(t, t.TempDir(), "answer", creditsLine, 0)
		adapter, session, state := mustStartSession(t, bin)

		turn1Args := buildArgs(state, 1, "first", state.passthrough)
		assertNoToken(t, turn1Args, "--resume")

		_, result, err := runChatTurn(t, adapter, session, "first")
		if err != nil {
			t.Fatalf("turn 1 RunTurn() error = %v", err)
		}
		if result.ExitReason != domain.EventTurnCompleted {
			t.Fatalf("turn 1 ExitReason = %q, want completed", result.ExitReason)
		}
		if !state.resumeRequested {
			t.Fatal("state.resumeRequested = false after successful turn 1, want true")
		}

		turn2Args := buildArgs(state, 2, "second", state.passthrough)
		assertHasToken(t, turn2Args, "--resume")
	})

	t.Run("resume not enabled after a failed turn 1", func(t *testing.T) {
		// t.Setenv is incompatible with t.Parallel.
		setValidAPIKey(t)

		// Turn 1 fails (non-zero exit), so resumeRequested must stay false.
		bin := fakeChatScript(t, t.TempDir(), "", "boom\n", 1)
		adapter, session, state := mustStartSession(t, bin)

		_, result, _ := runChatTurn(t, adapter, session, "first")
		if result.ExitReason != domain.EventTurnFailed {
			t.Fatalf("turn 1 ExitReason = %q, want failed", result.ExitReason)
		}
		if state.resumeRequested {
			t.Fatal("state.resumeRequested = true after a failed turn 1, want false")
		}

		turn2Args := buildArgs(state, 2, "second", state.passthrough)
		assertNoToken(t, turn2Args, "--resume")
	})
}

// TestOnFinalize_MarkerOnlyStderrSelectsZeroWorkRow pins the disposition
// consequence of an abandoned stderr drain: marker-only stderr classifies
// as neither the credits trailer nor an authentication failure, so the
// evidence StartSession's OnFinalize closure builds from it selects the
// shared decision's zero-work row and reports turn_failed rather than a
// success. An end-to-end kiro turn is not exercised here because
// drainGrace is unexported and this package cannot inject a short bound.
func TestOnFinalize_MarkerOnlyStderrSelectsZeroWorkRow(t *testing.T) {
	t.Parallel()

	creditsSeen, authFailed := classifyStderr([]string{procutil.AbandonedMarker})
	if creditsSeen || authFailed {
		t.Fatalf("classifyStderr(%v) = (creditsSeen=%v, authFailed=%v), want (false, false)",
			procutil.AbandonedMarker, creditsSeen, authFailed)
	}

	// Mirrors the TurnEvidence StartSession's OnFinalize closure builds in
	// kiro.go: neither the success nor the auth-failure switch arm matches
	// when creditsSeen and authFailed are both false, so Terminal stays
	// TerminalAbsent and the shared table decides from ExitCode and Work.
	ev := agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkUnobservable,
		WorkDetail:   "no credits trailer on stderr",
	}

	got := agentcore.DecideTurn(ev)

	if got.Row != agentcore.RowZeroWork {
		t.Errorf("DecideTurn(%+v).Row = %v, want %v", ev, got.Row, agentcore.RowZeroWork)
	}
	if got.ExitReason != domain.EventTurnFailed {
		t.Errorf("DecideTurn(%+v).ExitReason = %q, want %q", ev, got.ExitReason, domain.EventTurnFailed)
	}
}

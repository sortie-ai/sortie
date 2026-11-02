package agentcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestResolveSessionStart(t *testing.T) {
	t.Parallel()

	const resumeID = "7d0f2b1e-3c4a-4d5e-8f60-a1b2c3d4e5f6"

	tests := []struct {
		name           string
		resumeID       string
		endBeforeStart bool
		endDuringProbe bool
		signal         ResumeSignal[struct{}]
		probeErr       *domain.AgentError
		wantKind       domain.AgentErrorKind
		wantCause      error
	}{
		{
			name: "empty resume id skips the probe",
		},
		{
			name:           "fresh start after the start context ended",
			endBeforeStart: true,
			wantKind:       domain.ErrTurnCancelled,
			wantCause:      context.Canceled,
		},
		{
			name:     "unknown verdict starts a new session",
			resumeID: resumeID,
			signal:   ResumeSignal[struct{}]{Verdict: ResumeUnknown, Cause: io.ErrUnexpectedEOF},
		},
		{
			name:      "lost ssh connection fails the start",
			resumeID:  resumeID,
			signal:    ResumeSignal[struct{}]{Cause: fmt.Errorf("list session file: %w", sshutil.ErrConnectionFailed)},
			wantKind:  domain.ErrPortExit,
			wantCause: sshutil.ErrConnectionFailed,
		},
		{
			name:           "carried verdict after the start context ended",
			resumeID:       resumeID,
			endDuringProbe: true,
			signal:         ResumeSignal[struct{}]{Verdict: ResumeCarried},
			wantKind:       domain.ErrTurnCancelled,
			wantCause:      context.Canceled,
		},
		{
			name:           "probe error after the start context ended",
			resumeID:       resumeID,
			endDuringProbe: true,
			probeErr:       &domain.AgentError{Kind: domain.ErrPortExit, Message: "probe connection ended"},
			wantKind:       domain.ErrTurnCancelled,
			wantCause:      context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			if tt.endBeforeStart {
				cancel()
			}
			defer cancel()
			calls := 0
			probe := ResumeProbeFunc[struct{}](func(context.Context, string) (ResumeSignal[struct{}], *domain.AgentError) {
				calls++
				if tt.endDuringProbe {
					cancel()
				}
				return tt.signal, tt.probeErr
			})

			start, err := ResolveSessionStart(ctx, tt.resumeID, probe, slog.New(slog.DiscardHandler))

			wantCalls := 1
			if tt.resumeID == "" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Errorf("ResolveSessionStart(%q) probe calls = %d, want %d", tt.resumeID, calls, wantCalls)
			}
			if start.ResumeID() != "" || start.CarriedHistory() {
				t.Errorf("ResolveSessionStart(%q) start = %q, want a new session", tt.resumeID, start.ResumeID())
			}
			if tt.wantKind == "" {
				if err != nil {
					t.Fatalf("ResolveSessionStart(%q) error = %v, want nil", tt.resumeID, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ResolveSessionStart(%q) error = nil, want kind %q", tt.resumeID, tt.wantKind)
			}
			if err.Kind != tt.wantKind {
				t.Errorf("ResolveSessionStart(%q) error kind = %q, want %q", tt.resumeID, err.Kind, tt.wantKind)
			}
			if !errors.Is(err, tt.wantCause) {
				t.Errorf("ResolveSessionStart(%q) error = %v, want it to wrap %v", tt.resumeID, err, tt.wantCause)
			}
		})
	}
}

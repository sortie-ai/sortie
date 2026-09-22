package agenttest

import (
	"context"
	"testing"
	"time"
)

type toolServerFakeReporter struct {
	errors []string
}

func (f *toolServerFakeReporter) Helper() {}

func (f *toolServerFakeReporter) Errorf(format string, args ...any) {
	f.errors = append(f.errors, format)
}

func TestAssertToolServerIdentity_Violating(t *testing.T) {
	t.Parallel()

	want := toolServerRecording{DispatchID: "d1", Workspace: "/ws"}

	blockUntilCancelled := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}

	tests := []struct {
		name          string
		runSession    func(ctx context.Context) error
		readRecording func() (toolServerRecording, bool)
	}{
		{
			name:       "recording carries a different value",
			runSession: blockUntilCancelled,
			readRecording: func() (toolServerRecording, bool) {
				return toolServerRecording{DispatchID: "wrong", Workspace: want.Workspace}, true
			},
		},
		{
			name:       "recording carries neither value",
			runSession: blockUntilCancelled,
			readRecording: func() (toolServerRecording, bool) {
				return toolServerRecording{}, true
			},
		},
		{
			name:       "runSession returns before any recording exists",
			runSession: func(context.Context) error { return nil },
			readRecording: func() (toolServerRecording, bool) {
				return toolServerRecording{}, false
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &toolServerFakeReporter{}
			assertToolServerIdentity(reporter, tt.runSession, want, tt.readRecording, 200*time.Millisecond, 10*time.Millisecond)

			if len(reporter.errors) == 0 {
				t.Errorf("assertToolServerIdentity(%s) recorded no failures, want at least one", tt.name)
			}
		})
	}
}

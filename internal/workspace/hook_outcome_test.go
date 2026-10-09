package workspace

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func stubHookStart(t *testing.T, fn func(context.Context, *exec.Cmd, procutil.CaptureParams) (*procutil.Capture, error)) {
	t.Helper()
	orig := startHookCapture
	t.Cleanup(func() { startHookCapture = orig })
	startHookCapture = fn
}

func TestRunHook_ResumeFailureAfterTheDeadline(t *testing.T) {
	tests := []struct {
		name      string
		cancelled bool
		wantOp    string
	}{
		{name: "start failed without a cancellation", cancelled: false, wantOp: "start"},
		{name: "start refused by a cancellation", cancelled: true, wantOp: "timeout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			stubHookStart(t, func(context.Context, *exec.Cmd, procutil.CaptureParams) (*procutil.Capture, error) {
				cancel()
				return nil, &procutil.StartError{
					Stage:     procutil.StageProcessResume,
					Err:       errors.New("injected resume failure"),
					Cancelled: tt.cancelled,
				}
			})

			_, err := RunHook(ctx, HookParams{
				Script:    "exit 0",
				Dir:       t.TempDir(),
				Env:       map[string]string{},
				TimeoutMS: 5000,
			})

			he := requireHookError(t, err)
			if he.Op != tt.wantOp {
				t.Errorf("HookError.Op = %q, want %q", he.Op, tt.wantOp)
			}
		})
	}
}

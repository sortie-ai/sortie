package agentcore

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestConnectionFailedError(t *testing.T) {
	t.Parallel()

	err := ConnectionFailedError()

	if err.Kind != domain.ErrPortExit || !errors.Is(err, sshutil.ErrConnectionFailed) {
		t.Errorf("ConnectionFailedError() = {Kind: %q, Err: %v}, want port_exit wrapping sshutil.ErrConnectionFailed", err.Kind, err.Err)
	}
}

func TestReaperConnectionFailed(t *testing.T) {
	t.Parallel()

	exitedReaper := func(t *testing.T, code int) *procutil.Reaper {
		t.Helper()
		cmd := exec.Command(agenttest.FakeRuntime(t, t.TempDir(), "agent", agenttest.OutputScenario, agenttest.Output{ExitCode: code}))
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}
		r := procutil.StartReaper(cmd, nil)
		<-r.Done()
		return r
	}

	tests := []struct {
		name     string
		remote   bool
		exitCode int
		nilReap  bool
		want     bool
	}{
		{name: "remote exit 255", remote: true, exitCode: 255, want: true},
		{name: "remote exit 1", remote: true, exitCode: 1, want: false},
		{name: "local exit 255", remote: false, exitCode: 255, want: false},
		{name: "remote nil reaper", remote: true, nilReap: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var r *procutil.Reaper
			if !tt.nilReap {
				r = exitedReaper(t, tt.exitCode)
			}
			if got := ReaperConnectionFailed(tt.remote, r, time.Second); got != tt.want {
				t.Errorf("ReaperConnectionFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}

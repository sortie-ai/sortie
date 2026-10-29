//go:build unix

package agentcore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const hostVar = "SORTIE_TEST_HOST_VAR"

type hostRecord struct {
	Args []string
	Dir  string
	Var  string
}

func init() {
	scenarios["hostRecorder"] = func(args []string, _ json.RawMessage) int {
		dir, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(hostRecord{args, dir, os.Getenv(hostVar)})
		return 0
	}
}

func recordHostCommand(t *testing.T, target LaunchTarget, env []string, cmd HostCommand) hostRecord {
	t.Helper()
	out, err := target.RunHostCommand(waittest.Context(t), env, cmd)
	var rec hostRecord
	if err != nil || json.Unmarshal(out, &rec) != nil {
		t.Fatalf("RunHostCommand(%+v) = %q, %v, want a hostRecord", cmd, out, err)
	}
	rec.Dir = realPath(rec.Dir)
	return rec
}

func realPath(dir string) string {
	resolved, _ := filepath.EvalSymlinks(dir)
	return resolved
}

func TestRunHostCommand_InSessionLaunchWords(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	target := LaunchTarget{Command: agenttest.FakeRuntime(t, t.TempDir(), "npx", "hostRecorder", nil), Args: []string{"-y", "pkg", "acp"}, WorkspacePath: ws}

	got := recordHostCommand(t, target, []string{hostVar + "=from-env"}, HostCommand{LaunchWords: 3, Args: []string{"session", "export"}, InSession: true})

	if want := (hostRecord{[]string{"-y", "pkg", "session", "export"}, realPath(ws), "from-env"}); !reflect.DeepEqual(got, want) {
		t.Errorf("RunHostCommand(LaunchWords 3, InSession) recorded %+v, want %+v", got, want)
	}
}

func TestRunHostCommand_ZeroOptionsKeepSortieProcess(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv(hostVar, "from-process")
	tool := agenttest.FakeRuntime(t, t.TempDir(), "tool", "hostRecorder", nil)

	got := recordHostCommand(t, LaunchTarget{WorkspacePath: t.TempDir()}, []string{hostVar + "=from-env"}, HostCommand{Name: tool, Args: []string{"a"}})

	if want := (hostRecord{[]string{"a"}, realPath(cwd), "from-process"}); !reflect.DeepEqual(got, want) {
		t.Errorf("RunHostCommand(zero options) recorded %+v, want %+v", got, want)
	}
}

func TestRunHostCommand_OutputAndExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		output     agenttest.Output
		limit      int64
		wantErr    bool
		wantFailed bool
	}{
		{"output at the limit", agenttest.Output{Stdout: "abcd"}, 4, false, false},
		{"output above the limit", agenttest.Output{Stdout: "abcde"}, 4, true, false},
		{"non-zero exit", agenttest.Output{ExitCode: 3}, 0, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tool := agenttest.FakeRuntime(t, t.TempDir(), "tool", agenttest.OutputScenario, tt.output)

			got, err := LaunchTarget{}.RunHostCommand(waittest.Context(t), nil, HostCommand{Name: tool, Limit: tt.limit})

			if (err != nil) != tt.wantErr || errors.Is(err, ErrHostCommandFailed) != tt.wantFailed {
				t.Errorf("RunHostCommand(limit %d) = %q, %v, want error %t wrapping ErrHostCommandFailed %t", tt.limit, got, err, tt.wantErr, tt.wantFailed)
			}
		})
	}
}

func TestRunHostCommand_RemoteLaunchWordsKeepPrefixAsWritten(t *testing.T) {
	t.Parallel()

	ssh := agenttest.FakeRuntime(t, t.TempDir(), "ssh", "hostRecorder", nil)
	target := LaunchTarget{Command: ssh, SSHHost: "worker", WorkspacePath: t.TempDir(), RemoteCommand: "FOO=$HOME ~/bin/opencode acp"}

	got := recordHostCommand(t, target, nil, HostCommand{LaunchWords: 2, Args: []string{"session", "a b"}, InSession: true})

	want := "{ FOO=$HOME ~/bin/opencode 'session' 'a b'\n}"
	if line := got.Args[len(got.Args)-1]; !strings.Contains(line, want) || strings.Contains(line, "acp") {
		t.Errorf("RunHostCommand(LaunchWords 2) remote line = %q, want it to hold %q and no acp", line, want)
	}
}

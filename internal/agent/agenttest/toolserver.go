package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RecordedEnvScenario names the built-in [Scenario], alongside
// [OutputScenario], that records the current value of each name in a
// [RecordedEnv]'s Names from its own process environment, publishes them
// to Path, and then blocks until killed.
const RecordedEnvScenario = "agenttest.recordedenv"

// RecordedEnv parameterizes [RecordedEnvScenario].
type RecordedEnv struct {
	Path  string
	Names []string
}

// Run publishes the current value of each name in Names to Path as a
// JSON object, then blocks until the process is killed. Publication
// writes a fresh temporary file and renames it into place, so a reader
// polling Path never observes a partial write.
func (p RecordedEnv) Run() int {
	values := make(map[string]string, len(p.Names))
	for _, name := range p.Names {
		values[name] = os.Getenv(name)
	}
	data, err := json.Marshal(values)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: encode recorded environment: %v\n", err)
		return 2
	}
	tmp := p.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: write recorded environment: %v\n", err)
		return 2
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: publish recorded environment: %v\n", err)
		return 2
	}
	Hang()
	return 0
}

func runRecordedEnv(_ []string, params RecordedEnv) int {
	return params.Run()
}

const (
	toolServerIdentityBound = 60 * time.Second
	toolServerIdentityPoll  = 50 * time.Millisecond
)

// toolServerRecording is the dispatch identity a recording tool server
// observed in its own environment.
type toolServerRecording struct {
	DispatchID string
	Workspace  string
}

// AssertToolServerIdentity fails t unless a session runSession starts
// hands its tool server process the dispatch identity a generated MCP
// configuration declares, unchanged.
//
// It builds a workspace and a generated-shape MCP configuration under
// t.TempDir() declaring one stdio server, "sortie-tools", whose command
// is a recording tool server launched through [FakeRuntime] with
// [RecordedEnvScenario]. It calls runSession with the workspace and
// configuration paths, cancels its context once the recording exists,
// and fails t unless the recorded SORTIE_DISPATCH_ID and SORTIE_WORKSPACE
// equal the configured ones. It also fails t when runSession returns
// before any recording exists, or when none appears within 60 seconds.
func AssertToolServerIdentity(t *testing.T, runSession func(ctx context.Context, workspacePath, mcpConfigPath string) error) {
	t.Helper()

	dir := t.TempDir()
	workspacePath := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspacePath, 0o750); err != nil {
		t.Fatalf("AssertToolServerIdentity: create workspace: %v", err)
	}

	want := toolServerRecording{
		DispatchID: "assert-tool-server-identity",
		Workspace:  workspacePath,
	}

	recordingPath := filepath.Join(dir, "recorded.json")
	runtimePath := FakeRuntime(t, dir, "tool-server-recorder", RecordedEnvScenario, RecordedEnv{
		Path:  recordingPath,
		Names: []string{"SORTIE_DISPATCH_ID", "SORTIE_WORKSPACE"},
	})
	mcpConfigPath := writeGeneratedToolServerConfig(t, dir, runtimePath, want)

	assertToolServerIdentity(t,
		func(ctx context.Context) error { return runSession(ctx, workspacePath, mcpConfigPath) },
		want,
		func() (toolServerRecording, bool) { return readToolServerRecording(recordingPath) },
		toolServerIdentityBound, toolServerIdentityPoll)
}

// writeGeneratedToolServerConfig writes, under dir, an MCP configuration
// of the shape the worker generates: one stdio server, "sortie-tools",
// launching command with an env block carrying want's dispatch identity.
// It returns the configuration's path.
func writeGeneratedToolServerConfig(t *testing.T, dir, command string, want toolServerRecording) string {
	t.Helper()

	config := map[string]any{
		"mcpServers": map[string]any{
			"sortie-tools": map[string]any{
				"type":    "stdio",
				"command": command,
				"env": map[string]string{
					"SORTIE_DISPATCH_ID": want.DispatchID,
					"SORTIE_WORKSPACE":   want.Workspace,
				},
			},
		},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatalf("AssertToolServerIdentity: encode MCP config: %v", err)
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("AssertToolServerIdentity: write MCP config: %v", err)
	}
	return path
}

// readToolServerRecording reads the recording [RecordedEnv.Run] publishes
// at path, reporting ok false while it does not yet exist or does not yet
// parse.
func readToolServerRecording(path string) (toolServerRecording, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is this package's own t.TempDir fixture.
	if err != nil {
		return toolServerRecording{}, false
	}
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return toolServerRecording{}, false
	}
	return toolServerRecording{
		DispatchID: values["SORTIE_DISPATCH_ID"],
		Workspace:  values["SORTIE_WORKSPACE"],
	}, true
}

// toolServerReporter is the minimal reporting surface
// assertToolServerIdentity needs; [*testing.T] satisfies it.
type toolServerReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// assertToolServerIdentity fails t unless readRecording reports a value
// equal to want no later than the moment runSession returns or bound
// elapses, whichever comes first.
func assertToolServerIdentity(
	t toolServerReporter,
	runSession func(ctx context.Context) error,
	want toolServerRecording,
	readRecording func() (toolServerRecording, bool),
	bound, poll time.Duration,
) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionDone := make(chan error, 1)
	go func() { sessionDone <- runSession(ctx) }()

	deadline := time.Now().Add(bound)
	sessionReturned := false
	var sessionErr error

	for {
		if rec, ok := readRecording(); ok {
			cancel()
			if !sessionReturned {
				<-sessionDone
			}
			if rec != want {
				t.Errorf("AssertToolServerIdentity: recorded identity = %+v, want %+v", rec, want)
			}
			return
		}
		if sessionReturned {
			t.Errorf("AssertToolServerIdentity: no recording observed by the time runSession returned (error = %v)", sessionErr)
			return
		}
		if !time.Now().Before(deadline) {
			cancel()
			sessionErr = <-sessionDone
			if rec, ok := readRecording(); ok {
				if rec != want {
					t.Errorf("AssertToolServerIdentity: recorded identity = %+v, want %+v", rec, want)
				}
				return
			}
			t.Errorf("AssertToolServerIdentity: no recording observed within %s (runSession error = %v)", bound, sessionErr)
			return
		}
		select {
		case sessionErr = <-sessionDone:
			sessionReturned = true
		case <-time.After(poll):
		}
	}
}

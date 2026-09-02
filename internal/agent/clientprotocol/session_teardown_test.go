//go:build unix

package clientprotocol

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// teardownParkedOptionSize is the selected permission option's
// identifier length: at least four mebibytes, so no pipe buffer can
// hold the reply that echoes it back.
const teardownParkedOptionSize = 4*1024*1024 + 4096

// teardownParkedScriptTemplate is the fake agent for the parked
// teardown scenario. Substituting __DIR__ and __SIZE__ gives a script that:
//
//  1. Detaches two helper processes into their own session before
//     doing anything else, escaping this script's own process group:
//     one reads a small prefix of whatever arrives on its standard
//     input and then stops reading while holding that pipe's read end
//     open, and one simply holds its standard output's write end
//     open. Each explicitly redirects the file descriptor it does not
//     represent, and each is released from an ordinary shell "&"
//     background job's own implicit /dev/null substitution for
//     standard input by duplicating the saved descriptor explicitly
//     rather than leaving it to inherit fd 0 unredirected.
//  2. Writes a session/request_permission request whose selected
//     option carries an identifier of at least four mebibytes,
//     followed by a second, distinct request the adapter never
//     answers before teardown begins.
//  3. Closes its own copies of both piped descriptors and idles.
const teardownParkedScriptTemplate = `dir='__DIR__'
exec 3<&0
exec 4>&1
setsid sh -c 'echo $$ >"'"$dir"'/reader.pid"; dd bs=65536 count=1 of=/dev/null 2>/dev/null; touch "'"$dir"'/reader.done"; sleep 600' <&3 3<&- 4>&- >/dev/null 2>/dev/null &
setsid sh -c 'echo $$ >"'"$dir"'/writer.pid"; sleep 600' >&4 4>&- 3<&- <&- 2>/dev/null &
exec 3<&-
exec 4>&-
exec <&-
huge=$(head -c __SIZE__ /dev/zero | tr '\0' 'x')
printf '{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"%s"}],"toolCall":{"toolCallId":"tc-1","title":"work"}}}\n' "$huge"
printf '{"jsonrpc":"2.0","id":2,"method":"fs/read_text_file","params":{}}\n'
exec >&-
sleep 600
`

// parkedTeardownFixture bundles a session backed by a real subprocess
// and real OS pipes, already parked mid-write on a permission reply
// no pipe buffer can hold, with release cleanly ending the two
// detached helper processes that hold the pipe ends open.
type parkedTeardownFixture struct {
	state   *sessionState
	release func()
}

// pipeWiring selects how newParkedTeardownSession connects the
// subprocess's standard input and standard output.
type pipeWiring int

const (
	// pipeWiringAutoClosing uses cmd.StdinPipe and cmd.StdoutPipe,
	// exactly as startSession does. Their documented side effect,
	// that Cmd.Wait closes both once the process has been reaped, is
	// production behavior rather than a test artifact: this
	// fixture's own always-running reaper goroutine, mirroring
	// StartSession's, races that automatic close against teardown's
	// own closeStdin and closeStdout, and wins once
	// kill_process_group has actually reaped the process. That is
	// the right wiring for the bound case and for the first control,
	// where kill_process_group either succeeds immediately or never
	// runs at all, so the race never has a chance to matter.
	pipeWiringAutoClosing pipeWiring = iota

	// pipeWiringManual connects a plain os.Pipe pair directly to
	// cmd.Stdin and cmd.Stdout, which Cmd.Wait does not know to
	// close. The second control names omitting the standard-output
	// close as its own defect; with pipeWiringAutoClosing, killing
	// the process group first reaps the process, and Cmd.Wait's own
	// unrelated auto-close of the StdoutPipe races in and papers
	// over the very omission that control means to expose. Manual
	// wiring removes that confound so the control tests only the
	// property it names.
	pipeWiringManual
)

// newParkedTeardownSession launches the scenario's fake agent as a
// real subprocess, wires a session to it exactly as StartSession
// would (skipping the handshake calls, which this scenario has no use
// for), and waits for genuine evidence that the reply write is
// parked: the reader helper's own completion marker, written only
// once it has consumed its one bounded read. That evidence, combined
// with the reply being many times larger than any pipe buffer, is
// what makes the park a property of the setup rather than a timing
// assumption; the wait loop itself is bounded polling for that
// marker, not a sleep standing in for the park.
func newParkedTeardownSession(t *testing.T, wiring pipeWiring) *parkedTeardownFixture {
	t.Helper()
	requireSetsid(t)

	dir := t.TempDir()
	script := strings.NewReplacer(
		"__DIR__", dir,
		"__SIZE__", strconv.Itoa(teardownParkedOptionSize),
	).Replace(teardownParkedScriptTemplate)
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)

	var stdinCloser io.WriteCloser
	var stdoutCloser io.ReadCloser
	var childSideCloser func()
	switch wiring {
	case pipeWiringManual:
		stdinCloser, stdoutCloser, childSideCloser = wireManualPipes(t, cmd)
	default:
		var err error
		stdinCloser, err = cmd.StdinPipe()
		if err != nil {
			t.Fatalf("StdinPipe: %v", err)
		}
		stdoutCloser, err = cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("StdoutPipe: %v", err)
		}
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if childSideCloser != nil {
		// The child's copies were dup2'd onto its own descriptors
		// during Start; the parent's references to them are now
		// redundant, matching how cmd.StdinPipe and cmd.StdoutPipe
		// close their own child-side ends once Start returns.
		childSideCloser()
	}

	state := &sessionState{
		pid:          cmd.Process.Pid,
		stdinCloser:  stdinCloser,
		stdoutCloser: stdoutCloser,
		waitCh:       make(chan struct{}),
		itemCh:       make(chan pumpItem, pumpChannelCapacity),
		stopCh:       make(chan struct{}),
		pumpDone:     make(chan struct{}),
		logger:       discardLogger(),
		agentConfig:  domain.AgentConfig{ReadTimeoutMS: 60000},
		caps:         newCapabilityRecord(false),
	}
	state.stderrCollector = procutil.NewStderrCollector(stderrPipe, state.logger)

	go func() {
		cmd.Wait()                                 //nolint:errcheck,gosec // best-effort reap, mirroring startSession's own reaper
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)
		close(state.waitCh)
	}()

	state.conn = jsonrpc.NewConn(stdinCloser, stdoutCloser, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	markSessionKnown(state)

	waitForFile(t, filepath.Join(dir, "reader.done"), awaitTimeout)

	release := sync.OnceFunc(func() {
		// Each helper is its own session and process group leader, so
		// killing only the recorded pid leaves its own "sleep 600"
		// child (a separate process in the same group) holding the
		// pipe end open; the negative pid signals the whole group.
		killHelperGroup(filepath.Join(dir, "reader.pid"))
		killHelperGroup(filepath.Join(dir, "writer.pid"))
	})
	t.Cleanup(release)

	return &parkedTeardownFixture{state: state, release: release}
}

// wireManualPipes builds a plain os.Pipe pair for each direction and
// assigns the child's end directly to cmd.Stdin and cmd.Stdout,
// returning the parent-side ends the caller keeps and a closer for
// the child-side ends the caller invokes once Start has dup2'd them
// onto the child's own descriptors. Unlike cmd.StdinPipe and
// cmd.StdoutPipe, this registers nothing for Cmd.Wait to close on the
// caller's behalf.
func wireManualPipes(t *testing.T, cmd *exec.Cmd) (stdin io.WriteCloser, stdout io.ReadCloser, closeChildSide func()) {
	t.Helper()

	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdin): %v", err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe (stdout): %v", err)
	}

	cmd.Stdin = inRead
	cmd.Stdout = outWrite

	t.Cleanup(func() {
		_ = inWrite.Close()
		_ = outRead.Close()
	})

	return inWrite, outRead, func() {
		_ = inRead.Close()
		_ = outWrite.Close()
	}
}

// requireSetsid skips t cleanly when the setsid binary is not on
// PATH: the scenario needs it to detach the reader and writer helper
// processes into their own session, escaping the process group teardown
// terminates, which is what lets a descendant go on holding a pipe end
// open past that termination. setsid ships with util-linux and is not
// present on macOS, so this keeps the suite from failing there for a
// reason unrelated to what it tests.
func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("skipping: setsid not found on PATH: %v", err)
	}
}

// waitForFile polls for path to exist, failing t if timeout elapses
// first. This is a bounded wait for a concrete condition the fake
// agent script itself establishes, not a sleep standing in for the
// park it evidences.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to appear", path)
}

// killHelperGroup reads a pid from pidFile and signals its whole
// process group, tolerating a missing or already-gone file.
func killHelperGroup(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// assertSessionGoroutinesExited fails t unless the session's pump,
// its connection's reader, and its subprocess reaper have all
// exited: the leak check that fails when a session's goroutines
// outlive StopSession.
func assertSessionGoroutinesExited(t *testing.T, state *sessionState) {
	t.Helper()

	select {
	case <-state.pumpDone:
	default:
		t.Error("session leak: the pump goroutine is still running after teardown returned")
	}
	select {
	case <-state.conn.Done():
	default:
		t.Error("session leak: the connection's reader goroutine is still running after teardown returned")
	}
	select {
	case <-state.waitCh:
	default:
		t.Error("session leak: the subprocess reaper goroutine is still running after teardown returned")
	}
}

// TestStopSessionTeardownOrder pins teardown's step order by its
// bound: under the three parking conditions this package's scenario
// establishes, plus an agent-initiated request left unanswered,
// StopSession must still return within 2*procutil.DefaultDrainGrace,
// and leave no goroutine of the session running afterward.
func TestStopSessionTeardownOrder(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t, pipeWiringAutoClosing)

	start := time.Now()
	err := stopSession(context.Background(), fakeSession(fx.state))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}
	if bound := 2 * procutil.DefaultDrainGrace; elapsed >= bound {
		t.Errorf("stopSession() took %v, want under %v", elapsed, bound)
	}

	assertSessionGoroutinesExited(t, fx.state)
}

// runTeardownControl runs steps (a deliberately wrong variant of
// defaultTeardownOrder's slice) against fx, asserts it has not
// returned when a one-second observation window expires, releases
// the park, and asserts it returns afterward and leaves no goroutine
// running. No sleep stands in for the park: the wait before release
// is a fixed observation window whose whole point is that nothing
// should have happened yet, not a wait for a condition.
func runTeardownControl(t *testing.T, fx *parkedTeardownFixture, steps []teardownStep) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		runTeardown(fx.state, steps)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("runTeardown() returned before the one-second observation window expired, want it still parked")
	case <-time.After(1 * time.Second):
	}

	fx.release()

	select {
	case <-done:
	case <-time.After(2 * procutil.DefaultDrainGrace):
		t.Fatal("runTeardown() did not return after releasing the park")
	}

	assertSessionGoroutinesExited(t, fx.state)
}

// TestStopSessionTeardownControlConnectionBeforeKill is the first
// negative control: closing the connection before the process group
// is terminated waits on the write mutex the parked write holds, so
// teardown must not return until the park is released from outside.
func TestStopSessionTeardownControlConnectionBeforeKill(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t, pipeWiringAutoClosing)
	runTeardownControl(t, fx, []teardownStep{
		{name: "answer_open", run: signalAnswerOpen},
		{name: "close_connection", run: closeConnection},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_stdout", run: closeStdout},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap},
	})
}

// TestStopSessionTeardownControlNoStdoutClose is the second negative
// control: omitting the standard-output close leaves the connection's
// reader scanning a stream nothing further will ever close on its
// own, so stop_pump's wait for the pump to exit must not return until
// the park is released from outside.
func TestStopSessionTeardownControlNoStdoutClose(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t, pipeWiringManual)
	runTeardownControl(t, fx, []teardownStep{
		{name: "answer_open", run: signalAnswerOpen},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap},
	})
}

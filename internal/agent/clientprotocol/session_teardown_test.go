//go:build unix

package clientprotocol

import (
	"bytes"
	"context"
	"log/slog"
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

// teardownReturnOverhead is the margin a return-time assertion adds on
// top of teardown's specified ceiling. The ceiling bounds the sum of
// the steps that wait; the steps that do not wait still cost some
// scheduling time, so a bound asserted on the wall clock of the
// return must allow for it rather than asserting the ceiling exactly.
const teardownReturnOverhead = 2 * time.Second

// teardownParkedOptionSize is the selected permission option's
// identifier length: at least four mebibytes, so no pipe buffer can
// hold the reply that echoes it back.
const teardownParkedOptionSize = 4*1024*1024 + 4096

// teardownParkedScriptTemplate is the fake agent for the parked
// teardown scenario. Substituting __DIR__ and __SIZE__ gives a script
// that first detaches two helper processes into their own session
// before doing anything else, escaping this script's own process
// group: one reads a small prefix of whatever arrives on its standard
// input and then stops reading while holding that pipe's read end
// open, and one simply holds its standard output's write end open.
// Each explicitly redirects the file descriptor it does not
// represent, and each is released from an ordinary shell "&"
// background job's own implicit /dev/null substitution for standard
// input by duplicating the saved descriptor explicitly rather than
// leaving it to inherit fd 0 unredirected. It then writes a
// session/request_permission request whose selected option carries an
// identifier of at least four mebibytes, followed by a second,
// distinct request the adapter never answers before teardown begins.
// Finally it closes its own copies of both piped descriptors and
// idles.
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

// newParkedTeardownSession launches the scenario's fake agent as a
// real subprocess, wires a session to it exactly as startSession
// would (skipping the handshake calls, which this scenario has no use
// for), and waits for genuine evidence that the reply write is
// parked: the reader helper's own completion marker, written only
// once it has consumed its one bounded read. That evidence, combined
// with the reply being many times larger than any pipe buffer, is
// what makes the park a property of the setup rather than a timing
// assumption; the wait loop itself is bounded polling for that
// marker, not a sleep standing in for the park.
func newParkedTeardownSession(t *testing.T) *parkedTeardownFixture {
	t.Helper()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	script := strings.NewReplacer(
		"__DIR__", dir,
		"__SIZE__", strconv.Itoa(teardownParkedOptionSize),
	).Replace(teardownParkedScriptTemplate)
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)

	stdinCloser, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}

	pipes, err := procutil.StartWithOwnedPipes(cmd)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes: %v", err)
	}

	state := &sessionState{
		pid:         cmd.Process.Pid,
		stdinCloser: stdinCloser,
		pipes:       pipes,
		itemCh:      make(chan pumpItem, pumpChannelCapacity),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      discardLogger(),
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 60000},
		caps:        newCapabilityRecord(false),
	}
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, state.logger)

	reaper := procutil.StartReaper(cmd)
	state.waitCh = reaper.Done()

	state.conn = jsonrpc.NewConn(stdinCloser, pipes.Stdout, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	markSessionKnown(state)

	waitForFile(t, filepath.Join(dir, "reader.done"))

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

// teardownParkedStderrScriptTemplate extends teardownParkedScriptTemplate
// with a third detached helper that holds the standard-error write end
// open, for property P13's own run: with that helper alongside the
// reader and writer, drain_stderr_and_reap can only abandon the
// standard-error collector, and close_pipes is the only step left able
// to release it. This template is used only by that property's own
// fixture and MUST NOT replace the shared one: parking the collector on
// every run built from it would break the four runs whose step slices
// never reach close_pipes.
const teardownParkedStderrScriptTemplate = `dir='__DIR__'
exec 3<&0
exec 4>&1
setsid sh -c 'echo $$ >"'"$dir"'/reader.pid"; dd bs=65536 count=1 of=/dev/null 2>/dev/null; touch "'"$dir"'/reader.done"; sleep 600' <&3 3<&- 4>&- >/dev/null 2>/dev/null &
setsid sh -c 'echo $$ >"'"$dir"'/writer.pid"; sleep 600' >&4 4>&- 3<&- <&- 2>/dev/null &
setsid sh -c 'echo $$ >"'"$dir"'/stderr_holder.pid"; sleep 600' <&3 3<&- 4>&- >/dev/null &
exec 3<&-
exec 4>&-
exec <&-
huge=$(head -c __SIZE__ /dev/zero | tr '\0' 'x')
printf '{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"%s"}],"toolCall":{"toolCallId":"tc-1","title":"work"}}}\n' "$huge"
printf '{"jsonrpc":"2.0","id":2,"method":"fs/read_text_file","params":{}}\n'
exec >&-
sleep 600
`

// newParkedTeardownSessionWithStderrHolder behaves like
// newParkedTeardownSession, except its fake agent also detaches a third
// helper that holds the standard-error write end open, so
// drain_stderr_and_reap can only abandon the collector and close_pipes
// is the only remaining step able to release it. Used only by property
// P13's own tests.
func newParkedTeardownSessionWithStderrHolder(t *testing.T) *parkedTeardownFixture {
	t.Helper()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	script := strings.NewReplacer(
		"__DIR__", dir,
		"__SIZE__", strconv.Itoa(teardownParkedOptionSize),
	).Replace(teardownParkedStderrScriptTemplate)
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)

	stdinCloser, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}

	pipes, err := procutil.StartWithOwnedPipes(cmd)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes: %v", err)
	}

	state := &sessionState{
		pid:         cmd.Process.Pid,
		stdinCloser: stdinCloser,
		pipes:       pipes,
		itemCh:      make(chan pumpItem, pumpChannelCapacity),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      discardLogger(),
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 60000},
		caps:        newCapabilityRecord(false),
	}
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, state.logger)

	reaper := procutil.StartReaper(cmd)
	state.waitCh = reaper.Done()

	state.conn = jsonrpc.NewConn(stdinCloser, pipes.Stdout, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	markSessionKnown(state)

	waitForFile(t, filepath.Join(dir, "reader.done"))

	release := sync.OnceFunc(func() {
		killHelperGroup(filepath.Join(dir, "reader.pid"))
		killHelperGroup(filepath.Join(dir, "writer.pid"))
		killHelperGroup(filepath.Join(dir, "stderr_holder.pid"))
	})
	t.Cleanup(release)

	return &parkedTeardownFixture{state: state, release: release}
}

// waitForFile polls for path to exist, failing t if awaitTimeout elapses
// first. This is a bounded wait for a concrete condition the fake
// agent script itself establishes, not a sleep standing in for the
// park it evidences.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(awaitTimeout)
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
// its connection's reader, its subprocess reaper, and its standard-
// error collector have all exited: the leak check that fails when a
// session's goroutines outlive StopSession.
//
// The collector's check is a bounded wait, procutil.DefaultDrainGrace,
// rather than the non-blocking receive the other three use. Each of
// those three reads a channel teardown itself joins (stop_pump joins
// the pump, drain_stderr_and_reap waits on the reaper's channel, and
// the connection's reader is behind both), but the collector is the one
// session goroutine teardown never joins: drain_stderr_and_reap
// abandons it and close_pipes only unparks its read without waiting, so
// it is scheduled after stopSession has already returned. A non-
// blocking check there would report a leak that is not one on every
// fixture that leaves a descendant holding the standard-error write
// end, which is exactly property P13's own fixture.
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
	if state.stderrCollector != nil && !state.stderrCollector.WaitDone(procutil.DefaultDrainGrace) {
		t.Error("session leak: the standard-error collector goroutine is still running after teardown returned")
	}
}

// TestStopSessionTeardownOrder pins teardown's step order by its
// bound: under the three parking conditions this package's scenario
// establishes, plus an agent-initiated request left unanswered,
// StopSession must still return within teardown's specified ceiling,
// procutil.DefaultStopGrace plus three times procutil.DefaultDrainGrace,
// plus the overhead its non-blocking steps cost, and leave no
// goroutine of the session running afterward.
func TestStopSessionTeardownOrder(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t)

	start := time.Now()
	err := stopSession(context.Background(), fakeSession(fx.state))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}
	if bound := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead; elapsed >= bound {
		t.Errorf("stopSession() took %v, want under %v", elapsed, bound)
	}

	assertSessionGoroutinesExited(t, fx.state)
}

// TestStopSessionTeardownOrder_ClosePipesReleasesStderrCollector covers
// property P13: with the escaped stderr holder alongside the reader and
// writer, drain_stderr_and_reap can only abandon the standard-error
// collector, so assertSessionGoroutinesExited's bounded fourth check
// passes only because close_pipes, the order's last step, unparks that
// collector's read. teardown's ceiling is the same one
// TestStopSessionTeardownOrder pins; this run exercises the same order
// against a fixture that also parks the standard-error drain.
func TestStopSessionTeardownOrder_ClosePipesReleasesStderrCollector(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSessionWithStderrHolder(t)

	start := time.Now()
	err := stopSession(context.Background(), fakeSession(fx.state))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}
	if bound := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead; elapsed >= bound {
		t.Errorf("stopSession() took %v, want under %v", elapsed, bound)
	}

	assertSessionGoroutinesExited(t, fx.state)
}

// TestStopSessionTeardownOrder_ClosePipesPresenceControl is property
// P14's presence control for close_pipes: with the escaped stderr
// holder fixture, defaultTeardownOrder's steps minus close_pipes still
// return (drain_stderr_and_reap abandons the collector rather than
// waiting on it forever), but the collector itself is left parked,
// proven by a non-blocking WaitDone(0) reporting false. Calling
// closePipes directly afterward is what releases it. The non-blocking
// probe is correct on the first half, where the collector's read cannot
// return at all, and would be wrong on the second, where the collector
// is only waiting to be scheduled; the second half therefore uses the
// same bounded WaitDone(procutil.DefaultDrainGrace) every other drain
// bound in this file is already expressed in.
func TestStopSessionTeardownOrder_ClosePipesPresenceControl(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSessionWithStderrHolder(t)

	order := defaultTeardownOrder(context.Background(), context.Background(), procutil.DefaultStopGrace)
	steps := order[:len(order)-1]
	if steps[len(steps)-1].name == "close_pipes" {
		t.Fatal("defaultTeardownOrder's last step is not close_pipes; this control no longer drops the right step")
	}

	done := make(chan struct{})
	go func() {
		runTeardown(fx.state, steps)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(procutil.DefaultStopGrace + 2*procutil.DefaultDrainGrace + teardownReturnOverhead):
		t.Fatal("runTeardown() (minus close_pipes) did not return")
	}

	if fx.state.stderrCollector.WaitDone(0) {
		t.Fatal("stderrCollector.WaitDone(0) = true before close_pipes ran, want false: the escaped stderr holder must still be parking the read")
	}

	closePipes(fx.state)

	if !fx.state.stderrCollector.WaitDone(procutil.DefaultDrainGrace) {
		t.Error("stderrCollector.WaitDone() = false after close_pipes ran, want true")
	}

	fx.release()
}

// TestStopSessionTeardown_ClosePipesBeforeDrainLosesLateStderr is the
// clientprotocol half of property P9: moving close_pipes ahead of
// drain_stderr_and_reap, reproduced locally by calling the two step
// functions directly in the wrong order rather than through
// defaultTeardownOrder, loses a line written to standard error just
// before the write end closes. This wiring needs no real subprocess:
// closePipes and drainStderrAndReap operate on state.pipes and
// state.stderrCollector alone, so the ordering claim is verified
// against a plain os.Pipe, matching procutil's own P9 negative control
// for the shared skeleton and opencode.
func TestStopSessionTeardown_ClosePipesBeforeDrainLosesLateStderr(t *testing.T) {
	t.Parallel()

	t.Run("close_pipes before the drain loses the late write", func(t *testing.T) {
		t.Parallel()

		stdoutR, stdoutW, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}
		t.Cleanup(func() { stdoutW.Close() }) //nolint:errcheck // best-effort
		stderrR, stderrW, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}

		state := &sessionState{
			pipes:  &procutil.OwnedPipes{Stdout: stdoutR, Stderr: stderrR},
			logger: discardLogger(),
		}

		if _, err := stderrW.WriteString("late diagnostic\n"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		stderrW.Close() //nolint:errcheck // best-effort

		// Reproduces the defect this property guards against: closing
		// both read ends before the collector ever gets a chance to
		// drain the buffered line, rather than after
		// drain_stderr_and_reap has run.
		closePipes(state)
		state.stderrCollector = procutil.NewStderrCollector(state.pipes.Stderr, state.logger)

		lines := state.stderrCollector.Lines()
		if len(lines) == 1 && lines[0] == "late diagnostic" {
			t.Fatalf("Lines() = %v, want the write lost to the premature close (the negative control did not reproduce the loss)", lines)
		}
	})

	t.Run("drain_stderr_and_reap before close_pipes recovers the late write", func(t *testing.T) {
		t.Parallel()

		stdoutR, stdoutW, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}
		t.Cleanup(func() { stdoutW.Close() }) //nolint:errcheck // best-effort
		stderrR, stderrW, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}

		state := &sessionState{
			pipes:  &procutil.OwnedPipes{Stdout: stdoutR, Stderr: stderrR},
			logger: discardLogger(),
		}
		state.stderrCollector = procutil.NewStderrCollector(stderrR, state.logger)

		if _, err := stderrW.WriteString("late diagnostic\n"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		stderrW.Close() //nolint:errcheck // best-effort

		drainStderrAndReap(context.Background())(state)
		closePipes(state)

		want := []string{"late diagnostic"}
		if got := state.stderrCollector.Lines(); len(got) != 1 || got[0] != want[0] {
			t.Errorf("Lines() = %v, want %v", got, want)
		}
	})
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

// TestStopSessionTeardownReturnsWithConnectionClosedFirst checks a
// step order that closes the connection before the process group is
// terminated. Closing the connection no longer waits on a parked
// write, so this order returns inside the bound below instead of
// parking until the write is released from outside.
func TestStopSessionTeardownReturnsWithConnectionClosedFirst(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t)
	steps := []teardownStep{
		{name: "answer_open", run: signalAnswerOpen},
		{name: "close_connection", run: closeConnection},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_stdout", run: closeStdout},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap(context.Background())},
	}

	done := make(chan struct{})
	go func() {
		runTeardown(fx.state, steps)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3*procutil.DefaultDrainGrace + teardownReturnOverhead):
		t.Fatal("runTeardown() did not return inside the bound, want it to return without waiting for the parked write")
	}

	assertSessionGoroutinesExited(t, fx.state)
}

// TestStopSessionTeardownControlNoStdoutClose is the second negative
// control: omitting the standard-output close leaves the connection's
// reader scanning a stream nothing further will ever close on its
// own, so stop_pump's wait for the pump to exit must not return until
// the park is released from outside.
func TestStopSessionTeardownControlNoStdoutClose(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t)
	runTeardownControl(t, fx, []teardownStep{
		{name: "answer_open", run: signalAnswerOpen},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap(context.Background())},
	})
}

// teardownGracefulExitScript is a fake agent that installs a handler
// for the graceful signal: on TERM it waits delaySeconds, writes
// evidencePath, and exits. It writes readyPath immediately after the
// trap is installed, which newGracefulTeardownSession waits for before
// returning: sending the signal any earlier risks the shell's own
// default disposition running instead of the handler, on whichever of
// the two wins the race with the interpreter reaching the trap
// statement. The handler waits on nothing but the fixed interval, so
// the only timing in a fixture built from it is the one this scenario
// bounds on both sides.
func teardownGracefulExitScript(evidencePath, readyPath, delaySeconds string) string {
	return `trap 'sleep ` + delaySeconds + `; touch "` + evidencePath + `"; exit 0' TERM
touch "` + readyPath + `"
while :; do sleep 1; done
`
}

// teardownExitsImmediatelyScript is a fake agent that exits from the
// graceful signal at once, with no interval of its own. See
// teardownGracefulExitScript for why readyPath exists.
func teardownExitsImmediatelyScript(readyPath string) string {
	return `trap 'exit 0' TERM
touch "` + readyPath + `"
while :; do sleep 1; done
`
}

// teardownIgnoresGracefulScript is a fake agent that ignores the
// graceful signal entirely, so only the unconditional kill ends it.
// See teardownGracefulExitScript for why readyPath exists.
func teardownIgnoresGracefulScript(readyPath string) string {
	return `trap '' TERM
touch "` + readyPath + `"
while :; do sleep 1; done
`
}

// teardownExitsOnItsOwnScript is a fake agent that exits the moment it
// starts, without any signal from the caller, so it has no trap to
// race and needs no readiness marker.
func teardownExitsOnItsOwnScript() string {
	return "exit 0\n"
}

// newGracefulTeardownSession launches script as a real subprocess and
// wires a session to it exactly as newParkedTeardownSession does,
// skipping the handshake calls this scenario has no use for. logger is
// used as the session's logger; a nil logger falls back to
// discardLogger. A non-empty readyPath is awaited before this returns,
// so a caller that signals the process right away does not race the
// script's own startup.
func newGracefulTeardownSession(t *testing.T, script, readyPath string, logger *slog.Logger) *sessionState {
	t.Helper()

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}

	pipes, err := procutil.StartWithOwnedPipes(cmd)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes: %v", err)
	}

	if logger == nil {
		logger = discardLogger()
	}
	state := &sessionState{
		pid:         cmd.Process.Pid,
		stdinCloser: stdinPipe,
		pipes:       pipes,
		itemCh:      make(chan pumpItem, pumpChannelCapacity),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      logger,
		caps:        newCapabilityRecord(false),
	}
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, state.logger)
	state.conn = jsonrpc.NewConn(stdinPipe, pipes.Stdout, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	reaper := procutil.StartReaper(cmd)
	state.waitCh = reaper.Done()

	go runPump(state)

	t.Cleanup(func() {
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort; the process is expected to already be gone
	})

	if readyPath != "" {
		waitForFile(t, readyPath)
	}

	return state
}

// waitForPIDFile polls path until it holds a parseable positive PID,
// failing t if timeout elapses first. A shell's own "> file" redirect
// creates and truncates the file before the write that follows it
// lands, so a single read can observe it as present but empty; polling
// for content rather than mere existence closes that window.
func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to hold a valid PID", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStopSessionTeardownGracefulHandler: against a fake agent that
// installs a handler for the graceful signal and exits from it,
// teardown produces the handler's durable evidence, and the property
// fails if signal_graceful and kill_process_group are exchanged, or if
// await_exit is removed.
func TestStopSessionTeardownGracefulHandler(t *testing.T) {
	t.Parallel()

	t.Run("shipping_order_produces_the_handler_evidence", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		evidencePath := filepath.Join(dir, "evidence")
		readyPath := filepath.Join(dir, "ready")
		state := newGracefulTeardownSession(t, teardownGracefulExitScript(evidencePath, readyPath, "0.4"), readyPath, nil)

		if err := stopSession(context.Background(), fakeSession(state)); err != nil {
			t.Fatalf("stopSession() error = %v", err)
		}

		if _, err := os.Stat(evidencePath); err != nil {
			t.Errorf("handler evidence not found after stopSession(): %v", err)
		}
	})

	t.Run("reordering_signal_after_kill_loses_the_evidence", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		evidencePath := filepath.Join(dir, "evidence")
		readyPath := filepath.Join(dir, "ready")
		state := newGracefulTeardownSession(t, teardownGracefulExitScript(evidencePath, readyPath, "0.4"), readyPath, nil)

		callerCtx := context.Background()
		graceCtx, cancel := context.WithTimeout(callerCtx, procutil.DefaultStopGrace)
		defer cancel()
		runTeardown(state, []teardownStep{
			{name: "answer_open", run: signalAnswerOpen},
			{name: "kill_process_group", run: killProcessGroup},
			{name: "close_stdin", run: closeStdin},
			{name: "await_exit", run: awaitExit(callerCtx, graceCtx, procutil.DefaultStopGrace)},
			{name: "signal_graceful", run: signalGraceful},
			{name: "close_stdout", run: closeStdout},
			{name: "close_connection", run: closeConnection},
			{name: "stop_pump", run: stopPump},
			{name: "drain_stderr_and_reap", run: drainStderrAndReap(callerCtx)},
		})

		if _, err := os.Stat(evidencePath); err == nil {
			t.Error("handler evidence found with signal_graceful reordered after kill_process_group, want none")
		}
	})

	t.Run("removing_await_exit_loses_the_evidence", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		evidencePath := filepath.Join(dir, "evidence")
		readyPath := filepath.Join(dir, "ready")
		state := newGracefulTeardownSession(t, teardownGracefulExitScript(evidencePath, readyPath, "0.4"), readyPath, nil)

		callerCtx := context.Background()
		runTeardown(state, []teardownStep{
			{name: "answer_open", run: signalAnswerOpen},
			{name: "signal_graceful", run: signalGraceful},
			{name: "close_stdin", run: closeStdin},
			{name: "kill_process_group", run: killProcessGroup},
			{name: "close_stdout", run: closeStdout},
			{name: "close_connection", run: closeConnection},
			{name: "stop_pump", run: stopPump},
			{name: "drain_stderr_and_reap", run: drainStderrAndReap(callerCtx)},
		})

		if _, err := os.Stat(evidencePath); err == nil {
			t.Error("handler evidence found with await_exit removed, want none: kill_process_group must land before the handler's fixed interval elapses")
		}
	})
}

// TestStopSessionTeardownIgnoredSignal: against a fake agent that
// ignores the graceful signal, stopSession still returns within
// teardown's specified ceiling, and no member of the agent's process
// group survives the return.
func TestStopSessionTeardownIgnoredSignal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	script := `trap '' TERM
sh -c 'trap "" TERM; while :; do sleep 1; done' &
printf '%s\n' "$!" > "` + childPIDPath + `"
while :; do sleep 1; done
`
	state := newGracefulTeardownSession(t, script, "", nil)
	childPID := waitForPIDFile(t, childPIDPath, awaitTimeout)

	start := time.Now()
	err := stopSession(context.Background(), fakeSession(state))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}
	if bound := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead; elapsed >= bound {
		t.Errorf("stopSession() took %v, want under %v", elapsed, bound)
	}

	assertProcessGone(t, state.pid, awaitTimeout)
	assertProcessGone(t, childPID, awaitTimeout)
}

// assertProcessGone polls until pid no longer answers a signal-0
// probe, failing t if it still does after timeout. A descendant
// orphaned by the group kill is reparented before it is reaped, and a
// signal-0 probe against a zombie still succeeds during that window,
// so a single check would be flaky; polling absorbs the reparenting
// delay while still failing on a process that is genuinely still
// running.
func assertProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("process %d still alive after %v", pid, timeout)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStopSessionTeardownEscalationLogging: an escalation to the
// force kill is logged at Warn carrying the outcome, an ordinary stop
// is not, and neither is a stop whose agent had already exited when
// the grace ran out.
func TestStopSessionTeardownEscalationLogging(t *testing.T) {
	t.Parallel()

	t.Run("agent_exits_inside_grace_emits_no_warn", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		readyPath := filepath.Join(dir, "ready")
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		state := newGracefulTeardownSession(t, teardownExitsImmediatelyScript(readyPath), readyPath, logger)

		if err := stopSession(context.Background(), fakeSession(state)); err != nil {
			t.Fatalf("stopSession() error = %v", err)
		}

		if strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("teardown logged a Warn record for an agent that exited inside the grace: %s", buf.String())
		}
	})

	t.Run("escalation_emits_one_warn_with_the_outcome", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		readyPath := filepath.Join(dir, "ready")
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		state := newGracefulTeardownSession(t, teardownIgnoresGracefulScript(readyPath), readyPath, logger)

		// A short caller deadline is the lever that shortens the wait:
		// going through the real procutil.DefaultStopGrace here would
		// spend the full five-second grace in wall clock for a property
		// that is not the ceiling itself.
		callerCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := stopSession(callerCtx, fakeSession(state)); err != nil {
			t.Fatalf("stopSession() error = %v", err)
		}

		output := buf.String()
		if got := strings.Count(output, "level=WARN"); got != 1 {
			t.Errorf("teardown logged %d Warn records for an escalation, want 1: %s", got, output)
		}
		if !strings.Contains(output, `outcome="caller deadline"`) {
			t.Errorf("teardown's Warn record did not carry outcome=\"caller deadline\": %s", output)
		}
		// The record carries both the configured ceiling and the wait
		// that actually elapsed. A caller deadline shorter than the
		// grace ends the wait early, so elapsed must not be the whole
		// ceiling: reporting only the ceiling would tell an operator
		// the adapter waited five seconds when it waited a fraction of
		// one, and reporting only the elapsed time would hide what the
		// operator configured.
		if strings.Contains(output, "elapsed="+procutil.DefaultStopGrace.String()) {
			t.Errorf("teardown's Warn record reported the full grace ceiling as elapsed for a wait cut short by the caller's deadline: %s", output)
		}
		if !strings.Contains(output, "grace=") {
			t.Errorf("teardown's Warn record did not carry the configured grace ceiling: %s", output)
		}
	})

	t.Run("already_expired_context_against_an_already_exited_agent_emits_no_warn", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		state := newGracefulTeardownSession(t, teardownExitsOnItsOwnScript(), "", logger)

		select {
		case <-state.waitCh:
		case <-time.After(awaitTimeout):
			t.Fatal("agent was not reaped before the test needed it exited")
		}

		expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		defer cancel()
		if err := stopSession(expiredCtx, fakeSession(state)); err != nil {
			t.Fatalf("stopSession() error = %v", err)
		}

		if strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("teardown logged a Warn record for an agent that had already exited, want none (the re-read on the graceCtx arm must classify this as exited): %s", buf.String())
		}
	})
}

// TestStopSessionTeardownParkedWriteBoundsCloseSession confirms
// close_session against a real parked write: the pump has a
// permission reply parked on a full standard-input pipe, so the close
// call can never reach the wire, yet the step still returns inside
// its own bound, the process group is signalled no later than that
// bound after teardown starts, teardown itself still returns inside
// the existing parked-teardown ceiling, and no session goroutine
// outlives the return.
func TestStopSessionTeardownParkedWriteBoundsCloseSession(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t)
	fx.state.closeSessionID = "sess-parked-close"
	fx.state.agentConfig.StopGraceMS = 400
	grace := procutil.StopGrace(fx.state.agentConfig.StopGraceMS)
	bound := closeCallBound(grace)

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- stopSession(context.Background(), fakeSession(fx.state))
	}()

	assertProcessGone(t, fx.state.pid, bound+teardownReturnOverhead)

	ceiling := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopSession() error = %v", err)
		}
	case <-time.After(ceiling):
		t.Fatal("stopSession() did not return inside the parked-teardown ceiling")
	}

	if elapsed := time.Since(start); elapsed >= ceiling {
		t.Errorf("stopSession() took %v, want under %v (the parked-teardown ceiling)", elapsed, ceiling)
	}

	assertSessionGoroutinesExited(t, fx.state)
}

// TestStopSessionTeardownCloseAndAwaitExitShareGrace confirms
// close_session and await_exit spend no more than the resolved grace
// combined: both are bounded by the same graceCtx, so an agent that
// answers neither the close call nor the graceful signal cannot push
// their sum past grace, only up to it. teardown's stated total is
// therefore unchanged by adding the close_session step.
func TestStopSessionTeardownCloseAndAwaitExitShareGrace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	state := newGracefulTeardownSession(t, teardownIgnoresGracefulScript(readyPath), readyPath, nil)
	state.closeSessionID = "sess-combined-budget"

	const grace = 1 * time.Second
	callerCtx := context.Background()
	graceCtx, cancel := context.WithTimeout(callerCtx, grace)
	defer cancel()

	signalAnswerOpen(state)

	start := time.Now()
	closeSession(callerCtx, graceCtx, grace)(state)
	awaitExit(callerCtx, graceCtx, grace)(state)
	combinedElapsed := time.Since(start)

	runTeardown(state, []teardownStep{
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_stdout", run: closeStdout},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap(callerCtx)},
	})
	assertSessionGoroutinesExited(t, state)

	const schedulingOverhead = 400 * time.Millisecond
	if bound := grace + schedulingOverhead; combinedElapsed >= bound {
		t.Errorf("close_session and await_exit together took %v, want under %v (the resolved grace, shared by one deadline)", combinedElapsed, bound)
	}
}

// TestStopSessionTeardownCloseHalvesTheCallerWindow asserts that the
// session/close bound follows whatever remains on the graceful context
// rather than the configured grace. With a caller deadline nearer than
// that grace, a runtime that never answers must not cost the whole
// remaining window: the graceful signal behind the call needs its
// share of it.
func TestStopSessionTeardownCloseHalvesTheCallerWindow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	state := newGracefulTeardownSession(t, teardownIgnoresGracefulScript(readyPath), readyPath, nil)
	state.closeSessionID = "sess-caller-window"

	const (
		grace        = 10 * time.Second
		callerWindow = 2 * time.Second
	)
	callerCtx, cancelCaller := context.WithTimeout(context.Background(), callerWindow)
	defer cancelCaller()
	graceCtx, cancel := context.WithTimeout(callerCtx, grace)
	defer cancel()

	signalAnswerOpen(state)

	start := time.Now()
	closeSession(callerCtx, graceCtx, grace)(state)
	elapsed := time.Since(start)

	runTeardown(state, []teardownStep{
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_stdout", run: closeStdout},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap(callerCtx)},
	})
	assertSessionGoroutinesExited(t, state)

	const schedulingOverhead = 400 * time.Millisecond
	if bound := callerWindow - schedulingOverhead; elapsed >= bound {
		t.Errorf("close_session spent %v of the caller's %v window, want under %v so the graceful signal keeps a share", elapsed, callerWindow, bound)
	}
}

// TestStopSessionGrace_ConfiguredValueBoundsTheWait asserts that
// stopSession's graceCtx expires at a configured agent.stop_grace_ms,
// not at the built-in five-second default: a small configured grace
// against an agent that ignores the graceful signal entirely must force
// the process well short of the default's ceiling.
func TestStopSessionGrace_ConfiguredValueBoundsTheWait(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	state := newGracefulTeardownSession(t, teardownIgnoresGracefulScript(readyPath), readyPath, nil)
	state.agentConfig.StopGraceMS = 200

	start := time.Now()
	if err := stopSession(context.Background(), fakeSession(state)); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("stopSession() force-terminated after %v, want well under the built-in 5s default (proves the configured 200ms grace bounded graceCtx, not DefaultStopGrace)", elapsed)
	}
}

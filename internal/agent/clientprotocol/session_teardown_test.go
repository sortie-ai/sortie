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

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
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

// teardownParkedOptionSize is at least four mebibytes, so no pipe buffer can
// hold the reply that echoes it back.
const teardownParkedOptionSize = 4*1024*1024 + 4096

// teardownReaderBufSize is a single read well under teardownParkedOptionSize,
// so a reply built from that option still cannot drain through it.
const teardownReaderBufSize = 65536

type parkedTeardownFixture struct {
	state   *sessionState
	release func()
}

func newParkedTeardownSession(t *testing.T) *parkedTeardownFixture {
	t.Helper()
	return newParkedTeardownFixture(t, false)
}

// newParkedTeardownSessionWithStderrHolder detaches a third helper that holds
// the standard-error write end open, so drain_stderr_and_reap can only abandon
// the collector and close_pipes is the only step able to release it.
func newParkedTeardownSessionWithStderrHolder(t *testing.T) *parkedTeardownFixture {
	t.Helper()
	return newParkedTeardownFixture(t, true)
}

func newParkedTeardownFixture(t *testing.T, withStderrHolder bool) *parkedTeardownFixture {
	t.Helper()

	dir := t.TempDir()
	readerPIDPath := filepath.Join(dir, "reader.pid")
	readerDonePath := filepath.Join(dir, "reader.done")
	writerPIDPath := filepath.Join(dir, "writer.pid")

	readerPath := agenttest.FakeRuntime(t, dir, "reader", scenarioBoundedReader, boundedReaderParams{
		PIDPath:  readerPIDPath,
		DonePath: readerDonePath,
		BufSize:  teardownReaderBufSize,
	})
	writerPath := agenttest.FakeRuntime(t, dir, "writer", scenarioHoldOpen, holdOpenParams{PIDPath: writerPIDPath})

	agentParams := parkedAgentParams{
		ReaderHelperPath: readerPath,
		WriterHelperPath: writerPath,
		OptionSize:       teardownParkedOptionSize,
	}

	var stderrPIDPath string
	if withStderrHolder {
		stderrPIDPath = filepath.Join(dir, "stderr_holder.pid")
		agentParams.StderrHelperPath = agenttest.FakeRuntime(t, dir, "stderr-holder", scenarioHoldOpen, holdOpenParams{PIDPath: stderrPIDPath})
	}

	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioParkedAgent, agentParams)

	cmd := exec.Command(agentPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)

	stdinCloser, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}

	pipes, err := procutil.StartWithOwnedPipes(cmd, discardLogger())
	if err != nil {
		t.Fatalf("StartWithOwnedPipes: %v", err)
	}

	state := &sessionState{
		pid:         cmd.Process.Pid,
		stdinCloser: stdinCloser,
		pipes:       pipes,
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      discardLogger(),
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 60000},
		caps:        newCapabilityRecord(false, false),
		usage:       agentcore.NewTurnEndUsage(),
	}
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, state.logger)

	reaper := procutil.StartReaper(cmd, state.logger)
	state.waitCh = reaper.Done()

	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(stdinCloser, pipes.Stdout, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	markSessionKnown(state)

	waitForFile(t, readerDonePath)

	release := sync.OnceFunc(func() {
		// Each helper leads its own process group, so killing only the
		// recorded pid leaves its idle process holding the pipe end open;
		// the negative pid signals the whole group.
		killHelperGroup(readerPIDPath)
		killHelperGroup(writerPIDPath)
		if stderrPIDPath != "" {
			killHelperGroup(stderrPIDPath)
		}
	})
	t.Cleanup(release)

	return &parkedTeardownFixture{state: state, release: release}
}

// waitForFile polls for path to exist, failing t if awaitTimeout elapses first.
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

// killHelperGroup reads a pid from pidFile and signals its whole process group.
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

// assertSessionGoroutinesExited fails t unless the pump, the connection's
// reader, the reaper, and the stderr collector have all exited.
//
// The collector's check is a bounded wait, not the non-blocking receive the
// other three use, because teardown never joins the collector:
// drain_stderr_and_reap abandons it and close_pipes only unparks its read
// without waiting, so it is scheduled after stopSession has already returned.
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

func TestStopSessionTeardownOrder_ClosePipesPresenceControl(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSessionWithStderrHolder(t)

	order := defaultTeardownOrder(context.Background(), context.Background(), procutil.DefaultStopGrace)
	steps := order[:len(order)-1]
	if order[len(order)-1].name != "close_pipes" {
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

		// The defect: closing both read ends before the collector drains
		// the buffered line, rather than after drain_stderr_and_reap.
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

// runTeardownControl runs a deliberately wrong step order, asserts it has not
// returned when a one-second observation window expires, releases the park, and
// asserts it returns afterward with no goroutine left. The wait before release
// is a fixed observation window whose point is that nothing should have
// happened yet, not a wait for a condition.
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

func TestStopSessionTeardownReturnsWithConnectionClosedFirst(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t)
	steps := []teardownStep{
		{name: "answer_open", run: func(state *sessionState) { signalAnswerOpen(state) }},
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

func TestStopSessionTeardownControlNoStdoutClose(t *testing.T) {
	t.Parallel()

	fx := newParkedTeardownSession(t)
	runTeardownControl(t, fx, []teardownStep{
		{name: "answer_open", run: func(state *sessionState) { signalAnswerOpen(state) }},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap(context.Background())},
	})
}

// teardownGracefulExitScript is a fake agent whose TERM handler waits
// delaySeconds, writes evidencePath, and exits. It writes readyPath once the
// trap is installed; a caller must wait for readyPath before signalling, or the
// shell's default disposition may win the race with the trap statement.
func teardownGracefulExitScript(evidencePath, readyPath, delaySeconds string) string {
	return `trap 'sleep ` + delaySeconds + `; touch "` + evidencePath + `"; exit 0' TERM
touch "` + readyPath + `"
while :; do sleep 1; done
`
}

// teardownExitsImmediatelyScript exits from the graceful signal at once. See
// teardownGracefulExitScript for why readyPath exists.
func teardownExitsImmediatelyScript(readyPath string) string {
	return `trap 'exit 0' TERM
touch "` + readyPath + `"
while :; do sleep 1; done
`
}

// teardownIgnoresGracefulScript ignores the graceful signal entirely, so only
// the unconditional kill ends it. See teardownGracefulExitScript for readyPath.
func teardownIgnoresGracefulScript(readyPath string) string {
	return `trap '' TERM
touch "` + readyPath + `"
while :; do sleep 1; done
`
}

// teardownExitsOnItsOwnScript exits the moment it starts, so it has no trap to
// race and needs no readiness marker.
func teardownExitsOnItsOwnScript() string {
	return "exit 0\n"
}

// newGracefulTeardownSession launches script as a real subprocess and wires a
// session to it. A nil logger falls back to discardLogger; a non-empty
// readyPath is awaited before returning.
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

	pipes, err := procutil.StartWithOwnedPipes(cmd, logger)
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
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      logger,
		caps:        newCapabilityRecord(false, false),
		usage:       agentcore.NewTurnEndUsage(),
	}
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, state.logger)
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(stdinPipe, pipes.Stdout, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	reaper := procutil.StartReaper(cmd, state.logger)
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

// waitForPIDFile polls path until it holds a parseable positive PID. A shell's
// "> file" redirect creates the file before the write lands, so polling for
// content rather than existence closes that window.
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
			{name: "answer_open", run: func(state *sessionState) { signalAnswerOpen(state) }},
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
			{name: "answer_open", run: func(state *sessionState) { signalAnswerOpen(state) }},
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

// assertProcessGone polls until pid no longer answers a signal-0 probe. A
// signal-0 probe against a zombie still succeeds until it is reaped, so polling
// absorbs the reparenting delay while still failing on a live process.
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

		// A short caller deadline shortens the wait; the real grace would spend
		// its full five seconds for a property that is not the ceiling itself.
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
		// The record carries both the configured ceiling and the elapsed wait.
		// A caller deadline shorter than the grace ends the wait early, so
		// elapsed must not be the whole ceiling: reporting only the ceiling
		// would misstate how long the adapter waited, reporting only elapsed
		// would hide what the operator configured.
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

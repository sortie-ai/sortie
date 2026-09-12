//go:build unix

package codex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// handshakeReplies is the app-server side of the three handshake calls
// and the thread/started notification, shared by the scripts below.
const handshakeReplies = `read -r _init_req
printf '{"id":1,"result":{}}\n'
read -r _initialized_notif
read -r _account_read_req
printf '{"id":2,"result":{}}\n'
read -r _thread_start_req
printf '{"id":3,"result":{"thread":{"id":"fake-thread-1"}}}\n'
printf '{"method":"thread/started","params":{}}\n'
`

// writeFakeAppServerScriptStderrAtHandshake creates a script that writes
// one diagnostic line to standard error and exits before answering the
// initialize call, which is how a runtime that cannot start reports why.
func writeFakeAppServerScriptStderrAtHandshake(t *testing.T, line string) string {
	t.Helper()
	return agenttest.WriteScript(t, t.TempDir(), "fake-codex-app-server-stderr-handshake",
		fmt.Sprintf("printf '%s\\n' >&2\nexit 3\n", line))
}

// writeFakeAppServerScriptStderrMidTurn creates a script that completes
// the handshake, accepts one turn/start call and answers it, then writes
// a diagnostic line to standard error and exits, leaving the turn
// waiting on an output stream that has ended.
func writeFakeAppServerScriptStderrMidTurn(t *testing.T, line string) string {
	t.Helper()
	content := handshakeReplies + "read -r _turn_start_req\n" +
		"printf '{\"id\":4,\"result\":{\"turn\":{\"id\":\"t1\"}}}\\n'\n" +
		fmt.Sprintf("printf '%s\\n' >&2\nexit 7\n", line)
	return agenttest.WriteScript(t, t.TempDir(), "fake-codex-app-server-stderr-turn", content)
}

// writeFakeAppServerScriptStderrAfterHandshake creates a script that
// completes the handshake, then waits for one line on standard input
// before writing a diagnostic to standard error and exiting. The
// sentinel makes the runtime's death happen at a point the test chooses,
// so the turn that follows starts against a runtime already gone.
func writeFakeAppServerScriptStderrAfterHandshake(t *testing.T, line string) string {
	t.Helper()
	content := handshakeReplies + "read -r _sentinel\n" +
		fmt.Sprintf("printf '%s\\n' >&2\nexit 5\n", line)
	return agenttest.WriteScript(t, t.TempDir(), "fake-codex-app-server-stderr-after-handshake", content)
}

// writeFakeAppServerScriptQuietSession creates a script that completes
// the handshake, writes nothing to standard error and idles until it is
// terminated, which is the shape of a session the operator stops.
func writeFakeAppServerScriptQuietSession(t *testing.T) string {
	t.Helper()
	content := handshakeReplies + "while :; do sleep 0.05; done\n"
	return agenttest.WriteScript(t, t.TempDir(), "fake-codex-app-server-quiet", content)
}

func requireStderrLine(t *testing.T, spy *agenttest.LogSpy, label, want string) {
	t.Helper()
	lines := agenttest.RequireWarnLines(t, spy, label)
	for _, line := range lines {
		if strings.Contains(line, want) {
			return
		}
	}
	t.Errorf("WARN lines %v do not contain %q: the runtime's own diagnostic never reached the operator", lines, want)
}

// TestStartSession_HandshakeFailureReportsRuntimeStderr covers the
// session-start half of the defect: a runtime that explains itself on
// standard error and exits left the operator with an error naming the
// handshake and nothing the runtime said.
//
// Not run with t.Parallel(): it installs a global slog default and pins
// CODEX_API_KEY via t.Setenv.
func TestStartSession_HandshakeFailureReportsRuntimeStderr(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	spy := agenttest.InstallLogSpy(t)

	script := writeFakeAppServerScriptStderrAtHandshake(t, "codex: failed to load config: unexpected key at line 3")

	adapter := &CodexAdapter{drainGrace: 2 * time.Second}
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err == nil {
		t.Fatal("StartSession() error = nil, want a handshake failure")
	}

	requireStderrLine(t, spy, "handshake failure", "unexpected key at line 3")
}

// TestRunTurn_StdoutEndReportsRuntimeStderr covers the turn half: a
// runtime that dies mid-turn reported an exit code with no diagnostic.
//
// Not run with t.Parallel(): see above.
func TestRunTurn_StdoutEndReportsRuntimeStderr(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	spy := agenttest.InstallLogSpy(t)

	script := writeFakeAppServerScriptStderrMidTurn(t, "codex: model provider returned 503, giving up")

	adapter := &CodexAdapter{drainGrace: 2 * time.Second}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	_, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, runErr, domain.ErrPortExit)

	requireStderrLine(t, spy, "runtime exit mid-turn", "returned 503")
}

// TestRunTurn_TurnStartOnDeadRuntimeReportsStderr covers the turn that
// never starts: the runtime is already gone when turn/start is issued,
// so the call fails and its diagnostic is the only explanation.
//
// Not run with t.Parallel(): see above.
func TestRunTurn_TurnStartOnDeadRuntimeReportsStderr(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	spy := agenttest.InstallLogSpy(t)

	script := writeFakeAppServerScriptStderrAfterHandshake(t, "codex: lost connection to the model provider")

	adapter := &CodexAdapter{drainGrace: 2 * time.Second}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	if _, writeErr := fmt.Fprintln(state.stdin, "die"); writeErr != nil {
		t.Fatalf("writing the sentinel: %v", writeErr)
	}
	select {
	case <-state.readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection's reader did not end after the runtime exited")
	}

	if _, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	}); runErr == nil {
		t.Log("RunTurn() published the outcome rather than returning an error; either way the diagnostic must be reported")
	}

	requireStderrLine(t, spy, "turn start on a dead runtime", "lost connection")
}

// TestStopSession_LeavesNoDrainRunning covers the third verification
// point from the stop side: once the stop returns, the collector's
// goroutine is gone rather than holding a buffer nothing will read, and
// a session the operator stopped re-emits nothing.
//
// On this path the pipe close is what ends the drain. The case where the
// close cannot end it, because a descendant that escaped the group still
// holds the write end, is what reportStderr's bound covers and is
// pinned by TestReportStderr_AbandonsADrainThatCannotFinish.
//
// Not run with t.Parallel(): see above.
func TestStopSession_LeavesNoDrainRunning(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	spy := agenttest.InstallLogSpy(t)

	script := writeFakeAppServerScriptQuietSession(t)

	adapter := &CodexAdapter{drainGrace: 2 * time.Second}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	collector := state.stderrCollector
	if collector == nil {
		t.Fatal("state.stderrCollector = nil, want a collector started with the session")
	}
	if collector.WaitDone(0) {
		t.Fatal("the drain finished before the session was stopped, so this test would pass without the stop ending it")
	}

	if stopErr := adapter.StopSession(context.Background(), session); stopErr != nil {
		t.Fatalf("StopSession() error = %v, want nil", stopErr)
	}

	if !collector.WaitDone(0) {
		t.Error("the standard-error drain is still running after StopSession returned, want it ended by the stop")
	}
	if lines := spy.WarnLines(); len(lines) != 0 {
		t.Errorf("a stopped session re-emitted stderr lines %v, want none", lines)
	}
	if got := collector.Lines(); len(got) != 0 {
		t.Errorf("collector.Lines() = %v, want none from a runtime that wrote nothing", got)
	}
	if collector.Dropped() != 0 {
		t.Errorf("collector.Dropped() = %d, want 0", collector.Dropped())
	}
}

// writeFakeAppServerScriptStderrLiveRuntime creates a script that
// completes the handshake, answers one turn/start call, writes a
// diagnostic to standard error, marks readyFile so the test knows the
// turn is past its opening call, and then stays alive. The runtime
// holding its standard-error write end open is what separates a stop
// and a malformed line from a runtime that actually died.
func writeFakeAppServerScriptStderrLiveRuntime(t *testing.T, line, readyFile, extraStdout string) string {
	t.Helper()
	content := handshakeReplies + "read -r _turn_start_req\n" +
		"printf '{\"id\":4,\"result\":{\"turn\":{\"id\":\"t1\"}}}\\n'\n" +
		fmt.Sprintf("printf '%s\\n' >&2\n", line) +
		extraStdout +
		fmt.Sprintf("printf ready > %s\n", readyFile) +
		"while :; do sleep 0.05; done\n"
	return agenttest.WriteScript(t, t.TempDir(), "fake-codex-app-server-stderr-live", content)
}

// awaitReady blocks until the fake runtime has written its ready marker,
// so a test acts on a runtime that has reached a known point rather than
// on a sleep.
func awaitReady(t *testing.T, readyFile string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(readyFile); err == nil && len(data) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake runtime never reached its ready marker")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStopSession_DoesNotReportStderrOfATurnItInterrupts covers the
// contract StopSession states: the stop closes the connection, which
// ends the message channel and sends the turn still running down the
// same path a runtime that died takes. The operator asked for the stop,
// so the runtime's standard error is not a failure to explain.
//
// Not run with t.Parallel(): it installs a global slog default and pins
// CODEX_API_KEY via t.Setenv.
func TestStopSession_DoesNotReportStderrOfATurnItInterrupts(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	spy := agenttest.InstallLogSpy(t)

	readyFile := filepath.Join(t.TempDir(), "ready")
	const diagnostic = "codex: still talking when the operator stopped us"
	script := writeFakeAppServerScriptStderrLiveRuntime(t, diagnostic, readyFile, "")

	adapter := &CodexAdapter{drainGrace: 2 * time.Second}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		_, _ = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "work",
			OnEvent: func(domain.AgentEvent) {},
		})
	}()
	awaitReady(t, readyFile)

	if stopErr := adapter.StopSession(context.Background(), session); stopErr != nil {
		t.Errorf("StopSession() error = %v, want nil", stopErr)
	}
	select {
	case <-turnDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn did not end after the stop")
	}

	for _, line := range spy.WarnLines() {
		if strings.Contains(line, diagnostic) {
			t.Errorf("the stop reported the runtime's standard error %q, want a stop the operator asked for to explain nothing", line)
		}
	}
}

// TestRunTurn_MalformedLineDoesNotReportStderrOfALiveRuntime covers the
// distinction between a stream that ended and a line that failed to
// parse. The read loop dispatches a malformed line and keeps reading, so
// the runtime is still there with its standard-error write end open:
// reporting would wait the whole drain bound for output that is still
// coming, and would spend the session's one report on a runtime that had
// not died.
//
// Not run with t.Parallel(): it installs a global slog default and pins
// CODEX_API_KEY via t.Setenv.
func TestRunTurn_MalformedLineDoesNotReportStderrOfALiveRuntime(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	spy := agenttest.InstallLogSpy(t)

	readyFile := filepath.Join(t.TempDir(), "ready")
	const diagnostic = "codex: a warning from a runtime that is still running"
	const grace = 3 * time.Second
	script := writeFakeAppServerScriptStderrLiveRuntime(t, diagnostic, readyFile,
		"printf 'this is not json\\n'\n")

	adapter := &CodexAdapter{drainGrace: grace}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	start := time.Now()
	_, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})
	elapsed := time.Since(start)

	if runErr == nil {
		t.Fatal("RunTurn() error = nil, want the malformed line to end the turn")
	}
	if elapsed >= grace {
		t.Errorf("RunTurn() took %v, want well under the %v drain bound: a live runtime's output is still coming, so the turn must not wait for it", elapsed, grace)
	}
	for _, line := range spy.WarnLines() {
		if strings.Contains(line, diagnostic) {
			t.Errorf("a malformed line reported the standard error of a runtime that is still running: %q", line)
		}
	}
}

// TestRelease_EndsAStderrDrainNothingElseCanEnd pins the drain's own
// exit, which abandoning it does not give: abandonment releases whoever
// waited for the lines, while the scanner stays blocked in a read for as
// long as something holds the write end. Holding it here is what an
// escaped descendant does in production, and without the read-end close
// the collector would outlive every turn of the session.
func TestRelease_EndsAStderrDrainNothingElseCanEnd(t *testing.T) {
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = outWrite.Close()
		_ = outRead.Close()
		_ = errWrite.Close()
		_ = errRead.Close()
	})

	const grace = 200 * time.Millisecond
	collector := procutil.NewStderrCollector(errRead, slog.Default())
	if _, err := io.WriteString(errWrite, "codex: written before the descendant kept the pipe\n"); err != nil {
		t.Fatalf("writing to the stderr pipe: %v", err)
	}
	if collector.WaitDone(0) {
		t.Fatal("the drain ended on its own, so this test would pass without release ending it")
	}

	pipes := &procutil.OwnedPipes{Stdout: outRead, Stderr: errRead}
	reaped := make(chan struct{})
	close(reaped)
	release(&sessionState{}, pipes, collector, grace, make(chan struct{}), reaped, slog.Default())

	deadline := time.Now().Add(5 * time.Second)
	for !collector.WaitDone(0) {
		if time.Now().After(deadline) {
			t.Fatal("the standard-error drain is still blocked in its read, want release to have closed the read end once its bound resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReportStderr_AbandonsADrainThatCannotFinish pins the bound the
// reporting paths depend on. A drain whose write end is still held
// never reaches EOF, so an unbounded wait would park the caller that
// wants the diagnostic: the session's own drainGrace bounds the wait,
// an unfinished drain is abandoned, and the abandonment marker is
// appended after whatever the drain had already collected rather than
// replacing it.
//
// Not run with t.Parallel(): it installs a global slog default.
func TestReportStderr_AbandonsADrainThatCannotFinish(t *testing.T) {
	spy := agenttest.InstallLogSpy(t)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})

	state := &sessionState{drainGrace: 100 * time.Millisecond}
	state.stderrCollector = procutil.NewStderrCollector(reader, slog.Default())
	if _, err := io.WriteString(writer, "codex: still writing\n"); err != nil {
		t.Fatalf("writing to the stderr pipe: %v", err)
	}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		state.reportStderr(slog.Default())
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("reportStderr took %v on a 100ms bound, want it to give up inside the bound", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reportStderr never returned while the stderr write end was held: the wait is unbounded")
	}

	lines := agenttest.RequireWarnLines(t, spy, "abandoned drain")
	if !slices.Contains(lines, "codex: still writing") {
		t.Errorf("WARN lines %v do not contain the line collected before abandonment", lines)
	}
	if !slices.Contains(lines, procutil.AbandonedMarker) {
		t.Errorf("WARN lines %v do not contain the abandonment marker", lines)
	}
}

// TestStopSession_JoinsADrainThatCannotFinishOnItsOwn pins the
// no-collector-outlives-the-session guarantee against the case that can
// actually break it. A drain whose write end nothing closes never
// reaches end of file, so closing the read end is not enough on its own:
// the stop has to wait for the drain to end or abandon it. Holding the
// write end here is what the escaped descendant does in production.
func TestStopSession_JoinsADrainThatCannotFinishOnItsOwn(t *testing.T) {
	spy := agenttest.InstallLogSpy(t)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})

	const diagnostic = "codex: written before the stop"
	state := &sessionState{drainGrace: 100 * time.Millisecond}
	state.stderrCollector = procutil.NewStderrCollector(reader, slog.Default())
	if _, err := io.WriteString(writer, diagnostic+"\n"); err != nil {
		t.Fatalf("writing to the stderr pipe: %v", err)
	}
	if state.stderrCollector.WaitDone(0) {
		t.Fatal("the drain ended before the stop, so this test would pass without the stop ending it")
	}

	adapter := &CodexAdapter{}
	start := time.Now()
	if stopErr := adapter.StopSession(context.Background(), domain.Session{Internal: state}); stopErr != nil {
		t.Errorf("StopSession() error = %v, want nil", stopErr)
	}
	elapsed := time.Since(start)

	// A drain blocked in a read cannot be joined, only released by the
	// bound, so the guarantee is that the stop does not return while the
	// drain is still unresolved. Paying the bound is what proves it
	// waited; a stop that skipped the wait returns at once.
	if elapsed < state.drainGrace {
		t.Errorf("StopSession() returned after %v, want it to have waited out the %v bound the unfinished drain needs", elapsed, state.drainGrace)
	}
	resolved := make(chan struct{})
	go func() {
		_ = state.stderrCollector.Lines()
		close(resolved)
	}()
	select {
	case <-resolved:
	case <-time.After(2 * time.Second):
		t.Error("the standard-error drain was still unresolved after StopSession returned, want the stop to have finished or abandoned it")
	}
	for _, line := range spy.WarnLines() {
		if strings.Contains(line, diagnostic) {
			t.Errorf("the stop emitted the runtime's standard error %q, want it joined without reporting", line)
		}
	}
}

// TestReportStderr_StopInsideTheWaitSuppressesTheWarning covers the
// window the stop marker alone leaves open: the wait for the drain can
// last the whole bound, so a stop that arrives while a report is already
// inside it must still suppress the warning. The write end is held here
// for the same reason an escaped descendant holds it in production,
// which is what makes the wait long enough for the stop to land in it.
func TestReportStderr_StopInsideTheWaitSuppressesTheWarning(t *testing.T) {
	spy := agenttest.InstallLogSpy(t)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})

	const diagnostic = "codex: written before the operator stopped us"
	state := &sessionState{drainGrace: 500 * time.Millisecond}
	state.stderrCollector = procutil.NewStderrCollector(reader, slog.Default())
	if _, err := io.WriteString(writer, diagnostic+"\n"); err != nil {
		t.Fatalf("writing to the stderr pipe: %v", err)
	}

	reported := make(chan struct{})
	go func() {
		state.reportStderr(slog.Default())
		close(reported)
	}()

	// The report is inside its wait by now: the drain cannot finish
	// while this test holds the write end, so the wait runs the full
	// bound and the stop below lands inside it.
	time.Sleep(50 * time.Millisecond)
	adapter := &CodexAdapter{}
	if stopErr := adapter.StopSession(context.Background(), domain.Session{Internal: state}); stopErr != nil {
		t.Errorf("StopSession() error = %v, want nil", stopErr)
	}

	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("reportStderr never returned")
	}

	for _, line := range spy.WarnLines() {
		if strings.Contains(line, diagnostic) {
			t.Errorf("a stop that arrived during the drain wait still reported %q, want the stop to suppress it", line)
		}
	}
}

// TestReportStderr_LatchesToOneEmissionAcrossRepeatedCalls pins
// stderrReported: a session reports the runtime's standard error at
// most once, however many failure paths reach reportStderr over the
// session's life. A second call against the same dead runtime has
// nothing new to add and must not re-emit the same lines.
//
// Not run with t.Parallel(): it installs a global slog default.
func TestReportStderr_LatchesToOneEmissionAcrossRepeatedCalls(t *testing.T) {
	spy := agenttest.InstallLogSpy(t)

	state := &sessionState{drainGrace: time.Second}
	state.stderrCollector = procutil.NewStderrCollector(strings.NewReader("codex: fatal provider error\n"), slog.Default())

	state.reportStderr(slog.Default())
	state.reportStderr(slog.Default())

	lines := agenttest.RequireWarnLines(t, spy, "repeated reportStderr calls against a dead runtime")
	if len(lines) != 1 {
		t.Errorf("WarnLines() after two reportStderr calls = %v, want exactly one emission of the runtime's diagnostic", lines)
	}
}

// TestReaderEnded_SeesTheConnectionBeforeTheWatcher covers the window a
// turn/start failure lands in: Conn.Done() closes when the reader exits
// and watchTermination closes readerDone only after that, so a predicate
// reading readerDone alone would report a runtime that is already gone as
// still alive and skip its diagnostic.
func TestReaderEnded_SeesTheConnectionBeforeTheWatcher(t *testing.T) {
	t.Parallel()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	state := &sessionState{readerDone: make(chan struct{})}
	state.conn = jsonrpc.NewConn(io.Discard, reader, func(jsonrpc.Message) {})
	t.Cleanup(state.conn.Close)

	if state.readerEnded() {
		t.Fatal("readerEnded() = true on a live connection, want false")
	}

	// End the stream the way a runtime exit does, and leave readerDone
	// open: watchTermination has not run yet in this window.
	if closeErr := writer.Close(); closeErr != nil {
		t.Fatalf("closing the write end: %v", closeErr)
	}
	select {
	case <-state.conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection's reader did not exit after the stream ended")
	}

	select {
	case <-state.readerDone:
		t.Fatal("readerDone closed in this test, which no longer exercises the window between the two signals")
	default:
	}

	if !state.readerEnded() {
		t.Error("readerEnded() = false after the connection's reader exited, want true: the turn/start failure path would skip the runtime's diagnostic")
	}
}

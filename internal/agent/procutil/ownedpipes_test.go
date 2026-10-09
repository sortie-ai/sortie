package procutil

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestStartWithOwnedPipes_Success(t *testing.T) {
	t.Parallel()

	cmd := fakeRuntimeCmd(t, agenttest.Output{Stdout: "out-line\n", Stderr: "err-line\n"})
	pipes, _ := startOwned(t, cmd)

	outCh := make(chan []byte, 1)
	errCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(pipes.Stdout); outCh <- b }()
	go func() { b, _ := io.ReadAll(pipes.Stderr); errCh <- b }()

	var stdout, stderr []byte
	select {
	case stdout = <-outCh:
	case <-time.After(5 * time.Second):
		t.Fatal("reading Stdout did not reach EOF within 5s")
	}
	select {
	case stderr = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("reading Stderr did not reach EOF within 5s")
	}

	if got := string(stdout); got != "out-line\n" {
		t.Errorf("Stdout content = %q, want %q", got, "out-line\n")
	}
	if got := string(stderr); got != "err-line\n" {
		t.Errorf("Stderr content = %q, want %q", got, "err-line\n")
	}

	if err := cmd.Wait(); err != nil {
		t.Errorf("cmd.Wait() = %v, want nil", err)
	}
}

func TestStartWithOwnedPipes_StageProcessStart(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sortie-nonexistent-binary-99999")
	pipes, group, err := StartWithOwnedPipes(context.Background(), cmd, nil)
	if pipes != nil {
		t.Errorf("StartWithOwnedPipes() pipes = %v, want nil", pipes)
	}
	var startErr *StartError
	if !errors.As(err, &startErr) {
		t.Fatalf("error type = %T, want *StartError", err)
	}
	if startErr.Stage != StageProcessStart {
		t.Errorf("StartError.Stage = %d, want %d (StageProcessStart)", startErr.Stage, StageProcessStart)
	}
	if startErr.Unwrap() != startErr.Err {
		t.Errorf("Unwrap() = %v, want the same value as StartError.Err (%v)", startErr.Unwrap(), startErr.Err)
	}
	if _, ok := errors.AsType[*exec.Error](err); !ok {
		t.Errorf("errors.AsType[*exec.Error](err) ok = false, want true: unwrap chain must reach the underlying os/exec error")
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Errorf("cmd.Stdout=%v cmd.Stderr=%v after a failed Start, want both nil", cmd.Stdout, cmd.Stderr)
	}
	if group != nil {
		t.Errorf("StartWithOwnedPipes() group = %v, want nil after a failed Start", group)
	}
	if got := lookupGroup(cmd); got != nil {
		t.Errorf("lookupGroup(cmd) after a failed Start = %v, want nil", got)
	}
}

func TestStartWithOwnedPipes_ReturnsRecord(t *testing.T) {
	t.Parallel()

	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	_, group := startOwned(t, cmd)

	if group == nil {
		t.Fatal("StartWithOwnedPipes() group = nil, want the launch's record")
	}
	if got := lookupGroup(cmd); got != group {
		t.Errorf("lookupGroup(cmd) while the launch runs = %p, want the returned record %p", got, group)
	}

	r := StartReaper(cmd, nil)
	awaitDone(t, r, 5*time.Second)

	if got := lookupGroup(cmd); got != nil {
		t.Errorf("lookupGroup(cmd) after Done = %p, want nil (the reaper unregisters the launch)", got)
	}
}

func TestStartError_Discrimination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stage StartStage
	}{
		{"StageStdoutPipe", StageStdoutPipe},
		{"StageStderrPipe", StageStderrPipe},
		{"StageProcessStart", StageProcessStart},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cause := errors.New("underlying os error")
			se := &StartError{Stage: tt.stage, Err: cause}

			if got := se.Unwrap(); got != cause {
				t.Errorf("Unwrap() = %v, want %v", got, cause)
			}
			if !errors.Is(se, cause) {
				t.Errorf("errors.Is(StartError, cause) = false, want true")
			}
			if se.Error() == "" {
				t.Error("Error() = \"\", want a non-empty message")
			}
		})
	}
}

func TestOwnedPipes_CloseIdempotent(t *testing.T) {
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
	t.Cleanup(func() { stderrW.Close() }) //nolint:errcheck // best-effort

	pipes := &OwnedPipes{Stdout: stdoutR, Stderr: stderrR}

	if err := pipes.CloseStdout(); err != nil {
		t.Errorf("CloseStdout() first call = %v, want nil", err)
	}
	if err := pipes.CloseStdout(); err != nil {
		t.Errorf("CloseStdout() second call = %v, want nil (idempotent)", err)
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 1)
		stderrR.Read(buf) //nolint:errcheck // only used to detect return
	}()
	select {
	case <-readDone:
		t.Fatal("read on Stderr returned after CloseStdout, want the standard-error end left open")
	case <-time.After(100 * time.Millisecond):
	}

	if err := pipes.Close(); err != nil {
		t.Errorf("Close() after CloseStdout = %v, want nil", err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read on Stderr did not return after Close(), want the standard-error end released")
	}

	if err := pipes.Close(); err != nil {
		t.Errorf("Close() second call = %v, want nil (idempotent)", err)
	}
}

// The first subtest is a negative control: without the parent write-end close
// a reader hangs past an exited child, so the second subtest cannot pass by
// accident.
func TestStartWithOwnedPipes_ParentWriteEndCloseIsLoadBearing(t *testing.T) {
	t.Parallel()

	t.Run("without the close, a caller's read hangs past an exited child", func(t *testing.T) {
		t.Parallel()

		stdoutRead, stdoutWrite, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}
		t.Cleanup(func() { stdoutRead.Close() }) //nolint:errcheck // best-effort

		cmd := fakeRuntimeCmd(t, agenttest.Output{})
		cmd.Stdout = stdoutWrite
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}
		t.Cleanup(func() { cmd.Wait() }) //nolint:errcheck // best-effort reap

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			buf := make([]byte, 1)
			stdoutRead.Read(buf) //nolint:errcheck // only used to detect return
		}()

		select {
		case <-readDone:
			t.Fatal("read on Stdout returned before the parent's write end was closed, want it still parked (the negative control failed to reproduce the hang)")
		case <-time.After(300 * time.Millisecond):
		}

		if err := stdoutWrite.Close(); err != nil {
			t.Fatalf("stdoutWrite.Close() = %v", err)
		}
		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
			t.Fatal("read on Stdout did not return after closing the parent's write end")
		}
	})

	t.Run("with the real StartWithOwnedPipes, the read reaches EOF promptly", func(t *testing.T) {
		t.Parallel()

		cmd := fakeRuntimeCmd(t, agenttest.Output{})
		pipes, _ := startOwned(t, cmd)

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			buf := make([]byte, 1)
			stdoutRead := pipes.Stdout
			for {
				if _, err := stdoutRead.Read(buf); err != nil {
					return
				}
			}
		}()

		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
			t.Fatal("read on Stdout did not reach EOF within 2s of an exited child, want the parent write-end close to release it")
		}

		if err := cmd.Wait(); err != nil {
			t.Errorf("cmd.Wait() = %v, want nil", err)
		}
	})
}

// Adapters defer OwnedPipes.Close until after the collector's own bound
// because closing the read end first loses late descendant writes with no
// marker and no record.
func TestOwnedPipes_DeferredCloseAfterStderrBoundIsLoadBearing(t *testing.T) {
	t.Parallel()

	t.Run("closing before the drain loses the late write", func(t *testing.T) {
		t.Parallel()

		stderrRead, stderrWrite, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}
		pipes := &OwnedPipes{Stdout: nil, Stderr: stderrRead}

		if _, err := stderrWrite.WriteString("late diagnostic\n"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}

		if err := pipes.closeStderr(); err != nil {
			t.Fatalf("closeStderr() = %v", err)
		}
		stderrWrite.Close() //nolint:errcheck // best-effort

		collector := NewStderrCollector(stderrRead, nil)
		lines := collector.Lines()
		if len(lines) == 1 && lines[0] == "late diagnostic" {
			t.Fatalf("Lines() = %v, want the write lost to the premature close (the negative control did not reproduce the loss)", lines)
		}
	})

	t.Run("closing after the drain recovers the late write", func(t *testing.T) {
		t.Parallel()

		stderrRead, stderrWrite, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() = %v", err)
		}
		pipes := &OwnedPipes{Stdout: nil, Stderr: stderrRead}

		if _, err := stderrWrite.WriteString("late diagnostic\n"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		stderrWrite.Close() //nolint:errcheck // best-effort

		collector := NewStderrCollector(stderrRead, nil)
		if !collector.WaitDone(2 * time.Second) {
			t.Fatal("WaitDone() = false, want the drain to finish once the write end is closed")
		}
		if err := pipes.closeStderr(); err != nil {
			t.Errorf("closeStderr() after the drain = %v, want nil", err)
		}

		want := []string{"late diagnostic"}
		if got := collector.Lines(); len(got) != 1 || got[0] != want[0] {
			t.Errorf("Lines() = %v, want %v", got, want)
		}
	})
}

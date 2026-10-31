//go:build unix

package sshutil

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/waittest"
)

func startSupervised(t *testing.T, shell, agent, barrier string, stdin io.Reader, stdout *os.File) *exec.Cmd {
	t.Helper()

	sshdStandIn := exec.Command(shell, "-c", `"$0" -c "$1"; exit "$?"`, shell, supervisedAgent("{ "+agent+"\n}", time.Second, barrier))
	sshdStandIn.Stdin = stdin
	sshdStandIn.Stdout = stdout
	sshdStandIn.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sshdStandIn.Start(); err != nil {
		t.Fatalf("start %s: %v", shell, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-sshdStandIn.Process.Pid, syscall.SIGKILL) })
	return sshdStandIn
}

func waitExit(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return waittest.Receive(t, exited, "supervised shell exit")
}

func TestSupervisedAgent_PassesStdinAndExitStatus(t *testing.T) {
	t.Parallel()

	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			cmd := startSupervised(t, shell.path, `read code; exit "$code"`, "", strings.NewReader("7\n"), nil)

			var exitErr *exec.ExitError
			if err := waitExit(t, cmd); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("exit = %v, want exit status 7", err)
			}
		})
	}
}

func TestSupervisedAgent_EndsTheGroupOnceTheParentIsGone(t *testing.T) {
	t.Parallel()

	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			agentOut, agentOutWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentOut.Close() })
			cmd := startSupervised(t, shell.path, "echo started; exec sleep 600", "", nil, agentOutWriter)
			_ = agentOutWriter.Close()

			started, ended := make(chan struct{}), make(chan struct{})
			go func() {
				r := bufio.NewReader(agentOut)
				_, _ = r.ReadString('\n')
				close(started)
				_, _ = io.Copy(io.Discard, r)
				close(ended)
			}()
			waittest.Receive(t, started, "agent start")

			_ = cmd.Process.Kill()
			_ = waitExit(t, cmd)
			waittest.Receive(t, ended, "agent end")
		})
	}
}

func TestSupervisedAgent_EndsWhatTheAgentLeftWhenItExits(t *testing.T) {
	t.Parallel()

	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			leftover := filepath.Join(t.TempDir(), "leftover.pid")
			agent := "{ sleep 600 & echo $! >" + shellQuote(leftover) + "; exit 3\n}"
			// The shell leads its own group, as sshd makes it lead the session's.
			cmd := exec.Command(shell.path, "-c", supervisedAgent(agent, time.Second, ""))
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start %s: %v", shell.path, err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

			var exitErr *exec.ExitError
			if err := waitExit(t, cmd); !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
				t.Fatalf("exit = %v, want exit status 3", err)
			}
			waittest.ProcessGone(t, waittest.PIDFile(t, leftover))
		})
	}
}

func startExitWaiter(t *testing.T, shell, barrier string) <-chan error {
	t.Helper()

	waiter := exec.Command(shell, "-c", AwaitExitCommand(barrier))
	if err := waiter.Start(); err != nil {
		t.Fatalf("start waiter: %v", err)
	}
	t.Cleanup(func() { _ = waiter.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- waiter.Wait() }()
	return done
}

func TestAwaitExitCommand_WaitsForTheAgentTreeInItsOwnProcessGroup(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"flock", "setsid"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found on PATH", tool)
		}
	}
	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			ready, gate := filepath.Join(dir, "ready"), filepath.Join(dir, "gate")
			if err := syscall.Mkfifo(gate, 0o600); err != nil {
				t.Fatalf("Mkfifo: %v", err)
			}
			gateHolder, err := os.OpenFile(gate, os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open gate: %v", err)
			}
			release := sync.OnceFunc(func() { _ = gateHolder.Close() })
			t.Cleanup(release)
			barrier := NewExitBarrier()
			t.Cleanup(func() { _ = os.Remove(exitBarrierPath(barrier)) })

			agent := `setsid sh -c 'exec 5<"$2"; : >"$1"; read _ <&5' sh ` + shellQuote(ready) + " " + shellQuote(gate) + " &\nwait"
			supervised := startSupervised(t, shell.path, agent, barrier, nil, nil)
			waittest.File(t, ready)

			done := startExitWaiter(t, shell.path, barrier)
			waittest.Never(t, done, 200*time.Millisecond, "exit waiter return while the agent tree holds the barrier")
			release()

			if err := waittest.Receive(t, done, "exit waiter return after the agent tree exited"); err != nil {
				t.Errorf("exit waiter = %v, want exit status 0", err)
			}
			_ = waitExit(t, supervised)
		})
	}
}

func TestAwaitExitCommand_ReturnsAtOnceWithoutABarrierFile(t *testing.T) {
	t.Parallel()

	shell := availableShells(t)[0]

	done := startExitWaiter(t, shell.path, NewExitBarrier())

	if err := waittest.Receive(t, done, "exit waiter return"); err != nil {
		t.Errorf("exit waiter = %v, want exit status 0", err)
	}
}

package waittest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/waittest"
)

type recorder struct {
	testing.TB
	msg string
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func bubbles[T any](t *testing.T, cases map[string]T, fn func(t *testing.T, c T)) {
	t.Parallel()
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) { fn(t, c) })
		})
	}
}

func check[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func timed(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

func fatalMessage(t *testing.T, fn func(tb testing.TB)) string {
	t.Helper()
	rec := &recorder{TB: t}
	done := make(chan struct{})
	go func() { defer close(done); fn(rec) }()
	<-done
	return rec.msg
}

func TestReceiveWithin(t *testing.T) {
	const bound = time.Hour
	type outcome struct {
		value   int
		ok      bool
		elapsed time.Duration
	}
	type receiveCase struct {
		ch   func() <-chan int
		want outcome
	}
	delivered := func() <-chan int { ch := make(chan int, 1); ch <- 7; return ch }
	closed := func() <-chan int { ch := make(chan int); close(ch); return ch }
	silent := func() <-chan int { return make(chan int) }

	bubbles(t, map[string]receiveCase{
		"delivered value":         {delivered, outcome{7, true, 0}},
		"closed channel":          {closed, outcome{0, true, 0}},
		"silence until the bound": {silent, outcome{0, false, bound}},
	}, func(t *testing.T, c receiveCase) {
		ch := c.ch()
		var got outcome

		got.elapsed = timed(func() { got.value, got.ok = waittest.ReceiveWithin(ch, bound) })

		check(t, "ReceiveWithin()", got, c.want)
	})
}

func TestUntilWithin(t *testing.T) {
	const (
		bound    = 25 * time.Millisecond
		interval = 10 * time.Millisecond
	)
	type untilCase struct {
		succeedOn   int
		wantErr     bool
		wantElapsed time.Duration
	}

	bubbles(t, map[string]untilCase{
		"met at once":        {1, false, 0},
		"met on a later run": {3, false, 2 * interval},
		"never met":          {0, true, bound},
	}, func(t *testing.T, c untilCase) {
		calls := 0
		var err, lastErr error

		elapsed := timed(func() {
			err = waittest.UntilWithin(bound, interval, func() error {
				calls++
				if calls == c.succeedOn {
					return nil
				}
				lastErr = fmt.Errorf("attempt %d", calls)
				return lastErr
			})
		})

		check(t, "UntilWithin() error present", err != nil, c.wantErr)
		check(t, "UntilWithin() elapsed", elapsed, c.wantElapsed)
		if c.wantErr {
			check(t, "UntilWithin() wraps the last error", errors.Is(err, lastErr), true)
		}
	})
}

func TestWrappers(t *testing.T) {
	type wrapperCase struct {
		call        func(tb testing.TB)
		wantMsg     string
		wantElapsed time.Duration
	}
	received := func() chan int { ch := make(chan int, 1); ch <- 3; return ch }

	bubbles(t, map[string]wrapperCase{
		"Receive fails after the guard":   {func(tb testing.TB) { waittest.Receive(tb, make(chan int), "exit code") }, "waittest: exit code: nothing received within 1m0s", waittest.HangGuard},
		"Send fails after the guard":      {func(tb testing.TB) { waittest.Send(tb, make(chan int), 1, "event") }, "waittest: event: not taken within 1m0s", waittest.HangGuard},
		"Until fails with the last error": {func(tb testing.TB) { waittest.Until(tb, func() error { return errors.New("not ready") }) }, "waittest: condition not met within 1m0s: not ready", waittest.HangGuard},
		"Never fails on a value":          {func(tb testing.TB) { waittest.Never(tb, received(), time.Second, "tick") }, "waittest: tick: received 3 within 1s, want nothing", 0},
		"Never passes on silence":         {func(tb testing.TB) { waittest.Never(tb, make(chan int), time.Minute, "tick") }, "", time.Minute},
	}, func(t *testing.T, c wrapperCase) {
		var msg string

		elapsed := timed(func() { msg = fatalMessage(t, c.call) })

		check(t, "failure message", msg, c.wantMsg)
		check(t, "elapsed", elapsed, c.wantElapsed)
	})
}

func TestWithinLeavesReserveBeforeDeadline(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		if got := waittest.Within(t, time.Minute, time.Second); got != time.Minute {
			t.Fatalf("Within() without a deadline = %v, want the bound %v", got, time.Minute)
		}
		return
	}
	remaining := time.Until(deadline)

	if got := waittest.Within(t, remaining+time.Hour, time.Second); got > remaining-time.Second {
		t.Errorf("Within() = %v, want at most %v, the time left less the reserve", got, remaining-time.Second)
	}
	if got := waittest.Within(t, time.Second, 0); got != time.Second {
		t.Errorf("Within() = %v, want the bound %v when the deadline leaves more", got, time.Second)
	}
	if got := waittest.Within(t, time.Second, remaining+time.Hour); got != time.Millisecond {
		t.Errorf("Within() = %v, want %v when the reserve exceeds the time left", got, time.Millisecond)
	}
}

func TestContextsEndAtHangGuard(t *testing.T) {
	bubbles(t, map[string]func(tb testing.TB) context.Context{
		"Context":        waittest.Context,
		"CleanupContext": waittest.CleanupContext,
	}, func(t *testing.T, newContext func(tb testing.TB) context.Context) {
		ctx := newContext(t)

		elapsed := timed(func() { <-ctx.Done() })

		check(t, "Err() is DeadlineExceeded", errors.Is(ctx.Err(), context.DeadlineExceeded), true)
		check(t, "elapsed", elapsed, waittest.HangGuard)
	})
}

func TestContextLifetimeInLaterCleanup(t *testing.T) {
	t.Parallel()

	type lifetimeCase struct {
		newContext func(tb testing.TB) context.Context
		wantAlive  bool
	}
	for name, c := range map[string]lifetimeCase{
		"Context is done before cleanups run":  {waittest.Context, false},
		"CleanupContext is alive in a cleanup": {waittest.CleanupContext, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := c.newContext(t)
			t.Cleanup(func() { check(t, "Err() == nil in a later cleanup", ctx.Err() == nil, c.wantAlive) })
		})
	}
}

func TestPIDFileWaitsThroughUnusableContent(t *testing.T) {
	bubbles(t, map[string]string{
		"empty":        "",
		"not a number": "starting",
		"zero":         "0",
	}, func(t *testing.T, content string) {
		path := filepath.Join(t.TempDir(), "child.pid")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		got := make(chan int, 1)
		go func() { defer close(got); got <- waittest.PIDFile(&recorder{TB: t}, path) }()
		synctest.Wait()

		if err := os.WriteFile(path, []byte("4242\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}

		check(t, fmt.Sprintf("PIDFile(%q, then 4242)", content), <-got, 4242)
	})
}

func TestProcessGoneFailsWhileRunning(t *testing.T) {
	type runningCase struct {
		pid      int
		wantFail bool
	}

	bubbles(t, map[string]runningCase{
		"own process": {os.Getpid(), true},
		"zero pid":    {0, false},
	}, func(t *testing.T, c runningCase) {
		msg := fatalMessage(t, func(tb testing.TB) { waittest.ProcessGone(tb, c.pid) })

		check(t, fmt.Sprintf("ProcessGone(%d) failed", c.pid), strings.Contains(msg, fmt.Sprintf("process %d is still running", c.pid)), c.wantFail)
	})
}

func TestProcessGoneReturnsForExitedChild(t *testing.T) {
	t.Parallel()

	for name, reap := range map[string]bool{"reaped": true, "zombie": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if !reap && runtime.GOOS == "darwin" {
				t.Skip("darwin has no procfs to show a zombie")
			}
			cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // G204: re-runs this test binary
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start(): %v", err)
			}
			if reap {
				_ = cmd.Wait()
			} else {
				t.Cleanup(func() { _ = cmd.Wait() })
			}

			waittest.ProcessGone(t, cmd.Process.Pid)
		})
	}
}

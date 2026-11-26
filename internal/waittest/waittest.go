// Package waittest holds the waits test code uses: one hang guard, never-failing
// cores that take a caller-owned bound, and failing wrappers over them.
//
// It imports no internal package, so any test or test-support code may use it.
package waittest

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// HangGuard is the one hang guard of test code. It is the largest bound a
// wait outside the live suites needs, so no wait gets less time than it had,
// and it is a constant because a synctest bubble cannot read t.Deadline.
const HangGuard = time.Minute

const pollInterval = 5 * time.Millisecond

// ReceiveWithin returns the value ch delivers and true, the zero value and
// true once ch is closed, or the zero value and false when bound elapses
// first. It never fails a test and panics on a non-positive bound.
func ReceiveWithin[T any](ch <-chan T, bound time.Duration) (T, bool) {
	requirePositive("bound", bound)
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v, true
	case <-timer.C:
		var zero T
		return zero, false
	}
}

func sendWithin[T any](ch chan<- T, v T, bound time.Duration) bool {
	requirePositive("bound", bound)
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case ch <- v:
		return true
	case <-timer.C:
		return false
	}
}

// UntilWithin runs cond at once and then every interval until it returns nil,
// and returns nil. When bound elapses first it returns an error carrying the
// last error cond returned. It never fails a test and panics on a non-positive
// bound or interval.
func UntilWithin(bound, interval time.Duration, cond func() error) error {
	requirePositive("bound", bound)
	requirePositive("interval", interval)
	timer := time.NewTimer(bound)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		err := cond()
		if err == nil {
			return nil
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return fmt.Errorf("condition not met within %v: %w", bound, err)
		}
	}
}

// Within returns bound, shortened so that reserve still remains before t's
// deadline when the wait ends, and at least a millisecond. A live suite bounds
// each working wait with it, so a hung step fails its own test while the
// suite keeps the time to stop what it started and report, instead of the
// binary's timeout ending every test at once. Without a deadline it returns
// bound. It MUST NOT be called inside a synctest bubble, where t.Deadline
// panics.
func Within(t *testing.T, bound, reserve time.Duration) time.Duration {
	requirePositive("bound", bound)
	deadline, ok := t.Deadline()
	if !ok {
		return bound
	}
	return max(min(bound, time.Until(deadline)-reserve), time.Millisecond)
}

// Receive returns the value ch delivers, or the zero value once ch is closed,
// and fails the test when nothing arrives within HangGuard. what names the
// event. It MUST run on the test goroutine.
func Receive[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	v, ok := ReceiveWithin(ch, HangGuard)
	if !ok {
		t.Fatalf("waittest: %s: nothing received within %v", what, HangGuard)
	}
	return v
}

// Send delivers v on ch and fails the test when ch does not take it within
// HangGuard. what names the event. It MUST run on the test goroutine.
func Send[T any](t testing.TB, ch chan<- T, v T, what string) {
	t.Helper()
	if !sendWithin(ch, v, HangGuard) {
		t.Fatalf("waittest: %s: not taken within %v", what, HangGuard)
	}
}

// Never fails the test when ch delivers a value, or closes, within window.
// what names the event. It MUST run on the test goroutine.
func Never[T any](t testing.TB, ch <-chan T, window time.Duration, what string) {
	t.Helper()
	if v, ok := ReceiveWithin(ch, window); ok {
		t.Fatalf("waittest: %s: received %v within %v, want nothing", what, v, window)
	}
}

// Until polls cond every few milliseconds and fails the test with cond's last
// error when it is still unmet after HangGuard. cond runs on the test
// goroutine, so it MAY call t.Fatalf. Until MUST run on the test goroutine.
func Until(t testing.TB, cond func() error) {
	t.Helper()
	if err := UntilWithin(HangGuard, pollInterval, cond); err != nil {
		t.Fatalf("waittest: %v", err)
	}
}

// Context returns a context derived from t.Context() that is done with
// context.DeadlineExceeded after HangGuard. It is done before the test's
// cleanups run, so a cleanup that joins goroutines bound to it sees them
// stopped. It never fails the test and MAY be called from any goroutine.
func Context(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), HangGuard)
	t.Cleanup(cancel)
	return ctx
}

// CleanupContext returns a context that is done after HangGuard or when its
// own cleanup runs. Unlike Context it does not derive from t.Context(), so it
// is alive in every cleanup registered after it. It never fails the test and
// MAY be called from any goroutine.
func CleanupContext(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), HangGuard)
	t.Cleanup(cancel)
	return ctx
}

func requirePositive(name string, d time.Duration) {
	if d <= 0 {
		panic(fmt.Sprintf("waittest: %s must be positive, got %v", name, d))
	}
}

package trackermetrics

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// spyMetrics records calls to IncTrackerRequests and delegates all other
// methods to the embedded NoopMetrics.
type spyMetrics struct {
	domain.NoopMetrics
	calls []struct{ op, result string }
}

var _ domain.Metrics = (*spyMetrics)(nil)

func (s *spyMetrics) IncTrackerRequests(op, result string) {
	s.calls = append(s.calls, struct{ op, result string }{op, result})
}

func TestTrack_success(t *testing.T) {
	t.Parallel()

	spy := &spyMetrics{}
	err := Track(spy, "fetch_issue", func() error { return nil })
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("IncTrackerRequests calls = %d, want 1", len(spy.calls))
	}
	if spy.calls[0].op != "fetch_issue" {
		t.Errorf("IncTrackerRequests op = %q, want %q", spy.calls[0].op, "fetch_issue")
	}
	if spy.calls[0].result != "success" {
		t.Errorf("IncTrackerRequests result = %q, want %q", spy.calls[0].result, "success")
	}
}

func TestTrack_error(t *testing.T) {
	t.Parallel()

	spy := &spyMetrics{}
	fnErr := errors.New("fn-error")
	_ = Track(spy, "transition", func() error { return fnErr })
	if len(spy.calls) != 1 {
		t.Fatalf("IncTrackerRequests calls = %d, want 1", len(spy.calls))
	}
	if spy.calls[0].op != "transition" {
		t.Errorf("IncTrackerRequests op = %q, want %q", spy.calls[0].op, "transition")
	}
	if spy.calls[0].result != "error" {
		t.Errorf("IncTrackerRequests result = %q, want %q", spy.calls[0].result, "error")
	}
}

func TestTrack_errorUnchanged(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	spy := &spyMetrics{}
	err := Track(spy, "comment", func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("Track error = %v, want %v", err, sentinel)
	}
}

func TestTrack_nilMetrics(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("fn-error")
	err := Track(nil, "fetch_issue", func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("Track(nil) error = %v, want %v", err, sentinel)
	}
}

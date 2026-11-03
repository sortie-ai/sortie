package orchestrator

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// linkingTracker is a tracker adapter that names its project's web address.
type linkingTracker struct {
	domain.TrackerAdapter
	url string
}

func (t linkingTracker) ProjectURL() string { return t.url }

func TestTrackerProjectURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		adapter domain.TrackerAdapter
		want    string
	}{
		{name: "adapter names its project", adapter: linkingTracker{url: "https://github.com/owner/repo"}, want: "https://github.com/owner/repo"},
		{name: "adapter names none", adapter: struct{ domain.TrackerAdapter }{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := &Orchestrator{trackerAdapter: tt.adapter}
			if got := o.trackerProjectURL(); got != tt.want {
				t.Errorf("trackerProjectURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

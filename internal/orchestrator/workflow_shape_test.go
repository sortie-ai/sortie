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
		{name: "credentials are stripped", adapter: linkingTracker{url: "https://user:secret@gitea.example.com/owner/repo"}, want: "https://gitea.example.com/owner/repo"},
		{name: "trailing slash, query and fragment are dropped", adapter: linkingTracker{url: "https://gitlab.example.com/group/sub/?tab=issues#top"}, want: "https://gitlab.example.com/group/sub"},
		{name: "non-http scheme is dropped", adapter: linkingTracker{url: "ftp://example.com/owner/repo"}, want: ""},
		{name: "address without host is dropped", adapter: linkingTracker{url: "https:///owner/repo"}, want: ""},
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

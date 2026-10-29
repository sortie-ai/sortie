package quota

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

func headlessReplies(t *testing.T) []string {
	t.Helper()

	var replies []string
	for line := range strings.Lines(string(loadFixture(t, "headless_real.txt"))) {
		if message, ok := strings.CutPrefix(line, "<< "); ok {
			replies = append(replies, strings.TrimSpace(message))
		}
	}
	return replies
}

func TestPull_HeadlessRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		edit    func([]string) []string
		wantErr error
	}{
		{"exhausted window with the unlimited chat skipped", func(r []string) []string { return r }, nil},
		{"notifications ahead of an answer are skipped", func(r []string) []string {
			return slices.Insert(r, 1, `{"jsonrpc":"2.0","method":"log","params":{}}`, `{"jsonrpc":"2.0","id":9,"result":{}}`)
		}, nil},
		{"error answer to connect", func(r []string) []string {
			return slices.Replace(r, 0, 1, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}`)
		}, usage.ErrPullUnsupported},
		{"error answer to account.getQuota", func(r []string) []string {
			return slices.Replace(r, 1, 2, `{"jsonrpc":"2.0","id":2,"error":{"code":-32601}}`)
		}, usage.ErrPullUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt := &fakeRuntime{replies: tt.edit(headlessReplies(t))}

			readings, err := Pull(t.Context(), rt)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Pull() error = %v, want it to wrap %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pull() error = %v, want nil", err)
			}
			if connect, getQuota := strings.Index(rt.sent, "connect"), strings.Index(rt.sent, "account.getQuota"); connect < 0 || getQuota < connect {
				t.Errorf("Pull() sent %q, want connect before account.getQuota", rt.sent)
			}
			windows := usage.Windows(Source, readings, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
			if len(windows) != 1 {
				t.Fatalf("usage.Windows() = %d windows, want 1 (the unlimited chat entry is skipped)", len(windows))
			}
			w := windows[0]
			if w.Key != "premium_interactions" || w.UsedPercent == nil || *w.UsedPercent != 100 || !w.ResetsAt.IsZero() ||
				!w.ReportedAt.Equal(time.Date(2026, 10, 28, 0, 15, 43, 0, time.UTC)) {
				t.Errorf("usage.Windows() = %+v, want premium_interactions at 100 percent, no reset, reported 2026-10-28T00:15:43Z", w)
			}
		})
	}
}

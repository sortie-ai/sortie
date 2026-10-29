package quota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

var errRuntimeMissing = errors.New("runtime missing")

type fakeRuntime struct {
	env       map[string]string
	replies   []string
	exchanges int
	sent      string
}

func (r *fakeRuntime) Env(name string) (string, bool) {
	value, ok := r.env[name]
	return value, ok
}

func (r *fakeRuntime) Exchange(_ context.Context, _ []string, _ int64, exchange func(stdin io.Writer, stdout io.Reader) error) error {
	r.exchanges++
	if r.replies == nil {
		return errRuntimeMissing
	}

	var answers, requests bytes.Buffer
	for _, reply := range r.replies {
		_ = jsonrpc.WriteContentLengthFrame(&answers, []byte(reply))
	}
	err := exchange(&requests, &answers)
	r.sent = requests.String()
	return err
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func serveQuota(t *testing.T, status int, body []byte) (hits *atomic.Int32, auth *atomic.Value) {
	t.Helper()

	hits, auth = &atomic.Int32{}, &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	previous := quotaEndpoint
	quotaEndpoint = srv.URL
	t.Cleanup(func() { quotaEndpoint = previous })
	return
}

func TestPull_TokenPrecedenceAndWindows(t *testing.T) {
	fixture := loadFixture(t, "copilot_internal_user.json")
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"copilot token outranks the others", map[string]string{"GITHUB_TOKEN": "c", "GH_TOKEN": "b", "COPILOT_GITHUB_TOKEN": "a"}, "token a"},
		{"gh token outranks github token", map[string]string{"GITHUB_TOKEN": "c", "GH_TOKEN": "b"}, "token b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, auth := serveQuota(t, http.StatusOK, fixture)

			readings, err := Pull(t.Context(), &fakeRuntime{env: tt.env})
			if err != nil {
				t.Fatalf("Pull() error = %v, want nil", err)
			}
			windows := usage.Windows(Source, readings, time.Now())

			if got, _ := auth.Load().(string); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
			}
			if len(windows) != 1 {
				t.Fatalf("Windows() = %d windows, want 1 (the unlimited members have none)", len(windows))
			}
			w := windows[0]
			if w.Key != "premium_interactions" || w.UsedPercent == nil || *w.UsedPercent != 100 || !w.ResetsAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("Windows() = key %q used %v resets %v, want premium_interactions, 100, 2026-11-01T00:00:00Z", w.Key, w.UsedPercent, w.ResetsAt)
			}
		})
	}
}

func TestPull_Failures(t *testing.T) {
	const token = "ghp_secret_value"
	withToken := map[string]string{"GH_TOKEN": token}

	tests := []struct {
		name          string
		env           map[string]string
		status        int
		body          string
		wantHits      int32
		wantExchanges int
		wantErr       error
	}{
		{"unauthorized runs the headless route", withToken, http.StatusUnauthorized, "", 1, 1, errRuntimeMissing},
		{"forbidden runs the headless route", withToken, http.StatusForbidden, "", 1, 1, errRuntimeMissing},
		{"not found runs the headless route", withToken, http.StatusNotFound, "", 1, 1, errRuntimeMissing},
		{"no token runs the headless route without a request", nil, http.StatusOK, "", 0, 1, errRuntimeMissing},
		{"server error is an error", withToken, http.StatusInternalServerError, "", 1, 0, nil},
		{"malformed answer is an error", withToken, http.StatusOK, "{", 1, 0, nil},
		{"custom provider sends nothing", map[string]string{"GH_TOKEN": token, providerBaseURLEnv: "http://127.0.0.1:1"}, http.StatusOK, "", 0, 0, usage.ErrPullUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, _ := serveQuota(t, tt.status, []byte(tt.body))
			rt := &fakeRuntime{env: tt.env}

			_, err := Pull(t.Context(), rt)

			if err == nil {
				t.Fatal("Pull() error = nil, want error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Pull() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if rt.exchanges != tt.wantExchanges || hits.Load() != tt.wantHits {
				t.Errorf("Pull() = %d exchanges and %d requests, want %d and %d", rt.exchanges, hits.Load(), tt.wantExchanges, tt.wantHits)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("Pull() error holds the token: %v", err)
			}
		})
	}
}

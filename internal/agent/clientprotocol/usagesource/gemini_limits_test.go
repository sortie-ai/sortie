package usagesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

const geminiTestToken = "ya29.a0-test-access-token"

type geminiRuntime struct {
	env       map[string]string
	sessionID string
	scans     *int
}

func (r geminiRuntime) SessionID() string { return r.sessionID }

func (geminiRuntime) Request(context.Context, string, any) (json.RawMessage, error) {
	return nil, errors.New("unexpected session request")
}

func (r geminiRuntime) LaunchEnv(name string) (string, bool) {
	value, ok := r.env[name]
	return value, ok
}

func (r geminiRuntime) ReadHomeFile(ctx context.Context, homeVar, rel string, limit int64) ([]byte, error) {
	return agentcore.LaunchTarget{}.ReadRuntimeFile(ctx, []string{homeVar + "=" + r.env[homeVar]}, homeVar, rel, limit)
}

func (r geminiRuntime) ScanHomeFile(ctx context.Context, homeVar, rel string, limit, lineLimit int64, fn func(line []byte) error) error {
	if r.scans != nil {
		*r.scans++
	}
	return agentcore.LaunchTarget{}.ScanRuntimeFile(ctx, []string{homeVar + "=" + r.env[homeVar]}, homeVar, rel, limit, lineLimit, fn)
}

func (geminiRuntime) RemoteHost() bool { return false }

func (geminiRuntime) CommandWords() []string { return nil }

func (geminiRuntime) RunHostCommand(context.Context, agentcore.HostCommand) ([]byte, error) {
	return nil, errors.New("unexpected host command")
}

type geminiPull struct {
	windows   []domain.UsageWindow
	err       error
	calls     []string
	quotaBody string
	fileAfter string
}

func testSignInFile(expiry time.Time) string {
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":"1//refresh-secret","expiry_date":%d}`, geminiTestToken, expiry.UnixMilli())
}

func runGeminiPull(t *testing.T, signIn string, project string, quotaStatus int) geminiPull {
	t.Helper()

	quota, err := os.ReadFile("testdata/gemini_retrieve_user_quota.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	declaration, err := Declarations.Get(geminiAgentName)
	if err != nil {
		t.Fatalf("Declarations.Get(%q): %v", geminiAgentName, err)
	}

	var mu sync.Mutex
	var got geminiPull
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got.calls = append(got.calls, r.Header.Get("Authorization")+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"loaded-project"}`)
			return
		}
		mu.Lock()
		got.quotaBody = string(body)
		mu.Unlock()
		w.WriteHeader(quotaStatus)
		_, _ = w.Write(quota)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	path := filepath.Join(home, ".gemini", "oauth_creds.json")
	if signIn != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(signIn), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	env := map[string]string{"GEMINI_CLI_HOME": home, "CODE_ASSIST_ENDPOINT": srv.URL}
	if project != "" {
		env["GOOGLE_CLOUD_PROJECT"] = project
	}

	readings, pullErr := declaration.PullWindows(t.Context(), geminiRuntime{env: env})

	after, _ := os.ReadFile(path)
	mu.Lock()
	defer mu.Unlock()
	got.windows = usage.Windows(declaration.Pull, readings, time.Now())
	got.err, got.fileAfter = pullErr, string(after)
	return got
}

func TestGeminiPullServesQuotaFromTheStoredToken(t *testing.T) {
	t.Parallel()

	bearer := "Bearer " + geminiTestToken + " /v1internal:"
	tests := []struct {
		name        string
		project     string
		wantCalls   []string
		wantProject string
	}{
		{"project from loadCodeAssist", "", []string{bearer + "loadCodeAssist", bearer + "retrieveUserQuota"}, "loaded-project"},
		{"project from the launch environment", "env-project", []string{bearer + "retrieveUserQuota"}, "env-project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			signIn := testSignInFile(time.Now().Add(time.Hour))

			got := runGeminiPull(t, signIn, tt.project, http.StatusOK)

			if got.err != nil {
				t.Fatalf("PullWindows() error = %v, want nil", got.err)
			}
			if !slices.Equal(got.calls, tt.wantCalls) {
				t.Errorf("PullWindows() requests = %q, want %q", got.calls, tt.wantCalls)
			}
			if want := fmt.Sprintf(`{"project":%q}`, tt.wantProject); got.quotaBody != want {
				t.Errorf("PullWindows() retrieveUserQuota body = %q, want %q", got.quotaBody, want)
			}
			if got.fileAfter != signIn {
				t.Errorf("sign-in file after PullWindows() = %q, want %q", got.fileAfter, signIn)
			}
			if len(got.windows) != 1 {
				t.Fatalf("PullWindows() = %d windows, want 1: buckets without modelId or remainingFraction are skipped", len(got.windows))
			}
			w := got.windows[0]
			if w.Key != "gemini-2.5-pro/REQUESTS" || w.Label != "Gemini-2.5-pro limit" {
				t.Errorf("window key, label = %q, %q, want %q, %q", w.Key, w.Label, "gemini-2.5-pro/REQUESTS", "Gemini-2.5-pro limit")
			}
			if w.UsedPercent == nil || math.Abs(*w.UsedPercent-60) > 1e-9 {
				t.Errorf("window UsedPercent = %v, want 60", w.UsedPercent)
			}
			if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !w.ResetsAt.Equal(want) {
				t.Errorf("window ResetsAt = %v, want %v", w.ResetsAt, want)
			}
		})
	}
}

func TestGeminiPullSendsNothingWithAnExpiredToken(t *testing.T) {
	t.Parallel()

	got := runGeminiPull(t, testSignInFile(time.Now().Add(-time.Minute)), "", http.StatusOK)

	if got.err != nil || got.windows != nil || len(got.calls) != 0 {
		t.Errorf("PullWindows() = %v, %v after %d requests, want nil, nil after 0", got.windows, got.err, len(got.calls))
	}
}

func TestGeminiPullFailuresNeverHoldTheToken(t *testing.T) {
	t.Parallel()

	valid := testSignInFile(time.Now().Add(time.Hour))
	tests := []struct {
		name            string
		signIn          string
		quotaStatus     int
		wantUnsupported bool
	}{
		{"missing file ends the session's pulls", "", http.StatusOK, true},
		{"forbidden ends the session's pulls", valid, http.StatusForbidden, true},
		{"server error leaves the next pull running", valid, http.StatusInternalServerError, false},
		{"undecodable file", `{"access_token":"` + geminiTestToken + `" nope`, http.StatusOK, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runGeminiPull(t, tt.signIn, "p", tt.quotaStatus)

			if got.err == nil {
				t.Fatalf("PullWindows() error = nil, want error")
			}
			if unsupported := errors.Is(got.err, usage.ErrPullUnsupported); unsupported != tt.wantUnsupported {
				t.Errorf("PullWindows() errors.Is(ErrPullUnsupported) = %v, want %v", unsupported, tt.wantUnsupported)
			}
			if strings.Contains(got.err.Error(), geminiTestToken) {
				t.Error("PullWindows() error holds the access token")
			}
		})
	}
}

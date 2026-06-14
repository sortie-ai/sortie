package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// v2Config returns a baseline v2 adapter config against endpoint with a
// colon-free PAT (Bearer) credential and a non-Cloud host.
func v2Config(endpoint string) map[string]any {
	return map[string]any{
		"endpoint":    endpoint,
		"api_key":     "pat_token_abc",
		"project":     "SRV",
		"api_version": "2",
	}
}

// --- api_version normalization at the constructor (AC6, AC7) ---

func TestNewJiraAdapter_APIVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		apiVersion  any // omitted when nil and present=false
		present     bool
		wantVersion string
		wantBase    string
		wantErr     bool
		wantKind    domain.TrackerErrorKind
		wantMsgSubs []string
	}{
		{name: "absent defaults to 3", present: false, wantVersion: "3", wantBase: "/rest/api/3"},
		{name: "empty string defaults to 3", apiVersion: "", present: true, wantVersion: "3", wantBase: "/rest/api/3"},
		{name: "whitespace-only defaults to 3", apiVersion: "  ", present: true, wantVersion: "3", wantBase: "/rest/api/3"},
		{name: "explicit 3", apiVersion: "3", present: true, wantVersion: "3", wantBase: "/rest/api/3"},
		{name: "explicit 2", apiVersion: "2", present: true, wantVersion: "2", wantBase: "/rest/api/2"},
		{name: "trimmed 2", apiVersion: " 2 ", present: true, wantVersion: "2", wantBase: "/rest/api/2"},
		{name: "bare int 2 coerced", apiVersion: 2, present: true, wantVersion: "2", wantBase: "/rest/api/2"},
		{name: "bare int 3 coerced", apiVersion: 3, present: true, wantVersion: "3", wantBase: "/rest/api/3"},
		{name: "float64 whole 2 coerced", apiVersion: float64(2), present: true, wantVersion: "2", wantBase: "/rest/api/2"},
		{
			name: "invalid string value", apiVersion: "4", present: true,
			wantErr: true, wantKind: domain.ErrTrackerPayload,
			wantMsgSubs: []string{`"4"`, `"2"`, `"3"`},
		},
		{
			name: "invalid int value", apiVersion: 1, present: true,
			wantErr: true, wantKind: domain.ErrTrackerPayload,
			wantMsgSubs: []string{"1", `"2"`, `"3"`},
		},
		{
			name: "fractional float rejected", apiVersion: 2.5, present: true,
			wantErr: true, wantKind: domain.ErrTrackerPayload,
			wantMsgSubs: []string{`"2"`, `"3"`},
		},
		{
			name: "out-of-range whole float rejected", apiVersion: 1e20, present: true,
			wantErr: true, wantKind: domain.ErrTrackerPayload,
			wantMsgSubs: []string{`"2"`, `"3"`},
		},
		{
			name: "non-coercible type rejected", apiVersion: []any{"2"}, present: true,
			wantErr: true, wantKind: domain.ErrTrackerPayload,
			wantMsgSubs: []string{`"2"`, `"3"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A colon-free key is only valid under v2; for the v3 default
			// cases use the Basic email:token form so the only variable
			// under test is api_version.
			cfg := map[string]any{
				"endpoint": "https://jira.internal.example.com",
				"api_key":  "user@test.com:tok",
				"project":  "SRV",
			}
			if tt.present {
				cfg["api_version"] = tt.apiVersion
			}

			a, err := NewJiraAdapter(cfg)
			if tt.wantErr {
				assertTrackerErrorKind(t, err, tt.wantKind)
				var te *domain.TrackerError
				asTrackerError(t, err, &te)
				for _, sub := range tt.wantMsgSubs {
					if !strings.Contains(te.Message, sub) {
						t.Errorf("TrackerError.Message = %q, want to contain %q", te.Message, sub)
					}
				}
				if a != nil {
					t.Error("adapter should be nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewJiraAdapter(api_version=%v) unexpected error: %v", tt.apiVersion, err)
			}
			ja := a.(*JiraAdapter)
			if ja.apiVersion != tt.wantVersion {
				t.Errorf("apiVersion = %q, want %q", ja.apiVersion, tt.wantVersion)
			}
			if ja.basePath != tt.wantBase {
				t.Errorf("basePath = %q, want %q", ja.basePath, tt.wantBase)
			}
		})
	}
}

// asTrackerError populates target with the *domain.TrackerError from the
// error chain or fails the test.
func asTrackerError(t *testing.T, err error, target **domain.TrackerError) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.As(err, target) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
}

// --- resolveAuth matrix (AC4, AC6) ---

func TestResolveAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apiVersion string
		apiKey     string
		wantHeader string // exact header when non-empty and no error
		wantPrefix string // header prefix when the secret should not be matched verbatim
		wantErr    bool
	}{
		{
			name: "v3 email:token to Basic", apiVersion: "3", apiKey: "user@test.com:tok123",
			wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("user@test.com:tok123")),
		},
		{name: "v3 colon-free rejected", apiVersion: "3", apiKey: "patnocolon", wantErr: true},
		{name: "v3 empty user rejected", apiVersion: "3", apiKey: ":tok", wantErr: true},
		{name: "v3 empty secret rejected", apiVersion: "3", apiKey: "email:", wantErr: true},
		{
			name: "v2 user:password to Basic", apiVersion: "2", apiKey: "svcuser:s3cret",
			wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("svcuser:s3cret")),
		},
		{name: "v2 empty user rejected", apiVersion: "2", apiKey: ":password", wantErr: true},
		{name: "v2 empty secret rejected (trailing colon)", apiVersion: "2", apiKey: "user:", wantErr: true},
		{
			name: "v2 colon-free to Bearer", apiVersion: "2", apiKey: "pat_token_abc",
			wantHeader: "Bearer pat_token_abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveAuth(tt.apiVersion, tt.apiKey)
			if tt.wantErr {
				assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
				if got != "" {
					t.Errorf("resolveAuth(%q, %q) = %q, want empty header on error", tt.apiVersion, tt.apiKey, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAuth(%q, %q) unexpected error: %v", tt.apiVersion, tt.apiKey, err)
			}
			if tt.wantHeader != "" && got != tt.wantHeader {
				t.Errorf("resolveAuth(%q, %q) = %q, want %q", tt.apiVersion, tt.apiKey, got, tt.wantHeader)
			}
			if tt.wantPrefix != "" && !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("resolveAuth(%q, %q) = %q, want prefix %q", tt.apiVersion, tt.apiKey, got, tt.wantPrefix)
			}
		})
	}
}

// TestResolveAuth_GuardRelaxesOnlyForV2ColonFree asserts the relaxation
// is confined to the v2 colon-free shape: the same colon-free key is
// rejected on v3, and a malformed Basic shape is rejected on both
// versions identically.
func TestResolveAuth_GuardRelaxesOnlyForV2ColonFree(t *testing.T) {
	t.Parallel()

	const colonFree = "pat_token_abc"

	if _, err := resolveAuth("3", colonFree); err == nil {
		t.Errorf("resolveAuth(3, colon-free) = nil error, want ErrTrackerAuth")
	}
	if h, err := resolveAuth("2", colonFree); err != nil || h != "Bearer "+colonFree {
		t.Errorf("resolveAuth(2, colon-free) = (%q, %v), want (Bearer ..., nil)", h, err)
	}

	for _, version := range []string{"2", "3"} {
		if _, err := resolveAuth(version, "user:"); err == nil {
			t.Errorf("resolveAuth(%s, trailing-colon) = nil error, want ErrTrackerAuth (guard must not relax for Basic)", version)
		}
	}
}

// --- Host / version consistency guard, reject arm (AC11) ---

func TestNewJiraAdapter_HostVersionGuard_RejectCloudV2(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{
		"endpoint":    "https://acme.atlassian.net",
		"api_key":     "user@test.com:tok",
		"project":     "P",
		"api_version": "2",
	}

	a, err := NewJiraAdapter(cfg)
	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	if a != nil {
		t.Error("adapter should be nil when a Cloud host is combined with api_version 2")
	}

	var te *domain.TrackerError
	asTrackerError(t, err, &te)
	if !strings.Contains(te.Message, "acme.atlassian.net") {
		t.Errorf("TrackerError.Message = %q, want to name the host", te.Message)
	}
}

// TestNewJiraAdapter_HostVersionGuard_RejectUnparseableEndpoint verifies
// that an endpoint without a scheme and host is rejected at construction
// with ErrTrackerPayload and a nil adapter. The guard runs for both API
// versions, so the cases cover v2 and v3.
func TestNewJiraAdapter_HostVersionGuard_RejectUnparseableEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		apiKey   string
		version  string
	}{
		{name: "scheme-less host on v3", endpoint: "not-a-url", apiKey: "user@test.com:tok", version: "3"},
		{name: "scheme-less host on v2", endpoint: "not-a-url", apiKey: "pat_token_abc", version: "2"},
		{name: "malformed scheme on v3", endpoint: "://bad", apiKey: "user@test.com:tok", version: "3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := map[string]any{
				"endpoint":    tt.endpoint,
				"api_key":     tt.apiKey,
				"project":     "P",
				"api_version": tt.version,
			}

			a, err := NewJiraAdapter(cfg)
			assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			if a != nil {
				t.Error("adapter should be nil when the endpoint is not a URL with a scheme and host")
			}
		})
	}
}

// TestNewJiraAdapter_HostVersionGuard_ConsistentCombos verifies the two
// consistent (host, version) pairs construct successfully.
func TestNewJiraAdapter_HostVersionGuard_ConsistentCombos(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		apiKey   string
		version  string
	}{
		{name: "cloud host plus v3", endpoint: "https://acme.atlassian.net", apiKey: "user@test.com:tok", version: "3"},
		{name: "self-hosted host plus v2", endpoint: "https://jira.internal.example.com", apiKey: "pat_token_abc", version: "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := map[string]any{
				"endpoint":    tt.endpoint,
				"api_key":     tt.apiKey,
				"project":     "P",
				"api_version": tt.version,
			}
			if _, err := NewJiraAdapter(cfg); err != nil {
				t.Fatalf("NewJiraAdapter(%s) unexpected error: %v", tt.name, err)
			}
		})
	}
}

// TestNewJiraAdapter_HostVersionGuard_WarnArm covers the warn arm: a
// non-Cloud, non-loopback host with api_version 3 constructs
// successfully and emits a single warning that carries the host
// attribute and never the api_key. It mutates the default slog logger,
// so it must not run in parallel (no t.Parallel anywhere in its chain)
// and must restore the prior default.
func TestNewJiraAdapter_HostVersionGuard_WarnArm(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	cfg := map[string]any{
		"endpoint":    "https://jira.internal.example.com",
		"api_key":     "user@test.com:secret_tok",
		"project":     "P",
		"api_version": "3",
	}

	a, err := NewJiraAdapter(cfg)
	if err != nil {
		t.Fatalf("NewJiraAdapter (warn arm) unexpected error: %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil; warn arm must not block construction")
	}

	output := buf.String()
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("log output = %q, want a WARN line", output)
	}
	if !strings.Contains(output, "host=jira.internal.example.com") {
		t.Errorf("log output = %q, want the host attribute carrying the endpoint host", output)
	}
	if strings.Contains(output, "secret_tok") {
		t.Errorf("log output leaked api_key: %q", output)
	}
}

// TestNewJiraAdapter_HostVersionGuard_WarnArmLocalSuppressed covers the
// warn-arm exception: a loopback or localhost endpoint with api_version
// 3 is a test or local-dev target, never a real Server / Data Center
// instance, so construction succeeds and emits no warn line. It mutates
// the default slog logger, so it must not run in parallel (no t.Parallel
// anywhere in its chain) and must restore the prior default.
func TestNewJiraAdapter_HostVersionGuard_WarnArmLocalSuppressed(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "loopback IP", endpoint: srv.URL},
		{name: "localhost", endpoint: "http://localhost:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

			cfg := map[string]any{
				"endpoint":    tt.endpoint,
				"api_key":     "user@test.com:secret_tok",
				"project":     "P",
				"api_version": "3",
			}

			a, err := NewJiraAdapter(cfg)
			if err != nil {
				t.Fatalf("NewJiraAdapter (local endpoint) unexpected error: %v", err)
			}
			if a == nil {
				t.Fatal("adapter is nil; a local endpoint must not block construction")
			}

			output := buf.String()
			if strings.Contains(output, "level=WARN") {
				t.Errorf("log output = %q, want no WARN line for a local endpoint", output)
			}
			if strings.Contains(output, "secret_tok") {
				t.Errorf("log output leaked api_key: %q", output)
			}
		})
	}
}

// TestNewJiraAdapter_HostVersionGuard_NoWarnForConsistent verifies the
// consistent combos emit no warn line. Serialized for the same reason as
// the warn-arm test.
func TestNewJiraAdapter_HostVersionGuard_NoWarnForConsistent(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	tests := []struct {
		name     string
		endpoint string
		apiKey   string
		version  string
	}{
		{name: "cloud host plus v3", endpoint: "https://acme.atlassian.net", apiKey: "user@test.com:tok", version: "3"},
		{name: "self-hosted host plus v2", endpoint: "https://jira.internal.example.com", apiKey: "pat_token_abc", version: "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

			cfg := map[string]any{
				"endpoint":    tt.endpoint,
				"api_key":     tt.apiKey,
				"project":     "P",
				"api_version": tt.version,
			}
			if _, err := NewJiraAdapter(cfg); err != nil {
				t.Fatalf("NewJiraAdapter(%s) unexpected error: %v", tt.name, err)
			}
			if strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("log output = %q, want no WARN line for a consistent combo", buf.String())
			}
		})
	}
}

// --- v2 offset search pagination (AC1, AC3) ---

func TestPaginatedSearchV2_SinglePage(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotStartAt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotStartAt = r.URL.Query().Get("startAt")
		w.Write(loadFixture(t, "search_v2_single_page.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, v2Config(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues (v2): %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	if gotPath != "/rest/api/2/search" {
		t.Errorf("request path = %q, want /rest/api/2/search", gotPath)
	}
	if gotStartAt != "0" {
		t.Errorf("startAt = %q, want 0 on first request", gotStartAt)
	}
	if issues[0].Identifier != "SRV-1" {
		t.Errorf("issues[0].Identifier = %q, want SRV-1", issues[0].Identifier)
	}
	if issues[0].Comments != nil {
		t.Error("Comments should be nil for v2 search results")
	}
}

func TestPaginatedSearchV2_MultiPage(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "search_v2_multi_page_1.json")
	page2 := loadFixture(t, "search_v2_multi_page_2.json")

	var (
		callCount   atomic.Int32
		seenStartAt []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		seenStartAt = append(seenStartAt, r.URL.Query().Get("startAt"))
		if r.URL.Path != "/rest/api/2/search" {
			t.Errorf("request path = %q, want /rest/api/2/search", r.URL.Path)
		}
		if n == 1 {
			w.Write(page1) //nolint:errcheck // test helper
		} else {
			w.Write(page2) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, v2Config(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues (v2 multi-page): %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("len = %d, want 3 across 2 pages", len(issues))
	}
	if got := callCount.Load(); got != 2 {
		t.Errorf("request count = %d, want 2", got)
	}
	wantIDs := []string{"SRV-11", "SRV-12", "SRV-13"}
	for i, want := range wantIDs {
		if issues[i].Identifier != want {
			t.Errorf("issues[%d].Identifier = %q, want %q", i, issues[i].Identifier, want)
		}
	}
	// startAt must advance from 0 to the first page length (2), proving
	// the offset cursor moves and the loop terminates on the partial page.
	if len(seenStartAt) != 2 || seenStartAt[0] != "0" || seenStartAt[1] != "2" {
		t.Errorf("startAt sequence = %v, want [0 2]", seenStartAt)
	}
}

// TestPaginatedSearchV2_FinalFullPageTerminates guards against an
// infinite loop when the last page is exactly full: total equals the
// number of issues returned, so startAt + len >= total must terminate.
func TestPaginatedSearchV2_FinalFullPageTerminates(t *testing.T) {
	t.Parallel()

	// total == 2 and the single page returns 2 issues: startAt(0)+2 >= 2.
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Write(loadFixture(t, "search_v2_single_page.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, v2Config(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("request count = %d, want 1 (loop must terminate on a full final page)", got)
	}
}

// TestPaginatedSearchV2_DecodeError maps a malformed v2 search body to
// ErrTrackerPayload.
func TestPaginatedSearchV2_DecodeError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{invalid json`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, v2Config(srv.URL))
	_, err := a.FetchCandidateIssues(context.Background())
	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
}

// --- v2 comment write (AC5) ---

func TestCommentIssue_V2RawStringBody(t *testing.T) {
	t.Parallel()

	var (
		gotMethod string
		gotPath   string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	a := mustAdapter(t, v2Config(srv.URL))
	const text = "h2. Heading\n\n*bold* body"
	if err := a.CommentIssue(context.Background(), "SRV-1", text); err != nil {
		t.Fatalf("CommentIssue (v2): %v", err)
	}

	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/rest/api/2/issue/SRV-1/comment" {
		t.Errorf("path = %q, want /rest/api/2/issue/SRV-1/comment", gotPath)
	}

	// The v2 body is a raw string under "body", not an ADF document.
	var payload struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	var bodyStr string
	if err := json.Unmarshal(payload.Body, &bodyStr); err != nil {
		t.Fatalf("v2 body should be a raw JSON string, got %s: %v", payload.Body, err)
	}
	if bodyStr != text {
		t.Errorf("body = %q, want verbatim %q", bodyStr, text)
	}
}

// TestCommentPayload_ByVersion asserts the payload shape selector
// directly: v2 yields a raw-string body, v3 yields an ADF document.
func TestCommentPayload_ByVersion(t *testing.T) {
	t.Parallel()

	v2 := commentPayload("2", "hello")
	m, ok := v2.(map[string]any)
	if !ok {
		t.Fatalf("commentPayload(2) type = %T, want map[string]any", v2)
	}
	if m["body"] != "hello" {
		t.Errorf("commentPayload(2)[body] = %v, want %q", m["body"], "hello")
	}

	// v3 must produce an ADF document, never a raw-string body.
	v3 := commentPayload("3", "hello")
	raw, err := json.Marshal(v3)
	if err != nil {
		t.Fatalf("marshal v3 payload: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"doc"`) {
		t.Errorf("commentPayload(3) = %s, want an ADF document with type doc", raw)
	}
}

// --- Transport auth header and no-secret-logging (AC4, AC10) ---

func TestV2Transport_AuthHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apiKey     string
		wantHeader string
		secret     string // must never appear raw in a header beyond the encoded/bearer form check
	}{
		{name: "colon-free PAT to Bearer", apiKey: "pat_token_abc", wantHeader: "Bearer pat_token_abc", secret: "pat_token_abc"},
		{
			name:       "user:password to Basic",
			apiKey:     "svcuser:s3cret",
			wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("svcuser:s3cret")),
			secret:     "s3cret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.Write(loadFixture(t, "search_v2_single_page.json")) //nolint:errcheck // test helper
			}))
			defer srv.Close()

			cfg := v2Config(srv.URL)
			cfg["api_key"] = tt.apiKey
			a := mustAdapter(t, cfg)
			if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
				t.Fatalf("FetchCandidateIssues: %v", err)
			}

			if gotAuth != tt.wantHeader {
				t.Errorf("Authorization = %q, want %q", gotAuth, tt.wantHeader)
			}
			// For Basic, the raw password must be base64-encoded, never
			// present in cleartext in the header.
			if strings.HasPrefix(tt.wantHeader, "Basic ") && strings.Contains(gotAuth, tt.secret) {
				t.Errorf("Authorization header leaked the raw secret: %q", gotAuth)
			}
		})
	}
}

// TestV2Transport_NoSecretInLogs runs a full v2 fetch under a captured
// default logger and asserts neither the api_key nor the constructed
// Authorization header is logged anywhere.
func TestV2Transport_NoSecretInLogs(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(loadFixture(t, "search_v2_single_page.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	const secret = "pat_secret_value_xyz"
	cfg := v2Config(srv.URL)
	cfg["api_key"] = secret
	a := mustAdapter(t, cfg)
	if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, secret) {
		t.Errorf("log output leaked the api_key %q: %q", secret, output)
	}
	if strings.Contains(output, "Bearer "+secret) {
		t.Errorf("log output leaked the Authorization header: %q", output)
	}
}

// --- v2 HTTP error category mapping (AC8) ---

func TestV2_HTTPErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantKind domain.TrackerErrorKind
	}{
		{"401 unauthorized to auth", http.StatusUnauthorized, domain.ErrTrackerAuth},
		{"403 forbidden to auth", http.StatusForbidden, domain.ErrTrackerAuth},
		{"500 server error to transport", http.StatusInternalServerError, domain.ErrTrackerTransport},
		{"503 server error to transport", http.StatusServiceUnavailable, domain.ErrTrackerTransport},
		{"429 rate limited to API", http.StatusTooManyRequests, domain.ErrTrackerAPI},
		{"404 not found to not-found", http.StatusNotFound, domain.ErrTrackerNotFound},
		{"400 bad request to payload", http.StatusBadRequest, domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			a := mustAdapter(t, v2Config(srv.URL))
			_, err := a.FetchCandidateIssues(context.Background())
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

// --- v3 regression: a config without api_version keeps v3 behavior (AC6) ---

func TestV3Regression_NoAPIVersion(t *testing.T) {
	t.Parallel()

	var (
		gotSearchPath string
		gotAuth       string
		gotToken      atomic.Int32
	)
	page1 := loadFixture(t, "search_multi_page_1.json") // carries nextPageToken=cursor_abc
	page2 := loadFixture(t, "search_multi_page_2.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSearchPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Query().Get("nextPageToken") == "cursor_abc" {
			gotToken.Add(1)
			w.Write(page2) //nolint:errcheck // test helper
			return
		}
		w.Write(page1) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	// validConfig uses email:token (Basic) and omits api_version entirely.
	a := mustAdapter(t, validConfig(srv.URL))

	if a.apiVersion != "3" {
		t.Errorf("apiVersion = %q, want default 3", a.apiVersion)
	}
	if a.basePath != "/rest/api/3" {
		t.Errorf("basePath = %q, want /rest/api/3", a.basePath)
	}

	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	// Cursor pagination: the second page is reached via nextPageToken.
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2 across 2 cursor pages", len(issues))
	}
	if gotToken.Load() != 1 {
		t.Errorf("nextPageToken follow-up requests = %d, want 1 (cursor pagination)", gotToken.Load())
	}
	if gotSearchPath != "/rest/api/3/search/jql" {
		t.Errorf("search path = %q, want /rest/api/3/search/jql", gotSearchPath)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user@test.com:api_token_123"))
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q, want Basic email:token", gotAuth)
	}
}

// TestV3Regression_ADFFlattening confirms the v3 default path flattens
// the ADF description rather than carrying raw structure.
func TestV3Regression_ADFFlattening(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comment") {
			w.Write(loadFixture(t, "comments_empty.json")) //nolint:errcheck // test helper
			return
		}
		w.Write(loadFixture(t, "issue_detail.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issue, err := a.FetchIssueByID(context.Background(), "PROJ-5")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if strings.Contains(issue.Description, `"type"`) || strings.Contains(issue.Description, `"content"`) {
		t.Errorf("Description = %q, want flattened ADF text without raw JSON structure", issue.Description)
	}
	if !strings.Contains(issue.Description, "Refactor the persistence layer:") {
		t.Errorf("Description = %q, want flattened ADF prose", issue.Description)
	}
}

// TestV3Regression_ColonGuardPreserved confirms the v3 colon guard still
// rejects email: and :token at construction (AC6).
func TestV3Regression_ColonGuardPreserved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		apiKey string
	}{
		{name: "trailing colon (empty token)", apiKey: "email:"},
		{name: "leading colon (empty user)", apiKey: ":token"},
		{name: "colon-free on v3", apiKey: "noatsign"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := map[string]any{
				"endpoint": "https://x.atlassian.net",
				"api_key":  tt.apiKey,
				"project":  "P",
				// api_version omitted: defaults to v3.
			}
			a, err := NewJiraAdapter(cfg)
			assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
			if a != nil {
				t.Error("adapter should be nil on a malformed v3 credential")
			}
		})
	}
}

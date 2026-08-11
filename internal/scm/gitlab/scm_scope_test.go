package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- VerifyAutoMergeScopes: scope report (R25, R27, AC8) ---

func TestVerifyAutoMergeScopes_ScopeReport(t *testing.T) {
	t.Parallel()

	t.Run("scopes carrying api yields the scope list with an empty missing", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "token_self.json")
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if len(scopes) == 0 {
			t.Errorf("VerifyAutoMergeScopes() scopes = %v, want non-empty", scopes)
		}
		if len(missing) != 0 {
			t.Errorf("VerifyAutoMergeScopes() missing = %v, want empty", missing)
		}
	})

	t.Run("read_api-only scopes yields missing=[api] with a nil error", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "token_self_read_api_only.json")
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil {
			t.Errorf("VerifyAutoMergeScopes() scopes = %v, want nil", scopes)
		}
		if len(missing) != 1 || missing[0] != "api" {
			t.Errorf("VerifyAutoMergeScopes() missing = %v, want [api]", missing)
		}
	})
}

// --- VerifyAutoMergeScopes: fail-open sentinel (R26, R26a) ---

func TestVerifyAutoMergeScopes_FailOpen(t *testing.T) {
	t.Parallel()

	t.Run("a granular token yields the fail-open sentinel", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "token_self_granular.json")
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
		}
	})

	t.Run("an empty scopes array yields the fail-open sentinel", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "token_self_empty_scopes.json")
		srv := serveJSON(t, fixture)
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
		}
	})

	t.Run("an undecodable body yields the fail-open sentinel", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
		}
	})

	t.Run("a 404 on the introspection route yields the fail-open sentinel and logs one WARN naming the route", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_404_project.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
		}

		logOutput := buf.String()
		if !strings.Contains(logOutput, "gitlab token introspection route unavailable") {
			t.Errorf("log output = %q, want it to contain %q", logOutput, "gitlab token introspection route unavailable")
		}
	})
}

// --- VerifyAutoMergeScopes: error classes (R29, AC8) ---

func TestVerifyAutoMergeScopes_ErrorClasses(t *testing.T) {
	t.Parallel()

	t.Run("a 401 yields ErrSCMAuth", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_401.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, err), want (nil, nil, err)", scopes, missing)
		}
	})

	t.Run("a 500 yields ErrSCMTransport", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_500.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMTransport)
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, err), want (nil, nil, err)", scopes, missing)
		}
	})
}

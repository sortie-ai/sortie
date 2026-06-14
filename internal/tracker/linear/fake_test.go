package linear

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fakeResponse is one queued reply from the fake client: either a raw body or a
// transport-level error, optionally carrying response headers.
type fakeResponse struct {
	body    []byte
	headers http.Header
	err     error
}

// recordedCall captures a single Execute invocation for later assertion.
type recordedCall struct {
	query     string
	variables map[string]any
}

// fakeGraphQLClient is a stateful in-package [graphQLClient] keyed by a query
// substring. Each match pops the next queued response for that key, so a
// paginated operation queues one response per page. It records every call so
// tests can assert the query, the cursor, and the variables sent.
type fakeGraphQLClient struct {
	responses map[string][]fakeResponse
	calls     []recordedCall
}

var _ graphQLClient = (*fakeGraphQLClient)(nil)

// newFakeClient constructs an empty fake client.
func newFakeClient() *fakeGraphQLClient {
	return &fakeGraphQLClient{responses: map[string][]fakeResponse{}}
}

// queueBody appends a successful response body for queries containing key.
func (f *fakeGraphQLClient) queueBody(key string, body []byte) {
	f.responses[key] = append(f.responses[key], fakeResponse{body: body})
}

// queueResponse appends an arbitrary response (body, headers, or error) for
// queries containing key.
func (f *fakeGraphQLClient) queueResponse(key string, resp fakeResponse) {
	f.responses[key] = append(f.responses[key], resp)
}

// Execute matches query against the queued keys, records the call, and returns
// the next queued response. An unmatched or exhausted query fails the test
// indirectly by returning a payload error.
func (f *fakeGraphQLClient) Execute(_ context.Context, query string, variables map[string]any) ([]byte, http.Header, error) {
	f.calls = append(f.calls, recordedCall{query: query, variables: cloneVars(variables)})

	for key, queued := range f.responses {
		if !strings.Contains(query, key) || len(queued) == 0 {
			continue
		}
		resp := queued[0]
		f.responses[key] = queued[1:]
		return resp.body, resp.headers, resp.err
	}

	return nil, nil, &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: "fake: no queued response for query",
	}
}

// callsFor returns the recorded calls whose query contains key.
func (f *fakeGraphQLClient) callsFor(key string) []recordedCall {
	var matched []recordedCall
	for _, call := range f.calls {
		if strings.Contains(call.query, key) {
			matched = append(matched, call)
		}
	}
	return matched
}

// cloneVars makes a shallow copy of a variables map so a later in-place cursor
// mutation by the paginator does not rewrite a recorded call.
func cloneVars(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// --- Logger capture ---

// newTextLogger returns a logger backed by w at Debug level so tests can assert
// WARN output without depending on slog.Default().
func newTextLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- Fixture loading ---

// loadFixture reads a fixture body from testdata.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// --- Preflight seeding ---

// seedPreflight queues the viewer and team-states responses a successful
// construction requires, so per-test setup only queues the read responses.
func seedPreflight(f *fakeGraphQLClient, t *testing.T) {
	t.Helper()
	f.queueBody("query Me", loadFixture(t, "viewer.json"))
	f.queueBody("query TeamStates", loadFixture(t, "team_states.json"))
}

// newTestAdapter constructs a [LinearAdapter] through the offline seam with the
// default team-SOR config, no operator filter, and a nil logger, failing the
// test on a construction error.
func newTestAdapter(t *testing.T, f *fakeGraphQLClient) *LinearAdapter {
	t.Helper()
	return newTestAdapterWithFilter(t, f, nil)
}

// newTestAdapterWithFilter constructs a [LinearAdapter] through the offline seam
// with the default team-SOR config carrying the supplied parsed query filter and
// a nil logger, failing the test on a construction error. A nil queryFilter
// reproduces the unset passthrough behavior.
func newTestAdapterWithFilter(t *testing.T, f *fakeGraphQLClient, queryFilter map[string]any) *LinearAdapter {
	t.Helper()
	a, err := newAdapter(f, "SOR", defaultActiveStates, defaultTerminalStates, "In Review", queryFilter, nil)
	if err != nil {
		t.Fatalf("newAdapter: %v", err)
	}
	return a.(*LinearAdapter)
}

// --- Error assertions ---

// assertTrackerErrorKind asserts that err is a [*domain.TrackerError] with the
// expected kind.
func assertTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", want)
	}
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != want {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, want)
	}
}

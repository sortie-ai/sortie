package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// stubPage is the wire shape decoded by decodeStubBody in pagination tests.
type stubPage struct {
	Items    []string       `json:"items"`
	PageInfo linearPageInfo `json:"pageInfo"`
}

// stubBody marshals a page of string items with the given pagination state.
func stubBody(items []string, hasNext bool, endCursor string) []byte {
	raw, _ := json.Marshal(stubPage{
		Items:    items,
		PageInfo: linearPageInfo{HasNextPage: hasNext, EndCursor: endCursor},
	})
	return raw
}

// decodeStubBody unmarshals a [stubPage] for direct paginate tests.
func decodeStubBody(body []byte) ([]string, linearPageInfo, error) {
	var page stubPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, linearPageInfo{}, payloadError(err)
	}
	return page.Items, page.PageInfo, nil
}

// stubPageClient returns a sequence of pre-built page bodies, ignoring the
// query and variables, so paginate can be driven directly.
type stubPageClient struct {
	bodies [][]byte
	idx    int
}

func (s *stubPageClient) Execute(_ context.Context, _ string, _ map[string]any) ([]byte, http.Header, error) {
	if s.idx >= len(s.bodies) {
		return s.bodies[len(s.bodies)-1], nil, nil
	}
	body := s.bodies[s.idx]
	s.idx++
	return body, nil, nil
}

// countingClient returns a body that always reports another page with a valid
// cursor, counting how many times it was called.
type countingClient struct {
	calls int
	body  []byte
}

func (c *countingClient) Execute(_ context.Context, _ string, _ map[string]any) ([]byte, http.Header, error) {
	c.calls++
	return c.body, nil, nil
}

func decodeStub(body []byte) ([]string, linearPageInfo, error) {
	return decodeStubBody(body)
}

func TestPaginate(t *testing.T) {
	t.Parallel()

	t.Run("stops when hasNextPage is false", func(t *testing.T) {
		t.Parallel()

		client := &stubPageClient{bodies: [][]byte{
			stubBody([]string{"a", "b"}, true, "cursor-1"),
			stubBody([]string{"c"}, false, ""),
		}}

		items, err := paginate(context.Background(), client, "q", map[string]any{}, decodeStub, nil)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}

		want := []string{"a", "b", "c"}
		if strings.Join(items, ",") != strings.Join(want, ",") {
			t.Errorf("paginate items = %v, want %v", items, want)
		}
		if client.idx != 2 {
			t.Errorf("Execute call count = %d, want 2", client.idx)
		}
	})

	t.Run("missing end cursor returns missing-cursor error", func(t *testing.T) {
		t.Parallel()

		client := &stubPageClient{bodies: [][]byte{
			stubBody([]string{"a"}, true, ""),
		}}

		_, err := paginate(context.Background(), client, "q", map[string]any{}, decodeStub, nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerMissingCursor)
	})

	t.Run("MaxPages bound logs WARN and returns accumulated items", func(t *testing.T) {
		t.Parallel()

		client := &countingClient{body: stubBody([]string{"x"}, true, "always-more")}

		var buf strings.Builder
		log := newTextLogger(&buf)

		items, err := paginate(context.Background(), client, "q", map[string]any{}, decodeStub, log)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}

		if client.calls != maxPages {
			t.Errorf("Execute call count = %d, want maxPages (%d)", client.calls, maxPages)
		}
		if len(items) != maxPages {
			t.Errorf("accumulated items = %d, want %d", len(items), maxPages)
		}
		output := buf.String()
		if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "pagination limit reached") {
			t.Errorf("expected pagination-limit WARN\noutput: %s", output)
		}
	})

	t.Run("propagates transport error from the client", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueResponse("q", fakeResponse{err: &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}})

		_, err := paginate(context.Background(), f, "q", map[string]any{}, decodeStub, nil)

		assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
	})

	t.Run("honors a seeded after cursor on the first request", func(t *testing.T) {
		t.Parallel()

		f := newFakeClient()
		f.queueBody("q", stubBody([]string{"c"}, false, ""))

		_, err := paginate(context.Background(), f, "q", map[string]any{"after": "seed-cursor"}, decodeStub, nil)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}

		calls := f.callsFor("q")
		if len(calls) != 1 {
			t.Fatalf("Execute call count = %d, want 1", len(calls))
		}
		if calls[0].variables["after"] != "seed-cursor" {
			t.Errorf("first after = %v, want %q (seeded cursor, not reset to page 1)", calls[0].variables["after"], "seed-cursor")
		}
	})
}

func TestDecodeIssuesPage(t *testing.T) {
	t.Parallel()

	t.Run("valid page", func(t *testing.T) {
		t.Parallel()

		nodes, info, err := decodeIssuesPage(loadFixture(t, "candidates_page1.json"))
		if err != nil {
			t.Fatalf("decodeIssuesPage: %v", err)
		}
		if len(nodes) != 2 {
			t.Errorf("nodes len = %d, want 2", len(nodes))
		}
		if !info.HasNextPage {
			t.Error("HasNextPage = false, want true")
		}
		if info.EndCursor == "" {
			t.Error("EndCursor empty, want non-empty")
		}
	})

	t.Run("malformed body is a payload error", func(t *testing.T) {
		t.Parallel()

		_, _, err := decodeIssuesPage([]byte("{not json"))

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("errors array surfaces classified error", func(t *testing.T) {
		t.Parallel()

		_, _, err := decodeIssuesPage(loadFixture(t, "issue_not_found.json"))

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

func TestDecodeCommentsPage(t *testing.T) {
	t.Parallel()

	t.Run("valid page", func(t *testing.T) {
		t.Parallel()

		nodes, info, err := decodeCommentsPage(loadFixture(t, "comments_continuation.json"))
		if err != nil {
			t.Fatalf("decodeCommentsPage: %v", err)
		}
		if len(nodes) != 1 {
			t.Errorf("nodes len = %d, want 1", len(nodes))
		}
		if info.HasNextPage {
			t.Error("HasNextPage = true, want false")
		}
	})

	t.Run("null issue mid-pagination is not found", func(t *testing.T) {
		t.Parallel()

		_, _, err := decodeCommentsPage([]byte(`{"data":{"issue":null}}`))

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("errors array surfaces classified error", func(t *testing.T) {
		t.Parallel()

		_, _, err := decodeCommentsPage(loadFixture(t, "issue_not_found.json"))

		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

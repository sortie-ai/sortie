package httpkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// testTokenPage is the JSON response shape used in token-paginator tests.
type testTokenPage struct {
	Items []int  `json:"items"`
	Next  string `json:"next"`
}

func decodeIntsWithToken(body []byte) ([]int, string, error) {
	var p testTokenPage
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, "", err
	}
	return p.Items, p.Next, nil
}

func decodeInts(body []byte) ([]int, error) {
	var items []int
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func serveTokenPage(w http.ResponseWriter, items []int, next string) {
	page := testTokenPage{Items: items, Next: next}
	data, _ := json.Marshal(page) //nolint:errcheck // basic types, marshal never fails
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func serveLinkPage(w http.ResponseWriter, items []int, linkHeader string) {
	data, _ := json.Marshal(items) //nolint:errcheck // basic types, marshal never fails
	if linkHeader != "" {
		w.Header().Set("Link", linkHeader)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func TestTokenPaginator_order(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch int(reqCount.Add(1)) {
		case 1:
			serveTokenPage(w, []int{1, 2}, "page2")
		case 2:
			serveTokenPage(w, []int{3, 4}, "")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decodeIntsWithToken, PaginatorOptions{})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []int{1, 2, 3, 4}
	if len(items) != len(want) {
		t.Fatalf("All() = %v, want %v", items, want)
	}
	for i, v := range want {
		if items[i] != v {
			t.Errorf("All()[%d] = %d, want %d", i, items[i], v)
		}
	}
}

func TestTokenPaginator_singlePage(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqCount.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		serveTokenPage(w, []int{1}, "")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decodeIntsWithToken, PaginatorOptions{})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(items) != 1 || items[0] != 1 {
		t.Errorf("All() = %v, want [1]", items)
	}
	if n := int(reqCount.Load()); n != 1 {
		t.Errorf("request count = %d, want 1", n)
	}
}

func TestTokenPaginator_pageCap(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch int(reqCount.Add(1)) {
		case 1:
			serveTokenPage(w, []int{1}, "page2")
		case 2:
			serveTokenPage(w, []int{2}, "page3")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var limitCalled atomic.Int32
	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decodeIntsWithToken, PaginatorOptions{
		MaxPages: 2,
		OnLimitReached: func(limit int) {
			limitCalled.Add(1)
		},
	})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("All() len = %d, want 2", len(items))
	}
	if n := int(limitCalled.Load()); n != 1 {
		t.Errorf("OnLimitReached calls = %d, want 1", n)
	}
	if n := int(reqCount.Load()); n != 2 {
		t.Errorf("request count = %d, want 2", n)
	}
}

func TestTokenPaginator_noItems(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveTokenPage(w, []int{}, "")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decodeIntsWithToken, PaginatorOptions{})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if items == nil {
		t.Fatal("All() = nil, want non-nil empty slice")
	}
	if len(items) != 0 {
		t.Errorf("All() = %v, want empty", items)
	}
}

func TestTokenPaginator_decodeError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("decode-error")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not decodable as token page")
	}))
	defer srv.Close()

	decode := func(_ []byte) ([]int, string, error) {
		return nil, "", sentinel
	}

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decode, PaginatorOptions{})

	items, err := p.All(context.Background())
	if items != nil {
		t.Errorf("All() items = %v, want nil", items)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("All() err = %v, want %v", err, sentinel)
	}
}

func TestTokenPaginator_midFlightErrorDropsPartial(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("page2-error")
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch int(reqCount.Add(1)) {
		case 1:
			serveTokenPage(w, []int{1, 2}, "page2")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{
		BaseURL: srv.URL,
		ClassifyError: func(resp *http.Response, method, path string) error {
			return fmt.Errorf("%w: %d", sentinel, resp.StatusCode)
		},
	})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decodeIntsWithToken, PaginatorOptions{})

	items, err := p.All(context.Background())
	if items != nil {
		t.Errorf("All() items = %v, want nil after mid-flight error", items)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("All() err = %v, want %v", err, sentinel)
	}
}

func TestTokenPaginator_cancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"items":[],"next":""}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewTokenPaginator(c, "/items", nil, "cursor", decodeIntsWithToken, PaginatorOptions{})

	items, err := p.All(ctx)
	if items != nil {
		t.Errorf("All() items = %v, want nil", items)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("All() err = %v, want %v", err, context.Canceled)
	}
}

func TestLinkPaginator_order(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p1":
			serveLinkPage(w, []int{1, 2}, fmt.Sprintf(`<%s/p2>; rel="next"`, srv.URL))
		case "/p2":
			serveLinkPage(w, []int{3, 4}, "")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewLinkPaginator(c, "/p1", nil, decodeInts, PaginatorOptions{})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []int{1, 2, 3, 4}
	if len(items) != len(want) {
		t.Fatalf("All() = %v, want %v", items, want)
	}
	for i, v := range want {
		if items[i] != v {
			t.Errorf("All()[%d] = %d, want %d", i, items[i], v)
		}
	}
}

func TestLinkPaginator_malformedLink(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		serveLinkPage(w, []int{1, 2}, "not a proper link header")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewLinkPaginator(c, "/items", nil, decodeInts, PaginatorOptions{})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(items) != 2 || items[0] != 1 || items[1] != 2 {
		t.Errorf("All() = %v, want [1 2]", items)
	}
	if n := int(reqCount.Load()); n != 1 {
		t.Errorf("request count = %d, want 1", n)
	}
}

func TestLinkPaginator_pageCap(t *testing.T) {
	t.Parallel()

	var reqCount atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		switch r.URL.Path {
		case "/p1":
			serveLinkPage(w, []int{1, 2}, fmt.Sprintf(`<%s/p2>; rel="next"`, srv.URL))
		default:
			// /p2 and beyond must not be reached when MaxPages=1.
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var limitCalled atomic.Int32
	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewLinkPaginator(c, "/p1", nil, decodeInts, PaginatorOptions{
		MaxPages: 1,
		OnLimitReached: func(limit int) {
			limitCalled.Add(1)
		},
	})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("All() len = %d, want 2", len(items))
	}
	if n := int(limitCalled.Load()); n != 1 {
		t.Errorf("OnLimitReached calls = %d, want 1", n)
	}
	if n := int(reqCount.Load()); n != 1 {
		t.Errorf("request count = %d, want 1", n)
	}
}

func TestLinkPaginator_noItems(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveLinkPage(w, []int{}, "")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewLinkPaginator(c, "/items", nil, decodeInts, PaginatorOptions{})

	items, err := p.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if items == nil {
		t.Fatal("All() = nil, want non-nil empty slice")
	}
	if len(items) != 0 {
		t.Errorf("All() = %v, want empty", items)
	}
}

func TestLinkPaginator_midFlightErrorDropsPartial(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("page2-error")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p1":
			serveLinkPage(w, []int{1, 2}, fmt.Sprintf(`<%s/p2>; rel="next"`, srv.URL))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{
		BaseURL: srv.URL,
		ClassifyError: func(resp *http.Response, method, path string) error {
			return fmt.Errorf("%w: %d", sentinel, resp.StatusCode)
		},
	})
	p := NewLinkPaginator(c, "/p1", nil, decodeInts, PaginatorOptions{})

	items, err := p.All(context.Background())
	if items != nil {
		t.Errorf("All() items = %v, want nil after mid-flight error", items)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("All() err = %v, want %v", err, sentinel)
	}
}

func TestLinkPaginator_cancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "[]")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})
	p := NewLinkPaginator(c, "/items", nil, decodeInts, PaginatorOptions{})

	items, err := p.All(ctx)
	if items != nil {
		t.Errorf("All() items = %v, want nil", items)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("All() err = %v, want %v", err, context.Canceled)
	}
}

// buildIntRange returns a JSON array of n consecutive integers starting at
// start, modeling one page-number page without hand-authoring a repetitive
// fixture.
func buildIntRange(t *testing.T, start, n int) []byte {
	t.Helper()
	values := make([]int, n)
	for i := range n {
		values[i] = start + i
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal int range: %v", err)
	}
	return data
}

func TestPagePaginator(t *testing.T) {
	t.Parallel()

	t.Run("accumulates across a full page and a short page", func(t *testing.T) {
		t.Parallel()

		const pageSize = 50
		page1 := buildIntRange(t, 1, pageSize)
		page2 := buildIntRange(t, pageSize+1, 2)

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				if got := r.URL.Query().Get("page"); got != "1" {
					t.Errorf("page 1 request: page = %q, want %q", got, "1")
				}
				if got := r.URL.Query().Get("limit"); got != "50" {
					t.Errorf("page 1 request: limit = %q, want %q", got, "50")
				}
				_, _ = w.Write(page1)
				return
			}
			if got := r.URL.Query().Get("page"); got != "2" {
				t.Errorf("page 2 request: page = %q, want %q", got, "2")
			}
			if got := r.URL.Query().Get("limit"); got != "50" {
				t.Errorf("page 2 request: limit = %q, want %q", got, "50")
			}
			_, _ = w.Write(page2)
		}))
		defer srv.Close()

		c := mustClient(t, ClientOptions{BaseURL: srv.URL})
		p := NewPagePaginator(c, "/items", nil, decodeInts, PageOptions{PageParam: "page", SizeParam: "limit", PageSize: pageSize})

		got, err := p.All(context.Background())
		if err != nil {
			t.Fatalf("All: unexpected error: %v", err)
		}
		if len(got) != pageSize+2 {
			t.Fatalf("All() len = %d, want %d", len(got), pageSize+2)
		}
		if got[0] != 1 || got[len(got)-1] != pageSize+2 {
			t.Errorf("All() = [%d...%d], want [1...%d]", got[0], got[len(got)-1], pageSize+2)
		}
		if n := requestCount.Load(); n != 2 {
			t.Errorf("request count = %d, want 2", n)
		}
	})

	t.Run("a single short page stops after one request", func(t *testing.T) {
		t.Parallel()

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[1,2,3]`))
		}))
		defer srv.Close()

		c := mustClient(t, ClientOptions{BaseURL: srv.URL})
		p := NewPagePaginator(c, "/items", nil, decodeInts, PageOptions{PageParam: "page", SizeParam: "limit", PageSize: 50})

		got, err := p.All(context.Background())
		if err != nil {
			t.Fatalf("All: unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("All() len = %d, want 3", len(got))
		}
		if n := requestCount.Load(); n != 1 {
			t.Errorf("request count = %d, want 1 (a page shorter than the limit must stop pagination)", n)
		}
	})

	t.Run("MaxPages caps the walk and invokes OnLimitReached exactly once", func(t *testing.T) {
		t.Parallel()

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(buildIntRange(t, int(n-1)*3+1, 3))
		}))
		defer srv.Close()

		var limitCalled atomic.Int32
		c := mustClient(t, ClientOptions{BaseURL: srv.URL})
		p := NewPagePaginator(c, "/items", nil, decodeInts, PageOptions{
			PageParam: "page",
			SizeParam: "limit",
			PageSize:  3,
			MaxPages:  2,
			OnLimitReached: func(limit int) {
				limitCalled.Add(1)
			},
		})

		got, err := p.All(context.Background())
		if err != nil {
			t.Fatalf("All: unexpected error: %v", err)
		}
		if len(got) != 6 {
			t.Errorf("All() len = %d, want 6 (two full pages, then the cap stops the walk)", len(got))
		}
		if n := requestCount.Load(); n != 2 {
			t.Errorf("request count = %d, want 2 (MaxPages must stop further requests)", n)
		}
		if n := limitCalled.Load(); n != 1 {
			t.Errorf("OnLimitReached calls = %d, want 1", n)
		}
	})

	t.Run("a page reporting zero items returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()

		c := mustClient(t, ClientOptions{BaseURL: srv.URL})
		p := NewPagePaginator(c, "/items", nil, decodeInts, PageOptions{PageParam: "page", SizeParam: "limit", PageSize: 50})

		got, err := p.All(context.Background())
		if err != nil {
			t.Fatalf("All: unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("All() = nil, want non-nil empty slice")
		}
		if len(got) != 0 {
			t.Errorf("All() = %v, want empty", got)
		}
	})

	t.Run("a canceled context returns immediately with no request", func(t *testing.T) {
		t.Parallel()

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		c := mustClient(t, ClientOptions{BaseURL: srv.URL})
		p := NewPagePaginator(c, "/items", nil, decodeInts, PageOptions{PageParam: "page", SizeParam: "limit", PageSize: 50})

		items, err := p.All(ctx)
		// allPages checks ctx.Err() before issuing a request and returns it
		// unconverted, matching the token- and Link-mode cancellation
		// behavior: the error is the raw context error, not a classified
		// *domain.TrackerError, so a caller on the SCM boundary converts it
		// at its own call site (see the gitea package's own paginateSCM
		// test for that conversion).
		if items != nil {
			t.Errorf("All() items = %v, want nil", items)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("All() err = %v, want %v", err, context.Canceled)
		}
		if n := requestCount.Load(); n != 0 {
			t.Errorf("request count = %d, want 0 (a canceled context must be checked before the first request)", n)
		}
	})
}

func TestParseLinkRel(t *testing.T) {
	t.Parallel()

	multi := `<https://api.example.com/items?page=2>; rel="next", ` +
		`<https://api.example.com/items?page=1>; rel="prev", ` +
		`<https://api.example.com/items?page=1>; rel="first", ` +
		`<https://api.example.com/items?page=5>; rel="last"`

	tests := []struct {
		name   string
		header string
		rel    string
		want   string
	}{
		{"next relation", multi, "next", "https://api.example.com/items?page=2"},
		{"prev relation", multi, "prev", "https://api.example.com/items?page=1"},
		{"first relation", multi, "first", "https://api.example.com/items?page=1"},
		{"last relation", multi, "last", "https://api.example.com/items?page=5"},
		{"missing relation returns empty", multi, "next2", ""},
		{"empty header returns empty", "", "next", ""},
		{"unquoted rel value", `<https://api.example.com/items?page=2>; rel=next`, "next", "https://api.example.com/items?page=2"},
		{"multi-token rel matches first token", `<https://api.example.com/items?page=3>; rel="next prev"`, "next", "https://api.example.com/items?page=3"},
		{"multi-token rel matches second token", `<https://api.example.com/items?page=3>; rel="next prev"`, "prev", "https://api.example.com/items?page=3"},
		{"whitespace around rel attribute", `<https://api.example.com/items?page=4>;  rel = "next" `, "next", "https://api.example.com/items?page=4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParseLinkRel(tt.header, tt.rel)
			if got != tt.want {
				t.Errorf("ParseLinkRel(%q, %q) = %q, want %q", tt.header, tt.rel, got, tt.want)
			}
		})
	}
}

// TestParseLinkNext_Unchanged verifies that parseLinkNext, now a one-line
// wrapper around ParseLinkRel(header, "next"), preserves its exact prior
// behavior for the existing forward-paginating callers.
func TestParseLinkNext_Unchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"next present", `<https://api.example.com/items?page=2>; rel="next"`, "https://api.example.com/items?page=2"},
		{"no next relation", `<https://api.example.com/items?page=1>; rel="prev"`, ""},
		{"empty header", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseLinkNext(tt.header)
			if got != tt.want {
				t.Errorf("parseLinkNext(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

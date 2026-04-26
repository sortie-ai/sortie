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

package fakemodel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	holdPath     = "/v1/messages"
	holdAuxBody  = `{"model":"scripted-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	holdTurnBody = `{"model":"scripted-model","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`
	holdWait     = 5 * time.Second
)

type holdReply struct {
	status int
	body   []byte
	err    error
}

func startHeld(t *testing.T) (*Server, *recordingReporter) {
	t.Helper()

	reporter := &recordingReporter{TB: t}
	return Start(reporter, []Response{{Hold: true}}), reporter
}

func postModel(ctx context.Context, url, body string) holdReply {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+holdPath, strings.NewReader(body))
	if err != nil {
		return holdReply{err: err}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return holdReply{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return holdReply{status: resp.StatusCode, body: raw, err: err}
}

func postModelInBackground(ctx context.Context, url, body string) <-chan holdReply {
	reply := make(chan holdReply, 1)
	go func() { reply <- postModel(ctx, url, body) }()
	return reply
}

func awaitHolding(t *testing.T, srv *Server) {
	t.Helper()

	select {
	case <-srv.holding():
	case <-time.After(holdWait):
		t.Fatalf("holding() still open %s after the turn request was sent", holdWait)
	}
}

func awaitReply(t *testing.T, reply <-chan holdReply) holdReply {
	t.Helper()

	select {
	case got := <-reply:
		return got
	case <-time.After(holdWait):
		t.Fatalf("no reply within %s", holdWait)
		return holdReply{}
	}
}

func heldExchanges(srv *Server) []Exchange {
	var held []Exchange
	for _, ex := range srv.Exchanges() {
		if ex.Kind == ExchangeHeld {
			held = append(held, ex)
		}
	}
	return held
}

func TestHoldOpensOnlyForTheTurnRequest(t *testing.T) {
	t.Parallel()

	srv, _ := startHeld(t)

	if aux := postModel(t.Context(), srv.URL(), holdAuxBody); aux.err != nil {
		t.Fatalf("auxiliary request error = %v", aux.err)
	}
	select {
	case <-srv.holding():
		t.Fatal("holding() closed after an auxiliary request, want it open until a turn request is held")
	default:
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	postModelInBackground(ctx, srv.URL(), holdTurnBody)
	awaitHolding(t, srv)

	held := heldExchanges(srv)
	if len(held) != 1 || held[0].Step != 0 {
		t.Errorf("held exchanges = %d, want exactly one, at step 0", len(held))
	}
}

func TestHoldEndsQuietlyWhenTheClientDisconnects(t *testing.T) {
	t.Parallel()

	srv, reporter := startHeld(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	reply := postModelInBackground(ctx, srv.URL(), holdTurnBody)
	awaitHolding(t, srv)

	cancel()
	awaitReply(t, reply)

	if open := srv.awaitHolds(holdWait); len(open) != 0 {
		t.Errorf("awaitHolds(%s) = %d open holds after the client disconnected, want none", holdWait, len(open))
	}
	if failures := reporter.snapshot(); len(failures) != 0 {
		t.Errorf("failures after a client disconnect = %q, want none", failures)
	}
	if held := heldExchanges(srv); len(held) != 1 || held[0].Answer.Status != 0 || held[0].Answer.Text != "" {
		t.Errorf("held exchanges = %d, want one with a zero answer", len(held))
	}
}

func TestHoldReleaseAnswersConnectedClientWithTheWireError(t *testing.T) {
	t.Parallel()

	srv, reporter := startHeld(t)
	reply := postModelInBackground(t.Context(), srv.URL(), holdTurnBody)
	awaitHolding(t, srv)

	srv.release()
	got := awaitReply(t, reply)

	if got.err != nil {
		t.Fatalf("released request error = %v", got.err)
	}
	if got.status != http.StatusBadRequest {
		t.Errorf("released request status = %d, want %d", got.status, http.StatusBadRequest)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(got.body, &envelope); err != nil {
		t.Fatalf("decoding released body %q: %v", got.body, err)
	}
	if envelope.Type != "error" || envelope.Error.Type != "invalid_request_error" {
		t.Errorf("released body = %s, want the messages wire error envelope", got.body)
	}
	held := heldExchanges(srv)
	if len(held) != 1 {
		t.Fatalf("held exchanges = %d, want one", len(held))
	}
	if held[0].Answer.Status != http.StatusBadRequest || !bytes.Equal(held[0].Answer.Body, got.body) || held[0].Answer.Text != envelope.Error.Message {
		t.Errorf("recorded answer status = %d, text = %q, want status %d, the body the client received and its message", held[0].Answer.Status, held[0].Answer.Text, http.StatusBadRequest)
	}
	failures := reporter.snapshot()
	if len(failures) != 1 || !strings.Contains(failures[0], "request 1 (POST /v1/messages)") {
		t.Errorf("failures = %q, want exactly one naming request 1 (POST /v1/messages)", failures)
	}
}

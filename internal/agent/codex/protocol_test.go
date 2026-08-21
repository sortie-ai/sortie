//go:build unix

package codex

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// capturingWriteCloser records each Write call as one entry, in the order
// received, so a test can assert on the exact bytes a handshake function
// or the RunTurn event loop wrote back to the app-server. Safe for
// concurrent use, matching every production caller of sendResponse and
// sendErrorResponse, which write under state.mu.
type capturingWriteCloser struct {
	mu     sync.Mutex
	writes []string
}

func (w *capturingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func (w *capturingWriteCloser) Close() error { return nil }

// find returns the first captured write with the given prefix.
func (w *capturingWriteCloser) find(prefix string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, write := range w.writes {
		if strings.HasPrefix(write, prefix) {
			return write, true
		}
	}
	return "", false
}

func TestSendErrorResponse_WritesExactBytes(t *testing.T) {
	t.Parallel()

	stdin := &capturingWriteCloser{}
	state := &sessionState{stdin: stdin}

	err := sendErrorResponse(state, 7, -32001, "sortie refuses requests that only a person could answer")
	if err != nil {
		t.Fatalf("sendErrorResponse() error = %v", err)
	}

	write, ok := stdin.find(`{"id":7,`)
	if !ok {
		t.Fatal("sendErrorResponse() wrote nothing to stdin")
	}
	const want = `{"id":7,"error":{"code":-32001,"message":"sortie refuses requests that only a person could answer"}}` + "\n"
	if write != want {
		t.Errorf("sendErrorResponse(id=7, code=-32001) wrote %q, want %q", write, want)
	}
}

func TestSendNotification_WritesToStdin(t *testing.T) {
	t.Parallel()

	state := makeTestState(nil)
	err := sendNotification(state, "initialized", map[string]any{"version": "1.0"})
	if err != nil {
		t.Fatalf("sendNotification() error = %v", err)
	}
}

func TestReadResponse_SkipsNotifications(t *testing.T) {
	t.Parallel()

	scanCh := scanChanFromLines(
		`{"method":"some/notification","params":{}}`,
		`{"id":1,"result":{"ok":true}}`,
	)

	resp, err := readResponse(context.Background(), scanCh, 1)
	if err != nil {
		t.Fatalf("readResponse() error = %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("resp.ID = %d, want 1", resp.ID)
	}
}

func TestReadResponse_SkipsWrongID(t *testing.T) {
	t.Parallel()

	scanCh := scanChanFromLines(
		`{"id":99,"result":{}}`,
		`{"id":1,"result":{"ok":true}}`,
	)

	resp, err := readResponse(context.Background(), scanCh, 1)
	if err != nil {
		t.Fatalf("readResponse() error = %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("resp.ID = %d, want 1", resp.ID)
	}
}

func TestReadResponse_UnexpectedEOF(t *testing.T) {
	t.Parallel()

	scanCh := scanChanFromLines() // immediate EOF

	_, err := readResponse(context.Background(), scanCh, 1)
	if err == nil {
		t.Fatal("readResponse() expected error on empty input, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("readResponse() error = %q, want 'unexpected EOF'", err.Error())
	}
}

func TestReadResponse_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	scanCh := make(chan scanResult) // no sender; ctx.Done() is the only ready case
	_, err := readResponse(ctx, scanCh, 1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("readResponse() error = %v, want context.Canceled", err)
	}
}

func TestReadResponse_MalformedMessageSkipped(t *testing.T) {
	t.Parallel()

	scanCh := scanChanFromLines(
		"not-valid-json",
		`{"id":1,"result":{"ok":true}}`,
	)

	resp, err := readResponse(context.Background(), scanCh, 1)
	if err != nil {
		t.Fatalf("readResponse() error = %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("resp.ID = %d, want 1", resp.ID)
	}
}

func TestReadTimeout_CustomValue(t *testing.T) {
	t.Parallel()

	state := &sessionState{agentConfig: domain.AgentConfig{ReadTimeoutMS: 5000}}
	got := readTimeout(state)
	if got != 5*time.Second {
		t.Errorf("readTimeout() = %v, want 5s", got)
	}
}

func TestReadTimeout_DefaultsTo30s(t *testing.T) {
	t.Parallel()

	state := &sessionState{}
	got := readTimeout(state)
	if got != 30*time.Second {
		t.Errorf("readTimeout() = %v, want 30s", got)
	}
}

func TestIsAgentError_WithAgentError(t *testing.T) {
	t.Parallel()

	ae := &domain.AgentError{Kind: domain.ErrPortExit, Message: "subprocess exited"}
	var target *domain.AgentError
	if !isAgentError(ae, &target) {
		t.Fatal("isAgentError() = false for *domain.AgentError")
	}
	if target != ae {
		t.Error("isAgentError() did not set target to the input error")
	}
}

func TestIsAgentError_WithPlainError(t *testing.T) {
	t.Parallel()

	plain := errors.New("not an agent error")
	var target *domain.AgentError
	if isAgentError(plain, &target) {
		t.Fatal("isAgentError() = true for plain error")
	}
	if target != nil {
		t.Error("isAgentError() set target for plain error")
	}
}

func TestStartSession_SSHBinaryNotFound(t *testing.T) {
	// No t.Parallel() — uses t.Setenv which mutates process env.
	t.Setenv("PATH", "/nonexistent-path-for-test")

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		SSHHost:       "remote.example.com",
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

func TestReadResponse_ContextCancelledWhileBlocked(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	scanCh := startScannerCh(scanner, stop)

	// Pipe never writes; scanner goroutine blocks in pr.Read(). Only ctx.Done()
	// will be ready in the select, so the result is deterministic.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := readResponse(ctx, scanCh, 1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("readResponse() = %v, want context.Canceled", err)
	}
}

func TestStartScannerCh_NoLeakOnContextCancel(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	stop := make(chan struct{})
	scanCh := startScannerCh(scanner, stop)

	// Close stop to signal the goroutine, then close the write end so that
	// scanner.Scan unblocks and the goroutine can observe the stop signal.
	close(stop)
	_ = pw.Close()

	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-scanCh:
			if !ok {
				return // channel closed — goroutine exited, no leak
			}
		case <-deadline:
			t.Fatal("startScannerCh goroutine did not exit within 1s after stop closed")
		}
	}
}

//go:build unix

package codex

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// capturingWriteCloser records each Write call as one entry, in the order
// received, so a test can assert on the exact bytes a handshake function
// or the RunTurn event loop wrote back to the app-server. Safe for
// concurrent use: every production caller writes through state.conn,
// which owns its own write mutex rather than state.mu.
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

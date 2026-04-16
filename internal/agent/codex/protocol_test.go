//go:build unix

package codex

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

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

	fixture := strings.NewReader(
		"{\"method\":\"some/notification\",\"params\":{}}\n" +
			"{\"id\":1,\"result\":{\"ok\":true}}\n",
	)
	scanner := bufio.NewScanner(fixture)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	resp, err := readResponse(context.Background(), scanner, 1)
	if err != nil {
		t.Fatalf("readResponse() error = %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("resp.ID = %d, want 1", resp.ID)
	}
}

func TestReadResponse_SkipsWrongID(t *testing.T) {
	t.Parallel()

	fixture := strings.NewReader(
		"{\"id\":99,\"result\":{}}\n" + // wrong ID
			"{\"id\":1,\"result\":{\"ok\":true}}\n",
	)
	scanner := bufio.NewScanner(fixture)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	resp, err := readResponse(context.Background(), scanner, 1)
	if err != nil {
		t.Fatalf("readResponse() error = %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("resp.ID = %d, want 1", resp.ID)
	}
}

func TestReadResponse_UnexpectedEOF(t *testing.T) {
	t.Parallel()

	scanner := bufio.NewScanner(strings.NewReader(""))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	_, err := readResponse(context.Background(), scanner, 1)
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

	scanner := bufio.NewScanner(strings.NewReader("{\"id\":1,\"result\":{}}\n"))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	_, err := readResponse(ctx, scanner, 1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("readResponse() error = %v, want context.Canceled", err)
	}
}

func TestReadResponse_MalformedMessageSkipped(t *testing.T) {
	t.Parallel()

	fixture := strings.NewReader(
		"not-valid-json\n" +
			"{\"id\":1,\"result\":{\"ok\":true}}\n",
	)
	scanner := bufio.NewScanner(fixture)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	resp, err := readResponse(context.Background(), scanner, 1)
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

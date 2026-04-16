//go:build unix

package codex

import (
	"bufio"
	"context"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// handshakeState returns a sessionState suitable for handshake function tests.
// It has a valid workspacePath and a discarding stdin.
func handshakeState() *sessionState {
	return &sessionState{
		workspacePath: "/tmp",
		stdin:         nopWriteCloser{},
	}
}

// handshakeScanner returns a *bufio.Scanner reading from fixture JSONL lines.
func handshakeScanner(lines ...string) *bufio.Scanner {
	fixture := strings.Join(lines, "\n") + "\n"
	scanner := bufio.NewScanner(strings.NewReader(fixture))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	return scanner
}

// --- initializeHandshake ---

func TestInitializeHandshake_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(`{"id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"codex-app-server"}}}`)

	if err := initializeHandshake(context.Background(), state, scanner); err != nil {
		t.Fatalf("initializeHandshake() error = %v", err)
	}
}

func TestInitializeHandshake_ErrorResponse(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(`{"id":1,"error":{"code":-32600,"message":"invalid request"}}`)

	err := initializeHandshake(context.Background(), state, scanner)
	if err == nil {
		t.Fatal("initializeHandshake() expected error for error response, got nil")
	}
	if !strings.Contains(err.Error(), "initialize error") {
		t.Errorf("initializeHandshake() error = %q, want 'initialize error'", err.Error())
	}
}

func TestInitializeHandshake_EOF(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner() // empty fixture

	err := initializeHandshake(context.Background(), state, scanner)
	if err == nil {
		t.Fatal("initializeHandshake() expected error on EOF, got nil")
	}
}

// --- authenticateIfNeeded ---

func TestAuthenticateIfNeeded_AlreadyLoggedIn(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	// account/read response with non-null account
	scanner := handshakeScanner(`{"id":1,"result":{"account":{"id":"user-1","email":"user@example.com"}}}`)

	if err := authenticateIfNeeded(context.Background(), state, scanner); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil for logged-in account", err)
	}
}

func TestAuthenticateIfNeeded_NullAccountNoAPIKey(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	// account/read response with null account — CODEX_API_KEY not set → return nil
	scanner := handshakeScanner(`{"id":1,"result":{"account":null}}`)

	if err := authenticateIfNeeded(context.Background(), state, scanner); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil when API key absent", err)
	}
}

func TestAuthenticateIfNeeded_AccountReadError(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(`{"id":1,"error":{"code":-32000,"message":"server error"}}`)

	err := authenticateIfNeeded(context.Background(), state, scanner)
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for account/read error response")
	}
}

func TestAuthenticateIfNeeded_LoginSuccess(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "test-api-key-12345")

	state := handshakeState()
	// id=1: account/read → null account
	// id=2: account/login/start → success response
	// then: login/completed notification
	scanner := handshakeScanner(
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		`{"method":"account/login/completed","params":{"success":true}}`,
	)

	if err := authenticateIfNeeded(context.Background(), state, scanner); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil on successful login", err)
	}
}

func TestAuthenticateIfNeeded_LoginResponseError(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "invalid-key")

	state := handshakeState()
	// id=1: account/read → null
	// id=2: account/login/start → error
	scanner := handshakeScanner(
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"error":{"code":-32001,"message":"invalid API key"}}`,
	)

	err := authenticateIfNeeded(context.Background(), state, scanner)
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for login failure")
	}
}

func TestAuthenticateIfNeeded_LoginCompletedFailed(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "bad-key")

	state := handshakeState()
	scanner := handshakeScanner(
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		`{"method":"account/login/completed","params":{"success":false}}`,
	)

	err := authenticateIfNeeded(context.Background(), state, scanner)
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for failed login completion")
	}
	var ae *domain.AgentError
	if !requireErrAs(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != domain.ErrResponseError {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, domain.ErrResponseError)
	}
}

// requireErrAs is a non-fatal helper for type assertions in table-driven code.
func requireErrAs[T any](err error, target *T) bool {
	if err == nil {
		return false
	}
	switch e := err.(type) {
	case T:
		*target = e
		return true
	default:
		_ = e
		return false
	}
}

// --- startThread ---

func TestStartThread_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	threadID, err := startThread(context.Background(), state, scanner, passthroughConfig{}, nil)
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-abc" {
		t.Errorf("startThread() threadID = %q, want %q", threadID, "thread-abc")
	}
}

func TestStartThread_WithModelAndPersonality(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(
		`{"id":1,"result":{"thread":{"id":"thread-xyz"}}}`,
		`{"method":"thread/started","params":{}}`,
	)
	pt := passthroughConfig{
		Model:          "o4-mini",
		Personality:    "concise",
		ApprovalPolicy: "auto",
		ThreadSandbox:  "workspaceWrite",
	}

	threadID, err := startThread(context.Background(), state, scanner, pt, nil)
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-xyz" {
		t.Errorf("startThread() threadID = %q, want %q", threadID, "thread-xyz")
	}
}

func TestStartThread_WithTools(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(
		`{"id":1,"result":{"thread":{"id":"thread-tools"}}}`,
		`{"method":"thread/started","params":{}}`,
	)
	tools := []domain.AgentTool{
		&fakeTool{name: "list_issues", result: nil},
		&fakeTool{name: "create_issue", result: nil},
	}

	threadID, err := startThread(context.Background(), state, scanner, passthroughConfig{}, tools)
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-tools" {
		t.Errorf("startThread() threadID = %q, want %q", threadID, "thread-tools")
	}
}

func TestStartThread_ErrorResponse(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(`{"id":1,"error":{"code":-32000,"message":"workspace not found"}}`)

	_, err := startThread(context.Background(), state, scanner, passthroughConfig{}, nil)
	if err == nil {
		t.Fatal("startThread() expected error for error response")
	}
}

func TestStartThread_EmptyThreadID(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	// Response with empty thread ID.
	scanner := handshakeScanner(`{"id":1,"result":{"thread":{"id":""}}}`)

	_, err := startThread(context.Background(), state, scanner, passthroughConfig{}, nil)
	if err == nil {
		t.Fatal("startThread() expected error for empty thread ID")
	}
}

// --- resumeThread ---

func TestResumeThread_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(`{"id":1,"result":{}}`)

	if err := resumeThread(context.Background(), state, scanner, "existing-thread-id"); err != nil {
		t.Fatalf("resumeThread() error = %v", err)
	}
}

func TestResumeThread_ErrorResponse(t *testing.T) {
	t.Parallel()

	state := handshakeState()
	scanner := handshakeScanner(`{"id":1,"error":{"code":-32002,"message":"thread not found"}}`)

	err := resumeThread(context.Background(), state, scanner, "nonexistent-thread")
	if err == nil {
		t.Fatal("resumeThread() expected error for error response")
	}
}

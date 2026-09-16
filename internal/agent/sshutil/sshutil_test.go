package sshutil

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "''"},
		{"simple identifier", "hello", "'hello'"},
		{"spaces", "hello world", "'hello world'"},
		{"single quote", "it's", "'it'\\''s'"},
		{"double quote", `say "hi"`, `'say "hi"'`},
		{"backslash", `a\b`, `'a\b'`},
		{"newline", "line\nnewline", "'line\nnewline'"},
		{"tab", "tab\there", "'tab\there'"},
		{"semicolon", "cmd; rm -rf /", "'cmd; rm -rf /'"},
		{"pipe", "foo | bar", "'foo | bar'"},
		{"env var", "$HOME", "'$HOME'"},
		{"unicode", "héllo", "'héllo'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// mustReadAllBytes drains r into a byte slice, or returns nil for a nil
// reader.
func mustReadAllBytes(t *testing.T, r io.Reader) []byte {
	t.Helper()
	if r == nil {
		return nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	return data
}

// TestBuildSSHLaunch_ZeroEnv asserts that a nil or empty opts.Env
// produces Args byte-identical between the two, with the exact
// prefix and final-element shape BuildSSHArgs used to produce, and
// carries no preamble.
func TestBuildSSHLaunch_ZeroEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		host          string
		workspacePath string
		remoteCommand string
		agentArgs     []string
		opts          SSHOptions
		wantFinal     string
	}{
		{
			name:          "no args, default strict host key checking",
			host:          "example.test",
			workspacePath: "/workspace",
			remoteCommand: "codex app-server",
			wantFinal:     "cd -- '/workspace' && { codex app-server; }",
		},
		{
			name:          "with agent args needing quoting, whitespace-padded host",
			host:          "  example.test  ",
			workspacePath: "/work space",
			remoteCommand: "run --acp",
			agentArgs:     []string{"a b", "it's"},
			wantFinal:     "cd -- '/work space' && { run --acp 'a b' 'it'\\''s'; }",
		},
		{
			name:          "explicit strict host key checking",
			host:          "example.test",
			workspacePath: "/workspace",
			remoteCommand: "opencode run",
			opts:          SSHOptions{StrictHostKeyChecking: "no"},
			wantFinal:     "cd -- '/workspace' && { opencode run; }",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, label := range []string{"nil Env", "empty Env"} {
				opts := tt.opts
				if label == "empty Env" {
					opts.Env = []EnvVar{}
				}
				launch := BuildSSHLaunch(tt.host, tt.workspacePath, tt.remoteCommand, tt.agentArgs, opts)

				if len(launch.Args) == 0 {
					t.Fatalf("%s: BuildSSHLaunch(...).Args is empty", label)
				}
				gotFinal := launch.Args[len(launch.Args)-1]
				if gotFinal != tt.wantFinal {
					t.Errorf("%s: Args final element = %q, want %q", label, gotFinal, tt.wantFinal)
				}

				strictHostKey := opts.StrictHostKeyChecking
				if strictHostKey == "" {
					strictHostKey = "accept-new"
				}
				wantPrefix := []string{
					"-o", "StrictHostKeyChecking=" + strictHostKey,
					"-o", "BatchMode=yes",
					"-o", "ConnectTimeout=30",
					"-o", "ServerAliveInterval=15",
					"-o", "ServerAliveCountMax=3",
					"--",
					strings.TrimSpace(tt.host),
				}
				if len(launch.Args) != len(wantPrefix)+1 {
					t.Fatalf("%s: Args length = %d, want %d", label, len(launch.Args), len(wantPrefix)+1)
				}
				for i, want := range wantPrefix {
					if launch.Args[i] != want {
						t.Errorf("%s: Args[%d] = %q, want %q", label, i, launch.Args[i], want)
					}
				}

				if r := launch.StdinReader(); r != nil {
					t.Errorf("%s: StdinReader() = non-nil, want a nil interface value", label)
				}
				var rec recordingWriteCloser
				if got := launch.PrefixStdin(&rec); got != io.WriteCloser(&rec) {
					t.Errorf("%s: PrefixStdin(w) = %v, want w itself", label, got)
				}
			}
		})
	}
}

// TestSSHLaunch_ZeroValue asserts that the zero SSHLaunch behaves the
// same as a zero-Env BuildSSHLaunch result: no preamble, and
// PrefixStdin returns its argument unchanged.
func TestSSHLaunch_ZeroValue(t *testing.T) {
	t.Parallel()

	var zero SSHLaunch
	if r := zero.StdinReader(); r != nil {
		t.Errorf("zero SSHLaunch StdinReader() = non-nil, want a nil interface value")
	}
	var rec recordingWriteCloser
	if got := zero.PrefixStdin(&rec); got != io.WriteCloser(&rec) {
		t.Errorf("zero SSHLaunch PrefixStdin(w) = %v, want w itself", got)
	}
	if zero.Args != nil {
		t.Errorf("zero SSHLaunch Args = %v, want nil", zero.Args)
	}
}

// TestBuildSSHLaunch_NonEmptyEnv_ExactLiterals pins the exact preamble
// and final-element text for a two-entry Env.
func TestBuildSSHLaunch_NonEmptyEnv_ExactLiterals(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "run --acp", []string{"a"}, SSHOptions{
		Env: []EnvVar{{Name: "A", Value: "x"}, {Name: "B", Value: "y z"}},
	})

	const wantPreamble = "unset _sortie_env && export A='x' B='y z' && _sortie_complete=1"
	if len(wantPreamble) != 63 {
		t.Fatalf("test fixture error: wantPreamble length = %d, want 63", len(wantPreamble))
	}

	gotPreamble := mustReadAllBytes(t, launch.StdinReader())
	if string(gotPreamble) != wantPreamble {
		t.Errorf("preamble = %q, want %q", string(gotPreamble), wantPreamble)
	}

	const wantFinal = `cd -- '/w' && { command -v dd >/dev/null 2>&1 || { echo 'sortie: dd is required on the remote host to receive environment variables' >&2; exit 1; }; } && unset _sortie_complete && _sortie_env=$(dd bs=1 count=63 2>/dev/null) && eval "$_sortie_env" && [ "${_sortie_complete-}" = 1 ] && { run --acp 'a'; }`
	gotFinal := launch.Args[len(launch.Args)-1]
	if gotFinal != wantFinal {
		t.Errorf("final element = %q, want %q", gotFinal, wantFinal)
	}
}

// TestBuildSSHLaunch_ArgsNeverContainEnvValues asserts that no element
// of Args contains a carried value as a substring.
func TestBuildSSHLaunch_ArgsNeverContainEnvValues(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("host", "/workspace", "run --acp", []string{"arg1"}, SSHOptions{
		Env: []EnvVar{
			{Name: "A", Value: "occurs-nowhere-else-aaaa1111"},
			{Name: "B", Value: "occurs-nowhere-else-bbbb2222"},
		},
	})

	for _, value := range []string{"occurs-nowhere-else-aaaa1111", "occurs-nowhere-else-bbbb2222"} {
		for i, arg := range launch.Args {
			if strings.Contains(arg, value) {
				t.Errorf("Args[%d] = %q, contains carried value %q", i, arg, value)
			}
		}
	}
}

// TestBuildSSHLaunch_PanicsOnInvalidEnvName asserts that an invalid
// Env name panics with a message naming the entry's index and neither
// its name nor its value.
func TestBuildSSHLaunch_PanicsOnInvalidEnvName(t *testing.T) {
	t.Parallel()

	const secretValue = "super-secret-value-should-never-appear"

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("BuildSSHLaunch did not panic on an invalid Env name")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "2") {
			t.Errorf("panic message = %q, want it to name index 2", msg)
		}
		if strings.Contains(msg, "1BAD") {
			t.Errorf("panic message = %q, contains the invalid name", msg)
		}
		if strings.Contains(msg, secretValue) {
			t.Errorf("panic message = %q, contains the value", msg)
		}
	}()

	BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{
		Env: []EnvVar{
			{Name: "A", Value: "a"},
			{Name: "B", Value: "b"},
			{Name: "1BAD", Value: secretValue},
		},
	})
}

// TestBuildSSHLaunch_PanicsOnReservedEnvName asserts that an Env entry
// naming the completion marker panics with a message naming the
// entry's index and neither its name nor its value. Carrying that name
// would break the marker both ways: the preamble's own trailing
// assignment overwrites the carried value, and a preamble truncated
// just after the carried assignment satisfies the completeness test
// with none of the later variables exported.
func TestBuildSSHLaunch_PanicsOnReservedEnvName(t *testing.T) {
	t.Parallel()

	const secretValue = "reserved-name-value-should-never-appear"

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("BuildSSHLaunch did not panic on a reserved Env name")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "1") {
			t.Errorf("panic message = %q, want it to name index 1", msg)
		}
		if strings.Contains(msg, "_sortie_complete") {
			t.Errorf("panic message = %q, contains the reserved name", msg)
		}
		if strings.Contains(msg, secretValue) {
			t.Errorf("panic message = %q, contains the value", msg)
		}
	}()

	BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{
		Env: []EnvVar{
			{Name: "A", Value: "a"},
			{Name: "_sortie_complete", Value: secretValue},
		},
	})
}

// recordingWriteCloser is a test double for io.WriteCloser that
// records every Write call's bytes, optionally fails every Write with
// a fixed error, and records whether Close was called.
type recordingWriteCloser struct {
	mu       sync.Mutex
	writes   [][]byte
	closed   bool
	writeErr error
}

func (r *recordingWriteCloser) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (r *recordingWriteCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *recordingWriteCloser) recordedWrites() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.writes...)
}

// blockingWriteCloser is a test double for io.WriteCloser whose first
// Write blocks until Close is called, then reports an error, so a test
// can observe Close unblocking a Write in progress.
type blockingWriteCloser struct {
	mu      sync.Mutex
	closed  bool
	unblock chan struct{}
	started chan struct{}
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{unblock: make(chan struct{}), started: make(chan struct{}, 1)}
}

func (b *blockingWriteCloser) Write(p []byte) (int, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.unblock
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, errors.New("write on closed writer")
	}
	return len(p), nil
}

func (b *blockingWriteCloser) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	close(b.unblock)
	return nil
}

// TestPrefixStdin_Ordering asserts that the first Write sends the
// preamble then the caller's own bytes, in order, and every later
// Write passes straight through.
func TestPrefixStdin_Ordering(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{Env: []EnvVar{{Name: "A", Value: "v"}}})
	preamble := mustReadAllBytes(t, launch.StdinReader())

	rec := &recordingWriteCloser{}
	pw := launch.PrefixStdin(rec)

	if _, err := pw.Write([]byte("first")); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	if _, err := pw.Write([]byte("second")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}

	writes := rec.recordedWrites()
	if len(writes) != 3 {
		t.Fatalf("recorded %d writes, want 3 (preamble, first, second): %q", len(writes), writes)
	}
	if string(writes[0]) != string(preamble) {
		t.Errorf("first recorded write = %q, want the preamble %q", writes[0], preamble)
	}
	if string(writes[1]) != "first" {
		t.Errorf("second recorded write = %q, want %q", writes[1], "first")
	}
	if string(writes[2]) != "second" {
		t.Errorf("third recorded write = %q, want %q", writes[2], "second")
	}
}

// TestPrefixStdin_FailingPreambleWrite asserts that a failing preamble
// write returns that error and the caller's own bytes are never
// forwarded to the underlying writer.
func TestPrefixStdin_FailingPreambleWrite(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{Env: []EnvVar{{Name: "A", Value: "v"}}})
	wantErr := errors.New("boom")
	rec := &recordingWriteCloser{writeErr: wantErr}
	pw := launch.PrefixStdin(rec)

	n, err := pw.Write([]byte("payload"))
	if !errors.Is(err, wantErr) {
		t.Errorf("Write() error = %v, want %v", err, wantErr)
	}
	if n != 0 {
		t.Errorf("Write() n = %d, want 0", n)
	}
	for _, w := range rec.recordedWrites() {
		if string(w) == "payload" {
			t.Errorf("recorded writes include the caller's bytes, want none forwarded on a failing preamble write")
		}
	}
}

// shortWriteCloser is a test double for io.WriteCloser that reports a
// short count with a nil error on its first Write, the contract
// violation a preamble write must not accept as a delivered preamble.
type shortWriteCloser struct {
	writes [][]byte
}

func (s *shortWriteCloser) Write(p []byte) (int, error) {
	s.writes = append(s.writes, append([]byte(nil), p...))
	if len(s.writes) == 1 {
		return len(p) - 1, nil
	}
	return len(p), nil
}

func (s *shortWriteCloser) Close() error { return nil }

// TestPrefixStdin_ShortPreambleWrite asserts that a preamble write
// reporting fewer bytes than the preamble holds, with a nil error,
// fails with io.ErrShortWrite and forwards none of the caller's bytes.
func TestPrefixStdin_ShortPreambleWrite(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{Env: []EnvVar{{Name: "A", Value: "v"}}})
	rec := &shortWriteCloser{}
	pw := launch.PrefixStdin(rec)

	n, err := pw.Write([]byte("payload"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("Write() error = %v, want %v", err, io.ErrShortWrite)
	}
	if n != 0 {
		t.Errorf("Write() n = %d, want 0", n)
	}
	for _, w := range rec.writes {
		if string(w) == "payload" {
			t.Error("recorded writes include the caller's bytes, want none forwarded after a short preamble write")
		}
	}
}

// TestPrefixStdin_CloseBeforeWrite asserts that Close before any Write
// closes the underlying writer and forwards no bytes.
func TestPrefixStdin_CloseBeforeWrite(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{Env: []EnvVar{{Name: "A", Value: "v"}}})
	rec := &recordingWriteCloser{}
	pw := launch.PrefixStdin(rec)

	if err := pw.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if !rec.closed {
		t.Error("underlying writer Close() was not called")
	}
	if writes := rec.recordedWrites(); len(writes) != 0 {
		t.Errorf("recorded %d writes after Close before any Write, want 0", len(writes))
	}
}

// TestPrefixStdin_CloseUnblocksInFlightWrite asserts that Close
// returns while a Write is blocked on a peer that never reads, and
// that blocked Write then returns an error.
func TestPrefixStdin_CloseUnblocksInFlightWrite(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{Env: []EnvVar{{Name: "A", Value: "v"}}})
	bw := newBlockingWriteCloser()
	pw := launch.PrefixStdin(bw)

	writeErrCh := make(chan error, 1)
	go func() {
		_, err := pw.Write([]byte("payload"))
		writeErrCh <- err
	}()

	select {
	case <-bw.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not start within 2s")
	}

	closeDone := make(chan struct{})
	go func() {
		pw.Close() //nolint:errcheck // exercising the unblock-in-flight-write behavior, not Close's own error
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return while a Write was blocked")
	}

	select {
	case err := <-writeErrCh:
		if err == nil {
			t.Error("blocked Write returned nil error after Close, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Write did not return after Close")
	}
}

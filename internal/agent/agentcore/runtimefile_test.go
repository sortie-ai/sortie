package agentcore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanRuntimeFileRejects(t *testing.T) {
	t.Parallel()

	const homeVar = "SORTIE_TEST_RUNTIME_HOME"
	errStop := errors.New("stop")

	tests := []struct {
		name      string
		content   string
		skipFile  bool
		symlink   bool
		homeLink  bool
		limit     int64
		lineLimit int64
		stopAt    string
		wantErr   bool
		wantIs    error
		wantLines []string
	}{
		{
			name:      "line above the line cap",
			content:   "ok\n" + strings.Repeat("x", 11) + "\ntail\n",
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "unterminated last line above the line cap",
			content:   "ok\n" + strings.Repeat("x", 11),
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "file above the byte cap",
			content:   strings.Repeat("ab\n", 10),
			limit:     20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "missing file",
			skipFile:  true,
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
			wantIs:    fs.ErrNotExist,
		},
		{
			name:      "segment symlinked to a file outside the home",
			content:   "outside\n",
			symlink:   true,
			limit:     1 << 20,
			lineLimit: 10,
			wantErr:   true,
		},
		{
			name:      "home kept as a symlink is read",
			content:   "a\n",
			homeLink:  true,
			limit:     1 << 20,
			lineLimit: 10,
			wantLines: []string{"a"},
		},
		{
			name:      "error from the line callback",
			content:   "a\nb\nc\n",
			limit:     1 << 20,
			lineLimit: 10,
			stopAt:    "b",
			wantErr:   true,
			wantIs:    errStop,
			wantLines: []string{"a", "b"},
		},
		{
			name:      "line of exactly the line cap passes",
			content:   strings.Repeat("x", 10) + "\r\nnext",
			limit:     1 << 20,
			lineLimit: 10,
			wantLines: []string{strings.Repeat("x", 10), "next"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			file := filepath.Join(home, "events.jsonl")
			if tt.symlink {
				file = filepath.Join(t.TempDir(), "outside.jsonl")
				if err := os.Symlink(file, filepath.Join(home, "events.jsonl")); err != nil {
					t.Skipf("os.Symlink: %v", err)
				}
			}
			if !tt.skipFile {
				if err := os.WriteFile(file, []byte(tt.content), 0o600); err != nil {
					t.Fatalf("os.WriteFile: %v", err)
				}
			}
			if tt.homeLink {
				link := filepath.Join(t.TempDir(), "home")
				if err := os.Symlink(home, link); err != nil {
					t.Skipf("os.Symlink: %v", err)
				}
				home = link
			}
			var seen []string

			err := LaunchTarget{}.ScanRuntimeFile(t.Context(), []string{homeVar + "=" + home}, RuntimePath{HomeVar: homeVar, Segments: []string{"events.jsonl"}}, tt.limit, tt.lineLimit, func(line []byte) error {
				seen = append(seen, strings.TrimSuffix(string(line), "\r"))
				if tt.stopAt != "" && string(line) == tt.stopAt {
					return errStop
				}
				return nil
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("ScanRuntimeFile() error = %v, want error %t", err, tt.wantErr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("ScanRuntimeFile() error = %v, want wrapping %v", err, tt.wantIs)
			}
			for _, line := range seen {
				if int64(len(line)) > tt.lineLimit {
					t.Errorf("ScanRuntimeFile() delivered a line of %d bytes, want none above %d", len(line), tt.lineLimit)
				}
			}
			if tt.wantLines != nil && strings.Join(seen, "|") != strings.Join(tt.wantLines, "|") {
				t.Errorf("ScanRuntimeFile() lines = %q, want %q", seen, tt.wantLines)
			}
		})
	}
}

func TestRuntimePathRefusal(t *testing.T) {
	t.Parallel()

	const uuid = "0f8fad5b-d9cb-469f-a165-70867728950e"
	targets := map[string]LaunchTarget{
		"local":  {},
		"remote": {Command: filepath.Join(t.TempDir(), "absent-ssh"), SSHHost: "lab", WorkspacePath: t.TempDir(), RemoteCommand: "true"},
	}
	segments := func(elements ...string) RuntimePath {
		return RuntimePath{HomeVar: "AGENT_HOME", Segments: elements}
	}

	tests := []struct {
		name    string
		path    RuntimePath
		invalid bool
	}{
		{name: "home variable with a dash", path: RuntimePath{HomeVar: "AGENT-HOME", Segments: []string{"f"}}, invalid: true},
		{name: "empty home variable", path: RuntimePath{Segments: []string{"f"}}, invalid: true},
		{name: "no segments", path: segments(), invalid: true},
		{name: "current directory segment", path: segments("session-state", "."), invalid: true},
		{name: "parent directory segment", path: segments("session-state", ".."), invalid: true},
		{name: "segment with a slash", path: segments("a/b"), invalid: true},
		{name: "segment with a backslash", path: segments(`a\b`), invalid: true},
		{name: "absolute path as a segment", path: segments("/etc/passwd"), invalid: true},
		{name: "empty segment", path: segments("session-state", ""), invalid: true},
		{name: "parent segment in the home default", path: RuntimePath{HomeVar: "AGENT_HOME", HomeDefault: []string{".."}, Segments: []string{"f"}}, invalid: true},
		{name: "journal path with a session identifier", path: RuntimePath{HomeVar: "COPILOT_HOME", HomeDefault: []string{".copilot"}, Segments: []string{"session-state", uuid, "events.jsonl"}}},
		{name: "credential path without a home default", path: RuntimePath{HomeVar: "GEMINI_CLI_HOME", Segments: []string{".gemini", "oauth_creds.json"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := []string{tt.path.HomeVar + "=" + t.TempDir()}

			for host, target := range targets {
				_, readErr := target.ReadRuntimeFile(t.Context(), env, tt.path, 1<<10)
				scanErr := target.ScanRuntimeFile(t.Context(), env, tt.path, 1<<10, 1<<10, func([]byte) error { return nil })

				for op, err := range map[string]error{"ReadRuntimeFile": readErr, "ScanRuntimeFile": scanErr} {
					if got := errors.Is(err, ErrInvalidRuntimePath); got != tt.invalid {
						t.Errorf("%s %s(%+v) error = %v, errors.Is(ErrInvalidRuntimePath) = %t, want %t", host, op, tt.path, err, got, tt.invalid)
					}
				}
			}
		})
	}
}

func TestRunHostCommandRefusesBadCommand(t *testing.T) {
	t.Parallel()

	target := LaunchTarget{Command: "agent", Args: []string{"acp"}}
	for name, cmd := range map[string]HostCommand{
		"launch words above the word count": {LaunchWords: 3},
		"program that does not start":       {Name: filepath.Join(t.TempDir(), "absent")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got, err := target.RunHostCommand(t.Context(), nil, cmd); !errors.Is(err, ErrHostCommandFailed) {
				t.Errorf("RunHostCommand(%+v) = %q, %v, want error wrapping ErrHostCommandFailed", cmd, got, err)
			}
		})
	}
}

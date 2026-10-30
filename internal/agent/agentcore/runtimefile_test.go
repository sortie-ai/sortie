package agentcore

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

type matchCase struct {
	name        string
	homeVar     string
	homeDefault []string
	segments    []string
	want        []string
}

var matchTreeFiles = []string{
	"tmp/a/chats/session-B-abcd.jsonl",
	"tmp/a/chats/session-a-abcd.jsonl",
	"tmp/a/chats/session-c-zzzz.jsonl",
	"tmp/a/deep/chats/session-d-abcd.jsonl",
	"tmp/a-b/chats/session-e-abcd.jsonl",
	"tmp/proj b/chats/session-f-abcd.jsonl",
	"tmp/it's $HOME/chats/session-g-abcd.jsonl",
}

var matchTreeDirs = []string{"tmp/a/chats/session-h-abcd.jsonl"}

var matchTreeLinks = [][2]string{
	{"tmp/a/chats/session-i-abcd.jsonl", "session-a-abcd.jsonl"},
	{"tmp/linked", "a"},
	{"alias", "tmp"},
}

var matchCases = []matchCase{
	{
		name:     "regular files only, bytewise by joined path, none deeper than a star segment",
		segments: []string{"tmp", "*", "chats", "session-*-abcd.jsonl"},
		want: []string{
			"tmp/a-b/chats/session-e-abcd.jsonl",
			"tmp/a/chats/session-B-abcd.jsonl",
			"tmp/a/chats/session-a-abcd.jsonl",
			"tmp/it's $HOME/chats/session-g-abcd.jsonl",
			"tmp/proj b/chats/session-f-abcd.jsonl",
		},
	},
	{
		name:     "symlinked literal prefix, followed as a read follows it",
		segments: []string{"alias", "*", "chats", "session-*-abcd.jsonl"},
		want: []string{
			"alias/a-b/chats/session-e-abcd.jsonl",
			"alias/a/chats/session-B-abcd.jsonl",
			"alias/a/chats/session-a-abcd.jsonl",
			"alias/it's $HOME/chats/session-g-abcd.jsonl",
			"alias/proj b/chats/session-f-abcd.jsonl",
		},
	},
	{
		name:     "missing start directory",
		segments: []string{".absent", "*", "session-*.jsonl"},
	},
	{
		name:        "missing home",
		homeVar:     "SORTIE_TEST_UNSET_HOME",
		homeDefault: []string{".sortie-no-such-runtime-home"},
		segments:    []string{"tmp", "*", "chats", "session-*-abcd.jsonl"},
	},
}

func writeMatchTree(t *testing.T, root string) {
	t.Helper()

	at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	for _, dir := range slices.Concat(matchTreeDirs, matchTreeDirsOfFiles()) {
		if err := os.MkdirAll(at(dir), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q) = %v, want nil", dir, err)
		}
	}
	for _, file := range matchTreeFiles {
		if err := os.WriteFile(at(file), []byte("x"), 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q) = %v, want nil", file, err)
		}
	}
	for _, link := range matchTreeLinks {
		if err := os.Symlink(link[1], at(link[0])); err != nil {
			t.Logf("os.Symlink(%q) = %v, the tree keeps no link there", link[0], err)
		}
	}
}

func matchTreeDirsOfFiles() []string {
	dirs := make([]string, len(matchTreeFiles))
	for i, file := range matchTreeFiles {
		dirs[i] = path.Dir(file)
	}
	return dirs
}

func assertMatched(t *testing.T, got []RuntimePath, err error, pattern RuntimePath, want []string) {
	t.Helper()

	if err != nil {
		t.Fatalf("MatchRuntimeFiles(%q) error = %v, want nil", pattern.Segments, err)
	}
	labels := make([]string, 0, len(got))
	for _, path := range got {
		if path.HomeVar != pattern.HomeVar || !slices.Equal(path.HomeDefault, pattern.HomeDefault) {
			t.Errorf("MatchRuntimeFiles(%q) returned home %q %q, want %q %q", pattern.Segments, path.HomeVar, path.HomeDefault, pattern.HomeVar, pattern.HomeDefault)
		}
		labels = append(labels, strings.Join(path.Segments, "/"))
	}
	if !slices.Equal(labels, want) {
		t.Errorf("MatchRuntimeFiles(%q) = %q, want %q", pattern.Segments, labels, want)
	}
}

func outsideQuotes(command string) string {
	quotedSpan := regexp.MustCompile(`'[^']*'`)
	return quotedSpan.ReplaceAllString(strings.ReplaceAll(command, `'\''`, ""), "")
}

func invalidPathTargets(t *testing.T) map[string]LaunchTarget {
	t.Helper()

	return map[string]LaunchTarget{
		"local":  {},
		"remote": {Command: filepath.Join(t.TempDir(), "absent-ssh"), SSHHost: "lab", WorkspacePath: t.TempDir(), RemoteCommand: "true"},
	}
}

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
	targets := invalidPathTargets(t)
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

func TestLaunchTarget_MatchRuntimeFiles(t *testing.T) {
	t.Parallel()

	const homeVar = "SORTIE_TEST_RUNTIME_HOME"
	home := t.TempDir()
	writeMatchTree(t, home)
	env := []string{homeVar + "=" + home}

	for _, tt := range matchCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pattern := RuntimePath{HomeVar: cmp.Or(tt.homeVar, homeVar), HomeDefault: tt.homeDefault, Segments: tt.segments}

			got, err := LaunchTarget{}.MatchRuntimeFiles(t.Context(), env, pattern)

			assertMatched(t, got, err, pattern, tt.want)
		})
	}
}

func TestLaunchTarget_MatchRuntimeFiles_RefusesPattern(t *testing.T) {
	t.Parallel()

	targets := invalidPathTargets(t)
	tests := []struct {
		name     string
		segments []string
		invalid  bool
	}{
		{name: "question mark", segments: []string{"tmp", "session-?.jsonl"}, invalid: true},
		{name: "opening bracket", segments: []string{"tmp", "session-[a-z].jsonl"}, invalid: true},
		{name: "closing bracket", segments: []string{"tmp", "session-].jsonl"}, invalid: true},
		{name: "backslash", segments: []string{"tmp", `session-\*.jsonl`}, invalid: true},
		{name: "parent directory segment", segments: []string{"tmp", "..", "*"}, invalid: true},
		{name: "star wildcard", segments: []string{"tmp", "*", "session-*.jsonl"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pattern := RuntimePath{HomeVar: "AGENT_HOME", Segments: tt.segments}
			env := []string{pattern.HomeVar + "=" + t.TempDir()}

			for host, target := range targets {
				_, err := target.MatchRuntimeFiles(t.Context(), env, pattern)

				if got := errors.Is(err, ErrInvalidRuntimePath); got != tt.invalid {
					t.Errorf("%s MatchRuntimeFiles(%q) error = %v, errors.Is(ErrInvalidRuntimePath) = %t, want %t", host, tt.segments, err, got, tt.invalid)
				}
			}
		})
	}
}

func TestRemoteMatchCommandQuotesEverySegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern RuntimePath
	}{
		{name: "substitution and backtick", pattern: RuntimePath{HomeVar: "AGENT_HOME", Segments: []string{"$(canary)", "*", "`canary`.json"}}},
		{name: "separator and glob", pattern: RuntimePath{HomeVar: "AGENT_HOME", Segments: []string{"a;canary;b", "session-*.jsonl"}}},
		{name: "single quote and space", pattern: RuntimePath{HomeVar: "AGENT_HOME", Segments: []string{"it's canary", "*"}}},
		{name: "literal directory ahead of the wildcard", pattern: RuntimePath{HomeVar: "AGENT_HOME", Segments: []string{"canary", "chats", "*"}}},
		{name: "home default", pattern: RuntimePath{HomeVar: "AGENT_HOME", HomeDefault: []string{"$(canary)"}, Segments: []string{"canary", "*"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			command := remoteMatchCommand(tt.pattern)

			if strings.Contains(outsideQuotes(command), "canary") {
				t.Errorf("remoteMatchCommand(%q) = %q, want every segment inside single quotes", tt.pattern.Segments, command)
			}
			for _, segment := range slices.Concat(tt.pattern.HomeDefault, tt.pattern.Segments) {
				if want := sshutil.QuoteArgv([]string{segment}); !strings.Contains(command, want) {
					t.Errorf("remoteMatchCommand(%q) = %q, want it to hold %s", tt.pattern.Segments, command, want)
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

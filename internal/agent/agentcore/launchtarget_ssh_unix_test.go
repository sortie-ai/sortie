//go:build unix

package agentcore

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const (
	runtimeHomeVar = "SORTIE_TEST_RUNTIME_HOME"
	quotedHomeVar  = "SORTIE_TEST_QUOTED_HOME"
	remoteContent  = "remote-token"
	localContent   = "local-token"
	journalName    = "journal-64mib.jsonl"
	journalBound   = 10 << 20
)

// The lab shell sources ~/.bashrc, which is how the remote host sets a
// variable that Sortie never forwards.
var remoteHomeInstall = `home=/home/sortie/rt
quoted="/home/sortie/my dir/it's \"own\""
mkdir -p "$home" "$quoted" /home/sortie/carried /home/sortie/.rtdefault /home/sortie/.gemini
printf '%s' ` + remoteContent + ` | tee "$home/credentials.json" "$quoted/it's \"own\".json" > /dev/null
printf '%s' carried-token > /home/sortie/carried/credentials.json
printf '%s' default-token > /home/sortie/.rtdefault/credentials.json
printf '%s' gemini-token > /home/sortie/.gemini/oauth_creds.json
yes '{"k":"` + strings.Repeat("x", 1015) + `"}' | head -n 65536 > "$home/` + journalName + `"
head -n 1024 "$home/` + journalName + `" > "$home/journal-1mib.jsonl"
printf 'export ` + runtimeHomeVar + `=%s\nexport ` + quotedHomeVar + `="%s"\n' "$home" "$(printf '%s' "$quoted" | sed 's/"/\\"/g')" > /home/sortie/.bashrc
` + matchTreeInstall() + `
chown -R sortie:sortie /home/sortie`

func matchTreeInstall() string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	var commands []string
	for _, dir := range slices.Concat(matchTreeDirs, matchTreeDirsOfFiles()) {
		commands = append(commands, "mkdir -p "+quote(dir))
	}
	for _, file := range matchTreeFiles {
		commands = append(commands, ": > "+quote(file))
	}
	for _, link := range matchTreeLinks {
		commands = append(commands, "ln -s "+quote(link[1])+" "+quote(link[0]))
	}
	return `(cd "$home" && ` + strings.Join(commands, " && ") + `)`
}

func startRuntimeLab(t testing.TB) (LaunchTarget, agenttest.SSHLab) {
	t.Helper()

	if reason := agenttest.SSHLabSkipReason(); reason != "" {
		t.Skip(reason)
	}
	workspace := t.TempDir()
	lab := agenttest.StartSSHLab(t, agenttest.SSHLabParams{Install: remoteHomeInstall, Dirs: []string{workspace}})
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatalf("exec.LookPath(%q) = %v, want nil", "ssh", err)
	}
	return LaunchTarget{Command: ssh, SSHHost: lab.Host, WorkspacePath: workspace, RemoteCommand: "true"}, lab
}

func journalPath(name string) RuntimePath {
	return RuntimePath{HomeVar: runtimeHomeVar, Segments: []string{name}}
}

type runtimeRead func(ctx context.Context, target LaunchTarget, env []string, path RuntimePath, limit, lineLimit int64) (content string, fnCalls int, err error)

func readWhole(ctx context.Context, target LaunchTarget, env []string, path RuntimePath, limit, _ int64) (string, int, error) {
	content, err := target.ReadRuntimeFile(ctx, env, path, limit)
	return string(content), 0, err
}

func scanAll(ctx context.Context, target LaunchTarget, env []string, path RuntimePath, limit, lineLimit int64) (string, int, error) {
	var lines []string
	err := target.ScanRuntimeFile(ctx, env, path, limit, lineLimit, func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	})
	return strings.Join(lines, "\n"), len(lines), err
}

func TestLaunchTarget_ReadRuntimeFile_SSHLab(t *testing.T) {
	localHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(localHome, "credentials.json"), []byte(localContent), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	target, _ := startRuntimeLab(t)
	localEnv := []string{runtimeHomeVar + "=" + localHome}
	credentials := journalPath("credentials.json")
	tokenLen := int64(len(remoteContent))

	tests := []struct {
		name         string
		sshEnvNames  []string
		processValue string
		path         RuntimePath
		limit        int64
		lineLimit    int64
		want         string
		wantNotExist bool
		wantErr      bool
		scanOnly     bool
	}{
		{name: "host login home, not the local file at the same path", path: credentials, limit: 1024, want: remoteContent},
		{name: "home value and segment with a space and quotes", path: RuntimePath{HomeVar: quotedHomeVar, Segments: []string{`it's "own".json`}}, limit: 1024, want: remoteContent},
		{name: "blank carried home takes the host login value", sshEnvNames: []string{runtimeHomeVar}, processValue: "  ", path: credentials, limit: 1024, want: remoteContent},
		{name: "carried home takes the carried value", sshEnvNames: []string{runtimeHomeVar}, processValue: "/home/sortie/carried", path: credentials, limit: 1024, want: "carried-token"},
		{name: "unset home variable takes HOME joined with the home default", path: RuntimePath{HomeVar: "SORTIE_TEST_UNSET_HOME", HomeDefault: []string{".rtdefault"}, Segments: []string{"credentials.json"}}, limit: 1024, want: "default-token"},
		{name: "unset home variable and no home default takes HOME", path: RuntimePath{HomeVar: "SORTIE_TEST_UNSET_HOME", Segments: []string{".gemini", "oauth_creds.json"}}, limit: 1024, want: "gemini-token"},
		{name: "file exactly at the limit", path: credentials, limit: tokenLen, want: remoteContent},
		{name: "file above the limit", path: credentials, limit: tokenLen - 1, wantErr: true},
		{name: "line above the line limit", path: credentials, limit: 1024, lineLimit: tokenLen - 1, wantErr: true, scanOnly: true},
		{name: "missing file", path: journalPath("absent.json"), limit: 1024, wantNotExist: true, wantErr: true},
	}
	operations := map[string]runtimeRead{"ReadRuntimeFile": readWhole, "ScanRuntimeFile": scanAll}

	for _, tt := range tests {
		for op, read := range operations {
			if tt.scanOnly && op == "ReadRuntimeFile" {
				continue
			}
			t.Run(tt.name+"/"+op, func(t *testing.T) {
				target := target
				target.SSHEnvNames = tt.sshEnvNames
				if tt.processValue != "" {
					t.Setenv(runtimeHomeVar, tt.processValue)
				}

				got, calls, err := read(waittest.Context(t), target, localEnv, tt.path, tt.limit, cmp.Or(tt.lineLimit, 1024))

				if gotNotExist := errors.Is(err, fs.ErrNotExist); gotNotExist != tt.wantNotExist {
					t.Errorf("%s(%v, limit %d) error = %v, errors.Is(fs.ErrNotExist) = %t, want %t", op, tt.path.Segments, tt.limit, err, gotNotExist, tt.wantNotExist)
				}
				if (err != nil) != tt.wantErr {
					t.Fatalf("%s(%v, limit %d) error = %v, want error %t", op, tt.path.Segments, tt.limit, err, tt.wantErr)
				}
				if tt.wantNotExist && calls != 0 {
					t.Errorf("%s(%v) called fn %d times for a missing file, want 0", op, tt.path.Segments, calls)
				}
				if got != tt.want {
					t.Errorf("%s(%v, limit %d) = %q, want %q", op, tt.path.Segments, tt.limit, got, tt.want)
				}
			})
		}
	}
}

func TestLaunchTarget_MatchRuntimeFiles_SSHLab(t *testing.T) {
	target, _ := startRuntimeLab(t)

	for _, tt := range matchCases {
		t.Run(tt.name, func(t *testing.T) {
			pattern := RuntimePath{HomeVar: cmp.Or(tt.homeVar, runtimeHomeVar), HomeDefault: tt.homeDefault, Segments: tt.segments}

			got, err := target.MatchRuntimeFiles(waittest.Context(t), nil, pattern)

			assertMatched(t, got, err, pattern, tt.want)
		})
	}
}

func TestLaunchTarget_ScanRuntimeFile_SSHLab_CutStream(t *testing.T) {
	target, lab := startRuntimeLab(t)
	ctx := waittest.Context(t)
	firstLine := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		seen := false
		done <- target.ScanRuntimeFile(ctx, nil, journalPath(journalName), usage.MaxRecordsBytes, journalBound, func([]byte) error {
			if seen {
				return nil
			}
			seen = true
			close(firstLine)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	waittest.Receive(t, firstLine, "the scan delivering its first line")
	if err := lab.Kill(ctx); err != nil {
		t.Fatalf("SSHLab.Kill() = %v, want nil", err)
	}
	close(release)

	if err := waittest.Receive(t, done, "the scan ending"); err == nil {
		t.Error("ScanRuntimeFile() after the remote command died mid-file = nil, want an error")
	}
}

func BenchmarkLaunchTarget_ScanRuntimeFile_SSHLab(b *testing.B) {
	target, _ := startRuntimeLab(b)

	for _, bench := range []struct {
		name    string
		file    string
		bytes   int64
		wantErr error
	}{
		{name: "missing", file: "absent.jsonl", wantErr: fs.ErrNotExist},
		{name: "1MiB", file: "journal-1mib.jsonl", bytes: 1 << 20},
		{name: "64MiB", file: journalName, bytes: usage.MaxRecordsBytes},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.SetBytes(bench.bytes)
			b.ReportAllocs()

			for b.Loop() {
				err := target.ScanRuntimeFile(waittest.Context(b), nil, journalPath(bench.file), usage.MaxRecordsBytes, journalBound, func([]byte) error { return nil })
				if !errors.Is(err, bench.wantErr) {
					b.Fatalf("ScanRuntimeFile(%q) = %v, want %v", bench.file, err, bench.wantErr)
				}
			}
		})
	}
}

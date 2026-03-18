package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimalWorkflow returns a minimal valid WORKFLOW.md content.
func minimalWorkflow() []byte {
	return []byte("---\npolling:\n  interval_ms: 30000\n---\nDo {{ .issue.title }}.\n")
}

// writeWorkflowFile creates a WORKFLOW.md in dir and returns its absolute path.
func writeWorkflowFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, minimalWorkflow(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q", out, "sortie "+Version)
	}
	if !strings.Contains(out, "Copyright") {
		t.Errorf("stdout = %q, want to contain %q", out, "Copyright")
	}
	if !strings.Contains(out, "warranty") {
		t.Errorf("stdout = %q, want to contain %q", out, "warranty")
	}
}

func TestRunDumpVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"-dumpversion"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("stdout = %q, want %q", got, Version)
	}
}

func TestRunDumpVersionOverridesVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version", "-dumpversion"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("-dumpversion should take precedence; stdout = %q, want %q", got, Version)
	}
}

func TestRunVersionIgnoresWorkflowPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version", "/nonexistent/workflow.md"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--unknown"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("stderr should contain usage text")
	}
}

func TestRunTooManyArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"a", "b"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "too many arguments") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "too many arguments")
	}
}

func TestRunNonexistentPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{"/nonexistent/workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
}

func TestRunMissingDefaultWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
}

func TestRunValidWorkflowWithTimeout(t *testing.T) {
	dir := t.TempDir()
	wfPath := writeWorkflowFile(t, dir)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunAlreadyCancelledContext(t *testing.T) {
	dir := t.TempDir()
	wfPath := writeWorkflowFile(t, dir)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunPortFlagLogged(t *testing.T) {
	dir := t.TempDir()
	wfPath := writeWorkflowFile(t, dir)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{"--port", "8080", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "port=8080") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "port=8080")
	}
}

func TestResolveWorkflowPath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    []string
		wantEnd string
		wantErr bool
	}{
		{
			name:    "no args defaults to WORKFLOW.md",
			args:    []string{},
			wantEnd: "WORKFLOW.md",
		},
		{
			name:    "single arg returns absolute",
			args:    []string{"my-file.md"},
			wantEnd: fmt.Sprintf("%s/my-file.md", cwd),
		},
		{
			name:    "absolute arg returned as-is",
			args:    []string{"/tmp/wf.md"},
			wantEnd: "/tmp/wf.md",
		},
		{
			name:    "two args returns error",
			args:    []string{"a", "b"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWorkflowPath(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !filepath.IsAbs(got) {
				t.Errorf("path %q is not absolute", got)
			}
			if tt.wantEnd != "" && !strings.HasSuffix(got, filepath.Base(tt.wantEnd)) {
				t.Errorf("path = %q, want to end with %q", got, filepath.Base(tt.wantEnd))
			}
			if len(tt.args) == 1 && filepath.IsAbs(tt.args[0]) {
				if got != tt.wantEnd {
					t.Errorf("path = %q, want %q", got, tt.wantEnd)
				}
			}
		})
	}
}

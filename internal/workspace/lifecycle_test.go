//go:build unix

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %q to exist: %v", path, err)
	}
}

func assertFileNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %q not to exist, stat err: %v", path, err)
	}
}

func mustEnsure(t *testing.T, root, identifier string) EnsureResult {
	t.Helper()
	res, err := Ensure(root, identifier)
	if err != nil {
		t.Fatalf("Ensure(%q, %q): %v", root, identifier, err)
	}
	return res
}

func TestHookEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		issueID     string
		identifier  string
		workspace   string
		attempt     int
		wantAttempt string
	}{
		{"positive attempt", "id-1", "PROJ-42", "/ws/PROJ-42", 3, "3"},
		{"zero attempt", "id-2", "PROJ-99", "/ws/PROJ-99", 0, "0"},
		{"negative attempt clamped", "id-3", "X-1", "/ws/X-1", -5, "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := HookEnv(tt.issueID, tt.identifier, tt.workspace, tt.attempt)

			if got := env["SORTIE_ISSUE_ID"]; got != tt.issueID {
				t.Errorf("SORTIE_ISSUE_ID = %q, want %q", got, tt.issueID)
			}
			if got := env["SORTIE_ISSUE_IDENTIFIER"]; got != tt.identifier {
				t.Errorf("SORTIE_ISSUE_IDENTIFIER = %q, want %q", got, tt.identifier)
			}
			if got := env["SORTIE_WORKSPACE"]; got != tt.workspace {
				t.Errorf("SORTIE_WORKSPACE = %q, want %q", got, tt.workspace)
			}
			if got := env["SORTIE_ATTEMPT"]; got != tt.wantAttempt {
				t.Errorf("SORTIE_ATTEMPT = %q, want %q", got, tt.wantAttempt)
			}
			if len(env) != 4 {
				t.Errorf("env has %d keys, want 4", len(env))
			}
		})
	}
}

func TestHookEnv_SSHHost(t *testing.T) {
	t.Parallel()

	t.Run("with ssh host", func(t *testing.T) {
		t.Parallel()

		env := HookEnv("id-1", "PROJ-1", "/ws", 1, "worker-host")
		if got := env["SORTIE_SSH_HOST"]; got != "worker-host" {
			t.Errorf("SORTIE_SSH_HOST = %q, want %q", got, "worker-host")
		}
		if len(env) != 5 {
			t.Errorf("env has %d keys, want 5", len(env))
		}
	})

	t.Run("empty ssh host omitted", func(t *testing.T) {
		t.Parallel()

		env := HookEnv("id-1", "PROJ-1", "/ws", 1, "")
		if _, ok := env["SORTIE_SSH_HOST"]; ok {
			t.Error("SORTIE_SSH_HOST present with empty host, want absent")
		}
		if len(env) != 4 {
			t.Errorf("env has %d keys, want 4", len(env))
		}
	})

	t.Run("no ssh host arg", func(t *testing.T) {
		t.Parallel()

		env := HookEnv("id-1", "PROJ-1", "/ws", 1)
		if _, ok := env["SORTIE_SSH_HOST"]; ok {
			t.Error("SORTIE_SSH_HOST present without arg, want absent")
		}
		if len(env) != 4 {
			t.Errorf("env has %d keys, want 4", len(env))
		}
	})
}

func TestPrepare(t *testing.T) {
	t.Parallel()

	t.Run("new workspace no hooks", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-1",
			IssueID:       "id-1",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}
		if !result.CreatedNow {
			t.Errorf("Prepare().CreatedNow = false, want true")
		}
		assertFileExists(t, result.Path)
	})

	t.Run("new workspace after_create succeeds", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-2",
			IssueID:       "id-2",
			Attempt:       1,
			AfterCreate:   `touch "$SORTIE_WORKSPACE/.created"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}
		if !result.CreatedNow {
			t.Errorf("Prepare().CreatedNow = false, want true")
		}
		assertFileExists(t, filepath.Join(result.Path, ".created"))
	})

	t.Run("new workspace after_create fails rollback", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		_, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-3",
			IssueID:       "id-3",
			Attempt:       1,
			AfterCreate:   "exit 1",
			HookTimeoutMS: 5000,
		})
		_ = requireHookError(t, err)
		assertFileNotExists(t, filepath.Join(root, "PROJ-3"))
	})

	t.Run("new workspace after_create timeout", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		_, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-4",
			IssueID:       "id-4",
			Attempt:       1,
			AfterCreate:   "sleep 60",
			HookTimeoutMS: 100,
		})
		assertHookErrorOp(t, err, "timeout")
		assertFileNotExists(t, filepath.Join(root, "PROJ-4"))
	})

	t.Run("after_create fail then retry", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		params := PrepareParams{
			Root:          root,
			Identifier:    "PROJ-5",
			IssueID:       "id-5",
			Attempt:       1,
			AfterCreate:   "exit 1",
			HookTimeoutMS: 5000,
		}

		_, err := Prepare(context.Background(), params)
		if err == nil {
			t.Fatal("first Prepare() should fail")
		}

		// Retry with a succeeding hook.
		params.AfterCreate = `touch "$SORTIE_WORKSPACE/.created"`
		result, err := Prepare(context.Background(), params)
		if err != nil {
			t.Fatalf("second Prepare() error: %v", err)
		}
		if !result.CreatedNow {
			t.Errorf("Prepare().CreatedNow = false on retry, want true")
		}
		assertFileExists(t, filepath.Join(result.Path, ".created"))
	})

	t.Run("existing workspace after_create not run", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		wsPath := filepath.Join(root, "PROJ-6")
		if err := os.Mkdir(wsPath, 0o750); err != nil {
			t.Fatalf("setup: %v", err)
		}

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-6",
			IssueID:       "id-6",
			Attempt:       1,
			AfterCreate:   `touch "$SORTIE_WORKSPACE/.should_not_exist"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}
		if result.CreatedNow {
			t.Errorf("Prepare().CreatedNow = true, want false for existing workspace")
		}
		assertFileNotExists(t, filepath.Join(wsPath, ".should_not_exist"))
	})

	t.Run("before_run succeeds", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-7",
			IssueID:       "id-7",
			Attempt:       1,
			BeforeRun:     `touch "$SORTIE_WORKSPACE/.before_run"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		assertFileExists(t, filepath.Join(result.Path, ".before_run"))
	})

	t.Run("before_run fails workspace preserved", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		_, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-8",
			IssueID:       "id-8",
			Attempt:       1,
			BeforeRun:     "exit 1",
			HookTimeoutMS: 5000,
		})
		_ = requireHookError(t, err)
		assertFileExists(t, filepath.Join(root, "PROJ-8"))
	})

	t.Run("before_run timeout", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		_, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-9",
			IssueID:       "id-9",
			Attempt:       1,
			BeforeRun:     "sleep 60",
			HookTimeoutMS: 100,
		})
		assertHookErrorOp(t, err, "timeout")
	})

	t.Run("no hooks configured", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PROJ-10",
			IssueID:       "id-10",
			Attempt:       0,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}
		if result.Key != "PROJ-10" {
			t.Errorf("Key = %q, want %q", result.Key, "PROJ-10")
		}
		if !result.CreatedNow {
			t.Errorf("Prepare().CreatedNow = false, want true")
		}
	})

	t.Run("hook env vars correct", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		envScript := `printf "%s\n%s\n%s\n%s" "$SORTIE_ISSUE_ID" "$SORTIE_ISSUE_IDENTIFIER" "$SORTIE_WORKSPACE" "$SORTIE_ATTEMPT" > "$SORTIE_WORKSPACE/env.txt"`
		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "ENV-1",
			IssueID:       "tracker-id-99",
			Attempt:       7,
			BeforeRun:     envScript,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(result.Path, "env.txt"))
		if err != nil {
			t.Fatalf("ReadFile(env.txt): %v", err)
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) < 4 {
			t.Fatalf("expected 4 lines, got %d: %q", len(lines), string(data))
		}
		if lines[0] != "tracker-id-99" {
			t.Errorf("SORTIE_ISSUE_ID = %q, want %q", lines[0], "tracker-id-99")
		}
		if lines[1] != "ENV-1" {
			t.Errorf("SORTIE_ISSUE_IDENTIFIER = %q, want %q", lines[1], "ENV-1")
		}
		if lines[2] != result.Path {
			t.Errorf("SORTIE_WORKSPACE = %q, want %q", lines[2], result.Path)
		}
		if lines[3] != "7" {
			t.Errorf("SORTIE_ATTEMPT = %q, want %q", lines[3], "7")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := Prepare(ctx, PrepareParams{
			Root:          root,
			Identifier:    "PROJ-11",
			IssueID:       "id-11",
			Attempt:       1,
			BeforeRun:     "sleep 60",
			HookTimeoutMS: 30000,
		})
		if err == nil {
			t.Fatal("Prepare() with cancelled context should return error")
		}
	})
}

func TestFinish(t *testing.T) {
	t.Parallel()

	t.Run("after_run succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		marker := filepath.Join(dir, ".after_run")

		Finish(context.Background(), FinishParams{
			Path:          dir,
			Identifier:    "F-1",
			IssueID:       "id-1",
			Attempt:       1,
			AfterRun:      `touch "$SORTIE_WORKSPACE/.after_run"`,
			HookTimeoutMS: 5000,
		})

		assertFileExists(t, marker)
	})

	t.Run("after_run fails no panic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Should not panic or return error — failure is logged and ignored.
		Finish(context.Background(), FinishParams{
			Path:          dir,
			Identifier:    "F-2",
			IssueID:       "id-2",
			Attempt:       1,
			AfterRun:      "exit 1",
			HookTimeoutMS: 5000,
		})
	})

	t.Run("after_run timeout no panic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		Finish(context.Background(), FinishParams{
			Path:          dir,
			Identifier:    "F-3",
			IssueID:       "id-3",
			Attempt:       1,
			AfterRun:      "sleep 60",
			HookTimeoutMS: 100,
		})
	})

	t.Run("no after_run configured", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		Finish(context.Background(), FinishParams{
			Path:          dir,
			Identifier:    "F-4",
			IssueID:       "id-4",
			Attempt:       1,
			AfterRun:      "",
			HookTimeoutMS: 5000,
		})

		assertFileNotExists(t, filepath.Join(dir, ".should_not_exist"))
	})

	// context.WithoutCancel: teardown hook runs despite cancelled parent
	t.Run("cancelled parent context hook still runs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		marker := filepath.Join(dir, ".after_run_detached")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		Finish(ctx, FinishParams{
			Path:          dir,
			Identifier:    "F-5",
			IssueID:       "id-5",
			Attempt:       1,
			AfterRun:      `touch "` + marker + `"`,
			HookTimeoutMS: 5000,
		})

		assertFileExists(t, marker)
	})

	t.Run("SORTIE_SELF_REVIEW_STATUS defaults to disabled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "sr_status.txt")

		Finish(context.Background(), FinishParams{
			Path:             dir,
			Identifier:       "F-SR-1",
			IssueID:          "id-sr-1",
			Attempt:          1,
			AfterRun:         `echo -n "$SORTIE_SELF_REVIEW_STATUS" > "` + out + `"`,
			HookTimeoutMS:    5000,
			SelfReviewStatus: "", // empty → "disabled"
		})

		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading %q: %v", out, err)
		}
		if string(data) != "disabled" {
			t.Errorf("SORTIE_SELF_REVIEW_STATUS = %q, want %q", string(data), "disabled")
		}
	})

	t.Run("SORTIE_SELF_REVIEW_STATUS passed when set", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "sr_status.txt")

		Finish(context.Background(), FinishParams{
			Path:             dir,
			Identifier:       "F-SR-2",
			IssueID:          "id-sr-2",
			Attempt:          1,
			AfterRun:         `echo -n "$SORTIE_SELF_REVIEW_STATUS" > "` + out + `"`,
			HookTimeoutMS:    5000,
			SelfReviewStatus: "passed",
		})

		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading %q: %v", out, err)
		}
		if string(data) != "passed" {
			t.Errorf("SORTIE_SELF_REVIEW_STATUS = %q, want %q", string(data), "passed")
		}
	})

	t.Run("SORTIE_SELF_REVIEW_SUMMARY_PATH set when non-empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "sr_summary.txt")
		summaryPath := "/workspace/.sortie/review_summary.md"

		Finish(context.Background(), FinishParams{
			Path:                  dir,
			Identifier:            "F-SR-3",
			IssueID:               "id-sr-3",
			Attempt:               1,
			AfterRun:              `echo -n "$SORTIE_SELF_REVIEW_SUMMARY_PATH" > "` + out + `"`,
			HookTimeoutMS:         5000,
			SelfReviewStatus:      "passed",
			SelfReviewSummaryPath: summaryPath,
		})

		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading %q: %v", out, err)
		}
		if string(data) != summaryPath {
			t.Errorf("SORTIE_SELF_REVIEW_SUMMARY_PATH = %q, want %q", string(data), summaryPath)
		}
	})

	t.Run("SORTIE_SELF_REVIEW_SUMMARY_PATH absent when empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "sr_summary.txt")

		// Write "PRESENT" only if the env var exists and is non-empty.
		Finish(context.Background(), FinishParams{
			Path:                  dir,
			Identifier:            "F-SR-4",
			IssueID:               "id-sr-4",
			Attempt:               1,
			AfterRun:              `[ -n "$SORTIE_SELF_REVIEW_SUMMARY_PATH" ] && echo -n "PRESENT" > "` + out + `" || true`,
			HookTimeoutMS:         5000,
			SelfReviewStatus:      "disabled",
			SelfReviewSummaryPath: "",
		})

		// File should not be created (env var absent or empty).
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			data, _ := os.ReadFile(out)
			t.Errorf("SORTIE_SELF_REVIEW_SUMMARY_PATH should be absent, but hook saw it; content = %q", data)
		}
	})
}

func TestCleanup(t *testing.T) {
	t.Parallel()

	t.Run("workspace exists no hook", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		res := mustEnsure(t, root, "CLN-1")

		err := Cleanup(context.Background(), CleanupParams{
			Root:          root,
			Identifier:    "CLN-1",
			IssueID:       "id-1",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Cleanup() error: %v", err)
		}
		assertFileNotExists(t, res.Path)
	})

	t.Run("workspace exists before_remove succeeds", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		markerDir := t.TempDir()
		marker := filepath.Join(markerDir, "before_remove_marker")
		res := mustEnsure(t, root, "CLN-2")

		err := Cleanup(context.Background(), CleanupParams{
			Root:          root,
			Identifier:    "CLN-2",
			IssueID:       "id-2",
			Attempt:       1,
			BeforeRemove:  `touch "` + marker + `"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Cleanup() error: %v", err)
		}
		assertFileExists(t, marker)
		assertFileNotExists(t, res.Path)
	})

	t.Run("workspace exists before_remove fails removal proceeds", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		res := mustEnsure(t, root, "CLN-3")

		err := Cleanup(context.Background(), CleanupParams{
			Root:          root,
			Identifier:    "CLN-3",
			IssueID:       "id-3",
			Attempt:       1,
			BeforeRemove:  "exit 1",
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Cleanup() error: %v", err)
		}
		assertFileNotExists(t, res.Path)
	})

	t.Run("workspace does not exist idempotent", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		err := Cleanup(context.Background(), CleanupParams{
			Root:          root,
			Identifier:    "CLN-NONE",
			IssueID:       "id-4",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Cleanup() of non-existent workspace should return nil, got: %v", err)
		}
	})

	t.Run("invalid identifier", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		err := Cleanup(context.Background(), CleanupParams{
			Root:          root,
			Identifier:    "",
			IssueID:       "id-5",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err == nil {
			t.Fatal("Cleanup() with empty identifier should return error")
		}
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error type = %T, want *PathError", err)
		}
	})

	// context.WithoutCancel: before_remove hook runs despite cancelled parent
	t.Run("cancelled parent context hook still runs", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		markerDir := t.TempDir()
		marker := filepath.Join(markerDir, "detached_marker")
		res := mustEnsure(t, root, "CLN-6")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := Cleanup(ctx, CleanupParams{
			Root:          root,
			Identifier:    "CLN-6",
			IssueID:       "id-6",
			Attempt:       1,
			BeforeRemove:  `touch "` + marker + `"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("Cleanup() error: %v", err)
		}
		assertFileExists(t, marker)
		assertFileNotExists(t, res.Path)
	})
}

func TestCleanupByPath(t *testing.T) {
	t.Parallel()

	t.Run("workspace exists no hook", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wsDir := filepath.Join(dir, "WS-1")
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          wsDir,
			Identifier:    "WS-1",
			IssueID:       "id-1",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("CleanupByPath() error: %v", err)
		}
		assertFileNotExists(t, wsDir)
	})

	t.Run("workspace exists before_remove succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wsDir := filepath.Join(dir, "WS-2")
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		markerDir := t.TempDir()
		marker := filepath.Join(markerDir, "before_remove_marker")

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          wsDir,
			Identifier:    "WS-2",
			IssueID:       "id-2",
			Attempt:       1,
			BeforeRemove:  `touch "` + marker + `"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("CleanupByPath() error: %v", err)
		}
		assertFileExists(t, marker)
		assertFileNotExists(t, wsDir)
	})

	t.Run("workspace exists before_remove fails removal proceeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wsDir := filepath.Join(dir, "WS-3")
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          wsDir,
			Identifier:    "WS-3",
			IssueID:       "id-3",
			Attempt:       1,
			BeforeRemove:  "exit 1",
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("CleanupByPath() error: %v", err)
		}
		assertFileNotExists(t, wsDir)
	})

	t.Run("workspace does not exist idempotent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		nonExistent := filepath.Join(dir, "no-such-dir")

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          nonExistent,
			Identifier:    "NONE",
			IssueID:       "id-4",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("CleanupByPath() of non-existent path should return nil, got: %v", err)
		}
	})

	t.Run("empty path returns error", func(t *testing.T) {
		t.Parallel()

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          "",
			Identifier:    "EMPTY",
			IssueID:       "id-5",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err == nil {
			t.Fatal("CleanupByPath(\"\") should return error")
		}
		if !strings.Contains(err.Error(), "workspace path must not be empty") {
			t.Errorf("CleanupByPath(\"\") error = %q, want to contain %q",
				err.Error(), "workspace path must not be empty")
		}
	})

	t.Run("relative path returns error", func(t *testing.T) {
		t.Parallel()

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          "relative/path",
			Identifier:    "REL",
			IssueID:       "id-7",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err == nil {
			t.Fatal(`CleanupByPath("relative/path") should return error`)
		}
		if !strings.Contains(err.Error(), "workspace path must be absolute") {
			t.Errorf(`CleanupByPath("relative/path") error = %q, want to contain %q`,
				err.Error(), "workspace path must be absolute")
		}
	})

	t.Run("filesystem root rejected", func(t *testing.T) {
		t.Parallel()

		err := CleanupByPath(context.Background(), CleanupByPathParams{
			Path:          "/",
			Identifier:    "ROOT",
			IssueID:       "id-8",
			Attempt:       1,
			HookTimeoutMS: 5000,
		})
		if err == nil {
			t.Fatal(`CleanupByPath("/") should return error`)
		}
		if !strings.Contains(err.Error(), "refusing to remove filesystem root") {
			t.Errorf(`CleanupByPath("/") error = %q, want to contain %q`,
				err.Error(), "refusing to remove filesystem root")
		}
	})

	// context.WithoutCancel: before_remove hook runs despite cancelled parent.
	t.Run("cancelled parent context hook still runs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wsDir := filepath.Join(dir, "WS-6")
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		markerDir := t.TempDir()
		marker := filepath.Join(markerDir, "detached_marker")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := CleanupByPath(ctx, CleanupByPathParams{
			Path:          wsDir,
			Identifier:    "WS-6",
			IssueID:       "id-6",
			Attempt:       1,
			BeforeRemove:  `touch "` + marker + `"`,
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("CleanupByPath() error: %v", err)
		}
		assertFileExists(t, marker)
		assertFileNotExists(t, wsDir)
	})
}

// TestLifecycleFullSequence exercises the complete Prepare → Finish → Cleanup
// sequence with real hook scripts writing marker files at each stage.
func TestLifecycleFullSequence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	markerDir := t.TempDir()

	// Phase 1: Prepare with after_create and before_run hooks.
	result, err := Prepare(context.Background(), PrepareParams{
		Root:          root,
		Identifier:    "LIFE-1",
		IssueID:       "lifecycle-id",
		Attempt:       2,
		AfterCreate:   `touch "$SORTIE_WORKSPACE/.after_create_marker"`,
		BeforeRun:     `touch "$SORTIE_WORKSPACE/.before_run_marker"`,
		HookTimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("Prepare() error: %v", err)
	}
	if !result.CreatedNow {
		t.Errorf("Prepare().CreatedNow = false, want true")
	}
	assertFileExists(t, filepath.Join(result.Path, ".after_create_marker"))
	assertFileExists(t, filepath.Join(result.Path, ".before_run_marker"))

	// Phase 2: Finish with after_run hook.
	Finish(context.Background(), FinishParams{
		Path:          result.Path,
		Identifier:    "LIFE-1",
		IssueID:       "lifecycle-id",
		Attempt:       2,
		AfterRun:      `touch "$SORTIE_WORKSPACE/.after_run_marker"`,
		HookTimeoutMS: 5000,
	})
	assertFileExists(t, filepath.Join(result.Path, ".after_run_marker"))

	// Phase 3: Cleanup with before_remove hook writing outside workspace.
	beforeRemoveMarker := filepath.Join(markerDir, "before_remove_marker")
	err = Cleanup(context.Background(), CleanupParams{
		Root:          root,
		Identifier:    "LIFE-1",
		IssueID:       "lifecycle-id",
		Attempt:       2,
		BeforeRemove:  `touch "` + beforeRemoveMarker + `"`,
		HookTimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
	assertFileExists(t, beforeRemoveMarker)
	assertFileNotExists(t, result.Path)
}

// TestCleanupTerminal exercises the batch workspace removal function
// used during startup cleanup of terminal-state issues.
func TestCleanupTerminal(t *testing.T) {
	t.Parallel()

	t.Run("all workspaces removed no hook", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		mustEnsure(t, root, "T-1")
		mustEnsure(t, root, "T-2")

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{"T-1", "T-2"},
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 0 {
			t.Fatalf("CleanupTerminal() errors: %v", result.Errors)
		}
		if len(result.Removed) != 2 {
			t.Errorf("CleanupTerminal() removed %d, want 2", len(result.Removed))
		}
		assertFileNotExists(t, filepath.Join(root, "T-1"))
		assertFileNotExists(t, filepath.Join(root, "T-2"))
	})

	t.Run("missing workspace is idempotent", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{"NO-EXIST"},
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 0 {
			t.Errorf("CleanupTerminal() errors: %v", result.Errors)
		}
		if len(result.Removed) != 1 || result.Removed[0] != "NO-EXIST" {
			t.Errorf("CleanupTerminal().Removed = %v, want [NO-EXIST]", result.Removed)
		}
	})

	t.Run("before_remove hook writes marker", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		markerDir := t.TempDir()
		marker := filepath.Join(markerDir, "hook_ran")
		mustEnsure(t, root, "H-1")

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{"H-1"},
			BeforeRemove:  `touch "` + marker + `"`,
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 0 {
			t.Fatalf("CleanupTerminal() errors: %v", result.Errors)
		}
		assertFileExists(t, marker)
		assertFileNotExists(t, filepath.Join(root, "H-1"))
	})

	t.Run("empty identifier collected in errors", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{""},
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 1 {
			t.Fatalf("CleanupTerminal() errors count = %d, want 1", len(result.Errors))
		}
		if _, ok := result.Errors[""]; !ok {
			t.Error("CleanupTerminal() expected error for empty identifier")
		}
		var pe *PathError
		if !errors.As(result.Errors[""], &pe) {
			t.Errorf("error type = %T, want *PathError", result.Errors[""])
		}
	})

	t.Run("mixed valid and invalid", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		mustEnsure(t, root, "OK-1")

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{"OK-1", ""},
			HookTimeoutMS: 5000,
		})
		if len(result.Removed) != 1 || result.Removed[0] != "OK-1" {
			t.Errorf("Removed = %v, want [OK-1]", result.Removed)
		}
		if len(result.Errors) != 1 {
			t.Errorf("Errors count = %d, want 1", len(result.Errors))
		}
		assertFileNotExists(t, filepath.Join(root, "OK-1"))
	})

	t.Run("issue ID lookup from map", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		markerDir := t.TempDir()
		envFile := filepath.Join(markerDir, "env.txt")
		mustEnsure(t, root, "MAP-1")

		script := `printf "%s" "$SORTIE_ISSUE_ID" > "` + envFile + `"`
		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:        root,
			Identifiers: []string{"MAP-1"},
			IssueIDsByIdentifier: map[string]string{
				"MAP-1": "tracker-id-42",
			},
			BeforeRemove:  script,
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 0 {
			t.Fatalf("CleanupTerminal() errors: %v", result.Errors)
		}
		data, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got := string(data); got != "tracker-id-42" {
			t.Errorf("SORTIE_ISSUE_ID = %q, want %q", got, "tracker-id-42")
		}
	})

	t.Run("issue ID fallback to identifier", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		markerDir := t.TempDir()
		envFile := filepath.Join(markerDir, "env.txt")
		mustEnsure(t, root, "FALL-1")

		script := `printf "%s" "$SORTIE_ISSUE_ID" > "` + envFile + `"`
		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{"FALL-1"},
			BeforeRemove:  script,
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 0 {
			t.Fatalf("CleanupTerminal() errors: %v", result.Errors)
		}
		data, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got := string(data); got != "FALL-1" {
			t.Errorf("SORTIE_ISSUE_ID = %q, want %q (fallback to identifier)", got, "FALL-1")
		}
	})

	t.Run("empty identifiers list", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{},
			HookTimeoutMS: 5000,
		})
		if len(result.Removed) != 0 {
			t.Errorf("Removed = %v, want empty", result.Removed)
		}
		if len(result.Errors) != 0 {
			t.Errorf("Errors = %v, want empty", result.Errors)
		}
	})

	t.Run("nil identifiers list", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			HookTimeoutMS: 5000,
		})
		if len(result.Removed) != 0 {
			t.Errorf("Removed = %v, want empty", result.Removed)
		}
		if len(result.Errors) != 0 {
			t.Errorf("Errors = %v, want empty", result.Errors)
		}
	})

	t.Run("only terminal workspaces removed others preserved", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		mustEnsure(t, root, "ACTIVE-1")
		mustEnsure(t, root, "TERMINAL-1")
		mustEnsure(t, root, "TERMINAL-2")

		result := CleanupTerminal(context.Background(), CleanupTerminalParams{
			Root:          root,
			Identifiers:   []string{"TERMINAL-1", "TERMINAL-2"},
			HookTimeoutMS: 5000,
		})
		if len(result.Errors) != 0 {
			t.Fatalf("CleanupTerminal() errors: %v", result.Errors)
		}
		if len(result.Removed) != 2 {
			t.Errorf("CleanupTerminal() removed %d, want 2", len(result.Removed))
		}
		assertFileExists(t, filepath.Join(root, "ACTIVE-1"))
		assertFileNotExists(t, filepath.Join(root, "TERMINAL-1"))
		assertFileNotExists(t, filepath.Join(root, "TERMINAL-2"))
	})
}

// TestPrepare_PreRunFunc covers the PreRunFunc callback added to PrepareParams.
func TestPrepare_PreRunFunc(t *testing.T) {
	t.Parallel()

	t.Run("called with workspace path on new workspace", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		var capturedPath string

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PRE-1",
			IssueID:       "id-pre-1",
			Attempt:       0,
			HookTimeoutMS: 5000,
			PreRunFunc: func(wsPath string) {
				capturedPath = wsPath
			},
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}

		if capturedPath == "" {
			t.Fatal("PreRunFunc was not called")
		}
		if capturedPath != result.Path {
			t.Errorf("PreRunFunc path = %q, want %q", capturedPath, result.Path)
		}
	})

	t.Run("ordering: after after_create, before before_run", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()

		var afterCreateExistsAtPreRun bool
		var beforeRunExistsAtPreRun bool

		result, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PRE-2",
			IssueID:       "id-pre-2",
			Attempt:       0,
			AfterCreate:   `touch "$SORTIE_WORKSPACE/.after_create_marker"`,
			BeforeRun:     `touch "$SORTIE_WORKSPACE/.before_run_marker"`,
			HookTimeoutMS: 5000,
			PreRunFunc: func(wsPath string) {
				_, err1 := os.Stat(filepath.Join(wsPath, ".after_create_marker"))
				afterCreateExistsAtPreRun = err1 == nil
				_, err2 := os.Stat(filepath.Join(wsPath, ".before_run_marker"))
				beforeRunExistsAtPreRun = err2 == nil
			},
		})
		if err != nil {
			t.Fatalf("Prepare() error: %v", err)
		}
		if !result.CreatedNow {
			t.Fatal("expected workspace to be freshly created")
		}

		if !afterCreateExistsAtPreRun {
			t.Error("after_create marker not present when PreRunFunc ran, want present")
		}
		if beforeRunExistsAtPreRun {
			t.Error("before_run marker already present when PreRunFunc ran, want absent")
		}

		// Verify before_run ran after PreRunFunc.
		assertFileExists(t, filepath.Join(result.Path, ".before_run_marker"))
	})

	t.Run("called even when workspace already exists", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		// First call creates the workspace.
		_, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PRE-3",
			IssueID:       "id-pre-3",
			HookTimeoutMS: 5000,
		})
		if err != nil {
			t.Fatalf("first Prepare() error: %v", err)
		}

		var callCount int
		// Second call: workspace exists, after_create is skipped.
		_, err = Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PRE-3",
			IssueID:       "id-pre-3",
			HookTimeoutMS: 5000,
			PreRunFunc: func(_ string) {
				callCount++
			},
		})
		if err != nil {
			t.Fatalf("second Prepare() error: %v", err)
		}

		if callCount != 1 {
			t.Errorf("PreRunFunc call count = %d, want 1 on re-prepare", callCount)
		}
	})

	t.Run("nil PreRunFunc is no-op", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		// Must not panic with nil PreRunFunc.
		_, err := Prepare(context.Background(), PrepareParams{
			Root:          root,
			Identifier:    "PRE-4",
			IssueID:       "id-pre-4",
			HookTimeoutMS: 5000,
			PreRunFunc:    nil,
		})
		if err != nil {
			t.Fatalf("Prepare() with nil PreRunFunc error: %v", err)
		}
	})
}

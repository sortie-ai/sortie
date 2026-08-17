package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func handoffEvidenceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func newHandoffEvidenceGitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	handoffEvidenceGit(t, dir, "init")
	handoffEvidenceGit(t, dir, "config", "user.name", "Sortie Test")
	handoffEvidenceGit(t, dir, "config", "user.email", "sortie@example.test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	handoffEvidenceGit(t, dir, "add", "tracked.txt")
	handoffEvidenceGit(t, dir, "commit", "-m", "initial")
	return dir
}

func TestHandoffEvidenceBaselineNoChange(t *testing.T) {
	dir := newHandoffEvidenceGitWorkspace(t)
	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}

	change, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
	}
	if change.CommitMoved || change.WorktreeChanged {
		t.Errorf("change = %+v, want no change", change)
	}
}

func TestHandoffEvidenceDetectsCommitMovement(t *testing.T) {
	dir := newHandoffEvidenceGitWorkspace(t)
	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	handoffEvidenceGit(t, dir, "add", "tracked.txt")
	handoffEvidenceGit(t, dir, "commit", "-m", "work")

	change, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
	}
	if !change.CommitMoved {
		t.Errorf("change = %+v, want CommitMoved", change)
	}
	if change.WorktreeChanged {
		t.Errorf("change = %+v, clean tree should retain its clean-tree fingerprint", change)
	}
}

func TestHandoffEvidenceDetectsWorkingTreeChange(t *testing.T) {
	dir := newHandoffEvidenceGitWorkspace(t)
	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("modified\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	change, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
	}
	if !change.WorktreeChanged {
		t.Errorf("change = %+v, want WorktreeChanged", change)
	}
}

func TestHandoffEvidencePreexistingDirtyTreeNeedsFurtherChange(t *testing.T) {
	dir := newHandoffEvidenceGitWorkspace(t)
	path := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(path, []byte("dirty before run\n"), 0o600); err != nil {
		t.Fatalf("write preexisting dirty file: %v", err)
	}
	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}

	unchanged, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline unchanged: %v", err)
	}
	if unchanged.CommitMoved || unchanged.WorktreeChanged {
		t.Errorf("unchanged dirty tree = %+v, want no run-local change", unchanged)
	}

	if err := os.WriteFile(path, []byte("dirty and changed during run\n"), 0o600); err != nil {
		t.Fatalf("update dirty file: %v", err)
	}
	changed, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline changed: %v", err)
	}
	if !changed.WorktreeChanged {
		t.Errorf("changed dirty tree = %+v, want WorktreeChanged", changed)
	}
}

func TestHandoffEvidenceDetectsUntrackedContentChange(t *testing.T) {
	dir := newHandoffEvidenceGitWorkspace(t)
	path := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}
	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}
	if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
		t.Fatalf("update untracked file: %v", err)
	}

	change, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
	}
	if !change.WorktreeChanged {
		t.Errorf("change = %+v, want WorktreeChanged", change)
	}
}

func TestHandoffEvidenceExcludesSortieControlFiles(t *testing.T) {
	dir := newHandoffEvidenceGitWorkspace(t)
	baseline, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".sortie"), 0o700); err != nil {
		t.Fatalf("create .sortie: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sortie", "state.json"), []byte(`{"turn":1}`), 0o600); err != nil {
		t.Fatalf("write control file: %v", err)
	}

	change, err := CompareHandoffEvidenceBaseline(context.Background(), dir, baseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
	}
	if change.CommitMoved || change.WorktreeChanged {
		t.Errorf("change = %+v, want .sortie control files excluded", change)
	}
}

func TestHandoffEvidenceNonGitWorkspace(t *testing.T) {
	dir := t.TempDir()
	_, err := CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if !errors.Is(err, ErrNotGitWorkspace) {
		t.Fatalf("CaptureHandoffEvidenceBaseline error = %v, want ErrNotGitWorkspace", err)
	}
}

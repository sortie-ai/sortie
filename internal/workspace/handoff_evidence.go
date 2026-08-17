package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotGitWorkspace identifies a workspace that cannot provide a Git
// baseline. Callers treat this as an undeterminable evidence verdict rather
// than as a worker failure.
var ErrNotGitWorkspace = errors.New("workspace is not a Git work tree")

// HandoffEvidenceBaseline captures the durable Git state immediately before
// the agent session starts. The working-tree fingerprint excludes Sortie's
// .sortie control directory; pushed commit and pull-request metadata from that
// directory is evaluated separately as a positive signal.
type HandoffEvidenceBaseline struct {
	Commit              string
	WorktreeFingerprint [sha256.Size]byte
}

// HandoffEvidenceChange describes the workspace differences that can
// independently establish work observed for a run.
type HandoffEvidenceChange struct {
	CommitMoved     bool
	WorktreeChanged bool
}

// CaptureHandoffEvidenceBaseline inspects a Git workspace without modifying
// its index or working tree.
func CaptureHandoffEvidenceBaseline(ctx context.Context, workspacePath string) (HandoffEvidenceBaseline, error) {
	commit, fingerprint, err := inspectHandoffEvidenceState(ctx, workspacePath)
	if err != nil {
		return HandoffEvidenceBaseline{}, err
	}
	return HandoffEvidenceBaseline{
		Commit:              commit,
		WorktreeFingerprint: fingerprint,
	}, nil
}

// CompareHandoffEvidenceBaseline compares the current Git state with the
// run's frozen baseline.
func CompareHandoffEvidenceBaseline(ctx context.Context, workspacePath string, baseline HandoffEvidenceBaseline) (HandoffEvidenceChange, error) {
	commit, fingerprint, err := inspectHandoffEvidenceState(ctx, workspacePath)
	if err != nil {
		return HandoffEvidenceChange{}, err
	}
	return HandoffEvidenceChange{
		CommitMoved:     commit != baseline.Commit,
		WorktreeChanged: fingerprint != baseline.WorktreeFingerprint,
	}, nil
}

func inspectHandoffEvidenceState(ctx context.Context, workspacePath string) (string, [sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if workspacePath == "" {
		return "", zero, fmt.Errorf("workspace path is empty")
	}

	absPath, err := filepath.Abs(workspacePath)
	if err != nil {
		return "", zero, fmt.Errorf("resolve workspace path: %w", err)
	}
	absPath, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", zero, fmt.Errorf("resolve workspace symlinks: %w", err)
	}

	inside, err := runGit(ctx, absPath, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return "", zero, ErrNotGitWorkspace
	}

	topRaw, err := runGit(ctx, absPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", zero, fmt.Errorf("resolve Git work-tree root: %w", err)
	}
	top := filepath.Clean(strings.TrimSpace(string(topRaw)))
	relWorkspace, err := filepath.Rel(top, absPath)
	if err != nil || relWorkspace == ".." || strings.HasPrefix(relWorkspace, ".."+string(filepath.Separator)) {
		return "", zero, fmt.Errorf("workspace %q is outside Git work-tree root %q", absPath, top)
	}

	commitRaw, commitErr := runGit(ctx, absPath, "rev-parse", "--verify", "--quiet", "HEAD")
	commit := strings.TrimSpace(string(commitRaw))
	if commitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(commitErr, &exitErr) || exitErr.ExitCode() != 1 {
			return "", zero, fmt.Errorf("inspect Git HEAD: %w", commitErr)
		}
		commit = ""
	}

	pathspecs := handoffEvidencePathspecs(relWorkspace)
	hasher := sha256.New()
	for _, command := range [][]string{
		{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=no", "--"},
		{"diff", "--binary", "--no-ext-diff", "--no-textconv", "--submodule=diff", "--"},
		{"diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--submodule=diff", "--"},
	} {
		args := append(command, pathspecs...)
		output, cmdErr := runGit(ctx, absPath, args...)
		if cmdErr != nil {
			return "", zero, fmt.Errorf("inspect Git working tree: %w", cmdErr)
		}
		writeFingerprintPart(hasher, output)
	}

	untrackedArgs := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--full-name", "--"}, pathspecs...)
	untrackedRaw, err := runGit(ctx, absPath, untrackedArgs...)
	if err != nil {
		return "", zero, fmt.Errorf("list untracked files: %w", err)
	}
	for pathBytes := range bytes.SplitSeq(untrackedRaw, []byte{0}) {
		if len(pathBytes) == 0 {
			continue
		}
		relPath := string(pathBytes)
		writeFingerprintPart(hasher, pathBytes)
		if err := hashUntrackedFile(hasher, filepath.Join(top, filepath.FromSlash(relPath))); err != nil {
			return "", zero, fmt.Errorf("inspect untracked file %q: %w", relPath, err)
		}
	}

	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return commit, fingerprint, nil
}

func handoffEvidencePathspecs(relWorkspace string) []string {
	relWorkspace = filepath.ToSlash(filepath.Clean(relWorkspace))
	dotSortie := ".sortie"
	if relWorkspace != "." {
		dotSortie = relWorkspace + "/.sortie"
	}
	return []string{
		".",
		":(top,exclude)" + dotSortie,
		":(top,exclude)" + dotSortie + "/**",
	}
}

func writeFingerprintPart(dst io.Writer, value []byte) {
	_, _ = fmt.Fprintf(dst, "%d:", len(value))
	_, _ = dst.Write(value)
}

func hashUntrackedFile(dst io.Writer, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	writeFingerprintPart(dst, []byte(info.Mode().String()))

	switch {
	case info.Mode().IsRegular():
		file, err := os.Open(path) //nolint:gosec // path comes from git ls-files under the inspected work tree
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, _ = fmt.Fprintf(dst, "%d:", info.Size())
		if _, err := io.Copy(dst, file); err != nil {
			return err
		}
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		writeFingerprintPart(dst, []byte(target))
		return nil
	default:
		return fmt.Errorf("unsupported file mode %s", info.Mode())
	}
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // executable is the fixed git binary; only its argument vector varies
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return output, fmt.Errorf("git %s: %s: %w", args[0], message, err)
		}
		return output, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

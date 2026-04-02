// Package workspace manages per-issue workspace directories, path
// safety, and lifecycle hooks. Start with [ComputePath] for safe
// workspace path derivation from issue identifiers.
package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// unsafeChars matches any character not in the allowed set for
// workspace directory names.
var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// PathResult holds the computed workspace path and its sanitized key.
type PathResult struct {
	// Key is the sanitized directory name derived from the issue
	// identifier. Contains only [A-Za-z0-9._-] characters.
	Key string

	// Path is the absolute workspace path: <resolved_root>/<key>.
	Path string
}

// EnsureResult holds the outcome of ensuring a workspace directory
// exists for an issue. Create one via [Ensure]. The Key and Path
// fields mirror [PathResult]; CreatedNow indicates whether the
// directory was atomically created during the call.
type EnsureResult struct {
	// Key is the sanitized directory name derived from the issue
	// identifier. Contains only [A-Za-z0-9._-] characters.
	Key string

	// Path is the absolute workspace path: <resolved_root>/<key>.
	Path string

	// CreatedNow is true when the directory was created during this
	// call. The caller uses this to gate the after_create hook.
	CreatedNow bool
}

// SanitizeKey derives a safe directory name from an issue identifier
// by replacing every character not in [A-Za-z0-9._-] with underscore.
// Returns a [*PathError] if the input is empty or the sanitized result
// is "." or ".." (filesystem special names).
func SanitizeKey(identifier string) (string, error) {
	if identifier == "" {
		return "", &PathError{
			Op:         "sanitize",
			Identifier: identifier,
			Err:        errors.New("identifier must not be empty"),
		}
	}

	key := unsafeChars.ReplaceAllString(identifier, "_")

	if key == "." || key == ".." {
		return "", &PathError{
			Op:         "sanitize",
			Identifier: identifier,
			Err:        errors.New("sanitized key is a filesystem special name"),
		}
	}

	return key, nil
}

// ComputePath computes and validates the absolute workspace path for
// the given issue identifier under the specified workspace root. It
// sanitizes the identifier into a workspace key, resolves the root to
// an absolute path, joins root and key, and validates that the result
// is contained within the root directory. Returns a [PathResult] with
// the sanitized key and validated absolute path.
//
// Returns a [*PathError] on sanitization failure, root resolution
// failure, or containment violation.
func ComputePath(root, identifier string) (PathResult, error) {
	if root == "" {
		return PathResult{}, &PathError{
			Op:  "resolve",
			Err: errors.New("workspace root must not be empty"),
		}
	}

	key, err := SanitizeKey(identifier)
	if err != nil {
		return PathResult{}, err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return PathResult{}, &PathError{
			Op:   "resolve",
			Root: root,
			Err:  err,
		}
	}

	// Resolve symlinks on the root to get the real filesystem path.
	// This prevents a symlink at root from pointing outside the
	// intended directory tree.
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return PathResult{}, &PathError{
				Op:   "resolve",
				Root: root,
				Err:  err,
			}
		}
		// Root does not exist yet — fall back to the cleaned absolute
		// path so callers can create the workspace root on demand.
		resolvedRoot = filepath.Clean(absRoot)
	}

	workspacePath := filepath.Join(resolvedRoot, key)

	// Containment check: workspace_path must be a direct child of
	// resolved_root. filepath.Rel handles edge cases like root="/"
	// where a naive string prefix check would fail.
	rel, err := filepath.Rel(resolvedRoot, workspacePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == "." || strings.Contains(rel, string(filepath.Separator)) {
		return PathResult{}, &PathError{
			Op:         "containment",
			Root:       root,
			Identifier: identifier,
			Err:        errors.New("workspace path is not under root"),
		}
	}

	// If the workspace path already exists, reject symlinks to
	// prevent an attacker-planted symlink from escaping the root.
	fi, err := os.Lstat(workspacePath)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return PathResult{}, &PathError{
			Op:         "containment",
			Root:       root,
			Identifier: identifier,
			Err:        errors.New("workspace path is a symlink"),
		}
	}

	return PathResult{Key: key, Path: workspacePath}, nil
}

// Ensure ensures a workspace directory exists for the given issue
// identifier under the specified workspace root. It creates the
// directory if missing and reuses it if it already exists as a
// directory. If the path exists but is not a directory, Ensure
// returns a [*PathError] with Op "conflict" rather than silently
// replacing the entry.
//
// Directory creation uses atomic [os.Mkdir] so CreatedNow is
// reliable even when external processes share the filesystem.
//
// Returns an [EnsureResult] with CreatedNow=true when the directory
// was atomically created during this call, and CreatedNow=false
// when an existing directory was reused.
//
// Returns a [*PathError] on path computation failure, root creation
// failure, non-directory conflict, or filesystem errors.
func Ensure(root, identifier string) (EnsureResult, error) {
	pr, err := ComputePath(root, identifier)
	if err != nil {
		return EnsureResult{}, err
	}

	// Ensure the workspace root directory exists.
	parentDir := filepath.Dir(pr.Path)
	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return EnsureResult{}, &PathError{
			Op:         "create",
			Root:       root,
			Identifier: identifier,
			Err:        err,
		}
	}

	// Atomic creation of the workspace directory. Only the successful
	// Mkdir caller gets CreatedNow=true, eliminating TOCTOU races.
	mkdirErr := os.Mkdir(pr.Path, 0o750)
	if mkdirErr == nil {
		return EnsureResult{Key: pr.Key, Path: pr.Path, CreatedNow: true}, nil
	}
	if !errors.Is(mkdirErr, fs.ErrExist) {
		return EnsureResult{}, &PathError{
			Op:         "create",
			Root:       root,
			Identifier: identifier,
			Err:        mkdirErr,
		}
	}

	// Path already exists — verify it is a directory.
	fi, statErr := os.Lstat(pr.Path)
	if statErr != nil {
		return EnsureResult{}, &PathError{
			Op:         "stat",
			Root:       root,
			Identifier: identifier,
			Err:        statErr,
		}
	}

	if fi.IsDir() {
		return EnsureResult{Key: pr.Key, Path: pr.Path}, nil
	}

	// Non-directory entry at workspace path — hard error.
	return EnsureResult{}, &PathError{
		Op:         "conflict",
		Root:       root,
		Identifier: identifier,
		Err:        errors.New("path exists but is not a directory"),
	}
}

// ListWorkspaceKeys returns the names of direct child directories
// under root. Non-directory entries and symlinks are skipped. Returns
// an empty slice (not nil) if root does not exist or contains no
// directories.
//
// This function does not validate or reverse-map keys to identifiers;
// callers are responsible for matching keys against known issue
// identifiers.
func ListWorkspaceKeys(root string) ([]string, error) {
	if root == "" {
		return nil, &PathError{
			Op:  "resolve",
			Err: errors.New("workspace root must not be empty"),
		}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, &PathError{
			Op:   "resolve",
			Root: root,
			Err:  err,
		}
	}

	entries, err := os.ReadDir(absRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{}, nil
		}
		return nil, &PathError{
			Op:   "readdir",
			Root: absRoot,
			Err:  err,
		}
	}

	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		fi, statErr := os.Lstat(filepath.Join(absRoot, entry.Name()))
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return nil, statErr
		}
		if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
			keys = append(keys, entry.Name())
		}
	}
	return keys, nil
}

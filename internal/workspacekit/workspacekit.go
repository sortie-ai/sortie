// Package workspacekit verifies that a workspace directory and its .sortie
// subtree are real, unswapped directories at the moment a read, write, or
// removal touches them, and performs those operations through the handle it
// verified.
//
// VerifyDir checks a single directory. OpenSortieDir and OpenSubdir open a
// verified handle on .sortie and any directory beneath it. OpenSortieFile,
// ReadSortieFile, WriteSortieFile, and RemoveSortieFile compose those
// handles with a bounded read, an exclusive-create-then-rename write, and a
// refusal-safe removal, so a symbolic link swapped in after preparation
// fails the operation instead of redirecting it.
package workspacekit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SortieDir is the reserved directory name inside a workspace that holds
// files exchanged between the agent and the orchestrator.
const SortieDir = ".sortie"

var (
	// ErrLink reports that a path names a symbolic link where a real
	// directory or a plain file was required.
	ErrLink = errors.New("is a symbolic link")

	// ErrNotDirectory reports that a path does not name a directory.
	ErrNotDirectory = errors.New("is not a directory")

	// ErrNotPlainFile reports that a path names an entry whose type is
	// neither a symbolic link nor a plain file, such as a named pipe,
	// socket, or device.
	ErrNotPlainFile = errors.New("is not a plain file")

	// ErrChanged reports that the entry at a path no longer matches what
	// was verified there immediately before the entry was opened.
	ErrChanged = errors.New("changed while being opened")

	// ErrTooLarge reports that a file's content exceeds the caller's
	// configured size limit.
	ErrTooLarge = errors.New("exceeds the size limit")
)

// seam runs between a verification Lstat and the open that follows it,
// reproducing on demand a swap that production code never deliberately
// introduces. Its zero value is a no-op; only a test replaces it.
var seam = func() {}

// VerifyDir reports an error unless path names a real, unswapped directory:
// a symbolic link yields [ErrLink], a non-directory entry yields
// [ErrNotDirectory], and an absent path wraps [fs.ErrNotExist]. VerifyDir
// opens nothing; it inspects only the final path element.
func VerifyDir(path string) error {
	_, _, err := verifyDir(path)
	return err
}

func verifyDir(path string) (string, os.FileInfo, error) {
	if path == "" {
		return "", nil, errors.New("path is empty")
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return clean, nil, fmt.Errorf("%s: %w", clean, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return clean, nil, fmt.Errorf("%s: %w", clean, ErrLink)
	}
	if !info.IsDir() {
		return clean, nil, fmt.Errorf("%s: %w", clean, ErrNotDirectory)
	}
	return clean, info, nil
}

// openDir opens a verified handle on path: the directory Lstat reported
// there immediately before the open must match what the open handle itself
// reports, or the open fails with [ErrChanged].
func openDir(path string) (*os.Root, error) {
	clean, pre, err := verifyDir(path)
	if err != nil {
		return nil, err
	}

	seam()
	root, err := os.OpenRoot(clean)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clean, err)
	}
	post, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%s: %w", clean, err)
	}
	if !os.SameFile(pre, post) {
		_ = root.Close()
		return nil, fmt.Errorf("%s: %w", clean, ErrChanged)
	}
	return root, nil
}

// OpenSubdir opens a verified handle on the directory name inside parent.
// When create is true, OpenSubdir creates name with mode 0o750 if it is
// absent. The caller closes the returned root.
func OpenSubdir(parent *os.Root, name string, create bool) (*os.Root, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}

	if create {
		if err := parent.Mkdir(name, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	pre, err := parent.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if pre.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", name, ErrLink)
	}
	if !pre.IsDir() {
		return nil, fmt.Errorf("%s: %w", name, ErrNotDirectory)
	}

	seam()
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	post, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if !os.SameFile(pre, post) {
		_ = sub.Close()
		return nil, fmt.Errorf("%s: %w", name, ErrChanged)
	}
	return sub, nil
}

// OpenSortieDir opens a verified handle on <workspacePath>/[SortieDir].
// When create is true, OpenSortieDir creates the directory if it is absent.
// The caller closes the returned root.
func OpenSortieDir(workspacePath string, create bool) (*os.Root, error) {
	root, err := openDir(workspacePath)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // the workspace handle is not needed once .sortie is open

	return OpenSubdir(root, SortieDir, create)
}

// validateName reports an error unless name is a single, non-empty path
// element that is neither "." nor "..".
func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name %q is invalid", name)
	}
	return nil
}

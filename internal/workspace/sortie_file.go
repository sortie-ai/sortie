package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// sortieDir is the workspace-relative directory every WriteSortieFile
// write targets.
const sortieDir = ".sortie"

// WriteSortieFile replaces <workspacePath>/.sortie/<name> with data. It
// is the single writer for every file the orchestrator places in a
// workspace's .sortie directory.
//
// Every filesystem step resolves through an os.Root opened at
// workspacePath, so no step reaches outside the workspace directory
// whatever symbolic links exist there or appear during the call.
// WriteSortieFile returns an error, touching nothing, when name is
// empty, ".", "..", or contains a path separator. It returns an error,
// creating nothing, when .sortie is absent, a symbolic link, or not a
// directory; it never creates .sortie itself. It writes through a
// fresh, exclusively created temporary file inside .sortie and renames
// it onto name, so it never opens a pre-existing path for writing: a
// symbolic link already at name is replaced, not followed. It removes
// the temporary file when a step before the rename fails. It does not
// log.
func WriteSortieFile(workspacePath, name string, data []byte) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("sortie file name %q is invalid", name)
	}

	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close() //nolint:errcheck // best-effort cleanup in defer

	fi, err := root.Lstat(sortieDir)
	if err != nil {
		return fmt.Errorf("stat %s directory: %w", sortieDir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link, refusing to write", sortieDir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", sortieDir)
	}

	tmpName, err := createSortieTempFile(root, data)
	if err != nil {
		return err
	}

	if err := root.Rename(sortieDir+"/"+tmpName, sortieDir+"/"+name); err != nil {
		_ = root.Remove(sortieDir + "/" + tmpName)
		return fmt.Errorf("rename %q into place: %w", name, err)
	}
	return nil
}

// createSortieTempFile creates a temporary file inside .sortie under a
// fresh random name, the naming scheme [os.CreateTemp] uses, mode 0600,
// writes data, and closes it. It returns the temporary file's name
// relative to .sortie. The caller removes the temp file on any later
// failure.
func createSortieTempFile(root *os.Root, data []byte) (string, error) {
	for range 10000 {
		suffix, err := randomHex(16)
		if err != nil {
			return "", fmt.Errorf("generate temp file name: %w", err)
		}
		tmpName := "tmp-" + suffix
		f, err := root.OpenFile(sortieDir+"/"+tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create temp file: %w", err)
		}

		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			_ = root.Remove(sortieDir + "/" + tmpName)
			if writeErr != nil {
				return "", fmt.Errorf("write temp file: %w", writeErr)
			}
			return "", fmt.Errorf("close temp file: %w", closeErr)
		}
		return tmpName, nil
	}
	return "", errors.New("create temp file: exhausted random name attempts")
}

// randomHex returns a random hex-encoded string of n random bytes, read
// from crypto/rand so a temp file's name cannot be guessed ahead of its
// creation.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

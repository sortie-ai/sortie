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

const sortieDir = ".sortie"

// WriteSortieFile replaces <workspacePath>/.sortie/<name> with data.
//
// Operations are rooted at workspacePath to prevent symlinks from escaping the
// workspace. A fresh temporary file avoids writing through a pre-existing link.
func WriteSortieFile(workspacePath, name string, data []byte) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("sortie file name %q is invalid", name)
	}

	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close() //nolint:errcheck // There is no recovery path during cleanup.

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

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

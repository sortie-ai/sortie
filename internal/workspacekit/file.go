package workspacekit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// notPlainModes are the Lstat mode bits that disqualify an entry from being
// a plain file: a symbolic link, a directory, or one of the four
// device-like entry types. ModeIrregular is not included, because Go 1.26
// reports a Windows file whose reparse tag is neither a symlink nor a
// cloud-sync placeholder as irregular.
const notPlainModes = os.ModeSymlink | os.ModeDir | os.ModeNamedPipe | os.ModeSocket | os.ModeDevice | os.ModeCharDevice

func isPlainMode(mode os.FileMode) bool {
	return mode&notPlainModes == 0
}

func openPlain(dir *os.Root, name string) (*os.File, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}

	pre, err := dir.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if pre.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", name, ErrLink)
	}
	if !isPlainMode(pre.Mode()) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotPlainFile)
	}

	seam()
	f, err := dir.OpenFile(name, os.O_RDONLY|readOpenFlag, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	post, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if !isPlainMode(post.Mode()) || !os.SameFile(pre, post) {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", name, ErrChanged)
	}
	return f, nil
}

// OpenSortieFile opens the plain file name inside
// <workspacePath>/[SortieDir] for reading. The caller closes the returned
// file.
func OpenSortieFile(workspacePath, name string) (*os.File, error) {
	dir, err := OpenSortieDir(workspacePath, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close() //nolint:errcheck // the directory handle is not needed once the file is open

	return openPlain(dir, name)
}

// ReadSortieFile reads at most maxBytes from the plain file name inside
// <workspacePath>/[SortieDir]. maxBytes MUST be positive. Content beyond
// maxBytes yields [ErrTooLarge].
func ReadSortieFile(workspacePath, name string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("maxBytes must be positive")
	}

	f, err := OpenSortieFile(workspacePath, name)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable after data is read

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s: %w", name, ErrTooLarge)
	}
	return data, nil
}

// ReplaceFile replaces name inside dir with data through an exclusive-create
// temporary file and a rename, so a reader never observes a partially
// written file. The temporary file is removed if the write, close, or
// rename fails.
func ReplaceFile(dir *os.Root, name string, data []byte) error {
	if err := validateName(name); err != nil {
		return err
	}

	tmpName, err := createTempFile(dir, data)
	if err != nil {
		return err
	}
	if err := dir.Rename(tmpName, name); err != nil {
		_ = dir.Remove(tmpName)
		return fmt.Errorf("rename %q into place: %w", name, err)
	}
	return nil
}

func createTempFile(dir *os.Root, data []byte) (string, error) {
	for range 10000 {
		suffix, err := randomHex(16)
		if err != nil {
			return "", fmt.Errorf("generate temp file name: %w", err)
		}
		tmpName := "tmp-" + suffix
		f, err := dir.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create temp file: %w", err)
		}

		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			_ = dir.Remove(tmpName)
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

// WriteSortieFile replaces <workspacePath>/[SortieDir]/name with data. An
// absent .sortie directory is an error; WriteSortieFile does not create it.
func WriteSortieFile(workspacePath, name string, data []byte) error {
	dir, err := OpenSortieDir(workspacePath, false)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // the directory handle is not needed once the write completes

	return ReplaceFile(dir, name, data)
}

// RemoveFile removes the plain file name inside dir. A symbolic link yields
// [ErrLink] and any other non-plain entry yields [ErrNotPlainFile]; neither
// case removes anything. An absent name wraps [fs.ErrNotExist].
func RemoveFile(dir *os.Root, name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	info, err := dir.Lstat(name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: %w", name, ErrLink)
	}
	if !isPlainMode(info.Mode()) {
		return fmt.Errorf("%s: %w", name, ErrNotPlainFile)
	}
	return dir.Remove(name)
}

// RemoveSortieFile removes the plain file name inside
// <workspacePath>/[SortieDir], applying the same refusal rules as
// [RemoveFile].
func RemoveSortieFile(workspacePath, name string) error {
	dir, err := OpenSortieDir(workspacePath, false)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // the directory handle is not needed once the removal completes

	return RemoveFile(dir, name)
}

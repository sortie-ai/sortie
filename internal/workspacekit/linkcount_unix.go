//go:build unix

package workspacekit

import (
	"errors"
	"os"
	"syscall"
)

// linkCount reports the number of directory entries that name the file f
// has open. Nlink is narrower than uint64 on some targets, so it is
// widened.
func linkCount(f *os.File) (uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("file status carries no link count")
	}
	return uint64(stat.Nlink), nil //nolint:unconvert // Nlink is uint16 on darwin and uint32 on some 32-bit targets.
}

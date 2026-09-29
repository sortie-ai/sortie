//go:build windows

package workspacekit

import (
	"os"
	"syscall"
)

// linkCount reports the number of directory entries that name the file f
// has open.
func linkCount(f *os.File) (uint64, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}

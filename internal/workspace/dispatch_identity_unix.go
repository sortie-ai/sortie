//go:build unix

package workspace

import "syscall"

// Prevent a FIFO planted at the record path from blocking the reader.
const nonBlockingReadFlag = syscall.O_NONBLOCK

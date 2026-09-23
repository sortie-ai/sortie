//go:build unix

package workspacekit

import "syscall"

// readOpenFlag prevents a FIFO planted at a read path from blocking the open.
const readOpenFlag = syscall.O_NONBLOCK

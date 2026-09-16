//go:build unix

package workspace

import "syscall"

// nonBlockingReadFlag opens the dispatch identity record without
// waiting for a FIFO writer, so a FIFO planted at the record path
// cannot block the reader.
const nonBlockingReadFlag = syscall.O_NONBLOCK

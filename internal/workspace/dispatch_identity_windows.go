//go:build windows

package workspace

// nonBlockingReadFlag is 0 on Windows, which has no FIFO file a
// blocking open could stall on.
const nonBlockingReadFlag = 0

//go:build windows

package workspacekit

// readOpenFlag is a no-op on Windows: there is no non-blocking open flag to
// apply, and FIFOs do not exist on this platform.
const readOpenFlag = 0

//go:build !unix && !windows

package procutil

// WasSignaled always returns false on unsupported platforms.
func WasSignaled(_ error) bool {
	return false
}

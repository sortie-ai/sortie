package agentcore

import (
	"crypto/rand"
	"fmt"
)

// NewUUIDv4 returns a random RFC 4122 version-4 UUID string, or an error
// if crypto/rand is unavailable.
func NewUUIDv4() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("agentcore: crypto/rand unavailable: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

package scmcore

import (
	"fmt"
	"strings"
)

// IsBotAuthor reports whether a review author is an automated bot.
// platformBot is the platform's own bot marker, false on a platform that
// has none; allowlist entries match login case-insensitively. The two
// conditions form a union: either qualifies.
func IsBotAuthor(login string, platformBot bool, allowlist []string) bool {
	if platformBot {
		return true
	}
	for _, name := range allowlist {
		if strings.EqualFold(login, name) {
			return true
		}
	}
	return false
}

// SortableEventID formats a numeric journal-event id as a fixed-width,
// zero-padded decimal so lexical string comparison matches numeric order.
// The width covers the maximum int64.
func SortableEventID(id int64) string {
	return fmt.Sprintf("%019d", id)
}

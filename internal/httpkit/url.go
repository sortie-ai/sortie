package httpkit

import "strings"

// RedactURLUserinfo removes the complete userinfo prefix from raw while
// preserving the rest of the URL for diagnostics. It operates on the raw
// string instead of parsing it so malformed URLs are redacted too. When the
// authority marker is missing or malformed, the leading segment is treated as
// the authority so credentials in invalid endpoint values are still removed.
func RedactURLUserinfo(raw string) string {
	authorityStart := 0
	if marker := strings.Index(raw, "://"); marker >= 0 {
		authorityStart = marker + len("://")
	} else if marker := strings.Index(raw, "//"); marker >= 0 {
		authorityStart = marker + len("//")
	} else if marker := strings.Index(raw, ":/"); marker >= 0 {
		authorityStart = marker + len(":/")
	}

	authorityEnd := len(raw)
	if offset := strings.IndexAny(raw[authorityStart:], "/?#"); offset >= 0 {
		authorityEnd = authorityStart + offset
	}

	userinfoEnd := strings.LastIndex(raw[authorityStart:authorityEnd], "@")
	if userinfoEnd < 0 {
		return raw
	}
	userinfoEnd += authorityStart + len("@")

	return raw[:authorityStart] + raw[userinfoEnd:]
}

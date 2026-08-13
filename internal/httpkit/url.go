package httpkit

import "strings"

// RedactURLUserinfo removes the complete userinfo prefix from raw while
// preserving the rest of the URL for diagnostics. It operates on the raw
// string instead of parsing it so malformed URLs are redacted too. Values
// without an authority marker or userinfo delimiter are returned unchanged.
func RedactURLUserinfo(raw string) string {
	authorityStart := -1
	if strings.HasPrefix(raw, "//") {
		authorityStart = 2
	} else if marker := strings.Index(raw, "://"); marker >= 0 {
		authorityStart = marker + len("://")
	}
	if authorityStart < 0 {
		return raw
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

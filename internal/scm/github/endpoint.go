package github

import (
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/httpkit"
)

// defaultEndpoint is the public GitHub REST API base URL, applied when
// the adapter config omits "endpoint".
const defaultEndpoint = "https://api.github.com"

// resolveEndpoint turns a configured endpoint into the base URL a GitHub
// HTTP client is built from, and reports whether the configured value is
// usable at all.
//
// An empty or whitespace-only value resolves to [defaultEndpoint]. A
// present value must parse as an absolute http(s) URL carrying a host;
// anything else yields ok false, which every constructor turns into a
// configuration error rather than letting the value reach the transport.
// The [url.Parse] error is discarded rather than wrapped: its text quotes
// the whole raw URL, so wrapping it would republish any userinfo
// credentials the value carried. redacted is the configured value with
// its userinfo removed, and is the only form safe to place in an
// operator-facing message. It is empty when the value was empty.
func resolveEndpoint(raw string) (endpoint, redacted string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultEndpoint, "", true
	}

	redacted = httpkit.RedactURLUserinfo(trimmed)

	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", redacted, false
	}

	return strings.TrimRight(trimmed, "/"), redacted, true
}

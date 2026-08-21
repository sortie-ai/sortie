package httpkit

import (
	"net/url"
	"strings"
)

// Endpoint carries the facts a caller needs about a configured API base
// URL.
type Endpoint struct {
	// Base is the configured value, trimmed of surrounding whitespace and
	// trailing slashes.
	Base string
	// Host is the lowercased hostname, for host-class checks (for
	// example, distinguishing a Cloud host from a self-hosted one).
	Host string
	// Redacted is the configured value with any userinfo removed, safe
	// to place in an operator-facing message.
	Redacted string
	// Scheme is the parsed URL scheme, "http" or "https" (url.Parse
	// already lowercases it).
	Scheme string
	// Path is the parsed URL path, for checks that look at a configured
	// subpath (for example, detecting an already-appended API suffix).
	Path string
}

// ParseEndpoint parses raw as a configured API base URL and reports
// whether the value is usable.
//
// raw is trimmed of surrounding whitespace first. An empty result yields
// the zero Endpoint and false: this helper owns no default-value or
// required-field policy, so a caller whose adapter defaults an empty
// endpoint, or one that treats it as an error, applies that policy
// itself against the false result.
//
// A non-empty value is usable only when it parses with [url.Parse], the
// scheme is exactly "http" or "https" (url.Parse already lowercases the
// scheme), and the host is non-empty. On failure, ParseEndpoint still
// returns false with Redacted populated so the caller can build a safe
// diagnostic; the [url.Parse] error itself is discarded because its text
// quotes the whole raw URL, and republishing it would leak any userinfo
// credential the endpoint carried.
//
// ParseEndpoint performs no network I/O and does no logging.
func ParseEndpoint(raw string) (Endpoint, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Endpoint{}, false
	}

	redacted := RedactURLUserinfo(trimmed)

	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Endpoint{Redacted: redacted}, false
	}

	return Endpoint{
		Base:     strings.TrimRight(trimmed, "/"),
		Host:     strings.ToLower(parsed.Hostname()),
		Redacted: redacted,
		Scheme:   parsed.Scheme,
		Path:     parsed.Path,
	}, true
}

// ResolveEndpoint applies a caller's default-then-parse policy for a
// configured API base URL and reports whether the result is usable.
//
// raw is trimmed first. When the trimmed value is empty, defaultBase is
// parsed instead and that result is returned; otherwise the trimmed
// value is parsed. Both paths go through [ParseEndpoint], so every field
// of the returned [Endpoint] is populated consistently and there is no
// half-filled value for the default case.
//
// defaultBase is expected to be a caller-supplied compile-time constant.
// A false result for an empty raw therefore indicates a bad default,
// never bad operator input.
func ResolveEndpoint(raw, defaultBase string) (Endpoint, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ParseEndpoint(defaultBase)
	}
	return ParseEndpoint(trimmed)
}

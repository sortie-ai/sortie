package linear

import (
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// resolveEndpoint turns a configured endpoint into the base URL the Linear
// GraphQL client is built from, and reports whether the configured value is
// usable at all.
//
// An empty or whitespace-only value resolves to [defaultEndpoint]. A present
// value is parsed by [httpkit.ParseEndpoint], which requires an absolute
// http(s) URL carrying a host; anything else yields ok false, which the
// constructor turns into a configuration error rather than letting the value
// reach the transport. redacted is the configured value with its userinfo
// removed, and is the only form safe to place in an operator-facing message.
// It is empty when the value was empty.
func resolveEndpoint(raw string) (endpoint, redacted string, ok bool) {
	parsed, ok := httpkit.ResolveEndpoint(raw, defaultEndpoint)
	if !ok {
		return "", parsed.Redacted, false
	}

	return parsed.Base, parsed.Redacted, true
}

// Package redact masks secret values Sortie has learned about in text it
// logs, stores, or shows an operator. Every credential a launch, a log
// record, or a stored row can carry is registered here once known,
// add-only for the life of the process, and [Mask] replaces every
// registered value wherever it later occurs.
package redact

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/httpkit"
)

const (
	// Marker replaces every masked span.
	Marker = "[redacted]"

	// MinValueBytes is the shortest piece Mask ever matches. A shorter
	// value would mask too much ordinary text to be usable, so it is
	// left readable and its source is named in a warning instead.
	MinValueBytes = 8
)

// secretNameSuffixes are the name endings [IsSecretName] recognizes,
// matched against the last segment of a name split on "_" and "-".
var secretNameSuffixes = []string{
	"KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD",
	"PASS", "PAT", "CREDENTIAL", "CREDENTIALS", "AUTH",
	"AUTHORIZATION", "COOKIE", "COOKIE2",
}

const connectionStringSuffix = "_CONNECTION_STRING"

var (
	registerMu sync.Mutex
	registered atomic.Pointer[[]string]
	warned     = map[string]bool{}
)

// IsSecretName reports whether name follows a convention that marks its
// value as a credential: its last segment, split on "_" and "-", is a
// known suffix, or the whole name ends with "_CONNECTION_STRING".
func IsSecretName(name string) bool {
	upper := strings.ToUpper(name)
	normalized := strings.ReplaceAll(upper, "-", "_")
	if strings.HasSuffix(normalized, connectionStringSuffix) {
		return true
	}
	segments := strings.FieldsFunc(upper, func(r rune) bool { return r == '_' || r == '-' })
	if len(segments) == 0 {
		return false
	}
	return slices.Contains(secretNameSuffixes, segments[len(segments)-1])
}

// Add registers value under source: each line of a multi-line value
// (its trailing CR removed) and, for a line holding a space, the part
// after its first run of spaces, the credential of a "<scheme>
// <credential>" value such as "Bearer <token>".
func Add(source, value string) {
	addPieces(source, selectPieces(value), hasNonSpace(value))
}

// AddNamed registers value as a credential when name follows the
// [IsSecretName] convention, and as URL credentials otherwise.
func AddNamed(source, name, value string) {
	if IsSecretName(name) {
		Add(source, value)
		return
	}
	AddURLCredentials(source, value)
}

// AddURLCredentials registers the userinfo of value, a URL, and, when
// that userinfo holds a colon, the part after its first colon. A value
// holding no "://" contributes nothing.
func AddURLCredentials(source, value string) {
	if !strings.Contains(value, "://") {
		return
	}
	redacted := httpkit.RedactURLUserinfo(value)
	if redacted == value {
		return
	}
	userinfo := strings.TrimSuffix(removedSpan(value, redacted), "@")
	if userinfo == "" {
		return
	}
	pieces := []string{userinfo}
	if _, password, found := strings.Cut(userinfo, ":"); found {
		pieces = append(pieces, password)
	}
	addPieces(source, pieces, hasNonSpace(userinfo))
}

// AddEnviron registers each NAME=value entry of environ through
// [AddNamed], keyed by its own name.
func AddEnviron(environ []string) {
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		AddNamed(name, name, value)
	}
}

// selectPieces splits value into the candidate pieces Add registers:
// each line, its trailing CR removed, and, for a line holding a space,
// the part after its first run of spaces.
func selectPieces(value string) []string {
	lines := strings.Split(value, "\n")
	pieces := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		pieces = append(pieces, line)
		if idx := strings.IndexByte(line, ' '); idx >= 0 {
			rest := strings.TrimLeft(line[idx:], " ")
			if rest != "" {
				pieces = append(pieces, rest)
			}
		}
	}
	return pieces
}

// removedSpan returns the substring after removed from before to reach
// after, for the case where after equals before with one contiguous
// span deleted.
func removedSpan(before, after string) string {
	n, m := len(before), len(after)
	limit := min(n, m)
	prefix := 0
	for prefix < limit && before[prefix] == after[prefix] {
		prefix++
	}
	maxSuffix := limit - prefix
	suffix := 0
	for suffix < maxSuffix && before[n-1-suffix] == after[m-1-suffix] {
		suffix++
	}
	return before[prefix : n-suffix]
}

// hasNonSpace reports whether s holds at least one non-whitespace rune.
func hasNonSpace(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// addPieces registers every piece of pieces that meets the floor and is
// not already covered by Marker, publishing the union with the current
// registry. When every piece is dropped and credentialHasContent holds,
// it warns once per source for the life of the process.
func addPieces(source string, pieces []string, credentialHasContent bool) {
	var warn bool

	registerMu.Lock()
	current := currentValuesLocked()
	changed := false
	allDropped := true
	for _, p := range pieces {
		if len(p) < MinValueBytes || strings.Contains(Marker, p) {
			continue
		}
		allDropped = false
		if !slices.Contains(current, p) {
			current = append(current, p)
			changed = true
		}
	}
	if changed {
		registered.Store(&current)
	}
	if allDropped && credentialHasContent && !warned[source] {
		warned[source] = true
		warn = true
	}
	registerMu.Unlock()

	if warn {
		warnTooShort(source)
	}
}

// currentValuesLocked returns a fresh copy of the registry. Callers
// must hold registerMu.
func currentValuesLocked() []string {
	p := registered.Load()
	if p == nil {
		return nil
	}
	current := make([]string, len(*p))
	copy(current, *p)
	return current
}

// warnTooShort logs the one WARN a source whose every selected piece
// was dropped as too short earns, at most once per source.
func warnTooShort(source string) {
	slog.Default().Warn("secret value too short to mask",
		slog.String("name", source),
		slog.Int("min_bytes", MinValueBytes))
}

// Mask replaces every occurrence of every registered value in s with
// Marker, merging occurrences that overlap or touch into one span. It
// returns s unchanged when no registered value occurs in it.
func Mask(s string) string {
	values := loadRegistered()
	if len(values) == 0 {
		return s
	}
	spans := findSpans([]byte(s), values)
	if len(spans) == 0 {
		return s
	}
	return string(maskBytes([]byte(s), spans))
}

// Truncate returns [Mask](s) when it holds at most maxRunes runes;
// otherwise its first maxRunes runes followed by "…" (U+2026), applied
// after masking so a cut never splits a registered value.
func Truncate(s string, maxRunes int) string {
	masked := Mask(s)
	if utf8.RuneCountInString(masked) <= maxRunes {
		return masked
	}
	runes := []rune(masked)
	return string(runes[:maxRunes]) + "…"
}

// loadRegistered returns the currently published registry without
// locking.
func loadRegistered() []string {
	p := registered.Load()
	if p == nil {
		return nil
	}
	return *p
}

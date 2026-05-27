package kiro

import (
	"regexp"
	"strings"
)

const (
	// creditsMarker is the stderr cost-trailer prefix that headless Kiro
	// emits only after a turn actually executed. Matched as a substring
	// because the numeric credit and time values vary; the prefix shape is
	// the stable contract.
	creditsMarker = "▸ Credits:" //nolint:gosec // G101: stderr cost-trailer prefix, not a credential

	// authFailedMarker is the stderr line headless Kiro emits when the
	// credential is present but invalid. Matched as a substring because the
	// trailing detail text after the marker is not contracted.
	authFailedMarker = "Authentication failed."
)

// ansiEscapeRE matches the ANSI color and style escape sequences that
// headless Kiro leaves in stdout (for example "\x1b[38;5;141m> \x1b[0m").
// Compiled once at package load. With --wrap never there is no width-based
// wrapping, so only color and style escapes remain to strip.
var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI color and style escape sequences from s and
// returns the cleaned text.
func stripANSI(s string) string {
	return ansiEscapeRE.ReplaceAllString(s, "")
}

// classifyStderr scans the collected stderr lines for the two headless
// Kiro signals. creditsSeen is true when any line contains the cost-trailer
// marker, the one positive proof that a turn ran. authFailed is true when
// any line contains the authentication-failure marker. Both are matched by
// substring containment, never by the numeric values that follow them.
func classifyStderr(stderrLines []string) (creditsSeen bool, authFailed bool) {
	for _, line := range stderrLines {
		if strings.Contains(line, creditsMarker) {
			creditsSeen = true
		}
		if strings.Contains(line, authFailedMarker) {
			authFailed = true
		}
	}
	return creditsSeen, authFailed
}

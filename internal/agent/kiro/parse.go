package kiro

import (
	"regexp"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// creditsMarker prefixes the stderr cost trailer headless Kiro prints only
// after a turn ran; the values after it vary, so it is matched as a substring.
const creditsMarker = "▸ Credits:" //nolint:gosec // G101: stderr cost-trailer prefix, not a credential

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

// classifyStderr reports whether stderr proves a turn ran.
//
// A transcript the collector marked incomplete cannot prove a turn ran:
// the trailer is the last thing headless Kiro writes, so one collected
// before the drain was cut short may belong to output whose remainder
// never arrived.
func classifyStderr(stderrLines []string) (creditsSeen bool) {
	var incomplete bool
	for _, line := range stderrLines {
		if strings.Contains(line, procutil.AbandonedMarker) {
			incomplete = true
		}
		if strings.Contains(line, creditsMarker) {
			creditsSeen = true
		}
	}
	return creditsSeen && !incomplete
}

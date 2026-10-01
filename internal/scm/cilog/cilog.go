// Package cilog holds the CI log handling every forge CI provider shares:
// bounded line-by-line reading of a log stream, per-line sanitization, and
// assembly of the excerpt a failed check hands to the agent.
//
// A provider finds the failing step in its own forge's vocabulary, feeds the
// log's lines to a [Builder], and reads the excerpt back. Providers whose
// failure text is not a log use [TailLines] directly.
package cilog

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	// readCeiling is four times GitLab Runner's default output limit and
	// covers nearly every GitHub Actions job log.
	readCeiling int64 = 16 << 20

	// lineCap bounds the memory one retained line can hold now that the read
	// is no longer capped at the first mebibyte.
	lineCap = 4096

	lineCutSuffix = " [sortie: line cut]"

	readChunk = 32 << 10
)

// Scan reads body line by line until its end or until the read ceiling,
// calling yield with each line without its line feed.
//
// Only a line feed ends a line; a carriage return stays part of the line it
// is in. complete is true exactly when the end of body was observed within
// the ceiling. err is the first read error other than [io.EOF], and complete
// is then false. Every line finished before a stop is yielded, and bytes
// after the last line feed, at a stop or at the end, are yielded as a final
// line. A line over 4096 bytes is yielded as its first 4096 bytes, shortened
// to the last complete UTF-8 sequence, followed by a cut marker; the rest of
// that line is read and discarded. yield runs on the calling goroutine only.
func Scan(body io.Reader, yield func(line string)) (complete bool, err error) {
	// One byte beyond the ceiling tells a body that ends exactly at the
	// ceiling from one that continues past it.
	src := io.LimitReader(body, readCeiling+1)
	buf := make([]byte, readChunk)
	lines := lineAssembler{yield: yield}

	var consumed int64
	for {
		n, readErr := src.Read(buf)
		chunk := buf[:n]

		if room := readCeiling - consumed; int64(len(chunk)) > room {
			lines.write(chunk[:room])
			lines.flushPartial()
			return false, nil
		}
		consumed += int64(len(chunk))
		lines.write(chunk)

		switch {
		case readErr == io.EOF:
			lines.flushPartial()
			return true, nil
		case readErr != nil:
			lines.flushPartial()
			return false, readErr
		}
	}
}

// lineAssembler splits a byte stream into lines, keeping at most lineCap
// bytes of the line being assembled.
type lineAssembler struct {
	yield func(line string)
	cur   []byte
	over  bool
}

func (a *lineAssembler) write(chunk []byte) {
	for len(chunk) > 0 {
		end := bytes.IndexByte(chunk, '\n')
		if end < 0 {
			a.append(chunk)
			return
		}
		a.append(chunk[:end])
		a.flush()
		chunk = chunk[end+1:]
	}
}

func (a *lineAssembler) append(segment []byte) {
	if a.over {
		return
	}
	room := lineCap - len(a.cur)
	if len(segment) > room {
		a.cur = append(a.cur, segment[:room]...)
		a.over = true
		return
	}
	a.cur = append(a.cur, segment...)
}

func (a *lineAssembler) flush() {
	line := string(a.cur)
	if a.over {
		line = trimPartialRune(line) + lineCutSuffix
	}
	a.cur = a.cur[:0]
	a.over = false
	a.yield(line)
}

func (a *lineAssembler) flushPartial() {
	if len(a.cur) > 0 {
		a.flush()
	}
}

// TailLines returns the last maxLines non-empty sanitized lines of text,
// joined by line feeds, with no Sortie note; "" when maxLines is not
// positive. Each line is cut at the same length [Scan] cuts at.
func TailLines(text string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}

	var kept []string
	for raw := range strings.SplitSeq(text, "\n") {
		line := sanitize(cutLine(raw))
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}

	if len(kept) > maxLines {
		kept = kept[len(kept)-maxLines:]
	}
	return strings.Join(kept, "\n")
}

func cutLine(line string) string {
	if len(line) <= lineCap {
		return line
	}
	return trimPartialRune(line[:lineCap]) + lineCutSuffix
}

// trimPartialRune drops a UTF-8 sequence that s ends in the middle of.
func trimPartialRune(s string) string {
	for i := len(s) - 1; i >= 0 && i >= len(s)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(s[i]) {
			continue
		}
		if !utf8.FullRuneInString(s[i:]) {
			return s[:i]
		}
		return s
	}
	return s
}

// sanitize removes escape sequences and control characters from one line and
// trims its trailing spaces and tabs. Leading whitespace is kept because it
// carries the indentation of the logged output.
func sanitize(line string) string {
	if !needsSanitizing(line) {
		return line
	}

	var out strings.Builder
	out.Grow(len(line))
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == 0x1b:
			i += escapeLen(line[i:])
		case isDeletedControl(c):
			i++
		default:
			out.WriteByte(c)
			i++
		}
	}
	return strings.TrimRight(out.String(), " \t")
}

func needsSanitizing(line string) bool {
	if n := len(line); n > 0 && (line[n-1] == ' ' || line[n-1] == '\t') {
		return true
	}
	for i := 0; i < len(line); i++ {
		if isDeletedControl(line[i]) || line[i] == 0x1b {
			return true
		}
	}
	return false
}

func isDeletedControl(c byte) bool {
	return c <= 0x08 || (c >= 0x0a && c <= 0x1f) || c == 0x7f
}

// escapeLen returns the byte length of the escape sequence that opens s,
// which starts with ESC. A CSI or OSC sequence is consumed whole; any other
// ESC is consumed together with the one character after it.
func escapeLen(s string) int {
	if len(s) >= 2 {
		switch s[1] {
		case '[':
			if n := csiLen(s); n > 0 {
				return n
			}
		case ']':
			return oscLen(s)
		}
	}
	if len(s) == 1 {
		return 1
	}
	_, size := utf8.DecodeRuneInString(s[1:])
	return 1 + size
}

// csiLen returns the length of the CSI sequence that opens s: ESC [, bytes
// 0x30 to 0x3F, bytes 0x20 to 0x2F, then one byte 0x40 to 0x7E. It returns 0
// when s does not hold a complete one.
func csiLen(s string) int {
	i := 2
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		return i + 1
	}
	return 0
}

// oscLen returns the length of the OSC sequence that opens s: ESC ], through
// the first BEL or ESC backslash, or to the end of s when unterminated.
func oscLen(s string) int {
	for i := 2; i < len(s); i++ {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		}
	}
	return len(s)
}

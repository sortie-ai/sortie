package cilog_test

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/sortie-ai/sortie/internal/scm/cilog"
)

const (
	readCeiling = 16 << 20
	lineCap     = 4096
	cutSuffix   = " [sortie: line cut]"
)

var errBoom = errors.New("connection reset")

type cyclicReader struct {
	pattern string
	limit   int64
	served  int64
}

func (r *cyclicReader) Read(p []byte) (int, error) {
	if r.limit > 0 && r.served >= r.limit {
		return 0, io.EOF
	}
	n := len(p)
	if r.limit > 0 {
		n = int(min(int64(n), r.limit-r.served))
	}
	off := int(r.served % int64(len(r.pattern)))
	for filled := 0; filled < n; off = 0 {
		filled += copy(p[filled:n], r.pattern[off:])
	}
	r.served += int64(n)
	return n, nil
}

type dataThenErrReader struct {
	data string
	err  error
	done bool
}

func (r *dataThenErrReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

type scanOutcome struct {
	lines    []string
	complete bool
	err      error
}

func scan(body io.Reader) scanOutcome {
	var out scanOutcome
	out.complete, out.err = cilog.Scan(body, func(line string) { out.lines = append(out.lines, line) })
	return out
}

func TestScan_Lines(t *testing.T) {
	t.Parallel()

	rawLong := strings.Repeat("a", lineCap+1)
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"empty body yields nothing", "", nil},
		{"one terminated line", "alpha\n", []string{"alpha"}},
		{"trailing bytes without a line feed are a final line", "alpha\nbeta", []string{"alpha", "beta"}},
		{"blank lines are yielded", "\n\nx\n", []string{"", "", "x"}},
		{"carriage return stays in the line", "a\r\nb\r\n", []string{"a\r", "b\r"}},
		{"lone carriage return does not split", "a\rb\n", []string{"a\rb"}},
		{"line of exactly the cap is kept whole", strings.Repeat("a", lineCap) + "\nnext\n", []string{strings.Repeat("a", lineCap), "next"}},
		{"line one byte over the cap is cut", rawLong + "\nnext\n", []string{strings.Repeat("a", lineCap) + cutSuffix, "next"}},
		{"cut line far over the cap is discarded and the next line survives", strings.Repeat("b", 100_000) + "\nnext", []string{strings.Repeat("b", lineCap) + cutSuffix, "next"}},
		{"cut falls inside a two-byte rune", "x" + strings.Repeat("é", 3000) + "\nz", []string{"x" + strings.Repeat("é", 2047) + cutSuffix, "z"}},
		{"cut falls inside a four-byte rune", "xx" + strings.Repeat("😀", 2000) + "\nz", []string{"xx" + strings.Repeat("😀", 1023) + cutSuffix, "z"}},
		{"cut on a rune boundary keeps the whole rune", strings.Repeat("é", 2048) + "tail\nz", []string{strings.Repeat("é", 2048) + cutSuffix, "z"}},
		{"unterminated over-long final line is cut", rawLong, []string{strings.Repeat("a", lineCap) + cutSuffix}},
	}

	readers := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{"whole", func(r io.Reader) io.Reader { return r }},
		{"one byte at a time", iotest.OneByteReader},
		{"data with eof", iotest.DataErrReader},
	}

	for _, tt := range tests {
		for _, rd := range readers {
			t.Run(tt.name+"/"+rd.name, func(t *testing.T) {
				t.Parallel()

				got := scan(rd.wrap(strings.NewReader(tt.body)))

				if got.err != nil {
					t.Fatalf("Scan(%q) err = %v, want nil", tt.name, got.err)
				}
				if !got.complete {
					t.Errorf("Scan(%q) complete = false, want true", tt.name)
				}
				if !slices.Equal(got.lines, tt.want) {
					t.Errorf("Scan(%q) lines = %q, want %q", tt.name, got.lines, tt.want)
				}
			})
		}
	}
}

func TestScan_ReadError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body io.Reader
		want []string
	}{
		{
			name: "error after a partial line yields the lines read so far",
			body: io.MultiReader(strings.NewReader("a\nb\npart"), iotest.ErrReader(errBoom)),
			want: []string{"a", "b", "part"},
		},
		{
			name: "error right after a line feed leaves no extra line",
			body: io.MultiReader(strings.NewReader("a\nb\n"), iotest.ErrReader(errBoom)),
			want: []string{"a", "b"},
		},
		{
			name: "error returned together with the last bytes",
			body: &dataThenErrReader{data: "a\nb", err: errBoom},
			want: []string{"a", "b"},
		},
		{
			name: "error before any byte",
			body: iotest.ErrReader(errBoom),
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := scan(tt.body)

			if !errors.Is(got.err, errBoom) {
				t.Errorf("Scan err = %v, want %v", got.err, errBoom)
			}
			if got.complete {
				t.Error("Scan complete = true, want false after a read error")
			}
			if !slices.Equal(got.lines, tt.want) {
				t.Errorf("Scan lines = %q, want %q", got.lines, tt.want)
			}
		})
	}
}

func TestScan_ReadCeiling(t *testing.T) {
	t.Parallel()

	const lineLen = 1024
	fullLine := strings.Repeat("a", lineLen-1) + "\n"
	ceilingLines := readCeiling / lineLen

	t.Run("body of exactly the ceiling is complete", func(t *testing.T) {
		t.Parallel()

		body := &cyclicReader{pattern: fullLine, limit: readCeiling}

		got := scan(body)

		if got.err != nil {
			t.Fatalf("Scan err = %v, want nil", got.err)
		}
		if !got.complete {
			t.Error("Scan complete = false for a body of exactly the ceiling, want true")
		}
		if len(got.lines) != ceilingLines {
			t.Errorf("Scan yielded %d lines, want %d", len(got.lines), ceilingLines)
		}
	})

	t.Run("one byte past the ceiling is incomplete and not yielded", func(t *testing.T) {
		t.Parallel()

		body := &cyclicReader{pattern: fullLine, limit: readCeiling + 1}

		got := scan(body)

		if got.err != nil {
			t.Fatalf("Scan err = %v, want nil", got.err)
		}
		if got.complete {
			t.Error("Scan complete = true for a body one byte over the ceiling, want false")
		}
		if len(got.lines) != ceilingLines {
			t.Errorf("Scan yielded %d lines, want %d (the extra byte is outside the first 16 MiB)", len(got.lines), ceilingLines)
		}
		if body.served > readCeiling+1 {
			t.Errorf("Scan requested %d bytes, want at most %d", body.served, readCeiling+1)
		}
	})

	t.Run("endless body stops at the ceiling and yields the partial last line", func(t *testing.T) {
		t.Parallel()

		const pattern = "0123456789\n"
		body := &cyclicReader{pattern: pattern}

		got := scan(body)

		if got.err != nil {
			t.Fatalf("Scan err = %v, want nil", got.err)
		}
		if got.complete {
			t.Error("Scan complete = true for an endless body, want false")
		}
		if body.served > readCeiling+1 {
			t.Errorf("Scan requested %d bytes, want at most %d", body.served, readCeiling+1)
		}
		fullLines := readCeiling / len(pattern)
		partial := pattern[:readCeiling%len(pattern)]
		if len(got.lines) != fullLines+1 {
			t.Fatalf("Scan yielded %d lines, want %d", len(got.lines), fullLines+1)
		}
		if last := got.lines[len(got.lines)-1]; last != partial {
			t.Errorf("last yielded line = %q, want the partial %q", last, partial)
		}
		if first := got.lines[0]; first != "0123456789" {
			t.Errorf("first yielded line = %q, want %q", first, "0123456789")
		}
	})

	t.Run("endless line without a line feed is cut and incomplete", func(t *testing.T) {
		t.Parallel()

		body := &cyclicReader{pattern: "a"}

		got := scan(body)

		if got.complete {
			t.Error("Scan complete = true for an endless line, want false")
		}
		if body.served > readCeiling+1 {
			t.Errorf("Scan requested %d bytes, want at most %d", body.served, readCeiling+1)
		}
		want := []string{strings.Repeat("a", lineCap) + cutSuffix}
		if !slices.Equal(got.lines, want) {
			t.Errorf("Scan yielded %d lines, want one cut line of %d bytes", len(got.lines), len(want[0]))
		}
	})
}

func TestTailLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		maxLines int
		want     string
	}{
		{"keeps the last lines", "a\nb\nc\nd", 2, "c\nd"},
		{"fewer lines than the limit keeps all", "a\nb", 5, "a\nb"},
		{"exactly the limit keeps all", "a\nb\nc", 3, "a\nb\nc"},
		{"blank lines are dropped before counting", "a\n\n\nb\n  \n\t\nc\n\n", 2, "b\nc"},
		{"no trailing line feed", "a\nb\n", 5, "a\nb"},
		{"zero limit yields nothing", "a\nb", 0, ""},
		{"negative limit yields nothing", "a\nb", -3, ""},
		{"empty text", "", 5, ""},
		{"only blank lines", "\n \n\t\n", 5, ""},
		{"no sortie note is added", "[sortie] looks like a note\nx", 5, "[sortie] looks like a note\nx"},
		{"over-long line is cut", strings.Repeat("a", lineCap+10) + "\nz", 2, strings.Repeat("a", lineCap) + cutSuffix + "\nz"},
		{"cut falls inside a rune", "x" + strings.Repeat("é", 3000), 1, "x" + strings.Repeat("é", 2047) + cutSuffix},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := cilog.TailLines(tt.text, tt.maxLines)

			if got != tt.want {
				t.Errorf("TailLines(%q, %d) = %q, want %q", tt.text, tt.maxLines, got, tt.want)
			}
		})
	}
}

func TestTailLines_Sanitization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{"csi color", "\x1b[31mred\x1b[0m", "red"},
		{"csi with several parameters", "a\x1b[1;38;5;196mb", "ab"},
		{"csi private parameter", "a\x1b[?25lb", "ab"},
		{"csi erase line", "a\x1b[0Kb", "ab"},
		{"csi with an intermediate byte", "a\x1b[1 qb", "ab"},
		{"osc terminated by bel", "\x1b]0;window title\x07text", "text"},
		{"osc terminated by esc backslash", "\x1b]8;;https://example.invalid\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"osc without a terminator runs to the end of the line", "keep\x1b]0;title that never ends", "keep"},
		{"esc with the one character after it", "a\x1b=b\x1bcd", "abd"},
		{"esc before a multi-byte rune removes the whole rune", "a\x1bébc", "abc"},
		{"trailing esc", "a\x1b", "a"},
		{"incomplete csi loses esc and bracket only", "x\x1b[31", "x31"},
		{"control characters are deleted", "a\x00b\x01c\x07d\x08e\x7ff", "abcdef"},
		{"carriage return and vertical tab are deleted", "a\rb\vc\fd", "abcd"},
		{"tab is kept", "a\tb", "a\tb"},
		{"trailing spaces and tabs are trimmed", "text \t  \t", "text"},
		{"leading whitespace is kept", "    indented\t", "    indented"},
		{"trailing whitespace exposed by deleting a control character is trimmed", "text  \x1b[0m", "text"},
		{"only escape sequences leave nothing", "\x1b[0K\x1b[0m", ""},
		{"only whitespace leaves nothing", " \t ", ""},
		{"non-ascii text passes through", "héllo 😀", "héllo 😀"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := cilog.TailLines(tt.line, 10)

			if got != tt.want {
				t.Errorf("TailLines(%q, 10) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

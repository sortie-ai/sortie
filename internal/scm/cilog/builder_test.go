package cilog_test

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/scm/cilog"
)

const (
	tailNote    = "[sortie] The failing step could not be located, so these are the last lines of the job log."
	partialNote = "[sortie] The failing step could not be located and the job log was not read to its end, so these are the last lines that were read."
)

func locatedNote(label string) string {
	return `[sortie] Output of the failing step "` + label + `"; output from the rest of the job is left out.`
}

func concurrentNote(label string) string {
	return `[sortie] Output of the failing step "` + label + `", mixed with output from steps that ran at the same time; output from the rest of the job is left out.`
}

func omitted(count int) string {
	if count == 1 {
		return "[sortie] 1 line omitted."
	}
	return "[sortie] " + strconv.Itoa(count) + " lines omitted."
}

func numbered(prefix string, from, to int) []string {
	var lines []string
	for i := from; i <= to; i++ {
		lines = append(lines, prefix+strconv.Itoa(i))
	}
	return lines
}

func join(parts ...[]string) []string {
	return slices.Concat(parts...)
}

func record(b *cilog.Builder, kind cilog.LineKind, lines ...string) {
	for _, line := range lines {
		b.Line(line, kind)
	}
}

func excerptLines(excerpt string) []string {
	if excerpt == "" {
		return nil
	}
	return strings.Split(excerpt, "\n")
}

func TestBuilder_LocatedLayout(t *testing.T) {
	t.Parallel()

	const label = "build"
	tests := []struct {
		name   string
		n      int
		script func(b *cilog.Builder)
		want   []string
	}{
		{
			name: "step that fits the budget is shown whole",
			n:    10,
			script: func(b *cilog.Builder) {
				record(b, cilog.Plain, "before 1", "before 2")
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, "s1", "s2", "s3")
			},
			want: []string{"s1", "s2", "s3"},
		},
		{
			name: "step of exactly the budget is shown whole",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, numbered("s", 1, 10)...)
			},
			want: numbered("s", 1, 10),
		},
		{
			name: "step over the budget without a head keeps the last lines behind one omission",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, numbered("s", 1, 25)...)
			},
			want: join([]string{omitted(15)}, numbered("s", 16, 25)),
		},
		{
			name: "head at the start then a gap then the tail",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Head, "h1", "h2", "h3")
				record(b, cilog.Plain, numbered("p", 1, 20)...)
			},
			want: join([]string{"h1", "h2", omitted(13)}, numbered("p", 13, 20)),
		},
		{
			name: "lines before the head are omitted too",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, numbered("p", 1, 5)...)
				record(b, cilog.Head, "h1", "h2", "h3")
				record(b, cilog.Plain, numbered("q", 1, 20)...)
			},
			want: join([]string{omitted(5), "h1", "h2", omitted(13)}, numbered("q", 13, 20)),
		},
		{
			name: "gap of one line uses the singular form",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Head, "h1", "h2")
				record(b, cilog.Plain, numbered("p", 1, 9)...)
			},
			want: join([]string{"h1", "h2", omitted(1)}, numbered("p", 2, 9)),
		},
		{
			name: "head adjacent to the tail has no gap omission",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, "p1", "p2")
				record(b, cilog.Head, "h1", "h2")
				record(b, cilog.Plain, numbered("q", 1, 8)...)
			},
			want: join([]string{omitted(2), "h1", "h2"}, numbered("q", 1, 8)),
		},
		{
			name: "head overlapping the tail falls back to the plain tail layout",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, numbered("p", 1, 10)...)
				record(b, cilog.Head, "h1", "h2", "h3")
			},
			want: join([]string{omitted(3)}, numbered("p", 4, 10), []string{"h1", "h2", "h3"}),
		},
		{
			name: "head run is cut to a fifth of the budget from its start",
			n:    20,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Head, numbered("h", 1, 9)...)
				record(b, cilog.Plain, numbered("p", 1, 40)...)
			},
			want: join([]string{"h1", "h2", "h3", "h4", omitted(29)}, numbered("p", 25, 40)),
		},
		{
			name: "no head below a budget of five",
			n:    4,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Head, "h1", "h2", "h3")
				record(b, cilog.Plain, numbered("p", 1, 10)...)
			},
			want: join([]string{omitted(9)}, numbered("p", 7, 10)),
		},
		{
			name: "budget of five has a one line head",
			n:    5,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Head, "h1", "h2", "h3")
				record(b, cilog.Plain, numbered("p", 1, 20)...)
			},
			want: join([]string{"h1", omitted(18)}, numbered("p", 17, 20)),
		},
		{
			name: "last head run wins",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Head, "a1", "a2")
				record(b, cilog.Plain, "gap")
				record(b, cilog.Head, "b1", "b2")
				record(b, cilog.Plain, numbered("p", 1, 15)...)
			},
			want: join([]string{omitted(3), "b1", "b2", omitted(7)}, numbered("p", 8, 15)),
		},
		{
			name: "head from the end keeps the last lines of the run",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{HeadFromEnd: true})
				record(b, cilog.Head, "h1", "h2", "h3", "h4", "h5")
				record(b, cilog.Plain, numbered("p", 1, 20)...)
			},
			want: join([]string{omitted(3), "h4", "h5", omitted(12)}, numbered("p", 13, 20)),
		},
		{
			name: "head from the end with a run shorter than the cap keeps the whole run",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{HeadFromEnd: true})
				record(b, cilog.Head, "h1")
				record(b, cilog.Plain, numbered("p", 1, 20)...)
			},
			want: join([]string{"h1", omitted(11)}, numbered("p", 12, 20)),
		},
		{
			name: "blank lines neither count nor break a head run",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				b.Line("h1", cilog.Head)
				b.Line("", cilog.Plain)
				b.Line("  \x1b[0K", cilog.Plain)
				b.Line("h2", cilog.Head)
				record(b, cilog.Plain, numbered("p", 1, 20)...)
			},
			want: join([]string{"h1", "h2", omitted(12)}, numbered("p", 13, 20)),
		},
		{
			name: "end marked step ends at the last end line, not at the last line",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{EndMarked: true})
				record(b, cilog.Plain, numbered("a", 1, 15)...)
				b.Line("fail", cilog.End)
				record(b, cilog.Plain, "z1", "z2", "z3")
			},
			want: join([]string{omitted(6)}, numbered("a", 7, 15), []string{"fail"}),
		},
		{
			name: "end marked step uses the last of several end lines",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{EndMarked: true})
				record(b, cilog.Plain, "a1", "a2")
				b.Line("first fail", cilog.End)
				record(b, cilog.Plain, "b1", "b2")
				b.Line("second fail", cilog.End)
				record(b, cilog.Plain, "z1")
			},
			want: []string{"a1", "a2", "first fail", "b1", "b2", "second fail"},
		},
		{
			name: "end marked head and counts are those at the end line",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{EndMarked: true})
				record(b, cilog.Head, "h1", "h2", "h3")
				record(b, cilog.Plain, numbered("p", 1, 20)...)
				b.Line("fail", cilog.End)
				record(b, cilog.Head, "late head 1", "late head 2")
				record(b, cilog.Plain, numbered("z", 1, 500)...)
			},
			want: join([]string{"h1", "h2", omitted(14)}, numbered("p", 14, 20), []string{"fail"}),
		},
		{
			name: "end kind is ignored when the step is not end marked",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, "a1")
				b.Line("mid", cilog.End)
				record(b, cilog.Plain, "a2", "a3")
			},
			want: []string{"a1", "mid", "a2", "a3"},
		},
		{
			name: "lines recorded after the step closed are not part of it",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, "s1", "s2")
				b.CloseStep()
				record(b, cilog.Plain, "after 1", "after 2")
			},
			want: []string{"s1", "s2"},
		},
		{
			name: "opening a step again discards the first",
			n:    10,
			script: func(b *cilog.Builder) {
				b.OpenStep("first", cilog.StepRule{})
				record(b, cilog.Plain, "f1", "f2")
				b.OpenStep(label, cilog.StepRule{})
				record(b, cilog.Plain, "s1")
			},
			want: []string{"s1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := cilog.NewBuilder(tt.n)
			tt.script(b)

			got, fallback := b.Excerpt(true)

			if fallback != cilog.NoFallback {
				t.Fatalf("Excerpt(true) fallback = %q, want none", fallback)
			}
			want := join([]string{locatedNote(label)}, tt.want)
			if !slices.Equal(excerptLines(got), want) {
				t.Errorf("Excerpt(true) =\n%q\nwant\n%q", excerptLines(got), want)
			}
		})
	}
}

func TestBuilder_ConcurrentNote(t *testing.T) {
	t.Parallel()

	b := cilog.NewBuilder(10)
	record(b, cilog.Plain, "before")
	b.OpenStep("Typecheck", cilog.StepRule{EndMarked: true, Concurrent: true})
	record(b, cilog.Plain, "s1")
	b.Line("##[error]boom", cilog.End)

	got, fallback := b.Excerpt(true)

	if fallback != cilog.NoFallback {
		t.Fatalf("Excerpt(true) fallback = %q, want none", fallback)
	}
	want := []string{concurrentNote("Typecheck"), "s1", "##[error]boom"}
	if !slices.Equal(excerptLines(got), want) {
		t.Errorf("Excerpt(true) = %q, want %q", excerptLines(got), want)
	}
}

func TestBuilder_LocatedWhenOpenAndComplete(t *testing.T) {
	t.Parallel()

	b := cilog.NewBuilder(10)
	b.OpenStep("build", cilog.StepRule{})
	record(b, cilog.Plain, "s1", "s2")

	got, fallback := b.Excerpt(true)

	if fallback != cilog.NoFallback {
		t.Fatalf("Excerpt(true) fallback = %q, want none for a step still open at the end of the log", fallback)
	}
	want := []string{locatedNote("build"), "s1", "s2"}
	if !slices.Equal(excerptLines(got), want) {
		t.Errorf("Excerpt(true) = %q, want %q", excerptLines(got), want)
	}
}

func TestBuilder_LocatedWhenClosedAndIncomplete(t *testing.T) {
	t.Parallel()

	b := cilog.NewBuilder(10)
	record(b, cilog.Plain, "before")
	b.OpenStep("build", cilog.StepRule{})
	record(b, cilog.Plain, "s1", "s2")
	b.CloseStep()
	record(b, cilog.Plain, "after")

	got, fallback := b.Excerpt(false)

	if fallback != cilog.NoFallback {
		t.Fatalf("Excerpt(false) fallback = %q, want none for a step that closed before the read stopped", fallback)
	}
	want := []string{locatedNote("build"), "s1", "s2"}
	if !slices.Equal(excerptLines(got), want) {
		t.Errorf("Excerpt(false) = %q, want %q", excerptLines(got), want)
	}
}

func TestBuilder_Fallbacks(t *testing.T) {
	t.Parallel()

	jobLines := numbered("j", 1, 8)
	tests := []struct {
		name         string
		script       func(b *cilog.Builder)
		complete     bool
		wantFallback cilog.Fallback
	}{
		{
			name:         "step never opened",
			script:       func(b *cilog.Builder) { record(b, cilog.Plain, jobLines...) },
			complete:     true,
			wantFallback: cilog.StepNotFound,
		},
		{
			name:         "step never opened and the read stopped early",
			script:       func(b *cilog.Builder) { record(b, cilog.Plain, jobLines...) },
			complete:     false,
			wantFallback: cilog.ReadIncomplete,
		},
		{
			name: "close without an open step changes nothing",
			script: func(b *cilog.Builder) {
				b.CloseStep()
				record(b, cilog.Plain, jobLines...)
			},
			complete:     true,
			wantFallback: cilog.StepNotFound,
		},
		{
			name: "step dropped",
			script: func(b *cilog.Builder) {
				record(b, cilog.Plain, jobLines[:3]...)
				b.OpenStep("build", cilog.StepRule{})
				record(b, cilog.Plain, jobLines[3:]...)
				b.DropStep()
			},
			complete:     true,
			wantFallback: cilog.StepDropped,
		},
		{
			name: "step dropped after it closed",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{})
				record(b, cilog.Plain, jobLines...)
				b.CloseStep()
				b.DropStep()
			},
			complete:     true,
			wantFallback: cilog.StepDropped,
		},
		{
			name: "dropped wins over an incomplete read",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{})
				record(b, cilog.Plain, jobLines...)
				b.DropStep()
			},
			complete:     false,
			wantFallback: cilog.StepDropped,
		},
		{
			name: "step still open when the read stopped",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{})
				record(b, cilog.Plain, jobLines...)
			},
			complete:     false,
			wantFallback: cilog.ReadIncomplete,
		},
		{
			name: "incomplete read wins over a missing end marker",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{EndMarked: true})
				record(b, cilog.Plain, jobLines...)
			},
			complete:     false,
			wantFallback: cilog.ReadIncomplete,
		},
		{
			name: "end marked step with no end line",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{EndMarked: true})
				record(b, cilog.Plain, jobLines...)
				b.CloseStep()
			},
			complete:     true,
			wantFallback: cilog.NoEndMarker,
		},
		{
			name: "closed end marked step without an end line stays a missing marker on an incomplete read",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{EndMarked: true})
				record(b, cilog.Plain, jobLines...)
				b.CloseStep()
			},
			complete:     false,
			wantFallback: cilog.NoEndMarker,
		},
		{
			name: "end marked step with no lines at all",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{EndMarked: true})
				b.CloseStep()
				record(b, cilog.Plain, jobLines...)
			},
			complete:     true,
			wantFallback: cilog.NoEndMarker,
		},
		{
			name: "step closed before any line",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{})
				b.CloseStep()
				record(b, cilog.Plain, jobLines...)
			},
			complete:     true,
			wantFallback: cilog.EmptyStep,
		},
		{
			name: "step whose only lines are blank",
			script: func(b *cilog.Builder) {
				b.OpenStep("build", cilog.StepRule{})
				b.Line("", cilog.Plain)
				b.Line(" \x1b[0K", cilog.Plain)
				b.CloseStep()
				record(b, cilog.Plain, jobLines...)
			},
			complete:     true,
			wantFallback: cilog.EmptyStep,
		},
		{
			name: "step opened at the end of the log",
			script: func(b *cilog.Builder) {
				record(b, cilog.Plain, jobLines...)
				b.OpenStep("build", cilog.StepRule{})
			},
			complete:     true,
			wantFallback: cilog.EmptyStep,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := cilog.NewBuilder(5)
			tt.script(b)

			got, fallback := b.Excerpt(tt.complete)

			if fallback != tt.wantFallback {
				t.Errorf("Excerpt(%v) fallback = %q, want %q", tt.complete, fallback, tt.wantFallback)
			}
			note := tailNote
			if !tt.complete {
				note = partialNote
			}
			want := join([]string{note}, jobLines[3:])
			if !slices.Equal(excerptLines(got), want) {
				t.Errorf("Excerpt(%v) =\n%q\nwant\n%q", tt.complete, excerptLines(got), want)
			}
		})
	}
}

func TestBuilder_TailBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		n     int
		lines []string
		want  []string
	}{
		{"fewer job lines than the budget", 5, numbered("j", 1, 3), numbered("j", 1, 3)},
		{"exactly the budget", 5, numbered("j", 1, 5), numbered("j", 1, 5)},
		{"more job lines than the budget keeps the last ones with no omission line", 5, numbered("j", 1, 500), numbered("j", 496, 500)},
		{"budget of one", 1, numbered("j", 1, 9), []string{"j9"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := cilog.NewBuilder(tt.n)
			record(b, cilog.Plain, tt.lines...)

			got, fallback := b.Excerpt(true)

			if fallback != cilog.StepNotFound {
				t.Fatalf("Excerpt(true) fallback = %q, want %q", fallback, cilog.StepNotFound)
			}
			want := join([]string{tailNote}, tt.want)
			if !slices.Equal(excerptLines(got), want) {
				t.Errorf("Excerpt(true) =\n%q\nwant\n%q", excerptLines(got), want)
			}
		})
	}
}

func TestBuilder_TailBodyIncludesLinesOutsideTheStep(t *testing.T) {
	t.Parallel()

	b := cilog.NewBuilder(4)
	record(b, cilog.Plain, "pre1", "pre2")
	b.OpenStep("build", cilog.StepRule{EndMarked: true})
	record(b, cilog.Plain, "s1", "s2")
	b.CloseStep()
	record(b, cilog.Plain, "post1", "post2")

	got, fallback := b.Excerpt(true)

	if fallback != cilog.NoEndMarker {
		t.Fatalf("Excerpt(true) fallback = %q, want %q", fallback, cilog.NoEndMarker)
	}
	want := []string{tailNote, "s1", "s2", "post1", "post2"}
	if !slices.Equal(excerptLines(got), want) {
		t.Errorf("Excerpt(true) = %q, want %q", excerptLines(got), want)
	}
}

func TestBuilder_EmptyLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		script   func(b *cilog.Builder)
		complete bool
	}{
		{"nothing recorded", func(*cilog.Builder) {}, true},
		{"nothing recorded and incomplete", func(*cilog.Builder) {}, false},
		{"only blank lines", func(b *cilog.Builder) { record(b, cilog.Plain, "", "  ", "\x1b[0K", "\t") }, true},
		{"step opened but no line", func(b *cilog.Builder) { b.OpenStep("build", cilog.StepRule{}) }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := cilog.NewBuilder(5)
			tt.script(b)

			got, fallback := b.Excerpt(tt.complete)

			if got != "" || fallback != cilog.NoFallback {
				t.Errorf("Excerpt(%v) = %q, %q, want empty and no fallback", tt.complete, got, fallback)
			}
		})
	}
}

func TestBuilder_LineSanitization(t *testing.T) {
	t.Parallel()

	b := cilog.NewBuilder(10)
	b.OpenStep("build", cilog.StepRule{})
	record(b, cilog.Plain, "\x1b[31mred\x1b[0m  ", "  indented\tkeep\r", "a\x00b")

	got, _ := b.Excerpt(true)

	want := []string{locatedNote("build"), "red", "  indented\tkeep", "ab"}
	if !slices.Equal(excerptLines(got), want) {
		t.Errorf("Excerpt(true) = %q, want %q", excerptLines(got), want)
	}
}

func TestBuilder_Label(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		label string
		want  string
	}{
		{"plain label", "Run tests", "Run tests"},
		{"control characters become spaces", "a\x00b\nc\td\x7fe", "a b c d e"},
		{"surrounding control characters and spaces are trimmed", "\n  build \x00\t", "build"},
		{"label of exactly 200 runes is kept", strings.Repeat("é", 200), strings.Repeat("é", 200)},
		{"label over 200 runes is cut by rune", strings.Repeat("é", 201), strings.Repeat("é", 200) + "…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := cilog.NewBuilder(5)
			b.OpenStep(tt.label, cilog.StepRule{})
			b.Line("x", cilog.Plain)

			got, _ := b.Excerpt(true)

			want := []string{locatedNote(tt.want), "x"}
			if !slices.Equal(excerptLines(got), want) {
				t.Errorf("Excerpt(true) after OpenStep(%q) = %q, want %q", tt.label, excerptLines(got), want)
			}
		})
	}
}

func TestBuilder_LabelMasksRegisteredSecret(t *testing.T) {
	t.Parallel()

	secret := "tok_" + strconv.FormatUint(rand.Uint64(), 36) + strconv.FormatUint(rand.Uint64(), 36)
	redact.Add("cilog builder test", secret)
	b := cilog.NewBuilder(5)
	b.OpenStep("deploy "+secret, cilog.StepRule{})
	b.Line("x", cilog.Plain)

	got, _ := b.Excerpt(true)

	if strings.Contains(got, secret) {
		t.Errorf("Excerpt(true) = %q, want the registered secret masked in the label", got)
	}
	if !strings.Contains(got, `"deploy `) {
		t.Errorf("Excerpt(true) = %q, want the rest of the label kept", got)
	}
}

func TestBuilder_StepOpenAtReadCeiling(t *testing.T) {
	t.Parallel()

	const (
		n       = 50
		pattern = "0123456789\n"
	)
	tests := []struct {
		name string
		stop int
		note string
		want []string
	}{
		{
			name: "a step still open when the ceiling is reached gives the partial note",
			stop: 0,
			note: partialNote,
			want: append(slices.Repeat([]string{"0123456789"}, n-1), "01234"),
		},
		{
			name: "a step that closed before the ceiling stays located",
			stop: 10,
			note: locatedNote("build"),
			want: slices.Repeat([]string{"0123456789"}, 10),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := cilog.NewBuilder(n)
			b.OpenStep("build", cilog.StepRule{})
			lineNo := 0

			complete, err := cilog.Scan(&cyclicReader{pattern: pattern}, func(line string) {
				lineNo++
				b.Line(line, cilog.Plain)
				if lineNo == tt.stop {
					b.CloseStep()
				}
			})
			got, _ := b.Excerpt(complete)

			if err != nil || complete {
				t.Fatalf("Scan = %v, %v, want an incomplete read with no error", complete, err)
			}
			if want := join([]string{tt.note}, tt.want); !slices.Equal(excerptLines(got), want) {
				t.Errorf("Excerpt(false) =\n%q\nwant\n%q", excerptLines(got), want)
			}
		})
	}
}

func TestBuilder_RetentionIsBounded(t *testing.T) {
	const (
		budget     = 50
		lineCount  = 200_000
		lineLength = 256
		ceiling    = 16 << 20
	)

	tests := []struct {
		name   string
		script func(b *cilog.Builder, line func(i int) string)
	}{
		{
			name: "job tail with no step",
			script: func(b *cilog.Builder, line func(int) string) {
				for i := range lineCount {
					b.Line(line(i), cilog.Plain)
				}
			},
		},
		{
			name: "open step with a head run",
			script: func(b *cilog.Builder, line func(int) string) {
				b.OpenStep("build", cilog.StepRule{})
				for i := range lineCount {
					kind := cilog.Plain
					if i%7 < 3 {
						kind = cilog.Head
					}
					b.Line(line(i), kind)
				}
			},
		},
		{
			name: "end marked step with end lines throughout",
			script: func(b *cilog.Builder, line func(int) string) {
				b.OpenStep("build", cilog.StepRule{EndMarked: true})
				for i := range lineCount {
					kind := cilog.Plain
					if i%1000 == 999 {
						kind = cilog.End
					}
					b.Line(line(i), kind)
				}
			},
		},
		{
			name: "head from end with one endless head run",
			script: func(b *cilog.Builder, line func(int) string) {
				b.OpenStep("build", cilog.StepRule{HeadFromEnd: true})
				for i := range lineCount {
					b.Line(line(i), cilog.Head)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := func(i int) string {
				return fmt.Sprintf("%0*d", lineLength, i)
			}
			b := cilog.NewBuilder(budget)
			tt.script(b, line)

			runtime.GC()
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			excerpt, _ := b.Excerpt(true)
			runtime.KeepAlive(b)

			if stats.HeapAlloc > ceiling {
				t.Errorf("heap in use after %d lines of %d bytes = %d bytes, want at most %d (the budget is %d lines)", lineCount, lineLength, stats.HeapAlloc, ceiling, budget)
			}
			if got := len(excerptLines(excerpt)); got > budget+3 {
				t.Errorf("Excerpt holds %d lines, want at most %d", got, budget+3)
			}
		})
	}
}

func TestBuilder_ExcerptUnchangedByTrailingLines(t *testing.T) {
	t.Parallel()

	build := func(trailing int) string {
		b := cilog.NewBuilder(10)
		b.OpenStep("build", cilog.StepRule{EndMarked: true})
		record(b, cilog.Head, "h1", "h2", "h3")
		record(b, cilog.Plain, numbered("p", 1, 30)...)
		b.Line("fail", cilog.End)
		record(b, cilog.Plain, numbered("z", 1, trailing)...)
		got, _ := b.Excerpt(true)
		return got
	}

	short, long := build(0), build(100_000)

	if short != long {
		t.Errorf("Excerpt after 100000 trailing lines =\n%q\nwant the excerpt at the end line\n%q", excerptLines(long), excerptLines(short))
	}
}

func TestBuilder_ExcerptInvariants(t *testing.T) {
	t.Parallel()

	fragments := []string{
		"plain text", "x", "    indented", "\x1b[31mcolor\x1b[0m", "\x1b]0;title\x07", "tab\there",
		"é", "😀", "\r", "\x00", " ", "##[error]x", "$ cmd",
		strings.Repeat("long ", 600), strings.Repeat("é", 3000), "\n", "\n", "\n", "\n\n",
	}

	for seed := range uint64(200) {
		t.Run("seed "+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()

			rng := rand.New(rand.NewPCG(seed, 99))
			n := 1 + rng.IntN(12)
			var body strings.Builder
			for range rng.IntN(300) {
				body.WriteString(fragments[rng.IntN(len(fragments))])
			}

			b := cilog.NewBuilder(n)
			lineNo := 0
			complete, err := cilog.Scan(strings.NewReader(body.String()), func(line string) {
				lineNo++
				switch rng.IntN(25) {
				case 0:
					b.OpenStep("step "+strconv.Itoa(lineNo), cilog.StepRule{EndMarked: rng.IntN(2) == 0, HeadFromEnd: rng.IntN(2) == 0, Concurrent: rng.IntN(4) == 0})
				case 1:
					b.CloseStep()
				case 2:
					if rng.IntN(4) == 0 {
						b.DropStep()
					}
				}
				kind := cilog.Plain
				switch rng.IntN(6) {
				case 0:
					kind = cilog.Head
				case 1:
					kind = cilog.End
				}
				b.Line(line, kind)
			})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			excerpt, _ := b.Excerpt(complete)

			if excerpt == "" {
				return
			}
			lines := excerptLines(excerpt)
			if !strings.HasPrefix(lines[0], "[sortie] ") {
				t.Fatalf("first line = %q, want a Sortie note", lines[0])
			}
			sortieLines, bodyLines := 0, 0
			for _, line := range lines {
				if line == "" {
					t.Errorf("excerpt holds an empty line: %q", lines)
				}
				if strings.HasPrefix(line, "[sortie] ") {
					sortieLines++
					continue
				}
				bodyLines++
				if len(line) > lineCap+len(cutSuffix) {
					t.Errorf("body line of %d bytes, want at most %d", len(line), lineCap+len(cutSuffix))
				}
				if strings.ContainsAny(line, "\x1b\x00\r") {
					t.Errorf("body line %q holds an escape or control character", line)
				}
				if line != strings.TrimRight(line, " \t") {
					t.Errorf("body line %q has trailing whitespace", line)
				}
			}
			if sortieLines > 3 {
				t.Errorf("excerpt holds %d Sortie lines, want at most 3", sortieLines)
			}
			if bodyLines > n {
				t.Errorf("excerpt holds %d body lines for a budget of %d, want at most %d", bodyLines, n, n)
			}
		})
	}
}

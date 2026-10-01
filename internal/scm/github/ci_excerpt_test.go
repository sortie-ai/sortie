package github

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/scm/cilog"
)

const (
	tailNote    = "[sortie] The failing step could not be located, so these are the last lines of the job log."
	partialNote = "[sortie] The failing step could not be located and the job log was not read to its end, so these are the last lines that were read."

	sortieJobID    int64 = 101566349874
	nodeJobID      int64 = 105162574915
	concurrentJob  int64 = 110103731552
	waitStepJobID  int64 = 108839381533
	sortieLastLine       = "##[error]Process completed with exit code 1."
)

var excerptEpoch = time.Date(2026, 9, 6, 22, 0, 0, 0, time.UTC)

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

func stampAt(ms int) string {
	return excerptEpoch.Add(time.Duration(ms) * time.Millisecond).Format("2006-01-02T15:04:05.0000000Z")
}

func logAt(ms int, content string) string {
	return stampAt(ms) + " " + content
}

func windowStep(priorHeaders int, headerless, concurrent bool) *failingStep {
	return &failingStep{
		label:        "build",
		windowStart:  excerptEpoch.Add(10 * time.Second),
		windowEnd:    excerptEpoch.Add(13 * time.Second),
		priorHeaders: priorHeaders,
		headerless:   headerless,
		concurrent:   concurrent,
	}
}

func scanLines(step *failingStep, maxLines int, lines []string) ([]string, cilog.Fallback) {
	builder := cilog.NewBuilder(maxLines)
	scanner := newStepLogScanner(step, builder)
	for _, line := range lines {
		scanner.line(line)
	}
	text, fallback := builder.Excerpt(true)
	if text == "" {
		return nil, fallback
	}
	return strings.Split(text, "\n"), fallback
}

func jobStep(number int, name, conclusion string, startSec, endSec int) githubJobStep {
	step := githubJobStep{
		Name:   name,
		Status: "completed",
		Number: number,
	}
	if conclusion != "" {
		step.Conclusion = &conclusion
	}
	started := excerptEpoch.Add(time.Duration(startSec) * time.Second).Format(time.RFC3339)
	completed := excerptEpoch.Add(time.Duration(endSec) * time.Second).Format(time.RFC3339)
	step.StartedAt, step.CompletedAt = &started, &completed
	return step
}

func TestSelectFailingStep(t *testing.T) {
	t.Parallel()

	withStatus := func(step githubJobStep, status string) githubJobStep {
		step.Status = status
		return step
	}
	withoutStart := func(step githubJobStep) githubJobStep {
		step.StartedAt = nil
		return step
	}
	withoutEnd := func(step githubJobStep) githubJobStep {
		step.CompletedAt = nil
		return step
	}
	withUnparseableEnd := func(step githubJobStep) githubJobStep {
		garbage := "not a time"
		step.CompletedAt = &garbage
		return step
	}

	type want struct {
		label        string
		startSec     int
		endSec       int
		priorHeaders int
		headerless   bool
		concurrent   bool
	}
	tests := []struct {
		name  string
		steps []githubJobStep
		want  *want
	}{
		{name: "no steps", steps: nil, want: nil},
		{
			name:  "every step succeeded",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "Build", "success", 1, 5)},
			want:  nil,
		},
		{
			name:  "a skipped step is never chosen",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "Build", "skipped", 1, 1)},
			want:  nil,
		},
		{
			name:  "neutral and stale conclusions are not failures",
			steps: []githubJobStep{jobStep(1, "A", "neutral", 0, 1), jobStep(2, "B", "stale", 1, 2)},
			want:  nil,
		},
		{
			name:  "first failing step by number, whatever the list order",
			steps: []githubJobStep{jobStep(3, "Third", "failure", 5, 8), jobStep(2, "Second", "failure", 2, 5), jobStep(1, "Set up job", "success", 0, 2)},
			want:  &want{label: "Second", startSec: 2, endSec: 5, headerless: false},
		},
		{
			name:  "timed out counts as failing",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "Slow", "timed_out", 1, 9)},
			want:  &want{label: "Slow", startSec: 1, endSec: 9},
		},
		{
			name:  "action required counts as failing",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "Approve", "action_required", 1, 2)},
			want:  &want{label: "Approve", startSec: 1, endSec: 2},
		},
		{
			name:  "cancelled step is chosen when nothing failed",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "Stuck", "cancelled", 1, 9), jobStep(3, "Later", "cancelled", 9, 10)},
			want:  &want{label: "Stuck", startSec: 1, endSec: 9},
		},
		{
			name:  "a later failure wins over an earlier cancelled step",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "Stuck", "cancelled", 1, 3), jobStep(3, "Broken", "failure", 3, 5)},
			want:  &want{label: "Broken", startSec: 3, endSec: 5},
		},
		{
			name: "steps that did not complete are ignored",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				withStatus(jobStep(2, "Running", "failure", 1, 4), "in_progress"),
				jobStep(3, "Broken", "failure", 4, 6),
			},
			want: &want{label: "Broken", startSec: 4, endSec: 6},
		},
		{
			name: "steps without a conclusion are ignored",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Pending", "", 1, 4),
				jobStep(3, "Broken", "failure", 4, 6),
			},
			want: &want{label: "Broken", startSec: 4, endSec: 6},
		},
		{
			name: "steps without usable times are ignored",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				withoutStart(jobStep(2, "No start", "failure", 1, 4)),
				withoutEnd(jobStep(3, "No end", "failure", 1, 4)),
				withUnparseableEnd(jobStep(4, "Garbage end", "failure", 1, 4)),
				jobStep(5, "Backwards", "failure", 9, 7),
				jobStep(6, "Broken", "failure", 4, 6),
			},
			want: &want{label: "Broken", startSec: 4, endSec: 6},
		},
		{
			name:  "failing first executed step is headerless",
			steps: []githubJobStep{jobStep(1, "Set up job", "failure", 0, 3)},
			want:  &want{label: "Set up job", startSec: 0, endSec: 3, headerless: true},
		},
		{
			name:  "headerless follows executed steps, not step numbers",
			steps: []githubJobStep{jobStep(1, "Skipped first", "skipped", 0, 0), jobStep(2, "Broken", "failure", 5, 6)},
			want:  &want{label: "Broken", startSec: 5, endSec: 6, headerless: true},
		},
		{
			name: "steps that started in the same second count as prior headers",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Quick A", "success", 1, 1),
				jobStep(3, "Quick B", "success", 1, 1),
				jobStep(4, "Broken", "failure", 1, 5),
				jobStep(5, "Later", "success", 1, 1),
			},
			want: &want{label: "Broken", startSec: 1, endSec: 5, priorHeaders: 2},
		},
		{
			name: "the setup step prints no header and is not counted",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 1, 1),
				jobStep(2, "Broken", "failure", 1, 4),
			},
			want: &want{label: "Broken", startSec: 1, endSec: 4, priorHeaders: 0},
		},
		{
			name: "a skipped step in the same second prints no header and is not counted",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Skipped", "skipped", 1, 1),
				jobStep(3, "Broken", "failure", 1, 4),
			},
			want: &want{label: "Broken", startSec: 1, endSec: 4},
		},
		{
			name: "a step that started in an earlier second is not a prior header",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Earlier", "success", 1, 2),
				jobStep(3, "Broken", "failure", 2, 4),
			},
			want: &want{label: "Broken", startSec: 2, endSec: 4},
		},
		{
			name: "a step inside the span makes it concurrent",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Broken", "failure", 5, 9),
				jobStep(3, "Inside", "success", 6, 8),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9, concurrent: true},
		},
		{
			name: "a step that starts before the end and runs past it makes it concurrent",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Broken", "failure", 5, 9),
				jobStep(3, "Overhang", "success", 8, 20),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9, concurrent: true},
		},
		{
			name: "a step that ended before it began does not overlap",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Before", "success", 2, 5),
				jobStep(3, "Broken", "failure", 5, 9),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9},
		},
		{
			name: "a step that begins as it ends does not overlap",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Broken", "failure", 5, 9),
				jobStep(3, "After", "success", 9, 12),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9},
		},
		{
			name: "a zero length step at its start does not overlap",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Instant", "success", 5, 5),
				jobStep(3, "Broken", "failure", 5, 9),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9, priorHeaders: 1},
		},
		{
			name: "a zero length step inside the span makes it concurrent",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Broken", "failure", 5, 9),
				jobStep(3, "Instant", "success", 7, 7),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9, concurrent: true},
		},
		{
			name: "a skipped step overlapping the span does not make it concurrent",
			steps: []githubJobStep{
				jobStep(1, "Set up job", "success", 0, 1),
				jobStep(2, "Broken", "failure", 5, 9),
				jobStep(3, "Skipped", "skipped", 5, 9),
			},
			want: &want{label: "Broken", startSec: 5, endSec: 9},
		},
		{
			name:  "empty name becomes the step number",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(7, "", "failure", 1, 2)},
			want:  &want{label: "7", startSec: 1, endSec: 2},
		},
		{
			name:  "name of only control characters and spaces becomes the step number",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(12, " \x00\t\x7f \n", "failure", 1, 2)},
			want:  &want{label: "12", startSec: 1, endSec: 2},
		},
		{
			name:  "name with visible text is kept as is",
			steps: []githubJobStep{jobStep(1, "Set up job", "success", 0, 1), jobStep(2, "\x00 Run tests ", "failure", 1, 2)},
			want:  &want{label: "\x00 Run tests ", startSec: 1, endSec: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := selectFailingStep(tt.steps)

			if ok != (tt.want != nil) {
				t.Fatalf("selectFailingStep() ok = %v, want %v", ok, tt.want != nil)
			}
			if tt.want == nil {
				return
			}
			wantStart := excerptEpoch.Add(time.Duration(tt.want.startSec) * time.Second)
			wantEnd := excerptEpoch.Add(time.Duration(tt.want.endSec)*time.Second + time.Second)
			if got.label != tt.want.label {
				t.Errorf("selectFailingStep().label = %q, want %q", got.label, tt.want.label)
			}
			if !got.windowStart.Equal(wantStart) {
				t.Errorf("selectFailingStep().windowStart = %v, want %v", got.windowStart, wantStart)
			}
			if !got.windowEnd.Equal(wantEnd) {
				t.Errorf("selectFailingStep().windowEnd = %v, want %v (completed_at plus one second)", got.windowEnd, wantEnd)
			}
			if got.priorHeaders != tt.want.priorHeaders {
				t.Errorf("selectFailingStep().priorHeaders = %d, want %d", got.priorHeaders, tt.want.priorHeaders)
			}
			if got.headerless != tt.want.headerless {
				t.Errorf("selectFailingStep().headerless = %v, want %v", got.headerless, tt.want.headerless)
			}
			if got.concurrent != tt.want.concurrent {
				t.Errorf("selectFailingStep().concurrent = %v, want %v", got.concurrent, tt.want.concurrent)
			}
		})
	}
}

func TestStepLogScanner_Window(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		step         *failingStep
		n            int
		lines        []string
		want         []string
		wantFallback cilog.Fallback
	}{
		{
			name: "window includes its start and excludes its end",
			step: windowStep(0, true, false),
			n:    50,
			lines: []string{
				logAt(9_999, "before"),
				logAt(10_000, "first"),
				logAt(12_999, "##[error]last"),
				logAt(13_000, "##[error]at the boundary"),
				logAt(13_500, "##[error]late"),
			},
			want: []string{locatedNote("build"), "first", "##[error]last"},
		},
		{
			name: "lines without a timestamp keep their raw text",
			step: windowStep(0, true, false),
			n:    50,
			lines: []string{
				logAt(10_000, "first"),
				"  at Foo.bar (file.js:10)",
				"weird " + stampAt(11_000) + " inside",
				logAt(12_000, "##[error]x"),
			},
			want: []string{locatedNote("build"), "first", "  at Foo.bar (file.js:10)", "weird " + stampAt(11_000) + " inside", "##[error]x"},
		},
		{
			name: "byte order mark is dropped from the first line only",
			step: windowStep(0, true, false),
			n:    50,
			lines: []string{
				"\uFEFF" + logAt(10_000, "hello"),
				"\uFEFF" + logAt(11_000, "kept"),
				logAt(12_000, "##[error]boom"),
			},
			want: []string{locatedNote("build"), "hello", "\uFEFF" + stampAt(11_000) + " kept", "##[error]boom"},
		},
		{
			name: "carriage returns are not part of the content",
			step: windowStep(0, true, false),
			n:    50,
			lines: []string{
				logAt(10_000, "hello\r"),
				logAt(12_000, "##[error]boom\r"),
			},
			want: []string{locatedNote("build"), "hello", "##[error]boom"},
		},
		{
			name: "a line with a timestamp and no content still moves the window",
			step: windowStep(0, true, false),
			n:    50,
			lines: []string{
				logAt(10_000, "a"),
				stampAt(13_500),
				"##[error]x",
			},
			want:         []string{tailNote, "a", "##[error]x"},
			wantFallback: cilog.NoEndMarker,
		},
		{
			name: "window never reached",
			step: windowStep(0, true, false),
			n:    50,
			lines: []string{
				logAt(1_000, "one"),
				logAt(2_000, "##[error]two"),
			},
			want:         []string{tailNote, "one", "##[error]two"},
			wantFallback: cilog.StepNotFound,
		},
		{
			name: "no selected step records the job tail without timestamps",
			step: nil,
			n:    50,
			lines: []string{
				"\uFEFF" + logAt(1_000, "one\r"),
				logAt(2_000, "\x1b[31mtwo\x1b[0m"),
				"three",
			},
			want:         []string{tailNote, "one", "two", "three"},
			wantFallback: cilog.StepNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanLines(tt.step, tt.n, tt.lines)

			if fallback != tt.wantFallback {
				t.Errorf("fallback = %q, want %q", fallback, tt.wantFallback)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("excerpt =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestStepLogScanner_ErrorLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		lines        []string
		want         []string
		wantFallback cilog.Fallback
	}{
		{
			name: "output ends at the last error line",
			lines: []string{
				logAt(10_000, "first"),
				logAt(11_000, "##[error]one"),
				logAt(11_500, "middle"),
				logAt(12_000, "##[error]two"),
				logAt(12_500, "trailing"),
			},
			want: []string{locatedNote("build"), "first", "##[error]one", "middle", "##[error]two"},
		},
		{
			name: "error marker must open the line",
			lines: []string{
				logAt(10_000, "first"),
				logAt(11_000, "  ##[error]indented"),
				logAt(12_000, "text ##[error] inside"),
			},
			want:         []string{tailNote, "first", "  ##[error]indented", "text ##[error] inside"},
			wantFallback: cilog.NoEndMarker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanLines(windowStep(0, true, false), 50, tt.lines)

			if fallback != tt.wantFallback {
				t.Errorf("fallback = %q, want %q", fallback, tt.wantFallback)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("excerpt =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestStepLogScanner_Headers(t *testing.T) {
	t.Parallel()

	priorHeaderLines := []string{
		"##[group]Run previous step",
		"Post job cleanup.",
		"No background steps remaining to wait for.",
		"Waiting for background step(s) to complete: Lint",
		"Waiting for all background step(s) to complete: Lint",
		"Cancelling background step(s): Lint",
	}
	ownStep := func(first string) []string {
		return []string{
			logAt(10_000, first),
			logAt(10_100, "previous body"),
			logAt(10_900, "##[group]Run own"),
			logAt(10_950, "own body"),
			logAt(11_000, "##[endgroup]"),
			logAt(11_500, "output"),
			logAt(12_000, "##[error]boom"),
		}
	}

	for _, first := range priorHeaderLines {
		t.Run("prior header "+first, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanLines(windowStep(1, false, false), 50, ownStep(first))

			want := []string{locatedNote("build"), "##[group]Run own", "own body", "##[endgroup]", "output", "##[error]boom"}
			if fallback != cilog.NoFallback {
				t.Errorf("fallback = %q, want none", fallback)
			}
			if !slices.Equal(got, want) {
				t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
			}
		})
	}

	notHeaders := []string{
		"##[group]Operating System",
		"##[group]Runner Image",
		"Post job cleanup",
		"No background steps remaining to wait for",
		"##[group]Runbook",
	}
	for _, first := range notHeaders {
		t.Run("not a header "+first, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanLines(windowStep(0, false, false), 50, ownStep(first))

			want := []string{locatedNote("build"), "##[group]Run own", "own body", "##[endgroup]", "output", "##[error]boom"}
			if fallback != cilog.NoFallback {
				t.Errorf("fallback = %q, want none", fallback)
			}
			if !slices.Equal(got, want) {
				t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
			}
		})
	}

	t.Run("headerless step never restarts at a header", func(t *testing.T) {
		t.Parallel()

		lines := []string{logAt(10_000, "banner"), logAt(10_001, "##[group]Run inner"), logAt(10_002, "##[endgroup]")}
		for i := 1; i <= 22; i++ {
			lines = append(lines, logAt(10_002+i, "l"+strconv.Itoa(i)))
		}
		lines = append(lines, logAt(12_000, "##[error]x"))

		got, fallback := scanLines(windowStep(0, true, false), 10, lines)

		want := append([]string{locatedNote("build"), omitted(16)}, append(numbered("l", 14, 22), "##[error]x")...)
		if fallback != cilog.NoFallback {
			t.Errorf("fallback = %q, want none", fallback)
		}
		if !slices.Equal(got, want) {
			t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("concurrent step opens at the window start and ignores headers", func(t *testing.T) {
		t.Parallel()

		lines := []string{logAt(10_000, "c1"), logAt(10_001, "c2"), logAt(10_002, "##[group]Run other")}
		for i := 3; i <= 20; i++ {
			lines = append(lines, logAt(10_000+i, "c"+strconv.Itoa(i)))
		}
		lines = append(lines, logAt(12_000, "##[error]x"))

		got, fallback := scanLines(windowStep(0, false, true), 10, lines)

		want := append([]string{concurrentNote("build"), omitted(12)}, append(numbered("c", 12, 20), "##[error]x")...)
		if fallback != cilog.NoFallback {
			t.Errorf("fallback = %q, want none", fallback)
		}
		if !slices.Equal(got, want) {
			t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
		}
	})
}

func TestStepLogScanner_Head(t *testing.T) {
	t.Parallel()

	body := func(eol string, header string, group bool, plain int) []string {
		var lines []string
		add := func(content string) { lines = append(lines, logAt(10_000+len(lines), content+eol)) }
		add(header)
		if group {
			add("body")
			add("##[endgroup]")
		}
		for i := 1; i <= plain; i++ {
			add("p" + strconv.Itoa(i))
		}
		add("##[error]boom")
		return lines
	}

	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name:  "endgroup ends the head",
			lines: body("", "##[group]Run echo", true, 100),
			want:  append(append([]string{locatedNote("build"), "##[group]Run echo", "body", "##[endgroup]", omitted(54)}, numbered("p", 55, 100)...), "##[error]boom"),
		},
		{
			name:  "endgroup ends the head on a log with carriage returns",
			lines: body("\r", "##[group]Run echo", true, 100),
			want:  append(append([]string{locatedNote("build"), "##[group]Run echo", "body", "##[endgroup]", omitted(54)}, numbered("p", 55, 100)...), "##[error]boom"),
		},
		{
			name:  "a single line header is a one line head",
			lines: body("", "Post job cleanup.", false, 100),
			want:  append(append([]string{locatedNote("build"), "Post job cleanup.", omitted(52)}, numbered("p", 53, 100)...), "##[error]boom"),
		},
		{
			name:  "a single line header is recognized on a log with carriage returns",
			lines: body("\r", "Post job cleanup.", false, 100),
			want:  append(append([]string{locatedNote("build"), "Post job cleanup.", omitted(52)}, numbered("p", 53, 100)...), "##[error]boom"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanLines(windowStep(0, false, false), 50, tt.lines)

			if fallback != cilog.NoFallback {
				t.Errorf("fallback = %q, want none", fallback)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("excerpt =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestHeaderPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		content    string
		wantHeader bool
		wantLine   bool
	}{
		{"##[group]Run go test ./...", true, false},
		{"##[group]Run ", true, false},
		{"##[group]Run", false, false},
		{"##[group]Runner Image", false, false},
		{"##[group]Environment details", false, false},
		{"##[endgroup]", false, false},
		{"Post job cleanup.", true, true},
		{"Post job cleanup", false, false},
		{"Post job cleanup. extra", false, false},
		{"No background steps remaining to wait for.", true, true},
		{"No background steps remaining to wait for", false, false},
		{"Waiting for background step(s) to complete: Lint, Test", true, true},
		{"Waiting for all background step(s) to complete: Lint", true, true},
		{"Cancelling background step(s): Lint", true, true},
		{"Waiting for background step(s) to complete:", false, false},
		{"Finished waiting for background step(s).", false, false},
		{"", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			t.Parallel()

			if got := isHeader(tt.content); got != tt.wantHeader {
				t.Errorf("isHeader(%q) = %v, want %v", tt.content, got, tt.wantHeader)
			}
			if got := isLineHeader(tt.content); got != tt.wantLine {
				t.Errorf("isLineHeader(%q) = %v, want %v", tt.content, got, tt.wantLine)
			}
		})
	}
}

type reply struct {
	status  int
	body    []byte
	partial int
	block   bool
	endless []byte
}

func (rp reply) serve(w http.ResponseWriter, r *http.Request) {
	status := cmp.Or(rp.status, http.StatusOK)
	w.WriteHeader(status)
	if len(rp.endless) > 0 {
		for chunk := bytes.Repeat(rp.endless, 4096); ; {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}
	if status != http.StatusOK || rp.partial <= 0 {
		writeBody(w, rp.body)
		return
	}

	writeBody(w, rp.body[:rp.partial])
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if rp.block {
		<-r.Context().Done()
		return
	}
	panic(http.ErrAbortHandler)
}

func writeBody(w http.ResponseWriter, body []byte) {
	w.Write(body) //nolint:errcheck // test helper
}

type actionsFake struct {
	checkRuns []byte
	jobs      map[int64]reply
	logs      map[int64]reply

	redirectLogsTo string

	mu       sync.Mutex
	requests []string
}

func (f *actionsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	if strings.HasSuffix(r.URL.Path, "/check-runs") {
		w.Header().Set("Content-Type", "application/json")
		writeBody(w, f.checkRuns)
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/repos/owner/repo/actions/jobs/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	idText, isLog := strings.CutSuffix(rest, "/logs")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	table := f.jobs
	if isLog {
		if f.redirectLogsTo != "" {
			http.Redirect(w, r, f.redirectLogsTo, http.StatusFound)
			return
		}
		table = f.logs
	}
	rp, found := table[id]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rp.serve(w, r)
}

func (f *actionsFake) requested(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, requested := range f.requests {
		if requested == path {
			count++
		}
	}
	return count
}

func failingCheckRuns(t *testing.T, jobID int64) []byte {
	t.Helper()
	type app struct {
		Slug string `json:"slug"`
	}
	type checkRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		App        app    `json:"app"`
	}
	runs := struct {
		TotalCount int        `json:"total_count"`
		CheckRuns  []checkRun `json:"check_runs"`
	}{
		TotalCount: 2,
		CheckRuns: []checkRun{
			{ID: 7, Name: "lint", Status: "completed", Conclusion: "success", HTMLURL: "https://github.com/owner/repo/runs/7", App: app{Slug: "github-actions"}},
			{ID: jobID, Name: "test", Status: "completed", Conclusion: "failure", HTMLURL: "https://github.com/owner/repo/runs/" + strconv.FormatInt(jobID, 10), App: app{Slug: "github-actions"}},
		},
	}
	out, err := json.Marshal(runs)
	if err != nil {
		t.Fatalf("marshal check runs: %v", err)
	}
	return out
}

func fakeForJob(t *testing.T, jobID int64, jobObject, jobLog []byte) *actionsFake {
	t.Helper()
	return &actionsFake{
		checkRuns: failingCheckRuns(t, jobID),
		jobs:      map[int64]reply{jobID: {body: jobObject}},
		logs:      map[int64]reply{jobID: {body: jobLog}},
	}
}

func fetchFromFake(ctx context.Context, t *testing.T, f *actionsFake, maxLogLines int) domain.CIResult {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	result, err := newTestCIProvider(t, srv.URL, maxLogLines).FetchCIStatus(ctx, "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	return result
}

func excerptFrom(t *testing.T, f *actionsFake, maxLogLines int) []string {
	t.Helper()
	result := fetchFromFake(context.Background(), t, f, maxLogLines)
	if result.LogExcerpt == "" {
		return nil
	}
	return strings.Split(result.LogExcerpt, "\n")
}

func editSteps(t *testing.T, jobObject []byte, edit func(steps []githubJobStep) []githubJobStep) []byte {
	t.Helper()
	var job githubJob
	if err := json.Unmarshal(jobObject, &job); err != nil {
		t.Fatalf("decode job object: %v", err)
	}
	out, err := json.Marshal(githubJob{Steps: edit(job.Steps)})
	if err != nil {
		t.Fatalf("encode job object: %v", err)
	}
	return out
}

func stepByNumber(t *testing.T, steps []githubJobStep, number int) *githubJobStep {
	t.Helper()
	for i := range steps {
		if steps[i].Number == number {
			return &steps[i]
		}
	}
	t.Fatalf("job object has no step %d", number)
	return nil
}

func setTimes(step *githubJobStep, started, completed string) {
	step.StartedAt, step.CompletedAt = &started, &completed
}

func setConclusion(step *githubJobStep, conclusion string) {
	step.Conclusion = &conclusion
}

func splitLog(raw []byte) []string {
	return strings.Split(string(raw), "\n")
}

func joinLog(lines []string) []byte {
	return []byte(strings.Join(lines, "\n"))
}

func indexOfLine(t *testing.T, lines []string, needle string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, needle) {
			return i
		}
	}
	t.Fatalf("log has no line containing %q", needle)
	return -1
}

func lastIndexOfLine(t *testing.T, lines []string, needle string) int {
	t.Helper()
	for i, line := range slices.Backward(lines) {
		if strings.Contains(line, needle) {
			return i
		}
	}
	t.Fatalf("log has no line containing %q", needle)
	return -1
}

func retime(t *testing.T, lines []string, from, to int, first time.Time, step time.Duration) {
	t.Helper()
	for i := from; i < to; i++ {
		_, content, found := strings.Cut(lines[i], " ")
		if !found {
			t.Fatalf("log line %d has no timestamp: %q", i, lines[i])
		}
		stamp := first.Add(time.Duration(i-from) * step).Format("2006-01-02T15:04:05.0000000Z")
		lines[i] = stamp + " " + content
	}
}

func byteOffsetAfter(t *testing.T, raw []byte, lineIndex int) int {
	t.Helper()
	offset := 0
	for range lineIndex + 1 {
		next := bytes.IndexByte(raw[offset:], '\n')
		if next < 0 {
			t.Fatalf("log has fewer than %d lines", lineIndex+1)
		}
		offset += next + 1
	}
	return offset
}

var (
	oracleStamp  = regexp.MustCompile(`^\S+Z `)
	oracleEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

func plainLogLines(raw []byte) []string {
	var out []string
	for i, line := range splitLog(raw) {
		if i == 0 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		line = strings.TrimSuffix(line, "\r")
		line = oracleStamp.ReplaceAllString(line, "")
		line = oracleEscape.ReplaceAllString(line, "")
		line = strings.TrimRight(line, " \t")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func tailExcerpt(note string, raw []byte, maxLines int) []string {
	lines := plainLogLines(raw)
	return append([]string{note}, lines[max(0, len(lines)-maxLines):]...)
}

var sortieHead = []string{
	"##[group]Run go test -count=1 ./...",
	"go test -count=1 ./...",
	`shell: C:\Program Files\PowerShell\7\pwsh.EXE -command ". '{0}'"`,
	"env:",
	"  GOTOOLCHAIN: local",
	"##[endgroup]",
}

func assertSortieExcerpt(t *testing.T, got []string) {
	t.Helper()
	if len(got) != 1+len(sortieHead)+1+44 {
		t.Errorf("excerpt has %d lines, want %d", len(got), 1+len(sortieHead)+1+44)
	}
	if len(got) < 2+len(sortieHead) {
		t.Fatalf("excerpt = %q, too short", got)
	}
	if got[0] != locatedNote("Run tests") {
		t.Errorf("first line = %q, want %q", got[0], locatedNote("Run tests"))
	}
	if !slices.Equal(got[1:1+len(sortieHead)], sortieHead) {
		t.Errorf("head = %q, want %q", got[1:1+len(sortieHead)], sortieHead)
	}
	if got[1+len(sortieHead)] != omitted(26) {
		t.Errorf("line after the head = %q, want %q", got[1+len(sortieHead)], omitted(26))
	}
	if last := got[len(got)-1]; last != sortieLastLine {
		t.Errorf("last line = %q, want %q", last, sortieLastLine)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"--- FAIL: TestRunWorkerAttempt_HandoffEvidencePolicyIsFrozen (5.18s)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("excerpt lacks %q", want)
		}
	}
	for _, unwanted := range []string{"set GOVERSION=go1.26.1", "Post job cleanup."} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("excerpt holds %q from outside the failing step", unwanted)
		}
	}
}

func TestGitHubExcerpt_SortieJob(t *testing.T) {
	t.Parallel()

	steps := loadFixture(t, "job_101566349874_steps.json")
	jobLog := loadFixture(t, "job_101566349874.log")

	t.Run("failing step of the recorded job", func(t *testing.T) {
		t.Parallel()

		f := fakeForJob(t, sortieJobID, steps, jobLog)

		got := excerptFrom(t, f, 50)

		assertSortieExcerpt(t, got)
		if n := f.requested("/repos/owner/repo/actions/jobs/101566349874"); n != 1 {
			t.Errorf("job object requested %d times, want 1", n)
		}
		if n := f.requested("/repos/owner/repo/actions/jobs/101566349874/logs"); n != 1 {
			t.Errorf("job log requested %d times, want 1", n)
		}
	})

	t.Run("cancelled step with no failing conclusion gives the same excerpt", func(t *testing.T) {
		t.Parallel()

		cancelled := editSteps(t, steps, func(s []githubJobStep) []githubJobStep {
			setConclusion(stepByNumber(t, s, 4), "cancelled")
			return s
		})

		got := excerptFrom(t, fakeForJob(t, sortieJobID, cancelled, jobLog), 50)

		assertSortieExcerpt(t, got)
	})

	t.Run("job object served for another job gives the job tail", func(t *testing.T) {
		t.Parallel()

		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.jobs = map[int64]reply{sortieJobID + 1: {body: steps}}

		got := excerptFrom(t, f, 50)

		want := tailExcerpt(tailNote, jobLog, 50)
		if !slices.Equal(got, want) {
			t.Errorf("excerpt =\n%q\nwant the job tail\n%q", got, want)
		}
		if n := f.requested("/repos/owner/repo/actions/jobs/101566349874"); n != 1 {
			t.Errorf("job object requested %d times for the check run's id, want 1", n)
		}
	})

	t.Run("a log served for another job yields nothing", func(t *testing.T) {
		t.Parallel()

		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.logs = map[int64]reply{sortieJobID + 1: {body: jobLog}}

		got := excerptFrom(t, f, 50)

		if got != nil {
			t.Errorf("excerpt = %q, want none when the job's own log is missing", got)
		}
	})
}

func TestGitHubExcerpt_SharedSecondEdges(t *testing.T) {
	t.Parallel()

	steps := loadFixture(t, "job_101566349874_steps.json")
	jobLog := loadFixture(t, "job_101566349874.log")

	tests := []struct {
		name string
		edit func(t *testing.T, steps []githubJobStep, lines []string) []githubJobStep
	}{
		{
			name: "an earlier step started in the failing step's start second",
			edit: func(t *testing.T, steps []githubJobStep, lines []string) []githubJobStep {
				setupHeader := indexOfLine(t, lines, "##[group]Run actions/setup-go")
				testHeader := indexOfLine(t, lines, "##[group]Run go test")
				retime(t, lines, setupHeader, testHeader, excerptEpoch.Add(3*time.Minute+51*time.Second), time.Microsecond)
				setTimes(stepByNumber(t, steps, 3), "2026-09-06T22:03:51Z", "2026-09-06T22:03:51Z")
				return steps
			},
		},
		{
			name: "the failing step's header printed in the second after its start",
			edit: func(t *testing.T, steps []githubJobStep, lines []string) []githubJobStep {
				testHeader := indexOfLine(t, lines, "##[group]Run go test")
				postCleanup := indexOfLine(t, lines, "Post job cleanup.")
				retime(t, lines, testHeader, postCleanup, excerptEpoch.Add(3*time.Minute+52*time.Second), time.Millisecond)
				retime(t, lines, postCleanup, len(lines)-1, excerptEpoch.Add(8*time.Minute+18*time.Second), time.Millisecond)
				setTimes(stepByNumber(t, steps, 4), "2026-09-06T22:03:51Z", "2026-09-06T22:08:18Z")
				setTimes(stepByNumber(t, steps, 10), "2026-09-06T22:08:19Z", "2026-09-06T22:08:20Z")
				setTimes(stepByNumber(t, steps, 11), "2026-09-06T22:08:20Z", "2026-09-06T22:08:20Z")
				return steps
			},
		},
		{
			name: "a failing step that starts and completes in one second, followed by another step's header",
			edit: func(t *testing.T, steps []githubJobStep, lines []string) []githubJobStep {
				testHeader := indexOfLine(t, lines, "##[group]Run go test")
				retime(t, lines, testHeader, len(lines)-1, excerptEpoch.Add(3*time.Minute+51*time.Second+200*time.Millisecond), time.Microsecond)
				setTimes(stepByNumber(t, steps, 4), "2026-09-06T22:03:51Z", "2026-09-06T22:03:51Z")
				setTimes(stepByNumber(t, steps, 10), "2026-09-06T22:03:52Z", "2026-09-06T22:03:53Z")
				setTimes(stepByNumber(t, steps, 11), "2026-09-06T22:03:53Z", "2026-09-06T22:03:53Z")
				return steps
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lines := splitLog(jobLog)
			edited := editSteps(t, steps, func(s []githubJobStep) []githubJobStep { return tt.edit(t, s, lines) })

			got := excerptFrom(t, fakeForJob(t, sortieJobID, edited, joinLog(lines)), 50)

			assertSortieExcerpt(t, got)
		})
	}

	t.Run("the job's first step failing starts at the first log line", func(t *testing.T) {
		t.Parallel()

		edited := editSteps(t, steps, func(s []githubJobStep) []githubJobStep {
			for i := range s {
				setConclusion(&s[i], "skipped")
			}
			first := stepByNumber(t, s, 1)
			setConclusion(first, "failure")
			setTimes(first, "2026-09-06T22:02:49Z", "2026-09-06T22:08:17Z")
			return s
		})

		got := excerptFrom(t, fakeForJob(t, sortieJobID, edited, jobLog), 1000)

		if len(got) < 3 {
			t.Fatalf("excerpt = %q, too short", got)
		}
		if got[0] != locatedNote("Set up job") {
			t.Errorf("first line = %q, want %q", got[0], locatedNote("Set up job"))
		}
		if got[1] != "Current runner version: '2.337.0'" {
			t.Errorf("first body line = %q, want the log's first line without its byte order mark", got[1])
		}
		if last := got[len(got)-1]; last != sortieLastLine {
			t.Errorf("last line = %q, want %q", last, sortieLastLine)
		}
	})
}

func TestGitHubExcerpt_NodeJob(t *testing.T) {
	t.Parallel()

	f := fakeForJob(t, nodeJobID, loadFixture(t, "job_105162574915_steps.json"), loadFixture(t, "job_105162574915.log"))

	got := excerptFrom(t, f, 50)

	if len(got) < 3 {
		t.Fatalf("excerpt = %q, too short", got)
	}
	if got[0] != locatedNote("Lint JavaScript files") {
		t.Errorf("first line = %q, want %q", got[0], locatedNote("Lint JavaScript files"))
	}
	if got[1] != "##[group]Run set +e" {
		t.Errorf("first body line = %q, want %q", got[1], "##[group]Run set +e")
	}
	if last := got[len(got)-1]; last != "##[error]Process completed with exit code 2." {
		t.Errorf("last line = %q, want %q", last, "##[error]Process completed with exit code 2.")
	}
	annotations := 0
	for _, line := range got {
		if strings.HasPrefix(line, "##[error]") && strings.Contains(line, "Assertions must be wrapped into") {
			annotations++
		}
	}
	if annotations != 2 {
		t.Errorf("excerpt holds %d eslint ##[error] annotation lines, want 2", annotations)
	}
	joined := strings.Join(got, "\n")
	for _, unwanted := range []string{"##[group]Environment details", "Post job cleanup."} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("excerpt holds %q from outside the failing step", unwanted)
		}
	}
}

func TestGitHubExcerpt_ConcurrentSteps(t *testing.T) {
	t.Parallel()

	t.Run("typecheck beside three background steps", func(t *testing.T) {
		t.Parallel()

		f := fakeForJob(t, concurrentJob, loadFixture(t, "job_110103731552_steps.json"), loadFixture(t, "job_110103731552.log"))

		got := excerptFrom(t, f, 50)

		if len(got) < 3 {
			t.Fatalf("excerpt = %q, too short", got)
		}
		if got[0] != concurrentNote("Typecheck") {
			t.Errorf("first line = %q, want %q", got[0], concurrentNote("Typecheck"))
		}
		if got[1] != omitted(146) {
			t.Errorf("first body line = %q, want %q", got[1], omitted(146))
		}
		omissions := 0
		for _, line := range got[1:] {
			if strings.HasPrefix(line, "[sortie] ") {
				omissions++
			}
		}
		if omissions != 1 {
			t.Errorf("excerpt holds %d omission lines, want only one", omissions)
		}
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "error TS2428") {
			t.Error("excerpt lacks the error TS2428 line")
		}
		if last := got[len(got)-1]; last != sortieLastLine {
			t.Errorf("last line = %q, want %q", last, sortieLastLine)
		}
		for _, unwanted := range []string{"Finished waiting for background step(s).", "Post job cleanup."} {
			if strings.Contains(joined, unwanted) {
				t.Errorf("excerpt holds %q from after the failing step", unwanted)
			}
		}
	})

	t.Run("type check beside a sibling step that fails later", func(t *testing.T) {
		t.Parallel()

		f := fakeForJob(t, waitStepJobID, loadFixture(t, "job_108839381533_steps.json"), loadFixture(t, "job_108839381533.log"))

		got := excerptFrom(t, f, 50)

		if len(got) < 3 {
			t.Fatalf("excerpt = %q, too short", got)
		}
		if got[0] != concurrentNote("Check types (TypeScript latest)") {
			t.Errorf("first line = %q, want %q", got[0], concurrentNote("Check types (TypeScript latest)"))
		}
		if !strings.HasPrefix(got[1], "Waiting for background step(s) to complete: Prepare CommonJS and ES module fixtures") {
			t.Errorf("first body line = %q, want the wait step's opening line", got[1])
		}
		if last := got[len(got)-1]; last != sortieLastLine {
			t.Errorf("last line = %q, want %q", last, sortieLastLine)
		}
		joined := strings.Join(got, "\n")
		if strings.Contains(joined, "exit code 2") {
			t.Error("excerpt holds the exit code 2 error written after the window")
		}
		if strings.Contains(joined, "lines omitted") || strings.Contains(joined, "line omitted") {
			t.Errorf("excerpt = %q, want the whole window with no omission", got)
		}
	})

	t.Run("step edited to fail with an error line appended to its output", func(t *testing.T) {
		t.Parallel()

		steps := loadFixture(t, "job_108839381533_steps.json")
		jobLog := loadFixture(t, "job_108839381533.log")
		edited := editSteps(t, steps, func(s []githubJobStep) []githubJobStep {
			setConclusion(stepByNumber(t, s, 10), "failure")
			return s
		})
		lines := splitLog(jobLog)
		last := indexOfLine(t, lines, "API Extractor completed successfully")
		stamp, _, _ := strings.Cut(lines[last], " ")
		lines[last] = stamp + " ##[error]Process completed with exit code 1."

		got := excerptFrom(t, fakeForJob(t, waitStepJobID, edited, joinLog(lines)), 50)

		if len(got) < 3 {
			t.Fatalf("excerpt = %q, too short", got)
		}
		if got[0] != locatedNote("Check API surface (missing exports, inconsistent visibility, etc.)") {
			t.Errorf("first line = %q, want the located note for step 10", got[0])
		}
		if got[1] != "##[group]Run pnpm --filter fast-check run api-extractor" {
			t.Errorf("first body line = %q, want the step's own header", got[1])
		}
		if last := got[len(got)-1]; last != sortieLastLine {
			t.Errorf("last line = %q, want %q", last, sortieLastLine)
		}
		for _, line := range got {
			if strings.HasPrefix(line, "[sortie] ") && line != got[0] {
				t.Errorf("excerpt holds the extra Sortie line %q, want no omission", line)
			}
		}
	})
}

type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type recorder struct {
	mu      sync.Mutex
	records []logRecord
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]string)
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, logRecord{level: rec.Level, msg: rec.Message, attrs: attrs})
	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (r *recorder) named(msg string) []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []logRecord
	for _, rec := range r.records {
		if rec.msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
}

func captureDefaultLogger(t *testing.T) *recorder {
	t.Helper()
	original := slog.Default()
	rec := &recorder{}
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(original) })
	return rec
}

const (
	fallbackMessage   = "log excerpt fell back to the job tail"
	jobStepsWarning   = "failed to fetch job steps for log excerpt"
	jobLogWarning     = "failed to fetch job log"
	readEndedWarning  = "job log read ended early"
	sortieJobIDString = "101566349874"
)

func TestGitHubExcerpt_Fallbacks(t *testing.T) {
	steps := loadFixture(t, "job_101566349874_steps.json")
	jobLog := loadFixture(t, "job_101566349874.log")
	rec := captureDefaultLogger(t)

	tests := []struct {
		name       string
		jobs       reply
		logs       []byte
		wantReason string
		wantWarn   string
	}{
		{
			name:       "job object request fails",
			jobs:       reply{status: http.StatusInternalServerError, body: []byte(`{"message":"boom"}`)},
			logs:       jobLog,
			wantReason: "job_steps_unavailable",
			wantWarn:   jobStepsWarning,
		},
		{
			name:       "job object does not decode",
			jobs:       reply{body: []byte("<html>not json</html>")},
			logs:       jobLog,
			wantReason: "job_steps_unavailable",
			wantWarn:   jobStepsWarning,
		},
		{
			name: "no failing or cancelled step",
			jobs: reply{body: editSteps(t, steps, func(s []githubJobStep) []githubJobStep {
				setConclusion(stepByNumber(t, s, 4), "success")
				return s
			})},
			logs:       jobLog,
			wantReason: "no_failing_step",
		},
		{
			name: "no log line inside the window",
			jobs: reply{body: editSteps(t, steps, func(s []githubJobStep) []githubJobStep {
				for i := range s {
					started := time.Date(2026, 9, 7, 6, 0, 0, 0, time.UTC).Format(time.RFC3339)
					setTimes(&s[i], started, started)
				}
				setTimes(stepByNumber(t, s, 4), "2026-09-07T06:00:00Z", "2026-09-07T06:00:05Z")
				return s
			})},
			logs:       jobLog,
			wantReason: "step_not_found",
		},
		{
			name:       "no error line in the step",
			jobs:       reply{body: steps},
			logs:       bytes.ReplaceAll(jobLog, []byte("##[error]"), []byte("##[warning]")),
			wantReason: "no_end_marker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.reset()
			f := fakeForJob(t, sortieJobID, steps, tt.logs)
			f.jobs[sortieJobID] = tt.jobs

			got := excerptFrom(t, f, 50)

			want := tailExcerpt(tailNote, tt.logs, 50)
			if !slices.Equal(got, want) {
				t.Errorf("excerpt =\n%q\nwant the job tail\n%q", got, want)
			}
			records := rec.named(fallbackMessage)
			if len(records) != 1 {
				t.Fatalf("got %d %q records, want 1", len(records), fallbackMessage)
			}
			if records[0].level != slog.LevelDebug {
				t.Errorf("%q level = %v, want DEBUG", fallbackMessage, records[0].level)
			}
			if records[0].attrs["job_id"] != sortieJobIDString {
				t.Errorf("%q job_id = %q, want %q", fallbackMessage, records[0].attrs["job_id"], sortieJobIDString)
			}
			if records[0].attrs["reason"] != tt.wantReason {
				t.Errorf("%q reason = %q, want %q", fallbackMessage, records[0].attrs["reason"], tt.wantReason)
			}
			if tt.wantWarn != "" {
				warns := rec.named(tt.wantWarn)
				if len(warns) != 1 {
					t.Fatalf("got %d %q records, want 1", len(warns), tt.wantWarn)
				}
				if warns[0].level != slog.LevelWarn || warns[0].attrs["job_id"] != sortieJobIDString || warns[0].attrs["error"] == "" {
					t.Errorf("%q record = %+v, want WARN with job_id and error", tt.wantWarn, warns[0])
				}
			}
		})
	}

	t.Run("a located excerpt logs no fallback", func(t *testing.T) {
		rec.reset()

		got := excerptFrom(t, fakeForJob(t, sortieJobID, steps, jobLog), 50)

		if len(got) == 0 || got[0] != locatedNote("Run tests") {
			t.Fatalf("excerpt = %q, want the located excerpt", got)
		}
		if n := len(rec.named(fallbackMessage)); n != 0 {
			t.Errorf("got %d %q records for a located excerpt, want 0", n, fallbackMessage)
		}
	})

	t.Run("an empty log logs no fallback", func(t *testing.T) {
		rec.reset()

		got := excerptFrom(t, fakeForJob(t, sortieJobID, steps, []byte("\r\n \n\n")), 50)

		if got != nil {
			t.Errorf("excerpt = %q, want none", got)
		}
		if n := len(rec.named(fallbackMessage)); n != 0 {
			t.Errorf("got %d %q records for an empty log, want 0", n, fallbackMessage)
		}
	})

	t.Run("job object requested for the wrong job leaves the located excerpt unreachable", func(t *testing.T) {
		rec.reset()
		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.jobs = map[int64]reply{sortieJobID + 1: {body: steps}}

		got := excerptFrom(t, f, 50)

		if len(got) == 0 || got[0] != tailNote {
			t.Fatalf("excerpt = %q, want the job tail", got)
		}
		records := rec.named(fallbackMessage)
		if len(records) != 1 || records[0].attrs["reason"] != "job_steps_unavailable" {
			t.Errorf("fallback records = %+v, want one with reason job_steps_unavailable", records)
		}
	})
}

func TestGitHubExcerpt_ReadFailures(t *testing.T) {
	steps := loadFixture(t, "job_101566349874_steps.json")
	jobLog := loadFixture(t, "job_101566349874.log")
	rec := captureDefaultLogger(t)

	lines := splitLog(jobLog)
	afterClose := byteOffsetAfter(t, jobLog, lastIndexOfLine(t, lines, "Removing HTTP extra header"))
	insideStep := byteOffsetAfter(t, jobLog, indexOfLine(t, lines, "--- FAIL: TestRunWorkerAttempt_HandoffEvidencePolicyIsFrozen"))

	t.Run("a read that fails after the step closed keeps the located excerpt", func(t *testing.T) {
		rec.reset()
		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.logs[sortieJobID] = reply{body: jobLog, partial: afterClose}

		got := excerptFrom(t, f, 50)

		assertSortieExcerpt(t, got)
		warns := rec.named(readEndedWarning)
		if len(warns) != 1 || warns[0].attrs["job_id"] != sortieJobIDString || warns[0].attrs["error"] == "" {
			t.Errorf("%q records = %+v, want one with job_id and error", readEndedWarning, warns)
		}
		if n := len(rec.named(fallbackMessage)); n != 0 {
			t.Errorf("got %d %q records for a located excerpt, want 0", n, fallbackMessage)
		}
	})

	t.Run("a read that fails inside the step gives the lines read behind the partial note", func(t *testing.T) {
		rec.reset()
		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.logs[sortieJobID] = reply{body: jobLog, partial: insideStep}

		got := excerptFrom(t, f, 50)

		want := tailExcerpt(partialNote, jobLog[:insideStep], 50)
		if !slices.Equal(got, want) {
			t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
		}
		if n := len(rec.named(readEndedWarning)); n != 1 {
			t.Errorf("got %d %q records, want 1", n, readEndedWarning)
		}
		records := rec.named(fallbackMessage)
		if len(records) != 1 || records[0].attrs["reason"] != "read_incomplete" {
			t.Errorf("fallback records = %+v, want one with reason read_incomplete", records)
		}
	})

	t.Run("a read that fails past the context deadline gives nothing", func(t *testing.T) {
		rec.reset()
		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.logs[sortieJobID] = reply{body: jobLog, partial: insideStep, block: true}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		t.Cleanup(cancel)

		result := fetchFromFake(ctx, t, f, 50)

		if result.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty after the context deadline passed mid-body", result.LogExcerpt)
		}
		if result.Status != domain.CIStatusFailing {
			t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusFailing)
		}
		if n := len(rec.named(jobLogWarning)); n != 1 {
			t.Errorf("got %d %q records, want 1", n, jobLogWarning)
		}
		if n := len(rec.named(readEndedWarning)); n != 0 {
			t.Errorf("got %d %q records for an expired context, want 0", n, readEndedWarning)
		}
	})

	t.Run("a log past the read ceiling gives the partial note without a read error", func(t *testing.T) {
		rec.reset()
		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.logs[sortieJobID] = reply{endless: []byte(logAt(240_000, "noise\n"))}

		got := excerptFrom(t, f, 50)

		if len(got) != 51 || got[0] != partialNote {
			t.Fatalf("excerpt of %d lines opening with %q, want %d lines opening with %q", len(got), got[0], 51, partialNote)
		}
		if got[1] != "noise" {
			t.Errorf("first body line = %q, want %q", got[1], "noise")
		}
		if n := len(rec.named(readEndedWarning)); n != 0 {
			t.Errorf("got %d %q records for a read that stopped at the ceiling, want 0", n, readEndedWarning)
		}
		records := rec.named(fallbackMessage)
		if len(records) != 1 || records[0].attrs["reason"] != "read_incomplete" {
			t.Errorf("fallback records = %+v, want one with reason read_incomplete", records)
		}
	})

	t.Run("a log request answered 404 gives nothing", func(t *testing.T) {
		rec.reset()
		f := fakeForJob(t, sortieJobID, steps, jobLog)
		f.logs[sortieJobID] = reply{status: http.StatusNotFound, body: []byte(`{"message":"Not Found"}`)}

		got := excerptFrom(t, f, 50)

		if got != nil {
			t.Errorf("excerpt = %q, want none", got)
		}
		warns := rec.named(jobLogWarning)
		if len(warns) != 1 || warns[0].attrs["job_id"] != sortieJobIDString {
			t.Errorf("%q records = %+v, want one with job_id", jobLogWarning, warns)
		}
	})
}

func TestGitHubExcerpt_RedirectedLog(t *testing.T) {
	t.Parallel()

	steps := loadFixture(t, "job_101566349874_steps.json")
	jobLog := loadFixture(t, "job_101566349874.log")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeBody(w, jobLog)
	}))
	t.Cleanup(storage.Close)
	f := fakeForJob(t, sortieJobID, steps, jobLog)
	f.logs = nil
	f.redirectLogsTo = storage.URL + "/signed-log"

	got := excerptFrom(t, f, 50)

	assertSortieExcerpt(t, got)
}

func TestGitHubExcerpt_VerdictIsIndependentOfTheExcerpt(t *testing.T) {
	t.Parallel()

	steps := loadFixture(t, "job_101566349874_steps.json")
	jobLog := loadFixture(t, "job_101566349874.log")
	lines := splitLog(jobLog)
	insideStep := byteOffsetAfter(t, jobLog, indexOfLine(t, lines, "--- FAIL: TestRunWorkerAttempt_HandoffEvidencePolicyIsFrozen"))

	scenarios := map[string]func(f *actionsFake){
		"located": func(*actionsFake) {},
		"fallback": func(f *actionsFake) {
			f.logs[sortieJobID] = reply{body: bytes.ReplaceAll(jobLog, []byte("##[error]"), []byte("x"))}
		},
		"job object 500": func(f *actionsFake) { f.jobs[sortieJobID] = reply{status: http.StatusInternalServerError} },
		"log 404":        func(f *actionsFake) { f.logs[sortieJobID] = reply{status: http.StatusNotFound} },
		"mid-body error": func(f *actionsFake) { f.logs[sortieJobID] = reply{body: jobLog, partial: insideStep} },
	}

	baseline := fetchFromFake(context.Background(), t, fakeForJob(t, sortieJobID, steps, jobLog), 50)
	if baseline.Status != domain.CIStatusFailing || baseline.FailingCount != 1 || len(baseline.CheckRuns) != 2 {
		t.Fatalf("baseline verdict = %+v, want one failing check of two", baseline)
	}

	for name, apply := range scenarios {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := fakeForJob(t, sortieJobID, steps, jobLog)
			apply(f)

			got := fetchFromFake(context.Background(), t, f, 50)

			if got.Status != baseline.Status {
				t.Errorf("Status = %q, want %q", got.Status, baseline.Status)
			}
			if !reflect.DeepEqual(got.CheckRuns, baseline.CheckRuns) {
				t.Errorf("CheckRuns = %+v, want %+v", got.CheckRuns, baseline.CheckRuns)
			}
			if got.FailingCount != baseline.FailingCount {
				t.Errorf("FailingCount = %d, want %d", got.FailingCount, baseline.FailingCount)
			}
			if got.Ref != baseline.Ref {
				t.Errorf("Ref = %q, want %q", got.Ref, baseline.Ref)
			}
		})
	}
}

package gitlab

import (
	"bytes"
	"context"
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

	fallbackMessage  = "log excerpt fell back to the job tail"
	readEndedMessage = "job trace read ended early"
	traceFailMessage = "failed to fetch job trace"
	failingJobID     = 6002
	logBudget        = 50
)

func locatedNote(label string) string {
	return `[sortie] Output of the failing step "` + label + `"; output from the rest of the job is left out.`
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

func echo(command string) string {
	return "\x1b[32;1m$ " + command + "\x1b[0;m"
}

func runnerLine(content string) string {
	return "2026-10-01T02:10:08.696223Z 01O " + content
}

func scanTrace(lines ...string) ([]string, cilog.Fallback) {
	builder := cilog.NewBuilder(logBudget)
	scanner := newTraceScanner(builder)
	for _, line := range lines {
		scanner.line(line)
	}
	text, fallback := builder.Excerpt(true)
	if text == "" {
		return nil, fallback
	}
	return strings.Split(text, "\n"), fallback
}

func TestTraceScanner(t *testing.T) {
	t.Parallel()

	start := func(name string) string { return "section_start:1790820551:" + name + "\r\x1b[0K" }
	end := func(name string) string { return "section_end:1790820551:" + name + "\r\x1b[0K" }

	tests := []struct {
		name         string
		lines        []string
		want         []string
		wantFallback cilog.Fallback
	}{
		{
			name:  "stage output between its markers",
			lines: []string{"before", start("step_script"), "a", "b", end("step_script"), "after"},
			want:  []string{locatedNote("step_script"), "a", "b"},
		},
		{
			name:  "runner prefix is removed before markers are read",
			lines: []string{"2026-10-01T02:10:58.558063Z 01O+" + start("step_script"), runnerLine("a"), "2026-10-01T02:10:58.558064Z 00O+" + end("step_script")},
			want:  []string{locatedNote("step_script"), "a"},
		},
		{
			name:  "several markers on one line are read in order",
			lines: []string{start("get_sources"), "a", end("get_sources") + start("step_script") + "b", "c", end("step_script")},
			want:  []string{locatedNote("step_script"), "b", "c"},
		},
		{
			name:  "the last runner stage before the first post-script stage is the anchor",
			lines: []string{start("prepare_script"), "p", end("prepare_script"), start("get_sources"), "g", end("get_sources"), start("step_script"), "s", end("step_script")},
			want:  []string{locatedNote("step_script"), "s"},
		},
		{
			name:  "a stage that is not a user step anchors too",
			lines: []string{start("step_script"), "s", end("step_script"), start("restore_cache"), "cached", end("restore_cache")},
			want:  []string{locatedNote("restore_cache"), "cached"},
		},
		{
			name: "sections written by the job's own script never anchor",
			lines: []string{
				start("step_script"), "a", start("my_section"), "inside", end("my_section"), "b", end("step_script"),
			},
			want: []string{locatedNote("step_script"), "a", "inside", "b"},
		},
		{
			name: "a user section ending does not close the stage",
			lines: []string{
				start("step_script"), start("my_section"), end("my_section"), "after the section", end("step_script"),
			},
			want: []string{locatedNote("step_script"), "after the section"},
		},
		{
			name:  "a stage nested in a wrapper stage replaces the wrapper",
			lines: []string{start("concrete"), "wrapper", start("step_script"), "s", end("step_script"), end("concrete"), "ERROR: Job failed"},
			want:  []string{locatedNote("step_script"), "s"},
		},
		{
			name:  "a wrapper with no inner stage ends at the first post-script stage",
			lines: []string{start("concrete"), "wrapper", "more", start("after_script"), "post", end("after_script")},
			want:  []string{locatedNote("concrete"), "wrapper", "more"},
		},
		{
			name:  "a stage after a post-script stage is never an anchor",
			lines: []string{start("step_script"), "s", end("step_script"), start("after_script"), "post", end("after_script"), start("extra_stage"), "extra", end("extra_stage")},
			want:  []string{locatedNote("step_script"), "s"},
		},
		{
			name:         "a trace with no markers anchors nothing",
			lines:        []string{runnerLine("one"), runnerLine("two")},
			want:         []string{tailNote, "one", "two"},
			wantFallback: cilog.StepNotFound,
		},
		{
			name:         "a post-script stage with no anchor before it anchors nothing",
			lines:        []string{"one", start("after_script"), "two", end("after_script")},
			want:         []string{tailNote, "one", "two"},
			wantFallback: cilog.StepNotFound,
		},
		{
			name:         "a success-path stage with no anchor drops nothing",
			lines:        []string{"one", start("archive_cache"), "two"},
			want:         []string{tailNote, "one", "two"},
			wantFallback: cilog.StepNotFound,
		},
		{
			name:         "archive_cache after the anchor means the job did not stop there",
			lines:        []string{start("step_script"), "a", end("step_script"), start("archive_cache"), "b", end("archive_cache")},
			want:         []string{tailNote, "a", "b"},
			wantFallback: cilog.StepDropped,
		},
		{
			name:         "upload_artifacts_on_success after the anchor means the job did not stop there",
			lines:        []string{start("step_script"), "a", end("step_script"), start("upload_artifacts_on_success"), "b"},
			want:         []string{tailNote, "a", "b"},
			wantFallback: cilog.StepDropped,
		},
		{
			name:         "an anchored stage with no line",
			lines:        []string{start("step_script"), end("step_script"), start("after_script"), "post"},
			want:         []string{tailNote, "post"},
			wantFallback: cilog.EmptyStep,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanTrace(tt.lines...)

			if fallback != tt.wantFallback {
				t.Errorf("fallback = %q, want %q", fallback, tt.wantFallback)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("excerpt =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestTraceScanner_SectionNameEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		marker string
	}{
		{"option bracket", "section_start:1790820551:step_script[collapsed=true]\r\x1b[0K"},
		{"whitespace", "section_start:1790820551:step_script some title"},
		{"carriage return", "section_start:1790820551:step_script\rtitle"},
		{"escape", "section_start:1790820551:step_script\x1b[0K"},
		{"line end", "section_start:1790820551:step_script"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, _ := scanTrace(tt.marker, "a")

			if len(got) == 0 || got[0] != locatedNote("step_script") {
				t.Errorf("excerpt = %q, want it to open with the note for label step_script", got)
			}
		})
	}
}

func TestTraceScanner_PostScriptStages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		stage        string
		wantFallback cilog.Fallback
	}{
		{"after_script", cilog.NoFallback},
		{"archive_cache_on_failure", cilog.NoFallback},
		{"upload_artifacts_on_failure", cilog.NoFallback},
		{"cleanup_file_variables", cilog.NoFallback},
		{"archive_cache", cilog.StepDropped},
		{"upload_artifacts_on_success", cilog.StepDropped},
	}

	for _, tt := range tests {
		t.Run(tt.stage, func(t *testing.T) {
			t.Parallel()

			got, fallback := scanTrace(
				"section_start:1:step_script", "a", "section_end:1:step_script",
				"section_start:2:"+tt.stage, "later", "section_end:2:"+tt.stage)

			if fallback != tt.wantFallback {
				t.Errorf("fallback = %q, want %q", fallback, tt.wantFallback)
			}
			if tt.wantFallback == cilog.NoFallback && !slices.Equal(got, []string{locatedNote("step_script"), "a"}) {
				t.Errorf("excerpt = %q, want the step_script output only", got)
			}
		})
	}
}

func TestTraceScanner_Head(t *testing.T) {
	t.Parallel()

	var lines []string
	lines = append(lines, "section_start:1:step_script\r\x1b[0K")
	for i := 1; i <= 12; i++ {
		lines = append(lines, runnerLine(echo("cmd "+strconv.Itoa(i))))
	}
	for i := 1; i <= 100; i++ {
		lines = append(lines, runnerLine("p"+strconv.Itoa(i)))
	}
	lines = append(lines, "section_end:1:step_script\r\x1b[0K")

	t.Run("last echo lines of the run form the head", func(t *testing.T) {
		t.Parallel()

		got, fallback := scanTrace(lines...)

		want := append([]string{locatedNote("step_script"), omitted(2)}, "$ cmd 3", "$ cmd 4", "$ cmd 5", "$ cmd 6", "$ cmd 7", "$ cmd 8", "$ cmd 9", "$ cmd 10", "$ cmd 11", "$ cmd 12", omitted(60))
		want = append(want, numbered("p", 61, 100)...)
		if fallback != cilog.NoFallback {
			t.Errorf("fallback = %q, want none", fallback)
		}
		if !slices.Equal(got, want) {
			t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("a command without the runner's color is not an echo", func(t *testing.T) {
		t.Parallel()

		plain := []string{"section_start:1:step_script"}
		for i := 1; i <= 3; i++ {
			plain = append(plain, runnerLine("$ cmd "+strconv.Itoa(i)))
		}
		plain = append(plain, runnerLine(echo("real")))
		for i := 1; i <= 60; i++ {
			plain = append(plain, runnerLine("p"+strconv.Itoa(i)))
		}

		got, _ := scanTrace(plain...)

		want := append([]string{locatedNote("step_script"), omitted(3), "$ real", omitted(11)}, numbered("p", 12, 60)...)
		if !slices.Equal(got, want) {
			t.Errorf("excerpt =\n%q\nwant\n%q", got, want)
		}
	})
}

func TestIsCommandEcho(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want bool
	}{
		{"colored echo", "\x1b[32;1m$ ls -l\x1b[0;m", true},
		{"leading carriage return", "\r\x1b[32;1m$ ls", true},
		{"leading erase line", "\x1b[0K\x1b[32;1m$ ls", true},
		{"several carriage returns and erases", "\r\x1b[0K\r\x1b[0K\x1b[32;1m$ ls", true},
		{"no color", "$ ls", false},
		{"other color", "\x1b[31;1m$ ls", false},
		{"no space after the dollar sign", "\x1b[32;1m$", false},
		{"echo not at the start", "text \x1b[32;1m$ ls", false},
		{"other escape before the echo", "\x1b[0;m\x1b[32;1m$ ls", false},
		{"empty", "", false},
		{"only prefixes", "\r\x1b[0K", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isCommandEcho(tt.text); got != tt.want {
				t.Errorf("isCommandEcho(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func newTraceFake(t *testing.T, trace http.HandlerFunc) *fakeServer {
	t.Helper()
	s := newFakeServer(t)
	withCommitResolution(t, s, loadFixture(t, "commit_resolved.json"))
	withCommitStatuses(t, s, staticJSONHandler(loadFixture(t, "statuses_one_failed.json")))
	withPipelineJobs(t, s, loadFixture(t, "pipeline_jobs.json"))
	s.handle("/api/v4/projects/"+testEscapedProject+"/jobs/"+strconv.Itoa(failingJobID)+"/trace", trace)
	return s
}

func traceBody(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck // test helper
	}
}

func abortingTrace(body []byte, afterBytes int, block bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body[:afterBytes]) //nolint:errcheck // test helper
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if block {
			<-r.Context().Done()
			return
		}
		panic(http.ErrAbortHandler)
	}
}

func fetchTrace(ctx context.Context, t *testing.T, s *fakeServer) (domain.CIResult, string) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	provider := mustCIProvider(t, srv.URL, logBudget)
	log, buf := newCapturingLogger()
	provider.log = log

	result, err := provider.FetchCIStatus(ctx, "main")
	if err != nil {
		t.Fatalf("FetchCIStatus: unexpected error: %v", err)
	}
	return result, buf.String()
}

func excerptLines(excerpt string) []string {
	if excerpt == "" {
		return nil
	}
	return strings.Split(excerpt, "\n")
}

func logRecords(output, msg string) []string {
	var records []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) || strings.Contains(line, "msg="+msg+" ") {
			records = append(records, line)
		}
	}
	return records
}

var (
	oracleRunnerPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z \d{2}[OE]\+? ?`)
	oracleEscape       = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	oracleMarker       = regexp.MustCompile(`section_(start|end):[0-9]+:[^\[\s\x00-\x1f\x7f]+`)
)

func plainTraceLines(raw []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = oracleRunnerPrefix.ReplaceAllString(line, "")
		line = oracleMarker.ReplaceAllString(line, "")
		line = oracleEscape.ReplaceAllString(line, "")
		line = strings.ReplaceAll(line, "\r", "")
		line = strings.TrimRight(line, " \t")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func tailOf(note string, raw []byte, maxLines int) []string {
	lines := plainTraceLines(raw)
	return append([]string{note}, lines[max(0, len(lines)-maxLines):]...)
}

func TestGitLabExcerpt_RspecJob(t *testing.T) {
	t.Parallel()

	trace := loadFixture(t, "job_16853455906_trace.txt")
	var requests int
	var mu sync.Mutex
	s := newTraceFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		traceBody(trace)(w, r)
	})

	result, _ := fetchTrace(context.Background(), t, s)

	got := excerptLines(result.LogExcerpt)
	if len(got) != 1+1+3+1+47 {
		t.Fatalf("excerpt has %d lines, want %d: %q", len(got), 1+1+3+1+47, got)
	}
	if got[0] != locatedNote("step_script") {
		t.Errorf("first line = %q, want %q", got[0], locatedNote("step_script"))
	}
	if got[1] != omitted(65) {
		t.Errorf("line before the head = %q, want %q", got[1], omitted(65))
	}
	wantHead := []string{
		`$ export RSPEC_SKIPPED_TESTS_REPORT_PATH="rspec/skipped_tests-${CI_JOB_ID}.txt"`,
		`$ export RSPEC_RETRIED_TESTS_REPORT_PATH="rspec/retried_tests-${CI_JOB_ID}.txt"`,
	}
	if !slices.Equal(got[2:4], wantHead) {
		t.Errorf("head lines 1-2 = %q, want %q", got[2:4], wantHead)
	}
	if !strings.HasPrefix(got[4], "$ rspec_section rspec_fail_fast ") {
		t.Errorf("last head line = %q, want the rspec_section command", got[4])
	}
	if got[5] != omitted(149) {
		t.Errorf("line after the head = %q, want %q", got[5], omitted(149))
	}
	if last := got[len(got)-1]; last != "Saved rspec/rspec-16853455906.json." {
		t.Errorf("last line = %q, want %q", last, "Saved rspec/rspec-16853455906.json.")
	}
	joined := result.LogExcerpt
	if !strings.Contains(joined, "\nFailures:\n") {
		t.Error("excerpt lacks the rspec Failures: block")
	}
	for _, unwanted := range []string{"Running after_script", "Running after script", "section_start", "section_end", "\x1b"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("excerpt holds %q", unwanted)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Errorf("trace of job %d requested %d times, want 1", failingJobID, requests)
	}
}

func TestGitLabExcerpt_ConcreteWrapper(t *testing.T) {
	t.Parallel()

	trace := loadFixture(t, "job_16845977955_trace.txt")

	result, _ := fetchTrace(context.Background(), t, newTraceFake(t, traceBody(trace)))

	got := excerptLines(result.LogExcerpt)
	if len(got) < 3 {
		t.Fatalf("excerpt = %q, too short", got)
	}
	if got[0] != locatedNote("step_script") {
		t.Errorf("first line = %q, want %q", got[0], locatedNote("step_script"))
	}
	wantLast := "**If needed, you can retry the [🔁 `danger-review` job](https://gitlab.com/gitlab-org/gitlab-runner/-/jobs/16845977955) that generated this comment.**"
	if last := got[len(got)-1]; last != wantLast {
		t.Errorf("last line = %q, want the last line of the step_script section %q", last, wantLast)
	}
	if strings.Contains(result.LogExcerpt, `ERROR: Job failed: step "concrete"`) {
		t.Error("excerpt holds the wrapper's job-failed line from outside the step_script section")
	}
}

func TestGitLabExcerpt_Fallbacks(t *testing.T) {
	t.Parallel()

	prefixed := func(lines ...string) []byte {
		var out []string
		for _, line := range lines {
			out = append(out, runnerLine(line))
		}
		return []byte(strings.Join(out, "\n") + "\n")
	}
	tests := []struct {
		name       string
		trace      []byte
		wantReason string
	}{
		{
			name:       "no runner stage",
			trace:      prefixed("one", "two", "three"),
			wantReason: "step_not_found",
		},
		{
			name: "success-path stage after the anchor",
			trace: prefixed("section_start:1:step_script\r\x1b[0K", "a", "section_end:1:step_script\r\x1b[0K",
				"section_start:2:archive_cache\r\x1b[0K", "b", "section_end:2:archive_cache\r\x1b[0K"),
			wantReason: "step_dropped",
		},
		{
			name: "anchored stage with no line",
			trace: prefixed("section_start:1:step_script\r\x1b[0K", "section_end:1:step_script\r\x1b[0K",
				"section_start:2:after_script\r\x1b[0K", "x", "section_end:2:after_script\r\x1b[0K"),
			wantReason: "empty_step",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, output := fetchTrace(context.Background(), t, newTraceFake(t, traceBody(tt.trace)))

			if want := tailOf(tailNote, tt.trace, 50); !slices.Equal(excerptLines(result.LogExcerpt), want) {
				t.Errorf("excerpt =\n%q\nwant the job tail\n%q", excerptLines(result.LogExcerpt), want)
			}
			records := logRecords(output, fallbackMessage)
			if len(records) != 1 {
				t.Fatalf("got %d %q records in %q, want 1", len(records), fallbackMessage, output)
			}
			for _, want := range []string{"level=DEBUG", "job_id=" + strconv.Itoa(failingJobID), "reason=" + tt.wantReason} {
				if !strings.Contains(records[0], want) {
					t.Errorf("record %q lacks %q", records[0], want)
				}
			}
		})
	}

	t.Run("a located excerpt and an empty trace log no fallback", func(t *testing.T) {
		t.Parallel()

		located := prefixed("section_start:1:step_script\r\x1b[0K", "a", "section_end:1:step_script\r\x1b[0K")
		for name, trace := range map[string][]byte{"located": located, "empty": []byte("\r\n \n")} {
			_, output := fetchTrace(context.Background(), t, newTraceFake(t, traceBody(trace)))

			if records := logRecords(output, fallbackMessage); len(records) != 0 {
				t.Errorf("%s trace logged %q, want no fallback record", name, records)
			}
		}
	})

	t.Run("an empty trace gives an empty excerpt", func(t *testing.T) {
		t.Parallel()

		result, _ := fetchTrace(context.Background(), t, newTraceFake(t, traceBody([]byte("\r\n \n"))))

		if result.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty", result.LogExcerpt)
		}
	})
}

func TestGitLabExcerpt_ReadFailures(t *testing.T) {
	t.Parallel()

	trace := loadFixture(t, "job_16853455906_trace.txt")
	afterStep := bytes.Index(trace, []byte("section_end:1790820829:step_script"))
	afterStep += bytes.IndexByte(trace[afterStep:], '\n') + 1
	insideStep := bytes.Index(trace, []byte("Finished in 6.42 seconds"))
	insideStep += bytes.IndexByte(trace[insideStep:], '\n') + 1

	t.Run("a read failing after the stage closed keeps the located excerpt", func(t *testing.T) {
		t.Parallel()

		want, _ := fetchTrace(context.Background(), t, newTraceFake(t, traceBody(trace)))

		result, output := fetchTrace(context.Background(), t, newTraceFake(t, abortingTrace(trace, afterStep, false)))

		if result.LogExcerpt != want.LogExcerpt {
			t.Errorf("LogExcerpt =\n%q\nwant the located excerpt\n%q", result.LogExcerpt, want.LogExcerpt)
		}
		if len(logRecords(output, readEndedMessage)) != 1 {
			t.Errorf("log output %q lacks one %q record", output, readEndedMessage)
		}
		if records := logRecords(output, fallbackMessage); len(records) != 0 {
			t.Errorf("located excerpt logged %q", records)
		}
	})

	t.Run("a read failing inside the stage gives the lines read behind the partial note", func(t *testing.T) {
		t.Parallel()

		result, output := fetchTrace(context.Background(), t, newTraceFake(t, abortingTrace(trace, insideStep, false)))

		if want := tailOf(partialNote, trace[:insideStep], 50); !slices.Equal(excerptLines(result.LogExcerpt), want) {
			t.Errorf("excerpt =\n%q\nwant\n%q", excerptLines(result.LogExcerpt), want)
		}
		records := logRecords(output, fallbackMessage)
		if len(records) != 1 || !strings.Contains(records[0], "reason=read_incomplete") {
			t.Errorf("fallback records = %q, want one with reason read_incomplete", records)
		}
		if len(logRecords(output, readEndedMessage)) != 1 {
			t.Errorf("log output %q lacks one %q record", output, readEndedMessage)
		}
	})

	t.Run("a read failing past the context deadline gives nothing", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		t.Cleanup(cancel)

		result, output := fetchTrace(ctx, t, newTraceFake(t, abortingTrace(trace, insideStep, true)))

		if result.LogExcerpt != "" {
			t.Errorf("LogExcerpt = %q, want empty after the context deadline passed mid-body", result.LogExcerpt)
		}
		if result.Status != domain.CIStatusFailing {
			t.Errorf("Status = %q, want %q", result.Status, domain.CIStatusFailing)
		}
		if len(logRecords(output, traceFailMessage)) != 1 {
			t.Errorf("log output %q lacks one %q record", output, traceFailMessage)
		}
		if records := logRecords(output, readEndedMessage); len(records) != 0 {
			t.Errorf("expired read logged %q, want no early-end record", records)
		}
	})
}

func TestGitLabExcerpt_VerdictIsIndependentOfTheExcerpt(t *testing.T) {
	t.Parallel()

	trace := loadFixture(t, "job_16853455906_trace.txt")
	insideStep := bytes.Index(trace, []byte("Finished in 6.42 seconds"))

	baseline, _ := fetchTrace(context.Background(), t, newTraceFake(t, traceBody(trace)))
	if baseline.Status != domain.CIStatusFailing || baseline.FailingCount == 0 || baseline.LogExcerpt == "" {
		t.Fatalf("baseline = %+v, want a failing verdict with an excerpt", baseline)
	}

	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
	}
	scenarios := map[string]http.HandlerFunc{
		"located":        traceBody(trace),
		"fallback":       traceBody([]byte("plain\nlines\n")),
		"trace 404":      status(http.StatusNotFound),
		"trace 500":      status(http.StatusInternalServerError),
		"mid-body error": abortingTrace(trace, insideStep, false),
	}
	for name, handler := range scenarios {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, _ := fetchTrace(context.Background(), t, newTraceFake(t, handler))

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

package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/scm/cilog"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

const (
	groupRunPrefix  = "##[group]Run "
	endGroupLine    = "##[endgroup]"
	errorLinePrefix = "##[error]"
	postJobCleanup  = "Post job cleanup."
	noBackground    = "No background steps remaining to wait for."
)

var backgroundHeaderPrefixes = []string{
	"Waiting for background step(s) to complete: ",
	"Waiting for all background step(s) to complete: ",
	"Cancelling background step(s): ",
}

type githubJob struct {
	Steps []githubJobStep `json:"steps"`
}

type githubJobStep struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	Number      int     `json:"number"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

// failingStep is what the log scan needs to cut the failing step out of the job log.
type failingStep struct {
	label        string
	windowStart  time.Time // started_at, inclusive
	windowEnd    time.Time // completed_at plus one second, exclusive
	priorHeaders int       // executed steps before it that started in the same second, setup step excluded
	headerless   bool      // it is the job's first executed step, which prints no header
	concurrent   bool      // another executed step's span overlaps its span
}

// executedStep is a job step that ran, with its parsed span.
type executedStep struct {
	githubJobStep
	started   time.Time
	completed time.Time
}

// fetchJobSteps returns the step list of the Actions job jobID.
func (p *GitHubCIProvider) fetchJobSteps(ctx context.Context, jobID int64) ([]githubJobStep, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d", p.owner, p.repo, jobID)
	body, _, err := p.client.Get(ctx, path, nil)
	if err != nil {
		return nil, err
	}

	var job githubJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, fmt.Errorf("decoding job %d: %w", jobID, err)
	}
	return job.Steps, nil
}

// selectFailingStep picks the first executed step that failed, else the first
// that was cancelled, and reports false when there is none. A job-level
// timeout or cancellation marks the running step cancelled, so a timed-out
// check can hold no failing step. A continue-on-error failure reports
// success and is never chosen.
func selectFailingStep(steps []githubJobStep) (failingStep, bool) {
	executed := executedSteps(steps)

	chosen := slices.IndexFunc(executed, func(step executedStep) bool {
		return scmcore.IsFailingConclusion(mapCheckConclusion(step.Conclusion))
	})
	if chosen < 0 {
		chosen = slices.IndexFunc(executed, func(step executedStep) bool {
			return *step.Conclusion == "cancelled"
		})
	}
	if chosen < 0 {
		return failingStep{}, false
	}

	subject := executed[chosen]
	priorHeaders := 0
	concurrent := false
	for i, other := range executed {
		if i == chosen {
			continue
		}
		if i > 0 && other.Number < subject.Number && other.started.Equal(subject.started) {
			priorHeaders++
		}
		if other.started.Before(subject.completed) && other.completed.After(subject.started) {
			concurrent = true
		}
	}

	return failingStep{
		label:        stepLabel(subject.githubJobStep),
		windowStart:  subject.started,
		windowEnd:    subject.completed.Add(time.Second),
		priorHeaders: priorHeaders,
		headerless:   chosen == 0,
		concurrent:   concurrent,
	}, true
}

func executedSteps(steps []githubJobStep) []executedStep {
	var executed []executedStep
	for _, step := range steps {
		if step.Status != "completed" || step.Conclusion == nil || *step.Conclusion == "skipped" {
			continue
		}
		started := parseStepTime(step.StartedAt)
		completed := parseStepTime(step.CompletedAt)
		if started.IsZero() || completed.IsZero() || completed.Before(started) {
			continue
		}
		executed = append(executed, executedStep{githubJobStep: step, started: started, completed: completed})
	}
	slices.SortStableFunc(executed, func(a, b executedStep) int { return cmp.Compare(a.Number, b.Number) })
	return executed
}

func parseStepTime(value *string) time.Time {
	if value == nil {
		return time.Time{}
	}
	return scmcore.ParseTimestampOrZero(*value)
}

// stepLabel returns the step name, or its number when the name holds nothing
// the excerpt could show once control characters and spaces are gone.
func stepLabel(step githubJobStep) string {
	for _, r := range step.Name {
		if r > ' ' && r != 0x7f {
			return step.Name
		}
	}
	return strconv.Itoa(step.Number)
}

// stepLogScanner cuts the failing step out of a job log, line by line.
// Timestamps move a window around the step's recorded span, and the runner's
// own header and error lines decide the edges inside the seconds that
// neighboring steps share. It records every line into its builder.
type stepLogScanner struct {
	step    *failingStep
	builder *cilog.Builder

	seenFirst bool
	prevStamp time.Time
	opened    bool
	inWindow  bool
	inHead    bool
	headers   int
}

func newStepLogScanner(step *failingStep, builder *cilog.Builder) *stepLogScanner {
	return &stepLogScanner{step: step, builder: builder}
}

func (s *stepLogScanner) line(raw string) {
	if !s.seenFirst {
		raw = strings.TrimPrefix(raw, "\uFEFF")
		s.seenFirst = true
	}
	raw = strings.TrimSuffix(raw, "\r")

	stamp, content := s.prevStamp, raw
	token, rest, _ := strings.Cut(raw, " ")
	if parsed := scmcore.ParseTimestampOrZero(token); !parsed.IsZero() {
		stamp, content = parsed, rest
	}
	s.prevStamp = stamp

	kind := cilog.Plain
	if s.step != nil {
		kind = s.advance(stamp, content)
	}
	s.builder.Line(content, kind)
}

// advance moves the step window to stamp and classifies content within it.
func (s *stepLogScanner) advance(stamp time.Time, content string) cilog.LineKind {
	if !s.opened && !stamp.Before(s.step.windowStart) {
		s.builder.OpenStep(s.step.label, cilog.StepRule{EndMarked: true, Concurrent: s.step.concurrent})
		s.opened, s.inWindow = true, true
	}
	if s.inWindow && !stamp.Before(s.step.windowEnd) {
		s.builder.CloseStep()
		s.inWindow = false
	}
	if !s.inWindow {
		return cilog.Plain
	}

	if !s.step.concurrent && isHeader(content) {
		s.headers++
		if s.headers == s.step.priorHeaders+1 && !s.step.headerless {
			s.builder.OpenStep(s.step.label, cilog.StepRule{EndMarked: true})
			s.inHead = true
		}
	}

	kind := cilog.Plain
	if s.inHead {
		kind = cilog.Head
		if content == endGroupLine || isLineHeader(content) {
			s.inHead = false
		}
	}
	if strings.HasPrefix(content, errorLinePrefix) {
		kind = cilog.End
		s.inHead = false
	}
	return kind
}

func isHeader(content string) bool {
	return strings.HasPrefix(content, groupRunPrefix) || isLineHeader(content)
}

// isLineHeader reports whether content is a single-line step header: the
// post-job cleanup line or a line printed by a background-step control step.
func isLineHeader(content string) bool {
	if content == postJobCleanup || content == noBackground {
		return true
	}
	return slices.ContainsFunc(backgroundHeaderPrefixes, func(prefix string) bool {
		return strings.HasPrefix(content, prefix)
	})
}

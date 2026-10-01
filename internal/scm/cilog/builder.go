package cilog

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/redact"
)

const (
	maxLabelRunes = 200

	locatedNote    = "[sortie] Output of the failing step \"%s\"; output from the rest of the job is left out."
	concurrentNote = "[sortie] Output of the failing step \"%s\", mixed with output from steps that ran at the same time; output from the rest of the job is left out."
	tailNote       = "[sortie] The failing step could not be located, so these are the last lines of the job log."
	partialNote    = "[sortie] The failing step could not be located and the job log was not read to its end, so these are the last lines that were read."
)

// LineKind is the role a recorded line plays in the failing step.
type LineKind int

const (
	// Plain is a line with no role beyond its position.
	Plain LineKind = iota
	// Head is a line that shows the command the step ran.
	Head
	// End is a line the step's output reaches.
	End
)

// StepRule says how a forge's runner frames a step.
type StepRule struct {
	// EndMarked ends the step's output at its last End line; a step that
	// records none is not located.
	EndMarked bool
	// HeadFromEnd keeps the last lines of a head run over the head cap
	// instead of its first lines.
	HeadFromEnd bool
	// Concurrent marks a step whose lines are interleaved with other steps'
	// lines, so the excerpt opens with a note saying so.
	Concurrent bool
}

// Fallback says why [Builder.Excerpt] returned the job tail instead of the step.
type Fallback string

const (
	// NoFallback means the step was located, or no line was recorded.
	NoFallback Fallback = ""
	// StepNotFound means OpenStep was never called.
	StepNotFound Fallback = "step_not_found"
	// StepDropped means DropStep discarded the step.
	StepDropped Fallback = "step_dropped"
	// NoEndMarker means an EndMarked step recorded no End line.
	NoEndMarker Fallback = "no_end_marker"
	// ReadIncomplete means the step had not closed when the read stopped.
	ReadIncomplete Fallback = "read_incomplete"
	// EmptyStep means the step holds no line up to its end point.
	EmptyStep Fallback = "empty_step"
)

// Builder assembles the excerpt of one CI log from its lines, in log order.
// It retains a number of lines bounded by a constant multiple of the line
// budget however long the log is. A Builder is not safe for concurrent use.
type Builder struct {
	maxLines int
	headCap  int

	jobLines int
	jobTail  tailWindow

	step    *stepState
	dropped bool
}

// NewBuilder creates a [Builder] whose excerpt holds at most maxLines log
// lines. maxLines MUST be positive; a Builder created with a smaller value
// records nothing.
func NewBuilder(maxLines int) *Builder {
	return &Builder{
		maxLines: maxLines,
		headCap:  maxLines / 5,
		jobTail:  tailWindow{max: maxLines},
	}
}

// OpenStep starts the failing step at the next recorded line and discards any
// step opened before, closed or not. label is sanitized and shortened before
// the excerpt shows it.
func (b *Builder) OpenStep(label string, rule StepRule) {
	b.dropped = false
	b.step = &stepState{
		label:   sanitizeLabel(label),
		rule:    rule,
		headCap: b.headCap,
		tail:    tailWindow{max: b.maxLines},
	}
}

// CloseStep ends the open step after the last recorded line. It does nothing
// when no step is open.
func (b *Builder) CloseStep() {
	if b.step != nil {
		b.step.closed = true
	}
}

// DropStep discards the step, open or closed, so that [Builder.Excerpt] falls
// back to the job tail.
func (b *Builder) DropStep() {
	if b.step == nil {
		return
	}
	b.step = nil
	b.dropped = true
}

// Line records one line of the log. The forge's prefix and markers MUST
// already be removed from text. The text is sanitized, and a line that is
// empty afterwards is ignored entirely: it is not recorded, not counted, and
// does not break a head run. A recorded line belongs to the job and, while a
// step is open, to the step; kind matters only for the step.
func (b *Builder) Line(text string, kind LineKind) {
	if b.maxLines <= 0 {
		return
	}
	line := sanitize(text)
	if line == "" {
		return
	}

	b.jobLines++
	b.jobTail.push(line)

	if b.step != nil && !b.step.closed {
		b.step.record(line, kind)
	}
}

// Excerpt returns the excerpt of the recorded log and, when it is the job tail
// rather than the failing step, the reason. complete reports whether the log
// was read to its end. The result is empty when no line was recorded.
func (b *Builder) Excerpt(complete bool) (string, Fallback) {
	if b.jobLines == 0 {
		return "", NoFallback
	}

	end, fallback := b.locate(complete)
	if fallback != NoFallback {
		return b.tailExcerpt(complete), fallback
	}

	note := locatedNote
	if b.step.rule.Concurrent {
		note = concurrentNote
	}
	lines := append([]string{fmt.Sprintf(note, b.step.label)}, b.stepBody(end)...)
	return strings.Join(lines, "\n"), NoFallback
}

func (b *Builder) locate(complete bool) (endPoint, Fallback) {
	switch {
	case b.dropped:
		return endPoint{}, StepDropped
	case !complete && (b.step == nil || !b.step.closed):
		return endPoint{}, ReadIncomplete
	case b.step == nil:
		return endPoint{}, StepNotFound
	case b.step.rule.EndMarked && !b.step.hasEnd:
		return endPoint{}, NoEndMarker
	}

	end := b.step.endPoint()
	if end.count == 0 {
		return endPoint{}, EmptyStep
	}
	return end, NoFallback
}

func (b *Builder) tailExcerpt(complete bool) string {
	note := tailNote
	if !complete {
		note = partialNote
	}
	return strings.Join(append([]string{note}, b.jobTail.last(b.maxLines)...), "\n")
}

// stepBody lays out the located step's lines within the line budget.
func (b *Builder) stepBody(end endPoint) []string {
	budget := b.maxLines
	if end.count <= budget {
		return end.tail
	}

	headLines, headStart := end.head.lines, end.head.begin
	k := len(headLines)
	tailLen := budget - k
	tailStart := end.count - tailLen

	if k == 0 || headStart+k > tailStart {
		return append([]string{omission(end.count - budget)}, end.tail[len(end.tail)-budget:]...)
	}

	var body []string
	if headStart > 0 {
		body = append(body, omission(headStart))
	}
	body = append(body, headLines...)
	if between := tailStart - headStart - k; between > 0 {
		body = append(body, omission(between))
	}
	return append(body, end.tail[len(end.tail)-tailLen:]...)
}

func omission(count int) string {
	if count == 1 {
		return "[sortie] 1 line omitted."
	}
	return "[sortie] " + strconv.Itoa(count) + " lines omitted."
}

func sanitizeLabel(label string) string {
	spaced := strings.Map(func(r rune) rune {
		if r <= 0x1f || r == 0x7f {
			return ' '
		}
		return r
	}, label)
	return redact.Truncate(strings.Trim(spaced, " "), maxLabelRunes)
}

// tailWindow keeps the last max lines pushed into it in memory bounded by
// twice max, trimming in bulk so a push costs amortized constant time.
type tailWindow struct {
	max   int
	lines []string
}

func (w *tailWindow) push(line string) {
	if len(w.lines) >= 2*w.max {
		kept := copy(w.lines, w.lines[len(w.lines)-w.max:])
		clear(w.lines[kept:])
		w.lines = w.lines[:kept]
	}
	w.lines = append(w.lines, line)
}

// last returns up to n of the most recent lines; the slice aliases the window.
func (w *tailWindow) last(n int) []string {
	if len(w.lines) <= n {
		return w.lines
	}
	return w.lines[len(w.lines)-n:]
}

// headRun is the last run of consecutive Head lines of a step. It retains
// only the lines that fit the head cap, and begin is the step line index of
// the first one retained.
type headRun struct {
	begin int
	lines []string
}

func (r headRun) clone() headRun {
	r.lines = append([]string(nil), r.lines...)
	return r
}

// endPoint is the state of a step at the line its output ends on.
type endPoint struct {
	count int
	tail  []string
	head  headRun
}

// stepState holds what the assembly needs from the failing step. Under an
// EndMarked rule the end point can sit well before the latest recorded line,
// so the state at the latest End line is kept apart from the live state.
type stepState struct {
	label   string
	rule    StepRule
	headCap int
	closed  bool

	count    int
	tail     tailWindow
	run      headRun
	inRun    bool
	hasEnd   bool
	endDirty bool // the live state is the state at the latest End line
	end      endPoint
}

func (s *stepState) record(line string, kind LineKind) {
	isEnd := s.rule.EndMarked && kind == End
	if s.endDirty && !isEnd {
		s.end = s.liveEndPoint(true)
		s.endDirty = false
	}

	if kind == Head {
		s.recordHead(line)
	} else {
		s.inRun = false
	}
	s.count++
	s.tail.push(line)

	if isEnd {
		s.hasEnd = true
		s.endDirty = true
	}
}

func (s *stepState) recordHead(line string) {
	if !s.inRun {
		s.run = headRun{begin: s.count}
		s.inRun = true
	}

	if s.headCap == 0 {
		return
	}
	if !s.rule.HeadFromEnd {
		if len(s.run.lines) < s.headCap {
			s.run.lines = append(s.run.lines, line)
		}
		return
	}
	if len(s.run.lines) == s.headCap {
		s.run.lines = append(s.run.lines[:0], s.run.lines[1:]...)
		s.run.begin++
	}
	s.run.lines = append(s.run.lines, line)
}

func (s *stepState) endPoint() endPoint {
	if s.rule.EndMarked && !s.endDirty {
		return s.end
	}
	return s.liveEndPoint(false)
}

func (s *stepState) liveEndPoint(detach bool) endPoint {
	end := endPoint{count: s.count, tail: s.tail.last(s.tail.max), head: s.run}
	if detach {
		end.tail = append([]string(nil), end.tail...)
		end.head = end.head.clone()
	}
	return end
}

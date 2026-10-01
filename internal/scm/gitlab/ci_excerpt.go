package gitlab

import (
	"regexp"
	"strings"

	"github.com/sortie-ai/sortie/internal/scm/cilog"
)

// sectionMarkerPattern matches a section_start or section_end marker. A
// section name ends before an option bracket, whitespace, or a control
// character, so the carriage return the runner writes after a marker is not
// part of the name.
var sectionMarkerPattern = regexp.MustCompile(`section_(start|end):[0-9]+:([^\[\s\x00-\x1f\x7f]+)`)

// postScriptStages are the runner stages that run after the user steps. None
// is ever an anchor, and the first one to start ends the anchored stage.
var postScriptStages = map[string]bool{
	"after_script":                true,
	"archive_cache":               true,
	"archive_cache_on_failure":    true,
	"upload_artifacts_on_success": true,
	"upload_artifacts_on_failure": true,
	"cleanup_file_variables":      true,
}

// successPathStages run only when every user step succeeded, so one starting
// after the anchor means the job did not stop there.
var successPathStages = map[string]bool{
	"archive_cache":               true,
	"upload_artifacts_on_success": true,
}

const (
	userStepPrefix = "step_"

	// commandEchoPrefix is how the runner prints a script command: bold
	// green, "$ ", then the command.
	commandEchoPrefix = "\x1b[32;1m$ "
	eraseLineEscape   = "\x1b[0K"
)

// traceScanner finds the runner stage in which a job stopped while a trace is
// read line by line, and records every line into its builder. The runner
// wraps each stage in section markers, so the anchor is the last runner stage
// that started before the first post-script stage.
type traceScanner struct {
	builder *cilog.Builder

	openUserSteps map[string]struct{}
	anchor        string
	postBegun     bool
}

func newTraceScanner(builder *cilog.Builder) *traceScanner {
	return &traceScanner{
		builder:       builder,
		openUserSteps: make(map[string]struct{}),
	}
}

func (s *traceScanner) line(raw string) {
	content := runnerPrefixPattern.ReplaceAllString(raw, "")

	text := content
	if markers := sectionMarkerPattern.FindAllStringSubmatchIndex(content, -1); len(markers) > 0 {
		var stripped strings.Builder
		last := 0
		for _, marker := range markers {
			stripped.WriteString(content[last:marker[0]])
			last = marker[1]
			name := content[marker[4]:marker[5]]
			if content[marker[2]:marker[3]] == "start" {
				s.sectionStart(name)
			} else {
				s.sectionEnd(name)
			}
		}
		stripped.WriteString(content[last:])
		text = stripped.String()
	}

	kind := cilog.Plain
	if isCommandEcho(text) {
		kind = cilog.Head
	}
	s.builder.Line(text, kind)
}

func (s *traceScanner) sectionStart(name string) {
	runnerStage := len(s.openUserSteps) == 0
	if strings.HasPrefix(name, userStepPrefix) {
		s.openUserSteps[name] = struct{}{}
	}
	if !runnerStage {
		return
	}

	if postScriptStages[name] {
		s.builder.CloseStep()
		s.postBegun = true
		if successPathStages[name] && s.anchor != "" {
			s.builder.DropStep()
		}
		return
	}
	if !s.postBegun {
		s.builder.OpenStep(name, cilog.StepRule{HeadFromEnd: true})
		s.anchor = name
	}
}

func (s *traceScanner) sectionEnd(name string) {
	delete(s.openUserSteps, name)
	if name == s.anchor {
		s.builder.CloseStep()
	}
}

// isCommandEcho reports whether text is a runner echo of a script command,
// ignoring the carriage returns and erase-line escapes the runner writes in
// front of it.
func isCommandEcho(text string) bool {
	for {
		switch {
		case strings.HasPrefix(text, "\r"):
			text = text[1:]
		case strings.HasPrefix(text, eraseLineEscape):
			text = text[len(eraseLineEscape):]
		default:
			return strings.HasPrefix(text, commandEchoPrefix)
		}
	}
}

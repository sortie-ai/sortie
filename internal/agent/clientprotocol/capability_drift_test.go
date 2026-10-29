package clientprotocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/toolrelay"
	"github.com/sortie-ai/sortie/internal/domain"

	"gopkg.in/yaml.v3"
)

const capabilityDriftProfileEnv = "SORTIE_CLIENTPROTOCOL_PROFILE"

const capabilityDriftCommandEnv = "SORTIE_CLIENTPROTOCOL_COMMAND"

var capabilityGapLabelSet = []string{
	toolrelay.ToolServersLabel, capabilityLabelTokenCounts,
	capabilityLabelSessionContinuation, capabilityLabelAgentVersion,
}

// The messages are joined with a separator because one message's suffix and
// the next one's prefix could otherwise spell a label no notification
// reported. An unmeasured result counts as a token-counts gap because a
// capability lowered after the once-per-session notice leaves no
// notification of its own.
func liveCapabilityGapLabels(events []domain.AgentEvent, result domain.TurnResult) []string {
	var joined strings.Builder
	for _, event := range events {
		if event.Type == domain.EventNotification {
			if joined.Len() > 0 {
				joined.WriteByte('\n')
			}
			joined.WriteString(event.Message)
		}
	}
	text := joined.String()

	var labels []string
	for _, label := range capabilityGapLabelSet {
		if strings.Contains(text, label) {
			labels = append(labels, label)
		}
	}
	if !result.UsageMeasured && !slices.Contains(labels, capabilityLabelTokenCounts) {
		labels = append(labels, capabilityLabelTokenCounts)
	}
	sort.Strings(labels)
	return labels
}

func resolveDriftProfilePath(t *testing.T, path string) string {
	t.Helper()
	if filepath.IsAbs(path) {
		return path
	}
	root, err := nightlyRepositoryRoot()
	if err != nil {
		t.Fatalf("resolve repository root for %s=%q: %v", capabilityDriftProfileEnv, path, err)
	}
	return filepath.Join(root, path)
}

type profileDeclaration struct {
	Capability string `json:"capability"`
	Case       string `json:"case"`
	Reason     string `json:"reason"`
	Surface    string `json:"surface"`
}

type capabilityGapLabelsDocument struct {
	CapabilityGapLabels []string             `json:"capability_gap_labels"`
	Declarations        []profileDeclaration `json:"declarations"`
}

func readNamedProfile(t *testing.T) (data []byte, profilePath string, named bool) {
	t.Helper()

	profilePath, present := os.LookupEnv(capabilityDriftProfileEnv)
	if !present || strings.TrimSpace(profilePath) == "" {
		return nil, "", false
	}
	data, err := os.ReadFile(resolveDriftProfilePath(t, profilePath)) //nolint:gosec // the coordinate names a repository-tracked or operator-supplied profile
	if err != nil {
		t.Fatalf("load the profile %s names, %q: %v", capabilityDriftProfileEnv, profilePath, err)
	}
	return data, profilePath, true
}

func namedProfileFoldsRuntimeFailure(t *testing.T) bool {
	t.Helper()

	data, profilePath, named := readNamedProfile(t)
	if !named {
		return false
	}
	var doc capabilityGapLabelsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode the profile %s names, %q: %v", capabilityDriftProfileEnv, profilePath, err)
	}
	return slices.ContainsFunc(doc.Declarations, func(d profileDeclaration) bool {
		return d.Capability == "turn_disposition" && d.Case == "runtime_failure" && d.Reason == "outcome_folded_into_another" &&
			(d.Surface == "" || d.Surface == "protocol")
	})
}

// A profile path naming an unreadable file fails rather than skips, so a typo
// cannot pass green.
func assertCapabilityGapLabelsMatchProfile(t *testing.T, events []domain.AgentEvent, result domain.TurnResult) {
	t.Helper()

	data, profilePath, named := readNamedProfile(t)
	if !named {
		// Returning rather than skipping: the caller's own assertions follow.
		t.Logf("capability-gap drift comparison not run: %s is unset", capabilityDriftProfileEnv)
		return
	}
	var doc capabilityGapLabelsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode the profile %s names, %q: %v", capabilityDriftProfileEnv, profilePath, err)
	}

	live := liveCapabilityGapLabels(events, result)
	want := slices.Clone(doc.CapabilityGapLabels)
	sort.Strings(want)

	if !slices.Equal(live, want) {
		t.Fatalf("live capability-gap labels %v differ from profile %s's capability_gap_labels %v (UsageMeasured=%v, ExitReason=%q)",
			live, profilePath, want, result.UsageMeasured, result.ExitReason)
	}
}

type nightlyWorkflowStep struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type nightlyWorkflowJob struct {
	Env      map[string]string `yaml:"env"`
	Strategy struct {
		Matrix struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []nightlyWorkflowStep `yaml:"steps"`
}

type nightlyWorkflowDocument struct {
	Jobs map[string]nightlyWorkflowJob `yaml:"jobs"`
}

const nightlyWorkflowRelPath = "../../../.github/workflows/nightly.yml"

func readNightlyWorkflow() (nightlyWorkflowDocument, error) {
	raw, err := os.ReadFile(nightlyWorkflowRelPath) //nolint:gosec // the tracked workflow's own path, resolved from a package-relative constant
	if err != nil {
		return nightlyWorkflowDocument{}, err
	}
	var doc nightlyWorkflowDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nightlyWorkflowDocument{}, fmt.Errorf("decode %s: %w", nightlyWorkflowRelPath, err)
	}
	return doc, nil
}

func nightlyMatrixRowsNamingThisPackage(doc nightlyWorkflowDocument) []struct {
	jobName string
	job     nightlyWorkflowJob
	row     map[string]string
} {
	var rows []struct {
		jobName string
		job     nightlyWorkflowJob
		row     map[string]string
	}
	for jobName, job := range doc.Jobs {
		for _, row := range job.Strategy.Matrix.Include {
			if strings.Contains(row["test_path"], "internal/agent/clientprotocol") {
				rows = append(rows, struct {
					jobName string
					job     nightlyWorkflowJob
					row     map[string]string
				}{jobName: jobName, job: job, row: row})
			}
		}
	}
	return rows
}

func nightlyGoTestStep(steps []nightlyWorkflowStep) (nightlyWorkflowStep, bool) {
	for _, step := range steps {
		if strings.Contains(step.Run, "go test") {
			return step, true
		}
	}
	return nightlyWorkflowStep{}, false
}

func nightlyRepositoryRoot() (string, error) {
	abs, err := filepath.Abs(filepath.Join(".", nightlyWorkflowRelPath, "..", "..", ".."))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func callersOfAssertCapabilityGapLabels(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	var callers []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			calls := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "assertCapabilityGapLabelsMatchProfile" {
					calls = true
				}
				return true
			})
			if calls {
				callers = append(callers, fn.Name.Name)
			}
		}
	}
	return callers
}

// A guard that could not read through a matrix expression would pass
// vacuously on exactly the rows it exists to check.
func nightlyMatrixValue(row map[string]string, value string) string {
	trimmed := strings.TrimSpace(value)
	match := nightlyMatrixExpression.FindStringSubmatch(trimmed)
	if match == nil {
		return trimmed
	}
	return strings.TrimSpace(row[match[1]])
}

var nightlyMatrixExpression = regexp.MustCompile(`^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$`)

func TestLiveCapabilityGapLabelsAddsTokenCountsOnlyWhenUnmeasured(t *testing.T) {
	t.Parallel()

	notify := func(messages ...string) []domain.AgentEvent {
		events := make([]domain.AgentEvent, 0, len(messages))
		for _, message := range messages {
			events = append(events, domain.AgentEvent{Type: domain.EventNotification, Message: message})
		}
		return events
	}

	tests := []struct {
		name   string
		events []domain.AgentEvent
		result domain.TurnResult
		want   []string
	}{
		{
			name:   "an unmeasured result adds token counts to a notice naming a different gap",
			events: notify(toolrelay.GapNoticeStem + capabilityLabelAgentVersion),
			result: domain.TurnResult{UsageMeasured: false},
			want:   []string{capabilityLabelAgentVersion, capabilityLabelTokenCounts},
		},
		{
			name:   "an unmeasured result whose notice already names token counts adds nothing more",
			events: notify(toolrelay.GapNoticeStem + capabilityLabelTokenCounts),
			result: domain.TurnResult{UsageMeasured: false},
			want:   []string{capabilityLabelTokenCounts},
		},
		{
			name:   "a measured result leaves notice-derived labels unchanged",
			events: notify(toolrelay.GapNoticeStem + toolrelay.ToolServersLabel),
			result: domain.TurnResult{UsageMeasured: true},
			want:   []string{toolrelay.ToolServersLabel},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := liveCapabilityGapLabels(tt.events, tt.result)
			if !slices.Equal(got, tt.want) {
				t.Errorf("liveCapabilityGapLabels(...) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNightlyReachability(t *testing.T) {
	doc, err := readNightlyWorkflow()
	if err != nil {
		t.Fatalf("read the nightly workflow: %v", err)
	}

	rows := nightlyMatrixRowsNamingThisPackage(doc)
	if len(rows) == 0 {
		t.Fatal("nightly workflow names this package's test_path in no matrix row, want at least one")
	}

	callers := callersOfAssertCapabilityGapLabels(t)
	if len(callers) == 0 {
		t.Fatal("no top-level Test function in this package calls assertCapabilityGapLabelsMatchProfile, want at least one")
	}

	root, err := nightlyRepositoryRoot()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	type shard struct {
		command     string
		spendSource string
		reaches     bool
	}
	shards := map[shard]string{}
	commandReached := map[string]bool{}
	for _, row := range rows {
		name := row.row["name"]

		step, found := nightlyGoTestStep(row.job.Steps)
		if !found {
			t.Fatalf("row %q: job %q carries no step whose run script invokes go test", name, row.jobName)
		}

		env := map[string]string{}
		maps.Copy(env, row.job.Env)
		maps.Copy(env, step.Env)

		command := nightlyMatrixValue(row.row, env[capabilityDriftCommandEnv])
		if command == "" {
			t.Errorf("row %q: no %s in the environment job %q's go-test step inherits, or it is empty", name, capabilityDriftCommandEnv, row.jobName)
		}

		runFilter, skipFilter := row.row["run_filter"], row.row["skip_filter"]
		runPattern, runErr := regexp.Compile(runFilter)
		var skipPattern *regexp.Regexp
		var skipErr error
		if skipFilter != "" {
			skipPattern, skipErr = regexp.Compile(skipFilter)
		}
		if compileErr := errors.Join(runErr, skipErr); compileErr != nil {
			t.Errorf("row %q: compile run_filter %q and skip_filter %q as Go regexps: %v", name, runFilter, skipFilter, compileErr)
			continue
		}
		reaches := slices.ContainsFunc(callers, func(caller string) bool {
			return runPattern.MatchString(caller) && (skipPattern == nil || !skipPattern.MatchString(caller))
		})

		spendSource := nightlyMatrixValue(row.row, env[spendDeclarationEnv])
		key := shard{command: command, spendSource: spendSource, reaches: reaches}
		if previous, duplicate := shards[key]; duplicate {
			t.Errorf("rows %q and %q both drive this package with %s %q, %s %q and reaches=%v, want one row per runtime, spend declaration and tier", previous, name, capabilityDriftCommandEnv, command, spendDeclarationEnv, spendSource, reaches)
		}
		shards[key] = name
		if !rowRunsOnlyContextScenario(row.row) {
			commandReached[command] = commandReached[command] || reaches
		}

		if !reaches {
			continue
		}
		profile := nightlyMatrixValue(row.row, env[capabilityDriftProfileEnv])
		if _, statErr := os.Stat(filepath.Join(root, profile)); profile == "" || statErr != nil {
			t.Errorf("row %q: %s = %q names no existing file under %s", name, capabilityDriftProfileEnv, profile, root)
		}
	}

	for command, reached := range commandReached {
		if !reached {
			t.Errorf("no row that reaches any of %v drives %s %q", callers, capabilityDriftCommandEnv, command)
		}
	}
}

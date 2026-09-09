package clientprotocol

import (
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

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"

	"gopkg.in/yaml.v3"
)

// capabilityDriftProfileEnv is the coordinate the nightly conformance
// job's own environment names the tracked runtime profile through. It
// is owned by this package alongside SORTIE_CLIENTPROTOCOL_TEST and
// SORTIE_CLIENTPROTOCOL_COMMAND, behind the same one gate, so the
// one-gate-per-package rule stays unaffected.
const capabilityDriftProfileEnv = "SORTIE_CLIENTPROTOCOL_PROFILE"

const capabilityDriftCommandEnv = "SORTIE_CLIENTPROTOCOL_COMMAND"

// capabilityGapLabelSet is the four labels a session's capability-gap
// notice may report, in the record's own field order.
var capabilityGapLabelSet = []string{
	capabilityLabelToolServers, capabilityLabelTokenCounts,
	capabilityLabelSessionContinuation, capabilityLabelAgentVersion,
}

// liveCapabilityGapLabels reads the label set out of events' own
// notification stream: every domain.EventNotification message, joined
// in arrival order and separated, searched once per label. The
// separator is what keeps the reading honest: concatenating the
// messages directly lets one message's suffix and the next one's
// prefix spell a label no notification ever reported.
func liveCapabilityGapLabels(events []domain.AgentEvent) []string {
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
	sort.Strings(labels)
	return labels
}

// assertCapabilityGapLabelsMatchProfile reads the runtime profile named
// by SORTIE_CLIENTPROTOCOL_PROFILE and compares the live capability-gap
// label set events' notification stream carries against the profile's
// own capability_gap_labels, failing on a difference in either
// direction. It skips cleanly when the coordinate is absent, so an
// operator running the conformance suite as today is unaffected; a
// value naming a file it cannot read fails rather than skips, so a
// typo cannot pass green. It carries no Test prefix of its own by
// design: it is called from the event stream an existing conformance
// turn already collects, spending no turn of its own.
// resolveDriftProfilePath returns path unchanged when it is absolute,
// and otherwise resolves it against the repository root. go test runs
// with the package directory as its working directory, so a coordinate
// written relative to the repository, which is the form an operator
// and a CI job both reach for, would not otherwise resolve.
func resolveDriftProfilePath(t *testing.T, path string) string {
	t.Helper()
	if filepath.IsAbs(path) {
		return path
	}
	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root for %s=%q: %v", capabilityDriftProfileEnv, path, err)
	}
	return filepath.Join(root, path)
}

func assertCapabilityGapLabelsMatchProfile(t *testing.T, events []domain.AgentEvent) {
	t.Helper()

	profilePath, present := os.LookupEnv(capabilityDriftProfileEnv)
	if !present || strings.TrimSpace(profilePath) == "" {
		// Returning rather than skipping: this runs inside a caller whose
		// own assertions follow, and skipping would abort them too.
		t.Logf("capability-gap drift comparison not run: %s is unset", capabilityDriftProfileEnv)
		return
	}
	profile, err := qualification.ReadRuntimeProfileFile(resolveDriftProfilePath(t, profilePath))
	if err != nil {
		t.Fatalf("load the profile %s names, %q: %v", capabilityDriftProfileEnv, profilePath, err)
	}

	live := liveCapabilityGapLabels(events)
	want := slices.Clone(profile.CapabilityGapLabels)
	sort.Strings(want)

	if !slices.Equal(live, want) {
		t.Fatalf("live capability-gap labels %v differ from profile %s's capability_gap_labels %v", live, profilePath, want)
	}
}

// nightlyWorkflowStep is the subset of one workflow step's shape the
// reachability guard reads.
type nightlyWorkflowStep struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

// nightlyWorkflowJob is the subset of one workflow job's shape the
// reachability guard reads.
type nightlyWorkflowJob struct {
	Env      map[string]string `yaml:"env"`
	Strategy struct {
		Matrix struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []nightlyWorkflowStep `yaml:"steps"`
}

// nightlyWorkflowDocument is the subset of .github/workflows/nightly.yml
// the reachability guard reads.
type nightlyWorkflowDocument struct {
	Jobs map[string]nightlyWorkflowJob `yaml:"jobs"`
}

// nightlyWorkflowRelPath locates the nightly workflow file relative to
// this package directory, the shape a package-relative constant
// already establishes for a package-relative reader in this family.
const nightlyWorkflowRelPath = "../../../.github/workflows/nightly.yml"

// readNightlyWorkflow reads and decodes the nightly workflow document.
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

// nightlyMatrixRowsNamingThisPackage returns every matrix include entry,
// across every job, whose test_path names this package.
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

// nightlyGoTestStep returns the one step in steps whose run script
// invokes go test, or false when none does.
func nightlyGoTestStep(steps []nightlyWorkflowStep) (nightlyWorkflowStep, bool) {
	for _, step := range steps {
		if strings.Contains(step.Run, "go test") {
			return step, true
		}
	}
	return nightlyWorkflowStep{}, false
}

// nightlyRepositoryRoot resolves the repository root from this
// package's own directory.
func nightlyRepositoryRoot() (string, error) {
	abs, err := filepath.Abs(filepath.Join(".", nightlyWorkflowRelPath, "..", "..", ".."))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// callersOfAssertCapabilityGapLabels returns the name of every
// top-level Test function in this package directory whose body calls
// assertCapabilityGapLabelsMatchProfile.
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

// nightlyMatrixValue resolves value against row when it is a single
// "${{ matrix.<key> }}" expression, and returns it unchanged
// otherwise. A workflow that varies a coordinate per shard writes the
// expression rather than a literal, and a guard that could not read
// through it would pass vacuously on exactly the rows it exists to
// check.
func nightlyMatrixValue(row map[string]string, value string) string {
	trimmed := strings.TrimSpace(value)
	match := nightlyMatrixExpression.FindStringSubmatch(trimmed)
	if match == nil {
		return trimmed
	}
	return strings.TrimSpace(row[match[1]])
}

var nightlyMatrixExpression = regexp.MustCompile(`^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$`)

// TestNightlyReachability confirms the nightly workflow's own matrix
// rows and go-test steps actually reach and arm
// assertCapabilityGapLabelsMatchProfile, per the inputs the drift
// check's binding depends on: at least one row names this package;
// each such row names a distinct runtime command, so a duplicated
// shard is still caught; the profile coordinate carries a non-empty
// value in the environment that row's go-test step inherits (the union
// of the job-level and step-level env mappings, innermost winning,
// resolved through a matrix expression where the row supplies one);
// that value names a file that exists; exactly one top-level Test
// function in this package calls the comparison helper; and each row's
// run_filter, compiled as a Go regexp, matches that function's name. A
// coordinate declared where the step does not inherit it, a coordinate
// naming a file that does not exist, and an assertion the nightly's
// -run selector cannot reach each fail here rather than passing green
// and inert.
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
	if len(callers) != 1 {
		t.Fatalf("%d top-level Test functions in this package call assertCapabilityGapLabelsMatchProfile, want exactly 1: %v", len(callers), callers)
	}

	root, err := nightlyRepositoryRoot()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	commands := map[string]string{}
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
		if previous, duplicate := commands[command]; duplicate {
			t.Errorf("rows %q and %q both drive this package with %s %q, want one row per runtime", previous, name, capabilityDriftCommandEnv, command)
		}
		commands[command] = name

		value := nightlyMatrixValue(row.row, env[capabilityDriftProfileEnv])
		if value == "" {
			t.Errorf("row %q: no %s in the environment job %q's go-test step inherits, or it is empty", name, capabilityDriftProfileEnv, row.jobName)
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, value)); statErr != nil {
			t.Errorf("row %q: resolve the file %s names, %q: nothing exists under %s", name, capabilityDriftProfileEnv, value, root)
		}

		pattern, compileErr := regexp.Compile(row.row["run_filter"])
		if compileErr != nil {
			t.Errorf("row %q: compile run_filter %q as a Go regexp: %v", name, row.row["run_filter"], compileErr)
			continue
		}
		if !pattern.MatchString(callers[0]) {
			t.Errorf("row %q: run_filter %q does not match the calling test function %s", name, row.row["run_filter"], callers[0])
		}
	}
}

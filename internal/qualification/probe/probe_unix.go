//go:build unix

package probe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// setProcessGroup configures cmd to start in its own process group,
// inlined from internal/agent/procutil so this package's own import
// list stays confined to internal/qualification,
// internal/qualification/e2e, internal/agent/clientprotocol, and
// internal/domain. Must be called before [exec.Cmd.Start].
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to the entire process group led by pid,
// inlined for the same reason setProcessGroup is. It returns nil when
// the group no longer exists, since expiry is expected during
// best-effort cleanup.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// nativeProbeBound bounds every native surface launch.
const nativeProbeBound = 5 * time.Minute

// launchNativeProbe launches one native surface's bounded probe with
// the profile's own entry-point argv, captures its combined output
// through a line-bounded writer per stream, and drains its process
// group.
func launchNativeProbe(t *testing.T, commandPath string, argv []string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), commandPath, argv...) //nolint:gosec // the operator-selected executable with the profile's own documented flags
	setProcessGroup(cmd)

	stdout := &lineBoundedWriter{limit: 1 << 20}
	stderr := &lineBoundedWriter{limit: 1 << 20}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	capture := func() string {
		stdout.Flush()
		stderr.Flush()
		return stdout.String() + stderr.String()
	}
	if err := cmd.Start(); err != nil {
		return capture(), fmt.Errorf("native launch: %w: %w", errNativeLaunchFailed, err)
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		_ = signalProcessGroup(pgid, syscall.SIGKILL)
		qualification.AwaitProcessGroupAbsence(t, pgid)
		return capture(), waitErr
	case <-time.After(nativeProbeBound):
		_ = signalProcessGroup(pgid, syscall.SIGKILL)
		<-done
		qualification.AwaitProcessGroupAbsence(t, pgid)
		return capture(), fmt.Errorf("native probe: %w", errNativeBoundExceeded)
	}
}

// runAuthenticationCanary runs the profile's version_args once, before
// any graded surface spends a turn, so a missing or invalid credential
// fails the run before any other cost is spent.
func runAuthenticationCanary(t *testing.T, coords Coordinates) {
	t.Helper()
	// Through launchNativeProbe rather than its own exec: a runtime that
	// blocks on an interactive credential prompt while serving
	// version_args would otherwise hold the run until the whole go test
	// deadline expires, with no process group to drain it through.
	output, err := launchNativeProbe(t, coords.CommandPath, coords.Profile.VersionArgs)
	if err != nil {
		t.Fatalf("authentication canary %s %v failed: %v; output: %q",
			coords.CommandPath, coords.Profile.VersionArgs, err, strings.TrimSpace(output))
	}
}

// corroborateAbsentSurface confirms a declared-absent surface's own
// launch recognizes no terminal outcome, per the decoder-level rule
// that a declared absence is corroborated before any graded surface
// spends a turn.
func corroborateAbsentSurface(t *testing.T, coords Coordinates, surface qualification.Surface) {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(surface, coords.Model, "", "corroboration probe")
	if err != nil {
		t.Fatalf("build corroboration argv for %s: %v", surface, err)
	}
	output, launchErr := launchNativeProbe(t, coords.CommandPath, argv)
	_, _, found := nativeTerminal(coords.Profile, surface, output, launchErr)
	if found {
		t.Fatalf("declared-absent surface %s recognized a terminal outcome, contradicting the declaration", surface)
	}
}

// runPublishedPostureProbe launches the published sample's own
// posture, resolved to the profile's command path and model, into a
// fresh isolated workspace and confirms one turn completes with no
// permission request raised. It writes no qualification.Record and
// registers no process group with any tracker, so it cannot reach a
// graded row through either accumulator.
func runPublishedPostureProbe(t *testing.T, coords Coordinates) {
	t.Helper()

	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root for the published sample: %v", err)
	}
	sampleCommand, err := qualification.ReadPublishedSampleCommand(repositoryPath(t, root, coords.Profile.PublishedSample))
	if err != nil {
		t.Fatalf("read published sample command: %v", err)
	}
	argv, err := coords.Profile.PublishedPostureArgs(sampleCommand, coords.CommandPath, coords.Model)
	if err != nil {
		t.Fatalf("build the published-posture probe argv: %v", err)
	}

	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		t.Fatalf("construct the published-posture probe adapter: %v", err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig: domain.AgentConfig{
			Kind:           "agent-client-protocol",
			Command:        strings.Join(argv, " "),
			ReadTimeoutMS:  30000,
			TurnTimeoutMS:  300000,
			StallTimeoutMS: 60000,
		},
	})
	if err != nil {
		t.Fatalf("published-posture probe argv %v: launch the session: %v; a resolved command that is not the runtime the sample publishes fails here", argv, err)
	}
	t.Cleanup(func() {
		if err := adapter.StopSession(context.Background(), session); err != nil {
			t.Errorf("stop the published-posture probe session: %v", err)
		}
	})

	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "Reply with exactly SORTIE_PUBLISHED_POSTURE_OK and call no tool.",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("published-posture probe argv %v: run the turn: %v", argv, err)
	}
}

// defaultOutputDir returns a fresh run-scoped directory under the OS
// temporary root, refusing a path inside the repository tree.
func defaultOutputDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sortie-qualification-*")
	if err != nil {
		t.Fatalf("create the run-scoped output directory: %v", err)
	}
	if root, err := qualification.RepositoryRootFromWD(); err == nil {
		if pathWithin(dir, root) {
			// The guard fires after the directory exists and before any
			// caller can register its cleanup, so remove it here.
			_ = os.RemoveAll(dir)
			t.Fatalf("run-scoped output directory %s resolved inside the repository tree %s", dir, root)
		}
	}
	return dir
}

// pathWithin reports whether path is root itself or sits beneath it.
// A prefix test is not a containment test: it would also match a
// sibling whose name merely begins with root's.
func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// repositoryPath joins root and rel and confirms the result stays under
// root. A profile is operator-supplied data reached through an
// environment coordinate, so a rel carrying ".." would otherwise read
// outside the checkout.
func repositoryPath(t *testing.T, root, rel string) string {
	t.Helper()
	joined := filepath.Join(root, rel)
	if !pathWithin(joined, root) {
		t.Fatalf("profile path %q resolves to %s, outside the repository at %s", rel, joined, root)
	}
	return joined
}

// Run drives one live qualification collection against coords,
// corroborating every declared absence and writing the validated
// evidence, the bounded summary,
// and a fresh measurement artifact to Coordinates.OutputDir.
//
// The live tests built on Run MUST NOT call t.Parallel(): it launches
// real processes and spends model quota, so its output stays readable
// only while it runs alone.
func Run(t *testing.T, coords Coordinates) Result {
	t.Helper()

	outputDir := coords.OutputDir
	if outputDir == "" {
		outputDir = defaultOutputDir(t)
	}

	runAuthenticationCanary(t, coords)

	profile := coords.Profile
	for _, absent := range profile.AbsentSurfaces {
		corroborateAbsentSurface(t, coords, absent.Surface)
	}

	// The full live per-case induction catalog (cancellation, refusal,
	// oversize-input limit_reached, permission handling, tool-server
	// delivery, session continuation replay) is not reproduced here;
	// this collection scaffolds a schema-valid evidence set from the
	// declared and catalog-level state the profile itself carries, and
	// grades each measured surface's own launch classification. A
	// future pass wires the remaining live inducers without changing
	// this function's contract.
	fixture := qualification.NewFixture(qualification.FixtureUnmeasured, profile.AbsentSurfaces...)
	for _, declaration := range profile.Declarations {
		fixture.SetSemanticDeclaredGap(declaration.Capability, declaration.Case, declaration.Reason)
	}
	fixture.Finalize()

	verdict, err := qualification.ValidateObservationsWithDeclarations(qualification.WriteEvidenceFile(t, fixture.Records), profile)
	if err != nil {
		t.Fatalf("validate the collected evidence: %v", err)
	}

	conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
	if err != nil {
		t.Fatalf("derive the bounded summary: %v", err)
	}
	summary := FormatSummary(conclusions)

	evidencePath := filepath.Join(outputDir, "evidence.jsonl")
	if err := writeEvidenceRecords(evidencePath, fixture.Records); err != nil {
		t.Fatalf("write the evidence artifact: %v", err)
	}
	summaryPath := filepath.Join(outputDir, "summary.txt")
	if err := os.WriteFile(summaryPath, []byte(summary), 0o600); err != nil {
		t.Fatalf("write the summary artifact: %v", err)
	}

	measurement := qualification.Measurement{
		SchemaVersion: 1,
		ProfileDigest: profile.Digest(),
		MeasuredAt:    time.Now().UTC().Format("2006-01-02"),
		Expectation:   ExpectationFrom(conclusions),
	}
	measurementPath := filepath.Join(outputDir, "measurement.json")
	if err := writeMeasurement(measurementPath, measurement); err != nil {
		t.Fatalf("write the measurement artifact: %v", err)
	}

	t.Logf("qualification artifacts written: evidence=%s summary=%s measurement=%s", evidencePath, summaryPath, measurementPath)

	notesPath := repositoryPath(t, mustRepositoryRoot(t), profile.NotesPath)
	enforceNotesConsistency(t, notesPath, measurement.Expectation, summary)
	runPublishedPostureProbe(t, coords)

	return Result{
		Verdict:         verdict,
		Measurement:     measurement,
		EvidencePath:    evidencePath,
		SummaryPath:     summaryPath,
		MeasurementPath: measurementPath,
	}
}

// mustRepositoryRoot resolves the repository root or fails t.
func mustRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

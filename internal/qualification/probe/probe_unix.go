//go:build unix

package probe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// assertSessionGroupAbsent confirms the process group session's launch
// produced is gone, per the rule that a survivor is a leak rather than
// a slow exit. Every live session Run starts calls this after its own
// StopSession returns.
func assertSessionGroupAbsent(t *testing.T, session domain.Session) {
	t.Helper()
	pid, err := strconv.Atoi(session.AgentPID)
	if err != nil {
		t.Fatalf("session agent_pid %q did not parse as a process id: %v", session.AgentPID, err)
	}
	qualification.AwaitProcessGroupAbsence(t, pid)
}

// nativeProbeBound bounds every native surface launch.
const nativeProbeBound = 5 * time.Minute

// nativeDrainBound bounds what a launch waits once its process group has
// been taken down: the child's reap and the drain of the captured
// streams. A runtime that ignores a signal, or a descendant that
// inherited the streams and outlives its parent, is what this bound has
// to survive, so the wait carries its own bound.
const nativeDrainBound = 30 * time.Second

// boundedLaunch is one started native launch whose output is captured
// through pipes this package owns rather than the copier goroutines an
// [exec.Cmd] writing into an [io.Writer] starts. A descendant holding
// the inherited write end keeps those goroutines alive and
// [exec.Cmd.Wait] waits for them, so owning the pipes is what makes the
// wait end at all.
type boundedLaunch struct {
	pgid   int
	stdout *lineBoundedWriter
	stderr *lineBoundedWriter
	done   chan procutil.CaptureResult
	ended  chan struct{}
	cancel context.CancelFunc
}

// launchOwnershipInterval paces the process-table readings a live
// launch folds into the ownership ledger, about one query per
// launch-second: often enough that a launch long enough to serve a turn
// is read while it lives, rare enough to cost little.
const launchOwnershipInterval = time.Second

// startBoundedLaunch starts commandPath in its own process group,
// registers that group with owned, folds a process-table reading into
// owned while it runs, and charges owned with whatever the launch
// recorded on exit. A nil env inherits the calling process's
// environment, and a nil owned charges the launch to no ledger.
func startBoundedLaunch(commandPath string, argv []string, dir string, env []string, owned *ownedDescendants) (*boundedLaunch, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, commandPath, argv...) //nolint:gosec // the operator-selected executable with the profile's own documented flags
	cmd.Dir = dir
	cmd.Env = env
	procutil.SetGroupKill(cmd)

	// Before the process can exist, so a record read back later cannot
	// be one this run never wrote.
	if owned != nil {
		owned.watchReceipts(commandPath)
	}

	stdout := &lineBoundedWriter{limit: 1 << 20}
	stderr := &lineBoundedWriter{limit: 1 << 20}
	capture, err := procutil.StartCapture(cmd, procutil.CaptureParams{Stdout: stdout, Stderr: stderr, DrainGrace: nativeDrainBound})
	if err != nil {
		cancel()
		return nil, err
	}

	launch := &boundedLaunch{
		pgid:   cmd.Process.Pid,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan procutil.CaptureResult, 1),
		ended:  make(chan struct{}),
		cancel: cancel,
	}
	if owned != nil {
		owned.register(launch.pgid)
		go launch.observeOwnership(owned)
	}
	go func() {
		result := capture.Wait()
		// Before anything can see the launch as ended, so what it
		// recorded on exit is already in the ledger for every later
		// reading, with no ordering left to race.
		if owned != nil {
			owned.ingestReceipts()
		}
		close(launch.ended)
		launch.done <- result
	}()
	return launch, nil
}

// observeOwnership folds a process-table reading into owned until the
// launch ends. It reaches what no launch record can: a runtime killed
// outright never names what it started, so a reading taken while it ran
// is all that is left of it.
func (l *boundedLaunch) observeOwnership(owned *ownedDescendants) {
	ticker := time.NewTicker(launchOwnershipInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ended:
			return
		case <-ticker.C:
			if snapshot, err := psSnapshot(); err == nil {
				owned.observe(snapshot)
			}
		}
	}
}

// await blocks until the launch has ended and its streams have drained,
// or until bound passes, and reports which happened. The output is
// readable only when ended is true: until the capture's wait returns,
// the reader goroutine still owns the writers.
func (l *boundedLaunch) await(bound time.Duration) (result procutil.CaptureResult, output string, ended bool) {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case result = <-l.done:
		l.stdout.Flush()
		l.stderr.Flush()
		return result, l.stdout.String() + l.stderr.String(), true
	case <-timer.C:
		return procutil.CaptureResult{}, "", false
	}
}

// terminate takes the launch's process group down and awaits its end
// within the drain bound, so a launch that outlived its own bound still
// returns within one.
func (l *boundedLaunch) terminate() (result procutil.CaptureResult, output string, ended bool) {
	l.cancel()
	return l.await(nativeDrainBound)
}

// launchNativeProbe launches one native surface's bounded probe,
// captures its combined output, registers its process group with owned,
// and drains it.
func launchNativeProbe(t *testing.T, commandPath string, argv []string, dir string, env []string, owned *ownedDescendants) (string, error) {
	t.Helper()

	launch, err := startBoundedLaunch(commandPath, argv, dir, env, owned)
	if err != nil {
		return "", fmt.Errorf("native launch: %w: %w", errNativeLaunchFailed, err)
	}
	defer launch.cancel()

	result, output, ended := launch.await(nativeProbeBound)
	if !ended {
		_, output, ended = launch.terminate()
		if !ended {
			return "", fmt.Errorf("native probe: %w: the launch did not end within %s of its own group being taken down", errNativeBoundExceeded, nativeDrainBound)
		}
		qualification.AwaitProcessGroupAbsence(t, launch.pgid)
		return output, fmt.Errorf("native probe: %w", errNativeBoundExceeded)
	}
	qualification.AwaitProcessGroupAbsence(t, launch.pgid)
	return output, result.WaitErr
}

// runVersionCanary runs the profile's version_args once, before any
// graded surface spends a turn, so a runtime that cannot be launched
// under the collection's launch environment fails before any cost. It
// exercises no credential: a runtime prints its version whether or not
// anyone is authenticated.
func runVersionCanary(t *testing.T, coords Coordinates, fixture *sharedFixture) {
	t.Helper()
	// Through launchNativeProbe rather than its own exec: a runtime that
	// blocks on an interactive credential prompt while serving
	// version_args would otherwise hold the run until the go test
	// deadline, with no process group to drain it through.
	output, err := launchNativeProbe(t, coords.CommandPath, coords.Profile.VersionArgs, fixture.workspaceRoot, fixture.env, fixture.ownership())
	if err != nil {
		t.Fatalf("version canary %s %v failed: %v; output: %q",
			coords.CommandPath, coords.Profile.VersionArgs, err, strings.TrimSpace(output))
	}
}

// authenticationCanaryPrompt is the shortest turn that still needs a
// credential: a runtime answers it only once a provider accepts the
// request.
const authenticationCanaryPrompt = "Reply with exactly SORTIE_AUTH_CANARY_OK and call no tool."

// runAuthenticationCanary spends one turn against the protocol surface,
// under the launch environment every graded launch carries, and fails
// the run when it does not complete. Only a turn a provider answered
// reports that this collection's credential and configuration reach the
// runtime; running it first keeps a credential fault from being spent
// on, and graded as, twenty failed rows. It stops its session
// immediately so no later reading takes it for a graded launch's.
func runAuthenticationCanary(t *testing.T, coords Coordinates, fixture *sharedFixture) {
	t.Helper()

	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		t.Fatalf("build the authentication canary argv: %v", err)
	}
	adapter, session, err := startInductionSession(t, coords, argv, fixture.newLaunchWorkspace(t), "")
	if err != nil {
		t.Fatalf("authentication canary: the session did not start: %v", err)
	}
	_, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  authenticationCanaryPrompt,
		OnEvent: func(domain.AgentEvent) {},
	})
	if err := fixture.stopOpenSessions(context.Background()); err != nil {
		t.Fatalf("authentication canary: stop the canary session: %v", err)
	}
	if runErr != nil {
		t.Fatalf("authentication canary: the turn did not complete: %v; a version check passing here proves only that the executable runs", runErr)
	}
}

// corroborateAbsentSurface confirms a declared-absent surface's launch
// recognizes no terminal outcome, before any graded surface spends a
// turn.
func corroborateAbsentSurface(t *testing.T, coords Coordinates, fixture *sharedFixture, surface qualification.Surface) {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(surface, coords.Model, "", "corroboration probe")
	if err != nil {
		t.Fatalf("build corroboration argv for %s: %v", surface, err)
	}
	output, launchErr := launchNativeProbe(t, coords.CommandPath, argv, fixture.workspaceRoot, fixture.env, fixture.ownership())
	_, _, found := nativeTerminal(coords.Profile, surface, output, launchErr)
	if found {
		t.Fatalf("declared-absent surface %s recognized a terminal outcome, contradicting the declaration", surface)
	}
}

// runPublishedPostureProbe launches the published sample's own posture,
// resolved to the profile's command path and model, into a fresh
// isolated workspace and confirms one turn completes with no permission
// request raised. It writes no qualification.Record and registers no
// process group, so it cannot reach a graded row.
func runPublishedPostureProbe(t *testing.T, coords Coordinates, envWrapper string) {
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
			Command:        strings.Join(append([]string{envWrapper}, argv...), " "),
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
		assertSessionGroupAbsent(t, session)
	})

	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "Reply with exactly SORTIE_PUBLISHED_POSTURE_OK and call no tool.",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("published-posture probe argv %v: run the turn: %v", argv, err)
	}
}

const continuationInductionTurnBound = 3 * time.Minute

// awaitMinuteBoundary blocks until the wall clock leaves the UTC minute
// createdAt falls in, or a bounded deadline passes, per the protection
// this runtime family gives a session/load issued inside its own
// creation minute.
func awaitMinuteBoundary(createdAt time.Time) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().UTC().Truncate(time.Minute).Equal(createdAt.Truncate(time.Minute)) {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Second)
	}
}

// induceSessionContinuation drives one harness process throughout: a
// first session that leaves history, stopped, then a second session
// against the same workspace naming the first session's identifier as
// ResumeSessionID. resolveSession's own negative control fires
// automatically on this same path, before either continuation method
// it might attempt, so an unimplemented method and a broken one are
// already distinguished at the adapter's own error classification. The
// row is graded from what the replay did: a second session that
// returns the first session's own identifier confirms a replayed
// continuation; any other identifier is an unconfirmed fallback.
func induceSessionContinuation(t *testing.T, coords Coordinates) (qualification.Grade, string) {
	t.Helper()

	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("continuation induction could not resolve the protocol entry point: %v", err)
	}

	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("continuation induction could not construct the adapter: %v", err)
	}

	fullCommand := append([]string{coords.CommandPath}, argv...)
	launchConfig := domain.AgentConfig{
		Kind:           "agent-client-protocol",
		Command:        strings.Join(fullCommand, " "),
		ReadTimeoutMS:  30000,
		TurnTimeoutMS:  int(continuationInductionTurnBound / time.Millisecond),
		StallTimeoutMS: 60000,
	}

	workspace := t.TempDir()
	firstSession, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   launchConfig,
	})
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("continuation induction first session failed to start: %v", err)
	}
	createdAt := time.Now().UTC()

	_, runErr := adapter.RunTurn(context.Background(), firstSession, domain.RunTurnParams{
		Prompt:  "Reply with exactly one word: acknowledged.",
		OnEvent: func(domain.AgentEvent) {},
	})
	if stopErr := adapter.StopSession(context.Background(), firstSession); stopErr != nil {
		t.Errorf("stop the continuation induction's first session: %v", stopErr)
	}
	assertSessionGroupAbsent(t, firstSession)
	if runErr != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("continuation induction first turn did not complete: %v", runErr)
	}

	awaitMinuteBoundary(createdAt)

	secondSession, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     launchConfig,
		ResumeSessionID: firstSession.ID,
	})
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("continuation induction second session failed to start: %v", err)
	}
	t.Cleanup(func() {
		if err := adapter.StopSession(context.Background(), secondSession); err != nil {
			t.Errorf("stop the continuation induction's second session: %v", err)
		}
		assertSessionGroupAbsent(t, secondSession)
	})

	if secondSession.ID == firstSession.ID {
		return qualification.GradeUsable, "the second session returned the first session's own identifier, confirming a replayed continuation"
	}
	return qualification.GradeGap, "the second session returned a fresh identifier; continuation was not confirmed and the run fell back"
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

// pathWithin reports whether path is root itself or sits beneath it,
// comparing the two only after resolving both through their symlinks. A
// prefix test is not a containment test, since it matches a sibling
// whose name merely begins with root's, and a textual check on
// unresolved paths passes a symlink inside the checkout that points
// outside it. A side that cannot be resolved is reported as not
// contained rather than assumed safe.
func pathWithin(path, root string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
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
	if _, err := os.Lstat(joined); err != nil {
		// A path that does not exist is the reader's own error to report.
		return joined
	}
	if !pathWithin(joined, root) {
		t.Fatalf("profile path %q resolves outside the repository at %s", rel, root)
	}
	return joined
}

// Run drives one live qualification collection against coords,
// corroborating every declared absence and writing the validated
// evidence, the bounded summary, and a fresh measurement artifact to
// Coordinates.OutputDir.
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

	profile := coords.Profile
	fixtureState := resolveSharedFixture(t, coords)

	runVersionCanary(t, coords, fixtureState)
	runAuthenticationCanary(t, coords, fixtureState)

	for _, absent := range profile.AbsentSurfaces {
		corroborateAbsentSurface(t, coords, fixtureState, absent.Surface)
	}

	toolGrade, toolDetail := induceToolServerCall(t, coords, fixtureState)
	permissionGrade, permissionDetail := inducePermissionRequest(t, coords, fixtureState)
	continuationGrade, continuationDetail := induceSessionContinuation(t, coords)

	fixture, err := gradedEvidence(profile,
		inducedRow{grade: toolGrade, detail: toolDetail},
		inducedRow{grade: permissionGrade, detail: permissionDetail},
		inducedRow{grade: continuationGrade, detail: continuationDetail},
	)
	if err != nil {
		t.Fatalf("compose the collected evidence: %v", err)
	}

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
	runPublishedPostureProbe(t, coords, fixtureState.envWrapper)

	return Result{
		Verdict:         verdict,
		Measurement:     measurement,
		EvidencePath:    evidencePath,
		SummaryPath:     summaryPath,
		MeasurementPath: measurementPath,
	}
}

func mustRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

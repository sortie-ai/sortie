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
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// signalProcessGroup sends sig to the process group led by pid. It
// reports nil when the group no longer exists, since expiry is expected
// during best-effort cleanup.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	return procutil.SignalProcessGroup(pid, sig)
}

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

// nativeSignalBound bounds what a launch waits after one signal. A
// runtime that honours it ends well inside; one that ignores it has its
// group taken down rather than being waited on indefinitely.
const nativeSignalBound = time.Minute

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
	if _, err := fixture.stopOpenSessions(context.Background()); err != nil {
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

// induceIfInducible stores induce's result at caseID in byCase, unless
// the profile declares the (surface, caseID) pair not inducible. A
// declared pair grades from that declaration and reads no observation,
// so this skips the launch rather than pay its bounded wait.
func induceIfInducible(coords Coordinates, fixture *sharedFixture, byCase map[qualification.Case]qualification.Observation, surface qualification.Surface, caseID qualification.Case, induce func() qualification.Observation) {
	if _, notInducible := coords.Profile.NotInducibleDeclared(surface, caseID); notInducible {
		return
	}
	record(fixture, byCase, surface, caseID, induce())
}

// record stores obs at caseID in byCase and appends it to the run's
// journal, so an observation reaches disk when it is obtained rather
// than only once every launch has finished.
func record(fixture *sharedFixture, byCase map[qualification.Case]qualification.Observation, surface qualification.Surface, caseID qualification.Case, obs qualification.Observation) {
	byCase[caseID] = obs
	fixture.journal.append(string(surface), string(caseID), obs)
}

// induceProtocolSemantics drives every semantic case the protocol
// surface measures, plus the tool-server, permission, policy, and
// human_input induction, recording the results into collected.
func induceProtocolSemantics(t *testing.T, coords Coordinates, fixture *sharedFixture, collected *collectedObservations) {
	t.Helper()
	surface := qualification.SurfaceProtocol
	byCase := map[qualification.Case]qualification.Observation{}

	record(fixture, byCase, surface, qualification.CaseSuccess, induceSuccess(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseRuntimeFailure, func() qualification.Observation {
		return induceRuntimeFailure(t, coords, fixture, surface)
	})
	disposition, retry := induceRefusalPair(t, coords, fixture, surface)
	record(fixture, byCase, surface, qualification.CaseRuntimeRefusal, disposition)
	record(fixture, byCase, surface, qualification.CaseNonRetryableRefusal, retry)
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseCancellation, func() qualification.Observation {
		return induceCancellation(t, coords, fixture, surface)
	})
	record(fixture, byCase, surface, qualification.CaseRetryableTransport, induceRetryableTransport(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseLimitReached, func() qualification.Observation {
		return induceProtocolLimit(t, coords, fixture)
	})

	permission, policy, humanInput := inducePermissionPolicyHumanInput(t, coords, fixture)
	// This grouped launch always runs: permission and policy carry no
	// not-inducible declaration and read it, so a not-inducible
	// human_input declaration must not suppress it. Only the human_input
	// row it produced is withheld through the guard.
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseHumanInput, func() qualification.Observation {
		return humanInput
	})
	collected.toolServer = induceToolServer(t, coords, fixture)
	collected.permission = permission
	collected.policy = policy
	fixture.journal.append(string(surface), "tool_server", collected.toolServer)
	fixture.journal.append(string(surface), "permission", permission)
	fixture.journal.append(string(surface), "policy_precondition", policy)

	collected.semantic[surface] = byCase

	seed, recall := induceProtocolContinuation(t, coords, fixture)
	collected.continuationSeed[surface] = seed
	collected.continuationRecall[surface] = recall
	fixture.journal.append(string(surface), "continuation_seed", seed)
	fixture.journal.append(string(surface), "continuation_recall", recall)

	sessionID, paths, inventory, extension := protocolTokenInventory(fixture)
	collected.tokenSessionID[surface] = sessionID
	collected.tokenPaths[surface] = paths
	collected.tokenInventory[surface] = inventory
	collected.tokenExtension = extension
	collected.tokenCompensation = protocolTokenCompensation(fixture)
	fixture.journal.append(string(surface), "token_inventory", inventory)
}

// induceNativeSemantics drives every semantic case, the continuation
// pair, and the token inventory surface measures on one structured
// native surface.
func induceNativeSemantics(t *testing.T, coords Coordinates, fixture *sharedFixture, collected *collectedObservations, surface qualification.Surface) {
	t.Helper()
	byCase := map[qualification.Case]qualification.Observation{}

	record(fixture, byCase, surface, qualification.CaseSuccess, induceSuccess(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseRuntimeFailure, func() qualification.Observation {
		return induceRuntimeFailure(t, coords, fixture, surface)
	})
	disposition, retry := induceRefusalPair(t, coords, fixture, surface)
	record(fixture, byCase, surface, qualification.CaseRuntimeRefusal, disposition)
	record(fixture, byCase, surface, qualification.CaseNonRetryableRefusal, retry)
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseCancellation, func() qualification.Observation {
		return induceCancellation(t, coords, fixture, surface)
	})
	record(fixture, byCase, surface, qualification.CaseRetryableTransport, induceRetryableTransport(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, qualification.CaseHumanInput, func() qualification.Observation {
		return induceHumanInputNative(t, coords, fixture, surface)
	})

	collected.semantic[surface] = byCase

	seed, recall := induceNativeContinuation(t, coords, fixture, surface)
	collected.continuationSeed[surface] = seed
	collected.continuationRecall[surface] = recall
	fixture.journal.append(string(surface), "continuation_seed", seed)
	fixture.journal.append(string(surface), "continuation_recall", recall)

	sessionID, paths, inventory := nativeTokenInventory(t, coords, surface, fixture.nativeOutputsFor(surface))
	collected.tokenSessionID[surface] = sessionID
	collected.tokenPaths[surface] = paths
	collected.tokenInventory[surface] = inventory
	fixture.journal.append(string(surface), "token_inventory", inventory)
}

// checkNoBarePair rejects a composed record set carrying the bare
// (not_observed, not_observed) pair: every row that records nothing must
// state why, through declared_gap, not_inducible, or a not_observed
// failure outcome.
func checkNoBarePair(records []qualification.Record) error {
	for _, rec := range records {
		if rec.Grade == qualification.GradeNotObserved && rec.Outcome == qualification.OutcomeNotObserved {
			return fmt.Errorf("record %d (%s/%s/%s) carries the bare not_observed pair; every inducer must state a specific failure outcome or a declaration", rec.Sequence, rec.Scenario, rec.Surface, rec.Capability)
		}
	}
	return nil
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

	collectionStartedAt := time.Now().UTC()

	outputDir := coords.OutputDir
	if outputDir == "" {
		outputDir = defaultOutputDir(t)
	}

	profile := coords.Profile
	fixtureState := resolveSharedFixture(t, coords)
	fixtureState.journal = &observationJournal{path: filepath.Join(outputDir, "observations.jsonl")}

	runVersionCanary(t, coords, fixtureState)
	runAuthenticationCanary(t, coords, fixtureState)

	for _, absent := range profile.AbsentSurfaces {
		corroborateAbsentSurface(t, coords, fixtureState, absent.Surface)
	}

	collected := &collectedObservations{
		semantic:           map[qualification.Surface]map[qualification.Case]qualification.Observation{},
		continuationSeed:   map[qualification.Surface]qualification.Observation{},
		continuationRecall: map[qualification.Surface]qualification.Observation{},
		tokenSessionID:     map[qualification.Surface]string{},
		tokenPaths:         map[qualification.Surface][]qualification.TokenObservation{},
		tokenInventory:     map[qualification.Surface]qualification.Observation{},
	}

	induceProtocolSemantics(t, coords, fixtureState, collected)
	for _, surface := range profile.MeasuredSurfaces() {
		if surface == qualification.SurfaceProtocol {
			continue
		}
		induceNativeSemantics(t, coords, fixtureState, collected, surface)
	}

	identityObs, identities := induceRuntimeIdentity(fixtureState)
	collected.identityObs = identityObs
	collected.identities = identities
	collected.identityProtocolVersion = pinnedProtocolVersionMirror

	// The harness record's agent fields never reach the evidence: the
	// end-to-end row is written from the observation alone and identified
	// from its own session's handshake, so naming another session's agent
	// here would attribute a reading it never made.
	collected.endToEnd = induceEndToEnd(t, coords, fixtureState, "", "")

	collected.workspaceSecurity = induceWorkspaceSecurity(t, coords, fixtureState)
	collected.processCleanup = induceProcessCleanup(fixtureState)
	fixtureState.journal.append("run", "runtime_identity", identityObs)
	fixtureState.journal.append("run", "end_to_end", endToEndObservation(collected.endToEnd))
	fixtureState.journal.append("run", "workspace_security", collected.workspaceSecurity)
	fixtureState.journal.append("run", "process_cleanup", collected.processCleanup)

	fixture, err := gradedEvidence(profile, *collected, collectionStartedAt)
	if err != nil {
		t.Fatalf("compose the collected evidence: %v", err)
	}
	// A paid collection that fails a later check still leaves the rows it
	// drove, so the failure is readable without re-spending.
	evidencePath := filepath.Join(outputDir, "evidence.jsonl")
	if err := writeEvidenceRecords(evidencePath, fixture.Records); err != nil {
		t.Fatalf("write the evidence artifact: %v", err)
	}
	// A run that could not keep the journal for the whole collection says
	// so here, after the evidence is safely written.
	if err := fixtureState.journal.err(); err != nil {
		t.Errorf("the observation journal was not kept for the whole collection: %v", err)
	}
	unrecognizedPath := filepath.Join(outputDir, "unrecognized.jsonl")
	if err := writeUnrecognizedTerminals(unrecognizedPath, fixtureState.unrecognizedTerminals()); err != nil {
		t.Fatalf("write the unrecognized terminal artifact: %v", err)
	}

	if err := checkNoBarePair(fixture.Records); err != nil {
		t.Fatalf("%v", err)
	}

	verdict, err := qualification.ValidateObservationsWithDeclarations(evidencePath, profile)
	if err != nil {
		t.Fatalf("validate the collected evidence: %v", err)
	}

	conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
	if err != nil {
		t.Fatalf("derive the bounded summary: %v", err)
	}
	summary := FormatSummary(conclusions)

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

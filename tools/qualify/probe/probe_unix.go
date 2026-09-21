//go:build unix

package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/tools/qualify/eval"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// signalProcessGroup sends sig to the process group led by pid. It
// reports nil when the group no longer exists, since expiry is expected
// during best-effort cleanup.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	return procutil.SignalProcessGroup(pid, sig)
}

// assertSessionGroupAbsent confirms the process group session's launch
// produced is gone: a survivor is a leak, not a slow exit. Every live
// session calls this after its own StopSession returns.
func assertSessionGroupAbsent(t *testing.T, session domain.Session) {
	t.Helper()
	pid, err := strconv.Atoi(session.AgentPID)
	if err != nil {
		t.Fatalf("session agent_pid %q did not parse as a process id: %v", session.AgentPID, err)
	}
	procgroup.AwaitAbsence(t, pid)
}

// nativeProbeBound bounds every native surface launch.
const nativeProbeBound = 5 * time.Minute

// nativeDrainBound bounds the wait after a process group is taken down,
// for the reap and the stream drain, surviving a runtime that ignores a
// signal or a descendant holding the streams open.
const nativeDrainBound = 30 * time.Second

// nativeSignalBound bounds what a launch waits after one signal. A
// runtime that honours it ends well inside; one that ignores it has its
// group taken down rather than being waited on indefinitely.
const nativeSignalBound = time.Minute

// boundedLaunch captures output through pipes this package owns, not
// [exec.Cmd]'s own copier goroutines: a descendant holding the
// inherited write end would keep those alive and [exec.Cmd.Wait]
// waiting forever.
type boundedLaunch struct {
	pgid   int
	stdout *lineBoundedWriter
	stderr *lineBoundedWriter
	done   chan procutil.CaptureResult
	ended  chan struct{}
	cancel context.CancelFunc
}

// launchOwnershipInterval paces ownership-ledger reads to about one per
// launch-second: often enough to catch a turn-length launch alive, rare
// enough to cost little.
const launchOwnershipInterval = time.Second

// startBoundedLaunch accepts a nil env, which inherits the caller's
// environment, needed by the version canary and absent-surface
// corroboration before the shared fixture resolves one.
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

// observeOwnership folds process-table readings into owned until the
// launch ends: a runtime killed outright never names what it started,
// so a reading taken while it ran is all that's left of it.
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

// await blocks until the launch ends and its streams drain, or bound
// passes. Output is readable only when ended is true: until the
// capture's wait returns, the reader goroutine still owns the writers.
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
		procgroup.AwaitAbsence(t, launch.pgid)
		return output, fmt.Errorf("native probe: %w", errNativeBoundExceeded)
	}
	procgroup.AwaitAbsence(t, launch.pgid)
	return output, result.WaitErr
}

// runVersionCanary runs version_args once before any graded surface
// spends a turn, so a launch failure costs nothing. It exercises no
// credential: a runtime prints its version unauthenticated.
func runVersionCanary(t *testing.T, coords Coordinates, fixture *sharedFixture) {
	t.Helper()
	// Through launchNativeProbe, not a bare exec: a runtime blocking on
	// an interactive credential prompt would otherwise hold the run to
	// the go test deadline, with no process group to drain it through.
	output, err := launchNativeProbe(t, coords.CommandPath, coords.Profile.VersionArgs, fixture.workspaceRoot, fixture.env, fixture.ownership())
	if err != nil {
		t.Fatalf("version canary %s %v failed: %v; output: %q",
			coords.CommandPath, coords.Profile.VersionArgs, err, strings.TrimSpace(output))
	}
}

// runAuthenticationCanary fails the run if the turn doesn't complete,
// so a credential fault costs one turn rather than twenty failed rows.
// It stops its session immediately so no later reading mistakes it for
// a graded launch.
func runAuthenticationCanary(t *testing.T, coords Coordinates, fixture *sharedFixture) {
	t.Helper()

	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		t.Fatalf("build the authentication canary argv: %v", err)
	}
	adapter, session, err := startInductionSession(t, coords, argv, fixture.newLaunchWorkspace(t), "")
	if err != nil {
		t.Fatalf("authentication canary: the session did not start: %v", err)
	}
	_, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  agentcore.CredentialVerificationPrompt,
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
// recognizes no terminal outcome. It grades no row, so it asks
// eval.RecognizedTerminal rather than eval.Recognize.
func corroborateAbsentSurface(t *testing.T, coords Coordinates, fixture *sharedFixture, surface evidence.Surface) {
	t.Helper()
	argv, err := coords.Profile.EntryArgs(surface, coords.Model, "", "corroboration probe")
	if err != nil {
		t.Fatalf("build corroboration argv for %s: %v", surface, err)
	}
	output, launchErr := launchNativeProbe(t, coords.CommandPath, argv, fixture.workspaceRoot, fixture.env, fixture.ownership())
	launch := evidence.LaunchRecord{Argv: argv, AuthEnvNames: coords.AuthEnvNames, Model: coords.Model, Outcome: launchOutcomeFor(launchErr)}
	streams := boundStreamCapture(output)
	if eval.RecognizedTerminal(coords.Profile, surface, launch, streams) {
		t.Fatalf("declared-absent surface %s recognized a terminal outcome, contradicting the declaration", surface)
	}
}

// runPublishedPostureProbe launches the published sample's own posture
// and confirms one turn completes with no permission request raised. It
// writes no evidence.Record, so it can't reach a graded row.
func runPublishedPostureProbe(t *testing.T, coords Coordinates, envWrapper string) {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("resolve checkout root for the published sample: %v", err)
	}
	sampleCommand, err := profile.ReadPublishedSampleCommand(repositoryPath(t, root, coords.Profile.PublishedSample))
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

// awaitMinuteBoundary blocks until the wall clock leaves createdAt's
// UTC minute, or a bounded deadline passes: this runtime family
// protects a session/load issued within its own creation minute.
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
	if root, err := profile.CheckoutRoot(); err == nil {
		if pathWithin(dir, root) {
			// The guard fires after the directory exists and before any
			// caller can register its cleanup, so remove it here.
			_ = os.RemoveAll(dir)
			t.Fatalf("run-scoped output directory %s resolved inside the repository tree %s", dir, root)
		}
	}
	return dir
}

// pathWithin resolves both path and root through symlinks before
// comparing, since a prefix test would admit a sibling whose name
// merely begins with root's, or a symlink escaping it. An unresolved
// side reports not-contained rather than assumed safe.
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
// root: rel comes from operator-supplied profile data, so a ".." entry
// would otherwise read outside the checkout.
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
// the profile declares the pair not inducible, skipping the launch
// rather than paying its bounded wait.
func induceIfInducible(coords Coordinates, fixture *sharedFixture, byCase map[evidence.Case]evidence.Observation, surface evidence.Surface, caseID evidence.Case, induce func() gradedObservation) {
	if _, notInducible := coords.Profile.NotInducibleDeclared(surface, caseID); notInducible {
		return
	}
	record(fixture, byCase, surface, caseID, induce())
}

// record stores g's observation at caseID in byCase and appends g to
// the run's journal, so an observation reaches disk when it is obtained
// rather than only once every launch has finished.
func record(fixture *sharedFixture, byCase map[evidence.Case]evidence.Observation, surface evidence.Surface, caseID evidence.Case, g gradedObservation) {
	byCase[caseID] = g.obs
	fixture.journal.append(string(surface), string(caseID), g)
}

// induceProtocolSemantics drives every semantic case the protocol
// surface measures, plus the tool-server, permission, policy, and
// human_input induction, recording the results into collected.
func induceProtocolSemantics(t *testing.T, coords Coordinates, fixture *sharedFixture, collected *collectedObservations) {
	t.Helper()
	surface := evidence.SurfaceProtocol
	byCase := map[evidence.Case]evidence.Observation{}

	record(fixture, byCase, surface, evidence.CaseSuccess, induceSuccess(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseRuntimeFailure, func() gradedObservation {
		return induceRuntimeFailure(t, coords, fixture, surface)
	})
	disposition, retry := induceRefusalPair(t, coords, fixture, surface)
	record(fixture, byCase, surface, evidence.CaseRuntimeRefusal, disposition)
	record(fixture, byCase, surface, evidence.CaseNonRetryableRefusal, retry)
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseCancellation, func() gradedObservation {
		return induceCancellation(t, coords, fixture, surface)
	})
	record(fixture, byCase, surface, evidence.CaseRetryableTransport, induceRetryableTransport(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseLimitReached, func() gradedObservation {
		return induceProtocolLimit(t, coords, fixture)
	})

	permission, policy, humanInput := inducePermissionPolicyHumanInput(t, coords, fixture)
	// This grouped launch always runs: permission and policy read it
	// regardless, so a not-inducible human_input declaration only
	// withholds the human_input row, not the launch.
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseHumanInput, func() gradedObservation {
		return humanInput
	})
	toolServer := induceToolServer(t, coords, fixture)
	collected.toolServer = toolServer.obs
	collected.permission = permission.obs
	collected.policy = policy.obs
	fixture.journal.append(string(surface), "tool_server", toolServer)
	fixture.journal.append(string(surface), "permission", permission)
	fixture.journal.append(string(surface), "policy_precondition", policy)

	collected.semantic[surface] = byCase

	seed, recall := induceProtocolContinuation(t, coords, fixture)
	collected.continuationSeed[surface] = seed.obs
	collected.continuationRecall[surface] = recall.obs
	fixture.journal.append(string(surface), "continuation_seed", seed)
	fixture.journal.append(string(surface), "continuation_recall", recall)

	sessionID, paths, inventory, extension := protocolTokenInventory(fixture)
	collected.tokenSessionID[surface] = sessionID
	collected.tokenPaths[surface] = paths
	collected.tokenInventory[surface] = inventory.obs
	collected.tokenExtension = extension
	collected.tokenCompensation = protocolTokenCompensation(fixture)
	fixture.journal.append(string(surface), "token_inventory", inventory)
}

// induceNativeSemantics drives every semantic case, the continuation
// pair, and the token inventory surface measures on one structured
// native surface.
func induceNativeSemantics(t *testing.T, coords Coordinates, fixture *sharedFixture, collected *collectedObservations, surface evidence.Surface) {
	t.Helper()
	byCase := map[evidence.Case]evidence.Observation{}

	record(fixture, byCase, surface, evidence.CaseSuccess, induceSuccess(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseRuntimeFailure, func() gradedObservation {
		return induceRuntimeFailure(t, coords, fixture, surface)
	})
	disposition, retry := induceRefusalPair(t, coords, fixture, surface)
	record(fixture, byCase, surface, evidence.CaseRuntimeRefusal, disposition)
	record(fixture, byCase, surface, evidence.CaseNonRetryableRefusal, retry)
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseCancellation, func() gradedObservation {
		return induceCancellation(t, coords, fixture, surface)
	})
	record(fixture, byCase, surface, evidence.CaseRetryableTransport, induceRetryableTransport(t, coords, fixture, surface))
	induceIfInducible(coords, fixture, byCase, surface, evidence.CaseHumanInput, func() gradedObservation {
		return induceHumanInputNative(t, coords, fixture, surface)
	})

	collected.semantic[surface] = byCase

	seed, recall := induceNativeContinuation(t, coords, fixture, surface)
	collected.continuationSeed[surface] = seed.obs
	collected.continuationRecall[surface] = recall.obs
	fixture.journal.append(string(surface), "continuation_seed", seed)
	fixture.journal.append(string(surface), "continuation_recall", recall)

	sessionID, paths, inventory := nativeTokenInventory(coords.Profile, surface, fixture.nativeOutputsFor(surface))
	collected.tokenSessionID[surface] = sessionID
	collected.tokenPaths[surface] = paths
	collected.tokenInventory[surface] = inventory.obs
	fixture.journal.append(string(surface), "token_inventory", inventory)
}

// checkNoBarePair rejects a bare (not_observed, not_observed) row:
// every row recording nothing must state why, via declared_gap,
// not_inducible, or a not_observed failure outcome.
func checkNoBarePair(records []evidence.Record) error {
	for _, rec := range records {
		if rec.Grade == evidence.GradeNotObserved && rec.Outcome == evidence.OutcomeNotObserved {
			return fmt.Errorf("record %d (%s/%s/%s) carries the bare not_observed pair; every inducer must state a specific failure outcome or a declaration", rec.Sequence, rec.Scenario, rec.Surface, rec.Capability)
		}
	}
	return nil
}

// Run drives one live qualification collection against coords, writing
// evidence, summary, and measurement artifacts to Coordinates.OutputDir.
//
// Tests built on Run MUST NOT call t.Parallel(): it launches real
// processes and spends model quota, so its output stays readable only
// while it runs alone.
func Run(t *testing.T, coords Coordinates) Result {
	t.Helper()

	collectionStartedAt := time.Now().UTC()

	outputDir := coords.OutputDir
	if outputDir == "" {
		outputDir = defaultOutputDir(t)
	}

	p := coords.Profile
	fixtureState := resolveSharedFixture(t, coords)
	fixtureState.journal = &observationJournal{path: filepath.Join(outputDir, "observations.jsonl")}

	runVersionCanary(t, coords, fixtureState)
	runAuthenticationCanary(t, coords, fixtureState)

	for _, absent := range p.AbsentSurfaces {
		corroborateAbsentSurface(t, coords, fixtureState, absent.Surface)
	}

	collected := &collectedObservations{
		semantic:           map[evidence.Surface]map[evidence.Case]evidence.Observation{},
		continuationSeed:   map[evidence.Surface]evidence.Observation{},
		continuationRecall: map[evidence.Surface]evidence.Observation{},
		tokenSessionID:     map[evidence.Surface]string{},
		tokenPaths:         map[evidence.Surface][]evidence.TokenObservation{},
		tokenInventory:     map[evidence.Surface]evidence.Observation{},
	}

	induceProtocolSemantics(t, coords, fixtureState, collected)
	for _, surface := range p.MeasuredSurfaces() {
		if surface == evidence.SurfaceProtocol {
			continue
		}
		induceNativeSemantics(t, coords, fixtureState, collected, surface)
	}

	identityObs, identities := induceRuntimeIdentity(fixtureState)
	collected.identityObs = identityObs
	collected.identities = identities
	collected.identityProtocolVersion = pinnedProtocolVersionMirror

	// The harness record's agent fields never reach the evidence: the
	// row is identified from its own session's handshake, so naming
	// another session's agent here would attribute a reading it never made.
	collected.endToEnd = induceEndToEnd(t, coords, fixtureState, "", "")

	collected.workspaceSecurity = induceWorkspaceSecurity(t, coords, fixtureState)
	collected.processCleanup = induceProcessCleanup(fixtureState)
	fixtureState.journal.append("run", "runtime_identity", transportGraded(identityObs))
	fixtureState.journal.append("run", "end_to_end", transportGraded(endToEndObservation(collected.endToEnd)))
	fixtureState.journal.append("run", "workspace_security", transportGraded(collected.workspaceSecurity))
	fixtureState.journal.append("run", "process_cleanup", transportGraded(collected.processCleanup))

	records, err := composeLive(p, *collected, collectionStartedAt)
	if err != nil {
		t.Fatalf("compose the collected evidence: %v", err)
	}
	if err := checkNoBarePair(records); err != nil {
		t.Fatalf("%v", err)
	}

	// A paid collection that fails a later check still leaves the rows it
	// drove, so the failure is readable without re-spending. Nothing past
	// this point may delete or truncate what these three writes produce.
	evidencePath := filepath.Join(outputDir, "evidence.jsonl")
	if err := writeEvidenceRecords(evidencePath, records); err != nil {
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

	provenancePath := filepath.Join(outputDir, "provenance.json")
	if err := writeProvenance(provenancePath, collectionProvenanceOf(t, coords, collectionStartedAt, evidencePath, identities)); err != nil {
		t.Fatalf("write the provenance artifact: %v", err)
	}
	provenanceDigest, err := fileDigest(provenancePath)
	if err != nil {
		t.Fatalf("digest the provenance artifact: %v", err)
	}

	// Graded from the bytes this run just published, through the same
	// derivation an offline re-run of this directory would reach, so the
	// live path and the offline path share one derivation.
	result, err := eval.Run(eval.Input{CaptureDir: outputDir, Profile: p})
	if err != nil {
		t.Fatalf("evaluate the written capture: %v", err)
	}

	measuredAt := collectionStartedAt.Format("2006-01-02")
	summary, measurement, err := eval.Render(result, p, measuredAt, coords.Model, provenanceDigest)
	if err != nil {
		t.Fatalf("render the summary and measurement: %v", err)
	}

	summaryPath := filepath.Join(outputDir, "summary.txt")
	if err := os.WriteFile(summaryPath, []byte(summary), 0o600); err != nil {
		t.Fatalf("write the summary artifact: %v", err)
	}
	measurementPath := filepath.Join(outputDir, "measurement.json")
	if err := writeMeasurement(measurementPath, measurement); err != nil {
		t.Fatalf("write the measurement artifact: %v", err)
	}

	t.Logf("qualification artifacts written: evidence=%s summary=%s measurement=%s provenance=%s", evidencePath, summaryPath, measurementPath, provenancePath)

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("resolve checkout root for the notes document: %v", err)
	}
	notesPath := repositoryPath(t, root, p.NotesPath)
	enforceNotesConsistency(t, notesPath, measurement.Expectation, summary)
	runPublishedPostureProbe(t, coords, fixtureState.envWrapper)

	return Result{
		Verdict:         result.Parity,
		Measurement:     measurement,
		EvidencePath:    evidencePath,
		SummaryPath:     summaryPath,
		MeasurementPath: measurementPath,
		ProvenancePath:  provenancePath,
	}
}

// collectorSourceTree's digest tells a rerun under changed collector
// logic apart from a replay. It names the whole tool module: over-
// reporting a collector change is safe, missing one is not.
const collectorSourceTree = "tools/qualify"

type provenanceAgent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// collectionProvenance exists because a measurement alone states only
// a profile digest and date, which two different collector builds
// share.
type collectionProvenance struct {
	SchemaVersion   int                        `json:"schema_version"`
	ObservedAt      string                     `json:"observed_at"`
	ProfileDigest   string                     `json:"profile_digest"`
	CollectorDigest string                     `json:"collector_digest"`
	EvidenceFile    string                     `json:"evidence_file"`
	EvidenceDigest  string                     `json:"evidence_digest"`
	RequestedModel  string                     `json:"requested_model"`
	ProtocolVersion int                        `json:"protocol_version"`
	SessionAgents   map[string]provenanceAgent `json:"session_agents"`
	CredentialMode  map[string]string          `json:"credential_mode"`
}

// collectionProvenanceOf assembles the provenance of one run from the
// coordinates it measured, the moment it began, the evidence it
// published, and the handshakes it observed.
func collectionProvenanceOf(t *testing.T, coords Coordinates, startedAt time.Time, evidencePath string, identities map[string]evidence.SessionIdentity) collectionProvenance {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("resolve checkout root for the collector digest: %v", err)
	}
	collector, err := collectorDigest(filepath.Join(root, collectorSourceTree))
	if err != nil {
		t.Fatalf("digest the collector sources: %v", err)
	}
	evidenceDigest, err := fileDigest(evidencePath)
	if err != nil {
		t.Fatalf("digest the evidence artifact: %v", err)
	}

	agents := make(map[string]provenanceAgent, len(identities))
	for sessionID, identity := range identities {
		agents[sessionID] = provenanceAgent{Name: identity.Name, Version: identity.Version}
	}

	return collectionProvenance{
		SchemaVersion:   1,
		ObservedAt:      startedAt.UTC().Format(time.RFC3339),
		ProfileDigest:   coords.Profile.Digest(),
		CollectorDigest: collector,
		EvidenceFile:    filepath.Base(evidencePath),
		EvidenceDigest:  evidenceDigest,
		RequestedModel:  coords.Model,
		ProtocolVersion: pinnedProtocolVersionMirror,
		SessionAgents:   agents,
		CredentialMode:  credentialModes(coords.AuthEnvNames),
	}
}

// collectorDigest hashes every production Go source under root, each
// bound to its path so a moved file changes the sum. Test sources are
// excluded: they decide nothing a published grade rests on.
func collectorDigest(root string) (string, error) {
	var paths []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("walk %s: %w", root, walkErr)
	}
	slices.Sort(paths)

	sum := sha256.New()
	for _, path := range paths {
		content, readErr := os.ReadFile(path) //nolint:gosec // paths come from walking the collector's own source tree
		if readErr != nil {
			return "", fmt.Errorf("read %s: %w", path, readErr)
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return "", fmt.Errorf("relate %s to %s: %w", path, root, relErr)
		}
		sum.Write(fmt.Appendf(nil, "%s\n%d\n", filepath.ToSlash(relative), len(content)))
		sum.Write(content)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func fileDigest(path string) (string, error) {
	content, err := os.ReadFile(path) //nolint:gosec // an artifact this run just wrote under its own output directory
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

// credentialModes reports, per declared authentication variable name,
// whether the launch environment supplies it. Only that answer travels;
// a credential value never enters an artifact.
func credentialModes(names []string) map[string]string {
	modes := make(map[string]string, len(names))
	for _, name := range names {
		mode := "absent"
		if _, ok := os.LookupEnv(name); ok {
			mode = "present"
		}
		modes[name] = mode
	}
	return modes
}

func writeProvenance(path string, provenance collectionProvenance) error {
	encoded, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

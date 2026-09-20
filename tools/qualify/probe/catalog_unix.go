//go:build unix

package probe

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
)

// probeStartedMarker is the file every probe executable creates. It
// attests to a file, not execution: a runtime could create it without
// running the script, so awaitProbeExecution is used where that matters.
const probeStartedMarker = "sortie-probe-started"

// The three probe executables' script bodies: failing exits non-zero
// immediately, cancellation traps SIGINT for a graceful exit, and
// transport waits to be killed outright.
const (
	failingProbeScript = "touch " + probeStartedMarker + "\nexit 1\n"

	// The trap precedes the marker so a SIGINT sent as soon as the
	// marker appears is caught rather than killing the shell outright.
	cancellationProbeScript = "trap 'exit 0' INT\n" +
		"touch " + probeStartedMarker + "\n" +
		"while true; do sleep 0.1; done\n"

	transportProbeScript = "touch " + probeStartedMarker + "\n" +
		"while true; do sleep 0.1; done\n"
)

// usageTracker accumulates whether any protocol turn reported
// UsageMeasured and which session first did, so the protocol
// token-inventory reading is composed once at the end of the phase.
type usageTracker struct {
	mu        sync.Mutex
	measured  bool
	sessionID string
}

func (u *usageTracker) observe(sessionID string, measured bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if measured && !u.measured {
		u.measured = true
		u.sessionID = sessionID
	}
}

func (u *usageTracker) result() (measured bool, sessionID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.measured, u.sessionID
}

// groupTracker is the run-owned registry of every process-group id a
// graded launch starts. The process-cleanup inducer drains every group
// it holds.
type groupTracker struct {
	mu    sync.Mutex
	pgids []int
}

func (g *groupTracker) register(pgid int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Contains(g.pgids, pgid) {
		g.pgids = append(g.pgids, pgid)
	}
}

func (g *groupTracker) all() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.pgids)
}

// ownedSession is one protocol session this run started. stopped
// records that StopSession has already been called, so the stop happens
// once however many paths reach it.
type ownedSession struct {
	adapter domain.AgentAdapter
	session domain.Session
	pgid    int
	stopped bool
}

// sharedFixture bundles the launch substrate every inducer in this
// package shares.
type sharedFixture struct {
	workspaceRoot     string
	failingProbe      string
	cancellationProbe string
	transportProbe    string
	nonce             string
	env               []string
	envWrapper        string
	tracker           *groupTracker
	journal           *observationJournal

	// probeObservationBound and signalWaitBound override this run's
	// default wait bounds; zero falls back to the collection's own,
	// and only a control shortens them.
	probeObservationBound time.Duration
	signalWaitBound       time.Duration

	logSpy *agenttest.LogSpy
	usage  *usageTracker

	// wireTraceDir holds one file per protocol launch with the raw bytes
	// it wrote on the transport, since the adapter normalizes a prompt
	// result down to a turn outcome and discards them otherwise.
	wireTraceDir string

	ownershipMu sync.Mutex
	launches    []string          // guarded by ownershipMu
	sessions    []*ownedSession   // guarded by ownershipMu
	owned       *ownedDescendants // guarded by ownershipMu

	nativeOutputsMu sync.Mutex
	nativeOutputs   map[evidence.Surface][]string

	unrecognizedMu sync.Mutex
	unrecognized   []map[string]any
}

// recordNativeOutput appends output to surface's accumulated native
// induction outputs, read back by the per-surface token inventory once
// that surface's induction phase completes.
func (f *sharedFixture) recordNativeOutput(surface evidence.Surface, output string) {
	f.nativeOutputsMu.Lock()
	defer f.nativeOutputsMu.Unlock()
	if f.nativeOutputs == nil {
		f.nativeOutputs = map[evidence.Surface][]string{}
	}
	f.nativeOutputs[surface] = append(f.nativeOutputs[surface], output)
}

func (f *sharedFixture) nativeOutputsFor(surface evidence.Surface) []string {
	f.nativeOutputsMu.Lock()
	defer f.nativeOutputsMu.Unlock()
	return slices.Clone(f.nativeOutputs[surface])
}

// recordUnrecognized appends a terminal object no recognizer could
// resolve to a known outcome, read back at the end of Run to write the
// unrecognized.jsonl discovery artifact.
func (f *sharedFixture) recordUnrecognized(terminal map[string]any) {
	f.unrecognizedMu.Lock()
	defer f.unrecognizedMu.Unlock()
	f.unrecognized = append(f.unrecognized, terminal)
}

func (f *sharedFixture) unrecognizedTerminals() []map[string]any {
	f.unrecognizedMu.Lock()
	defer f.unrecognizedMu.Unlock()
	return slices.Clone(f.unrecognized)
}

// streamRetentionBound bounds Stdout and Stderr retention in the journal:
// enough for a recognizer's own re-derivation input, small enough that
// the journal stays bounded by the catalog, not by what a runtime prints.
const streamRetentionBound = 64 * 1024

// boundStream retains s at streamRetentionBound bytes, head and tail. An
// empty stream reports StreamRetentionFull, never a truncation.
func boundStream(s string) (bounded string, size int64, retention evidence.StreamRetention) {
	size = int64(len(s))
	if len(s) <= streamRetentionBound {
		return s, size, evidence.StreamRetentionFull
	}
	half := streamRetentionBound / 2
	return s[:half] + s[len(s)-half:], size, evidence.StreamRetentionTruncated
}

// boundStreamCapture bounds output into one StreamCapture. Stdout and
// stderr are already combined by boundedLaunch.await, so Stderr here is
// always empty; Combined() is what a recognizer reads regardless.
func boundStreamCapture(output string) evidence.StreamCapture {
	bounded, size, retention := boundStream(output)
	return evidence.StreamCapture{
		Stdout:      bounded,
		StdoutBytes: size,
		Retention:   retention,
	}
}

// gradedObservation bundles a grading decision with the derivation that
// reached it, plus, for "recognizer", the launch and streams a
// re-derivation replays, so the journal never loses that material.
type gradedObservation struct {
	obs        evidence.Observation
	derivation evidence.Derivation
	launch     *evidence.LaunchRecord
	streams    *evidence.StreamCapture
}

// transportGraded wraps obs as a "transport" derivation: decided from
// protocol events or process state rather than launch bytes, so a
// re-derivation carries it over rather than replaying it.
func transportGraded(obs evidence.Observation) gradedObservation {
	return gradedObservation{obs: obs, derivation: evidence.DerivationTransport}
}

// composedGraded wraps obs as a "composed" derivation: another entry's
// observation, restated.
func composedGraded(obs evidence.Observation) gradedObservation {
	return gradedObservation{obs: obs, derivation: evidence.DerivationComposed}
}

// recognizerGraded wraps obs as a "recognizer" derivation, carrying the
// launch and streams a re-derivation replays through eval.Recognize.
func recognizerGraded(obs evidence.Observation, launch evidence.LaunchRecord, streams evidence.StreamCapture) gradedObservation {
	return gradedObservation{obs: obs, derivation: evidence.DerivationRecognizer, launch: &launch, streams: &streams}
}

// observationJournal appends every observation to a run-scoped file as
// it is obtained, so a composition failure, which only runs once every
// launch ends, costs a report rather than the whole run.
type observationJournal struct {
	mu       sync.Mutex
	path     string
	firstErr error
}

// append writes one journal entry. A journal with no path writes nothing.
// derivation, launch and streams are captured by the caller before this
// call, so the critical section below never extends over subprocess I/O.
func (j *observationJournal) append(surface, caseID string, g gradedObservation) {
	if j == nil || j.path == "" {
		return
	}
	entry := evidence.JournalEntry{
		Surface:    surface,
		Case:       caseID,
		Grade:      string(g.obs.Grade),
		Outcome:    string(g.obs.Outcome),
		Detail:     g.obs.Detail,
		SessionID:  g.obs.SessionID,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		Derivation: g.derivation,
		Launch:     g.launch,
		Streams:    g.streams,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		j.fail(err)
		return
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	file, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		j.recordErr(err)
		return
	}
	defer file.Close() //nolint:errcheck // best-effort close after a completed append
	if _, err := file.Write(append(line, '\n')); err != nil {
		j.recordErr(err)
	}
}

func (j *observationJournal) fail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordErr(err)
}

// recordErr keeps the first failure. Callers hold j.mu.
func (j *observationJournal) recordErr(err error) {
	if j.firstErr == nil {
		j.firstErr = err
	}
}

// err reports the first failure the journal met, so a run whose
// observations were not all persisted says so rather than leaving a
// short file to be read as a short run.
func (j *observationJournal) err() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.firstErr
}

// resolveSharedFixture builds the shared fixture once for the whole
// collection. It fails the run when the controlled checkout carries a
// project_config_paths entry.
func resolveSharedFixture(t *testing.T, coords Coordinates) *sharedFixture {
	t.Helper()

	workspaceRoot := t.TempDir()
	initControlledCheckout(t, workspaceRoot)
	for _, rel := range coords.Profile.ProjectConfigPaths {
		if _, err := os.Stat(filepath.Join(workspaceRoot, rel)); err == nil {
			t.Fatalf("controlled checkout unexpectedly carries project_config_paths entry %s", rel)
		}
	}

	scriptDir := t.TempDir()
	traceDir := t.TempDir()
	fixture := &sharedFixture{
		workspaceRoot:     workspaceRoot,
		wireTraceDir:      traceDir,
		failingProbe:      agenttest.WriteRecordingScript(t, scriptDir, "failing-probe", failingProbeScript),
		cancellationProbe: agenttest.WriteRecordingScript(t, scriptDir, "cancellation-probe", cancellationProbeScript),
		transportProbe:    agenttest.WriteRecordingScript(t, scriptDir, "transport-probe", transportProbeScript),
		nonce:             newRunNonce(t),
		env:               launchEnvironment(coords),
		envWrapper:        writeLaunchEnvWrapper(t, scriptDir, launchEnvNames(coords), traceDir),
		tracker:           &groupTracker{},
		logSpy:            agenttest.InstallLogSpy(t),
		usage:             &usageTracker{},
	}
	fixture.ownership().watchReceipts(coords.CommandPath, fixture.failingProbe, fixture.cancellationProbe, fixture.transportProbe)
	return fixture
}

func initControlledCheckout(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "init") //nolint:gosec // fixed command, dir is this package's own temporary directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init controlled checkout: %v\n%s", err, out)
	}
}

func newRunNonce(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("generate run nonce: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// launchEnvNames resolves the closed list of environment variable
// names a launch may carry: PATH, HOME, the operator's authentication
// names, and the profile's config-root names.
func launchEnvNames(coords Coordinates) []string {
	names := append([]string{"PATH", "HOME"}, coords.AuthEnvNames...)
	return append(names, coords.Profile.ConfigRootEnvNames...)
}

// launchEnvironment resolves the launch environment allowlist every
// native launch carries: every name launchEnvNames lists, resolved to
// its value when the parent environment carries it, and no other.
func launchEnvironment(coords Coordinates) []string {
	names := launchEnvNames(coords)
	env := make([]string, 0, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// envNamePattern is the shape a launch environment name must have.
// Coordinates and profile data reach the generated wrapper as shell
// words, so a name outside this shape is refused rather than expanded.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvNames reports the first of names that is not a shell name;
// such a name would be expanded as code by the generated wrapper.
func validateEnvNames(names []string) error {
	for _, name := range names {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("launch environment name %q is not a shell name", name)
		}
	}
	return nil
}

// writeLaunchEnvWrapper re-execs with only the allowlisted names, each
// read from its own inherited environment, so an unset name stays
// unset rather than arriving empty.
func writeLaunchEnvWrapper(t *testing.T, dir string, names []string, traceDir string) string {
	t.Helper()
	if err := validateEnvNames(names); err != nil {
		t.Fatalf("resolve the launch environment: %v", err)
	}
	prologue, err := wireTracePrologue(traceDir)
	if err != nil {
		t.Fatalf("resolve the transport capture: %v", err)
	}
	body := "argc=$#\n" +
		"for name in " + strings.Join(names, " ") + "; do\n" +
		"\teval \"case \\${$name+carried} in carried) set -- \\\"\\$@\\\" \\\"$name=\\$$name\\\" ;; esac\"\n" +
		"done\n" +
		"while [ \"$argc\" -gt 0 ]; do\n" +
		"\tset -- \"$@\" \"$1\"\n" +
		"\tshift\n" +
		"\targc=$((argc - 1))\n" +
		"done\n" +
		prologue +
		"exec env -i \"$@\"\n"
	return agenttest.WriteScript(t, dir, "launch-env-wrapper", body)
}

// wireTraceFilePrefix names the per-launch transport capture files the
// wrapper writes. One file per launch, since a shared file would leave
// two sessions' results indistinguishable.
const wireTraceFilePrefix = "wire."

// wireTracePrologue runs through a named pipe, not a pipeline, so the
// wrapper still exec's its runtime rather than becoming a shell stage
// in front of it. It reads tee's operand copy, not its own output,
// since tee writes that first: every byte lands in the capture before
// a reading can catch it still being written.
func wireTracePrologue(traceDir string) (string, error) {
	if traceDir == "" {
		return "", nil
	}
	if strings.ContainsAny(traceDir, "'\n") {
		return "", fmt.Errorf("transport capture directory %q is not a shell word", traceDir)
	}
	// The pid alone would not name one launch: a long collection
	// outlives a pid, and a reused one would append a second launch's
	// results to the first one's capture.
	return "trace='" + traceDir + "'\n" +
		"stem=0\n" +
		"while [ -e \"$trace/" + wireTraceFilePrefix + "$$.$stem.jsonl\" ]; do\n" +
		"\tstem=$((stem + 1))\n" +
		"done\n" +
		"stem=\"$trace/" + wireTraceFilePrefix + "$$.$stem\"\n" +
		"if command -v tee >/dev/null 2>&1 && command -v cat >/dev/null 2>&1 && mkfifo \"$stem.fifo\" \"$stem.out\" 2>/dev/null; then\n" +
		"\tcat <\"$stem.out\" &\n" +
		"\ttee -a \"$stem.out\" <\"$stem.fifo\" >>\"$stem.jsonl\" &\n" +
		"\texec env -i \"$@\" >\"$stem.fifo\"\n" +
		"fi\n", nil
}

// controlledCommand renders the launch of commandPath with argv through
// the run's environment wrapper, in the single-string shape the adapter
// parses. A fixture carrying no wrapper launches directly.
func (f *sharedFixture) controlledCommand(commandPath string, argv []string) string {
	launch := append([]string{commandPath}, argv...)
	if f.envWrapper != "" {
		launch = append([]string{f.envWrapper}, launch...)
	}
	return strings.Join(launch, " ")
}

// launchOwners indexes every launch workspace by the fixture that
// created it, so a helper handed only that directory can charge the
// launch to the run that must account for it.
var launchOwners sync.Map

func fixtureOwningLaunch(workspace string) *sharedFixture {
	owner, ok := launchOwners.Load(workspace)
	if !ok {
		return nil
	}
	fixture, _ := owner.(*sharedFixture)
	return fixture
}

// newLaunchWorkspace creates a fresh launch subdirectory and records it,
// since the workspace-security reading inspects the directories
// recorded here.
func (f *sharedFixture) newLaunchWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(f.workspaceRoot, "launch-*")
	if err != nil {
		t.Fatalf("create launch workspace under the controlled checkout: %v", err)
	}
	f.ownershipMu.Lock()
	f.launches = append(f.launches, dir)
	f.ownershipMu.Unlock()
	launchOwners.Store(dir, f)
	t.Cleanup(func() { launchOwners.Delete(dir) })
	return dir
}

// ownership returns the run's ledger of processes its snapshots proved
// it owns. One ledger serves the whole collection because a descendant's
// parent trace closes long before the teardown reading needs it.
func (f *sharedFixture) ownership() *ownedDescendants {
	f.ownershipMu.Lock()
	defer f.ownershipMu.Unlock()
	if f.owned == nil {
		f.owned = newOwnedDescendants(f.tracker)
	}
	return f.owned
}

func (f *sharedFixture) launchDirs() []string {
	f.ownershipMu.Lock()
	defer f.ownershipMu.Unlock()
	return slices.Clone(f.launches)
}

// registerSession records session as one this run must account for and
// adds its launch's process group to the tracker.
func (f *sharedFixture) registerSession(adapter domain.AgentAdapter, session domain.Session) error {
	pgid, err := strconv.Atoi(session.AgentPID)
	if err != nil {
		return fmt.Errorf("session agent_pid %q did not parse as a process id: %w", session.AgentPID, err)
	}
	f.ownershipMu.Lock()
	f.sessions = append(f.sessions, &ownedSession{adapter: adapter, session: session, pgid: pgid})
	f.ownershipMu.Unlock()
	f.tracker.register(pgid)
	return nil
}

// claimOpenSessions marks every not-yet-stopped session as stopped and
// returns them, so StopSession runs once however many paths reach it.
func (f *sharedFixture) claimOpenSessions() []*ownedSession {
	f.ownershipMu.Lock()
	defer f.ownershipMu.Unlock()
	var open []*ownedSession
	for _, session := range f.sessions {
		if !session.stopped {
			session.stopped = true
			open = append(open, session)
		}
	}
	return open
}

func (f *sharedFixture) stopOpenSessions(ctx context.Context) (int, error) {
	open := f.claimOpenSessions()
	var firstErr error
	for _, owned := range open {
		stopCtx, cancel := context.WithTimeout(ctx, procgroup.ShutdownDeadline)
		err := owned.adapter.StopSession(stopCtx, owned.session)
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(open), firstErr
}

func (f *sharedFixture) observationBound() time.Duration {
	return cmp.Or(f.probeObservationBound, nativeProbeBound)
}

func (f *sharedFixture) signalBound() time.Duration {
	return cmp.Or(f.signalWaitBound, nativeSignalBound)
}

func probeStarted(workspace string) bool {
	_, err := os.Stat(filepath.Join(workspace, probeStartedMarker))
	return err == nil
}

const probeExecutionPollInterval = 100 * time.Millisecond

// awaitProbeExecution reads the process table, not the started marker,
// since a runtime could create that marker without running probePath.
// Every snapshot folds into owned: the last traceable moment before
// pgid is signalled.
func awaitProbeExecution(owned *ownedDescendants, probePath string, pgid int, deadline time.Time) bool {
	basename := filepath.Base(probePath)
	for {
		if snapshot, err := psSnapshot(); err == nil {
			owned.observe(snapshot)
			if launchRunsProgram(snapshot, pgid, basename) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(probeExecutionPollInterval)
	}
}

func launchRunsProgram(members []processMember, pgid int, basename string) bool {
	for _, m := range members {
		if m.basename != basename {
			continue
		}
		if m.pgid == pgid || descendantOf(members, m, pgid) {
			return true
		}
	}
	return false
}

// descendantOf reports whether member's parent chain inside members
// reaches pid. The walk is bounded by the snapshot's size, so a parent
// cycle a reused pid can fabricate ends it rather than hanging.
func descendantOf(members []processMember, member processMember, pid int) bool {
	byPID := make(map[int]processMember, len(members))
	for _, m := range members {
		byPID[m.pid] = m
	}
	for range len(members) {
		if member.ppid == pid {
			return true
		}
		parent, ok := byPID[member.ppid]
		if !ok {
			return false
		}
		member = parent
	}
	return false
}

// The evidence.RuntimeProfile.ProbePrompts keys this package
// substitutes, mirrored here because the map carries no named constants.
const (
	promptKeySuccess            = "success"
	promptKeyRuntimeRefusal     = "runtime_refusal"
	promptKeyToolCall           = "tool_call"
	promptKeyContinuationSeed   = "continuation_seed"
	promptKeyContinuationRecall = "continuation_recall"
)

func (f *sharedFixture) probePath(name string) string {
	switch name {
	case "failing":
		return f.failingProbe
	case "cancellation":
		return f.cancellationProbe
	case "transport":
		return f.transportProbe
	default:
		return ""
	}
}

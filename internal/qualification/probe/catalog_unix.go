//go:build unix

package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// probeStartedMarker is the file name every probe executable creates in
// its own working directory. It attests to a file and nothing more: the
// runtime is handed the probe's path and can create this marker without
// running the script.
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

// groupTracker is the run-owned registry of every process-group id a
// graded launch starts.
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

	logSpy *agenttest.LogSpy

	// wireTraceDir holds one file per protocol launch carrying the bytes
	// that launch wrote on the transport. The adapter normalizes a
	// prompt result down to a turn outcome, so without this the
	// collection could never say what the wire itself carried.
	wireTraceDir string

	ownershipMu sync.Mutex
	launches    []string          // guarded by ownershipMu
	sessions    []*ownedSession   // guarded by ownershipMu
	owned       *ownedDescendants // guarded by ownershipMu

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

// writeLaunchEnvWrapper writes the wrapper every protocol launch runs
// through, and returns its path. An empty traceDir launches without
// capturing the transport.
//
// The adapter hands the runtime the orchestrator's full environment,
// but a comparison needs the native surface's allowlist on both. The
// wrapper re-execs its arguments carrying only the allowlisted names,
// each read from its own inherited environment, so a name the parent
// does not carry stays unset rather than arriving empty.
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

// wireTracePrologue renders the shell that copies a launch's transport
// output into traceDir on the way through to the adapter.
//
// The copy runs through a named pipe rather than a pipeline so the
// wrapper keeps exec'ing its runtime, leaving the runtime as the
// process the adapter waits on, the group leader the cleanup drains,
// and the source of the exit status. Every step falls through to the
// plain launch, since an uncaptured transport is only unobserved while
// a launch that never starts costs the whole run.
//
// The adapter reads the operand tee copies to, not tee's own output,
// because tee writes its standard output before its operands: that
// order puts every byte the adapter can read into the capture first, so
// a reading taken the moment a turn returns is never of a capture still
// being written.
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

// newLaunchWorkspace creates a fresh subdirectory of the controlled
// checkout, records it as one of the run's launches, and returns its
// path.
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

// ownership returns the run's ledger of the processes its snapshots
// have proved it owns. One ledger serves the whole collection because a
// descendant is provably this run's only while its parent is still
// there to be traced through.
func (f *sharedFixture) ownership() *ownedDescendants {
	f.ownershipMu.Lock()
	defer f.ownershipMu.Unlock()
	if f.owned == nil {
		f.owned = newOwnedDescendants(f.tracker)
	}
	return f.owned
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

func (f *sharedFixture) stopOpenSessions(ctx context.Context) error {
	var firstErr error
	for _, owned := range f.claimOpenSessions() {
		stopCtx, cancel := context.WithTimeout(ctx, qualification.ShutdownDeadline)
		err := owned.adapter.StopSession(stopCtx, owned.session)
		cancel()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

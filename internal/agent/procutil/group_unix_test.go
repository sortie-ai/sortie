//go:build unix

package procutil

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func stubGroupKill(t *testing.T, fn func(pid int, sig syscall.Signal) error) {
	t.Helper()
	orig := groupKillFunc
	t.Cleanup(func() { groupKillFunc = orig })
	groupKillFunc = fn
}

func stubLiveMember(t *testing.T, fn func(pgid, leader int) (bool, error)) {
	t.Helper()
	orig := liveMemberFunc
	t.Cleanup(func() { liveMemberFunc = orig })
	liveMemberFunc = fn
}

func stubLeaderExit(t *testing.T, fn func(pid int) error) {
	t.Helper()
	orig := leaderExitFunc
	t.Cleanup(func() { leaderExitFunc = orig })
	leaderExitFunc = fn
}

func stubReleaseSeam(t *testing.T, fn func()) {
	t.Helper()
	orig := releaseSeam
	t.Cleanup(func() { releaseSeam = orig })
	releaseSeam = fn
}

func killedBySignal(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

func shortenDrainBound(t *testing.T, bound time.Duration) {
	t.Helper()
	orig := groupDrainBound
	t.Cleanup(func() { groupDrainBound = orig })
	groupDrainBound = bound
}

func spyGroupKill(t *testing.T, forward bool) *atomic.Int32 {
	t.Helper()
	orig := groupKillFunc
	calls := &atomic.Int32{}
	t.Cleanup(func() { groupKillFunc = orig })
	groupKillFunc = func(pid int, sig syscall.Signal) error {
		calls.Add(1)
		if forward {
			return orig(pid, sig)
		}
		return nil
	}
	return calls
}

func sequence[T any](items ...T) func() T {
	next := 0
	return func() T {
		item := items[min(next, len(items)-1)]
		next++
		return item
	}
}

func startLeader(t *testing.T, out agenttest.Output) *exec.Cmd {
	t.Helper()
	cmd := fakeRuntimeCmd(t, out)
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func startLeaderWithLiveMember(t *testing.T) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", "procutil.group-descendant", groupDescendantParams{
		Marker:  filepath.Join(dir, "descendant.terminated"),
		PIDFile: pidFile,
	})
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.group-leader", groupLeaderParams{
		DescendantPath: descendantPath,
	})
	cmd := exec.Command(leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	pollForPID(t, pidFile, 5*time.Second)
	return cmd
}

func unreapedRecord(t *testing.T) *Group {
	t.Helper()
	return newGroup(startLeader(t, agenttest.Output{Hang: true}))
}

func releasedLaunch(t *testing.T) (*Group, *exec.Cmd) {
	t.Helper()
	path := agenttest.FakeRuntime(t, t.TempDir(), "fake", agenttest.OutputScenario, agenttest.Output{})
	cmd := exec.CommandContext(t.Context(), path) //nolint:gosec // fake runtime path under t.TempDir()
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() = %v, want nil", err)
	}
	return releasedRecordFor(t, cmd), cmd
}

func releasedRecordFor(t *testing.T, cmd *exec.Cmd) *Group {
	t.Helper()
	g := newGroup(cmd)
	g.released = true
	groups.Store(cmd, g)
	t.Cleanup(func() { groups.Delete(cmd) })
	return g
}

func runReapToDone(t *testing.T, cmd *exec.Cmd, logger *slog.Logger) *Reaper {
	t.Helper()
	startOwned(t, cmd)
	r := StartReaper(cmd, logger)
	awaitDone(t, r, 10*time.Second)
	return r
}

func countRecords(spy *captureLogSpy, msg string) int {
	var count int
	for _, rec := range spy.snapshot() {
		if rec.Msg == msg {
			count++
		}
	}
	return count
}

func TestNilGroupIsNoOp(t *testing.T) {
	t.Parallel()

	var g *Group

	if err := g.SignalGraceful(); err != nil {
		t.Errorf("(*Group)(nil).SignalGraceful() = %v, want nil", err)
	}
	if err := g.Kill(); err != nil {
		t.Errorf("(*Group)(nil).Kill() = %v, want nil", err)
	}
	if g.Stopped() {
		t.Error("(*Group)(nil).Stopped() = true, want false")
	}
}

func TestGroupReleasedMakesNoSend(t *testing.T) {
	tests := []struct {
		name    string
		run     func(g *Group, cmd *exec.Cmd) error
		wantErr error
	}{
		{
			name: "SignalGraceful",
			run:  func(g *Group, _ *exec.Cmd) error { return g.SignalGraceful() },
		},
		{
			name: "Kill",
			run:  func(g *Group, _ *exec.Cmd) error { return g.Kill() },
		},
		{
			name: "SetGroupCancel closure",
			run: func(_ *Group, cmd *exec.Cmd) error {
				SetGroupCancel(cmd, time.Second)
				return cmd.Cancel()
			},
		},
		{
			name: "SetGroupKill closure",
			run: func(_ *Group, cmd *exec.Cmd) error {
				SetGroupKill(cmd)
				return cmd.Cancel()
			},
			wantErr: os.ErrProcessDone,
		},
		{
			name: "armed escalation",
			run: func(g *Group, _ *exec.Cmd) error {
				armGroupEscalation(g, time.Second)
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, cmd := releasedLaunch(t)
			calls := spyGroupKill(t, false)

			err := tt.run(g, cmd)

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("%s on a released record = %v, want %v", tt.name, err, tt.wantErr)
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("%s on a released record made %d group sends, want 0", tt.name, got)
			}
		})
	}
}

func TestGroupReleasedRecordSparesReusedIdentifier(t *testing.T) {
	victimCmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
	_, victim := startOwned(t, victimCmd)
	victimReaper := StartReaper(victimCmd, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { terminateAndAwait(t, victimCmd, victim, victimReaper) })

	proc, err := os.FindProcess(victimCmd.Process.Pid)
	if err != nil {
		t.Fatalf("os.FindProcess(%d) = %v", victimCmd.Process.Pid, err)
	}
	t.Cleanup(func() { _ = proc.Release() })
	reused := &exec.Cmd{Process: proc}
	g := releasedRecordFor(t, reused)
	calls := spyGroupKill(t, true)

	_ = g.SignalGraceful()
	_ = g.Kill()
	SetGroupCancel(reused, time.Second)
	_ = reused.Cancel()
	SetGroupKill(reused)
	_ = reused.Cancel()
	if _, ok := g.captureEscalation(); ok {
		t.Error("captureEscalation() on a released record ok = true, want false")
	}

	if got := calls.Load(); got != 0 {
		t.Errorf("operations on a released record sharing identifier %d made %d group sends, want 0", proc.Pid, got)
	}
	select {
	case <-victimReaper.Done():
		t.Errorf("the launch holding group %d ended during operations on a released record that shares its identifier", proc.Pid)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReapOutcomeForRealLaunch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		withDescendant bool
		wantLeftover   bool
	}{
		{name: "descendant outlives the direct child", withDescendant: true, wantLeftover: true},
		{name: "no descendant", withDescendant: false, wantLeftover: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			var cmd *exec.Cmd
			var descendantPID int
			if tt.withDescendant {
				childPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
				pidPath := filepath.Join(dir, "child.pid")
				leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-leader", captureLeaderParams{
					ChildPath:    childPath,
					ChildPIDPath: pidPath,
				})
				cmd = exec.Command(leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
				startOwned(t, cmd)
				descendantPID = pollCapturePIDFile(t, pidPath)
			} else {
				cmd = fakeRuntimeCmd(t, agenttest.Output{})
				startOwned(t, cmd)
			}
			pgid := cmd.Process.Pid
			if tt.withDescendant {
				got, err := syscall.Getpgid(descendantPID)
				if err != nil || got != pgid {
					t.Fatalf("Getpgid(descendant %d) = %d, %v, want group %d", descendantPID, got, err, pgid)
				}
			}

			r := StartReaper(cmd, slog.New(slog.DiscardHandler))
			awaitDone(t, r, 10*time.Second)

			if got := r.Leftover(); got != tt.wantLeftover {
				t.Errorf("Leftover() = %t, want %t", got, tt.wantLeftover)
			}
			if err := r.CleanupErr(); err != nil {
				t.Errorf("CleanupErr() = %v, want nil", err)
			}
			live, err := hasLiveMember(pgid, pgid)
			if err != nil {
				t.Fatalf("hasLiveMember(%d, %d) = %v", pgid, pgid, err)
			}
			if live {
				t.Errorf("group %d still holds a live member at Done, want none", pgid)
			}
		})
	}
}

func TestReapExitObservationFailure(t *testing.T) {
	forced := errors.New("forced exit observation failure")
	stubLeaderExit(t, func(int) error { return forced })
	kills := spyGroupKill(t, false)
	spy := &captureLogSpy{}
	cmd := fakeRuntimeCmd(t, agenttest.Output{})

	r := runReapToDone(t, cmd, slog.New(spy))

	if r.Leftover() {
		t.Error("Leftover() = true, want false when the exit was never observed")
	}
	if err := r.CleanupErr(); !errors.Is(err, forced) {
		t.Errorf("CleanupErr() = %v, want an error wrapping the observation failure", err)
	}
	if got := countRecords(spy, CaptureCleanupWarning); got != 1 {
		t.Errorf("%q records = %d, want 1", CaptureCleanupWarning, got)
	}
	if cmd.ProcessState == nil {
		t.Error("cmd.ProcessState = nil after Done, want the direct child reaped")
	}
	if got := kills.Load(); got != 0 {
		t.Errorf("group sends after an exit observation failure = %d, want 0", got)
	}
}

func TestReapSeamCallsPrecedeRelease(t *testing.T) {
	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	_, g := startOwned(t, cmd)
	var violations []string
	var killCalls, queryCalls int
	orig := groupKillFunc
	stubGroupKill(t, func(pid int, sig syscall.Signal) error {
		killCalls++
		if g.released {
			violations = append(violations, "groupKillFunc ran after release")
		}
		if cmd.ProcessState != nil {
			violations = append(violations, "groupKillFunc ran after cmd.Wait")
		}
		return orig(pid, sig)
	})
	nextLive := sequence(true, false)
	stubLiveMember(t, func(int, int) (bool, error) {
		queryCalls++
		g.mu.Lock()
		released := g.released
		g.mu.Unlock()
		if released {
			violations = append(violations, "liveMemberFunc ran after release")
		}
		if cmd.ProcessState != nil {
			violations = append(violations, "liveMemberFunc ran after cmd.Wait")
		}
		return nextLive(), nil
	})

	r := StartReaper(cmd, slog.New(slog.DiscardHandler))
	awaitDone(t, r, 10*time.Second)

	for _, v := range violations {
		t.Error(v)
	}
	if killCalls == 0 || queryCalls != 2 {
		t.Errorf("seam calls = %d sends, %d queries, want at least one send and 2 queries", killCalls, queryCalls)
	}
	if !g.released {
		t.Error("record released = false at Done, want true")
	}
	if !r.Leftover() {
		t.Error("Leftover() = false, want true from the query made before release")
	}
}

func TestReapDrain(t *testing.T) {
	tests := []struct {
		name         string
		sends        []error
		lives        []bool
		wantLeftover bool
		wantSends    int
		wantErr      error
	}{
		{
			name:      "group gone at the first send and nobody seen",
			sends:     []error{syscall.ESRCH},
			lives:     []bool{false},
			wantSends: 1,
		},
		{
			name:         "member seen before the first send",
			sends:        []error{syscall.ESRCH},
			lives:        []bool{true},
			wantLeftover: true,
			wantSends:    1,
		},
		{
			name:         "member seen only after the first send",
			sends:        []error{nil},
			lives:        []bool{false, true, false},
			wantLeftover: true,
			wantSends:    2,
		},
		{
			name:         "resends until no live member remains",
			sends:        []error{nil},
			lives:        []bool{true, true, false},
			wantLeftover: true,
			wantSends:    2,
		},
		{
			name:      "EPERM from a send is not a failure",
			sends:     []error{syscall.EPERM},
			lives:     []bool{false},
			wantSends: 1,
		},
		{
			name:      "a send failure ends the drain",
			sends:     []error{syscall.EINVAL},
			lives:     []bool{false},
			wantSends: 1,
			wantErr:   syscall.EINVAL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextSend := sequence(tt.sends...)
			var sends int
			stubGroupKill(t, func(int, syscall.Signal) error {
				sends++
				return nextSend()
			})
			nextLive := sequence(tt.lives...)
			stubLiveMember(t, func(int, int) (bool, error) { return nextLive(), nil })

			r := runReapToDone(t, fakeRuntimeCmd(t, agenttest.Output{}), slog.New(slog.DiscardHandler))

			if got := r.Leftover(); got != tt.wantLeftover {
				t.Errorf("Leftover() = %t, want %t", got, tt.wantLeftover)
			}
			if err := r.CleanupErr(); !errors.Is(err, tt.wantErr) {
				t.Errorf("CleanupErr() = %v, want %v", err, tt.wantErr)
			}
			if sends != tt.wantSends {
				t.Errorf("group sends = %d, want %d", sends, tt.wantSends)
			}
		})
	}
}

func TestReapDrainBounds(t *testing.T) {
	queryFailure := errors.New("forced membership failure")
	tests := []struct {
		name           string
		live           bool
		queryErr       error
		wantLeftover   bool
		wantQueryCause bool
	}{
		{name: "a live member never leaves", live: true, wantLeftover: true},
		{name: "membership cannot be read", queryErr: queryFailure, wantQueryCause: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortenDrainBound(t, 100*time.Millisecond)
			stubGroupKill(t, func(int, syscall.Signal) error { return nil })
			stubLiveMember(t, func(int, int) (bool, error) { return tt.live, tt.queryErr })
			spy := &captureLogSpy{}

			r := runReapToDone(t, fakeRuntimeCmd(t, agenttest.Output{}), slog.New(spy))

			err := r.CleanupErr()
			if err == nil {
				t.Fatal("CleanupErr() = nil, want the drain bound error")
			}
			if got := errors.Is(err, queryFailure); got != tt.wantQueryCause {
				t.Errorf("errors.Is(CleanupErr(), membership failure) = %t, want %t", got, tt.wantQueryCause)
			}
			if got := r.Leftover(); got != tt.wantLeftover {
				t.Errorf("Leftover() = %t, want %t", got, tt.wantLeftover)
			}
			if got := countRecords(spy, CaptureCleanupWarning); got != 1 {
				t.Errorf("%q records = %d, want 1", CaptureCleanupWarning, got)
			}
		})
	}
}

func TestUnixEscalationStandsDownAtRelease(t *testing.T) {
	g := unreapedRecord(t)
	calls := spyGroupKill(t, false)

	target, ok := g.captureEscalation()
	if !ok {
		t.Fatal("captureEscalation() ok = false for an unreleased record, want true")
	}
	before, err := target.hasRunningMember()
	if err != nil || !before {
		t.Fatalf("hasRunningMember() before release = %t, %v, want true, nil", before, err)
	}

	g.mu.Lock()
	g.released = true
	g.mu.Unlock()

	after, err := target.hasRunningMember()
	if err != nil || after {
		t.Errorf("hasRunningMember() after release = %t, %v, want false, nil", after, err)
	}
	if err := target.terminateAll(); err != nil {
		t.Errorf("terminateAll() after release = %v, want nil", err)
	}
	target.close()
	if got := calls.Load(); got != 0 {
		t.Errorf("escalation made %d group sends after release, want 0", got)
	}
	if _, ok := g.captureEscalation(); ok {
		t.Error("captureEscalation() after release ok = true, want false")
	}
}

func TestCancellationAfterOwnExitIsNotAStop(t *testing.T) {
	installs := []struct {
		name    string
		install func(*exec.Cmd)
	}{
		{name: "SetGroupCancel", install: func(cmd *exec.Cmd) { SetGroupCancel(cmd, 100*time.Millisecond) }},
		{name: "SetGroupKill", install: SetGroupKill},
	}
	points := []struct {
		name string
		at   func(t *testing.T, fire func())
	}{
		{
			name: "after the exit was observed",
			at: func(t *testing.T, fire func()) {
				t.Helper()
				stubLeaderExit(t, func(pid int) error {
					if err := observeLeaderExit(pid); err != nil {
						return err
					}
					fire()
					return nil
				})
			},
		},
		{
			name: "after the release",
			at: func(t *testing.T, fire func()) {
				t.Helper()
				stubReleaseSeam(t, fire)
			},
		},
	}

	for _, in := range installs {
		for _, pt := range points {
			t.Run(in.name+" "+pt.name, func(t *testing.T) {
				shortenDrainBound(t, 200*time.Millisecond)
				path := agenttest.FakeRuntime(t, t.TempDir(), "fake", agenttest.OutputScenario, agenttest.Output{})
				cmd, p := newCancelProbe(t, in.install, path)
				pt.at(t, p.fire)
				startOwned(t, cmd)

				r := StartReaper(cmd, slog.New(slog.DiscardHandler))
				awaitDone(t, r, 10*time.Second)

				if !p.observed.Load() {
					t.Fatal("the cancellation did not run before the wait, want it to have run")
				}
				if err := r.Err(); err != nil {
					t.Errorf("Err() = %v for a child that exited zero before the cancellation, want nil", err)
				}
				if r.Stopped() {
					t.Error("Stopped() = true for a cancellation after the child's own exit, want false")
				}
			})
		}
	}
}

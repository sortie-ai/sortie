package workspace

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) warnMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var msgs []string
	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			msgs = append(msgs, r.Message)
		}
	}
	return msgs
}

func newRecordingLogger() (*slog.Logger, *recordingHandler) {
	h := &recordingHandler{}
	return slog.New(h), h
}

func notificationSlotsPath(ws string) string {
	return filepath.Join(ws, workspacekit.SortieDir, notificationSlotsDir)
}

func readNotificationSlotNames(t *testing.T, workspacePath, dispatchID string) []string {
	t.Helper()
	entries, err := os.ReadDir(notificationSlotsPath(workspacePath))
	if err != nil {
		t.Fatalf("ReadDir(notification_slots): %v", err)
	}
	prefix := dispatchID + "-"
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names
}

func classifySlotNames(names []string) (slotFiles, lockFiles, pendingFiles []string) {
	for _, name := range names {
		switch {
		case strings.HasSuffix(name, slotLockSuffix):
			lockFiles = append(lockFiles, name)
		case strings.HasSuffix(name, slotPendingSuffix):
			pendingFiles = append(pendingFiles, name)
		default:
			slotFiles = append(slotFiles, name)
		}
	}
	return slotFiles, lockFiles, pendingFiles
}

func entryExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true
	case errors.Is(err, fs.ErrNotExist):
		return false
	default:
		t.Fatalf("Lstat(%q): %v", path, err)
		return false
	}
}

func endOnCleanup(t *testing.T, slot *NotificationSlot) {
	t.Helper()
	t.Cleanup(slot.Release)
}

func mustReserve(t *testing.T, ws, dispatchID string, limit int) *NotificationSlot {
	t.Helper()
	slot, reserved, err := ReserveNotificationSlot(ws, dispatchID, limit, nil)
	if slot != nil {
		endOnCleanup(t, slot)
	}
	if err != nil || !reserved || slot == nil {
		t.Fatalf("ReserveNotificationSlot(%q, %d) = slot=%v reserved=%v err=%v, want a claim", dispatchID, limit, slot, reserved, err)
	}
	return slot
}

func assertLimitOneExhausted(t *testing.T, ws, dispatchID string) {
	t.Helper()
	slot, reserved, err := ReserveNotificationSlot(ws, dispatchID, 1, nil)
	if slot != nil {
		endOnCleanup(t, slot)
	}
	if err != nil {
		t.Fatalf("ReserveNotificationSlot(%q, 1) error = %v, want nil", dispatchID, err)
	}
	if reserved || slot != nil {
		t.Errorf("ReserveNotificationSlot(%q, 1) = slot=%v reserved=%v, want nil, false", dispatchID, slot, reserved)
	}
}

func newSlotWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	createSortieDir(t, ws)
	return ws
}

func TestReserveNotificationSlot_ConcurrencyRespectsLimit(t *testing.T) {
	t.Parallel()

	ws := newSlotWorkspace(t)

	const (
		workers    = 32
		limit      = 5
		dispatchID = "dispatch-concurrency"
	)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		held []*NotificationSlot
	)
	for range workers {
		wg.Go(func() {
			slot, reserved, err := ReserveNotificationSlot(ws, dispatchID, limit, nil)
			if err != nil {
				t.Errorf("ReserveNotificationSlot: unexpected error: %v", err)
				return
			}
			if reserved {
				mu.Lock()
				held = append(held, slot)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, slot := range held {
		endOnCleanup(t, slot)
	}

	if got := len(held); got != limit {
		t.Fatalf("ReserveNotificationSlot concurrent reservations = %d, want %d", got, limit)
	}

	for _, slot := range held {
		slot.Commit()
	}

	slotFiles, lockFiles, pendingFiles := classifySlotNames(readNotificationSlotNames(t, ws, dispatchID))
	if len(slotFiles) != limit {
		t.Errorf("slot files for %q = %v, want %d", dispatchID, slotFiles, limit)
	}
	if len(lockFiles) != limit {
		t.Errorf("lock files for %q = %v, want %d", dispatchID, lockFiles, limit)
	}
	if len(pendingFiles) != 0 {
		t.Errorf("pending files for %q = %v, want none after every claim committed", dispatchID, pendingFiles)
	}
}

func TestReserveNotificationSlot_FencesByDispatch(t *testing.T) {
	t.Parallel()

	ws := newSlotWorkspace(t)

	const limit = 2
	mustReserve(t, ws, "dispatch-a", limit)
	committed := mustReserve(t, ws, "dispatch-a", limit)
	committed.Commit()
	before := readNotificationSlotNames(t, ws, "dispatch-a")

	mustReserve(t, ws, "dispatch-b", limit)

	after := readNotificationSlotNames(t, ws, "dispatch-a")
	if !slices.Equal(before, after) {
		t.Errorf("dispatch-a slots changed from %v to %v after dispatch-b's reservation, want unchanged", before, after)
	}
}

func TestReserveNotificationSlot_Refusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T) (workspacePath, dispatchID string)
	}{
		{
			name: "empty workspace path",
			setup: func(t *testing.T) (string, string) {
				return "", "dispatch-1"
			},
		},
		{
			name: "empty dispatch id",
			setup: func(t *testing.T) (string, string) {
				return newSlotWorkspace(t), ""
			},
		},
		{
			name: "dispatch id differs from its own sanitized key",
			setup: func(t *testing.T) (string, string) {
				return newSlotWorkspace(t), "a/b"
			},
		},
		{
			name: "dispatch id is a path traversal shape",
			setup: func(t *testing.T) (string, string) {
				return newSlotWorkspace(t), "../x"
			},
		},
		{
			name: "workspace path does not exist",
			setup: func(t *testing.T) (string, string) {
				return filepath.Join(t.TempDir(), "missing"), "dispatch-1"
			},
		},
		{
			name: "dot sortie absent",
			setup: func(t *testing.T) (string, string) {
				return t.TempDir(), "dispatch-1"
			},
		},
		{
			name: "dot sortie is a regular file",
			setup: func(t *testing.T) (string, string) {
				ws := t.TempDir()
				if err := os.WriteFile(filepath.Join(ws, workspacekit.SortieDir), []byte("x"), 0o600); err != nil {
					t.Fatalf("WriteFile(.sortie): %v", err)
				}
				return ws, "dispatch-1"
			},
		},
		{
			name: "notification_slots is a regular file",
			setup: func(t *testing.T) (string, string) {
				ws := newSlotWorkspace(t)
				if err := os.WriteFile(notificationSlotsPath(ws), []byte("x"), 0o600); err != nil {
					t.Fatalf("WriteFile(notification_slots): %v", err)
				}
				return ws, "dispatch-1"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspacePath, dispatchID := tt.setup(t)

			slot, reserved, err := ReserveNotificationSlot(workspacePath, dispatchID, 2, nil)
			if slot != nil {
				endOnCleanup(t, slot)
			}

			if err == nil {
				t.Fatalf("ReserveNotificationSlot(%q, %q) error = nil, want non-nil", workspacePath, dispatchID)
			}
			if reserved {
				t.Errorf("ReserveNotificationSlot(%q, %q) reserved = true, want false", workspacePath, dispatchID)
			}
			if slot != nil {
				t.Errorf("ReserveNotificationSlot(%q, %q) slot = non-nil, want nil", workspacePath, dispatchID)
			}
		})
	}
}

func TestReserveNotificationSlot_ExistingDirectoryCountsAsOccupied(t *testing.T) {
	t.Parallel()

	ws := newSlotWorkspace(t)
	slotsPath := notificationSlotsPath(ws)
	if err := os.MkdirAll(slotsPath, 0o750); err != nil {
		t.Fatalf("MkdirAll(notification_slots): %v", err)
	}
	if err := os.Mkdir(filepath.Join(slotsPath, "dispatch-occupied-1"), 0o750); err != nil {
		t.Fatalf("Mkdir(dispatch-occupied-1): %v", err)
	}

	assertLimitOneExhausted(t, ws, "dispatch-occupied")

	mustReserve(t, ws, "dispatch-occupied", 2)

	if !entryExists(t, filepath.Join(slotsPath, "dispatch-occupied-2.pending")) {
		t.Error("dispatch-occupied-2.pending is absent after the limit-2 reservation, want the pending file")
	}
	if entryExists(t, filepath.Join(slotsPath, "dispatch-occupied-2")) {
		t.Error("dispatch-occupied-2 exists before Commit, want only the pending file")
	}
}

func TestReserveNotificationSlot_ReleaseFreesTheSlotForReuse(t *testing.T) {
	t.Parallel()

	ws := newSlotWorkspace(t)

	const dispatchID = "dispatch-release"
	slot := mustReserve(t, ws, dispatchID, 1)

	slotPath := filepath.Join(notificationSlotsPath(ws), dispatchID+"-1")
	pendingPath := slotPath + slotPendingSuffix
	if !entryExists(t, pendingPath) || entryExists(t, slotPath) {
		t.Fatalf("before Release: pending exists = %v, slot exists = %v, want true, false", entryExists(t, pendingPath), entryExists(t, slotPath))
	}

	slot.Release()

	if entryExists(t, pendingPath) || entryExists(t, slotPath) {
		t.Fatalf("after Release: pending exists = %v, slot exists = %v, want false, false", entryExists(t, pendingPath), entryExists(t, slotPath))
	}

	mustReserve(t, ws, dispatchID, 1)
}

func TestNotificationSlot_CommitCountsTheSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		commit func(t *testing.T, slot *NotificationSlot, pendingPath string)
	}{
		{
			name:   "pending file present",
			commit: func(_ *testing.T, slot *NotificationSlot, _ string) { slot.Commit() },
		},
		{
			name: "pending file removed while the claim is held",
			commit: func(t *testing.T, slot *NotificationSlot, pendingPath string) {
				t.Helper()
				if err := os.Remove(pendingPath); err != nil {
					t.Fatalf("Remove(%q): %v", pendingPath, err)
				}
				slot.Commit()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ws := newSlotWorkspace(t)
			const dispatchID = "dispatch-commit"
			slot := mustReserve(t, ws, dispatchID, 1)
			slotPath := filepath.Join(notificationSlotsPath(ws), dispatchID+"-1")
			pendingPath := slotPath + slotPendingSuffix

			if !entryExists(t, pendingPath) {
				t.Fatalf("pending file %q is absent while the claim is held, want present", pendingPath)
			}
			assertLimitOneExhausted(t, ws, dispatchID)
			if entryExists(t, slotPath) {
				t.Fatalf("slot file %q exists before Commit, want absent", slotPath)
			}

			tt.commit(t, slot, pendingPath)

			if !entryExists(t, slotPath) {
				t.Errorf("slot file %q is absent after Commit, want present", slotPath)
			}
			if entryExists(t, pendingPath) {
				t.Errorf("pending file %q exists after Commit, want absent", pendingPath)
			}
			assertLimitOneExhausted(t, ws, dispatchID)
		})
	}
}

func TestNotificationSlot_SecondEndDoesNothing(t *testing.T) {
	t.Parallel()

	commit := (*NotificationSlot).Commit
	release := (*NotificationSlot).Release

	tests := []struct {
		name         string
		first        func(*NotificationSlot)
		second       func(*NotificationSlot)
		wantSlotFile bool
	}{
		{name: "commit then release", first: commit, second: release, wantSlotFile: true},
		{name: "commit then commit", first: commit, second: commit, wantSlotFile: true},
		{name: "release then commit", first: release, second: commit, wantSlotFile: false},
		{name: "release then release", first: release, second: release, wantSlotFile: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ws := newSlotWorkspace(t)
			const dispatchID = "dispatch-end-twice"
			logger, logged := newRecordingLogger()
			slot, reserved, err := ReserveNotificationSlot(ws, dispatchID, 1, logger)
			if err != nil || !reserved {
				t.Fatalf("ReserveNotificationSlot() = reserved=%v err=%v, want a claim", reserved, err)
			}
			slotPath := filepath.Join(notificationSlotsPath(ws), dispatchID+"-1")

			tt.first(slot)
			tt.second(slot)

			if got := entryExists(t, slotPath); got != tt.wantSlotFile {
				t.Errorf("slot file exists after both calls = %v, want %v", got, tt.wantSlotFile)
			}
			if msgs := logged.warnMessages(); len(msgs) != 0 {
				t.Errorf("warnings after both calls = %q, want none", msgs)
			}
		})
	}
}

func TestNotificationSlot_NilLoggerFallsBackToDefault(t *testing.T) {
	ws := newSlotWorkspace(t)
	logger, logged := newRecordingLogger()
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	const dispatchID = "dispatch-nil-logger"
	slot := mustReserve(t, ws, dispatchID, 1)
	pendingPath := filepath.Join(notificationSlotsPath(ws), dispatchID+"-1"+slotPendingSuffix)
	if err := os.Remove(pendingPath); err != nil {
		t.Fatalf("Remove(%q): %v", pendingPath, err)
	}

	slot.Release()

	if msgs := logged.warnMessages(); !slices.Equal(msgs, []string{"failed to release notification slot"}) {
		t.Errorf("default logger warnings = %q, want one \"failed to release notification slot\"", msgs)
	}
}

func runPlantedEntryCases(t *testing.T, tests []plantedEntryCase) {
	t.Helper()

	const dispatchID = "dispatch-planted"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ws := newSlotWorkspace(t)
			slotsPath := notificationSlotsPath(ws)
			if err := os.Mkdir(slotsPath, 0o750); err != nil {
				t.Fatalf("Mkdir(notification_slots): %v", err)
			}
			verify := tt.plant(t, filepath.Join(slotsPath, dispatchID+"-1"))

			if tt.occupies {
				assertLimitOneExhausted(t, ws, dispatchID)
				mustReserve(t, ws, dispatchID, 2)
				if !entryExists(t, filepath.Join(slotsPath, dispatchID+"-2"+slotPendingSuffix)) {
					t.Error("slot 2 has no pending file after the limit-2 reservation, want it claimed")
				}
			} else {
				mustReserve(t, ws, dispatchID, 1)
			}

			if verify != nil {
				verify(t)
			}
		})
	}
}

type plantedEntryCase struct {
	name     string
	plant    func(t *testing.T, slotPath string) (verify func(t *testing.T))
	occupies bool
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if string(got) != want {
		t.Errorf("content of %q = %q, want %q", path, got, want)
	}
}

func assertDirectoryAt(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	if !info.IsDir() {
		t.Errorf("%q mode = %v, want the planted directory left in place", path, info.Mode())
	}
}

func TestReserveNotificationSlot_PlantedEntries(t *testing.T) {
	t.Parallel()

	tests := []plantedEntryCase{
		{
			name:     "empty regular file at the slot file",
			occupies: true,
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				writeFixtureFile(t, slotPath, "")
				return nil
			},
		},
		{
			name:     "directory at the slot file",
			occupies: true,
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				if err := os.Mkdir(slotPath, 0o750); err != nil {
					t.Fatalf("Mkdir(%q): %v", slotPath, err)
				}
				return nil
			},
		},
		{
			name:     "directory at the lock file",
			occupies: true,
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				if err := os.Mkdir(slotPath+slotLockSuffix, 0o750); err != nil {
					t.Fatalf("Mkdir(lock): %v", err)
				}
				return func(t *testing.T) { assertDirectoryAt(t, slotPath+slotLockSuffix) }
			},
		},
		{
			name:     "hard link at the lock file to a file outside the slot directory",
			occupies: true,
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				target := filepath.Join(t.TempDir(), "outside")
				writeFixtureFile(t, target, "keep")
				if err := os.Link(target, slotPath+slotLockSuffix); err != nil {
					t.Fatalf("Link(lock): %v", err)
				}
				return func(t *testing.T) { assertFileContent(t, target, "keep") }
			},
		},
		{
			name:     "directory at the pending file",
			occupies: true,
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				if err := os.Mkdir(slotPath+slotPendingSuffix, 0o750); err != nil {
					t.Fatalf("Mkdir(pending): %v", err)
				}
				return func(t *testing.T) { assertDirectoryAt(t, slotPath+slotPendingSuffix) }
			},
		},
		{
			name: "plain file at the pending file",
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				writeFixtureFile(t, slotPath+slotPendingSuffix, "stale")
				return func(t *testing.T) { assertFileContent(t, slotPath+slotPendingSuffix, "") }
			},
		},
	}

	runPlantedEntryCases(t, tests)
}

func TestReserveNotificationSlot_LockUnavailableFallsBackToSlotFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		end          func(*NotificationSlot)
		wantSlotFile bool
	}{
		{name: "release", end: (*NotificationSlot).Release, wantSlotFile: false},
		{name: "commit", end: (*NotificationSlot).Commit, wantSlotFile: true},
	}

	refuseLock := func(*os.File) (bool, error) { return false, errors.New("locks are not supported") }

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ws := newSlotWorkspace(t)
			const dispatchID = "dispatch-fallback"
			logger, logged := newRecordingLogger()
			slotPath := filepath.Join(notificationSlotsPath(ws), dispatchID+"-1")

			slot, reserved, err := reserveNotificationSlot(ws, dispatchID, 1, logger, refuseLock)
			if slot != nil {
				endOnCleanup(t, slot)
			}
			if err != nil || !reserved {
				t.Fatalf("reserveNotificationSlot() = reserved=%v err=%v, want a claim", reserved, err)
			}

			if !entryExists(t, slotPath) {
				t.Error("slot file is absent before the claim ends, want the fallback claim to have created it")
			}
			if entryExists(t, slotPath+slotPendingSuffix) {
				t.Error("pending file exists for a fallback claim, want none")
			}
			if msgs := logged.warnMessages(); !slices.Equal(msgs, []string{"notification slot lock unavailable"}) {
				t.Errorf("warnings = %q, want one \"notification slot lock unavailable\"", msgs)
			}

			tt.end(slot)

			if got := entryExists(t, slotPath); got != tt.wantSlotFile {
				t.Errorf("slot file exists after the claim ends = %v, want %v", got, tt.wantSlotFile)
			}
			if tt.wantSlotFile {
				assertLimitOneExhausted(t, ws, dispatchID)
			} else {
				mustReserve(t, ws, dispatchID, 1)
			}
		})
	}
}

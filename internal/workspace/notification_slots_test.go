package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

func readNotificationSlotNames(t *testing.T, workspacePath, dispatchID string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(workspacePath, workspacekit.SortieDir, notificationSlotsDir))
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

func TestReserveNotificationSlot_ConcurrencyRespectsLimit(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	createSortieDir(t, ws)

	const (
		workers    = 32
		limit      = 5
		dispatchID = "dispatch-concurrency"
	)

	var wg sync.WaitGroup
	var reservedCount atomic.Int64
	for range workers {
		wg.Go(func() {
			_, reserved, err := ReserveNotificationSlot(ws, dispatchID, limit, nil)
			if err != nil {
				t.Errorf("ReserveNotificationSlot: unexpected error: %v", err)
				return
			}
			if reserved {
				reservedCount.Add(1)
			}
		})
	}
	wg.Wait()

	if got := reservedCount.Load(); got != limit {
		t.Errorf("ReserveNotificationSlot concurrent reservations = %d, want %d", got, limit)
	}

	names := readNotificationSlotNames(t, ws, dispatchID)
	if len(names) != limit {
		t.Errorf("notification_slots contains %d entries for %q, want %d", len(names), dispatchID, limit)
	}
}

func TestReserveNotificationSlot_FencesByDispatch(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	createSortieDir(t, ws)

	const limit = 2
	for range limit {
		_, reserved, err := ReserveNotificationSlot(ws, "dispatch-a", limit, nil)
		if err != nil || !reserved {
			t.Fatalf("ReserveNotificationSlot(dispatch-a) = reserved=%v err=%v, want reserved=true err=nil", reserved, err)
		}
	}
	before := readNotificationSlotNames(t, ws, "dispatch-a")

	release, reserved, err := ReserveNotificationSlot(ws, "dispatch-b", limit, nil)
	if err != nil {
		t.Fatalf("ReserveNotificationSlot(dispatch-b) error = %v, want nil", err)
	}
	if !reserved {
		t.Fatal("ReserveNotificationSlot(dispatch-b) reserved = false, want true")
	}
	t.Cleanup(release)

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
				ws := t.TempDir()
				createSortieDir(t, ws)
				return ws, ""
			},
		},
		{
			name: "dispatch id differs from its own sanitized key",
			setup: func(t *testing.T) (string, string) {
				ws := t.TempDir()
				createSortieDir(t, ws)
				return ws, "a/b"
			},
		},
		{
			name: "dispatch id is a path traversal shape",
			setup: func(t *testing.T) (string, string) {
				ws := t.TempDir()
				createSortieDir(t, ws)
				return ws, "../x"
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
				ws := t.TempDir()
				createSortieDir(t, ws)
				if err := os.WriteFile(filepath.Join(ws, workspacekit.SortieDir, notificationSlotsDir), []byte("x"), 0o600); err != nil {
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

			release, reserved, err := ReserveNotificationSlot(workspacePath, dispatchID, 2, nil)
			if err == nil {
				t.Fatalf("ReserveNotificationSlot(%q, %q) error = nil, want non-nil", workspacePath, dispatchID)
			}
			if reserved {
				t.Errorf("ReserveNotificationSlot(%q, %q) reserved = true, want false", workspacePath, dispatchID)
			}
			if release != nil {
				t.Errorf("ReserveNotificationSlot(%q, %q) release = non-nil, want nil", workspacePath, dispatchID)
			}
		})
	}
}

func TestReserveNotificationSlot_ExistingDirectoryCountsAsOccupied(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	createSortieDir(t, ws)
	slotsPath := filepath.Join(ws, workspacekit.SortieDir, notificationSlotsDir)
	if err := os.MkdirAll(slotsPath, 0o750); err != nil {
		t.Fatalf("MkdirAll(notification_slots): %v", err)
	}
	if err := os.Mkdir(filepath.Join(slotsPath, "dispatch-occupied-1"), 0o750); err != nil {
		t.Fatalf("Mkdir(dispatch-occupied-1): %v", err)
	}

	release, reserved, err := ReserveNotificationSlot(ws, "dispatch-occupied", 1, nil)
	if err != nil {
		t.Fatalf("ReserveNotificationSlot(limit=1) error = %v, want nil", err)
	}
	if reserved {
		t.Error("ReserveNotificationSlot(limit=1) reserved = true, want false (slot 1 occupied by a directory)")
	}
	if release != nil {
		t.Error("ReserveNotificationSlot(limit=1) release = non-nil, want nil")
	}

	release2, reserved2, err2 := ReserveNotificationSlot(ws, "dispatch-occupied", 2, nil)
	if err2 != nil {
		t.Fatalf("ReserveNotificationSlot(limit=2) error = %v, want nil", err2)
	}
	if !reserved2 {
		t.Fatal("ReserveNotificationSlot(limit=2) reserved = false, want true (slot 2 free)")
	}
	t.Cleanup(release2)
	if _, err := os.Lstat(filepath.Join(slotsPath, "dispatch-occupied-2")); err != nil {
		t.Errorf("Lstat(dispatch-occupied-2) = %v, want the slot file to exist", err)
	}
}

func TestReserveNotificationSlot_ReleaseFreesTheSlotForReuse(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	createSortieDir(t, ws)

	const dispatchID = "dispatch-release"
	release, reserved, err := ReserveNotificationSlot(ws, dispatchID, 1, nil)
	if err != nil || !reserved {
		t.Fatalf("ReserveNotificationSlot(first) = reserved=%v err=%v, want reserved=true err=nil", reserved, err)
	}

	slotPath := filepath.Join(ws, workspacekit.SortieDir, notificationSlotsDir, dispatchID+"-1")
	if _, err := os.Lstat(slotPath); err != nil {
		t.Fatalf("Lstat(%q) before release = %v, want the slot file to exist", slotPath, err)
	}

	release()

	if _, err := os.Lstat(slotPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(%q) after release = %v, want fs.ErrNotExist", slotPath, err)
	}

	release2, reserved2, err2 := ReserveNotificationSlot(ws, dispatchID, 1, nil)
	if err2 != nil {
		t.Fatalf("ReserveNotificationSlot(second) error = %v, want nil", err2)
	}
	if !reserved2 {
		t.Fatal("ReserveNotificationSlot(second) reserved = false, want true (limit 1, freed by release)")
	}
	t.Cleanup(release2)
}

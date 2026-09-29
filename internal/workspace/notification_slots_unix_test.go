//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

func assertReserveRefusedNoSlot(t *testing.T, workspacePath, target string) {
	t.Helper()

	slot, reserved, err := ReserveNotificationSlot(workspacePath, "dispatch-1", 2, nil)
	if slot != nil {
		endOnCleanup(t, slot)
	}
	if err == nil {
		t.Fatal("ReserveNotificationSlot error = nil, want non-nil")
	}
	if reserved {
		t.Error("ReserveNotificationSlot reserved = true, want false")
	}
	if slot != nil {
		t.Error("ReserveNotificationSlot slot = non-nil, want nil")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir(target): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("symlink target directory gained entries: %v, want none", entries)
	}
}

func makeSlotsDirReadOnly(t *testing.T, slotsPath string) {
	t.Helper()
	if err := os.Chmod(slotsPath, 0o555); err != nil {
		t.Fatalf("Chmod(notification_slots): %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotsPath, 0o750) })
}

func TestReserveNotificationSlot_DirectoryPermissionsDenyCreate(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("skipping: test requires non-root to enforce directory permissions")
	}

	tests := []struct {
		name  string
		plant func(t *testing.T, slotsPath string)
	}{
		{name: "no lock file", plant: func(*testing.T, string) {}},
		{
			name: "lock file only",
			plant: func(t *testing.T, slotsPath string) {
				writeFixtureFile(t, filepath.Join(slotsPath, "dispatch-1-1"+slotLockSuffix), "")
			},
		},
		{
			name: "lock file and plain pending file",
			plant: func(t *testing.T, slotsPath string) {
				writeFixtureFile(t, filepath.Join(slotsPath, "dispatch-1-1"+slotLockSuffix), "")
				writeFixtureFile(t, filepath.Join(slotsPath, "dispatch-1-1"+slotPendingSuffix), "")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ws := newSlotWorkspace(t)
			slotsPath := notificationSlotsPath(ws)
			if err := os.Mkdir(slotsPath, 0o750); err != nil {
				t.Fatalf("Mkdir(notification_slots): %v", err)
			}
			tt.plant(t, slotsPath)
			makeSlotsDirReadOnly(t, slotsPath)

			slot, reserved, err := ReserveNotificationSlot(ws, "dispatch-1", 2, nil)
			if slot != nil {
				endOnCleanup(t, slot)
			}

			if err == nil {
				t.Error("ReserveNotificationSlot error = nil, want non-nil")
			}
			if reserved {
				t.Error("ReserveNotificationSlot reserved = true, want false")
			}
			if slot != nil {
				t.Error("ReserveNotificationSlot slot = non-nil, want nil")
			}
			for _, name := range []string{"dispatch-1-1", "dispatch-1-2"} {
				if entryExists(t, filepath.Join(slotsPath, name)) {
					t.Errorf("slot file %q exists after a refused reservation, want absent", name)
				}
			}
		})
	}
}

func TestNotificationSlot_CommitAfterDirectoryTurnsReadOnly(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("skipping: test requires non-root to enforce directory permissions")
	}

	ws := newSlotWorkspace(t)
	logger, logged := newRecordingLogger()
	slot, reserved, err := ReserveNotificationSlot(ws, "dispatch-1", 1, logger)
	if err != nil || !reserved {
		t.Fatalf("ReserveNotificationSlot() = reserved=%v err=%v, want a claim", reserved, err)
	}
	endOnCleanup(t, slot)
	slotsPath := notificationSlotsPath(ws)
	makeSlotsDirReadOnly(t, slotsPath)

	slot.Commit()

	if msgs := logged.warnMessages(); !slices.Equal(msgs, []string{"failed to record notification slot"}) {
		t.Errorf("warnings after Commit = %q, want one \"failed to record notification slot\"", msgs)
	}
	if entryExists(t, filepath.Join(slotsPath, "dispatch-1-1")) {
		t.Error("slot file exists after a Commit the directory refused, want absent")
	}

	next, reserved, err := ReserveNotificationSlot(ws, "dispatch-1", 1, nil)
	if next != nil {
		endOnCleanup(t, next)
	}
	if err == nil {
		t.Error("ReserveNotificationSlot after the refused Commit error = nil, want non-nil")
	}
	if reserved {
		t.Error("ReserveNotificationSlot after the refused Commit reserved = true, want false")
	}
}

func TestReserveNotificationSlot_PlantedLinkAndPipeEntries(t *testing.T) {
	t.Parallel()

	symlinkTo := func(linkSuffix string, target func(t *testing.T, slotPath string) string, verify func(t *testing.T, target string)) func(t *testing.T, slotPath string) func(*testing.T) {
		return func(t *testing.T, slotPath string) func(*testing.T) {
			targetPath := target(t, slotPath)
			linkPath := slotPath + linkSuffix
			mustSymlink(t, targetPath, linkPath)
			return func(t *testing.T) {
				verify(t, targetPath)
				if info, err := os.Lstat(linkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("Lstat(%q) = %v, %v, want the planted symbolic link left in place", linkPath, info, err)
				}
			}
		}
	}
	outsideFile := func(t *testing.T, _ string) string {
		target := filepath.Join(t.TempDir(), "outside")
		writeFixtureFile(t, target, "keep")
		return target
	}
	insideFile := func(t *testing.T, slotPath string) string {
		target := filepath.Join(filepath.Dir(slotPath), "decoy")
		writeFixtureFile(t, target, "keep")
		return target
	}
	danglingTarget := func(_ *testing.T, slotPath string) string {
		return filepath.Join(filepath.Dir(slotPath), "dangling-target")
	}
	untouchedFile := func(t *testing.T, target string) { assertFileContent(t, target, "keep") }
	stillAbsent := func(t *testing.T, target string) {
		if entryExists(t, target) {
			t.Errorf("entry appeared at the link target %q, want none", target)
		}
	}

	tests := []plantedEntryCase{
		{name: "symbolic link at the slot file", occupies: true, plant: symlinkTo("", outsideFile, untouchedFile)},
		{name: "symbolic link at the lock file to a file in the slot directory", occupies: true, plant: symlinkTo(slotLockSuffix, insideFile, untouchedFile)},
		{name: "dangling symbolic link at the lock file", occupies: true, plant: symlinkTo(slotLockSuffix, danglingTarget, stillAbsent)},
		{
			name:     "named pipe at the lock file",
			occupies: true,
			plant: func(t *testing.T, slotPath string) func(*testing.T) {
				if err := syscall.Mkfifo(slotPath+slotLockSuffix, 0o600); err != nil {
					t.Fatalf("Mkfifo(lock): %v", err)
				}
				return nil
			},
		},
		{name: "symbolic link at the pending file", occupies: true, plant: symlinkTo(slotPendingSuffix, outsideFile, untouchedFile)},
	}

	runPlantedEntryCases(t, tests)
}

func TestReserveNotificationSlot_DotSortieIsSymlink(t *testing.T) {
	t.Parallel()

	t.Run("target inside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		target := filepath.Join(ws, "real-sortie")
		if err := os.Mkdir(target, 0o750); err != nil {
			t.Fatalf("Mkdir(target): %v", err)
		}
		if err := os.Symlink(target, filepath.Join(ws, workspacekit.SortieDir)); err != nil {
			t.Fatalf("Symlink(.sortie): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})

	t.Run("target outside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(ws, workspacekit.SortieDir)); err != nil {
			t.Fatalf("Symlink(.sortie): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})
}

func TestReserveNotificationSlot_NotificationSlotsIsSymlink(t *testing.T) {
	t.Parallel()

	t.Run("target inside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		createSortieDir(t, ws)
		target := filepath.Join(ws, "real-notification-slots")
		if err := os.Mkdir(target, 0o750); err != nil {
			t.Fatalf("Mkdir(target): %v", err)
		}
		if err := os.Symlink(target, filepath.Join(ws, workspacekit.SortieDir, notificationSlotsDir)); err != nil {
			t.Fatalf("Symlink(notification_slots): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})

	t.Run("target outside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		createSortieDir(t, ws)
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(ws, workspacekit.SortieDir, notificationSlotsDir)); err != nil {
			t.Fatalf("Symlink(notification_slots): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})
}

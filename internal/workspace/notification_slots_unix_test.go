//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func assertReserveRefusedNoSlot(t *testing.T, workspacePath, target string) {
	t.Helper()

	release, reserved, err := ReserveNotificationSlot(workspacePath, "dispatch-1", 2, nil)
	if err == nil {
		t.Fatal("ReserveNotificationSlot error = nil, want non-nil")
	}
	if reserved {
		t.Error("ReserveNotificationSlot reserved = true, want false")
	}
	if release != nil {
		t.Error("ReserveNotificationSlot release = non-nil, want nil")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir(target): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("symlink target directory gained entries: %v, want none", entries)
	}
}

func TestReserveNotificationSlot_DirectoryPermissionsDenyCreate(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("skipping: test requires non-root to enforce directory permissions")
	}

	ws := t.TempDir()
	createSortieDir(t, ws)
	slotsPath := filepath.Join(ws, sortieDir, notificationSlotsDir)
	if err := os.Mkdir(slotsPath, 0o750); err != nil {
		t.Fatalf("Mkdir(notification_slots): %v", err)
	}
	if err := os.Chmod(slotsPath, 0o555); err != nil {
		t.Fatalf("Chmod(notification_slots): %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotsPath, 0o750) })

	release, reserved, err := ReserveNotificationSlot(ws, "dispatch-1", 2, nil)
	if err == nil {
		t.Fatal("ReserveNotificationSlot error = nil, want non-nil")
	}
	if reserved {
		t.Error("ReserveNotificationSlot reserved = true, want false")
	}
	if release != nil {
		t.Error("ReserveNotificationSlot release = non-nil, want nil")
	}
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
		if err := os.Symlink(target, filepath.Join(ws, sortieDir)); err != nil {
			t.Fatalf("Symlink(.sortie): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})

	t.Run("target outside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(ws, sortieDir)); err != nil {
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
		if err := os.Symlink(target, filepath.Join(ws, sortieDir, notificationSlotsDir)); err != nil {
			t.Fatalf("Symlink(notification_slots): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})

	t.Run("target outside workspace", func(t *testing.T) {
		t.Parallel()

		ws := t.TempDir()
		createSortieDir(t, ws)
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(ws, sortieDir, notificationSlotsDir)); err != nil {
			t.Fatalf("Symlink(notification_slots): %v", err)
		}

		assertReserveRefusedNoSlot(t, ws, target)
	})
}

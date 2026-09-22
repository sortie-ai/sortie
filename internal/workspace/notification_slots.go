package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
)

const notificationSlotsDir = "notification_slots"

// ReserveNotificationSlot claims the lowest free notification slot for
// dispatchID among 1..limit under workspacePath's
// .sortie/notification_slots directory, creating that directory on first
// use. It returns (release, true, nil) on a successful claim, (nil,
// false, nil) when every slot in 1..limit already exists, and (nil,
// false, err) when workspacePath, dispatchID, or the .sortie tree is
// unusable. It never logs the error it returns; the caller does.
//
// ReserveNotificationSlot is safe for concurrent callers across any
// number of goroutines and processes: a claim is an exclusive file
// create, so no count is ever read and then written. The returned
// release removes only the slot this call claimed, is safe to call at
// most once, and logs its own failure through logger instead of
// returning it.
func ReserveNotificationSlot(workspacePath, dispatchID string, limit int, logger *slog.Logger) (release func(), reserved bool, err error) {
	if workspacePath == "" {
		return nil, false, errors.New("workspace path must not be empty")
	}
	key, keyErr := SanitizeKey(dispatchID)
	if keyErr != nil || key != dispatchID {
		return nil, false, fmt.Errorf("dispatch id %q is not a valid workspace key", dispatchID)
	}

	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		return nil, false, fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close() //nolint:errcheck // The root is only read from and created under here, so a close failure leaves nothing to recover.

	if err := requireRealDirectory(root, sortieDir); err != nil {
		return nil, false, fmt.Errorf("%s directory: %w", sortieDir, err)
	}

	slotsDir := sortieDir + "/" + notificationSlotsDir
	if mkdirErr := root.Mkdir(slotsDir, 0o750); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
		return nil, false, fmt.Errorf("create %s directory: %w", slotsDir, mkdirErr)
	}
	if err := requireRealDirectory(root, slotsDir); err != nil {
		return nil, false, fmt.Errorf("%s directory: %w", slotsDir, err)
	}

	for n := 1; n <= limit; n++ {
		name := slotsDir + "/" + dispatchID + "-" + strconv.Itoa(n)
		f, createErr := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			_ = f.Close() //nolint:errcheck // The exclusive create is the claim; a close failure does not undo it.
			return releaseNotificationSlot(workspacePath, name, logger), true, nil
		}
		if errors.Is(createErr, fs.ErrExist) {
			continue
		}
		// Windows reports an exclusive create over an existing directory as
		// EISDIR rather than ErrExist; a stat confirms occupancy regardless
		// of what already sits at name.
		if _, statErr := root.Lstat(name); statErr == nil {
			continue
		}
		return nil, false, fmt.Errorf("create notification slot %q: %w", name, createErr)
	}
	return nil, false, nil
}

// requireRealDirectory returns an error unless name resolves, inside
// root, to a directory that is not a symbolic link. An absent name
// surfaces the underlying stat error unchanged.
func requireRealDirectory(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", name)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", name)
	}
	return nil
}

// releaseNotificationSlot returns the release closure [ReserveNotificationSlot]
// returns for the slot claimed at name; a removal failure is logged through
// logger rather than returned, leaving the slot in place.
func releaseNotificationSlot(workspacePath, name string, logger *slog.Logger) func() {
	return func() {
		if logger == nil {
			logger = slog.Default()
		}
		if err := removeNotificationSlot(workspacePath, name); err != nil {
			logger.Warn("failed to release notification slot",
				slog.String("workspace", workspacePath),
				slog.Any("error", err))
		}
	}
}

// removeNotificationSlot removes the regular file at name after applying
// the same containment and symlink checks [ReserveNotificationSlot] uses.
func removeNotificationSlot(workspacePath, name string) error {
	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		return fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close() //nolint:errcheck // The root is only read from and removed under here, so a close failure leaves nothing to recover.

	if err := requireRealDirectory(root, sortieDir); err != nil {
		return fmt.Errorf("%s directory: %w", sortieDir, err)
	}
	slotsDir := sortieDir + "/" + notificationSlotsDir
	if err := requireRealDirectory(root, slotsDir); err != nil {
		return fmt.Errorf("%s directory: %w", slotsDir, err)
	}

	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("stat notification slot %q: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("notification slot %q is not a regular file", name)
	}
	return root.Remove(name)
}

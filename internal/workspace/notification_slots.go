package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"

	"github.com/sortie-ai/sortie/internal/workspacekit"
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

	sortieRoot, err := workspacekit.OpenSortieDir(workspacePath, false)
	if err != nil {
		return nil, false, fmt.Errorf("open sortie directory: %w", err)
	}
	defer sortieRoot.Close() //nolint:errcheck // the handle is not needed once the slots directory is open

	slotsRoot, err := workspacekit.OpenSubdir(sortieRoot, notificationSlotsDir, true)
	if err != nil {
		return nil, false, fmt.Errorf("%s directory: %w", notificationSlotsDir, err)
	}
	defer slotsRoot.Close() //nolint:errcheck // the handle is not needed once the claim loop returns

	for n := 1; n <= limit; n++ {
		name := dispatchID + "-" + strconv.Itoa(n)
		f, createErr := slotsRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
		if _, statErr := slotsRoot.Lstat(name); statErr == nil {
			continue
		}
		return nil, false, fmt.Errorf("create notification slot %q: %w", name, createErr)
	}
	return nil, false, nil
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
	sortieRoot, err := workspacekit.OpenSortieDir(workspacePath, false)
	if err != nil {
		return fmt.Errorf("open sortie directory: %w", err)
	}
	defer sortieRoot.Close() //nolint:errcheck // the handle is not needed once the slots directory is open

	slotsRoot, err := workspacekit.OpenSubdir(sortieRoot, notificationSlotsDir, false)
	if err != nil {
		return fmt.Errorf("%s directory: %w", notificationSlotsDir, err)
	}
	defer slotsRoot.Close() //nolint:errcheck // the handle is not needed once the removal returns

	return workspacekit.RemoveFile(slotsRoot, name)
}

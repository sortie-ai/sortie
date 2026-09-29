package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

const (
	notificationSlotsDir = "notification_slots"
	slotLockSuffix       = ".lock"
	slotPendingSuffix    = ".pending"
)

// heldSlotLocks keys the slot lock files this process holds a claim on.
// NFS grants a flock lock to the whole process rather than to one open file,
// and closing any open file drops that lock, so a claim must not lock, or
// even open, a lock file its own process already holds.
var heldSlotLocks = struct {
	sync.Mutex
	keys map[string]struct{}
}{keys: map[string]struct{}{}}

// holdSlotLock records key as held and reports false when it already is.
func holdSlotLock(key string) bool {
	heldSlotLocks.Lock()
	defer heldSlotLocks.Unlock()
	if _, held := heldSlotLocks.keys[key]; held {
		return false
	}
	heldSlotLocks.keys[key] = struct{}{}
	return true
}

// forgetSlotLock must run only after the lock file is closed.
func forgetSlotLock(key string) {
	heldSlotLocks.Lock()
	defer heldSlotLocks.Unlock()
	delete(heldSlotLocks.keys, key)
}

// tryLockFunc takes an exclusive lock on the file without waiting. It
// reports locked == false with a nil error when another open file holds the
// lock, and a non-nil error when the filesystem cannot grant it.
type tryLockFunc func(f *os.File) (locked bool, err error)

// slotAttempt is the outcome of one try at claiming a slot under its lock.
type slotAttempt int

const (
	slotClaimed slotAttempt = iota
	slotOccupied
	slotLockRefused
)

// NotificationSlot is a claim on one notification slot that ends through
// [NotificationSlot.Commit] or [NotificationSlot.Release].
//
// While a locked claim lives, its process holds an operating-system lock, so
// the slot frees itself when the process ends however it ends. Every holder
// MUST end every slot it obtains: a slot nobody ends keeps its lock until
// the garbage collector finalizes its file. A NotificationSlot is not safe
// for concurrent use.
type NotificationSlot struct {
	slots     *os.Root
	lock      *os.File // nil for a fallback claim
	lockKey   string
	name      string
	workspace string
	logger    *slog.Logger
	ended     bool
}

// Commit records the slot as counted and ends the claim. The first call of
// Commit or [NotificationSlot.Release] ends the claim and any later call of
// either does nothing.
//
// A locked claim makes the slot file exist before it drops the lock. When
// the slot file cannot be written, Commit logs the failure and the slot
// stays free.
func (s *NotificationSlot) Commit() {
	if s.ended {
		return
	}
	s.ended = true
	if s.lock != nil {
		s.recordSlotFile()
		dropLock(s.lock)
		forgetSlotLock(s.lockKey)
	}
	_ = s.slots.Close() //nolint:errcheck // the handle is not needed once the claim ends
}

// Release ends the claim without counting the slot, freeing it for a later
// call. The first call of [NotificationSlot.Commit] or Release ends the
// claim and any later call of either does nothing.
//
// A removal failure is logged rather than returned; a locked claim then
// leaves its pending file for the next claim of the slot to replace, and a
// fallback claim leaves its slot counted.
func (s *NotificationSlot) Release() {
	if s.ended {
		return
	}
	s.ended = true
	if s.lock != nil {
		s.removeEntry(s.name + slotPendingSuffix)
		dropLock(s.lock)
		forgetSlotLock(s.lockKey)
	} else {
		s.removeEntry(s.name)
	}
	_ = s.slots.Close() //nolint:errcheck // the handle is not needed once the claim ends
}

// recordSlotFile turns the pending file into the slot file, creating the
// slot file when the rename fails.
func (s *NotificationSlot) recordSlotFile() {
	renameErr := s.slots.Rename(s.name+slotPendingSuffix, s.name)
	if renameErr == nil {
		return
	}
	if found, _ := entryAt(s.slots, s.name); found {
		return
	}
	if _, _, createErr := createExclusive(s.slots, s.name, os.O_WRONLY, false); createErr != nil {
		s.logger.Warn("failed to record notification slot",
			slog.String("workspace", s.workspace),
			slog.Any("error", errors.Join(renameErr, createErr)))
	}
}

func (s *NotificationSlot) removeEntry(name string) {
	if err := workspacekit.RemoveFile(s.slots, name); err != nil {
		s.logger.Warn("failed to release notification slot",
			slog.String("workspace", s.workspace),
			slog.Any("error", err))
	}
}

// ReserveNotificationSlot claims the lowest unoccupied notification slot for
// dispatchID among 1..limit under workspacePath's .sortie/notification_slots
// directory, creating that directory on first use. It returns (slot, true,
// nil) on a claim, (nil, false, nil) when every slot in 1..limit is
// occupied, and (nil, false, err) when workspacePath, dispatchID, the
// .sortie tree, or the slot directory's entries are unusable. It never logs
// the error it returns; the caller does. A nil logger selects
// [slog.Default].
//
// A slot is occupied when its slot file exists, when another open file holds
// its lock, or when its lock file or pending file is an entry the claim
// refuses to touch. A claim never waits, and it holds no slot for a process
// that has ended. When the filesystem cannot grant a lock, the call logs one
// warning and claims by creating the slot file, and a claim whose process ends
// before it resolves then stays counted.
//
// ReserveNotificationSlot is safe for concurrent callers across any number
// of goroutines and processes. The caller MUST end the returned slot.
func ReserveNotificationSlot(workspacePath, dispatchID string, limit int, logger *slog.Logger) (slot *NotificationSlot, reserved bool, err error) {
	return reserveNotificationSlot(workspacePath, dispatchID, limit, logger, tryLockFile)
}

func reserveNotificationSlot(workspacePath, dispatchID string, limit int, logger *slog.Logger, tryLock tryLockFunc) (*NotificationSlot, bool, error) {
	if logger == nil {
		logger = slog.Default()
	}
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

	slot, err := claimLowestSlot(slotsRoot, canonicalWorkspace(workspacePath), workspacePath, dispatchID, limit, logger, tryLock)
	if slot == nil {
		_ = slotsRoot.Close() //nolint:errcheck // the handle is not needed once no claim owns it
	}
	if err != nil {
		return nil, false, err
	}
	return slot, slot != nil, nil
}

// canonicalWorkspace names workspacePath alike for every spelling of it, so
// that one process sees its own slot locks under any of them.
func canonicalWorkspace(workspacePath string) string {
	dir, err := filepath.Abs(workspacePath)
	if err != nil {
		dir = workspacePath
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

// claimLowestSlot returns a claim that owns slots, or nil without an error
// when every slot is occupied. workspaceKey is the canonical workspacePath.
func claimLowestSlot(slots *os.Root, workspaceKey, workspacePath, dispatchID string, limit int, logger *slog.Logger, tryLock tryLockFunc) (*NotificationSlot, error) {
	fallback := false
	for n := 1; n <= limit; n++ {
		name := dispatchID + "-" + strconv.Itoa(n)
		counted, err := entryAt(slots, name)
		if err != nil {
			return nil, err
		}
		if counted {
			continue
		}

		if !fallback {
			slot, attempt, err := claimUnderLock(slots, workspaceKey, workspacePath, name, logger, tryLock)
			if err != nil {
				return nil, err
			}
			switch attempt {
			case slotClaimed:
				return slot, nil
			case slotOccupied:
				continue
			case slotLockRefused:
				fallback = true
			}
		}

		_, exists, err := createExclusive(slots, name, os.O_WRONLY, false)
		if err != nil {
			return nil, fmt.Errorf("create notification slot %q: %w", name, err)
		}
		if exists {
			continue
		}
		return &NotificationSlot{slots: slots, name: name, workspace: workspacePath, logger: logger}, nil
	}
	return nil, nil
}

// claimUnderLock tries to claim the slot at name through its lock file. The
// slot file is checked again once the lock is held: a claim that committed
// between the first check and the lock leaves a slot file behind.
func claimUnderLock(slots *os.Root, workspaceKey, workspacePath, name string, logger *slog.Logger, tryLock tryLockFunc) (slot *NotificationSlot, attempt slotAttempt, err error) {
	lockKey := filepath.Join(workspaceKey, name)
	if !holdSlotLock(lockKey) {
		return nil, slotOccupied, nil
	}
	defer func() {
		if slot == nil {
			forgetSlotLock(lockKey)
		}
	}()

	lock, occupied, err := openLockFile(slots, name+slotLockSuffix)
	if err != nil {
		return nil, slotOccupied, err
	}
	if occupied {
		return nil, slotOccupied, nil
	}

	locked, lockErr := tryLock(lock)
	if lockErr != nil {
		_ = lock.Close() //nolint:errcheck // the lock was never taken
		logger.Warn("notification slot lock unavailable",
			slog.String("workspace", workspacePath),
			slog.Any("error", lockErr))
		return nil, slotLockRefused, nil
	}
	if !locked {
		_ = lock.Close() //nolint:errcheck // the lock was never taken
		return nil, slotOccupied, nil
	}

	counted, err := entryAt(slots, name)
	if err != nil {
		dropLock(lock)
		return nil, slotOccupied, err
	}
	if counted {
		dropLock(lock)
		return nil, slotOccupied, nil
	}

	placed, err := placePendingFile(slots, name+slotPendingSuffix)
	if err != nil {
		dropLock(lock)
		return nil, slotOccupied, err
	}
	if !placed {
		dropLock(lock)
		return nil, slotOccupied, nil
	}
	return &NotificationSlot{slots: slots, lock: lock, lockKey: lockKey, name: name, workspace: workspacePath, logger: logger}, slotClaimed, nil
}

// openLockFile returns the lock file at lockName, creating it when absent.
// occupied reports an existing entry the claim refuses to open: a link, a
// non-plain entry, one replaced during the open, or a file with more than
// one link. A refused entry is never locked or written.
func openLockFile(slots *os.Root, lockName string) (lock *os.File, occupied bool, err error) {
	lock, exists, err := createExclusive(slots, lockName, os.O_RDWR, true)
	if err != nil {
		return nil, false, fmt.Errorf("create notification slot lock %q: %w", lockName, err)
	}
	if !exists {
		return lock, false, nil
	}

	lock, err = workspacekit.OpenPlainFile(slots, lockName, os.O_RDWR)
	switch {
	case err == nil:
		return lock, false, nil
	case errors.Is(err, workspacekit.ErrLink),
		errors.Is(err, workspacekit.ErrNotPlainFile),
		errors.Is(err, workspacekit.ErrChanged),
		errors.Is(err, workspacekit.ErrLinkCount):
		return nil, true, nil
	default:
		return nil, false, fmt.Errorf("open notification slot lock %q: %w", lockName, err)
	}
}

// placePendingFile creates the pending file at pendingName while the caller
// holds the slot's lock, so a pending file already there was left by a claim
// whose process ended, or planted. It is removed and created again rather
// than reused, which keeps every claim an exclusive create. placed is false
// when the entry at pendingName is one the claim refuses to remove.
func placePendingFile(slots *os.Root, pendingName string) (placed bool, err error) {
	_, exists, err := createExclusive(slots, pendingName, os.O_WRONLY, false)
	if err != nil {
		return false, fmt.Errorf("create notification slot pending file %q: %w", pendingName, err)
	}
	if !exists {
		return true, nil
	}

	removeErr := workspacekit.RemoveFile(slots, pendingName)
	switch {
	case errors.Is(removeErr, workspacekit.ErrLink), errors.Is(removeErr, workspacekit.ErrNotPlainFile):
		return false, nil
	case removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist):
		return false, fmt.Errorf("remove stale notification slot pending file %q: %w", pendingName, removeErr)
	}

	_, exists, err = createExclusive(slots, pendingName, os.O_WRONLY, false)
	if err != nil {
		return false, fmt.Errorf("create notification slot pending file %q: %w", pendingName, err)
	}
	if exists {
		return false, fmt.Errorf("notification slot pending file %q reappeared after removal", pendingName)
	}
	return true, nil
}

// createExclusive creates name with mode 0o600 and never follows a link.
// The file is returned open only when keepOpen is set. exists reports an
// entry already at name, with a nil error.
func createExclusive(slots *os.Root, name string, flag int, keepOpen bool) (f *os.File, exists bool, err error) {
	f, err = slots.OpenFile(name, flag|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if keepOpen {
			return f, false, nil
		}
		_ = f.Close() //nolint:errcheck // The exclusive create is what matters; a close failure does not undo it.
		return nil, false, nil
	}
	if errors.Is(err, fs.ErrExist) {
		return nil, true, nil
	}
	// Windows reports an exclusive create over an existing directory as
	// EISDIR rather than ErrExist; a stat confirms occupancy regardless of
	// what already sits at name.
	if _, statErr := slots.Lstat(name); statErr == nil {
		return nil, true, nil
	}
	return nil, false, err
}

// entryAt reports whether any entry, of any type, sits at name.
func entryAt(slots *os.Root, name string) (found bool, err error) {
	_, err = slots.Lstat(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat notification slot %q: %w", name, err)
	}
}

// dropLock unlocks f before closing it, because Windows releases the lock of
// a file closed without an unlock only after a delay it does not bound.
func dropLock(f *os.File) {
	unlockFile(f)
	_ = f.Close() //nolint:errcheck // closing drops the lock; a failure changes nothing
}

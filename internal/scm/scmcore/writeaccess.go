package scmcore

import (
	"context"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
)

// WriteAccessLookup answers, without caching, whether the account behind
// authorID has write access to the project.
type WriteAccessLookup func(ctx context.Context, authorID string) (bool, error)

// WriteAccessCache remembers write-access answers over a
// [WriteAccessLookup]. A grant holds for the cache's lifetime and a
// refusal for the one comment it was asked about; a failed lookup is
// remembered as neither. Concurrent misses for one author each send a
// lookup. It is safe for concurrent use.
type WriteAccessCache struct {
	// mu guards both maps and is never held across a lookup, which is a
	// network request.
	mu      sync.Mutex
	lookup  WriteAccessLookup
	granted map[string]struct{} // keyed by author ID
	refused map[string]struct{} // keyed by comment ID
}

// NewWriteAccessCache returns an empty cache that answers misses through
// lookup.
func NewWriteAccessCache(lookup WriteAccessLookup) *WriteAccessCache {
	return &WriteAccessCache{
		lookup:  lookup,
		granted: make(map[string]struct{}),
		refused: make(map[string]struct{}),
	}
}

// CanWrite reports whether the author of c has write access. An empty
// AuthorID is refused without a lookup. A lookup error is returned with
// false and nothing is remembered.
func (w *WriteAccessCache) CanWrite(ctx context.Context, c domain.Comment) (bool, error) {
	if c.AuthorID == "" {
		return false, nil
	}

	w.mu.Lock()
	// The grant check comes first so a comment refused before a later
	// comment granted its author reads as granted.
	if _, ok := w.granted[c.AuthorID]; ok {
		w.mu.Unlock()
		return true, nil
	}
	if _, ok := w.refused[c.ID]; ok {
		w.mu.Unlock()
		return false, nil
	}
	w.mu.Unlock()

	ok, err := w.lookup(ctx, c.AuthorID)
	if err != nil {
		return false, err
	}

	w.mu.Lock()
	if ok {
		w.granted[c.AuthorID] = struct{}{}
	} else {
		w.refused[c.ID] = struct{}{}
	}
	w.mu.Unlock()
	return ok, nil
}

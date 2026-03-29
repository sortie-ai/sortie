package github

import (
	"fmt"
	"sync"
	"testing"
)

// etagCacheLen returns the current number of entries without going through
// the public API. White-box helper — lives only in the test file.
func etagCacheLen(t *testing.T, c *etagCache) int {
	t.Helper()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func TestETagCache_PutAndLookup(t *testing.T) {
	t.Parallel()

	c := newETagCache(10)
	c.put("/repos/o/r/issues/1", `"abc123"`, "in-progress")

	etag, state, ok := c.lookup("/repos/o/r/issues/1")
	if !ok {
		t.Fatal("lookup = miss, want hit")
	}
	if etag != `"abc123"` {
		t.Errorf("etag = %q, want %q", etag, `"abc123"`)
	}
	if state != "in-progress" {
		t.Errorf("state = %q, want %q", state, "in-progress")
	}
}

func TestETagCache_LookupMiss(t *testing.T) {
	t.Parallel()

	c := newETagCache(10)
	etag, state, ok := c.lookup("/repos/o/r/issues/99")
	if ok {
		t.Error("lookup on empty cache: ok = true, want false")
	}
	if etag != "" {
		t.Errorf("etag = %q, want empty", etag)
	}
	if state != "" {
		t.Errorf("state = %q, want empty", state)
	}
}

func TestETagCache_LookupMiss_AfterEviction(t *testing.T) {
	t.Parallel()

	c := newETagCache(1) // maxSize=1: adding a second entry evicts the first.

	c.put("/path/a", "etag-a", "state-a")
	// Backdate /path/a so it is unambiguously the LRU entry.
	c.mu.Lock()
	e := c.entries["/path/a"]
	e.accessedAt = 1
	c.entries["/path/a"] = e
	c.mu.Unlock()

	c.put("/path/b", "etag-b", "state-b") // capacity reached → evicts /path/a

	if _, _, ok := c.lookup("/path/a"); ok {
		t.Error("lookup(/path/a) = hit, want miss (evicted)")
	}
	if _, _, ok := c.lookup("/path/b"); !ok {
		t.Error("lookup(/path/b) = miss, want hit")
	}
}

func TestETagCache_Put_UpdatesExisting(t *testing.T) {
	t.Parallel()

	c := newETagCache(10)
	c.put("/path/x", "etag-v1", "state-v1")
	c.put("/path/x", "etag-v2", "state-v2")

	etag, state, ok := c.lookup("/path/x")
	if !ok {
		t.Fatal("lookup = miss, want hit")
	}
	if etag != "etag-v2" {
		t.Errorf("etag = %q, want %q", etag, "etag-v2")
	}
	if state != "state-v2" {
		t.Errorf("state = %q, want %q", state, "state-v2")
	}
	// Second put must not create a duplicate entry.
	if n := etagCacheLen(t, c); n != 1 {
		t.Errorf("cache len = %d, want 1 (no duplicate)", n)
	}
}

func TestETagCache_Eviction_RemovesLRU(t *testing.T) {
	t.Parallel()

	c := newETagCache(2)

	c.put("/path/a", "etag-a", "state-a")
	// Force /path/a to the oldest timestamp so it is definitely evicted.
	c.mu.Lock()
	ea := c.entries["/path/a"]
	ea.accessedAt = 1
	c.entries["/path/a"] = ea
	c.mu.Unlock()

	c.put("/path/b", "etag-b", "state-b") // newer
	c.put("/path/c", "etag-c", "state-c") // triggers eviction of /path/a (oldest)

	if _, _, ok := c.lookup("/path/a"); ok {
		t.Error("lookup(/path/a) = hit, want miss (LRU evicted)")
	}
	if _, _, ok := c.lookup("/path/b"); !ok {
		t.Error("lookup(/path/b) = miss, want hit")
	}
	if _, _, ok := c.lookup("/path/c"); !ok {
		t.Error("lookup(/path/c) = miss, want hit")
	}
}

func TestETagCache_Touch_UpdatesAccessTime(t *testing.T) {
	t.Parallel()

	c := newETagCache(2)

	c.put("/path/a", "etag-a", "state-a")
	// Backdate /path/a so it starts as the oldest entry.
	c.mu.Lock()
	ea := c.entries["/path/a"]
	ea.accessedAt = 1
	c.entries["/path/a"] = ea
	c.mu.Unlock()

	c.put("/path/b", "etag-b", "state-b")
	// Backdate /path/b to a slightly newer but still old epoch time.
	c.mu.Lock()
	eb := c.entries["/path/b"]
	eb.accessedAt = 2
	c.entries["/path/b"] = eb
	c.mu.Unlock()

	// Touch /path/a — its accessedAt is updated to time.Now() (much larger than 2).
	c.touch("/path/a")

	// Adding /path/c triggers eviction; /path/b has the smallest accessedAt (2).
	c.put("/path/c", "etag-c", "state-c")

	if _, _, ok := c.lookup("/path/a"); !ok {
		t.Error("lookup(/path/a) = miss, want hit (was touched, should survive)")
	}
	if _, _, ok := c.lookup("/path/b"); ok {
		t.Error("lookup(/path/b) = hit, want miss (LRU evicted after touch updated /path/a)")
	}
	if _, _, ok := c.lookup("/path/c"); !ok {
		t.Error("lookup(/path/c) = miss, want hit")
	}
}

func TestETagCache_MaxSizeZero_Disabled(t *testing.T) {
	t.Parallel()

	c := newETagCache(0)

	c.put("/path/a", "etag-a", "state-a")

	if n := etagCacheLen(t, c); n != 0 {
		t.Errorf("cache len = %d after put, want 0 (cache disabled)", n)
	}
	if _, _, ok := c.lookup("/path/a"); ok {
		t.Error("lookup = hit, want miss (cache disabled)")
	}

	// touch on a disabled cache must not panic.
	c.touch("/path/a")
}

func TestETagCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	c := newETagCache(10)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/path/%d", i%5) // shared paths to stress lock contention
			c.put(path, fmt.Sprintf("etag-%d", i), fmt.Sprintf("state-%d", i))
			c.lookup(path)
			c.touch(path)
		}(i)
	}
	wg.Wait()
}

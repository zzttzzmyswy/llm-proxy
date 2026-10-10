package main

import (
	"fmt"
	"strings"
	"testing"
)

// entryBytes sums the sizes the cache believes it holds.
func entryBytes(c *imageDescCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum int
	for _, e := range c.entries {
		sum += e.size
	}
	return sum
}

// Overwriting an existing key must keep c.size in step with the entries. The
// update path assigned e.size before reading it back for the delta, so the
// expression cancelled out and c.size never moved — the 20MB cap then stops
// being enforced, because it is only ever compared against a number that no
// longer describes the cache.
func TestImageDescCacheSizeTracksOverwrites(t *testing.T) {
	c := &imageDescCache{max: 1 << 20, entries: map[string]*imgCacheEntry{}}

	c.put("k", "short", 100)
	if got := c.size; got != 100 {
		t.Fatalf("after the first insert c.size=%d, want 100", got)
	}

	c.put("k", "a much longer description", 900)
	if got, want := c.size, entryBytes(c); got != want {
		t.Fatalf("c.size=%d disagrees with the %d bytes held", got, want)
	}
	if got := c.size; got != 900 {
		t.Fatalf("after overwriting 100 bytes with 900, c.size=%d, want 900", got)
	}

	// Shrinking must also be reflected, or the cache thinks it is fuller than it is.
	c.put("k", "x", 20)
	if got, want := c.size, entryBytes(c); got != want {
		t.Fatalf("after shrinking, c.size=%d disagrees with the %d bytes held", got, want)
	}
}

// The consequence of the accounting bug: with c.size stuck, an unbounded number
// of large entries fits "under" the cap and the cache grows without limit.
func TestImageDescCacheStaysUnderCapAfterOverwrites(t *testing.T) {
	const cap = 10_000
	c := &imageDescCache{max: cap, entries: map[string]*imgCacheEntry{}}

	// Repeatedly overwrite the same key with a large entry, and add others. With
	// a stuck c.size the eviction loop never fires and every entry is retained.
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("key-%d", i)
		c.put("shared", strings.Repeat("d", 600), 600)
		c.put(key, strings.Repeat("e", 600), 600)
	}

	if got := c.size; got > cap {
		t.Fatalf("c.size=%d exceeds the cap %d", got, cap)
	}
	if got := entryBytes(c); got > cap {
		t.Fatalf("the cache holds %d bytes, over its %d cap", got, cap)
	}
}

// The most-recently written entry must never be the one evicted to make room for
// itself.
func TestImageDescCacheKeepsTheEntryItJustWrote(t *testing.T) {
	c := &imageDescCache{max: 1000, entries: map[string]*imgCacheEntry{}}

	c.put("old", "old", 700)
	c.put("new", "new", 700)

	if got := c.get("new"); got == "" {
		t.Fatal("the entry just written must survive eviction")
	}
	if got := c.get("old"); got != "" {
		t.Fatal("the least-recently-used entry should have been evicted")
	}
}

// A rewrite must refresh the key's LRU position: it was used most recently, so a
// later eviction has to take a colder entry.
func TestImageDescCacheOverwriteRefreshesLRU(t *testing.T) {
	c := &imageDescCache{max: 1000, entries: map[string]*imgCacheEntry{}}

	c.put("a", "a", 400)
	c.put("b", "b", 400)
	// Rewriting a makes it the most recently used, so b becomes the eviction
	// candidate.
	c.put("a", "a2", 400)
	c.put("c", "c", 400)

	if got := c.get("a"); got == "" {
		t.Fatal("the rewritten key must not be evicted before a colder one")
	}
	if got := c.get("b"); got != "" {
		t.Fatal("the coldest key should have been evicted")
	}
}

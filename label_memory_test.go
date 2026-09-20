package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// oversizedLabel builds a client-supplied name whose visible, retained form is
// bounded by maxLabelLen but whose allocation is far larger.
func oversizedLabel(prefix string) string {
	return prefix + strings.Repeat("x", 1<<20)
}

// sharesStorage reports whether two strings point at the same backing array.
// That is exactly what keeps the caller's large allocation reachable after the
// visible label has been truncated.
func sharesStorage(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return unsafe.StringData(a) == unsafe.StringData(b)
}

// P1: a truncated label must not keep pointing into the client's string. Length
// assertions alone cannot catch this — the retained label is short either way.
func TestRetainedLabelsDetachFromClientString(t *testing.T) {
	c := newStatsCollector()
	rawModel := oversizedLabel("model-")
	rawAlias := oversizedLabel("alias-")
	c.beginReq(rawAlias, rawModel, "openai").failure(catUpstream4xx, 400, "unknown model")

	c.mu.Lock()
	defer c.mu.Unlock()

	m := c.models[truncateLabel(rawModel)]
	if m == nil {
		t.Fatalf("no bucket recorded for the client model name")
	}
	if len(m.model) > maxLabelLen {
		t.Fatalf("model label is %d bytes, limit is %d", len(m.model), maxLabelLen)
	}
	if sharesStorage(m.model, rawModel) {
		t.Fatalf("model label still points into the client's %d-byte string", len(rawModel))
	}

	if len(m.aliases) == 0 {
		t.Fatalf("alias was not recorded")
	}
	for alias := range m.aliases {
		if len(alias) > maxLabelLen {
			t.Fatalf("alias key is %d bytes, limit is %d", len(alias), maxLabelLen)
		}
		if sharesStorage(alias, rawAlias) {
			t.Fatalf("alias key still points into the client's %d-byte string", len(rawAlias))
		}
	}

	if len(m.recent) == 0 {
		t.Fatalf("failure detail was not recorded")
	}
	if got := len(m.recent[0].Alias); got > maxLabelLen {
		t.Fatalf("failure detail alias is %d bytes, limit is %d", got, maxLabelLen)
	}
	if sharesStorage(m.recent[0].Alias, rawAlias) {
		t.Fatalf("failure detail alias still points into the client's %d-byte string", len(rawAlias))
	}
}

// The warn path shares noteErrorLocked with the failure path, so it must be
// bounded by the same rule.
func TestWarnAliasIsBoundedAndDetached(t *testing.T) {
	c := newStatsCollector()
	rawAlias := oversizedLabel("warn-")
	c.warn(rawAlias, "vlm-model", "anthropic", catVLMDescribeFailed, "describe failed")

	c.mu.Lock()
	defer c.mu.Unlock()

	m := c.models["vlm-model"]
	if m == nil || len(m.recent) == 0 {
		t.Fatalf("warn detail was not recorded")
	}
	if got := len(m.recent[0].Alias); got > maxLabelLen {
		t.Fatalf("warn detail alias is %d bytes, limit is %d", got, maxLabelLen)
	}
	if sharesStorage(m.recent[0].Alias, rawAlias) {
		t.Fatalf("warn detail alias still points into the client's %d-byte string", len(rawAlias))
	}
}

// P1: the memory acceptance criterion the review measured. 32 distinct 1MiB
// model names must not leave their original allocations reachable; the 32
// buckets themselves account for roughly 2MiB.
func TestRetainedLabelsDoNotPinOversizedStrings(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	c := newStatsCollector()
	for i := 0; i < 32; i++ {
		label := fmt.Sprintf("model-%03d-", i) + strings.Repeat("x", 1<<20)
		c.beginReq(label, label, "openai").success(tokenUsage{Reported: true})
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(c)

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("32 models with visible labels <=%d bytes retain %d bytes after GC", maxLabelLen, retained)
	if retained > 16<<20 {
		t.Fatalf("label truncation retains the original oversized string allocations")
	}
}

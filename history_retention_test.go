package main

import (
	"testing"
	"time"
)

// The per-minute ring exists to hold 24 hours of traffic. Its slot was indexed by
// the minute-aligned SECOND timestamp (a multiple of 60) modulo the ring size,
// and gcd(60, 1440) = 60, so only every 60th slot was ever written: the "24 hour"
// ring retained just 24 distinct slots, and an hour later it overwrote them. A
// user opening the 24h chart therefore saw the last ~24 minutes and nothing else,
// no matter how long the proxy had been running.
func TestHistoryRingRetainsAFullDay(t *testing.T) {
	c, clk := newTestCollector()

	// Four widely separated minutes inside the 24h window. If the slot aliases,
	// later writes land on the same slot as earlier ones and the earlier traffic
	// disappears.
	marks := []int{0, 60, 300, 1439}
	for _, minute := range marks {
		if minute > 0 {
			clk.advance(time.Duration(minute-lastMark(marks, minute)) * time.Minute)
		}
		hit(c, "sonnet", "m1", 100, 0)
	}

	snap := c.history(historyRange24h, nil)
	s := seriesByName(t, snap, "m1")
	if len(s.Points) == 0 {
		t.Fatalf("the model has traffic, so it needs a series")
	}

	var nonzero int
	var total int64
	for _, p := range s.Points {
		if p.Tokens != 0 {
			nonzero++
			total += p.Tokens
		}
	}
	if nonzero != len(marks) {
		t.Fatalf("a 24h window must keep all %d separated minutes, got %d nonzero buckets (total tokens %d)",
			len(marks), nonzero, total)
	}
	if total != int64(100*len(marks)) {
		t.Fatalf("a 24h window must retain all %d tokens, got %d", 100*len(marks), total)
	}
}

// lastMark returns the largest mark strictly below m, or 0.
func lastMark(marks []int, m int) int {
	prev := 0
	for _, v := range marks {
		if v < m && v > prev {
			prev = v
		}
	}
	return prev
}

// The bug in its concrete form: the slot index collapsed to 24 values, so minutes
// 60 apart shared a slot and each hour overwrote the previous one. A full day of
// one-write-per-minute traffic must therefore survive, not just the last 24
// minutes of it.
func TestHistoryRingSlotsDoNotAlias(t *testing.T) {
	c, clk := newTestCollector()

	// Two hours of one write per minute: 120 distinct minutes, five times the 24
	// slots the aliasing index could address.
	const minutes = 120
	for i := 0; i < minutes; i++ {
		if i > 0 {
			clk.advance(time.Minute)
		}
		hit(c, "sonnet", "m1", 10, 0)
		// Read the series while every write is still inside the window: if slots
		// alias, the count stalls at the number of reachable slots.
		s := seriesByName(t, c.history(historyRange24h, nil), "m1")
		var got int64
		for _, p := range s.Points {
			got += p.Tokens
		}
		if want := int64(10 * (i + 1)); got != want {
			t.Fatalf("after %d minutes of traffic the 24h window holds %d tokens, want %d (ring slots are aliasing)",
				i+1, got, want)
		}
	}
}

// A whole day of per-minute traffic must survive: one write per minute for 24h,
// with every minute landing in its own slot.
func TestHistoryRingHoldsEveryMinuteOfADay(t *testing.T) {
	c, clk := newTestCollector()

	const minutes = historyRingMinutes
	for i := 0; i < minutes; i++ {
		if i > 0 {
			clk.advance(time.Minute)
		}
		hit(c, "sonnet", "m1", 10, 0)
	}

	snap := c.history(historyRange24h, nil)
	s := seriesByName(t, snap, "m1")
	var total int64
	for _, p := range s.Points {
		total += p.Tokens
	}
	// The oldest minute has just been pushed out by the newest, so allow the
	// window to miss at most the very first write.
	if total < int64(10*(minutes-1)) {
		t.Fatalf("a full day of traffic must be retained, got %d of %d tokens", total, 10*minutes)
	}
}

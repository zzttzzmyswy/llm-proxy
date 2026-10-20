package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The collector folds traffic into a bucket literally named "(other)" once it
// tracks maxTrackedModels models. The history chart then folds its own tail into
// a series with the same name, which used to put two same-named series in the
// payload: the page keys colours by name, so the two drew on top of each other and
// the tooltip reported only one of them.
func TestHistoryNeverEmitsTwoOtherSeries(t *testing.T) {
	c, clk := newTestCollector()
	for i := 0; i < maxTrackedModels+20; i++ {
		name := fmt.Sprintf("client-model-%03d", i)
		c.beginReq(name, name, "openai").success(tokenUsage{Input: 10, Output: 10, Reported: true})
	}
	for i := 0; i < 20; i++ {
		clk.advance(60_000_000_000)
	}

	snap := c.history(historyRange1h, nil)
	seen := 0
	for _, s := range snap.Models {
		if s.Model == otherModelKey {
			seen++
		}
	}
	if seen != 1 {
		names := make([]string, 0, len(snap.Models))
		for _, s := range snap.Models {
			names = append(names, s.Model)
		}
		b, _ := json.Marshal(names)
		t.Fatalf("%q must appear exactly once, got %d: %s", otherModelKey, seen, b)
	}

	// And the merged series must actually carry the tail's traffic, or the fix
	// would have replaced a duplicate with a silent under-count.
	var total int64
	for _, s := range snap.Models {
		if s.Model != otherModelKey {
			continue
		}
		for _, p := range s.Points {
			total += p.Requests
		}
	}
	if total == 0 {
		t.Fatal("the (other) series must carry the folded traffic")
	}
}


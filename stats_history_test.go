package main

import (
	"testing"
	"time"
)

// hit records one successful call on an explicit collector. It must not fall back
// to the package-level collector, which runs on the real clock and would put the
// traffic outside every window a fake-clock test asks about.
func hit(c *statsCollector, alias, model string, in, out int64) {
	c.beginReq(alias, model, "anthropic").success(tokenUsage{Input: int(in), Output: int(out), Reported: true})
}

func seriesByName(t *testing.T, snap historySnapshot, name string) historyModelSeries {
	t.Helper()
	for _, m := range snap.Models {
		if m.Model == name {
			return m
		}
	}
	return historyModelSeries{}
}

// The chart's x axis is one point per bucket, so a quiet minute has to come back
// as a zero point rather than being skipped: a gap would shift every later point
// left and silently misdate the whole series.
func TestHistoryEmitsZeroPointsForQuietBuckets(t *testing.T) {
	c, clk := newTestCollector()

	hit(c, "sonnet", "m1", 100, 20)
	clk.advance(3 * time.Minute)
	hit(c, "sonnet", "m1", 300, 0)

	snap := c.history(historyRange6h, nil)
	if snap.BucketSeconds != 300 {
		t.Fatalf("a 6h window must use five-minute buckets, got %d", snap.BucketSeconds)
	}
	if len(snap.Timestamps) != historyRange6h/300 {
		t.Fatalf("want %d points, got %d", historyRange6h/300, len(snap.Timestamps))
	}
	s := seriesByName(t, snap, "m1")
	if len(s.Points) != len(snap.Timestamps) {
		t.Fatalf("every bucket needs a point, got %d for %d timestamps", len(s.Points), len(snap.Timestamps))
	}
	last := s.Points[len(s.Points)-1]
	// The call three minutes back shares the newest five-minute bucket with the
	// current one.
	if last.Tokens != 420 {
		t.Fatalf("newest bucket must hold both calls, got %d", last.Tokens)
	}
	for _, i := range []int{len(s.Points) - 3, len(s.Points) - 2} {
		if s.Points[i].Tokens != 0 {
			t.Fatalf("quiet buckets must be zero points, got %+v", s.Points[i])
		}
	}
}

// The trailing-60s TPM/RPM totals hand the chart its right-hand edge, so the
// newest point has to agree with them.
func TestHistoryNewestBucketMatchesTheLiveWindow(t *testing.T) {
	c, _ := newTestCollector()

	hit(c, "sonnet", "m1", 120, 30)
	c.beginReq("sonnet", "m1", "anthropic").failure(catUpstream5xx, 503, "boom")

	live := modelByName(t, c.snapshot(), "m1")
	hist := seriesByName(t, c.history(historyRange1h, nil), "m1")
	if len(hist.Points) == 0 {
		t.Fatalf("the model has traffic, so it needs a series")
	}
	last := hist.Points[len(hist.Points)-1]
	if last.Tokens != live.TPM || last.Requests != live.RPM {
		t.Fatalf("newest point %+v must match live tpm=%d rpm=%d", last, live.TPM, live.RPM)
	}
	if last.Failures != 1 {
		t.Fatalf("the failure must be counted, got %+v", last)
	}
}

func TestHistoryCountsFailuresAndLatency(t *testing.T) {
	c, clk := newTestCollector()

	ok := c.beginReq("sonnet", "m1", "anthropic")
	clk.advance(200 * time.Millisecond)
	ok.success(tokenUsage{Input: 10, Reported: true})

	slow := c.beginReq("sonnet", "m1", "anthropic")
	clk.advance(300 * time.Millisecond)
	slow.failure(catStreamStalled, 0, "stalled")

	s := seriesByName(t, c.history(historyRange1h, nil), "m1")
	if len(s.Points) == 0 {
		t.Fatalf("the model has traffic, so it needs a series")
	}
	last := s.Points[len(s.Points)-1]
	if last.Requests != 2 || last.Failures != 1 {
		t.Fatalf("want 2 requests / 1 failure, got %+v", last)
	}
	if last.LatencyMS != 500 {
		t.Fatalf("latency must sum to 500ms, got %d", last.LatencyMS)
	}
	if last.MaxLatencyMS != 300 {
		t.Fatalf("max latency must be 300ms, got %d", last.MaxLatencyMS)
	}
}

// A bucket whose slot was reused by a later minute must not leak the old minute's
// counters, and a restart-free 24h run must keep answering for the oldest bucket
// it still holds.
func TestHistoryRingWrapsWithoutLeakingOldMinutes(t *testing.T) {
	c, clk := newTestCollector()

	hit(c, "sonnet", "m1", 999, 0)
	// Walk to exactly 24h later, one minute at a time, so every slot is reused.
	for i := 0; i < historyRingMinutes; i++ {
		clk.advance(time.Minute)
	}
	hit(c, "sonnet", "m1", 7, 0)

	s := seriesByName(t, c.history(historyRange24h, nil), "m1")
	for i, p := range s.Points {
		if i == len(s.Points)-1 {
			if p.Tokens != 7 {
				t.Fatalf("newest bucket must hold 7, got %d", p.Tokens)
			}
			continue
		}
		if p.Tokens != 0 {
			t.Fatalf("reused slot at index %d leaked %d tokens", i, p.Tokens)
		}
	}
}

func TestHistoryCapsModelSeriesAndFoldsTheRest(t *testing.T) {
	c, _ := newTestCollector()

	for i := 0; i < historyMaxSeries+5; i++ {
		hit(c, "sonnet", string(rune('a'+i)), int64(100*(i+1)), 0)
	}

	snap := c.history(historyRange1h, nil)
	// The capped series plus one (other) series carrying the folded tail.
	if len(snap.Models) != historyMaxSeries+1 {
		t.Fatalf("want %d series, got %d", historyMaxSeries+1, len(snap.Models))
	}
	if snap.ModelsExcluded != 5 {
		t.Fatalf("want 5 folded models, got %d", snap.ModelsExcluded)
	}
	// historyMaxSeries+5 models exist, so the cap keeps the heaviest
	// historyMaxSeries of them and folds the lightest five into (other).
	last := seriesByName(t, snap, otherModelKey).Points[len(snap.Timestamps)-1]
	var want int64
	for i := 0; i < 5; i++ {
		want += int64(100 * (i + 1))
	}
	if last.Tokens != want {
		t.Fatalf("(other) must hold the folded tail %d, got %d", want, last.Tokens)
	}
	if snap.Models[0].Model != "m" {
		t.Fatalf("series must be ranked by tokens, got %q first", snap.Models[0].Model)
	}
}

// Traffic that is older than the requested window must not appear in it, and the
// window has to start on a bucket boundary so the point count stays fixed.
func TestHistoryWindowExcludesOlderTraffic(t *testing.T) {
	c, clk := newTestCollector()

	hit(c, "sonnet", "m1", 500, 0)
	clk.advance(2 * time.Hour)
	hit(c, "sonnet", "m1", 42, 0)

	snap := c.history(historyRange1h, nil)
	if len(snap.Timestamps) != 60 {
		t.Fatalf("one hour wants 60 one-minute points, got %d", len(snap.Timestamps))
	}
	if snap.Timestamps[len(snap.Timestamps)-1]-snap.Timestamps[0] != int64(59*60) {
		t.Fatalf("points must be one minute apart, got %d", snap.Timestamps[len(snap.Timestamps)-1]-snap.Timestamps[0])
	}
	var total int64
	for _, p := range seriesByName(t, snap, "m1").Points {
		total += p.Tokens
	}
	if total != 42 {
		t.Fatalf("the 2h-old call must fall outside a 1h window, got %d tokens", total)
	}
}

func TestHistoryBucketSizesByRange(t *testing.T) {
	c, _ := newTestCollector()
	hit(c, "sonnet", "m1", 60, 0)

	cases := []struct {
		rangeSec int
		bucket   int
		points   int
	}{
		{historyRange1h, 60, 60},
		{historyRange6h, 300, 72},
		{historyRange24h, 900, 96},
	}
	for _, tc := range cases {
		snap := c.history(tc.rangeSec, nil)
		if snap.BucketSeconds != tc.bucket {
			t.Errorf("range %ds must use %ds buckets, got %d", tc.rangeSec, tc.bucket, snap.BucketSeconds)
		}
		if len(snap.Timestamps) != tc.points {
			t.Errorf("range %ds must have %d points, got %d", tc.rangeSec, tc.points, len(snap.Timestamps))
		}
		if snap.To-snap.From != int64(tc.rangeSec) {
			t.Errorf("range %ds must span exactly that, got %d", tc.rangeSec, snap.To-snap.From)
		}
	}
}

// Every bucket size divides an hour, so the axis lands on round minute marks
// instead of drifting a little further off the clock with each tick.
func TestHistoryBucketStartsAlignToAnHour(t *testing.T) {
	c, clk := newTestCollector()
	clk.advance(37*time.Minute + 23*time.Second)
	hit(c, "sonnet", "m1", 5, 0)

	for _, rangeSec := range []int{historyRange1h, historyRange6h, historyRange24h} {
		snap := c.history(rangeSec, nil)
		if snap.BucketSeconds%60 != 0 || 3600%snap.BucketSeconds != 0 {
			t.Fatalf("range %ds uses a %ds bucket, which does not divide an hour", rangeSec, snap.BucketSeconds)
		}
		for i, ts := range snap.Timestamps {
			if ts%int64(snap.BucketSeconds) != 0 {
				t.Fatalf("range %ds point %d at %d is off the bucket grid", rangeSec, i, ts)
			}
		}
		// The newest bucket is the one in progress, so it has to cover now.
		newest := snap.Timestamps[len(snap.Timestamps)-1]
		if now := clk.now().Unix(); now < newest || now >= newest+int64(snap.BucketSeconds) {
			t.Fatalf("range %ds newest bucket [%d,%d) does not contain now=%d", rangeSec, newest, newest+int64(snap.BucketSeconds), now)
		}
	}
}

func TestHistoryFiltersByModel(t *testing.T) {
	c, _ := newTestCollector()
	hit(c, "sonnet", "m1", 10, 0)
	hit(c, "opus", "m2", 20, 0)

	snap := c.history(historyRange1h, []string{"m2"})
	if len(snap.Models) != 1 || snap.Models[0].Model != "m2" {
		t.Fatalf("filter must narrow the series, got %+v", snap.Models)
	}
	if snap.Totals.Requests != 1 {
		t.Fatalf("totals must follow the filter, got %+v", snap.Totals)
	}
}

func TestHistoryTotalsSumEveryModel(t *testing.T) {
	c, _ := newTestCollector()
	hit(c, "sonnet", "m1", 10, 5)
	hit(c, "opus", "m2", 20, 0)
	c.beginReq("opus", "m2", "anthropic").failure(catRateLimited, 429, "429")

	snap := c.history(historyRange1h, nil)
	if snap.Totals.Requests != 3 || snap.Totals.Failures != 1 || snap.Totals.Tokens != 35 {
		t.Fatalf("totals: %+v", snap.Totals)
	}
	if len(snap.AllModels) != 2 || snap.AllModels[0] != "m2" {
		t.Fatalf("all_models must rank every model by tokens, got %v", snap.AllModels)
	}
}

// No traffic yet is the normal state right after a restart, and the page draws an
// empty state for it: the arrays must be empty rather than null.
func TestHistoryOnUntouchedCollector(t *testing.T) {
	c, _ := newTestCollector()

	snap := c.history(historyRange1h, nil)
	if snap.Models == nil || snap.AllModels == nil {
		t.Fatalf("collections must not be nil: %+v", snap)
	}
	if len(snap.Models) != 0 || len(snap.Timestamps) != 60 {
		t.Fatalf("want no series but a full axis, got %+v", snap)
	}
	if snap.Totals.Requests != 0 {
		t.Fatalf("totals must be zero, got %+v", snap.Totals)
	}
}

func TestHistoryRangeSecondsFallsBackTo24h(t *testing.T) {
	if got := historyWindowSeconds(""); got != historyRange24h {
		t.Fatalf("empty range must default to 24h, got %d", got)
	}
	if got := historyWindowSeconds("6h"); got != historyRange6h {
		t.Fatalf("6h must resolve to its own window, got %d", got)
	}
	// An unknown range must not silently return a window the ring cannot serve.
	if got := historyWindowSeconds("99h"); got != historyRange24h {
		t.Fatalf("unknown range must fall back to 24h, got %d", got)
	}
}

func TestHistoryResetClearsTheRing(t *testing.T) {
	c, _ := newTestCollector()
	hit(c, "sonnet", "m1", 10, 0)

	c.reset()

	snap := c.history(historyRange1h, nil)
	if len(snap.Models) != 0 {
		t.Fatalf("reset must drop history too, got %+v", snap.Models)
	}
}

// --- cache hit rate ---

// The hit rate is read tokens over every token the upstream had to read, so a
// cache write has to sit in the denominator: a request that only wrote to the
// cache read nothing, and counting it as a hit would flatter the ratio.
func TestHistoryCacheHitRateUsesTheWholePrompt(t *testing.T) {
	c, _ := newTestCollector()

	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{
		Input: 100, Output: 10, CacheRead: 700, CacheCreation: 200, Reported: true,
	})

	s := seriesByName(t, c.history(historyRange1h, nil), "m1")
	last := s.Points[len(s.Points)-1]
	if last.CacheRead != 700 || last.CacheCreation != 200 {
		t.Fatalf("cache counters must reach the bucket, got %+v", last)
	}
	if last.CacheHitRate != 0.7 {
		t.Fatalf("700 of 1000 prompt tokens must be 0.7, got %v", last.CacheHitRate)
	}
	if s.CacheHitRate != 0.7 {
		t.Fatalf("the series total must agree with its single bucket, got %v", s.CacheHitRate)
	}
}

// OpenAI counts cached tokens inside prompt_tokens, so the same request shape
// must not produce a different ratio on that path.
func TestHistoryCacheHitRateAgreesAcrossGateways(t *testing.T) {
	c, _ := newTestCollector()

	// The same 1000-token prompt, 700 of it cached, as each gateway reports it.
	anthropic := extractAnthropicUsage([]byte(`{"usage":{"input_tokens":300,"output_tokens":5,
		"cache_read_input_tokens":700}}`), false)
	openai := extractOpenAIUsage([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":5,
		"prompt_tokens_details":{"cached_tokens":700}}}`), false)
	c.beginReq("sonnet", "anthropic-model", "anthropic").success(anthropic)
	c.beginReq("sonnet", "openai-model", "openai").success(openai)

	snap := c.history(historyRange1h, nil)
	for _, name := range []string{"anthropic-model", "openai-model"} {
		s := seriesByName(t, snap, name)
		last := s.Points[len(s.Points)-1]
		if last.CacheHitRate != 0.7 {
			t.Fatalf("%s must read 0.7, got %v (prompt counted as %d)", name, last.CacheHitRate,
				last.Tokens)
		}
	}
}

// A bucket with no cache traffic at all is a real zero, not a missing value; and
// a bucket with no prompt tokens must not divide by zero.
func TestHistoryCacheHitRateOnEmptyAndUncachedBuckets(t *testing.T) {
	c, clk := newTestCollector()

	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 500, Output: 5, Reported: true})
	clk.advance(2 * time.Minute)
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 500, Output: 5, Reported: true})

	s := seriesByName(t, c.history(historyRange1h, nil), "m1")
	for i, p := range s.Points {
		if p.CacheHitRate != 0 {
			t.Fatalf("point %d must read 0%% when nothing was cached, got %v", i, p.CacheHitRate)
		}
	}
	if s.CacheHitRate != 0 {
		t.Fatalf("series total must be 0, got %v", s.CacheHitRate)
	}
}

// The interval summary is what the page ranks models by, so it needs the same
// ratio over the whole window rather than an average of per-bucket ratios.
func TestHistorySummarizesCacheHitRatePerModel(t *testing.T) {
	c, _ := newTestCollector()

	// A big cached request and a small uncached one: the ratio must be weighted by
	// tokens (700 of 1000 prompt tokens = 0.7), not the mean of 1.0 and 0.0.
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{
		Input: 100, Output: 5, CacheRead: 700, CacheCreation: 100, Reported: true,
	})
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 100, Output: 5, Reported: true})

	s := seriesByName(t, c.history(historyRange1h, nil), "m1")
	if s.CacheHitRate < 0.69 || s.CacheHitRate > 0.71 {
		t.Fatalf("want a token-weighted 0.7, got %v", s.CacheHitRate)
	}
	if s.CacheRead != 700 || s.CacheCreation != 100 {
		t.Fatalf("series must carry the raw counters too, got read=%d creation=%d", s.CacheRead, s.CacheCreation)
	}
}

func TestStatsSnapshotReportsCacheHitRate(t *testing.T) {
	c, _ := newTestCollector()

	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{
		Input: 200, Output: 10, CacheRead: 800, Reported: true,
	})

	m := modelByName(t, c.snapshot(), "m1")
	if m.CacheRead != 800 || m.CacheHitRate != 0.8 {
		t.Fatalf("want 800 read / 0.8, got %d / %v", m.CacheRead, m.CacheHitRate)
	}
	if m.InputTokens != 200 {
		t.Fatalf("the plain input count must not absorb the cache read, got %d", m.InputTokens)
	}
}

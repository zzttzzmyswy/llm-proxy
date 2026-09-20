package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestCollector() (*statsCollector, *fakeClock) {
	clk := newFakeClock()
	return newStatsCollectorWithClock(clk.now), clk
}

func modelByName(t *testing.T, snap statsSnapshot, name string) modelSnapshot {
	t.Helper()
	for _, m := range snap.Models {
		if m.Model == name {
			return m
		}
	}
	t.Fatalf("model %q missing from snapshot: %+v", name, snap.Models)
	return modelSnapshot{}
}

func TestStatsCountsRequestsAndFailures(t *testing.T) {
	c, _ := newTestCollector()

	c.beginReq("sonnet", "DeepSeek-Flash", "anthropic").success(tokenUsage{Input: 100, Output: 20, Reported: true})
	c.beginReq("sonnet", "DeepSeek-Flash", "anthropic").failure(catUpstream5xx, 503, "upstream 503")

	snap := c.snapshot()
	if snap.Totals.Requests != 2 || snap.Totals.Failures != 1 {
		t.Fatalf("totals: %+v", snap.Totals)
	}
	m := modelByName(t, snap, "DeepSeek-Flash")
	if m.Requests != 2 || m.Successes != 1 || m.Failures != 1 {
		t.Fatalf("counts: %+v", m)
	}
	if m.FailureRate != 0.5 {
		t.Fatalf("failure rate must be failures/requests, got %v", m.FailureRate)
	}
	if m.InputTokens != 100 || m.OutputTokens != 20 || m.TotalTokens != 120 {
		t.Fatalf("tokens: %+v", m)
	}
	if m.ErrorCounts[catUpstream5xx] != 1 {
		t.Fatalf("error counts: %+v", m.ErrorCounts)
	}
	if len(m.RecentErrors) != 1 || m.RecentErrors[0].Status != 503 {
		t.Fatalf("recent errors: %+v", m.RecentErrors)
	}
}

// TPM and RPM are trailing-60s windows, so traffic older than a minute must drop
// out of them while staying in the cumulative totals.
func TestStatsTPMIsATrailingSixtySecondWindow(t *testing.T) {
	c, clk := newTestCollector()

	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 300, Output: 0, Reported: true})
	clk.advance(30 * time.Second)
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 100, Output: 0, Reported: true})

	if got := modelByName(t, c.snapshot(), "m1").TPM; got != 400 {
		t.Fatalf("both calls are inside the window, want 400, got %d", got)
	}

	clk.advance(31 * time.Second)
	m := modelByName(t, c.snapshot(), "m1")
	if m.TPM != 100 || m.RPM != 1 {
		t.Fatalf("only the recent call is inside the window, want tpm=100 rpm=1, got tpm=%d rpm=%d", m.TPM, m.RPM)
	}

	clk.advance(30 * time.Second)
	if m := modelByName(t, c.snapshot(), "m1"); m.TPM != 0 || m.RPM != 0 {
		t.Fatalf("window must empty out, got tpm=%d rpm=%d", m.TPM, m.RPM)
	}
	if m := modelByName(t, c.snapshot(), "m1"); m.TotalTokens != 400 {
		t.Fatalf("cumulative tokens must survive the window, got %d", m.TotalTokens)
	}
}

func TestStatsSparklineBucketsByMinute(t *testing.T) {
	c, clk := newTestCollector()

	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 50, Reported: true})
	clk.advance(2 * time.Minute)
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 30, Reported: true})

	spark := modelByName(t, c.snapshot(), "m1").Sparkline
	if len(spark) != statsSparkMinutes {
		t.Fatalf("sparkline must be %d wide, got %d", statsSparkMinutes, len(spark))
	}
	if spark[len(spark)-1] != 30 {
		t.Fatalf("current minute bucket must hold 30, got %d", spark[len(spark)-1])
	}
	if spark[len(spark)-3] != 50 {
		t.Fatalf("bucket two minutes back must hold 50, got %d", spark[len(spark)-3])
	}
	for i := 0; i < len(spark)-3; i++ {
		if spark[i] != 0 {
			t.Fatalf("older buckets must stay empty, got %v", spark)
		}
	}
}

func TestStatsTracksAliasDistribution(t *testing.T) {
	c, _ := newTestCollector()
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Reported: true})
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Reported: true})
	c.beginReq("haiku", "m1", "anthropic").success(tokenUsage{Reported: true})

	m := modelByName(t, c.snapshot(), "m1")
	if m.Aliases["sonnet"] != 2 || m.Aliases["haiku"] != 1 {
		t.Fatalf("alias distribution: %+v", m.Aliases)
	}
}

func TestStatsLatencyAverageAndMax(t *testing.T) {
	c, clk := newTestCollector()

	first := c.beginReq("sonnet", "m1", "anthropic")
	clk.advance(100 * time.Millisecond)
	first.success(tokenUsage{Reported: true})

	second := c.beginReq("sonnet", "m1", "anthropic")
	clk.advance(300 * time.Millisecond)
	second.success(tokenUsage{Reported: true})

	m := modelByName(t, c.snapshot(), "m1")
	if m.AvgLatencyMS != 200 {
		t.Fatalf("average of 100ms and 300ms must be 200ms, got %d", m.AvgLatencyMS)
	}
	if m.MaxLatencyMS != 300 {
		t.Fatalf("max latency must be 300ms, got %d", m.MaxLatencyMS)
	}
}

// A model is flagged as estimated only when a successful call had no upstream
// usage to report; a failure with no tokens must not taint it.
func TestStatsEstimatedFlag(t *testing.T) {
	c, _ := newTestCollector()
	c.beginReq("sonnet", "reported", "anthropic").success(tokenUsage{Input: 1, Reported: true})
	if m := modelByName(t, c.snapshot(), "reported"); m.Estimated {
		t.Fatal("a model with real upstream usage must not be flagged as estimated")
	}

	c.beginReq("sonnet", "estimated", "anthropic").success(tokenUsage{Input: 10, Output: 5})
	if m := modelByName(t, c.snapshot(), "estimated"); !m.Estimated {
		t.Fatal("a model without upstream usage must be flagged as estimated")
	}

	c.beginReq("sonnet", "failed-only", "anthropic").failure(catNetworkError, 0, "boom")
	if m := modelByName(t, c.snapshot(), "failed-only"); m.Estimated {
		t.Fatal("a failed call must not flag the model as estimated")
	}
}

func TestStatsPublishesEachRequestOnce(t *testing.T) {
	c, _ := newTestCollector()
	r := c.beginReq("sonnet", "m1", "anthropic")
	r.success(tokenUsage{Reported: true})
	r.failure(catUpstream5xx, 500, "late failure")
	r.success(tokenUsage{Reported: true})

	m := modelByName(t, c.snapshot(), "m1")
	if m.Requests != 1 || m.Failures != 0 {
		t.Fatalf("a request must publish exactly once, got %+v", m)
	}
}

// A VLM describe failure is recovered from, so it must show up as an error
// without inflating the request or failure counters.
func TestStatsWarnRecordsWithoutCountingARequest(t *testing.T) {
	c, _ := newTestCollector()
	c.warn("sonnet", "MiniMax-M3", "anthropic", catVLMDescribeFailed, "vlm timeout")

	m := modelByName(t, c.snapshot(), "MiniMax-M3")
	if m.Requests != 0 || m.Failures != 0 {
		t.Fatalf("warn must not count a request, got %+v", m)
	}
	if m.ErrorCounts[catVLMDescribeFailed] != 1 {
		t.Fatalf("warn must be categorised, got %+v", m.ErrorCounts)
	}
	if len(m.RecentErrors) != 1 || m.RecentErrors[0].Category != catVLMDescribeFailed {
		t.Fatalf("warn must appear in recent errors, got %+v", m.RecentErrors)
	}
}

func TestStatsRecentErrorsAreNewestFirstAndCapped(t *testing.T) {
	c, clk := newTestCollector()
	for i := 0; i < recentErrorLimit+10; i++ {
		c.beginReq("sonnet", "m1", "anthropic").failure(catUpstream5xx, 500, "e")
		clk.advance(time.Second)
	}

	m := modelByName(t, c.snapshot(), "m1")
	if len(m.RecentErrors) != recentErrorLimit {
		t.Fatalf("recent errors must be capped at %d, got %d", recentErrorLimit, len(m.RecentErrors))
	}
	if !m.RecentErrors[0].Time.After(m.RecentErrors[1].Time) {
		t.Fatal("recent errors must be newest first")
	}
	if m.ErrorCounts[catUpstream5xx] != int64(recentErrorLimit+10) {
		t.Fatalf("the counter must keep every failure even when details are dropped, got %d", m.ErrorCounts[catUpstream5xx])
	}
}

// The passthrough endpoint takes the model name from the client body, so the
// number of tracked models has to stay bounded.
func TestStatsCapsTrackedModels(t *testing.T) {
	c, _ := newTestCollector()
	for i := 0; i < maxTrackedModels+50; i++ {
		c.beginReq("x", "model-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "openai").success(tokenUsage{Reported: true})
	}

	snap := c.snapshot()
	if len(snap.Models) > maxTrackedModels+1 {
		t.Fatalf("tracked models must stay bounded, got %d", len(snap.Models))
	}
	if m := modelByName(t, snap, otherModelKey); m.Requests == 0 {
		t.Fatal("overflow traffic must land in the (other) bucket")
	}
	if snap.Totals.Requests != int64(maxTrackedModels+50) {
		t.Fatalf("no request may be lost to the cap, got %d", snap.Totals.Requests)
	}
}

func TestStatsUnknownModelBucket(t *testing.T) {
	c, _ := newTestCollector()
	c.beginReq("", "", "anthropic").success(tokenUsage{Reported: true})

	if m := modelByName(t, c.snapshot(), unknownModelKey); m.Requests != 1 {
		t.Fatalf("a request with no model must land in %q, got %+v", unknownModelKey, m)
	}
}

func TestStatsReset(t *testing.T) {
	c, clk := newTestCollector()
	c.beginReq("sonnet", "m1", "anthropic").success(tokenUsage{Input: 10, Reported: true})
	clk.advance(5 * time.Second)

	c.reset()
	snap := c.snapshot()
	if len(snap.Models) != 0 || snap.Totals.Requests != 0 {
		t.Fatalf("reset must clear every model, got %+v", snap)
	}
	if snap.UptimeSeconds != 0 {
		t.Fatalf("reset must restart the uptime clock, got %d", snap.UptimeSeconds)
	}
}

func TestStatsUptime(t *testing.T) {
	c, clk := newTestCollector()
	clk.advance(90 * time.Second)
	if got := c.snapshot().UptimeSeconds; got != 90 {
		t.Fatalf("uptime must be 90s, got %d", got)
	}
}

func TestStatsSnapshotHasNoNilCollections(t *testing.T) {
	c, _ := newTestCollector()
	snap := c.snapshot()
	if snap.Models == nil {
		t.Fatal("an empty snapshot must serialise as [], not null")
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := map[int]string{
		200: "",
		400: catUpstream4xx,
		404: catUpstream4xx,
		429: catRateLimited,
		500: catUpstream5xx,
		503: catUpstream5xx,
	}
	for code, want := range cases {
		if got := classifyStatus(code); got != want {
			t.Fatalf("status %d: want %q, got %q", code, want, got)
		}
	}
}

func TestClassifyError(t *testing.T) {
	if got := classifyError(errBodyIdle); got != catStreamStalled {
		t.Fatalf("idle body must classify as %q, got %q", catStreamStalled, got)
	}
	if got := classifyError(context.DeadlineExceeded); got != catNetworkTimeout {
		t.Fatalf("deadline must classify as %q, got %q", catNetworkTimeout, got)
	}
	if got := classifyError(errors.New("connection reset by peer")); got != catNetworkError {
		t.Fatalf("plain error must classify as %q, got %q", catNetworkError, got)
	}
	if got := classifyError(nil); got != "" {
		t.Fatalf("nil error must classify as empty, got %q", got)
	}
}

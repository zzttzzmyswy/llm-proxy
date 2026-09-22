package main

import (
	"sort"
	"time"
)

// History is the chart's data source: a per-minute ring per model, kept
// alongside the per-second ring rather than replacing it. The two answer
// different questions — the second ring gives exact trailing-60s TPM/RPM, the
// minute ring gives a trend that outlives the 30-minute window — and the newest
// minute bucket is the same traffic the second ring sums, so the chart's
// right-hand edge agrees with the live numbers.
const (
	// historyRingMinutes is how deep the per-minute ring goes: 24 hours.
	historyRingMinutes = 24 * 60
	// historyMaxSeries bounds how many per-model lines the chart receives. The
	// admin page assigns one colour per line and stops being readable well before
	// this, but the cap also bounds the JSON for a proxy that has seen many models.
	historyMaxSeries = 8
)

// historyRangeSeconds are the windows the page offers, mapped to the bucket size
// used for each. Every bucket size divides an hour, so the x axis lands on round
// minute marks instead of drifting a little further off the clock per tick.
const (
	historyRange1h  = 3600
	historyRange6h  = 6 * 3600
	historyRange24h = 24 * 3600
)

var historyRangeSeconds = map[string]int{
	"1h":  historyRange1h,
	"6h":  historyRange6h,
	"24h": historyRange24h,
}

func historyWindowSeconds(name string) int {
	if sec, ok := historyRangeSeconds[name]; ok {
		return sec
	}
	return historyRangeSeconds["24h"]
}

// historyBucketSeconds picks the bucket size for a window. The target is roughly
// 60-100 points: enough resolution to see a hiccup, few enough that the page can
// draw one SVG path per model without a rendering budget.
func historyBucketSeconds(rangeSec int) int {
	switch {
	case rangeSec <= historyRange1h:
		return 60
	case rangeSec <= historyRange6h:
		return 300
	default:
		return 900
	}
}

// historyMinute is one minute of one model's traffic. Unlike secBucket it also
// keeps latency, because "did the upstream get slower" is half of what the chart
// is for, and a per-minute sum lets the page average it back out.
type historyMinute struct {
	min        int64
	tokens     int64
	requests   int64
	failures   int64
	latencyMS  int64
	maxLatency int64
	// input is the fresh (uncached) prompt tokens. It is kept apart from tokens
	// because tokens also counts the completion, and a cache hit rate whose
	// denominator included the model's own output would sag as answers got longer.
	input int64
	// cacheRead and cacheCreation are prompt tokens served from the upstream's
	// cache and written into it. They are kept apart from tokens so the chart can
	// show a hit rate: folding them into one number would lose the split.
	cacheRead     int64
	cacheCreation int64
}

func (m *historyMinute) add(o *historyMinute) {
	m.tokens += o.tokens
	m.requests += o.requests
	m.failures += o.failures
	m.latencyMS += o.latencyMS
	if o.maxLatency > m.maxLatency {
		m.maxLatency = o.maxLatency
	}
	m.input += o.input
	m.cacheRead += o.cacheRead
	m.cacheCreation += o.cacheCreation
}

// historyPoint is one bucket on the chart: the counters for a model inside
// [ts, ts+bucketSeconds).
type historyPoint struct {
	TS           int64   `json:"ts"`
	Tokens       int64   `json:"tokens"`
	Requests     int64   `json:"requests"`
	Failures     int64   `json:"failures"`
	LatencyMS    int64   `json:"latency_ms"`
	MaxLatencyMS int64   `json:"max_latency_ms"`
	FailureRate  float64 `json:"failure_rate"`
	AvgLatencyMS int64   `json:"avg_latency_ms"`
	// Input is the fresh (uncached) prompt tokens; with the two cache counters it
	// is the denominator the hit rate is computed from.
	Input     int64 `json:"input"`
	CacheRead int64 `json:"cache_read"`
	// CacheCreation is in the payload because it belongs in the denominator: a
	// bucket that only wrote to the cache read nothing, and hiding it would make
	// the ratio look better than it was.
	CacheCreation int64   `json:"cache_creation"`
	CacheHitRate  float64 `json:"cache_hit_rate"`
}

type historyModelSeries struct {
	Model  string         `json:"model"`
	Points []historyPoint `json:"points"`
	// The window totals behind the points, so the summary table does not have to
	// re-add 96 buckets in the browser.
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
	CacheHitRate  float64 `json:"cache_hit_rate"`
}

type historyTotals struct {
	Requests      int64 `json:"requests"`
	Failures      int64 `json:"failures"`
	Tokens        int64 `json:"tokens"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

type historySnapshot struct {
	// Range is the window name the caller asked for, echoed back so the page can
	// label the axis without re-deriving it from the timestamps.
	Range string `json:"range"`
	// From is the oldest bucket start, To is the exclusive end of the newest
	// bucket. Both are aligned to the bucket grid, so len(Timestamps) buckets
	// cover the window exactly.
	From          int64                `json:"from"`
	To            int64                `json:"to"`
	BucketSeconds int                  `json:"bucket_seconds"`
	Timestamps    []int64              `json:"timestamps"`
	Totals        historyTotals        `json:"totals"`
	Models        []historyModelSeries `json:"models"`
	// AllModels names every model seen in the window, ranked by tokens, so the
	// page's filter chips do not depend on which series survived the cap.
	AllModels []string `json:"all_models"`
	// ModelsExcluded counts the models folded into the (other) series.
	ModelsExcluded int `json:"models_excluded"`
}

// bumpHistory records one finished request into the current minute of the model's
// history ring. It runs under the collector lock, next to the second-ring write.
func (m *modelStats) bumpHistory(now time.Time, tokens int64, failed bool, latency time.Duration, input, cacheRead, cacheCreation int64) {
	min := now.Unix() / 60 * 60
	b := &m.history[min%historyRingMinutes]
	if b.min != min {
		*b = historyMinute{min: min}
	}
	b.tokens += tokens
	b.requests++
	if failed {
		b.failures++
	}
	b.input += input
	b.cacheRead += cacheRead
	b.cacheCreation += cacheCreation
	ms := latency.Milliseconds()
	b.latencyMS += ms
	if ms > b.maxLatency {
		b.maxLatency = ms
	}
}

// historySnapshotLocked builds the chart payload. Only buckets that are still
// inside both the requested window and the ring's retention are considered, so a
// slot whose minute has been overwritten can never contribute.
//
// A bucket with no traffic comes back as an explicit zero point: the page plots
// one point per bucket, and dropping empty buckets would shift every later point
// left and misdate the series.
func (c *statsCollector) historySnapshotLocked(now time.Time, rangeSec int, models []string) historySnapshot {
	bucket := int64(historyBucketSeconds(rangeSec))
	// The newest bucket is the one in progress, so it ends at the next boundary
	// rather than at now: a chart whose newest point is up to one bucket stale
	// would hide a spike until the bucket closed.
	end := now.Unix()/bucket*bucket + bucket
	points := int(rangeSec) / int(bucket)
	start := end - int64(points)*bucket
	timestamps := make([]int64, points)
	for i := range timestamps {
		timestamps[i] = start + int64(i)*bucket
	}

	snap := historySnapshot{
		From:          start,
		To:            end,
		BucketSeconds: int(bucket),
		Timestamps:    timestamps,
		Models:        []historyModelSeries{},
		AllModels:     []string{},
	}

	// Anything older than the ring's oldest retained minute is simply not there.
	// Reporting the whole window as zeros would claim "no traffic" for a period
	// this process cannot actually see, so the window is clipped to what exists.
	minRetained := now.Unix()/60*60 - int64(historyRingMinutes-1)*60
	firstBucket := start
	if firstBucket < minRetained {
		firstBucket = minRetained
	}

	wanted := map[string]bool{}
	for _, name := range models {
		wanted[name] = true
	}

	type ranked struct {
		model         string
		tokens        int64
		points        []historyPoint
		input         int64
		cacheRead     int64
		cacheCreation int64
	}
	var all []ranked

	for _, m := range c.models {
		if len(wanted) > 0 && !wanted[m.model] {
			continue
		}
		byBucket := make([]historyMinute, points)
		var tokens, input, cacheRead, cacheCreation int64
		seen := false
		for i := range m.history {
			h := &m.history[i]
			if h.min == 0 || h.min < firstBucket || h.min >= end {
				continue
			}
			idx := int((h.min - start) / bucket)
			if idx < 0 || idx >= points {
				continue
			}
			byBucket[idx].add(h)
			tokens += h.tokens
			input += h.input
			cacheRead += h.cacheRead
			cacheCreation += h.cacheCreation
			seen = true
		}
		if !seen {
			continue
		}
		series := make([]historyPoint, points)
		for i := range series {
			series[i] = historyPoint{
				TS:            timestamps[i],
				Tokens:        byBucket[i].tokens,
				Requests:      byBucket[i].requests,
				Failures:      byBucket[i].failures,
				LatencyMS:     byBucket[i].latencyMS,
				MaxLatencyMS:  byBucket[i].maxLatency,
				Input:         byBucket[i].input,
				CacheRead:     byBucket[i].cacheRead,
				CacheCreation: byBucket[i].cacheCreation,
			}
			if byBucket[i].requests > 0 {
				series[i].FailureRate = float64(byBucket[i].failures) / float64(byBucket[i].requests)
				series[i].AvgLatencyMS = byBucket[i].latencyMS / byBucket[i].requests
			}
			series[i].CacheHitRate = cacheHitRate(byBucket[i].cacheRead, byBucket[i].cacheCreation, byBucket[i].input)
		}
		all = append(all, ranked{
			model:         m.model,
			tokens:        tokens,
			points:        series,
			input:         input,
			cacheRead:     cacheRead,
			cacheCreation: cacheCreation,
		})
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].tokens != all[j].tokens {
			return all[i].tokens > all[j].tokens
		}
		return all[i].model < all[j].model
	})

	for _, r := range all {
		snap.AllModels = append(snap.AllModels, r.model)
	}

	// Everything past the cap is summed into one (other) series rather than
	// dropped: on a proxy that has seen many models, silently hiding the tail
	// would understate total traffic and hide failures along with it.
	kept := all
	if len(kept) > historyMaxSeries {
		snap.ModelsExcluded = len(kept) - historyMaxSeries
		kept = kept[:historyMaxSeries]
	}
	for _, r := range kept {
		snap.Models = append(snap.Models, historyModelSeries{
			Model:         r.model,
			Points:        r.points,
			CacheRead:     r.cacheRead,
			CacheCreation: r.cacheCreation,
			CacheHitRate:  cacheHitRate(r.cacheRead, r.cacheCreation, r.input),
		})
	}
	if snap.ModelsExcluded > 0 {
		other := make([]historyPoint, points)
		for i := range other {
			other[i].TS = timestamps[i]
		}
		var otherRead, otherCreation, otherInput int64
		for _, r := range all[historyMaxSeries:] {
			otherRead += r.cacheRead
			otherCreation += r.cacheCreation
			otherInput += r.input
			for i := range other {
				other[i].Tokens += r.points[i].Tokens
				other[i].Requests += r.points[i].Requests
				other[i].Failures += r.points[i].Failures
				other[i].LatencyMS += r.points[i].LatencyMS
				other[i].Input += r.points[i].Input
				other[i].CacheRead += r.points[i].CacheRead
				other[i].CacheCreation += r.points[i].CacheCreation
				if r.points[i].MaxLatencyMS > other[i].MaxLatencyMS {
					other[i].MaxLatencyMS = r.points[i].MaxLatencyMS
				}
			}
		}
		for i := range other {
			if other[i].Requests > 0 {
				other[i].FailureRate = float64(other[i].Failures) / float64(other[i].Requests)
				other[i].AvgLatencyMS = other[i].LatencyMS / other[i].Requests
			}
			other[i].CacheHitRate = cacheHitRate(other[i].CacheRead, other[i].CacheCreation, other[i].Input)
		}
		snap.Models = append(snap.Models, historyModelSeries{
			Model:         otherModelKey,
			Points:        other,
			CacheRead:     otherRead,
			CacheCreation: otherCreation,
			CacheHitRate:  cacheHitRate(otherRead, otherCreation, otherInput),
		})
	}

	// Totals cover every model in the window, not just the plotted ones, so the
	// headline numbers stay honest when the cap folds the tail away.
	for _, r := range all {
		snap.Totals.CacheRead += r.cacheRead
		snap.Totals.CacheCreation += r.cacheCreation
		for i := range r.points {
			snap.Totals.Requests += r.points[i].Requests
			snap.Totals.Failures += r.points[i].Failures
			snap.Totals.Tokens += r.points[i].Tokens
		}
	}
	return snap
}

// cacheHitRate is the share of the prompt the upstream served from its cache.
//
// The denominator is every prompt token the upstream had to read: fresh input
// plus the cache counters. Dividing by the fresh input alone would report a hit
// rate above 100% on a well-cached request, and leaving cache writes out of the
// denominator would count a request that only wrote to the cache as a pure win.
//
// A model that reported no cache counters at all reads as 0%, which is the
// honest answer: nothing was served from cache. A bucket with no prompt tokens
// divides by zero, so it is 0 as well.
func cacheHitRate(cacheRead, cacheCreation, freshInput int64) float64 {
	prompt := cacheRead + cacheCreation + freshInput
	if prompt <= 0 {
		return 0
	}
	return float64(cacheRead) / float64(prompt)
}

// history builds the chart payload under the collector lock.
func (c *statsCollector) history(rangeSec int, models []string) historySnapshot {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.historySnapshotLocked(now, rangeSec, models)
	for name, sec := range historyRangeSeconds {
		if sec == rangeSec {
			snap.Range = name
		}
	}
	return snap
}

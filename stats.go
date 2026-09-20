package main

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	// statsRingSeconds is how much history the per-second ring keeps: 30 minutes,
	// enough for both the trailing-60s TPM/RPM window and the sparkline.
	statsRingSeconds = 1800
	// statsSparkMinutes is the width of the per-minute token trend the UI draws.
	statsSparkMinutes = 30
	// maxTrackedModels bounds how many distinct model names the collector keeps.
	// The /v1/chat/completions passthrough takes the model straight from the
	// client body, so without a cap a caller could grow this map without bound.
	maxTrackedModels = 128
	// otherModelKey absorbs every model beyond maxTrackedModels.
	otherModelKey = "(other)"
	// unknownModelKey is used when a request carries no usable model name.
	unknownModelKey = "(unknown)"
	// recentErrorLimit is how many failure details are kept per model.
	recentErrorLimit = 50
)

// Failure categories surfaced by the admin page.
const (
	catUpstream5xx       = "upstream_5xx"
	catRateLimited       = "rate_limited"
	catUpstream4xx       = "upstream_4xx"
	catNetworkTimeout    = "network_timeout"
	catNetworkError      = "network_error"
	catEmptyResponse     = "empty_response"
	catStreamStalled     = "stream_stalled"
	catTranslateError    = "translate_error"
	catVLMDescribeFailed = "vlm_describe_failed"
)

// classifyStatus maps an upstream HTTP status onto a failure category. It returns
// "" for a status the proxy passes through as a success.
func classifyStatus(code int) string {
	switch {
	case code == 429:
		return catRateLimited
	case code >= 500:
		return catUpstream5xx
	case code >= 400:
		return catUpstream4xx
	}
	return ""
}

// classifyError maps a transport-level failure onto a failure category.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errBodyIdle) {
		return catStreamStalled
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return catNetworkTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return catNetworkTimeout
	}
	return catNetworkError
}

// secBucket is one second of one model's traffic. sec lets a stale slot be told
// apart from a live one without clearing the whole ring.
type secBucket struct {
	sec      int64
	tokens   int64
	requests int64
	failures int64
}

type errorEvent struct {
	Time     time.Time `json:"time"`
	Alias    string    `json:"alias,omitempty"`
	Category string    `json:"category"`
	Status   int       `json:"status,omitempty"`
	Message  string    `json:"message,omitempty"`
}

type modelStats struct {
	model    string
	upstream string
	aliases  map[string]int64

	requests   int64
	successes  int64
	failures   int64
	input      int64
	output     int64
	latency    time.Duration
	maxLatency time.Duration
	// estimated is set once any successful call on this model had to have its
	// token counts derived from text length instead of upstream usage.
	estimated bool

	ring   [statsRingSeconds]secBucket
	errors map[string]int64
	recent []errorEvent
}

func (m *modelStats) bump(now time.Time, tokens int64, failed bool) {
	sec := now.Unix()
	b := &m.ring[sec%statsRingSeconds]
	if b.sec != sec {
		*b = secBucket{sec: sec}
	}
	b.requests++
	b.tokens += tokens
	if failed {
		b.failures++
	}
}

// statsCollector aggregates per-model traffic in memory. It is deliberately
// allocation-free on the request path: recording does integer arithmetic and a
// fixed-size ring write, and only snapshot() builds objects.
type statsCollector struct {
	mu     sync.Mutex
	start  time.Time
	models map[string]*modelStats
	now    func() time.Time
}

func newStatsCollector() *statsCollector {
	return newStatsCollectorWithClock(time.Now)
}

func newStatsCollectorWithClock(now func() time.Time) *statsCollector {
	return &statsCollector{
		start:  now(),
		models: map[string]*modelStats{},
		now:    now,
	}
}

// stats is the process-wide collector the proxy handlers record into.
var stats = newStatsCollector()

// reqStat tracks one in-flight request. It publishes at most once, so adding a
// failure call next to an existing early return can never double-count.
type reqStat struct {
	c        *statsCollector
	alias    string
	model    string
	upstream string
	start    time.Time
	done     bool
}

// beginReq starts tracking a request against the model it will actually call.
func (c *statsCollector) beginReq(alias, model, upstream string) *reqStat {
	return c.beginReqAt(c.now(), alias, model, upstream)
}

// beginReqAt is beginReq with an explicit start time, so latency covers the whole
// request (including the VLM describe pass) rather than only the upstream call.
func (c *statsCollector) beginReqAt(start time.Time, alias, model, upstream string) *reqStat {
	return &reqStat{c: c, alias: alias, model: model, upstream: upstream, start: start}
}

// success records a completed request. usage may be a zero value when the
// upstream reported nothing.
func (r *reqStat) success(u tokenUsage) {
	if r == nil {
		return
	}
	r.c.record(r, u, "", 0, "")
}

// failure records a request that did not produce a usable reply.
func (r *reqStat) failure(category string, status int, message string) {
	if r == nil {
		return
	}
	r.c.record(r, tokenUsage{}, category, status, message)
}

// warn records a non-fatal problem against a model without counting a request.
// It is used for failures the proxy recovers from, such as a VLM describe call
// that failed before the request was re-routed.
func (c *statsCollector) warn(alias, model, upstream, category, message string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.modelEntryLocked(model)
	if m.upstream == "" {
		m.upstream = upstream
	}
	m.noteErrorLocked(c.now(), errorEvent{Alias: alias, Category: category, Message: truncate(message, 200)})
}

func (c *statsCollector) record(r *reqStat, u tokenUsage, category string, status int, message string) {
	if c == nil || r.done {
		return
	}
	r.done = true

	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	m := c.modelEntryLocked(r.model)
	if m.upstream == "" {
		m.upstream = r.upstream
	}
	if r.alias != "" {
		m.aliases[r.alias]++
	}

	failed := category != ""
	m.requests++
	if failed {
		m.failures++
	} else {
		m.successes++
	}
	m.input += int64(u.Input)
	m.output += int64(u.Output)
	if !u.Reported && !failed {
		m.estimated = true
	}

	elapsed := now.Sub(r.start)
	if elapsed < 0 {
		elapsed = 0
	}
	m.latency += elapsed
	if elapsed > m.maxLatency {
		m.maxLatency = elapsed
	}

	m.bump(now, int64(u.total()), failed)

	if failed {
		m.noteErrorLocked(now, errorEvent{
			Alias:    r.alias,
			Category: category,
			Status:   status,
			Message:  truncate(message, 200),
		})
	}
}

func (m *modelStats) noteErrorLocked(now time.Time, ev errorEvent) {
	ev.Time = now
	m.errors[ev.Category]++
	m.recent = append(m.recent, ev)
	if len(m.recent) > recentErrorLimit {
		m.recent = append(m.recent[:0], m.recent[len(m.recent)-recentErrorLimit:]...)
	}
}

func (c *statsCollector) modelEntryLocked(model string) *modelStats {
	if model == "" {
		model = unknownModelKey
	}
	if m, ok := c.models[model]; ok {
		return m
	}
	if len(c.models) >= maxTrackedModels {
		model = otherModelKey
		if m, ok := c.models[model]; ok {
			return m
		}
	}
	m := &modelStats{model: model, aliases: map[string]int64{}, errors: map[string]int64{}}
	c.models[model] = m
	return m
}

// reset clears all counters and restarts the uptime clock.
func (c *statsCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = map[string]*modelStats{}
	c.start = c.now()
}

type modelSnapshot struct {
	Model        string           `json:"model"`
	Upstream     string           `json:"upstream,omitempty"`
	Aliases      map[string]int64 `json:"aliases"`
	Estimated    bool             `json:"estimated"`
	Requests     int64            `json:"requests"`
	Successes    int64            `json:"successes"`
	Failures     int64            `json:"failures"`
	FailureRate  float64          `json:"failure_rate"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	TotalTokens  int64            `json:"total_tokens"`
	TPM          int64            `json:"tpm"`
	RPM          int64            `json:"rpm"`
	AvgLatencyMS int64            `json:"avg_latency_ms"`
	MaxLatencyMS int64            `json:"max_latency_ms"`
	ErrorCounts  map[string]int64 `json:"error_counts"`
	RecentErrors []errorEvent     `json:"recent_errors"`
	Sparkline    []int64          `json:"sparkline"`
}

type statsTotals struct {
	Requests int64 `json:"requests"`
	Failures int64 `json:"failures"`
	TPM      int64 `json:"tpm"`
	RPM      int64 `json:"rpm"`
}

type statsSnapshot struct {
	Version       string          `json:"version"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Totals        statsTotals     `json:"totals"`
	Models        []modelSnapshot `json:"models"`
}

func (c *statsCollector) snapshot() statsSnapshot {
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	snap := statsSnapshot{GeneratedAt: now, Models: []modelSnapshot{}}
	if !c.start.IsZero() && now.After(c.start) {
		snap.UptimeSeconds = int64(now.Sub(c.start).Seconds())
	}
	for _, m := range c.models {
		ms := m.snapshotLocked(now)
		snap.Models = append(snap.Models, ms)
		snap.Totals.Requests += ms.Requests
		snap.Totals.Failures += ms.Failures
		snap.Totals.TPM += ms.TPM
		snap.Totals.RPM += ms.RPM
	}
	sort.Slice(snap.Models, func(i, j int) bool {
		if snap.Models[i].TotalTokens != snap.Models[j].TotalTokens {
			return snap.Models[i].TotalTokens > snap.Models[j].TotalTokens
		}
		return snap.Models[i].Model < snap.Models[j].Model
	})
	return snap
}

func (m *modelStats) snapshotLocked(now time.Time) modelSnapshot {
	nowSec := now.Unix()
	var windowTokens, windowRequests int64

	// sparkline is oldest-first: index statsSparkMinutes-1 is the current minute.
	spark := make([]int64, statsSparkMinutes)
	for i := range m.ring {
		b := &m.ring[i]
		if b.sec == 0 {
			continue
		}
		age := nowSec - b.sec
		if age < 0 || age >= statsRingSeconds {
			continue
		}
		if age < 60 {
			windowTokens += b.tokens
			windowRequests += b.requests
		}
		if idx := statsSparkMinutes - 1 - int(age/60); idx >= 0 {
			spark[idx] += b.tokens
		}
	}

	ms := modelSnapshot{
		Model:        m.model,
		Upstream:     m.upstream,
		Aliases:      copyInt64Map(m.aliases),
		Estimated:    m.estimated,
		Requests:     m.requests,
		Successes:    m.successes,
		Failures:     m.failures,
		InputTokens:  m.input,
		OutputTokens: m.output,
		TotalTokens:  m.input + m.output,
		TPM:          windowTokens,
		RPM:          windowRequests,
		MaxLatencyMS: m.maxLatency.Milliseconds(),
		ErrorCounts:  copyInt64Map(m.errors),
		RecentErrors: copyErrorsNewestFirst(m.recent),
		Sparkline:    spark,
	}
	if m.requests > 0 {
		ms.FailureRate = float64(m.failures) / float64(m.requests)
		ms.AvgLatencyMS = (m.latency / time.Duration(m.requests)).Milliseconds()
	}
	return ms
}

func copyInt64Map(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copyErrorsNewestFirst returns the retained failures with the most recent first,
// which is the order the admin page renders them in.
func copyErrorsNewestFirst(in []errorEvent) []errorEvent {
	out := make([]errorEvent, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}

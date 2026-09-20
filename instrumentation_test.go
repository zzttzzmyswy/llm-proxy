package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withRoutes installs a routing table for the duration of a test.
func withRoutes(t *testing.T, routes map[string]RouteEntry) {
	t.Helper()
	cfgMu.Lock()
	old := routeTargets
	routeTargets = routes
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		routeTargets = old
		cfgMu.Unlock()
	})
}

func withCleanStats(t *testing.T) {
	t.Helper()
	stats.reset()
	t.Cleanup(stats.reset)
}

func findModel(t *testing.T, name string) modelSnapshot {
	t.Helper()
	for _, m := range stats.snapshot().Models {
		if m.Model == name {
			return m
		}
	}
	t.Fatalf("model %q was never recorded; snapshot has %+v", name, stats.snapshot().Models)
	return modelSnapshot{}
}

// A non-streaming Anthropic reply must be accounted with the usage the upstream
// reported, under the model the request was routed to.
func TestInstrumentAnthropicNonStream(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "application/json", nonStreamJSONBody)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "DeepSeek-Flash")
	if m.Requests != 1 || m.Successes != 1 || m.Failures != 0 {
		t.Fatalf("counts: %+v", m)
	}
	if m.InputTokens != 5 || m.OutputTokens != 5 {
		t.Fatalf("usage must come from the reply body, got in=%d out=%d", m.InputTokens, m.OutputTokens)
	}
	if m.Estimated {
		t.Fatal("a reply carrying usage must not be flagged as estimated")
	}
	if m.Aliases["sonnet"] != 1 {
		t.Fatalf("the alias must be recorded, got %+v", m.Aliases)
	}
}

// A streaming Anthropic reply reports usage across message_start and message_delta.
func TestInstrumentAnthropicStream(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "text/event-stream", strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":700,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, ""))

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "DeepSeek-Flash")
	if m.Requests != 1 || m.Failures != 0 {
		t.Fatalf("a completed stream must count as one success, got %+v", m)
	}
	if m.InputTokens != 700 || m.OutputTokens != 42 {
		t.Fatalf("streamed usage must be read from the SSE events, got in=%d out=%d", m.InputTokens, m.OutputTokens)
	}
}

func TestInstrumentUpstream5xx(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"overloaded"}`)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "DeepSeek-Flash")
	if m.Requests != 1 || m.Failures != 1 || m.Successes != 0 {
		t.Fatalf("a 5xx must count as a failure, got %+v", m)
	}
	if m.ErrorCounts[catUpstream5xx] != 1 {
		t.Fatalf("failure must be categorised, got %+v", m.ErrorCounts)
	}
	if len(m.RecentErrors) != 1 || m.RecentErrors[0].Status != 503 {
		t.Fatalf("the failure detail must be retained, got %+v", m.RecentErrors)
	}
}

func TestInstrumentUpstreamUnreachable(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	// A closed listener gives a connection-refused error on every attempt.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = deadURL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "DeepSeek-Flash")
	if m.Failures != 1 {
		t.Fatalf("an unreachable upstream must count as a failure, got %+v", m)
	}
	if m.ErrorCounts[catNetworkError] == 0 && m.ErrorCounts[catNetworkTimeout] == 0 {
		t.Fatalf("the transport failure must be categorised, got %+v", m.ErrorCounts)
	}
}

func TestInstrumentEmptyResponse(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "application/json", "")

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if m := findModel(t, "DeepSeek-Flash"); m.ErrorCounts[catEmptyResponse] != 1 {
		t.Fatalf("an empty upstream body must be categorised, got %+v", m.ErrorCounts)
	}
}

// A request routed to the OpenAI gateway is accounted against the model that
// gateway serves, with the token counts it reported.
func TestInstrumentOpenAIRoute(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"flash": {Model: "glm-5.3-flash", Upstream: "openai"}})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","model":"glm-5.3-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":33,"completion_tokens":7}}`)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.OpenAIURL
	cfg.Upstream.OpenAIURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.OpenAIURL = oldURL })

	callHandleMessages(t, `{"model":"flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "glm-5.3-flash")
	if m.Upstream != "openai" {
		t.Fatalf("the gateway must be recorded, got %q", m.Upstream)
	}
	if m.Requests != 1 || m.Failures != 0 {
		t.Fatalf("counts: %+v", m)
	}
	if m.InputTokens != 33 || m.OutputTokens != 7 {
		t.Fatalf("openai usage must be read, got in=%d out=%d", m.InputTokens, m.OutputTokens)
	}
}

// The passthrough endpoint reports usage too, and must not disturb the response
// the client receives.
func TestInstrumentChatCompletionsPassthrough(t *testing.T) {
	withCleanStats(t)

	const upstreamBody = `{"id":"c1","model":"glm-5.3-flash","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":12,"completion_tokens":4}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.OpenAIURL
	cfg.Upstream.OpenAIURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.OpenAIURL = oldURL })

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","messages":[]}`))
	w := httptest.NewRecorder()
	handleChatCompletions(w, req)

	if w.Body.String() != upstreamBody {
		t.Fatalf("the passthrough body must reach the client unchanged, got %q", w.Body.String())
	}
	m := findModel(t, "glm-5.3-flash")
	if m.InputTokens != 12 || m.OutputTokens != 4 {
		t.Fatalf("passthrough usage must be read, got in=%d out=%d", m.InputTokens, m.OutputTokens)
	}
}

// A save from the admin page must change where the next request goes, without a
// restart.
func TestInstrumentRoutingFollowsReload(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "Before-Model"}})
	withUpstream(t, "application/json", nonStreamJSONBody)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	findModel(t, "Before-Model")

	cfgMu.Lock()
	routes := make(map[string]RouteEntry, len(routeTargets))
	for k, v := range routeTargets {
		routes[k] = v
	}
	routes["sonnet"] = RouteEntry{Model: "After-Model"}
	routeTargets = routes
	cfgMu.Unlock()

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	findModel(t, "After-Model")
}

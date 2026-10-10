package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingUpstream starts an upstream that counts the TCP connections it accepts,
// so a test can tell a reused connection pool from a fresh connection per request.
func countingUpstream(t *testing.T, contentType, body string) (string, *int64) {
	t.Helper()
	var conns int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte(body))
	}))
	up.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	return up.URL, &conns
}

// Every upstream call must share one connection pool. A transport built per
// request owns its own pool and nothing ever closes it, so each request strands
// its connection: the process accumulates ESTABLISHED sockets to the upstream
// until it exhausts its file descriptors. Production reached 1046 established
// connections in two days on the live proxy.
func TestUpstreamConnectionsAreReusedAcrossRequests(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	url, conns := countingUpstream(t, "application/json", nonStreamJSONBody)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = url
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	const requests = 25
	for i := 0; i < requests; i++ {
		callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	}

	// A pooled client reuses one kept-alive connection for all of these. The
	// slack allows for a connection the server retires under load; the leak made
	// this equal the request count.
	if got := atomic.LoadInt64(conns); got > 3 {
		t.Fatalf("%d requests opened %d upstream connections; the pool is not being reused", requests, got)
	}
}

// The pool must survive a config reload that does not change the header timeout:
// the admin page reloads the config on every save, and a reload that dropped the
// pool would strand its connections exactly as a per-request transport does.
func TestUpstreamPoolSurvivesConfigReload(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	url, conns := countingUpstream(t, "application/json", nonStreamJSONBody)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = url
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	afterFirst := atomic.LoadInt64(conns)

	// A reload republishes the same config, as a no-op save from the admin page
	// does.
	storeConfig(cfg, routeTargets, declaredRoutes)

	for i := 0; i < 10; i++ {
		callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	}

	if got := atomic.LoadInt64(conns); got != afterFirst {
		t.Fatalf("a reload opened %d new upstream connections; the pool must be reused", got-afterFirst)
	}
}

// upstreamTransport must hand back the same pool for the same timeout, and the
// client a request builds must actually use it.
func TestUpstreamTransportIsSharedPerTimeout(t *testing.T) {
	a := upstreamTransport(120 * time.Second)
	b := upstreamTransport(120 * time.Second)
	if a != b {
		t.Fatal("the same header timeout must reuse one transport")
	}
	rc := reqConfig{cfg: Config{Upstream: UpstreamConfig{HeaderTimeoutSeconds: 120}}}
	if got := rc.httpClient().Transport; got != a {
		t.Fatalf("httpClient must use the shared transport, got %p want %p", got, a)
	}
}

// The transport cache is bounded, so a long sequence of distinct timeouts cannot
// grow it without limit.
func TestUpstreamTransportCacheIsBounded(t *testing.T) {
	upstreamTransportsMu.Lock()
	saved := upstreamTransports
	upstreamTransports = map[time.Duration]*http.Transport{}
	upstreamTransportsMu.Unlock()
	t.Cleanup(func() {
		upstreamTransportsMu.Lock()
		upstreamTransports = saved
		upstreamTransportsMu.Unlock()
	})

	for i := 0; i < maxUpstreamTransports*3; i++ {
		upstreamTransport(time.Duration(i+1) * time.Second)
	}

	upstreamTransportsMu.Lock()
	n := len(upstreamTransports)
	upstreamTransportsMu.Unlock()
	if n > maxUpstreamTransports {
		t.Fatalf("the transport cache grew to %d entries, cap is %d", n, maxUpstreamTransports)
	}
}

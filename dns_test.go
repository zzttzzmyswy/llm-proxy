package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResolver stands in for net.Resolver so the dialer's cache and fallback
// paths can be driven without a DNS server.
type fakeResolver struct {
	mu    sync.Mutex
	addrs []string
	err   error
	calls int
}

func (f *fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.addrs, nil
}

func (f *fakeResolver) answer(addrs []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs, f.err = addrs, err
}

func (f *fakeResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// servfailErr mirrors what the production log showed for www.sophnet.com: a
// SERVFAIL from the local resolver, which is not a timeout and not NXDOMAIN.
func servfailErr() error {
	return &net.DNSError{Err: "server misbehaving", Name: "upstream.test", IsTemporary: true}
}

// startListener returns a live loopback listener and its port, so a dial has
// something real to connect to.
func startListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A lookup that succeeded must cover the requests that follow it, so the hot
// path does not depend on the resolver being healthy every time.
func TestUpstreamDialerCachesLookup(t *testing.T) {
	port := startListener(t)
	r := &fakeResolver{addrs: []string{"127.0.0.1"}}
	d := newUpstreamDialer(r)

	addr := net.JoinHostPort("upstream.test", strconv.Itoa(port))
	for i := 0; i < 3; i++ {
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.Close()
	}
	if got := r.count(); got != 1 {
		t.Errorf("resolver calls = %d, want 1 (the cached address must be reused)", got)
	}
}

// Once the cached address expires the dialer must go back to the resolver, so a
// moved upstream is picked up without a restart.
func TestUpstreamDialerRefreshesAfterTTL(t *testing.T) {
	port := startListener(t)
	r := &fakeResolver{addrs: []string{"127.0.0.1"}}
	d := newUpstreamDialer(r)
	now := time.Now()
	d.now = func() time.Time { return now }

	addr := net.JoinHostPort("upstream.test", strconv.Itoa(port))
	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	conn.Close()

	now = now.Add(upstreamAddressTTL + time.Second)
	conn, err = d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial after TTL: %v", err)
	}
	conn.Close()

	if got := r.count(); got != 2 {
		t.Errorf("resolver calls = %d, want 2 (one per TTL window)", got)
	}
}

// The failure this whole mechanism exists for: the resolver goes bad after a
// successful lookup. The dial must still reach the upstream on the last known
// address instead of failing the request.
func TestUpstreamDialerFallsBackToLastKnownAddress(t *testing.T) {
	port := startListener(t)
	r := &fakeResolver{addrs: []string{"127.0.0.1"}}
	d := newUpstreamDialer(r)
	now := time.Now()
	d.now = func() time.Time { return now }

	addr := net.JoinHostPort("upstream.test", strconv.Itoa(port))
	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	conn.Close()

	var logged bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(oldOut)

	now = now.Add(upstreamAddressTTL + time.Second)
	r.answer(nil, servfailErr())

	conn, err = d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial during resolver outage: %v (want the cached address to be used)", err)
	}
	conn.Close()

	if !strings.Contains(logged.String(), "[DNS] lookup upstream.test failed") {
		t.Errorf("resolver outage must be logged, got %q", logged.String())
	}
}

// With nothing cached there is no address to fall back to: the resolver error
// must surface, and it must be one the retry loop considers retryable.
func TestUpstreamDialerFailsWithoutCachedAddress(t *testing.T) {
	r := &fakeResolver{err: servfailErr()}
	d := newUpstreamDialer(r)

	_, err := d.DialContext(context.Background(), "tcp", "upstream.test:443")
	if err == nil {
		t.Fatal("dial must fail when the resolver fails and nothing is cached")
	}
	if !isRetryableError(err) {
		t.Errorf("resolver failure %v must be retryable", err)
	}
}

// An address literal must bypass the resolver entirely — there is nothing to
// look up, and a failure here is a real connectivity problem.
func TestUpstreamDialerPassesThroughAddressLiterals(t *testing.T) {
	port := startListener(t)
	r := &fakeResolver{err: servfailErr()}
	d := newUpstreamDialer(r)

	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial to literal address: %v", err)
	}
	conn.Close()
	if got := r.count(); got != 0 {
		t.Errorf("resolver calls = %d, want 0 for an address literal", got)
	}
}

// A resolver outage must not make every request pay the resolver's timeout: one
// probe per interval is enough to notice recovery, and the requests in between
// go straight to the cached address.
func TestUpstreamDialerBacksOffResolverProbes(t *testing.T) {
	port := startListener(t)
	r := &fakeResolver{addrs: []string{"127.0.0.1"}}
	d := newUpstreamDialer(r)
	now := time.Now()
	d.now = func() time.Time { return now }

	addr := net.JoinHostPort("upstream.test", strconv.Itoa(port))
	dial := func() {
		t.Helper()
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn.Close()
	}

	dial() // warms the cache
	r.answer(nil, servfailErr())

	now = now.Add(upstreamAddressTTL + time.Second)
	dial()
	if got := r.count(); got != 2 {
		t.Fatalf("resolver calls = %d, want 2 (the expired cache must be re-probed once)", got)
	}

	// Inside the probe interval the resolver must be left alone.
	dial()
	dial()
	if got := r.count(); got != 2 {
		t.Errorf("resolver calls = %d, want 2 (probes must be backed off)", got)
	}

	now = now.Add(upstreamProbeInterval + time.Second)
	dial()
	if got := r.count(); got != 3 {
		t.Errorf("resolver calls = %d, want 3 (the probe must resume after the interval)", got)
	}
}

// The end-to-end contract the production incident broke: a resolver failure must
// consume the retry budget rather than becoming a 502 on the first attempt.
func TestPostUpstreamRetriesResolverFailure(t *testing.T) {
	r := &fakeResolver{err: servfailErr()}
	old := upstreamDial
	upstreamDial = newUpstreamDialer(r)
	t.Cleanup(func() { upstreamDial = old })

	rc := reqConfig{cfg: Config{Upstream: UpstreamConfig{MaxRetries: 2}}}
	_, err := postUpstream(context.Background(), "https://upstream.test/v1/messages", []byte("{}"), nil, rc)
	if err == nil {
		t.Fatal("postUpstream must report the failure once retries are exhausted")
	}
	// One lookup for the initial attempt plus one per retry.
	if got, want := r.count(), rc.maxRetries()+1; got != want {
		t.Errorf("resolver calls = %d, want %d", got, want)
	}
}

// Adding a custom dial hook is easy to get wrong: without ForceAttemptHTTP2
// net/http silently drops to HTTP/1.1, and the upstream gateway serves HTTP/2.
func TestUpstreamTransportKeepsHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := newUpstreamTransport(30 * time.Second)
	tr.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("upstream protocol = HTTP/%d, want HTTP/2", resp.ProtoMajor)
	}
}

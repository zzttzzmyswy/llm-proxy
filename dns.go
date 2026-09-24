package main

import (
	"context"
	"log"
	"net"
	"sync"
	"time"
)

// hostResolver is the part of net.Resolver the upstream dialer uses. Tests
// substitute a fake so the cache and fallback paths can be driven without a
// DNS server.
type hostResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// upstreamAddressTTL bounds how long a resolved upstream address is reused
// before it is looked up again. The upstream names are stable public endpoints
// behind a load balancer, so a few minutes of reuse costs nothing and keeps the
// resolver off the hot path.
const upstreamAddressTTL = 5 * time.Minute

// upstreamProbeInterval is how long the dialer waits before asking a failing
// resolver again. A resolver outage lasts minutes, and without this every
// request in that window would block on the resolver's own timeout before
// falling back to the cached address — the log shows lookups taking tens of
// seconds. One probe per interval keeps recovery prompt and the rest fast.
const upstreamProbeInterval = 30 * time.Second

// upstreamDialer dials upstream hostnames through a cache of the addresses that
// last worked.
//
// It exists because the local resolver is not reliable: the proxy's log shows
// www.sophnet.com failing to resolve with "server misbehaving" (SERVFAIL) and
// dropped UDP queries in bursts that outlast any retry budget, which turned into
// client-visible 502s. The upstream name is stable, so a lookup that succeeded
// once stays usable: when a fresh lookup fails, the dial falls back to the last
// known-good address and logs the degradation. If that address is itself dead
// the dial error is retryable and the next attempt resolves again, so recovery
// needs no operator action.
type upstreamDialer struct {
	resolver hostResolver
	dialer   *net.Dialer
	ttl      time.Duration
	probe    time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedAddrs
}

type cachedAddrs struct {
	addrs   []string
	expires time.Time
	// probeAfter defers the next resolver attempt after a failed one, so a long
	// outage costs one slow request per probe interval instead of all of them.
	probeAfter time.Time
}

func newUpstreamDialer(resolver hostResolver) *upstreamDialer {
	return &upstreamDialer{
		resolver: resolver,
		// Timeout and KeepAlive match the net/http defaults this dialer
		// replaces, so upstream dial behavior is otherwise unchanged.
		dialer: &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second},
		ttl:    upstreamAddressTTL,
		probe:  upstreamProbeInterval,
		now:    time.Now,
		cache:  make(map[string]cachedAddrs),
	}
}

// upstreamDial is the process-wide dialer behind every upstream request. The
// cache is shared across requests and across the per-request transports, so one
// successful lookup covers the whole proxy.
var upstreamDial = newUpstreamDialer(net.DefaultResolver)

// DialContext satisfies the http.Transport dial hook. It resolves host itself so
// the cache sits between the transport and the resolver; TLS is unaffected,
// because net/http still derives SNI and certificate verification from the
// request URL rather than from the address dialed here.
func (d *upstreamDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		// Not a host:port pair, or an address literal — nothing to resolve.
		return d.dialer.DialContext(ctx, network, addr)
	}
	addrs, err := d.addrs(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, a := range addrs {
		conn, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(a, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = &net.DNSError{Err: "no addresses", Name: host}
	}
	return nil, lastErr
}

// addrs returns the addresses to dial for host: the cached set while it is
// fresh, otherwise a fresh lookup, falling back to the last known-good set when
// that lookup fails.
func (d *upstreamDialer) addrs(ctx context.Context, host string) ([]string, error) {
	now := d.now()
	d.mu.Lock()
	cached, ok := d.cache[host]
	d.mu.Unlock()
	if ok {
		if now.Before(cached.expires) {
			return cached.addrs, nil
		}
		if now.Before(cached.probeAfter) {
			return cached.addrs, nil
		}
	}

	found, err := d.resolver.LookupHost(ctx, host)
	if err == nil && len(found) == 0 {
		err = &net.DNSError{Err: "no addresses", Name: host}
	}
	if err != nil {
		if ok && len(cached.addrs) > 0 {
			log.Printf("[DNS] lookup %s failed (%v); reusing last known address %v\n", host, err, cached.addrs)
			d.mu.Lock()
			// Re-read: another request may have refreshed the entry meanwhile.
			if cur, still := d.cache[host]; still {
				cur.probeAfter = now.Add(d.probe)
				d.cache[host] = cur
			}
			d.mu.Unlock()
			return cached.addrs, nil
		}
		return nil, err
	}

	d.mu.Lock()
	d.cache[host] = cachedAddrs{addrs: found, expires: now.Add(d.ttl)}
	d.mu.Unlock()
	return found, nil
}

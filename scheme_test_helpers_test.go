package main

// Test helpers for the scheme-file configuration model.
//
// The old tests mutated global config fields directly (cfg.Upstream.AnthropicURL
// and friends). Those fields no longer exist: a request's upstream comes from the
// active scheme, so every test that needs a different upstream publishes a whole
// Scheme instead. That is also closer to what production does — a scheme is
// published atomically and never patched field by field.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testUpstream builds a resolved upstream pointing at url, so a test server can
// stand in for a real gateway.
func testUpstream(name, protocol, url string) Upstream {
	return Upstream{
		Name:          name,
		Protocol:      protocol,
		URL:           url,
		Key:           "test-key",
		HeaderTimeout: defaultHeaderTimeoutSeconds * time.Second,
		BodyIdle:      defaultBodyIdleSeconds * time.Second,
		MaxRetries:    defaultMaxRetries,
		Spec: UpstreamSpec{
			Protocol: protocol,
			URL:      url,
			Key:      "test-key",
		},
	}
}

// anthropicTextUpstream is the common case: an anthropic upstream at url.
func anthropicTextUpstream(url string) Upstream {
	return testUpstream("test", protocolAnthropic, url)
}

// testScheme is a scheme with the given text upstream and model table, no VLM and
// no chat upstream. Tests that need those fill the fields in.
func testScheme(text Upstream, routes map[string]RouteEntry) *Scheme {
	if routes == nil {
		routes = map[string]RouteEntry{}
	}
	declared := make(map[string]bool, len(routes))
	for alias := range routes {
		declared[alias] = true
	}
	return &Scheme{Name: "test", Text: text, Routes: routes, DeclaredRoutes: declared}
}

// ensureScheme publishes a fresh scheme if none is active, and returns it. It is
// the single place tests borrow the global state: every mutator below patches the
// published scheme in place, so withRoutes and withAnthropicUpstream can be called
// in either order.
func ensureScheme(t *testing.T) *Scheme {
	t.Helper()
	if s := currentScheme(); s != nil {
		return s
	}
	s := testScheme(anthropicTextUpstream("http://127.0.0.1:1"), nil)
	withScheme(t, s)
	return s
}

// withScheme publishes a scheme for the duration of a test and restores the
// previous one on cleanup, so tests never leak upstream state into each other.
func withScheme(t *testing.T, s *Scheme) {
	t.Helper()
	prevCfg, prevScheme := currentConfig(), currentScheme()
	cfgMu.Lock()
	cfg, scheme = prevCfg, s
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg, scheme = prevCfg, prevScheme
		cfgMu.Unlock()
	})
}

// withAnthropicUpstream points the active scheme's text upstream at url, so a test
// server stands in for the real gateway. The scheme keeps whatever model table it
// has; a test that wants one calls withRoutes.
func withAnthropicUpstream(t *testing.T, url string) {
	t.Helper()
	s := ensureScheme(t)
	s.Text = keepTimeouts(s.Text, anthropicTextUpstream(url))
}

// withRoutes installs a model table on the active scheme.
func withRoutes(t *testing.T, routes map[string]RouteEntry) {
	t.Helper()
	s := ensureScheme(t)
	declared := make(map[string]bool, len(routes))
	for alias := range routes {
		declared[alias] = true
	}
	s.Routes, s.DeclaredRoutes = routes, declared
}

// withVLM adds an image-describe pass to the active scheme. The describe calls
// reuse the scheme's text upstream, which is the common single-gateway shape.
func withVLM(t *testing.T, model string, maxTokens int) {
	t.Helper()
	s := ensureScheme(t)
	if model == "" {
		// An empty model is how a scheme disables the describe pass.
		s.VLM = nil
		return
	}
	s.VLM = &VLMScheme{Upstream: s.Text, Model: model, MaxTokens: maxTokens}
	s.VLMInheritsText = true
}

// withOpenAIChatUpstream adds an openai [chat] upstream at url to the scheme.
func withOpenAIChatUpstream(t *testing.T, url string) {
	t.Helper()
	s := ensureScheme(t)
	s.Chat = testUpstream("test/chat", protocolOpenAI, url)
	s.ChatInheritsText = false
}

// withOpenAITextUpstream switches the active scheme's text upstream to an openai
// one at url, which is how a test exercises the protocol-translation path.
func withOpenAITextUpstream(t *testing.T, url string) {
	t.Helper()
	s := ensureScheme(t)
	s.Text = keepTimeouts(s.Text, testUpstream("test", protocolOpenAI, url))
	s.Chat = s.Text
	s.ChatInheritsText = true
}

// withTimeouts overrides the text upstream's timeout/retry fields.
func withTimeouts(t *testing.T, headerSec, bodyIdleSec, maxRetries int) {
	t.Helper()
	up := &ensureScheme(t).Text
	if headerSec > 0 {
		up.HeaderTimeout = time.Duration(headerSec) * time.Second
	}
	if bodyIdleSec > 0 {
		up.BodyIdle = time.Duration(bodyIdleSec) * time.Second
	}
	if maxRetries > 0 {
		up.MaxRetries = maxRetries
	}
}

// writeSchemeTree writes a main config and one scheme file into a temporary
// directory and points LLM_PROXY_CONFIG at it, so loadConfig and the admin save
// path can be exercised end to end.
func writeSchemeTree(t *testing.T, mainTOML, schemeName, schemeTOML string) (configPath, schemePath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(mainTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	schemeDir := filepath.Join(dir, "schemes")
	if err := os.MkdirAll(schemeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	schemePath = filepath.Join(schemeDir, schemeName+".toml")
	if err := os.WriteFile(schemePath, []byte(schemeTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_PROXY_CONFIG", configPath)
	return configPath, schemePath
}

// textUpstreamOrFail returns the snapshot's text upstream, failing the test when no
// scheme is published.
func (rc reqConfig) textUpstreamOrFail(t *testing.T) Upstream {
	t.Helper()
	up, ok := rc.textUpstream()
	if !ok {
		t.Fatal("no scheme is published")
	}
	return up
}

// withSchemeAnthropic publishes an anthropic text upstream at url and re-points an
// already-declared describe pass at it. Tests that then call withSchemeChat end up
// with an openai text upstream and an anthropic VLM, which is the two-gateway shape
// the translation path exists for.
func withSchemeAnthropic(t *testing.T, url string) {
	t.Helper()
	s := ensureScheme(t)
	s.Text = keepTimeouts(s.Text, anthropicTextUpstream(url))
	if s.VLM != nil {
		s.VLM.Upstream = s.Text
	}
}

// keepTimeouts copies the text upstream's timeout/retry settings onto a
// replacement, so a test that shortens them is not undone by a later call that
// swaps the upstream itself.
func keepTimeouts(old, next Upstream) Upstream {
	next.HeaderTimeout = old.HeaderTimeout
	next.BodyIdle = old.BodyIdle
	next.MaxRetries = old.MaxRetries
	next.Spec.HeaderTimeoutSeconds = old.Spec.HeaderTimeoutSeconds
	next.Spec.BodyIdleSeconds = old.Spec.BodyIdleSeconds
	next.Spec.MaxRetries = old.Spec.MaxRetries
	return next
}

// withSchemeChat publishes an openai text upstream (and the matching chat
// passthrough) at url. An already-declared describe pass keeps the anthropic
// upstream it was given, because an openai gateway cannot serve /v1/messages.
func withSchemeChat(t *testing.T, url string) {
	t.Helper()
	s := ensureScheme(t)
	s.Text = keepTimeouts(s.Text, testUpstream("test", protocolOpenAI, url))
	s.Chat = s.Text
	s.ChatInheritsText = true
	if s.VLM != nil {
		s.VLMInheritsText = false
	}
}

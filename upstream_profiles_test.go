package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream is a stand-in upstream that records what reached it: the path it
// was called on, the model in the body, and the key it was given. Tests assert
// that two profiles route to their own server with their own key, so both the
// byte counts and the header spelling (x-api-key vs Authorization: Bearer)
// matter.
type fakeUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	calls  int
	paths  []string
	models []string
	keys   []string
	auth   []string
}

// anthropicUpstream answers /v1/messages in the Anthropic shape.
func anthropicUpstream(t *testing.T, text string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(body, &m)
		model, _ := m["model"].(string)

		f.mu.Lock()
		f.calls++
		f.paths = append(f.paths, r.URL.Path)
		f.models = append(f.models, model)
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","model":%q,"id":"m1","usage":{"input_tokens":1,"output_tokens":1}}`, text, model))
	}))
	t.Cleanup(f.Close)
	return f
}

// openAIUpstream answers /v1/chat/completions in the OpenAI shape.
func openAIUpstream(t *testing.T, text string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(body, &m)
		model, _ := m["model"].(string)

		f.mu.Lock()
		f.calls++
		f.paths = append(f.paths, r.URL.Path)
		f.models = append(f.models, model)
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, model, text))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) snapshot() (calls int, paths, models, keys, auth []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.paths...), append([]string(nil), f.models...),
		append([]string(nil), f.keys...), append([]string(nil), f.auth...)
}

// withProfiles loads a config from a file and restores the live config when the
// test ends. It mirrors withTempConfig but keeps the declared profile map, which
// the admin round-trip test needs.
func withProfiles(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_PROXY_CONFIG", path)

	cfgMu.Lock()
	prevCfg, prevRoutes, prevDeclared := cfg, routeTargets, declaredRoutes
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg, routeTargets, declaredRoutes = prevCfg, prevRoutes, prevDeclared
		cfgMu.Unlock()
	})

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return path
}

// ---- 1. an existing config keeps behaving exactly as it did ----

// A config that predates profiles — only [upstream] anthropic_url/openai_url,
// [keys].sophnet and default_upstream — must resolve to the same routes, URLs and
// key as before, with no edit. The two implicit profiles stand in for it.
func TestLegacyConfigResolvesToImplicitProfiles(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `[upstream]
anthropic_url = "https://anthropic.example/base"
openai_url = "https://openai.example/v1"

[keys]
sophnet = "legacy-key"

[routing]
sonnet = "DeepSeek-V4-Flash-0731"
flash = { model = "glm-5.3-flash", upstream = "openai" }
`)

	rc := snapshotConfig()
	if up, ok := rc.profile(""); !ok {
		t.Fatal("the implicit anthropic profile must always resolve")
	} else {
		if up.Protocol != protocolAnthropic {
			t.Fatalf("implicit anthropic profile must speak the anthropic protocol, got %q", up.Protocol)
		}
		if up.URL != "https://anthropic.example/base" {
			t.Fatalf("implicit anthropic profile must take [upstream].anthropic_url, got %q", up.URL)
		}
		if up.Key != "legacy-key" {
			t.Fatalf("implicit anthropic profile must take [keys].sophnet, got %q", up.Key)
		}
	}
	if up, ok := rc.profile("openai"); !ok {
		t.Fatal("the implicit openai profile must always resolve")
	} else if up.Protocol != protocolOpenAI || up.URL != "https://openai.example/v1" || up.Key != "legacy-key" {
		t.Fatalf("implicit openai profile must take [upstream].openai_url and the legacy key, got %+v", up)
	}

	// The routing table is unchanged: a plain string stays on "" (the implicit
	// anthropic profile) and an explicit "openai" keeps its name.
	if e := rc.routes["sonnet"]; e.Model != "DeepSeek-V4-Flash-0731" || e.Upstream != "" {
		t.Fatalf("a legacy plain-string route must stay on the default profile, got %+v", e)
	}
	if e := rc.routes["flash"]; e.Upstream != "openai" {
		t.Fatalf("a legacy upstream=\"openai\" route must keep its name, got %+v", e)
	}

	// And the endpoints are byte-identical to the pre-profile ones.
	if got := rc.forUpstream(mustProfile(t, rc, "")).anthropicMessagesURL(); got != "https://anthropic.example/base/v1/messages" {
		t.Fatalf("anthropic endpoint changed: %q", got)
	}
	if got := rc.forUpstream(mustProfile(t, rc, "openai")).openAICompletionsURL(); got != "https://openai.example/v1/v1/chat/completions" {
		t.Fatalf("openai endpoint changed: %q", got)
	}
}

// mustProfile resolves a profile or fails the test.
func mustProfile(t *testing.T, rc reqConfig, name string) Upstream {
	t.Helper()
	up, ok := rc.profile(name)
	if !ok {
		t.Fatalf("profile %q did not resolve", name)
	}
	return up
}

// ---- 2. two profiles, two protocols, two keys, no crossing ----

// A routing entry's upstream is a profile name. Two profiles speaking different
// protocols must each reach their own server carrying their own key, in the
// header spelling their protocol uses.
func TestTwoProfilesRouteToTheirOwnUpstreamAndKey(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	anthro := anthropicUpstream(t, "from deepseek")
	oai := openAIUpstream(t, "from local")

	withProfiles(t, `
[upstreams.deepseek]
protocol = "anthropic"
url = "`+anthro.URL+`"
key = "key-deepseek"

[upstreams.local]
protocol = "openai"
url = "`+oai.URL+`"
key = "key-local"

[routing]
sonnet = { model = "deepseek-chat", upstream = "deepseek" }
flash = { model = "local-glm", upstream = "local" }
`)

	sonnetResp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	flashResp := callHandleMessages(t, `{"model":"flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(sonnetResp, "from deepseek") {
		t.Fatalf("sonnet must be answered by the deepseek profile, got %q", sonnetResp)
	}
	if !strings.Contains(flashResp, "from local") {
		t.Fatalf("flash must be answered by the local profile, got %q", flashResp)
	}

	ac, apaths, amodels, akeys, aauth := anthro.snapshot()
	if ac != 1 || apaths[0] != "/v1/messages" {
		t.Fatalf("the anthropic profile must be called once on /v1/messages, got %d calls on %v", ac, apaths)
	}
	if amodels[0] != "deepseek-chat" {
		t.Fatalf("the anthropic profile must receive its own model, got %q", amodels[0])
	}
	// The anthropic protocol authenticates with x-api-key, never a bearer token.
	if akeys[0] != "key-deepseek" {
		t.Fatalf("the anthropic profile must send its own key as x-api-key, got %q", akeys[0])
	}
	if aauth[0] != "" {
		t.Fatalf("the anthropic profile must not send Authorization, got %q", aauth[0])
	}

	oc, opaths, omodels, okeys, oauth := oai.snapshot()
	if oc != 1 || opaths[0] != "/v1/chat/completions" {
		t.Fatalf("the openai profile must be called once on /v1/chat/completions, got %d calls on %v", oc, opaths)
	}
	if omodels[0] != "local-glm" {
		t.Fatalf("the openai profile must receive its own model, got %q", omodels[0])
	}
	if oauth[0] != "Bearer key-local" {
		t.Fatalf("the openai profile must send its own key as a bearer token, got %q", oauth[0])
	}
	if okeys[0] != "" {
		t.Fatalf("the openai profile must not send x-api-key, got %q", okeys[0])
	}
}

// ---- 3. key_env beats key; the implicit profiles keep the legacy key ----

// key_env names an environment variable that wins over the plaintext key, so a
// deployment can keep the secret out of the file.
func TestKeyEnvOverridesPlaintextKey(t *testing.T) {
	withCleanStats(t)
	t.Setenv("DEEPSEEK_API_KEY", "key-from-env")
	t.Setenv("SOPHNET_API_KEY", "legacy-env")

	up := anthropicUpstream(t, "ok")
	withProfiles(t, `
[upstreams.deepseek]
protocol = "anthropic"
url = "`+up.URL+`"
key = "key-from-file"
key_env = "DEEPSEEK_API_KEY"

[routing]
sonnet = { model = "m", upstream = "deepseek" }
`)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	_, _, _, keys, _ := up.snapshot()
	if keys[0] != "key-from-env" {
		t.Fatalf("key_env must win over the plaintext key, got %q", keys[0])
	}
}

// An unset key_env falls back to the plaintext key rather than skipping straight
// to the legacy secret.
func TestKeyEnvUnsetFallsBackToPlaintextKey(t *testing.T) {
	withCleanStats(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("SOPHNET_API_KEY", "legacy-env")

	up := anthropicUpstream(t, "ok")
	withProfiles(t, `
[upstreams.deepseek]
protocol = "anthropic"
url = "`+up.URL+`"
key = "key-from-file"
key_env = "DEEPSEEK_API_KEY"

[routing]
sonnet = { model = "m", upstream = "deepseek" }
`)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	_, _, _, keys, _ := up.snapshot()
	if keys[0] != "key-from-file" {
		t.Fatalf("an unset key_env must fall back to the plaintext key, got %q", keys[0])
	}
}

// A declared profile with no key of its own must not borrow the legacy secret:
// that would hand the default gateway's credential to a third-party upstream.
func TestDeclaredProfileWithoutKeyDoesNotBorrowLegacySecret(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "legacy-env")

	up := anthropicUpstream(t, "ok")
	withProfiles(t, `
[upstreams.thirdparty]
protocol = "anthropic"
url = "`+up.URL+`"

[routing]
sonnet = { model = "m", upstream = "thirdparty" }
`)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	_, _, _, keys, _ := up.snapshot()
	if keys[0] != "" {
		t.Fatalf("a keyless declared profile must send no key, got %q", keys[0])
	}
}

// The implicit profiles keep the historical SOPHNET_API_KEY-over-[keys].sophnet
// resolution, so an existing deployment's environment still authenticates.
func TestImplicitProfileKeepsLegacyKeyResolution(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "env-wins")
	withProfiles(t, `[upstream]
anthropic_url = "https://anthropic.example/api"

[keys]
sophnet = "from-file"
`)
	rc := snapshotConfig()
	if got := mustProfile(t, rc, "").Key; got != "env-wins" {
		t.Fatalf("SOPHNET_API_KEY must override [keys].sophnet, got %q", got)
	}

	t.Setenv("SOPHNET_API_KEY", "")
	rc = snapshotConfig()
	if got := mustProfile(t, rc, "").Key; got != "from-file" {
		t.Fatalf("without the env var the implicit profile must use [keys].sophnet, got %q", got)
	}
}

// ---- 4. per-profile timeout / retry override the globals ----

// A profile's timeout and retry settings override the [upstream] globals; leaving
// them unset inherits the global value.
func TestProfileTimeoutAndRetryOverrideGlobals(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `[upstream]
anthropic_url = "https://anthropic.example/api"
header_timeout_seconds = 120
body_idle_seconds = 90
max_retries = 2

[upstreams.slow]
protocol = "anthropic"
url = "https://slow.example/api"
header_timeout_seconds = 7
body_idle_seconds = 3
max_retries = 5

[upstreams.inherits]
protocol = "anthropic"
url = "https://inherits.example/api"

[routing]
sonnet = { model = "m", upstream = "slow" }
opus = { model = "m", upstream = "inherits" }
`)

	rc := snapshotConfig()
	slow := rc.forUpstream(mustProfile(t, rc, "slow"))
	if got := slow.headerTimeout().Seconds(); got != 7 {
		t.Fatalf("profile header timeout must override the global, got %v", got)
	}
	if got := slow.bodyIdle().Seconds(); got != 3 {
		t.Fatalf("profile body idle must override the global, got %v", got)
	}
	if got := slow.maxRetries(); got != 5 {
		t.Fatalf("profile max retries must override the global, got %d", got)
	}

	inh := rc.forUpstream(mustProfile(t, rc, "inherits"))
	if got := inh.headerTimeout().Seconds(); got != 120 {
		t.Fatalf("an unset profile header timeout must inherit the global, got %v", got)
	}
	if got := inh.bodyIdle().Seconds(); got != 90 {
		t.Fatalf("an unset profile body idle must inherit the global, got %v", got)
	}
	if got := inh.maxRetries(); got != 2 {
		t.Fatalf("an unset profile retry count must inherit the global, got %d", got)
	}

	// The retry count is what postUpstream actually loops on, so the override must
	// reach the wire rather than only the helper.
	if slow.headerTimeout() == inh.headerTimeout() {
		t.Fatal("the two profiles must not share one header timeout")
	}
}

// A profile's retry count is the one the request path uses: with a dead upstream
// the request must be attempted max_retries+1 times.
func TestProfileRetryCountReachesTheRequestPath(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	var attempts int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(503)
	}))
	t.Cleanup(dead.Close)

	withProfiles(t, `
[upstreams.flaky]
protocol = "anthropic"
url = "`+dead.URL+`"
max_retries = 1

[routing]
sonnet = { model = "m", upstream = "flaky" }
`)
	// Keep the retry backoff from dominating the test.
	old := retryBackoffs
	retryBackoffs = []time.Duration{0}
	t.Cleanup(func() { retryBackoffs = old })

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if got := atomic.LoadInt64(&attempts); got != 2 {
		t.Fatalf("max_retries=1 must produce 2 attempts, got %d", got)
	}
}

// ---- 5. default_upstream takes a profile name; unknown profiles are skipped ----

// default_upstream may name any defined profile, and every route without an
// explicit upstream follows it.
func TestDefaultUpstreamNamesAProfile(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	up := anthropicUpstream(t, "from deepseek")
	withProfiles(t, `
[upstreams.deepseek]
protocol = "anthropic"
url = "`+up.URL+`"
key = "k"

[upstream]
default_upstream = "deepseek"

[routing]
sonnet = "deepseek-chat"
`)

	if e := currentRoutes()["sonnet"]; e.Upstream != "deepseek" {
		t.Fatalf("default_upstream must fill the route with the profile name, got %+v", e)
	}

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if calls, _, models, keys, _ := up.snapshot(); calls != 1 || models[0] != "deepseek-chat" || keys[0] != "k" {
		t.Fatalf("default_upstream=deepseek must route there with its own key, got %d calls model=%v key=%v",
			calls, models, keys)
	}
}

// A routing entry naming a profile the config does not define is skipped with a
// warning rather than aborting the load or silently picking another gateway.
func TestUnknownProfileRouteIsSkippedWithAWarning(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	var logged strings.Builder
	oldOut := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(oldOut)

	withProfiles(t, `[routing]
ghost = { model = "m", upstream = "nonexistent" }
sonnet = "DeepSeek-V4-Flash-0731"
`)

	if _, ok := currentRoutes()["ghost"]; ok {
		t.Fatal("a route naming an undefined profile must be dropped from the served table")
	}
	if !strings.Contains(logged.String(), "nonexistent") {
		t.Fatalf("the dropped route must be reported, log was: %q", logged.String())
	}
	// The healthy route is untouched, so one typo does not disable the proxy.
	if e := currentRoutes()["sonnet"]; e.Model != "DeepSeek-V4-Flash-0731" {
		t.Fatalf("a valid route must survive a sibling typo, got %+v", e)
	}
	// A dropped builtin alias still gets its builtin fallback target.
	if e, ok := currentRoutes()["ghost"]; ok {
		t.Fatalf("the dropped alias must not reappear, got %+v", e)
	}
}

// A builtin alias whose declared upstream does not exist must fall back to the
// builtin default target rather than vanishing from the served table.
func TestBuiltinAliasWithUnknownProfileFallsBack(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `[routing]
sonnet = { model = "m", upstream = "typo-profile" }
`)
	e, ok := currentRoutes()["sonnet"]
	if !ok || e.Model == "" {
		t.Fatalf("sonnet must still be served after its profile is dropped, got %+v ok=%v", e, ok)
	}
	if e.Upstream != "" {
		t.Fatalf("the fallback must use the default profile, got upstream %q", e.Upstream)
	}
}

// ---- 6. vlm_upstream / chat_upstream pick independent profiles ----

// [proxy] vlm_upstream sends the image-description calls to its own upstream,
// separate from the route's profile.
func TestVLMUpstreamRoutesDescriptionToItsOwnProfile(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)
	t.Setenv("SOPHNET_API_KEY", "")

	text := anthropicUpstream(t, "text answer")
	vlm := anthropicUpstream(t, "一只猫")

	withProfiles(t, `
[proxy]
vlm_model = "MiniMax-M3"
vlm_max_tokens = 100
vlm_upstream = "vlm-profile"

[upstreams.vlm-profile]
protocol = "anthropic"
url = "`+vlm.URL+`"
key = "vlm-key"

[upstreams.text-profile]
protocol = "anthropic"
url = "`+text.URL+`"
key = "text-key"

[routing]
sonnet = { model = "deepseek-chat", upstream = "text-profile" }
`)

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"what is this"}]}]}`)

	if !strings.Contains(resp, "text answer") {
		t.Fatalf("the described request must reach the text profile, got %q", resp)
	}
	vc, _, vmodels, vkeys, _ := vlm.snapshot()
	if vc != 1 || vmodels[0] != "MiniMax-M3" || vkeys[0] != "vlm-key" {
		t.Fatalf("the describe call must go to the VLM profile with its own key, got %d calls model=%v key=%v",
			vc, vmodels, vkeys)
	}
	tc, _, tmodels, tkeys, _ := text.snapshot()
	if tc != 1 || tmodels[0] != "deepseek-chat" || tkeys[0] != "text-key" {
		t.Fatalf("the text call must go to the route's profile with its own key, got %d calls model=%v key=%v",
			tc, tmodels, tkeys)
	}
}

// The VLM fallback path (the describe pass failed, so the whole request is
// re-routed to the VLM) also runs on the VLM profile.
func TestVLMFallbackUsesTheVLMProfile(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)
	t.Setenv("SOPHNET_API_KEY", "")

	var vlmCalls int64
	// The VLM profile fails the describe pass, then answers the fallback.
	vlm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(body, &m)
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			if atomic.AddInt64(&vlmCalls, 1) == 1 {
				w.WriteHeader(500)
				io.WriteString(w, `{"error":{"message":"down"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"vlm fallback"}],"model":"MiniMax-M3","id":"v","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		t.Errorf("the text profile must not be called when the describe pass failed")
		w.WriteHeader(500)
	}))
	t.Cleanup(vlm.Close)

	var textCalls int64
	textUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&textCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(textUp.Close)

	withProfiles(t, `
[proxy]
vlm_model = "MiniMax-M3"
vlm_upstream = "vlm-profile"

[upstreams.vlm-profile]
protocol = "anthropic"
url = "`+vlm.URL+`"

[upstreams.text-profile]
protocol = "anthropic"
url = "`+textUp.URL+`"

[routing]
sonnet = { model = "deepseek-chat", upstream = "text-profile" }
`)

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}},{"type":"text","text":"what is this"}]}]}`)

	if !strings.Contains(resp, "vlm fallback") {
		t.Fatalf("the fallback must be answered by the VLM profile, got %q", resp)
	}
	if got := atomic.LoadInt64(&textCalls); got != 0 {
		t.Fatalf("the text profile must not be called on a describe failure, got %d calls", got)
	}
}

// [proxy] chat_upstream is the profile /v1/chat/completions forwards to.
func TestChatUpstreamRoutesPassthroughToItsOwnProfile(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	def := openAIUpstream(t, "default")
	alt := openAIUpstream(t, "alternate")

	withProfiles(t, `
[proxy]
chat_upstream = "alt"

[upstreams.alt]
protocol = "openai"
url = "`+alt.URL+`"
key = "alt-key"

[upstream]
openai_url = "`+def.URL+`"

[keys]
sophnet = "default-key"
`)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","messages":[]}`))
	w := httptest.NewRecorder()
	handleChatCompletions(w, req)

	if !strings.Contains(w.Body.String(), "alternate") {
		t.Fatalf("chat_upstream must forward to its profile, got %q", w.Body.String())
	}
	if calls, _, _, _, _ := alt.snapshot(); calls != 1 {
		t.Fatalf("the chat_upstream profile must be called once, got %d", calls)
	}
	_, _, _, _, auth := alt.snapshot()
	if auth[0] != "Bearer alt-key" {
		t.Fatalf("the chat passthrough must use the profile's key, got %q", auth[0])
	}
	if calls, _, _, _, _ := def.snapshot(); calls != 0 {
		t.Fatalf("the implicit openai profile must not be called when chat_upstream is set, got %d", calls)
	}
}

// A chat_upstream naming a profile that speaks the wrong protocol is refused at
// load and the passthrough keeps working on the implicit openai profile.
func TestChatUpstreamWithWrongProtocolFallsBack(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	def := openAIUpstream(t, "default")
	withProfiles(t, `
[proxy]
chat_upstream = "anthropic-only"

[upstreams.anthropic-only]
protocol = "anthropic"
url = "https://anthropic.example/api"

[upstream]
openai_url = "`+def.URL+`"
`)
	if calls, _, _, _, _ := def.snapshot(); calls != 0 {
		t.Fatal("no call should have happened at load")
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	w := httptest.NewRecorder()
	handleChatCompletions(w, req)

	if !strings.Contains(w.Body.String(), "default") {
		t.Fatalf("a wrong-protocol chat_upstream must fall back to the implicit openai profile, got %q", w.Body.String())
	}
	if calls, _, _, _, _ := def.snapshot(); calls != 1 {
		t.Fatalf("the implicit openai profile must serve the request, got %d calls", calls)
	}
}

// ---- 7. concurrency: two profiles under load, no key crossing ----

// Two profiles serving 50 concurrent requests each must all succeed, each with
// its own key, over a reused connection pool.
func TestConcurrentProfilesKeepTheirOwnKeysAndReuseConnections(t *testing.T) {
	withCleanStats(t)
	t.Setenv("SOPHNET_API_KEY", "")

	const perProfile = 50

	var anthroConns, openAIConns int64
	newCounting := func(name, body string, conns *int64) (*httptest.Server, *fakeUpstream) {
		f := &fakeUpstream{}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			f.mu.Lock()
			f.calls++
			f.keys = append(f.keys, r.Header.Get("x-api-key"))
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}))
		srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
			if s == http.StateNew {
				atomic.AddInt64(conns, 1)
			}
		}
		srv.Start()
		f.Server = srv
		return srv, f
	}

	anthroSrv, anthro := newCounting("anthropic", nonStreamJSONBody, &anthroConns)
	t.Cleanup(anthroSrv.Close)
	oaiSrv, oai := newCounting("openai",
		`{"id":"c1","model":"local-glm","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		&openAIConns)
	t.Cleanup(oaiSrv.Close)

	withProfiles(t, `
[upstreams.deepseek]
protocol = "anthropic"
url = "`+anthroSrv.URL+`"
key = "key-deepseek"

[upstreams.local]
protocol = "openai"
url = "`+oaiSrv.URL+`"
key = "key-local"

[routing]
sonnet = { model = "deepseek-chat", upstream = "deepseek" }
flash = { model = "local-glm", upstream = "local" }
`)

	var wg sync.WaitGroup
	for i := 0; i < perProfile; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		}()
		go func() {
			defer wg.Done()
			callHandleMessages(t, `{"model":"flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		}()
	}
	wg.Wait()

	ac, _, _, akeys, aauth := anthro.snapshot()
	if ac != perProfile {
		t.Fatalf("the anthropic profile must serve %d requests, got %d", perProfile, ac)
	}
	for i, k := range akeys {
		if k != "key-deepseek" {
			t.Fatalf("anthropic request %d carried key %q, want key-deepseek", i, k)
		}
	}
	for i, a := range aauth {
		if a != "" {
			t.Fatalf("anthropic request %d carried Authorization %q", i, a)
		}
	}

	oc, _, _, okeys, oauth := oai.snapshot()
	if oc != perProfile {
		t.Fatalf("the openai profile must serve %d requests, got %d", perProfile, oc)
	}
	for i, a := range oauth {
		if a != "Bearer key-local" {
			t.Fatalf("openai request %d carried Authorization %q, want Bearer key-local", i, a)
		}
	}
	for i, k := range okeys {
		if k != "" {
			t.Fatalf("openai request %d carried x-api-key %q", i, k)
		}
	}

	// Connections are pooled, not built per request. A concurrent burst opens as
	// many sockets as it has requests in flight, so the reuse property is asserted
	// the way upstream_pool_test.go asserts it: a batch that runs AFTER the burst
	// has finished must open no new connection at all, because every request in it
	// finds an idle pooled connection waiting.
	anthroAfter := atomic.LoadInt64(&anthroConns)
	openAIAfter := atomic.LoadInt64(&openAIConns)

	if anthroAfter >= int64(perProfile) {
		t.Fatalf("%d concurrent anthropic requests opened %d connections; each request stranded its own socket",
			perProfile, anthroAfter)
	}
	if openAIAfter >= int64(perProfile) {
		t.Fatalf("%d concurrent openai requests opened %d connections; each request stranded its own socket",
			perProfile, openAIAfter)
	}

	const sequential = 20
	for i := 0; i < sequential; i++ {
		callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		callHandleMessages(t, `{"model":"flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	}

	if got := atomic.LoadInt64(&anthroConns); got != anthroAfter {
		t.Fatalf("a sequential batch after the burst opened %d new anthropic connections; the pool is not reused",
			got-anthroAfter)
	}
	if got := atomic.LoadInt64(&openAIConns); got != openAIAfter {
		t.Fatalf("a sequential batch after the burst opened %d new openai connections; the pool is not reused",
			got-openAIAfter)
	}
}

// ---- 8. admin save must not lose [upstreams.*] ----

// The admin page does not edit profiles yet, but its save rewrites the whole
// file. A save must write the profiles and the two selectors back unchanged, or
// every route naming one would be left unroutable.
func TestAdminSavePreservesUpstreamProfiles(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	path := withProfiles(t, `
[proxy]
port = 8088
vlm_model = "MiniMax-M3"
vlm_upstream = "deepseek"
chat_upstream = "local"

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[upstreams.deepseek]
protocol = "anthropic"
url = "https://api.deepseek.com/anthropic"
key = "deepseek-secret"
header_timeout_seconds = 30

[upstreams.local]
protocol = "openai"
url = "http://127.0.0.1:9000/v1/chat/completions"
key_env = "LOCAL_API_KEY"

[keys]
sophnet = "sk-original"

[admin]
token = "s3cret"

[routing]
sonnet = { model = "deepseek-chat", upstream = "deepseek" }
`)

	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("config view: %d %s", w.Code, w.Body.String())
	}
	var view adminConfigView
	decodeBody(t, w, &view)

	res := postConfig(t, payloadFromView(t, view), "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}

	// The profiles and their keys survived to disk.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "[upstreams.deepseek]") || !strings.Contains(string(onDisk), "[upstreams.local]") {
		t.Fatalf("a save must keep the [upstreams.*] tables, file is:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), "deepseek-secret") {
		t.Fatalf("a save must keep the profile key, file is:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), `key_env = "LOCAL_API_KEY"`) {
		t.Fatalf("a save must keep key_env, file is:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), `vlm_upstream = "deepseek"`) ||
		!strings.Contains(string(onDisk), `chat_upstream = "local"`) {
		t.Fatalf("a save must keep the two profile selectors, file is:\n%s", onDisk)
	}

	// And they are live in the reloaded config, so the routes still resolve.
	rc := snapshotConfig()
	if _, ok := rc.profile("deepseek"); !ok {
		t.Fatal("the deepseek profile must survive the save")
	}
	if got := mustProfile(t, rc, "deepseek").Key; got != "deepseek-secret" {
		t.Fatalf("the profile key must survive the save, got %q", got)
	}
	if got := mustProfile(t, rc, "local").HeaderTimeout.Seconds(); got != 120 {
		t.Fatalf("an unset profile timeout must inherit the global after a save, got %v", got)
	}
	if rc.routes["sonnet"].Upstream != "deepseek" {
		t.Fatalf("the route must still name its profile, got %+v", rc.routes["sonnet"])
	}
	if rc.cfg.Proxy.VLMUpstream != "deepseek" || rc.cfg.Proxy.ChatUpstream != "local" {
		t.Fatalf("the selectors must survive the save, got %q / %q",
			rc.cfg.Proxy.VLMUpstream, rc.cfg.Proxy.ChatUpstream)
	}
}

// A save that references a profile the config does not define is rejected, so the
// page cannot write a route to a gateway that does not exist.
func TestAdminSaveRejectsUnknownProfileReference(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `
[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[admin]
token = "s3cret"

[routing]
sonnet = "Model-A"
`)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	p := payloadFromView(t, view)
	p.Routing[0].Upstream = "ghost-profile"
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("a save naming an undefined profile must be rejected, got %d %s", res.Code, res.Body.String())
	}

	// default_upstream is checked the same way.
	p = payloadFromView(t, view)
	p.Upstream.DefaultUpstream = "ghost-profile"
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("default_upstream naming an undefined profile must be rejected, got %d %s", res.Code, res.Body.String())
	}

	// A defined profile name is accepted.
	p = payloadFromView(t, view)
	p.Routing[0].Upstream = "openai"
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("the builtin profile names must still be accepted, got %d %s", res.Code, res.Body.String())
	}
}

// ---- keys never reach the logs ----

// No key may appear in the log output: not the profile's own, not the legacy one.
func TestProfileKeysNeverAppearInLogs(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)

	const profileSecret = "sk-profile-secret-abcdef"
	const legacySecret = "sk-legacy-secret-abcdef"
	t.Setenv("SOPHNET_API_KEY", legacySecret)
	t.Setenv("PROFILE_KEY_ENV", profileSecret)

	up := anthropicUpstream(t, "ok")
	var logged strings.Builder
	oldOut := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(oldOut)

	withProfiles(t, `
[proxy]
vlm_model = "MiniMax-M3"
vlm_upstream = "deepseek"

[upstreams.deepseek]
protocol = "anthropic"
url = "`+up.URL+`"
key = "`+profileSecret+`"

[upstreams.envkey]
protocol = "anthropic"
url = "`+up.URL+`"
key_env = "PROFILE_KEY_ENV"

[upstream]
anthropic_url = "`+up.URL+`"

[routing]
sonnet = { model = "deepseek-chat", upstream = "deepseek" }
opus = { model = "m", upstream = "envkey" }
`)

	// Exercise the routes, the VLM describe pass (which also fails once, so the
	// fallback path logs too) and a passthrough request.
	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	callHandleMessages(t, `{"model":"opus","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"x"}]}]}`)

	out := logged.String()
	for _, secret := range []string{profileSecret, legacySecret} {
		if strings.Contains(out, secret) {
			t.Fatalf("a key leaked into the log output:\n%s", out)
		}
	}
	if out == "" {
		t.Fatal("no log output was captured; the assertion would pass vacuously")
	}
}

// The admin config view must never hand the page a profile key — only whether one
// is configured and where it comes from.
func TestAdminViewNeverReturnsProfileKeys(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `
[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[upstreams.deepseek]
protocol = "anthropic"
url = "https://api.deepseek.com/anthropic"
key = "sk-must-not-be-returned"

[admin]
token = "s3cret"

[routing]
sonnet = { model = "deepseek-chat", upstream = "deepseek" }
`)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-must-not-be-returned") {
		t.Fatalf("the config view must not return a profile key, got %s", w.Body.String())
	}
}

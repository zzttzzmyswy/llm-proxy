package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The collector folds traffic into a bucket literally named "(other)" once it
// tracks maxTrackedModels models. The history chart then folds its own tail into
// a series with the same name, which used to put two same-named series in the
// payload: the page keys colours by name, so the two drew on top of each other and
// the tooltip reported only one of them.
func TestHistoryNeverEmitsTwoOtherSeries(t *testing.T) {
	c, clk := newTestCollector()
	for i := 0; i < maxTrackedModels+20; i++ {
		name := fmt.Sprintf("client-model-%03d", i)
		c.beginReq(name, name, "openai").success(tokenUsage{Input: 10, Output: 10, Reported: true})
	}
	for i := 0; i < 20; i++ {
		clk.advance(60_000_000_000)
	}

	snap := c.history(historyRange1h, nil)
	seen := 0
	for _, s := range snap.Models {
		if s.Model == otherModelKey {
			seen++
		}
	}
	if seen != 1 {
		names := make([]string, 0, len(snap.Models))
		for _, s := range snap.Models {
			names = append(names, s.Model)
		}
		b, _ := json.Marshal(names)
		t.Fatalf("%q must appear exactly once, got %d: %s", otherModelKey, seen, b)
	}

	// And the merged series must actually carry the tail's traffic, or the fix
	// would have replaced a duplicate with a silent under-count.
	var total int64
	for _, s := range snap.Models {
		if s.Model != otherModelKey {
			continue
		}
		for _, p := range s.Points {
			total += p.Requests
		}
	}
	if total == 0 {
		t.Fatal("the (other) series must carry the folded traffic")
	}
}

// profileByName folds "claude" into the anthropic profile, so a table under that
// name could never be reached. Accepting one at load would silently discard its
// URL and key; it is rejected instead.
func TestClaudeIsNotAUsableProfileName(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	withProfiles(t, `
[proxy]
port = 8088

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[upstreams.claude]
protocol = "openai"
url = "https://claude-named.example/v1"
key = "sk-claude"

[routing]
sonnet = { model = "m", upstream = "claude" }
`)

	c := currentConfig()
	if _, declared := c.Upstreams[profileNameClaudeAlias]; declared {
		t.Fatalf("[upstreams.claude] must be dropped at load, got %+v", c.Upstreams)
	}
	// And the alias itself still resolves to the anthropic profile, as documented.
	up, ok := c.profileByName(profileNameClaudeAlias)
	if !ok || up.Name != profileNameAnthropic || up.URL != "https://anthropic.example/api" {
		t.Fatalf("\"claude\" must keep resolving to the anthropic profile, got %+v ok=%v", up, ok)
	}
}

// The save validator must reject the same reserved name, so the operator is told
// while they can still rename the row.
func TestAdminSaveRejectsAProfileNamedClaude(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	configTOMLForTest(t, `
[proxy]
port = 8088
vlm_model = "VLM"

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[admin]
token = "s3cret"
`)

	p := adminConfigPayload{
		Proxy:    currentConfigView().Proxy,
		Upstream: currentConfigView().Upstream,
		Keys:     adminKeysPayload{Action: "keep"},
		Admin:    adminAuthPayload{Action: "keep"},
		Upstreams: []adminUpstreamProfilePayload{
			{Name: "claude", Protocol: "anthropic", URL: "https://other.example/api", KeyAction: "set", Key: "sk-x"},
		},
	}
	if _, err := validateConfigPayload(&p, currentConfig().Upstreams); err == nil {
		t.Fatal("a profile named \"claude\" must be rejected")
	}
}

// A route spelled upstream = "claude" is accepted by the loader, but the admin
// form's dropdowns offer profile names rather than that alias. Reporting the alias
// verbatim made the row fall back to the first option, so an untouched save
// silently rerouted it. The view reports the canonical name the dropdown has.
func TestAdminViewReportsClaudeRoutesAsTheAnthropicProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	withProfiles(t, `
[proxy]
port = 8088

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[admin]
token = "s3cret"

[routing]
sonnet = { model = "m1", upstream = "claude" }
`)

	view := currentConfigView()

	// Build the option set the page's dropdown actually offers.
	offered := map[string]bool{"": true}
	for _, p := range view.Upstreams {
		offered[p.Name] = true
	}
	for _, e := range view.Routing {
		if !offered[e.Upstream] {
			t.Fatalf("route %q reports upstream %q, which the dropdown does not offer", e.Alias, e.Upstream)
		}
	}
	if got := view.Upstream.DefaultUpstream; got != "" && !offered[got] {
		t.Fatalf("default_upstream %q is not among the dropdown options", got)
	}
	if got := view.Proxy.VLMUpstream; got != "" && !offered[got] {
		t.Fatalf("vlm_upstream %q is not among the dropdown options", got)
	}
	if got := view.Proxy.ChatUpstream; got != "" && !offered[got] {
		t.Fatalf("chat_upstream %q is not among the dropdown options", got)
	}
}

// The round trip has to be behaviour-preserving: an untouched save of a config
// whose route names "claude" must leave the route pointing at the anthropic
// profile, not at whatever the dropdown's first option happens to be.
func TestAdminUntouchedSaveKeepsAClaudeRouteWhereItWas(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	configTOMLForTest(t, `
[proxy]
port = 8088

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"
default_upstream = "deepseek"

[upstreams.deepseek]
protocol = "anthropic"
url = "https://deepseek.example/api"
key = "sk-d"

[admin]
token = "s3cret"

[routing]
sonnet = { model = "m1", upstream = "claude" }
`)

	before := currentRoutes()["sonnet"]
	if before.Upstream != profileNameClaudeAlias {
		t.Fatalf("fixture must start with the alias, got %+v", before)
	}

	w := postConfig(t, payloadFromViewWithProfiles(t, currentConfigView()), "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("save must succeed: %d %s", w.Code, w.Body.String())
	}

	after := currentRoutes()["sonnet"]
	if up, ok := currentConfig().profileByName(after.Upstream); !ok || up.Name != profileNameAnthropic {
		t.Fatalf("the route must still resolve to the anthropic profile after an untouched save, got %+v -> %+v", after, up)
	}
}

// Two saves running at once must each leave a whole payload behind, never a
// mixture and never an unparseable file.
//
// The save transaction reads the running config, resolves "keep" values out of
// it, then writes; the read therefore has to happen inside configSaveMu, or the
// second save of a pair bases itself on a snapshot taken before the first one
// committed and silently undoes it. That ordering is what the lock now
// guarantees, but the window is a few instructions wide, so this test checks the
// observable invariants around it rather than claiming to reproduce the
// interleaving every run.
func TestConcurrentSavesLeaveOneWholePayload(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	path := configTOMLForTest(t, `
[proxy]
port = 8088

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[admin]
token = "s3cret"

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"
key = "sk-alpha"

[upstreams.beta]
protocol = "anthropic"
url = "https://beta.example/api"
key = "sk-beta"

[routing]
sonnet = { model = "m", upstream = "alpha" }
`)

	base := payloadFromViewWithProfiles(t, currentConfigView())

	// Save A sets alpha's key; save B sets beta's key. Both start from the same
	// payload, which is what two browser tabs would post.
	a := base
	a.Upstreams = append([]adminUpstreamProfilePayload(nil), base.Upstreams...)
	b := base
	b.Upstreams = append([]adminUpstreamProfilePayload(nil), base.Upstreams...)
	for i := range a.Upstreams {
		if a.Upstreams[i].Name == "alpha" {
			a.Upstreams[i].KeyAction = "set"
			a.Upstreams[i].Key = "sk-alpha-new"
		}
	}
	for i := range b.Upstreams {
		if b.Upstreams[i].Name == "beta" {
			b.Upstreams[i].KeyAction = "set"
			b.Upstreams[i].Key = "sk-beta-new"
		}
	}

	var wg sync.WaitGroup
	for _, p := range []adminConfigPayload{a, b} {
		wg.Add(1)
		go func(p adminConfigPayload) {
			defer wg.Done()
			postConfig(t, p, "s3cret")
		}(p)
	}
	wg.Wait()

	// Whichever save landed last, the file must be one whole payload — never a
	// mixture that keeps a stale key from a snapshot taken before the other save.
	var reloaded Config
	if err := loadFromDiskInto(t, path, &reloaded); err != nil {
		t.Fatalf("the saved file must parse: %v", err)
	}
	normalizeUpstreamProfiles(&reloaded)
	alpha := reloaded.Upstreams["alpha"]
	beta := reloaded.Upstreams["beta"]
	if alpha.Key == "" || beta.Key == "" {
		t.Fatalf("both profiles must keep a key, got alpha=%q beta=%q", alpha.Key, beta.Key)
	}
	if alpha.Key != "sk-alpha-new" && beta.Key != "sk-beta-new" {
		t.Fatalf("at least one save's change must survive, got alpha=%q beta=%q", alpha.Key, beta.Key)
	}
}

// Clearing the key of a profile named after a builtin gateway does not leave it
// keyless: the legacy [keys].sophnet / SOPHNET_API_KEY credential still applies to
// that name. The save must say so instead of letting the operator believe the
// credential is gone.
func TestClearingABuiltinProfilesKeyExplainsTheLegacyFallback(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	configTOMLForTest(t, `
[proxy]
port = 8088

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[keys]
sophnet = "sk-legacy"

[admin]
token = "s3cret"

[upstreams.anthropic]
protocol = "anthropic"
url = "https://anthropic.example/api"
`)

	p := payloadFromViewWithProfiles(t, currentConfigView())
	for i := range p.Upstreams {
		if p.Upstreams[i].Name == profileNameAnthropic {
			p.Upstreams[i].KeyAction = "clear"
		}
	}

	w := postConfig(t, p, "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("save must succeed: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	decodeBody(t, w, &resp)

	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "keys].sophnet") {
		t.Fatalf("the warning must name the credential that still applies, got %q", joined)
	}

	// And the fallback really is in force — that is why the warning is needed.
	up, ok := currentConfig().profileByName(profileNameAnthropic)
	if !ok || up.Key != "sk-legacy" {
		t.Fatalf("the builtin name must still resolve to the legacy key, got %+v ok=%v", up, ok)
	}
}

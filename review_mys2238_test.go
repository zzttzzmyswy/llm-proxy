package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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

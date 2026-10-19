package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A scheme that declares its own vlm_model and vlm_max_tokens describes images
// with that model, without touching the global [proxy] section. This is the point
// of per-scheme VLMs: two upstreams can each bring their own vision model, so
// adding a scheme never means editing the shared config.
func TestSchemeOwnVLMModelDrivesTheDescribePass(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)
	t.Setenv("SOPHNET_API_KEY", "")

	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	var describeModels []string
	var describeMaxTokens []float64
	var textModel string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(body, &m)
		model, _ := m["model"].(string)

		<-mu
		if strings.HasPrefix(model, "vision-of-") {
			describeModels = append(describeModels, model)
			if mt, ok := m["max_tokens"].(float64); ok {
				describeMaxTokens = append(describeMaxTokens, mt)
			}
		} else {
			textModel = model
		}
		mu <- struct{}{}

		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(model, "vision-of-") {
			io.WriteString(w, fmt.Sprintf(`{"type":"message","content":[{"type":"text","text":"described by %s"}],"model":%q,"id":"v","usage":{"input_tokens":1,"output_tokens":1}}`, model, model))
			return
		}
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)

	withProfiles(t, `
[proxy]
vlm_model = "global-vision"
vlm_max_tokens = 8000

[upstreams.alpha]
protocol = "anthropic"
url = "`+upstream.URL+`"
vlm_model = "vision-of-alpha"
vlm_max_tokens = 111

[upstreams.beta]
protocol = "anthropic"
url = "`+upstream.URL+`"
vlm_model = "vision-of-beta"
vlm_max_tokens = 222

[routing]
sonnet = { model = "alpha-text", upstream = "alpha" }
opus = { model = "beta-text", upstream = "beta" }
`)

	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[`+image+`,{"type":"text","text":"alpha?"}]}]}`)
	<-mu
	if len(describeModels) != 1 || describeModels[0] != "vision-of-alpha" {
		t.Fatalf("a route on scheme alpha must describe with alpha's VLM, got %v", describeModels)
	}
	if describeMaxTokens[0] != 111 {
		t.Fatalf("alpha's vlm_max_tokens must cap its describe call, got %v", describeMaxTokens[0])
	}
	if textModel != "alpha-text" {
		t.Fatalf("the described request must still go to alpha's text model, got %q", textModel)
	}
	mu <- struct{}{}

	describeModels, describeMaxTokens, textModel = nil, nil, ""
	callHandleMessages(t, `{"model":"opus","max_tokens":10,"messages":[{"role":"user","content":[`+image+`,{"type":"text","text":"beta?"}]}]}`)
	<-mu
	if len(describeModels) != 1 || describeModels[0] != "vision-of-beta" {
		t.Fatalf("a route on scheme beta must describe with beta's VLM, got %v", describeModels)
	}
	if describeMaxTokens[0] != 222 {
		t.Fatalf("beta's vlm_max_tokens must cap its describe call, got %v", describeMaxTokens[0])
	}
	if textModel != "beta-text" {
		t.Fatalf("the described request must still go to beta's text model, got %q", textModel)
	}
	mu <- struct{}{}
}

// A scheme that declares no VLM of its own keeps inheriting the global [proxy]
// trio, and in particular the global vlm_upstream. That inheritance is what makes
// every existing config behave unchanged.
func TestSchemeWithoutOwnVLMInheritsTheGlobalTrio(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)
	t.Setenv("SOPHNET_API_KEY", "")

	vlm := anthropicUpstream(t, "global描述")
	text := anthropicUpstream(t, "text answer")

	withProfiles(t, `
[proxy]
vlm_model = "global-vision"
vlm_max_tokens = 4096
vlm_upstream = "vision-profile"

[upstreams.vision-profile]
protocol = "anthropic"
url = "`+vlm.URL+`"
key = "vision-key"

[upstreams.text-profile]
protocol = "anthropic"
url = "`+text.URL+`"
key = "text-key"

[routing]
sonnet = { model = "deepseek-chat", upstream = "text-profile" }
`)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"what is this"}]}]}`)

	vc, _, vmodels, vkeys, _ := vlm.snapshot()
	if vc != 1 || vmodels[0] != "global-vision" || vkeys[0] != "vision-key" {
		t.Fatalf("a scheme with no VLM of its own must use the global model on the global vlm_upstream, got %d calls model=%v key=%v",
			vc, vmodels, vkeys)
	}
	tc, _, tmodels, tkeys, _ := text.snapshot()
	if tc != 1 || tmodels[0] != "deepseek-chat" || tkeys[0] != "text-key" {
		t.Fatalf("the text call must go to the route's own profile, got %d calls model=%v key=%v", tc, tmodels, tkeys)
	}
}

// A scheme may point its describe calls at a different profile while keeping its
// own vision model — the "dedicated vision gateway" shape.
func TestSchemeVLMUpstreamOverridesTheGlobalOne(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)
	t.Setenv("SOPHNET_API_KEY", "")

	globalVLM := anthropicUpstream(t, "global vlm")
	schemeVLM := anthropicUpstream(t, "scheme vlm")
	text := anthropicUpstream(t, "text answer")

	withProfiles(t, `
[proxy]
vlm_model = "global-vision"
vlm_upstream = "global-vision-profile"

[upstreams.global-vision-profile]
protocol = "anthropic"
url = "`+globalVLM.URL+`"

[upstreams.scheme-vision]
protocol = "anthropic"
url = "`+schemeVLM.URL+`"
key = "scheme-vision-key"

[upstreams.text-profile]
protocol = "anthropic"
url = "`+text.URL+`"

[upstreams.alpha]
protocol = "anthropic"
url = "`+text.URL+`"
vlm_model = "alpha-vision"
vlm_upstream = "scheme-vision"

[routing]
sonnet = { model = "deepseek-chat", upstream = "alpha" }
`)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"hi"}]}]}`)

	if gc, _, _, _, _ := globalVLM.snapshot(); gc != 0 {
		t.Fatalf("the global vlm_upstream must not be used when the scheme names its own, got %d calls", gc)
	}
	sc, _, smodels, skeys, _ := schemeVLM.snapshot()
	if sc != 1 || smodels[0] != "alpha-vision" || skeys[0] != "scheme-vision-key" {
		t.Fatalf("the describe call must use the scheme's vision model on the scheme's vlm_upstream, got %d calls model=%v key=%v",
			sc, smodels, skeys)
	}
}

// [upstreams.<name>.routing] gives a scheme its own model mapping table, so
// adding an upstream does not require editing the global [routing] section.
func TestSchemeRoutingTableServesItsOwnAliases(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	alpha := anthropicUpstream(t, "alpha answer")

	withProfiles(t, `
[proxy]
vlm_model = ""

[upstreams.alpha]
protocol = "anthropic"
url = "`+alpha.URL+`"
key = "alpha-key"

[upstreams.alpha.routing]
alpha-fast = { model = "alpha-fast-model" }
alpha-smart = { model = "alpha-smart-model", supports_image = true }

[routing]
sonnet = { model = "global-model", upstream = "alpha" }
`)

	rc := snapshotConfig()

	fast := rc.routeTarget("alpha-fast")
	if fast.Model != "alpha-fast-model" || fast.Upstream != "alpha" {
		t.Fatalf("a scheme alias must resolve to the scheme's model and profile, got %+v", fast)
	}
	smart := rc.routeTarget("alpha-smart")
	if smart.Model != "alpha-smart-model" || !smart.SupportsImage || smart.Upstream != "alpha" {
		t.Fatalf("a scheme alias must keep supports_image, got %+v", smart)
	}
	// The scheme table is additive: the global one still resolves as it did.
	if got := rc.routeTarget("sonnet"); got.Model != "global-model" || got.Upstream != "alpha" {
		t.Fatalf("the global routing table must be unaffected, got %+v", got)
	}

	// And the alias must actually be servable end to end.
	resp := callHandleMessages(t, `{"model":"alpha-fast","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(resp, "alpha answer") {
		t.Fatalf("the scheme alias must reach its upstream, got %q", resp)
	}
	_, _, models, _, _ := alpha.snapshot()
	if len(models) != 1 || models[0] != "alpha-fast-model" {
		t.Fatalf("the scheme alias must send its own model name, got %v", models)
	}
}

// The global [routing] table wins a conflict with a scheme table: adding a scheme
// must never silently reroute traffic the operator already pointed somewhere.
func TestGlobalRoutingWinsOverASchemeTable(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	withProfiles(t, `
[proxy]
vlm_model = ""

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"

[upstreams.alpha.routing]
sonnet = { model = "scheme-sonnet" }

[upstreams.beta]
protocol = "anthropic"
url = "https://beta.example/api"

[upstreams.beta.routing]
sonnet = { model = "other-scheme-sonnet" }

[routing]
sonnet = "global-sonnet"
`)

	got := snapshotConfig().routeTarget("sonnet")
	if got.Model != "global-sonnet" {
		t.Fatalf("the global routing table must win, got %+v", got)
	}
}

// A scheme table may not name another profile: the table is scoped to the scheme
// that declares it, and an entry pointing elsewhere would make "this scheme's
// models" ambiguous. Such an entry is skipped, the same way a route naming an
// undefined profile is.
func TestSchemeRoutingCannotNameAnotherProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	withProfiles(t, `
[proxy]
vlm_model = ""

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"

[upstreams.alpha.routing]
stray = { model = "m", upstream = "beta" }

[upstreams.beta]
protocol = "anthropic"
url = "https://beta.example/api"
`)

	rc := snapshotConfig()
	if got := rc.routeTarget("stray"); got.Model != "" || got.Upstream != "" {
		t.Fatalf("a scheme route naming another profile must be skipped, got %+v", got)
	}
	// The scheme's own aliases still resolve to it.
	if _, ok := rc.routes["stray"]; ok {
		t.Fatalf("the skipped entry must not appear in the resolved table")
	}
}

// A scheme that declares vlm_model while speaking the openai protocol cannot
// serve its own describe calls (they need the anthropic framing). The load-time
// check reports it and the request path falls back to the anthropic profile
// instead of sending /v1/messages to an OpenAI endpoint.
func TestSchemeOnOpenAIProtocolFallsBackForDescribeCalls(t *testing.T) {
	withCleanStats(t)
	resetImageDescCacheForTests()
	t.Cleanup(resetImageDescCacheForTests)
	t.Setenv("SOPHNET_API_KEY", "")

	var openaiCalls int64
	// The describe call must never arrive here: this server only speaks chat
	// completions.
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&openaiCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","model":"oa-text","choices":[{"index":0,"message":{"role":"assistant","content":"openai answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(openaiUp.Close)

	var describeCalls int64
	var describeModel string
	anthropicUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(body, &m)
		model, _ := m["model"].(string)
		if strings.HasPrefix(model, "vision-of") {
			atomic.AddInt64(&describeCalls, 1)
			describeModel = model
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"described"}],"id":"v","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(anthropicUp.Close)

	withProfiles(t, `
[proxy]
vlm_model = "global-vision"

[upstream]
anthropic_url = "`+anthropicUp.URL+`"
openai_url = "`+openaiUp.URL+`"

[upstreams.oa]
protocol = "openai"
url = "`+openaiUp.URL+`"
vlm_model = "vision-of-oa"

[routing]
sonnet = { model = "oa-text", upstream = "oa" }
`)

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"hi"}]}]}`)

	if got := atomic.LoadInt64(&describeCalls); got != 1 {
		t.Fatalf("the describe call must fall back to the anthropic profile, got %d calls", got)
	}
	if describeModel != "vision-of-oa" {
		t.Fatalf("the scheme's vision model must still be used, got %q", describeModel)
	}
}

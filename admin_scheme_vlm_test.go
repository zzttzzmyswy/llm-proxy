package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// configTOMLForTest loads content as the live config and returns the file path it
// was written to, so a test can assert on what a save left behind.
func configTOMLForTest(t *testing.T, content string) string {
	t.Helper()
	return withProfiles(t, content)
}

// payloadFromViewWithProfiles is payloadFromView plus the profile list, which is
// what the page actually posts. payloadFromView deliberately omits it (an older
// page shape), so the scheme tests need their own builder.
func payloadFromViewWithProfiles(t *testing.T, view adminConfigView) adminConfigPayload {
	t.Helper()
	p := payloadFromView(t, view)
	for _, prof := range view.Upstreams {
		if prof.Builtin {
			// The page filters builtin rows out of the submit, so a save writes
			// only the declared profiles. Sending them would create
			// [upstreams.anthropic] tables that shadow the implicit profile.
			continue
		}
		p.Upstreams = append(p.Upstreams, adminUpstreamProfilePayload{
			Name:                 prof.Name,
			Protocol:             prof.Protocol,
			URL:                  prof.URL,
			KeyAction:            "keep",
			KeyEnv:               prof.KeyEnv,
			HeaderTimeoutSeconds: prof.HeaderTimeoutSeconds,
			BodyIdleSeconds:      prof.BodyIdleSeconds,
			MaxRetries:           prof.MaxRetries,
			VLMModel:             prof.VLMModel,
			VLMMaxTokens:         prof.VLMMaxTokens,
			VLMUpstream:          prof.VLMUpstream,
		})
	}
	return p
}

// setSchemeVLM edits one profile's VLM fields in a payload, as the page's form
// would when an operator types into that row.
func setSchemeVLM(p *adminConfigPayload, name, model, upstream string, maxTokens int) {
	for i := range p.Upstreams {
		if p.Upstreams[i].Name == name {
			p.Upstreams[i].VLMModel = model
			p.Upstreams[i].VLMUpstream = upstream
			p.Upstreams[i].VLMMaxTokens = maxTokens
		}
	}
}

// loadFromDiskInto parses a config file into c without publishing it, so a test
// can assert on exactly what the save wrote.
func loadFromDiskInto(t *testing.T, path string, into *Config) error {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return toml.Unmarshal(data, into)
}

// A scheme's own VLM fields survive the admin round trip: the page reads them,
// posts them back, and the rendered file parses to the same values. Without this
// a save would silently strip every per-scheme vision model.
func TestAdminRoundTripsSchemeVLMFields(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	path := configTOMLForTest(t, `
[proxy]
port = 8088
vlm_model = "global-vision"

[admin]
token = "s3cret"

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"
key = "sk-alpha"
vlm_model = "alpha-vision"
vlm_max_tokens = 1234

[upstreams.beta]
protocol = "anthropic"
url = "https://beta.example/api"
key = "sk-beta"
vlm_model = "beta-vision"
vlm_upstream = "gamma"

[upstreams.gamma]
protocol = "anthropic"
url = "https://gamma.example/api"
key = "sk-gamma"

[routing]
sonnet = { model = "alpha-text", upstream = "alpha" }
`)

	view := currentConfigView()
	byName := map[string]adminUpstreamProfileView{}
	for _, p := range view.Upstreams {
		byName[p.Name] = p
	}

	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatalf("alpha must be listed: %+v", view.Upstreams)
	}
	if alpha.VLMModel != "alpha-vision" || alpha.VLMMaxTokens != 1234 {
		t.Fatalf("alpha's own VLM must be reported as declared, got %+v", alpha)
	}
	if alpha.EffectiveVLMModel != "alpha-vision" {
		t.Fatalf("alpha's effective model must be its own, got %q", alpha.EffectiveVLMModel)
	}

	beta := byName["beta"]
	if beta.VLMModel != "beta-vision" || beta.VLMUpstream != "gamma" {
		t.Fatalf("beta's VLM selector must be reported, got %+v", beta)
	}

	// The builtin profiles inherit the global model, and the page says so.
	builtin := byName[profileNameAnthropic]
	if builtin.EffectiveVLMModel != "global-vision" {
		t.Fatalf("a builtin profile must report the inherited global model, got %q", builtin.EffectiveVLMModel)
	}
	if builtin.VLMModel != "" {
		t.Fatalf("a builtin profile must not claim a VLM of its own, got %q", builtin.VLMModel)
	}

	// Now post it back, as the page does, and check the file round trips.
	w := postConfig(t, payloadFromViewWithProfiles(t, view), "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("save must succeed: %d %s", w.Code, w.Body.String())
	}

	var reloaded Config
	if err := loadFromDiskInto(t, path, &reloaded); err != nil {
		t.Fatalf("the saved file must parse: %v", err)
	}
	normalizeUpstreamProfiles(&reloaded)
	if got := reloaded.Upstreams["alpha"]; got.VLMModel != "alpha-vision" || got.VLMMaxTokens != 1234 {
		t.Fatalf("alpha's VLM did not round trip: %+v", got)
	}
	if got := reloaded.Upstreams["beta"]; got.VLMUpstream != "gamma" {
		t.Fatalf("beta's vlm_upstream did not round trip: %+v", got)
	}
}

// An unset per-scheme VLM must stay unset across a save. Writing the inherited
// [proxy] value into the scheme would freeze the inheritance, so a later change to
// the global model would stop reaching the schemes that never declared one.
func TestAdminSaveDoesNotFreezeAnInheritedSchemeVLM(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	path := configTOMLForTest(t, `
[proxy]
port = 8088
vlm_model = "global-vision"

[admin]
token = "s3cret"

[upstreams.plain]
protocol = "anthropic"
url = "https://plain.example/api"
key = "sk-plain"

[routing]
sonnet = { model = "m", upstream = "plain" }
`)

	w := postConfig(t, payloadFromViewWithProfiles(t, currentConfigView()), "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("save must succeed: %d %s", w.Code, w.Body.String())
	}

	var reloaded Config
	if err := loadFromDiskInto(t, path, &reloaded); err != nil {
		t.Fatalf("the saved file must parse: %v", err)
	}
	plain := reloaded.Upstreams["plain"]
	if plain.VLMModel != "" || plain.VLMMaxTokens != 0 || plain.VLMUpstream != "" {
		t.Fatalf("a scheme that declared no VLM must not gain one from a save: %+v", plain)
	}
	if reloaded.Proxy.VLMModel != "global-vision" {
		t.Fatalf("the global model must be preserved, got %q", reloaded.Proxy.VLMModel)
	}
}

// A scheme's vlm_upstream has to name an anthropic-protocol profile. Accepting an
// openai one would save cleanly and then send /v1/messages to a chat-completions
// endpoint at describe time.
func TestAdminSaveRejectsSchemeVLMPointingAtOpenAIProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	configTOMLForTest(t, `
[proxy]
port = 8088
vlm_model = "global-vision"

[admin]
token = "s3cret"

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"
key = "sk-alpha"

[upstreams.chat]
protocol = "openai"
url = "https://chat.example/v1"
key = "sk-chat"

[routing]
sonnet = { model = "m", upstream = "alpha" }
`)

	p := payloadFromViewWithProfiles(t, currentConfigView())
	setSchemeVLM(&p, "alpha", "alpha-vision", "chat", 0)

	w := postConfig(t, p, "s3cret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a describe call aimed at an openai profile must be rejected, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vlm_upstream") {
		t.Fatalf("the error must name the offending field: %s", w.Body.String())
	}
}

// An unknown profile name in a scheme's vlm_upstream is rejected at save time,
// where the operator can still fix it.
func TestAdminSaveRejectsUnknownSchemeVLMUpstream(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	configTOMLForTest(t, `
[proxy]
port = 8088
vlm_model = "global-vision"

[admin]
token = "s3cret"

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"
key = "sk-alpha"

[routing]
sonnet = { model = "m", upstream = "alpha" }
`)

	p := payloadFromViewWithProfiles(t, currentConfigView())
	setSchemeVLM(&p, "alpha", "alpha-vision", "does-not-exist", 0)

	w := postConfig(t, p, "s3cret")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an undefined vlm_upstream must be rejected, got %d %s", w.Code, w.Body.String())
	}
}

// A scheme may name itself as its describe-call server; that is the normal
// "images stay on my own upstream" shape and must not be rejected as an
// unresolved reference.
func TestAdminSaveAcceptsSchemeServingItsOwnVLMCalls(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")

	configTOMLForTest(t, `
[proxy]
port = 8088
vlm_model = "global-vision"

[admin]
token = "s3cret"

[upstreams.alpha]
protocol = "anthropic"
url = "https://alpha.example/api"
key = "sk-alpha"

[routing]
sonnet = { model = "m", upstream = "alpha" }
`)

	p := payloadFromViewWithProfiles(t, currentConfigView())
	setSchemeVLM(&p, "alpha", "alpha-vision", "alpha", 500)

	w := postConfig(t, p, "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("a scheme may serve its own describe calls, got %d %s", w.Code, w.Body.String())
	}
}

// The scheme payload fields have to be on the wire for any of the above to work.
func TestAdminSchemeVLMFieldsAreInTheJSONContract(t *testing.T) {
	p := adminUpstreamProfilePayload{
		Name: "alpha", Protocol: "anthropic", URL: "https://a/b",
		VLMModel: "alpha-vision", VLMMaxTokens: 999, VLMUpstream: "gamma",
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"vlm_model":"alpha-vision"`, `"vlm_max_tokens":999`, `"vlm_upstream":"gamma"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("the payload must carry %s: %s", want, body)
		}
	}

	view := adminUpstreamProfileView{Name: "alpha", VLMModel: "v", EffectiveVLMModel: "v"}
	vb, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vb), `"effective_vlm_model":"v"`) {
		t.Fatalf("the view must report the effective model: %s", vb)
	}
}

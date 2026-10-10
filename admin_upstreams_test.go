package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// adminUpstreamsTestConfig is a two-profile config the profile-editing tests
// start from: one anthropic and one openai profile, each with its own key, plus a
// route that names one of them.
const adminUpstreamsTestConfig = `[proxy]
port = 8088
vlm_model = "MiniMax-M3"
vlm_upstream = "deepseek"
chat_upstream = "local"

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"
default_upstream = "deepseek"

[upstreams.deepseek]
protocol = "anthropic"
url = "https://api.deepseek.com/anthropic"
key = "sk-deepseek"
header_timeout_seconds = 30

[upstreams.local]
protocol = "openai"
url = "http://127.0.0.1:9000/v1/chat/completions"
key = "sk-local"

[keys]
sophnet = "sk-original"

[admin]
token = "s3cret"

[routing]
sonnet = { model = "deepseek-chat", upstream = "deepseek" }
`

// profilePayloadFromView turns the view's editable profiles into the payload the
// page submits, keeping each stored key with a "keep" action — which is exactly
// what a page load followed by an untouched save does. The builtin rows are
// skipped, since they are not written as [upstreams.*] tables.
func profilePayloadFromView(view adminConfigView) []adminUpstreamProfilePayload {
	out := make([]adminUpstreamProfilePayload, 0, len(view.Upstreams))
	for _, p := range view.Upstreams {
		if p.Builtin {
			continue
		}
		out = append(out, adminUpstreamProfilePayload{
			Name:                 p.Name,
			Protocol:             p.Protocol,
			URL:                  p.URL,
			KeyAction:            "keep",
			KeyEnv:               p.KeyEnv,
			HeaderTimeoutSeconds: p.HeaderTimeoutSeconds,
			BodyIdleSeconds:      p.BodyIdleSeconds,
			MaxRetries:           p.MaxRetries,
		})
	}
	return out
}

// loadConfigView reads GET /admin/api/config, the request the page boots from.
func loadConfigView(t *testing.T) (adminConfigView, string) {
	t.Helper()
	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("config view: %d %s", w.Code, w.Body.String())
	}
	var view adminConfigView
	decodeBody(t, w, &view)
	return view, w.Body.String()
}

// fullPayloadFromView is the payload an untouched page submits: every field the
// form models, including the profile list.
func fullPayloadFromView(t *testing.T, view adminConfigView) adminConfigPayload {
	t.Helper()
	p := payloadFromView(t, view)
	p.Proxy = view.Proxy
	p.Upstreams = profilePayloadFromView(view)
	return p
}

// readDiskConfig parses the saved file, so a test asserts on what reached the disk
// rather than on what the process happens to hold.
func readDiskConfig(t *testing.T, path string) Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		t.Fatalf("saved config must be valid TOML: %v\n%s", err, data)
	}
	return c
}

// ---- 1. the view never hands the page a key ----

// Every profile key must be redacted the way [keys].sophnet is: the page learns
// whether a key exists and where it comes from, never its value.
func TestAdminViewRedactsEveryProfileKey(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `
[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[upstreams.deepseek]
protocol = "anthropic"
url = "https://api.deepseek.com/anthropic"
key = "sk-deepseek-secret"

[upstreams.local]
protocol = "openai"
url = "http://127.0.0.1:9000/v1/chat/completions"
key_env = "LOCAL_KEY_ENV"

[upstreams.plain]
protocol = "anthropic"
url = "https://plain.example/api"
key = ""

[admin]
token = "s3cret"
`)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")
	t.Setenv("LOCAL_KEY_ENV", "sk-from-env-secret")

	view, body := loadConfigView(t)

	for _, secret := range []string{"sk-deepseek-secret", "sk-from-env-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("a profile key leaked into the config view: %s", body)
		}
	}

	byName := map[string]adminUpstreamProfileView{}
	for _, p := range view.Upstreams {
		byName[p.Name] = p
	}
	if p := byName["deepseek"]; !p.KeySet || p.KeyFromEnv || p.Builtin {
		t.Fatalf("a plaintext-keyed profile must report key_set without key_from_env: %+v", p)
	}
	// key_env itself is not a secret: the operator has to see which variable is
	// in play, and the page echoes it back on the next save.
	if p := byName["local"]; !p.KeyFromEnv || p.KeyEnv != "LOCAL_KEY_ENV" {
		t.Fatalf("a key_env-backed profile must report its variable: %+v", p)
	}
	if p := byName["plain"]; p.KeySet || p.KeyFromEnv {
		t.Fatalf("a keyless profile must report neither key state: %+v", p)
	}
}

// The two profiles synthesised from [upstream] are listed, flagged builtin, and
// carry the state the request path actually resolves.
func TestAdminViewListsBuiltinProfiles(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withTempConfig(t, testConfigTOML)

	view, _ := loadConfigView(t)
	if len(view.Upstreams) != 2 {
		t.Fatalf("a config with no [upstreams.*] still has two builtin profiles, got %+v", view.Upstreams)
	}
	for _, p := range view.Upstreams {
		if !p.Builtin {
			t.Fatalf("neither entry of a profileless config may be editable: %+v", p)
		}
	}
	byName := map[string]adminUpstreamProfileView{}
	for _, p := range view.Upstreams {
		byName[p.Name] = p
	}
	// Key state comes from the legacy [keys].sophnet-over-SOPHNET_API_KEY pair.
	if p := byName["anthropic"]; p.Protocol != protocolAnthropic || !p.KeySet ||
		p.URL != "https://anthropic.example/api" {
		t.Fatalf("the implicit anthropic profile must show its resolved state: %+v", p)
	}
	if p := byName["openai"]; p.Protocol != protocolOpenAI || !p.KeySet {
		t.Fatalf("the implicit openai profile must show its resolved state: %+v", p)
	}
}

// A declared [upstreams.anthropic] replaces the synthesised profile: the list must
// show one entry, marked editable, or a save would write the builtin back twice.
func TestAdminViewDeclaredProfileReplacesBuiltin(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	withProfiles(t, `
[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[upstreams.anthropic]
protocol = "anthropic"
url = "https://moved.example/api"
key = "sk-moved"

[admin]
token = "s3cret"
`)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	var anthropic []adminUpstreamProfileView
	for _, p := range view.Upstreams {
		if p.Name == profileNameAnthropic {
			anthropic = append(anthropic, p)
		}
	}
	if len(anthropic) != 1 {
		t.Fatalf("a declared builtin name must appear once, got %d entries: %+v", len(anthropic), view.Upstreams)
	}
	if anthropic[0].Builtin {
		t.Fatalf("a declared profile is editable, not builtin: %+v", anthropic[0])
	}
	if anthropic[0].URL != "https://moved.example/api" {
		t.Fatalf("the declared table must win over [upstream]: %+v", anthropic[0])
	}
}

// ---- 2. add / edit / delete round trips ----

// Adding a profile writes its table, and a route can then name it.
func TestAdminSaveAddsProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	p.Upstreams = append(p.Upstreams, adminUpstreamProfilePayload{
		Name:                 "newglm",
		Protocol:             "openai",
		URL:                  "http://127.0.0.1:9100/v1",
		KeyAction:            "set",
		Key:                  "sk-new",
		HeaderTimeoutSeconds: 45,
		MaxRetries:           4,
	})
	p.Routing = append(p.Routing, adminRoutingEntry{
		Alias: "glm", Model: "glm-5.3-flash", Upstream: "newglm",
	})

	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}

	onDisk := readDiskConfig(t, path)
	added, ok := onDisk.Upstreams["newglm"]
	if !ok {
		t.Fatalf("the new profile must reach the file: %+v", onDisk.Upstreams)
	}
	if added.Protocol != protocolOpenAI || added.URL != "http://127.0.0.1:9100/v1" ||
		added.Key != "sk-new" || added.HeaderTimeoutSeconds != 45 || added.MaxRetries != 4 {
		t.Fatalf("the new profile must be written field for field: %+v", added)
	}

	// And it is live: the route resolves to it, through its own protocol and key.
	rc := snapshotConfig()
	if rc.routes["glm"].Upstream != "newglm" {
		t.Fatalf("the route must name the new profile, got %+v", rc.routes["glm"])
	}
	up, ok := rc.profile("newglm")
	if !ok {
		t.Fatal("the new profile must be resolvable after the save")
	}
	if up.Protocol != protocolOpenAI || up.Key != "sk-new" || up.MaxRetries != 4 {
		t.Fatalf("the reloaded profile must carry its fields: %+v", up)
	}
}

// Editing a profile replaces its fields on disk and in the running config, and a
// route following it picks the change up. The protocol is left alone here because
// a [proxy] selector still points at this profile; changing the protocol out from
// under a selector is its own rejection case below.
func TestAdminSaveEditsProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	for i := range p.Upstreams {
		if p.Upstreams[i].Name != "deepseek" {
			continue
		}
		p.Upstreams[i].URL = "https://api.deepseek.com/v1"
		p.Upstreams[i].HeaderTimeoutSeconds = 55
		p.Upstreams[i].KeyAction = "set"
		p.Upstreams[i].Key = "sk-rotated"
	}

	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}

	onDisk := readDiskConfig(t, path)
	edited := onDisk.Upstreams["deepseek"]
	if edited.URL != "https://api.deepseek.com/v1" || edited.Key != "sk-rotated" ||
		edited.HeaderTimeoutSeconds != 55 || edited.Protocol != protocolAnthropic {
		t.Fatalf("the edit must reach the file: %+v", edited)
	}

	rc := snapshotConfig()
	up, ok := rc.profile("deepseek")
	if !ok || up.URL != "https://api.deepseek.com/v1" || up.Key != "sk-rotated" ||
		up.HeaderTimeout.Seconds() != 55 {
		t.Fatalf("the edit must be live: %+v (ok=%v)", up, ok)
	}
}

// Changing a profile's protocol out from under a selector that names it is
// rejected: the save would otherwise succeed and then be silently folded back to
// the builtin profile at load, leaving the operator's choice not in force.
func TestAdminSaveRejectsFlippingASelectedProfilesProtocol(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")
	before, _ := os.ReadFile(path)

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	for i := range p.Upstreams {
		if p.Upstreams[i].Name == "deepseek" {
			p.Upstreams[i].Protocol = "openai"
		}
	}

	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("flipping a selected profile's protocol must be rejected, got %d %s", res.Code, res.Body.String())
	}
	var out struct {
		Error string `json:"error"`
	}
	decodeBody(t, res, &out)
	if !strings.Contains(out.Error, "vlm_upstream") {
		t.Fatalf("the rejection must name the selector in the way, got %q", out.Error)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a rejected save must leave the config file untouched")
	}
}

// Deleting a profile removes its table once nothing references it.
func TestAdminSaveDeletesUnreferencedProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	// Drop every reference to "local" first, then the profile itself.
	p.Proxy.ChatUpstream = ""
	kept := p.Upstreams[:0]
	for _, sp := range p.Upstreams {
		if sp.Name != "local" {
			kept = append(kept, sp)
		}
	}
	p.Upstreams = kept

	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}

	onDisk := readDiskConfig(t, path)
	if _, ok := onDisk.Upstreams["local"]; ok {
		t.Fatalf("the deleted profile must be gone from the file: %+v", onDisk.Upstreams)
	}
	if _, ok := onDisk.Upstreams["deepseek"]; !ok {
		t.Fatalf("the untouched profile must survive: %+v", onDisk.Upstreams)
	}
	if rc := snapshotConfig(); rc.cfg.Proxy.ChatUpstream != "" {
		t.Fatalf("the cleared selector must be live, got %q", rc.cfg.Proxy.ChatUpstream)
	}
}

// The three key actions: keep carries the stored value over, set replaces it and
// clear empties it.
func TestAdminProfileKeyActions(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	// The page never receives the key, so an untouched save must keep it.
	if res := postConfig(t, fullPayloadFromView(t, view), "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("keep failed: %s", res.Body.String())
	}
	if got := readDiskConfig(t, path).Upstreams["deepseek"].Key; got != "sk-deepseek" {
		t.Fatalf("keep must preserve the stored key, got %q", got)
	}

	setKey := func(action, key string) {
		t.Helper()
		view, _ := loadConfigView(t)
		p := fullPayloadFromView(t, view)
		for i := range p.Upstreams {
			if p.Upstreams[i].Name == "deepseek" {
				p.Upstreams[i].KeyAction = action
				p.Upstreams[i].Key = key
			}
		}
		if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
			t.Fatalf("%s failed: %s", action, res.Body.String())
		}
	}

	setKey("set", "sk-replaced")
	if got := readDiskConfig(t, path).Upstreams["deepseek"].Key; got != "sk-replaced" {
		t.Fatalf("set must store the new key, got %q", got)
	}
	setKey("clear", "")
	if got := readDiskConfig(t, path).Upstreams["deepseek"].Key; got != "" {
		t.Fatalf("clear must empty the key, got %q", got)
	}
}

// A profile added by the page has no stored key to keep, so a blank key field must
// be rejected rather than silently written as a keyless profile.
func TestAdminSaveRejectsKeepOnANewProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")
	before, _ := os.ReadFile(path)

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	p.Upstreams = append(p.Upstreams, adminUpstreamProfilePayload{
		Name: "brand-new", Protocol: "anthropic", URL: "https://new.example/api",
		KeyAction: "keep",
	})

	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("keep on a new profile must be rejected, got %d %s", res.Code, res.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a rejected save must leave the config file untouched")
	}
}

// A profile can be turned into a [upstreams.anthropic] override, and deleting that
// override again brings the synthesised profile back.
func TestAdminSaveOverridesAndRestoresBuiltinProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	p.Upstreams = append(p.Upstreams, adminUpstreamProfilePayload{
		Name: "anthropic", Protocol: "anthropic", URL: "https://moved.example/api",
		KeyAction: "set", Key: "sk-moved",
	})
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("override save failed: %d %s", res.Code, res.Body.String())
	}
	if got := readDiskConfig(t, path).Upstreams["anthropic"].URL; got != "https://moved.example/api" {
		t.Fatalf("the override must reach the file, got %q", got)
	}
	if up, ok := snapshotConfig().profile("anthropic"); !ok || up.URL != "https://moved.example/api" {
		t.Fatalf("the override must be live: %+v (ok=%v)", up, ok)
	}

	// Removing it falls back to [upstream].anthropic_url again.
	view, _ = loadConfigView(t)
	p = fullPayloadFromView(t, view)
	kept := p.Upstreams[:0]
	for _, sp := range p.Upstreams {
		if sp.Name != "anthropic" {
			kept = append(kept, sp)
		}
	}
	p.Upstreams = kept
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("restore save failed: %d %s", res.Code, res.Body.String())
	}
	if _, ok := readDiskConfig(t, path).Upstreams["anthropic"]; ok {
		t.Fatal("dropping the override must remove the table")
	}
	if up, ok := snapshotConfig().profile("anthropic"); !ok || up.URL != "https://anthropic.example/api" {
		t.Fatalf("the synthesised profile must come back: %+v (ok=%v)", up, ok)
	}
}

// ---- 3. the [proxy] selectors ----

// The two [proxy] selectors round trip, and each has to speak the protocol its
// caller uses: the VLM describe pass is anthropic, the chat passthrough openai.
func TestAdminSaveRoundTripsProxySelectors(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	if view.Proxy.VLMUpstream != "deepseek" || view.Proxy.ChatUpstream != "local" {
		t.Fatalf("the view must report the selectors: %+v", view.Proxy)
	}

	p := fullPayloadFromView(t, view)
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}
	onDisk := readDiskConfig(t, path)
	if onDisk.Proxy.VLMUpstream != "deepseek" || onDisk.Proxy.ChatUpstream != "local" {
		t.Fatalf("the selectors must round trip: %+v", onDisk.Proxy)
	}

	// Clearing them falls back to the builtin profiles.
	p = fullPayloadFromView(t, view)
	p.Proxy.VLMUpstream = ""
	p.Proxy.ChatUpstream = ""
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("clearing the selectors failed: %d %s", res.Code, res.Body.String())
	}
	if got := readDiskConfig(t, path); got.Proxy.VLMUpstream != "" || got.Proxy.ChatUpstream != "" {
		t.Fatalf("a cleared selector must be omitted: %+v", got.Proxy)
	}
}

// ---- 4. one rejection case per validation rule ----

// Each rule the page has to enforce is checked with the config file left
// untouched, so a rejected save cannot half-apply.
func TestAdminSaveRejectsBadProfilesWithoutWriting(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")
	before, _ := os.ReadFile(path)

	good := adminUpstreamProfilePayload{
		Name: "extra", Protocol: "anthropic", URL: "https://extra.example/api",
		KeyAction: "set", Key: "sk-extra",
	}

	cases := []struct {
		name   string
		mutate func(*adminConfigPayload)
	}{
		{"empty profile name", func(p *adminConfigPayload) {
			bad := good
			bad.Name = "  "
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"profile name with illegal characters", func(p *adminConfigPayload) {
			bad := good
			bad.Name = "bad name!"
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"profile name too long", func(p *adminConfigPayload) {
			bad := good
			bad.Name = strings.Repeat("a", maxProfileNameLen+1)
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"duplicate profile name", func(p *adminConfigPayload) {
			dup := good
			dup.Name = "DEEPSEEK" // names are case-insensitive
			p.Upstreams = append(p.Upstreams, dup)
		}},
		{"unknown protocol", func(p *adminConfigPayload) {
			bad := good
			bad.Protocol = "gemini"
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"empty profile url", func(p *adminConfigPayload) {
			bad := good
			bad.URL = ""
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"profile url without a scheme", func(p *adminConfigPayload) {
			bad := good
			bad.URL = "api.example.com/v1"
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"profile url with a non-http scheme", func(p *adminConfigPayload) {
			bad := good
			bad.URL = "ftp://api.example.com/v1"
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"negative profile timeout", func(p *adminConfigPayload) {
			bad := good
			bad.HeaderTimeoutSeconds = -1
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"negative profile retries", func(p *adminConfigPayload) {
			bad := good
			bad.MaxRetries = -3
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"set without a profile key", func(p *adminConfigPayload) {
			bad := good
			bad.Key = ""
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"unknown profile key action", func(p *adminConfigPayload) {
			bad := good
			bad.KeyAction = "wipe"
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"control character in a profile url", func(p *adminConfigPayload) {
			bad := good
			bad.URL = "https://extra.example/api\x00"
			p.Upstreams = append(p.Upstreams, bad)
		}},
		{"route naming a deleted profile", func(p *adminConfigPayload) {
			kept := p.Upstreams[:0]
			for _, sp := range p.Upstreams {
				if sp.Name != "deepseek" {
					kept = append(kept, sp)
				}
			}
			p.Upstreams = kept
		}},
		{"route naming an undefined profile", func(p *adminConfigPayload) {
			p.Routing[0].Upstream = "ghost"
		}},
		{"default_upstream naming an undefined profile", func(p *adminConfigPayload) {
			p.Upstream.DefaultUpstream = "ghost"
		}},
		{"vlm_upstream naming an undefined profile", func(p *adminConfigPayload) {
			p.Proxy.VLMUpstream = "ghost"
		}},
		{"chat_upstream naming an undefined profile", func(p *adminConfigPayload) {
			p.Proxy.ChatUpstream = "ghost"
		}},
		// The VLM describe pass speaks anthropic and the chat passthrough openai,
		// so a selector at the other protocol would be folded back at load.
		{"vlm_upstream pointing at an openai profile", func(p *adminConfigPayload) {
			p.Proxy.VLMUpstream = "local"
		}},
		{"chat_upstream pointing at an anthropic profile", func(p *adminConfigPayload) {
			p.Proxy.ChatUpstream = "deepseek"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, _ := loadConfigView(t)
			p := fullPayloadFromView(t, view)
			tc.mutate(&p)
			res := postConfig(t, p, "s3cret")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("must be rejected with 400, got %d %s", res.Code, res.Body.String())
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("a rejected save must leave the config file untouched")
			}
		})
	}
}

// A profile that is still referenced cannot be deleted, and the rejection has to
// name the referrer so the operator knows what to change first.
func TestAdminSaveRefusesToDeleteAReferencedProfile(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")
	before, _ := os.ReadFile(path)

	// Each referrer kind is exercised on its own, so the message has to name it.
	cases := []struct {
		name      string
		mutate    func(*adminConfigPayload)
		wantInMsg string
	}{
		{"a route", func(p *adminConfigPayload) {}, "sonnet"},
		{"default_upstream", func(p *adminConfigPayload) {
			p.Routing[0].Upstream = "local"
		}, "default_upstream"},
		{"vlm_upstream", func(p *adminConfigPayload) {
			p.Routing[0].Upstream = "local"
		}, "vlm_upstream"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, _ := loadConfigView(t)
			p := fullPayloadFromView(t, view)
			tc.mutate(&p)
			// Delete "deepseek" while it is still referenced.
			kept := p.Upstreams[:0]
			for _, sp := range p.Upstreams {
				if sp.Name != "deepseek" {
					kept = append(kept, sp)
				}
			}
			p.Upstreams = kept

			res := postConfig(t, p, "s3cret")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("deleting a referenced profile must be rejected, got %d %s", res.Code, res.Body.String())
			}
			var out struct {
				Error string `json:"error"`
			}
			decodeBody(t, res, &out)
			if !strings.Contains(out.Error, tc.wantInMsg) {
				t.Fatalf("the rejection must name %q, got %q", tc.wantInMsg, out.Error)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("a rejected deletion must leave the config file untouched")
			}
		})
	}
}

// A save that omits the profile list entirely is an older page, not a request to
// delete every profile: the running profiles are carried over.
func TestAdminSaveWithoutProfileListKeepsThem(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	p := payloadFromView(t, view) // no Upstreams field at all
	p.Proxy.VLMUpstream = view.Proxy.VLMUpstream
	p.Proxy.ChatUpstream = view.Proxy.ChatUpstream

	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}
	onDisk := readDiskConfig(t, path)
	if _, ok := onDisk.Upstreams["deepseek"]; !ok {
		t.Fatalf("a payload without a profile list must not delete profiles: %+v", onDisk.Upstreams)
	}
	if _, ok := onDisk.Upstreams["local"]; !ok {
		t.Fatalf("every running profile must survive: %+v", onDisk.Upstreams)
	}
}

// An empty (non-nil) list is a deliberate deletion of every row the page showed,
// so it must be honoured — that is how the page expresses "remove them all".
func TestAdminSaveWithEmptyProfileListDeletesThem(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	p.Upstreams = []adminUpstreamProfilePayload{}
	p.Proxy.VLMUpstream = ""
	p.Proxy.ChatUpstream = ""
	p.Upstream.DefaultUpstream = ""
	p.Routing[0].Upstream = ""

	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}
	if got := readDiskConfig(t, path).Upstreams; len(got) != 0 {
		t.Fatalf("an empty profile list must delete every table, got %+v", got)
	}
	// The builtin profiles remain, so the routes still resolve.
	if up, ok := snapshotConfig().profile(profileNameAnthropic); !ok || up.Protocol != protocolAnthropic {
		t.Fatalf("the builtin profiles must survive deleting every declared one: %+v (ok=%v)", up, ok)
	}
}

// ---- 5. the profile list is capped ----

func TestAdminSaveRejectsTooManyProfiles(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	path := withProfiles(t, adminUpstreamsTestConfig)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "s3cret")
	before, _ := os.ReadFile(path)

	view, _ := loadConfigView(t)
	p := fullPayloadFromView(t, view)
	for i := 0; i <= maxProfileCount; i++ {
		p.Upstreams = append(p.Upstreams, adminUpstreamProfilePayload{
			Name:     "extra-" + strconv.Itoa(i),
			Protocol: "anthropic", URL: "https://x.example/api",
			KeyAction: "set", Key: "sk",
		})
	}

	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("an oversized profile list must be rejected, got %d %s", res.Code, res.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a rejected save must leave the config file untouched")
	}
}

// ---- 6. the generated TOML round trips ----

// A profile set written by the page must parse back into the same profiles, keys
// and per-profile overrides — including a name that needs quoting.
func TestRenderConfigTOMLWritesProfiles(t *testing.T) {
	p := adminConfigPayload{
		Proxy: adminProxyView{
			Port: 8088, VLMModel: "MiniMax-M3", VLMMaxTokens: 8000,
			VLMUpstream: "vlm-prof", ChatUpstream: "chat-prof",
		},
		Upstream: adminUpstreamView{
			AnthropicURL:    "https://anthropic.example/api",
			OpenAIURL:       "https://openai.example/v1",
			DefaultUpstream: "vlm-prof",
		},
		Routing: []adminRoutingEntry{
			{Alias: "sonnet", Model: "deepseek-chat", Upstream: "vlm-prof"},
		},
	}

	profiles := map[string]UpstreamProfile{
		"vlm-prof": {
			Protocol: protocolAnthropic, URL: "https://vlm.example/api",
			Key: "sk-vlm", KeyEnv: "VLM_KEY", HeaderTimeoutSeconds: 30,
		},
		"chat-prof": {
			Protocol: protocolOpenAI, URL: "http://127.0.0.1:9000/v1",
			Key: "sk-chat", BodyIdleSeconds: 15, MaxRetries: 5,
		},
	}

	rendered := renderConfigTOML(p, "tok", "sk-legacy", profiles)

	var got Config
	if err := toml.Unmarshal([]byte(rendered), &got); err != nil {
		t.Fatalf("rendered config must parse: %v\n%s", err, rendered)
	}
	if got.Proxy.VLMUpstream != "vlm-prof" || got.Proxy.ChatUpstream != "chat-prof" {
		t.Fatalf("the selectors must be written: %+v", got.Proxy)
	}
	if got.Keys.Sophnet != "sk-legacy" || got.Admin.Token != "tok" {
		t.Fatalf("secrets must be carried over: %+v", got)
	}

	normalizeUpstreamProfiles(&got)
	for name, want := range profiles {
		both, ok := got.Upstreams[name]
		if !ok {
			t.Fatalf("profile %q is missing from the round trip: %+v", name, got.Upstreams)
		}
		if both.Protocol != want.Protocol || both.URL != want.URL || both.Key != want.Key ||
			both.KeyEnv != want.KeyEnv || both.HeaderTimeoutSeconds != want.HeaderTimeoutSeconds ||
			both.BodyIdleSeconds != want.BodyIdleSeconds || both.MaxRetries != want.MaxRetries {
			t.Fatalf("profile %q did not round trip: got %+v, want %+v", name, both, want)
		}
	}
	// The route, the default gateway and the selectors all resolve to a profile.
	routes := buildRouteTargets(got.Routing)
	applyDefaultUpstream(&got, routes)
	dropUnknownProfileRoutes(&got, routes)
	if up, ok := got.profileByName(routes["sonnet"].Upstream); !ok || up.Key != "sk-vlm" {
		t.Fatalf("the round-tripped route must resolve to its profile: %+v (ok=%v)", up, ok)
	}
}

// A profile JSON round trip is what the page's save actually posts, so the payload
// shape has to survive encoding.
func TestAdminProfilePayloadJSONRoundTrip(t *testing.T) {
	p := adminConfigPayload{
		Upstreams: []adminUpstreamProfilePayload{{
			Name: "deepseek", Protocol: "anthropic", URL: "https://api.deepseek.com/anthropic",
			KeyAction: "keep", KeyEnv: "DEEPSEEK_API_KEY", HeaderTimeoutSeconds: 30,
		}},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back adminConfigPayload
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Upstreams) != 1 || back.Upstreams[0] != p.Upstreams[0] {
		t.Fatalf("the profile payload must survive JSON: %+v", back.Upstreams)
	}
	if !strings.Contains(string(body), `"key_action":"keep"`) {
		t.Fatalf("the key action must be on the wire: %s", body)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

)

// testMainTOML is the main configuration file the admin tests load: process
// settings plus which scheme is active. Everything about reaching a model lives in
// testSchemeTOML below.
const testMainTOML = `[proxy]
port = 8088
scheme = "main"

[admin]
token = "s3cret"
`

// testSchemeTOML is one complete scheme: an anthropic text upstream, a VLM on the
// same gateway, and a model table.
const testSchemeTOML = `protocol = "anthropic"
url = "https://anthropic.example/api"
key = "sk-original"

[vlm]
model = "MiniMax-M3"
max_tokens = 8000

[chat]
protocol = "openai"
url = "https://openai.example/v1"
key = "sk-chat"

[routing]
sonnet = { model = "DeepSeek-Flash", supports_image = true }
opus = "GLM-5.2"
"weird.alias" = "some-model"
`

// withTempConfig writes a scheme tree, points the proxy at it, loads it, and
// restores the previous live config when the test ends.
func withTempConfig(t *testing.T, mainTOML, schemeTOML string) (configPath, schemePath string) {
	t.Helper()
	configPath, schemePath = writeSchemeTree(t, mainTOML, "main", schemeTOML)
	t.Setenv("SOPHNET_API_KEY", "")
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "")

	prevCfg, prevScheme := currentConfig(), currentScheme()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg, scheme = prevCfg, prevScheme
		cfgMu.Unlock()
	})

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return configPath, schemePath
}

// withDefaultConfig is the common setup: the test tree above.
func withDefaultConfig(t *testing.T) (configPath, schemePath string) {
	t.Helper()
	return withTempConfig(t, testMainTOML, testSchemeTOML)
}

func adminCall(t *testing.T, handler http.HandlerFunc, method, target, body, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if password != "" {
		req.SetBasicAuth("admin", password)
	}
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, into interface{}) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

// An unset admin token disables the whole subtree; a 404 keeps the endpoint's
// existence unadvertised on a network-reachable port.
func TestAdminDisabledWithoutToken(t *testing.T) {
	withTempConfig(t, strings.Replace(testMainTOML, `token = "s3cret"`, `token = ""`, 1), testSchemeTOML)

	for _, h := range []http.HandlerFunc{handleAdminPage, handleAdminConfig, handleAdminStats} {
		w := adminCall(t, requireAdmin(h), http.MethodGet, "/admin", "", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("disabled admin must return 404, got %d", w.Code)
		}
	}
}

func TestAdminRejectsBadCredentials(t *testing.T) {
	withDefaultConfig(t)

	w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing credentials must return 401, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("401 must carry a Basic challenge, got %q", w.Header().Get("WWW-Authenticate"))
	}

	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password must return 401, got %d", w.Code)
	}
	w = adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("correct password must return 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "llm-proxy 管理页面") {
		t.Fatal("admin page must serve the embedded UI")
	}
}

// The env var takes precedence so the password does not have to be stored in the
// config file.
func TestAdminTokenFromEnvOverridesConfig(t *testing.T) {
	withDefaultConfig(t)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "env-token")

	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "s3cret"); w.Code != http.StatusUnauthorized {
		t.Fatalf("the config token must be ignored once the env var is set, got %d", w.Code)
	}
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "env-token"); w.Code != http.StatusOK {
		t.Fatalf("the env token must be accepted, got %d", w.Code)
	}
}

// The view reports the active scheme and every upstream's credential state without
// ever returning a key.
func TestAdminConfigViewRedactsKey(t *testing.T) {
	withDefaultConfig(t)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	for _, secret := range []string{"sk-original", "sk-chat"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("the upstream key %q must never be returned to the page", secret)
		}
	}

	var view adminConfigView
	decodeBody(t, w, &view)
	if view.Proxy.Scheme != "main" {
		t.Fatalf("the active scheme must be reported, got %q", view.Proxy.Scheme)
	}
	if len(view.Proxy.SchemeList) != 1 || view.Proxy.SchemeList[0] != "main" {
		t.Fatalf("the scheme list must come from the schemes directory, got %v", view.Proxy.SchemeList)
	}
	if !view.Upstream.KeySet || view.Upstream.KeyFromEnv {
		t.Fatalf("text upstream key state: %+v", view.Upstream)
	}
	if view.Upstream.Protocol != protocolAnthropic || view.Upstream.URL != "https://anthropic.example/api" {
		t.Fatalf("text upstream: %+v", view.Upstream)
	}
	if view.Upstream.HeaderTimeoutSeconds != 120 || view.Upstream.BodyIdleSeconds != 90 || view.Upstream.MaxRetries != 2 {
		t.Fatalf("documented defaults must be applied to the view: %+v", view.Upstream)
	}
	if !view.VLM.Active || view.VLM.Model != "MiniMax-M3" || view.VLM.MaxTokens != 8000 {
		t.Fatalf("the [vlm] table must be reported: %+v", view.VLM)
	}
	if !view.Chat.Available || view.Chat.Upstream.Protocol != protocolOpenAI {
		t.Fatalf("the [chat] table must be reported: %+v", view.Chat)
	}

	// The editable table carries the aliases the file declares, exactly.
	byAlias := map[string]adminRoutingEntry{}
	for _, e := range view.Routing {
		byAlias[e.Alias] = e
	}
	if len(byAlias) != 3 {
		t.Fatalf("the view must report the declared aliases only, got %+v", byAlias)
	}
	if e := byAlias["sonnet"]; !e.SupportsImage || e.Model != "DeepSeek-Flash" {
		t.Fatalf("sonnet route: %+v", e)
	}
	if _, ok := byAlias["weird.alias"]; !ok {
		t.Fatalf("an alias needing a quoted TOML key must round-trip, got %+v", byAlias)
	}
	// haiku is derived from sonnet, so it is effective but not declared.
	if e, ok := view.EffectiveRouting["haiku"]; !ok || e.Declared || e.Model != "DeepSeek-Flash" {
		t.Fatalf("the derived haiku fallback must be reported as effective and undeclared, got %+v", e)
	}
	if e, ok := view.EffectiveRouting["sonnet"]; !ok || !e.Declared {
		t.Fatalf("a declared route must be reported as declared, got %+v", e)
	}
}

// payloadFromView builds the save payload a round trip of the page would send: the
// view's own values back, with every key action left at "keep".
func payloadFromView(view adminConfigView) adminConfigPayload {
	return adminConfigPayload{
		Proxy:    view.Proxy,
		Upstream: upstreamPayloadFromView(view.Upstream, "keep", ""),
		VLM: adminVLMPayload{
			Active:    view.VLM.Active,
			Model:     view.VLM.Model,
			MaxTokens: view.VLM.MaxTokens,
			Inherit:   view.VLM.Inherit,
			Upstream:  upstreamPayloadFromView(view.VLM.Upstream, "keep", ""),
		},
		Chat: adminChatPayload{
			Available: view.Chat.Available,
			Inherit:   view.Chat.Inherit,
			Upstream:  upstreamPayloadFromView(view.Chat.Upstream, "keep", ""),
		},
		Admin:   adminAuthPayload{Action: "keep"},
		Routing: view.Routing,
	}
}

func upstreamPayloadFromView(v adminUpstreamView, keyAction, key string) adminUpstreamPayload {
	return adminUpstreamPayload{
		Protocol:             v.Protocol,
		URL:                  v.URL,
		KeyAction:            keyAction,
		Key:                  key,
		KeyEnv:               v.KeyEnv,
		HeaderTimeoutSeconds: v.HeaderTimeoutSeconds,
		BodyIdleSeconds:      v.BodyIdleSeconds,
		MaxRetries:           v.MaxRetries,
	}
}

func postConfig(t *testing.T, p adminConfigPayload, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return adminCall(t, handleAdminConfig, http.MethodPost, "/admin/api/config", string(body), password)
}

func currentView(t *testing.T) adminConfigView {
	t.Helper()
	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var view adminConfigView
	decodeBody(t, w, &view)
	return view
}

// A save rewrites the scheme file, backs the previous one up, and takes effect
// without a restart.
func TestAdminConfigSaveAppliesAndBacksUp(t *testing.T) {
	_, schemePath := withDefaultConfig(t)

	p := payloadFromView(currentView(t))
	p.Upstream.URL = "https://changed.example/api"
	p.VLM.Model = "Qwen-VL"
	p.Routing = append(p.Routing, adminRoutingEntry{Alias: "fast", Model: "glm-flash"})

	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("save must succeed, got %d: %s", res.Code, res.Body.String())
	}
	// The save is live without a reload of the process.
	if got := currentScheme().Text.URL; got != "https://changed.example/api" {
		t.Fatalf("the text upstream must be live, got %q", got)
	}
	if got := currentScheme().VLM.Model; got != "Qwen-VL" {
		t.Fatalf("the vlm model must be live, got %q", got)
	}
	if e, ok := currentScheme().Routes["fast"]; !ok || e.Model != "glm-flash" {
		t.Fatalf("the new route must be live, got %+v", currentScheme().Routes)
	}
	// The stored key survived a "keep" save even though the page never saw it.
	if got := currentScheme().Text.Key; got != "sk-original" {
		t.Fatalf("a keep save must preserve the stored key, got %q", got)
	}
	// The written file carries the new value and a backup holds the old one.
	written, err := os.ReadFile(schemePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "https://changed.example/api") {
		t.Fatalf("the scheme file must carry the new URL, got:\n%s", written)
	}
	backups, _ := filepath.Glob(schemePath + ".bak.*")
	if len(backups) != 1 {
		t.Fatalf("exactly one backup must be kept, got %v", backups)
	}
	old, _ := os.ReadFile(backups[0])
	if !strings.Contains(string(old), "https://anthropic.example/api") {
		t.Fatalf("the backup must hold the previous file, got:\n%s", old)
	}
}

// A save that would produce an unservable scheme is rejected and nothing is
// written.
func TestAdminConfigRejectsInvalidPayloadWithoutWriting(t *testing.T) {
	_, schemePath := withDefaultConfig(t)
	before, err := os.ReadFile(schemePath)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*adminConfigPayload)
		want   string
	}{
		{"empty url", func(p *adminConfigPayload) { p.Upstream.URL = "" }, "url"},
		{"bad protocol", func(p *adminConfigPayload) { p.Upstream.Protocol = "grpc" }, "protocol"},
		{"negative timeout", func(p *adminConfigPayload) { p.Upstream.BodyIdleSeconds = -1 }, "负数"},
		{"vlm without model", func(p *adminConfigPayload) { p.VLM.Active = true; p.VLM.Model = "" }, "vlm model"},
		{"vlm inheriting an openai upstream", func(p *adminConfigPayload) {
			p.Upstream.Protocol = protocolOpenAI
			p.Upstream.URL = "https://openai.example/v1"
			p.VLM.Active = true
			p.VLM.Inherit = true
			p.VLM.Model = "m"
		}, "anthropic"},
		{"chat on an anthropic upstream", func(p *adminConfigPayload) {
			p.Chat.Available = true
			p.Chat.Inherit = true
			p.Chat.Upstream = adminUpstreamPayload{}
		}, "openai"},
		{"empty route alias", func(p *adminConfigPayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "", Model: "m"})
		}, "别名"},
		{"empty route model", func(p *adminConfigPayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "x", Model: ""})
		}, "模型名"},
		{"duplicate route alias", func(p *adminConfigPayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "sonnet", Model: "m"})
		}, "重复"},
		{"port out of range", func(p *adminConfigPayload) { p.Proxy.Port = 70000 }, "端口"},
		{"switching to a missing scheme", func(p *adminConfigPayload) { p.Proxy.Scheme = "ghost" }, "不存在"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payloadFromView(currentView(t))
			tc.mutate(&p)
			res := postConfig(t, p, "s3cret")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), tc.want) {
				t.Fatalf("the rejection must mention %q, got %s", tc.want, res.Body.String())
			}
			after, err := os.ReadFile(schemePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("a rejected save must not touch the file, got:\n%s", after)
			}
		})
	}
}

// A key is a three-state choice: keep leaves it alone, set replaces it, clear
// removes it.
func TestAdminConfigKeyActions(t *testing.T) {
	withDefaultConfig(t)

	for _, tc := range []struct {
		action string
		key    string
		want   string
	}{
		{"keep", "ignored-when-keeping", "sk-original"},
		{"set", "sk-new", "sk-new"},
		{"clear", "", ""},
	} {
		t.Run(tc.action, func(t *testing.T) {
			p := payloadFromView(currentView(t))
			p.Upstream.KeyAction, p.Upstream.Key = tc.action, tc.key
			if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if got := currentScheme().Text.Key; got != tc.want {
				t.Fatalf("key after %s: got %q want %q", tc.action, got, tc.want)
			}
		})
	}

	// "set" with an empty value is a mistake, not a clear.
	p := payloadFromView(currentView(t))
	p.Upstream.KeyAction, p.Upstream.Key = "set", ""
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("set with an empty key must be rejected, got %d", res.Code)
	}
}

// A key_env reference survives a save verbatim and its value is resolved from the
// environment rather than written into the file.
func TestAdminConfigKeyEnvIsNotCopiedIntoTheFile(t *testing.T) {
	_, schemePath := withDefaultConfig(t)
	t.Setenv("MYS_TEST_SCHEME_KEY", "from-env")

	p := payloadFromView(currentView(t))
	p.Upstream.KeyAction, p.Upstream.Key, p.Upstream.KeyEnv = "clear", "", "MYS_TEST_SCHEME_KEY"
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := currentScheme().Text.Key; got != "from-env" {
		t.Fatalf("the env key must win, got %q", got)
	}
	written, _ := os.ReadFile(schemePath)
	if strings.Contains(string(written), "from-env") {
		t.Fatalf("a key resolved from the environment must not be written to the file, got:\n%s", written)
	}
	if !strings.Contains(string(written), `key_env = "MYS_TEST_SCHEME_KEY"`) {
		t.Fatalf("the key_env reference must be written, got:\n%s", written)
	}
}

func TestAdminConfigSaveKeepsAdminTokenByDefault(t *testing.T) {
	withDefaultConfig(t)
	p := payloadFromView(currentView(t))
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := currentConfig().Admin.Token; got != "s3cret" {
		t.Fatalf("a keep save must preserve the password, got %q", got)
	}
}

func TestAdminConfigViewReportsAdminTokenState(t *testing.T) {
	withDefaultConfig(t)
	if v := currentView(t); !v.Admin.TokenSet || v.Admin.TokenFromEnv {
		t.Fatalf("token state: %+v", v.Admin)
	}
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "env")
	if v := currentView(t); !v.Admin.TokenFromEnv {
		t.Fatalf("the env source must be reported: %+v", v.Admin)
	}
}

func TestAdminConfigAdminTokenActions(t *testing.T) {
	withDefaultConfig(t)

	p := payloadFromView(currentView(t))
	p.Admin = adminAuthPayload{Action: "set", Token: "brand-new"}
	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		ReauthRequired bool `json:"reauth_required"`
	}
	decodeBody(t, res, &out)
	if !out.ReauthRequired {
		t.Fatal("a changed password must ask the page to re-authenticate")
	}
	if got := currentConfig().Admin.Token; got != "brand-new" {
		t.Fatalf("the new password must be live, got %q", got)
	}

	// Clearing closes the page.
	p = payloadFromView(currentView(t))
	p.Admin = adminAuthPayload{Action: "clear"}
	if res := postConfig(t, p, "brand-new"); res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := currentConfig().Admin.Token; got != "" {
		t.Fatalf("clear must remove the password, got %q", got)
	}
}

func TestAdminConfigRejectsEmptyAdminTokenSet(t *testing.T) {
	withDefaultConfig(t)
	p := payloadFromView(currentView(t))
	p.Admin = adminAuthPayload{Action: "set", Token: ""}
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("set with an empty password must be rejected, got %d", res.Code)
	}
}

func TestAdminConfigSaveWarnsAboutPortChange(t *testing.T) {
	withDefaultConfig(t)
	p := payloadFromView(currentView(t))
	p.Proxy.Port = 9090
	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
}

// Declaring an anthropic-only scheme leaves the chat passthrough unavailable, and
// a save that leaves it unavailable writes no [chat] table.
func TestAdminConfigAnthropicOnlySchemeHasNoChatPassthrough(t *testing.T) {
	_, schemePath := withTempConfig(t, testMainTOML, `protocol = "anthropic"
url = "https://anthropic.example/api"
key = "sk"

[routing]
sonnet = "m"
`)
	view := currentView(t)
	if view.Chat.Available {
		t.Fatalf("an anthropic-only scheme must report no chat upstream, got %+v", view.Chat)
	}
	if view.VLM.Active {
		t.Fatalf("no [vlm] table means the describe pass is off, got %+v", view.VLM)
	}

	p := payloadFromView(view)
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	written, _ := os.ReadFile(schemePath)
	// The file's own header comment names both sections, so look for the table
	// headers at the start of a line rather than anywhere in the text.
	for _, section := range []string{"\n[chat]", "\n[vlm]"} {
		if strings.Contains(string(written), section) {
			t.Fatalf("an unavailable section must not be written, got:\n%s", written)
		}
	}
}

// Switching the active scheme re-points the main file and leaves every scheme file
// untouched.
func TestAdminConfigSchemeSwitchLeavesOtherFilesAlone(t *testing.T) {
	configPath, mainSchemePath := withDefaultConfig(t)
	// Add a second scheme file.
	second := filepath.Join(filepath.Dir(mainSchemePath), "other.toml")
	const otherBody = `protocol = "openai"
url = "https://other.example/v1"
key = "sk-other"
`
	if err := os.WriteFile(second, []byte(otherBody), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeMainFile, _ := os.ReadFile(mainSchemePath)

	p := payloadFromView(currentView(t))
	p.Proxy.Scheme = "other"
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("switch must succeed, got %d: %s", res.Code, res.Body.String())
	}
	if got := currentConfig().Proxy.Scheme; got != "other" {
		t.Fatalf("the active scheme must change, got %q", got)
	}
	if got := currentScheme().Text.URL; got != "https://other.example/v1" {
		t.Fatalf("the newly active scheme must be live, got %q", got)
	}
	afterMainFile, _ := os.ReadFile(mainSchemePath)
	if string(afterMainFile) != string(beforeMainFile) {
		t.Fatal("switching must not rewrite the previously active scheme file")
	}
	afterOther, _ := os.ReadFile(second)
	if string(afterOther) != otherBody {
		t.Fatalf("switching must not rewrite the target scheme file, got:\n%s", afterOther)
	}
	mainWritten, _ := os.ReadFile(configPath)
	if !strings.Contains(string(mainWritten), `scheme = "other"`) {
		t.Fatalf("the main file must record the new scheme, got:\n%s", mainWritten)
	}
}

// --- history endpoint ---

// The chart asks for one window at a time; an unrecognised range must still get
// a usable answer rather than an error, because the value only ever comes from
// the page's own buttons.
func TestAdminHistoryDefaultsTo24hAndEchoesRange(t *testing.T) {
	withDefaultConfig(t)

	for _, tc := range []struct{ query, want string }{
		{"", "24h"},
		{"?range=1h", "1h"},
		{"?range=6h", "6h"},
		{"?range=24h", "24h"},
		{"?range=nonsense", "24h"},
	} {
		w := adminCall(t, handleAdminStatsHistory, http.MethodGet, "/admin/api/stats/history"+tc.query, "", "s3cret")
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status %d", tc.query, w.Code)
		}
		var snap historySnapshot
		decodeBody(t, w, &snap)
		if snap.Range != tc.want {
			t.Errorf("%q: range %q, want %q", tc.query, snap.Range, tc.want)
		}
		if len(snap.Timestamps) == 0 {
			t.Errorf("%q: the page needs an axis even with no traffic", tc.query)
		}
	}
}

// The series and the axis have to agree, or the page would plot points against
// timestamps that do not line up.
func TestAdminHistoryPointsMatchTheAxis(t *testing.T) {
	withDefaultConfig(t)

	w := adminCall(t, handleAdminStatsHistory, http.MethodGet, "/admin/api/stats/history?range=6h", "", "s3cret")
	var snap historySnapshot
	decodeBody(t, w, &snap)
	for _, m := range snap.Models {
		if len(m.Points) != len(snap.Timestamps) {
			t.Fatalf("model %q has %d points for %d timestamps", m.Model, len(m.Points), len(snap.Timestamps))
		}
		for i, p := range m.Points {
			if p.TS != snap.Timestamps[i] {
				t.Fatalf("model %q point %d is at %d, axis says %d", m.Model, i, p.TS, snap.Timestamps[i])
			}
		}
	}
}

func TestAdminHistoryRejectsNonGet(t *testing.T) {
	withDefaultConfig(t)

	w := adminCall(t, handleAdminStatsHistory, http.MethodPost, "/admin/api/stats/history", "", "s3cret")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

// The endpoint is as closed as the rest of /admin: no token configured means the
// whole subtree is off, and a wrong password gets 401.
func TestAdminHistoryIsBehindAuth(t *testing.T) {
	withTempConfig(t, strings.Replace(testMainTOML, `token = "s3cret"`, `token = ""`, 1), testSchemeTOML)
	if w := adminCall(t, requireAdmin(handleAdminStatsHistory), http.MethodGet, "/admin/api/stats/history", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("without a token the route must 404, got %d", w.Code)
	}

	withDefaultConfig(t)
	if w := adminCall(t, requireAdmin(handleAdminStatsHistory), http.MethodGet, "/admin/api/stats/history", "", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("a bad password must 401, got %d", w.Code)
	}
	if w := adminCall(t, requireAdmin(handleAdminStatsHistory), http.MethodGet, "/admin/api/stats/history", "", "s3cret"); w.Code != http.StatusOK {
		t.Fatalf("the right password must pass, got %d", w.Code)
	}
}

// A caller must not be able to make the server build an unbounded number of
// series by repeating ?model=.
func TestAdminHistoryCapsTheModelFilter(t *testing.T) {
	withDefaultConfig(t)

	q := "/admin/api/stats/history?range=1h"
	for i := 0; i < historyMaxFilter+20; i++ {
		q += "&model=m" + strconv.Itoa(i)
	}
	w := adminCall(t, handleAdminStatsHistory, http.MethodGet, q, "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var snap historySnapshot
	decodeBody(t, w, &snap)
	if len(snap.Timestamps) != 60 {
		t.Fatalf("the window must still be served, got %d points", len(snap.Timestamps))
	}
}

// The cache columns in the interval summary are read straight off the series, so
// the payload has to carry the window totals and not only the per-bucket points.
func TestAdminHistoryCarriesCacheTotals(t *testing.T) {
	withDefaultConfig(t)

	stats.reset()
	c := stats.beginReq("sonnet", "DeepSeek-Flash", "anthropic")
	c.success(tokenUsage{Input: 300, Output: 5, CacheRead: 700, Reported: true})

	w := adminCall(t, handleAdminStatsHistory, http.MethodGet, "/admin/api/stats/history?range=1h", "", "s3cret")
	var snap historySnapshot
	decodeBody(t, w, &snap)
	if len(snap.Models) != 1 {
		t.Fatalf("want one series, got %d", len(snap.Models))
	}
	m := snap.Models[0]
	if m.CacheRead != 700 || m.CacheHitRate != 0.7 {
		t.Fatalf("series must report 700 read / 0.7, got %d / %v", m.CacheRead, m.CacheHitRate)
	}
	if snap.Totals.CacheRead != 700 {
		t.Fatalf("totals must carry the cache counters, got %+v", snap.Totals)
	}
	last := m.Points[len(m.Points)-1]
	if last.Input != 300 || last.CacheRead != 700 || last.CacheHitRate != 0.7 {
		t.Fatalf("the newest bucket must carry its own counters, got %+v", last)
	}
	stats.reset()
}

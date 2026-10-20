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

// schemePayloadFromView builds the scheme-editor payload a round trip of the page
// would send: the view's own values back, with every key action left at "keep".
func schemePayloadFromView(view adminSchemeView) adminSchemePayload {
	return adminSchemePayload{
		Name:     view.Name,
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

// postScheme saves one scheme file through the editor endpoint.
func postScheme(t *testing.T, p adminSchemePayload, password string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, handleAdminSaveScheme, "/admin/api/config/scheme", p, password)
}

// postMain saves the main configuration.
func postMain(t *testing.T, p adminMainPayload, password string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, handleAdminSaveMain, "/admin/api/config/main", p, password)
}

// postActivate switches the active scheme.
func postActivate(t *testing.T, name, password string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, handleAdminActivate, "/admin/api/config/activate", adminActivatePayload{Scheme: name}, password)
}

func postJSON(t *testing.T, h http.HandlerFunc, path string, payload interface{}, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return adminCall(t, h, http.MethodPost, path, string(body), password)
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

// schemeEditorView loads one scheme through the editor endpoint, which is how the
// scheme page reads any file — active or not.
func schemeEditorView(t *testing.T, name string) adminSchemeView {
	t.Helper()
	target := "/admin/api/schemes"
	if name != "" {
		target += "?name=" + name
	}
	w := adminCall(t, handleAdminSchemes, http.MethodGet, target, "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("scheme view status %d: %s", w.Code, w.Body.String())
	}
	var view adminSchemeView
	decodeBody(t, w, &view)
	return view
}

// A save rewrites the scheme file, backs the previous one up, and takes effect
// without a restart.
func TestAdminConfigSaveAppliesAndBacksUp(t *testing.T) {
	_, schemePath := withDefaultConfig(t)

	p := schemePayloadFromView(schemeEditorView(t, ""))
	p.Upstream.URL = "https://changed.example/api"
	p.VLM.Model = "Qwen-VL"
	p.Routing = append(p.Routing, adminRoutingEntry{Alias: "fast", Model: "glm-flash"})

	res := postScheme(t, p, "s3cret")
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
		mutate func(*adminSchemePayload)
		want   string
	}{
		{"empty url", func(p *adminSchemePayload) { p.Upstream.URL = "" }, "url"},
		{"bad protocol", func(p *adminSchemePayload) { p.Upstream.Protocol = "grpc" }, "protocol"},
		{"negative timeout", func(p *adminSchemePayload) { p.Upstream.BodyIdleSeconds = -1 }, "负数"},
		{"vlm without model", func(p *adminSchemePayload) { p.VLM.Active = true; p.VLM.Model = "" }, "vlm model"},
		{"chat on an anthropic upstream", func(p *adminSchemePayload) {
			p.Chat.Available = true
			p.Chat.Inherit = true
			p.Chat.Upstream = adminUpstreamPayload{}
		}, "openai"},
		{"empty route alias", func(p *adminSchemePayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "", Model: "m"})
		}, "别名"},
		{"empty route model", func(p *adminSchemePayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "x", Model: ""})
		}, "模型名"},
		{"duplicate route alias", func(p *adminSchemePayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "sonnet", Model: "m"})
		}, "重复"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := schemePayloadFromView(schemeEditorView(t, ""))
			tc.mutate(&p)
			res := postScheme(t, p, "s3cret")
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
			p := schemePayloadFromView(schemeEditorView(t, ""))
			p.Upstream.KeyAction, p.Upstream.Key = tc.action, tc.key
			if res := postScheme(t, p, "s3cret"); res.Code != http.StatusOK {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if got := currentScheme().Text.Key; got != tc.want {
				t.Fatalf("key after %s: got %q want %q", tc.action, got, tc.want)
			}
		})
	}

	// "set" with an empty value is a mistake, not a clear.
	p := schemePayloadFromView(schemeEditorView(t, ""))
	p.Upstream.KeyAction, p.Upstream.Key = "set", ""
	if res := postScheme(t, p, "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("set with an empty key must be rejected, got %d", res.Code)
	}
}

// A key_env reference survives a save verbatim and its value is resolved from the
// environment rather than written into the file.
func TestAdminConfigKeyEnvIsNotCopiedIntoTheFile(t *testing.T) {
	_, schemePath := withDefaultConfig(t)
	t.Setenv("MYS_TEST_SCHEME_KEY", "from-env")

	p := schemePayloadFromView(schemeEditorView(t, ""))
	p.Upstream.KeyAction, p.Upstream.Key, p.Upstream.KeyEnv = "clear", "", "MYS_TEST_SCHEME_KEY"
	if res := postScheme(t, p, "s3cret"); res.Code != http.StatusOK {
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

// The admin password belongs to the main form: a scheme save neither reads nor
// changes it.
func TestAdminMainSaveKeepsAdminTokenByDefault(t *testing.T) {
	withDefaultConfig(t)
	if res := postMain(t, adminMainPayload{Port: 8088, Admin: adminAuthPayload{Action: "keep"}}, "s3cret"); res.Code != http.StatusOK {
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

func TestAdminMainAdminTokenActions(t *testing.T) {
	withDefaultConfig(t)

	res := postMain(t, adminMainPayload{Port: 8088, Admin: adminAuthPayload{Action: "set", Token: "brand-new"}}, "s3cret")
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
	if res := postMain(t, adminMainPayload{Port: 8088, Admin: adminAuthPayload{Action: "clear"}}, "brand-new"); res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := currentConfig().Admin.Token; got != "" {
		t.Fatalf("clear must remove the password, got %q", got)
	}
}

func TestAdminMainRejectsEmptyAdminTokenSet(t *testing.T) {
	withDefaultConfig(t)
	if res := postMain(t, adminMainPayload{Port: 8088, Admin: adminAuthPayload{Action: "set", Token: ""}}, "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("set with an empty password must be rejected, got %d", res.Code)
	}
}

func TestAdminMainRejectsPortOutOfRange(t *testing.T) {
	withDefaultConfig(t)
	res := postMain(t, adminMainPayload{Port: 70000, Admin: adminAuthPayload{Action: "keep"}}, "s3cret")
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "端口") {
		t.Fatalf("an out-of-range port must be rejected with a port message, got %d: %s", res.Code, res.Body.String())
	}
}

func TestAdminMainSaveWarnsAboutPortChange(t *testing.T) {
	withDefaultConfig(t)
	res := postMain(t, adminMainPayload{Port: 9090, Admin: adminAuthPayload{Action: "keep"}}, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	decodeBody(t, res, &out)
	joined := strings.Join(out.Warnings, " | ")
	if !strings.Contains(joined, "重启") {
		t.Fatalf("a port change must say a restart is needed, got: %s", joined)
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
	view := schemeEditorView(t, "")
	if view.Chat.Available {
		t.Fatalf("an anthropic-only scheme must report no chat upstream, got %+v", view.Chat)
	}
	if view.VLM.Active {
		t.Fatalf("no [vlm] table means the describe pass is off, got %+v", view.VLM)
	}

	p := schemePayloadFromView(view)
	if res := postScheme(t, p, "s3cret"); res.Code != http.StatusOK {
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

	if res := postActivate(t, "other", "s3cret"); res.Code != http.StatusOK {
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

// A pre-0.15 main file still carries the old global sections. Telling its owner to
// "add a scheme" would leave an upstream definition with nowhere to live, so the
// error has to name the old format and point at the migration notes.
func TestLoadConfigReportsTheOldFormatAndPointsAtTheMigration(t *testing.T) {
	for _, tc := range []struct {
		name string
		main string
		want []string
	}{
		{
			name: "old global upstream and keys",
			main: `[proxy]
port = 8088

[upstream]
anthropic_url = "https://a.example/api"
openai_url = "https://o.example/v1"

[keys]
sophnet = "k"
`,
			want: []string{"[keys]", "[upstream]", "旧格式", "从 0.14.x 升级"},
		},
		{
			name: "old profiles and routing",
			main: `[proxy]
port = 8088
vlm_model = "m"

[upstreams.alpha]
protocol = "openai"
url = "https://a.example/v1"

[routing]
sonnet = "m"
`,
			want: []string{"[proxy].vlm_model", "[routing]", "[upstreams]", "旧格式"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeSchemeTree(t, tc.main, "unused", "")
			err := loadConfig()
			if err == nil {
				t.Fatal("a pre-0.15 main file must be rejected")
			}
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("the error must mention %q, got: %s", want, msg)
				}
			}
		})
	}
}

// A new-format file that merely forgot the scheme line gets the plain message: it
// is not the old format and must not be sent to the migration notes.
func TestLoadConfigMissingSchemeOnANewFileIsNotCalledLegacy(t *testing.T) {
	writeSchemeTree(t, "[proxy]\nport = 8088\n", "unused", "")
	err := loadConfig()
	if err == nil {
		t.Fatal("a file with no scheme must be rejected")
	}
	if strings.Contains(err.Error(), "旧格式") {
		t.Fatalf("a new-format file must not be reported as legacy, got: %v", err)
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("the error must say what to set, got: %v", err)
	}
}

// SOPHNET_API_KEY is the supported way to keep a key out of the file — the shipped
// example uses it — so warning that it "is no longer read" whenever it is set is a
// false alarm. The warning belongs to the leftover case: set in the environment,
// referenced by no upstream in the scheme being saved.
func TestAdminConfigSOPHNETKeyEnvWarningOnlyWhenUnreferenced(t *testing.T) {
	withDefaultConfig(t)
	t.Setenv("SOPHNET_API_KEY", "env-secret")

	save := func(mutate func(*adminSchemePayload)) []string {
		t.Helper()
		p := schemePayloadFromView(schemeEditorView(t, ""))
		mutate(&p)
		res := postScheme(t, p, "s3cret")
		if res.Code != http.StatusOK {
			t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
		}
		var out struct {
			Warnings []string `json:"warnings"`
		}
		decodeBody(t, res, &out)
		return out.Warnings
	}

	joined := strings.Join(save(func(p *adminSchemePayload) {
		p.Upstream.KeyAction, p.Upstream.Key, p.Upstream.KeyEnv = "clear", "", "SOPHNET_API_KEY"
	}), " | ")
	if strings.Contains(joined, "都没有引用它") {
		t.Fatalf("a scheme that reads the variable must not be warned about, got: %s", joined)
	}

	// The page resubmits the key_env it read back, so dropping the reference is an
	// explicit act (the field cleared), not the default.
	joined = strings.Join(save(func(p *adminSchemePayload) {
		p.Upstream.KeyAction, p.Upstream.Key, p.Upstream.KeyEnv = "set", "sk-inline", ""
	}), " | ")
	if !strings.Contains(joined, "都没有引用它") {
		t.Fatalf("an unreferenced variable must be reported, got: %s", joined)
	}

	// A [vlm] that reads it counts too.
	joined = strings.Join(save(func(p *adminSchemePayload) {
		p.VLM.Active, p.VLM.Model = true, "vl"
		p.VLM.Inherit = false
		p.VLM.Upstream = adminUpstreamPayload{
			Protocol: protocolAnthropic, URL: "https://vlm.example/api", KeyAction: "clear", KeyEnv: "SOPHNET_API_KEY",
		}
	}), " | ")
	if strings.Contains(joined, "都没有引用它") {
		t.Fatalf("a [vlm] reference must count as using the variable, got: %s", joined)
	}
}

// The haiku hint has two different truths: with sonnet declared, haiku inherits it;
// with nothing declared, haiku passes through. Saying "passes through" in the first
// case would be wrong.
func TestAdminConfigHaikuHintReflectsTheInheritance(t *testing.T) {
	withDefaultConfig(t)
	save := func(routing []adminRoutingEntry) string {
		t.Helper()
		p := schemePayloadFromView(schemeEditorView(t, ""))
		p.Routing = routing
		res := postScheme(t, p, "s3cret")
		if res.Code != http.StatusOK {
			t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
		}
		var out struct {
			Warnings []string `json:"warnings"`
		}
		decodeBody(t, res, &out)
		return strings.Join(out.Warnings, " | ")
	}

	// sonnet declared, haiku not: haiku inherits sonnet.
	joined := save([]adminRoutingEntry{{Alias: "sonnet", Model: "m"}})
	if !strings.Contains(joined, "沿用 sonnet") {
		t.Fatalf("a declared sonnet must be reported as what haiku inherits, got: %s", joined)
	}
	if strings.Contains(joined, "未声明 haiku：客户端请求 haiku（含 claude-haiku-4-5 这类完整名字）时会原样转发") {
		t.Fatalf("haiku must not be described as passing through when sonnet is declared, got: %s", joined)
	}

	// Nothing declared: haiku really does pass through.
	joined = save(nil)
	if !strings.Contains(joined, "未声明 haiku") || !strings.Contains(joined, "原样转发") {
		t.Fatalf("with nothing declared haiku must be reported as passing through, got: %s", joined)
	}
}

// A [vlm]/[chat] section with no url inherits the whole text upstream, so any
// upstream field on it would be discarded. That is the silent fallback this design
// removes, so the load fails and names the offending fields.
func TestSchemeSectionWithoutURLRejectsDiscardedFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scheme string
		want   []string
	}{
		{
			name: "vlm sets a key_env but no url",
			scheme: `protocol = "anthropic"
url = "https://anthropic.example/api"

[vlm]
model = "vl"
key_env = "SOPHNET_API_KEY"
`,
			want: []string{"[vlm]", "key_env", "no url"},
		},
		{
			name: "chat sets a protocol and key but no url",
			scheme: `protocol = "openai"
url = "https://openai.example/v1"

[chat]
protocol = "openai"
key = "sk-x"
`,
			want: []string{"[chat]", "protocol", "key"},
		},
		{
			name: "vlm sets a timeout but no url",
			scheme: `protocol = "anthropic"
url = "https://anthropic.example/api"

[vlm]
model = "vl"
max_retries = 3
`,
			want: []string{"[vlm]", "max_retries"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, schemePath := writeSchemeTree(t, testMainTOML, "main", tc.scheme)
			// The "fixed" half below loads successfully, so restore the published
			// config afterwards like every other config-loading test does.
			prevCfg, prevScheme := currentConfig(), currentScheme()
			t.Cleanup(func() {
				cfgMu.Lock()
				cfg, scheme = prevCfg, prevScheme
				cfgMu.Unlock()
			})
			err := loadConfig()
			if err == nil {
				t.Fatal("a section that would discard its own fields must be rejected")
			}
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("the error must mention %q, got: %s", want, msg)
				}
			}

			// The same file without the discarded fields loads, so the error is
			// about the contradiction and not about inheriting in general.
			fixed := strings.NewReplacer(
				"key_env = \"SOPHNET_API_KEY\"\n", "",
				"protocol = \"openai\"\nkey = \"sk-x\"\n", "",
				"max_retries = 3\n", "",
			).Replace(tc.scheme)
			if err := os.WriteFile(schemePath, []byte(fixed), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := loadConfig(); err != nil {
				t.Fatalf("inheriting with no extra fields must load, got: %v\n%s", err, fixed)
			}
		})
	}
}

// The three save endpoints are separate on purpose: the main form must not rewrite
// a scheme file, and the scheme editor must not rewrite the main file. Both used to
// be one endpoint that always wrote both.
func TestAdminMainSaveDoesNotTouchAnySchemeFile(t *testing.T) {
	_, schemePath := withDefaultConfig(t)
	before, err := os.ReadFile(schemePath)
	if err != nil {
		t.Fatal(err)
	}

	res := postMain(t, adminMainPayload{Port: 8089, Admin: adminAuthPayload{Action: "keep"}}, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := currentConfig().Proxy.Port; got != 8089 {
		t.Fatalf("the new port must be live, got %d", got)
	}
	after, _ := os.ReadFile(schemePath)
	if string(after) != string(before) {
		t.Fatalf("a main save must not rewrite the scheme file, got:\n%s", after)
	}
	backups, _ := filepath.Glob(schemePath + ".bak.*")
	if len(backups) != 0 {
		t.Fatalf("a main save must not back up a scheme file, got %v", backups)
	}
}

func TestAdminSchemeSaveDoesNotTouchTheMainFile(t *testing.T) {
	configPath, schemePath := withDefaultConfig(t)
	beforeMain, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	p := schemePayloadFromView(schemeEditorView(t, ""))
	p.Upstream.URL = "https://changed.example/api"
	if res := postScheme(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if got := currentScheme().Text.URL; got != "https://changed.example/api" {
		t.Fatalf("the active scheme must be reloaded, got %q", got)
	}
	afterMain, _ := os.ReadFile(configPath)
	if string(afterMain) != string(beforeMain) {
		t.Fatalf("a scheme save must not rewrite the main file, got:\n%s", afterMain)
	}
	// The scheme file itself did change, which is the point.
	written, _ := os.ReadFile(schemePath)
	if !strings.Contains(string(written), "https://changed.example/api") {
		t.Fatalf("the scheme file must carry the change, got:\n%s", written)
	}
}

// The editor can load and save a scheme that is not running, and saving it must not
// disturb the traffic the active one is serving.
func TestAdminSchemeEditorEditsAnInactiveScheme(t *testing.T) {
	_, activeSchemePath := withDefaultConfig(t)
	second := filepath.Join(filepath.Dir(activeSchemePath), "other.toml")
	const otherBody = `protocol = "openai"
url = "https://other.example/v1"
key = "sk-other"

[routing]
sonnet = "other-model"
`
	if err := os.WriteFile(second, []byte(otherBody), 0o600); err != nil {
		t.Fatal(err)
	}

	view := schemeEditorView(t, "other")
	if view.Name != "other" || view.Active {
		t.Fatalf("the editor must load an inactive scheme and say it is inactive, got %+v", view)
	}
	if view.Upstream.URL != "https://other.example/v1" {
		t.Fatalf("the editor must show that file's own values, got %+v", view.Upstream)
	}

	p := schemePayloadFromView(view)
	p.Upstream.URL = "https://other-changed.example/v1"
	res := postScheme(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var out struct {
		Applied bool `json:"applied"`
	}
	decodeBody(t, res, &out)
	if out.Applied {
		t.Fatal("saving an inactive scheme must not claim it is applied")
	}

	// The running scheme is untouched and still on its own URL.
	if got := currentScheme().Text.URL; got != "https://anthropic.example/api" {
		t.Fatalf("the active scheme must be unaffected, got %q", got)
	}
	if got := currentConfig().Proxy.Scheme; got != "main" {
		t.Fatalf("saving another scheme must not switch to it, got %q", got)
	}
	written, _ := os.ReadFile(second)
	if !strings.Contains(string(written), "https://other-changed.example/v1") {
		t.Fatalf("the inactive scheme file must carry the change, got:\n%s", written)
	}
}

// The editor only edits existing files: creating one is a filesystem action, and a
// save must not invent a scheme the operator cannot see in the directory.
func TestAdminSchemeSaveRejectsAnUnknownScheme(t *testing.T) {
	withDefaultConfig(t)
	p := schemePayloadFromView(schemeEditorView(t, ""))
	p.Name = "ghost"
	res := postScheme(t, p, "s3cret")
	if res.Code != http.StatusNotFound {
		t.Fatalf("an unknown scheme must be rejected, got %d: %s", res.Code, res.Body.String())
	}
}

func TestAdminActivateRejectsMissingAndUnloadableSchemes(t *testing.T) {
	_, activeSchemePath := withDefaultConfig(t)

	if res := postActivate(t, "ghost", "s3cret"); res.Code != http.StatusBadRequest {
		t.Fatalf("activating a missing scheme must be rejected, got %d", res.Code)
	}

	// A file that exists but would not load is refused before it can take the proxy
	// down on the next request.
	broken := filepath.Join(filepath.Dir(activeSchemePath), "broken.toml")
	if err := os.WriteFile(broken, []byte("protocol = \"grpc\"\nurl = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := postActivate(t, "broken", "s3cret")
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "无法加载") {
		t.Fatalf("an unloadable scheme must be refused with a reason, got %d: %s", res.Code, res.Body.String())
	}
	if got := currentConfig().Proxy.Scheme; got != "main" {
		t.Fatalf("a refused switch must leave the active scheme alone, got %q", got)
	}
}

func TestAdminActivateAlreadyActiveIsANoOp(t *testing.T) {
	configPath, _ := withDefaultConfig(t)
	before, _ := os.ReadFile(configPath)

	res := postActivate(t, "main", "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	after, _ := os.ReadFile(configPath)
	if string(after) != string(before) {
		t.Fatalf("activating the running scheme must not rewrite the main file, got:\n%s", after)
	}
}

// The old combined POST is gone: a page still calling it gets a clear error rather
// than a save that writes files it did not model.
func TestAdminConfigCombinedPostIsGone(t *testing.T) {
	withDefaultConfig(t)
	body, _ := json.Marshal(map[string]interface{}{"proxy": map[string]interface{}{"port": 9000}})
	w := adminCall(t, handleAdminConfig, http.MethodPost, "/admin/api/config", string(body), "s3cret")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("the combined save must be refused, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/admin/api/config/main") {
		t.Fatalf("the error must name the endpoints that replaced it, got %s", w.Body.String())
	}
}

// The three write endpoints are POST-only, and say so rather than failing on a
// missing body: a client pointed at the wrong verb gets an actionable message.
func TestAdminWriteEndpointsRejectNonPost(t *testing.T) {
	withDefaultConfig(t)
	for _, h := range []http.HandlerFunc{handleAdminSaveMain, handleAdminSaveScheme, handleAdminActivate} {
		w := adminCall(t, h, http.MethodGet, "/admin/api/config/x", "", "s3cret")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET must be refused, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "accepts POST") {
			t.Fatalf("the error must name the verb, got %s", w.Body.String())
		}
	}
}

// The save that used to fail: an openai text upstream with [vlm] inheriting it. The
// describe call is framed per protocol now, so there is nothing to reject — and the
// promise that a [vlm] section may name its own upstream in either protocol holds
// too.
func TestAdminSchemeSaveAcceptsVLMOnAnOpenAIUpstream(t *testing.T) {
	withDefaultConfig(t)

	// Inheriting an openai upstream.
	p := schemePayloadFromView(schemeEditorView(t, ""))
	p.Upstream = adminUpstreamPayload{
		Protocol: protocolOpenAI, URL: "https://openai.example/v1", KeyAction: "keep",
	}
	p.VLM = adminVLMPayload{Active: true, Model: "vision-model", Inherit: true}
	res := postScheme(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("inheriting an openai upstream must be accepted, got %d: %s", res.Code, res.Body.String())
	}
	if got := currentScheme().VLM.Upstream.Protocol; got != protocolOpenAI {
		t.Fatalf("the describe pass must run on the openai upstream, got %q", got)
	}
	if !currentScheme().VLMInheritsText {
		t.Fatal("the section must still be recorded as inheriting")
	}

	// A [vlm] naming its own openai upstream.
	p = schemePayloadFromView(schemeEditorView(t, ""))
	p.VLM = adminVLMPayload{
		Active: true, Model: "vision-model", Inherit: false,
		Upstream: adminUpstreamPayload{Protocol: protocolOpenAI, URL: "https://vision.example/v1", KeyAction: "keep"},
	}
	res = postScheme(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("an explicit openai [vlm] must be accepted, got %d: %s", res.Code, res.Body.String())
	}
	if got := currentScheme().VLM.Upstream.URL; got != "https://vision.example/v1" {
		t.Fatalf("the [vlm] upstream must be the one written, got %q", got)
	}
	// And it must survive a reload from the file, not just live in memory.
	reloaded, err := parseSchemeFile(schemePath(dirOf(t), "main"), "main")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.VLM == nil || reloaded.VLM.Upstream.Protocol != protocolOpenAI {
		t.Fatalf("the written file must keep the openai describe upstream, got %+v", reloaded.VLM)
	}
}

// dirOf is the schemes directory of the running config.
func dirOf(t *testing.T) string {
	t.Helper()
	c := currentConfig()
	return schemesDir(&c)
}

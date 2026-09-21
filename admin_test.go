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

	"github.com/BurntSushi/toml"
)

const testConfigTOML = `[proxy]
port = 8088
vlm_model = "MiniMax-M3"

[upstream]
anthropic_url = "https://anthropic.example/api"
openai_url = "https://openai.example/v1"

[keys]
sophnet = "sk-original"

[admin]
token = "s3cret"

[routing]
sonnet = { model = "DeepSeek-Flash", supports_image = true }
haiku = { model = "glm-5.3-flash", upstream = "openai" }
`

// withTempConfig points the proxy at a throwaway config file, loads it, and
// restores the previous live config when the test ends.
func withTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_PROXY_CONFIG", path)
	t.Setenv("SOPHNET_API_KEY", "")
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "")

	cfgMu.Lock()
	oldCfg, oldRoutes := cfg, routeTargets
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg, routeTargets = oldCfg, oldRoutes
		cfgMu.Unlock()
	})

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return path
}

func adminCall(t *testing.T, handler http.HandlerFunc, method, target, body, password string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
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
	withTempConfig(t, strings.Replace(testConfigTOML, `token = "s3cret"`, `token = ""`, 1))

	for _, h := range []http.HandlerFunc{handleAdminPage, handleAdminConfig, handleAdminStats} {
		w := adminCall(t, requireAdmin(h), http.MethodGet, "/admin", "", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("disabled admin must return 404, got %d", w.Code)
		}
	}
}

func TestAdminRejectsBadCredentials(t *testing.T) {
	withTempConfig(t, testConfigTOML)

	w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing credentials must return 401, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("401 must carry a Basic challenge, got %q", w.Header().Get("WWW-Authenticate"))
	}

	w = adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "wrong")
	if w.Code != http.StatusUnauthorized {
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
	withTempConfig(t, testConfigTOML)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "env-token")

	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "s3cret"); w.Code != http.StatusUnauthorized {
		t.Fatalf("the config token must be ignored once the env var is set, got %d", w.Code)
	}
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "env-token"); w.Code != http.StatusOK {
		t.Fatalf("the env token must be accepted, got %d", w.Code)
	}
}

func TestAdminConfigViewRedactsKey(t *testing.T) {
	withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-original") {
		t.Fatal("the upstream key must never be returned to the page")
	}

	var view adminConfigView
	decodeBody(t, w, &view)
	if !view.Keys.SophnetSet {
		t.Fatal("the page must know a key is configured")
	}
	if view.Keys.SophnetFromEnv {
		t.Fatal("no env key is set in this test")
	}
	if view.Proxy.VLMModel != "MiniMax-M3" || view.Upstream.HeaderTimeoutSeconds != 120 {
		t.Fatalf("defaults must be applied to the view: %+v", view)
	}
	// routing lists what the file declares, so a save cannot freeze an inherited
	// value into the file. The builtin opus fallback is not declared, so it shows
	// up only in the effective view.
	if len(view.Routing) != 2 {
		t.Fatalf("routing must list the declared aliases only, got %+v", view.Routing)
	}
	byAlias := map[string]adminRoutingEntry{}
	for _, e := range view.Routing {
		byAlias[e.Alias] = e
	}
	if _, ok := byAlias["opus"]; ok {
		t.Fatalf("an undeclared builtin fallback must not appear in the editable routing, got %+v", byAlias["opus"])
	}
	if e := byAlias["sonnet"]; !e.SupportsImage || e.Model != "DeepSeek-Flash" {
		t.Fatalf("sonnet route: %+v", e)
	}
	// The fallback is still reported as effective, so the operator can see it.
	if e, ok := view.EffectiveRouting["opus"]; !ok || e.Model != "GLM-5.2" || e.Declared {
		t.Fatalf("the builtin opus fallback must be reported as effective and undeclared, got %+v", e)
	}
	if e, ok := view.EffectiveRouting["haiku"]; !ok || !e.Declared || e.Upstream != "openai" {
		t.Fatalf("a declared route must be reported as effective and declared, got %+v", e)
	}
}

func payloadFromView(t *testing.T, view adminConfigView) adminConfigPayload {
	t.Helper()
	return adminConfigPayload{
		Proxy:    view.Proxy,
		Upstream: view.Upstream,
		Keys:     adminKeysPayload{Action: "keep"},
		Routing:  view.Routing,
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

// A save must land on disk, be reloaded into the running process, and leave a
// timestamped backup of what was there before.
func TestAdminConfigSaveAppliesAndBacksUp(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	p := payloadFromView(t, view)
	p.Proxy.VLMModel = "Qwen3-VL"
	p.Upstream.MaxRetries = 5
	for i := range p.Routing {
		if p.Routing[i].Alias == "sonnet" {
			p.Routing[i].Model = "DeepSeek-V5"
		}
	}

	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", res.Code, res.Body.String())
	}

	// Live process picked the change up without a restart.
	if got := routeModelName("sonnet"); got != "DeepSeek-V5" {
		t.Fatalf("save must reload the running config, sonnet -> %q", got)
	}
	if got := currentConfig().Proxy.VLMModel; got != "Qwen3-VL" {
		t.Fatalf("save must reload the running config, vlm -> %q", got)
	}
	if got := snapshotConfig().maxRetries(); got != 5 {
		t.Fatalf("save must reload retries, got %d", got)
	}

	// And it survived to disk, parseable.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var reread Config
	if err := toml.Unmarshal(onDisk, &reread); err != nil {
		t.Fatalf("saved config must be valid TOML: %v\n%s", err, onDisk)
	}
	if reread.Proxy.VLMModel != "Qwen3-VL" {
		t.Fatalf("disk copy out of sync: %+v", reread.Proxy)
	}

	backups, err := filepath.Glob(path + ".bak.*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("exactly one timestamped backup expected, got %v (err=%v)", backups, err)
	}
	backup, _ := os.ReadFile(backups[0])
	if !strings.Contains(string(backup), "MiniMax-M3") {
		t.Fatal("the backup must hold the pre-save contents")
	}
}

// A rejected save must not touch the file or the running config.
func TestAdminConfigRejectsInvalidPayloadWithoutWriting(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)
	before, _ := os.ReadFile(path)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	cases := []struct {
		name   string
		mutate func(*adminConfigPayload)
	}{
		{"port out of range", func(p *adminConfigPayload) { p.Proxy.Port = 70000 }},
		{"empty anthropic url", func(p *adminConfigPayload) { p.Upstream.AnthropicURL = "" }},
		{"bad url scheme", func(p *adminConfigPayload) { p.Upstream.OpenAIURL = "ftp://x/y" }},
		{"bad default upstream", func(p *adminConfigPayload) { p.Upstream.DefaultUpstream = "gemini" }},
		{"duplicate alias", func(p *adminConfigPayload) {
			p.Routing = append(p.Routing, adminRoutingEntry{Alias: "sonnet", Model: "dup"})
		}},
		{"empty model", func(p *adminConfigPayload) { p.Routing[0].Model = "  " }},
		{"empty alias", func(p *adminConfigPayload) { p.Routing[0].Alias = "" }},
		{"bad route upstream", func(p *adminConfigPayload) { p.Routing[0].Upstream = "bedrock" }},
		{"set without a key", func(p *adminConfigPayload) { p.Keys = adminKeysPayload{Action: "set"} }},
		{"unknown key action", func(p *adminConfigPayload) { p.Keys = adminKeysPayload{Action: "wipe"} }},
		{"negative retries", func(p *adminConfigPayload) { p.Upstream.MaxRetries = -1 }},
		{"control char in model", func(p *adminConfigPayload) { p.Routing[0].Model = "bad\x00model" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payloadFromView(t, view)
			tc.mutate(&p)
			res := postConfig(t, p, "s3cret")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("invalid payload must be rejected with 400, got %d %s", res.Code, res.Body.String())
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("a rejected save must leave the config file untouched")
			}
			if got := routeModelName("sonnet"); got != "DeepSeek-Flash" {
				t.Fatalf("a rejected save must not touch the running config, sonnet -> %q", got)
			}
		})
	}
}

func TestAdminConfigKeyActions(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)
	readKey := func(t *testing.T) string {
		t.Helper()
		data, _ := os.ReadFile(path)
		var c Config
		if err := toml.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		return c.Keys.Sophnet
	}

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	// keep leaves the stored key alone even though the page never sent one.
	p := payloadFromView(t, view)
	p.Keys = adminKeysPayload{Action: "keep"}
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("keep failed: %s", res.Body.String())
	}
	if got := readKey(t); got != "sk-original" {
		t.Fatalf("keep must preserve the key, got %q", got)
	}

	// set replaces it.
	p.Keys = adminKeysPayload{Action: "set", Sophnet: "sk-new"}
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("set failed: %s", res.Body.String())
	}
	if got := readKey(t); got != "sk-new" {
		t.Fatalf("set must store the new key, got %q", got)
	}

	// clear empties it.
	p.Keys = adminKeysPayload{Action: "clear"}
	if res := postConfig(t, p, "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("clear failed: %s", res.Body.String())
	}
	if got := readKey(t); got != "" {
		t.Fatalf("clear must empty the key, got %q", got)
	}
}

// The admin token is editable from the page, but a save that does not ask to
// change it must carry it over rather than locking the operator out.
func TestAdminConfigSaveKeepsAdminTokenByDefault(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)
	if res := postConfig(t, payloadFromView(t, view), "s3cret"); res.Code != http.StatusOK {
		t.Fatalf("save failed: %s", res.Body.String())
	}

	data, _ := os.ReadFile(path)
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.Admin.Token != "s3cret" {
		t.Fatalf("a save must preserve the admin token, got %q", c.Admin.Token)
	}
	// And the page is still reachable with the same password.
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "s3cret"); w.Code != http.StatusOK {
		t.Fatalf("the page must still accept the unchanged password, got %d", w.Code)
	}
}

func TestAdminConfigViewReportsAdminTokenState(t *testing.T) {
	withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)
	if !view.Admin.TokenSet {
		t.Fatal("the page must know a password is configured")
	}
	if view.Admin.TokenFromEnv {
		t.Fatal("no env password is set in this test")
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatal("the admin password must never be returned to the page")
	}

	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "env-token")
	w = adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "env-token")
	decodeBody(t, w, &view)
	if !view.Admin.TokenFromEnv {
		t.Fatal("the page must report when the env var supplies the password")
	}
}

// set replaces the password and makes the old one stop working; clear closes the
// page entirely.
func TestAdminConfigAdminTokenActions(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)
	readToken := func(t *testing.T) string {
		t.Helper()
		data, _ := os.ReadFile(path)
		var c Config
		if err := toml.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		return c.Admin.Token
	}

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	p := payloadFromView(t, view)
	p.Admin = adminAuthPayload{Action: "set", Token: "new-secret"}
	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("set failed: %s", res.Body.String())
	}
	if got := readToken(t); got != "new-secret" {
		t.Fatalf("set must store the new password, got %q", got)
	}
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "new-secret"); w.Code != http.StatusOK {
		t.Fatalf("the new password must authenticate, got %d", w.Code)
	}
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "s3cret"); w.Code != http.StatusUnauthorized {
		t.Fatalf("the old password must stop working, got %d", w.Code)
	}

	// clear disables the page: requireAdmin then 404s.
	p = payloadFromView(t, view)
	p.Admin = adminAuthPayload{Action: "clear"}
	if res := postConfig(t, p, "new-secret"); res.Code != http.StatusOK {
		t.Fatalf("clear failed: %s", res.Body.String())
	}
	if got := readToken(t); got != "" {
		t.Fatalf("clear must empty the password, got %q", got)
	}
	if w := adminCall(t, requireAdmin(handleAdminPage), http.MethodGet, "/admin", "", "new-secret"); w.Code != http.StatusNotFound {
		t.Fatalf("a cleared password must close the page with 404, got %d", w.Code)
	}
}

// Changing the password invalidates the credentials the browser is still
// holding, so the page has to be told to re-authenticate. A password supplied by
// the environment cannot change from the page, so it needs no re-auth.
func TestAdminConfigSaveReportsReauthRequired(t *testing.T) {
	withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	var out struct {
		ReauthRequired bool     `json:"reauth_required"`
		Warnings       []string `json:"warnings"`
	}

	res := postConfig(t, payloadFromView(t, view), "s3cret")
	decodeBody(t, res, &out)
	if out.ReauthRequired {
		t.Fatal("a save that keeps the password must not ask for re-auth")
	}

	p := payloadFromView(t, view)
	p.Admin = adminAuthPayload{Action: "set", Token: "rotated"}
	decodeBody(t, postConfig(t, p, "s3cret"), &out)
	if !out.ReauthRequired {
		t.Fatal("changing the password must ask the browser to re-authenticate")
	}

	// With the env var in charge, a file edit has no effect, so no re-auth.
	view = adminConfigView{}
	w = adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	decodeBody(t, w, &view)
	t.Setenv("LLM_PROXY_ADMIN_TOKEN", "env-token")
	p = payloadFromView(t, view)
	p.Admin = adminAuthPayload{Action: "set", Token: "ignored"}
	decodeBody(t, postConfig(t, p, "env-token"), &out)
	if out.ReauthRequired {
		t.Fatal("a password supplied by the environment cannot change, so no re-auth is needed")
	}
	if !strings.Contains(strings.Join(out.Warnings, " | "), "LLM_PROXY_ADMIN_TOKEN") {
		t.Fatalf("the env override must be reported, got %v", out.Warnings)
	}
}

func TestAdminConfigRejectsEmptyAdminTokenSet(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)
	before, _ := os.ReadFile(path)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	for _, action := range []string{"set", "wipe"} {
		p := payloadFromView(t, view)
		p.Admin = adminAuthPayload{Action: action, Token: ""}
		res := postConfig(t, p, "s3cret")
		if res.Code != http.StatusBadRequest {
			t.Fatalf("action %q must be rejected, got %d %s", action, res.Code, res.Body.String())
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("a rejected save must leave the config file untouched")
	}
}

func TestAdminConfigSaveWarnsAboutPortChange(t *testing.T) {
	withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	p := payloadFromView(t, view)
	p.Proxy.Port = 9099
	// Drop a builtin alias to check the fallback is reported.
	kept := p.Routing[:0]
	for _, e := range p.Routing {
		if e.Alias != "opus" {
			kept = append(kept, e)
		}
	}
	p.Routing = kept

	res := postConfig(t, p, "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("save failed: %s", res.Body.String())
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	decodeBody(t, res, &out)

	joined := strings.Join(out.Warnings, " | ")
	if !strings.Contains(joined, "重启") {
		t.Fatalf("a port change must warn that a restart is required, got %q", joined)
	}
	if !strings.Contains(joined, "opus") {
		t.Fatalf("an undeclared builtin alias must be flagged, got %q", joined)
	}
}

// Anything the page does not model is dropped by the rewrite, so the operator has
// to be told.
func TestAdminConfigSaveWarnsAboutDroppedUnknownKeys(t *testing.T) {
	withTempConfig(t, testConfigTOML+"\n[experimental]\nflag = true\n")

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	res := postConfig(t, payloadFromView(t, view), "s3cret")
	if res.Code != http.StatusOK {
		t.Fatalf("save failed: %s", res.Body.String())
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	decodeBody(t, res, &out)

	if !strings.Contains(strings.Join(out.Warnings, " | "), "experimental") {
		t.Fatalf("dropping an unmodelled section must be reported, got %v", out.Warnings)
	}
}

func TestAdminStatsEndpoint(t *testing.T) {
	withTempConfig(t, testConfigTOML)
	stats.reset()
	t.Cleanup(stats.reset)

	stats.beginReq("sonnet", "DeepSeek-Flash", "anthropic").success(tokenUsage{Input: 40, Output: 10, Reported: true})
	stats.beginReq("sonnet", "DeepSeek-Flash", "anthropic").failure(catUpstream5xx, 503, "boom")

	w := adminCall(t, handleAdminStats, http.MethodGet, "/admin/api/stats", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var snap statsSnapshot
	decodeBody(t, w, &snap)

	if snap.Version != version {
		t.Fatalf("the page needs the version, got %q", snap.Version)
	}
	if snap.Totals.Requests != 2 || snap.Totals.Failures != 1 {
		t.Fatalf("totals: %+v", snap.Totals)
	}
	var found bool
	for _, m := range snap.Models {
		if m.Model != "DeepSeek-Flash" {
			continue
		}
		found = true
		if m.TPM != 50 || m.Failures != 1 || m.ErrorCounts[catUpstream5xx] != 1 {
			t.Fatalf("model snapshot: %+v", m)
		}
	}
	if !found {
		t.Fatal("the recorded model must appear in the snapshot")
	}
}

func TestAdminStatsResetEndpoint(t *testing.T) {
	withTempConfig(t, testConfigTOML)
	stats.reset()
	t.Cleanup(stats.reset)

	stats.beginReq("sonnet", "m", "anthropic").success(tokenUsage{Reported: true})

	w := adminCall(t, handleAdminStatsReset, http.MethodPost, "/admin/api/stats/reset", "", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := stats.snapshot().Totals.Requests; got != 0 {
		t.Fatalf("reset must clear the collector, got %d", got)
	}
}

func TestAdminMethodsAreRestricted(t *testing.T) {
	withTempConfig(t, testConfigTOML)

	if w := adminCall(t, handleAdminConfig, http.MethodDelete, "/admin/api/config", "", "s3cret"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("config delete must be rejected, got %d", w.Code)
	}
	if w := adminCall(t, handleAdminStats, http.MethodPost, "/admin/api/stats", "", "s3cret"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("stats post must be rejected, got %d", w.Code)
	}
	if w := adminCall(t, handleAdminStatsReset, http.MethodGet, "/admin/api/stats/reset", "", "s3cret"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("reset get must be rejected, got %d", w.Code)
	}
}

// The generated TOML must survive a round trip through the parser the proxy uses
// at startup, including aliases that need quoting.
func TestRenderConfigTOMLRoundTrip(t *testing.T) {
	p := adminConfigPayload{
		Proxy: adminProxyView{Port: 8088, VLMModel: "MiniMax-M3", VLMMaxTokens: 8000},
		Upstream: adminUpstreamView{
			AnthropicURL:         "https://anthropic.example/api",
			OpenAIURL:            "https://openai.example/v1",
			DefaultUpstream:      "openai",
			HeaderTimeoutSeconds: 120,
			BodyIdleSeconds:      90,
			MaxRetries:           2,
		},
		Routing: []adminRoutingEntry{
			{Alias: "sonnet", Model: "DeepSeek-Flash", SupportsImage: true},
			{Alias: "opus", Model: "GLM-5.3", Upstream: "openai"},
			{Alias: "with space", Model: "quoted-alias"},
			{Alias: "plain", Model: "plain-model"},
		},
	}

	rendered := renderConfigTOML(p, "tok", "sk-x")

	var got Config
	if err := toml.Unmarshal([]byte(rendered), &got); err != nil {
		t.Fatalf("rendered config must parse: %v\n%s", err, rendered)
	}
	if got.Keys.Sophnet != "sk-x" || got.Admin.Token != "tok" {
		t.Fatalf("secrets must be carried over: %+v", got)
	}

	routes := buildRouteTargets(got.Routing)
	applyDefaultUpstream(&got, routes)
	if routes["sonnet"].Model != "DeepSeek-Flash" || !routes["sonnet"].SupportsImage {
		t.Fatalf("sonnet route lost its fields: %+v", routes["sonnet"])
	}
	// opus declared no upstream, so default_upstream=openai must fill it in.
	if routes["opus"].Upstream != "openai" {
		t.Fatalf("default_upstream must apply to the round-tripped config: %+v", routes["opus"])
	}
	if routes["with space"].Model != "quoted-alias" {
		t.Fatalf("a quoted alias must round trip: %+v", routes)
	}
	if routes["plain"].Upstream != "openai" {
		t.Fatalf("a plain string route must still pick up the default gateway: %+v", routes["plain"])
	}
}

// A save must not leave the file and the running config disagreeing when the new
// file cannot be loaded.
func TestAdminConfigSaveRollsBackOnReloadFailure(t *testing.T) {
	path := withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminConfig, http.MethodGet, "/admin/api/config", "", "s3cret")
	var view adminConfigView
	decodeBody(t, w, &view)

	// Break the reload by pointing LLM_PROXY_CONFIG at a file that disappears
	// between the write and the reload.
	p := payloadFromView(t, view)
	p.Proxy.VLMModel = "Should-Not-Stick"
	body, _ := json.Marshal(p)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/config", strings.NewReader(string(body)))
	req.SetBasicAuth("admin", "s3cret")
	rec := httptest.NewRecorder()

	t.Setenv("LLM_PROXY_CONFIG", filepath.Join(t.TempDir(), "missing", "config.toml"))
	handleAdminConfig(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("an unloadable save must fail, got %d %s", rec.Code, rec.Body.String())
	}
	if got := currentConfig().Proxy.VLMModel; got == "Should-Not-Stick" {
		t.Fatal("a failed reload must not change the running config")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the original config file must still exist: %v", err)
	}
}

// --- history endpoint ---

// The chart asks for one window at a time; an unrecognised range must still get
// a usable answer rather than an error, because the value only ever comes from
// the page's own buttons.
func TestAdminHistoryDefaultsTo24hAndEchoesRange(t *testing.T) {
	withTempConfig(t, testConfigTOML)

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
	withTempConfig(t, testConfigTOML)

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
	withTempConfig(t, testConfigTOML)

	w := adminCall(t, handleAdminStatsHistory, http.MethodPost, "/admin/api/stats/history", "", "s3cret")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

// The endpoint is as closed as the rest of /admin: no token configured means the
// whole subtree is off, and a wrong password gets 401.
func TestAdminHistoryIsBehindAuth(t *testing.T) {
	withTempConfig(t, strings.Replace(testConfigTOML, `token = "s3cret"`, `token = ""`, 1))
	if w := adminCall(t, requireAdmin(handleAdminStatsHistory), http.MethodGet, "/admin/api/stats/history", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("without a token the route must 404, got %d", w.Code)
	}

	withTempConfig(t, testConfigTOML)
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
	withTempConfig(t, testConfigTOML)

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

package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// registerAdmin wires the admin page onto the proxy's own listener. Every route
// is behind requireAdmin, and an unset token disables the subtree entirely.
func registerAdmin() {
	http.HandleFunc("/admin", requireAdmin(handleAdminPage))
	http.HandleFunc("/admin/", requireAdmin(handleAdminPage))
	http.HandleFunc("/admin/api/config", requireAdmin(handleAdminConfig))
	http.HandleFunc("/admin/api/stats", requireAdmin(handleAdminStats))
	http.HandleFunc("/admin/api/stats/history", requireAdmin(handleAdminStatsHistory))
	http.HandleFunc("/admin/api/stats/reset", requireAdmin(handleAdminStatsReset))
}

// requireAdmin enforces HTTP Basic auth. The username is ignored; the password
// is the configured admin token. An unset token returns 404 rather than 401 so
// the endpoint's existence is not advertised.
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := adminToken()
		if token == "" {
			http.NotFound(w, r)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="llm-proxy admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	snap := stats.snapshot()
	snap.Version = version
	writeJSON(w, http.StatusOK, snap)
}

// historyMaxFilter bounds how many ?model= values the history endpoint will act
// on. The page only ever sends the handful of models it draws, so anything past
// this is a caller asking the server to do unbounded work.
const historyMaxFilter = 32

// handleAdminStatsHistory serves the trend chart behind the admin page's
// historical-rate view.
func handleAdminStatsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Repeated ?model= params narrow the response to the series the page draws;
	// anything else is folded away server-side so the payload stays bounded.
	models := r.URL.Query()["model"]
	if len(models) > historyMaxFilter {
		models = models[:historyMaxFilter]
	}
	writeJSON(w, http.StatusOK, stats.history(historyWindowSeconds(r.URL.Query().Get("range")), models))
}

func handleAdminStatsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stats.reset()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, currentConfigView())
	case http.MethodPost:
		handleAdminConfigSave(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type adminProxyView struct {
	Port         int    `json:"port"`
	VLMModel     string `json:"vlm_model"`
	VLMMaxTokens int    `json:"vlm_max_tokens"`
	// VLMUpstream / ChatUpstream name the profile the builtin image-description
	// calls and the /v1/chat/completions passthrough go through. "" means the
	// implicit "anthropic" / "openai" profile, which is what a config predating
	// profiles resolves to.
	VLMUpstream  string `json:"vlm_upstream"`
	ChatUpstream string `json:"chat_upstream"`
}

type adminUpstreamView struct {
	AnthropicURL         string `json:"anthropic_url"`
	OpenAIURL            string `json:"openai_url"`
	DefaultUpstream      string `json:"default_upstream"`
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
	BodyIdleSeconds      int    `json:"body_idle_seconds"`
	MaxRetries           int    `json:"max_retries"`
}

// adminUpstreamProfileView is one upstream profile as the page reads it. The key
// is never returned — only whether one is stored and whether the environment
// supplies it, the same redaction [keys].sophnet gets. key_env, by contrast, is
// returned verbatim: it names an environment variable, it is not a secret, and
// the operator has to see which one is in play.
//
// Builtin marks a profile the file does not declare: the two synthesised from
// [upstream].anthropic_url / openai_url. The page renders those read-only, and
// writing an [upstreams.<name>] table with the same name overrides one.
type adminUpstreamProfileView struct {
	Name                 string `json:"name"`
	Protocol             string `json:"protocol"`
	URL                  string `json:"url"`
	KeySet               bool   `json:"key_set"`
	KeyFromEnv           bool   `json:"key_from_env"`
	KeyEnv               string `json:"key_env"`
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
	BodyIdleSeconds      int    `json:"body_idle_seconds"`
	MaxRetries           int    `json:"max_retries"`
	Builtin              bool   `json:"builtin"`
}

type adminKeysView struct {
	SophnetSet     bool `json:"sophnet_set"`
	SophnetFromEnv bool `json:"sophnet_from_env"`
}

// adminAuthView reports the state of the page's own password. The password is
// never returned, only whether one is configured and which source wins.
type adminAuthView struct {
	// TokenSet is false when no password is configured, in which case the whole
	// /admin subtree is closed.
	TokenSet bool `json:"token_set"`
	// TokenFromEnv is true when LLM_PROXY_ADMIN_TOKEN supplies the effective
	// password, so a value saved into the file would not take effect.
	TokenFromEnv bool `json:"token_from_env"`
}

type adminRoutingEntry struct {
	Alias         string `json:"alias"`
	Model         string `json:"model"`
	Upstream      string `json:"upstream"`
	SupportsImage bool   `json:"supports_image"`
}

// adminEffectiveRoute is the gateway and model an alias actually resolves to
// after the default gateway and the builtin fallbacks are applied. It is shown
// read-only: the form edits what the file declares, so a value inherited from
// default_upstream is not frozen into the file by a save.
type adminEffectiveRoute struct {
	Model    string `json:"model"`
	Upstream string `json:"upstream"`
	Declared bool   `json:"declared"`
}

type adminConfigView struct {
	ConfigPath string            `json:"config_path"`
	Admin      adminAuthView     `json:"admin"`
	Proxy      adminProxyView    `json:"proxy"`
	Upstream   adminUpstreamView `json:"upstream"`
	// Upstreams lists every profile the page may select: the declared
	// [upstreams.*] tables plus the two builtin ones, each flagged so the page can
	// render the builtin entries read-only.
	Upstreams []adminUpstreamProfileView `json:"upstreams"`
	Keys      adminKeysView              `json:"keys"`
	Routing   []adminRoutingEntry        `json:"routing"`
	// EffectiveRouting covers every alias the proxy will serve, including the
	// builtin fallbacks that are not written in the file.
	EffectiveRouting map[string]adminEffectiveRoute `json:"effective_routing"`
}

type adminKeysPayload struct {
	// Action is "keep" (default), "set" or "clear". It makes the intent explicit,
	// so an empty password field never silently wipes a working key.
	Action  string `json:"sophnet_action"`
	Sophnet string `json:"sophnet"`
}

type adminAuthPayload struct {
	// Action is "keep" (default), "set" or "clear", with the same meaning as for
	// the upstream key.
	Action string `json:"token_action"`
	Token  string `json:"token"`
}

// adminUpstreamProfilePayload is one upstream profile as the form submits it: the
// whole profile, not a diff. KeyAction is the same keep/set/clear three-state
// choice the [keys] section uses, with a difference at the edges — a profile the
// page just added has no stored key to keep, so the page submits "set" with the
// typed key, or "clear" when it has none.
type adminUpstreamProfilePayload struct {
	Name                 string `json:"name"`
	Protocol             string `json:"protocol"`
	URL                  string `json:"url"`
	KeyAction            string `json:"key_action"`
	Key                  string `json:"key"`
	KeyEnv               string `json:"key_env"`
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
	BodyIdleSeconds      int    `json:"body_idle_seconds"`
	MaxRetries           int    `json:"max_retries"`
}

type adminConfigPayload struct {
	Proxy    adminProxyView      `json:"proxy"`
	Upstream adminUpstreamView   `json:"upstream"`
	Keys     adminKeysPayload    `json:"keys"`
	Admin    adminAuthPayload    `json:"admin"`
	Routing  []adminRoutingEntry `json:"routing"`
	// Upstreams is the complete profile list the save should write, so a profile
	// the operator deleted is one that is simply absent here. A nil list means the
	// request does not model profiles at all (an older page), and the running
	// config's profiles are carried over instead of being read as a deletion.
	Upstreams []adminUpstreamProfilePayload `json:"upstreams"`
}

// currentConfigView renders the running config for the page. The upstream key is
// never returned — only whether one is configured and where it comes from.
//
// routing lists the aliases as declared in the file, which is what the form
// edits; effective_routing reports where each alias actually goes once the
// default gateway and the builtin fallbacks are applied.
func currentConfigView() adminConfigView {
	c := currentConfig()
	declared := currentDeclaredRoutes()
	resolved := currentRoutes()

	aliases := make([]string, 0, len(declared))
	for alias := range declared {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	entries := make([]adminRoutingEntry, 0, len(aliases))
	for _, alias := range aliases {
		e := declared[alias]
		entries = append(entries, adminRoutingEntry{
			Alias:         alias,
			Model:         e.Model,
			Upstream:      e.Upstream,
			SupportsImage: e.SupportsImage,
		})
	}

	effective := make(map[string]adminEffectiveRoute, len(resolved))
	for alias, e := range resolved {
		_, isDeclared := declared[alias]
		effective[alias] = adminEffectiveRoute{
			Model:    e.Model,
			Upstream: e.Upstream,
			Declared: isDeclared,
		}
	}

	return adminConfigView{
		ConfigPath: configPathFromEnv(),
		Admin: adminAuthView{
			TokenSet:     adminToken() != "",
			TokenFromEnv: os.Getenv("LLM_PROXY_ADMIN_TOKEN") != "",
		},
		Proxy: adminProxyView{
			Port:         c.Proxy.Port,
			VLMModel:     c.Proxy.VLMModel,
			VLMMaxTokens: c.Proxy.VLMMaxTokens,
			VLMUpstream:  c.Proxy.VLMUpstream,
			ChatUpstream: c.Proxy.ChatUpstream,
		},
		Upstream: adminUpstreamView{
			AnthropicURL:         c.Upstream.AnthropicURL,
			OpenAIURL:            c.Upstream.OpenAIURL,
			DefaultUpstream:      c.Upstream.DefaultUpstream,
			HeaderTimeoutSeconds: c.Upstream.HeaderTimeoutSeconds,
			BodyIdleSeconds:      c.Upstream.BodyIdleSeconds,
			MaxRetries:           c.Upstream.MaxRetries,
		},
		Upstreams: profileViews(c),
		Keys: adminKeysView{
			SophnetSet:     c.Keys.Sophnet != "",
			SophnetFromEnv: os.Getenv("SOPHNET_API_KEY") != "",
		},
		Routing:          entries,
		EffectiveRouting: effective,
	}
}

// profileViews lists every profile the page can name: the declared [upstreams.*]
// tables in name order, then the two builtin ones that are not declared. A
// declared [upstreams.anthropic] or [upstreams.openai] replaces the matching
// builtin entry rather than appearing twice, which is exactly the precedence
// profileByName applies.
//
// The builtin entries are read from the profile the request path would actually
// resolve, so the operator sees the URL, key state and timeouts in force rather
// than the raw [upstream] fields behind them. The key is reported only as a
// boolean pair — its value never leaves the process.
func profileViews(c Config) []adminUpstreamProfileView {
	names := make([]string, 0, len(c.Upstreams))
	for name := range c.Upstreams {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]adminUpstreamProfileView, 0, len(names)+2)
	for _, name := range names {
		v := adminProfileView(name, c.Upstreams[name], false)
		// A declared profile carrying a builtin gateway name still falls back to
		// the legacy key, so its key state comes from the resolution rather than
		// from the table alone — otherwise a [upstreams.anthropic] with no key of
		// its own would read as unkeyed while authenticating fine.
		if up, ok := c.profileByName(name); ok {
			v.KeySet = up.Key != ""
		}
		out = append(out, v)
	}
	for _, name := range []string{profileNameAnthropic, profileNameOpenAI} {
		if _, declared := c.Upstreams[name]; declared {
			continue
		}
		up, ok := c.profileByName(name)
		if !ok {
			continue
		}
		out = append(out, adminUpstreamProfileView{
			Name:     up.Name,
			Protocol: up.Protocol,
			URL:      up.URL,
			KeySet:   up.Key != "",
			// The builtin profiles carry no key_env: their key is the legacy
			// SOPHNET_API_KEY-over-[keys].sophnet pair.
			KeyFromEnv:           os.Getenv(legacyKeyEnv) != "",
			HeaderTimeoutSeconds: int(up.HeaderTimeout.Seconds()),
			BodyIdleSeconds:      int(up.BodyIdle.Seconds()),
			MaxRetries:           up.MaxRetries,
			Builtin:              true,
		})
	}
	return out
}

// adminProfileView renders one declared profile. The URL is shown trimmed the way
// the request path uses it, so the page echoes back what the proxy would send.
func adminProfileView(name string, p UpstreamProfile, builtin bool) adminUpstreamProfileView {
	return adminUpstreamProfileView{
		Name:                 name,
		Protocol:             p.Protocol,
		URL:                  strings.TrimSuffix(p.URL, "/"),
		KeySet:               p.Key != "",
		KeyFromEnv:           p.KeyEnv != "" && os.Getenv(p.KeyEnv) != "",
		KeyEnv:               p.KeyEnv,
		HeaderTimeoutSeconds: p.HeaderTimeoutSeconds,
		BodyIdleSeconds:      p.BodyIdleSeconds,
		MaxRetries:           p.MaxRetries,
		Builtin:              builtin,
	}
}

// configSaveMu serializes the whole save transaction — reading the previous
// value, backing up, writing, reloading or rolling back. cfgMu only protects
// publishing, so without this two concurrent saves can interleave and leave the
// file and the running config disagreeing.
var configSaveMu sync.Mutex

func handleAdminConfigSave(w http.ResponseWriter, r *http.Request) {
	var payload adminConfigPayload
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	// The running config is read before validation: the save is checked against
	// the profiles that exist, and a profile key the form asked to keep is carried
	// over from it — the page is never sent one.
	oldConfig := currentConfig()
	profiles, err := validateConfigPayload(&payload, oldConfig.Upstreams)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	configSaveMu.Lock()
	defer configSaveMu.Unlock()

	path := configPathFromEnv()

	newKey := oldConfig.Keys.Sophnet
	switch payload.Keys.Action {
	case "set":
		newKey = payload.Keys.Sophnet
	case "clear":
		newKey = ""
	}

	// The admin password is editable from the page. "keep" carries the stored
	// value over verbatim so a save never silently drops it.
	newAdminTok := oldConfig.Admin.Token
	switch payload.Admin.Action {
	case "set":
		newAdminTok = payload.Admin.Token
	case "clear":
		newAdminTok = ""
	}

	// The form edits [upstreams.*] and the two [proxy] selectors now, so what the
	// payload carries is what gets written: profiles is the validated set, and it
	// carries the plaintext key each profile needs — a "keep" action resolved
	// against the running config, which is the only place a stored key exists.
	rendered := renderConfigTOML(payload, newAdminTok, newKey, profiles)

	// Parse before writing: a config file that cannot be read would take the
	// proxy down on its next restart.
	var probe Config
	if err := toml.Unmarshal([]byte(rendered), &probe); err != nil {
		writeJSONError(w, http.StatusBadRequest, "生成的配置无法解析: "+err.Error())
		return
	}

	// Read the keys the rewrite is about to drop before the file is replaced.
	dropped := unknownConfigKeys(path)

	backup, backupPath, err := backupConfigFile(path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "备份配置失败: "+err.Error())
		return
	}
	if err := writeFileAtomic(path, []byte(rendered)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "写入配置失败: "+err.Error())
		return
	}
	if err := loadConfig(); err != nil {
		// The file is on disk but unusable: put the previous one back so the
		// running proxy and the file never disagree. Report each way the rollback
		// itself can fail rather than claiming success unconditionally.
		restoreErr := writeFileAtomic(path, backup)
		var reloadErr error
		if restoreErr == nil {
			reloadErr = loadConfig()
		}
		msg := "配置重载失败: " + err.Error()
		switch {
		case restoreErr != nil:
			msg += "；回滚写入也失败: " + restoreErr.Error() + "（配置文件可能已是新内容，保存前的副本在 " + backupPath + "）"
		case reloadErr != nil:
			msg += "；已写回保存前的配置，但重新加载仍失败: " + reloadErr.Error()
		default:
			msg += "；已回滚到保存前的配置"
		}
		writeJSONError(w, http.StatusInternalServerError, msg)
		return
	}

	warnings := configWarnings(payload, oldConfig, dropped)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"config": currentConfigView(),
		// The browser is still holding the old password, so it has to
		// re-authenticate before the page can talk to the API again. A password
		// supplied by the environment cannot change from here, so it needs no
		// re-auth.
		"reauth_required": newAdminTok != oldConfig.Admin.Token && os.Getenv("LLM_PROXY_ADMIN_TOKEN") == "",
		"warnings":        warnings,
	})
}

func configWarnings(payload adminConfigPayload, old Config, droppedKeys []string) []string {
	var warnings []string
	if payload.Proxy.Port != old.Proxy.Port {
		warnings = append(warnings, fmt.Sprintf("监听端口由 %d 改为 %d：端口变更需重启 llm-proxy 服务才能生效，其余配置已立即生效。",
			old.Proxy.Port, payload.Proxy.Port))
	}
	declared := map[string]bool{}
	for _, e := range payload.Routing {
		declared[e.Alias] = true
	}
	for _, alias := range []string{"sonnet", "opus", "haiku"} {
		if !declared[alias] {
			warnings = append(warnings, fmt.Sprintf("未声明 %s 路由，将回落到内置默认目标。", alias))
		}
	}
	if len(droppedKeys) > 0 {
		warnings = append(warnings, fmt.Sprintf("原配置中的顶层字段 %s 不被管理页面识别，本次保存已丢弃（备份文件仍保留）。",
			strings.Join(droppedKeys, ", ")))
	}
	if os.Getenv("SOPHNET_API_KEY") != "" {
		warnings = append(warnings, "环境变量 SOPHNET_API_KEY 已设置，它的优先级高于配置文件里的密钥。")
	}
	if envAdminTok := os.Getenv("LLM_PROXY_ADMIN_TOKEN"); envAdminTok != "" {
		if payload.Admin.Action == "clear" {
			// An environment password outranks the file, so clearing the file
			// does not close the page here and the warning must not claim it did.
			warnings = append(warnings, "配置文件中的管理口令已清空，但环境变量 LLM_PROXY_ADMIN_TOKEN 仍然生效，管理页面保持开放。")
		} else {
			warnings = append(warnings, "环境变量 LLM_PROXY_ADMIN_TOKEN 已设置，它的优先级高于配置文件里的管理口令；在页面上修改口令不会生效。")
		}
	} else if payload.Admin.Action == "clear" {
		warnings = append(warnings, "管理口令已清空：管理页面现已关闭，所有 /admin* 返回 404。如需重新启用，请在配置文件或 LLM_PROXY_ADMIN_TOKEN 中设置口令。")
	}
	return warnings
}

// maxProfileCount bounds how many [upstreams.*] tables a save may write. The
// form is a hand-edited list and the file is parsed on every reload; a few dozen
// is already far past any real deployment.
const maxProfileCount = 64

// maxProfileKeyLen bounds a profile's plaintext key. A key past this is a
// misplaced blob rather than a credential, and rejecting it here keeps the
// generated TOML to a size the reload can parse quickly.
const maxProfileKeyLen = 8 << 10

// validateConfigPayload rejects a save that would produce a config the proxy
// cannot serve, and returns the [upstreams.*] set the save should write. Nothing
// is written when it fails.
//
// oldProfiles is the running config's profile set. The form submits the whole
// list, so an entry missing from the payload was deleted by the operator — and a
// "keep" key action only has a value to keep for a name that set still carries.
func validateConfigPayload(p *adminConfigPayload, oldProfiles map[string]UpstreamProfile) (map[string]UpstreamProfile, error) {
	if p.Proxy.Port < 1 || p.Proxy.Port > 65535 {
		return nil, fmt.Errorf("端口 %d 超出范围（1-65535）", p.Proxy.Port)
	}
	if p.Proxy.VLMMaxTokens < 0 {
		return nil, fmt.Errorf("vlm_max_tokens 不能为负数")
	}
	if err := validateURL("anthropic_url", p.Upstream.AnthropicURL); err != nil {
		return nil, err
	}
	if err := validateURL("openai_url", p.Upstream.OpenAIURL); err != nil {
		return nil, err
	}
	if p.Upstream.HeaderTimeoutSeconds < 0 || p.Upstream.BodyIdleSeconds < 0 || p.Upstream.MaxRetries < 0 {
		return nil, fmt.Errorf("超时与重试次数不能为负数")
	}

	profiles, err := resolveProfiles(p.Upstreams, oldProfiles)
	if err != nil {
		return nil, err
	}
	if err := rejectRemovedProfiles(p, oldProfiles, profiles); err != nil {
		return nil, err
	}
	known := profileNames(profiles)

	// The three selectors name a profile that has to exist; the two [proxy] ones
	// additionally have to speak the protocol their caller uses, or the request
	// path would fall back to the builtin profile and the operator's choice would
	// appear to save without taking effect.
	if !validProfileSelector(p.Upstream.DefaultUpstream, known) {
		return nil, fmt.Errorf("default_upstream 必须是已定义的上游 profile 名（或 \"\"、\"claude\"、\"anthropic\"、\"openai\"）")
	}
	if err := checkProtocolSelector("vlm_upstream", p.Proxy.VLMUpstream, profiles, protocolAnthropic); err != nil {
		return nil, err
	}
	if err := checkProtocolSelector("chat_upstream", p.Proxy.ChatUpstream, profiles, protocolOpenAI); err != nil {
		return nil, err
	}

	switch p.Keys.Action {
	case "", "keep", "set", "clear":
	default:
		return nil, fmt.Errorf("sophnet_action 只能是 keep、set 或 clear")
	}
	if p.Keys.Action == "set" && p.Keys.Sophnet == "" {
		return nil, fmt.Errorf("sophnet_action 为 set 时密钥不能为空（如需清空请用 clear）")
	}

	switch p.Admin.Action {
	case "", "keep", "set", "clear":
	default:
		return nil, fmt.Errorf("token_action 只能是 keep、set 或 clear")
	}
	if p.Admin.Action == "set" && p.Admin.Token == "" {
		return nil, fmt.Errorf("token_action 为 set 时管理口令不能为空（如需关闭管理页面请用 clear）")
	}
	if hasControlChars(p.Admin.Token) {
		return nil, fmt.Errorf("管理口令含非法控制字符")
	}

	seen := map[string]bool{}
	for _, e := range p.Routing {
		if strings.TrimSpace(e.Alias) == "" {
			return nil, fmt.Errorf("路由别名不能为空")
		}
		if seen[e.Alias] {
			return nil, fmt.Errorf("路由别名 %q 重复", e.Alias)
		}
		seen[e.Alias] = true
		if strings.TrimSpace(e.Model) == "" {
			return nil, fmt.Errorf("路由 %q 的模型名不能为空", e.Alias)
		}
		if !validProfileSelector(e.Upstream, known) {
			return nil, fmt.Errorf("路由 %q 的 upstream 必须是已定义的上游 profile 名（或 \"\"、\"claude\"、\"anthropic\"、\"openai\"）", e.Alias)
		}
		for _, field := range []struct{ name, value string }{
			{"别名", e.Alias}, {"模型名", e.Model},
		} {
			if hasControlChars(field.value) {
				return nil, fmt.Errorf("路由 %q 的%s含非法控制字符", e.Alias, field.name)
			}
		}
	}
	for _, field := range []struct{ name, value string }{
		{"vlm_model", p.Proxy.VLMModel},
		{"anthropic_url", p.Upstream.AnthropicURL},
		{"openai_url", p.Upstream.OpenAIURL},
	} {
		if hasControlChars(field.value) {
			return nil, fmt.Errorf("%s 含非法控制字符", field.name)
		}
	}
	if hasControlChars(p.Keys.Sophnet) {
		return nil, fmt.Errorf("密钥含非法控制字符")
	}
	return profiles, nil
}

// resolveProfiles turns the submitted profile list into the set the save should
// write, rejecting each way a submission can be unusable. The map is keyed by the
// normalised (lowercase) name and carries the plaintext key to write: a "keep"
// action copies it out of the running config, which is the only place the page
// can get it, since a stored key is never sent to the browser.
//
// A nil list means the request does not model profiles at all — an older page
// whose form had no profile editor. Those profiles are inherited from the running
// config rather than read as a deletion, so such a save keeps the file working.
// The page itself always sends the list, so an operator who deletes every row
// sends an empty (non-nil) list and gets exactly that.
//
// A "keep" for a name the running config does not declare is rejected rather than
// read as "no key". It is what an operator gets after adding a row, leaving the
// key blank and saving; writing a keyless profile there would leave the route
// failing at request time with nothing in the file to explain why.
//
// The two builtin names are ordinary names here: [upstreams.anthropic] and
// [upstreams.openai] are exactly how the synthesised profiles are overridden.
func resolveProfiles(submitted []adminUpstreamProfilePayload, old map[string]UpstreamProfile) (map[string]UpstreamProfile, error) {
	if submitted == nil {
		inherited := make(map[string]UpstreamProfile, len(old))
		for name, p := range old {
			inherited[normalizeUpstreamName(name)] = p
		}
		return inherited, nil
	}
	if len(submitted) > maxProfileCount {
		return nil, fmt.Errorf("上游 profile 数量不能超过 %d 个", maxProfileCount)
	}

	out := make(map[string]UpstreamProfile, len(submitted))
	for _, item := range submitted {
		raw := strings.TrimSpace(item.Name)
		name := normalizeUpstreamName(raw)
		if name == "" {
			return nil, fmt.Errorf("上游 profile 名不能为空")
		}
		if len(name) > maxProfileNameLen {
			return nil, fmt.Errorf("上游 profile 名 %q 过长（%d 个字符，最多 %d 个）", raw, len(name), maxProfileNameLen)
		}
		if !validProfileName(name) {
			return nil, fmt.Errorf("上游 profile 名 %q 只能包含小写字母、数字、'-' 和 '_'", raw)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("上游 profile 名 %q 重复（名字大小写不敏感）", name)
		}

		protocol := normalizeUpstreamName(item.Protocol)
		if protocol != protocolAnthropic && protocol != protocolOpenAI {
			return nil, fmt.Errorf("上游 profile %q 的 protocol 只能是 %q 或 %q", name, protocolAnthropic, protocolOpenAI)
		}
		if err := validateURL(fmt.Sprintf("上游 profile %q 的 url", name), item.URL); err != nil {
			return nil, err
		}
		if item.HeaderTimeoutSeconds < 0 || item.BodyIdleSeconds < 0 || item.MaxRetries < 0 {
			return nil, fmt.Errorf("上游 profile %q 的超时与重试次数不能为负数", name)
		}

		keyEnv := strings.TrimSpace(item.KeyEnv)
		if hasControlChars(keyEnv) {
			return nil, fmt.Errorf("上游 profile %q 的 key_env 含非法控制字符", name)
		}

		var key string
		switch item.KeyAction {
		case "", "keep":
			prev, ok := old[name]
			if !ok {
				return nil, fmt.Errorf("上游 profile %q 没有已保存的密钥可 keep（新增 profile 请选择 set 或 clear）", name)
			}
			key = prev.Key
		case "set":
			if item.Key == "" {
				return nil, fmt.Errorf("上游 profile %q 选择 set 时密钥不能为空（如需清空请用 clear）", name)
			}
			key = item.Key
		case "clear":
			key = ""
		default:
			return nil, fmt.Errorf("上游 profile %q 的 key_action 只能是 keep、set 或 clear", name)
		}
		if len(key) > maxProfileKeyLen {
			return nil, fmt.Errorf("上游 profile %q 的密钥过长（最多 %d 个字符）", name, maxProfileKeyLen)
		}
		if hasControlChars(key) {
			return nil, fmt.Errorf("上游 profile %q 的密钥含非法控制字符", name)
		}

		out[name] = UpstreamProfile{
			Protocol:             protocol,
			URL:                  strings.TrimSpace(item.URL),
			Key:                  key,
			KeyEnv:               keyEnv,
			HeaderTimeoutSeconds: item.HeaderTimeoutSeconds,
			BodyIdleSeconds:      item.BodyIdleSeconds,
			MaxRetries:           item.MaxRetries,
		}
	}
	return out, nil
}

// rejectRemovedProfiles refuses a save that drops a profile something still
// points at, naming each referrer. The config load tolerates a route to a missing
// profile by skipping it, but a save happens while the operator is looking at the
// page, so this is the moment to tell them what has to change first.
func rejectRemovedProfiles(p *adminConfigPayload, old, next map[string]UpstreamProfile) error {
	var removed []string
	for rawName := range old {
		name := normalizeUpstreamName(rawName)
		if _, kept := next[name]; !kept {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)

	for _, name := range removed {
		var refs []string
		if selectorRefersTo(p.Upstream.DefaultUpstream, name) {
			refs = append(refs, "default_upstream")
		}
		if selectorRefersTo(p.Proxy.VLMUpstream, name) {
			refs = append(refs, "vlm_upstream")
		}
		if selectorRefersTo(p.Proxy.ChatUpstream, name) {
			refs = append(refs, "chat_upstream")
		}
		for _, e := range p.Routing {
			if selectorRefersTo(e.Upstream, name) {
				refs = append(refs, "路由 "+e.Alias)
			}
		}
		if len(refs) > 0 {
			return fmt.Errorf("上游 profile %q 仍被引用，无法删除：%s", name, strings.Join(refs, "、"))
		}
	}
	return nil
}

// selectorRefersTo reports whether a route's upstream field or a [proxy] selector
// resolves to the named profile. "claude" is the long-standing alias for the
// anthropic profile, so it counts as a reference to it.
func selectorRefersTo(value, profile string) bool {
	name := normalizeUpstreamName(value)
	if name == "claude" {
		name = profileNameAnthropic
	}
	return name != "" && name == profile
}

// profileNames is the set of names a selector may use: the profiles this save
// writes plus the two builtin names, which every config can always reach.
func profileNames(profiles map[string]UpstreamProfile) map[string]bool {
	names := map[string]bool{
		profileNameAnthropic: true,
		profileNameOpenAI:    true,
	}
	for name := range profiles {
		names[normalizeUpstreamName(name)] = true
	}
	return names
}

// checkProtocolSelector validates one [proxy] profile selector, which unlike a
// route's upstream has to match the protocol its caller speaks: the VLM describe
// pass only knows the anthropic framing and the chat passthrough only the openai
// one, so accepting the other protocol here would save cleanly and then be folded
// back to the builtin profile at load.
func checkProtocolSelector(field, value string, profiles map[string]UpstreamProfile, want string) error {
	name := normalizeUpstreamName(value)
	if name == "" {
		return nil
	}
	if name == "claude" {
		name = profileNameAnthropic
	}
	got, ok := selectorProtocol(name, profiles)
	if !ok {
		return fmt.Errorf("%s 必须是已定义的上游 profile 名（或 \"\"、\"claude\"、\"anthropic\"、\"openai\"）", field)
	}
	if got != want {
		return fmt.Errorf("%s 指向的 profile %q 是 %s 协议，必须是 %s 协议", field, value, got, want)
	}
	return nil
}

// selectorProtocol answers the protocol the profile a selector names speaks. A
// declared profile is asked directly, so a selector can point at a
// [upstreams.anthropic] the operator re-pointed at the openai protocol and be
// told about it. The two builtin names answer with the framing they imply.
func selectorProtocol(name string, profiles map[string]UpstreamProfile) (string, bool) {
	if p, ok := profiles[name]; ok {
		return p.Protocol, true
	}
	switch name {
	case profileNameAnthropic:
		return protocolAnthropic, true
	case profileNameOpenAI:
		return protocolOpenAI, true
	}
	return "", false
}

// validUpstreamName reports whether s is one of the gateway names the proxy has
// always understood, ignoring case. It is the fallback half of
// validProfileSelector and is kept separate so the meaning of the two builtin
// names stays legible.
func validUpstreamName(s string) bool {
	switch normalizeUpstreamName(s) {
	case "", "claude", profileNameAnthropic, profileNameOpenAI:
		return true
	}
	return false
}

// validProfileSelector reports whether s may be written into a route's upstream
// field or into default_upstream. An empty value, the builtin gateway names, and
// any profile name this save writes are accepted; anything else would name a
// profile the config cannot resolve, which the loader would silently skip — so
// the save is rejected instead, at the one point where the operator can still fix
// it.
func validProfileSelector(s string, profiles map[string]bool) bool {
	if validUpstreamName(s) {
		return true
	}
	return profiles[normalizeUpstreamName(s)]
}

func validateURL(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s 不能为空", field)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s 不是合法 URL: %v", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s 必须以 http:// 或 https:// 开头", field)
	}
	if u.Host == "" {
		return fmt.Errorf("%s 缺少主机名", field)
	}
	return nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

var bareTOMLKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(s string) string {
	if bareTOMLKey.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

func tomlString(s string) string { return strconv.Quote(s) }

// renderConfigTOML writes the config file the admin page produces. Hand-written
// comments in the previous file are not preserved; the backup taken before each
// save is the recovery path for anything the rewrite drops.
//
// profiles is the validated [upstreams.*] set the payload asked for, already
// resolved by validateConfigPayload: it carries the plaintext key for each
// profile, which the page itself never sees.
func renderConfigTOML(p adminConfigPayload, adminTok, sophnetKey string, profiles map[string]UpstreamProfile) string {
	var b strings.Builder

	b.WriteString("# llm-proxy configuration.\n")
	b.WriteString("# Written by the web admin page. The previous file is kept next to this one as\n")
	b.WriteString("# <config>.bak.<timestamp>; hand-written comments are not preserved by a save.\n\n")

	b.WriteString("[proxy]\n")
	fmt.Fprintf(&b, "port = %d\n", p.Proxy.Port)
	b.WriteString("# VLM model used to describe image-carrying requests before they are routed to\n")
	b.WriteString("# the text model. Routes marked supports_image = true skip this pass.\n")
	fmt.Fprintf(&b, "vlm_model = %s\n", tomlString(p.Proxy.VLMModel))
	b.WriteString("# Max output tokens for each image-description call.\n")
	fmt.Fprintf(&b, "vlm_max_tokens = %d\n", p.Proxy.VLMMaxTokens)
	writeProxyProfileSelectors(&b, p.Proxy)
	b.WriteString("\n")

	b.WriteString("[upstream]\n")
	fmt.Fprintf(&b, "anthropic_url = %s\n", tomlString(p.Upstream.AnthropicURL))
	fmt.Fprintf(&b, "openai_url = %s\n", tomlString(p.Upstream.OpenAIURL))
	b.WriteString("# Gateway for routing entries that do not name one:\n")
	b.WriteString("#   \"\" / \"claude\" / \"anthropic\" -> Anthropic (claude-format) gateway\n")
	b.WriteString("#   \"openai\"                     -> OpenAI gateway (request translated)\n")
	b.WriteString("#   any [upstreams.<name>]        -> that named profile\n")
	fmt.Fprintf(&b, "default_upstream = %s\n", tomlString(p.Upstream.DefaultUpstream))
	b.WriteString("# Per-attempt wait for upstream response headers, in seconds.\n")
	fmt.Fprintf(&b, "header_timeout_seconds = %d\n", p.Upstream.HeaderTimeoutSeconds)
	b.WriteString("# Longest silence allowed while reading an upstream response body, in seconds.\n")
	fmt.Fprintf(&b, "body_idle_seconds = %d\n", p.Upstream.BodyIdleSeconds)
	b.WriteString("# Extra attempts after a transient upstream error or a 429/5xx.\n")
	fmt.Fprintf(&b, "max_retries = %d\n\n", p.Upstream.MaxRetries)

	writeUpstreamProfiles(&b, profiles)

	b.WriteString("[keys]\n")
	b.WriteString("# Alternative: export SOPHNET_API_KEY=... and leave this empty.\n")
	fmt.Fprintf(&b, "sophnet = %s\n\n", tomlString(sophnetKey))

	b.WriteString("[admin]\n")
	b.WriteString("# Web admin page password (HTTP Basic, any username). Empty disables the page\n")
	b.WriteString("# entirely. LLM_PROXY_ADMIN_TOKEN overrides this value.\n")
	fmt.Fprintf(&b, "token = %s\n\n", tomlString(adminTok))

	b.WriteString("[routing]\n")
	b.WriteString("# Builtin aliases (sonnet/opus/haiku) and custom aliases alike. A plain string\n")
	b.WriteString("# uses default_upstream; a table may pick a profile and declare image support.\n")
	for _, e := range p.Routing {
		key := tomlKey(e.Alias)
		if e.Upstream == "" && !e.SupportsImage {
			fmt.Fprintf(&b, "%s = %s\n", key, tomlString(e.Model))
			continue
		}
		parts := []string{fmt.Sprintf("model = %s", tomlString(e.Model))}
		if e.Upstream != "" {
			parts = append(parts, fmt.Sprintf("upstream = %s", tomlString(e.Upstream)))
		}
		if e.SupportsImage {
			parts = append(parts, "supports_image = true")
		}
		fmt.Fprintf(&b, "%s = { %s }\n", key, strings.Join(parts, ", "))
	}
	return b.String()
}

// writeProxyProfileSelectors writes the two [proxy] selectors that name an
// upstream profile. An empty selector is omitted, so a config that never used
// profiles keeps the file it had and the loader's builtin default keeps applying.
func writeProxyProfileSelectors(b *strings.Builder, proxy adminProxyView) {
	if proxy.VLMUpstream != "" {
		b.WriteString("# Upstream profile the builtin image-description calls go through (must speak\n")
		b.WriteString("# the anthropic protocol). Empty means the implicit \"anthropic\" profile.\n")
		fmt.Fprintf(b, "vlm_upstream = %s\n", tomlString(proxy.VLMUpstream))
	}
	if proxy.ChatUpstream != "" {
		b.WriteString("# Upstream profile /v1/chat/completions forwards to (must speak the openai\n")
		b.WriteString("# protocol). Empty means the implicit \"openai\" profile.\n")
		fmt.Fprintf(b, "chat_upstream = %s\n", tomlString(proxy.ChatUpstream))
	}
}

// writeUpstreamProfiles writes the [upstreams.<name>] tables. Names are emitted in
// sorted order so a save is reproducible, and the plaintext key is written whenever
// the profile has one — the page never receives it, so it reaches this function
// only from the running config, via the keep action validateConfigPayload resolved.
func writeUpstreamProfiles(b *strings.Builder, profiles map[string]UpstreamProfile) {
	if len(profiles) == 0 {
		return
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		p := profiles[name]
		fmt.Fprintf(b, "[upstreams.%s]\n", tomlKey(name))
		fmt.Fprintf(b, "protocol = %s\n", tomlString(p.Protocol))
		fmt.Fprintf(b, "url = %s\n", tomlString(p.URL))
		fmt.Fprintf(b, "key = %s\n", tomlString(p.Key))
		if p.KeyEnv != "" {
			fmt.Fprintf(b, "key_env = %s\n", tomlString(p.KeyEnv))
		}
		if p.HeaderTimeoutSeconds > 0 {
			fmt.Fprintf(b, "header_timeout_seconds = %d\n", p.HeaderTimeoutSeconds)
		}
		if p.BodyIdleSeconds > 0 {
			fmt.Fprintf(b, "body_idle_seconds = %d\n", p.BodyIdleSeconds)
		}
		if p.MaxRetries > 0 {
			fmt.Fprintf(b, "max_retries = %d\n", p.MaxRetries)
		}
		b.WriteString("\n")
	}
}

// backupConfigFile copies the current config aside and returns its contents plus
// the path it was written to, so a failed reload can be rolled back without
// re-reading a file we just replaced.
//
// The name is timestamped to the second, which two saves in the same second
// would collide on; O_EXCL plus a numeric suffix guarantees the first save's
// copy of the original file is never overwritten.
func backupConfigFile(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}

	stamp := time.Now().Format("20060102-150405")
	for attempt := 0; attempt < 100; attempt++ {
		backupPath := fmt.Sprintf("%s.bak.%s", path, stamp)
		if attempt > 0 {
			backupPath = fmt.Sprintf("%s.bak.%s-%d", path, stamp, attempt)
		}
		f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return nil, "", err
		}
		if err := f.Close(); err != nil {
			return nil, "", err
		}
		return data, backupPath, nil
	}
	return nil, "", fmt.Errorf("同秒内备份次数过多，无法生成唯一备份文件名")
}

// writeFileAtomic replaces path via a temporary file in the same directory, so a
// crash mid-write cannot leave a truncated config behind.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".llm-proxy-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// unknownConfigKeys reports top-level config keys the admin page does not model,
// so a save can warn that the rewrite would drop them.
func unknownConfigKeys(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw map[string]interface{}
	if toml.Unmarshal(data, &raw) != nil {
		return nil
	}
	known := map[string]bool{"proxy": true, "upstream": true, "upstreams": true, "keys": true, "admin": true, "routing": true}
	var unknown []string
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{"ok": false, "error": message})
}

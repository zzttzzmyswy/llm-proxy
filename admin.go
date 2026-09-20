package main

import (
	"crypto/subtle"
	_ "embed"
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

//go:embed admin.html
var adminHTML []byte

// registerAdmin wires the admin page onto the proxy's own listener. Every route
// is behind requireAdmin, and an unset token disables the subtree entirely.
func registerAdmin() {
	http.HandleFunc("/admin", requireAdmin(handleAdminPage))
	http.HandleFunc("/admin/", requireAdmin(handleAdminPage))
	http.HandleFunc("/admin/api/config", requireAdmin(handleAdminConfig))
	http.HandleFunc("/admin/api/stats", requireAdmin(handleAdminStats))
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

func handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(adminHTML)
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
}

type adminUpstreamView struct {
	AnthropicURL         string `json:"anthropic_url"`
	OpenAIURL            string `json:"openai_url"`
	DefaultUpstream      string `json:"default_upstream"`
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
	BodyIdleSeconds      int    `json:"body_idle_seconds"`
	MaxRetries           int    `json:"max_retries"`
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
	ConfigPath string              `json:"config_path"`
	Admin      adminAuthView       `json:"admin"`
	Proxy      adminProxyView      `json:"proxy"`
	Upstream   adminUpstreamView   `json:"upstream"`
	Keys       adminKeysView       `json:"keys"`
	Routing    []adminRoutingEntry `json:"routing"`
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

type adminConfigPayload struct {
	Proxy    adminProxyView      `json:"proxy"`
	Upstream adminUpstreamView   `json:"upstream"`
	Keys     adminKeysPayload    `json:"keys"`
	Admin    adminAuthPayload    `json:"admin"`
	Routing  []adminRoutingEntry `json:"routing"`
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
		},
		Upstream: adminUpstreamView{
			AnthropicURL:         c.Upstream.AnthropicURL,
			OpenAIURL:            c.Upstream.OpenAIURL,
			DefaultUpstream:      c.Upstream.DefaultUpstream,
			HeaderTimeoutSeconds: c.Upstream.HeaderTimeoutSeconds,
			BodyIdleSeconds:      c.Upstream.BodyIdleSeconds,
			MaxRetries:           c.Upstream.MaxRetries,
		},
		Keys: adminKeysView{
			SophnetSet:     c.Keys.Sophnet != "",
			SophnetFromEnv: os.Getenv("SOPHNET_API_KEY") != "",
		},
		Routing:          entries,
		EffectiveRouting: effective,
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
	if err := validateConfigPayload(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	configSaveMu.Lock()
	defer configSaveMu.Unlock()

	path := configPathFromEnv()
	oldConfig := currentConfig()

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

	rendered := renderConfigTOML(payload, newAdminTok, newKey)

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

// validateConfigPayload rejects a save that would produce a config the proxy
// cannot serve. Nothing is written when it fails.
func validateConfigPayload(p *adminConfigPayload) error {
	if p.Proxy.Port < 1 || p.Proxy.Port > 65535 {
		return fmt.Errorf("端口 %d 超出范围（1-65535）", p.Proxy.Port)
	}
	if p.Proxy.VLMMaxTokens < 0 {
		return fmt.Errorf("vlm_max_tokens 不能为负数")
	}
	if err := validateURL("anthropic_url", p.Upstream.AnthropicURL); err != nil {
		return err
	}
	if err := validateURL("openai_url", p.Upstream.OpenAIURL); err != nil {
		return err
	}
	if !validUpstreamName(p.Upstream.DefaultUpstream) {
		return fmt.Errorf("default_upstream 只能是 \"\"、\"claude\"、\"anthropic\" 或 \"openai\"")
	}
	if p.Upstream.HeaderTimeoutSeconds < 0 || p.Upstream.BodyIdleSeconds < 0 || p.Upstream.MaxRetries < 0 {
		return fmt.Errorf("超时与重试次数不能为负数")
	}
	switch p.Keys.Action {
	case "", "keep", "set", "clear":
	default:
		return fmt.Errorf("sophnet_action 只能是 keep、set 或 clear")
	}
	if p.Keys.Action == "set" && p.Keys.Sophnet == "" {
		return fmt.Errorf("sophnet_action 为 set 时密钥不能为空（如需清空请用 clear）")
	}

	switch p.Admin.Action {
	case "", "keep", "set", "clear":
	default:
		return fmt.Errorf("token_action 只能是 keep、set 或 clear")
	}
	if p.Admin.Action == "set" && p.Admin.Token == "" {
		return fmt.Errorf("token_action 为 set 时管理口令不能为空（如需关闭管理页面请用 clear）")
	}
	if hasControlChars(p.Admin.Token) {
		return fmt.Errorf("管理口令含非法控制字符")
	}

	seen := map[string]bool{}
	for _, e := range p.Routing {
		if strings.TrimSpace(e.Alias) == "" {
			return fmt.Errorf("路由别名不能为空")
		}
		if seen[e.Alias] {
			return fmt.Errorf("路由别名 %q 重复", e.Alias)
		}
		seen[e.Alias] = true
		if strings.TrimSpace(e.Model) == "" {
			return fmt.Errorf("路由 %q 的模型名不能为空", e.Alias)
		}
		if !validUpstreamName(e.Upstream) {
			return fmt.Errorf("路由 %q 的 upstream 只能是 \"\"、\"claude\"、\"anthropic\" 或 \"openai\"", e.Alias)
		}
		for _, field := range []struct{ name, value string }{
			{"别名", e.Alias}, {"模型名", e.Model},
		} {
			if hasControlChars(field.value) {
				return fmt.Errorf("路由 %q 的%s含非法控制字符", e.Alias, field.name)
			}
		}
	}
	for _, field := range []struct{ name, value string }{
		{"vlm_model", p.Proxy.VLMModel},
		{"anthropic_url", p.Upstream.AnthropicURL},
		{"openai_url", p.Upstream.OpenAIURL},
	} {
		if hasControlChars(field.value) {
			return fmt.Errorf("%s 含非法控制字符", field.name)
		}
	}
	if hasControlChars(p.Keys.Sophnet) {
		return fmt.Errorf("密钥含非法控制字符")
	}
	return nil
}

func validUpstreamName(s string) bool {
	switch strings.ToLower(s) {
	case "", "claude", "anthropic", "openai":
		return true
	}
	return false
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
func renderConfigTOML(p adminConfigPayload, adminTok, sophnetKey string) string {
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
	fmt.Fprintf(&b, "vlm_max_tokens = %d\n\n", p.Proxy.VLMMaxTokens)

	b.WriteString("[upstream]\n")
	fmt.Fprintf(&b, "anthropic_url = %s\n", tomlString(p.Upstream.AnthropicURL))
	fmt.Fprintf(&b, "openai_url = %s\n", tomlString(p.Upstream.OpenAIURL))
	b.WriteString("# Gateway for routing entries that do not name one:\n")
	b.WriteString("#   \"\" / \"claude\" / \"anthropic\" -> Anthropic (claude-format) gateway\n")
	b.WriteString("#   \"openai\"                     -> OpenAI gateway (request translated)\n")
	fmt.Fprintf(&b, "default_upstream = %s\n", tomlString(p.Upstream.DefaultUpstream))
	b.WriteString("# Per-attempt wait for upstream response headers, in seconds.\n")
	fmt.Fprintf(&b, "header_timeout_seconds = %d\n", p.Upstream.HeaderTimeoutSeconds)
	b.WriteString("# Longest silence allowed while reading an upstream response body, in seconds.\n")
	fmt.Fprintf(&b, "body_idle_seconds = %d\n", p.Upstream.BodyIdleSeconds)
	b.WriteString("# Extra attempts after a transient upstream error or a 429/5xx.\n")
	fmt.Fprintf(&b, "max_retries = %d\n\n", p.Upstream.MaxRetries)

	b.WriteString("[keys]\n")
	b.WriteString("# Alternative: export SOPHNET_API_KEY=... and leave this empty.\n")
	fmt.Fprintf(&b, "sophnet = %s\n\n", tomlString(sophnetKey))

	b.WriteString("[admin]\n")
	b.WriteString("# Web admin page password (HTTP Basic, any username). Empty disables the page\n")
	b.WriteString("# entirely. LLM_PROXY_ADMIN_TOKEN overrides this value.\n")
	fmt.Fprintf(&b, "token = %s\n\n", tomlString(adminTok))

	b.WriteString("[routing]\n")
	b.WriteString("# Builtin aliases (sonnet/opus/haiku) and custom aliases alike. A plain string\n")
	b.WriteString("# uses default_upstream; a table may pick a gateway and declare image support.\n")
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
	known := map[string]bool{"proxy": true, "upstream": true, "keys": true, "admin": true, "routing": true}
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

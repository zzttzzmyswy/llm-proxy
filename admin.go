package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
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

// adminProxyView is the process-level settings the page edits.
type adminProxyView struct {
	Port int `json:"port"`
	// Scheme names the active scheme file, and SchemeList is every scheme file the
	// schemes directory holds, so the page offers exactly the choices that exist
	// rather than a free-text field that fails on save.
	Scheme     string   `json:"scheme"`
	SchemesDir string   `json:"schemes_dir"`
	SchemeList []string `json:"scheme_list"`
}

// adminUpstreamView is one upstream as the page shows it: where it points, how it
// authenticates, and how long it waits. The key is never returned — only whether
// one is stored and whether the environment supplies it. key_env, by contrast, is
// returned verbatim: it names an environment variable, not a secret, and the
// operator has to see which one is in play.
type adminUpstreamView struct {
	Protocol             string `json:"protocol"`
	URL                  string `json:"url"`
	KeySet               bool   `json:"key_set"`
	KeyFromEnv           bool   `json:"key_from_env"`
	KeyEnv               string `json:"key_env"`
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
	BodyIdleSeconds      int    `json:"body_idle_seconds"`
	MaxRetries           int    `json:"max_retries"`
}

// adminVLMView is the [vlm] table. Active is false when the scheme disables the
// describe pass, which the page shows as such rather than as an empty form.
type adminVLMView struct {
	Active    bool `json:"active"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// Inherit is true when the describe calls ride the scheme's text upstream. The
	// page shows that as the placeholder instead of echoing the text upstream's
	// address into [vlm], which would silently stop following a later change.
	Inherit  bool              `json:"inherit"`
	Upstream adminUpstreamView `json:"upstream"`
}

// adminChatView is the [chat] table.
type adminChatView struct {
	// Available is false when the scheme has no openai upstream for the chat
	// passthrough: an anthropic-only scheme with no [chat] table.
	Available bool              `json:"available"`
	Inherit   bool              `json:"inherit"`
	Upstream  adminUpstreamView `json:"upstream"`
}

type adminRoutingEntry struct {
	Alias         string `json:"alias"`
	Model         string `json:"model"`
	SupportsImage bool   `json:"supports_image"`
}

// adminEffectiveRoute is the upstream model and image capability an alias actually
// resolves to. Declared separates a mapping the file carries from one the loader
// derived (the haiku fallback), which the page shows read-only.
type adminEffectiveRoute struct {
	Model         string `json:"model"`
	SupportsImage bool   `json:"supports_image"`
	Declared      bool   `json:"declared"`
}

// adminAuthView reports the state of the page's own password.
type adminAuthView struct {
	TokenSet     bool `json:"token_set"`
	TokenFromEnv bool `json:"token_from_env"`
}

// adminConfigView is the whole editable state: the main file's process settings,
// the active scheme with its three upstreams and model table, and the scheme list.
type adminConfigView struct {
	ConfigPath string        `json:"config_path"`
	Admin      adminAuthView `json:"admin"`
	Proxy      adminProxyView `json:"proxy"`
	// SchemePath is the file the scheme below lives in, so the page can show which
	// file a save writes.
	SchemePath string `json:"scheme_path"`
	// Upstream is the scheme's text upstream: the top-level protocol/url/key of the
	// scheme file.
	Upstream adminUpstreamView `json:"upstream"`
	VLM      adminVLMView      `json:"vlm"`
	Chat     adminChatView     `json:"chat"`
	// Routing is the scheme's own model table, as declared.
	Routing []adminRoutingEntry `json:"routing"`
	// EffectiveRouting covers every alias the proxy will serve, including the
	// haiku fallback that is not written in the file.
	EffectiveRouting map[string]adminEffectiveRoute `json:"effective_routing"`
}

// adminUpstreamPayload is one upstream as the form submits it. KeyAction is the
// keep/set/clear three-state choice: a stored key is never sent to the browser, so
// blank means "keep whatever is stored", and clear is the only way to remove one.
type adminUpstreamPayload struct {
	Protocol             string `json:"protocol"`
	URL                  string `json:"url"`
	KeyAction            string `json:"key_action"`
	Key                  string `json:"key"`
	KeyEnv               string `json:"key_env"`
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
	BodyIdleSeconds      int    `json:"body_idle_seconds"`
	MaxRetries           int    `json:"max_retries"`
}

// adminVLMPayload is the [vlm] table as submitted.
type adminVLMPayload struct {
	Active    bool                 `json:"active"`
	Model     string               `json:"model"`
	MaxTokens int                  `json:"max_tokens"`
	Inherit   bool                 `json:"inherit"`
	Upstream  adminUpstreamPayload `json:"upstream"`
}

// adminChatPayload is the [chat] table as submitted.
type adminChatPayload struct {
	Available bool                 `json:"available"`
	Inherit   bool                 `json:"inherit"`
	Upstream  adminUpstreamPayload `json:"upstream"`
}

type adminAuthPayload struct {
	Action string `json:"token_action"`
	Token  string `json:"token"`
}

type adminConfigPayload struct {
	Proxy    adminProxyView       `json:"proxy"`
	Upstream adminUpstreamPayload `json:"upstream"`
	VLM      adminVLMPayload      `json:"vlm"`
	Chat     adminChatPayload     `json:"chat"`
	Admin    adminAuthPayload     `json:"admin"`
	// Routing is the scheme's complete model table, submitted whole. A nil slice
	// means the request does not model it and the running table is carried over; an
	// empty non-nil slice is the operator having deleted every row.
	Routing []adminRoutingEntry `json:"routing"`
}

// currentConfigView renders the running config for the page.
func currentConfigView() adminConfigView {
	c := currentConfig()
	s := currentScheme()
	dir := schemesDir(&c)

	v := adminConfigView{
		ConfigPath: configPathFromEnv(),
		Admin: adminAuthView{
			TokenSet:     adminToken() != "",
			TokenFromEnv: os.Getenv("LLM_PROXY_ADMIN_TOKEN") != "",
		},
		Proxy: adminProxyView{
			Port:       c.Proxy.Port,
			Scheme:     c.Proxy.Scheme,
			SchemesDir: dir,
			SchemeList: listSchemes(dir),
		},
		Upstream:         upstreamView(s.Text),
		Chat:             adminChatView{Available: s.Chat.Name != "", Inherit: s.ChatInheritsText, Upstream: upstreamView(s.Chat)},
		Routing:          routingViews(s),
		EffectiveRouting: effectiveRouting(s),
	}
	if s.VLM != nil {
		v.VLM = adminVLMView{
			Active:    true,
			Model:     s.VLM.Model,
			MaxTokens: s.VLM.MaxTokens,
			Inherit:   s.VLMInheritsText,
			Upstream:  upstreamView(s.VLM.Upstream),
		}
	}
	if s.Name != "" {
		v.SchemePath = schemePath(dir, s.Name)
	}
	return v
}

// upstreamView renders one resolved upstream. A zero Upstream (a scheme with no
// chat upstream) renders empty, so the page cannot read "no upstream" as "upstream
// with an empty key".
func upstreamView(up Upstream) adminUpstreamView {
	if up.Name == "" {
		return adminUpstreamView{}
	}
	return adminUpstreamView{
		Protocol:             up.Protocol,
		URL:                  strings.TrimSuffix(up.URL, "/"),
		KeySet:               up.Key != "",
		KeyFromEnv:           up.Spec.KeyEnv != "" && os.Getenv(up.Spec.KeyEnv) != "",
		KeyEnv:               up.Spec.KeyEnv,
		HeaderTimeoutSeconds: int(up.HeaderTimeout.Seconds()),
		BodyIdleSeconds:      int(up.BodyIdle.Seconds()),
		MaxRetries:           up.MaxRetries,
	}
}

// routingViews lists a scheme's declared model table in alias order.
func routingViews(s *Scheme) []adminRoutingEntry {
	aliases := make([]string, 0, len(s.DeclaredRoutes))
	for alias := range s.DeclaredRoutes {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	out := make([]adminRoutingEntry, 0, len(aliases))
	for _, alias := range aliases {
		e := s.Routes[alias]
		out = append(out, adminRoutingEntry{Alias: alias, Model: e.Model, SupportsImage: e.SupportsImage})
	}
	return out
}

// effectiveRouting reports where every alias actually goes, including the derived
// haiku fallback.
func effectiveRouting(s *Scheme) map[string]adminEffectiveRoute {
	out := make(map[string]adminEffectiveRoute, len(s.Routes))
	for alias, e := range s.Routes {
		out[alias] = adminEffectiveRoute{
			Model:         e.Model,
			SupportsImage: e.SupportsImage,
			Declared:      s.DeclaredRoutes[alias],
		}
	}
	return out
}

// configSaveMu serializes the whole save transaction — reading the previous value,
// backing up, writing, reloading or rolling back. cfgMu only protects publishing,
// so without this two concurrent saves can interleave and leave the files and the
// running config disagreeing.
var configSaveMu sync.Mutex

func handleAdminConfigSave(w http.ResponseWriter, r *http.Request) {
	var payload adminConfigPayload
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	// Take the transaction lock before reading the running config, not after: the
	// "keep" key and password carry the stored value over, and the backup is the
	// previous file. Reading the old value outside the lock lets two concurrent
	// saves both base themselves on the same snapshot and the later one silently
	// undo the earlier one's change.
	configSaveMu.Lock()
	defer configSaveMu.Unlock()

	oldConfig := currentConfig()
	oldScheme := currentScheme()
	dir := schemesDir(&oldConfig)

	active := normalizeSchemeName(payload.Proxy.Scheme)
	if active == "" {
		active = oldConfig.Proxy.Scheme
	}
	if !validSchemeName(active) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("方案名 %q 只能包含小写字母、数字、'-' 和 '_'（最多 %d 个字符）", payload.Proxy.Scheme, maxSchemeNameLen))
		return
	}
	// Switching and editing are different operations. The submitted body belongs to
	// the scheme the form was rendered from, so a switch only re-points the main
	// file and must not write one scheme's settings into another scheme's file.
	switching := active != oldScheme.Name
	if switching {
		target := schemePath(dir, active)
		if _, err := os.Stat(target); err != nil {
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("要切换到的方案 %q 不存在（%s）；请先在方案目录里创建它，或选择已有方案 %s",
					active, target, strings.Join(listSchemes(dir), "、")))
			return
		}
	}

	newAdminTok, err := resolveAdminToken(payload.Admin, oldConfig.Admin.Token)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if payload.Proxy.Port < 1 || payload.Proxy.Port > 65535 {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("端口 %d 超出范围（1-65535）", payload.Proxy.Port))
		return
	}

	newMain := Config{
		Proxy: ProxyConfig{Port: payload.Proxy.Port, Scheme: active, SchemesDir: oldConfig.Proxy.SchemesDir},
		Admin: AdminConfig{Token: newAdminTok},
	}
	mainPath := configPathFromEnv()
	renderedMain := renderMainTOML(&newMain)

	// Validate the scheme body up front so a bad field is reported before anything
	// is written. A switch carries no scheme body: the target file already exists
	// and is left exactly as it is.
	var renderedScheme, schemeFile string
	if !switching {
		f, err := validateSchemePayload(&payload, oldScheme)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		renderedScheme = renderSchemeTOML(active, f)
		schemeFile = schemePath(dir, oldScheme.Name)
	}

	// Parse before writing: a file that cannot be read would take the proxy down on
	// its next restart.
	if err := probeFiles(active, renderedMain, schemeFile, renderedScheme); err != nil {
		writeJSONError(w, http.StatusBadRequest, "生成的配置无法解析: "+err.Error())
		return
	}

	// Only the files this save rewrites are backed up.
	mainBackup, _, err := backupConfigFile(mainPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "备份配置文件失败: "+err.Error())
		return
	}
	var schemeBackup []byte
	if !switching {
		schemeBackup, _, err = backupConfigFile(schemeFile)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "备份方案文件失败: "+err.Error())
			return
		}
	}

	if err := writeFileAtomic(mainPath, []byte(renderedMain)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "写入配置文件失败: "+err.Error())
		return
	}
	if !switching {
		if err := writeFileAtomic(schemeFile, []byte(renderedScheme)); err != nil {
			// The main file is already written; put it back so the two files never
			// disagree about which scheme runs.
			rollback(mainPath, mainBackup)
			writeJSONError(w, http.StatusInternalServerError, "写入方案文件失败: "+err.Error())
			return
		}
	}

	if err := loadConfig(); err != nil {
		// The files are on disk but unusable: put the previous ones back so the
		// running proxy and the files never disagree.
		msg := "配置重载失败: " + err.Error()
		var problems []string
		if e := rollback(mainPath, mainBackup); e != "" {
			problems = append(problems, "回滚主配置失败: "+e)
		}
		if !switching {
			if e := rollback(schemeFile, schemeBackup); e != "" {
				problems = append(problems, "回滚方案文件失败: "+e)
			}
		}
		if len(problems) > 0 {
			msg += "；" + strings.Join(problems, "；")
		} else {
			msg += "；已回滚到保存前的配置"
		}
		writeJSONError(w, http.StatusInternalServerError, msg)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"config": currentConfigView(),
		// The browser is still holding the old password, so it has to re-authenticate
		// before the page can talk to the API again. A password supplied by the
		// environment cannot change from here, so it needs no re-auth.
		"reauth_required": newAdminTok != oldConfig.Admin.Token && os.Getenv("LLM_PROXY_ADMIN_TOKEN") == "",
		"warnings":        configWarnings(&payload, switching),
	})
}

// rollback puts a file's previous contents back and returns what went wrong, or ""
// when the restore succeeded.
func rollback(path string, previous []byte) string {
	if err := writeFileAtomic(path, previous); err != nil {
		return err.Error()
	}
	return ""
}

// resolveAdminToken applies the keep/set/clear choice to the page password.
func resolveAdminToken(p adminAuthPayload, old string) (string, error) {
	switch p.Action {
	case "", "keep":
		return old, nil
	case "set":
		if p.Token == "" {
			return "", fmt.Errorf("token_action 为 set 时管理口令不能为空（如需关闭管理页面请用 clear）")
		}
		if hasControlChars(p.Token) {
			return "", fmt.Errorf("管理口令含非法控制字符")
		}
		return p.Token, nil
	case "clear":
		return "", nil
	default:
		return "", fmt.Errorf("token_action 只能是 keep、set 或 clear")
	}
}

// validateSchemePayload validates the scheme body and returns what to write. Every
// check happens here, at the one point where the operator can still fix it: a value
// the loader would silently drop is a no-op from the page's point of view.
func validateSchemePayload(p *adminConfigPayload, old *Scheme) (*SchemeFile, error) {
	text, err := validateUpstreamPayload("上游", p.Upstream, old.Text, true)
	if err != nil {
		return nil, err
	}
	if hasControlChars(p.VLM.Model) {
		return nil, fmt.Errorf("vlm_model 含非法控制字符")
	}
	if p.VLM.MaxTokens < 0 {
		return nil, fmt.Errorf("vlm_max_tokens 不能为负数")
	}
	routing, err := encodeRouting(p.Routing, old)
	if err != nil {
		return nil, err
	}

	f := &SchemeFile{UpstreamSpec: text.Spec, Routing: routing}

	// [vlm]: active with a model is a describe pass; inactive writes no table.
	switch {
	case !p.VLM.Active:
		if p.VLM.Model != "" {
			return nil, fmt.Errorf("关闭图片描述时不要填写 vlm model（要启用请勾选后再填模型名）")
		}
	case strings.TrimSpace(p.VLM.Model) == "":
		return nil, fmt.Errorf("启用图片描述时必须填写 vlm model")
	default:
		sec := &VLMSection{Model: strings.TrimSpace(p.VLM.Model), MaxTokens: p.VLM.MaxTokens}
		if p.VLM.Inherit {
			// Inherited: no upstream fields at all, so the describe calls follow the
			// scheme's text upstream.
			if text.Protocol != protocolAnthropic {
				return nil, fmt.Errorf("图片描述调用必须是 anthropic 协议；当前上游是 %q，请取消“沿用主上游”并为 [vlm] 指定一个 anthropic 上游", text.Protocol)
			}
		} else {
			up, err := validateUpstreamPayload("[vlm]", p.VLM.Upstream, vlmBase(old), true)
			if err != nil {
				return nil, err
			}
			if up.Protocol != protocolAnthropic {
				return nil, fmt.Errorf("[vlm] 协议必须是 %q（图片描述调用只说 anthropic）", protocolAnthropic)
			}
			sec.UpstreamSpec = up.Spec
		}
		f.VLM = sec
	}

	// [chat]: only meaningful for an openai upstream.
	if p.Chat.Available {
		sec := &ChatSection{}
		if p.Chat.Inherit {
			if text.Protocol != protocolOpenAI {
				return nil, fmt.Errorf("/v1/chat/completions 透传必须走 openai 协议；当前上游是 %q，请取消“沿用主上游”并为 [chat] 指定一个 openai 上游", text.Protocol)
			}
		} else {
			up, err := validateUpstreamPayload("[chat]", p.Chat.Upstream, chatBase(old), true)
			if err != nil {
				return nil, err
			}
			if up.Protocol != protocolOpenAI {
				return nil, fmt.Errorf("[chat] 协议必须是 %q（透传端点只说 openai）", protocolOpenAI)
			}
			sec.UpstreamSpec = up.Spec
		}
		f.Chat = sec
	}
	return f, nil
}

// vlmBase / chatBase are the upstreams a section's "keep" key action resolves
// against, so saving a section whose key the page never saw does not wipe it.
func vlmBase(old *Scheme) Upstream {
	if old != nil && old.VLM != nil {
		return old.VLM.Upstream
	}
	return Upstream{}
}

func chatBase(old *Scheme) Upstream {
	if old != nil {
		return old.Chat
	}
	return Upstream{}
}

// maxProfileKeyLen bounds a plaintext key. A key past this is a misplaced blob
// rather than a credential, and rejecting it here keeps the generated TOML to a
// size the reload can parse quickly.
const maxProfileKeyLen = 8 << 10

// validateUpstreamPayload validates one submitted upstream and resolves its key
// against the value the running config holds, so a "keep" survives a save.
func validateUpstreamPayload(label string, p adminUpstreamPayload, old Upstream, requireURL bool) (Upstream, error) {
	protocol := normalizeUpstreamName(p.Protocol)
	if protocol != protocolAnthropic && protocol != protocolOpenAI {
		return Upstream{}, fmt.Errorf("%s 的 protocol 只能是 %q 或 %q", label, protocolAnthropic, protocolOpenAI)
	}
	if strings.TrimSpace(p.URL) == "" {
		if requireURL {
			return Upstream{}, fmt.Errorf("%s 的 url 不能为空", label)
		}
	} else if err := validateProfileURL(p.URL); err != nil {
		return Upstream{}, fmt.Errorf("%s: %v", label, err)
	}
	if p.HeaderTimeoutSeconds < 0 || p.BodyIdleSeconds < 0 || p.MaxRetries < 0 {
		return Upstream{}, fmt.Errorf("%s 的超时与重试次数不能为负数", label)
	}
	keyEnv := strings.TrimSpace(p.KeyEnv)
	if hasControlChars(keyEnv) {
		return Upstream{}, fmt.Errorf("%s 的 key_env 含非法控制字符", label)
	}

	var key string
	switch p.KeyAction {
	case "", "keep":
		// Keep the stored key. A section that had none keeps having none, which is
		// legal: a local gateway may need no credential.
		key = old.Key
	case "set":
		if p.Key == "" {
			return Upstream{}, fmt.Errorf("%s 选择 set 时密钥不能为空（如需清空请用 clear）", label)
		}
		key = p.Key
	case "clear":
		key = ""
	default:
		return Upstream{}, fmt.Errorf("%s 的 key_action 只能是 keep、set 或 clear", label)
	}
	if len(key) > maxProfileKeyLen {
		return Upstream{}, fmt.Errorf("%s 的密钥过长（最多 %d 个字符）", label, maxProfileKeyLen)
	}
	if hasControlChars(key) {
		return Upstream{}, fmt.Errorf("%s 的密钥含非法控制字符", label)
	}

	spec := UpstreamSpec{
		Protocol:             protocol,
		URL:                  strings.TrimSpace(p.URL),
		Key:                  key,
		KeyEnv:               keyEnv,
		HeaderTimeoutSeconds: p.HeaderTimeoutSeconds,
		BodyIdleSeconds:      p.BodyIdleSeconds,
		MaxRetries:           p.MaxRetries,
	}
	return Upstream{
		Protocol:      protocol,
		URL:           strings.TrimSuffix(spec.URL, "/"),
		Key:           key,
		HeaderTimeout: secondsOrDefault(spec.HeaderTimeoutSeconds, defaultHeaderTimeoutSeconds),
		BodyIdle:      secondsOrDefault(spec.BodyIdleSeconds, defaultBodyIdleSeconds),
		MaxRetries:    retriesOrDefault(spec.MaxRetries),
		Spec:          spec,
	}, nil
}

// encodeRouting turns the submitted model rows into the TOML values the [routing]
// table holds. Every row is validated here rather than left to the loader, which
// would skip a bad row with a warning the page never shows.
func encodeRouting(entries []adminRoutingEntry, old *Scheme) (map[string]toml.Primitive, error) {
	if entries == nil {
		// The client does not model the table, so the running one is carried over
		// rather than read as a deletion.
		if old == nil || len(old.DeclaredRoutes) == 0 {
			return map[string]toml.Primitive{}, nil
		}
		carried := make([]adminRoutingEntry, 0, len(old.DeclaredRoutes))
		for alias := range old.DeclaredRoutes {
			e := old.Routes[alias]
			carried = append(carried, adminRoutingEntry{Alias: alias, Model: e.Model, SupportsImage: e.SupportsImage})
		}
		return encodeRouting(carried, nil)
	}
	if len(entries) == 0 {
		// A non-nil empty map, not nil: the renderer uses nil to mean "write no
		// table", and an operator who deleted every row means exactly that — but an
		// absent payload field (nil) means "carry the stored table over" and is
		// handled by the caller.
		return map[string]toml.Primitive{}, nil
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, e := range entries {
		alias := strings.TrimSpace(e.Alias)
		model := strings.TrimSpace(e.Model)
		if alias == "" {
			return nil, fmt.Errorf("路由别名不能为空")
		}
		if model == "" {
			return nil, fmt.Errorf("路由 %q 的模型名不能为空", alias)
		}
		if hasControlChars(alias) || hasControlChars(model) {
			return nil, fmt.Errorf("路由 %q 含非法控制字符", alias)
		}
		if seen[alias] {
			return nil, fmt.Errorf("路由别名 %q 重复", alias)
		}
		seen[alias] = true
		if e.SupportsImage {
			fmt.Fprintf(&b, "%s = { model = %s, supports_image = true }\n", tomlKey(alias), tomlString(model))
		} else {
			fmt.Fprintf(&b, "%s = %s\n", tomlKey(alias), tomlString(model))
		}
	}
	var table map[string]toml.Primitive
	if _, err := toml.Decode(b.String(), &table); err != nil {
		return nil, fmt.Errorf("路由表无法生成: %v", err)
	}
	return table, nil
}

// renderMainTOML writes the main config file: process settings plus the active
// scheme. Everything about reaching a model lives in the scheme file.
func renderMainTOML(c *Config) string {
	var b strings.Builder
	b.WriteString("# llm-proxy main configuration.\n")
	b.WriteString("# Written by the web admin page; the previous file is kept next to this one as\n")
	b.WriteString("# <config>.bak.<timestamp>. Hand-written comments are not preserved by a save.\n")
	b.WriteString("#\n")
	b.WriteString("# Upstream, VLM and model-table settings are NOT here: each scheme file under the\n")
	b.WriteString("# schemes directory is a complete configuration of its own, and [proxy] scheme\n")
	b.WriteString("# selects the one this process runs.\n\n")

	b.WriteString("[proxy]\n")
	fmt.Fprintf(&b, "port = %d\n", c.Proxy.Port)
	b.WriteString("# The scheme file (without .toml) this process runs. Exactly one is active.\n")
	fmt.Fprintf(&b, "scheme = %s\n", tomlString(c.Proxy.Scheme))
	if c.Proxy.SchemesDir != "" {
		b.WriteString("# Directory holding the scheme files. Empty means $LLM_PROXY_SCHEMES_DIR, else a\n")
		b.WriteString("# \"schemes\" directory next to this file.\n")
		fmt.Fprintf(&b, "schemes_dir = %s\n", tomlString(c.Proxy.SchemesDir))
	}
	b.WriteString("\n[admin]\n")
	b.WriteString("# Web admin page password (HTTP Basic, any username). Empty disables the page\n")
	b.WriteString("# entirely. LLM_PROXY_ADMIN_TOKEN overrides this value.\n")
	fmt.Fprintf(&b, "token = %s\n", tomlString(c.Admin.Token))
	return b.String()
}

// renderSchemeTOML writes one scheme file. The plaintext key is written whenever
// the upstream has one — the page never receives it, so it reaches this function
// only from the running config via the keep action.
func renderSchemeTOML(name string, f *SchemeFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# llm-proxy scheme %q.\n", name)
	b.WriteString("# One complete upstream configuration: the text upstream (top level), the image\n")
	b.WriteString("# description pass ([vlm]), the chat passthrough ([chat]) and this scheme's model\n")
	b.WriteString("# table ([routing]). The main config.toml selects which scheme runs.\n\n")

	writeUpstreamSpec(&b, "", f.UpstreamSpec)

	if f.VLM != nil {
		b.WriteString("\n[vlm]\n")
		b.WriteString("# Vision model used to describe image-carrying requests before they are routed\n")
		b.WriteString("# to the text model. Routes marked supports_image = true skip this pass.\n")
		fmt.Fprintf(&b, "model = %s\n", tomlString(f.VLM.Model))
		fmt.Fprintf(&b, "max_tokens = %d\n", f.VLM.MaxTokens)
		if f.VLM.URL != "" {
			b.WriteString("# Describe calls go to this upstream (must speak the anthropic protocol).\n")
			b.WriteString("# Omit these fields to reuse the scheme's own upstream above.\n")
			writeUpstreamSpec(&b, "", f.VLM.UpstreamSpec)
		} else {
			b.WriteString("# No upstream here: describe calls reuse the scheme's own upstream above.\n")
		}
	}

	if f.Chat != nil {
		b.WriteString("\n[chat]\n")
		if f.Chat.URL != "" {
			b.WriteString("# Upstream /v1/chat/completions forwards to (must speak the openai protocol).\n")
			b.WriteString("# Omit these fields to reuse the scheme's own upstream above.\n")
			writeUpstreamSpec(&b, "", f.Chat.UpstreamSpec)
		} else {
			b.WriteString("# No upstream here: the passthrough reuses the scheme's own upstream above.\n")
		}
	}

	if len(f.Routing) > 0 {
		b.WriteString("\n[routing]\n")
		b.WriteString("# Aliases Claude Code asks for, mapped to this scheme's upstream model names.\n")
		b.WriteString("# Values may be a plain model string or { model = \"...\", supports_image = true }.\n")
		b.WriteString("# An alias not listed here is forwarded under its own name. haiku falls back to\n")
		b.WriteString("# sonnet when it is not declared.\n")
		aliases := make([]string, 0, len(f.Routing))
		for alias := range f.Routing {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			e, err := decodeRouteEntry(f.Routing[alias])
			if err != nil {
				continue
			}
			if e.SupportsImage {
				fmt.Fprintf(&b, "%s = { model = %s, supports_image = true }\n", tomlKey(alias), tomlString(e.Model))
			} else {
				fmt.Fprintf(&b, "%s = %s\n", tomlKey(alias), tomlString(e.Model))
			}
		}
	}
	return b.String()
}

// writeUpstreamSpec writes one upstream's fields. An empty timeout/retry is
// omitted rather than written as 0, so the documented default keeps applying and
// the file says what the operator actually chose.
func writeUpstreamSpec(b *strings.Builder, indent string, s UpstreamSpec) {
	fmt.Fprintf(b, "protocol = %s\n", tomlString(s.Protocol))
	fmt.Fprintf(b, "url = %s\n", tomlString(strings.TrimSuffix(strings.TrimSpace(s.URL), "/")))
	if s.Key != "" {
		fmt.Fprintf(b, "key = %s\n", tomlString(s.Key))
	}
	if s.KeyEnv != "" {
		fmt.Fprintf(b, "key_env = %s\n", tomlString(s.KeyEnv))
	}
	if s.HeaderTimeoutSeconds > 0 {
		fmt.Fprintf(b, "header_timeout_seconds = %d\n", s.HeaderTimeoutSeconds)
	}
	if s.BodyIdleSeconds > 0 {
		fmt.Fprintf(b, "body_idle_seconds = %d\n", s.BodyIdleSeconds)
	}
	if s.MaxRetries > 0 {
		fmt.Fprintf(b, "max_retries = %d\n", s.MaxRetries)
	}
}

// probeFiles parses what a save is about to write through the same code the
// running process uses, so an unusable combination is rejected while the operator
// can still see why. Nothing is written when this fails.
func probeFiles(active, mainTOML, schemePath, schemeTOML string) error {
	var mainCfg Config
	if err := toml.Unmarshal([]byte(mainTOML), &mainCfg); err != nil {
		return fmt.Errorf("主配置: %w", err)
	}
	if schemePath == "" {
		// Switching schemes: the target file already exists and is not rewritten.
		return nil
	}
	// Parse the scheme through a temporary file, which is the same path the process
	// takes at load and so exercises the same resolution.
	dir, err := os.MkdirTemp("", "llm-proxy-probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, active+".toml")
	if err := os.WriteFile(tmp, []byte(schemeTOML), 0o600); err != nil {
		return err
	}
	if _, err := parseSchemeFile(tmp, active); err != nil {
		return err
	}
	return nil
}

// configWarnings reports what a save changed that the operator should know about.
func configWarnings(payload *adminConfigPayload, switching bool) []string {
	var warnings []string
	if switching {
		warnings = append(warnings, fmt.Sprintf(
			"已切换到方案 %q。切换只改主配置里选中的方案，其他方案文件的内容未被修改。", payload.Proxy.Scheme))
		return warnings
	}
	declared := map[string]bool{}
	for _, e := range payload.Routing {
		if alias := strings.TrimSpace(e.Alias); alias != "" {
			declared[alias] = true
		}
	}
	// An alias the scheme leaves out is not necessarily forwarded under its own
	// name: routeTarget maps a composed name (claude-sonnet-4-5) to the family
	// alias, and haiku additionally inherits sonnet. Say which case applies, so
	// the warning does not promise a passthrough that will not happen.
	for _, alias := range []string{"sonnet", "opus", "haiku"} {
		if declared[alias] {
			continue
		}
		switch {
		case alias == "haiku" && declared["sonnet"]:
			warnings = append(warnings, "方案未声明 haiku：客户端请求 haiku（含 claude-haiku-4-5 这类完整名字）时沿用 sonnet 的映射。")
		case aliasUndeclaredFallsBack(declared, alias):
			// Nothing in this family is mapped, so the composed and exact names
			// both go to the upstream as written.
			warnings = append(warnings, fmt.Sprintf(
				"方案未声明 %s：客户端请求 %s（含 claude-%s-4-5 这类完整名字）时会原样转发给上游，由上游决定是否支持该模型。", alias, alias, alias))
		}
	}
	// The scheme's own key_env is the supported way to keep a secret out of the
	// file, and the shipped example uses SOPHNET_API_KEY for it. Only warn when
	// the variable is set but no upstream in the saved scheme reads it — that is
	// the leftover-from-the-old-global-config case.
	if os.Getenv("SOPHNET_API_KEY") != "" && !schemeReferencesKeyEnv(payload, "SOPHNET_API_KEY") {
		warnings = append(warnings, "环境变量 SOPHNET_API_KEY 已设置，但本方案的任何上游都没有引用它：每个上游只用自己的 key / key_env，该变量被忽略。")
	}
	if os.Getenv("LLM_PROXY_ADMIN_TOKEN") != "" {
		if payload.Admin.Action == "clear" {
			warnings = append(warnings, "配置文件中的管理口令已清空，但环境变量 LLM_PROXY_ADMIN_TOKEN 仍然生效，管理页面保持开放。")
		} else {
			warnings = append(warnings, "环境变量 LLM_PROXY_ADMIN_TOKEN 已设置，它的优先级高于配置文件里的管理口令；在页面上修改口令不会生效。")
		}
	}
	return warnings
}

// aliasUndeclaredFallsBack reports whether an alias the scheme leaves out will be
// forwarded as written. haiku is excluded by the caller: it inherits sonnet.
func aliasUndeclaredFallsBack(declared map[string]bool, alias string) bool {
	return alias != "haiku" || !declared["sonnet"]
}

// schemeReferencesKeyEnv reports whether any upstream the save will actually write
// reads the named environment variable.
//
// A section that inherits the text upstream contributes nothing of its own, so its
// key_env is not a reference — the save discards it, which is why validateSchemePayload
// never reads those fields in that case either.
func schemeReferencesKeyEnv(p *adminConfigPayload, name string) bool {
	referenced := []string{p.Upstream.KeyEnv}
	if p.VLM.Active && !p.VLM.Inherit {
		referenced = append(referenced, p.VLM.Upstream.KeyEnv)
	}
	if p.Chat.Available && !p.Chat.Inherit {
		referenced = append(referenced, p.Chat.Upstream.KeyEnv)
	}
	for _, env := range referenced {
		if strings.TrimSpace(env) == name {
			return true
		}
	}
	return false
}

func validateURL(field, raw string) error {
	if err := validateProfileURL(raw); err != nil {
		return fmt.Errorf("%s: %v", field, err)
	}
	return nil
}

// hasControlChars reports whether s carries a control character, which no config
// field may: it would corrupt the generated TOML or the log line carrying it.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

var bareTOMLKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlKey renders a table key bare when it is a simple identifier and quoted
// otherwise, so an alias with a dot or a space still round-trips.
func tomlKey(s string) string {
	if bareTOMLKey.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

func tomlString(s string) string { return strconv.Quote(s) }

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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]interface{}{"ok": false, "error": message})
}

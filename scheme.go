package main

// This file owns the configuration layer: the small main file (process settings
// plus which scheme is active) and the per-scheme files under the schemes
// directory.
//
// There are deliberately no global upstream or VLM settings. Everything that
// describes how to reach a model — protocol, URL, credential, timeouts, the
// vision model, the chat passthrough upstream and the model mapping table —
// belongs to one scheme file, and exactly one scheme is active at a time. A
// change of provider is therefore a change of active file, not an edit spread
// across several global sections that quietly affect each other.

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the main configuration file (/etc/llm-proxy/config.toml). It holds
// only what is not part of an upstream scheme: the listen port, which scheme file
// is active, where the scheme files live, and the admin page's password.
type Config struct {
	Proxy ProxyConfig `toml:"proxy"`
	Admin AdminConfig `toml:"admin"`
}

// ProxyConfig holds the process-level settings.
type ProxyConfig struct {
	Port int `toml:"port"`
	// Scheme names the active scheme: the file name (without .toml) under the
	// schemes directory. Exactly one scheme is active, and it is the only source
	// of upstream and VLM settings.
	Scheme string `toml:"scheme"`
	// SchemesDir overrides where the scheme files live. Empty means
	// $LLM_PROXY_SCHEMES_DIR, else a "schemes" directory next to this file.
	SchemesDir string `toml:"schemes_dir"`
}

// AdminConfig configures the web admin page. An empty Token disables the whole
// /admin subtree (fail closed): the proxy listens on a routable address, so an
// unauthenticated config editor would expose the upstream keys and the routing
// tables to the network.
type AdminConfig struct {
	Token string `toml:"token"`
}

// UpstreamSpec is one upstream as a scheme file declares it: how to talk to it,
// where it is, how to authenticate, and how long to wait. It is embedded at the
// top level of a scheme (the text upstream) and inside [vlm] / [chat], so each of
// the three can carry a complete upstream of its own without repeating the type.
type UpstreamSpec struct {
	// Protocol is "anthropic" or "openai": the framing requests to this upstream
	// are sent in. Required at the top level; for [vlm] and [chat] it may be left
	// out when the section inherits the scheme's text upstream.
	Protocol string `toml:"protocol"`
	// URL is the upstream base or endpoint, http(s). For the openai protocol a
	// base URL gets /v1/chat/completions appended, a URL already ending in
	// /chat/completions is used as-is. For anthropic, /v1/messages is appended.
	URL string `toml:"url"`
	// Key is the plaintext credential. KeyEnv names an environment variable that
	// wins over it when non-empty, so a key can stay out of the file.
	Key    string `toml:"key"`
	KeyEnv string `toml:"key_env"`
	// Zero means "use the documented default" (120 / 90 / 2).
	HeaderTimeoutSeconds int `toml:"header_timeout_seconds"`
	BodyIdleSeconds      int `toml:"body_idle_seconds"`
	MaxRetries           int `toml:"max_retries"`
}

// VLMSection is the [vlm] table: the vision model images are described with
// before a request is routed to the text model.
//
// Its own upstream fields are optional. Left out, the describe calls go to the
// scheme's text upstream, which only works when that one speaks the anthropic
// framing the describe call uses — a scheme whose text upstream is openai has to
// name an anthropic upstream here. Omitting the whole [vlm] table (or leaving out
// model) disables the describe pass, and image-carrying requests are then
// forwarded untouched.
type VLMSection struct {
	UpstreamSpec
	Model     string `toml:"model"`
	MaxTokens int    `toml:"max_tokens"`
}

// ChatSection is the [chat] table: the upstream the OpenAI-compatible
// /v1/chat/completions passthrough forwards to. Its upstream fields are optional
// and inherit the scheme's text upstream when that one speaks the openai framing.
type ChatSection struct {
	UpstreamSpec
}

// SchemeFile is one schemes/<name>.toml. Its top-level protocol/url/key/timeouts
// are the text upstream — the one Claude Code's /v1/messages traffic goes to —
// and [routing] is that scheme's own model mapping table.
type SchemeFile struct {
	UpstreamSpec
	VLM     *VLMSection              `toml:"vlm"`
	Chat    *ChatSection             `toml:"chat"`
	Routing map[string]toml.Primitive `toml:"routing"`
}

// Upstream is one resolved upstream: everything needed to send a request to it,
// with the documented defaults already applied and the key resolved from key_env.
// A request carries this rather than the config fields, so one request cannot mix
// two versions of a scheme.
type Upstream struct {
	// Name identifies the upstream in logs and in the stats collector: the scheme
	// name for the text upstream, "<scheme>/vlm" and "<scheme>/chat" otherwise.
	Name          string
	Protocol      string
	URL           string
	Key           string
	HeaderTimeout time.Duration
	BodyIdle      time.Duration
	MaxRetries    int
	// Spec is what the file declared, before key_env resolution and before the
	// documented defaults filled the zero fields in. The admin page saves it back,
	// so a key that lives in the environment is never copied into the file, and an
	// inherited default is never frozen as if the operator had chosen it.
	Spec UpstreamSpec
}

// VLMScheme is a scheme's resolved describe pass: the vision model, its output
// cap, and the upstream that serves the describe calls.
type VLMScheme struct {
	Upstream  Upstream
	Model     string
	MaxTokens int
}

// Scheme is one resolved scheme file, ready for the request path.
type Scheme struct {
	Name string
	// Text is the upstream /v1/messages traffic goes to.
	Text Upstream
	// VLM is nil when the scheme disables the image-describe pass.
	VLM *VLMScheme
	// Chat is the upstream the /v1/chat/completions passthrough forwards to. Its
	// Name is "" when the scheme has no openai upstream for it.
	Chat Upstream
	// Routes is this scheme's model mapping table, with the haiku→sonnet fallback
	// already applied.
	Routes map[string]RouteEntry
	// DeclaredRoutes marks the aliases the file itself declares, so the admin page
	// can show a derived alias (the haiku fallback) as read-only rather than let a
	// save write it out as if it had been chosen.
	DeclaredRoutes map[string]bool
	// VLMInheritsText / ChatInheritsText record that a section carried no upstream
	// of its own and rode the scheme's text upstream. The admin page needs the
	// distinction: "inherited" follows a later change to the text URL, while an
	// explicitly written copy does not.
	VLMInheritsText  bool
	ChatInheritsText bool
}

const (
	protocolAnthropic = "anthropic"
	protocolOpenAI    = "openai"
)

// Documented defaults, applied when a scheme file leaves the field out.
const (
	defaultHeaderTimeoutSeconds = 120
	defaultBodyIdleSeconds      = 90
	defaultMaxRetries           = 2
	defaultPort                 = 8088
)

// maxSchemeNameLen bounds a scheme name, so a malformed file name cannot create
// an unbounded key.
const maxSchemeNameLen = 32

// defaultMainConfigPath is where the main file lives when LLM_PROXY_CONFIG is
// unset.
const configPath = "/etc/llm-proxy/config.toml"

// configPathFromEnv returns the main config file path, honoring LLM_PROXY_CONFIG
// so the proxy can be deployed without writing to /etc.
func configPathFromEnv() string {
	if p := os.Getenv("LLM_PROXY_CONFIG"); p != "" {
		return p
	}
	return configPath
}

// schemesDir resolves the directory the scheme files live in: the main config's
// schemes_dir, else LLM_PROXY_SCHEMES_DIR, else a "schemes" directory next to the
// main config file. Keeping the default relative to the config file means a
// deployment that points LLM_PROXY_CONFIG at a staging directory gets its schemes
// from the same place without a second setting.
func schemesDir(c *Config) string {
	if d := strings.TrimSpace(c.Proxy.SchemesDir); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("LLM_PROXY_SCHEMES_DIR")); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(configPathFromEnv()), "schemes")
}

// schemePath returns the file a scheme name refers to.
func schemePath(dir, name string) string {
	return filepath.Join(dir, name+".toml")
}

// validSchemeName reports whether a (already lowercase) scheme name uses only the
// characters the format allows. The name doubles as a file name, so allowing
// anything else would let a config reach outside its directory.
func validSchemeName(name string) bool {
	if name == "" || len(name) > maxSchemeNameLen {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// normalizeSchemeName canonicalises a configured scheme name. Names are
// case-folded so "DSO" and "dso" address the same file.
func normalizeSchemeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// parseMainConfig reads and validates the main config file. It never touches the
// live globals, so a failed parse leaves the running config intact.
func parseMainConfig() (Config, error) {
	var c Config
	data, err := os.ReadFile(configPathFromEnv())
	if err != nil {
		return c, fmt.Errorf("read config %s: %w", configPathFromEnv(), err)
	}
	if err := toml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	applyConfigDefaults(&c)
	return c, nil
}

// applyConfigDefaults fills every unset process-level field.
func applyConfigDefaults(c *Config) {
	if c.Proxy.Port == 0 {
		c.Proxy.Port = defaultPort
	}
	c.Proxy.Scheme = normalizeSchemeName(c.Proxy.Scheme)
}

// parseSchemeFile reads one scheme file and resolves it into a Scheme. name is the
// file stem, which is also how the scheme is selected and logged.
func parseSchemeFile(path, name string) (*Scheme, error) {
	var f SchemeFile
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scheme %s: %w", name, err)
	}
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse scheme %s: %w", name, err)
	}

	text, err := resolveUpstream(name, name, f.UpstreamSpec, true)
	if err != nil {
		return nil, err
	}
	s := &Scheme{Name: name, Text: text}

	if f.VLM != nil {
		s.VLM, s.VLMInheritsText, err = resolveVLM(name, text, f.VLM)
		if err != nil {
			return nil, err
		}
	}
	s.Chat, s.ChatInheritsText, err = resolveChat(name, text, f.Chat)
	if err != nil {
		return nil, err
	}
	s.Routes, s.DeclaredRoutes = buildSchemeRoutes(name, f.Routing)
	return s, nil
}

// resolveUpstream validates one upstream spec and resolves its key and timeouts.
// When inherit is allowed and the spec names no URL, the caller's upstream is
// returned unchanged — that is how [vlm] and [chat] stay on the scheme's own
// gateway without repeating its address.
func resolveUpstream(label, name string, spec UpstreamSpec, requireURL bool) (Upstream, error) {
	protocol := normalizeUpstreamName(spec.Protocol)
	if protocol == "" && !requireURL {
		// A [vlm] / [chat] section that names no protocol names no upstream at
		// all: the section inherits the scheme's text upstream, which is how a
		// single-gateway scheme avoids repeating its address three times.
		return Upstream{}, errInheritUpstream
	}
	if protocol != protocolAnthropic && protocol != protocolOpenAI {
		return Upstream{}, fmt.Errorf("scheme %s: protocol %q must be %q or %q",
			label, spec.Protocol, protocolAnthropic, protocolOpenAI)
	}
	if err := validateProfileURL(spec.URL); err != nil {
		if requireURL || strings.TrimSpace(spec.URL) != "" {
			return Upstream{}, fmt.Errorf("scheme %s: %v", label, err)
		}
		return Upstream{}, errInheritUpstream
	}
	return Upstream{
		Name:          name,
		Protocol:      protocol,
		URL:           strings.TrimSuffix(strings.TrimSpace(spec.URL), "/"),
		Key:           resolveKey(spec),
		HeaderTimeout: secondsOrDefault(spec.HeaderTimeoutSeconds, defaultHeaderTimeoutSeconds),
		BodyIdle:      secondsOrDefault(spec.BodyIdleSeconds, defaultBodyIdleSeconds),
		MaxRetries:    retriesOrDefault(spec.MaxRetries),
		Spec:          spec,
	}, nil
}

// errInheritUpstream is the sentinel resolveUpstream returns when a section
// carries no URL and is allowed to inherit its caller's upstream.
var errInheritUpstream = fmt.Errorf("inherit upstream")

// resolveKey resolves a spec's credential: key_env wins over the plaintext key
// when it names a non-empty variable, so a deployment can keep the secret out of
// the file. An upstream with neither is allowed — a local gateway may need no
// credential.
func resolveKey(spec UpstreamSpec) string {
	if env := strings.TrimSpace(spec.KeyEnv); env != "" {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return spec.Key
}

// resolveVLM builds the scheme's describe pass. The describe call always speaks
// the anthropic framing (/v1/messages), so calling one through an openai upstream
// is rejected at load rather than failing per image at request time.
// resolveVLM builds the scheme's describe pass. The describe call always speaks
// the anthropic framing (/v1/messages), so calling one through an openai upstream
// is rejected at load rather than failing per image at request time.
func resolveVLM(scheme string, text Upstream, sec *VLMSection) (*VLMScheme, bool, error) {
	if strings.TrimSpace(sec.Model) == "" {
		// No model: the describe pass is off, which is a documented way to run.
		// Upstream fields without a model are a mistake, not a disabled pass.
		if strings.TrimSpace(sec.URL) != "" || strings.TrimSpace(sec.Protocol) != "" {
			return nil, false, fmt.Errorf("scheme %s: [vlm] has upstream fields but no model; add model = \"...\" or drop the table", scheme)
		}
		log.Printf("config: scheme %s disables the image-describe pass ([vlm] has no model)\n", scheme)
		return nil, false, nil
	}

	up, err := resolveUpstream(scheme+" [vlm]", scheme+"/vlm", sec.UpstreamSpec, false)
	inherits := false
	switch {
	case err == errInheritUpstream:
		// No upstream of its own: the describe calls ride the scheme's text
		// upstream, which has to be an anthropic one.
		if text.Protocol != protocolAnthropic {
			return nil, false, fmt.Errorf("scheme %s: [vlm] declares model = %q but no url, and the scheme's own upstream speaks %q; "+
				"describe calls speak %q, so give [vlm] its own anthropic url (or remove the model to disable image description)",
				scheme, sec.Model, text.Protocol, protocolAnthropic)
		}
		up = text
		inherits = true
	case err != nil:
		return nil, false, err
	case up.Protocol != protocolAnthropic:
		return nil, false, fmt.Errorf("scheme %s: [vlm] protocol is %q, but describe calls speak %q",
			scheme, up.Protocol, protocolAnthropic)
	}
	maxTokens := sec.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8000
	}
	return &VLMScheme{Upstream: up, Model: sec.Model, MaxTokens: maxTokens}, inherits, nil
}

// resolveChat builds the upstream the /v1/chat/completions passthrough forwards
// to. That endpoint speaks the OpenAI framing, so an anthropic upstream here is a
// configuration error rather than a request-time fallback.
func resolveChat(scheme string, text Upstream, sec *ChatSection) (Upstream, bool, error) {
	if sec == nil {
		// No [chat] table: the passthrough rides the text upstream when that one
		// speaks the OpenAI framing. A scheme on the anthropic protocol simply has
		// no passthrough upstream, which the handler reports rather than
		// misrouting the request.
		if text.Protocol == protocolOpenAI {
			return text, true, nil
		}
		return Upstream{}, false, nil
	}
	up, err := resolveUpstream(scheme+" [chat]", scheme+"/chat", sec.UpstreamSpec, false)
	switch {
	case err == errInheritUpstream:
		if text.Protocol != protocolOpenAI {
			return Upstream{}, false, fmt.Errorf("scheme %s: [chat] declares no url, and the scheme's own upstream speaks %q; "+
				"the chat passthrough speaks %q, so give [chat] its own openai url",
				scheme, text.Protocol, protocolOpenAI)
		}
		return text, true, nil
	case err != nil:
		return Upstream{}, false, err
	case up.Protocol != protocolOpenAI:
		return Upstream{}, false, fmt.Errorf("scheme %s: [chat] protocol is %q, but the chat passthrough speaks %q",
			scheme, up.Protocol, protocolOpenAI)
	}
	return up, false, nil
}

func secondsOrDefault(v, def int) time.Duration {
	if v > 0 {
		return time.Duration(v) * time.Second
	}
	return time.Duration(def) * time.Second
}

func retriesOrDefault(v int) int {
	if v > 0 {
		return v
	}
	return defaultMaxRetries
}

// buildSchemeRoutes decodes a scheme's own [routing] table. A malformed entry is
// skipped with a warning rather than aborting the load: one typo must not take the
// whole proxy down.
//
// haiku keeps its long-standing fallback to sonnet's target, because Claude Code
// always asks for one of the three builtin names and an undeclared haiku is far
// more often an omission than an intent to leave it unroutable. An alias the table
// does not declare at all is passed through to the text upstream under the client's
// own model name (see routeTarget).
func buildSchemeRoutes(scheme string, routing map[string]toml.Primitive) (map[string]RouteEntry, map[string]bool) {
	routes := make(map[string]RouteEntry, len(routing)+1)
	declared := make(map[string]bool, len(routing))
	for alias, prim := range routing {
		e, err := decodeRouteEntry(prim)
		if err != nil {
			log.Printf("config: scheme %s: invalid routing entry %q: %v (skipped)\n", scheme, alias, err)
			continue
		}
		routes[alias] = e
		declared[alias] = true
	}
	if _, ok := routes["haiku"]; !ok {
		if sonnet, hasSonnet := routes["sonnet"]; hasSonnet {
			routes["haiku"] = sonnet
		}
	}
	return routes, declared
}

// decodeRouteEntry turns one [routing] value into a RouteEntry. A plain string is
// the shorthand for "forward to this upstream model with no image support"; a table
// carries at least model and may add supports_image.
func decodeRouteEntry(p toml.Primitive) (RouteEntry, error) {
	var s string
	if err := toml.PrimitiveDecode(p, &s); err == nil {
		return RouteEntry{Model: s}, nil
	}
	var t struct {
		Model         string `toml:"model"`
		SupportsImage bool   `toml:"supports_image"`
	}
	if err := toml.PrimitiveDecode(p, &t); err == nil && t.Model != "" {
		return RouteEntry{Model: t.Model, SupportsImage: t.SupportsImage}, nil
	}
	return RouteEntry{}, fmt.Errorf("must be a model string or { model = \"...\", supports_image = true }")
}

// validateProfileURL checks an upstream URL. http(s) only, with a host.
func validateProfileURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url 不是合法 URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url 必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return fmt.Errorf("url 缺少主机名")
	}
	return nil
}

// normalizeUpstreamName canonicalises a protocol or scheme name's case, keeping
// its meaning. Only the case and surrounding space are rewritten, never the value
// itself: "" stays "" so a caller can tell "unset" from an explicit choice.
func normalizeUpstreamName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// anthropicMessagesEndpoint appends the Anthropic messages path to a base URL.
func anthropicMessagesEndpoint(base string) string {
	return strings.TrimSuffix(base, "/") + "/v1/messages"
}

// openAICompletionsEndpoint returns the OpenAI chat-completions endpoint for a URL
// that may be a base URL (path appended) or already carry the full
// /chat/completions endpoint.
func openAICompletionsEndpoint(base string) string {
	base = strings.TrimSuffix(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/v1/chat/completions"
}

// listSchemes returns the scheme names available in dir, sorted. A directory that
// does not exist yields no names; loadConfig reports the missing active scheme
// separately, which is the failure an operator actually has to fix.
func listSchemes(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := normalizeSchemeName(strings.TrimSuffix(e.Name(), ".toml"))
		if !validSchemeName(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// loadConfig reads the main file and the active scheme and publishes both. It is
// the only writer of cfg and scheme, so a reload swaps them atomically from a
// reader's point of view.
func loadConfig() error {
	c, err := parseMainConfig()
	if err != nil {
		return err
	}
	if c.Proxy.Scheme == "" {
		return fmt.Errorf("config: no scheme selected; set [proxy] scheme = \"<name>\" to the scheme file to run")
	}
	if !validSchemeName(c.Proxy.Scheme) {
		return fmt.Errorf("config: scheme %q may only contain a-z, 0-9, '-' and '_' (max %d chars)",
			c.Proxy.Scheme, maxSchemeNameLen)
	}
	dir := schemesDir(&c)
	path := schemePath(dir, c.Proxy.Scheme)
	if _, err := os.Stat(path); err != nil {
		available := listSchemes(dir)
		if len(available) == 0 {
			return fmt.Errorf("config: scheme %q not found in %s (the directory holds no scheme files)", c.Proxy.Scheme, dir)
		}
		return fmt.Errorf("config: scheme %q not found in %s (available: %s)",
			c.Proxy.Scheme, dir, strings.Join(available, ", "))
	}
	s, err := parseSchemeFile(path, c.Proxy.Scheme)
	if err != nil {
		return err
	}
	storeConfig(c, s)
	return nil
}

// storeConfig publishes a freshly parsed config. It is the only writer of cfg and
// scheme, so a reload swaps both atomically from a reader's point of view.
func storeConfig(c Config, s *Scheme) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfg = c
	scheme = s
}

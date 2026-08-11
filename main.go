package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// Config represents /etc/llm-proxy/config.toml
type Config struct {
	Proxy    ProxyConfig    `toml:"proxy"`
	Upstream UpstreamConfig `toml:"upstream"`
	Keys     KeysConfig     `toml:"keys"`
	Routing  RoutingConfig  `toml:"routing"`
}

type ProxyConfig struct {
	Port         int    `toml:"port"`
	VLMModel     string `toml:"vlm_model"`
	VLMMaxTokens int    `toml:"vlm_max_tokens"`
}

type UpstreamConfig struct {
	AnthropicURL string `toml:"anthropic_url"`
	OpenAIURL    string `toml:"openai_url"`
}

type KeysConfig struct {
	Sophnet string `toml:"sophnet"`
}

type RoutingConfig struct {
	Sonnet string `toml:"sonnet"`
	Opus   string `toml:"opus"`
	Haiku  string `toml:"haiku"`
}

var cfg Config

const configPath = "/etc/llm-proxy/config.toml"

// configPathFromEnv returns the config file path, honoring LLM_PROXY_CONFIG so
// the proxy can be deployed without writing to /etc.
func configPathFromEnv() string {
	if p := os.Getenv("LLM_PROXY_CONFIG"); p != "" {
		return p
	}
	return configPath
}

// apiKey returns the sophnet upstream key, preferring the SOPHNET_API_KEY env var
// so the key does not have to be stored in plaintext in a config file.
func apiKey() string {
	if k := os.Getenv("SOPHNET_API_KEY"); k != "" {
		return k
	}
	return cfg.Keys.Sophnet
}

func loadConfig() error {
	data, err := os.ReadFile(configPathFromEnv())
	if err != nil {
		return fmt.Errorf("read config %s: %w", configPathFromEnv(), err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	// defaults
	if cfg.Proxy.Port == 0 {
		cfg.Proxy.Port = 8088
	}
	if cfg.Proxy.VLMModel == "" {
		cfg.Proxy.VLMModel = "Qwen3.5-397B-A17B"
	}
	if cfg.Proxy.VLMMaxTokens == 0 {
		cfg.Proxy.VLMMaxTokens = 8000
	}
	if cfg.Upstream.AnthropicURL == "" {
		cfg.Upstream.AnthropicURL = "https://www.sophnet.com/api/open-apis/anthropic"
	}
	if cfg.Upstream.OpenAIURL == "" {
		cfg.Upstream.OpenAIURL = "https://www.sophnet.com/api/open-apis/openai"
	}
	if cfg.Routing.Sonnet == "" {
		cfg.Routing.Sonnet = "DeepSeek-V4-Pro"
	}
	if cfg.Routing.Opus == "" {
		cfg.Routing.Opus = "GLM-5.2"
	}
	if cfg.Routing.Haiku == "" {
		cfg.Routing.Haiku = cfg.Routing.Sonnet
	}

	return nil
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

func httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression:    true,
			ResponseHeaderTimeout: 180 * time.Second,
		},
		Timeout: 0,
	}
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("config: %v", err)
	}

	http.HandleFunc("/v1/messages", handleMessages)
	http.HandleFunc("/v1/chat/completions", handleChatCompletions)

	log.Printf("proxy-go :%d | sonnet->%s opus->%s haiku->%s vlm=%s\n",
		cfg.Proxy.Port, cfg.Routing.Sonnet, cfg.Routing.Opus, cfg.Routing.Haiku, cfg.Proxy.VLMModel)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", cfg.Proxy.Port), nil))
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "read body failed", 500)
		return
	}

	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if m, ok := req["model"].(string); ok {
		model = m
	}

	// Routing strategy:
	//   - text-only requests go to the mapped text model (LLM)
	//   - image-carrying requests first send each image to the VLM for a
	//     description, replace the image blocks with text describing them, then
	//     route the now text-only request to the text model
	//   - if the VLM describe pass fails (upstream error / timeout), fall back to
	//     routing the original request to the VLM so the images are still handled
	newModel := routeModel(model)
	if containsImage(req) && cfg.Proxy.VLMModel != "" {
		if describeImages(req) {
			body, _ = json.Marshal(req)
		} else {
			// Describe failed partway (some images replaced, some not): restore the
			// original request and route the whole thing to the VLM so no image is lost.
			json.Unmarshal(body, &req)
			newModel = cfg.Proxy.VLMModel
		}
	}
	if newModel != "" {
		req["model"] = newModel
		body, _ = json.Marshal(req)
	}

	// Text models reject thinking mode: they require content[].thinking from the
	// prior turn to be passed back verbatim, which Claude Code omits → 400
	// "content[].thinking in the thinking mode must be passed back to the API".
	// Strip thinking blocks so the request is acceptable to the text upstream.
	if stripped, changed := stripThinking(body); changed {
		json.Unmarshal(stripped, &req)
		body = stripped
		log.Printf("[THINKING] stripped thinking blocks\n")
	}

	log.Printf("[%s] %s -> %s len=%d\n", time.Now().Format("15:04:05"), model, newModel, len(body))

	resp, err := doUpstreamRequest(body, r)
	if err != nil {
		log.Printf("[RESP] error: %v\n", err)
		http.Error(w, "upstream error", 502)
		return
	}

	// Fallback: if the text upstream rejects an image-carrying request that static
	// detection missed (400 "Model do not support image input"), retry once with the
	// VLM model. A non-image 400 passes through unchanged.
	if resp.StatusCode == 400 && cfg.Proxy.VLMModel != "" && newModel != cfg.Proxy.VLMModel {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(respBody), "do not support image") {
			log.Printf("[RETRY] image 400 -> vlm %s\n", cfg.Proxy.VLMModel)
			req["model"] = cfg.Proxy.VLMModel
			body, _ = json.Marshal(req)
			resp, err = doUpstreamRequest(body, r)
			if err != nil {
				log.Printf("[RESP] retry error: %v\n", err)
				http.Error(w, "upstream error", 502)
				return
			}
		} else {
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
		}
	}
	defer resp.Body.Close()

	log.Printf("[RESP] status=%d\n", resp.StatusCode)

	// Empty response → error event for retry
	if resp.ContentLength == 0 {
		log.Printf("[RESP] empty body\n")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"empty_response\",\"message\":\"upstream returned empty\"}}\n\n")
		return
	}

	// Copy headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	fw := &flushWriter{w: w, f: flusher}

	var buf bytes.Buffer
	totalBytes, _ := io.Copy(io.MultiWriter(fw, &buf), resp.Body)

	// Safety net: only SSE streams may be missing the closing message_stop frame.
	// Non-streaming JSON replies must pass through untouched, or the appended SSE
	// footer corrupts the body into invalid JSON ("API Error: Failed to parse JSON").
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if isSSE && !strings.Contains(buf.String(), "message_stop") {
		log.Printf("[STREAM_END] + safety_stop bytes=%d\n", totalBytes)
		fmt.Fprintf(fw, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	} else {
		log.Printf("[STREAM_END] ok bytes=%d\n", totalBytes)
	}
}

func routeModel(model string) string {
	if strings.Contains(model, "opus") {
		return cfg.Routing.Opus
	}
	if strings.Contains(model, "haiku") {
		return cfg.Routing.Haiku
	}
	if strings.Contains(model, "sonnet") {
		return cfg.Routing.Sonnet
	}
	return ""
}

// doUpstreamRequest forwards the (already model-routed) body to the Anthropic
// upstream and returns the response. Reused for the initial attempt and the VLM
// image-fallback retry.
func doUpstreamRequest(body []byte, r *http.Request) (*http.Response, error) {
	proxyReq, err := http.NewRequest("POST", cfg.Upstream.AnthropicURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("x-api-key", apiKey())
	proxyReq.Header.Set("anthropic-version", "2023-06-01")
	proxyReq.Header.Set("Accept", "application/json")
	if beta := r.Header.Get("anthropic-beta"); beta != "" {
		proxyReq.Header.Set("anthropic-beta", beta)
	}
	return httpClient().Do(proxyReq)
}

// stripThinking removes `thinking` content blocks from every message. Text
// upstreams that do not support thinking mode reject requests that carry thinking
// blocks but omit the required pass-back, so they are dropped before forwarding.
// Returns the modified body and whether anything changed.
func stripThinking(body []byte) ([]byte, bool) {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}
	messages, ok := req["messages"].([]interface{})
	if !ok {
		return body, false
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		filtered := content[:0]
		for _, block := range content {
			b, ok := block.(map[string]interface{})
			if ok && b["type"] == "thinking" {
				changed = true
				continue
			}
			filtered = append(filtered, block)
		}
		msg["content"] = filtered
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

// containsImage reports whether any message in the request carries an image block
// (Anthropic "image" or OpenAI "image_url"), which text-only upstreams reject.
// The scan recurses into nested content (tool results, arrays) because Claude Code
// routinely wraps screenshots inside tool_result blocks.
func containsImage(req map[string]interface{}) bool {
	messages, ok := req["messages"].([]interface{})
	if !ok {
		return false
	}
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if contentHasImage(msg["content"]) {
			return true
		}
	}
	return false
}

func contentHasImage(v interface{}) bool {
	switch c := v.(type) {
	case string:
		return false
	case []interface{}:
		for _, item := range c {
			if blockHasImage(item) {
				return true
			}
		}
	}
	return false
}

// describeImages replaces every image block in the request with a text block that
// carries the VLM's description of that image ("这里有一个 image，其内容如下：xxx").
// The image itself is decoded to base64 and sent to the VLM. Returns false when the
// describe pass fails (e.g. upstream unavailable), in which case the caller falls
// back to routing the unmodified request to the VLM model.
func describeImages(req map[string]interface{}) bool {
	messages, hasMessages := req["messages"].([]interface{})
	if !hasMessages {
		return true
	}
	ok := true
	for _, m := range messages {
		msg, isMap := m.(map[string]interface{})
		if !isMap {
			continue
		}
		if !describeContent(msg["content"]) {
			ok = false
		}
	}
	return ok
}

func describeContent(v interface{}) bool {
	switch c := v.(type) {
	case string:
		return true
	case []interface{}:
		for i, item := range c {
			if !describeBlock(item) {
				return false
			}
			if b, isMap := item.(map[string]interface{}); isMap && isImageBlock(b) {
				block, ok := imageDescriptionBlock(b)
				if !ok {
					return false
				}
				c[i] = block
			}
		}
		return true
	}
	return true
}

func describeBlock(v interface{}) bool {
	switch b := v.(type) {
	case map[string]interface{}:
		if isImageBlock(b) {
			return true
		}
		return describeContent(b["content"])
	case []interface{}:
		return describeContent(b)
	}
	return true
}

func isImageBlock(b map[string]interface{}) bool {
	switch b["type"] {
	case "image", "image_url":
		return true
	}
	return false
}

// imageDescriptionBlock asks the VLM to describe the image and returns a text block
// carrying that description. ok is false when the describe call failed, signalling
// the caller to fall back to VLM routing instead of losing the image.
func imageDescriptionBlock(b map[string]interface{}) (block map[string]interface{}, ok bool) {
	desc, ok := describeImageWithVLM(b)
	if !ok {
		return nil, false
	}
	return map[string]interface{}{
		"type": "text",
		"text": fmt.Sprintf("这里有一个 image，其内容如下：%s", desc),
	}, true
}

// imageBlockFromDataURL converts a `data:<media_type>;base64,<data>` URL into an
// Anthropic image block. Returns nil if the URL is not a base64 data URL.
func imageBlockFromDataURL(url string) map[string]interface{} {
	i := strings.Index(url, ",")
	if i < 0 {
		return nil
	}
	meta := strings.TrimPrefix(url[:i], "data:")
	parts := strings.Split(meta, ";")
	mediaType := ""
	for _, p := range parts {
		if strings.HasPrefix(p, "image/") {
			mediaType = p
			break
		}
	}
	if mediaType == "" {
		return nil
	}
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": mediaType,
			"data":       url[i+1:],
		},
	}
}

// extractTextFromResponse concatenates the top-level text blocks from a
// non-streaming Anthropic /v1/messages response.
func extractTextFromResponse(respBody []byte) string {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func blockHasImage(v interface{}) bool {
	switch b := v.(type) {
	case map[string]interface{}:
		switch b["type"] {
		case "image", "image_url":
			return true
		}
		// Recurse into wrapped content (e.g. tool_result.content, nested arrays).
		return contentHasImage(b["content"])
	case []interface{}:
		for _, item := range b {
			if blockHasImage(item) {
				return true
			}
		}
	}
	return false
}

// imageDescCache memoizes VLM descriptions keyed by the hash of the image bytes.
// Conversation history is resent in full on every request, so without this cache a
// screenshot that appears in the history is re-described on every agent turn — the
// same image would trigger a VLM call N times. The cache is capped at 20MB of
// combined payload + description bytes to bound process memory.
type imageDescCache struct {
	mu      sync.Mutex
	max     int
	size    int
	entries map[string]*imgCacheEntry
	head    *imgCacheEntry
	tail    *imgCacheEntry
}

type imgCacheEntry struct {
	key  string
	desc string
	size int
	prev *imgCacheEntry
	next *imgCacheEntry
}

var descCache = &imageDescCache{
	max:     20 * 1024 * 1024,
	entries: map[string]*imgCacheEntry{},
}

// resetImageDescCacheForTests clears the cache. Tests share the global cache, so a
// cached description from one test would mask the VLM call in another.
func resetImageDescCacheForTests() {
	descCache.mu.Lock()
	defer descCache.mu.Unlock()
	descCache.entries = map[string]*imgCacheEntry{}
	descCache.size = 0
	descCache.head = nil
	descCache.tail = nil
}

// get returns the cached description for key, or "" on miss.
func (c *imageDescCache) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return ""
	}
	// Move to front (LRU).
	if e != c.head {
		if e.prev != nil {
			e.prev.next = e.next
		}
		if e.next != nil {
			e.next.prev = e.prev
		}
		if e == c.tail {
			c.tail = e.prev
		}
		e.prev = nil
		e.next = c.head
		c.head.prev = e
		c.head = e
	}
	return e.desc
}

// put stores desc under key, evicting least-recently-used entries while total size
// exceeds the cap.
func (c *imageDescCache) put(key, desc string, size int) {
	if size > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.desc = desc
		e.size = size
		c.size = c.size - e.size + size
		return
	}
	e := &imgCacheEntry{key: key, desc: desc, size: size}
	c.entries[key] = e
	if c.head == nil {
		c.head, c.tail = e, e
	} else {
		e.next = c.head
		c.head.prev = e
		c.head = e
	}
	c.size += size
	for c.size > c.max && c.tail != nil {
		c.removeLocked(c.tail)
	}
}

// removeLocked drops e from the LRU list and the map. Caller holds the lock.
func (c *imageDescCache) removeLocked(e *imgCacheEntry) {
	delete(c.entries, e.key)
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if c.head == e {
		c.head = e.next
	}
	if c.tail == e {
		c.tail = e.prev
	}
	c.size -= e.size
}

func (c *imageDescCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// imageCacheKey returns a stable cache key for an image block: the sha256 of the
// encoded image payload. Remote image_urls (non-data URLs) have no payload to hash,
// so they always miss and go to the VLM.
func imageCacheKey(block map[string]interface{}) (string, bool) {
	img := block
	if b, ok := block["type"].(string); ok && b == "image_url" {
		if u, ok := block["image_url"].(map[string]interface{}); ok {
			if url, ok := u["url"].(string); ok && strings.HasPrefix(url, "data:") {
				if anthro := imageBlockFromDataURL(url); anthro != nil {
					img = anthro
				} else {
					return "", false
				}
			} else {
				return "", false
			}
		} else {
			return "", false
		}
	}

	src, ok := img["source"].(map[string]interface{})
	if !ok {
		return "", false
	}
	data, ok := src["data"].(string)
	if !ok || data == "" {
		return "", false
	}
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:]), true
}

// describeImageWithVLM calls the VLM model with the single image block and returns
// the model's description. The result is memoized by image content hash so repeated
// turns that resend the same image reuse the description instead of re-calling VLM.
func describeImageWithVLM(block map[string]interface{}) (string, bool) {
	img := block
	// OpenAI-style image_url with a data URL must be converted to an Anthropic
	// image block, or the upstream rejects it. Non-data image_urls (remote URLs)
	// are passed through as-is.
	if b, ok := block["type"].(string); ok && b == "image_url" {
		if u, ok := block["image_url"].(map[string]interface{}); ok {
			if url, ok := u["url"].(string); ok && strings.HasPrefix(url, "data:") {
				if anthro := imageBlockFromDataURL(url); anthro != nil {
					img = anthro
				}
			}
		}
	}

	// Cache lookup before any network call.
	if key, ok := imageCacheKey(block); ok {
		if desc := descCache.get(key); desc != "" {
			log.Printf("[VLM] cached image description hit len=%d\n", len(desc))
			return desc, true
		}
	}

	req := map[string]interface{}{
		"model":      cfg.Proxy.VLMModel,
		"max_tokens": cfg.Proxy.VLMMaxTokens,
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					img,
					map[string]interface{}{"type": "text", "text": "请详细描述这张图片的内容。"},
				},
			},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", false
	}

	httpReq, err := http.NewRequest("POST", cfg.Upstream.AnthropicURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey())
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(httpReq)
	if err != nil {
		log.Printf("[VLM] describe error: %v\n", err)
		return "", false
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("[VLM] describe status=%d body=%s\n", resp.StatusCode, truncate(string(respBody), 200))
		return "", false
	}

	desc := extractTextFromResponse(respBody)
	if desc == "" {
		log.Printf("[VLM] describe returned empty text\n")
		return "", false
	}
	// Memoize the description so the same image in a later turn does not re-call VLM.
	if key, ok := imageCacheKey(block); ok {
		// Entry size: hashed payload length + description length.
		entrySize := len(key) + len(desc)
		descCache.put(key, desc, entrySize)
		log.Printf("[VLM] described image: %s\n", truncate(desc, 200))
	} else {
		log.Printf("[VLM] described image (uncacheable): %s\n", truncate(desc, 200))
	}
	return desc, true
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "read body failed", 500)
		return
	}

	proxyReq, _ := http.NewRequest("POST", cfg.Upstream.OpenAIURL+"/v1/chat/completions", bytes.NewReader(body))
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Authorization", "Bearer "+apiKey())

	resp, err := httpClient().Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error", 502)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

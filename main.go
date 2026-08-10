package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
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
	Port     int    `toml:"port"`
	VLMModel string `toml:"vlm_model"`
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

	// Route model: image-carrying requests go to the VLM model (text models like
	// DeepSeek reject image input with 400 InvalidParameter). Otherwise map by name.
	newModel := ""
	if containsImage(req) && cfg.Proxy.VLMModel != "" {
		newModel = cfg.Proxy.VLMModel
	} else {
		newModel = routeModel(model)
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

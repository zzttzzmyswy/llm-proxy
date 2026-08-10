package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withUpstreamCapture(t *testing.T, contentType, body string, capture func(*http.Request)) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture(r)
		}
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)

	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })
	return upstream
}

// modelSentToUpstream returns the "model" field of the request body the upstream received.
func modelSentToUpstream(t *testing.T, reqBody string) string {
	t.Helper()
	var got string
	withUpstreamCapture(t, "application/json", nonStreamJSONBody, func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		if json.Unmarshal(b, &m) == nil {
			got, _ = m["model"].(string)
		}
	})
	callHandleMessages(t, reqBody)
	return got
}

const nonStreamJSONBody = `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","model":"DeepSeek-V4-Flash-0731","id":"test-1","usage":{"input_tokens":5,"output_tokens":5}}`

func withUpstream(t *testing.T, contentType, body string) *httptest.Server {
	return withUpstreamCapture(t, contentType, body, nil)
}

func callHandleMessages(t *testing.T, reqBody string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleMessages(w, req)
	return w.Body.String()
}

// An Anthropic request carrying an image block must be routed to the VLM model,
// not the text model that rejects image input.
func TestImageRequestRoutedToVLM(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	t.Cleanup(func() { cfg.Proxy.VLMModel = "" })

	got := modelSentToUpstream(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"describe"}]}]}`)

	if got != "MiniMax-M3" {
		t.Fatalf("image request should route to VLM model, upstream received %q", got)
	}
}

// An Anthropic request without images keeps the text routing.
func TestTextRequestKeepsTextRouting(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	got := modelSentToUpstream(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"describe this"}]}`)

	if got != cfg.Routing.Sonnet {
		t.Fatalf("text request should keep text routing, upstream received %q", got)
	}
}

// OpenAI-style image_url blocks (Claude Code adapters) must also route to the VLM.
func TestImageURLBlockRoutedToVLM(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	t.Cleanup(func() { cfg.Proxy.VLMModel = "" })

	got := modelSentToUpstream(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"text","text":"describe"}]}]}`)

	if got != "MiniMax-M3" {
		t.Fatalf("image_url request should route to VLM model, upstream received %q", got)
	}
}

// A request named "haiku" must be routed to the haiku target when configured.
func TestHaikuExplicitRouting(t *testing.T) {
	cfg.Routing.Haiku = "GLM-5.2"
	t.Cleanup(func() { cfg.Routing.Haiku = "" })

	got := modelSentToUpstream(t, `{"model":"haiku","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if got != "GLM-5.2" {
		t.Fatalf("haiku request should route to configured haiku target, upstream received %q", got)
	}
}

// When the config declares no haiku, loadConfig must fall back to the sonnet target.
func TestHaikuFallsBackToSonnetWhenUnconfigured(t *testing.T) {
	before := cfg
	defer func() { cfg = before }()

	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Routing.Haiku == "" {
		t.Fatal("haiku must fall back to a non-empty target")
	}
	if cfg.Routing.Haiku != cfg.Routing.Sonnet {
		t.Fatalf("unconfigured haiku must fall back to sonnet target, got haiku=%q sonnet=%q", cfg.Routing.Haiku, cfg.Routing.Sonnet)
	}
}

// Regression: a non-streaming JSON reply must pass through untouched. Before the
// fix, the safety-net appended SSE framing, corrupting the JSON into "JSON + SSE tail".
func TestNonStreamJSONResponseUnmodified(t *testing.T) {
	withUpstream(t, "application/json", nonStreamJSONBody)

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if !json.Valid([]byte(resp)) {
		t.Fatalf("non-streaming response must remain valid JSON, got: %q", resp)
	}
	if strings.Contains(resp, "event: message_stop") {
		t.Fatalf("non-streaming response must not get SSE footer appended, got: %q", resp)
	}
	if resp != nonStreamJSONBody {
		t.Fatalf("non-streaming response must be passed through byte-identical, got: %q", resp)
	}
}

// An SSE stream that is missing message_stop still gets the safety footer.
func TestSSEMissingStopGetsFooter(t *testing.T) {
	withUpstream(t, "text/event-stream", "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(resp, "event: message_stop") {
		t.Fatalf("SSE stream missing stop frame should get footer appended, got: %q", resp)
	}
}

// An SSE stream that already ends with message_stop must not be duplicated.
func TestSSEWithStopNotDuplicated(t *testing.T) {
	withUpstream(t, "text/event-stream", "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if n := strings.Count(resp, "event: message_stop"); n != 1 {
		t.Fatalf("SSE stream with stop frame must not be duplicated, got %d occurrences: %q", n, resp)
	}
}

// LLM_PROXY_CONFIG env var overrides the hardcoded config path.
func TestConfigPathFromEnv(t *testing.T) {
	t.Setenv("LLM_PROXY_CONFIG", "/tmp/llm-proxy-test-"+t.Name()+".toml")
	if configPathFromEnv() != "/tmp/llm-proxy-test-"+t.Name()+".toml" {
		t.Fatal("LLM_PROXY_CONFIG should override the default config path")
	}
}

// SOPHNET_API_KEY env var overrides the key from the config file.
func TestAPIKeyFromEnvOverridesConfig(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "env-key-123")
	t.Setenv("LLM_PROXY_CONFIG", "/nonexistent-"+t.Name()+".toml")
	cfg.Keys.Sophnet = "file-key"
	t.Cleanup(func() { cfg.Keys.Sophnet = "" })

	got := apiKey()
	if got != "env-key-123" {
		t.Fatalf("SOPHNET_API_KEY should override config key, got %q", got)
	}
}

// Without the env var, the key comes from the config file.
func TestAPIKeyFallsBackToConfig(t *testing.T) {
	t.Setenv("SOPHNET_API_KEY", "")
	cfg.Keys.Sophnet = "file-key"
	t.Cleanup(func() { cfg.Keys.Sophnet = "" })

	if got := apiKey(); got != "file-key" {
		t.Fatalf("config key should be used when env var is empty, got %q", got)
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// An Anthropic request carrying an image block must first have its images described
// by the VLM, then route the now-text-only request to the text model.
func TestImageRequestDescribedThenRoutedToText(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	got := modelSentToUpstream(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"describe"}]}]}`)

	if got != cfg.Routing.Sonnet {
		t.Fatalf("image request should route to text model after describe, upstream received %q", got)
	}
}

// A request carrying an image must have the image replaced by a VLM description
// (text block "这里有一个 image，其内容如下：...") before being sent to the text model.
func TestImageReplacedWithVLMDescription(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var describeCalls int
	var finalBody string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		if json.Unmarshal(b, &m) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			describeCalls++
			// VLM describe reply
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"这是 MiniMax 对图片的描述。"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		finalBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"describe"}]}]}`
	callHandleMessages(t, reqBody)

	mu.Lock()
	defer mu.Unlock()
	if describeCalls != 1 {
		t.Fatalf("expected exactly 1 VLM describe call, got %d", describeCalls)
	}
	if !strings.Contains(finalBody, "这里有一个 image，其内容如下：这是 MiniMax 对图片的描述。") {
		t.Fatalf("final request must carry the VLM description inserted in place of the image, got: %s", finalBody)
	}
	if strings.Contains(finalBody, `"type":"image"`) {
		t.Fatalf("final request must not carry the original image block, got: %s", finalBody)
	}
	var m map[string]interface{}
	json.Unmarshal([]byte(finalBody), &m)
	if mm, _ := m["model"].(string); mm != "DeepSeek-V4-Flash-0731" {
		t.Fatalf("final request must go to the text model, got model %q", mm)
	}
}

// When the VLM describe call fails (non-200), the request must fall back to routing
// to the VLM model unchanged so the image is not lost.
func TestImageDescribeFailFallsBackToVLM(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var calls int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		mu.Lock()
		calls++
		n := calls
		model, _ := m["model"].(string)
		mu.Unlock()
		if model == "MiniMax-M3" {
			if n == 1 {
				// First MiniMax call is the describe pass: fail it.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(500)
				io.WriteString(w, `{"error":{"message":"vlm down"}}`)
				return
			}
			// Second MiniMax call is the VLM fallback: succeed with an image-aware reply.
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"vlm fallback ok"}],"model":"MiniMax-M3","id":"vlm-2","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	resp := callHandleMessages(t, reqBody)

	mu.Lock()
	defer mu.Unlock()
	// describe (1, fails) + fallback to VLM (2)
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (describe fail + VLM fallback), got %d", calls)
	}
	if !strings.Contains(resp, "vlm fallback ok") {
		t.Fatalf("expected the VLM fallback reply, got: %q", resp)
	}
}

// OpenAI-style image_url data URLs must also be described by the VLM, then routed
// to the text model. The VLM describe call must convert the data URL to an Anthropic
// image block.
func TestImageURLDataDescribedThenRoutedToText(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var describeBody string
	var finalModel string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			describeBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"图片描述结果"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		finalModel = model
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUFB"}},{"type":"text","text":"describe"}]}]}`
	callHandleMessages(t, reqBody)

	mu.Lock()
	defer mu.Unlock()
	if finalModel != "DeepSeek-V4-Flash-0731" {
		t.Fatalf("image_url request should route to text model after describe, got %q", finalModel)
	}
	if !strings.Contains(describeBody, `"type":"image"`) {
		t.Fatalf("VLM describe call should convert data URL to Anthropic image block, got: %s", describeBody)
	}
	if !strings.Contains(describeBody, `"media_type":"image/png"`) {
		t.Fatalf("VLM describe call should carry media_type, got: %s", describeBody)
	}
}

// Images nested inside tool_result content must be described and replaced too.
func TestNestedToolResultImageDescribed(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var finalBody string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"截图描述"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		finalBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`
	callHandleMessages(t, reqBody)

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(finalBody, "这里有一个 image，其内容如下：截图描述") {
		t.Fatalf("nested tool_result image must be replaced with VLM description, got: %s", finalBody)
	}
	if strings.Contains(finalBody, `"type":"image"`) {
		t.Fatalf("nested image must be replaced, got: %s", finalBody)
	}
}

// A request with multiple images must describe each and replace them all in order.
func TestMultipleImagesAllDescribed(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var describeCount int
	var finalBody string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			describeCount++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"描述"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		finalBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A"}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"B"}}]}]}`
	callHandleMessages(t, reqBody)

	mu.Lock()
	defer mu.Unlock()
	if describeCount != 2 {
		t.Fatalf("expected 2 VLM describe calls, got %d", describeCount)
	}
	if n := strings.Count(finalBody, "这里有一个 image，其内容如下：描述"); n != 2 {
		t.Fatalf("expected 2 descriptions inserted, got %d: %s", n, finalBody)
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

func TestContainsImageNested(t *testing.T) {
	cases := []struct {
		name string
		req  string
		want bool
	}{
		{"tool_result wraps image", `{"model":"sonnet","messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","data":"x"}}]}]}]}`, true},
		{"openai tool message wraps image_url", `{"model":"sonnet","messages":[{"role":"tool","content":[{"type":"image_url","image_url":{"url":"https://x"}}]}]}`, true},
		{"plain text still false", `{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`, false},
		{"nested array in content", `{"model":"sonnet","messages":[{"role":"user","content":[[{"type":"image","source":{"type":"base64","data":"x"}}]]}]}`, true},
	}
	for _, c := range cases {
		var req map[string]interface{}
		if err := json.Unmarshal([]byte(c.req), &req); err != nil {
			t.Fatalf("%s: bad json: %v", c.name, err)
		}
		if got := containsImage(req); got != c.want {
			t.Errorf("%s: containsImage = %v, want %v", c.name, got, c.want)
		} else {
			t.Logf("%s: ok (got %v)", c.name, got)
		}
	}
}

// stripThinking removes thinking blocks from the outgoing request. DeepSeek-family
// text models reject the thinking mode: they require `content[].thinking` from the
// prior turn to be passed back verbatim, which Claude Code omits → 400
// "content[].thinking in the thinking mode must be passed back to the API".
func TestStripThinkingRemovesUserThinkingBlocks(t *testing.T) {
	body := `{"model":"sonnet","messages":[{"role":"user","content":[{"type":"thinking","thinking":"secret chain"},{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"done"}]}]}`
	got, changed := stripThinking([]byte(body))
	if !changed {
		t.Fatal("stripThinking should report changed=true when thinking blocks are removed")
	}
	if strings.Contains(string(got), `"type":"thinking"`) {
		t.Fatalf("thinking blocks must be removed, got: %s", got)
	}
	if !strings.Contains(string(got), `"type":"text"`) {
		t.Fatalf("text blocks must be preserved, got: %s", got)
	}
}

func TestStripThinkingLeavesPlainRequestUntouched(t *testing.T) {
	body := `{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`
	got, changed := stripThinking([]byte(body))
	if changed {
		t.Fatal("stripThinking should report changed=false when no thinking blocks exist")
	}
	if string(got) != body {
		t.Fatalf("stripThinking must not alter requests without thinking blocks, got: %s", got)
	}
}

func TestStripThinkingNoThinkingContent(t *testing.T) {
	body := `{"model":"sonnet","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	_, changed := stripThinking([]byte(body))
	if changed {
		t.Fatal("stripThinking should report changed=false when content has no thinking blocks")
	}
}

// 400 "Model do not support image input" on the first attempt must transparently
// retry with the VLM model and return the successful reply.
func TestImageRejectRetriesWithVLM(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var mu sync.Mutex
	models := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		if json.Unmarshal(b, &m) == nil {
			mu.Lock()
			models = append(models, m["model"].(string))
			mu.Unlock()
		}
		if m["model"] == "DeepSeek-V4-Flash-0731" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"Model do not support image input","param":"image_url","code":"InvalidParameter"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}],"model":"MiniMax-M3","id":"r1","usage":{"input_tokens":5,"output_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	// A request whose image is missed by static detection (e.g. hidden in an
	// unexpected nesting), sent as sonnet → DeepSeek → 400 → must retry as MiniMax.
	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`
	// containsImage DOES detect this shape, so force the missed case through an
	// undetectable wrapper to prove the retry path is independent of detection.
	reqBody = `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}}]}]}`

	resp := callHandleMessages(t, reqBody)
	if resp == "" {
		t.Fatal("expected a successful reply after VLM retry, got empty")
	}
	if !strings.Contains(resp, `"ok"`) {
		t.Fatalf("expected VLM success body, got: %q", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(models) != 2 || models[0] != "DeepSeek-V4-Flash-0731" || models[1] != "MiniMax-M3" {
		t.Fatalf("expected retry sequence [DeepSeek, MiniMax], got %v", models)
	}
}

// A non-image 400 must NOT trigger the VLM retry.
func TestNonImage400DoesNotRetry(t *testing.T) {
	cfg.Proxy.VLMModel = "MiniMax-M3"
	t.Cleanup(func() { cfg.Proxy.VLMModel = "" })

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"some other error"}}`)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if calls != 1 {
		t.Fatalf("non-image 400 must not retry, got %d upstream calls", calls)
	}
	if resp == "" {
		t.Fatal("expected the 400 body to pass through")
	}
	if !strings.Contains(resp, "some other error") {
		t.Fatalf("expected the original 400 body, got: %q", resp)
	}
}

// When the VLM model is not configured, an image 400 must pass through unchanged.
func TestImage400WithoutVLMConfigPassesThrough(t *testing.T) {
	cfg.Proxy.VLMModel = ""
	t.Cleanup(func() { cfg.Proxy.VLMModel = "" })

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"Model do not support image input"}}`)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if calls != 1 {
		t.Fatalf("without VLM config there must be no retry, got %d upstream calls", calls)
	}
}

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
	resetImageDescCacheForTests()
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
	resetImageDescCacheForTests()
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
	resetImageDescCacheForTests()
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
	resetImageDescCacheForTests()
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
	resetImageDescCacheForTests()
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
	resetImageDescCacheForTests()
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

// The same image repeated across turns must be described only once: the second
// request reuses the cached description without calling the VLM again.
func TestImageDescriptionCachedAcrossRequests(t *testing.T) {
	resetImageDescCacheForTests()
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var describeCalls int
	var finalBodies []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			describeCalls++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"稳定的图片描述"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		finalBodies = append(finalBodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	reqBody := `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"SAMEIMAGE"}},{"type":"text","text":"describe"}]}]}`
	callHandleMessages(t, reqBody) // first turn: VLM call, cached
	callHandleMessages(t, reqBody) // second turn: cache hit

	mu.Lock()
	defer mu.Unlock()
	if describeCalls != 1 {
		t.Fatalf("same image across turns must be described once, got %d VLM calls", describeCalls)
	}
	if len(finalBodies) != 2 {
		t.Fatalf("expected 2 final upstream calls, got %d", len(finalBodies))
	}
	for i, fb := range finalBodies {
		if !strings.Contains(fb, "这里有一个 image，其内容如下：稳定的图片描述") {
			t.Fatalf("turn %d must carry the cached description, got: %s", i+1, fb)
		}
	}
}

// Different images must each be described; the cache must not conflate them.
func TestDistinctImagesBothDescribed(t *testing.T) {
	resetImageDescCacheForTests()
	cfg.Proxy.VLMModel = "MiniMax-M3"
	cfg.Routing.Sonnet = "DeepSeek-V4-Flash-0731"
	t.Cleanup(func() {
		cfg.Proxy.VLMModel = ""
		cfg.Routing.Sonnet = ""
	})

	var describeCalls int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		model, _ := m["model"].(string)
		if model == "MiniMax-M3" {
			describeCalls++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"desc"}],"model":"MiniMax-M3","id":"vlm-1","usage":{"input_tokens":5,"output_tokens":5}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, nonStreamJSONBody)
	}))
	t.Cleanup(upstream.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = upstream.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMG-A"}}]}]}`)
	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMG-B"}}]}]}`)

	mu.Lock()
	defer mu.Unlock()
	if describeCalls != 2 {
		t.Fatalf("distinct images must each be described, got %d VLM calls", describeCalls)
	}
}

// The cache must not grow without bound: total entry bytes stay under the 20MB cap.
func TestImageDescCacheEvictionEnforcesCap(t *testing.T) {
	resetImageDescCacheForTests()

	oldMax := descCache.max
	descCache.max = 100 // tiny cap for test
	t.Cleanup(func() { descCache.max = oldMax })

	// A single entry larger than the cap is not stored.
	bigKey := strings.Repeat("k", 40)
	descCache.put(bigKey, strings.Repeat("d", 200), 200)
	if got := descCache.get(bigKey); got != "" {
		t.Fatalf("entry larger than the cap must not be stored, got %q", got)
	}

	// Two entries that together exceed the cap evict the LRU one.
	descCache.put("a", "a-desc", 60)
	descCache.put("b", "b-desc", 60)
	if got := descCache.get("a"); got != "" {
		t.Fatalf("LRU entry a should be evicted once the cap is exceeded, got %q", got)
	}
	if got := descCache.get("b"); got != "b-desc" {
		t.Fatalf("most-recent entry b must survive, got %q", got)
	}

	// Total size never exceeds the cap after eviction.
	descCache.mu.Lock()
	under := descCache.size <= descCache.max
	descCache.mu.Unlock()
	if !under {
		t.Fatalf("cache size %d exceeds cap %d", descCache.size, descCache.max)
	}
}

// imageCacheKey must return a stable key for the same image and miss for remote URLs.
func TestImageCacheKey(t *testing.T) {
	resetImageDescCacheForTests()

	var blk map[string]interface{}
	json.Unmarshal([]byte(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ABCD"}}`), &blk)
	k1, ok := imageCacheKey(blk)
	if !ok || k1 == "" {
		t.Fatalf("base64 image must produce a cache key, got %q ok=%v", k1, ok)
	}
	// Same data → same key.
	var blk2 map[string]interface{}
	json.Unmarshal([]byte(`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"ABCD"}}`), &blk2)
	k2, _ := imageCacheKey(blk2)
	if k2 != k1 {
		t.Fatalf("same image bytes must produce the same key, got %q vs %q", k1, k2)
	}
	// Different data → different key.
	var blk3 map[string]interface{}
	json.Unmarshal([]byte(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ABCE"}}`), &blk3)
	k3, _ := imageCacheKey(blk3)
	if k3 == k1 {
		t.Fatalf("different image bytes must produce different keys, got %q", k1)
	}
	// Remote image_url (no payload) → no key.
	var blk4 map[string]interface{}
	json.Unmarshal([]byte(`{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}`), &blk4)
	if k, ok := imageCacheKey(blk4); ok {
		t.Fatalf("remote image_url must not be cacheable, got key %q", k)
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
	resetImageDescCacheForTests()
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

// stripThinking must flip the `thinking` parameter to disabled when it strips
// thinking blocks, so the upstream does not stream a thinking response back.
func TestStripThinkingDisablesThinkingParam(t *testing.T) {
	body := `{"model":"sonnet","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"hi"}]}]}`
	got, changed := stripThinking([]byte(body))
	if !changed {
		t.Fatal("stripThinking should report changed=true when thinking blocks are removed")
	}
	var req map[string]interface{}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("stripped body must be valid JSON: %v", err)
	}
	thinking, ok := req["thinking"].(map[string]interface{})
	if !ok {
		t.Fatal("thinking param must remain present")
	}
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking.type should be disabled after stripping, got %q", thinking["type"])
	}
}

// stripThinking with an adaptive thinking param must also disable it.
func TestStripThinkingDisablesAdaptiveParam(t *testing.T) {
	body := `{"model":"sonnet","thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"done"}]}]}`
	got, changed := stripThinking([]byte(body))
	if !changed {
		t.Fatal("stripThinking should report changed=true")
	}
	var req map[string]interface{}
	json.Unmarshal(got, &req)
	thinking := req["thinking"].(map[string]interface{})
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking.type should be disabled, got %q", thinking["type"])
	}
}

// A thinking block whose `thinking` field is missing must be pinned to an empty
// string so Claude Code does not crash on `.thinking.length`.
func TestNormalizeThinkingBlockMissingThinkingField(t *testing.T) {
	raw := json.RawMessage(`{"type":"thinking","signature":"sig123"}`)
	out, ok := normalizeThinkingBlock(raw)
	if !ok {
		t.Fatal("normalize should rewrite a thinking block missing the thinking field")
	}
	var block map[string]interface{}
	json.Unmarshal(out, &block)
	if block["thinking"] != "" {
		t.Fatalf("thinking field should be empty string, got %#v", block["thinking"])
	}
	if block["signature"] != "sig123" {
		t.Fatalf("signature must be preserved, got %#v", block["signature"])
	}
}

// A valid thinking block with a string thinking field must pass through unchanged.
func TestNormalizeThinkingBlockValidPassesThrough(t *testing.T) {
	raw := json.RawMessage(`{"type":"thinking","thinking":"hello","signature":"sig123"}`)
	if _, ok := normalizeThinkingBlock(raw); ok {
		t.Fatal("normalize should not rewrite a valid thinking block")
	}
}

// A non-thinking block must never be rewritten.
func TestNormalizeThinkingBlockNonThinking(t *testing.T) {
	raw := json.RawMessage(`{"type":"text","text":"hi"}`)
	if _, ok := normalizeThinkingBlock(raw); ok {
		t.Fatal("normalize should not rewrite a text block")
	}
}

// The SSE normalizing reader must rewrite a broken thinking content_block_start and
// leave the rest of the stream intact.
func TestThinkingNormalizingReaderRewritesBrokenThinkingStart(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"signature\":\"sig123\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out, err := io.ReadAll(newThinkingNormalizingReader(strings.NewReader(stream)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"thinking":""`) {
		t.Fatalf("thinking field should be normalized to empty string, got: %s", out)
	}
	if !strings.Contains(string(out), `"signature":"sig123"`) {
		t.Fatalf("signature must be preserved, got: %s", out)
	}
	if !strings.Contains(string(out), "event: message_stop") {
		t.Fatalf("stream tail must be preserved, got: %s", out)
	}
}

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// anthropicSSEWithOpenAIBilling is one streamed reply as the deployment's
// Anthropic gateway actually frames an OpenAI-backed model. The message_delta
// payload is captured verbatim from the live upstream
// (sophnet anthropic endpoint, deepseek-flash, warm turn): input_tokens 4281 is
// OpenAI's prompt_tokens with the cached 4224 counted inside, and
// billing_usage.semantic="openai" marks the convention. Summing the Anthropic
// counters therefore counts those 4224 tokens twice.
func anthropicSSEWithOpenAIBilling() string {
	return strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1745,\"output_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ACK\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4281,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":4224,\"output_tokens\":8,\"claude_cache_creation_5_m_tokens\":0,\"claude_cache_creation_1_h_tokens\":0,\"billing_usage\":{\"source\":\"oai_chat\",\"semantic\":\"openai\",\"openai_usage\":{\"prompt_tokens\":4281,\"completion_tokens\":8,\"total_tokens\":4289,\"prompt_tokens_details\":{\"cached_tokens\":4224,\"text_tokens\":0,\"audio_tokens\":0,\"image_tokens\":0},\"completion_tokens_details\":{\"text_tokens\":0,\"audio_tokens\":0,\"image_tokens\":0,\"reasoning_tokens\":8},\"input_tokens\":0,\"output_tokens\":0,\"input_tokens_details\":null,\"claude_cache_creation_5_m_tokens\":0,\"claude_cache_creation_1_h_tokens\":0}}},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")
}

// wireUsage folds the stream the way the DeepSeek Messages adapter does: later
// events overwrite earlier fields, then the prompt size is the sum of the three
// counters. This is the arithmetic behind the reported 49%.
type wireUsage struct {
	inputTokens  float64
	outputTokens float64
	cacheRead    float64
	cacheWrite   float64
}

func foldForClient(t *testing.T, stream string) wireUsage {
	t.Helper()
	var u wireUsage
	for _, block := range strings.Split(stream, "\n\n") {
		var data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if data == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Usage map[string]json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage map[string]json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		apply := func(m map[string]json.RawMessage) {
			for k, raw := range m {
				var v float64
				if json.Unmarshal(raw, &v) != nil {
					continue
				}
				switch k {
				case "input_tokens":
					u.inputTokens = v
				case "output_tokens":
					u.outputTokens = v
				case "cache_read_input_tokens":
					u.cacheRead = v
				case "cache_creation_input_tokens":
					u.cacheWrite = v
				}
			}
		}
		if ev.Message != nil {
			apply(ev.Message.Usage)
		}
		if ev.Usage != nil {
			apply(ev.Usage)
		}
	}
	return u
}

// The client-visible stream must carry the Anthropic convention. Left as the
// upstream sent it, a client that sums the counters (the DeepSeek Messages
// adapter dsh uses) reads the cached tokens twice and reports ~50% on a fully
// cached conversation — the reported 49%.
func TestWireUsageIsNormalisedForAnthropicClients(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "text/event-stream", anthropicSSEWithOpenAIBilling())

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	u := foldForClient(t, resp)
	if u.cacheRead != 4224 {
		t.Fatalf("the cache read must reach the client verbatim, got %v", u.cacheRead)
	}
	if u.inputTokens != 57 {
		t.Fatalf("input_tokens must be the fresh prompt (4281-4224=57), got %v\nstream:\n%s", u.inputTokens, resp)
	}
	prompt := u.inputTokens + u.cacheRead + u.cacheWrite
	// 4224 of the 4281 prompt tokens came from cache. Reading the raw upstream
	// block instead would put the prompt at 4281+4224 and report 0.497 — the
	// reported 49%.
	if prompt != 4281 {
		t.Fatalf("the prompt size must be the upstream's own total (4281), got %.0f", prompt)
	}
	if rate := u.cacheRead / prompt; rate < 0.98 {
		t.Fatalf("an almost fully cached conversation must read near 100%%, got %.4f", rate)
	}
	if strings.Contains(resp, "billing_usage") {
		t.Fatalf("the superseded marker must not survive the rewrite, got:\n%s", resp)
	}
}

// The proxy's own statistics must agree with what the client is told: the
// dashboard's hit rate is the same number dsh computes, from the same stream.
func TestStatsAgreeWithTheNormalisedWire(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "text/event-stream", anthropicSSEWithOpenAIBilling())

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "DeepSeek-Flash")
	if m.InputTokens != 57 || m.CacheRead != 4224 {
		t.Fatalf("stats must use the normalised counts, got in=%d cache_read=%d", m.InputTokens, m.CacheRead)
	}
	if m.CacheHitRate < 0.98 {
		t.Fatalf("a well-cached conversation must not be recorded as a ~50%% hit, got %.4f", m.CacheHitRate)
	}
}

// anthropicJSONWithOpenAIBilling is the non-streaming twin of the fixture above:
// the same upstream usage at the top level of a plain message reply. The payload
// is captured from the live upstream (non-stream, warm turn).
func anthropicJSONWithOpenAIBilling() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"DeepSeek-Flash",` +
		`"content":[{"type":"text","text":"ACK"}],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":4281,"cache_creation_input_tokens":0,"cache_read_input_tokens":4224,` +
		`"output_tokens":8,"billing_usage":{"source":"oai_chat","semantic":"openai",` +
		`"openai_usage":{"prompt_tokens":4281,"completion_tokens":8,` +
		`"prompt_tokens_details":{"cached_tokens":4224}}}}}`
}

// The non-streaming reply must be normalised the same way, or the same
// conversation reads as a 50% hit whenever the client does not stream.
func TestNonStreamUsageIsNormalisedForAnthropicClients(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "application/json", anthropicJSONWithOpenAIBilling())

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if !json.Valid([]byte(resp)) {
		t.Fatalf("the normalised reply must stay valid JSON, got: %q", resp)
	}
	var parsed struct {
		Usage struct {
			InputTokens float64 `json:"input_tokens"`
			CacheRead   float64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Usage.InputTokens != 57 {
		t.Fatalf("input_tokens must be the fresh prompt (4281-4224=57), got %v\nbody: %s", parsed.Usage.InputTokens, resp)
	}
	if parsed.Usage.CacheRead != 4224 {
		t.Fatalf("the cache read must reach the client verbatim, got %v", parsed.Usage.CacheRead)
	}
	if strings.Contains(resp, "billing_usage") {
		t.Fatalf("the superseded marker must not survive the rewrite, got:\n%s", resp)
	}

	m := findModel(t, "DeepSeek-Flash")
	if m.InputTokens != 57 || m.CacheRead != 4224 {
		t.Fatalf("stats must use the normalised counts, got in=%d cache_read=%d", m.InputTokens, m.CacheRead)
	}
	if m.CacheHitRate < 0.98 {
		t.Fatalf("the dashboard must not record a ~50%% hit, got %.4f", m.CacheHitRate)
	}
}

// A genuine Anthropic-served non-streaming reply carries no billing marker: it
// must pass through byte-identical, or the proxy would reorder the client's JSON.
func TestNonStreamUnmarkedReplyPassesThroughUnchanged(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "application/json", nonStreamJSONBody)

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if resp != nonStreamJSONBody {
		t.Fatalf("an unmarked reply must pass through byte-identical, got: %q", resp)
	}
}

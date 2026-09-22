package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestExtractAnthropicUsageNonStream(t *testing.T) {
	body := []byte(`{"type":"message","usage":{"input_tokens":120,"output_tokens":34}}`)
	got := extractAnthropicUsage(body, false)
	if !got.Reported || got.Input != 120 || got.Output != 34 {
		t.Fatalf("want 120/34 reported, got %+v", got)
	}
}

func TestExtractAnthropicUsageNonStreamWithoutUsage(t *testing.T) {
	got := extractAnthropicUsage([]byte(`{"type":"message","content":[]}`), false)
	if got.Reported || got.Input != 0 || got.Output != 0 {
		t.Fatalf("missing usage must yield a zero, unreported value, got %+v", got)
	}
}

// An SSE stream reports input tokens in message_start and the cumulative output
// total in message_delta, so the final output count must win over the initial one.
func TestExtractAnthropicUsageStream(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":900,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":250}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")
	got := extractAnthropicUsage([]byte(stream), true)
	if !got.Reported || got.Input != 900 || got.Output != 250 {
		t.Fatalf("want 900/250 reported, got %+v", got)
	}
}

func TestExtractOpenAIUsageNonStream(t *testing.T) {
	body := []byte(`{"id":"c1","usage":{"prompt_tokens":11,"completion_tokens":22}}`)
	got := extractOpenAIUsage(body, false)
	if !got.Reported || got.Input != 11 || got.Output != 22 {
		t.Fatalf("want 11/22 reported, got %+v", got)
	}
}

// Gateways that do not honour stream_options omit usage entirely; that must be
// reported as unreported rather than as a real zero.
func TestExtractOpenAIUsageStreamWithoutUsage(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	got := extractOpenAIUsage([]byte(stream), true)
	if got.Reported {
		t.Fatalf("stream without a usage chunk must be unreported, got %+v", got)
	}
}

func TestExtractOpenAIUsageStream(t *testing.T) {
	stream := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":9}}\n\n",
		"data: [DONE]\n\n",
	}, "")
	got := extractOpenAIUsage([]byte(stream), true)
	if !got.Reported || got.Input != 7 || got.Output != 9 {
		t.Fatalf("want 7/9 reported, got %+v", got)
	}
}

// The tail buffer must stay bounded and keep the newest bytes, which is where a
// streaming gateway puts its usage chunk.
func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := newTailBuffer(8)
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		if n, err := tb.Write([]byte(s)); err != nil || n != 4 {
			t.Fatalf("write %q: n=%d err=%v", s, n, err)
		}
	}
	if got := string(tb.Bytes()); got != "bbbbcccc" {
		t.Fatalf("tail buffer must keep the last 8 bytes, got %q", got)
	}
}

func TestTailBufferWriteLargerThanMax(t *testing.T) {
	tb := newTailBuffer(4)
	if _, err := tb.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if got := string(tb.Bytes()); got != "6789" {
		t.Fatalf("oversized write must keep only the tail, got %q", got)
	}
}

func TestTailBufferDisabled(t *testing.T) {
	tb := newTailBuffer(0)
	if _, err := tb.Write([]byte("anything")); err != nil {
		t.Fatal(err)
	}
	if len(tb.Bytes()) != 0 {
		t.Fatalf("a zero-size tail buffer must retain nothing, got %q", tb.Bytes())
	}
}

func TestForEachSSEDataSkipsDoneAndNonDataLines(t *testing.T) {
	body := bytes.Join([][]byte{
		[]byte("event: message_start"),
		[]byte("data: {\"a\":1}"),
		[]byte(""),
		[]byte("data: [DONE]"),
		[]byte("data:"),
		[]byte(": keep-alive"),
	}, []byte("\n"))

	var seen []string
	forEachSSEData(body, func(p []byte) { seen = append(seen, string(p)) })
	if len(seen) != 1 || seen[0] != `{"a":1}` {
		t.Fatalf("want only the real payload, got %v", seen)
	}
}

// Anthropic reports cache traffic as its own counters rather than folding it
// into input_tokens, so all three have to be read for a prompt total to be
// meaningful.
func TestExtractAnthropicUsageReadsCacheCounters(t *testing.T) {
	body := []byte(`{"type":"message","usage":{"input_tokens":10,"output_tokens":5,
		"cache_creation_input_tokens":100,"cache_read_input_tokens":890}}`)
	got := extractAnthropicUsage(body, false)
	if got.CacheCreation != 100 || got.CacheRead != 890 {
		t.Fatalf("want 100 created / 890 read, got %+v", got)
	}
	if got.promptTokens() != 1000 {
		t.Fatalf("prompt total must include both cache counters, got %d", got.promptTokens())
	}
}

func TestExtractAnthropicUsageStreamReadsCacheCounters(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":1,\"cache_creation_input_tokens\":300,\"cache_read_input_tokens\":700}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":80,\"cache_read_input_tokens\":700}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")
	got := extractAnthropicUsage([]byte(stream), true)
	if got.CacheCreation != 300 || got.CacheRead != 700 {
		t.Fatalf("want 300 created / 700 read, got %+v", got)
	}
	if got.Input != 4 || got.Output != 80 {
		t.Fatalf("cache counters must not disturb the plain counts, got %+v", got)
	}
}

// A later event that carries only cache counters must not wipe the input count
// reported earlier in the stream.
func TestMergeAnthropicUsageKeepsCountsAcrossEvents(t *testing.T) {
	u := mergeAnthropicUsage(tokenUsage{}, anthropicUsageFields{InputTokens: 50, CacheReadTokens: 200})
	u = mergeAnthropicUsage(u, anthropicUsageFields{OutputTokens: 9})
	if u.Input != 50 || u.Output != 9 || u.CacheRead != 200 {
		t.Fatalf("want input 50 / output 9 / read 200, got %+v", u)
	}
}

// OpenAI counts cached tokens inside prompt_tokens, so the cached half has to be
// subtracted: leaving it in would double it once the cache counters are added
// back, and the hit rate would come out far too low.
func TestExtractOpenAIUsageReadsCachedTokensWithoutDoubleCounting(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":20,
		"prompt_tokens_details":{"cached_tokens":750}}}`)
	got := extractOpenAIUsage(body, false)
	if got.CacheRead != 750 {
		t.Fatalf("want 750 cached, got %+v", got)
	}
	if got.Input != 250 {
		t.Fatalf("input must be the fresh prompt (1000-750), got %d", got.Input)
	}
	if got.promptTokens() != 1000 {
		t.Fatalf("the prompt total must stay 1000, got %d", got.promptTokens())
	}
}

// Anthropic already reports the fresh input, so its numbers must pass through
// untouched — the two gateways have to agree on what a prompt is.
func TestAnthropicAndOpenAIAgreeOnPromptTotal(t *testing.T) {
	anthropic := extractAnthropicUsage([]byte(`{"usage":{"input_tokens":300,"output_tokens":5,
		"cache_read_input_tokens":700}}`), false)
	openai := extractOpenAIUsage([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":5,
		"prompt_tokens_details":{"cached_tokens":700}}}`), false)
	if anthropic.promptTokens() != openai.promptTokens() {
		t.Fatalf("same traffic must give the same prompt total: %d vs %d",
			anthropic.promptTokens(), openai.promptTokens())
	}
	if anthropic.CacheRead != openai.CacheRead {
		t.Fatalf("same traffic must give the same cached count: %d vs %d", anthropic.CacheRead, openai.CacheRead)
	}
}

// A gateway that reports cached tokens above prompt_tokens must not produce a
// negative fresh prompt, which would push the hit rate over 100%.
func TestExtractOpenAIUsageClampsImpossibleCacheCounts(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":1,
		"prompt_tokens_details":{"cached_tokens":500}}}`)
	got := extractOpenAIUsage(body, false)
	if got.Input != 0 {
		t.Fatalf("fresh prompt must clamp at 0, got %d", got.Input)
	}
	if got.promptTokens() != 500 {
		t.Fatalf("prompt total must not go negative, got %d", got.promptTokens())
	}
}

func TestExtractOpenAIUsageStreamReadsCachedTokens(t *testing.T) {
	stream := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":400,\"completion_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":100}}}\n\n",
		"data: [DONE]\n\n",
	}, "")
	got := extractOpenAIUsage([]byte(stream), true)
	if got.CacheRead != 100 || got.Input != 300 || got.promptTokens() != 400 {
		t.Fatalf("want 100 cached of 400 (300 fresh), got %+v", got)
	}
}

// An upstream that reports no cache counters must read as a real zero, not as a
// missing value the page would have to guess at.
func TestCacheCountersDefaultToZero(t *testing.T) {
	got := extractAnthropicUsage([]byte(`{"usage":{"input_tokens":5,"output_tokens":5}}`), false)
	if got.CacheRead != 0 || got.CacheCreation != 0 {
		t.Fatalf("absent cache counters must be zero, got %+v", got)
	}
	if got.promptTokens() != 5 {
		t.Fatalf("prompt total falls back to input, got %d", got.promptTokens())
	}
}

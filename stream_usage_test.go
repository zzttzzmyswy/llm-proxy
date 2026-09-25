package main

import (
	"strconv"
	"strings"
	"testing"
)

// openAISSEWithUsage builds the smallest stream carrying an upstream usage
// chunk, the shape DeepSeek emits on the OpenAI gateway.
func openAISSEWithUsage(prompt, cached, completion int) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]," +
		"\"usage\":{\"prompt_tokens\":" + strconv.Itoa(prompt) +
		",\"completion_tokens\":" + strconv.Itoa(completion) +
		",\"prompt_tokens_details\":{\"cached_tokens\":" + strconv.Itoa(cached) + "}}}\n\n" +
		"data: [DONE]\n\n"
}

// TestStreamUsageSubtractsCachedFromInput pins the normalisation the reported
// cache hit rate depends on. OpenAI's prompt_tokens already counts the cached
// tokens, so leaving them in the fresh input double-counts every cached token —
// once as input, once as a cache read — which halves the hit rate of a
// well-cached conversation. The non-streaming path normalises via freshPrompt;
// the streaming path must agree.
func TestStreamUsageSubtractsCachedFromInput(t *testing.T) {
	const prompt, cached = 10000, 9990
	var buf strings.Builder
	usage, err := translateOpenAIStream(strings.NewReader(openAISSEWithUsage(prompt, cached, 4)), &buf, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Reported {
		t.Fatal("an upstream usage chunk must mark the usage as reported")
	}
	if usage.CacheRead != cached {
		t.Fatalf("cache read must carry the upstream cached count: got %d want %d", usage.CacheRead, cached)
	}
	if usage.Input != prompt-cached {
		t.Fatalf("fresh input must exclude the cached tokens: got %d want %d", usage.Input, prompt-cached)
	}
	if got := cacheHitRate(int64(usage.CacheRead), int64(usage.CacheCreation), int64(usage.Input)); got < 0.99 {
		t.Fatalf("a fully cached stream must report a hit rate near 1, got %.3f", got)
	}
}

// TestStreamUsageWithoutCachedCountsAllInput covers the other gateway shape: no
// cache counters at all, so every prompt token is fresh and the rate is zero.
func TestStreamUsageWithoutCachedCountsAllInput(t *testing.T) {
	var buf strings.Builder
	usage, err := translateOpenAIStream(strings.NewReader(openAISSEWithUsage(500, 0, 4)), &buf, "m")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Input != 500 || usage.CacheRead != 0 {
		t.Fatalf("uncached stream must read as 500 fresh input and no cache read, got input=%d cache=%d",
			usage.Input, usage.CacheRead)
	}
}

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

// TestStreamCarriesRealUsageToTheClient pins the client-visible half of the same
// accounting. The Anthropic framing reports usage twice: once in the opening
// `message_start` and once in the closing `message_delta`. message_start is
// emitted before the upstream stream has been read, so it cannot know the real
// counts and carries placeholders. A client that reads only message_start — the
// DeepSeek Messages adapter dsh uses does exactly that — therefore depends on
// the closing delta carrying the real numbers.
func TestStreamCarriesRealUsageToTheClient(t *testing.T) {
	const prompt, cached, completion = 10000, 9990, 4
	var buf strings.Builder
	if _, err := translateOpenAIStream(strings.NewReader(openAISSEWithUsage(prompt, cached, completion)), &buf, "m"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	var deltaUsage string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"message_delta"`) {
			deltaUsage = line
		}
	}
	if deltaUsage == "" {
		t.Fatal("the stream must close with a message_delta event")
	}
	// Fresh input, normalised like every other reporting path.
	if !strings.Contains(deltaUsage, `"input_tokens":`+strconv.Itoa(prompt-cached)) {
		t.Fatalf("message_delta must carry the real fresh input count (%d), got: %s", prompt-cached, deltaUsage)
	}
	if !strings.Contains(deltaUsage, `"cache_read_input_tokens":`+strconv.Itoa(cached)) {
		t.Fatalf("message_delta must carry the cached count (%d), got: %s", cached, deltaUsage)
	}
	if !strings.Contains(deltaUsage, `"output_tokens":`+strconv.Itoa(completion)) {
		t.Fatalf("message_delta must carry the completion count (%d), got: %s", completion, deltaUsage)
	}
}

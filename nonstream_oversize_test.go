package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// sizedReply builds a valid Anthropic reply whose body is exactly size bytes.
// The padding sits inside a text block, so the body stays parseable JSON and
// losing even one byte off the end — or any byte in the middle — makes
// json.Valid fail rather than going unnoticed.
func sizedReply(size int) string {
	const head = `{"id":"msg_big","type":"message","role":"assistant","model":"DeepSeek-Flash",` +
		`"content":[{"type":"text","text":"`
	const tail = `"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":5}}`
	if size < len(head)+len(tail) {
		panic("size too small")
	}
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

// A non-streaming reply that overflows the inspection buffer must reach the
// client byte-for-byte. The buffered prefix and the remaining upstream bytes are
// written as two separate copies, and the boundary between them is exactly where
// an off-by-one loses a byte: the reader consumes the byte at offset max to
// discover the overflow, so that byte has to be forwarded too.
func TestOversizedNonStreamReplyReachesTheClientWhole(t *testing.T) {
	for _, size := range []int{
		maxNonStreamBuffer - 1, // fits: buffered and inspected
		maxNonStreamBuffer,     // exactly the cap: fits, and must not be mistaken for an overflow
		maxNonStreamBuffer + 1, // one byte over: the narrowest overflow
		maxNonStreamBuffer + 2,
		maxNonStreamBuffer + 4096, // a normal chunk boundary past the cap
	} {
		body := sizedReply(size)
		if len(body) != size {
			t.Fatalf("fixture is %d bytes, wanted %d", len(body), size)
		}

		withCleanStats(t)
		withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
		withUpstream(t, "application/json", body)

		resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

		if resp != body {
			t.Fatalf("size %d: the reply must reach the client byte-for-byte, got %d bytes (lost %d)",
				size, len(resp), len(body)-len(resp))
		}
		// The reply is what the client parses; a dropped byte turns it into a
		// "Failed to parse JSON" on the client side even though the length may
		// look plausible.
		if !json.Valid([]byte(resp)) {
			t.Fatalf("size %d: the reply must stay valid JSON", size)
		}
	}
}

// The overflow path cannot inspect the usage block, so it must fall back to the
// request-side estimate and still record the turn — an oversized reply that
// reports nothing would quietly drop the conversation from the dashboard.
func TestOversizedNonStreamReplyIsStillAccounted(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})
	withUpstream(t, "application/json", sizedReply(maxNonStreamBuffer+1))

	callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	m := findModel(t, "DeepSeek-Flash")
	if m.Requests != 1 {
		t.Fatalf("the oversized turn must still be recorded, got %d requests", m.Requests)
	}
}

// A reply that exactly fills the buffer is complete, not truncated: it must be
// inspected like any other, not diverted into the oversized pass-through path.
func TestReplyExactlyAtTheBufferCapIsStillInspected(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	// Build a marked reply that lands exactly on the cap so it takes the
	// inspect-and-normalise path rather than the overflow path.
	const head = `{"id":"msg_1","type":"message","role":"assistant","model":"DeepSeek-Flash","content":[{"type":"text","text":"`
	const mid = `"}],"stop_reason":"end_turn","usage":`
	const tail = `{"input_tokens":4281,"cache_creation_input_tokens":0,"cache_read_input_tokens":4224,"output_tokens":8,"billing_usage":{"source":"oai_chat","semantic":"openai","openai_usage":{"prompt_tokens":4281}}}}`
	body := head + strings.Repeat("y", maxNonStreamBuffer-len(head)-len(mid)-len(tail)) + mid + tail
	if len(body) != maxNonStreamBuffer {
		t.Fatalf("fixture is %d bytes, wanted %d", len(body), maxNonStreamBuffer)
	}
	withUpstream(t, "application/json", body)

	resp := callHandleMessages(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	// The normalised body is legitimately shorter than the fixture (the
	// billing_usage marker is dropped), so the length alone proves nothing; what
	// matters is that all of it was read and parsed rather than cut short.
	if !json.Valid([]byte(resp)) {
		t.Fatalf("a reply filling the buffer must not be truncated, got %d bytes: %q", len(resp), resp[max(0, len(resp)-80):])
	}
	var parsed struct {
		Usage struct {
			InputTokens float64 `json:"input_tokens"`
			CacheRead   float64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resp), &parsed); err != nil {
		t.Fatalf("the reply must stay valid JSON: %v", err)
	}
	if parsed.Usage.InputTokens != 57 || parsed.Usage.CacheRead != 4224 {
		t.Fatalf("a reply at exactly the cap must still be normalised, got in=%v cache_read=%v",
			parsed.Usage.InputTokens, parsed.Usage.CacheRead)
	}
}

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveThroughRealServer drives handleMessages through a real net/http server.
// httptest.NewRecorder only records what the handler wrote and ignores
// Content-Length entirely, so a reply whose declared framing disagrees with its
// body passes every recorder-based test and still breaks a real client.
func serveThroughRealServer(t *testing.T, reqBody string) (string, http.Header, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handleMessages))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(resp.Body)
	return string(b), resp.Header, readErr
}

// withFramedAnthropicUpstream starts an upstream that answers with an explicit
// Content-Length, which is what a real HTTP/1.1 gateway does.
func withFramedAnthropicUpstream(t *testing.T, contentType, body string) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(200)
		io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = up.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })
}

// A non-streaming reply that the proxy shortens (here by dropping the upstream's
// "openai" usage marker) must not keep the upstream's Content-Length. Copying it
// makes net/http refuse the shorter write and the client sees a truncated body —
// "unexpected EOF" on a reply the upstream answered correctly.
func TestNonStreamReplyDoesNotInheritUpstreamContentLength(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	body := `{"id":"msg_1","type":"message","role":"assistant","model":"DeepSeek-Flash",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":4281,"cache_creation_input_tokens":0,"cache_read_input_tokens":4224,` +
		`"output_tokens":8,"billing_usage":{"source":"oai_chat","semantic":"openai",` +
		`"openai_usage":{"prompt_tokens":4281}}}}`
	withFramedAnthropicUpstream(t, "application/json", body)

	got, hdr, readErr := serveThroughRealServer(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if readErr != nil {
		t.Fatalf("the client could not read the reply: %v (header Content-Length=%q, got %d of %d bytes)",
			readErr, hdr.Get("Content-Length"), len(got), len(body))
	}
	if len(got) == 0 {
		t.Fatal("the client received an empty body")
	}
	if !strings.Contains(got, `"hi"`) {
		t.Fatalf("the reply must reach the client whole, got %q", got)
	}
	// The declared framing must describe the body that was actually sent.
	if cl := hdr.Get("Content-Length"); cl != "" && cl != fmt.Sprint(len(got)) {
		t.Fatalf("declared Content-Length %q does not match the %d bytes sent", cl, len(got))
	}
}

// A streaming reply is the same hazard. An upstream that frames its SSE body with
// a Content-Length (any HTTP/1.1 gateway) has that length describe the bytes it
// sent; the proxy appends the missing message_stop, so the body it writes is a
// different size. net/http then refuses the extra write, and a 200 reaches the
// client truncated — with the closing frame, the part the client waits for, gone.
func TestStreamReplyDoesNotInheritUpstreamContentLength(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n"
	// An explicit Content-Length on the stream, which is what a real gateway
	// sends; the body deliberately omits message_stop so the proxy appends it.
	withFramedAnthropicUpstream(t, "text/event-stream", stream)

	got, hdr, readErr := serveThroughRealServer(t, `{"model":"sonnet","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)

	if readErr != nil {
		t.Fatalf("the client could not read the stream: %v (header Content-Length=%q, got %d bytes)",
			readErr, hdr.Get("Content-Length"), len(got))
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("the streamed content must reach the client, got %d bytes: %q", len(got), got)
	}
	if !strings.Contains(got, "message_stop") {
		t.Fatalf("the appended message_stop must reach the client, got:\n%s", got)
	}
	if cl := hdr.Get("Content-Length"); cl != "" && cl != fmt.Sprint(len(got)) {
		t.Fatalf("declared Content-Length %q does not match the %d bytes sent", cl, len(got))
	}
}

// The negation, so the fix cannot be "drop every upstream header": a header that
// describes the payload rather than its framing must still be forwarded.
func TestUpstreamPayloadHeadersAreStillForwarded(t *testing.T) {
	withCleanStats(t)
	withRoutes(t, map[string]RouteEntry{"sonnet": {Model: "DeepSeek-Flash"}})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-abc")
		w.Header().Set("Retry-After", "7")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":5}}`)
	}))
	t.Cleanup(up.Close)
	oldURL := cfg.Upstream.AnthropicURL
	cfg.Upstream.AnthropicURL = up.URL
	t.Cleanup(func() { cfg.Upstream.AnthropicURL = oldURL })

	_, hdr, _ := serveThroughRealServer(t, `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if hdr.Get("X-Request-Id") != "req-abc" {
		t.Fatalf("an upstream payload header must still reach the client, got %q", hdr.Get("X-Request-Id"))
	}
	if hdr.Get("Retry-After") != "7" {
		t.Fatalf("Retry-After must still reach the client, got %q", hdr.Get("Retry-After"))
	}
}

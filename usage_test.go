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

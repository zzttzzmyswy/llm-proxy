package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two parallel tool calls in one reply. Anthropic numbers every content block
// with `index`, and the blocks must close in that order: finish() used to range
// over the tool map, whose iteration order Go randomises, so the same reply
// could close block 2 before block 1. Repeated because a single pass can get the
// order right by luck.
func TestStreamClosesToolBlocksInIndexOrder(t *testing.T) {
	chunks := `data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"A","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"B","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	for attempt := 0; attempt < 200; attempt++ {
		var buf strings.Builder
		translateOpenAIStream(strings.NewReader(chunks), &buf, "m")
		out := buf.String()
		var stops []int
		for _, p := range ssePayloadsAll(out, "content_block_stop") {
			var ev struct {
				Index int `json:"index"`
			}
			if json.Unmarshal([]byte(p), &ev) == nil {
				stops = append(stops, ev.Index)
			}
		}
		for i := 1; i < len(stops); i++ {
			if stops[i] < stops[i-1] {
				t.Fatalf("content_block_stop order %v is not ascending on attempt %d:\n%s", stops, attempt, out)
			}
		}
	}
}

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tool call's arguments travel to the gateway as a JSON string the upstream
// parses. jsonString truncates at 500 bytes to bound a VLM prompt, and the
// translator used to share it, so an assistant history turn whose tool input was
// longer than 500 bytes went out as a cut-off, unparseable JSON document. The
// upstream then rejects the tool call (or the whole request), which breaks any
// conversation whose agent wrote a large file or sent a long command.
func TestAnthropicToOpenAIToolArgumentsAreNotTruncated(t *testing.T) {
	const payload = 4000
	body := strings.Repeat("A", payload)

	out := anthropicToOpenAIRequest(map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "assistant",
				"content": []interface{}{
					map[string]interface{}{
						"type": "tool_use",
						"id":   "toolu_1",
						"name": "Write",
						"input": map[string]interface{}{
							"file_path": "/tmp/x",
							"content":   body,
						},
					},
				},
			},
		},
	}, "glm-5.3-flash")

	msgs, ok := out["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		t.Fatalf("the translated request must carry the assistant turn, got %+v", out["messages"])
	}
	msg := msgs[0].(map[string]interface{})
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) == 0 {
		t.Fatalf("the assistant turn must carry tool_calls, got %+v", msg)
	}
	fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
	args, _ := fn["arguments"].(string)

	// The arguments field IS the JSON document the upstream parses: cutting it
	// anywhere makes the tool call unusable.
	if !json.Valid([]byte(args)) {
		t.Fatalf("tool arguments must stay valid JSON, got %d bytes ending %q", len(args), args[max(0, len(args)-40):])
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("tool arguments must parse: %v", err)
	}
	if got, _ := parsed["content"].(string); len(got) != payload {
		t.Fatalf("tool arguments lost %d bytes of the input", payload-len(got))
	}
}

// The same rule on the reply path: a translated tool_use block reaches the client
// as an input_json_delta, which Claude Code parses as JSON. Truncating it leaves
// the client with a tool call it cannot execute.
func TestAnthropicSSEToolInputIsNotTruncated(t *testing.T) {
	const payload = 4000
	translated, err := json.Marshal(map[string]interface{}{
		"id":   "msg_1",
		"type": "message",
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{
				"type":  "tool_use",
				"id":    "call_1",
				"name":  "Write",
				"input": map[string]interface{}{"content": strings.Repeat("B", payload)},
			},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]interface{}{"input_tokens": 1, "output_tokens": 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	sse, err := anthropicMessageToSSE(translated, "glm-5.3-flash")
	if err != nil {
		t.Fatalf("anthropicMessageToSSE: %v", err)
	}

	var partial string
	for _, p := range ssePayloadsAll(string(sse), "content_block_delta") {
		var ev struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(p), &ev) != nil {
			continue
		}
		if ev.Delta.Type == "input_json_delta" {
			partial = ev.Delta.PartialJSON
		}
	}
	if partial == "" {
		t.Fatalf("the tool_use block must emit an input_json_delta, got:\n%s", sse)
	}
	if !json.Valid([]byte(partial)) {
		t.Fatalf("partial_json must stay valid JSON, got %d bytes", len(partial))
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(partial), &parsed); err != nil {
		t.Fatalf("partial_json must parse: %v", err)
	}
	if got, _ := parsed["content"].(string); len(got) != payload {
		t.Fatalf("partial_json lost %d bytes of the input", payload-len(got))
	}
}

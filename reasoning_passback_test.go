package main

import (
	"encoding/json"
	"testing"
)

// turnReq builds an Anthropic request whose history holds n completed
// assistant tool_use turns, each followed by its tool_result.
func turnReq(n int) map[string]interface{} {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "text", "text": "start"},
		}},
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs, map[string]interface{}{"role": "assistant", "content": []interface{}{
			map[string]interface{}{
				"type": "tool_use", "id": "t", "name": "Read",
				"input": map[string]interface{}{"file_path": "/tmp/a"},
			},
		}})
		msgs = append(msgs, map[string]interface{}{"role": "user", "content": []interface{}{
			map[string]interface{}{
				"type": "tool_result", "tool_use_id": "t",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "body"}},
			},
		}})
	}
	raw, err := json.Marshal(map[string]interface{}{"model": "m", "messages": msgs})
	if err != nil {
		panic(err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(raw, &req); err != nil {
		panic(err)
	}
	return req
}

func translated(t *testing.T, n int) []interface{} {
	t.Helper()
	out := anthropicToOpenAIRequest(turnReq(n), "target")
	msgs, _ := out["messages"].([]interface{})
	return msgs
}

// TestReasoningPassBackEveryToolCallTurn requires the placeholder on every
// tool_calls turn. The upstream rejects a history that omits it on an earlier
// turn just as it rejects the final one.
func TestReasoningPassBackEveryToolCallTurn(t *testing.T) {
	for _, m := range translated(t, 4) {
		msg := m.(map[string]interface{})
		if _, has := msg["tool_calls"]; !has {
			continue
		}
		if _, has := msg["reasoning_content"]; !has {
			t.Fatalf("every tool_calls turn must carry reasoning_content, got %+v", msg)
		}
	}
}

// TestReasoningPassBackKeepsEarlierTurnsStable pins the property the upstream's
// prefix cache depends on: appending a turn must leave every already-sent
// message identical. Patching only the last assistant message made the field
// appear while a turn was last and vanish once a newer turn arrived, so the
// prefix diverged at that message on every turn and the whole accumulated tail
// was re-billed as a cache miss.
func TestReasoningPassBackKeepsEarlierTurnsStable(t *testing.T) {
	for n := 1; n <= 5; n++ {
		before := translated(t, n)
		after := translated(t, n+1)
		if len(after) <= len(before) {
			t.Fatalf("appending a turn must not shrink the history: %d -> %d", len(before), len(after))
		}
		for i, want := range before {
			gotA, _ := json.Marshal(after[i])
			gotB, _ := json.Marshal(want)
			if string(gotA) != string(gotB) {
				t.Fatalf("turn %d: message %d changed when a newer turn arrived:\n before %s\n after  %s",
					n, i, gotB, gotA)
			}
		}
	}
}
